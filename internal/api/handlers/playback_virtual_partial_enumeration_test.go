package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/scanner"
)

// TestResolveVirtualPlaybackSourcePartialEnumerationHoldsPending pins the
// synchronous resolve: a probe that returns an incomplete stream table
// (scanner.ErrVirtualProbePartial) is not a verdict about the candidate's bytes,
// so the resolve must report pending (non-verified) and must not consume the
// probe-failure budget. Reporting failed here would condemn and rotate a
// candidate whose bytes were never measured to be bad.
func TestResolveVirtualPlaybackSourcePartialEnumerationHoldsPending(t *testing.T) {
	stored := provenanceResolveRow()
	probeKey := virtualProbeFailureKey(stored.FilePath, stored.VirtualOwnerInstallationID)
	virtualProbeFailures.clear(probeKey)
	t.Cleanup(func() { virtualProbeFailures.clear(probeKey) })

	h := virtualProvenanceResolution(stored, func(_ context.Context, _ string, _ *models.MediaFile) (*models.MediaFile, error) {
		return nil, scanner.ErrVirtualProbePartial
	})

	resolved, err := h.resolveVirtualPlaybackSource(provenanceResolveRequest(), stored, "profile-1", false, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if resolved.Provenance != ProbeProvenancePending {
		t.Fatalf("provenance = %q, want pending after a partial enumeration", resolved.Provenance)
	}
	if resolved.ProbeSucceeded {
		t.Fatal("a partial enumeration must not report a successful probe")
	}
	if got := virtualProbeFailures.count(probeKey); got != 0 {
		t.Fatalf("partial enumeration consumed the probe-failure budget (%d marks)", got)
	}
}

// TestDeferredProbePartialEnumerationStaysPending drives the post-commit durable
// probe with a partial enumeration. The result must not be persisted, must not
// be recorded as verified, and must not publish an inventory_updated event;
// the session stays pending so the convergence watchdog is the only thing that
// can end the lifecycle.
func TestDeferredProbePartialEnumerationStaysPending(t *testing.T) {
	fx := newDeferredLifecycleFixture(t, 951, "virtual://movie/tt-partial?result=cand-1")
	fx.saveAttempt(t)
	conn := fx.registerPush(t)

	probeKey := virtualProbeFailureKey("virtual://movie/tt-partial?result=cand-1", 5)
	virtualProbeFailures.clear(probeKey)
	t.Cleanup(func() { virtualProbeFailures.clear(probeKey) })

	fx.handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, _ *models.MediaFile) (*models.MediaFile, error) {
		return nil, scanner.ErrVirtualProbePartial
	}

	deferred := fx.job("virtual://movie/tt-partial?result=cand-1")
	if !fx.handler.armDeferredProbePending(context.Background(), deferred) {
		t.Fatal("admission rejected the fresh job")
	}

	outcome, verifiedFileID := fx.handler.probeVirtualSourceAndPersistWith(
		context.Background(),
		deferred.stickyKey, deferred.file, deferred.streamURL,
		*deferred.probeTransient, deferred.cand, deferred.expectedRuntimeMinutes, deferred.ownerID,
		true, nil,
	)
	if outcome != probeOutcomePartial {
		t.Fatalf("outcome = %q, want partial for an incomplete enumeration", outcome)
	}
	if verifiedFileID != 0 {
		t.Fatalf("verifiedFileID = %d, want 0: a partial probe commits nothing", verifiedFileID)
	}
	if got := fx.saverCallCount(); got != 0 {
		t.Fatalf("partial probe attempted %d evidence writes, want 0", got)
	}
	if got := virtualProbeFailures.count(probeKey); got != 0 {
		t.Fatalf("partial probe marked the failure damper %d times, want 0", got)
	}

	live, err := fx.manager.GetSession(fx.session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome != probeOutcomePending {
		t.Fatalf("session outcome = %q, want pending after a partial enumeration", live.VirtualProbeOutcome)
	}

	// A partial result writes no outcome and publishes nothing, so a verified (or
	// any) inventory_updated must not arrive before the watchdog runs.
	select {
	case event := <-conn.ch:
		t.Fatalf("partial enumeration published an inventory_updated (status %q)", event.payload.InventoryStatus)
	case <-time.After(150 * time.Millisecond):
	}

	// The convergence watchdog is the deadline: it terminally fails the
	// still-pending lifecycle and publishes the failure so a client leaves
	// loading.
	fx.handler.failOverdueDeferredProbes(time.Now().Add(time.Second))
	fx.pollInventoryStatus(t, string(ProbeProvenanceFailed))
	select {
	case event := <-conn.ch:
		if event.payload.InventoryStatus != string(ProbeProvenanceFailed) {
			t.Fatalf("watchdog push status = %q, want failed", event.payload.InventoryStatus)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the watchdog never published the failed terminal state")
	}
}

// TestDeferredProbeCompleteEnumerationVerifies guards the other direction: a
// complete enumeration still commits its evidence durably and reports verified,
// so the partial guard cannot silently disable the verified path.
func TestDeferredProbeCompleteEnumerationVerifies(t *testing.T) {
	fx := newDeferredLifecycleFixture(t, 952, "virtual://movie/tt-complete?result=cand-1")
	deferred := fx.job("virtual://movie/tt-complete?result=cand-1")
	if !fx.handler.armDeferredProbePending(context.Background(), deferred) {
		t.Fatal("admission rejected the fresh job")
	}
	// The fixture prober holds until released, then returns its complete audio
	// and subtitle inventory.
	close(fx.releaseProbe)

	outcome, verifiedFileID := fx.handler.probeVirtualSourceAndPersistWith(
		context.Background(),
		deferred.stickyKey, deferred.file, deferred.streamURL,
		*deferred.probeTransient, deferred.cand, deferred.expectedRuntimeMinutes, deferred.ownerID,
		true, nil,
	)
	if outcome != probeOutcomeVerified {
		t.Fatalf("outcome = %q, want verified for a complete enumeration", outcome)
	}
	if verifiedFileID != fx.session.MediaFileID {
		t.Fatalf("verifiedFileID = %d, want the committed row %d", verifiedFileID, fx.session.MediaFileID)
	}
	if got := fx.saverCallCount(); got != 1 {
		t.Fatalf("complete probe committed %d evidence writes, want 1", got)
	}
}
