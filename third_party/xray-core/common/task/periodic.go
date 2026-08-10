package task

import (
	"sync"
	"time"
)

// Periodic is a task that runs periodically.
type Periodic struct {
	// Interval of the task being run
	Interval time.Duration
	// Execute is the task function
	Execute func() error

	access     sync.Mutex
	timer      *time.Timer
	running    bool
	executing  bool
	generation uint64
	starts     uint64
}

func (t *Periodic) checkedExecute(generation, start uint64) error {
	t.access.Lock()
	if !t.running {
		t.executing = false
		t.access.Unlock()
		return nil
	}
	if t.generation != generation {
		generation = t.generation
		start = t.starts
	}
	t.access.Unlock()

	err := t.Execute()

	t.access.Lock()
	if !t.running {
		t.executing = false
		t.access.Unlock()
		return err
	}

	if t.generation != generation {
		nextGeneration := t.generation
		nextStart := t.starts
		t.access.Unlock()
		go func() {
			_ = t.checkedExecute(nextGeneration, nextStart)
		}()
		return err
	}

	if err != nil {
		if t.starts != start {
			nextStart := t.starts
			t.access.Unlock()
			go func() {
				_ = t.checkedExecute(generation, nextStart)
			}()
			return err
		}
		t.running = false
		t.executing = false
		t.generation++
		t.access.Unlock()
		return err
	}

	t.executing = false
	t.timer = time.AfterFunc(t.Interval, func() {
		t.runTimer(generation)
	})
	t.access.Unlock()

	return nil
}

func (t *Periodic) runTimer(generation uint64) {
	t.access.Lock()
	if !t.running || t.generation != generation || t.executing {
		t.access.Unlock()
		return
	}
	t.timer = nil
	t.executing = true
	start := t.starts
	t.access.Unlock()

	_ = t.checkedExecute(generation, start)
}

// Start implements common.Runnable.
func (t *Periodic) Start() error {
	t.access.Lock()
	t.starts++
	if t.running {
		t.access.Unlock()
		return nil
	}
	t.running = true
	t.generation++
	generation := t.generation
	start := t.starts
	if t.executing {
		t.access.Unlock()
		return nil
	}
	t.executing = true
	t.access.Unlock()

	return t.checkedExecute(generation, start)
}

// Close implements common.Closable.
func (t *Periodic) Close() error {
	t.access.Lock()
	defer t.access.Unlock()

	t.running = false
	t.generation++
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}

	return nil
}
