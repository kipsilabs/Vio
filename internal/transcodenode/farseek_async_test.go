package transcodenode

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/playback"
)

// TestNodeFarSeekAnswersStaleGenerationAndRestartsDetached pins the node-side
// fast-start seek fix: a far-seek segment request is answered with the
// stale-generation verdict immediately and the seek restart runs detached, so
// the client is not held for the restart it cannot serve.
func TestNodeFarSeekAnswersStaleGenerationAndRestartsDetached(t *testing.T) {
	const sessionID = "node-far-seek"
	server := newTestServer(t)
	outputDir := t.TempDir()
	argsPath := filepath.Join(t.TempDir(), "restart-args")
	ffmpegPath := filepath.Join(t.TempDir(), "fake-ffmpeg.sh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + argsPath + "\"\nexec sleep 30\n"
	if err := os.WriteFile(ffmpegPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}

	session, err := playback.NewReadyTranscodeSessionForTesting(outputDir, playback.TranscodeOpts{
		SessionID:        sessionID,
		InputPath:        "/media/movie.mkv",
		FFmpegPath:       ffmpegPath,
		TargetCodecVideo: "h264",
		TargetCodecAudio: "aac",
		SegmentDuration:  2,
	})
	if err != nil {
		t.Fatalf("ready transcode session: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	server.sessions[sessionID] = session
	server.lastAccess[sessionID] = time.Now()

	const segmentName = "seg_00452.ts"
	req := withNodeRouteParams(
		httptest.NewRequest(http.MethodGet, "/transcode/"+sessionID+"/segment/"+segmentName, nil),
		map[string]string{"session_id": sessionID, "name": segmentName},
	)
	rr := httptest.NewRecorder()

	start := time.Now()
	server.handleSegment(rr, req)
	elapsed := time.Since(start)

	if rr.Code != http.StatusPreconditionFailed {
		t.Fatalf("far-seek status = %d body=%q, want 412 stale generation", rr.Code, rr.Body.String())
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("far-seek request held for %v, want the detached stale-generation verdict (<100ms)", elapsed)
	}

	// The restart runs detached; wait on the observable spawn argument, not a
	// fixed sleep.
	deadline := time.Now().Add(5 * time.Second)
	for {
		args, readErr := os.ReadFile(argsPath)
		if readErr == nil && strings.Contains(string(args), "-start_number\n452\n") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("detached seek restart never spawned ffmpeg (read err=%v)", readErr)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
