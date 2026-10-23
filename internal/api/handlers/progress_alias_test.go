package handlers

import (
	"errors"
	"testing"

	"github.com/Silo-Server/silo-server/internal/access"
	"github.com/Silo-Server/silo-server/internal/userstore"
)

// TestSyncProgressWritesCanonicalEpisodeForCachedAlias proves the offline/v1/v2
// sync path resolves a cached layout-alias id before both the access check and
// the store write, and publishes the canonical identity.
func TestSyncProgressWritesCanonicalEpisodeForCachedAlias(t *testing.T) {
	store := &bulkProgressStore{}
	h := NewProgressHandler(&bulkProgressProvider{store: store})
	lookup := &fakeEpisodeAliasLookup{canonical: "episode-canonical", found: true}
	h.EpisodeAliasLookup = lookup
	accessLookup := &fakeProgressLookup{accessible: map[string]bool{"episode-canonical": true}}
	h.LibraryLookup = accessLookup
	ctx := access.SetScope(t.Context(), access.Scope{AllowedLibraryIDs: []int{4}})

	out, err := h.SyncProgress(ctx, 17, "child", []ProgressSyncUpdate{{
		MediaItemID: "episode-cached-alias",
		CheckAccess: true,
		Position:    600,
		Duration:    1800,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Status != "ok" || out[0].MediaItemID != "episode-cached-alias" {
		t.Fatalf("results = %+v, want ok echoed under the submitted id", out)
	}
	if len(store.items) != 1 || store.items[0] != "episode-canonical" {
		t.Fatalf("store writes = %v, want [episode-canonical]", store.items)
	}
	if len(accessLookup.gotContentIDs) != 1 || accessLookup.gotContentIDs[0] != "episode-canonical" {
		t.Fatalf("access check ids = %v, want [episode-canonical]", accessLookup.gotContentIDs)
	}
	if len(lookup.seen) != 1 || lookup.seen[0] != "episode-cached-alias" {
		t.Fatalf("alias lookup = %v, want [episode-cached-alias]", lookup.seen)
	}
}

// TestSyncProgressLookupFailureWritesNothing proves a failed canonical lookup
// is a retryable per-item failure, not a silent write under the alias id.
func TestSyncProgressLookupFailureWritesNothing(t *testing.T) {
	store := &bulkProgressStore{}
	h := NewProgressHandler(&bulkProgressProvider{store: store})
	h.EpisodeAliasLookup = &fakeEpisodeAliasLookup{err: errors.New("temporary lookup failure")}
	h.LibraryLookup = &fakeProgressLookup{accessible: map[string]bool{"episode-cached-alias": true}}
	ctx := access.SetScope(t.Context(), access.Scope{AllowedLibraryIDs: []int{4}})

	out, err := h.SyncProgress(ctx, 17, "child", []ProgressSyncUpdate{{
		MediaItemID: "episode-cached-alias",
		CheckAccess: true,
		Position:    600,
		Duration:    1800,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Status != "error" || out[0].FailureStatus != 503 {
		t.Fatalf("results = %+v, want one retryable error", out)
	}
	if len(store.items) != 0 {
		t.Fatalf("store writes = %v, want none", store.items)
	}
}

// keep the interface assertion close to the test so a future store change does
// not silently let the fake bypass the methods under test.
var _ userstore.UserStore = (*bulkProgressStore)(nil)
