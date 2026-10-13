package handlers

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/scanner"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/resolver"
)

// TestResolveVirtualPlaybackSourcePartialEnumerationSettlesDeclared pins the
// synchronous resolve: a probe that returns an incomplete stream table
// (scanner.ErrVirtualProbePartial) is not a verdict about the candidate's bytes,
// so the resolve must not consume the probe-failure budget and must not release
// the pin. This synchronous path has no deferred session lifecycle, so pending
// would never reach a terminal state; the resolve instead settles on the
// candidate's declared metadata (non-verified) immediately, and the
// incomplete-enumeration streak bounds re-probing across starts. Reporting
// failed here would condemn and rotate a candidate whose bytes were never
// measured to be bad.
//
// The candidate deliberately declares no audio: a candidate with complete
// declared metadata takes the instant declared-evidence start above this probe
// instead, so an incomplete-declaration candidate is what exercises the
// synchronous probe path.
func TestResolveVirtualPlaybackSourcePartialEnumerationSettlesDeclared(t *testing.T) {
	stored := provenanceResolveRow()
	probeKey := virtualProbeFailureKey(stored.FilePath, stored.VirtualOwnerInstallationID)
	virtualProbeFailures.clear(probeKey)
	virtualProbeFailures.clearPartial(probeKey)
	t.Cleanup(func() {
		virtualProbeFailures.clear(probeKey)
		virtualProbeFailures.clearPartial(probeKey)
	})

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
	if resolved.Provenance != ProbeProvenanceDeclared {
		t.Fatalf("provenance = %q, want declared (terminal non-verified) after a partial enumeration", resolved.Provenance)
	}
	if !resolved.IncompleteEnumeration {
		t.Fatal("a partial enumeration must be marked as an incomplete-enumeration settlement")
	}
	if resolved.ProbeSucceeded {
		t.Fatal("a partial enumeration must not report a successful probe")
	}
	if got := virtualProbeFailures.count(probeKey); got != 0 {
		t.Fatalf("partial enumeration consumed the probe-failure budget (%d marks)", got)
	}
	if virtualProbeFailures.recent(probeKey) {
		t.Fatal("a single partial enumeration armed the failure damper")
	}
}

// TestSyncProbePartialEnumerationHasBoundedStreak pins the synchronous resolve's
// bound. That path has no session lifecycle for the convergence watchdog to
// fail, so a persistent partial must arm the failure damper after a repeated
// streak; only then do later starts stop re-probing and reuse the declared
// disposition instead of holding a start pending forever.
func TestSyncProbePartialEnumerationHasBoundedStreak(t *testing.T) {
	stored := provenanceResolveRow()
	probeKey := virtualProbeFailureKey(stored.FilePath, stored.VirtualOwnerInstallationID)
	virtualProbeFailures.clear(probeKey)
	virtualProbeFailures.clearPartial(probeKey)
	t.Cleanup(func() {
		virtualProbeFailures.clear(probeKey)
		virtualProbeFailures.clearPartial(probeKey)
	})

	proberCalls := 0
	h := virtualProvenanceResolution(stored, func(_ context.Context, _ string, _ *models.MediaFile) (*models.MediaFile, error) {
		proberCalls++
		return nil, scanner.ErrVirtualProbePartial
	})
	h.VirtualPlaybackStreamLister = VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
		return []VirtualPlaybackStream{{
			ID: "cand-1", URI: stored.FilePath,
			Resolution: "1080p", CodecVideo: "h264", Container: "mkv",
		}}, nil
	})

	for i := 0; i < virtualProbePartialRepeatThreshold; i++ {
		resolved, err := h.resolveVirtualPlaybackSource(provenanceResolveRequest(), stored, "profile-1", false, nil, "", "", 0, false)
		if err != nil {
			t.Fatalf("resolve %d error: %v", i, err)
		}
		if resolved.Provenance != ProbeProvenanceDeclared {
			t.Fatalf("resolve %d provenance = %q, want declared", i, resolved.Provenance)
		}
	}
	if !virtualProbeFailures.recent(probeKey) {
		t.Fatalf("a %d-attempt sync partial streak did not arm the failure damper", virtualProbePartialRepeatThreshold)
	}
	// The bound: once the streak arms the damper, later starts take the
	// damper's cache-only recovery path and never re-probe.
	before := proberCalls
	if _, err := h.resolveVirtualPlaybackSource(provenanceResolveRequest(), stored, "profile-1", false, nil, "", "", 0, false); err != nil {
		t.Fatalf("post-bound resolve error: %v", err)
	}
	if proberCalls != before {
		t.Fatalf("sync prober ran %d more time(s) after the bound; want 0", proberCalls-before)
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

	outcome, committed := fx.handler.probeVirtualSourceAndPersistWith(
		context.Background(),
		deferred.stickyKey, deferred.file, deferred.streamURL,
		*deferred.probeTransient, deferred.cand, resolver.PersistedIdentityTiers{}, deferred.expectedRuntimeMinutes, deferred.ownerID,
		true, nil,
	)
	if outcome != probeOutcomePartial {
		t.Fatalf("outcome = %q, want partial for an incomplete enumeration", outcome)
	}
	if committed.committed() {
		t.Fatalf("committed.row = %d, want no commit: a partial probe commits nothing", committed.fileID)
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
		return h.probeVirtualSourceAndPersist(context.Background(), "sticky-key", file, "http://127.0.0.1/x", transient, cand, resolver.PersistedIdentityTiers{}, 0, 5)
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

// gateAwareThinProber mirrors scanner.ProbeVirtualSource's completeness gate for
// a video-with-subtitles-but-no-audio enumeration: the result is partial exactly
// when the probe input carries the expected-audio signal. Otherwise the thin
// enumeration is a "complete" answer, which is what the production build did
// before the gate existed.
func gateAwareThinProber() (VirtualPlaybackSourceProber, *int) {
	calls := new(int)
	return func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		expectAudio := strings.TrimSpace(f.CodecAudio) != "" || len(f.AudioTracks) > 0
		if expectAudio {
			return nil, scanner.ErrVirtualProbePartial
		}
		out := *f
		out.VideoTracks = []models.VideoTrack{{Codec: "hevc", Width: 3840, Height: 2160}}
		out.SubtitleTracks = []models.SubtitleTrack{{Index: 2, Codec: "ass", Language: "fr"}}
		*calls++
		return &out, nil
	}, calls
}

// TestBackgroundProbeForwardsCandidateAudioBeforeGate pins the
// four-second-thin-verify timeline at its verified-setting site. A session is
// bound to a candidate that declares audio, but the background/rotation probe
// input is the catalog row alone (no audio declaration). scanner.ProbeVirtualSource
// can only judge a video-with-subtitles-but-no-audio enumeration partial when the
// probe input carries the item's expected audio; before the fix the handler
// forwarded none, the gate missed, and the thin enumeration was persisted and
// reported verified. The probe helper behaves exactly like the real gate, so a
// non-partial outcome here is the bug.
func TestBackgroundProbeForwardsCandidateAudioBeforeGate(t *testing.T) {
	prober, thinCompletions := gateAwareThinProber()
	writes := 0
	h := &PlaybackHandler{
		VirtualPlaybackSourceProber: prober,
		VirtualFileMetadataSaver: func(context.Context, models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
			writes++
			return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
		},
	}

	cand := VirtualPlaybackStream{
		ID: "cand-1", URI: "virtual://movie/tt-thin?result=cand-1",
		CodecAudio: "aac", AudioLanguages: []string{"FRE"},
	}
	probeKey := virtualProbeFailureKey(cand.URI, 5)
	virtualProbeFailures.clear(probeKey)
	virtualProbeFailures.clearPartial(probeKey)
	t.Cleanup(func() {
		virtualProbeFailures.clear(probeKey)
		virtualProbeFailures.clearPartial(probeKey)
	})

	file := &models.MediaFile{ID: 941, FilePath: cand.URI, VirtualOwnerInstallationID: 5}
	// The probe input the rotation/background path builds: the catalog row alone.
	rowOnly := models.MediaFile{ID: 941, FilePath: cand.URI}

	outcome := h.probeVirtualSourceAndPersist(context.Background(), "sticky-thin", file, "http://127.0.0.1/x", rowOnly, cand, resolver.PersistedIdentityTiers{}, 0, 5)
	if outcome != probeOutcomePartial {
		t.Fatalf("outcome = %q, want partial: a candidate that declares audio must not verify a video-without-audio enumeration", outcome)
	}
	if *thinCompletions != 0 {
		t.Fatalf("the gate missed the declared audio: %d thin enumeration(s) accepted as complete", *thinCompletions)
	}
	if writes != 0 {
		t.Fatalf("thin enumeration wrote evidence %d time(s), want 0", writes)
	}
	if virtualProbeFailures.recent(probeKey) {
		t.Fatal("a single partial enumeration armed the failure damper")
	}
}

// TestResolveVirtualPlaybackSyncForwardsCandidateAudioBeforeGate pins the
// synchronous-resolve half of the same hole: the sync probe transient is built
// before the candidate's declared metadata is merged, so a candidate that
// declares audio only through its language list reaches the prober without an
// audio signal. The gate-aware prober returns the real gate's verdict, so the
// resolve must settle on declared metadata (non-verified) instead of adopting a
// thin verified inventory.
func TestResolveVirtualPlaybackSyncForwardsCandidateAudioBeforeGate(t *testing.T) {
	stored := provenanceResolveRow()
	prober, thinCompletions := gateAwareThinProber()
	h := virtualProvenanceResolution(stored, prober)
	h.VirtualPlaybackStreamLister = VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
		return []VirtualPlaybackStream{{
			ID: "cand-1", URI: stored.FilePath,
			Resolution: "1080p", CodecVideo: "h264", Container: "mkv",
			AudioLanguages: []string{"FRE"},
		}}, nil
	})
	probeKey := virtualProbeFailureKey(stored.FilePath, stored.VirtualOwnerInstallationID)
	virtualProbeFailures.clear(probeKey)
	virtualProbeFailures.clearPartial(probeKey)
	t.Cleanup(func() {
		virtualProbeFailures.clear(probeKey)
		virtualProbeFailures.clearPartial(probeKey)
	})

	resolved, err := h.resolveVirtualPlaybackSource(provenanceResolveRequest(), stored, "profile-1", false, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if resolved.Provenance != ProbeProvenanceDeclared {
		t.Fatalf("provenance = %q, want declared (terminal non-verified) after a partial enumeration", resolved.Provenance)
	}
	if resolved.ProbeSucceeded {
		t.Fatal("a partial enumeration must not report a successful probe")
	}
	if *thinCompletions != 0 {
		t.Fatalf("the gate missed the declared audio: %d thin enumeration(s) accepted as complete", *thinCompletions)
	}
	if got := virtualProbeFailures.count(probeKey); got != 0 {
		t.Fatalf("partial enumeration consumed the probe-failure budget (%d marks)", got)
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

	outcome, committed := fx.handler.probeVirtualSourceAndPersistWith(
		context.Background(),
		deferred.stickyKey, deferred.file, deferred.streamURL,
		*deferred.probeTransient, deferred.cand, resolver.PersistedIdentityTiers{}, deferred.expectedRuntimeMinutes, deferred.ownerID,
		true, nil,
	)
	if outcome != probeOutcomeVerified {
		t.Fatalf("outcome = %q, want verified for a complete enumeration", outcome)
	}
	if committed.fileID != fx.session.MediaFileID {
		t.Fatalf("committed.row = %d, want the committed row %d", committed.fileID, fx.session.MediaFileID)
	}
	if got := fx.saverCallCount(); got != 1 {
		t.Fatalf("complete probe committed %d evidence writes, want 1", got)
	}
}

// TestDeferredProbeDeclaredAudioHoldsPendingUntilProbed pins the four-second
// thin-verify timeline against the real enumerator. The row declares FRE audio,
// so a probe that returns video and one forced French subtitle but no audio is a
// partial read: the deferred probe must hold the session pending, write no
// evidence, and arm no failure damper. Only a later enumeration that actually
// carries probed audio verifies.
func TestDeferredProbeDeclaredAudioHoldsPendingUntilProbed(t *testing.T) {
	const uri = "virtual://series/tt-declared-audio?result=cand-1"
	fx := newDeferredLifecycleFixture(t, 953, uri)
	probeKey := virtualProbeFailureKey(uri, 5)
	virtualProbeFailures.clear(probeKey)
	virtualProbeFailures.clearPartial(probeKey)
	t.Cleanup(func() {
		virtualProbeFailures.clear(probeKey)
		virtualProbeFailures.clearPartial(probeKey)
	})

	ffprobe := filepath.Join(t.TempDir(), "ffprobe")
	writeExecutableScript(t, ffprobe, "#!/bin/sh\nprintf '%s' '{\"format\":{\"format_name\":\"matroska\",\"duration\":\"3600\"},\"streams\":[{\"index\":0,\"codec_name\":\"hevc\",\"codec_type\":\"video\",\"width\":1920,\"height\":1080},{\"index\":3,\"codec_name\":\"ass\",\"codec_type\":\"subtitle\",\"disposition\":{\"forced\":1},\"tags\":{\"language\":\"fre\"}}]}'\n")
	fx.handler.VirtualPlaybackSourceProber = func(ctx context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		return scanner.ProbeVirtualSource(ctx, ffprobe, "", "http://127.0.0.1/source", f, nil)
	}

	// The deferred probe input is the row (declared FRE audio), as the start path
	// builds it before enqueuing the probe.
	declared := *fx.source
	declared.FilePath = uri
	declared.CodecAudio = "aac"
	declared.AudioTracks = []models.AudioTrack{{Index: -1, Language: "FRE", Codec: "aac", Channels: 2, Default: true}}

	pending := fx.job(uri)
	pending.cand.AudioLanguages = []string{"FRE"}
	pending.probeTransient = &declared
	outcome, committed := fx.handler.probeVirtualSourceAndPersistWith(
		context.Background(),
		pending.stickyKey, pending.file, pending.streamURL,
		*pending.probeTransient, pending.cand, resolver.PersistedIdentityTiers{}, pending.expectedRuntimeMinutes, pending.ownerID,
		true, nil,
	)
	if outcome != probeOutcomePartial {
		t.Fatalf("outcome = %q, want partial: declared audio must not verify a video-without-audio enumeration", outcome)
	}
	if committed.committed() {
		t.Fatalf("committed.row = %d, want no commit: a partial probe commits nothing", committed.fileID)
	}
	if got := fx.saverCallCount(); got != 0 {
		t.Fatalf("partial probe attempted %d evidence writes, want 0", got)
	}
	if virtualProbeFailures.recent(probeKey) {
		t.Fatal("a partial enumeration armed the failure damper")
	}

	// The audio lands on a later enumeration: the same row now verifies, so the
	// hold is a retry state, not a terminal failure.
	writeExecutableScript(t, ffprobe, "#!/bin/sh\nprintf '%s' '{\"format\":{\"format_name\":\"matroska\",\"duration\":\"3600\"},\"streams\":[{\"index\":0,\"codec_name\":\"hevc\",\"codec_type\":\"video\",\"width\":1920,\"height\":1080},{\"index\":1,\"codec_name\":\"eac3\",\"codec_type\":\"audio\",\"channels\":6,\"tags\":{\"language\":\"fre\"}},{\"index\":3,\"codec_name\":\"ass\",\"codec_type\":\"subtitle\",\"disposition\":{\"forced\":1},\"tags\":{\"language\":\"fre\"}}]}'\n")
	verified := fx.job(uri)
	verified.cand.AudioLanguages = []string{"FRE"}
	verified.probeTransient = &declared
	outcome, committed = fx.handler.probeVirtualSourceAndPersistWith(
		context.Background(),
		verified.stickyKey, verified.file, verified.streamURL,
		*verified.probeTransient, verified.cand, resolver.PersistedIdentityTiers{}, verified.expectedRuntimeMinutes, verified.ownerID,
		true, nil,
	)
	if outcome != probeOutcomeVerified {
		t.Fatalf("outcome = %q, want verified once probed audio lands", outcome)
	}
	if committed.fileID != fx.session.MediaFileID {
		t.Fatalf("committed.row = %d, want the committed row %d", committed.fileID, fx.session.MediaFileID)
	}
	if got := fx.saverCallCount(); got != 1 {
		t.Fatalf("complete probe committed %d evidence writes, want 1", got)
	}
}
