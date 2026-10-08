package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/playback"
)

// TestReplanDeliverySwapMarkerAndLog proves issue #244 item 3 end to end, and
// issue #245 item 5: the marker is exposed only on the native v2 surface. A
// direct-play session whose delivery fails and is demoted mid-session is
// replanned onto a different delivery. On v2 the plan carries an additive marker
// naming the old -> new route and the one decision line names the same; on the
// frozen v1 bridge the identical flow produces the swap with no marker. Before
// the marker the swap was indistinguishable; before the surface gate the frozen
// bridge would have grown a new field.
func TestReplanDeliverySwapMarkerAndLog(t *testing.T) {
	setup := func(t *testing.T) (*PlaybackHandler, playback.StartRequestV3) {
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
		// The fallback route: without an advertised HLS delivery the demotion of
		// direct play would leave no viable route and the recovery would terminal.
		start.ClientPlaybackContext.Deliveries[playback.DeliveryClassHLSV3] = playback.DeliveryCapabilityV3{
			Enabled: true, SupportedOnDevice: true,
			Containers:        []string{"hls"},
			VideoCodecs:       []string{"h264"},
			AudioDecodeCodecs: []string{"aac"},
			Subtitles:         playback.DeliverySubtitleCapabilitiesV3{EmbeddedText: true, SidecarText: true},
		}
		return handler, start
	}
	recoveryFor := func(start playback.StartRequestV3, plan *playback.PlanV3) playback.ReplanRequestV3 {
		return playback.ReplanRequestV3{
			ProtocolVersion: playback.ProtocolV3, ClientFeatures: start.ClientFeatures,
			Operation: playback.ReplanOperationFailureRecoveryV3, PlaybackAttemptID: start.PlaybackAttemptID,
			ReplanRequestID: "delivery-swap-recovery-0001", FailedPlanID: plan.PlanID, PlanAttemptID: "delivery-swap-attempt-0001",
			PlanAttemptKey: plan.PlanAttemptKey, AttemptedPlanKeys: []string{plan.PlanAttemptKey}, AttemptCount: 1,
			PositionSeconds: 10, SelectedTracks: plan.SelectedTracks,
			Failure:               playback.FailureV3{Classification: "decoder_failure"},
			Capabilities:          start.Capabilities,
			ClientPlaybackContext: start.ClientPlaybackContext,
		}
	}

	t.Run("v2 exposes the marker and logs old to new", func(t *testing.T) {
		logs := captureHandlerLogs(t)
		handler, start := setup(t)
		started := startV3PlaybackV2ForHandlerTest(t, handler, start)
		plan := started.PlaybackPlan
		if plan.Delivery != playback.DeliveryOriginalHTTPV3 {
			t.Fatalf("fixture expected a direct-play start, got %s (%s)", plan.Delivery, plan.DecisionReason)
		}
		if plan.DeliveryChange != nil {
			t.Fatalf("start plan unexpectedly carried a delivery change marker: %#v", plan.DeliveryChange)
		}

		recovered := postPlaybackReplanV2ForHandlerTest(t, handler, started.SessionID, recoveryFor(start, plan))
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
	})

	t.Run("v1 withholds the marker on the same swap", func(t *testing.T) {
		handler, start := setup(t)
		// The identical start and swap, driven through the frozen v1 bridge.
		rr := httptest.NewRecorder()
		handler.HandleStartPlayback(rr, httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", strings.NewReader(marshalV3StartRequest(t, start))).WithContext(newAuthorizedPlaybackContext()))
		var started playback.DecisionResponseV3
		if rr.Code != http.StatusCreated || json.Unmarshal(rr.Body.Bytes(), &started) != nil || started.PlaybackPlan == nil {
			t.Fatalf("start: %d %s", rr.Code, rr.Body.String())
		}
		plan := started.PlaybackPlan
		if plan.Delivery != playback.DeliveryOriginalHTTPV3 {
			t.Fatalf("fixture expected a direct-play start, got %s", plan.Delivery)
		}

		recovered := postPlaybackReplanV3(t, handler, started.SessionID, recoveryFor(start, plan))
		if recovered.Terminal != nil || recovered.PlaybackPlan == nil {
			t.Fatalf("v1 failure recovery did not plan a route: terminal=%#v", recovered.Terminal)
		}
		if playback.DeliveryClassV3(recovered.PlaybackPlan.Delivery) == playback.DeliveryClassOriginalHTTPV3 {
			t.Fatal("v1 fixture did not actually swap the route; the withhold precondition was not exercised")
		}
		if recovered.PlaybackPlan.DeliveryChange != nil {
			t.Fatalf("v1 replan leaked the v2-only delivery_change marker: %#v", recovered.PlaybackPlan.DeliveryChange)
		}
	})

	// The durable replan lease is shared by both surfaces: a v2 decision that
	// swapped the route is cached, and a replay of the identical canonical
	// request through v1 must not re-emit the v2-only marker from the cached
	// bytes. The replay is byte-identical to the v2 request (same body, same
	// replan_request_id), so the lease matches and execution is skipped; the
	// surface gate at the response boundary is the only thing that keeps the
	// frozen bridge from growing the field.
	t.Run("v1 replay of a cached v2 swap withholds the marker", func(t *testing.T) {
		handler, start := setup(t)
		started := startV3PlaybackV2ForHandlerTest(t, handler, start)
		plan := started.PlaybackPlan
		if plan.Delivery != playback.DeliveryOriginalHTTPV3 {
			t.Fatalf("fixture expected a direct-play start, got %s", plan.Delivery)
		}
		recovery := recoveryFor(start, plan)

		recovered := postPlaybackReplanV2ForHandlerTest(t, handler, started.SessionID, recovery)
		if recovered.Terminal != nil || recovered.PlaybackPlan == nil || recovered.PlaybackPlan.DeliveryChange == nil {
			t.Fatalf("fixture precondition: the v2 recovery must have swapped the route with a marker: terminal=%#v plan=%v", recovered.Terminal, recovered.PlaybackPlan)
		}

		// The identical canonical request through the frozen v1 bridge: the
		// lease replays the cached decision without re-executing.
		replayed := postPlaybackReplanV3(t, handler, started.SessionID, recovery)
		if replayed.Terminal != nil || replayed.PlaybackPlan == nil {
			t.Fatalf("v1 replay did not return the cached plan: terminal=%#v plan=%v", replayed.Terminal, replayed.PlaybackPlan)
		}
		if replayed.PlaybackPlan.PlanID != recovered.PlaybackPlan.PlanID {
			t.Fatalf("v1 replay plan = %q, want the cached v2 plan %q", replayed.PlaybackPlan.PlanID, recovered.PlaybackPlan.PlanID)
		}
		if replayed.PlaybackPlan.DeliveryChange != nil {
			t.Fatalf("v1 replay of a cached v2 swap leaked delivery_change: %#v", replayed.PlaybackPlan.DeliveryChange)
		}

		// The durable v2 decision still carries the marker: a v2 replay of the
		// same request returns the cached bytes unchanged.
		replayedV2 := postPlaybackReplanV2ForHandlerTest(t, handler, started.SessionID, recovery)
		if replayedV2.Terminal != nil || replayedV2.PlaybackPlan == nil || replayedV2.PlaybackPlan.DeliveryChange == nil {
			t.Fatalf("v2 replay lost the marker: terminal=%#v plan=%v", replayedV2.Terminal, replayedV2.PlaybackPlan)
		}
	})
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

// TestSeekReanchorAfterDeliverySwapOmitsMarker proves issue #245 item 3: a
// failure recovery that swaps A -> B is followed by a seek reanchor on B. A seek
// replays the durable route verbatim by copying record.CurrentPlan wholesale, so
// the copied plan already carries the recovery's marker; the handler must clear
// it (assign nil) rather than re-emit the stale A -> B swap on a plan that
// changed nothing. Both consecutive seeks must omit delivery_change.
func TestSeekReanchorAfterDeliverySwapOmitsMarker(t *testing.T) {
	file := v3HandlerFixtureFile(t)
	manager := playback.NewSessionManager(0, 0)
	handler := NewPlaybackHandler(manager, testPlaybackFileResolver{file: file})
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
	started := startV3PlaybackV2ForHandlerTest(t, handler, start)
	plan := started.PlaybackPlan
	if plan.Delivery != playback.DeliveryOriginalHTTPV3 {
		t.Fatalf("fixture expected a direct-play start, got %s", plan.Delivery)
	}
	if err := manager.UpdateProgress(started.SessionID, 12, true); err != nil {
		t.Fatal(err)
	}

	recoveryReq := playback.ReplanRequestV3{
		ProtocolVersion: playback.ProtocolV3, ClientFeatures: start.ClientFeatures,
		Operation: playback.ReplanOperationFailureRecoveryV3, PlaybackAttemptID: start.PlaybackAttemptID,
		ReplanRequestID: "seek-after-swap-recovery-0001", FailedPlanID: plan.PlanID, PlanAttemptID: "seek-after-swap-attempt-0001",
		PlanAttemptKey: plan.PlanAttemptKey, AttemptedPlanKeys: []string{plan.PlanAttemptKey}, AttemptCount: 1,
		PositionSeconds: 10, SelectedTracks: plan.SelectedTracks,
		Failure:               playback.FailureV3{Classification: "decoder_failure"},
		Capabilities:          start.Capabilities,
		ClientPlaybackContext: start.ClientPlaybackContext,
	}
	recovered := postPlaybackReplanV2ForHandlerTest(t, handler, started.SessionID, recoveryReq)
	if recovered.Terminal != nil || recovered.PlaybackPlan == nil {
		t.Fatalf("failure recovery did not plan a route: terminal=%#v", recovered.Terminal)
	}
	recoveredPlan := recovered.PlaybackPlan
	if recoveredPlan.DeliveryChange == nil {
		t.Fatal("fixture precondition: the recovery replan must have swapped the route")
	}
	if !isHLSDeliveryV3(recoveredPlan.Delivery) {
		t.Fatalf("fixture precondition: the swapped route must be an HLS delivery for a seek reanchor, got %s", recoveredPlan.Delivery)
	}

	seek := playback.ReplanRequestV3{
		ProtocolVersion: playback.ProtocolV3, ClientFeatures: start.ClientFeatures,
		Operation: playback.ReplanOperationSeekReanchorV3, PlaybackAttemptID: start.PlaybackAttemptID,
		ReplanRequestID: "seek-after-swap-0001", FailedPlanID: recoveredPlan.PlanID, PlanAttemptID: "seek-after-swap-attempt-0002",
		PlanAttemptKey: recoveredPlan.PlanAttemptKey, AttemptedPlanKeys: []string{recoveredPlan.PlanAttemptKey}, AttemptCount: 1,
		QualityPreference: "original", PositionSeconds: 30,
		SelectedTracks:        recoveredPlan.SelectedTracks,
		Failure:               playback.FailureV3{},
		Capabilities:          start.Capabilities,
		ClientPlaybackContext: start.ClientPlaybackContext,
	}
	for i, id := range []string{"seek-after-swap-0001", "seek-after-swap-0002"} {
		seek.ReplanRequestID = id
		response := postPlaybackReplanV2ForHandlerTest(t, handler, started.SessionID, seek)
		if response.Terminal != nil || response.PlaybackPlan == nil {
			t.Fatalf("seek %d returned terminal=%#v plan=%v", i, response.Terminal, response.PlaybackPlan)
		}
		if response.PlaybackPlan.DeliveryChange != nil {
			t.Fatalf("seek %d re-emitted a stale delivery_change: %#v", i, response.PlaybackPlan.DeliveryChange)
		}
	}
}
