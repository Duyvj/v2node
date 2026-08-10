package task

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestTaskRejectsInvalidInterval(t *testing.T) {
	task := &Task{Name: "invalid", Execute: func(context.Context) error { return nil }}
	if err := task.Start(false); err == nil {
		t.Fatal("zero interval was accepted")
	}
	if task.Running() {
		t.Fatal("invalid task started a scheduler")
	}
}

func TestTaskCloseCancelsAndJoinsRepeatedly(t *testing.T) {
	var active atomic.Int32
	var maxActive atomic.Int32
	for i := 0; i < 100; i++ {
		task := &Task{
			Name:         "join",
			Interval:     time.Hour,
			CloseTimeout: time.Second,
			Execute: func(ctx context.Context) error {
				current := active.Add(1)
				defer active.Add(-1)
				for {
					old := maxActive.Load()
					if current <= old || maxActive.CompareAndSwap(old, current) {
						break
					}
				}
				<-ctx.Done()
				return ctx.Err()
			},
		}
		if err := task.Start(true); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(time.Second)
		for active.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if err := task.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if got := active.Load(); got != 0 {
		t.Fatalf("%d executions remain active", got)
	}
	if got := maxActive.Load(); got != 1 {
		t.Fatalf("maximum concurrent executions = %d, want 1", got)
	}
}

func TestContextIgnoringTaskCannotCreateReplacementGeneration(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	task := &Task{
		Name:         "blocked",
		Interval:     time.Hour,
		CloseTimeout: 20 * time.Millisecond,
		Execute: func(context.Context) error {
			started <- struct{}{}
			<-release
			return nil
		},
	}
	if err := task.Start(true); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := task.Close(); err == nil {
		t.Fatal("Close succeeded while Execute ignored cancellation")
	}
	if err := task.Start(true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
		t.Fatal("a replacement invocation started while the old one was live")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for task.Running() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if task.Running() {
		t.Fatal("task did not finish after the blocker was released")
	}
}

func TestTaskInternalDeadlineExceededDoesNotRequestReload(t *testing.T) {
	reload := make(chan struct{}, 1)
	task := &Task{
		Name:     "panel-timeout",
		Interval: time.Hour,
		ReloadCh: reload,
		Execute: func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				t.Fatalf("scheduler context unexpectedly ended: %v", err)
			}
			return context.DeadlineExceeded
		},
	}

	if err := task.ExecuteWithTimeout(); err != nil {
		t.Fatalf("ExecuteWithTimeout() error = %v, want nil for a transient callback deadline", err)
	}
	select {
	case <-reload:
		t.Fatal("internal deadline error requested a process reload")
	default:
	}
}

func TestTaskStartContinuesAfterCallbackDeadlineExceeded(t *testing.T) {
	reload := make(chan struct{}, 1)
	invocations := make(chan int, 2)
	invocation := 0
	task := &Task{
		Name:         "panel-timeout",
		Interval:     10 * time.Millisecond,
		ReloadCh:     reload,
		CloseTimeout: time.Second,
		Execute: func(context.Context) error {
			invocation++
			invocations <- invocation
			return context.DeadlineExceeded
		},
	}

	if err := task.Start(true); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := task.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()

	for want := 1; want <= 2; want++ {
		select {
		case got := <-invocations:
			if got != want {
				t.Fatalf("invocation = %d, want %d", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for invocation %d", want)
		}
	}
	if err := task.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reload:
		t.Fatal("callback-local deadline requested a process reload")
	default:
	}
}

func TestTaskSchedulerTimeoutRequestsReload(t *testing.T) {
	reload := make(chan struct{}, 1)
	task := &Task{
		Name:     "timeout",
		Interval: 2 * time.Millisecond,
		ReloadCh: reload,
		Execute: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}
	if err := task.ExecuteWithTimeout(); err != nil {
		t.Fatalf("ExecuteWithTimeout() error = %v, want nil", err)
	}
	select {
	case <-reload:
	case <-time.After(time.Second):
		t.Fatal("task timeout did not request reload")
	}
}
