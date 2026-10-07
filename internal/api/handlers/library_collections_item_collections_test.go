package handlers

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
)

// stubItemCollectionIndex is the reverse membership lookup without a database:
// it answers the collections seeded for a membership id and records the id it
// was asked for.
type stubItemCollectionIndex struct {
	byMembership   map[string][]*models.LibraryCollection
	err            error
	lastMembership string
}

func (s *stubItemCollectionIndex) ListContainingItem(_ context.Context, mediaItemID string) ([]*models.LibraryCollection, error) {
	s.lastMembership = mediaItemID
	if s.err != nil {
		return nil, s.err
	}
	return s.byMembership[mediaItemID], nil
}

// TestItemCollectionsReverseLookup pins the item detail "collections" field's
// service contract: a member of two visible collections lists both, a member
// of none answers an empty slice, and a collection outside the viewer's library
// scope is dropped. The stub index stands in for the SQL repository, so the
// test does not touch a database. Posters resolve through the real collection
// service, which drops a stored poster when it has no artwork resolver.
func TestItemCollectionsReverseLookup(t *testing.T) {
	const membership = "movie:dune-1984"
	alpha := &models.LibraryCollection{ID: "alpha", Title: "Dune Saga", Visibility: "visible", LibraryIDs: []int{1}, ItemCount: 3}
	zeta := &models.LibraryCollection{ID: "zeta", Title: "Sci-Fi", Visibility: "visible", LibraryIDs: []int{2}, ItemCount: 9}
	hidden := &models.LibraryCollection{ID: "hidden", Title: "Hidden", Visibility: "hidden", LibraryIDs: []int{1}}
	offScope := &models.LibraryCollection{ID: "offscope", Title: "Restricted", Visibility: "visible", LibraryIDs: []int{99}}

	t.Run("member of two collections", func(t *testing.T) {
		index := &stubItemCollectionIndex{byMembership: map[string][]*models.LibraryCollection{membership: {alpha, zeta}}}
		h := &LibraryCollectionHandler{itemCollectionIndex: index}
		got, err := h.ItemCollections(context.Background(), membership, catalog.AccessFilter{AllowedLibraryIDs: []int{1, 2}})
		if err != nil {
			t.Fatalf("ItemCollections: %v", err)
		}
		if index.lastMembership != membership {
			t.Fatalf("membership = %q, want %q", index.lastMembership, membership)
		}
		want := []ItemCollectionView{
			{ID: "alpha", Title: "Dune Saga", ItemCount: 3},
			{ID: "zeta", Title: "Sci-Fi", ItemCount: 9},
		}
		if len(got) != len(want) {
			t.Fatalf("collections = %+v, want %+v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("collections[%d] = %+v, want %+v", i, got[i], want[i])
			}
		}
	})

	t.Run("member of none", func(t *testing.T) {
		index := &stubItemCollectionIndex{byMembership: map[string][]*models.LibraryCollection{}}
		h := &LibraryCollectionHandler{itemCollectionIndex: index}
		got, err := h.ItemCollections(context.Background(), membership, catalog.AccessFilter{})
		if err != nil {
			t.Fatalf("ItemCollections: %v", err)
		}
		if got == nil || len(got) != 0 {
			t.Fatalf("collections = %#v, want an empty non-nil slice", got)
		}
	})

	t.Run("inaccessible collections excluded", func(t *testing.T) {
		// The viewer may see library 1 only. A collection in library 2 and a
		// hidden collection are both dropped even though they contain the item.
		index := &stubItemCollectionIndex{byMembership: map[string][]*models.LibraryCollection{membership: {alpha, zeta, hidden, offScope}}}
		h := &LibraryCollectionHandler{itemCollectionIndex: index}
		got, err := h.ItemCollections(context.Background(), membership, catalog.AccessFilter{AllowedLibraryIDs: []int{1}})
		if err != nil {
			t.Fatalf("ItemCollections: %v", err)
		}
		if len(got) != 1 || got[0].ID != "alpha" {
			t.Fatalf("collections = %+v, want only alpha", got)
		}
	})

	t.Run("empty membership", func(t *testing.T) {
		index := &stubItemCollectionIndex{}
		h := &LibraryCollectionHandler{itemCollectionIndex: index}
		got, err := h.ItemCollections(context.Background(), "", catalog.AccessFilter{})
		if err != nil {
			t.Fatalf("ItemCollections: %v", err)
		}
		if len(got) != 0 || index.lastMembership != "" {
			t.Fatalf("collections = %#v, lookup = %q", got, index.lastMembership)
		}
	})

	t.Run("lookup failure is an API error", func(t *testing.T) {
		index := &stubItemCollectionIndex{err: errors.New("boom")}
		h := &LibraryCollectionHandler{itemCollectionIndex: index}
		_, err := h.ItemCollections(context.Background(), membership, catalog.AccessFilter{})
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusInternalServerError {
			t.Fatalf("err = %v, want a 500 APIError", err)
		}
	})
}
