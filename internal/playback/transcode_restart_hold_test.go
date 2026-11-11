package playback

import (
	"errors"
	"testing"
	"time"
)

// restartHoldSession builds a running session whose current generation spawned
// at generationStartedAt, with an optional measured startup latency already
// remembered from an earlier generation.
func restartHoldSession(t *testing.T, generationStartedAt time.Time) *TranscodeSession {
	t.Helper()
	return &TranscodeSession{
		outputDir: t.TempDir(),
		running:   true,
		opts: TranscodeOpts{
			SessionID:          "session-restart-hold",
			TargetCodecVideo:   "h264",
			SegmentDuration:    2,
			StartSegmentNumber: 0,
		},
		generationStartedAt: generationStartedAt,
	}
}

// TestSegmentProgressMeasuresGenerationStartupLatency proves the encoder's
// observed open cost is measured from the generation's own spawn stamp: the
// first segment the generation wrote is the timestamp the hold decision needs,
// and it is remembered across generations.
func TestSegmentProgressMeasuresGenerationStartupLatency(t *testing.T) {
	now := time.Now()
	spawnedAt := now.Add(-30 * time.Second)
	session := restartHoldSession(t, spawnedAt)
	writeManifestRange(t, session.outputDir, 0, 3, ".ts")
	// Two segments from the current generation; the earlier one is its first
	// output, 4.7s after the spawn.
	writeSegmentFile(t, session.outputDir, "seg_00000.ts", []byte("x"), spawnedAt.Add(4700*time.Millisecond))
	writeSegmentFile(t, session.outputDir, "seg_00001.ts", []byte("x"), spawnedAt.Add(6500*time.Millisecond))
	// A retained segment from the previous generation must not be read as this
	// generation's first output.
	writeSegmentFile(t, session.outputDir, "seg_00002.ts", []byte("x"), spawnedAt.Add(-2*time.Second))

	progress := session.SegmentProgress(now)
	if progress.StartupLatency != 4700*time.Millisecond {
		t.Fatalf("StartupLatency = %v, want the 4.7s open this generation needed", progress.StartupLatency)
	}
	if got := session.StartupLatency(); got != 4700*time.Millisecond {
		t.Fatalf("session StartupLatency = %v, want the measured 4.7s kept for the next generation", got)
	}
}

// TestWaitForRestartContinuationSkipsHoldWhenRestartsStartSlow proves a far-seek
// request is answered with the stale-generation verdict immediately when this
// session's own restarts cannot produce a first segment inside the stall window
// the hold watches. Holding it would spend the client's read budget and answer
// the same verdict seconds later.
func TestWaitForRestartContinuationSkipsHoldWhenRestartsStartSlow(t *testing.T) {
	session := restartHoldSession(t, time.Now())
	writeManifestRange(t, session.outputDir, 0, 3, ".ts")
	session.observedStartupLatency = 4700 * time.Millisecond

	started := time.Now()
	lease, err := session.WaitForRestartContinuationWithin("seg_00003.ts", SeekRestartServeTimeout)
	elapsed := time.Since(started)
	if lease != nil {
		_ = lease.Close()
		t.Fatal("a restart that cannot start inside the stall window served a lease")
	}
	if !errors.Is(err, ErrStaleSegmentGeneration) {
		t.Fatalf("error = %v, want the stale-generation precondition", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("hold took %v, want the verdict without waiting out the restart", elapsed)
	}
}

// TestWaitForRestartContinuationHoldsWhenRestartsStartFast proves a session whose
// restarts open quickly keeps the hold that serves the seek in one request, so a
// seek the encoder serves in a few hundred milliseconds is not turned into a
// playlist reload.
func TestWaitForRestartContinuationHoldsWhenRestartsStartFast(t *testing.T) {
	session := restartHoldSession(t, time.Now())
	writeManifestRange(t, session.outputDir, 0, 3, ".ts")
	session.observedStartupLatency = 200 * time.Millisecond

	started := time.Now()
	lease, err := session.WaitForRestartContinuationWithin("seg_00003.ts", 400*time.Millisecond)
	elapsed := time.Since(started)
	if lease != nil {
		_ = lease.Close()
		t.Fatal("a missing segment was served")
	}
	if !errors.Is(err, ErrStaleSegmentGeneration) {
		t.Fatalf("error = %v, want the stale-generation verdict after the hold expired", err)
	}
	if elapsed < 300*time.Millisecond {
		t.Fatalf("hold took %v, want it to wait out the restart continuation", elapsed)
	}
}
