package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"

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

// cancelOnStopCommitProgressStore simulates a client that cancels its stop
// request at the instant the stop commit lands, and records the context each
// later durable step of the finalization saw. A memory store ignores the
// context, so a wrapper is the only way a test can tell a detached
// finalization from one that inherited the canceled request.
type cancelOnStopCommitProgressStore struct {
	*playback.MemoryPlanStoreV3
	cancel           context.CancelFunc
	stopCtxErr       error
	claimCtxErr      error
	receiptCtxErr    error
	receiptFinalized bool
}

func (s *cancelOnStopCommitProgressStore) StopAttempt(
	ctx context.Context, sessionID, stopID string, final *playback.ProgressSampleV3,
) (playback.StopReceiptV3, bool, error) {
	receipt, first, err := s.MemoryPlanStoreV3.StopAttempt(ctx, sessionID, stopID, final)
	if err == nil {
		s.stopCtxErr = ctx.Err()
		if first && s.cancel != nil {
			s.cancel()
		}
	}
	return receipt, first, err
}

func (s *cancelOnStopCommitProgressStore) ClaimStopFinalization(
	ctx context.Context, sessionID string, leaseUntil time.Time,
) (bool, error) {
	s.claimCtxErr = ctx.Err()
	return s.MemoryPlanStoreV3.ClaimStopFinalization(ctx, sessionID, leaseUntil)
}

func (s *cancelOnStopCommitProgressStore) RecordStopReceipt(
	ctx context.Context, sessionID string, receipt playback.StopReceiptV3,
) error {
	s.receiptCtxErr = ctx.Err()
	s.receiptFinalized = receipt.Finalized
	return s.MemoryPlanStoreV3.RecordStopReceipt(ctx, sessionID, receipt)
}

// cancelOnCleanupAdminStore cancels the client's request while the teardown
// cleanup runs, and still records the synced-session delete when the detached
// cleanup context is live.
type cancelOnCleanupAdminStore struct {
	cancel     context.CancelFunc
	canceledAt int
	deleted    map[string]bool
}

func (s *cancelOnCleanupAdminStore) RecordHistory(context.Context, AdminPlaybackHistoryEntry) error {
	return nil
}

func (s *cancelOnCleanupAdminStore) DeleteSession(ctx context.Context, sessionID string) error {
	if s.cancel != nil {
		s.cancel()
		s.canceledAt++
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.deleted == nil {
		s.deleted = make(map[string]bool)
	}
	s.deleted[sessionID] = true
	return nil
}

func newTeardownStopFixture(t *testing.T) (*playbackServiceFixture, *cancelOnStopCommitProgressStore, context.Context, context.CancelFunc) {
	t.Helper()
	f := newPlaybackServiceFixture(t)
	reqCtx, cancelReq := context.WithCancel(f.ctx)
	store := &cancelOnStopCommitProgressStore{
		MemoryPlanStoreV3: f.handler.PlanStoreV3.(*playback.MemoryPlanStoreV3),
		cancel:            cancelReq,
	}
	f.handler.PlanStoreV3 = store
	return f, store, reqCtx, cancelReq
}

// A client that cancels immediately after its stop commit must still get the
// full finalization: the claim, the cleanup, and the receipt write all run
// detached from the canceled request. Otherwise the stop stays unfinalized and
// a later replay redoes it.
func TestStopFinalizationDetachesClaimAndReceiptFromCanceledRequest(t *testing.T) {
	f, store, reqCtx, cancelReq := newTeardownStopFixture(t)
	defer cancelReq()
	stopID := uuid.NewString()
	view, err := f.handler.StopPlaybackV2(reqCtx, f.caller, f.session.ID, PlaybackStopCommand{StopID: stopID})
	if err != nil {
		t.Fatal(err)
	}
	if store.stopCtxErr != nil {
		t.Fatalf("stop commit ran on a canceled context: %v", store.stopCtxErr)
	}
	if store.claimCtxErr != nil {
		t.Fatalf("finalization claim ran on the canceled request context: %v", store.claimCtxErr)
	}
	if store.receiptCtxErr != nil {
		t.Fatalf("receipt write ran on the canceled request context: %v", store.receiptCtxErr)
	}
	if !store.receiptFinalized {
		t.Fatal("receipt was not finalized")
	}
	if view.Outcome != PlaybackOutcomeStopped {
		t.Fatalf("view = %+v", view)
	}
	if _, err := f.manager.GetSession(f.session.ID); !errors.Is(err, playback.ErrSessionNotFound) {
		t.Fatalf("cleanup did not tear the session down: %v", err)
	}
}

// A client that cancels while the detached cleanup is still running must not
// skip the receipt write: the receipt is persisted under a fresh detached
// bound, so the stop cannot be replayed as unfinished.
func TestStopFinalizationPersistsReceiptWhenClientCancelsDuringCleanup(t *testing.T) {
	f, store, reqCtx, cancelReq := newTeardownStopFixture(t)
	defer cancelReq()
	admin := &cancelOnCleanupAdminStore{cancel: cancelReq}
	f.handler.AdminStore = admin

	view, err := f.handler.StopPlaybackV2(reqCtx, f.caller, f.session.ID, PlaybackStopCommand{StopID: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	if admin.canceledAt == 0 {
		t.Fatal("precondition: cleanup never canceled the request")
	}
	if !admin.deleted[f.session.ID] {
		t.Fatal("cleanup did not delete the synced session")
	}
	if store.receiptCtxErr != nil {
		t.Fatalf("receipt write ran on the canceled request context: %v", store.receiptCtxErr)
	}
	if !store.receiptFinalized {
		t.Fatal("receipt was not finalized after a cancel during cleanup")
	}
	if view.Outcome != PlaybackOutcomeStopped {
		t.Fatalf("view = %+v", view)
	}
}

// The start-effect wait can run the whole playbackStartSideEffectsTimeoutV3.
// The history write and synced-session delete must still run on a durable
// budget that starts once the effect has settled, not one that expired while
// waiting for it.
func TestStopTeardownDurableWritesSurviveSlowStartEffect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := playback.NewSessionManager(0, 0)
		file := &models.MediaFile{ID: 91, FilePath: "/media/movie.mkv", FileSize: 1}
		session, err := manager.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
		if err != nil {
			t.Fatal(err)
		}
		adminStore := &cancelSensitiveAdminStore{}
		h := NewPlaybackHandler(manager, testPlaybackFileResolver{file: file})
		h.AdminStore = adminStore
		// A start effect that has begun but never settles until canceled: the
		// teardown's wait blocks the full timeout before the durable writes.
		state := &playbackStartSideEffectsStateV3{
			ctx:     context.Background(),
			cancel:  func() {},
			done:    make(chan struct{}),
			started: true,
		}
		h.v3StartEffectsPending = map[string]*playbackStartSideEffectsStateV3{session.ID: state}

		h.finalizeSessionStopWithResult(context.Background(), session, false, "", true)

		if !adminStore.deleted[session.ID] {
			t.Fatalf("durable session delete did not run after a slow start effect (calls=%d)", adminStore.deleteCalls)
		}
	})
}

// leaseObservingStore counts the finalization claims won and the receipts that
// actually reached the store, so a test can prove exactly one finalization ran
// even when a replay races the winner.
type leaseObservingStore struct {
	*playback.MemoryPlanStoreV3
	mu              sync.Mutex
	claimsWon       int
	finalizedWrites int
}

func (s *leaseObservingStore) ClaimStopFinalization(ctx context.Context, sessionID string, leaseUntil time.Time) (bool, error) {
	won, err := s.MemoryPlanStoreV3.ClaimStopFinalization(ctx, sessionID, leaseUntil)
	if won {
		s.mu.Lock()
		s.claimsWon++
		s.mu.Unlock()
	}
	return won, err
}

func (s *leaseObservingStore) RecordStopReceipt(ctx context.Context, sessionID string, receipt playback.StopReceiptV3) error {
	err := s.MemoryPlanStoreV3.RecordStopReceipt(ctx, sessionID, receipt)
	if err == nil && receipt.Finalized {
		s.mu.Lock()
		s.finalizedWrites++
		s.mu.Unlock()
	}
	return err
}

// wedgedCleanupAdminStore blocks the synced-session delete until its teardown
// context ends, the way a slow or wedged durable store would, and signals when
// it has entered the block.
type wedgedCleanupAdminStore struct {
	entered     chan struct{}
	enteredOnce sync.Once
}

func (s *wedgedCleanupAdminStore) RecordHistory(context.Context, AdminPlaybackHistoryEntry) error {
	return nil
}

func (s *wedgedCleanupAdminStore) DeleteSession(ctx context.Context, sessionID string) error {
	s.enteredOnce.Do(func() { close(s.entered) })
	<-ctx.Done()
	return ctx.Err()
}

// A claimed finalization must finish inside its single end-to-end budget, below
// the lease, even when a start effect holds the wait open and the cleanup then
// wedges. Nested fresh budgets used to stack past the lease, letting a
// concurrent replay reclaim the claim while the winner was still writing; here
// the replay must stay a replay and the winner must finalize once, in budget.
func TestStopFinalizationSlowCleanupStaysInsideLease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newPlaybackServiceFixture(t)
		store := &leaseObservingStore{MemoryPlanStoreV3: f.handler.PlanStoreV3.(*playback.MemoryPlanStoreV3)}
		f.handler.PlanStoreV3 = store
		admin := &wedgedCleanupAdminStore{entered: make(chan struct{})}
		f.handler.AdminStore = admin

		// A start effect that holds the cancel-wait open for 8s, so the durable
		// cleanup only starts after the wait releases and must then finish inside
		// the same budget rather than a fresh one.
		state := &playbackStartSideEffectsStateV3{
			ctx:     context.Background(),
			cancel:  func() {},
			done:    make(chan struct{}),
			started: true,
		}
		f.handler.v3StartEffectsPending = map[string]*playbackStartSideEffectsStateV3{f.session.ID: state}
		go func() {
			time.Sleep(8 * time.Second)
			close(state.done)
		}()

		type stopResult struct {
			view PlaybackMutationView
			err  error
		}
		start := time.Now()
		firstDone := make(chan stopResult, 1)
		go func() {
			view, err := f.handler.StopPlaybackV2(newAuthorizedPlaybackContext(), f.caller, f.session.ID, PlaybackStopCommand{StopID: uuid.NewString()})
			firstDone <- stopResult{view, err}
		}()
		// Only start the racing replay once the winner is inside its wedged
		// cleanup, so the two finalizations overlap for real.
		<-admin.entered
		replayDone := make(chan stopResult, 1)
		go func() {
			view, err := f.handler.StopPlaybackV2(newAuthorizedPlaybackContext(), f.caller, f.session.ID, PlaybackStopCommand{StopID: uuid.NewString()})
			replayDone <- stopResult{view, err}
		}()

		first := <-firstDone
		replay := <-replayDone
		elapsed := time.Since(start)

		if first.err != nil {
			t.Fatalf("winner stop failed: %v", first.err)
		}
		if first.view.Outcome != PlaybackOutcomeStopped {
			t.Fatalf("winner outcome = %v, want stopped", first.view.Outcome)
		}
		if replay.err != nil {
			t.Fatalf("replay stop failed: %v", replay.err)
		}
		if replay.view.Outcome != PlaybackOutcomeReplayed {
			t.Fatalf("replay outcome = %v, want replayed", replay.view.Outcome)
		}
		store.mu.Lock()
		claims, writes := store.claimsWon, store.finalizedWrites
		store.mu.Unlock()
		if claims != 1 {
			t.Fatalf("finalization claimed %d times, want exactly 1: a replay must not take over a live lease", claims)
		}
		if writes != 1 {
			t.Fatalf("receipt finalized %d times, want exactly 1: writers interleaved", writes)
		}
		if elapsed > stopFinalizationBudget {
			t.Fatalf("finalization ran %v, past the %v end-to-end budget", elapsed, stopFinalizationBudget)
		}
		if elapsed >= stopFinalizationLease {
			t.Fatalf("finalization ran %v, not safely inside the %v lease", elapsed, stopFinalizationLease)
		}
		if _, err := f.manager.GetSession(f.session.ID); !errors.Is(err, playback.ErrSessionNotFound) {
			t.Fatalf("winner did not tear the session down: %v", err)
		}
	})
}

// blockingPollStore blocks the store call the replay polling loop makes, and
// records that it was entered. It embeds the memory store so it still satisfies
// every plan/progress method.
type blockingPollStore struct {
	*playback.MemoryPlanStoreV3
	mu          sync.Mutex
	stopCalls   int
	blockAfter  int
	blocked     chan struct{}
	enteredOnce sync.Once
}

func (s *blockingPollStore) StopAttempt(
	ctx context.Context, sessionID, stopID string, final *playback.ProgressSampleV3,
) (playback.StopReceiptV3, bool, error) {
	s.mu.Lock()
	s.stopCalls++
	call := s.stopCalls
	s.mu.Unlock()
	if s.blockAfter > 0 && call > s.blockAfter {
		s.enteredOnce.Do(func() { close(s.blocked) })
		<-ctx.Done()
		return playback.StopReceiptV3{}, false, ctx.Err()
	}
	return s.MemoryPlanStoreV3.StopAttempt(ctx, sessionID, stopID, final)
}

// A replay that loses the claim polls the stored receipt. If the request
// context is still live, a blocked store call must not outrun the detached
// finalization deadline: the poll is bounded by both. Without the finalization
// bound the call would hang on the never-canceled request until the test
// deadlocks.
func TestStopReplayPollingIsBoundedByFinalizationDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newPlaybackServiceFixture(t)
		base := f.handler.PlanStoreV3.(*playback.MemoryPlanStoreV3)
		if _, _, err := base.StopAttempt(context.Background(), f.session.ID, uuid.NewString(), nil); err != nil {
			t.Fatal(err)
		}
		if won, err := base.ClaimStopFinalization(context.Background(), f.session.ID, time.Now().Add(time.Minute)); err != nil || !won {
			t.Fatalf("seed claim: %v %v", won, err)
		}
		store := &blockingPollStore{MemoryPlanStoreV3: base, blockAfter: 1, blocked: make(chan struct{})}
		f.handler.PlanStoreV3 = store

		start := time.Now()
		view, err := f.handler.StopPlaybackV2(f.ctx, f.caller, f.session.ID, PlaybackStopCommand{StopID: uuid.NewString()})
		elapsed := time.Since(start)

		if err != nil || view.Outcome != PlaybackOutcomeReplayed {
			t.Fatalf("replay = %+v, %v", view, err)
		}
		select {
		case <-store.blocked:
		default:
			t.Fatal("precondition: the replay poll never entered the blocked store call")
		}
		if elapsed > stopFinalizationBudget {
			t.Fatalf("replay poll ran %v, past the %v finalization deadline", elapsed, stopFinalizationBudget)
		}
		if elapsed >= stopFinalizationLease {
			t.Fatalf("replay poll ran %v, not inside the %v lease", elapsed, stopFinalizationLease)
		}
		if _, err := f.manager.GetSession(f.session.ID); err != nil {
			t.Fatalf("a replay that lost the claim must not tear the session down: %v", err)
		}
	})
}
