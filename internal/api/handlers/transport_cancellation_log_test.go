package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// levelRecordingHandler captures slog records so tests can assert on both the
// level and the message of a log line.
type levelRecordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *levelRecordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *levelRecordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.records = append(h.records, r.Clone())
	h.mu.Unlock()
	return nil
}

func (h *levelRecordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *levelRecordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *levelRecordingHandler) has(level slog.Level, msg string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, record := range h.records {
		if record.Level == level && record.Message == msg {
			return true
		}
	}
	return false
}

func TestIsClientCancellation(t *testing.T) {
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"direct canceled", context.Background(), context.Canceled, true},
		{"wrapped canceled", context.Background(), fmt.Errorf("resolve: %w", context.Canceled), true},
		{"canceled ctx masks error", canceledCtx, errors.New("provider blew up"), true},
		{"deadline is not cancellation", context.Background(), context.DeadlineExceeded, false},
		{"provider error is not cancellation", context.Background(), errors.New("provider returned HTTP 500"), false},
		{"nil error and ctx", context.Background(), nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isClientCancellation(tc.ctx, tc.err); got != tc.want {
				t.Fatalf("isClientCancellation(%v, %v) = %v, want %v", tc.ctx, tc.err, got, tc.want)
			}
		})
	}
}

func TestLogVirtualStreamFailureDowngradesClientCancellation(t *testing.T) {
	capture := &levelRecordingHandler{}
	previous := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(previous) })

	file := &models.MediaFile{ID: 7, FilePath: "virtual://movie/tt14538850"}

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	logVirtualStreamFailure(canceledCtx, "session-1", file, fmt.Errorf("resolve virtual input: %w", context.Canceled))

	if capture.has(slog.LevelWarn, "virtual stream transport failed") {
		t.Fatal("client cancellation logged as a WARN transport failure")
	}
	if !capture.has(slog.LevelDebug, "virtual stream transport canceled by client") {
		t.Fatal("client cancellation did not log the debug cancellation line")
	}

	// A genuine timeout must still warn.
	logVirtualStreamFailure(context.Background(), "session-1", file, fmt.Errorf("provider fetch: %w", context.DeadlineExceeded))
	if !capture.has(slog.LevelWarn, "virtual stream transport failed") {
		t.Fatal("timeout did not log as a WARN transport failure")
	}
}

// The failure line must join to the serve attempt: it carries the playback
// session id and both the pinned and the delivered candidate ids, so a
// virtual_stream_unavailable names the release it actually served instead of
// only its session. A rotation that rebinds the served path must attribute
// the sibling, not the pin; a resolve failure that never served one reports
// both identities honestly (pin set, delivered empty).
func TestLogVirtualStreamFailureCarriesSessionAndCandidate(t *testing.T) {
	capture := func(t *testing.T, file *models.MediaFile, err error, delivered ...string) map[string]any {
		t.Helper()
		var buf bytes.Buffer
		previous := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		t.Cleanup(func() { slog.SetDefault(previous) })

		logVirtualStreamFailure(context.Background(), "session-9", file, err, delivered...)

		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			var candidate map[string]any
			if err := json.Unmarshal([]byte(line), &candidate); err != nil {
				continue
			}
			if candidate["msg"] == "virtual stream transport failed" {
				return candidate
			}
		}
		t.Fatalf("no virtual stream transport failed line captured; log was:\n%s", buf.String())
		return nil
	}

	// Common path: served the pin, both identities agree.
	file := &models.MediaFile{ID: 7, FilePath: "virtual://movie/tt14538850?result=cand-9"}
	entry := capture(t, file, errors.New("relay returned HTTP 502"), "virtual://movie/tt14538850?result=cand-9")
	if entry["playback_session_id"] != "session-9" {
		t.Fatalf("playback_session_id = %#v, want session-9", entry["playback_session_id"])
	}
	if entry["pinned_candidate_id"] != "cand-9" {
		t.Fatalf("pinned_candidate_id = %#v, want cand-9", entry["pinned_candidate_id"])
	}
	if entry["delivered_candidate_id"] != "cand-9" {
		t.Fatalf("delivered_candidate_id = %#v, want cand-9", entry["delivered_candidate_id"])
	}

	// A → B rotation that then fails attributes the sibling, not the pin.
	entry = capture(t, file, errors.New("relay returned HTTP 502"), "virtual://movie/tt14538850?result=cand-4")
	if entry["pinned_candidate_id"] != "cand-9" {
		t.Fatalf("pinned_candidate_id = %#v, want cand-9", entry["pinned_candidate_id"])
	}
	if entry["delivered_candidate_id"] != "cand-4" {
		t.Fatalf("delivered_candidate_id = %#v, want cand-4 (the served sibling, not the pin)", entry["delivered_candidate_id"])
	}

	// A resolve failure serves nothing: delivered stays empty rather than
	// echoing the pin.
	entry = capture(t, file, errors.New("no streams available from provider"))
	if entry["pinned_candidate_id"] != "cand-9" {
		t.Fatalf("pinned_candidate_id = %#v, want cand-9", entry["pinned_candidate_id"])
	}
	if delivered, present := entry["delivered_candidate_id"]; !present || delivered != "" {
		t.Fatalf("delivered_candidate_id = %#v, want empty (nothing served)", entry["delivered_candidate_id"])
	}
}

// The proxy verdict names how a failed direct-play proxy ended before any
// heal or rotation runs, so the verdict log records the release and the path
// taken while that state is still observable. Each disposition maps to one
// first-attempt outcome; client cancellation stays debug and never warns.
func TestVirtualProxyDisposition(t *testing.T) {
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name                          string
		ctx                           context.Context
		err                           error
		status                        int
		relayNotFound, relayTemporary bool
		want                          string
	}{
		{"client canceled wins", canceledCtx, errors.New("boom"), 0, false, false, "client_canceled"},
		{"relay not found", context.Background(), errors.New("relay token not found"), 0, true, false, "relay_not_found"},
		{"relay temporary", context.Background(), errors.New("relay returned HTTP 502"), 0, false, true, "relay_temporary"},
		{"committed headers", context.Background(), errors.New("boom"), 206, false, false, "committed"},
		{"failed open", context.Background(), errors.New("boom"), 0, false, false, "failed_open"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := virtualProxyDisposition(tc.ctx, tc.err, tc.status, tc.relayNotFound, tc.relayTemporary); got != tc.want {
				t.Fatalf("virtualProxyDisposition = %q, want %q", got, tc.want)
			}
		})
	}
}

// The verdict line joins to the serve attempt: session, pinned and delivered
// candidate ids, and the disposition above. The client only ever sees the
// generic problem envelope; this line is the server-side join key.
func TestLogVirtualProxyVerdictCarriesJoinKeys(t *testing.T) {
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	file := &models.MediaFile{ID: 7, FilePath: "virtual://movie/tt14538850?result=cand-9"}
	logVirtualProxyVerdict(context.Background(), "session-9", file, "virtual://movie/tt14538850?result=cand-9", "failed_open", errors.New("relay returned HTTP 502"))

	var entry map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var candidate map[string]any
		if err := json.Unmarshal([]byte(line), &candidate); err != nil {
			continue
		}
		if candidate["msg"] == "virtual stream proxy verdict" {
			entry = candidate
			break
		}
	}
	if entry == nil {
		t.Fatalf("no virtual stream proxy verdict line captured; log was:\n%s", buf.String())
	}
	for key, want := range map[string]any{
		"playback_session_id":    "session-9",
		"pinned_candidate_id":    "cand-9",
		"delivered_candidate_id": "cand-9",
		"disposition":            "failed_open",
	} {
		if entry[key] != want {
			t.Fatalf("%s = %#v, want %#v", key, entry[key], want)
		}
	}

	// Client cancellation downgrades to debug so it cannot be mistaken for a
	// provider outage.
	buf.Reset()
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	logVirtualProxyVerdict(canceledCtx, "session-9", file, "virtual://movie/tt14538850?result=cand-9", "client_canceled", context.Canceled)
	found := false
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var candidate map[string]any
		if err := json.Unmarshal([]byte(line), &candidate); err != nil {
			continue
		}
		if candidate["msg"] != "virtual stream proxy verdict" {
			continue
		}
		found = true
		if candidate["level"] != "DEBUG" {
			t.Fatalf("canceled verdict level = %#v, want DEBUG", candidate["level"])
		}
	}
	if !found {
		t.Fatalf("no canceled verdict line captured; log was:\n%s", buf.String())
	}
}

func TestHandleTransportStartFailureDowngradesClientCancellation(t *testing.T) {
	capture := &levelRecordingHandler{}
	previous := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(previous) })

	filePath := writePlaybackTestMediaFile(t, "movie.mkv")
	file := &models.MediaFile{
		ID:        42,
		ContentID: "movie-1",
		FilePath:  filePath,
		Duration:  3600,
	}
	baseMgr := playback.NewSessionManager(0, 0)
	session, err := baseMgr.StartSession(1, "profile-1", 42, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	handler := NewStreamHandler(baseMgr, testPlaybackFileResolver{file: file})

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	handler.handleTransportStartFailure(canceledCtx, session, file, context.Canceled)
	if capture.has(slog.LevelWarn, "stream transport startup failed") {
		t.Fatal("client cancellation logged as a WARN transport failure")
	}
	if !capture.has(slog.LevelDebug, "stream transport canceled by client") {
		t.Fatal("client cancellation did not log the debug cancellation line")
	}

	handler.handleTransportStartFailure(context.Background(), session, file, errors.New("ffmpeg unavailable"))
	if !capture.has(slog.LevelWarn, "stream transport startup failed") {
		t.Fatal("a genuine transport error did not log as WARN")
	}
}
