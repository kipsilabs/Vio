package requests

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// blockingRouter holds the first fulfillment until the test releases it, then
// delegates to the regular fake so the submission path sees a normal target
// response.
type blockingRouter struct {
	*fakeRouterProvider
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingRouter) Fulfill(ctx context.Context, installationID int, capabilityID string, req Request, qualities []Quality, conns []ResolvedRouterConnection) ([]RouterTarget, string, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return b.fakeRouterProvider.Fulfill(ctx, installationID, capabilityID, req, qualities, conns)
}

// Auto-approval commits an approved row and returns it before fulfillment runs:
// a router that blocks cannot hold the create response open.
func TestCreateRequestDoesNotBlockOnDeferredFulfillment(t *testing.T) {
	store := newFakeStore()
	store.settings.RequestsEnabled = true
	store.settings.GlobalAutoApprovalEnabled = true
	store.integrations = []Integration{autoApproveRouterInst("router-1", "radarr-key")}
	router := &blockingRouter{
		fakeRouterProvider: &fakeRouterProvider{},
		started:            make(chan struct{}),
		release:            make(chan struct{}),
	}
	service := newTestService(store)
	service.SetRouterProvider(router)
	lifecycle := &recordingLifecycleNotifier{}
	service.SetLifecycleNotifier(lifecycle)
	work := &deferredEnrichment{}
	service.asyncWork = work.schedule

	type result struct {
		req *Request
		err error
	}
	done := make(chan result, 1)
	go func() {
		req, err := service.CreateRequest(context.Background(), testViewer(1), fightClubInput)
		done <- result{req: req, err: err}
	}()

	var got result
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("CreateRequest waited for fulfillment")
	}
	if got.err != nil {
		t.Fatalf("CreateRequest returned error: %v", got.err)
	}
	if got.req.Status != StatusApproved || got.req.Outcome != OutcomeActive {
		t.Fatalf("request = %s/%s, want approved/active", got.req.Status, got.req.Outcome)
	}
	if got.req.LastError != "" {
		t.Fatalf("request last error = %q, want none at creation", got.req.LastError)
	}
	if work.pending() != 2 {
		t.Fatalf("deferred work = %d, want enrichment and submission", work.pending())
	}
	if router.fulfillCalls != 0 {
		t.Fatalf("fulfill calls = %d, want none before the response", router.fulfillCalls)
	}
	if len(lifecycle.submitted) != 1 || lifecycle.submitted[0] != got.req.ID {
		t.Fatalf("submitted events = %v, want the new request before fulfillment", lifecycle.submitted)
	}
	if len(lifecycle.approved) != 1 || lifecycle.approved[0].requestID != got.req.ID || lifecycle.approved[0].origin != ApprovalOriginPolicy {
		t.Fatalf("approved events = %+v, want the policy approval before fulfillment", lifecycle.approved)
	}

	drained := make(chan struct{})
	go func() {
		work.drain()
		close(drained)
	}()
	select {
	case <-router.started:
	case <-time.After(5 * time.Second):
		t.Fatal("deferred submission did not reach the router")
	}
	close(router.release)
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("deferred submission did not finish")
	}
	if gotStored := store.requests[got.req.ID]; gotStored.Status != StatusQueued {
		t.Fatalf("stored request = %s, want queued after the deferred submission", gotStored.Status)
	}
}

// A failure after the response is recorded on the committed row with the
// retry schedule, instead of becoming a client-visible error at create time.
func TestCreateRequestDeferredFulfillmentFailureRecordsLastError(t *testing.T) {
	store := newFakeStore()
	store.settings.RequestsEnabled = true
	store.settings.GlobalAutoApprovalEnabled = true
	store.integrations = []Integration{autoApproveRouterInst("router-1", "radarr-key")}
	router := &fakeRouterProvider{fulfillErr: errors.New("radarr: connection refused")}
	service := newTestService(store)
	service.SetRouterProvider(router)
	work := &deferredEnrichment{}
	service.asyncWork = work.schedule

	req, err := service.CreateRequest(context.Background(), testViewer(1), fightClubInput)
	if err != nil {
		t.Fatalf("CreateRequest returned error: %v", err)
	}
	if req.Status != StatusApproved || req.Outcome != OutcomeActive || req.LastError != "" {
		t.Fatalf("request = %+v, want approved before the deferred submission", req)
	}
	work.drain()

	stored := store.requests[req.ID]
	if stored.Status != StatusApproved || stored.Outcome != OutcomeActive {
		t.Fatalf("stored request = %s/%s, want approved/active", stored.Status, stored.Outcome)
	}
	if !strings.Contains(stored.LastError, "connection refused") {
		t.Fatalf("last error = %q, want the fulfillment failure", stored.LastError)
	}
	if stored.NextSubmitAt == nil || stored.SubmitAttempts != 1 {
		t.Fatalf("stored request = %+v, want one failed attempt with a retry time", stored)
	}
	if router.fulfillCalls != 1 {
		t.Fatalf("fulfill calls = %d, want 1", router.fulfillCalls)
	}
}
