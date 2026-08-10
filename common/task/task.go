package task

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const closeWait = 30 * time.Second

// Task runs at most one invocation at a time. Execute is called directly by
// the scheduler goroutine so a timeout can never orphan another goroutine.
type Task struct {
	Name     string
	Interval time.Duration
	Execute  func(context.Context) error
	ReloadCh chan struct{}
	// CloseTimeout is primarily useful for embedding/tests. Zero uses 30s.
	CloseTimeout time.Duration

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	done    chan struct{}
}

func (t *Task) Start(first bool) error {
	if t.Interval <= 0 {
		return fmt.Errorf("task %s has invalid interval %s", t.Name, t.Interval)
	}
	if t.Execute == nil {
		return fmt.Errorf("task %s has no execute function", t.Name)
	}

	t.mu.Lock()
	if t.running {
		t.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.running = true
	t.cancel = cancel
	t.done = done
	t.mu.Unlock()

	go t.run(ctx, done, first)
	return nil
}

func (t *Task) run(ctx context.Context, done chan struct{}, first bool) {
	defer func() {
		t.mu.Lock()
		if t.done == done {
			t.running = false
			t.cancel = nil
		}
		t.mu.Unlock()
		close(done)
	}()

	if first {
		if err := t.executeWithTimeout(ctx); err != nil {
			if !errors.Is(err, context.Canceled) {
				log.Errorf("Task %s execution error: %v", t.Name, err)
			}
			return
		}
	}

	timer := time.NewTimer(t.Interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		if err := t.executeWithTimeout(ctx); err != nil {
			if !errors.Is(err, context.Canceled) {
				log.Errorf("Task %s execution error: %v", t.Name, err)
			}
			return
		}
		timer.Reset(t.Interval)
	}
}

func (t *Task) executeWithTimeout(parent context.Context) error {
	timeout := 5 * time.Minute
	if t.Interval < time.Minute {
		timeout = 5 * t.Interval
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	err := t.Execute(ctx)
	if parent.Err() != nil {
		return parent.Err()
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		log.Errorf("Task %s execution timed out, reloading", t.Name)
		if t.ReloadCh == nil {
			return fmt.Errorf("task %s timed out without a reload channel", t.Name)
		}
		select {
		case t.ReloadCh <- struct{}{}:
		default:
		}
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// ExecuteWithTimeout keeps the public helper used by older callers while
// retaining the single-goroutine execution model.
func (t *Task) ExecuteWithTimeout() error {
	if t.Interval <= 0 {
		return fmt.Errorf("task %s has invalid interval %s", t.Name, t.Interval)
	}
	if t.Execute == nil {
		return fmt.Errorf("task %s has no execute function", t.Name)
	}
	return t.executeWithTimeout(context.Background())
}

// Close cancels the active invocation and joins the scheduler. If Execute
// ignores cancellation, Close fails and callers must not start a replacement
// generation; this bounds retention to one task instead of leaking on reload.
func (t *Task) Close() error {
	t.mu.Lock()
	if !t.running {
		t.mu.Unlock()
		return nil
	}
	cancel := t.cancel
	done := t.done
	t.mu.Unlock()

	cancel()
	wait := t.CloseTimeout
	if wait <= 0 {
		wait = closeWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-done:
		log.Warningf("Task %s stopped", t.Name)
		return nil
	case <-timer.C:
		return fmt.Errorf("task %s did not stop within %s", t.Name, wait)
	}
}

func (t *Task) Running() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.running
}
