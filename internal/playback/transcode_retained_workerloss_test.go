package playback

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// failingFFmpegScript writes a shell script that exits with an error (and a
// stderr line) as soon as it is spawned, standing in for a worker/ffmpeg whose
// process dies mid-switch.
func failingFFmpegScript(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffmpeg-fail")
	script := "#!/bin/sh\necho 'simulated worker loss' >&2\nexit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// startCrashingTranscode starts a real local transcode whose ffmpeg process
// exits with an error, so MonitorLocalTranscodeExit observes a genuine worker
// loss rather than a hand-built dead session.
func startCrashingTranscode(t *testing.T, sessionID, outputDir string) *TranscodeSession {
	t.Helper()
	ts, err := StartTranscode(context.Background(), TranscodeOpts{
		SessionID:        sessionID,
		InputPath:        "/nonexistent/input.mkv",
		OutputDir:        outputDir,
		TargetCodecVideo: "h264",
		TargetCodecAudio: "aac",
		SegmentDuration:  2,
		FFmpegPath:       failingFFmpegScript(t),
	})
	if err != nil {
		t.Fatalf("start crashing transcode: %v", err)
	}
	return ts
}

// waitForGoroutinesWithinBaseline reports whether runtime.NumGoroutine fails to
// settle back within slack of target before timeout. It observes the runtime
// count (not a fixed sleep) so a leaked monitor goroutine is caught while
// scheduler noise is not falsely flagged.
func waitForGoroutinesWithinBaseline(target, slack int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if runtime.NumGoroutine() <= target+slack {
			return false
		}
		if time.Now().After(deadline) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRetainedGenerationSurvivesSuccessorWorkerLoss proves the switchover
// overlap survives the live generation's process dying: the predecessor stays
// retained and servable for its bounded window while the crashed successor is
// removed and its output directory reaped.
func TestRetainedGenerationSurvivesSuccessorWorkerLoss(t *testing.T) {
	m := NewTranscodeManager()
	preDir := filepath.Join(t.TempDir(), "gen-old")
	pre := readyRetainedSession(t, preDir)
	m.RetireTranscodeSessionPredecessor("s1", pre, RetainedGenerationRetention)

	liveDir := filepath.Join(t.TempDir(), "gen-live")
	live := startCrashingTranscode(t, "s1", liveDir)
	if !m.RegisterTranscodeSession("s1", live) {
		t.Fatal("register live transcode failed")
	}

	crash := make(chan *TranscodeSession, 1)
	m.OnFFmpegCrash = func(_ context.Context, sessionID string, dead *TranscodeSession) {
		// Mirror the production compare-and-delete gate without the upstream
		// session stop: the dead generation is removed and reaped, the retained
		// predecessor is left for its overlap window.
		if m.CloseTranscodeSessionIf(sessionID, dead, "") {
			crash <- dead
		}
	}
	m.MonitorLocalTranscodeExit("s1", live)

	select {
	case dead := <-crash:
		if dead != live {
			t.Fatalf("crash callback reported %p, want the dead live generation %p", dead, live)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("successor worker loss was never observed")
	}

	if got := m.GetTranscodeSession("s1"); got != nil {
		t.Fatalf("crashed successor still holds the live slot: %v", got)
	}
	if _, err := os.Stat(liveDir); !os.IsNotExist(err) {
		t.Fatalf("crashed successor output dir survived: %v", err)
	}

	retained := m.GetRetainedTranscodeSession("s1")
	if retained != pre {
		t.Fatalf("retained generation = %v, want the displaced predecessor", retained)
	}
	lease, err := retained.OpenSegment("seg_00000.ts")
	if err != nil {
		t.Fatalf("retained generation cannot serve after the successor died: %v", err)
	}
	_ = lease.Close()
}

// TestRetainedGenerationWorkerLossIsBounded proves the retained predecessor
// keeps serving until its window lapses and is then reaped: a worker loss must
// never turn into indefinite stale-success. Retention outlasts the monitor's
// crash-detection delay so the "inside the window" assertion is meaningful.
func TestRetainedGenerationWorkerLossIsBounded(t *testing.T) {
	m := NewTranscodeManager()
	preDir := filepath.Join(t.TempDir(), "gen-old")
	pre := readyRetainedSession(t, preDir)
	m.RetireTranscodeSessionPredecessor("s1", pre, 4*time.Second)

	liveDir := filepath.Join(t.TempDir(), "gen-live")
	live := startCrashingTranscode(t, "s1", liveDir)
	m.RegisterTranscodeSession("s1", live)

	crashed := make(chan struct{}, 1)
	m.OnFFmpegCrash = func(_ context.Context, sessionID string, dead *TranscodeSession) {
		if m.CloseTranscodeSessionIf(sessionID, dead, "") {
			close(crashed)
		}
	}
	m.MonitorLocalTranscodeExit("s1", live)

	select {
	case <-crashed:
	case <-time.After(10 * time.Second):
		t.Fatal("successor worker loss was never observed")
	}

	// Inside the window the predecessor must still serve.
	if retained := m.GetRetainedTranscodeSession("s1"); retained == nil {
		t.Fatal("retained predecessor vanished before its window lapsed")
	} else if _, err := retained.OpenSegment("seg_00000.ts"); err != nil {
		t.Fatalf("retained predecessor failed inside its window: %v", err)
	}

	// Past the window a read reaps the entry and its directory (the entry's own
	// timer also reaps it out-of-band). Wait on the observable expiry deadline,
	// not a fixed sleep. The Close removes the directory asynchronously to the
	// winning read, so poll the directory itself before asserting it is gone:
	// the entry being nil proves the reap won, the stat proves the Close
	// finished removing the bytes.
	deadline := time.Now().Add(6 * time.Second)
	for {
		if retained := m.GetRetainedTranscodeSession("s1"); retained == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("retained predecessor outlived its bounded window")
		}
		time.Sleep(5 * time.Millisecond)
	}
	dirDeadline := time.Now().Add(6 * time.Second)
	for {
		if _, err := os.Stat(preDir); os.IsNotExist(err) {
			break
		}
		if time.Now().After(dirDeadline) {
			t.Fatalf("expired retained dir still exists: %v", errForTestDir(preDir))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := m.GetRetainedTranscodeSession("s1"); got != nil {
		t.Fatal("expired retained generation reappeared after reaping")
	}
}

// errForTestDir stats dir for a failure message without discarding the error.
func errForTestDir(dir string) error {
	_, err := os.Stat(dir)
	return err
}

// TestDeadLiveGenerationSegmentFailsDeterministically proves a segment the dead
// live generation never produced fails fast (no hang, no stale success), while
// the retained predecessor still serves the segment it owns.
func TestDeadLiveGenerationSegmentFailsDeterministically(t *testing.T) {
	m := NewTranscodeManager()
	pre := readyRetainedSession(t, filepath.Join(t.TempDir(), "gen-old"))
	m.RetireTranscodeSessionPredecessor("s1", pre, RetainedGenerationRetention)

	liveDir := filepath.Join(t.TempDir(), "gen-live")
	live := startCrashingTranscode(t, "s1", liveDir)
	m.RegisterTranscodeSession("s1", live)

	crashed := make(chan struct{}, 1)
	m.OnFFmpegCrash = func(_ context.Context, sessionID string, dead *TranscodeSession) {
		if m.CloseTranscodeSessionIf(sessionID, dead, "") {
			close(crashed)
		}
	}
	m.MonitorLocalTranscodeExit("s1", live)

	select {
	case <-crashed:
	case <-time.After(10 * time.Second):
		t.Fatal("successor worker loss was never observed")
	}

	// Retained bytes still serve.
	if retained := m.GetRetainedTranscodeSession("s1"); retained == nil {
		t.Fatal("retained predecessor missing")
	} else if _, err := retained.OpenSegment("seg_00000.ts"); err != nil {
		t.Fatalf("retained segment no longer serves: %v", err)
	}

	// The dead generation's missing segment fails immediately rather than
	// waiting out a segment timeout.
	start := time.Now()
	lease, err := live.WaitForOpenSegment("seg_99999.ts", 5*time.Second)
	if err == nil {
		_ = lease.Close()
		t.Fatal("dead live generation served a segment it never produced")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("missing-segment lookup took %s, want a bounded fail-fast", elapsed)
	}
	if _, err := live.OpenSegment("seg_99999.ts"); err == nil {
		t.Fatal("dead live generation OpenSegment returned stale success")
	}
}

// TestSwitchoverWorkerLossDoesNotLeakGoroutines proves the worker-loss path owns
// no goroutine past its bounded teardown: the monitor and process reaper settle
// back near the pre-start count once the crash callback has run and the output
// is torn down.
func TestSwitchoverWorkerLossDoesNotLeakGoroutines(t *testing.T) {
	baseline := runtime.NumGoroutine()
	m := NewTranscodeManager()
	pre := readyRetainedSession(t, filepath.Join(t.TempDir(), "gen-old"))
	m.RetireTranscodeSessionPredecessor("s1", pre, 100*time.Millisecond)

	liveDir := filepath.Join(t.TempDir(), "gen-live")
	live := startCrashingTranscode(t, "s1", liveDir)
	m.RegisterTranscodeSession("s1", live)

	crashed := make(chan struct{}, 1)
	m.OnFFmpegCrash = func(_ context.Context, sessionID string, dead *TranscodeSession) {
		if m.CloseTranscodeSessionIf(sessionID, dead, "") {
			close(crashed)
		}
	}
	m.MonitorLocalTranscodeExit("s1", live)

	select {
	case <-crashed:
	case <-time.After(10 * time.Second):
		t.Fatal("successor worker loss was never observed")
	}

	if waitForGoroutinesWithinBaseline(baseline, 8, 3*time.Second) {
		t.Fatalf("goroutines = %d, want within 8 of the pre-start baseline %d; a worker-loss goroutine leaked",
			runtime.NumGoroutine(), baseline)
	}
}

// TestRetainedGenerationReapsWhenSwitchNeverRegistersSuccessor covers the
// switch being lost after the predecessor is displaced: with no live successor
// ever registered and no serve request arriving, the retained predecessor must
// still be reaped at its window by the entry timer. Before the timer fix,
// read-time-only reaping left the directory pinned until the coarse orphan
// sweep hours later.
func TestRetainedGenerationReapsWhenSwitchNeverRegistersSuccessor(t *testing.T) {
	m := NewTranscodeManager()
	dir := filepath.Join(t.TempDir(), "gen-orphaned")
	pre := readyRetainedSession(t, dir)

	m.RetireTranscodeSessionPredecessor("s-lost", pre, 100*time.Millisecond)

	if got := m.GetRetainedTranscodeSession("s-lost"); got != pre {
		t.Fatal("displaced predecessor was not retained for the abandoned switch")
	}
	// No further Get: wait on the directory itself, which the entry timer must
	// reap without any read.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("retained predecessor from an abandoned switch was never reaped without a read")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The registry entry is gone too, so the deadline cannot resurrect it.
	if got := m.GetRetainedTranscodeSession("s-lost"); got != nil {
		t.Fatal("reaped retained generation reappeared")
	}
}
