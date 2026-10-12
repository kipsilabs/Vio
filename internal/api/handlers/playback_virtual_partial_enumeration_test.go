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
//
// The candidate deliberately declares no audio: a candidate with complete
// declared metadata takes the instant declared-evidence start above this probe
// instead, so an incomplete-declaration candidate is what exercises the
// synchronous probe path.
func TestResolveVirtualPlaybackSourcePartialEnumerationHoldsPending(t *testing.T) {
	stored := provenanceResolveRow()
	probeKey := virtualProbeFailureKey(stored.FilePath, stored.VirtualOwnerInstallationID)
	virtualProbeFailures.clear(probeKey)
	t.Cleanup(func() { virtualProbeFailures.clear(probeKey) })

	h := virtualProvenanceResolution(stored, func(_ context.Context, _ string, _ *models.MediaFile) (*models.MediaFile, error) {
		return nil, scanner.ErrVirtualProbePartial
	})
	h.VirtualPlaybackStreamLister = VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
		return []VirtualPlaybackStream{{
			ID: "cand-1", URI: stored.FilePath,
			Resolution: "1080p", CodecVideo: "h264", Container: "mkv",
		}}, nil
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

// TestBackgroundProbePartialEnumerationHasBoundedStreak pins the
// non-negotiated background path's bound. That path has no session lifecycle
// for the convergence watchdog to fail, so a partial enumeration must be
// bounded by a repeated-attempt streak: a single partial is not a verdict (the
// pin is kept and the failure damper stays clear), but a persistent streak arms
// the failure damper so the resolve stops re-probing and settles on the
// candidate's declared metadata instead of staying pending forever.
func TestBackgroundProbePartialEnumerationHasBoundedStreak(t *testing.T) {
	probeKey := virtualProbeFailureKey("virtual://movie/tt-bounded?result=cand-1", 5)
	virtualProbeFailures.clear(probeKey)
	virtualProbeFailures.clearPartial(probeKey)
	t.Cleanup(func() {
		virtualProbeFailures.clear(probeKey)
		virtualProbeFailures.clearPartial(probeKey)
	})

	h := &PlaybackHandler{
		VirtualPlaybackSourceProber: func(_ context.Context, _ string, _ *models.MediaFile) (*models.MediaFile, error) {
			return nil, scanner.ErrVirtualProbePartial
		},
	}
	cand := VirtualPlaybackStream{ID: "cand-1", URI: "virtual://movie/tt-bounded?result=cand-1"}
	file := &models.MediaFile{ID: 940, FilePath: cand.URI, VirtualOwnerInstallationID: 5}
	transient := models.MediaFile{ID: 940, FilePath: cand.URI}

	run := func() string {
		return h.probeVirtualSourceAndPersist(context.Background(), "sticky-key", file, "http://127.0.0.1/x", transient, cand, 0, 5)
	}

	// The first partial is an incomplete read, not a verdict about the bytes.
	if outcome := run(); outcome != probeOutcomePartial {
		t.Fatalf("outcome = %q, want partial", outcome)
	}
	if virtualProbeFailures.recent(probeKey) {
		t.Fatal("a single partial enumeration armed the failure damper")
	}

	// The streak reaches the bound: the damper arms so the non-negotiated
	// background path cannot re-probe an incomplete source forever.
	for i := 1; i < virtualProbePartialRepeatThreshold; i++ {
		if outcome := run(); outcome != probeOutcomePartial {
			t.Fatalf("attempt %d outcome = %q, want partial", i+1, outcome)
		}
	}
	if !virtualProbeFailures.recent(probeKey) {
		t.Fatalf("a %d-attempt partial streak did not arm the failure damper", virtualProbePartialRepeatThreshold)
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
