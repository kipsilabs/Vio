package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// cancelSensitiveAdminStore fails a durable write whenever the context it was
// handed is already canceled, the way a Postgres/Redis call does. It lets a
// test tell a detached teardown from one that inherited the canceled request.
type cancelSensitiveAdminStore struct {
	deleteCalls int
	deleted     map[string]bool
}

func (s *cancelSensitiveAdminStore) RecordHistory(context.Context, AdminPlaybackHistoryEntry) error {
	return nil
}

func (s *cancelSensitiveAdminStore) DeleteSession(ctx context.Context, sessionID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.deleteCalls++
	if s.deleted == nil {
		s.deleted = make(map[string]bool)
	}
	s.deleted[sessionID] = true
	return nil
}

// cancelAwarePlanStore records the context state the attempt stop saw. It
// embeds the concrete memory store so it keeps satisfying every PlanStoreV3
// and ProgressStoreV3 method, and overrides only StopAttempt.
type cancelAwarePlanStore struct {
	*playback.MemoryPlanStoreV3
	stopCalled bool
	stopCtxErr error
}

func (s *cancelAwarePlanStore) StopAttempt(
	ctx context.Context, sessionID, stopID string, final *playback.ProgressSampleV3,
) (playback.StopReceiptV3, bool, error) {
	s.stopCalled = true
	if err := ctx.Err(); err != nil {
		s.stopCtxErr = err
		return playback.StopReceiptV3{}, false, err
	}
	return s.MemoryPlanStoreV3.StopAttempt(ctx, sessionID, stopID, final)
}

func canceledPlaybackContext() context.Context {
	ctx, cancel := context.WithCancel(newAuthorizedPlaybackContext())
	cancel()
	return ctx
}

// A client that cancels its remux stream mid-teardown (a track switch aborts
// the old transport) must not strand the teardown: the synced-session delete
// has to run to completion under a detached context, not fail with
// "context canceled" because the request it was reached from is gone.
func TestStreamAbortTeardownCompletesUnderCanceledContext(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	file := &models.MediaFile{ID: 4030316, FilePath: "/media/movie.mkv", FileSize: 1}
	session, err := manager.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatal(err)
	}
	adminStore := &cancelSensitiveAdminStore{}
	h := NewStreamHandler(manager, testPlaybackFileResolver{file: file})
	h.AdminStore = adminStore

	h.abortPlaybackSession(canceledPlaybackContext(), session)

	if !adminStore.deleted[session.ID] {
		t.Fatalf("durable session delete did not complete under a canceled context (calls=%d)", adminStore.deleteCalls)
	}
	if _, err := manager.GetSession(session.ID); !errors.Is(err, playback.ErrSessionNotFound) {
		t.Fatalf("session still live after abort: err=%v", err)
	}
}

// The expired-attempt stop and its deny marker are durable teardown too. A
// canceled request context must not skip the attempt stop.
func TestMarkAttemptStoppedServerSideDetachesFromCanceledContext(t *testing.T) {
	store := &cancelAwarePlanStore{MemoryPlanStoreV3: playback.NewMemoryPlanStoreV3()}
	const sessionID = "669f2d8c-0000-4000-8000-000000000000"
	if err := store.SaveAttempt(context.Background(), playback.AttemptRecordV3{
		PlaybackAttemptID: "attempt-teardown",
		SessionID:         sessionID,
		ExpiresAt:         time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	markAttemptStoppedServerSide(canceledPlaybackContext(), store, nil, sessionID)

	if !store.stopCalled {
		t.Fatal("StopAttempt was never reached")
	}
	if store.stopCtxErr != nil {
		t.Fatalf("StopAttempt ran with a canceled context: %v", store.stopCtxErr)
	}
	record, err := store.GetAttempt(context.Background(), sessionID)
	if err != nil || record == nil || record.StoppedAt == nil {
		t.Fatalf("attempt was not stopped: record=%+v err=%v", record, err)
	}
}

// A completed teardown leaves no half-dead session: the live session, the
// synced-session row, and the durable attempt are all gone/stopped together, so
// a follow-up replan deterministically takes the terminal path instead of
// racing a locally-gone session against a live attempt.
func TestCompletedTeardownLeavesNoHalfDeadSession(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	store := &cancelAwarePlanStore{MemoryPlanStoreV3: playback.NewMemoryPlanStoreV3()}
	file := &models.MediaFile{ID: 88, FilePath: "/media/movie.mkv", FileSize: 1}
	session, err := manager.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAttempt(context.Background(), playback.AttemptRecordV3{
		PlaybackAttemptID: "attempt-complete",
		SessionID:         session.ID,
		ExpiresAt:         time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	adminStore := &cancelSensitiveAdminStore{}
	h := NewStreamHandler(manager, testPlaybackFileResolver{file: file})
	h.AdminStore = adminStore
	h.PlanStoreV3 = store

	h.abortPlaybackSession(canceledPlaybackContext(), session)

	if _, err := manager.GetSession(session.ID); !errors.Is(err, playback.ErrSessionNotFound) {
		t.Fatalf("live session survived a completed teardown: err=%v", err)
	}
	if !adminStore.deleted[session.ID] {
		t.Fatal("synced session row survived a completed teardown")
	}
	record, err := store.GetAttempt(context.Background(), session.ID)
	if err != nil || record == nil || record.StoppedAt == nil {
		t.Fatalf("durable attempt stayed live after a completed teardown: record=%+v err=%v", record, err)
	}
}

// A second teardown after the session is already gone is a no-op: the stop
// fails ErrSessionNotFound and the durable half does not run again.
func TestStreamAbortPlaybackSessionIsIdempotent(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	file := &models.MediaFile{ID: 77, FilePath: "/media/movie.mkv", FileSize: 1}
	session, err := manager.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatal(err)
	}
	adminStore := &cancelSensitiveAdminStore{}
	h := NewStreamHandler(manager, testPlaybackFileResolver{file: file})
	h.AdminStore = adminStore

	h.abortPlaybackSession(context.Background(), session)
	h.abortPlaybackSession(context.Background(), session)

	if adminStore.deleteCalls != 1 {
		t.Fatalf("durable delete ran %d times, want exactly 1", adminStore.deleteCalls)
	}
}

// A client-canceled file lookup is not a stop: the client still owns the
// session (a track switch aborts the stream and replans), so the handler must
// answer 204 and leave the session live and recoverable rather than stopping
// and denying it into a terminal 404/410.
func TestStreamCanceledFileLookupKeepsSessionLive(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	const fileID = 4030316
	session, err := manager.StartSession(1, "profile-1", fileID, playback.PlayRemux, false)
	if err != nil {
		t.Fatal(err)
	}
	h := NewStreamHandler(manager, errStreamFileResolver{err: context.Canceled})

	req := httptest.NewRequest(http.MethodGet, "/api/v2/stream/"+session.ID, nil).WithContext(canceledPlaybackContext())
	req = withPlaybackRouteParam(req, "session_id", session.ID)
	rec := httptest.NewRecorder()
	h.HandleStream(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	current, err := manager.GetSession(session.ID)
	if err != nil || current == nil {
		t.Fatalf("session was reaped on a client cancel: err=%v", err)
	}
	if !current.ClientCancelNamesCurrentRoute() {
		t.Fatalf("client cancel was not recorded on the live session: %+v", current.ClientCanceled)
	}
	if h.StreamDeny.Denied(context.Background(), session.ID) {
		t.Fatal("a client cancel wrote a terminal deny marker")
	}
}

// Cancel responsiveness of a live progressive remux is unchanged: a canceled
// request still closes the abort channel promptly.
func TestProgressiveRemuxAbortStillEndsOnRequestCancel(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", 5, playback.PlayRemux, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	abort, release := watchProgressiveRemuxAbort(ctx, manager, session.ID)
	defer release()

	cancel()
	select {
	case <-abort:
	case <-time.After(2 * time.Second):
		t.Fatal("abort did not close after the request context was canceled")
	}
}
