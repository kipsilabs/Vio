package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// writePlaybackTestFFmpegFarSeek writes a fake ffmpeg that produces a short
// manifest (seg_0..2) on the initial start, and additionally emits the far
// segment seg_00452.m4s when it is invoked with the seek restart's
// -start_number 452, so a test can observe the asynchronous restart's output.
func writePlaybackTestFFmpegFarSeek(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-ffmpeg-farseek.sh")
	script := "#!/bin/sh\n" +
		"last=\"\"\n" +
		"for arg in \"$@\"; do last=\"$arg\"; done\n" +
		"case \" $* \" in *\" -bsfs \"*|*\" -encoders \"*|*\" -f null - \"*) exit 0 ;; esac\n" +
		"case \"$last\" in\n" +
		"  *.m3u8) out=\"$(dirname \"$last\")\"; mkdir -p \"$out\"; " +
		"printf x > \"$out/init.mp4\"; printf x > \"$out/seg_0.m4s\"; " +
		"printf x > \"$out/seg_1.m4s\"; printf x > \"$out/seg_2.m4s\"; " +
		"printf '#EXTM3U\\n#EXT-X-VERSION:7\\n#EXT-X-TARGETDURATION:2\\n" +
		"#EXT-X-MEDIA-SEQUENCE:0\\n#EXT-X-MAP:URI=\"init.mp4\"\\n" +
		"#EXTINF:2.0,\\nseg_0.m4s\\n#EXTINF:2.0,\\nseg_1.m4s\\n" +
		"#EXTINF:2.0,\\nseg_2.m4s\\n' > \"$last\"; " +
		"case \" $* \" in *\" -start_number 452 \"*) printf 'far-seek-bytes' > \"$out/seg_00452.m4s\" ;; esac ;;\n" +
		"esac\n" +
		"exec sleep 30\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	return path
}

// TestFarSeekBeyondProducedWindowServesAfterAsyncRestart pins the seek fix: a
// request for a segment far past the produced window schedules an asynchronous
// seek restart and then waits for the restart's own segment instead of answering
// the stale-generation verdict. The old verdict made hls.js burn its four-attempt
// fragment retry budget on the same stale URL before refetching the playlist,
// which cost ~4s of dead air per far seek. The restart targets exactly the
// requested absolute segment, so the rotated generation's bytes are the stream
// this request asked for and the response is servable.
func TestFarSeekBeyondProducedWindowServesAfterAsyncRestart(t *testing.T) {
	outputDir := t.TempDir()
	ffmpegPath := writePlaybackTestFFmpegFarSeek(t)

	const sessionID = "far-seek-session"
	transcodeSession, err := playback.StartTranscode(context.Background(), playback.TranscodeOpts{
		SessionID:          sessionID,
		InputPath:          "/media/movie.mkv",
		OutputDir:          outputDir,
		FFmpegPath:         ffmpegPath,
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
	t.Cleanup(func() { _ = transcodeSession.Close() })

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, statErr := os.Stat(filepath.Join(outputDir, "seg_2.m4s")); statErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake transcode never produced the initial segments")
		}
		time.Sleep(5 * time.Millisecond)
	}

	sessions := playback.NewSessionManager(0, 0)
	sessions.RegisterReconstructed(&playback.Session{
		ID:          sessionID,
		UserID:      1,
		MediaFileID: 91,
		PlayMethod:  playback.PlayTranscode,
	})
	handler := NewPlaybackHandler(sessions)
	handler.TranscodeManager().RegisterTranscodeSession(sessionID, transcodeSession)
	handler.fileResolver = mapPlaybackFileResolver{files: map[int]*models.MediaFile{
		91: {ID: 91, FilePath: "/media/movie.mkv", Container: "mkv"},
	}}

	const segmentName = "seg_00452.m4s"
	serve := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handler.HandleGetTranscodeSegment(rec, playbackTestRequest(
			http.MethodGet,
			"/api/v1/playback/transcode/"+sessionID+"/segment/"+segmentName,
			nil,
			map[string]string{"session_id": sessionID, "name": segmentName},
		))
		return rec
	}

	start := time.Now()
	first := serve()
	elapsed := time.Since(start)

	if first.Code != http.StatusOK {
		t.Fatalf("far-seek status = %d body=%q, want 200 served by the restarted generation", first.Code, first.Body.String())
	}
	if first.Body.String() != "far-seek-bytes" {
		t.Fatalf("far-seek body = %q, want the restarted generation's bytes", first.Body.String())
	}
	if elapsed > 5*time.Second {
		t.Fatalf("far-seek request held for %s, want the restart's segment within the serve bound", elapsed)
	}

	// The triggering request now waits through the restart instead of being
	// refused, so the segment it asked for exists by the time it returns.
	if _, statErr := os.Stat(filepath.Join(outputDir, segmentName)); statErr != nil {
		t.Fatalf("far-seek response returned before the restart produced %s: %v", segmentName, statErr)
	}

	// A repeat request on the rotated generation's own token serves the same
	// bytes without another restart.
	second := serve()
	if second.Code != http.StatusOK || second.Body.String() != "far-seek-bytes" {
		t.Fatalf("repeat far-seek = %d %q, want 200 far-seek-bytes", second.Code, second.Body.String())
	}
}

// TestFarSeekStaleGenerationAfterRestartServesFromLiveGeneration replays the
// production seek sequence: the client holds a playlist minted before a far-seek
// restart, and every segment URI in it still names the displaced generation. The
// triggering request itself now waits through the restart and is served, so the
// client never sees the 412; requests for segments at or after the restart's
// start segment are likewise served from the live generation because absolute
// segment numbering makes them the same timeline position. A segment before the
// restart point still gets the stale-generation verdict: the live generation
// never produced it, so answering it would mix timelines.
func TestFarSeekStaleGenerationAfterRestartServesFromLiveGeneration(t *testing.T) {
	outputDir := t.TempDir()
	const sessionID = "farseek-sgen"
	handler, transcodeSession := startFarSeekHandlerSession(t, sessionID, writePlaybackTestFFmpegFarSeek(t), outputDir, playback.TranscodeOpts{
		InputPath:          "/media/movie.mkv",
		TargetCodecVideo:   "h264",
		TargetCodecAudio:   "aac",
		SegmentDuration:    2,
		AudioTrackIndex:    -1,
		SubtitleTrackIndex: -1,
		TotalDuration:      1200,
	})

	const segmentName = "seg_00452.m4s"
	staleToken := transcodeSession.GenerationToken()

	segmentRequest := func(name, token string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handler.HandleGetTranscodeSegment(rec, playbackTestRequest(
			http.MethodGet,
			"/api/v1/playback/transcode/"+sessionID+"/segment/"+name+"?sgen="+url.QueryEscape(token),
			nil,
			map[string]string{"session_id": sessionID, "name": name},
		))
		return rec
	}

	// The triggering request names the still-live generation and is held through
	// the far-seek restart it schedules.
	first := segmentRequest(segmentName, staleToken)
	if first.Code != http.StatusOK || first.Body.String() != "far-seek-bytes" {
		t.Fatalf("triggering far-seek = %d %q, want 200 far-seek-bytes", first.Code, first.Body.String())
	}
	currentToken := transcodeSession.GenerationToken()
	if currentToken == staleToken {
		t.Fatal("the generation token did not rotate across the far-seek restart")
	}

	// The client has not refetched the playlist yet, so it still names the
	// displaced generation. At and after the restart's start segment the live
	// generation owns the same absolute position and must serve it.
	stale := segmentRequest(segmentName, staleToken)
	if stale.Code != http.StatusOK || stale.Body.String() != "far-seek-bytes" {
		t.Fatalf("stale-token continuation = %d %q, want 200 far-seek-bytes", stale.Code, stale.Body.String())
	}

	// Before the restart point the live generation never produced the segment,
	// so the same stale token must still be refused rather than answered with
	// another timeline's bytes.
	beforeRestart := segmentRequest("seg_00001.m4s", staleToken)
	if beforeRestart.Code != http.StatusPreconditionFailed || !strings.Contains(beforeRestart.Body.String(), "stale_generation") {
		t.Fatalf("pre-restart stale token = %d %q, want the stale_generation verdict", beforeRestart.Code, beforeRestart.Body.String())
	}

	// The init segment is generation-independent for the same recipe, so a
	// playlist fetched before the rotation keeps working through it.
	staleInit := segmentRequest("init.mp4", staleToken)
	if staleInit.Code != http.StatusOK {
		t.Fatalf("stale-token init = %d %q, want 200 from the live generation", staleInit.Code, staleInit.Body.String())
	}

	// The replacement generation's own token serves the rest of its playlist.
	initRec := segmentRequest("init.mp4", currentToken)
	if initRec.Code != http.StatusOK {
		t.Fatalf("replacement init status = %d body=%q, want 200", initRec.Code, initRec.Body.String())
	}
	manifestRec := httptest.NewRecorder()
	handler.HandleGetTranscodeManifest(manifestRec, playbackTestRequest(
		http.MethodGet,
		"/api/v1/playback/transcode/"+sessionID+"/master.m3u8",
		nil,
		map[string]string{"session_id": sessionID},
	))
	if manifestRec.Code != http.StatusOK {
		t.Fatalf("manifest status = %d body=%q, want 200", manifestRec.Code, manifestRec.Body.String())
	}
	if manifestToken := manifestGenerationToken(t, manifestRec.Body.Bytes()); manifestToken != currentToken {
		t.Fatalf("manifest token = %q, want the live generation %q", manifestToken, currentToken)
	}
}

// manifestGenerationToken extracts the sgen token from an HLS manifest body, or
// "" when the manifest carries none.
func manifestGenerationToken(t *testing.T, body []byte) string {
	t.Helper()
	for _, line := range strings.Split(string(body), "\n") {
		i := strings.Index(line, "sgen=")
		if i < 0 {
			continue
		}
		raw := line[i+len("sgen="):]
		if j := strings.IndexAny(raw, "&\"\r "); j >= 0 {
			raw = raw[:j]
		}
		token, err := url.QueryUnescape(raw)
		if err != nil {
			t.Fatalf("unescape sgen %q: %v", raw, err)
		}
		return token
	}
	return ""
}

// startFarSeekHandlerSession starts a fake transcode, waits for its initial
// segments, and registers it with a handler wired to resolve the test file.
func startFarSeekHandlerSession(t *testing.T, sessionID, ffmpegPath, outputDir string, opts playback.TranscodeOpts) (*PlaybackHandler, *playback.TranscodeSession) {
	t.Helper()
	opts.SessionID = sessionID
	opts.OutputDir = outputDir
	opts.FFmpegPath = ffmpegPath
	transcodeSession, err := playback.StartTranscode(context.Background(), opts)
	if err != nil {
		t.Fatalf("StartTranscode: %v", err)
	}
	t.Cleanup(func() { _ = transcodeSession.Close() })

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, statErr := os.Stat(filepath.Join(outputDir, "seg_2.m4s")); statErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake transcode never produced the initial segments")
		}
		time.Sleep(5 * time.Millisecond)
	}

	sessions := playback.NewSessionManager(0, 0)
	sessions.RegisterReconstructed(&playback.Session{
		ID:          sessionID,
		UserID:      1,
		MediaFileID: 91,
		PlayMethod:  playback.PlayTranscode,
	})
	handler := NewPlaybackHandler(sessions)
	handler.TranscodeManager().RegisterTranscodeSession(sessionID, transcodeSession)
	handler.fileResolver = mapPlaybackFileResolver{files: map[int]*models.MediaFile{
		91: {ID: 91, FilePath: "/media/movie.mkv", Container: "mkv"},
	}}
	return handler, transcodeSession
}

func farSeekServe(t *testing.T, handler *PlaybackHandler, sessionID, segmentName string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.HandleGetTranscodeSegment(rec, playbackTestRequest(
		http.MethodGet,
		"/api/v1/playback/transcode/"+sessionID+"/segment/"+segmentName,
		nil,
		map[string]string{"session_id": sessionID, "name": segmentName},
	))
	return rec
}

// TestFarSeekAsyncRestartRotatesGeneration pins that the far-seek restart
// rotates the generation token and that the triggering request is served from
// the rotated generation it created.
func TestFarSeekAsyncRestartRotatesGeneration(t *testing.T) {
	outputDir := t.TempDir()
	handler, transcodeSession := startFarSeekHandlerSession(t, "farseek-rotate", writePlaybackTestFFmpegFarSeek(t), outputDir, playback.TranscodeOpts{
		InputPath:          "/media/movie.mkv",
		TargetCodecVideo:   "h264",
		TargetCodecAudio:   "aac",
		SegmentDuration:    2,
		AudioTrackIndex:    -1,
		SubtitleTrackIndex: -1,
		TotalDuration:      1200,
	})

	const segmentName = "seg_00452.m4s"
	before := transcodeSession.GenerationToken()
	if first := farSeekServe(t, handler, "farseek-rotate", segmentName); first.Code != http.StatusOK || first.Body.String() != "far-seek-bytes" {
		t.Fatalf("far-seek status = %d body=%q, want 200 far-seek-bytes served through the restart", first.Code, first.Body.String())
	}
	if after := transcodeSession.GenerationToken(); after == before {
		t.Fatalf("generation token = %q, want it to rotate across the restart", after)
	}
	if transcodeSession.MatchesGenerationToken(before) {
		t.Fatal("the pre-restart generation token still matched after the restart")
	}
	if _, statErr := os.Stat(filepath.Join(outputDir, segmentName)); statErr != nil {
		t.Fatalf("restart did not produce %s: %v", segmentName, statErr)
	}
}

// TestFarSeekCopyModeStaysSynchronous proves a copy-mode far seek is not
// answered with the async stale-generation verdict: copy target resolution
// reads the live manifest under the lifecycle lock and can legitimately report
// an unresolved target, which stays a retryable 404 on the synchronous path.
func TestFarSeekCopyModeStaysSynchronous(t *testing.T) {
	outputDir := t.TempDir()
	handler, _ := startFarSeekHandlerSession(t, "copy-far-seek", writePlaybackTestFFmpegFarSeek(t), outputDir, playback.TranscodeOpts{
		InputPath:          "/media/movie.mkv",
		TargetCodecVideo:   "copy",
		TargetCodecAudio:   "copy",
		SegmentDuration:    2,
		AudioTrackIndex:    -1,
		SubtitleTrackIndex: -1,
	})

	const segmentName = "seg_00452.m4s"
	rec := farSeekServe(t, handler, "copy-far-seek", segmentName)
	if rec.Code == http.StatusPreconditionFailed || strings.Contains(rec.Body.String(), "stale_generation") {
		t.Fatalf("copy-mode far seek took the async verdict: %d %q", rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("copy-mode far seek status = %d body=%q, want 404 for an unresolved synchronous target", rec.Code, rec.Body.String())
	}
}

// writePlaybackTestFFmpegFarSeekDemux writes a fake ffmpeg that stamps the
// session's source candidate as demux-failed on start (three video-scoped
// "Error during demuxing" lines) and then produces the same short manifest as
// writePlaybackTestFFmpegFarSeek.
func writePlaybackTestFFmpegFarSeekDemux(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-ffmpeg-farseek-demux.sh")
	script := "#!/bin/sh\n" +
		"i=0\n" +
		"while [ $i -lt 3 ]; do echo 'Error during demuxing: vist#0:0' 1>&2; i=$((i+1)); done\n" +
		"last=\"\"\n" +
		"for arg in \"$@\"; do last=\"$arg\"; done\n" +
		"case \" $* \" in *\" -bsfs \"*|*\" -encoders \"*|*\" -f null - \"*) exit 0 ;; esac\n" +
		"case \"$last\" in\n" +
		"  *.m3u8) out=\"$(dirname \"$last\")\"; mkdir -p \"$out\"; " +
		"printf x > \"$out/init.mp4\"; printf x > \"$out/seg_0.m4s\"; " +
		"printf x > \"$out/seg_1.m4s\"; printf x > \"$out/seg_2.m4s\"; " +
		"printf '#EXTM3U\\n#EXT-X-VERSION:7\\n#EXT-X-TARGETDURATION:2\\n" +
		"#EXT-X-MEDIA-SEQUENCE:0\\n#EXT-X-MAP:URI=\"init.mp4\"\\n" +
		"#EXTINF:2.0,\\nseg_0.m4s\\n#EXTINF:2.0,\\nseg_1.m4s\\n" +
		"#EXTINF:2.0,\\nseg_2.m4s\\n' > \"$last\" ;;\n" +
		"esac\n" +
		"exec sleep 30\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	return path
}

// TestFarSeekAsyncFailureDoesNotStaleGenerationLoop pins the poison-latch: a
// far-seek on an already demux-stamped session answers stale_generation once,
// and the next request surfaces the terminal demux verdict instead of
// scheduling another restart and returning stale_generation again.
func TestFarSeekAsyncFailureDoesNotStaleGenerationLoop(t *testing.T) {
	outputDir := t.TempDir()
	handler, transcodeSession := startFarSeekHandlerSession(t, "farseek-demux", writePlaybackTestFFmpegFarSeekDemux(t), outputDir, playback.TranscodeOpts{
		InputPath:          "/media/movie.mkv",
		TargetCodecVideo:   "h264",
		TargetCodecAudio:   "aac",
		SegmentDuration:    2,
		AudioTrackIndex:    -1,
		SubtitleTrackIndex: -1,
		TotalDuration:      1200,
	})

	deadline := time.Now().Add(5 * time.Second)
	for !transcodeSession.IsDemuxFailed() {
		if time.Now().After(deadline) {
			t.Fatal("session never stamped its source candidate demux-failed")
		}
		time.Sleep(5 * time.Millisecond)
	}

	const segmentName = "seg_00452.m4s"
	// The triggering request now waits through the restart, so it surfaces the
	// terminal demux verdict directly instead of answering 412 and letting the
	// next request discover it.
	first := farSeekServe(t, handler, "farseek-demux", segmentName)
	if first.Code == http.StatusPreconditionFailed || strings.Contains(first.Body.String(), "stale_generation") {
		t.Fatalf("first far-seek looped on stale_generation: %d %q", first.Code, first.Body.String())
	}
	if first.Code != http.StatusNotFound {
		t.Fatalf("first far-seek status = %d body=%q, want the terminal demux verdict (404)", first.Code, first.Body.String())
	}

	// The latch is recorded by the detached worker; wait for the observable
	// verdict before the retry.
	deadline = time.Now().Add(5 * time.Second)
	for transcodeSession.AsyncRestartFailure() == nil {
		if time.Now().After(deadline) {
			t.Fatal("the detached restart never latched its demux verdict")
		}
		time.Sleep(5 * time.Millisecond)
	}

	second := farSeekServe(t, handler, "farseek-demux", segmentName)
	if second.Code == http.StatusPreconditionFailed || strings.Contains(second.Body.String(), "stale_generation") {
		t.Fatalf("second request looped on stale_generation: %d %q", second.Code, second.Body.String())
	}
	if second.Code != http.StatusNotFound {
		t.Fatalf("second request status = %d body=%q, want the terminal demux verdict (404)", second.Code, second.Body.String())
	}
}
