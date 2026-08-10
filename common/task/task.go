package task

import (
	"context"
	"errors"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

type Task struct {
	Name     string
	Interval time.Duration
	Execute  func(context.Context) error
	Access   sync.RWMutex
	Running  bool
	ReloadCh chan struct{}
	Stop     chan struct{}
}

func (t *Task) Start(first bool) error {
	t.Access.Lock()
	if t.Running {
		t.Access.Unlock()
		return nil
	}
	if t.Interval <= 0 {
		// A malformed panel interval must not become a busy loop.
		t.Interval = 30 * time.Second
	}
	t.Running = true
	t.Stop = make(chan struct{})
	stop := t.Stop
	t.Access.Unlock()
	go func() {
		defer func() {
			t.Access.Lock()
			if t.Stop == stop {
				t.Running = false
			}
			t.Access.Unlock()
		}()
		if first {
			if err := t.ExecuteWithTimeout(); err != nil {
				return
			}
		}
		// Create the timer after an optional first run. This avoids a stale
		// timer event when a first API call takes longer than the interval.
		timer := time.NewTimer(t.Interval)
		defer timer.Stop()

		for {
			select {
			case <-timer.C:
				// continue
			case <-stop:
				return
			}

			if err := t.ExecuteWithTimeout(); err != nil {
				log.Errorf("Task %s execution error: %v", t.Name, err)
				return
			}
			timer.Reset(t.Interval)
		}
	}()

	return nil
}

func (t *Task) ExecuteWithTimeout() error {
	ctx, cancel := context.WithTimeout(context.Background(), min(5*t.Interval, 5*time.Minute))
	defer cancel()
	done := make(chan error, 1)

	go func() {
		done <- t.Execute(ctx)
	}()

	select {
	case <-ctx.Done():
		log.Errorf("Task %s execution timed out, reloading", t.Name)
		if t.ReloadCh != nil {
			select {
			case t.ReloadCh <- struct{}{}:
			default:
			}
		} else {
			log.Panic("Reload failed")
		}
		return nil
	case err := <-done:
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil
		}
		return err
	}
}

func (t *Task) safeStop() {
	t.Access.Lock()
	if t.Running {
		t.Running = false
		close(t.Stop)
	}
	t.Access.Unlock()
}

func (t *Task) Close() {
	t.safeStop()
	log.Warningf("Task %s stopped", t.Name)
}
