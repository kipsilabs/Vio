package handlers

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

	"github.com/Silo-Server/silo-server/internal/models"
	chimw "github.com/go-chi/chi/v5/middleware"
)

// virtualTraceFieldsToMap converts the stable []any key/value shape returned by
// fields() into a map for assertions.
func virtualTraceFieldsToMap(t *testing.T, fields []any) map[string]any {
	t.Helper()
	if len(fields)%2 != 0 {
		t.Fatalf("fields have odd length %d", len(fields))
	}
	out := make(map[string]any, len(fields)/2)
	for i := 0; i < len(fields); i += 2 {
		key, ok := fields[i].(string)
		if !ok {
			t.Fatalf("field key at %d is not a string: %#v", i, fields[i])
		}
		out[key] = fields[i+1]
	}
	return out
}

// A stage that ran in under a millisecond must keep its <name>_ms field (value
// 0) with <name>_ran=true, while a stage that did not run must omit its
// duration entirely and report <name>_ran=false. total_ms is the exact sum of
// the displayed stage durations.
func TestVirtualResolveTraceRanVersusZero(t *testing.T) {
	trace := &virtualResolveTrace{
		started: time.Now(),
		list:    3 * time.Millisecond,
		listRan: true,
		resolve: time.Millisecond,
		// Ran, but below the millisecond resolution the field reports.
		probe:      500 * time.Microsecond,
		probeRan:   true,
		resolveRan: true,
	}
	if got := trace.totalMS(); got != 4 {
		t.Fatalf("totalMS = %d, want 4", got)
	}

	fields := virtualTraceFieldsToMap(t, trace.fields())
	for key, want := range map[string]any{
		"list_ran":    true,
		"list_ms":     int64(3),
		"resolve_ran": true,
		"resolve_ms":  int64(1),
		"probe_ran":   true,
		"probe_ms":    int64(0), // ran in under 1 ms, still a measurement
		"total_ms":    int64(4),
	} {
		if got := fields[key]; got != want {
			t.Fatalf("%s = %#v, want %#v", key, got, want)
		}
	}
	if fields["fast_path"] != false {
		t.Fatalf("fast_path = %#v, want false", fields["fast_path"])
	}
	// fallback did not run: no duration field at all, not even a zero.
	if got := fields["fallback_ran"]; got != false {
		t.Fatalf("fallback_ran = %#v, want false", got)
	}
	if _, present := fields["fallback_ms"]; present {
		t.Fatal("fallback_ms present for a stage that did not run")
	}
}

// captureVirtualResolveTiming runs fn with the default slog logger redirected
// to a buffer and returns the first JSON entry whose msg is the timing line.
// Tests using it must not call t.Parallel: it swaps the process-wide logger.
func captureVirtualResolveTiming(t *testing.T, fn func()) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
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
		if entry["msg"] == "virtual resolve timing" {
			return entry
		}
	}
	t.Fatalf("no virtual resolve timing line captured; log was:\n%s", buf.String())
	return nil
}

// A real cold resolve that lists and resolves must report those stages as ran
// with a non-zero list duration, and total_ms must equal the sum of the
// displayed stage durations.
func TestResolveVirtualTimingReportsRanStages(t *testing.T) {
	uri := "virtual://movie/tt-trace-cold"
	file := &models.MediaFile{
		ID:                         990,
		ContentID:                  "movie-trace-cold",
		FilePath:                   uri,
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
	}
	detailedCalls, legacyCalls := 0, 0
	h := virtualRepeatPlayHandler(&detailedCalls, &legacyCalls)
	h.VirtualPlaybackStreamLister = VirtualPlaybackStreamListerFunc(
		func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			// Guarantee a measurable list stage, not a sub-millisecond one.
			time.Sleep(2 * time.Millisecond)
			return []VirtualPlaybackStream{{
				URI: uri + "?result=cand-1", Resolution: "1080p",
				CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
			}}, nil
		})

	entry := captureVirtualResolveTiming(t, func() {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
		// The trace must carry the start's own edge id so it joins to its
		// start timing and its provider fetch without timestamp forensics.
		req = req.WithContext(context.WithValue(req.Context(), chimw.RequestIDKey, "trace-start-req-1"))
		if _, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false); err != nil {
			t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
		}
	})

	if entry["request_id"] != "trace-start-req-1" {
		t.Fatalf("request_id = %#v, want the start request's edge id", entry["request_id"])
	}

	for _, stage := range []string{"list", "resolve"} {
		if entry[stage+"_ran"] != true {
			t.Fatalf("%s_ran = %#v, want true", stage, entry[stage+"_ran"])
		}
	}
	// The start path defers the probe; it must not be reported as ran.
	if entry["probe_ran"] != false {
		t.Fatalf("probe_ran = %#v, want false on a deferred-probe start", entry["probe_ran"])
	}
	if entry["fast_path"] != false {
		t.Fatalf("fast_path = %#v, want false", entry["fast_path"])
	}
	if entry["listed"] != true {
		t.Fatalf("listed = %#v, want true", entry["listed"])
	}
	listMS, ok := entry["list_ms"].(float64)
	if !ok || listMS < 1 {
		t.Fatalf("list_ms = %#v, want a non-zero measured duration", entry["list_ms"])
	}
	if _, present := entry["probe_ms"]; present {
		t.Fatal("probe_ms present for a stage that did not run")
	}

	var sum float64
	for _, stage := range []string{"list_ms", "resolve_ms", "probe_ms", "fallback_ms"} {
		if value, ok := entry[stage].(float64); ok {
			sum += value
		}
	}
	total, ok := entry["total_ms"].(float64)
	if !ok {
		t.Fatalf("total_ms = %#v, want a number", entry["total_ms"])
	}
	if sum != total {
		t.Fatalf("stage sum = %v, total_ms = %v: total must equal the sum of the stages that ran", sum, total)
	}
}

// The repeat-play fast path does no provider work. It must say so explicitly
// (fast_path=true, every stage *_ran=false, no *_ms field, total_ms=0) instead
// of emitting an all-zero line that could be misread as a measurement.
func TestResolveVirtualTimingFlagsFastPathWithoutRanStages(t *testing.T) {
	detailedCalls, legacyCalls := 0, 0
	h := virtualRepeatPlayHandler(&detailedCalls, &legacyCalls)
	file := virtualRepeatPlayFile("virtual://movie/tt-trace-fast?result=cand-1")

	entry := captureVirtualResolveTiming(t, func() {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
		if _, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false); err != nil {
			t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
		}
	})

	if entry["fast_path"] != true {
		t.Fatalf("fast_path = %#v, want true", entry["fast_path"])
	}
	if total, _ := entry["total_ms"].(float64); total != 0 {
		t.Fatalf("total_ms = %#v, want 0", entry["total_ms"])
	}
	for _, stage := range []string{"list", "resolve", "probe", "fallback"} {
		if entry[stage+"_ran"] != false {
			t.Fatalf("%s_ran = %#v, want false on the fast path", stage, entry[stage+"_ran"])
		}
		if _, present := entry[stage+"_ms"]; present {
			t.Fatalf("%s_ms present for a stage that did not run", stage)
		}
	}
}

// A resolve that outlives its budget must trip the verdict: the trace
// reports budget_exceeded=true with the budget it ran under, so an overrun
// names its stages instead of silently passing. The 15s listing backstop is
// deliberately larger than the test budget, so the listing here burns past
// the 120ms cold deadline while still inside its own cap: elapsed must exceed
// the budget even though no stage was canceled mid-flight.
func TestResolveVirtualTimingFlagsBudgetExceededOnStalledListing(t *testing.T) {
	previousBudget := virtualStartupBudget
	virtualStartupBudget = 120 * time.Millisecond
	t.Cleanup(func() { virtualStartupBudget = previousBudget })

	uri := "virtual://movie/tt-trace-budget"
	file := &models.MediaFile{
		ID:                         991,
		ContentID:                  "movie-trace-budget",
		FilePath:                   uri,
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
	}
	detailedCalls, legacyCalls := 0, 0
	h := virtualRepeatPlayHandler(&detailedCalls, &legacyCalls)
	h.VirtualPlaybackStreamLister = VirtualPlaybackStreamListerFunc(
		func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			// Burns past the cold budget while ignoring cancellation: the
			// resolve still completes (slowly), so the verdict measures the
			// overrun instead of the cancellation.
			time.Sleep(300 * time.Millisecond)
			return []VirtualPlaybackStream{{
				URI: uri + "?result=cand-1", Resolution: "1080p",
				CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
			}}, nil
		})

	entry := captureVirtualResolveTiming(t, func() {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
		_, _ = h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false)
	})

	if entry["budget_exceeded"] != true {
		t.Fatalf("budget_exceeded = %#v, want true for a resolve that outlived its budget", entry["budget_exceeded"])
	}
	if entry["budget_ms"] != float64(120) {
		t.Fatalf("budget_ms = %#v, want 120", entry["budget_ms"])
	}
	if entry["list_ran"] != true {
		t.Fatalf("list_ran = %#v, want true (the slow stage)", entry["list_ran"])
	}
	if elapsed, _ := entry["elapsed_ms"].(float64); elapsed < 120 {
		t.Fatalf("elapsed_ms = %#v, want >= 120 for the overrun", entry["elapsed_ms"])
	}
}

// A healthy cold start stays inside the budget: budget_exceeded=false and the
// elapsed time fits the budget it ran under.
func TestResolveVirtualTimingBudgetHoldsOnHealthyStart(t *testing.T) {
	uri := "virtual://movie/tt-trace-budget-ok"
	file := &models.MediaFile{
		ID:                         992,
		ContentID:                  "movie-trace-budget-ok",
		FilePath:                   uri,
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
	}
	detailedCalls, legacyCalls := 0, 0
	h := virtualRepeatPlayHandler(&detailedCalls, &legacyCalls)
	h.VirtualPlaybackStreamLister = VirtualPlaybackStreamListerFunc(
		func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{{
				URI: uri + "?result=cand-1", Resolution: "1080p",
				CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
			}}, nil
		})

	entry := captureVirtualResolveTiming(t, func() {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
		if _, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false); err != nil {
			t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
		}
	})

	if entry["budget_exceeded"] != false {
		t.Fatalf("budget_exceeded = %#v, want false for a healthy start", entry["budget_exceeded"])
	}
	if _, present := entry["budget_ms"]; !present {
		t.Fatal("budget_ms missing: every trace must name the budget it ran under")
	}
}
