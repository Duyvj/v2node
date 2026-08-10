package tuic

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	stderrors "errors"
	"io"
	stdnet "net"
	"os"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/apernet/quic-go"

	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/hysteria/congestion"
	"github.com/xtls/xray-core/transport/internet/hysteria/congestion/bbr"
)

const (
	// A TUIC connection multiplexes application connections over QUIC. Keeping
	// these limits finite prevents one peer from making quic-go retain an
	// effectively unbounded stream table while still allowing substantial
	// multiplexing for normal clients.
	maxIncomingStreams    int64 = 256
	maxIncomingUniStreams int64 = 256

	maxPendingTasks          = 256
	maxInboundQueuedUDPBytes = 4 * 1024 * 1024

	defaultMaxUDPSessions = 128
	minMaxUDPSessions     = 16
	maxMaxUDPSessions     = 4096
	dissociateQueueSize   = 32
	controlWriteTimeout   = time.Second

	maxUDPSessionsEnv = "V2NODE_TUIC_MAX_UDP_SESSIONS"
)

var (
	errTooManyPendingTasks = stderrors.New("too many pre-authentication commands")
	errTooManyUDPSessions  = stderrors.New("too many TUIC UDP sessions")
	errInboundUDPQueueFull = stderrors.New("TUIC inbound UDP queue byte budget exceeded")
)

type serverOptions struct {
	Context           context.Context
	TLSConfig         *tls.Config
	CongestionControl string
	AuthTimeout       time.Duration
	ZeroRTTHandshake  bool
	UDPTimeout        time.Duration
	Authenticator     Authenticator
	Handler           internet.ConnHandler
	LocalAddr         stdnet.Addr
}

type serverService struct {
	ctx               context.Context
	tlsConfig         *tls.Config
	quicConfig        *quic.Config
	congestionControl string
	authTimeout       time.Duration
	udpTimeout        time.Duration
	authenticator     Authenticator
	handler           internet.ConnHandler
	localAddr         stdnet.Addr
	maxUDPSessions    int
	listener          io.Closer
	tr                *quic.Transport
}

func newServerService(options serverOptions) (*serverService, error) {
	if options.Context == nil {
		options.Context = context.Background()
	}
	if options.AuthTimeout == 0 {
		options.AuthTimeout = 3 * time.Second
	}
	if options.UDPTimeout <= 0 {
		options.UDPTimeout = 60 * time.Second
	}
	switch options.CongestionControl {
	case "":
		options.CongestionControl = "cubic"
	case "cubic", "new_reno", "bbr":
	default:
		return nil, errors.New("unknown congestion control algorithm: ", options.CongestionControl)
	}
	return &serverService{
		ctx:       options.Context,
		tlsConfig: options.TLSConfig,
		quicConfig: &quic.Config{
			DisablePathMTUDiscovery:        !(runtime.GOOS == "windows" || runtime.GOOS == "linux" || runtime.GOOS == "android" || runtime.GOOS == "darwin"),
			EnableDatagrams:                true,
			Allow0RTT:                      options.ZeroRTTHandshake,
			MaxIncomingStreams:             maxIncomingStreams,
			MaxIncomingUniStreams:          maxIncomingUniStreams,
			MaxDatagramFrameSize:           1200,
			AssumePeerMaxDatagramFrameSize: 1200,
			DisablePathManager:             true,
		},
		congestionControl: options.CongestionControl,
		authTimeout:       options.AuthTimeout,
		udpTimeout:        options.UDPTimeout,
		authenticator:     options.Authenticator,
		handler:           options.Handler,
		localAddr:         options.LocalAddr,
		maxUDPSessions:    configuredMaxUDPSessions(os.Getenv(maxUDPSessionsEnv)),
	}, nil
}

func configuredMaxUDPSessions(value string) int {
	limit, err := strconv.Atoi(value)
	if err != nil || value == "" {
		return defaultMaxUDPSessions
	}
	if limit < minMaxUDPSessions {
		return minMaxUDPSessions
	}
	if limit > maxMaxUDPSessions {
		return maxMaxUDPSessions
	}
	return limit
}

func (s *serverService) Start(conn stdnet.PacketConn) error {
	s.tr = &quic.Transport{Conn: conn}
	var listener interface {
		Accept(context.Context) (*quic.Conn, error)
		Close() error
	}
	var err error
	if s.quicConfig.Allow0RTT {
		listener, err = s.tr.ListenEarly(s.tlsConfig, s.quicConfig)
	} else {
		listener, err = s.tr.Listen(s.tlsConfig, s.quicConfig)
	}
	if err != nil {
		return err
	}
	s.listener = listener
	go s.acceptLoop(listener)
	return nil
}

func (s *serverService) CloseWithError() error {
	var errs []error
	if s.listener != nil {
		errs = append(errs, s.listener.Close())
	}
	if s.tr != nil {
		errs = append(errs, s.tr.Close())
	}
	return stderrors.Join(errs...)
}

func (s *serverService) acceptLoop(listener interface {
	Accept(context.Context) (*quic.Conn, error)
}) {
	for {
		conn, err := listener.Accept(s.ctx)
		if err != nil {
			if !stderrors.Is(err, quic.ErrServerClosed) && !stderrors.Is(err, context.Canceled) {
				errors.LogWarning(s.ctx, "TUIC accept error: ", err)
			}
			return
		}
		errors.LogDebug(s.ctx, "TUIC accepted QUIC connection from ", conn.RemoteAddr())
		go s.handleConnection(conn)
	}
}

func (s *serverService) handleConnection(conn *quic.Conn) {
	s.setCongestion(conn)
	sessionCtx, cancel, stopServiceWatch := newServerSessionContext(s.ctx, conn.Context())
	session := &serverSession{
		serverService:    s,
		ctx:              sessionCtx,
		cancel:           cancel,
		stopServiceWatch: stopServiceWatch,
		quicConn:         conn,
		udpTransport:     newQUICUDPPacketTransport(conn),
		connDone:         make(chan struct{}),
		authDone:         make(chan struct{}),
		udpConnMap:       make(map[uint16]*udpPacketConn),
		inboundUDPBudget: newUDPQueueBudget(maxInboundQueuedUDPBytes),
		dissociateQueue:  make(chan uint16, dissociateQueueSize),
	}
	errors.LogDebug(s.ctx, "TUIC starting session for ", conn.RemoteAddr())
	session.handle()
}

func newServerSessionContext(serviceCtx, connectionCtx context.Context) (context.Context, context.CancelCauseFunc, func() bool) {
	ctx, cancel := context.WithCancelCause(connectionCtx)
	stopServiceWatch := context.AfterFunc(serviceCtx, func() {
		cancel(context.Cause(serviceCtx))
	})
	return ctx, cancel, stopServiceWatch
}

func (s *serverService) setCongestion(conn *quic.Conn) {
	if s.congestionControl != "bbr" {
		return
	}
	congestion.UseBBR(conn, bbr.ProfileStandard)
}

type serverSession struct {
	*serverService
	ctx               context.Context
	cancel            context.CancelCauseFunc
	stopServiceWatch  func() bool
	quicConn          *quic.Conn
	udpTransport      udpPacketTransport
	closeOnce         sync.Once
	connDone          chan struct{}
	connErr           error
	workers           sync.WaitGroup
	authAccess        sync.Mutex
	authDone          chan struct{}
	authUser          *protocol.MemoryUser
	udpAccess         sync.RWMutex
	udpConnMap        map[uint16]*udpPacketConn
	inboundUDPBudget  *udpQueueBudget
	pendingAccess     sync.Mutex
	pendingTasks      []pendingTask
	dissociateQueue   chan uint16
	openControlStream func(context.Context) (udpSendStream, error)
}

type pendingTask struct {
	run     func() error
	discard func()
}

func (t pendingTask) discardTask() {
	if t.discard != nil {
		t.discard()
	}
}

func (s *serverSession) handle() {
	s.goWorker(s.loopUniStreams)
	s.goWorker(s.loopStreams)
	s.goWorker(s.loopMessages)
	s.goWorker(s.loopDissociate)
	s.goWorker(s.handleAuthTimeout)
	<-s.ctx.Done()
	s.closeWithError(context.Cause(s.ctx))
	s.workers.Wait()
}

func (s *serverSession) goWorker(worker func()) {
	if worker == nil {
		return
	}
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		worker()
	}()
}

func (s *serverSession) loopUniStreams() {
	for {
		stream, err := s.quicConn.AcceptUniStream(s.ctx)
		if err != nil {
			return
		}
		s.goWorker(func() {
			if err := s.handleUniStream(stream); err != nil {
				s.closeWithError(errors.New("handle uni stream").Base(err))
			}
		})
	}
}

func (s *serverSession) handleUniStream(stream *quic.ReceiveStream) error {
	releaseStream := true
	defer func() {
		if releaseStream {
			stream.CancelRead(0)
		}
	}()
	buffer := make([]byte, 2)
	if _, err := io.ReadFull(stream, buffer); err != nil {
		return err
	}
	if buffer[0] != tuicVersion {
		return errors.New("unknown version ", buffer[0])
	}
	switch buffer[1] {
	case commandAuthenticate:
		authPayload := make([]byte, authenticateLen-2)
		if _, err := io.ReadFull(stream, authPayload); err != nil {
			return err
		}
		var userUUID [16]byte
		copy(userUUID[:], authPayload[:16])
		if s.authenticator == nil {
			return errors.New("missing TUIC authenticator")
		}
		user, ok := s.authenticator.Authenticate(s.ctx, userUUID, authPayload[16:48], s.quicConn.ConnectionState().TLS)
		if !ok {
			return errors.New("token mismatch")
		}
		s.authAccess.Lock()
		defer s.authAccess.Unlock()
		select {
		case <-s.authDone:
			return errors.New("multiple authentication requests")
		default:
		}
		s.authUser = user
		close(s.authDone)
		errors.LogDebug(s.ctx, "TUIC authentication succeeded for ", s.quicConn.RemoteAddr(), " user: ", user.Email)
		s.goWorker(s.resumePendingTasks)
		return nil
	case commandPacket, commandDissociate:
		if s.authReady() {
			return s.handlePendingUniStream(stream, buffer)
		}
		errors.LogDebug(s.ctx, "TUIC queued pre-auth uni-stream command ", buffer[1], " from ", s.quicConn.RemoteAddr())
		err := s.enqueuePendingTask(pendingTask{
			run: func() error {
				return s.handlePendingUniStream(stream, buffer)
			},
			discard: func() {
				stream.CancelRead(0)
			},
		})
		if err == nil {
			releaseStream = false
		}
		return err
	default:
		return errors.New("unknown command ", buffer[1])
	}
}

func (s *serverSession) handlePendingUniStream(stream *quic.ReceiveStream, header []byte) error {
	defer stream.CancelRead(0)
	if err := s.waitAuth(); err != nil {
		return err
	}
	switch header[1] {
	case commandPacket:
		message := new(udpMessage)
		if err := readUDPMessageWithBudget(message, stream, s.inboundUDPBudget); err != nil {
			if stderrors.Is(err, errInboundUDPQueueFull) {
				return nil
			}
			return err
		}
		errors.LogDebug(s.ctx, "TUIC processed UDP relay packet from uni-stream session=", message.sessionID, " size=", len(message.data), " dest=", message.destination)
		s.handleUDPMessage(message, true)
		return nil
	case commandDissociate:
		var sessionID uint16
		if err := binary.Read(stream, binary.BigEndian, &sessionID); err != nil {
			return err
		}
		s.udpAccess.Lock()
		udpConn := s.udpConnMap[sessionID]
		if udpConn != nil {
			delete(s.udpConnMap, sessionID)
		}
		s.udpAccess.Unlock()
		if udpConn != nil {
			errors.LogDebug(s.ctx, "TUIC dissociating UDP session ", sessionID, " from ", s.quicConn.RemoteAddr())
			udpConn.closeWithError(io.ErrClosedPipe)
		}
		return nil
	default:
		return errors.New("unknown command ", header[1])
	}
}

func (s *serverSession) handleAuthTimeout() {
	timer := time.NewTimer(s.authTimeout)
	defer timer.Stop()
	select {
	case <-s.connDone:
	case <-s.authDone:
	case <-timer.C:
		s.closeWithError(errors.New("authentication timeout"))
	}
}

func (s *serverSession) loopStreams() {
	for {
		stream, err := s.quicConn.AcceptStream(s.ctx)
		if err != nil {
			return
		}
		s.goWorker(func() {
			if err := s.handleStream(stream); err != nil {
				stream.CancelRead(0)
				_ = stream.Close()
				errors.LogWarning(s.ctx, "TUIC stream error: ", err)
			}
		})
	}
}

func (s *serverSession) handleStream(stream *quic.Stream) error {
	if s.authReady() {
		return s.handlePendingStream(stream)
	}
	errors.LogDebug(s.ctx, "TUIC queued pre-auth connect stream from ", s.quicConn.RemoteAddr())
	return s.enqueuePendingTask(pendingTask{
		run: func() error {
			return s.handlePendingStream(stream)
		},
		discard: func() {
			stream.CancelRead(0)
			stream.CancelWrite(0)
			_ = stream.Close()
		},
	})
}

func (s *serverSession) handlePendingStream(stream *quic.Stream) error {
	if err := s.waitAuth(); err != nil {
		return err
	}
	conn := &streamConn{
		Stream: stream,
		local:  s.localAddr,
		remote: s.quicConn.RemoteAddr(),
		user:   s.authUser,
	}
	errors.LogDebug(s.ctx, "TUIC accepting TCP relay stream from ", s.quicConn.RemoteAddr(), " user=", s.authUser.Email)
	s.handler(conn)
	return nil
}

func (s *serverSession) loopMessages() {
	for {
		data, err := s.quicConn.ReceiveDatagram(s.ctx)
		if err != nil {
			s.closeWithError(err)
			return
		}
		if err := s.handleMessage(data); err != nil {
			s.closeWithError(err)
			return
		}
	}
}

func (s *serverSession) handleMessage(data []byte) error {
	if len(data) < 2 {
		return errors.New("invalid message")
	}
	if data[0] != tuicVersion {
		return errors.New("unknown version ", data[0])
	}
	switch data[1] {
	case commandPacket:
		if !s.authReady() {
			errors.LogDebug(s.ctx, "TUIC queued pre-auth datagram from ", s.quicConn.RemoteAddr())
			return s.enqueuePendingTask(pendingTask{
				run: func() error {
					return s.handleMessage(data)
				},
			})
		}
		message := new(udpMessage)
		if err := decodeUDPMessage(message, data[2:]); err != nil {
			return err
		}
		if !s.reserveInboundUDPMessage(message) {
			return nil
		}
		errors.LogDebug(s.ctx, "TUIC processed UDP relay packet from datagram session=", message.sessionID, " size=", len(message.data), " dest=", message.destination)
		s.handleUDPMessage(message, false)
		return nil
	case commandHeartbeat:
		return nil
	default:
		return errors.New("unknown command ", data[1])
	}
}

func (s *serverSession) handleUDPMessage(message *udpMessage, udpStream bool) {
	select {
	case <-s.ctx.Done():
		message.releaseQueueBytes()
		return
	default:
	}
	s.udpAccess.RLock()
	udpConn := s.udpConnMap[message.sessionID]
	s.udpAccess.RUnlock()
	if udpConn == nil || udpConn.done() {
		errors.LogDebug(s.ctx, "TUIC creating UDP relay session ", message.sessionID, " from ", s.quicConn.RemoteAddr(), " viaStream=", udpStream)
		sessionID := message.sessionID
		newUDPConn := s.newUDPConn(sessionID, udpStream)
		var installed, limitReached bool
		udpConn, installed, limitReached = s.installUDPConn(sessionID, newUDPConn)
		if udpConn == nil {
			message.releaseQueueBytes()
			if limitReached {
				s.rejectUDPSession(sessionID, newUDPConn)
				errors.LogWarning(s.ctx, "TUIC UDP session limit reached for ", s.quicConn.RemoteAddr())
			} else {
				newUDPConn.closeWithError(context.Cause(s.ctx))
			}
			return
		}
		if !installed {
			newUDPConn.closeWithError(io.ErrClosedPipe)
		}
	}
	if !udpConn.inputPacket(message) {
		return
	}
	destination := message.destination
	shouldStart := udpConn.markStarted(destination)
	if !shouldStart {
		errors.LogDebug(s.ctx, "TUIC relaying UDP packet for existing session ", message.sessionID)
		return
	}
	errors.LogDebug(s.ctx, "TUIC started UDP relay session ", message.sessionID, " for ", message.destination)
	s.goWorker(func() {
		s.handler(udpConn)
	})
}

func (s *serverSession) reserveInboundUDPMessage(message *udpMessage) bool {
	if message == nil {
		return false
	}
	budget := s.inboundUDPBudget
	if budget == nil || !budget.reserve(len(message.data)) {
		return false
	}
	message.queueBudget = budget
	message.queuedBytes = len(message.data)
	return true
}

func (s *serverSession) newUDPConn(sessionID uint16, udpStream bool) *udpPacketConn {
	var newUDPConn *udpPacketConn
	newUDPConn = newUDPPacketConnWithTransport(s.ctx, s.udpTransport, udpStream, true, s.authUser, s.udpTimeout, func() {
		s.udpAccess.Lock()
		if s.udpConnMap[sessionID] == newUDPConn {
			delete(s.udpConnMap, sessionID)
		}
		s.udpAccess.Unlock()
	})
	newUDPConn.sessionID = sessionID
	return newUDPConn
}

func (s *serverSession) installUDPConn(sessionID uint16, newUDPConn *udpPacketConn) (udpConn *udpPacketConn, installed, limitReached bool) {
	if newUDPConn == nil {
		return nil, false, false
	}
	s.udpAccess.Lock()
	defer s.udpAccess.Unlock()
	select {
	case <-s.ctx.Done():
		return nil, false, false
	default:
	}
	if newUDPConn.done() {
		return nil, false, false
	}
	udpConn = s.udpConnMap[sessionID]
	if udpConn != nil && !udpConn.done() {
		return udpConn, false, false
	}
	if udpConn != nil {
		delete(s.udpConnMap, sessionID)
	}
	limit := defaultMaxUDPSessions
	if s.serverService != nil && s.serverService.maxUDPSessions > 0 {
		limit = s.serverService.maxUDPSessions
	}
	if len(s.udpConnMap) >= limit {
		return nil, false, true
	}
	s.udpConnMap[sessionID] = newUDPConn
	return newUDPConn, true, false
}

func (s *serverSession) rejectUDPSession(sessionID uint16, udpConn *udpPacketConn) {
	if udpConn != nil {
		udpConn.closeWithError(errTooManyUDPSessions)
	}
	s.notifyUDPSessionLimit(sessionID)
}

func (s *serverSession) notifyUDPSessionLimit(sessionID uint16) {
	if s.dissociateQueue == nil {
		return
	}
	select {
	case <-s.ctx.Done():
	case s.dissociateQueue <- sessionID:
	default:
	}
}

func (s *serverSession) loopDissociate() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case sessionID := <-s.dissociateQueue:
			ctx, cancel := context.WithTimeout(s.ctx, controlWriteTimeout)
			err := writeDissociate(ctx, s.openControlUniStream, sessionID)
			cancel()
			if err != nil && !stderrors.Is(err, context.Canceled) {
				errors.LogDebug(s.ctx, "TUIC failed to notify peer about rejected UDP session ", sessionID, ": ", err)
			}
		}
	}
}

func (s *serverSession) openControlUniStream(ctx context.Context) (udpSendStream, error) {
	if s.openControlStream != nil {
		return s.openControlStream(ctx)
	}
	if s.quicConn == nil {
		return nil, io.ErrClosedPipe
	}
	return s.quicConn.OpenUniStreamSync(ctx)
}

func writeDissociate(ctx context.Context, open func(context.Context) (udpSendStream, error), sessionID uint16) error {
	if open == nil {
		return io.ErrClosedPipe
	}
	stream, err := open(ctx)
	if err != nil {
		return err
	}
	deadline, hasDeadline := ctx.Deadline()
	if hasDeadline {
		_ = stream.SetWriteDeadline(deadline)
	}
	stopCancel := context.AfterFunc(ctx, func() {
		stream.CancelWrite(0)
	})
	defer stopCancel()

	buffer := bytes.NewBuffer(make([]byte, 0, 4))
	buffer.WriteByte(tuicVersion)
	buffer.WriteByte(commandDissociate)
	_ = binary.Write(buffer, binary.BigEndian, sessionID)
	if _, err = stream.Write(buffer.Bytes()); err != nil {
		stream.CancelWrite(0)
		return err
	}
	return stream.Close()
}

func (s *serverSession) authReady() bool {
	select {
	case <-s.authDone:
		return true
	default:
		return false
	}
}

func (s *serverSession) enqueuePendingTask(task pendingTask) error {
	if task.run == nil {
		task.discardTask()
		return nil
	}
	s.pendingAccess.Lock()
	select {
	case <-s.connDone:
		s.pendingAccess.Unlock()
		task.discardTask()
		return s.connectionError()
	default:
	}
	if s.authReady() {
		s.pendingAccess.Unlock()
		return task.run()
	}
	if len(s.pendingTasks) >= maxPendingTasks {
		s.pendingAccess.Unlock()
		task.discardTask()
		return errTooManyPendingTasks
	}
	s.pendingTasks = append(s.pendingTasks, task)
	s.pendingAccess.Unlock()
	return nil
}

func (s *serverSession) resumePendingTasks() {
	s.pendingAccess.Lock()
	tasks := s.pendingTasks
	s.pendingTasks = nil
	s.pendingAccess.Unlock()
	for index, task := range tasks {
		select {
		case <-s.connDone:
			discardPendingTasks(tasks[index:])
			return
		default:
		}
		if task.run == nil {
			task.discardTask()
			continue
		}
		if err := task.run(); err != nil {
			discardPendingTasks(tasks[index+1:])
			s.closeWithError(errors.New("resume pending task").Base(err))
			return
		}
	}
}

func discardPendingTasks(tasks []pendingTask) {
	for _, task := range tasks {
		task.discardTask()
	}
}

func (s *serverSession) waitAuth() error {
	select {
	case <-s.connDone:
		return s.connectionError()
	default:
	}
	select {
	case <-s.connDone:
		return s.connectionError()
	case <-s.authDone:
		return nil
	}
}

func (s *serverSession) closeWithError(err error) {
	s.closeOnce.Do(func() {
		if err == nil {
			err = context.Canceled
		}
		s.connErr = err
		close(s.connDone)
		if s.cancel != nil {
			s.cancel(err)
		}
		if s.stopServiceWatch != nil {
			s.stopServiceWatch()
		}

		s.pendingAccess.Lock()
		pendingTasks := s.pendingTasks
		s.pendingTasks = nil
		s.pendingAccess.Unlock()
		discardPendingTasks(pendingTasks)

		s.udpAccess.Lock()
		udpConns := make([]*udpPacketConn, 0, len(s.udpConnMap))
		for _, udpConn := range s.udpConnMap {
			udpConns = append(udpConns, udpConn)
		}
		s.udpConnMap = nil
		s.udpAccess.Unlock()
		for _, udpConn := range udpConns {
			udpConn.closeWithError(err)
		}

		if err != nil && !stderrors.Is(err, context.Canceled) && !stderrors.Is(err, quic.ErrServerClosed) {
			errors.LogWarning(s.ctx, "TUIC connection closed: ", err)
		} else {
			errors.LogDebug(s.ctx, "TUIC connection closed: ", err)
		}
		if s.quicConn != nil {
			_ = s.quicConn.CloseWithError(0, "")
		}
	})
}

func (s *serverSession) connectionError() error {
	if s.connErr != nil {
		return s.connErr
	}
	return io.ErrClosedPipe
}

type streamConn struct {
	*quic.Stream
	local  stdnet.Addr
	remote stdnet.Addr
	user   *protocol.MemoryUser
}

func (c *streamConn) User() *protocol.MemoryUser {
	return c.user
}

func (c *streamConn) LocalAddr() stdnet.Addr {
	return c.local
}

func (c *streamConn) RemoteAddr() stdnet.Addr {
	return c.remote
}

func (c *streamConn) Close() error {
	c.Stream.CancelRead(0)
	return c.Stream.Close()
}
