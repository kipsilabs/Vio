package opslog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"testing"
)

// TestHandlerSnapshotsValuesTheCallerOwns logs a map and a slice, then changes
// both the way a caller may once the log call returns. The consumer encodes
// the entry later on its own goroutine, so the entry must not share them.
func TestHandlerSnapshotsValuesTheCallerOwns(t *testing.T) {
	t.Parallel()
	writer := &recordingWriter{}
	logger := slog.New(NewHandler(slog.DiscardHandler, writer, slog.LevelInfo, "node-a"))

	counts := map[string]int{"movies": 1}
	ids := []int{1, 2}
	logger.With("static", counts).InfoContext(context.Background(), "probe: snapshot",
		"counts", counts, "ids", ids, "error", errors.New("boom"), "status", 200)
	counts["movies"] = 2
	counts["shows"] = 3
	ids[0] = 9

	writer.mu.Lock()
	entry := writer.entries[0]
	writer.mu.Unlock()
	got, err := json.Marshal(entry.Attrs)
	if err != nil {
		t.Fatal(err)
	}
	// encoding/json sorts map keys, so the encoding is stable.
	want := `{"counts":{"movies":1},"error":"boom","ids":[1,2],"static":{"movies":1},"status":200}`
	if string(got) != want {
		t.Fatalf("attrs = %s, want %s", got, want)
	}
}

// TestHandlerKeepsAValueItCannotEncode checks that a value encoding/json
// rejects is kept as text instead of emptying the entry's attrs.
func TestHandlerKeepsAValueItCannotEncode(t *testing.T) {
	t.Parallel()
	writer := &recordingWriter{}
	logger := slog.New(NewHandler(slog.DiscardHandler, writer, slog.LevelInfo, "node-a"))

	logger.InfoContext(context.Background(), "probe: unencodable", "ch", make(chan int), "status", 200)

	writer.mu.Lock()
	entry := writer.entries[0]
	writer.mu.Unlock()
	if s, ok := entry.Attrs["ch"].(string); !ok || s == "" {
		t.Fatalf("attrs[ch] = %#v, want its text form", entry.Attrs["ch"])
	}
	if entry.Attrs["status"] != int64(200) {
		t.Fatalf("attrs[status] = %#v, want 200", entry.Attrs["status"])
	}
}

// TestHandlerErrorAttrStaysDiagnosticAndSanitized proves the two properties
// the playback fix depends on: an error attribute persists its message text
// (not {}) through the handler, and a URL-bearing error is sanitized by the
// central sink so no call site can persist a provider URL by logging its
// error raw.
func TestHandlerErrorAttrStaysDiagnosticAndSanitized(t *testing.T) {
	t.Parallel()
	writer := &recordingWriter{}
	logger := slog.New(NewHandler(slog.DiscardHandler, writer, slog.LevelInfo, "node-a"))

	providerErr := &url.Error{
		Op:  "Get",
		URL: "https://operator:node-password@node.example/stream?access_token=query-secret",
		Err: errors.New("connection refused"),
	}
	logger.InfoContext(context.Background(), "probe: transport",
		"error", providerErr,
		"wrapped", fmt.Errorf("resolve virtual input: %w", providerErr),
		"plain", errors.New("boom"),
	)

	writer.mu.Lock()
	entry := writer.entries[0]
	writer.mu.Unlock()
	for key, secret := range map[string]string{
		"error":   "node-password",
		"wrapped": "query-secret",
	} {
		if s, _ := entry.Attrs[key].(string); !strings.Contains(s, "connection refused") {
			t.Fatalf("attrs[%s] = %#v, want the cause text", key, entry.Attrs[key])
		}
		if s, _ := entry.Attrs[key].(string); strings.Contains(s, secret) {
			t.Fatalf("attrs[%s] leaked a credential: %#v", key, entry.Attrs[key])
		}
	}
	if entry.Attrs["plain"] != "boom" {
		t.Fatalf("attrs[plain] = %#v, want boom", entry.Attrs["plain"])
	}
	got, err := json.Marshal(entry.Attrs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "{}") {
		t.Fatalf("attrs encoded an empty error object: %s", got)
	}
}

// TestHandlerErrorTextBounded proves a long error chain (a multi-error join
// of provider messages) cannot bloat the attrs column: the persisted text is
// capped and the truncation records the original length.
func TestHandlerErrorTextBounded(t *testing.T) {
	t.Parallel()
	writer := &recordingWriter{}
	logger := slog.New(NewHandler(slog.DiscardHandler, writer, slog.LevelInfo, "node-a"))

	chain := errors.New("provider timeout")
	for i := 0; i < 50; i++ {
		chain = fmt.Errorf("layer %d wrapping a long provider message: %w", i, chain)
	}
	logger.InfoContext(context.Background(), "probe: long chain", "error", chain)

	writer.mu.Lock()
	entry := writer.entries[0]
	writer.mu.Unlock()
	text, _ := entry.Attrs["error"].(string)
	if len(text) > maxPersistedErrorText+64 {
		t.Fatalf("persisted error text = %d bytes, want bounded near %d", len(text), maxPersistedErrorText)
	}
	if !strings.Contains(text, "truncated") {
		t.Fatalf("persisted error text is not marked truncated: %q", text[:64])
	}
	if !strings.Contains(text, "layer 49") {
		t.Fatalf("truncation dropped the actionable prefix: %q", text[:64])
	}
}
