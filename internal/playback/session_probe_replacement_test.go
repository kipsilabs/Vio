package playback

import (
	"testing"
	"time"
)

// TestArmVirtualProbeReplacementOnlyForUnOwnedCurrentBinding pins the
// dropped-probe recovery fence. A replacement may only arm the session's CURRENT
// binding, and only when that binding carries no live lifecycle of its own: an
// absent mark, or a stale one left by a row-only move that never advanced the
// generation. A live pending (a worker will land it), a terminal verdict, a
// candidate the caller cannot probe, and an unknown session are all refused
// without a write, so a replacement never double-arms or clobbers a reached
// outcome.
func TestArmVirtualProbeReplacementOnlyForUnOwnedCurrentBinding(t *testing.T) {
	m := NewSessionManager(0, 0)
	session, err := m.StartSession(1, "profile-1", 1, PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	const candidateURI = "virtual://movie/tt-replacement?result=cand-1"
	if err := m.SetVirtualSource(session.ID, candidateURI, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}

	// A different candidate may never be armed: a replacement built from the
	// dropped probe's input cannot describe it.
	if binding, applied, err := m.ArmVirtualProbeReplacementForCurrentBinding(session.ID, "virtual://movie/tt-other?result=cand-2"); err != nil || applied {
		t.Fatalf("candidate-mismatch arm = (%#v, %v, %v), want (_, false, nil)", binding, applied, err)
	}

	// No mark at all: the current binding has no probe, so the replacement owns it.
	binding, applied, err := m.ArmVirtualProbeReplacementForCurrentBinding(session.ID, candidateURI)
	if err != nil || !applied {
		t.Fatalf("unarmed replacement = (%#v, %v, %v), want (_, true, nil)", binding, applied, err)
	}
	if binding.Generation == 0 || binding.EffectiveMediaFileID != 1 {
		t.Fatalf("armed binding = %#v, want the current generation and effective row", binding)
	}
	// The armed mark is live: a second replacement attempt must not duplicate it.
	if _, applied, err := m.ArmVirtualProbeReplacementForCurrentBinding(session.ID, candidateURI); err != nil || applied {
		t.Fatalf("live-pending re-arm = (_, %v, %v), want (false, nil)", applied, err)
	}

	// A row-only move (SetEffectiveMediaFileID) leaves the generation and the
	// pending mark in place but changes the effective row, so the mark becomes
	// stale: no admitted probe's fence can land it. The replacement must re-arm
	// the current binding.
	if err := m.SetEffectiveMediaFileID(session.ID, 2); err != nil {
		t.Fatalf("SetEffectiveMediaFileID: %v", err)
	}
	stale, err := m.VirtualSourceBinding(session.ID)
	if err != nil {
		t.Fatalf("VirtualSourceBinding: %v", err)
	}
	if stale.EffectiveMediaFileID != 2 || stale.Generation != binding.Generation {
		t.Fatalf("row-only move = %#v, want row 2 at the unchanged generation %d", stale, binding.Generation)
	}
	if live := m.sessions[session.ID]; live == nil || live.virtualProbePendingIsLiveLocked() {
		t.Fatal("expected the pending mark to be stale after the row-only move")
	} else if live.VirtualProbeOutcome != VirtualProbeOutcomePending {
		t.Fatalf("outcome = %q, want pending still recorded", live.VirtualProbeOutcome)
	}
	rearmed, applied, err := m.ArmVirtualProbeReplacementForCurrentBinding(session.ID, candidateURI)
	if err != nil || !applied {
		t.Fatalf("stale replacement = (%#v, %v, %v), want (_, true, nil)", rearmed, applied, err)
	}
	if rearmed.EffectiveMediaFileID != 2 {
		t.Fatalf("replacement armed the wrong row: %#v, want row 2", rearmed)
	}
	// The replacement is now the live owner: a further attempt is refused.
	if _, applied, err := m.ArmVirtualProbeReplacementForCurrentBinding(session.ID, candidateURI); err != nil || applied {
		t.Fatalf("post-replacement re-arm = (_, %v, %v), want (false, nil)", applied, err)
	}
	// It is visible to the convergence watchdog, so it is not stranded.
	if pending := m.VirtualProbePendingSessions(time.Now().Add(time.Hour), 10); len(pending) != 1 || pending[0].SessionID != session.ID {
		t.Fatalf("pending sessions = %#v, want the re-armed session", pending)
	}

	// A terminal verdict for the current binding is never overwritten.
	if _, err := m.SetVirtualProbeOutcomeIfGeneration(session.ID, rearmed.Generation, VirtualProbeOutcomeVerified); err != nil {
		t.Fatalf("SetVirtualProbeOutcomeIfGeneration: %v", err)
	}
	if _, applied, err := m.ArmVirtualProbeReplacementForCurrentBinding(session.ID, candidateURI); err != nil || applied {
		t.Fatalf("terminal replacement = (_, %v, %v), want (false, nil)", applied, err)
	}
	live, err := m.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome != VirtualProbeOutcomeVerified {
		t.Fatalf("outcome = %q, want the reached verified verdict untouched", live.VirtualProbeOutcome)
	}

	// An unknown session writes nothing and reports absence.
	if _, applied, err := m.ArmVirtualProbeReplacementForCurrentBinding("missing", candidateURI); err != ErrSessionNotFound || applied {
		t.Fatalf("unknown-session replacement = (_, %v, %v), want (false, ErrSessionNotFound)", applied, err)
	}
}

// TestArmVirtualProbeReplacementRefusedAcrossRotation pins that a rotation which
// advances the generation clears the pending mark outright (see
// setVirtualSourceLocked), so a replacement for a candidate the session has left
// is refused rather than re-arming superseded work. This is the generation half of
// the fence the mid-probe A→B→A test exercises.
func TestArmVirtualProbeReplacementRefusedAcrossRotation(t *testing.T) {
	m := NewSessionManager(0, 0)
	session, err := m.StartSession(1, "profile-1", 1, PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	const firstURI = "virtual://movie/tt-rot-a?result=cand-a"
	if err := m.SetVirtualSource(session.ID, firstURI, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	if _, applied, err := m.ArmVirtualProbeReplacementForCurrentBinding(session.ID, firstURI); err != nil || !applied {
		t.Fatalf("initial arm = (_, %v, %v), want (true, nil)", applied, err)
	}
	// Rotate away: the first candidate is superseded and its mark is cleared.
	if err := m.SetVirtualSource(session.ID, "virtual://movie/tt-rot-b?result=cand-b", 5); err != nil {
		t.Fatalf("SetVirtualSource rotate: %v", err)
	}
	live, err := m.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome != "" {
		t.Fatalf("rotation left outcome %q, want cleared", live.VirtualProbeOutcome)
	}
	// The dropped candidate can no longer be re-armed.
	if _, applied, err := m.ArmVirtualProbeReplacementForCurrentBinding(session.ID, firstURI); err != nil || applied {
		t.Fatalf("superseded replacement = (_, %v, %v), want (false, nil)", applied, err)
	}
	// The current candidate can.
	if _, applied, err := m.ArmVirtualProbeReplacementForCurrentBinding(session.ID, "virtual://movie/tt-rot-b?result=cand-b"); err != nil || !applied {
		t.Fatalf("current replacement = (_, %v, %v), want (true, nil)", applied, err)
	}
}
