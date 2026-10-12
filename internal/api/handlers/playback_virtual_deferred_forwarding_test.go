package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

// TestDeferredProbeThinRowDeclaredAudioHoldsPending reproduces the production
// cold-start timeline through the real post-commit deferred path: a thin row
// with no audio metadata and a candidate that declares audio only through its
// language list. The deferred worker must carry the declaration onto its probe
// input before the completeness gate sees it; the gate then reports the
// video-with-subtitles-but-no-audio enumeration as a partial read, so the
// session stays pending, nothing is persisted, and nothing is published as
// verified. Only a later enumeration that actually carries probed audio
// verifies.
//
// The probe input is the row alone, exactly as the deferred job is built at the
// clone (see the allowDefer branch and virtualDeferredProbeForUnstampedRow):
// without the forward the gate would see expectsAudio=false, accept the thin
// enumeration as complete, and the caller would merge synthesized audio onto it
// and publish it as a verified inventory.
func TestDeferredProbeThinRowDeclaredAudioHoldsPending(t *testing.T) {
	const uri = "virtual://series/tt-thin-deferred?result=cand-1"
	fx := newDeferredLifecycleFixture(t, 957, uri)
	fx.saveAttempt(t)
	conn := fx.registerPush(t)

	prober, thinCompletions := gateAwareThinProber()
	fx.handler.VirtualPlaybackSourceProber = prober

	probeKey := virtualProbeFailureKey(uri, 5)
	virtualProbeFailures.clear(probeKey)
	virtualProbeFailures.clearPartial(probeKey)
	t.Cleanup(func() {
		virtualProbeFailures.clear(probeKey)
		virtualProbeFailures.clearPartial(probeKey)
	})

	// The deferred job as production builds it: the probe input is the row
	// alone (no audio metadata), while the candidate declares audio only
	// through its language list.
	deferred := fx.job(uri)
	deferred.probeTransient = &models.MediaFile{ID: fx.session.MediaFileID}
	deferred.cand = VirtualPlaybackStream{ID: "cand-1", URI: uri, AudioLanguages: []string{"POR"}}

	if !fx.handler.armDeferredProbePending(context.Background(), deferred) {
		t.Fatal("admission rejected the fresh deferred job")
	}
	fx.handler.runDeferredVirtualProbe(deferred)

	if *thinCompletions != 0 {
		t.Fatalf("the gate missed the declared audio: %d thin enumeration(s) accepted as complete", *thinCompletions)
	}
	if got := fx.saverCallCount(); got != 0 {
		t.Fatalf("a partial enumeration wrote evidence %d time(s), want 0", got)
	}
	live, err := fx.manager.GetSession(fx.session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome == probeOutcomeVerified {
		t.Fatalf("session outcome = %q: a thin video-without-audio enumeration was published as verified", live.VirtualProbeOutcome)
	}
	if live.VirtualProbeOutcome != probeOutcomePending {
		t.Fatalf("session outcome = %q, want pending after a partial enumeration", live.VirtualProbeOutcome)
	}
	select {
	case event := <-conn.ch:
		t.Fatalf("partial enumeration published an inventory_updated (status %q)", event.payload.InventoryStatus)
	case <-time.After(150 * time.Millisecond):
	}

	// The probed audio lands on a later enumeration: the same deferred tail now
	// verifies, so the hold is a retry state, not a terminal failure. The tail is
	// driven directly here because the first job already armed the binding's
	// pending lifecycle (arming is a from-empty fence).
	fx.handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		out := *f
		out.VideoTracks = []models.VideoTrack{{Codec: "hevc", Width: 3840, Height: 2160}}
		out.AudioTracks = []models.AudioTrack{{Codec: "eac3", Channels: 6, Language: "por", Default: true}}
		out.SubtitleTracks = []models.SubtitleTrack{{Index: 2, Codec: "ass", Language: "por"}}
		return &out, nil
	}
	verified := fx.job(uri)
	verified.probeTransient = &models.MediaFile{ID: fx.session.MediaFileID}
	verified.cand = VirtualPlaybackStream{ID: "cand-1", URI: uri, AudioLanguages: []string{"POR"}}
	outcome, verifiedFileID := fx.handler.probeVirtualSourceAndPersistWith(
		context.Background(),
		verified.stickyKey, verified.file, verified.streamURL,
		*verified.probeTransient, verified.cand, verified.expectedRuntimeMinutes, verified.ownerID,
		true, nil,
	)
	if outcome != probeOutcomeVerified {
		t.Fatalf("later enumeration outcome = %q, want verified once probed audio lands", outcome)
	}
	if verifiedFileID != fx.session.MediaFileID {
		t.Fatalf("verifiedFileID = %d, want the committed row %d", verifiedFileID, fx.session.MediaFileID)
	}
	if got := fx.saverCallCount(); got != 1 {
		t.Fatalf("complete probe committed %d evidence writes, want 1", got)
	}
}

// TestDeferredJobProbeInputCarriesDeclaredAudio pins the construction-site half
// of the deferred-path fix: the resolve builds the post-commit job from the plan
// transient, and the job's probe input must already carry the served candidate's
// declared audio before it can reach the probe-cache key or the completeness
// gate. The candidate here declares audio only through its language list and the
// row carries no audio metadata, which is the production cold-start shape.
func TestDeferredJobProbeInputCarriesDeclaredAudio(t *testing.T) {
	stored := provenanceResolveRow()
	prober, _ := gateAwareThinProber()
	h := virtualProvenanceResolution(stored, prober)
	h.VirtualPlaybackStreamLister = VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
		return []VirtualPlaybackStream{{
			ID: "cand-1", URI: stored.FilePath,
			Resolution: "1080p", CodecVideo: "h264", Container: "mkv",
			AudioLanguages: []string{"POR"},
		}}, nil
	})
	probeKey := virtualProbeFailureKey(stored.FilePath, stored.VirtualOwnerInstallationID)
	virtualProbeFailures.clear(probeKey)
	virtualProbeFailures.clearPartial(probeKey)
	t.Cleanup(func() {
		virtualProbeFailures.clear(probeKey)
		virtualProbeFailures.clearPartial(probeKey)
	})

	resolved, err := h.resolveVirtualPlaybackSource(
		provenanceResolveRequest(), stored, "profile-1", true, nil, "", "", 0, false,
		virtualResolveOptionsV3{deferProbePastCommit: true},
	)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if resolved.DeferredProbe == nil {
		t.Fatal("the fresh-start resolve did not hand back a deferred probe")
	}
	input := resolved.DeferredProbe.probeTransient
	if input == nil {
		t.Fatal("the deferred job carries no probe input")
	}
	if input.CodecAudio == "" && len(input.AudioTracks) == 0 {
		t.Fatal("the deferred probe input carries no expected-audio signal: the completeness gate would accept a video-without-audio enumeration as complete")
	}
}
