package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/playback"
)

// A retryable subtitle answer keeps its 503 + Retry-After shape and adds, not
// changes, what it tells the client: the covered range and, when the extraction
// knows one, a more specific reason.
func TestWriteSubtitleRetryKeepsTheRetryableShape(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set(playback.SubtitleCoverageHeader, "600.000-720.000")
	rec.Header().Set(playback.SubtitleWindowedHeader, "true")
	writeSubtitleRetry(rec, &playback.SubtitleExtractionRetryError{RetryAfter: 1500 * time.Millisecond})

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "2" {
		t.Fatalf("Retry-After = %q, want the delay rounded up to 2", got)
	}
	// The requested window is not what was produced, so it must not stay on the
	// response as if it were.
	if got := rec.Header().Get(playback.SubtitleCoverageHeader); got != "" {
		t.Fatalf("coverage = %q, want none: the page was not produced", got)
	}
	if got := rec.Header().Get(playback.SubtitleWindowedHeader); got != "" {
		t.Fatalf("windowed = %q, want none", got)
	}
	if !strings.Contains(rec.Body.String(), `"error":"subtitle_source_unavailable"`) {
		t.Fatalf("body = %q, want the existing problem code", rec.Body.String())
	}
}

// A page refused for its read cost reports its own code, and a page the
// progressive artifact has partly parsed advertises that range.
func TestWriteSubtitleRetryReportsCostAndCoverage(t *testing.T) {
	rec := httptest.NewRecorder()
	writeSubtitleRetry(rec, &playback.SubtitleExtractionRetryError{
		RetryAfter:   2 * time.Second,
		Reason:       playback.SubtitlePageTooCostlyReason,
		CoveredUntil: 200,
	})

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if got := rec.Header().Get(playback.SubtitleCoverageHeader); got != "0.000-200.000" {
		t.Fatalf("coverage = %q, want the parsed range 0.000-200.000", got)
	}
	if got := rec.Header().Get(playback.SubtitleWindowedHeader); got != "true" {
		t.Fatalf("windowed = %q, want true with a coverage range", got)
	}
	if !strings.Contains(rec.Body.String(), `"error":"`+playback.SubtitlePageTooCostlyReason+`"`) {
		t.Fatalf("body = %q, want the page-cost problem code", rec.Body.String())
	}
}
