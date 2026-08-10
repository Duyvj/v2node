package pubsub

import (
	"strconv"
	"testing"
)

func TestCleanupCapacityScalesWithSubscribersNotTopics(t *testing.T) {
	const topics = 4096
	s := NewService()
	defer s.ctask.Close()

	for i := 0; i < topics; i++ {
		s.Subscribe(strconv.Itoa(i))
	}
	if err := s.Cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	totalCapacity := 0
	for _, subscribers := range s.subs {
		totalCapacity += cap(subscribers)
	}
	if totalCapacity > topics {
		t.Fatalf("subscriber backing capacity = %d, want at most %d", totalCapacity, topics)
	}
}

func TestCleanupCompactsPeakSubscriberSliceWithOneSurvivor(t *testing.T) {
	const subscribers = 4096
	s := NewService()
	defer s.ctask.Close()

	all := make([]*Subscriber, 0, subscribers)
	for i := 0; i < subscribers; i++ {
		all = append(all, s.Subscribe("hot-topic"))
	}
	survivor := all[len(all)-1]
	for _, sub := range all[:len(all)-1] {
		if err := sub.Close(); err != nil {
			t.Fatalf("close subscriber: %v", err)
		}
	}

	if err := s.Cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	remaining := s.subs["hot-topic"]
	if len(remaining) != 1 || remaining[0] != survivor {
		t.Fatalf("remaining subscribers = %d, want the one live survivor", len(remaining))
	}
	if got := cap(remaining); got != 1 {
		t.Fatalf("survivor slice capacity = %d, want 1", got)
	}

	s.Publish("hot-topic", "alive")
	select {
	case got := <-survivor.Wait():
		if got != "alive" {
			t.Fatalf("published value = %v, want alive", got)
		}
	default:
		t.Fatal("surviving subscriber did not receive publication")
	}
}
