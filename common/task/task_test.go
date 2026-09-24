package task

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestCloseCancelsAndJoinsWorker(t *testing.T) {
	started := make(chan struct{})
	exited := make(chan struct{})
	task := &Task{Name: "test", Interval: time.Hour, Execute: func(ctx context.Context) error { close(started); <-ctx.Done(); close(exited); return ctx.Err() }}
	if err := task.Start(true); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := task.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	default:
		t.Fatal("worker survived close")
	}
	if err := task.Close(); err != nil {
		t.Fatal(err)
	}
}
func TestTransientErrorsRetryWithoutOverlap(t *testing.T) {
	var active, maximum, calls atomic.Int32
	enough := make(chan struct{})
	task := &Task{Name: "retry", Interval: time.Millisecond, Execute: func(ctx context.Context) error {
		n := active.Add(1)
		if n > maximum.Load() {
			maximum.Store(n)
		}
		defer active.Add(-1)
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Millisecond):
		}
		if calls.Add(1) == 3 {
			close(enough)
		}
		return errors.New("transient")
	}}
	task.Start(true)
	select {
	case <-enough:
	case <-time.After(time.Second):
		t.Fatal("task stopped retrying")
	}
	if err := task.Close(); err != nil {
		t.Fatal(err)
	}
	if maximum.Load() != 1 {
		t.Fatal("overlapping executions")
	}
}
