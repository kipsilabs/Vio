package playback

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	chimw "github.com/go-chi/chi/v5/middleware"
)

// captureSubtitleTrace runs fn with the default logger redirected to a buffer
// and returns the first JSON entry whose msg matches. Tests using it must not
// call t.Parallel: it swaps the process-wide logger.
func captureSubtitleTrace(t *testing.T, msg string, fn func()) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	fn()

	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if entry["msg"] == msg {
			return entry
		}
	}
	t.Fatalf("no %q line captured; log was:\n%s", msg, buf.String())
	return nil
}

// A copy output is the fetch phase; anything else is a convert. The phase must
// follow streamExtractOutput, the function that builds the ffmpeg command.
func TestSubtitlePhaseForOutputDistinguishesFetchFromConvert(t *testing.T) {
	cases := []struct {
		codec  string
		target string
		want   string
	}{
		{codec: "ass", want: SubtitleTracePhaseFetch},
		{codec: "pgs", want: SubtitleTracePhaseFetch},
		{codec: "subrip", want: SubtitleTracePhaseConvert},
		{codec: "srt", target: "vtt", want: SubtitleTracePhaseConvert},
	}
	for _, tc := range cases {
		if got := subtitlePhaseForOutput(tc.codec, tc.target); got != tc.want {
			t.Fatalf("subtitlePhaseForOutput(%q, %q) = %q, want %q", tc.codec, tc.target, got, tc.want)
		}
	}
}

// The shared field builder omits unknown values rather than emitting misleading
// empties: request_id and codec are absent when unknown, bytes/elapsed are
// absent when unmeasured.
func TestSubtitleTraceFieldsOmitUnknownValues(t *testing.T) {
	ctx := context.WithValue(context.Background(), chimw.RequestIDKey, "req-42")
	fields := subtitleTraceFields(ctx, "subrip", SubtitleTracePhaseConvert, 1234, SubtitleTraceOutcomeSuccess, 1500*time.Millisecond)
	got := map[string]any{}
	for i := 0; i+1 < len(fields); i += 2 {
		got[fields[i].(string)] = fields[i+1]
	}
	for key, want := range map[string]any{
		"request_id": "req-42",
		"codec":      "subrip",
		"phase":      SubtitleTracePhaseConvert,
		"bytes":      int64(1234),
		"outcome":    SubtitleTraceOutcomeSuccess,
		"elapsed_ms": int64(1500),
	} {
		if got[key] != want {
			t.Fatalf("%s = %#v, want %#v", key, got[key], want)
		}
	}

	unknown := subtitleTraceFields(context.Background(), "", SubtitleTracePhaseNotify, SubtitleTraceBytesUnknown, SubtitleTraceOutcomeSkipped, -1)
	got = map[string]any{}
	for i := 0; i+1 < len(unknown); i += 2 {
		got[unknown[i].(string)] = unknown[i+1]
	}
	for _, key := range []string{"request_id", "codec", "bytes", "elapsed_ms"} {
		if _, present := got[key]; present {
			t.Fatalf("%s present for an unknown value: %#v", key, got)
		}
	}
	if got["outcome"] != SubtitleTraceOutcomeSkipped || got["phase"] != SubtitleTracePhaseNotify {
		t.Fatalf("known fields missing: %#v", got)
	}
}

// A completed extract trace carries the request ID, codec, convert phase, the
// measured byte count and a success outcome.
func TestServeExtractEmitsSubtitleTraceWithBytesAndOutcome(t *testing.T) {
	cache := NewSubtitleCache(func() string { return t.TempDir() })
	body := []byte("WEBVTT\n\n00:00.000 --> 00:01.000\nhi\n")

	req := httptest.NewRequest(http.MethodGet, "/subtitles/x.vtt", nil)
	req = req.WithContext(context.WithValue(req.Context(), chimw.RequestIDKey, "req-sub-1"))
	rec := httptest.NewRecorder()

	entry := captureSubtitleTrace(t, "subtitle extract served", func() {
		_, err := cache.ServeExtractWithResult(rec, req, StreamExtractOpts{
			InputPath: "virtual://movie/tt-trace", CacheIdentity: "identity-trace", TrackIndex: 0, SourceCodec: "subrip",
		}, func(_ context.Context, opts StreamExtractOpts) error {
			_, writeErr := opts.Writer.Write(body)
			return writeErr
		})
		if err != nil {
			t.Fatalf("ServeExtractWithResult: %v", err)
		}
	})

	for key, want := range map[string]any{
		"request_id": "req-sub-1",
		"codec":      "subrip",
		"phase":      SubtitleTracePhaseConvert,
		"bytes":      float64(len(body)),
		"outcome":    SubtitleTraceOutcomeSuccess,
	} {
		if entry[key] != want {
			t.Fatalf("%s = %#v, want %#v (entry %#v)", key, entry[key], want, entry)
		}
	}
	if _, present := entry["elapsed_ms"]; !present {
		t.Fatal("elapsed_ms missing from a completed extract trace")
	}
	if got := rec.Body.String(); got != string(body) {
		t.Fatalf("served body = %q, want %q", got, string(body))
	}
}

// A failed extract reports outcome=failure and still counts the bytes that
// reached the client. A cancellation reports outcome=canceled.
func TestSubtitleTraceOutcomeForFailureAndCancellation(t *testing.T) {
	cache := NewSubtitleCache(func() string { return t.TempDir() })

	t.Run("failure", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/subtitles/x.vtt", nil)
		rec := httptest.NewRecorder()
		// A tiny error body (7 bytes) must still be counted and marked failed,
		// not folded into a bare duration.
		entry := captureSubtitleTrace(t, "subtitle extract served", func() {
			_, _ = cache.ServeExtractWithResult(rec, req, StreamExtractOpts{
				InputPath: "virtual://movie/tt-trace-fail", CacheIdentity: "identity-fail", TrackIndex: 0, SourceCodec: "subrip",
			}, func(_ context.Context, opts StreamExtractOpts) error {
				_, _ = opts.Writer.Write([]byte("7 bytes"))
				return context.DeadlineExceeded
			})
		})
		if entry["outcome"] != SubtitleTraceOutcomeFailure {
			t.Fatalf("outcome = %#v, want failure", entry["outcome"])
		}
		if entry["bytes"] != float64(len("7 bytes")) {
			t.Fatalf("bytes = %#v, want %d for the tiny error body", entry["bytes"], len("7 bytes"))
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		req := httptest.NewRequest(http.MethodGet, "/subtitles/x.vtt", nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		entry := captureSubtitleTrace(t, "subtitle extract served", func() {
			_, _ = cache.ServeExtractWithResult(rec, req, StreamExtractOpts{
				InputPath: "virtual://movie/tt-trace-cancel", CacheIdentity: "identity-cancel", TrackIndex: 0, SourceCodec: "subrip",
			}, func(_ context.Context, opts StreamExtractOpts) error {
				_, _ = opts.Writer.Write([]byte("partial"))
				cancel()
				return ctx.Err()
			})
		})
		if entry["outcome"] != SubtitleTraceOutcomeCanceled {
			t.Fatalf("outcome = %#v, want canceled", entry["outcome"])
		}
	})
}

// A detached warm trace names its phase and outcome, so a warm that never
// reaches the client is still attributable.
func TestWarmTrackEmitsSubtitleTraceWithPhaseAndOutcome(t *testing.T) {
	cache := NewSubtitleCache(func() string { return t.TempDir() })
	entry := captureSubtitleTrace(t, "subtitle cache warm finished", func() {
		done := cache.WarmTrackInBackground(StreamExtractOpts{
			InputPath: "virtual://movie/tt-warm", CacheIdentity: "identity-warm", TrackIndex: 0, SourceCodec: "subrip",
		}, func(_ context.Context, opts StreamExtractOpts) error {
			_, err := opts.Writer.Write([]byte("WEBVTT\n\n"))
			return err
		})
		<-done
	})
	if entry["phase"] != SubtitleTracePhaseWarm {
		t.Fatalf("phase = %#v, want warm", entry["phase"])
	}
	if entry["outcome"] != SubtitleTraceOutcomeCommitted {
		t.Fatalf("outcome = %#v, want committed", entry["outcome"])
	}
	if entry["codec"] != "subrip" {
		t.Fatalf("codec = %#v, want subrip", entry["codec"])
	}
	if bytes, ok := entry["bytes"].(float64); !ok || bytes <= 0 {
		t.Fatalf("bytes = %#v, want a positive measured count", entry["bytes"])
	}
}

// The counting writer must forward Flush so wrapping the stream extract's
// per-chunk flushing writer keeps delivering cues in real time.
func TestSubtitleCountingWriterForwardsFlush(t *testing.T) {
	flusher := &recordingFlusher{}
	counter := &subtitleCountingWriter{w: flusher}
	if _, err := counter.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	counter.Flush()
	if counter.bytes != 3 {
		t.Fatalf("bytes = %d, want 3", counter.bytes)
	}
	if flusher.flushes != 1 {
		t.Fatalf("flushes = %d, want 1", flusher.flushes)
	}
}

type recordingFlusher struct {
	bytes   bytes.Buffer
	flushes int
}

func (f *recordingFlusher) Write(p []byte) (int, error) { return f.bytes.Write(p) }
func (f *recordingFlusher) Flush()                      { f.flushes++ }
