package handlers

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// A replan for a session whose in-memory entry has been reaped must return the
// session_not_found 404 immediately, without queueing behind the replan slot
// budget. Production saw an ~11s slot wait before that verdict because the
// cheap lookups ran after the slot, per-session mutex, and advisory lock.
func TestHandleReplanPlaybackV3DeadSessionFailsFastWithoutReplanSlot(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	handler := NewPlaybackHandler(manager, testPlaybackFileResolver{file: v3HandlerFixtureFile(t)})

	// The attempt row outlives the in-memory session (a reaped/stopped session
	// on this replica), so the store lookup succeeds and only the live-session
	// check can produce the 404.
	const sessionID = "11111111-1111-1111-1111-111111111111"
	const attemptID = "attempt-dead-session-0001"
	if err := handler.PlanStoreV3.SaveAttempt(context.Background(), playback.AttemptRecordV3{
		SessionID:         sessionID,
		PlaybackAttemptID: attemptID,
		UserID:            1,
		ProfileID:         "profile-1",
		ExpiresAt:         time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("save attempt: %v", err)
	}

	// Saturate the replan slot budget. A request that reaches the slot blocks
	// until the request context expires and reports capacity exhaustion; the
	// dead session must be rejected before that.
	handler.v3ReplanSlotsOnce.Do(func() { handler.v3ReplanSlots = make(chan struct{}, 1) })
	handler.v3ReplanSlots <- struct{}{}

	base := v3HandlerStartRequest()
	replan := playback.ReplanRequestV3{
		ProtocolVersion:       playback.ProtocolV3,
		Operation:             playback.ReplanOperationFailureRecoveryV3,
		PlaybackAttemptID:     attemptID,
		ReplanRequestID:       "replan-dead-session-0001",
		FailedPlanID:          "plan-failed-0001",
		PlanAttemptID:         "plan-attempt-0001",
		PlanAttemptKey:        "v3:plan-attempt-key-0001",
		AttemptCount:          1,
		PositionSeconds:       1,
		Failure:               playback.FailureV3{Classification: "transcode_start_failed"},
		Capabilities:          base.Capabilities,
		ClientPlaybackContext: base.ClientPlaybackContext,
	}
	body, err := json.Marshal(replan)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(newAuthorizedPlaybackContext(), 5*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/"+sessionID+"/replan", strings.NewReader(string(body))).WithContext(ctx)
	req = withPlaybackRouteParam(req, "session_id", sessionID)
	rr := httptest.NewRecorder()

	started := time.Now()
	handler.HandleReplanPlaybackV3(rr, req)
	elapsed := time.Since(started)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rr.Code, rr.Body.String())
	}
	if elapsed > time.Second {
		t.Fatalf("dead-session replan took %v; it queued for a replan slot instead of failing fast", elapsed)
	}
	// The dead session must not have consumed the saturated slot: a second
	// token still does not fit.
	select {
	case handler.v3ReplanSlots <- struct{}{}:
		t.Fatal("dead-session replan consumed the saturated replan slot")
	default:
	}
}

// The dead-session 404 is the correct fail-fast answer to a client that
// replanned a session another replica already retired, so it must be accounted
// for at info. A warning here is what turned a client's ordinary replan loop
// into operator pages.
func TestReplanDeadSessionIsLoggedAtInfoNotWarn(t *testing.T) {
	capture := &levelRecordingHandler{}
	previous := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(previous) })

	manager := playback.NewSessionManager(0, 0)
	handler := NewPlaybackHandler(manager, testPlaybackFileResolver{file: v3HandlerFixtureFile(t)})

	const sessionID = "22222222-2222-2222-2222-222222222222"
	const attemptID = "attempt-dead-session-log-0001"
	if err := handler.PlanStoreV3.SaveAttempt(context.Background(), playback.AttemptRecordV3{
		SessionID:         sessionID,
		PlaybackAttemptID: attemptID,
		UserID:            1,
		ProfileID:         "profile-1",
		ExpiresAt:         time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("save attempt: %v", err)
	}

	base := v3HandlerStartRequest()
	body, err := json.Marshal(playback.ReplanRequestV3{
		ProtocolVersion:       playback.ProtocolV3,
		Operation:             playback.ReplanOperationFailureRecoveryV3,
		PlaybackAttemptID:     attemptID,
		ReplanRequestID:       "replan-dead-session-log-0001",
		FailedPlanID:          "plan-failed-0001",
		PlanAttemptID:         "plan-attempt-0001",
		PlanAttemptKey:        "v3:plan-attempt-key-0001",
		AttemptCount:          1,
		PositionSeconds:       1,
		Failure:               playback.FailureV3{Classification: "transcode_start_failed"},
		Capabilities:          base.Capabilities,
		ClientPlaybackContext: base.ClientPlaybackContext,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/"+sessionID+"/replan", strings.NewReader(string(body))).
		WithContext(newAuthorizedPlaybackContext())
	req = withPlaybackRouteParam(req, "session_id", sessionID)
	rr := httptest.NewRecorder()
	handler.HandleReplanPlaybackV3(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want the fast 404; body = %s", rr.Code, rr.Body.String())
	}

	if !capture.has(slog.LevelInfo, "playback replan answered unchanged") {
		t.Fatalf("dead-session replan was not accounted for at info; records = %v", capture.messages())
	}
	for _, message := range capture.messagesAt(slog.LevelWarn) {
		if strings.Contains(message, "replan") || strings.Contains(message, "session") {
			t.Fatalf("dead-session replan logged a warning: %q", message)
		}
	}
}

// A replan for a session that has already ended must return the same fast 404// as an unknown session: the attempt row can outlive a finished play, and the
// handler must reject it before the slot, mutex, and advisory lock.
func TestHandleReplanPlaybackV3EndedSessionFailsFastWithoutReplanSlot(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	file := v3HandlerFixtureFile(t)
	handler := NewPlaybackHandler(manager, testPlaybackFileResolver{file: file})

	live, err := manager.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	sessionID := live.ID
	const attemptID = "attempt-ended-session-0001"
	if err := handler.PlanStoreV3.SaveAttempt(context.Background(), playback.AttemptRecordV3{
		SessionID:         sessionID,
		PlaybackAttemptID: attemptID,
		UserID:            1,
		ProfileID:         "profile-1",
		ExpiresAt:         time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("save attempt: %v", err)
	}
	if err := manager.FinishSession(context.Background(), sessionID); err != nil {
		t.Fatalf("finish session: %v", err)
	}

	// Saturate the replan slot budget exactly as the dead-session test does; a
	// request that reaches the queue would block until its context expires.
	handler.v3ReplanSlotsOnce.Do(func() { handler.v3ReplanSlots = make(chan struct{}, 1) })
	handler.v3ReplanSlots <- struct{}{}

	base := v3HandlerStartRequest()
	replan := playback.ReplanRequestV3{
		ProtocolVersion:       playback.ProtocolV3,
		Operation:             playback.ReplanOperationFailureRecoveryV3,
		PlaybackAttemptID:     attemptID,
		ReplanRequestID:       "replan-ended-session-0001",
		FailedPlanID:          "plan-failed-0001",
		PlanAttemptID:         "plan-attempt-0001",
		PlanAttemptKey:        "v3:plan-attempt-key-0001",
		AttemptCount:          1,
		PositionSeconds:       1,
		Failure:               playback.FailureV3{Classification: "transcode_start_failed"},
		Capabilities:          base.Capabilities,
		ClientPlaybackContext: base.ClientPlaybackContext,
	}
	body, err := json.Marshal(replan)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(newAuthorizedPlaybackContext(), 5*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/"+sessionID+"/replan", strings.NewReader(string(body))).WithContext(ctx)
	req = withPlaybackRouteParam(req, "session_id", sessionID)
	rr := httptest.NewRecorder()

	started := time.Now()
	handler.HandleReplanPlaybackV3(rr, req)
	elapsed := time.Since(started)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), playbackSessionNotFoundErrorCode) {
		t.Fatalf("body = %s, want %s", rr.Body.String(), playbackSessionNotFoundErrorCode)
	}
	if elapsed > time.Second {
		t.Fatalf("ended-session replan took %v; it queued for a replan slot instead of failing fast", elapsed)
	}
	select {
	case handler.v3ReplanSlots <- struct{}{}:
		t.Fatal("ended-session replan consumed the saturated replan slot")
	default:
	}
}

// reapingAfterFirstGetAttemptPlanStoreV3 models a stop landing while a replan
// request waits on the replan slot and the per-session locks: the pre-lock read
// sees the session live, and the post-lock re-read reaps it from the in-memory
// manager before the handler can reserve a replan lease. It counts the lease
// mutations so a test can prove none happened.
type reapingAfterFirstGetAttemptPlanStoreV3 struct {
	playback.PlanStoreV3
	reap                func()
	reads               int
	beginReplanCalls    int
	completeReplanCalls int
}

func (s *reapingAfterFirstGetAttemptPlanStoreV3) GetAttempt(ctx context.Context, sessionID string) (*playback.AttemptRecordV3, error) {
	record, err := s.PlanStoreV3.GetAttempt(ctx, sessionID)
	s.reads++
	if s.reads == 2 && s.reap != nil {
		s.reap()
	}
	return record, err
}

func (s *reapingAfterFirstGetAttemptPlanStoreV3) BeginReplan(ctx context.Context, sessionID, replanRequestID, digest, baseReplanRequestID string, expiresAt time.Time) (playback.ReplanLeaseV3, error) {
	s.beginReplanCalls++
	return s.PlanStoreV3.BeginReplan(ctx, sessionID, replanRequestID, digest, baseReplanRequestID, expiresAt)
}

func (s *reapingAfterFirstGetAttemptPlanStoreV3) CompleteReplan(ctx context.Context, sessionID, requestID, leaseToken, baseReplanRequestID string, response json.RawMessage, record playback.AttemptRecordV3) error {
	s.completeReplanCalls++
	return s.PlanStoreV3.CompleteReplan(ctx, sessionID, requestID, leaseToken, baseReplanRequestID, response, record)
}

// A session reaped while the replan request waited for the locks must still
// read as the fast 404 after the post-lock attempt re-read. Without the
// post-lock live-session check the handler reserved a replan lease and
// persisted executeReplanV3's session_expired as a terminal 200, which the web
// client does not rebuild from.
func TestHandleReplanPlaybackV3ReapedDuringLockWaitReturns404WithoutLease(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	file := v3HandlerFixtureFile(t)
	handler := NewPlaybackHandler(manager, testPlaybackFileResolver{file: file})

	live, err := manager.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	sessionID := live.ID
	const attemptID = "attempt-reaped-lock-wait-0001"
	const failedPlanID = "plan-failed-0001"

	// The durable attempt outlives the in-memory session, so only the
	// live-session check can produce the 404. CurrentPlanID matches the replan's
	// failed plan so, without that check, the request reaches executeReplanV3
	// and persists its session_expired terminal as an HTTP 200.
	if err := handler.PlanStoreV3.SaveAttempt(context.Background(), playback.AttemptRecordV3{
		SessionID:            sessionID,
		PlaybackAttemptID:    attemptID,
		UserID:               1,
		ProfileID:            "profile-1",
		CurrentPlanID:        failedPlanID,
		RequestedMediaFileID: file.ID,
		EffectiveMediaFileID: file.ID,
		ExpiresAt:            time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("save attempt: %v", err)
	}

	// Reap the session on the post-lock attempt re-read, reproducing a stop
	// that landed while this request queued behind the replan locks.
	store := &reapingAfterFirstGetAttemptPlanStoreV3{
		PlanStoreV3: handler.PlanStoreV3,
		reap: func() {
			if stopErr := manager.StopSession(sessionID); stopErr != nil {
				t.Errorf("stop session: %v", stopErr)
			}
		},
	}
	handler.PlanStoreV3 = store

	base := v3HandlerStartRequest()
	replan := playback.ReplanRequestV3{
		ProtocolVersion:       playback.ProtocolV3,
		Operation:             playback.ReplanOperationFailureRecoveryV3,
		PlaybackAttemptID:     attemptID,
		ReplanRequestID:       "replan-reaped-lock-wait-0001",
		FailedPlanID:          failedPlanID,
		PlanAttemptID:         "plan-attempt-0001",
		PlanAttemptKey:        "v3:plan-attempt-key-0001",
		AttemptCount:          1,
		PositionSeconds:       1,
		Failure:               playback.FailureV3{Classification: "transcode_start_failed"},
		Capabilities:          base.Capabilities,
		ClientPlaybackContext: base.ClientPlaybackContext,
	}
	body, err := json.Marshal(replan)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/"+sessionID+"/replan", strings.NewReader(string(body))).WithContext(newAuthorizedPlaybackContext())
	req = withPlaybackRouteParam(req, "session_id", sessionID)
	rr := httptest.NewRecorder()

	handler.HandleReplanPlaybackV3(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), playbackSessionNotFoundErrorCode) {
		t.Fatalf("body = %s, want %s", rr.Body.String(), playbackSessionNotFoundErrorCode)
	}
	if store.beginReplanCalls != 0 {
		t.Fatalf("BeginReplan called %d times, want 0: a reaped session must not reserve a replan lease", store.beginReplanCalls)
	}
	if store.completeReplanCalls != 0 {
		t.Fatalf("CompleteReplan called %d times, want 0: a reaped session must not persist a terminal response", store.completeReplanCalls)
	}
}

// stoppingDuringExecutionResolverV3 models a stop landing inside executeReplanV3
// after the handler's post-lock live-session check has already observed the
// session live. The check holds no lock across BeginReplan or execution, so the
// first source load inside execution is a real window for the stop to land.
type stoppingDuringExecutionResolverV3 struct {
	FilePathResolver
	stop func()
	once atomic.Bool
}

func (r *stoppingDuringExecutionResolverV3) GetByID(ctx context.Context, id int) (*models.MediaFile, error) {
	if r.once.CompareAndSwap(false, true) && r.stop != nil {
		r.stop()
	}
	return r.FilePathResolver.GetByID(ctx, id)
}

// countingReplanPlanStoreV3 counts the lease mutations a replan request makes,
// so a test can prove no completed 200 terminal was persisted.
type countingReplanPlanStoreV3 struct {
	playback.PlanStoreV3
	beginReplanCalls    int
	completeReplanCalls int
}

func (s *countingReplanPlanStoreV3) BeginReplan(ctx context.Context, sessionID, replanRequestID, digest, baseReplanRequestID string, expiresAt time.Time) (playback.ReplanLeaseV3, error) {
	s.beginReplanCalls++
	return s.PlanStoreV3.BeginReplan(ctx, sessionID, replanRequestID, digest, baseReplanRequestID, expiresAt)
}

func (s *countingReplanPlanStoreV3) CompleteReplan(ctx context.Context, sessionID, requestID, leaseToken, baseReplanRequestID string, response json.RawMessage, record playback.AttemptRecordV3) error {
	s.completeReplanCalls++
	return s.PlanStoreV3.CompleteReplan(ctx, sessionID, requestID, leaseToken, baseReplanRequestID, response, record)
}

// A stop or idle reap can land after the post-lock live-session check but
// before executeReplanV3 reads the session, because that check holds no lock
// across BeginReplan or execution. The late session_expired must translate to
// the fast 404 instead of being persisted through CompleteReplan as a terminal
// HTTP 200 the web client does not rebuild from, and the reserved lease must be
// released non-terminally.
func TestHandleReplanPlaybackV3StopDuringExecutionReturns404WithoutTerminal(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	file := v3HandlerFixtureFile(t)
	handler := NewPlaybackHandler(manager, testPlaybackFileResolver{file: file})

	live, err := manager.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	sessionID := live.ID
	const attemptID = "attempt-stop-mid-execution-0001"
	const failedPlanID = "plan-failed-0001"

	// The durable attempt outlives the in-memory session, so only the live check
	// inside executeReplanV3 can refuse the replan. CurrentPlanID matches the
	// failed plan so, without the translation, the request completes the lease
	// by persisting its session_expired terminal as an HTTP 200.
	if err := handler.PlanStoreV3.SaveAttempt(context.Background(), playback.AttemptRecordV3{
		SessionID:            sessionID,
		PlaybackAttemptID:    attemptID,
		UserID:               1,
		ProfileID:            "profile-1",
		CurrentPlanID:        failedPlanID,
		RequestedMediaFileID: file.ID,
		EffectiveMediaFileID: file.ID,
		ExpiresAt:            time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("save attempt: %v", err)
	}

	// Stop the session from inside executeReplanV3's first source load, after
	// the handler already passed its post-lock GetSession check and reserved the
	// replan lease.
	handler.fileResolver = &stoppingDuringExecutionResolverV3{
		FilePathResolver: handler.fileResolver,
		stop: func() {
			if stopErr := manager.StopSession(sessionID); stopErr != nil {
				t.Errorf("stop session: %v", stopErr)
			}
		},
	}
	store := &countingReplanPlanStoreV3{PlanStoreV3: handler.PlanStoreV3}
	handler.PlanStoreV3 = store

	base := v3HandlerStartRequest()
	replan := playback.ReplanRequestV3{
		ProtocolVersion:       playback.ProtocolV3,
		Operation:             playback.ReplanOperationFailureRecoveryV3,
		PlaybackAttemptID:     attemptID,
		ReplanRequestID:       "replan-stop-mid-execution-0001",
		FailedPlanID:          failedPlanID,
		PlanAttemptID:         "plan-attempt-0001",
		PlanAttemptKey:        "v3:plan-attempt-key-0001",
		AttemptCount:          1,
		PositionSeconds:       1,
		Failure:               playback.FailureV3{Classification: "transcode_start_failed"},
		Capabilities:          base.Capabilities,
		ClientPlaybackContext: base.ClientPlaybackContext,
	}
	body, err := json.Marshal(replan)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/"+sessionID+"/replan", strings.NewReader(string(body))).WithContext(newAuthorizedPlaybackContext())
	req = withPlaybackRouteParam(req, "session_id", sessionID)
	rr := httptest.NewRecorder()

	handler.HandleReplanPlaybackV3(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), playbackSessionNotFoundErrorCode) {
		t.Fatalf("body = %s, want %s", rr.Body.String(), playbackSessionNotFoundErrorCode)
	}
	if store.beginReplanCalls != 1 {
		t.Fatalf("BeginReplan called %d times, want 1: the stop landed after the lease was reserved", store.beginReplanCalls)
	}
	if store.completeReplanCalls != 0 {
		t.Fatalf("CompleteReplan called %d times, want 0: a session stopped mid-execution must not persist a terminal 200", store.completeReplanCalls)
	}
}
