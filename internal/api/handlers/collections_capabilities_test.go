package handlers

import (
	"slices"
	"testing"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/userstore"
)

<<<<<<< HEAD
func TestCollectionCapabilitiesAdvertiseSortSupport(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/collections/capabilities", nil)
	NewCollectionHandler(nil).HandleCapabilities(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got CollectionCapabilitiesView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !got.CollectionDefaultSort || !got.CollectionSortPreferences || !got.EffectiveCollectionSort || !got.AdminItemMaterialize {
		t.Fatalf("collection capabilities not fully advertised: %+v", got)
	}

	// Every advertised kind must actually be accepted, or a client that trusts
	// the capability gets a 400. This is the guard against the list drifting
	// away from what catalog.NormalizeSortPreferenceRef allows.
=======
// Every advertised sort-preference kind must be accepted by the sort
// preference writer, or a client that trusts the capability gets a 400.
func TestCollectionCapabilitiesAdvertiseOnlyAcceptedSortKinds(t *testing.T) {
	got := NewCollectionHandler(nil).Capabilities()
>>>>>>> upstream/main
	for _, kind := range got.SortPreferenceKinds {
		if _, _, ok := catalog.NormalizeSortPreferenceRef(kind, "collection-1"); !ok {
			t.Fatalf("advertised kind %q is rejected by normalizeCollectionRef", kind)
		}
	}
	for _, want := range []string{
		userstore.CollectionKindLibrary,
		userstore.CollectionKindUser,
		userstore.CollectionKindWatchlist,
		userstore.CollectionKindFavorites,
	} {
		if !slices.Contains(got.SortPreferenceKinds, want) {
			t.Fatalf("sort_preference_kinds = %v, missing %q", got.SortPreferenceKinds, want)
		}
	}
}
