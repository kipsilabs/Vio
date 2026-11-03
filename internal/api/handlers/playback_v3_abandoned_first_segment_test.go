package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/playback"
)

// waitForAbandonedBeforeFirstSegment waits on the observable session state the
// guard reads instead of racing the fake FFmpeg's playlist write with a sleep.
func waitForAbandonedBeforeFirstSegment(t *testing.T, handler *PlaybackHandler, sessionID string) *playback.TranscodeSession {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		live := handler.tm.GetTranscodeSession(sessionID)
		if live != nil && live.AbandonedBeforeFirstSegment() {
			return live
		}
		if time.Now().After(deadline) {
			t.Fatal("the live generation never reached the abandoned-before-first-segment state")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// fetchPlaybackManifestV3 serves the session's playlist through the same handler
// a client hits, which is what makes the session's manifest-served evidence (the
// precondition for the abandoned verdict) observable.
func fetchPlaybackManifestV3(t *testing.T, handler *PlaybackHandler, sessionID, streamURL string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1"+streamURL, nil).WithContext(newAuthorizedPlaybackContext())
	req = withPlaybackRouteParam(req, "session_id", sessionID)
	rr := httptest.NewRecorder()
	handler.HandleGetTranscodeManifest(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "#EXTM3U") {
		t.Fatalf("manifest status = %d body = %q, want a playlist", rr.Code, rr.Body.String())
	}
}

// postReplanV3Raw posts a replan without asserting the status, for the paths
// whose contract is a refusal rather than a planned route.
func postReplanV3Raw(t *testing.T, handler *PlaybackHandler, sessionID string, request playback.ReplanRequestV3) (int, playback.DecisionResponseV3) {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/"+sessionID+"/replan", strings.NewReader(string(body))).WithContext(newAuthorizedPlaybackContext())
	req = withPlaybackRouteParam(req, "session_id", sessionID)
	rr := httptest.NewRecorder()
	handler.HandleReplanPlaybackV3(rr, req)
	var response playback.DecisionResponseV3
	if rr.Body.Len() > 0 {
		_ = json.Unmarshal(rr.Body.Bytes(), &response)
	}
	return rr.Code, response
}

// TestFailureRecoveryWithoutSegmentRequestStopsInsteadOfIndicting proves a
// failure_recovery after the client fetched the playlist but never asked for a
// segment is answered as a gone session: the session stops, no plan is
// indicted, no delivery is demoted, and the failure is never persisted as a
// verdict against a route that delivered nothing.
func TestFailureRecoveryWithoutSegmentRequestStopsInsteadOfIndicting(t *testing.T) {
	f := newDecodeRotationFixture(t, decodeRotationOptions{candidateIDs: []string{"A", "B"}})
	start := f.request()

	code, started := f.start(t, start)
	if code != http.StatusCreated || started.PlaybackPlan == nil {
		t.Fatalf("start status=%d response=%+v", code, started)
	}
	defer f.handler.tm.CloseTranscodeSession(started.SessionID, "")
	fetchPlaybackManifestV3(t, f.handler, started.SessionID, started.PlaybackPlan.Stream.URL)
	waitForAbandonedBeforeFirstSegment(t, f.handler, started.SessionID)
	resolvesBefore := len(f.recordedExclusions())

	replan := timeoutRotationReplanRequest(start, started.PlaybackPlan, "abandoned-first-segment-0001")
	code, _ = postReplanV3Raw(t, f.handler, started.SessionID, replan)
	if code != http.StatusNotFound {
		t.Fatalf("abandoned recovery status = %d, want %d", code, http.StatusNotFound)
	}

	if _, err := f.handler.sessionMgr.GetSession(started.SessionID); err == nil {
		t.Fatal("abandoned session was left live")
	}
	if resolvesAfter := len(f.recordedExclusions()); resolvesAfter != resolvesBefore {
		t.Fatalf("abandoned recovery ran %d candidate resolutions, want 0", resolvesAfter-resolvesBefore)
	}
	record, err := f.handler.PlanStoreV3.GetAttempt(t.Context(), started.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if record.StoppedAt == nil {
		t.Fatal("abandoned attempt was not stopped")
	}
	caps := record.NormalizedRequest.ClientPlaybackContext.Deliveries[playback.DeliveryClassHLSV3]
	if !caps.Enabled || caps.FailureReason == demoteDeliveryReasonV3 {
		t.Fatalf("abandoned recovery demoted the HLS delivery: %+v", caps)
	}
}

// TestFailureRecoveryWithoutManifestRequestKeepsPlannedHandling proves the
// guard needs the playlist evidence: a client that reported a failure before
// the session ever served a playlist keeps the ordinary failure handling,
// because that failure happened inside the stream, not after a successful read.
func TestFailureRecoveryWithoutManifestRequestKeepsPlannedHandling(t *testing.T) {
	f := newDecodeRotationFixture(t, decodeRotationOptions{candidateIDs: []string{"A", "B"}})
	start := f.request()

	code, started := f.start(t, start)
	if code != http.StatusCreated || started.PlaybackPlan == nil {
		t.Fatalf("start status=%d response=%+v", code, started)
	}
	defer f.handler.tm.CloseTranscodeSession(started.SessionID, "")
	if live := f.handler.tm.GetTranscodeSession(started.SessionID); live == nil || live.AbandonedBeforeFirstSegment() {
		t.Fatal("fixture precondition: the client must not have read the playlist")
	}

	replan := timeoutRotationReplanRequest(start, started.PlaybackPlan, "no-manifest-recovery-0001")
	code, _ = postReplanV3Raw(t, f.handler, started.SessionID, replan)
	if code != http.StatusOK {
		t.Fatalf("pre-playlist recovery status = %d, want %d", code, http.StatusOK)
	}
	if _, err := f.handler.sessionMgr.GetSession(started.SessionID); err != nil {
		t.Fatalf("session was stopped without playlist evidence: %v", err)
	}
}

// TestFailureRecoveryAfterSegmentRequestKeepsPlannedHandling is the other half
// of the abandonment rule: once the client asked for a segment the recovery is
// ordinary failure handling, and the guard must not stop the session.
func TestFailureRecoveryAfterSegmentRequestKeepsPlannedHandling(t *testing.T) {
	f := newDecodeRotationFixture(t, decodeRotationOptions{candidateIDs: []string{"A", "B"}})
	start := f.request()

	code, started := f.start(t, start)
	if code != http.StatusCreated || started.PlaybackPlan == nil {
		t.Fatalf("start status=%d response=%+v", code, started)
	}
	defer f.handler.tm.CloseTranscodeSession(started.SessionID, "")
	fetchPlaybackManifestV3(t, f.handler, started.SessionID, started.PlaybackPlan.Stream.URL)
	waitForAbandonedBeforeFirstSegment(t, f.handler, started.SessionID)

	rec := httptest.NewRecorder()
	f.handler.HandleGetTranscodeSegment(rec, playbackTestRequest(
		http.MethodGet,
		"/api/v1/playback/transcode/"+started.SessionID+"/segment/seg_0.m4s",
		nil,
		map[string]string{"session_id": started.SessionID, "name": "seg_0.m4s"},
	))
	if rec.Code != http.StatusOK {
		t.Fatalf("segment status = %d body=%q, want 200", rec.Code, rec.Body.String())
	}

	replan := timeoutRotationReplanRequest(start, started.PlaybackPlan, "served-first-segment-0001")
	code, _ = postReplanV3Raw(t, f.handler, started.SessionID, replan)
	if code != http.StatusOK {
		t.Fatalf("served-session recovery status = %d, want %d", code, http.StatusOK)
	}
	if _, err := f.handler.sessionMgr.GetSession(started.SessionID); err != nil {
		t.Fatalf("served session was stopped by the abandoned guard: %v", err)
	}
}
