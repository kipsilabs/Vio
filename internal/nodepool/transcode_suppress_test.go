package nodepool

import (
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/netaccess"
)

// transcodePoolWithClock returns a pool whose wall clock is fixed, so
// cooldown expiry is deterministic without sleeping.
func transcodePoolWithClock(nodes []*Node, now time.Time) (*TranscodePool, *time.Time) {
	p := NewTranscodePool()
	p.SetNodes(nodes)
	clock := now
	p.now = func() time.Time { return clock }
	return p, &clock
}

func TestSuppressNodeSkipsAcquireUntilCooldown(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	p, clock := transcodePoolWithClock([]*Node{
		{ID: 1, URL: "http://tc-a", Enabled: true, Healthy: true},
		{ID: 2, URL: "http://tc-b", Enabled: true, Healthy: true},
	}, now)

	p.SuppressNode("http://tc-a", 0, 15*time.Second)
	if !p.SuppressedForTest("http://tc-a") {
		t.Fatal("freshly suppressed node must report suppressed")
	}
	if got := p.Acquire(); got == nil || got.URL != "http://tc-b" {
		t.Fatalf("Acquire skipped the healthy sibling, got %+v", got)
	}
	// Past cooldown + jitter window: suppression must lapse on its own.
	*clock = now.Add(21 * time.Second)
	if p.SuppressedForTest("http://tc-a") {
		t.Fatal("suppression survived past cooldown+jitter")
	}
	if got := p.Acquire(); got == nil {
		t.Fatal("Acquire returned nil after cooldown lapsed")
	}
}

func TestSuppressNodeStaleCompletionCannotRearm(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	p, _ := transcodePoolWithClock([]*Node{
		{ID: 1, URL: "http://tc-a", Enabled: true, Healthy: true},
	}, now)

	// A start admitted at epoch 0 fails and suppresses the node.
	admitted := p.RecoveryEpoch("http://tc-a")
	p.SuppressNode("http://tc-a", admitted, time.Minute)
	if !p.SuppressedForTest("http://tc-a") {
		t.Fatal("fresh trip must suppress")
	}
	// The sweep recovers the node: suppression cleared, epoch bumped.
	p.ApplyHealth(1, "http://tc-a", true, 0, 0, "", nil, netaccess.NodeNetworkAccess{}, now)
	if p.SuppressedForTest("http://tc-a") {
		t.Fatal("healthy sweep must clear suppression immediately")
	}
	// A second start admitted BEFORE the recovery (stale epoch) fails after
	// it: the stale completion must be refused and the node stays eligible.
	p.SuppressNode("http://tc-a", admitted, time.Minute)
	if p.SuppressedForTest("http://tc-a") {
		t.Fatal("stale completion re-armed suppression after a healthy recovery")
	}
	// A start admitted AFTER the recovery fails: fresh evidence suppresses.
	fresh := p.RecoveryEpoch("http://tc-a")
	if fresh <= admitted {
		t.Fatalf("recovery epoch did not advance: admitted=%d fresh=%d", admitted, fresh)
	}
	p.SuppressNode("http://tc-a", fresh, time.Minute)
	if !p.SuppressedForTest("http://tc-a") {
		t.Fatal("fresh trip after recovery must suppress again")
	}
}

func TestSuppressNodeNilSafe(t *testing.T) {
	var p *TranscodePool
	p.SuppressNode("http://tc-a", 0, 0)
	if p.SuppressedForTest("http://tc-a") {
		t.Fatal("nil pool must never report suppressed")
	}
	if p.Acquire() != nil {
		t.Fatal("nil pool Acquire must return nil")
	}
	p.ClearSuppressionForTest("http://tc-a")
}

func TestTranscodeEligibleSkipsSuppressed(t *testing.T) {
	f := newFixture(nil, []*Node{
		transcodeNode(1, "http://tc-a", nil, 0),
		transcodeNode(2, "http://tc-b", nil, 0),
	})
	f.transcodes.now = func() time.Time { return f.now }
	f.transcodes.SuppressNode("http://tc-a", 0, time.Minute)

	plan := f.planner.PlanSession("s1", "", true, 0)
	if plan.TranscodeNode == nil || plan.TranscodeNode.URL != "http://tc-b" {
		t.Fatalf("suppressed node must lose selection, got %+v", plan.TranscodeNode)
	}
}

func TestTranscodeEligibleAdmitsAfterHealthySweep(t *testing.T) {
	f := newFixture(nil, []*Node{
		transcodeNode(1, "http://tc-a", nil, 0),
	})
	f.transcodes.now = func() time.Time { return f.now }
	f.transcodes.SuppressNode("http://tc-a", 0, time.Minute)
	f.transcodes.ApplyHealth(1, "http://tc-a", true, 0, 0, "", nil, netaccess.NodeNetworkAccess{}, f.now)

	plan := f.planner.PlanSession("s1", "", true, 0)
	if plan.TranscodeNode == nil || plan.TranscodeNode.URL != "http://tc-a" {
		t.Fatalf("healthy sweep must restore eligibility, got %+v", plan.TranscodeNode)
	}
}

func TestSuppressNodeSetNodesPrunesRemoved(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	p, _ := transcodePoolWithClock([]*Node{
		{ID: 1, URL: "http://tc-a", Enabled: true, Healthy: true},
	}, now)
	p.SuppressNode("http://tc-a", 0, time.Minute)
	p.SetNodes([]*Node{
		{ID: 2, URL: "http://tc-b", Enabled: true, Healthy: true},
	})
	if p.SuppressedForTest("http://tc-a") {
		t.Fatal("removed node must lose its suppression entry on SetNodes")
	}
}
