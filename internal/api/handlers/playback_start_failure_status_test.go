package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/noderouting"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// failingSaveAttemptPlanStoreV3 fails every terminal-start save with one fixed
// error while leaving the attempt lookup on the memory store. It models the
// production shape where the plan/transport failed and the subsequent terminal
// save fails on the same request budget.
type failingSaveAttemptPlanStoreV3 struct {
	playback.PlanStoreV3
	err error
}

func (s *failingSaveAttemptPlanStoreV3) SaveAttempt(context.Context, playback.AttemptRecordV3) error {
	return s.err
}

// A start that hits its request budget after a retryable transport failure must
// answer the retryable problem with Retry-After, not the 500 internal_error the
// failed terminal save used to produce. The session limit stands in for the
// transport failure: it makes startPlannedPlaybackV3 return a retryable
// statusErr, and the store makes the terminal save fail with the expired
// request budget, exactly as a 30 s virtual start does in production.
func TestHandleStartPlaybackV3RetryableStartTimeoutAnswers503(t *testing.T) {
	source := v3HandlerFixtureFile(t)
	manager := playback.NewSessionManager(1, 0)
	if _, err := manager.StartSession(1, "profile-1", 99, playback.PlayDirect, false); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	handler := NewPlaybackHandler(manager, mapPlaybackFileResolver{files: map[int]*models.MediaFile{source.ID: source}})
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	handler.PlanStoreV3 = &failingSaveAttemptPlanStoreV3{PlanStoreV3: handler.PlanStoreV3, err: context.DeadlineExceeded}

	rr := httptest.NewRecorder()
	handler.HandleStartPlayback(rr, httptest.NewRequest(
		http.MethodPost,
		"/api/v1/playback/start",
		strings.NewReader(marshalV3StartRequest(t, v3HandlerStartRequest())),
	).WithContext(newAuthorizedPlaybackContext()))

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("start status = %d, want 503, body = %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Retry-After"); got != "5" {
		t.Fatalf("Retry-After = %q, want 5", got)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error != "dependency_unavailable" {
		t.Fatalf("error code = %q, want dependency_unavailable", body.Error)
	}
}

// A genuine store fault is still a server error: the attempt save fails
// before any transport commits, and the failed terminal save must keep
// reporting the store fault rather than disguising it as a retryable refusal.
func TestHandleStartPlaybackV3NonRetryablePersistFaultAnswers500(t *testing.T) {
	source := v3HandlerFixtureFile(t)
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0), mapPlaybackFileResolver{files: map[int]*models.MediaFile{source.ID: source}})
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	handler.PlanStoreV3 = &failingSaveAttemptPlanStoreV3{PlanStoreV3: handler.PlanStoreV3, err: errors.New("database unavailable")}

	rr := httptest.NewRecorder()
	handler.HandleStartPlayback(rr, httptest.NewRequest(
		http.MethodPost,
		"/api/v1/playback/start",
		strings.NewReader(marshalV3StartRequest(t, v3HandlerStartRequest())),
	).WithContext(newAuthorizedPlaybackContext()))

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("start status = %d, want 500, body = %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After = %q, want none on a genuine internal fault", got)
	}
	if !strings.Contains(rr.Body.String(), `"error":"internal_error"`) {
		t.Fatalf("body = %s, want internal_error", rr.Body.String())
	}
}

// Route-capacity exhaustion is a real transient state, not a defect: it keeps
// its existing machine-readable code and the retryable status even when the
// terminal save also fails.
func TestStartTerminalPersistOperationErrorV3RouteCapacityIsRetryable(t *testing.T) {
	err := startTerminalPersistOperationErrorV3(
		context.Background(),
		errors.New("plan store unavailable"),
		true,
		&transportErrorV3{
			reason:    string(noderouting.OutcomeCapacityUnavailable),
			message:   "No playback route satisfies the configured policy and current node availability.",
			retryable: true,
		},
	)
	var operation *PlaybackOperationError
	if !errors.As(err, &operation) {
		t.Fatalf("err = %v, want *PlaybackOperationError", err)
	}
	if operation.Status != http.StatusServiceUnavailable || operation.Code != string(noderouting.OutcomeCapacityUnavailable) {
		t.Fatalf("operation = %+v, want 503 %s", operation, noderouting.OutcomeCapacityUnavailable)
	}
	if operation.RetryAfter != playbackStartRetryAfterSeconds {
		t.Fatalf("RetryAfter = %d, want %d", operation.RetryAfter, playbackStartRetryAfterSeconds)
	}
	if !strings.Contains(strings.ToLower(operation.Message), "capacity") {
		t.Fatalf("message = %q, want it to name the capacity state", operation.Message)
	}
}

// The request deadline can expire on a non-retryable verdict too; the client
// still gets a retryable answer because the save was cut off by the budget, not
// by a store fault.
func TestStartTerminalPersistOperationErrorV3ExpiredBudgetIsRetryable(t *testing.T) {
	err := startTerminalPersistOperationErrorV3(context.Background(), context.DeadlineExceeded, false, nil)
	var operation *PlaybackOperationError
	if !errors.As(err, &operation) {
		t.Fatalf("err = %v, want *PlaybackOperationError", err)
	}
	if operation.Status != http.StatusServiceUnavailable || operation.Code != "dependency_unavailable" || operation.RetryAfter == 0 {
		t.Fatalf("operation = %+v, want a retryable 503", operation)
	}
}
