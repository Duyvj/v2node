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
	t.Running = true
	t.Stop = make(chan struct{})
	t.Access.Unlock()

	go func() {
		if first {
			if err := t.ExecuteWithTimeout(); err != nil {
				log.Errorf("Task %s initial execution error: %v", t.Name, err)
			}
		}

		interval := t.Interval
		if interval <= 0 {
			interval = 30 * time.Second
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				if err := t.ExecuteWithTimeout(); err != nil {
					log.Errorf("Task %s execution error: %v", t.Name, err)
				}
			case <-t.Stop:
				return
			}
		}
	}()

	return nil
}

func (t *Task) ExecuteWithTimeout() error {
	timeout := min(5*t.Interval, 5*time.Minute)
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	err := t.Execute(ctx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
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
		}
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	return nil
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
