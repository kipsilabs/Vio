package apiv2

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/api/handlers"
	"github.com/Silo-Server/silo-server/internal/playback"
)

func playbackReplanFixture(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile("../playback/testdata/protocol_v3/start_request.json")
	if err != nil {
		t.Fatal(err)
	}
	var start map[string]any
	if err := json.Unmarshal(data, &start); err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"installation_id":         playbackTestInstallation,
		"protocol_version":        3,
		"operation":               "seek_reanchor",
		"playback_attempt_id":     "attempt-0123456789",
		"replan_request_id":       "replan-0123456789",
		"failed_plan_id":          "plan-0123456789",
		"plan_attempt_id":         "plan-attempt-0123",
		"plan_attempt_key":        "v3:0123456789abcdef",
		"attempted_plan_keys":     []string{},
		"attempt_count":           1,
		"quality_preference":      "auto",
		"position_seconds":        120.5,
		"metered":                 false,
		"selected_tracks":         map[string]any{},
		"client_capabilities":     start["client_capabilities"],
		"client_playback_context": start["client_playback_context"],
	}
}

func TestPlaybackV2ReplanUsesTypedServiceAndDigest(t *testing.T) {
	deps, _ := catalogDeps(t)
	fake := &fakePlaybackService{}
	deps.Playback = fake
	data, err := os.ReadFile("../playback/testdata/protocol_v3/decision_response.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fake.response); err != nil {
		t.Fatal(err)
	}
	h := newTestHandler(t, deps)
	body := playbackReplanFixture(t)
	first := do(t, h, http.MethodPost, Prefix+"/playback/11111111-1111-4111-8111-111111111111/replan", playbackJSON(t, body), viewerHeaders())
	if first.Code != 200 {
		t.Fatalf("replan: %d %s", first.Code, first.Body.String())
	}
	if fake.calls != 1 || fake.session != "11111111-1111-4111-8111-111111111111" || fake.caller.InstallationID != playbackTestInstallation || fake.replan.Request.ReplanRequestID != "replan-0123456789" || fake.replan.Request.Operation != playback.ReplanOperationSeekReanchorV3 || fake.replan.Request.PositionSeconds != 120.5 || fake.replan.Digest == "" {
		t.Fatalf("service call: %+v %+v", fake.caller, fake.replan)
	}
	if !strings.Contains(first.Body.String(), `"requested_media_file_id":"`) || !strings.Contains(first.Body.String(), `"stream":`) {
		t.Fatalf("opaque IDs: %s", first.Body.String())
	}
	digest := fake.replan.Digest
	// The digest fingerprints the accepted body: identical input replays the
	// same digest, a changed position changes it, so a reused request id with
	// different input is detectable by the store.
	do(t, h, http.MethodPost, Prefix+"/playback/11111111-1111-4111-8111-111111111111/replan", playbackJSON(t, body), viewerHeaders())
	if fake.replan.Digest != digest {
		t.Fatal("identical replan produced a different digest")
	}
	body["position_seconds"] = 121
	do(t, h, http.MethodPost, Prefix+"/playback/11111111-1111-4111-8111-111111111111/replan", playbackJSON(t, body), viewerHeaders())
	if fake.replan.Digest == digest {
		t.Fatal("changed replan kept the same digest")
	}
	for name, mutate := range map[string]func(map[string]any){
		"protocol":         func(b map[string]any) { b["protocol_version"] = 2 },
		"short request id": func(b map[string]any) { b["replan_request_id"] = "x" },
		"unknown field":    func(b map[string]any) { b["unexpected"] = true },
		"bad operation":    func(b map[string]any) { b["operation"] = "teleport" },
		"negative pos":     func(b map[string]any) { b["position_seconds"] = -1 },
		"installation":     func(b map[string]any) { b["installation_id"] = "not-a-uuid" },
	} {
		t.Run(name, func(t *testing.T) {
			calls := fake.calls
			body := playbackReplanFixture(t)
			mutate(body)
			rec := do(t, h, http.MethodPost, Prefix+"/playback/11111111-1111-4111-8111-111111111111/replan", playbackJSON(t, body), viewerHeaders())
			if rec.Code != 422 || fake.calls != calls {
				t.Fatalf("validation: %d %s calls=%d", rec.Code, rec.Body.String(), fake.calls-calls)
			}
		})
	}
}

// TestPlaybackV2ReplanCarriesPlanInvalidationAnswer pins the correlation the
// default-audio withdrawal depends on. A client that negotiated
// default_audio_reconcile_response_v1 answers a withdrawal by naming its reason
// on the replan; if v2 dropped the field the answer would be lost, and since the
// schema forbids additional properties the request would be refused outright
// rather than merely ignored.
func TestPlaybackV2ReplanCarriesPlanInvalidationAnswer(t *testing.T) {
	const reason = "default_audio_reconciliation"
	deps, _ := catalogDeps(t)
	fake := &fakePlaybackService{}
	deps.Playback = fake
	data, err := os.ReadFile("../playback/testdata/protocol_v3/decision_response.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fake.response); err != nil {
		t.Fatal(err)
	}
	h := newTestHandler(t, deps)

	// The echoed reason survives strict validation and reaches the service.
	body := playbackReplanFixture(t)
	body["answers_plan_invalidation"] = reason
	rec := do(t, h, http.MethodPost, Prefix+"/playback/11111111-1111-4111-8111-111111111111/replan", playbackJSON(t, body), viewerHeaders())
	if rec.Code != 200 {
		t.Fatalf("replan answering a withdrawal: %d %s", rec.Code, rec.Body.String())
	}
	if fake.replan.Request.AnswersPlanInvalidation != reason {
		t.Fatalf("answers_plan_invalidation = %q, want %q", fake.replan.Request.AnswersPlanInvalidation, reason)
	}

	// The field is optional: an ordinary intent replan omits it entirely and is
	// still accepted, so a client that never sees a withdrawal is unaffected.
	plain := playbackReplanFixture(t)
	rec = do(t, h, http.MethodPost, Prefix+"/playback/11111111-1111-4111-8111-111111111111/replan", playbackJSON(t, plain), viewerHeaders())
	if rec.Code != 200 {
		t.Fatalf("replan without the answer: %d %s", rec.Code, rec.Body.String())
	}
	if fake.replan.Request.AnswersPlanInvalidation != "" {
		t.Fatalf("answers_plan_invalidation = %q, want it absent", fake.replan.Request.AnswersPlanInvalidation)
	}

	// The answer is part of the body the digest fingerprints, so a retry that
	// claims a different answer is detectable rather than silently replayed.
	answered := playbackReplanFixture(t)
	answered["answers_plan_invalidation"] = reason
	do(t, h, http.MethodPost, Prefix+"/playback/11111111-1111-4111-8111-111111111111/replan", playbackJSON(t, answered), viewerHeaders())
	answeredDigest := fake.replan.Digest
	answered["answers_plan_invalidation"] = "some_other_reason"
	do(t, h, http.MethodPost, Prefix+"/playback/11111111-1111-4111-8111-111111111111/replan", playbackJSON(t, answered), viewerHeaders())
	if fake.replan.Digest == answeredDigest {
		t.Fatal("a changed answers_plan_invalidation kept the same digest")
	}
}

func TestPlaybackV2ReplanValidationRejectsBadAnswers(t *testing.T) {
	deps, _ := catalogDeps(t)
	fake := &fakePlaybackService{}
	deps.Playback = fake
	data, err := os.ReadFile("../playback/testdata/protocol_v3/decision_response.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fake.response); err != nil {
		t.Fatal(err)
	}
	h := newTestHandler(t, deps)
	body := playbackReplanFixture(t)
	body["answers_plan_invalidation"] = string(make([]byte, 65))
	rec := do(t, h, http.MethodPost, Prefix+"/playback/11111111-1111-4111-8111-111111111111/replan", playbackJSON(t, body), viewerHeaders())
	if rec.Code != 422 {
		t.Fatalf("an over-long answer must be refused: %d %s", rec.Code, rec.Body.String())
	}
}

// TestPlaybackV2ReplanMapsOperationErrors covers the replan error mapping that
// used to live at the end of TestPlaybackV2ReplanUsesTypedServiceAndDigest; it is
// its own test so the digest and validation assertions above stay readable.
func TestPlaybackV2ReplanMapsOperationErrors(t *testing.T) {
	deps, _ := catalogDeps(t)
	fake := &fakePlaybackService{}
	deps.Playback = fake
	data, err := os.ReadFile("../playback/testdata/protocol_v3/decision_response.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fake.response); err != nil {
		t.Fatal(err)
	}
	h := newTestHandler(t, deps)
	for _, tc := range []struct {
		err  *handlers.PlaybackOperationError
		want ProblemType
	}{
		{&handlers.PlaybackOperationError{Status: 404, Code: "session_not_found", Message: "no"}, TypeNotFound},
		{&handlers.PlaybackOperationError{Status: 409, Code: "stale_playback_plan", Message: "no"}, TypeConflict},
		{&handlers.PlaybackOperationError{Status: 409, Code: "idempotency_key_reused", Message: "no"}, TypeConflict},
		{&handlers.PlaybackOperationError{Status: 503, Code: "unavailable", Message: "no"}, TypeDependencyUnavailable},
	} {
		fake.err = tc.err
		requireProblem(t, do(t, h, http.MethodPost, Prefix+"/playback/11111111-1111-4111-8111-111111111111/replan", playbackJSON(t, playbackReplanFixture(t)), viewerHeaders()), tc.want)
	}
	fake.err = nil
	requireProblem(t, do(t, h, http.MethodPost, Prefix+"/playback/11111111-1111-4111-8111-111111111111/replan", playbackJSON(t, playbackReplanFixture(t)), nil), TypeAuthenticationRequired)
	requireProblem(t, do(t, h, http.MethodPost, Prefix+"/playback/11111111-1111-4111-8111-111111111111/replan", playbackJSON(t, playbackReplanFixture(t)), bearer(memberToken)), TypeValidationFailed)
	deps.Playback = nil
	requireProblem(t, do(t, newTestHandler(t, deps), http.MethodPost, Prefix+"/playback/11111111-1111-4111-8111-111111111111/replan", playbackJSON(t, playbackReplanFixture(t)), viewerHeaders()), TypeCapabilityNotConfigured)
}

func playbackReplanFixtureCases() []fixtureCase {
	var start map[string]any
	fixturePlaybackRead("start_request.json", &start)
	replan := map[string]any{
		"installation_id": playbackTestInstallation, "protocol_version": 3, "operation": "seek_reanchor",
		"playback_attempt_id": "attempt-0123456789", "replan_request_id": "replan-0123456789", "failed_plan_id": "plan-0123456789",
		"plan_attempt_id": "plan-attempt-0123", "plan_attempt_key": "v3:0123456789abcdef", "attempted_plan_keys": []string{}, "attempt_count": 1,
		"quality_preference": "auto", "position_seconds": 120.5, "metered": false, "selected_tracks": map[string]any{},
		"client_capabilities": start["client_capabilities"], "client_playback_context": start["client_playback_context"],
	}
	session := "11111111-1111-4111-8111-111111111111"
	return []fixtureCase{
		{name: "playback_replan_reanchored", operationID: "replanPlayback", method: http.MethodPost, path: Prefix + "/playback/" + session + "/replan", body: fixturePlaybackJSON(replan), schema: "#/components/schemas/PlaybackDecision", scenario: "A seek re-anchor answers a whole replacement plan with opaque identifiers; track, quality and output changes use the same operation.", status: 200, headers: viewerHeaders(), assertHeaders: []string{"Content-Type", "Cache-Control"}},
	}
}
