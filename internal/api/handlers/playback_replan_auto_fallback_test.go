package handlers

import (
	"context"
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

// TestRenegotiateAutoFallbackV3 pins the mid-session re-arm: a replan that
// carries auto_fallback replaces the session flag so a later dead-source
// recovery rotates for a session that started explicit, while an omitted field
// leaves the negotiated intent untouched.
func TestRenegotiateAutoFallbackV3(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", 42, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	// The session starts explicit, which is the state the bug leaves behind.
	if err := manager.SetAutoFallback(session.ID, false); err != nil {
		t.Fatalf("SetAutoFallback off: %v", err)
	}
	handler := NewPlaybackHandler(manager)
	record := &playback.AttemptRecordV3{SessionID: session.ID}

	enabled := true
	if err := handler.renegotiateAutoFallbackV3(session.ID, record, playback.ReplanRequestV3{AutoFallback: &enabled}); err != nil {
		t.Fatalf("renegotiate: %v", err)
	}
	if got, ok := manager.AutoFallback(session.ID); !ok || !got {
		t.Fatalf("after re-arm AutoFallback = (%v, %v), want (true, true)", got, ok)
	}

	if err := handler.renegotiateAutoFallbackV3(session.ID, record, playback.ReplanRequestV3{}); err != nil {
		t.Fatalf("renegotiate omitted: %v", err)
	}
	if got, ok := manager.AutoFallback(session.ID); !ok || !got {
		t.Fatalf("omitted auto_fallback changed the intent: AutoFallback = (%v, %v), want (true, true)", got, ok)
	}

	disabled := false
	if err := handler.renegotiateAutoFallbackV3(session.ID, record, playback.ReplanRequestV3{AutoFallback: &disabled}); err != nil {
		t.Fatalf("renegotiate off: %v", err)
	}
	if got, ok := manager.AutoFallback(session.ID); !ok || got {
		t.Fatalf("after disarming AutoFallback = (%v, %v), want (false, true)", got, ok)
	}
}

// TestReconstructedSessionKeepsNegotiatedAutoFallbackV3 is the reconstruction
// regression: a mid-session re-arm lives only on the live session, so after the
// process (or replica) rebuilds the session from its durable attempt the policy
// must come from the stored request, not the original start selection.
func TestReconstructedSessionKeepsNegotiatedAutoFallbackV3(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", 42, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	handler := NewPlaybackHandler(manager)
	record := &playback.AttemptRecordV3{
		SessionID:         session.ID,
		NormalizedRequest: playback.StartRequestV3{FileSelection: playback.FileSelectionExplicitV3},
	}

	if resolveReplanAutoFallbackV3(manager, session.ID, record) {
		t.Fatal("an explicit start resolved to auto fallback before any re-arm")
	}

	enabled := true
	if err := handler.renegotiateAutoFallbackV3(session.ID, record, playback.ReplanRequestV3{AutoFallback: &enabled}); err != nil {
		t.Fatalf("renegotiate: %v", err)
	}

	// The rebuilt manager has no in-memory flag for this session: the durable
	// request is the only surviving record of the negotiated policy.
	reconstructed := playback.NewSessionManager(0, 0)
	if !resolveReplanAutoFallbackV3(reconstructed, session.ID, record) {
		t.Fatal("a reconstructed session reverted to the start request instead of the negotiated auto re-arm")
	}

	// A later mid-session disarm must survive reconstruction just the same.
	disabled := false
	if err := handler.renegotiateAutoFallbackV3(session.ID, record, playback.ReplanRequestV3{AutoFallback: &disabled}); err != nil {
		t.Fatalf("renegotiate off: %v", err)
	}
	if resolveReplanAutoFallbackV3(reconstructed, session.ID, record) {
		t.Fatal("a reconstructed session ignored a mid-session disarm")
	}
}

// TestRenegotiateAutoFallbackV3FailsClosed pins the refusal: a re-arm the
// session cannot adopt must surface as an error so the replan fails instead of
// continuing with a policy the session will not honor.
func TestRenegotiateAutoFallbackV3FailsClosed(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	handler := NewPlaybackHandler(manager)
	record := &playback.AttemptRecordV3{}
	enabled := true

	err := handler.renegotiateAutoFallbackV3("missing-session", record, playback.ReplanRequestV3{AutoFallback: &enabled})
	if !errors.Is(err, playback.ErrSessionNotFound) {
		t.Fatalf("missing session err = %v, want ErrSessionNotFound", err)
	}

	unsupported := NewPlaybackHandler(nil)
	err = unsupported.renegotiateAutoFallbackV3("any-session", record, playback.ReplanRequestV3{AutoFallback: &enabled})
	if !errors.Is(err, errAutoFallbackUnsupportedV3) {
		t.Fatalf("unsupported manager err = %v, want errAutoFallbackUnsupportedV3", err)
	}
}

// autoFallbackSetFailManager is a session manager whose auto-fallback set always
// fails, modeling a session that cannot adopt the negotiated policy.
type autoFallbackSetFailManager struct {
	*playback.SessionManager
}

func (autoFallbackSetFailManager) SetAutoFallback(string, bool) error {
	return errors.New("session refuses auto-fallback negotiation")
}

// TestHandleReplanPlaybackV3FailsClosedWhenAutoFallbackSetFails drives the full
// replan path: when the session refuses the stated policy the request must fail
// and no terminal decision or durable policy change may be persisted.
func TestHandleReplanPlaybackV3FailsClosedWhenAutoFallbackSetFails(t *testing.T) {
	base := playback.NewSessionManager(0, 0)
	manager := autoFallbackSetFailManager{SessionManager: base}
	file := v3HandlerFixtureFile(t)
	handler := NewPlaybackHandler(manager, testPlaybackFileResolver{file: file})

	live, err := base.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	const attemptID = "attempt-auto-fallback-fail-0001"
	const failedPlanID = "plan-failed-0001"
	if err := handler.PlanStoreV3.SaveAttempt(context.Background(), playback.AttemptRecordV3{
		SessionID:            live.ID,
		PlaybackAttemptID:    attemptID,
		UserID:               1,
		ProfileID:            "profile-1",
		CurrentPlanID:        failedPlanID,
		RequestedMediaFileID: file.ID,
		EffectiveMediaFileID: file.ID,
		NormalizedRequest:    playback.StartRequestV3{FileSelection: playback.FileSelectionExplicitV3},
		ExpiresAt:            time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("save attempt: %v", err)
	}
	store := &countingReplanPlanStoreV3{PlanStoreV3: handler.PlanStoreV3}
	handler.PlanStoreV3 = store

	baseReq := v3HandlerStartRequest()
	enabled := true
	replan := playback.ReplanRequestV3{
		ProtocolVersion:       playback.ProtocolV3,
		Operation:             playback.ReplanOperationFailureRecoveryV3,
		PlaybackAttemptID:     attemptID,
		ReplanRequestID:       "replan-auto-fallback-fail-0001",
		FailedPlanID:          failedPlanID,
		PlanAttemptID:         "plan-attempt-0001",
		PlanAttemptKey:        "v3:plan-attempt-key-0001",
		AttemptCount:          1,
		PositionSeconds:       1,
		AutoFallback:          &enabled,
		Failure:               playback.FailureV3{Classification: "transcode_start_failed"},
		Capabilities:          baseReq.Capabilities,
		ClientPlaybackContext: baseReq.ClientPlaybackContext,
	}
	body, err := json.Marshal(replan)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/"+live.ID+"/replan", strings.NewReader(string(body))).WithContext(newAuthorizedPlaybackContext())
	req = withPlaybackRouteParam(req, "session_id", live.ID)
	rr := httptest.NewRecorder()
	handler.HandleReplanPlaybackV3(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", rr.Code, rr.Body.String())
	}
	if store.completeReplanCalls != 0 {
		t.Fatalf("CompleteReplan called %d times, want 0: the refused policy must not persist a decision", store.completeReplanCalls)
	}
	record, err := handler.PlanStoreV3.GetAttempt(context.Background(), live.ID)
	if err != nil {
		t.Fatalf("reload attempt: %v", err)
	}
	if record.NormalizedRequest.FileSelection != playback.FileSelectionExplicitV3 || record.NormalizedRequest.AllowAlternateVersions != nil {
		t.Fatalf("durable policy changed despite the refusal: %#v", record.NormalizedRequest)
	}
}

// failingReplanFileResolverV3 refuses every file load, so executeReplanV3 fails
// with source_unavailable — a terminal the handler persists while the live
// session is still intact. That is the state a failed replan must leave the
// auto-fallback policy in: exactly as it was before the request applied its
// speculative re-negotiation.
type failingReplanFileResolverV3 struct{}

func (failingReplanFileResolverV3) GetByID(context.Context, int) (*models.MediaFile, error) {
	return nil, errors.New("resolver unavailable")
}

// TestHandleReplanPlaybackV3FailedReplanKeepsOriginalAutoFallbackPolicy drives a
// replan that renegotiates auto-fallback to enabled and then fails during
// execution. The failed replan must not leave the speculative policy behind:
// the live session flag (value and set-bit) and the durable normalized request
// must both read exactly as they did before the request.
func TestHandleReplanPlaybackV3FailedReplanKeepsOriginalAutoFallbackPolicy(t *testing.T) {
	file := v3HandlerFixtureFile(t)

	for _, test := range []struct {
		name string
		// original is nil for a session that never negotiated the field, so the
		// set-bit stays false; otherwise it is the explicit starting value.
		original *bool
	}{
		{name: "unset session goes back to unset"},
		{name: "explicit off stays explicitly off", original: boolPtr(false)},
		{name: "explicit on stays explicitly on", original: boolPtr(true)},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := playback.NewSessionManager(0, 0)
			handler := NewPlaybackHandler(manager, testPlaybackFileResolver{file: file})

			live, err := manager.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
			if err != nil {
				t.Fatalf("start session: %v", err)
			}
			if test.original != nil {
				if err := manager.SetAutoFallback(live.ID, *test.original); err != nil {
					t.Fatalf("seed auto-fallback: %v", err)
				}
			}
			wantEnabled, wantSet := false, false
			if test.original != nil {
				wantEnabled, wantSet = *test.original, true
			}

			const attemptID = "attempt-failed-policy-0001"
			const failedPlanID = "plan-failed-0001"
			if err := handler.PlanStoreV3.SaveAttempt(context.Background(), playback.AttemptRecordV3{
				SessionID:            live.ID,
				PlaybackAttemptID:    attemptID,
				UserID:               1,
				ProfileID:            "profile-1",
				CurrentPlanID:        failedPlanID,
				RequestedMediaFileID: file.ID,
				EffectiveMediaFileID: file.ID,
				NormalizedRequest: playback.StartRequestV3{
					FileSelection: playback.FileSelectionExplicitV3,
					// An explicit pin of the original policy: absent here, so
					// the durable record reads explicit + allows-alternates nil.
				},
				ExpiresAt: time.Now().Add(time.Hour),
			}); err != nil {
				t.Fatalf("save attempt: %v", err)
			}

			// Fail execution at the first source load, after the handler has
			// already applied the speculative policy.
			handler.fileResolver = failingReplanFileResolverV3{}

			baseReq := v3HandlerStartRequest()
			enabled := true
			replan := playback.ReplanRequestV3{
				ProtocolVersion:       playback.ProtocolV3,
				Operation:             playback.ReplanOperationFailureRecoveryV3,
				PlaybackAttemptID:     attemptID,
				ReplanRequestID:       "replan-failed-policy-0001",
				FailedPlanID:          failedPlanID,
				PlanAttemptID:         "plan-attempt-0001",
				PlanAttemptKey:        "v3:plan-attempt-key-0001",
				AttemptCount:          1,
				PositionSeconds:       1,
				AutoFallback:          &enabled,
				Failure:               playback.FailureV3{Classification: "transcode_start_failed"},
				Capabilities:          baseReq.Capabilities,
				ClientPlaybackContext: baseReq.ClientPlaybackContext,
			}
			body, err := json.Marshal(replan)
			if err != nil {
				t.Fatal(err)
			}

			req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/"+live.ID+"/replan", strings.NewReader(string(body))).WithContext(newAuthorizedPlaybackContext())
			req = withPlaybackRouteParam(req, "session_id", live.ID)
			rr := httptest.NewRecorder()
			handler.HandleReplanPlaybackV3(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 terminal; body = %s", rr.Code, rr.Body.String())
			}
			var response playback.DecisionResponseV3
			if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if response.Terminal == nil {
				t.Fatalf("response = %#v, want a terminal from the failed replan", response)
			}

			if gotEnabled, gotSet := manager.AutoFallback(live.ID); gotEnabled != wantEnabled || gotSet != wantSet {
				t.Fatalf("live auto-fallback = (%v, %v), want (%v, %v): a failed replan must not keep the speculative policy",
					gotEnabled, gotSet, wantEnabled, wantSet)
			}

			record, err := handler.PlanStoreV3.GetAttempt(context.Background(), live.ID)
			if err != nil {
				t.Fatalf("reload attempt: %v", err)
			}
			if record.NormalizedRequest.FileSelection != playback.FileSelectionExplicitV3 || record.NormalizedRequest.AllowAlternateVersions != nil {
				t.Fatalf("durable policy = %#v, want the pre-replan explicit pin with no explicit allow flag", record.NormalizedRequest)
			}
		})
	}
}
