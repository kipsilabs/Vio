package handlers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestFontExtractFailLogThrottlesRepeats pins the font-extraction log throttle:
// the first failure per file+track warns, repeats drop to debug, and a
// successful extraction clears the key so a later regression warns again. The
// extraction itself never fails playback — the endpoint 500 stays the signal.
func TestFontExtractFailLogThrottlesRepeats(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	var l fontExtractFailLog
	ctx := context.Background()
	key := fontExtractFailureKey(7, 1)

	l.failed(ctx, key, "file_id", 7, "track", 1)
	if got := strings.Count(logs.String(), `"level":"WARN"`); got != 1 {
		t.Fatalf("warn count after first failure = %d, want 1\n%s", got, logs.String())
	}

	l.failed(ctx, key, "file_id", 7, "track", 1)
	if got := strings.Count(logs.String(), `"level":"WARN"`); got != 1 {
		t.Fatalf("warn count after repeat = %d, want the repeat throttled\n%s", got, logs.String())
	}
	if got := strings.Count(logs.String(), `"level":"DEBUG"`); got != 1 {
		t.Fatalf("debug count after repeat = %d, want 1\n%s", got, logs.String())
	}

	l.recovered(key)
	l.failed(ctx, key, "file_id", 7, "track", 1)
	if got := strings.Count(logs.String(), `"level":"WARN"`); got != 2 {
		t.Fatalf("warn count after recovery = %d, want the key to warn again\n%s", got, logs.String())
	}
}

// TestFontExtractFailLogRetentionBounded pins the process-lifetime bound: every
// failure for a unique key must not grow the throttle map beyond
// fontExtractFailLogMaxEntries. Before the cap this leaked one entry per
// unique session/track for as long as the process ran.
func TestFontExtractFailLogRetentionBounded(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	var l fontExtractFailLog
	ctx := context.Background()
	unique := fontExtractFailLogMaxEntries * 4
	for i := range unique {
		l.failed(ctx, fmt.Sprintf("session:sess-%d:track:%d", i, i%9), "session", fmt.Sprintf("sess-%d", i), "track", i%9)
	}
	l.mu.Lock()
	size := len(l.seen)
	l.mu.Unlock()
	if size != fontExtractFailLogMaxEntries {
		t.Fatalf("throttle map size = %d, want capped at %d", size, fontExtractFailLogMaxEntries)
	}
	// The very first key was evicted long ago, so failing it again must warn
	// (its old entry is gone) rather than stay throttled as a repeat.
	warnsBefore := strings.Count(logs.String(), `"level":"WARN"`)
	l.failed(ctx, "session:sess-0:track:0", "session", "sess-0", "track", 0)
	if got := strings.Count(logs.String(), `"level":"WARN"`); got != warnsBefore+1 {
		t.Fatalf("warn count delta after eviction = %d, want the evicted key to warn again\nlogs tail: %s", got-warnsBefore, logs.String()[max(0, len(logs.String())-2000):])
	}
	if got := len(l.seen); got > fontExtractFailLogMaxEntries {
		t.Fatalf("throttle map size after re-failure = %d, want <= %d", got, fontExtractFailLogMaxEntries)
	}
}

// TestFontExtractFailLogExpiredTTLWarnsAgain pins the TTL: an entry that never
// sees a success path (failed or abandoned session) expires after
// fontExtractFailLogTTL, so the target's next failure warns again instead of
// staying throttled to debug forever.
func TestFontExtractFailLogExpiredTTLWarnsAgain(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	current := time.Now()
	var l fontExtractFailLog
	l.now = func() time.Time { return current }
	ctx := context.Background()
	key := fontExtractFailureKey(11, 2)

	l.failed(ctx, key, "file_id", 11, "track", 2)
	l.failed(ctx, key, "file_id", 11, "track", 2) // throttled to debug
	if got := strings.Count(logs.String(), `"level":"WARN"`); got != 1 {
		t.Fatalf("warn count before TTL = %d, want 1\n%s", got, logs.String())
	}

	current = current.Add(fontExtractFailLogTTL)
	l.failed(ctx, key, "file_id", 11, "track", 2)
	if got := strings.Count(logs.String(), `"level":"WARN"`); got != 2 {
		t.Fatalf("warn count after TTL expiry = %d, want the expired entry to warn again\n%s", got, logs.String())
	}
}

// TestFontExtractFailLogRecoveryMatchesFailure pins the identity fix: the key
// recovered() clears is the key failed() inserted (cause travels as an
// attribute now), so failure → recovery → failure warns again even when the
// two failures carry different causes.
func TestFontExtractFailLogRecoveryMatchesFailure(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	var l fontExtractFailLog
	ctx := context.Background()
	key := fontExtractFailureKey(42, 3)

	l.failed(ctx, key, "file_id", 42, "track", 3, "cause", "session_load", "error", errors.New("load"))
	l.recovered(key)
	l.failed(ctx, key, "file_id", 42, "track", 3, "cause", "source_preflight", "error", errors.New("preflight"))
	if got := strings.Count(logs.String(), `"level":"WARN"`); got != 2 {
		t.Fatalf("warn count after failure → recovery → failure = %d, want 2\n%s", got, logs.String())
	}
}

// TestHandleSubtitleFontsPendingKeepsFailureThrottleKey pins the ordering fix:
// a request that serves a pending bundle (extraction still in flight) must not
// clear a prior failure key, because recovery is not yet confirmed. A slow
// retry that also fails must therefore stay throttled to debug.
func TestHandleSubtitleFontsPendingKeepsFailureThrottleKey(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	restore := setFontBundleClientWait(50 * time.Millisecond)
	defer restore()

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	handler, session, file, _, gatePath := newFontBundleHTTPFixture(t, assTestTracks(), "gate")
	// Release the gated extraction before the test ends so the detached shell
	// probe cannot spin forever against a removed temp dir.
	defer func() { _ = os.WriteFile(gatePath, []byte("go"), 0o600) }()

	key := fontExtractFailureKey(file.ID, 0)
	// Simulate the prior failure the throttle already saw for this file+track.
	handler.fontExtractFailures.failed(context.Background(), key, "file_id", file.ID, "track", 0)

	recorder := httptest.NewRecorder()
	handler.HandleSubtitleFonts(recorder, fontBundleHTTPRequest(session.ID, "0"))
	if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != "[]" {
		t.Fatalf("pending response = %d %q, want 200 empty bundle", recorder.Code, recorder.Body.String())
	}

	// The failure key must survive the pending response, so the next failure
	// for the same target throttles to debug instead of warning again.
	handler.fontExtractFailures.failed(context.Background(), key, "file_id", file.ID, "track", 0)
	if got := strings.Count(logs.String(), `"level":"WARN"`); got != 1 {
		t.Fatalf("warn count after pending retry = %d, want the prior failure key kept\n%s", got, logs.String())
	}
}

// TestHandleSubtitleFontsCacheHitClearsFailureThrottleKey pins the recovery
// path: a detached extraction can commit between requests, so serving its
// cached bundle must clear the failure key and let a later regression warn.
func TestHandleSubtitleFontsCacheHitClearsFailureThrottleKey(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	restore := setFontBundleClientWait(50 * time.Millisecond)
	defer restore()

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	handler, session, file, _, gatePath := newFontBundleHTTPFixture(t, assTestTracks(), "gate")

	// First request serves a pending bundle; release the gate and wait for the
	// detached extraction to commit so the next request is a cache hit.
	recorder := httptest.NewRecorder()
	handler.HandleSubtitleFonts(recorder, fontBundleHTTPRequest(session.ID, "0"))
	if err := os.WriteFile(gatePath, []byte("go"), 0o600); err != nil {
		t.Fatalf("release gate: %v", err)
	}
	key := fontBundleCacheKey(file, "", handler.ffmpegPath())
	if !pollHandlerFontBundle(t, handler.SubtitleCache, key) {
		t.Fatal("detached extraction never committed to the cache")
	}

	// Seed a prior failure, then serve the cache hit: the hit is confirmed
	// recovery, so the key clears and the next failure warns again.
	logicalKey := fontExtractFailureKey(file.ID, 0)
	handler.fontExtractFailures.failed(context.Background(), logicalKey, "file_id", file.ID, "track", 0)

	hit := httptest.NewRecorder()
	handler.HandleSubtitleFonts(hit, fontBundleHTTPRequest(session.ID, "0"))
	if hit.Code != http.StatusOK {
		t.Fatalf("cache-hit response = %d, want 200", hit.Code)
	}

	handler.fontExtractFailures.failed(context.Background(), logicalKey, "file_id", file.ID, "track", 0)
	if got := strings.Count(logs.String(), `"level":"WARN"`); got != 2 {
		t.Fatalf("warn count after cache-hit recovery = %d, want the key cleared and warned again\n%s", got, logs.String())
	}
}

// TestSubtitleFontInternalErrorLogsCauseOnce pins the diagnostic gap: the font
// route's internal_error returns (session load, source preflight) used to log
// nothing, so a client retrying a broken font URL produced a flood the v2
// request log described only as a bare internal_error. The first failure per
// target now warns with file_id+track and the cause; repeats drop to debug.
func TestSubtitleFontInternalErrorLogsCauseOnce(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	var handler StreamHandler
	in := SubtitleFontRequest{SessionID: "sess-1", Track: "3"}
	cause := errors.New("media file is gone")

	handler.logSubtitleFontInternalError(context.Background(), in, 42, 3, "source_preflight", cause)
	body := logs.String()
	if got := strings.Count(body, `"level":"WARN"`); got != 1 {
		t.Fatalf("warn count after first internal error = %d, want 1\n%s", got, body)
	}
	if !strings.Contains(body, `"file_id":42`) || !strings.Contains(body, `"track":3`) ||
		!strings.Contains(body, `"cause":"source_preflight"`) ||
		!strings.Contains(body, "media file is gone") {
		t.Fatalf("first internal error missing file/track/cause: %s", body)
	}

	// A repeat of the same cause stays throttled to debug.
	handler.logSubtitleFontInternalError(context.Background(), in, 42, 3, "source_preflight", cause)
	if got := strings.Count(logs.String(), `"level":"WARN"`); got != 1 {
		t.Fatalf("repeat internal error warned again: %d warns\n%s", got, logs.String())
	}
	if got := strings.Count(logs.String(), `"level":"DEBUG"`); got != 1 {
		t.Fatalf("repeat internal error debug count = %d, want 1", got)
	}
}

// TestSubtitleFontInternalErrorLogsBeforeFileResolution pins the session-load
// path: when the file id is not known yet, the log still carries the session
// and track so a flood is diagnosable.
func TestSubtitleFontInternalErrorLogsBeforeFileResolution(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	var handler StreamHandler
	handler.logSubtitleFontInternalError(
		context.Background(),
		SubtitleFontRequest{SessionID: "sess-9", Track: "1"},
		0, -1, "session_load", errors.New("reconstruct failed"),
	)
	body := logs.String()
	if got := strings.Count(body, `"level":"WARN"`); got != 1 {
		t.Fatalf("warn count = %d, want 1\n%s", got, body)
	}
	if !strings.Contains(body, `"session":"sess-9"`) || !strings.Contains(body, `"track":"1"`) ||
		!strings.Contains(body, `"cause":"session_load"`) {
		t.Fatalf("session-load internal error missing session/track/cause: %s", body)
	}
}
