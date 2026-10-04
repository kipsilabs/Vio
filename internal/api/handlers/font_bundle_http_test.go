package handlers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

const fontProbeJSON = `cat <<'JSON'
{"streams":[{"index":2,"codec_name":"ttf","codec_type":"attachment","extradata_size":4,"tags":{"filename":"MyFont.ttf","mimetype":"font/ttf"}}]}
JSON
`

func writeExecutableScript(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func fontProbeScript(probeLog, gatePath string, gate bool, fail bool) string {
	script := "#!/bin/sh\necho probe >> '" + probeLog + "'\n"
	if fail {
		return script + "exit 1\n"
	}
	if gate {
		script += "while [ ! -f '" + gatePath + "' ]; do sleep 0.01; done\n"
	}
	return script + fontProbeJSON
}

func fontFFmpegDumpScript(dumpLog string) string {
	return "#!/bin/sh\necho dump >> '" + dumpLog + "'\nfor a in \"$@\"; do\n" +
		"  case \"$a\" in\n    -dump_attachment:*) printf 'FONT' ;;\n  esac\ndone\n"
}

// setFontBundleClientWait temporarily overrides the handler's client-facing
// wait so tests don't pay the production budget.
func setFontBundleClientWait(d time.Duration) func() {
	old := fontBundleClientWait
	fontBundleClientWait = d
	return func() { fontBundleClientWait = old }
}

func countLogLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return 0
	}
	return len(strings.Split(trimmed, "\n"))
}

func assTestTracks() []models.SubtitleTrack {
	return []models.SubtitleTrack{{Index: 4, Codec: "ass"}, {Index: 7, Codec: "ass"}}
}

// newFontBundleHTTPFixture builds a StreamHandler whose font extraction runs
// through a fake ffprobe/ffmpeg pair. probeMode is "ok", "gate", or "fail".
func newFontBundleHTTPFixture(t *testing.T, tracks []models.SubtitleTrack, probeMode string) (*StreamHandler, *playback.Session, *models.MediaFile, string, string) {
	t.Helper()
	dir := t.TempDir()
	probeLog := filepath.Join(dir, "probe.log")
	dumpLog := filepath.Join(dir, "dump.log")
	gatePath := filepath.Join(dir, "gate")
	writeExecutableScript(t, filepath.Join(dir, "ffprobe"), fontProbeScript(probeLog, gatePath, probeMode == "gate", probeMode == "fail"))
	writeExecutableScript(t, filepath.Join(dir, "ffmpeg"), fontFFmpegDumpScript(dumpLog))

	mediaPath := writePlaybackTestMediaFile(t, "movie.mkv")
	info, err := os.Stat(mediaPath)
	if err != nil {
		t.Fatalf("stat media file: %v", err)
	}
	mtime := info.ModTime()
	file := &models.MediaFile{ID: 42, ContentID: "movie-1", FilePath: mediaPath,
		FileSize: info.Size(), FileModifiedAt: &mtime, SubtitleTracks: tracks}
	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	handler := NewStreamHandler(manager, testPlaybackFileResolver{file: file})
	handler.PlaybackConfig = playbackTestConfig(filepath.Join(dir, "ffmpeg"), dir)
	handler.SubtitleCache = playback.NewSubtitleCache(func() string { return dir })
	return handler, session, file, probeLog, gatePath
}

func fontBundleHTTPRequest(sessionID, track string) *http.Request {
	return playbackTestRequest(http.MethodGet,
		"/api/v1/stream/"+sessionID+"/subtitles/"+track+"/fonts?file_id=42", nil,
		map[string]string{"session_id": sessionID, "track": track})
}

func pollHandlerFontBundle(t *testing.T, c *playback.SubtitleCache, key playback.FontBundleKey) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := c.LookupFontBundle(key); ok {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// The cold path must hand the client a valid empty bundle after a short budget
// instead of holding the request for the extraction. The detached flight keeps
// running and commits for the next fetch.
func TestHandleSubtitleFontsReturnsEmptyBeforeDetachedExtractionCompletes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	restore := setFontBundleClientWait(50 * time.Millisecond)
	defer restore()

	handler, session, file, probeLog, gatePath := newFontBundleHTTPFixture(t, assTestTracks(), "gate")
	recorder := httptest.NewRecorder()
	start := time.Now()
	handler.HandleSubtitleFonts(recorder, fontBundleHTTPRequest(session.ID, "0"))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200 empty bundle", recorder.Code, recorder.Body.String())
	}
	if strings.TrimSpace(recorder.Body.String()) != "[]" {
		t.Fatalf("body = %q, want empty bundle", recorder.Body.String())
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("cold font fetch took %s, want a bounded wait", elapsed)
	}

	// Release the extraction; it must still land in the cache.
	if err := os.WriteFile(gatePath, []byte("go"), 0o600); err != nil {
		t.Fatalf("release gate: %v", err)
	}
	key := fontBundleCacheKey(file, "", handler.ffmpegPath())
	if !pollHandlerFontBundle(t, handler.SubtitleCache, key) {
		t.Fatal("detached extraction never committed to the cache")
	}
	if got := countLogLines(t, probeLog); got != 1 {
		t.Fatalf("ffprobe ran %d times, want 1", got)
	}
}

func TestHandleSubtitleFontsReturns500OnDefinitiveFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	handler, session, _, _, _ := newFontBundleHTTPFixture(t, assTestTracks(), "fail")
	recorder := httptest.NewRecorder()
	handler.HandleSubtitleFonts(recorder, fontBundleHTTPRequest(session.ID, "0"))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s, want 500 for a definitive extraction failure", recorder.Code, recorder.Body.String())
	}
}

// A non-ASS embedded ordinal must answer 400, not reach extraction or the
// cache paths. The client prefetch guards on codec, but a mistyped or
// hand-built URL must not mint a 500 a retrying client re-issues.
func TestHandleSubtitleFontsRejectsNonASSTrackBeforeExtraction(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	handler, session, _, probeLog, _ := newFontBundleHTTPFixture(t, []models.SubtitleTrack{{Index: 4, Codec: "srt"}}, "ok")
	recorder := httptest.NewRecorder()
	handler.HandleSubtitleFonts(recorder, fontBundleHTTPRequest(session.ID, "0"))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s, want 400 for a non-ASS font request", recorder.Code, recorder.Body.String())
	}
	if got := countLogLines(t, probeLog); got != 0 {
		t.Fatalf("ffprobe ran %d times for a rejected request, want 0", got)
	}
}

// An ordinal outside the embedded range must answer 404 without reaching the
// extraction or cache paths (which can 500 on a missing file or bind a
// shared flight for a request no retry can satisfy).
func TestHandleSubtitleFontsRejectsOutOfRangeOrdinalBeforePreflight(t *testing.T) {
	handler, session, _, _, _ := newFontBundleHTTPFixture(t, assTestTracks(), "fail")
	recorder := httptest.NewRecorder()
	handler.HandleSubtitleFonts(recorder, fontBundleHTTPRequest(session.ID, "9"))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s, want 404 for an out-of-range ordinal", recorder.Code, recorder.Body.String())
	}
}

// Two parallel track requests for the same file must not run two extractions:
// the fast-empty path still registers the shared flight.
func TestHandleSubtitleFontsCoalescesParallelTrackRequests(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	restore := setFontBundleClientWait(50 * time.Millisecond)
	defer restore()

	handler, session, file, probeLog, gatePath := newFontBundleHTTPFixture(t, assTestTracks(), "gate")

	var wg sync.WaitGroup
	for _, track := range []string{"0", "1"} {
		wg.Add(1)
		go func(track string) {
			defer wg.Done()
			recorder := httptest.NewRecorder()
			handler.HandleSubtitleFonts(recorder, fontBundleHTTPRequest(session.ID, track))
			if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != "[]" {
				t.Errorf("track %s = %d %q, want 200 empty bundle", track, recorder.Code, recorder.Body.String())
			}
		}(track)
	}
	wg.Wait()

	if err := os.WriteFile(gatePath, []byte("go"), 0o600); err != nil {
		t.Fatalf("release gate: %v", err)
	}
	key := fontBundleCacheKey(file, "", handler.ffmpegPath())
	if !pollHandlerFontBundle(t, handler.SubtitleCache, key) {
		t.Fatal("coalesced extraction never committed to the cache")
	}
	if got := countLogLines(t, probeLog); got != 1 {
		t.Fatalf("ffprobe ran %d times for two parallel requests, want exactly 1", got)
	}
}
