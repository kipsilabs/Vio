package playback

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newAsyncRestartTestSession builds a session whose ffmpeg stub appends the
// value of every -ss argument to logPath and then sleeps, so a test can observe
// which seek positions were actually applied without a real transcode pipeline.
func newAsyncRestartTestSession(t *testing.T, logPath string) *TranscodeSession {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-ffmpeg.sh")
	script := "#!/bin/sh\n" +
		"prev=\"\"\n" +
		"for arg in \"$@\"; do\n" +
		"  if [ \"$prev\" = \"-ss\" ]; then echo \"$arg\" >> \"" + logPath + "\"; fi\n" +
		"  prev=\"$arg\"\n" +
		"done\n" +
		"exec sleep 30\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	return &TranscodeSession{
		outputDir: dir,
		opts: TranscodeOpts{
			SessionID:        "sess-async",
			InputPath:        "/media/movie.mkv",
			OutputDir:        dir,
			FFmpegPath:       bin,
			TargetCodecVideo: "h264",
			TargetCodecAudio: "aac",
			SegmentDuration:  2,
		},
	}
}

// waitAsyncRestartsDrained blocks until the manager has no coalesced far-seek
// worker left for sessionID, so the test observes a settled state rather than a
// fixed sleep.
func waitAsyncRestartsDrained(t *testing.T, m *TranscodeManager, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.asyncRestartMu.Lock()
		pending := m.asyncRestarts[sessionID]
		m.asyncRestartMu.Unlock()
		if pending == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("async seek restart worker did not drain")
		}
		time.Sleep(time.Millisecond)
	}
}

func readSeekLog(t *testing.T, logPath string) []string {
	t.Helper()
	raw, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read seek log: %v", err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// waitSeekLogLast polls until the log's final applied seek is want. Spawning the
// ffmpeg stub returns before the shell has written its argument line, so the
// test must observe the write rather than the spawn.
func waitSeekLogLast(t *testing.T, logPath, want string) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		seeks := readSeekLog(t, logPath)
		if len(seeks) > 0 && seeks[len(seeks)-1] == want {
			return seeks
		}
		if time.Now().After(deadline) {
			t.Fatalf("seek log = %v, want the last applied seek to be %q", seeks, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestRestartSegmentAsyncCoalescesToLatestTarget pins the far-seek coalescing:
// two different positions queued while the lifecycle lock is held must produce
// exactly one restart, at the newest target, rather than racing to be the last
// writer (which could leave ffmpeg at the older position).
func TestRestartSegmentAsyncCoalescesToLatestTarget(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "seeks.log")
	m := NewTranscodeManager()
	s := newAsyncRestartTestSession(t, logPath)
	const sid = "sess-async"
	m.RegisterTranscodeSession(sid, s)
	t.Cleanup(func() { _ = s.Close() })

	// Hold the lifecycle lock so both requests land before the worker runs; the
	// worker must read the coalesced latest target, not the first.
	release := m.LockSessionLifecycle(sid)
	m.RestartSegmentAsync(sid, s, 10)
	m.RestartSegmentAsync(sid, s, 20)
	release()

	waitAsyncRestartsDrained(t, m, sid)
	seeks := waitSeekLogLast(t, logPath, "40.000")
	if len(seeks) != 1 {
		t.Fatalf("applied seeks = %v, want exactly one restart at 40.000 (segment 20)", seeks)
	}
}

// TestRestartSegmentAsyncConvergesWhenTargetArrivesMidRestart pins the second
// half of the coalescing: a newer target that arrives while the first restart is
// still resolving makes the worker run once more, so the final applied position
// is the latest one the player asked for.
func TestRestartSegmentAsyncConvergesWhenTargetArrivesMidRestart(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "seeks.log")
	m := NewTranscodeManager()
	s := newAsyncRestartTestSession(t, logPath)
	const sid = "sess-async"
	m.RegisterTranscodeSession(sid, s)
	t.Cleanup(func() { _ = s.Close() })

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	s.opts.RefreshInput = func(context.Context) (string, func(), error) {
		once.Do(func() { close(entered) })
		<-release
		return s.opts.InputPath, nil, nil
	}

	m.RestartSegmentAsync(sid, s, 10)
	<-entered // the first restart is now inside restart(), holding the lifecycle lock
	m.RestartSegmentAsync(sid, s, 20)
	close(release)

	waitAsyncRestartsDrained(t, m, sid)
	seeks := waitSeekLogLast(t, logPath, "40.000")
	if len(seeks) > 2 {
		t.Fatalf("applied seeks = %v, want convergence in at most two restarts", seeks)
	}
}

// TestRestartSegmentAsyncAppliesTargetArrivingAtCompletion pins the completion
// window. The worker reads whether the coalesced target moved and then leaves the
// loop; a request landing between that read and the entry's removal used to find
// the entry still present, record its target, and return without starting a
// worker, after which the exiting worker deleted the entry — the newest target
// was never applied. The hook fires after the atomic completion check, when the
// entry is already gone, so the late request must start a fresh worker and its
// target must be the final applied seek.
func TestRestartSegmentAsyncAppliesTargetArrivingAtCompletion(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "seeks.log")
	m := NewTranscodeManager()
	s := newAsyncRestartTestSession(t, logPath)
	const sid = "sess-async"
	m.RegisterTranscodeSession(sid, s)
	t.Cleanup(func() { _ = s.Close() })

	// The first worker exits at segment 10 (seek 20.000); enqueue the late
	// target exactly in its completion window. Once guards the second worker's
	// own completion from enqueueing again.
	var once sync.Once
	m.asyncRestartFinishHook = func() {
		once.Do(func() { m.RestartSegmentAsync(sid, s, 20) })
	}

	m.RestartSegmentAsync(sid, s, 10)

	waitAsyncRestartsDrained(t, m, sid)
	// The final applied seek must be the late target. The first restart's stub
	// can be killed before its shell writes its argument, so only the last is
	// asserted; under the old check-then-delete the late target was dropped and
	// ffmpeg was left at segment 10 (seek 20.000).
	waitSeekLogLast(t, logPath, "40.000")
}

// TestRestartSegmentAsyncLatchesTerminalFailure pins the poison-latch: a
// detached restart refused by a dead source records its verdict so the next
// segment request can surface the real failure instead of a second
// stale_generation.
func TestRestartSegmentAsyncLatchesTerminalFailure(t *testing.T) {
	m := NewTranscodeManager()
	s := newAsyncRestartTestSession(t, filepath.Join(t.TempDir(), "seeks.log"))
	const sid = "sess-async"
	m.RegisterTranscodeSession(sid, s)
	t.Cleanup(func() { _ = s.Close() })
	s.demuxStamped = true

	m.RestartSegmentAsync(sid, s, 20)
	waitAsyncRestartsDrained(t, m, sid)

	if got := s.AsyncRestartFailure(); !errors.Is(got, ErrVirtualSourceDemuxFailed) {
		t.Fatalf("latched async restart failure = %v, want ErrVirtualSourceDemuxFailed", got)
	}
}

// TestRestartSegmentAsyncIgnoresSupersededTeardown pins that routine teardown
// does not poison the session: when the worker loses to a close, there is no
// failure verdict to surface on a session that no longer exists.
func TestRestartSegmentAsyncIgnoresSupersededTeardown(t *testing.T) {
	m := NewTranscodeManager()
	s := newAsyncRestartTestSession(t, filepath.Join(t.TempDir(), "seeks.log"))
	const sid = "sess-async"
	m.RegisterTranscodeSession(sid, s)
	t.Cleanup(func() { _ = s.Close() })
	s.demuxStamped = true

	// Unregister before the worker runs so restartSessionLocked reports
	// ErrSessionSuperseded rather than rebuilding a dead source.
	m.transcodeMu.Lock()
	delete(m.transcodes, sid)
	m.transcodeMu.Unlock()

	m.RestartSegmentAsync(sid, s, 20)
	waitAsyncRestartsDrained(t, m, sid)

	if got := s.AsyncRestartFailure(); got != nil {
		t.Fatalf("latched failure after a superseded restart = %v, want nil", got)
	}
}
