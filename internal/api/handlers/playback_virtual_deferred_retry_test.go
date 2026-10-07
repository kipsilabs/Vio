package handlers

import (
	"context"
	"errors"
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
