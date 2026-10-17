package playback

import (
	"errors"
	"testing"
	"time"
)

// TestArmVirtualProbeReplacementOnlyForUnOwnedCurrentBinding pins the
// dropped-probe recovery fence. A replacement may only arm the session's CURRENT
// binding, and only when that binding carries no live lifecycle of its own: an
// absent mark, or a stale one left by a row-only move that never advanced the
// generation. A live pending (a worker will land it), a terminal verdict, a
// candidate the caller cannot probe, the same candidate under a DIFFERENT owner,
// and an unknown session are all refused without a write, so a replacement never
// double-arms, clobbers a reached outcome, or reuses input resolved for another
// source.
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
	if binding, applied, err := m.ArmVirtualProbeReplacementForCurrentBinding(session.ID, "virtual://movie/tt-other?result=cand-2", 5); err != nil || applied {
		t.Fatalf("candidate-mismatch arm = (%#v, %v, %v), want (_, false, nil)", binding, applied, err)
	}

	// The same candidate served by a different owner is a different source behind
	// the candidate URI: the dropped job's input was resolved for another
	// installation, so it cannot describe the current binding and must not arm.
	if binding, applied, err := m.ArmVirtualProbeReplacementForCurrentBinding(session.ID, candidateURI, 9); err != nil || applied {
		t.Fatalf("owner-mismatch arm = (%#v, %v, %v), want (_, false, nil)", binding, applied, err)
	}

	// No mark at all: the current binding has no probe, so the replacement owns it.
	binding, applied, err := m.ArmVirtualProbeReplacementForCurrentBinding(session.ID, candidateURI, 5)
	if err != nil || !applied {
		t.Fatalf("unarmed replacement = (%#v, %v, %v), want (_, true, nil)", binding, applied, err)
	}
	if binding.Generation == 0 || binding.EffectiveMediaFileID != 1 {
		t.Fatalf("armed binding = %#v, want the current generation and effective row", binding)
	}
	// The armed mark is live: a second replacement attempt must not duplicate it.
	if _, applied, err := m.ArmVirtualProbeReplacementForCurrentBinding(session.ID, candidateURI, 5); err != nil || applied {
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
	rearmed, applied, err := m.ArmVirtualProbeReplacementForCurrentBinding(session.ID, candidateURI, 5)
	if err != nil || !applied {
		t.Fatalf("stale replacement = (%#v, %v, %v), want (_, true, nil)", rearmed, applied, err)
	}
	if rearmed.EffectiveMediaFileID != 2 {
		t.Fatalf("replacement armed the wrong row: %#v, want row 2", rearmed)
	}
	// The replacement is now the live owner: a further attempt is refused.
	if _, applied, err := m.ArmVirtualProbeReplacementForCurrentBinding(session.ID, candidateURI, 5); err != nil || applied {
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
	if _, applied, err := m.ArmVirtualProbeReplacementForCurrentBinding(session.ID, candidateURI, 5); err != nil || applied {
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
	if _, applied, err := m.ArmVirtualProbeReplacementForCurrentBinding("missing", candidateURI, 5); !errors.Is(err, ErrSessionNotFound) || applied {
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
	if _, applied, err := m.ArmVirtualProbeReplacementForCurrentBinding(session.ID, firstURI, 5); err != nil || !applied {
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
	if _, applied, err := m.ArmVirtualProbeReplacementForCurrentBinding(session.ID, firstURI, 5); err != nil || applied {
		t.Fatalf("superseded replacement = (_, %v, %v), want (false, nil)", applied, err)
	}
	// The current candidate can.
	if _, applied, err := m.ArmVirtualProbeReplacementForCurrentBinding(session.ID, "virtual://movie/tt-rot-b?result=cand-b", 5); err != nil || !applied {
		t.Fatalf("current replacement = (_, %v, %v), want (true, nil)", applied, err)
	}
}

// TestFailVirtualProbeReplacementAtomicAcrossRowMoves is the B -> C -> B barrier
// for the saturated fallback. The fallback must arm the current binding and record
// the failure in ONE critical section: a row-only move does not advance the
// generation, so if the arm and the write were separate calls a B -> C -> B move
// could let the failure land on a NEWER B lifecycle another recovery armed after
// the row returned. The test pins both halves of the stronger contract — the
// failure is committed atomically (never a lingering pending mark a delayed write
// could still target), and it refuses to overwrite a live worker-owned pending mark
// even after the same row value is restored.
func TestFailVirtualProbeReplacementAtomicAcrossRowMoves(t *testing.T) {
	m := NewSessionManager(0, 0)
	session, err := m.StartSession(1, "profile-1", 900, PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	const candidateURI = "virtual://movie/tt-fallback-aba?result=cand-1"
	if err := m.SetVirtualSource(session.ID, candidateURI, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}

	// The fallback arms and fails the current binding atomically. The outcome is
	// terminal the moment the call returns, so there is no pending window in which
	// a later move could misattribute the write.
	binding, applied, err := m.FailVirtualProbeReplacementForCurrentBinding(session.ID, candidateURI, 5)
	if err != nil || !applied {
		t.Fatalf("fallback fail = (%#v, %v, %v), want (_, true, nil)", binding, applied, err)
	}
	if binding.EffectiveMediaFileID != 900 {
		t.Fatalf("fallback binding row = %d, want 900", binding.EffectiveMediaFileID)
	}
	live, err := m.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome != VirtualProbeOutcomeFailed {
		t.Fatalf("outcome = %q, want failed committed with the arm", live.VirtualProbeOutcome)
	}

	// B -> C -> B: a row-only move out and back leaves the generation unchanged.
	if err := m.SetEffectiveMediaFileID(session.ID, 901); err != nil {
		t.Fatalf("SetEffectiveMediaFileID C: %v", err)
	}
	if err := m.SetEffectiveMediaFileID(session.ID, 900); err != nil {
		t.Fatalf("SetEffectiveMediaFileID B: %v", err)
	}
	// The returned-to B cannot be handed a fresh lifecycle for a delayed fallback
	// write to stomp: the failure already owns the binding.
	if _, applied, err := m.ArmVirtualProbeReplacementForCurrentBinding(session.ID, candidateURI, 5); err != nil || applied {
		t.Fatalf("post-ABA arm = (_, %v, %v), want (false, nil): a terminal outcome owns the binding", applied, err)
	}
	live, err = m.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome != VirtualProbeOutcomeFailed {
		t.Fatalf("outcome after ABA = %q, want failed still", live.VirtualProbeOutcome)
	}

	// The ownership guarantee also covers a LIVE worker-owned pending: the
	// fallback must not overwrite a lifecycle a worker will land, even when the
	// row has moved away and come back to the same value.
	second, err := m.StartSession(1, "profile-1", 910, PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession second: %v", err)
	}
	if err := m.SetVirtualSource(second.ID, candidateURI, 5); err != nil {
		t.Fatalf("SetVirtualSource second: %v", err)
	}
	if _, applied, err := m.ArmVirtualProbeReplacementForCurrentBinding(second.ID, candidateURI, 5); err != nil || !applied {
		t.Fatalf("second arm = (_, %v, %v), want (true, nil)", applied, err)
	}
	if err := m.SetEffectiveMediaFileID(second.ID, 911); err != nil {
		t.Fatalf("SetEffectiveMediaFileID second C: %v", err)
	}
	if err := m.SetEffectiveMediaFileID(second.ID, 910); err != nil {
		t.Fatalf("SetEffectiveMediaFileID second B: %v", err)
	}
	if _, applied, err := m.FailVirtualProbeReplacementForCurrentBinding(second.ID, candidateURI, 5); err != nil || applied {
		t.Fatalf("live-owner fallback fail = (_, %v, %v), want (false, nil): a worker still owns the binding", applied, err)
	}
	liveSecond, err := m.GetSession(second.ID)
	if err != nil {
		t.Fatalf("GetSession second: %v", err)
	}
	if liveSecond.VirtualProbeOutcome != VirtualProbeOutcomePending {
		t.Fatalf("outcome second = %q, want the worker's pending mark untouched", liveSecond.VirtualProbeOutcome)
	}
}

// TestSetVirtualProbeOutcomeIfLifecycleFencesRowABA pins the ownership fence the
// replacement shed needs. Two arms for the same binding (a B -> C -> B row-only
// move between them) produce byte-for-byte equal snapshots, so binding equality
// alone cannot tell them apart; the lifecycle token does. Only the current
// lifecycle's token may write.
func TestSetVirtualProbeOutcomeIfLifecycleFencesRowABA(t *testing.T) {
	m := NewSessionManager(0, 0)
	session, err := m.StartSession(1, "profile-1", 730, PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	const candidateURI = "virtual://movie/tt-lifecycle-aba?result=cand-1"
	if err := m.SetVirtualSource(session.ID, candidateURI, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}

	first, applied, err := m.ArmVirtualProbeReplacementForCurrentBinding(session.ID, candidateURI, 5)
	if err != nil || !applied {
		t.Fatalf("first arm = (%#v, %v, %v), want applied", first, applied, err)
	}
	if first.ProbeLifecycle == 0 {
		t.Fatal("first arm did not stamp a lifecycle token")
	}
	// A row-only move out and back leaves the binding byte-for-byte equal at a
	// new lifecycle.
	if err := m.SetEffectiveMediaFileID(session.ID, 731); err != nil {
		t.Fatalf("SetEffectiveMediaFileID C: %v", err)
	}
	if _, applied, err := m.ArmVirtualProbeReplacementForCurrentBinding(session.ID, candidateURI, 5); err != nil || !applied {
		t.Fatalf("arm C = (_, %v, %v), want applied", applied, err)
	}
	if err := m.SetEffectiveMediaFileID(session.ID, 730); err != nil {
		t.Fatalf("SetEffectiveMediaFileID B: %v", err)
	}
	second, applied, err := m.ArmVirtualProbeReplacementForCurrentBinding(session.ID, candidateURI, 5)
	if err != nil || !applied {
		t.Fatalf("second arm = (%#v, %v, %v), want applied", second, applied, err)
	}
	if second.ProbeLifecycle == first.ProbeLifecycle {
		t.Fatal("the returned-to binding reused the earlier lifecycle token; ABA is indistinguishable")
	}

	// The superseded first lifecycle must not write over the newer one.
	applied, superseded, err := m.SetVirtualProbeOutcomeIfLifecycle(session.ID, first.ProbeLifecycle, first, VirtualProbeOutcomeFailed)
	if err != nil || applied || !superseded {
		t.Fatalf("stale lifecycle write = (%v, %v, %v), want (false, true, nil)", applied, superseded, err)
	}
	live, err := m.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome != VirtualProbeOutcomePending {
		t.Fatalf("outcome = %q, want the newer pending lifecycle untouched", live.VirtualProbeOutcome)
	}

	// The current lifecycle's token writes.
	applied, superseded, err = m.SetVirtualProbeOutcomeIfLifecycle(session.ID, second.ProbeLifecycle, second, VirtualProbeOutcomeFailed)
	if err != nil || !applied || superseded {
		t.Fatalf("current lifecycle write = (%v, %v, %v), want (true, false, nil)", applied, superseded, err)
	}
	live, err = m.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome != VirtualProbeOutcomeFailed {
		t.Fatalf("outcome = %q, want failed", live.VirtualProbeOutcome)
	}
}
