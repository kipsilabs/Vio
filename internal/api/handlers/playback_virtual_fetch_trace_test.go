package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary/resolver"
)

// newTraceCountingProvider is a minimal Stremio provider for the handler-side
// fetch-observation tests: it answers a valid manifest and counts stream
// endpoint requests so a test can prove provider-call counts, not just
// wall-clock. It lives here rather than importing the resolver test helper,
// which is package-private.
func newTraceCountingProvider(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/manifest.json") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"org.stremio.trace","resources":["stream"],"types":["movie"]}`)
			return
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"streams": []map[string]any{{"url": "https://cdn.example/a.mkv", "name": "Trace.Release.1080p"}},
		})
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

// observeVirtualProviderFetches must surface the resolver's real provider
// discovery to the startup trace: one call counted for a cold resolve, and
// zero for a repeat resolve served from the resolver cache.
func TestObserveVirtualProviderFetchesCountsDiscoveryThenReuse(t *testing.T) {
	server, calls := newTraceCountingProvider(t)
	r := resolver.New(resolver.Config{ManifestURL: server.URL + "/manifest.json", AllowInsecure: true})
	ctx, snapshot := observeVirtualProviderFetches(context.Background())

	if _, _, _, err := r.GetCandidates(ctx, "virtual://movie/tt-trace-fetch"); err != nil {
		t.Fatalf("first GetCandidates: %v", err)
	}
	duration, callsSeen := snapshot()
	if callsSeen != 1 {
		t.Fatalf("observed fetch calls = %d, want 1", callsSeen)
	}
	if duration <= 0 {
		t.Fatalf("observed fetch duration = %v, want a positive measurement", duration)
	}

	if _, _, _, err := r.GetCandidates(ctx, "virtual://movie/tt-trace-fetch"); err != nil {
		t.Fatalf("second GetCandidates: %v", err)
	}
	durationAfter, callsAfter := snapshot()
	if callsAfter != 1 {
		t.Fatalf("observed fetch calls after reuse = %d, want 1 (no redundant discovery)", callsAfter)
	}
	if durationAfter != duration {
		t.Fatalf("observed duration changed on a cache hit: %v -> %v", duration, durationAfter)
	}
	if *calls != 1 {
		t.Fatalf("provider requests = %d, want 1", *calls)
	}
}

// A stage whose observed discovery exceeds its measured wall time is floored at
// zero, so a late observer can never emit a negative stage duration.
func TestVirtualStageNetElapsedFloorsAtZero(t *testing.T) {
	if got := virtualStageNetElapsed(time.Now().Add(-5*time.Millisecond), 10*time.Millisecond); got != 0 {
		t.Fatalf("net elapsed = %v, want 0 when discovery exceeds the stage", got)
	}
	start := time.Now().Add(-20 * time.Millisecond)
	got := virtualStageNetElapsed(start, 5*time.Millisecond)
	if got <= 0 || got > 20*time.Millisecond {
		t.Fatalf("net elapsed = %v, want the wall time minus discovery", got)
	}
}
