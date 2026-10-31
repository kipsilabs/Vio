package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/playback"
)

func replanLogSeekFixture(t *testing.T) (*PlaybackHandler, *playback.SessionManager, playback.StartRequestV3, playback.DecisionResponseV3) {
	t.Helper()
	file := v3HandlerFixtureFile(t)
	manager := playback.NewSessionManager(0, 0)
	handler := NewPlaybackHandler(manager, testPlaybackFileResolver{file: file})
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"allow_4k_transcode": "false"}}
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	start := v3HandlerStartRequest()
	rr := httptest.NewRecorder()
	handler.HandleStartPlayback(rr, httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", strings.NewReader(marshalV3StartRequest(t, start))).WithContext(newAuthorizedPlaybackContext()))
	var started playback.DecisionResponseV3
	if rr.Code != http.StatusCreated || json.Unmarshal(rr.Body.Bytes(), &started) != nil || started.PlaybackPlan == nil {
		t.Fatalf("start status = %d, body = %s", rr.Code, rr.Body.String())
	}
	return handler, manager, start, started
}

func replanLogSeekRequest(start playback.StartRequestV3, plan *playback.PlanV3, requestID string, position float64) playback.ReplanRequestV3 {
	return playback.ReplanRequestV3{
		ProtocolVersion:       playback.ProtocolV3,
		Operation:             playback.ReplanOperationSeekReanchorV3,
		PlaybackAttemptID:     start.PlaybackAttemptID,
		ReplanRequestID:       requestID,
		FailedPlanID:          plan.PlanID,
		PlanAttemptID:         requestID + "-attempt",
		PlanAttemptKey:        plan.PlanAttemptKey,
		AttemptCount:          1,
		QualityPreference:     start.QualityPreference,
		PositionSeconds:       position,
		SelectedTracks:        plan.SelectedTracks,
		Capabilities:          start.Capabilities,
		ClientPlaybackContext: start.ClientPlaybackContext,
	}
}

// TestReplanSeekReanchorToCurrentPositionAnswersNoMovement pins the explicit
// no-change answer for a seek that resolves to the timeline the plan and the
// live session already serve. The plan stays byte-identical; only the outcome
// changes, so a player can stop re-anchoring the same position.
func TestReplanSeekReanchorToCurrentPositionAnswersNoMovement(t *testing.T) {
	handler, manager, start, started := replanLogSeekFixture(t)
	position := started.PlaybackPlan.Timeline.SourceStartSeconds
	if err := manager.UpdateProgress(started.SessionID, position, false); err != nil {
		t.Fatal(err)
	}
	before := *started.PlaybackPlan

	response := postPlaybackReplanV3(t, handler, started.SessionID,
		replanLogSeekRequest(start, started.PlaybackPlan, "seek-no-movement-0001", position))

	if response.Outcome != playback.OutcomeSeekNoMovementV3 {
		t.Fatalf("outcome = %q, want %q (terminal=%+v)", response.Outcome, playback.OutcomeSeekNoMovementV3, response.Terminal)
	}
	if response.Terminal != nil {
		t.Fatalf("no-movement seek answered a terminal: %+v", response.Terminal)
	}
	if response.PlaybackPlan == nil {
		t.Fatal("no-movement seek returned no plan")
	}
	if !reflect.DeepEqual(*response.PlaybackPlan, before) {
		t.Fatalf("no-movement seek rewrote the plan:\nbefore=%#v\nafter=%#v", before, *response.PlaybackPlan)
	}
}

// TestReplanSeekReanchorToNewPositionStillMoves keeps the fast path honest: a
// seek to a different position still re-anchors the timeline and answers with
// the ordinary playable outcome.
func TestReplanSeekReanchorToNewPositionStillMoves(t *testing.T) {
	handler, _, start, started := replanLogSeekFixture(t)
	target := started.PlaybackPlan.Timeline.SourceStartSeconds + 120

	response := postPlaybackReplanV3(t, handler, started.SessionID,
		replanLogSeekRequest(start, started.PlaybackPlan, "seek-moved-0001", target))

	if response.Outcome != playback.OutcomePlayableV3 {
		t.Fatalf("outcome = %q, want %q (terminal=%+v)", response.Outcome, playback.OutcomePlayableV3, response.Terminal)
	}
	if response.PlaybackPlan == nil || response.PlaybackPlan.Timeline.SourceStartSeconds != target {
		t.Fatalf("real seek did not move the timeline to %v: %#v", target, response.PlaybackPlan)
	}
}

// TestReplanIdempotentReplayLogsAnsweredUnchanged covers the production gap
// where a replayed replan answered 200 without any line: the decision log stays
// reserved for a route the server actually chose, and the replay gets one
// concise answer line naming the operation, outcome, reason, and duration.
func TestReplanIdempotentReplayLogsAnsweredUnchanged(t *testing.T) {
	handler, _, start, started := replanLogSeekFixture(t)
	request := replanLogSeekRequest(start, started.PlaybackPlan, "seek-replay-log-0001", started.PlaybackPlan.Timeline.SourceStartSeconds+120)
	first := postPlaybackReplanV3(t, handler, started.SessionID, request)
	if first.PlaybackPlan == nil {
		t.Fatalf("fixture precondition: first replan returned no plan (terminal=%+v)", first.Terminal)
	}

	logs := captureHandlerLogs(t)
	replay := postPlaybackReplanV3(t, handler, started.SessionID, request)
	if !reflect.DeepEqual(replay, first) {
		t.Fatalf("idempotent replay changed the decision:\nfirst=%#v\nreplay=%#v", first, replay)
	}

	logged := logs.String()
	if !strings.Contains(logged, `"msg":"playback replan answered unchanged"`) {
		t.Fatalf("replay answer line missing from logs:\n%s", logged)
	}
	if strings.Contains(logged, `"msg":"playback replan decided"`) {
		t.Fatalf("replay also emitted a decision line:\n%s", logged)
	}
	for _, want := range []string{
		`"operation":"seek_reanchor"`,
		`"outcome":"plan"`,
		`"reason":"idempotent_replay"`,
		`"decision_reason":"`,
		`"requested_file_id":42`,
		`"effective_file_id":42`,
		`"duration_ms":`,
	} {
		if !strings.Contains(logged, want) {
			t.Fatalf("replay answer log missing %s:\n%s", want, logged)
		}
	}
}

// TestReplanNoMovementSeekLogsAnsweredUnchanged pins that the explicit
// no-movement answer is accounted for by the unchanged line, not the decision
// line: the server did not choose a new route.
func TestReplanNoMovementSeekLogsAnsweredUnchanged(t *testing.T) {
	handler, manager, start, started := replanLogSeekFixture(t)
	position := started.PlaybackPlan.Timeline.SourceStartSeconds
	if err := manager.UpdateProgress(started.SessionID, position, false); err != nil {
		t.Fatal(err)
	}

	logs := captureHandlerLogs(t)
	postPlaybackReplanV3(t, handler, started.SessionID,
		replanLogSeekRequest(start, started.PlaybackPlan, "seek-no-movement-log-0001", position))

	logged := logs.String()
	if !strings.Contains(logged, `"msg":"playback replan answered unchanged"`) {
		t.Fatalf("no-movement answer line missing from logs:\n%s", logged)
	}
	if strings.Contains(logged, `"msg":"playback replan decided"`) {
		t.Fatalf("no-movement seek also emitted a decision line:\n%s", logged)
	}
	for _, want := range []string{
		`"operation":"seek_reanchor"`,
		`"outcome":"no_change"`,
		`"reason":"seek_no_movement"`,
	} {
		if !strings.Contains(logged, want) {
			t.Fatalf("no-movement answer log missing %s:\n%s", want, logged)
		}
	}
}
