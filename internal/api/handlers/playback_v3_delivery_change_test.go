package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/playback"
)

// TestReplanDeliverySwapMarkerAndLog proves issue #244 item 3 end to end. A
// direct-play session whose delivery fails and is demoted mid-session is
// replanned onto a different delivery. The client sees the new plan only, so
// the replan must both carry an additive marker naming the old -> new route on
// the plan and emit the same decision on its one log line. Before the marker
// the swap was indistinguishable from a planned route change.
func TestReplanDeliverySwapMarkerAndLog(t *testing.T) {
	logs := captureHandlerLogs(t)
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
		Enabled: true, SupportedOnDevice: true,
		VideoCodecs: []string{"h264"},
	}
	// The fallback route: without an advertised HLS delivery the demotion of
	// direct play would leave no viable route and the recovery would terminal.
	start.ClientPlaybackContext.Deliveries[playback.DeliveryClassHLSV3] = playback.DeliveryCapabilityV3{
		Enabled: true, SupportedOnDevice: true,
		Containers:        []string{"hls"},
		VideoCodecs:       []string{"h264"},
		AudioDecodeCodecs: []string{"aac"},
		Subtitles:         playback.DeliverySubtitleCapabilitiesV3{EmbeddedText: true, SidecarText: true},
	}
	rr := httptest.NewRecorder()
	handler.HandleStartPlayback(rr, httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", strings.NewReader(marshalV3StartRequest(t, start))).WithContext(newAuthorizedPlaybackContext()))
	var started playback.DecisionResponseV3
	if rr.Code != http.StatusCreated || json.Unmarshal(rr.Body.Bytes(), &started) != nil || started.PlaybackPlan == nil {
		t.Fatalf("start: %d %s", rr.Code, rr.Body.String())
	}
	plan := started.PlaybackPlan
	if plan.Delivery != playback.DeliveryOriginalHTTPV3 {
		t.Fatalf("fixture expected a direct-play start, got %s (%s)", plan.Delivery, plan.DecisionReason)
	}
	// The start plan itself is not a mid-session replan, so it carries no swap
	// marker.
	if plan.DeliveryChange != nil {
		t.Fatalf("start plan unexpectedly carried a delivery change marker: %#v", plan.DeliveryChange)
	}

	recoveryReq := playback.ReplanRequestV3{
		ProtocolVersion: playback.ProtocolV3, ClientFeatures: start.ClientFeatures,
		Operation: playback.ReplanOperationFailureRecoveryV3, PlaybackAttemptID: start.PlaybackAttemptID,
		ReplanRequestID: "delivery-swap-recovery-0001", FailedPlanID: plan.PlanID, PlanAttemptID: "delivery-swap-attempt-0001",
		PlanAttemptKey: plan.PlanAttemptKey, AttemptedPlanKeys: []string{plan.PlanAttemptKey}, AttemptCount: 1,
		PositionSeconds: 10, SelectedTracks: plan.SelectedTracks,
		Failure:               playback.FailureV3{Classification: "decoder_failure"},
		Capabilities:          start.Capabilities,
		ClientPlaybackContext: start.ClientPlaybackContext,
	}
	recovered := postPlaybackReplanV3(t, handler, started.SessionID, recoveryReq)
	if recovered.Terminal != nil || recovered.PlaybackPlan == nil {
		t.Fatalf("failure recovery did not plan a route: terminal=%#v", recovered.Terminal)
	}
	recoveredPlan := recovered.PlaybackPlan
	if playback.DeliveryClassV3(recoveredPlan.Delivery) == playback.DeliveryClassOriginalHTTPV3 {
		t.Fatalf("failure recovery returned the delivery it just abandoned: %s", recoveredPlan.Delivery)
	}

	change := recoveredPlan.DeliveryChange
	if change == nil {
		t.Fatal("mid-session delivery swap carried no DeliveryChange marker")
	}
	if change.PreviousDelivery != playback.DeliveryOriginalHTTPV3 {
		t.Fatalf("marker previous delivery = %q, want %q", change.PreviousDelivery, playback.DeliveryOriginalHTTPV3)
	}
	if change.Delivery != recoveredPlan.Delivery {
		t.Fatalf("marker delivery = %q, want the served delivery %q", change.Delivery, recoveredPlan.Delivery)
	}
	if !change.DeliveryChanged {
		t.Fatalf("marker did not flag the delivery change: %#v", change)
	}
	if change.PreviousPlayMethod != playback.PlayDirect {
		t.Fatalf("marker previous play method = %q, want %q", change.PreviousPlayMethod, playback.PlayDirect)
	}

	// The one decision line for this replan names the same old -> new route.
	logged := logs.String()
	if !strings.Contains(logged, `"msg":"playback replan decided"`) {
		t.Fatalf("replan decision line missing from logs:\n%s", logged)
	}
	for _, want := range []string{
		`"previous_delivery":"` + string(playback.DeliveryOriginalHTTPV3) + `"`,
		`"new_delivery":"` + string(recoveredPlan.Delivery) + `"`,
		`"previous_play_method":"` + string(playback.PlayDirect) + `"`,
		`"delivery_changed":true`,
	} {
		if !strings.Contains(logged, want) {
			t.Fatalf("replan decision log missing %s:\n%s", want, logged)
		}
	}
}

// TestDeliveryChangeV3NoMarkerForUnchangedRoute pins the marker's nil contract:
// an unchanged delivery and play method (or an unknown previous method) emits no
// marker, so an ordinary replan that replays the same route never tells the
// client a swap happened.
func TestDeliveryChangeV3NoMarkerForUnchangedRoute(t *testing.T) {
	if change := deliveryChangeV3(playback.DeliveryTranscodeHLSV3, playback.PlayTranscode, playback.DeliveryTranscodeHLSV3, playback.PlayTranscode); change != nil {
		t.Fatalf("unchanged route produced a marker: %#v", change)
	}
	// A reconstructed session may have no cached method; an unchanged delivery
	// must not be reported as a play-method swap on an unknown baseline.
	if change := deliveryChangeV3(playback.DeliveryTranscodeHLSV3, "", playback.DeliveryTranscodeHLSV3, ""); change != nil {
		t.Fatalf("unknown previous method produced a marker: %#v", change)
	}
	// A real delivery swap still reports, even without a cached method.
	change := deliveryChangeV3(playback.DeliveryTranscodeHLSV3, "", playback.DeliveryRemuxProgressiveV3, "")
	if change == nil || !change.DeliveryChanged || change.PlayMethodChanged {
		t.Fatalf("delivery-only swap marker = %#v, want delivery_changed only", change)
	}
}
