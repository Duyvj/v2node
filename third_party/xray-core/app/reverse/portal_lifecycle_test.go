package reverse

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	proxyman "github.com/xtls/xray-core/app/proxyman"
	proxymanOutbound "github.com/xtls/xray-core/app/proxyman/outbound"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/mux"
	"github.com/xtls/xray-core/common/signal"
	"github.com/xtls/xray-core/common/task"
	"github.com/xtls/xray-core/transport"
	"github.com/xtls/xray-core/transport/pipe"
)

func TestPortalCloseStopsStaticMuxPickerCleanup(t *testing.T) {
	manager, err := proxymanOutbound.New(context.Background(), &proxyman.OutboundConfig{})
	if err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	picker := &StaticMuxPicker{
		cTask: &task.Periodic{
			Interval: time.Hour,
			Execute: func() error {
				calls.Add(1)
				return nil
			},
		},
	}
	if err := picker.cTask.Start(); err != nil {
		t.Fatal(err)
	}
	defer picker.cTask.Close()

	portal := &Portal{
		ohm:    manager,
		tag:    "portal-lifecycle-test",
		picker: picker,
	}
	if err := portal.Close(); err != nil {
		t.Fatal(err)
	}

	// A stopped Periodic runs immediately when restarted. If Portal.Close left the
	// picker task running, this Start would be a no-op until the one-hour timer.
	if err := picker.cTask.Start(); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("picker cleanup executed %d times after restart, want 2", got)
	}
}

func TestStaticMuxPickerCloseReleasesWorkersAndRejectsLateWorkers(t *testing.T) {
	newWorker := func() (*PortalWorker, *task.Periodic, <-chan struct{}, *atomic.Int32) {
		calls := new(atomic.Int32)
		control := &task.Periodic{
			Interval: time.Hour,
			Execute: func() error {
				calls.Add(1)
				return nil
			},
		}
		if err := control.Start(); err != nil {
			t.Fatal(err)
		}

		timedOut := make(chan struct{})
		var timeoutOnce sync.Once
		activity := signal.CancelAfterInactivity(context.Background(), func() {
			timeoutOnce.Do(func() { close(timedOut) })
		}, time.Hour)
		return &PortalWorker{control: control, timer: activity}, control, timedOut, calls
	}

	worker, workerControl, workerClosed, workerCalls := newWorker()
	picker := &StaticMuxPicker{
		workers: []*PortalWorker{worker},
		cTask:   &task.Periodic{Interval: time.Hour, Execute: func() error { return nil }},
	}
	if err := picker.Close(); err != nil {
		t.Fatal(err)
	}
	if !picker.closed || picker.workers != nil {
		t.Fatalf("closed picker retained workers: closed=%v workers=%d", picker.closed, len(picker.workers))
	}
	select {
	case <-workerClosed:
	default:
		t.Fatal("picker close did not stop the worker activity timer")
	}
	if err := workerControl.Start(); err != nil {
		t.Fatal(err)
	}
	defer workerControl.Close()
	if got := workerCalls.Load(); got != 2 {
		t.Fatalf("worker control task was not stopped: executions after restart=%d, want 2", got)
	}
	if worker.control != nil || worker.timer != nil || worker.client != nil || worker.reader != nil || worker.writer != nil {
		t.Fatal("closed worker retained one or more owned resources")
	}

	lateWorker, lateControl, lateWorkerClosed, lateWorkerCalls := newWorker()
	picker.AddWorker(lateWorker)
	if picker.workers != nil {
		t.Fatalf("closed picker accepted %d late worker(s)", len(picker.workers))
	}
	select {
	case <-lateWorkerClosed:
	default:
		t.Fatal("late worker was not closed")
	}
	if err := lateControl.Start(); err != nil {
		t.Fatal(err)
	}
	defer lateControl.Close()
	if got := lateWorkerCalls.Load(); got != 2 {
		t.Fatalf("late worker control task was not stopped: executions after restart=%d, want 2", got)
	}
	if err := picker.Close(); err != nil {
		t.Fatalf("second picker close failed: %v", err)
	}
}

func TestStaticMuxPickerCleanupDropsClosedBurstBackingArray(t *testing.T) {
	const burst = 4096
	picker := &StaticMuxPicker{workers: make([]*PortalWorker, burst)}
	for i := range picker.workers {
		picker.workers[i] = &PortalWorker{closed: true}
	}

	if err := picker.cleanup(); err != nil {
		t.Fatal(err)
	}
	if picker.workers != nil {
		t.Fatalf("cleanup retained closed burst: len=%d cap=%d", len(picker.workers), cap(picker.workers))
	}
}

func TestPortalWorkerCloseReleasesMuxAndControlPipes(t *testing.T) {
	clientReader, peerWriter := pipe.New(pipe.WithSizeLimit(16 * 1024))
	peerReader, clientWriter := pipe.New(pipe.WithSizeLimit(16 * 1024))
	client, err := mux.NewClientWorker(transport.Link{
		Reader: clientReader,
		Writer: clientWriter,
	}, mux.ClientStrategy{})
	if err != nil {
		t.Fatal(err)
	}
	defer peerWriter.Interrupt()
	defer peerReader.Interrupt()

	worker, err := NewPortalWorker(client)
	if err != nil {
		t.Fatal(err)
	}
	controlReader := worker.reader
	controlWriter := worker.writer

	if err := worker.Close(); err != nil {
		t.Fatal(err)
	}
	if err := worker.Close(); err != nil {
		t.Fatalf("second worker close failed: %v", err)
	}
	select {
	case <-client.WaitClosed():
	case <-time.After(time.Second):
		t.Fatal("mux client did not close")
	}

	if err := controlWriter.WriteMultiBuffer(buf.MultiBuffer{buf.FromBytes([]byte("x"))}); err == nil {
		t.Fatal("closed worker control writer still accepts data")
	}
	if mb, err := controlReader.ReadMultiBuffer(); err == nil {
		buf.ReleaseMulti(mb)
		t.Fatal("closed worker control reader still accepts data")
	}
	if worker.control != nil || worker.timer != nil || worker.client != nil || worker.reader != nil || worker.writer != nil {
		t.Fatal("closed worker retained one or more owned resources")
	}
	if !worker.Closed() {
		t.Fatal("closed worker reports itself active")
	}
	if !worker.IsFull() {
		t.Fatal("closed worker reports capacity")
	}
	if err := peerWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := clientWriter.Close(); err != nil {
		t.Fatal(err)
	}
}
