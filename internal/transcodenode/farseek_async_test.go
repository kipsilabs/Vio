package transcodenode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/playback"
)

// writeNodeFarSeekFFmpeg writes a fake ffmpeg that produces a short initial
// manifest and additionally emits the far segment seg_00452.ts when the seek
// restart asks for -start_number 452.
func writeNodeFarSeekFFmpeg(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-ffmpeg-farseek.sh")
	script := "#!/bin/sh\n" +
		"last=\"\"\n" +
		"for arg in \"$@\"; do last=\"$arg\"; done\n" +
		"case \" $* \" in *\" -bsfs \"*|*\" -encoders \"*|*\" -f null - \"*) exit 0 ;; esac\n" +
		"case \"$last\" in\n" +
		"  *.m3u8) out=\"$(dirname \"$last\")\"; mkdir -p \"$out\"; " +
		"printf x > \"$out/seg_0.ts\"; printf x > \"$out/seg_1.ts\"; printf x > \"$out/seg_2.ts\"; " +
		"printf '#EXTM3U\\n#EXT-X-VERSION:3\\n#EXT-X-TARGETDURATION:2\\n" +
		"#EXT-X-MEDIA-SEQUENCE:0\\n#EXTINF:2.0,\\nseg_0.ts\\n#EXTINF:2.0,\\nseg_1.ts\\n" +
		"#EXTINF:2.0,\\nseg_2.ts\\n' > \"$last\"; " +
		"case \" $* \" in *\" -start_number 452 \"*) printf 'far-seek-bytes' > \"$out/seg_00452.ts\" ;; esac ;;\n" +
		"esac\n" +
		"exec sleep 30\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	return path
}

// TestNodeFarSeekServesAfterAsyncRestart pins the node-side half of the
// continuation contract: a far-seek segment request schedules the same detached
// seek restart as before, then waits for and serves that restart's segment
// instead of answering 412. Cluster deployments therefore behave like the
// integrated server instead of paying an extra playlist round trip per seek.
func TestNodeFarSeekServesAfterAsyncRestart(t *testing.T) {
	const sessionID = "node-far-seek"
	server := newTestServer(t)
	outputDir := t.TempDir()
	session, err := playback.StartTranscode(context.Background(), playback.TranscodeOpts{
		SessionID:          sessionID,
		InputPath:          "/media/movie.mkv",
		OutputDir:          outputDir,
		FFmpegPath:         writeNodeFarSeekFFmpeg(t),
		TargetCodecVideo:   "h264",
		TargetCodecAudio:   "aac",
		SegmentDuration:    2,
		AudioTrackIndex:    -1,
		SubtitleTrackIndex: -1,
		TotalDuration:      1200,
	})
	if err != nil {
		t.Fatalf("StartTranscode: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	server.sessions[sessionID] = session
	server.lastAccess[sessionID] = time.Now()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, statErr := os.Stat(filepath.Join(outputDir, "seg_2.ts")); statErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake transcode never produced the initial segments")
		}
		time.Sleep(5 * time.Millisecond)
	}

	const segmentName = "seg_00452.ts"
	staleToken := session.GenerationToken()
	serve := func(token string) *httptest.ResponseRecorder {
		requestURL := "/transcode/" + sessionID + "/segment/" + segmentName
		if token != "" {
			requestURL += "?sgen=" + url.QueryEscape(token)
		}
		req := withNodeRouteParams(
			httptest.NewRequest(http.MethodGet, requestURL, nil),
			map[string]string{"session_id": sessionID, "name": segmentName},
		)
		rr := httptest.NewRecorder()
		start := time.Now()
		server.handleSegment(rr, req)
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("far-seek request held for %v, want the restart's segment within the serve bound", elapsed)
		}
		return rr
	}

	first := serve(staleToken)
	if first.Code != http.StatusOK || first.Body.String() != "far-seek-bytes" {
		t.Fatalf("far-seek = %d %q, want 200 far-seek-bytes", first.Code, first.Body.String())
	}

	// A client still holding the displaced playlist's token is served from the
	// live generation only through the explicit predecessor relationship.
	second := serve(staleToken)
	if second.Code != http.StatusOK || second.Body.String() != "far-seek-bytes" {
		t.Fatalf("repeat far-seek = %d %q, want 200 far-seek-bytes", second.Code, second.Body.String())
	}
}

// writeNodeFarSeekAnyStartFFmpeg writes a fake ffmpeg that emits a segment for
// every restart's -start_number, so a second rotation can produce the segment a
// held request waits for.
func writeNodeFarSeekAnyStartFFmpeg(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-ffmpeg-any-start.sh")
	script := "#!/bin/sh\n" +
		"last=\"\"; start=\"\"; prev=\"\"\n" +
		"for arg in \"$@\"; do last=\"$arg\"; if [ \"$prev\" = \"-start_number\" ]; then start=\"$arg\"; fi; prev=\"$arg\"; done\n" +
		"case \" $* \" in *\" -bsfs \"*|*\" -encoders \"*|*\" -f null - \"*) exit 0 ;; esac\n" +
		"case \"$last\" in\n" +
		"  *.m3u8) out=\"$(dirname \"$last\")\"; mkdir -p \"$out\"; " +
		"printf x > \"$out/seg_0.ts\"; printf x > \"$out/seg_1.ts\"; printf x > \"$out/seg_2.ts\"; " +
		"printf '#EXTM3U\\n#EXT-X-VERSION:3\\n#EXT-X-TARGETDURATION:2\\n" +
		"#EXT-X-MEDIA-SEQUENCE:0\\n#EXTINF:2.0,\\nseg_0.ts\\n#EXTINF:2.0,\\nseg_1.ts\\n" +
		"#EXTINF:2.0,\\nseg_2.ts\\n' > \"$last\"; " +
		"if [ -n \"$start\" ]; then printf 'far-seek-bytes' > \"$out/$(printf 'seg_%05d.ts' \"$start\")\"; fi ;;\n" +
		"esac\n" +
		"exec sleep 30\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	return path
}

// TestNodeSegmentContinuationAcrossTwoRotationsAnswersStaleGeneration is the
// regression for the node final-fence panic. A stale token is first accepted as
// the predecessor of generation B, then the request waits for a segment B has
// not produced. A second in-place restart (B to C) lands during the hold and
// produces the segment, so the lease names C while the continuation identity is
// still B. The node must answer 412 instead of dereferencing the lease it just
// cleared.
func TestNodeSegmentContinuationAcrossTwoRotationsAnswersStaleGeneration(t *testing.T) {
	const sessionID = "node-two-rotations"
	server := newTestServer(t)
	outputDir := t.TempDir()
	session, err := playback.StartTranscode(context.Background(), playback.TranscodeOpts{
		SessionID:          sessionID,
		InputPath:          "/media/movie.mkv",
		OutputDir:          outputDir,
		FFmpegPath:         writeNodeFarSeekAnyStartFFmpeg(t),
		TargetCodecVideo:   "h264",
		TargetCodecAudio:   "aac",
		SegmentDuration:    2,
		AudioTrackIndex:    -1,
		SubtitleTrackIndex: -1,
		TotalDuration:      1200,
	})
	if err != nil {
		t.Fatalf("StartTranscode: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	server.sessions[sessionID] = session
	server.lastAccess[sessionID] = time.Now()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, statErr := os.Stat(filepath.Join(outputDir, "seg_2.ts")); statErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake transcode never produced the initial segments")
		}
		time.Sleep(5 * time.Millisecond)
	}

	const segmentName = "seg_00455.ts"
	staleToken := session.GenerationToken()
	if err := session.Restart(context.Background(), 904, 452); err != nil {
		t.Fatalf("first restart: %v", err)
	}
	if session.GenerationToken() == staleToken {
		t.Fatal("first restart did not rotate the generation")
	}

	requestURL := "/transcode/" + sessionID + "/segment/" + segmentName + "?sgen=" + url.QueryEscape(staleToken)
	req := withNodeRouteParams(
		httptest.NewRequest(http.MethodGet, requestURL, nil),
		map[string]string{"session_id": sessionID, "name": segmentName},
	)
	rr := httptest.NewRecorder()
	server.handleSegment(rr, req)

	if rr.Code != http.StatusPreconditionFailed {
		t.Fatalf("two-rotation node continuation = %d %q, want 412", rr.Code, rr.Body.String())
	}
	if _, statErr := os.Stat(filepath.Join(outputDir, segmentName)); statErr != nil {
		t.Fatalf("the second restart did not produce the requested segment: %v", statErr)
	}
}
