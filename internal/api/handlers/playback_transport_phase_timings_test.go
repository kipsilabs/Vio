package handlers

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/playback"
)

// phaseAttrs indexes the timing line's flat key/value attribute slice.
func phaseAttrs(t *testing.T, timings *playbackStartTimingsV3) map[string]any {
	t.Helper()
	attrs := make(map[string]any, len(timings.attrs)/2)
	for i := 0; i+1 < len(timings.attrs); i += 2 {
		name, ok := timings.attrs[i].(string)
		if !ok {
			t.Fatalf("timing attr %d = %T, want string", i, timings.attrs[i])
		}
		attrs[name] = timings.attrs[i+1]
	}
	return attrs
}

// TestTransportPhaseTimingsSplitTheCommit pins that every transport sub-phase
// lands on the start timing line under its own name, that the sub-phases do not
// disturb the composite session_transport_commit delta, and that a negative
// duration is clamped rather than producing a nonsensical negative millisecond.
func TestTransportPhaseTimingsSplitTheCommit(t *testing.T) {
	ctx, recorder := withTransportPhaseTimingsV3(context.Background())
	if recorder == nil {
		t.Fatal("withTransportPhaseTimingsV3 returned a nil recorder")
	}

	recordTransportPhaseV3(ctx, "transport_anchor", time.Now().Add(-25*time.Millisecond))
	addTransportPhaseDurationV3(ctx, "transport_spawn", 40*time.Millisecond)
	addTransportPhaseDurationV3(ctx, "transport_readiness", -5*time.Millisecond)

	timings := newPlaybackStartTimingsV3()
	timings.mark("session_transport_commit")
	timings.observeTransportPhasesV3(recorder)

	attrs := phaseAttrs(t, timings)
	for _, name := range transportPhaseNamesV3 {
		if _, ok := attrs[name+"_ms"]; !ok {
			t.Fatalf("phase %s was not emitted; attrs=%v", name, attrs)
		}
	}
	if got := attrs["transport_anchor_ms"].(int64); got < 20 || got > 200 {
		t.Fatalf("transport_anchor_ms = %d, want roughly 25", got)
	}
	if got := attrs["transport_spawn_ms"].(int64); got != 40 {
		t.Fatalf("transport_spawn_ms = %d, want 40", got)
	}
	if got := attrs["transport_readiness_ms"].(int64); got != 0 {
		t.Fatalf("transport_readiness_ms = %d, want 0 (a negative duration is clamped)", got)
	}
	// observeTransportPhasesV3 must append through the attribute slice directly,
	// never through mark: a mark would move t.last and shrink the next delta.
	// The composite mark was taken before observe ran, so it must still be the
	// only session_transport_commit entry on the line.
	var commitCount int
	for i := 0; i < len(timings.attrs); i += 2 {
		if timings.attrs[i] == "session_transport_commit_ms" {
			commitCount++
		}
	}
	if commitCount != 1 {
		t.Fatalf("session_transport_commit_ms appears %d times, want 1", commitCount)
	}
}

// TestStartTimingsSplitPostPlanRecovery pins the Citizen Vigilante
// attribution fix: the post_plan_recovery mark isolates the
// subtitle/audio-degrade + alternate-hunt window from the remux-escalation
// helper, so a 13s recovery walk cannot misattribute onto an ~0ms
// escalation. Both marks must exist on the line, in order, with the
// recovery delta holding the window and the escalation delta near zero when
// the helper early-returns.
func TestStartTimingsSplitPostPlanRecovery(t *testing.T) {
	timings := newPlaybackStartTimingsV3()
	timings.mark("planning")
	time.Sleep(2 * time.Millisecond)
	timings.mark("post_plan_recovery")
	timings.mark("remux_escalation")

	attrs := phaseAttrs(t, timings)
	recovery, ok := attrs["post_plan_recovery_ms"]
	if !ok {
		t.Fatalf("post_plan_recovery_ms missing; attrs=%v", attrs)
	}
	if _, ok := attrs["remux_escalation_ms"]; !ok {
		t.Fatalf("remux_escalation_ms missing; attrs=%v", attrs)
	}
	if got := recovery.(int64); got < 1 {
		t.Fatalf("post_plan_recovery_ms = %d, want >= 1 (the window must hold the recovery walk)", got)
	}
	// Order: planning, then recovery, then escalation. The flat slice keeps
	// insertion order, so the recovery key must sit between the other two.
	var planningIdx, recoveryIdx, escalationIdx = -1, -1, -1
	for i := 0; i < len(timings.attrs); i += 2 {
		switch timings.attrs[i] {
		case "planning_ms":
			planningIdx = i
		case "post_plan_recovery_ms":
			recoveryIdx = i
		case "remux_escalation_ms":
			escalationIdx = i
		}
	}
	if planningIdx < 0 || recoveryIdx < 0 || escalationIdx < 0 {
		t.Fatalf("marks out of order: planning=%d recovery=%d escalation=%d", planningIdx, recoveryIdx, escalationIdx)
	}
	if planningIdx > recoveryIdx || recoveryIdx > escalationIdx {
		t.Fatalf("marks out of order: planning=%d recovery=%d escalation=%d", planningIdx, recoveryIdx, escalationIdx)
	}
}

// TestTransportPhaseTimingsRequireARecorder pins that recording without an
// installed recorder is a no-op, so an uninstalled path pays nothing and cannot
// panic.
func TestTransportPhaseTimingsRequireARecorder(t *testing.T) {
	ctx := context.Background()
	if transportPhaseTimingsFromV3(ctx) != nil {
		t.Fatal("a bare context unexpectedly carries a phase recorder")
	}
	if got := transportPhaseDurationV3(ctx, "transport_spawn"); got != 0 {
		t.Fatalf("transportPhaseDurationV3 without a recorder = %v, want 0", got)
	}
	recordTransportPhaseV3(ctx, "transport_spawn", time.Now().Add(-time.Second))
	addTransportPhaseDurationV3(ctx, "transport_spawn", time.Second)
	// A nil recorder must not panic; the calls above returning is the assertion.
}

// TestTransportPhaseTimingsReplaceOnReattach pins that attaching a second
// recorder to a derived context replaces rather than accumulates, so a nested
// or retried start measures its own commit.
func TestTransportPhaseTimingsReplaceOnReattach(t *testing.T) {
	outer, outerRecorder := withTransportPhaseTimingsV3(context.Background())
	addTransportPhaseDurationV3(outer, "transport_spawn", 10*time.Millisecond)

	inner, innerRecorder := withTransportPhaseTimingsV3(outer)
	addTransportPhaseDurationV3(inner, "transport_spawn", 20*time.Millisecond)

	if innerRecorder == outerRecorder {
		t.Fatal("re-attaching returned the outer recorder; a nested start would accumulate")
	}
	if got := transportPhaseDurationV3(inner, "transport_spawn"); got != 20*time.Millisecond {
		t.Fatalf("inner transport_spawn = %v, want 20ms", got)
	}
	if got := transportPhaseDurationV3(outer, "transport_spawn"); got != 10*time.Millisecond {
		t.Fatalf("outer transport_spawn = %v, want 10ms (untouched)", got)
	}
}

// TestTransportPhaseSpawnIsNetOfUpstreamResolve pins the attribution arithmetic
// on the non-auto local path: transport_spawn reports the transcode launch net
// of the upstream reopen the launch performs inline. The stubbed input resolve
// sleeps, so a decomposition that folded the resolve into spawn would report
// roughly upstream+spawn here and fail.
func TestTransportPhaseSpawnIsNetOfUpstreamResolve(t *testing.T) {
	const (
		upstreamDelay = 250 * time.Millisecond
		spawnDelay    = 250 * time.Millisecond
		tolerance     = 180 * time.Millisecond
	)
	ctx, _ := withTransportPhaseTimingsV3(context.Background())
	canonical := "virtual://movie/tt-net-spawn?result=pinned"

	h := &PlaybackHandler{
		sessionMgr: playback.NewSessionManager(0, 0),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
			time.Sleep(upstreamDelay)
			return ResolvedVirtualMedia{URL: "http://127.0.0.1:9/source.mp4", URI: canonical, CandidateID: "pinned"}, nil
		}),
		StartTranscodeFunc: func(_ context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
			time.Sleep(spawnDelay)
			return playback.NewReadyTranscodeSessionForTesting(opts.OutputDir, opts)
		},
	}
	opts := playback.TranscodeOpts{
		MediaFileID:                      42,
		InputPath:                        canonical,
		VirtualSourceOwnerInstallationID: 5,
		SessionID:                        "net-of-upstream",
		OutputDir:                        t.TempDir(),
	}
	ts, failure := h.startReadyLocalPlaybackTransportV3(ctx, opts)
	if failure != nil {
		t.Fatalf("start failed: %+v", failure)
	}
	defer func() { _ = ts.Close() }()

	upstream := transportPhaseDurationV3(ctx, "transport_upstream")
	spawn := transportPhaseDurationV3(ctx, "transport_spawn")
	if diff := upstream - upstreamDelay; diff < -tolerance || diff > tolerance {
		t.Fatalf("transport_upstream = %v, want ~%v (diff %v)", upstream, upstreamDelay, diff)
	}
	if diff := spawn - spawnDelay; diff < -tolerance || diff > tolerance {
		t.Fatalf("transport_spawn = %v, want ~%v net of the inline upstream resolve (diff %v)", spawn, spawnDelay, diff)
	}
}

// autoReadinessTestManifestV3 is the playable playlist the manifest-delay stub
// writes back after the launch returns, matching the ready-session fixture so
// the held-back manifest satisfies the same readiness gate.
const autoReadinessTestManifestV3 = "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:4.000000,\nseg_00000.ts\n#EXTINF:4.000000,\nseg_00001.ts\n#EXTINF:4.000000,\nseg_00002.ts\n"

// TestTransportPhaseAutoReadinessIsNetOfSpawn pins the attribution arithmetic on
// the hw_accel=auto walk: transport_readiness is the walk's wall time net of the
// spawn time already recorded, so it equals wall_total minus spawn. The stub
// launch sleeps, and the manifest is held back for a further delay, so a
// decomposition that folded spawn into readiness would report the sum and fail
// the identity.
func TestTransportPhaseAutoReadinessIsNetOfSpawn(t *testing.T) {
	const (
		spawnDelay     = 300 * time.Millisecond
		readinessDelay = 300 * time.Millisecond
		tolerance      = 150 * time.Millisecond
	)
	ctx, _ := withTransportPhaseTimingsV3(context.Background())
	tempDir := t.TempDir()
	manifestPath := filepath.Join(tempDir, "stream.m3u8")

	h := &PlaybackHandler{
		StartTranscodeFunc: func(_ context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
			time.Sleep(spawnDelay)
			ts, err := playback.NewReadyTranscodeSessionForTesting(opts.OutputDir, opts)
			if err != nil {
				return nil, err
			}
			// Hold readiness: the process is running but has not written its
			// manifest yet. The delayed, atomic write below models FFmpeg
			// producing it after the launch returned.
			if err := os.Remove(manifestPath); err != nil {
				return nil, err
			}
			go func() {
				time.Sleep(readinessDelay)
				tmp := manifestPath + ".tmp"
				if writeErr := os.WriteFile(tmp, []byte(autoReadinessTestManifestV3), 0o644); writeErr != nil {
					return
				}
				_ = os.Rename(tmp, manifestPath)
			}()
			return ts, nil
		},
	}
	opts := playback.TranscodeOpts{
		InputPath:          "/media/movie.mkv",
		OutputDir:          tempDir,
		SessionID:          "auto-net-of-spawn",
		TargetCodecVideo:   "h264",
		TargetCodecAudio:   "aac",
		SegmentDuration:    2,
		HWAccel:            "nvenc",
		AudioTrackIndex:    -1,
		SubtitleTrackIndex: -1,
	}
	pipeline := playback.NewResolvedAutoTranscodePipelineForTest(opts)

	begin := time.Now()
	ts, failure := h.startReadyAutoLocalPlaybackTransportV3(ctx, pipeline)
	totalWall := time.Since(begin)
	if failure != nil {
		t.Fatalf("auto start failed: %+v", failure)
	}
	defer func() { _ = ts.Close() }()

	spawn := transportPhaseDurationV3(ctx, "transport_spawn")
	readiness := transportPhaseDurationV3(ctx, "transport_readiness")
	if diff := spawn - spawnDelay; diff < -tolerance || diff > tolerance {
		t.Fatalf("transport_spawn = %v, want ~%v (diff %v)", spawn, spawnDelay, diff)
	}
	// readiness = wall_total - spawn is the attribution the split promises.
	if diff := readiness - (totalWall - spawn); diff < -tolerance || diff > tolerance {
		t.Fatalf("transport_readiness = %v, want wall_total-spawn = %v (diff %v)", readiness, totalWall-spawn, diff)
	}
	if readiness < readinessDelay-tolerance {
		t.Fatalf("transport_readiness = %v, want at least ~%v", readiness, readinessDelay-tolerance)
	}
}

// TestTransportPhaseAutoReadinessNetsInlineManifestWait pins that a virtual
// transport's inline first-manifest wait, recorded inside the start callback as
// transport_manifest, is not counted again in transport_readiness by the
// hw_accel=auto walk. The callback below records the phase during its spawn span,
// exactly where the virtual start path records it.
func TestTransportPhaseAutoReadinessNetsInlineManifestWait(t *testing.T) {
	const (
		spawnDelay     = 300 * time.Millisecond
		manifestDelay  = 250 * time.Millisecond
		readinessDelay = 300 * time.Millisecond
		tolerance      = 120 * time.Millisecond
	)
	ctx, _ := withTransportPhaseTimingsV3(context.Background())
	tempDir := t.TempDir()
	manifestPath := filepath.Join(tempDir, "stream.m3u8")

	h := &PlaybackHandler{
		StartTranscodeFunc: func(_ context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
			time.Sleep(spawnDelay)
			// Model the virtual start path: it waits for the inline first manifest
			// and records that wait as transport_manifest before returning.
			addTransportPhaseDurationV3(ctx, transportPhaseManifestV3, manifestDelay)
			ts, err := playback.NewReadyTranscodeSessionForTesting(opts.OutputDir, opts)
			if err != nil {
				return nil, err
			}
			if err := os.Remove(manifestPath); err != nil {
				return nil, err
			}
			go func() {
				time.Sleep(readinessDelay)
				tmp := manifestPath + ".tmp"
				if writeErr := os.WriteFile(tmp, []byte(autoReadinessTestManifestV3), 0o644); writeErr != nil {
					return
				}
				_ = os.Rename(tmp, manifestPath)
			}()
			return ts, nil
		},
	}
	opts := playback.TranscodeOpts{
		InputPath:          "/media/movie.mkv",
		OutputDir:          tempDir,
		SessionID:          "auto-net-of-inline-manifest",
		TargetCodecVideo:   "h264",
		TargetCodecAudio:   "aac",
		SegmentDuration:    2,
		HWAccel:            "nvenc",
		AudioTrackIndex:    -1,
		SubtitleTrackIndex: -1,
	}
	pipeline := playback.NewResolvedAutoTranscodePipelineForTest(opts)

	begin := time.Now()
	ts, failure := h.startReadyAutoLocalPlaybackTransportV3(ctx, pipeline)
	totalWall := time.Since(begin)
	if failure != nil {
		t.Fatalf("auto start failed: %+v", failure)
	}
	defer func() { _ = ts.Close() }()

	manifest := transportPhaseDurationV3(ctx, transportPhaseManifestV3)
	spawn := transportPhaseDurationV3(ctx, "transport_spawn")
	readiness := transportPhaseDurationV3(ctx, "transport_readiness")
	if diff := manifest - manifestDelay; diff < -tolerance || diff > tolerance {
		t.Fatalf("transport_manifest = %v, want ~%v", manifest, manifestDelay)
	}
	// Spawn is recorded net of the inline manifest wait it performed.
	if want := spawnDelay - manifestDelay; spawn < want-tolerance || spawn > want+tolerance {
		t.Fatalf("transport_spawn = %v, want ~%v (net of the inline manifest wait)", spawn, want)
	}
	// Readiness excludes both spawn and the inline manifest wait; the buggy
	// decomposition (wall - spawn) would count the wait a second time.
	want := totalWall - spawn - manifest
	if diff := readiness - want; diff < -tolerance || diff > tolerance {
		t.Fatalf("transport_readiness = %v, want wall-spawn-manifest = %v (diff %v)", readiness, want, diff)
	}
	if readiness > totalWall-spawn-tolerance {
		t.Fatalf("transport_readiness = %v still includes the inline manifest wait (wall-spawn = %v)", readiness, totalWall-spawn)
	}
}
