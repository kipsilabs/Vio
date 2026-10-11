package playback

import (
	"testing"
	"time"
)

// TestVirtualProbePendingSessionsBoundedByCutoff pins the read side of the
// convergence watchdog: only a session whose deferred probe is still pending and
// was armed at or before the cutoff is listed. A session armed after the cutoff,
// a session that already reached verified or failed, and a session that never
// armed a lifecycle are all absent, so a warm resume is never mistaken for an
// overdue probe.
func TestVirtualProbePendingSessionsBoundedByCutoff(t *testing.T) {
	m := NewSessionManager(0, 0)
	session, err := m.StartSession(1, "profile-1", 1, PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := m.SetVirtualSource(session.ID, "virtual://movie/tt-pending?result=cand-1", 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	binding, err := m.VirtualSourceBinding(session.ID)
	if err != nil {
		t.Fatalf("VirtualSourceBinding: %v", err)
	}
	if applied, err := m.ArmVirtualProbePendingIfGeneration(session.ID, binding.Generation); err != nil || !applied {
		t.Fatalf("arm = (%v, %v), want (true, nil)", applied, err)
	}

	now := time.Now()
	if got := m.VirtualProbePendingSessions(now.Add(-time.Hour), 10); len(got) != 0 {
		t.Fatalf("cutoff before the arm listed %d sessions, want 0", len(got))
	}
	pending := m.VirtualProbePendingSessions(now.Add(time.Hour), 10)
	if len(pending) != 1 || pending[0].SessionID != session.ID || pending[0].Generation != binding.Generation {
		t.Fatalf("pending sessions = %#v, want the armed session at generation %d", pending, binding.Generation)
	}

	// A verified verdict is terminal: it drops out of the pending set.
	if _, err := m.SetVirtualProbeOutcomeIfGeneration(session.ID, binding.Generation, VirtualProbeOutcomeVerified); err != nil {
		t.Fatalf("SetVirtualProbeOutcomeIfGeneration: %v", err)
	}
	if got := m.VirtualProbePendingSessions(now.Add(time.Hour), 10); len(got) != 0 {
		t.Fatalf("verified session still listed as pending: %#v", got)
	}
}

// TestFailVirtualProbeIfPendingIsCasFromPendingOnly is the watchdog's write
// contract: it fails a pending lifecycle and nothing else. A verified outcome
// and a session whose binding rotated are both refused, so the bound can never
// turn success into failure or stamp a superseded generation.
func TestFailVirtualProbeIfPendingIsCasFromPendingOnly(t *testing.T) {
	m := NewSessionManager(0, 0)
	session, err := m.StartSession(1, "profile-1", 1, PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := m.SetVirtualSource(session.ID, "virtual://movie/tt-cas?result=cand-1", 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	binding, err := m.VirtualSourceBinding(session.ID)
	if err != nil {
		t.Fatalf("VirtualSourceBinding: %v", err)
	}

	// Never armed: not pending, so the failure CAS must not fire.
	if applied, err := m.FailVirtualProbeIfPendingIfGeneration(session.ID, binding.Generation, time.Now().Add(time.Hour)); err != nil || applied {
		t.Fatalf("fail-before-arm = (%v, %v), want (false, nil)", applied, err)
	}

	if applied, err := m.ArmVirtualProbePendingIfGeneration(session.ID, binding.Generation); err != nil || !applied {
		t.Fatalf("arm = (%v, %v), want (true, nil)", applied, err)
	}
	if applied, err := m.FailVirtualProbeIfPendingIfGeneration(session.ID, binding.Generation, time.Now().Add(-time.Hour)); err != nil || applied {
		t.Fatalf("fail-before-cutoff = (%v, %v), want (false, nil)", applied, err)
	}
	if applied, err := m.FailVirtualProbeIfPendingIfGeneration(session.ID, binding.Generation, time.Now().Add(time.Hour)); err != nil || !applied {
		t.Fatalf("fail-overdue = (%v, %v), want (true, nil)", applied, err)
	}
	if _, err := m.GetSession(session.ID); err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if applied, err := m.FailVirtualProbeIfPendingIfGeneration(session.ID, binding.Generation, time.Now().Add(time.Hour)); err != nil || applied {
		t.Fatalf("second fail after terminal = (%v, %v), want (false, nil)", applied, err)
	}

	// A rotation after a verified verdict is refused by generation.
	other, err := m.StartSession(1, "profile-1", 2, PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := m.SetVirtualSource(other.ID, "virtual://movie/tt-cas-2?result=cand-1", 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	otherBinding, err := m.VirtualSourceBinding(other.ID)
	if err != nil {
		t.Fatalf("VirtualSourceBinding: %v", err)
	}
	if applied, err := m.ArmVirtualProbePendingIfGeneration(other.ID, otherBinding.Generation); err != nil || !applied {
		t.Fatalf("arm other = (%v, %v), want (true, nil)", applied, err)
	}
	if _, err := m.SetVirtualProbeOutcomeIfGeneration(other.ID, otherBinding.Generation, VirtualProbeOutcomeVerified); err != nil {
		t.Fatalf("verify other: %v", err)
	}
	if applied, err := m.FailVirtualProbeIfPendingIfGeneration(other.ID, otherBinding.Generation, time.Now().Add(time.Hour)); err != nil || applied {
		t.Fatalf("fail verified = (%v, %v), want (false, nil)", applied, err)
	}
	live, err := m.GetSession(other.ID)
	if err != nil {
		t.Fatalf("GetSession other: %v", err)
	}
	if live.VirtualProbeOutcome != VirtualProbeOutcomeVerified {
		t.Fatalf("watchdog overwrote verified with %q", live.VirtualProbeOutcome)
	}
}
