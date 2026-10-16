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

// TestFarSeekBeyondProducedWindowAnswersStaleGenerationImmediately pins the
// fast-start seek fix: a request for a segment far past the produced window is
// recovered by an asynchronous seek restart, so the triggering request must not
// be held for the restart's bytes (which its own generation fence would refuse).
// It answers the stale-generation verdict immediately and a later request for
// the same segment is served once the restart has produced it.
func TestFarSeekBeyondProducedWindowAnswersStaleGenerationImmediately(t *testing.T) {
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

	if first.Code != http.StatusPreconditionFailed {
		t.Fatalf("far-seek status = %d body=%q, want %d (stale generation)", first.Code, first.Body.String(), http.StatusPreconditionFailed)
	}
	if !strings.Contains(first.Body.String(), "stale_generation") {
		t.Fatalf("far-seek body = %q, want the stale_generation code", first.Body.String())
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("far-seek request held for %s, want an immediate stale-generation answer (<100ms)", elapsed)
	}

	// The restart runs detached; wait on the observable output (the segment on
	// disk), not a fixed sleep.
	deadline = time.Now().Add(5 * time.Second)
	for {
		if _, statErr := os.Stat(filepath.Join(outputDir, segmentName)); statErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("asynchronous seek restart never produced the requested segment")
		}
		time.Sleep(5 * time.Millisecond)
	}

	second := serve()
	if second.Code != http.StatusOK {
		t.Fatalf("post-restart status = %d body=%q, want 200 once the segment is produced", second.Code, second.Body.String())
	}
	if second.Body.String() != "far-seek-bytes" {
		t.Fatalf("post-restart body = %q, want the restarted generation's bytes", second.Body.String())
	}
}

// TestFarSeekSignedGenerationRecoversWithinBudget drives the client recovery
// contract the web player depends on: a request signed with the generation it
// holds (sgen) gets the 412 stale-generation verdict immediately, the manifest
// reloads at once, and the replacement init and segment are servable under the
// generation the reload advertises — inside the two-consecutive-recovery budget
// from #323. The existing immediate-verdict test waits for output on disk and
// omits sgen, so it never exercises the reload-then-serve handoff.
func TestFarSeekSignedGenerationRecoversWithinBudget(t *testing.T) {
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

	// 1. The signed segment request is answered the stale verdict at once. This
	// is the recovery the client counts against its budget.
	first := httptest.NewRecorder()
	handler.HandleGetTranscodeSegment(first, playbackTestRequest(
		http.MethodGet,
		"/api/v1/playback/transcode/"+sessionID+"/segment/"+segmentName+"?sgen="+url.QueryEscape(staleToken),
		nil,
		map[string]string{"session_id": sessionID, "name": segmentName},
	))
	if first.Code != http.StatusPreconditionFailed || !strings.Contains(first.Body.String(), "stale_generation") {
		t.Fatalf("signed far-seek status = %d body=%q, want %d stale_generation", first.Code, first.Body.String(), http.StatusPreconditionFailed)
	}

	// 2. Reload the manifest immediately; it must answer without waiting for the
	// detached restart, then advertise the replacement generation. One reload is
	// one recovery, well inside the two-consecutive-recovery budget.
	var replacementToken string
	deadline := time.Now().Add(5 * time.Second)
	for {
		manifestRec := httptest.NewRecorder()
		handler.HandleGetTranscodeManifest(manifestRec, playbackTestRequest(
			http.MethodGet,
			"/api/v1/playback/transcode/"+sessionID+"/master.m3u8",
			nil,
			map[string]string{"session_id": sessionID},
		))
		if manifestRec.Code != http.StatusOK {
			t.Fatalf("manifest reload status = %d body=%q, want 200 immediately after the verdict", manifestRec.Code, manifestRec.Body.String())
		}
		if token := manifestGenerationToken(t, manifestRec.Body.Bytes()); token != "" && token != staleToken {
			replacementToken = token
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the manifest never advertised a replacement generation within the recovery budget")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if transcodeSession.MatchesGenerationToken(staleToken) {
		t.Fatal("the stale generation token still matched the replacement manifest")
	}

	// 3. The replacement generation serves both the init segment and the sought
	// segment under the token the reload advertised.
	initRec := httptest.NewRecorder()
	handler.HandleGetTranscodeSegment(initRec, playbackTestRequest(
		http.MethodGet,
		"/api/v1/playback/transcode/"+sessionID+"/segment/init.mp4?sgen="+url.QueryEscape(replacementToken),
		nil,
		map[string]string{"session_id": sessionID, "name": "init.mp4"},
	))
	if initRec.Code != http.StatusOK {
		t.Fatalf("replacement init status = %d body=%q, want 200", initRec.Code, initRec.Body.String())
	}

	segRec := httptest.NewRecorder()
	handler.HandleGetTranscodeSegment(segRec, playbackTestRequest(
		http.MethodGet,
		"/api/v1/playback/transcode/"+sessionID+"/segment/"+segmentName+"?sgen="+url.QueryEscape(replacementToken),
		nil,
		map[string]string{"session_id": sessionID, "name": segmentName},
	))
	if segRec.Code != http.StatusOK || segRec.Body.String() != "far-seek-bytes" {
		t.Fatalf("replacement segment status = %d body=%q, want 200 far-seek-bytes", segRec.Code, segRec.Body.String())
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

// TestFarSeekAsyncRestartRotatesGeneration pins that the detached far-seek
// restart actually rotates the generation token, which is why the triggering
// request cannot be held for the restart's bytes.
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
	if first := farSeekServe(t, handler, "farseek-rotate", segmentName); first.Code != http.StatusPreconditionFailed {
		t.Fatalf("far-seek status = %d body=%q, want 412 before the restart", first.Code, first.Body.String())
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, statErr := os.Stat(filepath.Join(outputDir, segmentName)); statErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("asynchronous seek restart never produced the requested segment")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if after := transcodeSession.GenerationToken(); after == before {
		t.Fatalf("generation token = %q, want it to rotate across the restart", after)
	}
	if transcodeSession.MatchesGenerationToken(before) {
		t.Fatal("the pre-restart generation token still matched after the restart")
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
	if first := farSeekServe(t, handler, "farseek-demux", segmentName); first.Code != http.StatusPreconditionFailed {
		t.Fatalf("first far-seek status = %d body=%q, want 412", first.Code, first.Body.String())
	}

	// The latch is recorded by the detached worker; wait for the observable
	// verdict before the retry, mirroring a client that reloads the manifest
	// after the 412.
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
