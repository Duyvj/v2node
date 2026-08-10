package tuic

import (
	"bytes"
	"context"
	"encoding/binary"
	stderrors "errors"
	"io"
	"math"
	stdnet "net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apernet/quic-go"

	"github.com/xtls/xray-core/common/errors"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
)

type PacketConn interface {
	stdnet.Conn
	ReadPacket() ([]byte, xnet.Destination, error)
	WritePacket([]byte, xnet.Destination) error
}

const (
	// FRAG_TOTAL is an unsigned byte in TUIC. Packet-count and byte limits below
	// bound defragmentation memory while preserving the complete wire range.
	maxUDPFragments         = 255
	maxIncompleteUDPPackets = 32
	maxUDPDefragBytes       = 512 * 1024
	udpDefragTTL            = 10 * time.Second
)

var errUDPIdleTimeout = stderrors.New("TUIC UDP session idle timeout")

type udpQueueBudget struct {
	limit int64
	used  atomic.Int64
}

func newUDPQueueBudget(limit int64) *udpQueueBudget {
	return &udpQueueBudget{limit: limit}
}

func (b *udpQueueBudget) reserve(size int) bool {
	if b == nil || size == 0 {
		return true
	}
	if size < 0 || int64(size) > b.limit {
		return false
	}
	for {
		used := b.used.Load()
		if used+int64(size) > b.limit {
			return false
		}
		if b.used.CompareAndSwap(used, used+int64(size)) {
			return true
		}
	}
}

func (b *udpQueueBudget) release(size int) {
	if b != nil && size > 0 {
		b.used.Add(-int64(size))
	}
}

type udpMessage struct {
	sessionID     uint16
	packetID      uint16
	fragmentTotal uint8
	fragmentID    uint8
	destination   xnet.Destination
	data          []byte
	queuedBytes   int
	queueBudget   *udpQueueBudget
}

func (m *udpMessage) releaseQueueBytes() {
	if m == nil || m.queuedBytes == 0 || m.queueBudget == nil {
		return
	}
	m.queueBudget.release(m.queuedBytes)
	m.queuedBytes = 0
}

func (m *udpMessage) pack() ([]byte, error) {
	buffer := bytes.NewBuffer(make([]byte, 0, m.headerSize()+len(m.data)))
	buffer.WriteByte(tuicVersion)
	buffer.WriteByte(commandPacket)
	if err := binary.Write(buffer, binary.BigEndian, m.sessionID); err != nil {
		return nil, err
	}
	if err := binary.Write(buffer, binary.BigEndian, m.packetID); err != nil {
		return nil, err
	}
	if err := binary.Write(buffer, binary.BigEndian, m.fragmentTotal); err != nil {
		return nil, err
	}
	if err := binary.Write(buffer, binary.BigEndian, m.fragmentID); err != nil {
		return nil, err
	}
	if err := binary.Write(buffer, binary.BigEndian, uint16(len(m.data))); err != nil {
		return nil, err
	}
	if err := writeDestination(buffer, m.destination); err != nil {
		return nil, err
	}
	buffer.Write(m.data)
	return buffer.Bytes(), nil
}

func (m *udpMessage) headerSize() int {
	return 10 + destinationLen(m.destination)
}

func fragUDPMessage(message *udpMessage, maxPacketSize int) []*udpMessage {
	udpMTU := maxPacketSize - message.headerSize()
	if udpMTU <= 0 || len(message.data) <= udpMTU {
		return []*udpMessage{message}
	}
	var fragments []*udpMessage
	originPacket := message.data
	for remaining := len(originPacket); remaining > 0; remaining -= udpMTU {
		fragment := *message
		if remaining > udpMTU {
			fragment.data = originPacket[:udpMTU]
			originPacket = originPacket[udpMTU:]
		} else {
			fragment.data = originPacket
			originPacket = nil
		}
		fragments = append(fragments, &fragment)
	}
	for index, fragment := range fragments {
		fragment.fragmentID = uint8(index)
		fragment.fragmentTotal = uint8(len(fragments))
		if index > 0 {
			fragment.destination = xnet.Destination{}
		}
	}
	return fragments
}

var _ PacketConn = (*udpPacketConn)(nil)

type udpSendStream interface {
	Write([]byte) (int, error)
	Close() error
	CancelWrite(quic.StreamErrorCode)
	SetWriteDeadline(time.Time) error
}

type udpPacketTransport interface {
	SendDatagram(context.Context, []byte) error
	OpenUniStream(context.Context) (udpSendStream, error)
	LocalAddr() stdnet.Addr
	RemoteAddr() stdnet.Addr
}

type quicUDPPacketTransport struct {
	conn      *quic.Conn
	datagrams chan datagramSendRequest
}

type datagramSendRequest struct {
	ctx    context.Context
	data   []byte
	result chan error
}

func newQUICUDPPacketTransport(conn *quic.Conn) *quicUDPPacketTransport {
	transport := &quicUDPPacketTransport{
		conn:      conn,
		datagrams: make(chan datagramSendRequest, 32),
	}
	go transport.sendDatagrams()
	return transport
}

func (t *quicUDPPacketTransport) SendDatagram(ctx context.Context, data []byte) error {
	request := datagramSendRequest{
		ctx:    ctx,
		data:   data,
		result: make(chan error, 1),
	}
	select {
	case t.datagrams <- request:
	case <-t.conn.Context().Done():
		return context.Cause(t.conn.Context())
	case <-ctx.Done():
		return context.Cause(ctx)
	}
	select {
	case err := <-request.result:
		return err
	case <-t.conn.Context().Done():
		return context.Cause(t.conn.Context())
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (t *quicUDPPacketTransport) sendDatagrams() {
	for {
		select {
		case <-t.conn.Context().Done():
			return
		case request := <-t.datagrams:
			select {
			case <-request.ctx.Done():
				request.result <- context.Cause(request.ctx)
				continue
			default:
			}
			request.result <- t.conn.SendDatagram(request.data)
		}
	}
}

func (t *quicUDPPacketTransport) OpenUniStream(ctx context.Context) (udpSendStream, error) {
	return t.conn.OpenUniStreamSync(ctx)
}

func (t *quicUDPPacketTransport) LocalAddr() stdnet.Addr {
	return t.conn.LocalAddr()
}

func (t *quicUDPPacketTransport) RemoteAddr() stdnet.Addr {
	return t.conn.RemoteAddr()
}

type udpPacketConn struct {
	ctx           context.Context
	cancel        context.CancelCauseFunc
	sessionID     uint16
	transport     udpPacketTransport
	data          chan *udpMessage
	udpStream     bool
	udpMTU        atomic.Int64
	packetID      atomic.Uint32
	closeOnce     sync.Once
	isServer      bool
	defragger     *udpDefragger
	onDestroy     func()
	idleTimeout   time.Duration
	idleAccess    sync.Mutex
	idleTimer     *time.Timer
	lastActivity  atomic.Int64
	readDeadline  packetDeadline
	writeDeadline packetDeadline
	startAccess   sync.Mutex
	started       bool
	user          *protocol.MemoryUser

	writeAccess   sync.Mutex
	writeClosing  bool
	writeWorkers  sync.WaitGroup
	activeStreams map[udpSendStream]struct{}
	dataAccess    sync.Mutex
	inputClosed   bool
}

func newUDPPacketConn(ctx context.Context, quicConn *quic.Conn, udpStream bool, isServer bool, user *protocol.MemoryUser, idleTimeout time.Duration, onDestroy func()) *udpPacketConn {
	var transport udpPacketTransport
	if quicConn != nil {
		transport = newQUICUDPPacketTransport(quicConn)
	}
	return newUDPPacketConnWithTransport(ctx, transport, udpStream, isServer, user, idleTimeout, onDestroy)
}

func newUDPPacketConnWithTransport(ctx context.Context, transport udpPacketTransport, udpStream bool, isServer bool, user *protocol.MemoryUser, idleTimeout time.Duration, onDestroy func()) *udpPacketConn {
	ctx, cancel := context.WithCancelCause(ctx)
	conn := &udpPacketConn{
		ctx:           ctx,
		cancel:        cancel,
		transport:     transport,
		data:          make(chan *udpMessage, 64),
		udpStream:     udpStream,
		isServer:      isServer,
		defragger:     newUDPDefragger(),
		onDestroy:     onDestroy,
		idleTimeout:   idleTimeout,
		readDeadline:  newPacketDeadline(),
		writeDeadline: newPacketDeadline(),
		activeStreams: make(map[udpSendStream]struct{}),
		user:          user,
	}
	conn.udpMTU.Store(1200 - 3)
	conn.touch()
	conn.startIdleTimer()
	return conn
}

func (c *udpPacketConn) User() *protocol.MemoryUser {
	return c.user
}

func (c *udpPacketConn) done() bool {
	select {
	case <-c.ctx.Done():
		return true
	default:
		return false
	}
}

func (c *udpPacketConn) markStarted(destination xnet.Destination) bool {
	if !destination.IsValid() {
		return false
	}
	c.startAccess.Lock()
	defer c.startAccess.Unlock()
	if c.started {
		return false
	}
	c.started = true
	return true
}

func (c *udpPacketConn) ReadPacket() ([]byte, xnet.Destination, error) {
	for {
		notify, deadline, deadlineClosed := c.readDeadline.snapshot()
		if deadlineClosed {
			return nil, xnet.Destination{}, io.ErrClosedPipe
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return nil, xnet.Destination{}, os.ErrDeadlineExceeded
		}
		select {
		case p := <-c.data:
			if p == nil {
				return nil, xnet.Destination{}, io.ErrClosedPipe
			}
			p.releaseQueueBytes()
			return p.data, p.destination, nil
		case <-c.ctx.Done():
			return nil, xnet.Destination{}, io.ErrClosedPipe
		case <-notify:
		}
	}
}

func (c *udpPacketConn) ReadFrom(p []byte) (n int, addr stdnet.Addr, err error) {
	data, destination, err := c.ReadPacket()
	if err != nil {
		return 0, nil, err
	}
	n = copy(p, data)
	return n, destinationAddr{destination: destination}, nil
}

func (c *udpPacketConn) Read(p []byte) (int, error) {
	n, _, err := c.ReadFrom(p)
	return n, err
}

func (c *udpPacketConn) WritePacket(data []byte, destination xnet.Destination) error {
	select {
	case <-c.ctx.Done():
		return stdnet.ErrClosed
	default:
	}
	if len(data) > 0xffff {
		return &quic.DatagramTooLargeError{MaxDatagramPayloadSize: 0xffff}
	}
	if !destination.IsValid() {
		return os.ErrInvalid
	}
	if !c.beginWrite() {
		return stdnet.ErrClosed
	}
	defer c.writeWorkers.Done()
	c.touch()
	packetID := uint16(c.packetID.Add(1) % math.MaxUint16)
	message := &udpMessage{
		sessionID:     c.sessionID,
		packetID:      packetID,
		fragmentTotal: 1,
		destination:   destination,
		data:          data,
	}
	return c.writePacketOrFragments(message, len(data))
}

func (c *udpPacketConn) WriteTo(p []byte, addr stdnet.Addr) (n int, err error) {
	destination, err := destinationFromNetAddr(addr, xnet.Network_UDP)
	if err != nil {
		return 0, err
	}
	if err = c.WritePacket(p, destination); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *udpPacketConn) Write(p []byte) (int, error) {
	if c.transport == nil {
		return 0, io.ErrClosedPipe
	}
	return c.WriteTo(p, c.transport.RemoteAddr())
}

func (c *udpPacketConn) writePacketOrFragments(message *udpMessage, dataLen int) error {
	udpMTU := int(c.udpMTU.Load())
	var err error
	if !c.udpStream && dataLen > udpMTU-message.headerSize() {
		err = c.writePackets(fragUDPMessage(message, udpMTU))
	} else {
		err = c.writePacket(message)
	}
	if err == nil {
		return nil
	}
	var tooLargeErr *quic.DatagramTooLargeError
	if !stderrors.As(err, &tooLargeErr) {
		return err
	}
	udpMTU = int(tooLargeErr.MaxDatagramPayloadSize) - 3
	c.udpMTU.Store(int64(udpMTU))
	return c.writePackets(fragUDPMessage(message, udpMTU))
}

func (c *udpPacketConn) inputPacket(message *udpMessage) bool {
	select {
	case <-c.ctx.Done():
		message.releaseQueueBytes()
		return false
	default:
	}
	if message.fragmentTotal <= 1 {
		if c.isServer {
			errors.LogDebug(c.ctx, "TUIC received UDP packet on session=", c.sessionID, " size=", len(message.data), " dest=", message.destination)
		}
		if !c.enqueuePacket(message) {
			message.releaseQueueBytes()
			return false
		}
		c.touch()
		return true
	}
	newMessage, accepted := c.defragger.feedWithStatus(message)
	if newMessage != nil {
		if c.isServer {
			errors.LogDebug(c.ctx, "TUIC reassembled UDP fragment set on session=", c.sessionID, " size=", len(newMessage.data))
		}
		if !c.enqueuePacket(newMessage) {
			newMessage.releaseQueueBytes()
			return false
		}
	}
	if accepted {
		c.touch()
	}
	return accepted
}

func (c *udpPacketConn) enqueuePacket(message *udpMessage) bool {
	c.dataAccess.Lock()
	defer c.dataAccess.Unlock()
	if c.inputClosed {
		return false
	}
	select {
	case c.data <- message:
		return true
	default:
		return false
	}
}

func (c *udpPacketConn) writePackets(messages []*udpMessage) error {
	for _, message := range messages {
		if err := c.writePacket(message); err != nil {
			return err
		}
	}
	return nil
}

func (c *udpPacketConn) writePacket(message *udpMessage) error {
	buffer, err := message.pack()
	if err != nil {
		return err
	}
	if !c.udpStream {
		errors.LogDebug(c.ctx, "TUIC sending UDP datagram session=", c.sessionID, " size=", len(buffer))
		return c.sendDatagramCancelAware(buffer)
	}
	if c.transport == nil {
		return io.ErrClosedPipe
	}
	stream, err := c.transport.OpenUniStream(c.ctx)
	if err != nil {
		return err
	}
	if !c.registerStream(stream) {
		stream.CancelWrite(0)
		return stdnet.ErrClosed
	}
	defer c.unregisterStream(stream)
	stopCancel := context.AfterFunc(c.ctx, func() {
		stream.CancelWrite(0)
	})
	defer stopCancel()
	errors.LogDebug(c.ctx, "TUIC sending UDP stream packet session=", c.sessionID, " size=", len(buffer))
	_, err = stream.Write(buffer)
	closeErr := stream.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func (c *udpPacketConn) beginWrite() bool {
	c.writeAccess.Lock()
	defer c.writeAccess.Unlock()
	if c.writeClosing || c.done() {
		return false
	}
	c.writeWorkers.Add(1)
	return true
}

func (c *udpPacketConn) registerStream(stream udpSendStream) bool {
	if stream == nil {
		return false
	}
	c.writeAccess.Lock()
	defer c.writeAccess.Unlock()
	if c.writeClosing || c.done() {
		return false
	}
	c.activeStreams[stream] = struct{}{}
	_, deadline, deadlineClosed := c.writeDeadline.snapshot()
	if deadlineClosed {
		delete(c.activeStreams, stream)
		return false
	}
	if !deadline.IsZero() {
		_ = stream.SetWriteDeadline(deadline)
	}
	return true
}

func (c *udpPacketConn) unregisterStream(stream udpSendStream) {
	c.writeAccess.Lock()
	delete(c.activeStreams, stream)
	c.writeAccess.Unlock()
}

func (c *udpPacketConn) sendDatagramCancelAware(buffer []byte) error {
	if c.transport == nil {
		return io.ErrClosedPipe
	}
	opCtx, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)
	result := make(chan error, 1)
	go func() {
		result <- c.transport.SendDatagram(opCtx, buffer)
	}()
	for {
		notify, deadline, deadlineClosed := c.writeDeadline.snapshot()
		if deadlineClosed {
			cancel(stdnet.ErrClosed)
			return stdnet.ErrClosed
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			cancel(os.ErrDeadlineExceeded)
			return os.ErrDeadlineExceeded
		}
		select {
		case err := <-result:
			return err
		case <-c.ctx.Done():
			cancel(stdnet.ErrClosed)
			return stdnet.ErrClosed
		case <-notify:
		}
	}
}

func (c *udpPacketConn) Close() error {
	c.closeWithError(os.ErrClosed)
	return nil
}

func (c *udpPacketConn) closeWithError(err error) {
	c.closeOnce.Do(func() {
		c.cancel(err)
		c.writeAccess.Lock()
		c.writeClosing = true
		streams := make([]udpSendStream, 0, len(c.activeStreams))
		for stream := range c.activeStreams {
			streams = append(streams, stream)
		}
		c.writeAccess.Unlock()
		for _, stream := range streams {
			stream.CancelWrite(0)
		}
		c.writeWorkers.Wait()
		c.writeAccess.Lock()
		c.activeStreams = nil
		c.writeAccess.Unlock()

		c.dataAccess.Lock()
		c.inputClosed = true
		for {
			select {
			case message := <-c.data:
				if message != nil {
					message.releaseQueueBytes()
				}
			default:
				c.dataAccess.Unlock()
				goto queueDrained
			}
		}
	queueDrained:

		c.idleAccess.Lock()
		if c.idleTimer != nil {
			c.idleTimer.Stop()
			c.idleTimer = nil
		}
		c.idleAccess.Unlock()
		c.readDeadline.close()
		c.writeDeadline.close()
		c.defragger.close()
		if !c.isServer && c.transport != nil {
			ctx, cancel := context.WithTimeout(context.Background(), controlWriteTimeout)
			_ = writeDissociate(ctx, c.transport.OpenUniStream, c.sessionID)
			cancel()
		}
		if c.onDestroy != nil {
			c.onDestroy()
		}
	})
}

func (c *udpPacketConn) touch() {
	c.lastActivity.Store(time.Now().UnixNano())
}

func (c *udpPacketConn) startIdleTimer() {
	if c.idleTimeout <= 0 {
		return
	}
	c.idleAccess.Lock()
	defer c.idleAccess.Unlock()
	if c.done() {
		return
	}
	c.idleTimer = time.AfterFunc(c.idleTimeout, c.handleIdleTimeout)
}

func (c *udpPacketConn) handleIdleTimeout() {
	c.idleAccess.Lock()
	if c.done() {
		c.idleTimer = nil
		c.idleAccess.Unlock()
		return
	}
	idleFor := time.Duration(time.Now().UnixNano() - c.lastActivity.Load())
	if idleFor < c.idleTimeout {
		c.idleTimer.Reset(c.idleTimeout - idleFor)
		c.idleAccess.Unlock()
		return
	}
	c.idleTimer = nil
	c.idleAccess.Unlock()
	c.closeWithError(errUDPIdleTimeout)
}

func (c *udpPacketConn) LocalAddr() stdnet.Addr {
	if c.transport == nil {
		return nil
	}
	return c.transport.LocalAddr()
}

func (c *udpPacketConn) RemoteAddr() stdnet.Addr {
	if c.transport == nil {
		return nil
	}
	return c.transport.RemoteAddr()
}

func (c *udpPacketConn) SetDeadline(t time.Time) error {
	c.readDeadline.set(t)
	c.setWriteDeadline(t)
	return nil
}

func (c *udpPacketConn) SetReadDeadline(t time.Time) error {
	c.readDeadline.set(t)
	return nil
}

func (c *udpPacketConn) SetWriteDeadline(t time.Time) error {
	c.setWriteDeadline(t)
	return nil
}

func (c *udpPacketConn) setWriteDeadline(t time.Time) {
	c.writeDeadline.set(t)
	c.writeAccess.Lock()
	for stream := range c.activeStreams {
		_ = stream.SetWriteDeadline(t)
	}
	c.writeAccess.Unlock()
}

type udpDefragger struct {
	access           sync.Mutex
	packetMap        map[uint16]*packetItem
	bufferedBytes    int
	ttl              time.Duration
	maxPackets       int
	maxBufferedBytes int
	timer            *time.Timer
	closed           bool
}

func newUDPDefragger() *udpDefragger {
	return newUDPDefraggerWithLimits(udpDefragTTL, maxIncompleteUDPPackets, maxUDPDefragBytes)
}

func newUDPDefraggerWithLimits(ttl time.Duration, maxPackets, maxBufferedBytes int) *udpDefragger {
	if ttl <= 0 {
		ttl = udpDefragTTL
	}
	if maxPackets <= 0 {
		maxPackets = maxIncompleteUDPPackets
	}
	if maxBufferedBytes <= 0 {
		maxBufferedBytes = maxUDPDefragBytes
	}
	return &udpDefragger{
		packetMap:        make(map[uint16]*packetItem),
		ttl:              ttl,
		maxPackets:       maxPackets,
		maxBufferedBytes: maxBufferedBytes,
	}
}

type packetItem struct {
	messages  []*udpMessage
	count     uint8
	bytes     int
	expiresAt time.Time
}

func (d *udpDefragger) feed(m *udpMessage) *udpMessage {
	message, _ := d.feedWithStatus(m)
	return message
}

func (d *udpDefragger) feedWithStatus(m *udpMessage) (*udpMessage, bool) {
	if m == nil {
		return nil, false
	}
	if m.fragmentTotal <= 1 {
		return m, true
	}
	if m.fragmentID >= m.fragmentTotal || len(m.data) > math.MaxUint16 {
		m.releaseQueueBytes()
		return nil, false
	}
	d.access.Lock()
	defer d.access.Unlock()
	if d.closed {
		m.releaseQueueBytes()
		return nil, false
	}
	now := time.Now()
	d.removeExpiredLocked(now)
	defer d.scheduleExpirationLocked(now)
	item := d.packetMap[m.packetID]
	if item == nil || int(m.fragmentTotal) != len(item.messages) {
		if item != nil {
			d.removePacketLocked(m.packetID)
		}
		for len(d.packetMap) >= d.maxPackets {
			d.removeOldestLocked(0, false)
		}
		item = &packetItem{
			messages:  make([]*udpMessage, m.fragmentTotal),
			expiresAt: now.Add(d.ttl),
		}
		d.packetMap[m.packetID] = item
	}
	if item.messages[m.fragmentID] != nil {
		m.releaseQueueBytes()
		return nil, false
	}
	if item.bytes+len(m.data) > math.MaxUint16 {
		d.removePacketLocked(m.packetID)
		m.releaseQueueBytes()
		return nil, false
	}
	for d.bufferedBytes+len(m.data) > d.maxBufferedBytes {
		if !d.removeOldestLocked(m.packetID, true) {
			d.removePacketLocked(m.packetID)
			m.releaseQueueBytes()
			return nil, false
		}
	}
	item.messages[m.fragmentID] = m
	item.count++
	item.bytes += len(m.data)
	d.bufferedBytes += len(m.data)
	if int(item.count) != len(item.messages) {
		return nil, true
	}
	d.detachPacketLocked(m.packetID)
	newMessage := *item.messages[0]
	if item.bytes == 0 {
		return nil, true
	}
	newMessage.data = make([]byte, 0, item.bytes)
	newMessage.queuedBytes = item.bytes
	for _, message := range item.messages {
		newMessage.data = append(newMessage.data, message.data...)
		message.queuedBytes = 0
	}
	return &newMessage, true
}

func (d *udpDefragger) removePacketLocked(packetID uint16) {
	item := d.packetMap[packetID]
	if item == nil {
		return
	}
	for _, message := range item.messages {
		if message != nil {
			message.releaseQueueBytes()
		}
	}
	d.detachPacketLocked(packetID)
}

func (d *udpDefragger) detachPacketLocked(packetID uint16) {
	item := d.packetMap[packetID]
	if item == nil {
		return
	}
	d.bufferedBytes -= item.bytes
	if d.bufferedBytes < 0 {
		d.bufferedBytes = 0
	}
	delete(d.packetMap, packetID)
}

func (d *udpDefragger) removeOldestLocked(excludedPacketID uint16, exclude bool) bool {
	var oldestID uint16
	var oldestExpiry time.Time
	found := false
	for packetID, item := range d.packetMap {
		if exclude && packetID == excludedPacketID {
			continue
		}
		if !found || item.expiresAt.Before(oldestExpiry) {
			oldestID = packetID
			oldestExpiry = item.expiresAt
			found = true
		}
	}
	if found {
		d.removePacketLocked(oldestID)
	}
	return found
}

func (d *udpDefragger) removeExpiredLocked(now time.Time) {
	for packetID, item := range d.packetMap {
		if !now.Before(item.expiresAt) {
			d.removePacketLocked(packetID)
		}
	}
}

func (d *udpDefragger) scheduleExpirationLocked(now time.Time) {
	if d.closed || len(d.packetMap) == 0 {
		if d.timer != nil {
			d.timer.Stop()
			d.timer = nil
		}
		return
	}
	var earliest time.Time
	for _, item := range d.packetMap {
		if earliest.IsZero() || item.expiresAt.Before(earliest) {
			earliest = item.expiresAt
		}
	}
	delay := earliest.Sub(now)
	if delay < 0 {
		delay = 0
	}
	if d.timer == nil {
		d.timer = time.AfterFunc(delay, d.expire)
		return
	}
	d.timer.Reset(delay)
}

func (d *udpDefragger) expire() {
	d.access.Lock()
	defer d.access.Unlock()
	if d.closed {
		return
	}
	now := time.Now()
	d.removeExpiredLocked(now)
	d.scheduleExpirationLocked(now)
}

func (d *udpDefragger) close() {
	d.access.Lock()
	defer d.access.Unlock()
	if d.closed {
		return
	}
	d.closed = true
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
	for packetID := range d.packetMap {
		d.removePacketLocked(packetID)
	}
	d.packetMap = nil
	d.bufferedBytes = 0
}

func (d *udpDefragger) retained() (packets, bytes int) {
	d.access.Lock()
	defer d.access.Unlock()
	return len(d.packetMap), d.bufferedBytes
}

func readUDPMessage(message *udpMessage, reader io.Reader) error {
	return readUDPMessageWithBudget(message, reader, nil)
}

func readUDPMessageWithBudget(message *udpMessage, reader io.Reader, budget *udpQueueBudget) error {
	if err := binary.Read(reader, binary.BigEndian, &message.sessionID); err != nil {
		return err
	}
	if err := binary.Read(reader, binary.BigEndian, &message.packetID); err != nil {
		return err
	}
	if err := binary.Read(reader, binary.BigEndian, &message.fragmentTotal); err != nil {
		return err
	}
	if err := binary.Read(reader, binary.BigEndian, &message.fragmentID); err != nil {
		return err
	}
	var dataLength uint16
	if err := binary.Read(reader, binary.BigEndian, &dataLength); err != nil {
		return err
	}
	destination, err := readDestination(reader, xnet.Network_UDP)
	if err != nil {
		return err
	}
	message.destination = destination
	if !budget.reserve(int(dataLength)) {
		return errInboundUDPQueueFull
	}
	message.queueBudget = budget
	message.queuedBytes = int(dataLength)
	message.data = make([]byte, int(dataLength))
	if _, err = io.ReadFull(reader, message.data); err != nil {
		message.releaseQueueBytes()
		return err
	}
	return nil
}

func decodeUDPMessage(message *udpMessage, data []byte) error {
	reader := bytes.NewReader(data)
	if err := binary.Read(reader, binary.BigEndian, &message.sessionID); err != nil {
		return err
	}
	if err := binary.Read(reader, binary.BigEndian, &message.packetID); err != nil {
		return err
	}
	if err := binary.Read(reader, binary.BigEndian, &message.fragmentTotal); err != nil {
		return err
	}
	if err := binary.Read(reader, binary.BigEndian, &message.fragmentID); err != nil {
		return err
	}
	var dataLength uint16
	if err := binary.Read(reader, binary.BigEndian, &dataLength); err != nil {
		return err
	}
	destination, err := readDestination(reader, xnet.Network_UDP)
	if err != nil {
		return err
	}
	if reader.Len() != int(dataLength) {
		return io.ErrUnexpectedEOF
	}
	message.destination = destination
	message.data = data[len(data)-reader.Len():]
	return nil
}

type packetDeadline struct {
	access   sync.Mutex
	timer    *time.Timer
	notify   chan struct{}
	when     time.Time
	serial   uint64
	signaled bool
	closed   bool
}

func newPacketDeadline() packetDeadline {
	return packetDeadline{notify: make(chan struct{})}
}

func (d *packetDeadline) snapshot() (<-chan struct{}, time.Time, bool) {
	d.access.Lock()
	defer d.access.Unlock()
	return d.notify, d.when, d.closed
}

func (d *packetDeadline) set(t time.Time) {
	d.access.Lock()
	defer d.access.Unlock()
	if d.closed {
		return
	}
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
	d.signalLocked()
	d.serial++
	serial := d.serial
	d.notify = make(chan struct{})
	d.signaled = false
	d.when = t
	if t.IsZero() {
		return
	}
	duration := time.Until(t)
	if duration <= 0 {
		d.signalLocked()
		return
	}
	d.timer = time.AfterFunc(duration, func() {
		d.access.Lock()
		defer d.access.Unlock()
		if d.closed || d.serial != serial {
			return
		}
		d.timer = nil
		d.signalLocked()
	})
}

func (d *packetDeadline) close() {
	d.access.Lock()
	defer d.access.Unlock()
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
	d.closed = true
	d.when = time.Time{}
	d.signalLocked()
}

func (d *packetDeadline) signalLocked() {
	if d.signaled {
		return
	}
	close(d.notify)
	d.signaled = true
}

type destinationAddr struct {
	destination xnet.Destination
}

func (a destinationAddr) Network() string {
	return a.destination.Network.SystemString()
}

func (a destinationAddr) String() string {
	return a.destination.NetAddr()
}

func destinationFromNetAddr(addr stdnet.Addr, network xnet.Network) (xnet.Destination, error) {
	if addr == nil {
		return xnet.Destination{}, os.ErrInvalid
	}
	switch typedAddr := addr.(type) {
	case destinationAddr:
		destination := typedAddr.destination
		destination.Network = network
		return destination, nil
	case *stdnet.UDPAddr:
		return destinationFromAddress(network, xnet.IPAddress(typedAddr.IP), xnet.Port(typedAddr.Port)), nil
	case *stdnet.TCPAddr:
		return destinationFromAddress(network, xnet.IPAddress(typedAddr.IP), xnet.Port(typedAddr.Port)), nil
	default:
		destination, err := xnet.ParseDestination(network.SystemString() + ":" + addr.String())
		if err != nil {
			return xnet.Destination{}, err
		}
		return destination, nil
	}
}
