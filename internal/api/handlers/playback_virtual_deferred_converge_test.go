package handlers

import (
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// TestDeferredProbeConvergenceWatchdogFailsStuckProbe is the point-1
// regression: a deferred probe that never reaches a terminal outcome must not
// leave the session pending forever. The lifecycle is armed pending (as a real
// cold start does at transport commit) and no worker ever finishes it. The
// convergence watchdog must fail the session within its bound, and the failure
// must be actionable on both the push and the poll, so a client leaves the
// loading state instead of waiting for a probe no worker will complete.
func TestDeferredProbeConvergenceWatchdogFailsStuckProbe(t *testing.T) {
	const fileID = 8200
	candidateURI := "virtual://movie/tt-converge-stuck?result=cand-1"
	fx := newDeferredLifecycleFixture(t, fileID, candidateURI)
	fx.saveAttempt(t)
	conn := fx.registerPush(t)

	// A short bound and cadence keep the assertion bounded without changing
	// production behavior; the override is the handler seam, not a package var.
	fx.handler.probeConvergenceOverride = 60 * time.Millisecond
	fx.handler.probeWatchdogIntervalOverride = 10 * time.Millisecond

	// Admission arms the lifecycle pending exactly as the cold start does after
	// the transport commit; no worker runs, standing in for a probe whose
	// provider never returns.
	deferred := readmitDeferredProbe(t, fx.handler, fx.job(candidateURI))
	if deferred.bindingGeneration == 0 {
		t.Fatal("admission did not capture a binding generation")
	}
	live, err := fx.manager.GetSession(fx.session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome != playback.VirtualProbeOutcomePending {
		t.Fatalf("armed outcome = %q, want pending", live.VirtualProbeOutcome)
	}

	fx.handler.startDeferredProbeWatchdog()

	// Push surface: the terminal failure must be delivered so a push-driven
	// client exits loading.
	select {
	case event := <-conn.ch:
		if event.payload.InventoryStatus != string(ProbeProvenanceFailed) {
			t.Fatalf("pushed inventory status = %q, want failed", event.payload.InventoryStatus)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("convergence watchdog did not fail the stuck probe within the bound")
	}

	// Poll surface: a client that never holds the push connection observes the
	// same terminal status.
	inventory := fx.pollInventoryStatus(t, string(ProbeProvenanceFailed))
	if inventory.InventoryStatus != string(ProbeProvenanceFailed) {
		t.Fatalf("poll status = %q, want failed", inventory.InventoryStatus)
	}
}

// TestDeferredProbeWatchdogDoesNotOverrideVerifiedOutcome is the never-regress
// half of the bound: the watchdog's failure write is a compare-and-swap from
// pending only, so a probe that reaches verified before the deadline keeps its
// verdict. A verified inventory is the success state the whole cold path exists
// to reach; failing it late would be strictly worse than leaving it pending.
func TestDeferredProbeWatchdogDoesNotOverrideVerifiedOutcome(t *testing.T) {
	const fileID = 8201
	candidateURI := "virtual://movie/tt-converge-verified?result=cand-1"
	fx := newDeferredLifecycleFixture(t, fileID, candidateURI)

	deferred := readmitDeferredProbe(t, fx.handler, fx.job(candidateURI))
	if _, err := fx.manager.SetVirtualProbeOutcomeIfGeneration(fx.session.ID, deferred.bindingGeneration, playback.VirtualProbeOutcomeVerified); err != nil {
		t.Fatalf("SetVirtualProbeOutcomeIfGeneration: %v", err)
	}

	// Force every pending session overdue: the verified session must survive.
	fx.handler.failOverdueDeferredProbes(time.Now().Add(time.Hour))

	live, err := fx.manager.GetSession(fx.session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome != playback.VirtualProbeOutcomeVerified {
		t.Fatalf("watchdog overwrote a verified outcome with %q", live.VirtualProbeOutcome)
	}
}

// TestDeferredProbeWatchdogIgnoresWarmSession is the point-4 regression: a warm
// resume never defers a probe, so its outcome is empty and the watchdog must
// leave it alone. The watchdog bounds only the pending state; treating an empty
// outcome as overdue would fail a perfectly final inventory.
func TestDeferredProbeWatchdogIgnoresWarmSession(t *testing.T) {
	const fileID = 8202
	candidateURI := "virtual://movie/tt-converge-warm?result=cand-1"
	fx := newDeferredLifecycleFixture(t, fileID, candidateURI)
	conn := fx.registerPush(t)

	// No arm: the session carries no deferred lifecycle, exactly like a warm
	// resume that planned from persisted probe evidence.
	fx.handler.failOverdueDeferredProbes(time.Now().Add(time.Hour))

	live, err := fx.manager.GetSession(fx.session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome != "" {
		t.Fatalf("watchdog touched a warm session outcome: %q", live.VirtualProbeOutcome)
	}
	select {
	case event := <-conn.ch:
		t.Fatalf("watchdog pushed an inventory event for a warm session: %#v", event.payload)
	case <-time.After(150 * time.Millisecond):
	}
}

// TestDeferredProbeWatchdogFailurePushAdoptsInPlace is the point-2 regression
// for the failure surface: the terminal notification the watchdog parks is
// delivered through the same realtime lane as every other inventory_updated
// event, for the same live session and effective release. There is no new
// session, no replan, and no transport replacement, so a client adopts the
// terminal state in place.
func TestDeferredProbeWatchdogFailurePushAdoptsInPlace(t *testing.T) {
	const fileID = 8203
	candidateURI := "virtual://movie/tt-converge-adopt?result=cand-1"
	fx := newDeferredLifecycleFixture(t, fileID, candidateURI)
	fx.saveAttempt(t)
	conn := fx.registerPush(t)

	readmitDeferredProbe(t, fx.handler, fx.job(candidateURI))
	fx.handler.failOverdueDeferredProbes(time.Now().Add(time.Hour))

	select {
	case event := <-conn.ch:
		if event.payload.InventoryStatus != string(ProbeProvenanceFailed) {
			t.Fatalf("pushed status = %q, want failed", event.payload.InventoryStatus)
		}
		if event.payload.EffectiveMediaFileID != fileID {
			t.Fatalf("pushed effective file = %d, want the bound row %d (same session, no replacement)",
				event.payload.EffectiveMediaFileID, fileID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no inventory_updated push after the watchdog failure")
	}

	// The session is intact: the failure is an inventory disposition, not a
	// session or transport replacement.
	live, err := fx.manager.GetSession(fx.session.ID)
	if err != nil {
		t.Fatalf("session vanished after the watchdog failure: %v", err)
	}
	if live.ID != fx.session.ID {
		t.Fatalf("session id changed %q -> %q", fx.session.ID, live.ID)
	}
}

// TestDeferredProbeWatchdogFailureKeepsProvisionalSelection is the point-3
// regression for the failure surface: when a probe fails terminally the
// provisional declared inventory stays selectable and the committed selection
// is untouched. A terminal failure is not a reason to move the viewer's audio
// index; the correction path runs only on a verified commit.
func TestDeferredProbeWatchdogFailureKeepsProvisionalSelection(t *testing.T) {
	const fileID = 8204
	candidateURI := "virtual://movie/tt-converge-selection?result=cand-1"
	fx := newDeferredLifecycleFixture(t, fileID, candidateURI)
	planTime := []models.AudioTrack{
		{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo", Default: true},
		{Index: 3, Language: "pt", Codec: "eac3", Channels: 6, Layout: "5.1"},
	}
	if err := fx.manager.UpdateStreamState(fx.session.ID, playback.SessionStreamState{
		AudioTrackIndex:            1,
		VirtualSourceURI:           candidateURI,
		VirtualSourceSet:           true,
		VirtualSubtitleEvidenceSet: true,
		VirtualAudioTracks:         planTime,
	}); err != nil {
		t.Fatalf("UpdateStreamState: %v", err)
	}

	readmitDeferredProbe(t, fx.handler, fx.job(candidateURI))
	fx.handler.failOverdueDeferredProbes(time.Now().Add(time.Hour))

	live, err := fx.manager.GetSession(fx.session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.AudioTrackIndex != 1 {
		t.Fatalf("watchdog failure moved the committed audio index to %d, want 1", live.AudioTrackIndex)
	}
	if len(live.VirtualAudioTracks) == 0 {
		t.Fatal("provisional declared audio inventory was cleared by the watchdog failure")
	}
}

// TestDeferredProbeConvergenceBoundDocumentsBudget pins the documented bound,
// so a change to the probe budget or the slack is a visible decision rather
// than a silent shift in how long a client may wait.
func TestDeferredProbeConvergenceBoundDocumentsBudget(t *testing.T) {
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	if got, want := h.probeConvergenceBound(), virtualBackgroundProbeBudget+virtualProbeConvergenceSlack; got != want {
		t.Fatalf("default convergence bound = %s, want %s", got, want)
	}
	if h.probeConvergenceBound() > 2*time.Minute {
		t.Fatalf("convergence bound %s exceeds a client-tolerable wait", h.probeConvergenceBound())
	}
}
