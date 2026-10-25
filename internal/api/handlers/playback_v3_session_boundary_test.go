package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

const (
	sessionBoundaryForeignSession = "99999999-9999-4999-8999-999999999999"
	sessionBoundaryForeignPlan    = "plan-foreign-session"
)

// assertForeignSessionDecisionRefused fails when a cached decision carrying
// another session's plan was served instead of the retryable session_expired
// terminal. A playable plan here is the client-visible defect: the player would
// adopt it and then refuse the stream as a replacement belonging to another
// session.
func assertForeignSessionDecisionRefused(t *testing.T, response playback.DecisionResponseV3) {
	t.Helper()
	if response.PlaybackPlan != nil {
		t.Fatalf("cached decision replayed a foreign plan: %#v", response.PlaybackPlan)
	}
	if response.SessionID == sessionBoundaryForeignSession {
		t.Fatalf("cached decision replayed the foreign session id %q", response.SessionID)
	}
	if response.Terminal == nil || response.Terminal.Reason != "session_expired" || !response.Terminal.Retryable {
		t.Fatalf("foreign-session replay = %#v, want a retryable session_expired terminal", response)
	}
}

// The concurrent-duplicate start branch (another request won the SaveAttempt
// CAS) replays the winner's durable decision without passing through the
// ordinary idempotency lookup. It must apply the same session-ownership guard:
// a winner whose stored decision names another session must be refused, not
// handed to the losing request as its plan.
func TestStartPlaybackV3ConcurrentReplayRejectsForeignPlanSession(t *testing.T) {
	file := v3HandlerFixtureFile(t)
	manager := playback.NewSessionManager(0, 0)
	handler := NewPlaybackHandler(manager, testPlaybackFileResolver{file: file})
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{}}
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	startRequest := v3HandlerStartRequest()
	body := marshalV3StartRequest(t, startRequest)

	start := func() *httptest.ResponseRecorder {
		t.Helper()
		rr := httptest.NewRecorder()
		handler.HandleStartPlayback(rr, httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", strings.NewReader(body)).WithContext(newAuthorizedPlaybackContext()))
		return rr
	}

	winner := start()
	if winner.Code != http.StatusCreated {
		t.Fatalf("winner start status = %d, body = %s", winner.Code, winner.Body.String())
	}
	var started playback.DecisionResponseV3
	if err := json.Unmarshal(winner.Body.Bytes(), &started); err != nil || started.PlaybackPlan == nil {
		t.Fatalf("winner start response: err=%v response=%#v", err, started)
	}
	record, err := handler.PlanStoreV3.GetAttempt(context.Background(), started.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	original := *record
	// Deep-copy the stored decision's plan: the corruption below mutates the
	// plan the record points at.
	originalResponse := record.StartResponse
	originalPlan := *record.StartResponse.PlaybackPlan
	originalResponse.PlaybackPlan = &originalPlan
	original.StartResponse = originalResponse
	memoryStore := handler.PlanStoreV3.(*playback.MemoryPlanStoreV3)
	// A mixed-version or partially applied writer left the durable decision
	// describing a different session than the attempt owns.
	record.StartResponse.SessionID = sessionBoundaryForeignSession
	record.StartResponse.PlaybackPlan.SessionID = sessionBoundaryForeignSession
	memoryStore.ReplaceAttempt(context.Background(), *record)
	// Drive the collision branch deterministically: the pre-save idempotency
	// lookup misses and SaveAttempt then reports another request won.
	handler.PlanStoreV3 = &forceConflictOnSavePlanStoreV3{PlanStoreV3: handler.PlanStoreV3}

	loser := start()
	if loser.Code != http.StatusCreated {
		t.Fatalf("collision replay status = %d, body = %s", loser.Code, loser.Body.String())
	}
	var refused playback.DecisionResponseV3
	if err := json.Unmarshal(loser.Body.Bytes(), &refused); err != nil {
		t.Fatal(err)
	}
	assertForeignSessionDecisionRefused(t, refused)
	// The losing request rolled its own session back; only the winner's session
	// may remain live.
	if sessions := manager.AllSessions(); len(sessions) != 1 || sessions[0].ID != started.SessionID {
		t.Fatalf("sessions after refused collision = %#v, want only the winner's %q", sessions, started.SessionID)
	}

	// The same branch must still replay an owned decision idempotently.
	memoryStore.ReplaceAttempt(context.Background(), original)
	replay := start()
	if replay.Code != http.StatusCreated {
		t.Fatalf("valid collision replay status = %d, body = %s", replay.Code, replay.Body.String())
	}
	var replayed playback.DecisionResponseV3
	if err := json.Unmarshal(replay.Body.Bytes(), &replayed); err != nil {
		t.Fatal(err)
	}
	if replayed.PlaybackPlan == nil || replayed.SessionID != started.SessionID || replayed.PlaybackPlan.PlanID != started.PlaybackPlan.PlanID {
		t.Fatalf("valid collision replay = %#v, want the winner's plan %q in session %q", replayed, started.PlaybackPlan.PlanID, started.SessionID)
	}

	// A stored terminal publishes no plan, so it replays unchanged rather than
	// being turned into the foreign-session refusal.
	terminalRecord := original
	terminalRecord.StartResponse = playback.NewTerminalResponseV3("source_unavailable", "The provider listed no streams for this title.", true)
	memoryStore.ReplaceAttempt(context.Background(), terminalRecord)
	terminal := start()
	if terminal.Code != http.StatusCreated {
		t.Fatalf("terminal collision replay status = %d, body = %s", terminal.Code, terminal.Body.String())
	}
	var terminalResponse playback.DecisionResponseV3
	if err := json.Unmarshal(terminal.Body.Bytes(), &terminalResponse); err != nil {
		t.Fatal(err)
	}
	if terminalResponse.Terminal == nil || terminalResponse.Terminal.Reason != "source_unavailable" {
		t.Fatalf("terminal collision replay = %#v, want the stored terminal replay", terminalResponse)
	}
}

// The terminal-save collision branch (a losing terminal start re-reads the
// concurrent winner) also replays the durable playable decision directly. It
// must refuse a winner whose decision belongs to another session rather than
// resurrect that plan on the losing request.
func TestStartPlaybackV3TerminalSaveCollisionRejectsForeignPlanSession(t *testing.T) {
	const sessionID = "11111111-1111-1111-1111-111111111111"
	startRequest := v3HandlerStartRequest()
	record := playback.AttemptRecordV3{
		PlaybackAttemptID:    startRequest.PlaybackAttemptID,
		SessionID:            sessionID,
		UserID:               1,
		ProfileID:            "profile-1",
		RequestedMediaFileID: 42,
		EffectiveMediaFileID: 42,
		ExpiresAt:            time.Now().Add(time.Hour),
		RequestDigest:        "digest-terminal-collision",
		NormalizedRequest:    playback.StartRequestV3{ClientFeatures: startRequest.ClientFeatures},
		CurrentPlan:          playback.PlanV3{PlanID: sessionBoundaryForeignPlan, SessionID: sessionBoundaryForeignSession},
		StartResponse: playback.DecisionResponseV3{
			ProtocolVersion: playback.ProtocolV3,
			Outcome:         playback.OutcomePlayableV3,
			SessionID:       sessionBoundaryForeignSession,
			ServerFeatures:  playback.ServerFeaturesV3(),
			PlaybackPlan:    &playback.PlanV3{PlanID: sessionBoundaryForeignPlan, SessionID: sessionBoundaryForeignSession},
		},
	}
	handler := seedTerminalSaveCollisionStore(t, record)
	terminal := playback.NewTerminalResponseV3("source_unavailable", "The provider listed no streams for this title.", true)
	response, err := handler.persistTerminalStartDecisionV3(
		newAuthorizedPlaybackContext(), 1, "profile-1", startRequest,
		playbackStartRequestDigestsV3{current: "digest-terminal-collision"}, 42, 42, terminal,
	)
	if err != nil {
		t.Fatalf("terminal-save collision returned an error: %v", err)
	}
	assertForeignSessionDecisionRefused(t, response)
}

// A completed replan lease is a cache of the decision the attempt committed.
// A cached decision that names another session must never be replayed: the
// client would receive a replacement plan for a session it does not own. The
// handler answers the retryable stale_playback_plan instead, which the client
// recovers from by starting fresh.
func TestHandleReplanPlaybackV3CompletedLeaseForeignSessionNotReplayed(t *testing.T) {
	file := v3HandlerFixtureFile(t)
	file.AudioTracks = append(file.AudioTracks, models.AudioTrack{Codec: "aac", Channels: 2, Layout: "stereo", Language: "spa"})
	manager := playback.NewSessionManager(0, 0)
	handler := NewPlaybackHandler(manager, testPlaybackFileResolver{file: file})
	handler.JWTSecret = "test-secret"
	stubCopySeekAnchorV3(handler)
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"allow_4k_transcode": "true"}}
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	startRequest := v3HandlerStartRequest()
	startRR := httptest.NewRecorder()
	handler.HandleStartPlayback(startRR, httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", strings.NewReader(marshalV3StartRequest(t, startRequest))).WithContext(newAuthorizedPlaybackContext()))
	if startRR.Code != http.StatusCreated {
		t.Fatalf("start status = %d, body = %s", startRR.Code, startRR.Body.String())
	}
	var started playback.DecisionResponseV3
	if err := json.Unmarshal(startRR.Body.Bytes(), &started); err != nil || started.PlaybackPlan == nil {
		t.Fatalf("start response: err=%v response=%#v", err, started)
	}
	store := handler.PlanStoreV3.(*playback.MemoryPlanStoreV3)
	// A completed lease written for this session, but with the replacement
	// decision (top-level and plan) naming a different one.
	audioIndex := 1
	failedKey := playback.PlanAttemptKeyV3(*started.PlaybackPlan, startRequest.ClientPlaybackContext.Output.OutputContextID, nil)
	replan := playback.ReplanRequestV3{
		ProtocolVersion:       playback.ProtocolV3,
		PlaybackAttemptID:     startRequest.PlaybackAttemptID,
		ReplanRequestID:       "foreign-session-replan-0001",
		FailedPlanID:          started.PlaybackPlan.PlanID,
		PlanAttemptID:         "plan-attempt-foreign-0001",
		PlanAttemptKey:        failedKey,
		AttemptedPlanKeys:     []string{failedKey},
		AttemptCount:          1,
		PositionSeconds:       12,
		SelectedTracks:        playback.SelectedTracksV3{Audio: &playback.TrackIdentityV3{ID: playback.TrackIDV3(file.ID, "audio", audioIndex), Index: &audioIndex}},
		Failure:               playback.FailureV3{Classification: "audio_renderer_error"},
		Capabilities:          startRequest.Capabilities,
		ClientPlaybackContext: startRequest.ClientPlaybackContext,
	}
	body, err := json.Marshal(replan)
	if err != nil {
		t.Fatal(err)
	}
	digestBytes := sha256.Sum256(body)
	record, err := store.GetAttempt(context.Background(), started.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.BeginReplan(context.Background(), started.SessionID, replan.ReplanRequestID, hex.EncodeToString(digestBytes[:]), record.CurrentReplanRequestID, time.Now().Add(time.Minute))
	if err != nil || lease.State != playback.ReplanLeaseOwnedV3 {
		t.Fatalf("seed replan lease = %#v, err=%v", lease, err)
	}
	foreignPlan := *started.PlaybackPlan
	foreignPlan.PlanID = sessionBoundaryForeignPlan
	foreignPlan.SessionID = sessionBoundaryForeignSession
	foreignResponse := playback.DecisionResponseV3{
		ProtocolVersion: playback.ProtocolV3,
		Outcome:         playback.OutcomePlayableV3,
		ServerFeatures:  playback.ServerFeaturesV3(),
		SessionID:       sessionBoundaryForeignSession,
		PlaybackPlan:    &foreignPlan,
	}
	encoded, err := json.Marshal(foreignResponse)
	if err != nil {
		t.Fatal(err)
	}
	updated := *record
	updated.CurrentPlanID = foreignPlan.PlanID
	updated.CurrentPlan = foreignPlan
	updated.CurrentReplanRequestID = replan.ReplanRequestID
	if err := store.CompleteReplan(context.Background(), started.SessionID, replan.ReplanRequestID, lease.LeaseToken, record.CurrentReplanRequestID, encoded, updated); err != nil {
		t.Fatalf("seed completed replan: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/"+started.SessionID+"/replan", strings.NewReader(string(body))).WithContext(newAuthorizedPlaybackContext())
	req = withPlaybackRouteParam(req, "session_id", started.SessionID)
	rr := httptest.NewRecorder()
	handler.HandleReplanPlaybackV3(rr, req)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "stale_playback_plan") {
		t.Fatalf("foreign-session replan replay status = %d, body = %s; want 409 stale_playback_plan", rr.Code, rr.Body.String())
	}
	if body := rr.Body.String(); strings.Contains(body, sessionBoundaryForeignSession) || strings.Contains(body, sessionBoundaryForeignPlan) {
		t.Fatalf("foreign-session replan replay leaked the cached decision: %s", body)
	}
}

// completedReplanResponseMatchesAttemptV3 owns the replan-side session check. A
// terminal is session-less by construction; one that names another session was
// minted for a different attempt and must not be replayed for this one.
func TestCompletedReplanResponseMatchesAttemptV3RequiresSessionOwnership(t *testing.T) {
	plan := playback.PlanV3{PlanID: "plan-1", SessionID: "session-1"}
	record := &playback.AttemptRecordV3{SessionID: "session-1", CurrentPlan: plan}
	raw := func(response playback.DecisionResponseV3) json.RawMessage {
		t.Helper()
		encoded, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	terminal := func(sessionID string) playback.DecisionResponseV3 {
		response := playback.NewTerminalResponseV3("source_unavailable", "The provider listed no streams for this title.", true)
		response.SessionID = sessionID
		return response
	}
	ownedPlan := plan
	foreignPlan := plan
	foreignPlan.SessionID = sessionBoundaryForeignSession

	for _, tc := range []struct {
		name string
		raw  json.RawMessage
		want bool
	}{
		{name: "owned plan", raw: raw(playback.DecisionResponseV3{ProtocolVersion: playback.ProtocolV3, Outcome: playback.OutcomePlayableV3, SessionID: "session-1", PlaybackPlan: &ownedPlan}), want: true},
		{name: "foreign plan", raw: raw(playback.DecisionResponseV3{ProtocolVersion: playback.ProtocolV3, Outcome: playback.OutcomePlayableV3, SessionID: sessionBoundaryForeignSession, PlaybackPlan: &foreignPlan}), want: false},
		{name: "session-less terminal", raw: raw(terminal("")), want: true},
		{name: "owned terminal", raw: raw(terminal("session-1")), want: true},
		{name: "foreign terminal", raw: raw(terminal(sessionBoundaryForeignSession)), want: false},
		{name: "empty decision", raw: json.RawMessage(`{}`), want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := completedReplanResponseMatchesAttemptV3(tc.raw, record); got != tc.want {
				t.Fatalf("completedReplanResponseMatchesAttemptV3 = %v, want %v", got, tc.want)
			}
		})
	}
}

// A committed start supersedes the profile's earlier playback. Any replan lease
// still in flight for an older session must be fenced so its result cannot
// commit after the viewer adopted the new session; a completed lease stays as
// that older session's durable decision.
func TestStartPlaybackV3FencesPriorProfileReplanLeases(t *testing.T) {
	file := v3HandlerFixtureFile(t)
	manager := playback.NewSessionManager(0, 0)
	handler := NewPlaybackHandler(manager, testPlaybackFileResolver{file: file})
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{}}
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	store := handler.PlanStoreV3.(*playback.MemoryPlanStoreV3)

	start := func(attemptID string) playback.DecisionResponseV3 {
		t.Helper()
		request := v3HandlerStartRequest()
		request.PlaybackAttemptID = attemptID
		rr := httptest.NewRecorder()
		handler.HandleStartPlayback(rr, httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", strings.NewReader(marshalV3StartRequest(t, request))).WithContext(newAuthorizedPlaybackContext()))
		if rr.Code != http.StatusCreated {
			t.Fatalf("start %q status = %d, body = %s", attemptID, rr.Code, rr.Body.String())
		}
		var response playback.DecisionResponseV3
		if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil || response.SessionID == "" {
			t.Fatalf("start %q response: err=%v response=%#v", attemptID, err, response)
		}
		return response
	}

	prior := start("attempt-fence-0001")
	ctx := context.Background()
	staleLease, err := store.BeginReplan(ctx, prior.SessionID, "prior-replan-inflight", "digest-inflight", "", time.Now().Add(time.Minute))
	if err != nil || staleLease.State != playback.ReplanLeaseOwnedV3 {
		t.Fatalf("seed in-flight prior lease = %#v, err=%v", staleLease, err)
	}
	record, err := store.GetAttempt(ctx, prior.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := store.BeginReplan(ctx, prior.SessionID, "prior-replan-completed", "digest-completed", record.CurrentReplanRequestID, time.Now().Add(time.Minute))
	if err != nil || completed.State != playback.ReplanLeaseOwnedV3 {
		t.Fatalf("seed completed prior lease = %#v, err=%v", completed, err)
	}
	completedResponse := json.RawMessage(`{"protocol_version":3,"outcome":"playable"}`)
	finished := *record
	finished.CurrentReplanRequestID = "prior-replan-completed"
	if err := store.CompleteReplan(ctx, prior.SessionID, "prior-replan-completed", completed.LeaseToken, record.CurrentReplanRequestID, completedResponse, finished); err != nil {
		t.Fatalf("seed completed prior replan: %v", err)
	}

	next := start("attempt-fence-0002")
	if next.SessionID == prior.SessionID {
		t.Fatalf("second start reused the prior session %q", prior.SessionID)
	}
	// The durable revision after the seeded completion, so the fenced owner's
	// failure below is the missing lease row and not a base-revision mismatch.
	postCompletionRevision := "prior-replan-completed"
	// The fenced owner cannot commit: its lease row is gone, which the store
	// reports as a superseded replacement so the handler answers 409 instead of
	// committing a stale plan across the session boundary.
	if err := store.CompleteReplan(ctx, prior.SessionID, "prior-replan-inflight", staleLease.LeaseToken, postCompletionRevision, completedResponse, *record); !errors.Is(err, playback.ErrReplanSupersededV3) {
		t.Fatalf("stale owner completion error = %v, want ErrReplanSupersededV3", err)
	}
	// The same request now begins a fresh lease instead of reporting an
	// in-flight one, so a client still on the prior session can replan again.
	fenced, err := store.BeginReplan(ctx, prior.SessionID, "prior-replan-inflight", "digest-inflight", postCompletionRevision, time.Now().Add(time.Minute))
	if err != nil || fenced.State != playback.ReplanLeaseOwnedV3 {
		t.Fatalf("prior in-flight lease survived the new start: %#v, err=%v", fenced, err)
	}
	// A completed lease is the prior session's durable decision and stays.
	replayed, err := store.BeginReplan(ctx, prior.SessionID, "prior-replan-completed", "digest-completed", postCompletionRevision, time.Now().Add(time.Minute))
	if err != nil || replayed.State != playback.ReplanLeaseCompletedV3 || string(replayed.Response) != string(completedResponse) {
		t.Fatalf("completed prior lease = %#v, err=%v; want it preserved", replayed, err)
	}
	// The new session's own leases are unaffected by its start.
	if lease, err := store.BeginReplan(ctx, next.SessionID, "next-replan-inflight", "digest-next", "", time.Now().Add(time.Minute)); err != nil || lease.State != playback.ReplanLeaseOwnedV3 {
		t.Fatalf("new session lease = %#v, err=%v", lease, err)
	}
}
