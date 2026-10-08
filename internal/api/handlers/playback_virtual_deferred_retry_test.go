package handlers

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/playback"
)

// failThenRecoverPlanStoreV3 fails GetAttempt a bounded number of times, then
// delegates to the healthy store. It models a one-shot plan-store outage on the
// deferred publish path.
type failThenRecoverPlanStoreV3 struct {
	playback.PlanStoreV3
	remaining atomic.Int64
}

func (s *failThenRecoverPlanStoreV3) GetAttempt(ctx context.Context, sessionID string) (*playback.AttemptRecordV3, error) {
	if s.remaining.Load() > 0 {
		s.remaining.Add(-1)
		return nil, errors.New("plan store unavailable")
	}
	return s.PlanStoreV3.GetAttempt(ctx, sessionID)
}

// TestDeferredProbeRetryablePublishReParksUntilDelivered is the #2 regression:
// a plan-store read failure while publishing a terminal failure notification must
// not be acknowledged as delivered. The notification is re-parked with a bounded
// backoff and the delivered watermark stays put, so the dispatcher retries it on
// its own; once the store recovers the terminal push lands with no reconnect or
// poll from the client. Without this the ack advanced the watermark on a
// suppressed push and a push-only client stayed loading forever.
func TestDeferredProbeRetryablePublishReParksUntilDelivered(t *testing.T) {
	fx := newDeferredLifecycleFixture(t, 933, "virtual://movie/tt-retryable?result=cand-1")
	fx.saveAttempt(t)
	conn := fx.registerPush(t)

	healthy := fx.handler.PlanStoreV3
	// Fail the first reads, then recover: the worker's first attempt must not
	// deliver or ack, and its retry must succeed.
	failing := &failThenRecoverPlanStoreV3{PlanStoreV3: healthy}
	failing.remaining.Store(1)
	fx.handler.PlanStoreV3 = failing
	t.Cleanup(func() { fx.handler.PlanStoreV3 = healthy })

	deferred := fx.job("virtual://movie/tt-retryable?result=cand-1")
	if !fx.handler.armDeferredProbePending(context.Background(), deferred) {
		t.Fatal("admission rejected the retryable job as stale")
	}
	generation := deferred.bindingGeneration

	// Hold the gate so the terminal notification parks, then release it and let
	// the dispatcher deliver. The first delivery attempt sees the failing store.
	release := holdDetachedGate(t, fx.handler)
	fx.handler.shedDeferredVirtualProbe(deferred)

	// The watermark must not advance on the suppressed push. Wait until the
	// first publish attempt has actually consulted the failing store and cleared
	// its in-flight marker, then assert the notification is still owed.
	release()
	waitDeferredCondition(t, func() bool {
		fx.handler.deferredPublishMu.Lock()
		_, active := fx.handler.deferredPublishInFlight[fx.session.ID]
		fx.handler.deferredPublishMu.Unlock()
		return !active && failing.remaining.Load() == 0
	}, "first publish attempt recorded")

	notified, err := fx.manager.VirtualProbeNotifiedGeneration(fx.session.ID)
	if err != nil {
		t.Fatalf("VirtualProbeNotifiedGeneration: %v", err)
	}
	if notified >= generation {
		t.Fatalf("watermark advanced to %d on a suppressed terminal push; want it still owed", notified)
	}

	// The dispatcher retries on its own; no reconnect and no poll. Observable
	// state: the terminal push arrives and the watermark catches up.
	select {
	case event := <-conn.ch:
		if event.payload.InventoryStatus != string(ProbeProvenanceFailed) {
			t.Fatalf("retried push status = %q, want failed", event.payload.InventoryStatus)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("terminal push was never delivered after the store recovered; a push-only client would stay loading")
	}
	waitDeferredCondition(t, func() bool {
		got, err := fx.manager.VirtualProbeNotifiedGeneration(fx.session.ID)
		return err == nil && got == generation
	}, "watermark caught up after the successful retry")
}

// TestDeferredProbeAdmissionRefusesCompletedLifecycle is the #3 regression: a
// duplicate start that adopted the concurrent winner's session must not re-arm
// and re-probe a lifecycle the winner already completed. Admission is
// idempotent per lifecycle — it arms pending only from the empty outcome — so a
// collision after the winner reached failed or verified is refused.
func TestDeferredProbeAdmissionRefusesCompletedLifecycle(t *testing.T) {
	for _, completed := range []string{probeOutcomeFailed, probeOutcomeVerified} {
		t.Run(completed, func(t *testing.T) {
			fx := newDeferredLifecycleFixture(t, 934, "virtual://movie/tt-completed?result=cand-1")
			if err := fx.manager.SetVirtualProbeOutcome(fx.session.ID, completed); err != nil {
				t.Fatalf("SetVirtualProbeOutcome: %v", err)
			}
			deferred := fx.job("virtual://movie/tt-completed?result=cand-1")
			if fx.handler.armDeferredProbePending(context.Background(), deferred) {
				t.Fatal("admission re-armed a completed lifecycle")
			}
			live, err := fx.manager.GetSession(fx.session.ID)
			if err != nil {
				t.Fatalf("GetSession: %v", err)
			}
			if live.VirtualProbeOutcome != completed {
				t.Fatalf("outcome = %q, want the winner's terminal %q", live.VirtualProbeOutcome, completed)
			}
			// The job must not have been admitted into the worker pool either.
			fx.handler.enqueueDeferredVirtualProbeV3(context.Background(), deferred)
			select {
			case <-fx.handler.deferredProbeQueue:
				t.Fatal("a duplicate probe for a completed lifecycle was queued")
			default:
			}
		})
	}

	t.Run("fresh binding is armed", func(t *testing.T) {
		fx := newDeferredLifecycleFixture(t, 935, "virtual://movie/tt-fresh?result=cand-1")
		deferred := fx.job("virtual://movie/tt-fresh?result=cand-1")
		if !fx.handler.armDeferredProbePending(context.Background(), deferred) {
			t.Fatal("admission rejected a fresh binding")
		}
		live, err := fx.manager.GetSession(fx.session.ID)
		if err != nil {
			t.Fatalf("GetSession: %v", err)
		}
		if live.VirtualProbeOutcome != probeOutcomePending {
			t.Fatalf("fresh binding outcome = %q, want pending", live.VirtualProbeOutcome)
		}
	})

	t.Run("a second admission on the armed lifecycle is refused", func(t *testing.T) {
		fx := newDeferredLifecycleFixture(t, 936, "virtual://movie/tt-armed?result=cand-1")
		first := fx.job("virtual://movie/tt-armed?result=cand-1")
		if !fx.handler.armDeferredProbePending(context.Background(), first) {
			t.Fatal("first admission rejected a fresh binding")
		}
		duplicate := fx.job("virtual://movie/tt-armed?result=cand-1")
		if fx.handler.armDeferredProbePending(context.Background(), duplicate) {
			t.Fatal("a duplicate admission re-armed an already-pending lifecycle")
		}
	})
}

// TestDeferredProbeDirectPathRetryableWithoutPark drives the unsaturated publish
// path: the gate has a free slot, so publishDeferredProbeInventory never parks
// and never initializes the pending map or starts the dispatcher. A one-shot
// publication failure on that path must not panic on a nil-map write, and the
// retry must still be delivered with no reconnect and no poll. The existing
// retry regression saturates the gate first, which both initializes the map and
// starts the dispatcher, so it masked both bugs.
func TestDeferredProbeDirectPathRetryableWithoutPark(t *testing.T) {
	fx := newDeferredLifecycleFixture(t, 940, "virtual://movie/tt-direct-retry?result=cand-1")
	fx.saveAttempt(t)
	conn := fx.registerPush(t)

	healthy := fx.handler.PlanStoreV3
	failing := &failThenRecoverPlanStoreV3{PlanStoreV3: healthy}
	failing.remaining.Store(1)
	fx.handler.PlanStoreV3 = failing
	t.Cleanup(func() { fx.handler.PlanStoreV3 = healthy })

	deferred := fx.job("virtual://movie/tt-direct-retry?result=cand-1")
	if !fx.handler.armDeferredProbePending(context.Background(), deferred) {
		t.Fatal("admission rejected the direct-path job as stale")
	}
	generation := deferred.bindingGeneration

	// No gate saturation: publishDeferredProbeInventory takes the free slot and
	// runs the worker directly. Assert the direct path was taken — the pending
	// map must still be nil before the attempt, so a naive retry write would
	// panic instead of parking.
	fx.handler.deferredPublishMu.Lock()
	pendingNil := fx.handler.deferredPublishPending == nil
	fx.handler.deferredPublishMu.Unlock()
	if !pendingNil {
		t.Fatal("pending map was already initialized; the direct path was not exercised")
	}

	fx.handler.shedDeferredVirtualProbe(deferred)

	// The retryable attempt must re-park and the dispatcher must be started by
	// the worker completion, not by a park. Observable: the terminal push lands.
	select {
	case event := <-conn.ch:
		if event.payload.InventoryStatus != string(ProbeProvenanceFailed) {
			t.Fatalf("direct-path retried push status = %q, want failed", event.payload.InventoryStatus)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("direct-path retryable publish was never delivered; the dispatcher was not started")
	}
	waitDeferredCondition(t, func() bool {
		got, err := fx.manager.VirtualProbeNotifiedGeneration(fx.session.ID)
		return err == nil && got == generation
	}, "watermark caught up on the direct path")
}

// TestDeferredProbeOverflowProgressesDespiteRetryBackoff is the combined
// overflow + delayed-retry regression: one parked entry waits out its retryable
// backoff while a separate terminal session overflows and owes a rescan. The
// delayed entry must neither spin the drain (a "more work" return with nothing
// dispatchable) nor stall the overflow recovery: the rescan must still re-park
// and deliver the overflowed session, and the drain must return "sleep" once
// only the backoff entry remains.
func TestDeferredProbeOverflowProgressesDespiteRetryBackoff(t *testing.T) {
	fx := newDeferredLifecycleFixture(t, 941, "virtual://movie/tt-overflow-retry?result=cand-1")
	fx.saveAttempt(t)
	conn := fx.registerPush(t)

	// The fixture's own session reaches a terminal outcome but is not parked:
	// it is the overflow the rescan must recover.
	if err := fx.manager.SetVirtualProbeOutcome(fx.session.ID, probeOutcomeFailed); err != nil {
		t.Fatalf("SetVirtualProbeOutcome: %v", err)
	}
	targetGen, err := fx.manager.VirtualSourceGeneration(fx.session.ID)
	if err != nil {
		t.Fatalf("VirtualSourceGeneration: %v", err)
	}

	// A second, unrelated session is parked with a retryable backoff that has
	// not elapsed. Its map entry must survive the batch and must not be treated
	// as dispatchable work.
	const backoffSession = "session-in-retry-backoff"
	fx.handler.deferredPublishMu.Lock()
	fx.handler.deferredPublishPending = map[string]deferredPublishIntent{
		backoffSession: {fileID: 999, generation: 3, attempts: 1, notBefore: time.Now().Add(time.Minute)},
	}
	fx.handler.deferredPublishRescan = true
	fx.handler.deferredPublishMu.Unlock()

	// The drain recovers the overflow despite the delayed retry, delivers it,
	// and then reports sleep (false) because only the backoff entry is left.
	if more := fx.handler.drainDeferredPublishBatch(fx.handler.ServiceContext); more {
		t.Fatal("drain requested another immediate pass with only a backoff entry left")
	}
	select {
	case event := <-conn.ch:
		if event.payload.InventoryStatus != string(ProbeProvenanceFailed) {
			t.Fatalf("overflow push status = %q, want failed", event.payload.InventoryStatus)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("overflow recovery stalled behind the retryable backoff entry")
	}
	waitDeferredCondition(t, func() bool {
		got, err := fx.manager.VirtualProbeNotifiedGeneration(fx.session.ID)
		return err == nil && got == targetGen
	}, "overflow session acked after rescan")

	fx.handler.deferredPublishMu.Lock()
	backoff, parked := fx.handler.deferredPublishPending[backoffSession]
	fx.handler.deferredPublishMu.Unlock()
	if !parked {
		t.Fatal("the backoff entry was dropped; it must stay parked until its delay elapses")
	}
	if backoff.generation != 3 {
		t.Fatalf("backoff entry generation = %d, want it unchanged at 3", backoff.generation)
	}
}

// TestDeferredPublishRetryInsertHonorsPendingBound proves the retry path cannot
// grow the pending map past its bound: a retryable attempt for a session with no
// existing entry while the map is full must not insert, and must retain the
// intent through the overflow rescan flag instead. Without this the retry insert
// was an unbounded second writer of the map.
func TestDeferredPublishRetryInsertHonorsPendingBound(t *testing.T) {
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	h.deferredPublishPending = make(map[string]deferredPublishIntent, virtualDeferredPublishPendingCap)
	for i := 0; i < virtualDeferredPublishPendingCap; i++ {
		h.deferredPublishPending[fmt.Sprintf("filler-%d", i)] = deferredPublishIntent{fileID: i + 1, generation: 1}
	}

	intent := deferredPublishIntent{fileID: 4242, generation: 2}
	h.deferredPublishMu.Lock()
	h.reparkDeferredPublishRetryLocked("new-session", intent)
	h.deferredPublishMu.Unlock()

	if _, exists := h.deferredPublishPending["new-session"]; exists {
		t.Fatal("retry insert grew the pending map past its bound")
	}
	if len(h.deferredPublishPending) != virtualDeferredPublishPendingCap {
		t.Fatalf("pending map size = %d, want the bound %d", len(h.deferredPublishPending), virtualDeferredPublishPendingCap)
	}
	if !h.deferredPublishRescan {
		t.Fatal("retry insert at the bound did not set the overflow rescan flag")
	}
}

// TestDeferredPublishFailedAcquisitionReparkHonorsPendingBound is the regression
// for the failed-acquisition repark bypassing the pending cap. Selection deletes
// the winner from the map BEFORE the gate claim, so the freed slot can be refilled
// by a concurrent park while the batch is between selection and the failed
// acquisition; an unconditional reinsert would then grow the map past its bound.
// The test drives the real production steps: selectDeferredPublishCandidate
// deletes the seeded entry, a concurrent park refills the freed slot to the cap,
// the saturated gate makes takeDeferredPublishSlot fail, and the repark must
// retain the intent through the rescan flag instead of inserting a cap+1'th entry.
func TestDeferredPublishFailedAcquisitionReparkHonorsPendingBound(t *testing.T) {
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	const selected = "selected-session"
	h.deferredPublishPending = map[string]deferredPublishIntent{
		selected: {fileID: 4242, generation: 2},
	}
	releaseGate := holdDetachedGate(t, h)
	defer releaseGate()

	// Production selection deletes the winner before the gate claim, leaving its
	// slot free.
	id, intent, found := h.selectDeferredPublishCandidate()
	if !found || id != selected {
		t.Fatalf("selection = (%q, found=%v), want the seeded session", id, found)
	}
	if _, still := h.deferredPublishPending[selected]; still {
		t.Fatal("selection did not delete the entry before the gate claim")
	}

	// The freed slot is refilled by a concurrent park, so the map is back at its
	// cap when the failed acquisition runs.
	h.deferredPublishMu.Lock()
	for i := 0; i < virtualDeferredPublishPendingCap; i++ {
		h.deferredPublishPending[fmt.Sprintf("filler-%d", i)] = deferredPublishIntent{fileID: i + 1, generation: 1}
	}
	h.deferredPublishMu.Unlock()

	// The gate is fully held, so the claim fails and the batch repark runs.
	claimed, _ := h.takeDeferredPublishSlot(id, intent)
	if claimed {
		t.Fatal("slot claim succeeded against a saturated gate; the regression path was not exercised")
	}
	h.deferredPublishMu.Lock()
	h.reparkDeferredPublishSelectedLocked(id, intent)
	h.deferredPublishMu.Unlock()

	if _, exists := h.deferredPublishPending[selected]; exists {
		t.Fatal("failed-acquisition repark grew the pending map past its bound")
	}
	if len(h.deferredPublishPending) != virtualDeferredPublishPendingCap {
		t.Fatalf("pending map size = %d, want the bound %d", len(h.deferredPublishPending), virtualDeferredPublishPendingCap)
	}
	if !h.deferredPublishRescan {
		t.Fatal("failed-acquisition repark at the bound did not set the overflow rescan flag")
	}
}

// TestDeferredPublishFailedAcquisitionReparkDoesNotCountAsRetry proves the
// failed-acquisition repark preserves the intent's schedule: a saturated gate is
// backpressure, not a failed publish, so the reinsert must NOT increment the retry
// attempt count or set a not-before backoff (unlike the retryable-outcome repark,
// which does both). With room in the map the selected intent is stored as-is.
func TestDeferredPublishFailedAcquisitionReparkDoesNotCountAsRetry(t *testing.T) {
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))

	h.deferredPublishMu.Lock()
	h.reparkDeferredPublishSelectedLocked("session-a", deferredPublishIntent{fileID: 7, generation: 3})
	h.deferredPublishMu.Unlock()

	stored, parked := h.deferredPublishPending["session-a"]
	if !parked {
		t.Fatal("failed-acquisition repark with room did not park the intent")
	}
	if stored.attempts != 0 {
		t.Fatalf("failed-acquisition repark bumped the retry attempt count to %d; a saturated gate must not count as a failed publish", stored.attempts)
	}
	if !stored.notBefore.IsZero() {
		t.Fatalf("failed-acquisition repark set a retry backoff at %v; a saturated gate is not a failed publish", stored.notBefore)
	}
	if h.deferredPublishRescan {
		t.Fatal("failed-acquisition repark set the rescan flag with room available")
	}
}
