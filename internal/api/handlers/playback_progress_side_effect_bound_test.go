package handlers

import (
	"context"
	"testing"
	"time"
)

// blockingSessionSyncer models the admin-session sync that held progress
// responses for 45-60s in production: it blocks until its context is canceled.
type blockingSessionSyncer struct {
	started chan struct{}
}

func (s *blockingSessionSyncer) SyncNow(ctx context.Context) error {
	select {
	case s.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

// A wedged progress side effect must not hold the response past its budget. The
// row CAS has already committed, so the client gets its acknowledgement and the
// next heartbeat replays the idempotent writers.
func TestApplyProgressV2BoundsSideEffectHold(t *testing.T) {
	f := newPlaybackServiceFixture(t)
	syncer := &blockingSessionSyncer{started: make(chan struct{}, 1)}
	f.handler.SessionSyncer = syncer

	previousBudget := progressSideEffectBudget
	progressSideEffectBudget = 100 * time.Millisecond
	t.Cleanup(func() { progressSideEffectBudget = previousBudget })

	started := time.Now()
	view, err := f.handler.ApplyProgressV2(f.ctx, f.caller, f.session.ID, PlaybackProgressCommand{
		Sequence: 1,
		Position: 42,
		IsPaused: true, // flips the pause state, which triggers the session sync
	})
	elapsed := time.Since(started)

	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if view.Accepted == nil {
		t.Fatalf("progress view = %+v, want the sample accepted", view)
	}
	select {
	case <-syncer.started:
	default:
		t.Fatal("the pause flip never reached the session syncer")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("progress response held for %s, want it bounded by the side-effect budget", elapsed)
	}
}

// A pause scrobble is emitted once: the next heartbeat sees no transition and
// will not send it again. It must therefore run before a slow admin session
// sync can consume the side-effect budget, not after it.
func TestApplyProgressV2PauseScrobbleSurvivesSlowSessionSync(t *testing.T) {
	f := newPlaybackServiceFixture(t)
	syncer := &blockingSessionSyncer{started: make(chan struct{}, 1)}
	f.handler.SessionSyncer = syncer
	scrobbler := &contextCheckingPauseScrobbler{}
	f.handler.WatchScrobbler = scrobbler

	previousBudget := progressSideEffectBudget
	progressSideEffectBudget = 100 * time.Millisecond
	t.Cleanup(func() { progressSideEffectBudget = previousBudget })

	view, err := f.handler.ApplyProgressV2(f.ctx, f.caller, f.session.ID, PlaybackProgressCommand{
		Sequence: 1,
		Position: 42,
		IsPaused: true,
	})
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if view.Accepted == nil {
		t.Fatalf("progress view = %+v, want the sample accepted", view)
	}
	select {
	case <-syncer.started:
	default:
		t.Fatal("the pause flip never reached the session syncer")
	}
	if len(scrobbler.pauses) != 1 {
		t.Fatalf("pause scrobbles = %d, want 1 even though the session sync blocked", len(scrobbler.pauses))
	}
	if scrobbler.pauseContextErr != nil {
		t.Fatalf("pause scrobble ran on an expired context: %v", scrobbler.pauseContextErr)
	}
}
