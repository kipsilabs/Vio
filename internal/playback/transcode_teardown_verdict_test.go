package playback

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// sleepingFFmpegScript writes a stand-in FFmpeg that produces no output and
// stays alive until the session cancels it, so a test controls exactly when the
// process dies rather than racing a real encoder.
func sleepingFFmpegScript(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffmpeg-sleep")
	script := "#!/bin/sh\nexec sleep 30\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAbandonedBeforeFirstSegment pins the client-abandoned fingerprint: a live
// session that answered a playlist request and which no segment request ever
// reached.
func TestAbandonedBeforeFirstSegment(t *testing.T) {
	dir := t.TempDir()
	session := readyRetainedSession(t, dir)
	session.opts.SegmentDuration = 2
	session.mu.Lock()
	session.running = true
	session.mu.Unlock()

	if session.AbandonedBeforeFirstSegment() {
		t.Fatal("session with no served playlist was treated as abandoned")
	}

	if _, err := session.BuildPlaybackManifest("segment/", ""); err != nil {
		t.Fatalf("serve the session playlist: %v", err)
	}
	if !session.AbandonedBeforeFirstSegment() {
		t.Fatal("servable session with no segment request was not treated as abandoned")
	}

	if _, err := session.OpenSegment("seg_00000.ts"); err != nil {
		t.Fatalf("serve the produced segment: %v", err)
	}
	if session.AbandonedBeforeFirstSegment() {
		t.Fatal("a session that served a segment request was still treated as abandoned")
	}

	var nilSession *TranscodeSession
	if nilSession.AbandonedBeforeFirstSegment() {
		t.Fatal("nil session reported abandoned")
	}
}

// TestAbandonedBeforeFirstSegmentKeepsGenuineFailures proves the predicate does
// not swallow real failures: a dead process, a session with no servable
// playlist, and a rejected source all keep the ordinary handling.
func TestAbandonedBeforeFirstSegmentKeepsGenuineFailures(t *testing.T) {
	dir := t.TempDir()
	session := readyRetainedSession(t, dir)
	session.opts.SegmentDuration = 2
	if _, err := session.BuildPlaybackManifest("segment/", ""); err != nil {
		t.Fatalf("serve the session playlist: %v", err)
	}

	session.mu.Lock()
	session.running = false
	session.mu.Unlock()
	if session.AbandonedBeforeFirstSegment() {
		t.Fatal("dead generation reported abandoned")
	}

	session.mu.Lock()
	session.running = true
	session.decodeStamped = true
	session.mu.Unlock()
	if session.AbandonedBeforeFirstSegment() {
		t.Fatal("decoder-rejected generation reported abandoned")
	}

	empty := NewTranscodeSessionForTest(t.TempDir())
	empty.mu.Lock()
	empty.running = true
	empty.manifestServed = true
	empty.opts.SegmentDuration = 2
	empty.mu.Unlock()
	if empty.AbandonedBeforeFirstSegment() {
		t.Fatal("session without a servable playlist reported abandoned")
	}
}

// TestSelfCanceledTeardownIsNotATranscodeFailure proves our own teardown kill is
// answered as a stale generation (the client reloads the playlist) instead of a
// transcode failure the client reports as failure_recovery.
func TestSelfCanceledTeardownIsNotATranscodeFailure(t *testing.T) {
	session, err := StartTranscode(context.Background(), TranscodeOpts{
		SessionID:        "teardown-verdict",
		InputPath:        "/nonexistent/input.mkv",
		OutputDir:        t.TempDir(),
		FFmpegPath:       sleepingFFmpegScript(t),
		TargetCodecVideo: "h264",
		TargetCodecAudio: "aac",
		SegmentDuration:  2,
	})
	if err != nil {
		t.Fatalf("start transcode: %v", err)
	}
	if err := session.CloseProcess(); err != nil {
		t.Fatalf("close process: %v", err)
	}

	if _, err := session.WaitForSegment("seg_00005.ts", 200*time.Millisecond); !errors.Is(err, ErrStaleSegmentGeneration) {
		t.Fatalf("segment wait after self-cancel = %v, want %v", err, ErrStaleSegmentGeneration)
	}
	if _, err := session.WaitForOpenSegment("seg_00005.ts", 200*time.Millisecond); !errors.Is(err, ErrStaleSegmentGeneration) {
		t.Fatalf("open-segment wait after self-cancel = %v, want %v", err, ErrStaleSegmentGeneration)
	}
	if _, err := session.GetManifest(); !errors.Is(err, ErrManifestNotReady) {
		t.Fatalf("manifest after self-cancel = %v, want retryable %v", err, ErrManifestNotReady)
	}
}

// TestGenuineCrashStillReportsTranscodeFailure proves the cancellation guard did
// not soften a real ffmpeg death: a non-zero exit with no cancellation keeps the
// existing transcode-failure verdict on both the manifest and segment paths.
func TestGenuineCrashStillReportsTranscodeFailure(t *testing.T) {
	session := startCrashingTranscode(t, "genuine-crash", t.TempDir())
	select {
	case <-session.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("crashing ffmpeg was never reaped")
	}
	session.mu.Lock()
	session.running = false
	session.mu.Unlock()

	if _, err := session.WaitForSegment("seg_00005.ts", 200*time.Millisecond); !errors.Is(err, ErrTranscodeFailed) {
		t.Fatalf("segment wait after crash = %v, want %v", err, ErrTranscodeFailed)
	}
	if _, err := session.GetManifest(); !errors.Is(err, ErrTranscodeFailed) {
		t.Fatalf("manifest after crash = %v, want %v", err, ErrTranscodeFailed)
	}
}

// TestSelfCanceledTeardownLogsNoExitError proves the ffmpeg event log stops
// reporting our own teardown kill as an exit error, while a genuine crash keeps
// its error event.
func TestSelfCanceledTeardownLogsNoExitError(t *testing.T) {
	logs := &recordingFFmpegLogSink{}
	session, err := StartTranscode(context.Background(), TranscodeOpts{
		SessionID:        "teardown-log",
		InputPath:        "/nonexistent/input.mkv",
		OutputDir:        t.TempDir(),
		FFmpegPath:       sleepingFFmpegScript(t),
		TargetCodecVideo: "h264",
		TargetCodecAudio: "aac",
		SegmentDuration:  2,
		FFmpegLogSink:    logs,
	})
	if err != nil {
		t.Fatalf("start transcode: %v", err)
	}
	if err := session.CloseProcess(); err != nil {
		t.Fatalf("close process: %v", err)
	}

	logs.mu.Lock()
	defer logs.mu.Unlock()
	for _, entry := range logs.entries {
		if entry.event == "ffmpeg process exit error" {
			t.Fatalf("self-canceled teardown logged an exit error: %+v", entry)
		}
	}
}

// TestGenuineCrashLogsExitError is the other half of the guard: a real ffmpeg
// failure still reaches the event log with its exit error.
func TestGenuineCrashLogsExitError(t *testing.T) {
	logs := &recordingFFmpegLogSink{}
	session, err := StartTranscode(context.Background(), TranscodeOpts{
		SessionID:        "crash-log",
		InputPath:        "/nonexistent/input.mkv",
		OutputDir:        t.TempDir(),
		FFmpegPath:       failingFFmpegScript(t),
		TargetCodecVideo: "h264",
		TargetCodecAudio: "aac",
		SegmentDuration:  2,
		FFmpegLogSink:    logs,
	})
	if err != nil {
		t.Fatalf("start transcode: %v", err)
	}
	select {
	case <-session.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("crashing ffmpeg was never reaped")
	}

	logs.mu.Lock()
	defer logs.mu.Unlock()
	for _, entry := range logs.entries {
		if entry.event == "ffmpeg process exit error" && entry.attrs.ExitError != "" {
			return
		}
	}
	t.Fatalf("genuine crash never logged an exit error: %+v", logs.entries)
}
