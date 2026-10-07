package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

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

// itemCollectionsTestPool mirrors the repo's Postgres test pattern: it skips
// unless SILO_TEST_DATABASE_URL names a disposable database, and refuses a
// database whose name does not look like a test/purge fixture.
func itemCollectionsTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	var databaseName string
	if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&databaseName); err != nil {
		t.Fatalf("identify test database: %v", err)
	}
	if !strings.Contains(strings.ToLower(databaseName), "test") && !strings.Contains(strings.ToLower(databaseName), "purge") {
		t.Fatalf("refusing destructive handler fixture database %q", databaseName)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestItemCollectionsReverseLookupDB exercises the real SQL behind the handler:
// the item detail "collections" field answers visible stored memberships the
// viewer's library scope can reach, drops hidden and out-of-scope collections,
// reports the global item_count, and leaves a smart (live-query) collection out
// because it stores no membership rows.
func TestItemCollectionsReverseLookupDB(t *testing.T) {
	pool := itemCollectionsTestPool(t)
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	slug := func(name string) string { return fmt.Sprintf("%s-%d", name, suffix) }

	var visibleLibrary, otherLibrary int
	if err := pool.QueryRow(ctx, `INSERT INTO media_folders (type, name, enabled) VALUES ('movies', $1, TRUE) RETURNING id`, slug("ic-visible")).Scan(&visibleLibrary); err != nil {
		t.Fatalf("seed visible library: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO media_folders (type, name, enabled) VALUES ('movies', $1, TRUE) RETURNING id`, slug("ic-other")).Scan(&otherLibrary); err != nil {
		t.Fatalf("seed other library: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_folders WHERE id = ANY($1)`, []int{visibleLibrary, otherLibrary})
	})

	member := slug("ic-member")
	companion := slug("ic-companion")
	for _, item := range []struct{ id, title string }{{member, "Member"}, {companion, "Companion"}} {
		if _, err := pool.Exec(ctx, `INSERT INTO media_items (content_id, type, title, sort_title, year, genres) VALUES ($1, 'movie', $2, $2, 2001, '{}'::text[])`, item.id, item.title); err != nil {
			t.Fatalf("seed item %s: %v", item.id, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_items WHERE content_id = ANY($1)`, []string{member, companion})
	})

	repo := catalog.NewLibraryCollectionRepository(pool)
	itemRepo := catalog.NewItemRepository(pool)
	service := catalog.NewLibraryCollectionService(repo, itemRepo, nil, nil)
	h := &LibraryCollectionHandler{repo: repo, service: service}

	create := func(name, title, visibility string, libraryID int, items ...string) string {
		t.Helper()
		collection, err := repo.Create(ctx, catalog.CreateLibraryCollectionInput{
			LibraryID: libraryID, LibraryIDs: []int{libraryID}, Slug: slug(name), Title: title,
			CollectionType: "manual", Visibility: visibility,
		})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		t.Cleanup(func() { _ = repo.Delete(context.Background(), collection.ID) })
		inputs := make([]catalog.LibraryCollectionItemInput, 0, len(items))
		for _, id := range items {
			inputs = append(inputs, catalog.LibraryCollectionItemInput{MediaItemID: id})
		}
		if err := repo.ReplaceItems(ctx, collection.ID, inputs); err != nil {
			t.Fatalf("replace items %s: %v", name, err)
		}
		return collection.ID
	}

	visible := create("visible", "Visible Set", "visible", visibleLibrary, member, companion)
	hidden := create("hidden", "Hidden Set", "hidden", visibleLibrary, member)
	offScope := create("offscope", "Off Scope Set", "visible", otherLibrary, member)

	// A smart collection derives its members from its query and stores no
	// membership rows, so ListContainingItem never reports it.
	smart, err := repo.Create(ctx, catalog.CreateLibraryCollectionInput{
		LibraryID: visibleLibrary, LibraryIDs: []int{visibleLibrary}, Slug: slug("smart"), Title: "Smart Set",
		CollectionType: "smart", Visibility: "visible",
	})
	if err != nil {
		t.Fatalf("create smart: %v", err)
	}
	t.Cleanup(func() { _ = repo.Delete(context.Background(), smart.ID) })

	// The viewer may reach only visibleLibrary, so hidden and off-scope drop out.
	// visible remains (the smart collection never stored a row to match).
	got, err := h.ItemCollections(ctx, member, catalog.AccessFilter{AllowedLibraryIDs: []int{visibleLibrary}})
	if err != nil {
		t.Fatalf("ItemCollections: %v", err)
	}
	ids := make([]string, 0, len(got))
	for _, c := range got {
		ids = append(ids, c.ID)
	}
	if !slices.Equal(ids, []string{visible}) {
		t.Fatalf("collections = %v, want [%s]", ids, visible)
	}
	if got[0].ItemCount != 2 {
		t.Fatalf("visible item_count = %d, want the global total 2", got[0].ItemCount)
	}

	// An unrestricted viewer (no allowlist) still drops hidden collections; an
	// off-scope collection is legitimately visible because no allowlist
	// restricts libraries. The allowlisted call above already proved off-scope
	// drops when the viewer's scope cannot reach its library.
	unrestricted, err := h.ItemCollections(ctx, member, catalog.AccessFilter{})
	if err != nil {
		t.Fatalf("unrestricted ItemCollections: %v", err)
	}
	sawOffScope := false
	for _, c := range unrestricted {
		if c.ID == hidden {
			t.Fatalf("unrestricted viewer saw hidden collection %s", hidden)
		}
		if c.ID == offScope {
			sawOffScope = true
		}
	}
	if !sawOffScope {
		t.Fatalf("unrestricted viewer should reach the visible off-scope collection %s", offScope)
	}

	// A member in no collection answers an empty non-nil slice.
	none, err := h.ItemCollections(ctx, slug("ic-nobody"), catalog.AccessFilter{})
	if err != nil {
		t.Fatalf("no-member ItemCollections: %v", err)
	}
	if none == nil || len(none) != 0 {
		t.Fatalf("no-member collections = %#v, want an empty non-nil slice", none)
	}
}
