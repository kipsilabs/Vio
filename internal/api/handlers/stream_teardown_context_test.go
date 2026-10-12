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

// expiredDeadlinePlaybackContext is a request whose deadline has already passed:
// the timeout case, where the request is done but the teardown it triggered must
// still run. The context records a past deadline; detachedWithin must ignore it.
func expiredDeadlinePlaybackContext() context.Context {
	ctx, cancel := context.WithDeadline(newAuthorizedPlaybackContext(), time.Now().Add(-time.Second))
	cancel()
	return ctx
}

// detachedWithin must not inherit an already-expired request deadline: doing so
// would hand a timeout-triggered teardown a context that is canceled at birth.
// It takes a fresh budget instead. Only an enclosing detached budget narrows it.
func TestDetachedWithinIgnoresExpiredRequestDeadline(t *testing.T) {
	ctx, cancel := detachedWithin(expiredDeadlinePlaybackContext(), time.Minute)
	defer cancel()
	if err := ctx.Err(); err != nil {
		t.Fatalf("detached context was already done: %v", err)
	}
	deadline, ok := ctx.Deadline()
	if !ok || !deadline.After(time.Now()) {
		t.Fatalf("deadline = %v, ok = %v, want a fresh future budget", deadline, ok)
	}
}

// A detach nested inside another detached budget is clamped to it, so a v2 stop
// finalization's nested teardowns cannot reset a fresh full timeout and outlive
// the lease that fences the finalization.
func TestDetachedWithinClampsToEnclosingDetachedBudget(t *testing.T) {
	outer, cancelOuter := detachedWithin(context.Background(), 50*time.Millisecond)
	defer cancelOuter()
	outerDeadline, ok := outer.Deadline()
	if !ok {
		t.Fatal("outer detached context has no deadline")
	}

	inner, cancelInner := detachedWithin(outer, time.Hour)
	defer cancelInner()
	innerDeadline, ok := inner.Deadline()
	if !ok {
		t.Fatal("inner detached context has no deadline")
	}
	if !innerDeadline.Equal(outerDeadline) {
		t.Fatalf("nested deadline = %v, want the enclosing %v", innerDeadline, outerDeadline)
	}
}

// An already-expired request deadline is the common timeout case, not just the
// canceled case above. The teardown it triggers must still complete its durable
// writes on a fresh budget; otherwise the synced-session delete runs on an
// already-canceled context and the session is stranded.
func TestStreamAbortTeardownCompletesUnderExpiredRequestDeadline(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	file := &models.MediaFile{ID: 4030317, FilePath: "/media/movie.mkv", FileSize: 1}
	session, err := manager.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatal(err)
	}
	adminStore := &cancelSensitiveAdminStore{}
	h := NewStreamHandler(manager, testPlaybackFileResolver{file: file})
	h.AdminStore = adminStore

	h.abortPlaybackSession(expiredDeadlinePlaybackContext(), session)

	if !adminStore.deleted[session.ID] {
		t.Fatalf("durable session delete did not complete under an expired deadline (calls=%d)", adminStore.deleteCalls)
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

		_, _ = h.finalizeSessionStopWithResult(context.Background(), session, false, "", true, "")

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

// stagedCleanupAdminStore models the synced-session delete of the stop cleanup.
// While failing it blocks until its teardown context ends and returns that
// error, standing in for a wedged durable store; otherwise it succeeds after an
// optional delay and records the delete. It signals when the delete was first
// entered.
type stagedCleanupAdminStore struct {
	failing     bool
	delay       time.Duration
	entered     chan struct{}
	enteredOnce sync.Once
	mu          sync.Mutex
	deleted     map[string]bool
}

func (s *stagedCleanupAdminStore) RecordHistory(context.Context, AdminPlaybackHistoryEntry) error {
	return nil
}

func (s *stagedCleanupAdminStore) DeleteSession(ctx context.Context, sessionID string) error {
	s.enteredOnce.Do(func() { close(s.entered) })
	s.mu.Lock()
	failing, delay := s.failing, s.delay
	s.mu.Unlock()
	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if failing {
		<-ctx.Done()
		return ctx.Err()
	}
	s.mu.Lock()
	if s.deleted == nil {
		s.deleted = make(map[string]bool)
	}
	s.deleted[sessionID] = true
	s.mu.Unlock()
	return nil
}

func (s *stagedCleanupAdminStore) repair() {
	s.mu.Lock()
	s.failing = false
	s.mu.Unlock()
}

func (s *stagedCleanupAdminStore) deletedSession(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleted[sessionID]
}

// A claimed finalization must finish inside its single end-to-end budget, below
// the lease, even when a start effect holds the wait open and the cleanup is
// slow. Nested fresh budgets used to stack past the lease, letting a concurrent
// replay reclaim the claim while the winner was still writing; here the replay
// must stay a replay and the winner must finalize once, in budget.
func TestStopFinalizationSlowCleanupStaysInsideLease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newPlaybackServiceFixture(t)
		store := &leaseObservingStore{MemoryPlanStoreV3: f.handler.PlanStoreV3.(*playback.MemoryPlanStoreV3)}
		f.handler.PlanStoreV3 = store
		admin := &stagedCleanupAdminStore{delay: 5 * time.Second, entered: make(chan struct{})}
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
		// Only start the racing replay once the winner is inside its slow
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

// A cleanup that fails must not finalize the stop. The receipt stays
// unfinalized and its claim is released, so a replay re-runs the idempotent
// writers and repairs the rows the first attempt stranded instead of the stop
// being pinned complete over a still-present synced session.
func TestStopFinalizationFailedCleanupStaysReplayable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newPlaybackServiceFixture(t)
		store := &leaseObservingStore{MemoryPlanStoreV3: f.handler.PlanStoreV3.(*playback.MemoryPlanStoreV3)}
		f.handler.PlanStoreV3 = store
		admin := &stagedCleanupAdminStore{failing: true, entered: make(chan struct{})}
		f.handler.AdminStore = admin

		start := time.Now()
		first, err := f.handler.StopPlaybackV2(f.ctx, f.caller, f.session.ID, PlaybackStopCommand{StopID: uuid.NewString()})
		elapsed := time.Since(start)
		if err != nil || first.Outcome != PlaybackOutcomeStopped {
			t.Fatalf("winner stop = %+v, %v", first, err)
		}
		if elapsed > stopFinalizationBudget {
			t.Fatalf("failed cleanup ran %v, past the %v end-to-end budget", elapsed, stopFinalizationBudget)
		}
		select {
		case <-admin.entered:
		default:
			t.Fatal("precondition: cleanup never reached the synced-session delete")
		}
		store.mu.Lock()
		claims, writes := store.claimsWon, store.finalizedWrites
		store.mu.Unlock()
		if claims != 1 || writes != 0 {
			t.Fatalf("failed cleanup claimed %d, finalized %d, want 1 and 0", claims, writes)
		}
		if receipt, _, _ := store.StopAttempt(context.Background(), f.session.ID, uuid.NewString(), nil); receipt.Finalized {
			t.Fatalf("a failed cleanup finalized the receipt: %+v", receipt)
		}
		if admin.deletedSession(f.session.ID) {
			t.Fatal("a failed cleanup reported the synced session deleted")
		}

		// Repair the durable store: the replay must pick the released claim back
		// up and finish the cleanup.
		admin.repair()
		second, err := f.handler.StopPlaybackV2(f.ctx, f.caller, f.session.ID, PlaybackStopCommand{StopID: uuid.NewString()})
		if err != nil || second.Outcome != PlaybackOutcomeReplayed {
			t.Fatalf("repairing replay = %+v, %v", second, err)
		}
		if !admin.deletedSession(f.session.ID) {
			t.Fatal("the replay did not delete the stranded synced session")
		}
		store.mu.Lock()
		claims, writes = store.claimsWon, store.finalizedWrites
		store.mu.Unlock()
		if claims != 2 || writes != 1 {
			t.Fatalf("after repair claimed %d, finalized %d, want 2 and 1", claims, writes)
		}
		if receipt, _, _ := store.StopAttempt(context.Background(), f.session.ID, uuid.NewString(), nil); !receipt.Finalized {
			t.Fatalf("the repaired stop is not finalized: %+v", receipt)
		}
	})
}

// The stop finalization waits for the per-session lifecycle lock a replacement
// preparation may hold. That wait is bounded by the finalization's own budget,
// not by the request that triggered it: an already-expired request deadline must
// not become the teardown's deadline, or a timeout-triggered stop would skip its
// durable writes entirely. A finalizer blocked past its lease is a different
// problem, and the budget stays below the lease so it cannot start teardown after
// another replica took the stop over. Here the holder outlasts the budget, so the
// wait ends on the budget and nothing is torn down or finalized.
func TestStopFinalizationLifecycleWaitBoundedByBudgetNotRequestDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newPlaybackServiceFixture(t)
		store := &leaseObservingStore{MemoryPlanStoreV3: f.handler.PlanStoreV3.(*playback.MemoryPlanStoreV3)}
		f.handler.PlanStoreV3 = store
		admin := &cancelSensitiveAdminStore{}
		f.handler.AdminStore = admin

		// The replacement holds the lifecycle lock past the finalization budget.
		unlock := f.handler.tm.LockSessionLifecycle(f.session.ID)
		holderDone := make(chan struct{})
		go func() {
			time.Sleep(25 * time.Second)
			unlock()
			close(holderDone)
		}()

		// The request is already past its deadline: the teardown must take its
		// own budget rather than inherit the expired one.
		start := time.Now()
		view, err := f.handler.StopPlaybackV2(expiredDeadlinePlaybackContext(), f.caller, f.session.ID, PlaybackStopCommand{StopID: uuid.NewString()})
		elapsed := time.Since(start)
		if err != nil || view.Outcome != PlaybackOutcomeStopped {
			t.Fatalf("stop = %+v, %v", view, err)
		}
		if elapsed < 10*time.Second {
			t.Fatalf("stop waited only %v for the lifecycle lock, so it inherited the expired request deadline", elapsed)
		}
		if elapsed > stopFinalizationBudget {
			t.Fatalf("stop waited %v for the lifecycle lock, past the %v finalization budget", elapsed, stopFinalizationBudget)
		}
		if elapsed >= stopFinalizationLease {
			t.Fatalf("stop ran %v, not inside the %v lease", elapsed, stopFinalizationLease)
		}
		if _, err := f.manager.GetSession(f.session.ID); err != nil {
			t.Fatalf("an expired teardown ran once the lock was free: %v", err)
		}
		if admin.deleteCalls != 0 {
			t.Fatalf("expired teardown deleted the synced session %d times", admin.deleteCalls)
		}
		if receipt, _, _ := store.StopAttempt(context.Background(), f.session.ID, uuid.NewString(), nil); receipt.Finalized {
			t.Fatalf("expired finalization marked the stop complete: %+v", receipt)
		}
		<-holderDone
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

// failFinalizedReceiptOnce drops the first finalized receipt write, standing in
// for a crash between the watch-history insert and RecordStopReceipt. The durable
// attempt row keeps the stop id, so the replay must derive the same history row
// rather than mint a second one.
type failFinalizedReceiptOnce struct {
	*playback.MemoryPlanStoreV3
	mu     sync.Mutex
	failed bool
}

func (s *failFinalizedReceiptOnce) RecordStopReceipt(ctx context.Context, sessionID string, receipt playback.StopReceiptV3) error {
	s.mu.Lock()
	fail := receipt.Finalized && !s.failed
	if fail {
		s.failed = true
	}
	s.mu.Unlock()
	if fail {
		return errors.New("simulated crash before the receipt write")
	}
	return s.MemoryPlanStoreV3.RecordStopReceipt(ctx, sessionID, receipt)
}

// A crash between the watch-history insert and the receipt write must not let a
// replay mint a second history row for one stop. The history key is derived from
// the durable stop id before the insert, so the replay derives the same row and
// AddVisibleHistory reports it already exists instead of duplicating the play.
func TestStopReplayAfterLostReceiptReusesHistoryRow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newPlaybackServiceFixture(t)
		userStore := newPlaybackTestStore(t)
		f.handler.StoreProvider = testUserStoreProvider{store: userStore}
		store := &failFinalizedReceiptOnce{MemoryPlanStoreV3: f.handler.PlanStoreV3.(*playback.MemoryPlanStoreV3)}
		f.handler.PlanStoreV3 = store

		final := 120.0
		first, err := f.handler.StopPlaybackV2(f.ctx, f.caller, f.session.ID, PlaybackStopCommand{StopID: uuid.NewString(), Sequence: 1, Position: &final})
		if err != nil || first.Outcome != PlaybackOutcomeStopped {
			t.Fatalf("first stop = %+v, %v", first, err)
		}
		if first.HistoryID == "" {
			t.Fatal("first stop recorded no history id")
		}
		history, err := userStore.ListHistory(context.Background(), "profile-1", 10, 0)
		if err != nil {
			t.Fatalf("ListHistory: %v", err)
		}
		if len(history) != 1 {
			t.Fatalf("history after the lost receipt = %d rows, want 1", len(history))
		}

		// A crash left the finalization claim behind; past its lease a replay may
		// reclaim it and must reuse the same history row.
		time.Sleep(stopFinalizationLease + time.Second)
		second, err := f.handler.StopPlaybackV2(f.ctx, f.caller, f.session.ID, PlaybackStopCommand{StopID: uuid.NewString()})
		if err != nil || second.Outcome != PlaybackOutcomeReplayed {
			t.Fatalf("replayed stop = %+v, %v", second, err)
		}
		if second.HistoryID != first.HistoryID {
			t.Fatalf("replay history id = %q, want %q: the stop re-minted the history row", second.HistoryID, first.HistoryID)
		}
		history, err = userStore.ListHistory(context.Background(), "profile-1", 10, 0)
		if err != nil {
			t.Fatalf("ListHistory: %v", err)
		}
		if len(history) != 1 {
			t.Fatalf("history after replay = %d rows, want 1: a replay duplicated the stop", len(history))
		}
	})
}
