package transcodenode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/playback"
)

// newNodeAsyncRestartTestSession starts a session whose ffmpeg stub appends the
// value of every -ss argument to logPath and then sleeps, so a test can observe
// which seek positions the node actually applied without a real pipeline. A
// non-nil refresh blocks the restart at its input-refresh step.
func newNodeAsyncRestartTestSession(t *testing.T, logPath string, refresh func(context.Context) (string, func(), error)) *playback.TranscodeSession {
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
	session, err := playback.StartTranscode(context.Background(), playback.TranscodeOpts{
		SessionID:          "sess-node-async",
		InputPath:          "/media/movie.mkv",
		OutputDir:          dir,
		FFmpegPath:         bin,
		TargetCodecVideo:   "h264",
		TargetCodecAudio:   "aac",
		SegmentDuration:    2,
		AudioTrackIndex:    -1,
		SubtitleTrackIndex: -1,
		RefreshInput:       refresh,
	})
	if err != nil {
		t.Fatalf("StartTranscode: %v", err)
	}
	return session
}

// waitNodeAsyncRestartsDrained blocks until the node has no coalesced far-seek
// worker left for sessionID.
func waitNodeAsyncRestartsDrained(t *testing.T, s *Server, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.asyncRestartMu.Lock()
		pending := s.asyncRestarts[sessionID]
		s.asyncRestartMu.Unlock()
		if pending == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("node async seek restart worker did not drain")
		}
		time.Sleep(time.Millisecond)
	}
}

func readNodeSeekLog(t *testing.T, logPath string) []string {
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

// waitNodeSeekLogLast polls until the log's final applied seek is want.
func waitNodeSeekLogLast(t *testing.T, logPath, want string) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		seeks := readNodeSeekLog(t, logPath)
		if len(seeks) > 0 && seeks[len(seeks)-1] == want {
			return seeks
		}
		if time.Now().After(deadline) {
			t.Fatalf("seek log = %v, want the last applied seek to be %q", seeks, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestNodeRestartSegmentAsyncCoalescesBurstToLatestTarget pins the node-side
// coalescing: two positions queued while the lifecycle lock is held must produce
// one restart at the newest target. Without it the goroutine per request
// serializes through the lifecycle lock in arrival order and the older target
// can win.
func TestNodeRestartSegmentAsyncCoalescesBurstToLatestTarget(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "seeks.log")
	session := newNodeAsyncRestartTestSession(t, logPath, nil)
	t.Cleanup(func() { _ = session.Close() })
	const sid = "sess-node-async"
	server := &Server{sessions: map[string]*playback.TranscodeSession{sid: session}}

	release := server.lockSessionLifecycle(sid)
	server.restartSegmentAsync(sid, session, 10)
	server.restartSegmentAsync(sid, session, 20)
	release()

	waitNodeAsyncRestartsDrained(t, server, sid)
	seeks := waitNodeSeekLogLast(t, logPath, "40.000")
	if len(seeks) != 1 {
		t.Fatalf("applied seeks = %v, want exactly one restart at 40.000 (segment 20)", seeks)
	}
}

// TestNodeRestartSegmentAsyncConvergesWhenTargetArrivesMidRestart pins the
// second half of the coalescing: a newer target arriving while the first restart
// is still resolving makes the worker run once more, so the final applied
// position is the latest the player asked for.
func TestNodeRestartSegmentAsyncConvergesWhenTargetArrivesMidRestart(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "seeks.log")
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	session := newNodeAsyncRestartTestSession(t, logPath, func(context.Context) (string, func(), error) {
		once.Do(func() { close(entered) })
		<-release
		return "/media/movie.mkv", nil, nil
	})
	t.Cleanup(func() { _ = session.Close() })
	const sid = "sess-node-async"
	server := &Server{sessions: map[string]*playback.TranscodeSession{sid: session}}

	server.restartSegmentAsync(sid, session, 10)
	<-entered // the first restart is now inside restart(), holding the lifecycle lock
	server.restartSegmentAsync(sid, session, 20)
	close(release)

	waitNodeAsyncRestartsDrained(t, server, sid)
	seeks := waitNodeSeekLogLast(t, logPath, "40.000")
	if len(seeks) > 2 {
		t.Fatalf("applied seeks = %v, want convergence in at most two restarts", seeks)
	}
}
