package splithttp

import (
	"errors"
	"io"
	"sync"
	"time"
)

const (
	maxSessionIDBytes               = 256
	defaultMaxPendingSessions       = 256
	defaultMaxActiveSessions        = 1024
	defaultPendingSessionTTL        = 30 * time.Second
	defaultRequestBodyReadTimeout   = 30 * time.Second
	defaultMaxSessionRetainedBytes  = 32 << 20
	defaultMaxListenerRetainedBytes = 64 << 20
)

var (
	errSessionStoreClosed = errors.New("XHTTP session store is closed")
	errTooManyPending     = errors.New("too many pending XHTTP sessions")
	errTooManySessions    = errors.New("too many active XHTTP sessions")
	errInvalidSessionID   = errors.New("invalid XHTTP session ID")
	errSessionNotFound    = errors.New("XHTTP session no longer exists")
	errSessionConnected   = errors.New("XHTTP session already has a downlink")
	errSessionByteBudget  = errors.New("XHTTP session retained-byte budget exceeded")
	errListenerByteBudget = errors.New("XHTTP listener retained-byte budget exceeded")
)

type httpSession struct {
	uploadQueue   *uploadQueue
	pending       bool
	expiresAt     time.Time
	retainedBytes int64
	bodies        map[*trackedRequestBody]struct{}
}

type trackedRequestBody struct {
	mu             sync.Mutex
	body           io.ReadCloser
	deadline       time.Time
	deadlineSetter func(time.Time) error
	closing        bool
	finished       bool
	closeOnce      sync.Once
}

func (b *trackedRequestBody) close() {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		if b.finished {
			b.mu.Unlock()
			return
		}
		b.closing = true
		if b.deadlineSetter != nil {
			_ = b.deadlineSetter(time.Now())
		}
		body := b.body
		b.body = nil
		b.deadlineSetter = nil
		b.mu.Unlock()
		if body != nil {
			_ = body.Close()
		}
	})
}

func (b *trackedRequestBody) finish() {
	b.mu.Lock()
	if !b.closing {
		b.finished = true
		b.body = nil
		b.deadlineSetter = nil
	}
	b.mu.Unlock()
}

func (b *trackedRequestBody) armDeadline() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closing || b.finished || b.deadlineSetter == nil {
		return false
	}
	return b.deadlineSetter(b.deadline) == nil
}

func (b *trackedRequestBody) clearDeadline() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.closing && !b.finished && b.deadlineSetter != nil {
		_ = b.deadlineSetter(time.Time{})
	}
}

// byteReservation accounts for memory before an HTTP handler starts reading
// or decoding a packet. Ownership is transferred to Packet and released by
// exactly one of queue consumption, rejection, expiry, or listener close.
type byteReservation struct {
	mu       sync.Mutex
	store    *sessionStore
	session  *httpSession
	bytes    int64
	released bool
}

func (r *byteReservation) shrinkTo(bytes int64) {
	if bytes < 0 {
		bytes = 0
	}
	r.mu.Lock()
	if r.released || bytes >= r.bytes {
		r.mu.Unlock()
		return
	}
	delta := r.bytes - bytes
	r.bytes = bytes
	r.store.mu.Lock()
	r.session.retainedBytes -= delta
	r.store.retainedBytes -= delta
	r.store.mu.Unlock()
	r.mu.Unlock()
}

func (r *byteReservation) release() {
	r.mu.Lock()
	if r.released {
		r.mu.Unlock()
		return
	}
	r.released = true
	bytes := r.bytes
	r.bytes = 0
	r.store.mu.Lock()
	r.session.retainedBytes -= bytes
	r.store.retainedBytes -= bytes
	r.store.mu.Unlock()
	r.mu.Unlock()
}

// sessionStore owns all split HTTP sessions and retained packet memory for one
// listener. A single sweeper replaces per-session and per-request timer
// goroutines.
type sessionStore struct {
	mu                       sync.Mutex
	sessions                 map[string]*httpSession
	pending                  int
	maxPending               int
	maxSessions              int
	ttl                      time.Duration
	bodyReadTimeout          time.Duration
	maxSessionRetainedBytes  int64
	maxListenerRetainedBytes int64
	retainedBytes            int64
	closed                   bool

	stop      chan struct{}
	sweepDone chan struct{}
	closeOnce sync.Once
}

func newSessionStore(maxPending int, ttl time.Duration) *sessionStore {
	if maxPending <= 0 {
		maxPending = defaultMaxPendingSessions
	}
	if ttl <= 0 {
		ttl = defaultPendingSessionTTL
	}
	s := &sessionStore{
		sessions:                 make(map[string]*httpSession),
		maxPending:               maxPending,
		maxSessions:              defaultMaxActiveSessions,
		ttl:                      ttl,
		bodyReadTimeout:          defaultRequestBodyReadTimeout,
		maxSessionRetainedBytes:  defaultMaxSessionRetainedBytes,
		maxListenerRetainedBytes: defaultMaxListenerRetainedBytes,
		stop:                     make(chan struct{}),
		sweepDone:                make(chan struct{}),
	}
	go s.sweep()
	return s
}

func validSessionID(id string) bool {
	return id == "" || len(id) <= maxSessionIDBytes
}

func (s *sessionStore) upsert(id string, maxBufferedPosts int) (*httpSession, error) {
	if !validSessionID(id) || id == "" {
		return nil, errInvalidSessionID
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errSessionStoreClosed
	}
	if current := s.sessions[id]; current != nil {
		return current, nil
	}
	if len(s.sessions) >= s.maxSessions {
		return nil, errTooManySessions
	}
	if s.pending >= s.maxPending {
		return nil, errTooManyPending
	}

	current := &httpSession{
		uploadQueue: NewUploadQueue(maxBufferedPosts),
		pending:     true,
		expiresAt:   time.Now().Add(s.ttl),
		bodies:      make(map[*trackedRequestBody]struct{}),
	}
	s.sessions[id] = current
	s.pending++
	return current, nil
}

// markConnected atomically claims the sole downlink for a session. A second
// GET is rejected instead of sharing ownership or deleting the live session.
func (s *sessionStore) markConnected(id string, expected *httpSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.sessions[id]
	if current == nil || current != expected || s.closed {
		return errSessionNotFound
	}
	if !current.pending {
		return errSessionConnected
	}
	current.pending = false
	current.expiresAt = time.Time{}
	s.pending--
	return nil
}

func (s *sessionStore) reserveBytes(id string, expected *httpSession, bytes int64) (*byteReservation, error) {
	if bytes < 0 {
		return nil, errSessionByteBudget
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.sessions[id] != expected {
		return nil, errSessionNotFound
	}
	if bytes > s.maxSessionRetainedBytes-expected.retainedBytes {
		return nil, errSessionByteBudget
	}
	if bytes > s.maxListenerRetainedBytes-s.retainedBytes {
		return nil, errListenerByteBudget
	}
	expected.retainedBytes += bytes
	s.retainedBytes += bytes
	return &byteReservation{store: s, session: expected, bytes: bytes}, nil
}

// reservePacketBytes installs byte ownership while the store lock still
// protects the accounting update. Listener close can therefore never return
// between charging bytes and attaching their release path to the queue.
func (s *sessionStore) reservePacketBytes(id string, expected *httpSession, packet *packetReservation, bytes int64) (*byteReservation, error) {
	if bytes < 0 {
		return nil, errSessionByteBudget
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.sessions[id] != expected {
		return nil, errSessionNotFound
	}
	if bytes > s.maxSessionRetainedBytes-expected.retainedBytes {
		return nil, errSessionByteBudget
	}
	if bytes > s.maxListenerRetainedBytes-s.retainedBytes {
		return nil, errListenerByteBudget
	}
	reservation := &byteReservation{store: s, session: expected, bytes: bytes}
	expected.retainedBytes += bytes
	s.retainedBytes += bytes
	if !packet.attachBytes(reservation) {
		expected.retainedBytes -= bytes
		s.retainedBytes -= bytes
		return nil, errPacketQueueClosed
	}
	return reservation, nil
}

func (s *sessionStore) registerBody(id string, expected *httpSession, body io.ReadCloser, deadlineSetter ...func(time.Time) error) (*trackedRequestBody, error) {
	tracked := &trackedRequestBody{body: body, deadline: time.Now().Add(s.bodyReadTimeout)}
	if len(deadlineSetter) > 0 {
		tracked.deadlineSetter = deadlineSetter[0]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.sessions[id] != expected {
		return nil, errSessionNotFound
	}
	expected.bodies[tracked] = struct{}{}
	return tracked, nil
}

func (s *sessionStore) unregisterBody(expected *httpSession, tracked *trackedRequestBody) {
	if tracked == nil {
		return
	}
	s.mu.Lock()
	delete(expected.bodies, tracked)
	s.mu.Unlock()
	tracked.finish()
}

func (s *sessionStore) delete(id string, expected *httpSession) {
	var queue *uploadQueue
	var bodies []*trackedRequestBody
	s.mu.Lock()
	if current := s.sessions[id]; current != nil && current == expected {
		delete(s.sessions, id)
		if current.pending {
			s.pending--
		}
		queue = current.uploadQueue
		bodies = takeBodies(current)
	}
	s.mu.Unlock()
	closeBodies(bodies)
	if queue != nil {
		_ = queue.Close()
	}
}

func takeBodies(session *httpSession) []*trackedRequestBody {
	bodies := make([]*trackedRequestBody, 0, len(session.bodies))
	for body := range session.bodies {
		bodies = append(bodies, body)
	}
	session.bodies = nil
	return bodies
}

func closeBodies(bodies []*trackedRequestBody) {
	for _, body := range bodies {
		body.close()
	}
}

func (s *sessionStore) sweep() {
	defer close(s.sweepDone)
	interval := s.ttl / 4
	if bodyInterval := s.bodyReadTimeout / 4; bodyInterval < interval {
		interval = bodyInterval
	}
	if interval > time.Second {
		interval = time.Second
	}
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case now := <-ticker.C:
			s.expire(now)
		case <-s.stop:
			return
		}
	}
}

func (s *sessionStore) expire(now time.Time) {
	var queues []*uploadQueue
	var bodies []*trackedRequestBody
	s.mu.Lock()
	for id, current := range s.sessions {
		if current.pending && !now.Before(current.expiresAt) {
			delete(s.sessions, id)
			s.pending--
			queues = append(queues, current.uploadQueue)
			bodies = append(bodies, takeBodies(current)...)
			continue
		}
		for body := range current.bodies {
			if !now.Before(body.deadline) {
				delete(current.bodies, body)
				bodies = append(bodies, body)
			}
		}
	}
	s.mu.Unlock()
	closeBodies(bodies)
	for _, queue := range queues {
		_ = queue.Close()
	}
}

func (s *sessionStore) Close() error {
	s.closeOnce.Do(func() {
		var queues []*uploadQueue
		var bodies []*trackedRequestBody
		s.mu.Lock()
		s.closed = true
		close(s.stop)
		for _, current := range s.sessions {
			queues = append(queues, current.uploadQueue)
			bodies = append(bodies, takeBodies(current)...)
		}
		s.sessions = nil
		s.pending = 0
		s.mu.Unlock()

		closeBodies(bodies)
		for _, queue := range queues {
			_ = queue.Close()
		}
		<-s.sweepDone
	})
	return nil
}

func (s *sessionStore) counts() (sessions, pending int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions), s.pending
}

func (s *sessionStore) retained() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.retainedBytes
}
