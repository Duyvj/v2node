package hysteria

import (
	"context"
	"encoding/binary"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apernet/quic-go"
	"github.com/apernet/quic-go/quicvarint"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/transport/internet"
)

type interConn struct {
	stream *quic.Stream
	local  net.Addr
	remote net.Addr

	client bool
	user   *protocol.MemoryUser
}

func (c *interConn) User() *protocol.MemoryUser {
	return c.user
}

func (c *interConn) Read(b []byte) (int, error) {
	return c.stream.Read(b)
}

func (c *interConn) Write(b []byte) (int, error) {
	if c.client {
		c.client = false
		if _, err := c.stream.Write(append(quicvarint.Append(nil, FrameTypeTCPRequest), b...)); err != nil {
			return 0, err
		}
		return len(b), nil
	}

	return c.stream.Write(b)
}

func (c *interConn) Close() error {
	c.stream.CancelRead(0)
	return c.stream.Close()
}

func (c *interConn) LocalAddr() net.Addr {
	return c.local
}

func (c *interConn) RemoteAddr() net.Addr {
	return c.remote
}

func (c *interConn) SetDeadline(t time.Time) error {
	return c.stream.SetDeadline(t)
}

func (c *interConn) SetReadDeadline(t time.Time) error {
	return c.stream.SetReadDeadline(t)
}

func (c *interConn) SetWriteDeadline(t time.Time) error {
	return c.stream.SetWriteDeadline(t)
}

type InterConn struct {
	local  net.Addr
	remote net.Addr

	id     uint32
	queue  *udpDatagramQueue
	time   time.Time
	mutex  sync.Mutex
	closed atomic.Bool

	write func(p []byte) error
	close func()
	// releaseQueued returns one permit to the owning session manager after a
	// datagram leaves the queue, either through Read or close-time draining.
	releaseQueued func()
	user          *protocol.MemoryUser
}

func (i *InterConn) User() *protocol.MemoryUser {
	return i.user
}

func (c *InterConn) Time() time.Time {
	c.mutex.Lock()
	v := c.time
	c.mutex.Unlock()
	return v
}

func (c *InterConn) Update() {
	c.mutex.Lock()
	c.time = time.Now()
	c.mutex.Unlock()
}

func (c *InterConn) Read(p []byte) (int, error) {
	b, ok := c.queue.pop()
	if !ok {
		return 0, io.EOF
	}
	if c.releaseQueued != nil {
		c.releaseQueued()
	}
	if len(p) < len(b) {
		return 0, io.ErrShortBuffer
	}
	c.Update()
	return copy(p, b), nil
}

type udpDatagramQueue struct {
	access sync.Mutex
	data   [][]byte
	notify chan struct{}
	closed bool
}

func newUDPDatagramQueue() *udpDatagramQueue {
	return &udpDatagramQueue{
		data:   make([][]byte, 0, udpMessageQueueInitialSize),
		notify: make(chan struct{}),
	}
}

func (q *udpDatagramQueue) push(datagram []byte) bool {
	q.access.Lock()
	defer q.access.Unlock()
	if q.closed || len(q.data) >= udpMessageChanSize {
		return false
	}
	q.data = append(q.data, datagram)
	close(q.notify)
	q.notify = make(chan struct{})
	return true
}

func (q *udpDatagramQueue) pop() ([]byte, bool) {
	for {
		q.access.Lock()
		if len(q.data) > 0 {
			datagram := q.data[0]
			q.data[0] = nil
			q.data = q.data[1:]
			// Slicing from the front reduces cap to zero by the final pop, but a
			// zero-capacity slice can still keep the burst backing array alive.
			// Always replace the empty slice so a 1024-packet burst is collectible.
			if len(q.data) == 0 {
				q.data = make([][]byte, 0, udpMessageQueueInitialSize)
			}
			q.access.Unlock()
			return datagram, true
		}
		if q.closed {
			q.access.Unlock()
			return nil, false
		}
		notify := q.notify
		q.access.Unlock()
		<-notify
	}
}

func (q *udpDatagramQueue) closeAndDrain() int {
	q.access.Lock()
	defer q.access.Unlock()
	if q.closed {
		return 0
	}
	q.closed = true
	dropped := len(q.data)
	q.data = nil
	close(q.notify)
	return dropped
}

func (q *udpDatagramQueue) len() int {
	q.access.Lock()
	defer q.access.Unlock()
	return len(q.data)
}

func (q *udpDatagramQueue) capacity() int {
	q.access.Lock()
	defer q.access.Unlock()
	return cap(q.data)
}

func (c *InterConn) Write(p []byte) (int, error) {
	if c.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	binary.BigEndian.PutUint32(p, c.id)
	if err := c.write(p); err != nil {
		return 0, err
	}
	c.Update()
	return len(p), nil
}

func (c *InterConn) Close() error {
	c.close()
	return nil
}

func (c *InterConn) LocalAddr() net.Addr {
	return c.local
}

func (c *InterConn) RemoteAddr() net.Addr {
	return c.remote
}

func (c *InterConn) SetDeadline(t time.Time) error {
	return nil
}

func (c *InterConn) SetReadDeadline(t time.Time) error {
	return nil
}

func (c *InterConn) SetWriteDeadline(t time.Time) error {
	return nil
}

type udpSessionManager struct {
	sync.RWMutex

	conn   *quic.Conn
	m      map[uint32]*InterConn
	next   uint32
	closed bool
	queued atomic.Int32

	addConn        internet.ConnHandler
	udpIdleTimeout time.Duration
	user           *protocol.MemoryUser
}

func (m *udpSessionManager) acquireQueueSlot() bool {
	for {
		queued := m.queued.Load()
		if queued >= maxQueuedUDPDatagrams {
			return false
		}
		if m.queued.CompareAndSwap(queued, queued+1) {
			return true
		}
	}
}

func (m *udpSessionManager) releaseQueueSlot() {
	m.queued.Add(-1)
}

// enqueueLocked must be called with at least the manager read lock held. The
// lock prevents closeConnLocked from closing the queue during the non-blocking send.
func (m *udpSessionManager) enqueueLocked(udpConn *InterConn, d []byte) bool {
	if udpConn == nil || udpConn.closed.Load() || !m.acquireQueueSlot() {
		return false
	}
	if udpConn.queue.push(d) {
		return true
	}
	// A full per-session queue must not consume the manager-wide permit.
	m.releaseQueueSlot()
	return false
}

func (m *udpSessionManager) newUDPConnLocked(id uint32) *InterConn {
	udpConn := &InterConn{
		id:            id,
		queue:         newUDPDatagramQueue(),
		time:          time.Now(),
		releaseQueued: m.releaseQueueSlot,
	}
	if m.conn != nil {
		udpConn.local = m.conn.LocalAddr()
		udpConn.remote = m.conn.RemoteAddr()
		udpConn.write = m.conn.SendDatagram
	} else {
		// A nil QUIC connection is useful in lifecycle tests and keeps teardown
		// paths defensive. Production managers always have a QUIC connection.
		udpConn.write = func([]byte) error { return io.ErrClosedPipe }
	}
	udpConn.close = func() {
		m.Lock()
		m.closeConnLocked(udpConn)
		m.Unlock()
	}
	return udpConn
}

// closeConnLocked detaches a session and drops any queued datagrams while the
// manager write lock prevents concurrent senders from touching its queue.
func (m *udpSessionManager) closeConnLocked(udpConn *InterConn) {
	if udpConn == nil || udpConn.closed.Swap(true) {
		return
	}
	current, found := m.m[udpConn.id]
	removed := found && current == udpConn
	if removed {
		delete(m.m, udpConn.id)
	}
	if removed && len(m.m) == 0 && !m.closed {
		// Release buckets after a burst of short-lived sessions.
		m.m = make(map[uint32]*InterConn)
	}
	dropped := udpConn.queue.closeAndDrain()
	if udpConn.releaseQueued != nil {
		for range dropped {
			udpConn.releaseQueued()
		}
	}
}

func (m *udpSessionManager) shutdown() {
	m.Lock()
	if m.closed {
		m.Unlock()
		return
	}
	m.closed = true
	for _, udpConn := range m.m {
		m.closeConnLocked(udpConn)
	}
	m.m = nil
	m.Unlock()
}

func (m *udpSessionManager) clean() {
	ticker := time.NewTicker(idleCleanupInterval)
	defer ticker.Stop()

	for range ticker.C {
		m.RLock()
		if m.closed {
			m.RUnlock()
			return
		}
		now := time.Now()
		timeoutConn := make([]*InterConn, 0, len(m.m))
		for _, udpConn := range m.m {
			if now.Sub(udpConn.Time()) > m.udpIdleTimeout {
				timeoutConn = append(timeoutConn, udpConn)
			}
		}
		m.RUnlock()

		for _, udpConn := range timeoutConn {
			m.Lock()
			if now.Sub(udpConn.Time()) > m.udpIdleTimeout {
				m.closeConnLocked(udpConn)
			}
			m.Unlock()
		}
	}
}

func (m *udpSessionManager) run() {
	defer m.shutdown()
	for {
		d, err := m.conn.ReceiveDatagram(context.Background())
		if err != nil {
			break
		}

		if len(d) < 4 {
			continue
		}
		id := binary.BigEndian.Uint32(d[:4])

		m.feed(id, d)
	}

}

func (m *udpSessionManager) udp() (*InterConn, error) {
	m.Lock()
	defer m.Unlock()

	if m.closed {
		return nil, errors.New("closed")
	}
	if len(m.m) >= maxUDPSessionsPerConn {
		return nil, errors.New("too many UDP sessions")
	}

	id := m.next
	for {
		if _, found := m.m[id]; !found {
			break
		}
		id++
	}

	udpConn := m.newUDPConnLocked(id)
	m.m[id] = udpConn
	m.next = id + 1

	return udpConn, nil
}

func (m *udpSessionManager) feed(id uint32, d []byte) {
	m.RLock()
	if m.closed {
		m.RUnlock()
		return
	}
	udpConn, ok := m.m[id]
	if ok {
		m.enqueueLocked(udpConn, d)
		m.RUnlock()
		return
	}
	m.RUnlock()

	if m.addConn == nil {
		return
	}

	m.Lock()
	if m.closed {
		m.Unlock()
		return
	}
	udpConn, ok = m.m[id]
	created := false
	if !ok {
		if len(m.m) >= maxUDPSessionsPerConn {
			m.Unlock()
			return
		}
		udpConn = m.newUDPConnLocked(id)
		udpConn.user = m.user
		m.m[id] = udpConn
		created = true
	}

	m.enqueueLocked(udpConn, d)
	addConn := m.addConn
	m.Unlock()

	// ConnHandler is external code and may immediately call Close, which takes
	// the manager lock. Invoke it only after releasing that lock.
	if created {
		addConn(udpConn)
	}
}
