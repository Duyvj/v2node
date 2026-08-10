package task

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPeriodicStartDuringFailingExecutionIsNotLost(t *testing.T) {
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	restarted := make(chan struct{})
	var calls atomic.Int32
	task := &Periodic{
		Interval: time.Hour,
		Execute: func() error {
			switch calls.Add(1) {
			case 1:
				close(firstStarted)
				<-releaseFirst
				return errors.New("first execution failed")
			case 2:
				close(restarted)
			}
			return nil
		},
	}
	defer task.Close()

	firstResult := make(chan error, 1)
	go func() { firstResult <- task.Start() }()
	<-firstStarted
	if err := task.Start(); err != nil {
		t.Fatal(err)
	}
	close(releaseFirst)

	select {
	case <-restarted:
	case <-time.After(time.Second):
		t.Fatal("Start racing with a failing execution was lost")
	}
	if err := <-firstResult; err == nil {
		t.Fatal("failing execution unexpectedly returned nil")
	}
}

func TestPeriodicTaskStop(t *testing.T) {
	value := 0
	task := &Periodic{
		Interval: time.Second * 2,
		Execute: func() error {
			value++
			return nil
		},
	}
	if err := task.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second * 5)
	if err := task.Close(); err != nil {
		t.Fatal(err)
	}
	if value != 3 {
		t.Fatal("expected 3, but got ", value)
	}
	time.Sleep(time.Second * 4)
	if value != 3 {
		t.Fatal("expected 3, but got ", value)
	}
	if err := task.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second * 3)
	if value != 5 {
		t.Fatal("Expected 5, but ", value)
	}
	if err := task.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPeriodicCloseStartWaitsForOldGeneration(t *testing.T) {
	oldError := errors.New("old generation failed")
	tests := []struct {
		name string
		err  error
	}{
		{name: "old success"},
		{name: "old error", err: oldError},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const interval = 20 * time.Millisecond

			firstStarted := make(chan struct{})
			releaseFirst := make(chan struct{})
			restarted := make(chan struct{})
			timerStarted := make(chan struct{})
			releaseTimer := make(chan struct{})
			timerReturned := make(chan struct{})
			var releaseFirstOnce sync.Once
			var releaseTimerOnce sync.Once
			var timerStartedOnce sync.Once
			var calls atomic.Int32
			var active atomic.Int32
			var maxActive atomic.Int32

			periodic := &Periodic{
				Interval: interval,
				Execute: func() error {
					call := calls.Add(1)
					current := active.Add(1)
					defer active.Add(-1)
					for {
						maximum := maxActive.Load()
						if current <= maximum || maxActive.CompareAndSwap(maximum, current) {
							break
						}
					}

					switch call {
					case 1:
						close(firstStarted)
						<-releaseFirst
						return test.err
					case 2:
						close(restarted)
						return nil
					default:
						timerStartedOnce.Do(func() { close(timerStarted) })
						<-releaseTimer
						if call == 3 {
							close(timerReturned)
						}
						return nil
					}
				},
			}
			defer func() {
				_ = periodic.Close()
				releaseFirstOnce.Do(func() { close(releaseFirst) })
				releaseTimerOnce.Do(func() { close(releaseTimer) })
			}()

			firstResult := make(chan error, 1)
			go func() { firstResult <- periodic.Start() }()
			<-firstStarted

			if err := periodic.Close(); err != nil {
				t.Fatal(err)
			}
			if err := periodic.Start(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-restarted:
				t.Fatal("new generation executed before the old generation returned")
			default:
			}

			releaseFirstOnce.Do(func() { close(releaseFirst) })
			select {
			case <-restarted:
			case <-time.After(time.Second):
				t.Fatal("new generation did not start after the old generation returned")
			}

			result := <-firstResult
			if !errors.Is(result, test.err) {
				t.Fatalf("old generation returned %v, want %v", result, test.err)
			}

			select {
			case <-timerStarted:
			case <-time.After(time.Second):
				t.Fatal("new generation timer did not fire")
			}
			time.Sleep(3 * interval)
			if got := calls.Load(); got != 3 {
				t.Fatalf("found duplicate timer chains: got %d executions, want 3", got)
			}
			if got := maxActive.Load(); got != 1 {
				t.Fatalf("Execute concurrency = %d, want 1", got)
			}

			if err := periodic.Close(); err != nil {
				t.Fatal(err)
			}
			releaseTimerOnce.Do(func() { close(releaseTimer) })
			select {
			case <-timerReturned:
			case <-time.After(time.Second):
				t.Fatal("timer execution did not return")
			}
		})
	}
}
