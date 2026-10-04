package apiv2

import (
	"context"
	"net/http"
)

// VirtualLibraryStatusService reports whether the virtual-library indexer paths
// are wired. Both reads are configuration probes, not network calls.
type VirtualLibraryStatusService interface {
	IndexerCapabilities() (indexerSearch bool, indexerRequest bool)
	// RefreshPrunesDeadCandidates reports whether a virtual-candidates refresh
	// deletes dead, absent provider-candidate rows it no longer lists (honoring
	// the sweep's retention) instead of leaving them in place.
	RefreshPrunesDeadCandidates() bool
	// WaitForImports reports whether playback waits (bounded) for a release
	// AltMount is actively fetching instead of skipping it, so a client can
	// show a waiting state rather than a failure when the picked release is
	// still importing.
	WaitForImports() bool
}

// VirtualLibraryCapabilities is the virtual-library feature-detection
// document. indexer_search gates the "what's on the indexers" list;
// indexer_request gates the per-release request action;
// refresh_prunes_dead_candidates gates refresh-time cleanup of dead rows,
// so a client can tell whether a refresh also garbage-collects;
// wait_for_imports gates the bounded wait for in-flight AltMount imports,
// so a client can tell a waiting release from a dead one.
type VirtualLibraryCapabilities struct {
	Capability
	IndexerSearch               bool `json:"indexer_search"`
	IndexerRequest              bool `json:"indexer_request"`
	RefreshPrunesDeadCandidates bool `json:"refresh_prunes_dead_candidates"`
	WaitForImports              bool `json:"wait_for_imports"`
}

type VirtualLibraryCapabilitiesOutput struct {
	Status       int
	ETag         string `header:"ETag"`
	CacheControl string `header:"Cache-Control"`
	Body         VirtualLibraryCapabilities
}

func registerVirtualLibraryCapabilities(reg *Registry) {
	Register(reg, Operation{Operation: humaOp(http.MethodGet, Prefix+"/capabilities/virtual-library", "getVirtualLibraryCapabilities", "watch",
		"Discover whether indexer search and provider release requests are available."), Class: ClassProfileScoped, ProfileOptional: true, ServiceBacked: true},
		func(_ context.Context, _ *CapabilityInput) (*VirtualLibraryCapabilitiesOutput, error) {
			indexerSearch, indexerRequest := false, false
			prunes := false
			waits := false
			if reg.deps.VirtualLibraryStatus != nil {
				indexerSearch, indexerRequest = reg.deps.VirtualLibraryStatus.IndexerCapabilities()
				prunes = reg.deps.VirtualLibraryStatus.RefreshPrunesDeadCandidates()
				waits = reg.deps.VirtualLibraryStatus.WaitForImports()
			}
			state := StateNotConfigured
			if indexerSearch || indexerRequest {
				state = StateAvailable
			}
			return &VirtualLibraryCapabilitiesOutput{Body: VirtualLibraryCapabilities{
				Capability:                  Capability{State: state},
				IndexerSearch:               indexerSearch,
				IndexerRequest:              indexerRequest,
				RefreshPrunesDeadCandidates: prunes,
				WaitForImports:              waits,
			}}, nil
		})
}
