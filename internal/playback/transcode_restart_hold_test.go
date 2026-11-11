package playback

import (
	"errors"
	"os"
	"path/filepath"
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
// and it is latched for the generation that produced it.
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
		t.Fatalf("session StartupLatency = %v, want the measured 4.7s for the owning generation", got)
	}
}

// TestStartupLatencyLatchesFirstMeasurementAcrossPruning proves a later scan
// cannot inflate a generation's measurement when retention already deleted the
// segment the measurement came from. Only a first-segment latch keeps the value
// stable in a long session.
func TestStartupLatencyLatchesFirstMeasurementAcrossPruning(t *testing.T) {
	now := time.Now()
	spawnedAt := now.Add(-30 * time.Second)
	session := restartHoldSession(t, spawnedAt)
	writeManifestRange(t, session.outputDir, 0, 6, ".ts")
	writeSegmentFile(t, session.outputDir, "seg_00000.ts", []byte("x"), spawnedAt.Add(200*time.Millisecond))
	writeSegmentFile(t, session.outputDir, "seg_00001.ts", []byte("x"), spawnedAt.Add(400*time.Millisecond))
	session.SegmentProgress(now)
	if got := session.StartupLatency(); got != 200*time.Millisecond {
		t.Fatalf("session StartupLatency = %v, want the measured 200ms", got)
	}

	// Retention removes everything the first measurement was taken from. The
	// earliest surviving current-generation segment is now 9s after the spawn.
	for _, name := range []string{"seg_00000.ts", "seg_00001.ts"} {
		if err := os.Remove(filepath.Join(session.outputDir, name)); err != nil {
			t.Fatalf("remove pruned segment %s: %v", name, err)
		}
	}
	writeSegmentFile(t, session.outputDir, "seg_00005.ts", []byte("x"), spawnedAt.Add(9*time.Second))
	session.SegmentProgress(now)
	if got := session.StartupLatency(); got != 200*time.Millisecond {
		t.Fatalf("session StartupLatency = %v after pruning, want the latched 200ms", got)
	}
}

// TestStartupLatencyResetsWhenAGenerationStarts proves a replacement generation
// begins unmeasured, so the seek that follows it keeps the hold instead of
// being judged by what an earlier generation cost.
func TestStartupLatencyResetsWhenAGenerationStarts(t *testing.T) {
	now := time.Now()
	spawnedAt := now.Add(-30 * time.Second)
	session := restartHoldSession(t, spawnedAt)
	writeManifestRange(t, session.outputDir, 0, 3, ".ts")
	writeSegmentFile(t, session.outputDir, "seg_00000.ts", []byte("x"), spawnedAt.Add(4700*time.Millisecond))
	session.SegmentProgress(now)
	if got := session.StartupLatency(); got != 4700*time.Millisecond {
		t.Fatalf("session StartupLatency = %v, want the measured 4.7s before the restart", got)
	}

	session.mu.Lock()
	session.beginGeneration(now)
	session.mu.Unlock()
	if got := session.StartupLatency(); got != 0 {
		t.Fatalf("session StartupLatency = %v after a new generation started, want unmeasured", got)
	}
	if progress := session.SegmentProgress(now); progress.StartupLatency != 0 {
		t.Fatalf("progress StartupLatency = %v, want the new generation unmeasured", progress.StartupLatency)
	}

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
		t.Fatalf("hold took %v, want an unmeasured generation to keep the hold", elapsed)
	}
}

// TestRestartHoldJudgesTheCurrentGeneration proves the skip decision follows the
// generation the seek is waiting on rather than a session-wide high-water mark.
// A slow cold generation must not make every later seek skip a hold its fast
// replacement could satisfy, and a slow replacement must still skip.
func TestRestartHoldJudgesTheCurrentGeneration(t *testing.T) {
	now := time.Now()

	// Generation 1 opened slowly: 4.7s to its first segment.
	slow := restartHoldSession(t, now.Add(-30*time.Second))
	writeManifestRange(t, slow.outputDir, 0, 3, ".ts")
	writeSegmentFile(t, slow.outputDir, "seg_00000.ts", []byte("x"), now.Add(-30*time.Second).Add(4700*time.Millisecond))
	slow.SegmentProgress(now)

	started := time.Now()
	if _, err := slow.WaitForRestartContinuationWithin("seg_00003.ts", SeekRestartServeTimeout); !errors.Is(err, ErrStaleSegmentGeneration) {
		t.Fatalf("slow generation error = %v, want the stale-generation verdict", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("slow generation hold took %v, want the verdict without waiting", elapsed)
	}

	// Generation 2 replaces it. Until it produces its first segment it has no
	// measurement of its own, so a seek arriving now must keep the hold rather
	// than inherit generation 1's 4.7s.
	slow.mu.Lock()
	slow.beginGeneration(now)
	slow.mu.Unlock()
	if got := slow.StartupLatency(); got != 0 {
		t.Fatalf("session StartupLatency = %v after the restart, want unmeasured", got)
	}
	started = time.Now()
	if _, err := slow.WaitForRestartContinuationWithin("seg_00004.ts", 400*time.Millisecond); !errors.Is(err, ErrStaleSegmentGeneration) {
		t.Fatalf("unmeasured replacement error = %v, want the stale-generation verdict after the hold", err)
	}
	if elapsed := time.Since(started); elapsed < 300*time.Millisecond {
		t.Fatalf("unmeasured replacement hold took %v, want it to keep the hold", elapsed)
	}

	// Generation 2 opens in 200ms. The seek that follows its first segment must
	// still hold: the current generation can reach the stall window.
	writeSegmentFile(t, slow.outputDir, "seg_00003.ts", []byte("x"), now.Add(200*time.Millisecond))
	slow.SegmentProgress(now)
	if got := slow.StartupLatency(); got != 200*time.Millisecond {
		t.Fatalf("session StartupLatency = %v, want the fast generation's 200ms", got)
	}
	started = time.Now()
	if _, err := slow.WaitForRestartContinuationWithin("seg_00005.ts", 400*time.Millisecond); !errors.Is(err, ErrStaleSegmentGeneration) {
		t.Fatalf("fast generation error = %v, want the stale-generation verdict after the hold", err)
	}
	if elapsed := time.Since(started); elapsed < 300*time.Millisecond {
		t.Fatalf("fast generation hold took %v, want it to wait out the hold", elapsed)
	}

	// The inverse: a fast generation followed by a slow replacement skips.
	fastThenSlow := restartHoldSession(t, now.Add(-30*time.Second))
	writeManifestRange(t, fastThenSlow.outputDir, 0, 3, ".ts")
	writeSegmentFile(t, fastThenSlow.outputDir, "seg_00000.ts", []byte("x"), now.Add(-30*time.Second).Add(200*time.Millisecond))
	fastThenSlow.SegmentProgress(now)
	fastThenSlow.mu.Lock()
	fastThenSlow.beginGeneration(now)
	fastThenSlow.mu.Unlock()
	writeSegmentFile(t, fastThenSlow.outputDir, "seg_00003.ts", []byte("x"), now.Add(4700*time.Millisecond))
	fastThenSlow.SegmentProgress(now)
	started = time.Now()
	if _, err := fastThenSlow.WaitForRestartContinuationWithin("seg_00004.ts", SeekRestartServeTimeout); !errors.Is(err, ErrStaleSegmentGeneration) {
		t.Fatalf("slow replacement error = %v, want the stale-generation verdict", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("slow replacement hold took %v, want the verdict without waiting", elapsed)
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
