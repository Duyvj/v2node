package dispatcher

import (
	"context"
	"testing"

	"github.com/xtls/xray-core/app/observatory"
	"google.golang.org/protobuf/proto"
)

type mockObservatory struct {
	status []*observatory.OutboundStatus
}

func (m *mockObservatory) Type() interface{} {
	return nil
}

func (m *mockObservatory) Start() error {
	return nil
}

func (m *mockObservatory) Close() error {
	return nil
}

func (m *mockObservatory) GetObservation(ctx context.Context) (proto.Message, error) {
	return &observatory.ObservationResult{
		Status: m.status,
	}, nil
}

func TestStickyBalancerKeepsSessionFixed(t *testing.T) {
	b := NewStickyBalancer()
	defer b.Close()

	candidates := []string{"WG-A", "WG-B", "WG-C"}
	b.SetCandidates(candidates)

	first := b.PickOutbound("user-1")
	if first == "" {
		t.Fatal("expected non-empty outbound tag")
	}

	for i := 0; i < 20; i++ {
		got := b.PickOutbound("user-1")
		if got != first {
			t.Fatalf("session affinity broken: got %s, want %s on iteration %d", got, first, i)
		}
	}
}

func TestStickyBalancerDistributesDistinctSessions(t *testing.T) {
	b := NewStickyBalancer()
	defer b.Close()

	candidates := []string{"WG-A", "WG-B", "WG-C"}
	b.SetCandidates(candidates)

	s1 := b.PickOutbound("user-1")
	s2 := b.PickOutbound("user-2")
	s3 := b.PickOutbound("user-3")

	tags := map[string]bool{s1: true, s2: true, s3: true}
	if len(tags) != 3 {
		t.Fatalf("expected 3 distinct tags for 3 sessions, got: %s, %s, %s", s1, s2, s3)
	}

	// Verify stickiness persists after distribution
	if got := b.PickOutbound("user-1"); got != s1 {
		t.Fatalf("user-1 affinity broken: got %s, want %s", got, s1)
	}
	if got := b.PickOutbound("user-2"); got != s2 {
		t.Fatalf("user-2 affinity broken: got %s, want %s", got, s2)
	}
	if got := b.PickOutbound("user-3"); got != s3 {
		t.Fatalf("user-3 affinity broken: got %s, want %s", got, s3)
	}
}

func TestStickyBalancerFailoverOnObservatoryUnhealthy(t *testing.T) {
	b := NewStickyBalancer()
	defer b.Close()

	candidates := []string{"WG-A", "WG-B"}
	b.SetCandidates(candidates)

	mockObs := &mockObservatory{
		status: []*observatory.OutboundStatus{
			{OutboundTag: "WG-A", Alive: true, Delay: 50},
			{OutboundTag: "WG-B", Alive: true, Delay: 60},
		},
	}
	b.SetObservatory(context.Background(), mockObs)

	initial := b.PickOutbound("user-failover")
	if initial == "" {
		t.Fatal("expected non-empty tag")
	}

	// Mark initial node down
	mockObs.status = []*observatory.OutboundStatus{
		{OutboundTag: initial, Alive: false, Delay: 9999},
	}
	for _, c := range candidates {
		if c != initial {
			mockObs.status = append(mockObs.status, &observatory.OutboundStatus{
				OutboundTag: c,
				Alive:       true,
				Delay:       50,
			})
		}
	}

	// Next pick must migrate to the alive candidate
	next := b.PickOutbound("user-failover")
	if next == initial {
		t.Fatalf("user remained on dead node %s", initial)
	}

	// Subsequent calls must remain sticky to the new healthy node
	for i := 0; i < 5; i++ {
		if got := b.PickOutbound("user-failover"); got != next {
			t.Fatalf("user failed to remain sticky on failover node: got %s, want %s", got, next)
		}
	}
}
