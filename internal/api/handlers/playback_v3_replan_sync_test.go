package handlers

import (
	"context"
	"testing"
)

// replanSyncContextRecorder captures the context the post-replan sync ran on.
type replanSyncContextRecorder struct {
	calls int
	errs  []error
}

func (r *replanSyncContextRecorder) SyncNow(ctx context.Context) error {
	r.calls++
	r.errs = append(r.errs, ctx.Err())
	return nil
}

// The durable replan commit can run after executeReplanV3 has returned, so a
// sync bound to the request context was already canceled. The detached helper
// must hand the syncer a live context or the session snapshot never persists.
func TestReplanSessionSyncUsesDetachedContext(t *testing.T) {
	recorder := &replanSyncContextRecorder{}
	handler := &PlaybackHandler{SessionSyncer: recorder}

	requestCtx, cancel := context.WithCancel(context.Background())
	cancel()

	handler.syncSessionsAfterReplanDetached(requestCtx)

	if recorder.calls != 1 {
		t.Fatalf("sync calls = %d, want 1", recorder.calls)
	}
	if err := recorder.errs[0]; err != nil {
		t.Fatalf("sync context was canceled: %v", err)
	}
}
