package api

import (
	"context"
	"testing"

	mediarequests "github.com/Silo-Server/silo-server/internal/requests"
)

// AttachRequestRouter must tolerate absent dependencies (e.g. a build with the
// plugin service disabled) without panicking — fulfillment then degrades to the
// existing "no backend configured" failure rather than crashing the wiring.
func TestAttachRequestRouterNilDependenciesDoesNotPanic(t *testing.T) {
	AttachRequestRouter(nil, nil)
}

// A router adapter installed without a plugin service must return a controlled
// error rather than panic on the request path.
func TestPluginRequestRouterAdapterNilServiceReturnsError(t *testing.T) {
	adapter := PluginRequestRouterAdapter{}
	if _, err := adapter.RequestRouterClient(context.Background(), 1, "request_router.v1"); err == nil {
		t.Fatal("RequestRouterClient with nil Svc = nil error, want a controlled error")
	}
}

type featureRouterStub struct {
	mediarequests.RequestRouterProvider
	features mediarequests.RouterFeatures
}

func (f *featureRouterStub) RouterFeatures(context.Context, int, string) (mediarequests.RouterFeatures, error) {
	return f.features, nil
}

// The composite must forward feature reads to the owning provider: the
// Service asserts RouterFeatureReader on the outer wrapper, so without
// forwarding every season/progress-capable plugin reads as incapable in
// mixed core-plus-plugin deployments.
func TestCompositeRouterForwardsRouterFeatures(t *testing.T) {
	ctx := context.Background()
	want := mediarequests.RouterFeatures{SupportsSeasons: true, ReportsDownloadProgress: true}
	composite := &compositeRequestRouter{plugin: &featureRouterStub{features: want}}

	got, err := composite.RouterFeatures(ctx, 7, "request_router.v1")
	if err != nil {
		t.Fatalf("RouterFeatures returned error: %v", err)
	}
	if got != want {
		t.Fatalf("RouterFeatures = %+v, want %+v", got, want)
	}

	// The core virtual side declares no optional features.
	got, err = composite.RouterFeatures(ctx, 0, "virtual-library-requests")
	if err != nil {
		t.Fatalf("virtual RouterFeatures returned error: %v", err)
	}
	if got != (mediarequests.RouterFeatures{}) {
		t.Fatalf("virtual RouterFeatures = %+v, want zero", got)
	}

	// A plugin provider without the reader degrades to zero, not an error.
	bare := &compositeRequestRouter{plugin: nil}
	if _, err := bare.RouterFeatures(ctx, 7, "request_router.v1"); err != nil {
		t.Fatalf("nil-plugin RouterFeatures returned error: %v", err)
	}
}
