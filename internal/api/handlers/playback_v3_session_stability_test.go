package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// sessionStabilityHandlerV3 builds a handler whose h264 fixture can start on
// direct play and fall back to HLS, the route shape the delivery-demotion and
// recovery tests use.
func sessionStabilityHandlerV3(t *testing.T) (*PlaybackHandler, playback.StartRequestV3, *models.MediaFile) {
	t.Helper()
	file := v3HandlerFixtureFile(t)
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0), testPlaybackFileResolver{file: file})
	stubCopySeekAnchorV3(handler)
	handler.PlaybackConfig = playbackTestConfig(writePlaybackTestFFmpeg(t), t.TempDir())
	presetLocalRegistryV3(handler, playback.NewTransformationRegistryV3([]playback.TransformationSpecV3{
		{Name: "audio_to_aac", RecipeVersion: "2", Available: true},
		{Name: "video_to_h264", RecipeVersion: "2", Available: true},
	}))
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"allow_4k_transcode": "true"}}

	start := v3HandlerStartRequest()
	start.ClientPlaybackContext.Deliveries[playback.DeliveryClassOriginalHTTPV3] = playback.DeliveryCapabilityV3{
		Enabled: true, SupportedOnDevice: true, VideoCodecs: []string{"h264"},
	}
	start.ClientPlaybackContext.Deliveries[playback.DeliveryClassHLSV3] = playback.DeliveryCapabilityV3{
		Enabled: true, SupportedOnDevice: true,
		Containers:        []string{"hls"},
		VideoCodecs:       []string{"h264"},
		AudioDecodeCodecs: []string{"aac"},
		Subtitles:         playback.DeliverySubtitleCapabilitiesV3{EmbeddedText: true, SidecarText: true},
	}
	return handler, start, file
}

func sessionStabilityStartV3(t *testing.T, handler *PlaybackHandler, start playback.StartRequestV3) playback.DecisionResponseV3 {
	t.Helper()
	rr := httptest.NewRecorder()
	handler.HandleStartPlayback(rr, httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", strings.NewReader(marshalV3StartRequest(t, start))).WithContext(newAuthorizedPlaybackContext()))
	var started playback.DecisionResponseV3
	if rr.Code != http.StatusCreated || json.Unmarshal(rr.Body.Bytes(), &started) != nil || started.PlaybackPlan == nil {
		t.Fatalf("start: %d %s", rr.Code, rr.Body.String())
	}
	return started
}

func sessionStabilityRecoveryV3(start playback.StartRequestV3, plan *playback.PlanV3, requestID string) playback.ReplanRequestV3 {
	return playback.ReplanRequestV3{
		ProtocolVersion: playback.ProtocolV3, ClientFeatures: start.ClientFeatures,
		Operation: playback.ReplanOperationFailureRecoveryV3, PlaybackAttemptID: start.PlaybackAttemptID,
		ReplanRequestID: requestID, FailedPlanID: plan.PlanID, PlanAttemptID: requestID + "-plan",
		PlanAttemptKey: plan.PlanAttemptKey, AttemptedPlanKeys: []string{plan.PlanAttemptKey}, AttemptCount: 1,
		PositionSeconds: 10, SelectedTracks: plan.SelectedTracks,
		Failure:               playback.FailureV3{Classification: "decoder_failure"},
		Capabilities:          start.Capabilities,
		ClientPlaybackContext: start.ClientPlaybackContext,
	}
}

func postReplanRawV3(t *testing.T, handler *PlaybackHandler, sessionID string, request playback.ReplanRequestV3) (int, playback.DecisionResponseV3) {
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
	_ = json.Unmarshal(rr.Body.Bytes(), &response)
	return rr.Code, response
}

// A live client that canceled one transport must still be able to recover: the
// cancel is evidence about that request, not a verdict on the session. The
// recovery runs and returns a plan instead of answering the healthy session
// with the missing-session 404 that would retire it.
func TestReplanFailureRecoveryAfterTransportCancelRuns(t *testing.T) {
	handler, start, _ := sessionStabilityHandlerV3(t)
	started := sessionStabilityStartV3(t, handler, start)
	plan := started.PlaybackPlan
	if plan.Delivery != playback.DeliveryOriginalHTTPV3 {
		t.Fatalf("fixture expected direct play, got %s", plan.Delivery)
	}

	// The transport observed the client cancel (a transient abort/hls.js retry).
	if err := handler.sessionMgr.MarkClientCanceled(started.SessionID); err != nil {
		t.Fatalf("mark canceled: %v", err)
	}
	status, response := postReplanRawV3(t, handler, started.SessionID, sessionStabilityRecoveryV3(start, plan, "live-cancel-recovery-0001"))
	if status != http.StatusOK {
		t.Fatalf("live cancel recovery status = %d, want 200 (body terminal=%#v)", status, response.Terminal)
	}
	if response.Terminal != nil || response.PlaybackPlan == nil {
		t.Fatalf("recovery after a transport cancel did not run: terminal=%#v", response.Terminal)
	}
	// The attempt is still live: nothing was stopped or terminalized.
	record, err := handler.PlanStoreV3.GetAttempt(t.Context(), started.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if record.StoppedAt != nil {
		t.Fatalf("recovery after a cancel stopped the attempt: %v", record.StoppedAt)
	}
}

// A predecessor route's cancel must not outlive its route. Once a successor
// commits, the mark is stale: the recovery runs, and the stale evidence is
// dropped so it cannot color a later read of the successor.
func TestReplanFailureRecoveryIgnoresLatePredecessorCancel(t *testing.T) {
	handler, start, _ := sessionStabilityHandlerV3(t)
	started := sessionStabilityStartV3(t, handler, start)
	plan := started.PlaybackPlan
	if plan.Delivery != playback.DeliveryOriginalHTTPV3 {
		t.Fatalf("fixture expected direct play, got %s", plan.Delivery)
	}

	// The cancel is recorded on the current route, then a successor commits a
	// new revision (the session's own route advanced past the canceled request).
	if err := handler.sessionMgr.MarkClientCanceled(started.SessionID); err != nil {
		t.Fatalf("mark canceled: %v", err)
	}
	if err := handler.sessionMgr.UpdateStreamState(started.SessionID, playback.SessionStreamState{
		PlayMethod: playback.PlayDirect, BasePlayMethod: playback.PlayDirect,
		TranscodeRouteSet: true, SubtitleTrackIndex: -1,
	}); err != nil {
		t.Fatalf("commit successor revision: %v", err)
	}
	session, err := handler.sessionMgr.GetSession(started.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if session.ClientCancelNamesCurrentRoute() {
		t.Fatal("a cancel for the superseded route still names the current route")
	}

	status, response := postReplanRawV3(t, handler, started.SessionID, sessionStabilityRecoveryV3(start, plan, "successor-recovery-0002"))
	if status != http.StatusOK {
		t.Fatalf("successor recovery status = %d, want 200 (body terminal=%#v)", status, response.Terminal)
	}
	if response.Terminal != nil || response.PlaybackPlan == nil {
		t.Fatalf("successor recovery did not run: terminal=%#v", response.Terminal)
	}
	// The predecessor's mark is gone, so it cannot gate or mislabel a later
	// recovery on the successor.
	session, err = handler.sessionMgr.GetSession(started.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !session.ClientCanceled.At.IsZero() {
		t.Fatalf("stale predecessor cancel survived the successor recovery: %#v", session.ClientCanceled)
	}
}

// Only a genuine stop fences recovery. A canceled transport does not; a stopped
// session still answers the missing-session verdict the client retires on.
func TestReplanFailureRecoveryAfterStopFences(t *testing.T) {
	handler, start, _ := sessionStabilityHandlerV3(t)
	started := sessionStabilityStartV3(t, handler, start)
	plan := started.PlaybackPlan
	if plan.Delivery != playback.DeliveryOriginalHTTPV3 {
		t.Fatalf("fixture expected direct play, got %s", plan.Delivery)
	}

	if err := handler.sessionMgr.StopSession(started.SessionID); err != nil {
		t.Fatalf("stop session: %v", err)
	}
	status, response := postReplanRawV3(t, handler, started.SessionID, sessionStabilityRecoveryV3(start, plan, "stopped-recovery-0003"))
	if status != http.StatusNotFound {
		t.Fatalf("stopped-session recovery status = %d, want 404 (body terminal=%#v)", status, response.Terminal)
	}
	if response.Terminal != nil {
		t.Fatalf("stopped-session recovery returned a terminal: %#v", response.Terminal)
	}
}

// The zombie window is bounded: a cancel older than the window is stale
// liveness evidence, so a genuinely live client that reconnects is never
// blocked.
func TestSessionClientCanceledRecentlyV3Window(t *testing.T) {
	now := time.Now()
	if !sessionClientCanceledRecentlyV3(now.Add(-5*time.Second), now) {
		t.Fatal("a recent cancel was not recognized")
	}
	if sessionClientCanceledRecentlyV3(now.Add(-clientCanceledRecoveryWindow-time.Second), now) {
		t.Fatal("a cancel older than the window was treated as recent")
	}
	if sessionClientCanceledRecentlyV3(time.Time{}, now) {
		t.Fatal("a zero cancel time was treated as recent")
	}
}

// Delivery stickiness is a user-intent backstop, not a failure-recovery policy:
// a failure recovery must stay free to move off the failed route.
func TestReplanStickyDeliveryV3Policy(t *testing.T) {
	if got := replanStickyDeliveryV3(playback.ReplanOperationFailureRecoveryV3, playback.DeliveryOriginalHTTPV3); got != "" {
		t.Fatalf("failure recovery pinned delivery %q, want none", got)
	}
	if got := replanStickyDeliveryV3(playback.ReplanOperationSeekFailureRecoveryV3, playback.DeliveryTranscodeHLSV3); got != "" {
		t.Fatalf("seek failure recovery pinned delivery %q, want none", got)
	}
	for _, op := range []playback.ReplanOperationV3{
		playback.ReplanOperationTrackChangeV3,
		playback.ReplanOperationQualityChangeV3,
		playback.ReplanOperationOutputChangeV3,
		playback.ReplanOperationSeekReanchorV3,
	} {
		if got := replanStickyDeliveryV3(op, playback.DeliveryTranscodeHLSV3); got != playback.DeliveryTranscodeHLSV3 {
			t.Fatalf("operation %q pinned %q, want the running delivery", op, got)
		}
	}
}
