package tuic

import (
	"bytes"
	"context"
	"encoding/binary"
	stderrors "errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apernet/quic-go"
)

type recordingSendStream struct {
	access sync.Mutex
	data   []byte
	wrote  chan struct{}
	once   sync.Once
}

func (s *recordingSendStream) Write(data []byte) (int, error) {
	s.access.Lock()
	s.data = append(s.data, data...)
	s.access.Unlock()
	s.once.Do(func() { close(s.wrote) })
	return len(data), nil
}

func (*recordingSendStream) Close() error { return nil }

func (*recordingSendStream) CancelWrite(quic.StreamErrorCode) {}

func (*recordingSendStream) SetWriteDeadline(time.Time) error { return nil }

func (s *recordingSendStream) bytes() []byte {
	s.access.Lock()
	defer s.access.Unlock()
	return append([]byte(nil), s.data...)
}

func newTestServerSession() *serverSession {
	ctx, cancel := context.WithCancelCause(context.Background())
	return &serverSession{
		serverService: &serverService{
			ctx:            ctx,
			udpTimeout:     time.Hour,
			maxUDPSessions: defaultMaxUDPSessions,
		},
		ctx:              ctx,
		cancel:           cancel,
		connDone:         make(chan struct{}),
		authDone:         make(chan struct{}),
		udpConnMap:       make(map[uint16]*udpPacketConn),
		inboundUDPBudget: newUDPQueueBudget(maxInboundQueuedUDPBytes),
		dissociateQueue:  make(chan uint16, dissociateQueueSize),
	}
}

func TestServerServiceUsesBoundedQUICLimits(t *testing.T) {
	t.Setenv(maxUDPSessionsEnv, "")
	service, err := newServerService(serverOptions{Context: context.Background()})
	if err != nil {
		t.Fatal(err)
	}
	if got := service.quicConfig.MaxIncomingStreams; got != maxIncomingStreams || got >= 1<<20 {
		t.Fatalf("unexpected incoming stream limit: %d", got)
	}
	if got := service.quicConfig.MaxIncomingUniStreams; got != maxIncomingUniStreams || got >= 1<<20 {
		t.Fatalf("unexpected incoming unidirectional stream limit: %d", got)
	}
	if got := service.maxUDPSessions; got != defaultMaxUDPSessions {
		t.Fatalf("default UDP session limit = %d, want %d", got, defaultMaxUDPSessions)
	}
}

func TestConfiguredMaxUDPSessionsOverrideAndClamp(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  int
	}{
		{name: "empty", value: "", want: defaultMaxUDPSessions},
		{name: "invalid", value: "not-a-number", want: defaultMaxUDPSessions},
		{name: "override", value: "512", want: 512},
		{name: "minimum", value: "1", want: minMaxUDPSessions},
		{name: "maximum", value: "100000", want: maxMaxUDPSessions},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(maxUDPSessionsEnv, test.value)
			service, err := newServerService(serverOptions{Context: context.Background()})
			if err != nil {
				t.Fatal(err)
			}
			if got := service.maxUDPSessions; got != test.want {
				t.Fatalf("UDP session limit = %d, want %d", got, test.want)
			}
		})
	}
}

func TestServerSessionContextFollowsConnectionAndService(t *testing.T) {
	t.Run("connection", func(t *testing.T) {
		serviceCtx, cancelService := context.WithCancelCause(context.Background())
		connectionCtx, cancelConnection := context.WithCancelCause(context.Background())
		ctx, cancel, stopServiceWatch := newServerSessionContext(serviceCtx, connectionCtx)
		defer cancel(context.Canceled)
		defer cancelService(context.Canceled)
		defer stopServiceWatch()

		cause := stderrors.New("connection ended")
		cancelConnection(cause)
		waitForContext(t, ctx)
		if !stderrors.Is(context.Cause(ctx), cause) {
			t.Fatalf("session cause = %v, want %v", context.Cause(ctx), cause)
		}
	})

	t.Run("service", func(t *testing.T) {
		serviceCtx, cancelService := context.WithCancelCause(context.Background())
		connectionCtx, cancelConnection := context.WithCancelCause(context.Background())
		ctx, cancel, stopServiceWatch := newServerSessionContext(serviceCtx, connectionCtx)
		defer cancel(context.Canceled)
		defer cancelConnection(context.Canceled)
		defer stopServiceWatch()

		cause := stderrors.New("service ended")
		cancelService(cause)
		waitForContext(t, ctx)
		if !stderrors.Is(context.Cause(ctx), cause) {
			t.Fatalf("session cause = %v, want %v", context.Cause(ctx), cause)
		}
	})
}

func TestPendingTaskQueueIsBoundedAndReleasedOnClose(t *testing.T) {
	session := newTestServerSession()
	var ran atomic.Int32
	var discarded atomic.Int32

	for index := 0; index < maxPendingTasks+100; index++ {
		err := session.enqueuePendingTask(pendingTask{
			run: func() error {
				ran.Add(1)
				return nil
			},
			discard: func() {
				discarded.Add(1)
			},
		})
		if index < maxPendingTasks && err != nil {
			t.Fatalf("task %d rejected before limit: %v", index, err)
		}
		if index >= maxPendingTasks && !stderrors.Is(err, errTooManyPendingTasks) {
			t.Fatalf("task %d error = %v, want queue limit error", index, err)
		}
	}

	if got := len(session.pendingTasks); got != maxPendingTasks {
		t.Fatalf("retained pending tasks = %d, want %d", got, maxPendingTasks)
	}
	session.closeWithError(io.ErrClosedPipe)
	if got := len(session.pendingTasks); got != 0 {
		t.Fatalf("retained pending tasks after close = %d", got)
	}
	if got := discarded.Load(); got != maxPendingTasks+100 {
		t.Fatalf("discarded tasks = %d, want %d", got, maxPendingTasks+100)
	}
	if got := ran.Load(); got != 0 {
		t.Fatalf("ran %d task(s) before authentication", got)
	}
}

func TestPendingTaskQueuedDuringAuthenticationRunsImmediately(t *testing.T) {
	session := newTestServerSession()
	close(session.authDone)
	var ran atomic.Int32
	if err := session.enqueuePendingTask(pendingTask{run: func() error {
		ran.Add(1)
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	if got := ran.Load(); got != 1 {
		t.Fatalf("ran task %d times, want once", got)
	}
	if got := len(session.pendingTasks); got != 0 {
		t.Fatalf("retained %d authenticated task(s)", got)
	}
	session.closeWithError(context.Canceled)
}

func TestInboundUDPByteBudgetIsSharedAcrossSessions(t *testing.T) {
	session := newTestServerSession()
	session.inboundUDPBudget = newUDPQueueBudget(10)
	first := session.newUDPConn(1, false)
	second := session.newUDPConn(2, false)

	firstMessage := &udpMessage{data: make([]byte, 6)}
	if !session.reserveInboundUDPMessage(firstMessage) || !first.inputPacket(firstMessage) {
		t.Fatal("first session failed to queue within aggregate budget")
	}
	secondMessage := &udpMessage{data: make([]byte, 5)}
	activity := second.lastActivity.Load()
	time.Sleep(time.Millisecond)
	if session.reserveInboundUDPMessage(secondMessage) {
		t.Fatal("second session exceeded aggregate byte budget")
	}
	if got := second.lastActivity.Load(); got != activity {
		t.Fatal("budget-rejected packet refreshed UDP idle lifetime")
	}

	first.closeWithError(io.ErrClosedPipe)
	if got := session.inboundUDPBudget.used.Load(); got != 0 {
		t.Fatalf("dissociated session retained %d reserved bytes", got)
	}
	if !session.reserveInboundUDPMessage(secondMessage) || !second.inputPacket(secondMessage) {
		t.Fatal("released aggregate budget was not reusable")
	}
	second.closeWithError(errUDPIdleTimeout)
	if got := session.inboundUDPBudget.used.Load(); got != 0 {
		t.Fatalf("timed-out session retained %d reserved bytes", got)
	}
	session.closeWithError(context.Canceled)
}

func TestUniStreamPayloadReservesBeforeAllocation(t *testing.T) {
	original := &udpMessage{
		sessionID:     1,
		packetID:      2,
		fragmentTotal: 1,
		destination:   testUDPDestination(),
		data:          make([]byte, 32),
	}
	packed, err := original.pack()
	if err != nil {
		t.Fatal(err)
	}
	budget := newUDPQueueBudget(16)
	message := new(udpMessage)
	if err := readUDPMessageWithBudget(message, bytes.NewReader(packed[2:]), budget); !stderrors.Is(err, errInboundUDPQueueFull) {
		t.Fatalf("uni-stream queue error = %v, want byte-budget rejection", err)
	}
	if message.data != nil {
		t.Fatal("uni-stream payload allocated before byte reservation")
	}
	if got := budget.used.Load(); got != 0 {
		t.Fatalf("rejected uni-stream retained %d reserved bytes", got)
	}
}

func TestUDPSessionMapIsBoundedAndReleasedOnClose(t *testing.T) {
	session := newTestServerSession()
	var destroyed atomic.Int32
	connections := make([]*udpPacketConn, 0, defaultMaxUDPSessions+1)

	for index := 0; index < defaultMaxUDPSessions+1; index++ {
		conn := newUDPPacketConn(session.ctx, nil, false, true, nil, time.Hour, func() {
			destroyed.Add(1)
		})
		conn.sessionID = uint16(index)
		connections = append(connections, conn)
		selected, installed, limitReached := session.installUDPConn(conn.sessionID, conn)
		if index < defaultMaxUDPSessions {
			if selected != conn || !installed || limitReached {
				t.Fatalf("session %d was not installed", index)
			}
			continue
		}
		if selected != nil || installed || !limitReached {
			t.Fatalf("session beyond limit was accepted: selected=%p installed=%v limit=%v", selected, installed, limitReached)
		}
		conn.closeWithError(errTooManyUDPSessions)
	}

	if got := len(session.udpConnMap); got != defaultMaxUDPSessions {
		t.Fatalf("retained UDP sessions = %d, want %d", got, defaultMaxUDPSessions)
	}
	session.closeWithError(io.ErrClosedPipe)
	if session.udpConnMap != nil {
		t.Fatalf("UDP session map was not released")
	}
	if got := destroyed.Load(); got != defaultMaxUDPSessions+1 {
		t.Fatalf("destroyed UDP sessions = %d, want %d", got, defaultMaxUDPSessions+1)
	}
	for index, conn := range connections {
		if !conn.done() {
			t.Fatalf("UDP session %d remains open", index)
		}
	}
}

func TestClosingStaleUDPSessionDoesNotDeleteReplacement(t *testing.T) {
	session := newTestServerSession()
	const sessionID = 9

	var oldConn *udpPacketConn
	oldConn = newUDPPacketConn(session.ctx, nil, false, true, nil, time.Hour, func() {
		session.udpAccess.Lock()
		if session.udpConnMap[sessionID] == oldConn {
			delete(session.udpConnMap, sessionID)
		}
		session.udpAccess.Unlock()
	})
	oldConn.sessionID = sessionID
	if selected, installed, _ := session.installUDPConn(sessionID, oldConn); selected != oldConn || !installed {
		t.Fatal("old UDP session was not installed")
	}

	// Model the narrow interval where cancellation is visible before the old
	// connection's destroy callback removes it from the session map.
	oldConn.cancel(io.ErrClosedPipe)
	newConn := newUDPPacketConn(session.ctx, nil, false, true, nil, time.Hour, nil)
	newConn.sessionID = sessionID
	if selected, installed, _ := session.installUDPConn(sessionID, newConn); selected != newConn || !installed {
		t.Fatal("replacement UDP session was not installed")
	}
	oldConn.closeWithError(io.ErrClosedPipe)

	session.udpAccess.RLock()
	retained := session.udpConnMap[sessionID]
	session.udpAccess.RUnlock()
	if retained != newConn {
		t.Fatal("stale destroy callback removed the replacement UDP session")
	}
	session.closeWithError(context.Canceled)
}

func TestUDPDestroyCallbackCapturesSessionIDValue(t *testing.T) {
	session := newTestServerSession()
	message := &udpMessage{sessionID: 41}
	conn := session.newUDPConn(message.sessionID, false)
	if selected, installed, _ := session.installUDPConn(message.sessionID, conn); selected != conn || !installed {
		t.Fatal("UDP session was not installed")
	}

	message.sessionID = 42
	conn.closeWithError(io.ErrClosedPipe)
	session.udpAccess.RLock()
	_, retainedOriginal := session.udpConnMap[41]
	session.udpAccess.RUnlock()
	if retainedOriginal {
		t.Fatal("destroy callback retained the original session after the source message changed")
	}
	session.closeWithError(context.Canceled)
}

func TestUDPSessionCapSendsDissociateNotification(t *testing.T) {
	session := newTestServerSession()
	session.maxUDPSessions = minMaxUDPSessions
	for index := 0; index < minMaxUDPSessions; index++ {
		conn := session.newUDPConn(uint16(index), false)
		if selected, installed, _ := session.installUDPConn(uint16(index), conn); selected != conn || !installed {
			t.Fatalf("failed to install UDP session %d", index)
		}
	}

	recording := &recordingSendStream{wrote: make(chan struct{})}
	session.openControlStream = func(context.Context) (udpSendStream, error) {
		return recording, nil
	}
	session.goWorker(session.loopDissociate)

	const rejectedID uint16 = 60000
	rejected := session.newUDPConn(rejectedID, false)
	selected, installed, limitReached := session.installUDPConn(rejectedID, rejected)
	if selected != nil || installed || !limitReached {
		t.Fatalf("session beyond cap was accepted: selected=%p installed=%v limit=%v", selected, installed, limitReached)
	}
	session.rejectUDPSession(rejectedID, rejected)

	select {
	case <-recording.wrote:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for DISSOCIATE notification")
	}
	payload := recording.bytes()
	if len(payload) != 4 || payload[0] != tuicVersion || payload[1] != commandDissociate {
		t.Fatalf("invalid DISSOCIATE payload: %x", payload)
	}
	if got := binary.BigEndian.Uint16(payload[2:]); got != rejectedID {
		t.Fatalf("DISSOCIATE session ID = %d, want %d", got, rejectedID)
	}

	session.closeWithError(context.Canceled)
	session.workers.Wait()
	if session.udpConnMap != nil {
		t.Fatalf("retained %d UDP sessions after close", len(session.udpConnMap))
	}
}

func waitForContext(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for context cancellation")
	}
}
