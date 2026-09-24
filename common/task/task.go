package task

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// A Task owns one worker. Cancellation and join prevent reloads from leaving
// detached requests, overlapping reports or callbacks using a closed core.
type Task struct {
	Name     string
	Interval time.Duration
	Execute  func(context.Context) error
	ReloadCh chan struct{}
	mu       sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
}

func (t *Task) Start(first bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done != nil {
		return nil
	}
	if t.Interval <= 0 || t.Execute == nil {
		return fmt.Errorf("invalid periodic task %q", t.Name)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.cancel = cancel
	t.done = done
	go t.run(ctx, done, first)
	return nil
}
func (t *Task) run(ctx context.Context, done chan struct{}, first bool) {
	defer close(done)
	timer := time.NewTimer(t.Interval)
	defer timer.Stop()
	for {
		if !first {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
		}
		first = false
		if ctx.Err() != nil {
			return
		}
		err := t.execute(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.WithError(err).Warnf("Task %s failed; retrying next interval", t.Name)
		}
		timer.Reset(t.Interval)
	}
}
func (t *Task) execute(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, min(5*t.Interval, 5*time.Minute))
	defer cancel()
	err := t.Execute(ctx)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) && parent.Err() == nil && t.ReloadCh != nil {
		select {
		case t.ReloadCh <- struct{}{}:
		default:
		}
	}
	return err
}
func (t *Task) ExecuteWithTimeout() error { return t.execute(context.Background()) }
func (t *Task) Close() error {
	t.mu.Lock()
	done, cancel := t.done, t.cancel
	t.mu.Unlock()
	if done == nil {
		return nil
	}
	cancel()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-done:
		t.mu.Lock()
		if t.done == done {
			t.done = nil
			t.cancel = nil
		}
		t.mu.Unlock()
		return nil
	case <-timer.C:
		return fmt.Errorf("task %s did not stop", t.Name)
	}
}
