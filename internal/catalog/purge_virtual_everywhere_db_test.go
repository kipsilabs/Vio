package catalog

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

var purgeTestFolderCounter atomic.Uint64

func seedPurgeFolder(t *testing.T, pool *pgxpool.Pool, folderID int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO media_folders(id,name,type,enabled) VALUES($1,$2,'movies',true)
		 ON CONFLICT (id) DO NOTHING`, folderID, fmt.Sprintf("Purge Test %d", folderID)); err != nil {
		t.Fatalf("seed purge folder %d: %v", folderID, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_folders WHERE id = $1`, folderID)
	})
}

func seedPurgeCollection(t *testing.T, pool *pgxpool.Pool, collectionID, title string, libraryID int) {
	t.Helper()
	// library_collections has no enabled column; visibility is the schema's
	// equivalent, and the row still needs its library, slug, and type.
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO library_collections(id, library_id, slug, title, collection_type, visibility, sort_order)
		VALUES($1, $2, $1, $3, 'manual', 'visible', 0) ON CONFLICT (id) DO NOTHING`, collectionID, libraryID, title); err != nil {
		t.Fatalf("seed purge collection %s: %v", collectionID, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM library_collections WHERE id = $1`, collectionID)
	})
}

func seedPurgeVirtualMovie(t *testing.T, pool *pgxpool.Pool, id string, folderID int, installationID int) {
	t.Helper()
	seedPurgeFolder(t, pool, folderID)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id, type, title, virtual_owner_installation_id)
		VALUES($1, 'movie', $2, $3)`, id, "Virtual "+id, installationID); err != nil {
		t.Fatalf("seed media_items %s: %v", id, err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_item_libraries(content_id, media_folder_id) VALUES($1, $2)`, id, folderID); err != nil {
		t.Fatalf("seed membership %s: %v", id, err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id)
		VALUES($1, $2, $3, 'virtual', $4)`, id, folderID, "virtual://movie/"+id, installationID); err != nil {
		t.Fatalf("seed media_files %s: %v", id, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_items WHERE content_id = $1`, id)
	})
}

func TestPurgeVirtualClearsHomeAfterCommit(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	repo := NewItemRepository(pool)
	folderID := 9001 + int(purgeTestFolderCounter.Add(1)%100)
	seedPurgeVirtualMovie(t, pool, "purge-home-movie", folderID, 42)
	seedPurgeCollection(t, pool, "purge-home-collection", "Purge Home", folderID)
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_items(collection_id, media_item_id)
		VALUES('purge-home-collection', 'purge-home-movie')`); err != nil {
		t.Fatalf("seed collection link: %v", err)
	}
	var revisionBefore int64
	if err := pool.QueryRow(ctx, `SELECT revision FROM catalog_purge_revision WHERE id = 1`).Scan(&revisionBefore); err != nil {
		t.Fatalf("read revision before: %v", err)
	}
	result, err := repo.PurgeVirtualPlaybackItems(ctx, VirtualPurgeOptions{DryRun: false})
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if result.FilesDeleted != 1 || result.ItemsDeleted != 1 {
		t.Fatalf("purge counts = files:%d items:%d, want 1/1", result.FilesDeleted, result.ItemsDeleted)
	}
	// library_collection_items.media_item_id is ON DELETE CASCADE in this
	// schema, so deleting the item already removed the link and the orphan
	// sweep has nothing left to count. Assert the rail is empty rather than
	// the count, which the cascade makes zero here.
	if result.CollectionsPruned != 0 {
		t.Fatalf("collections pruned = %d, want 0 with a cascading link", result.CollectionsPruned)
	}
	var remainingLinks int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM library_collection_items WHERE collection_id = 'purge-home-collection'`).Scan(&remainingLinks); err != nil {
		t.Fatal(err)
	}
	if remainingLinks != 0 {
		t.Fatalf("collection links remaining = %d, want 0", remainingLinks)
	}
	var remainingFiles int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_files WHERE content_id = 'purge-home-movie'`).Scan(&remainingFiles); err != nil {
		t.Fatal(err)
	}
	if remainingFiles != 0 {
		t.Fatalf("media_files remaining = %d", remainingFiles)
	}
	var remainingItems int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_items WHERE content_id = 'purge-home-movie'`).Scan(&remainingItems); err != nil {
		t.Fatal(err)
	}
	if remainingItems != 0 {
		t.Fatalf("media_items remaining = %d", remainingItems)
	}
	if result.PurgeRevision != revisionBefore+1 {
		t.Fatalf("result revision = %d, want %d (exactly +1)", result.PurgeRevision, revisionBefore+1)
	}
	var stored int64
	if err := pool.QueryRow(ctx, `SELECT revision FROM catalog_purge_revision WHERE id = 1`).Scan(&stored); err != nil {
		t.Fatalf("read stored revision: %v", err)
	}
	if stored != revisionBefore+1 {
		t.Fatalf("stored revision = %d, want %d (exactly +1)", stored, revisionBefore+1)
	}
}

func TestPurgeVirtualDryRunDoesNotBumpRevision(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	repo := NewItemRepository(pool)
	folderID := 9101 + int(purgeTestFolderCounter.Add(1)%100)
	seedPurgeVirtualMovie(t, pool, "purge-dryrun-movie", folderID, 42)
	var revisionBefore int64
	if err := pool.QueryRow(ctx, `SELECT revision FROM catalog_purge_revision WHERE id = 1`).Scan(&revisionBefore); err != nil {
		t.Fatalf("read revision before: %v", err)
	}
	result, err := repo.PurgeVirtualPlaybackItems(ctx, VirtualPurgeOptions{DryRun: true})
	if err != nil {
		t.Fatalf("dry-run purge: %v", err)
	}
	if result.FilesDeleted != 1 || result.ItemsDeleted != 1 {
		t.Fatalf("dry-run counts = files:%d items:%d, want 1/1", result.FilesDeleted, result.ItemsDeleted)
	}
	if result.StateRowsDeleted != 0 || result.CollectionsPruned != 0 || result.PurgeRevision != 0 {
		t.Fatalf("dry-run cleanup = state:%d collections:%d revision:%d, want 0/0/0", result.StateRowsDeleted, result.CollectionsPruned, result.PurgeRevision)
	}
	var revisionAfter int64
	if err := pool.QueryRow(ctx, `SELECT revision FROM catalog_purge_revision WHERE id = 1`).Scan(&revisionAfter); err != nil {
		t.Fatalf("read revision after: %v", err)
	}
	if revisionAfter != revisionBefore {
		t.Fatalf("dry-run bumped revision %d -> %d", revisionBefore, revisionAfter)
	}
	var remainingFiles int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_files WHERE content_id = 'purge-dryrun-movie'`).Scan(&remainingFiles); err != nil {
		t.Fatal(err)
	}
	if remainingFiles != 1 {
		t.Fatalf("dry-run deleted media_files: %d remaining", remainingFiles)
	}
}

func TestPurgeVirtualEvictsMalformedRecommendationCache(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	repo := NewItemRepository(pool)
	folderID := 9201 + int(purgeTestFolderCounter.Add(1)%100)
	seedPurgeVirtualMovie(t, pool, "purge-malformed-movie", folderID, 42)
	// Malformed rows keep a surviving source item so the source-cleanup
	// delete skips them and the type guard must evict them instead.
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id, type, title) VALUES('malformed-source', 'movie', 'Malformed Source')`); err != nil {
		t.Fatalf("seed malformed source: %v", err)
	}
	// Register cache cleanup BEFORE seeding: recommendation sources have
	// no FK, so a mid-test failure would otherwise leave orphan rows that
	// pollute later counting tests. Cache rows are removed before their
	// source item.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `
			DELETE FROM recommendation_cache
			WHERE user_id = 1 AND profile_id = 'profile' AND source_item_id = 'malformed-source'`)
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_items WHERE content_id = 'malformed-source'`)
	})
	for i, items := range []string{"null", `"scalar"`, `{"key":"val"}`} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO recommendation_cache(user_id, profile_id, rec_type, source_item_id, items, expires_at)
			VALUES(1, 'profile', $1, 'malformed-source', $2::jsonb, NOW() + INTERVAL '1 hour')`,
			fmt.Sprintf("popular-%d", i), items); err != nil {
			t.Fatalf("seed malformed recommendation %d: %v", i, err)
		}
	}
	result, err := repo.PurgeVirtualPlaybackItems(ctx, VirtualPurgeOptions{DryRun: false})
	if err != nil {
		t.Fatalf("purge with malformed cache: %v", err)
	}
	if result.CollectionsPruned != 3 {
		t.Fatalf("collections pruned = %d, want 3 (malformed rows evicted)", result.CollectionsPruned)
	}
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM recommendation_cache`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("malformed recommendation rows remaining = %d", remaining)
	}
}

func TestPurgeVirtualScopedRetainsPhysicalSeriesEpisodes(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	repo := NewItemRepository(pool)
	virtualFolder := 9301 + int(purgeTestFolderCounter.Add(1)%100)
	physicalFolder := 9302 + int(purgeTestFolderCounter.Add(1)%100)
	seedPurgeFolder(t, pool, virtualFolder)
	seedPurgeFolder(t, pool, physicalFolder)
	seriesID := fmt.Sprintf("purge-scoped-series-%d", purgeTestFolderCounter.Add(1))
	epID := seriesID + "-e1"
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id, type, title) VALUES($1, 'series', 'Scoped Series')`, seriesID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM episodes WHERE content_id = $1`, epID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_items WHERE content_id = $1`, seriesID)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO episodes(content_id, series_id, season_number, episode_number, title)
		VALUES($1, $2, 1, 1, 'Ep 1')`, epID, seriesID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_item_libraries(content_id, media_folder_id) VALUES($1, $2)`, seriesID, virtualFolder); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id)
		VALUES($1, $2, $3, 'virtual', 42)`, seriesID, virtualFolder, "virtual://series/"+seriesID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(episode_id, media_folder_id, file_path, container)
		VALUES($1, $2, $3, 'mp4')`, epID, physicalFolder, "/media/ep1.mkv"); err != nil {
		t.Fatal(err)
	}
	result, err := repo.PurgeVirtualPlaybackItems(ctx, VirtualPurgeOptions{DryRun: false, LibraryID: virtualFolder})
	if err != nil {
		t.Fatalf("scoped purge: %v", err)
	}
	if result.FilesDeleted != 1 {
		t.Fatalf("scoped purge files = %d, want 1 (virtual only)", result.FilesDeleted)
	}
	if result.ItemsDeleted != 0 {
		t.Fatalf("scoped purge items = %d, want 0 (series retained)", result.ItemsDeleted)
	}
	var episodes int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM episodes WHERE content_id = $1`, epID).Scan(&episodes); err != nil {
		t.Fatal(err)
	}
	if episodes != 1 {
		t.Fatalf("episodes remaining = %d, want 1", episodes)
	}
	var physicalFiles int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_files WHERE file_path = '/media/ep1.mkv'`).Scan(&physicalFiles); err != nil {
		t.Fatal(err)
	}
	if physicalFiles != 1 {
		t.Fatalf("physical files remaining = %d, want 1", physicalFiles)
	}
	var virtualFiles int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_files WHERE container = 'virtual'`).Scan(&virtualFiles); err != nil {
		t.Fatal(err)
	}
	if virtualFiles != 0 {
		t.Fatalf("virtual files remaining = %d, want 0", virtualFiles)
	}
}

func TestPurgeVirtualRecommendationPrunedToEmptyCountsOnce(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	repo := NewItemRepository(pool)
	folderID := 9401 + int(purgeTestFolderCounter.Add(1)%100)
	seedPurgeVirtualMovie(t, pool, "purge-pruneonce-movie", folderID, 42)
	// Register cache cleanup BEFORE seeding (recommendation sources have
	// no FK; a mid-test failure would otherwise leave orphan rows). Cache
	// rows go before their source items on cleanup.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `
			DELETE FROM recommendation_cache
			WHERE user_id = 1 AND profile_id = 'profile' AND source_item_id IN ('pruneonce-source','surviving-source')`)
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_items WHERE content_id IN ('pruneonce-source','surviving-source','surviving-other')`)
	})
	// One row references ONLY the purged item: prune empties it, delete
	// removes it. Must count once (as removed), not twice.
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id, type, title) VALUES('pruneonce-source', 'movie', 'Prune Source')`); err != nil {
		t.Fatalf("seed prune source: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO recommendation_cache(user_id, profile_id, rec_type, source_item_id, items, expires_at)
		VALUES(1, 'profile', 'emptiable', 'pruneonce-source', $1::jsonb, NOW() + INTERVAL '1 hour')`,
		`[{"media_item_id":"purge-pruneonce-movie","score":0.9}]`); err != nil {
		t.Fatalf("seed recommendation row: %v", err)
	}
	// A second row references the purged item plus a surviving item: prune
	// shrinks it, row survives. Counts once (as pruned). Distinct identity
	// (rec_type) avoids the (user,profile,rec_type,source) PK collision.
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id, type, title) VALUES('surviving-source', 'movie', 'Survivor')`); err != nil {
		t.Fatalf("seed surviving source: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id, type, title) VALUES('surviving-other', 'movie', 'Other')`); err != nil {
		t.Fatalf("seed surviving other: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO recommendation_cache(user_id, profile_id, rec_type, source_item_id, items, expires_at)
		VALUES(1, 'profile', 'shrinkable', 'surviving-source', $1::jsonb, NOW() + INTERVAL '1 hour')`,
		`[{"media_item_id":"purge-pruneonce-movie","score":0.9},{"media_item_id":"surviving-other","score":0.5}]`); err != nil {
		t.Fatalf("seed surviving recommendation row: %v", err)
	}
	result, err := repo.PurgeVirtualPlaybackItems(ctx, VirtualPurgeOptions{DryRun: false})
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	// 1 emptied+deleted row + 1 pruned-surviving row = exactly 2.
	if result.CollectionsPruned != 2 {
		t.Fatalf("collections pruned = %d, want 2 (one removed, one pruned, no double-count)", result.CollectionsPruned)
	}
	var shrunk string
	if err := pool.QueryRow(ctx, `
		SELECT items::text FROM recommendation_cache
		WHERE source_item_id = 'surviving-source' AND items::text LIKE '%surviving-other%'`).Scan(&shrunk); err != nil {
		t.Fatalf("surviving row missing or fully removed: %v", err)
	}
}

func TestPurgeVirtualPreExistingEmptiesCountSeparately(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	repo := NewItemRepository(pool)
	folderID := 9501 + int(purgeTestFolderCounter.Add(1)%100)
	seedPurgeVirtualMovie(t, pool, "purge-preempty-movie", folderID, 42)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `
			DELETE FROM recommendation_cache
			WHERE user_id = 1 AND profile_id = 'profile' AND source_item_id = 'preempty-source'`)
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_items WHERE content_id = 'preempty-source'`)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id, type, title) VALUES('preempty-source', 'movie', 'Pre Source')`); err != nil {
		t.Fatalf("seed preempty source: %v", err)
	}
	// One row is already empty before the purge; one references ONLY the
	// purged item and will be emptied by the prune. Distinct identities.
	for _, seed := range []struct {
		recType, source, items string
	}{
		{"already-empty", "preempty-source", `[]`},
		{"will-empty", "preempty-source", `[{"media_item_id":"purge-preempty-movie","score":0.9}]`},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO recommendation_cache(user_id, profile_id, rec_type, source_item_id, items, expires_at)
			VALUES(1, 'profile', $1, 'preempty-source', $2::jsonb, NOW() + INTERVAL '1 hour')`,
			seed.recType, seed.items); err != nil {
			t.Fatalf("seed %s: %v", seed.recType, err)
		}
	}
	result, err := repo.PurgeVirtualPlaybackItems(ctx, VirtualPurgeOptions{DryRun: false})
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	// 1 pre-existing empty + 1 pruned-then-removed = exactly 2.
	if result.CollectionsPruned != 2 {
		t.Fatalf("collections pruned = %d, want 2 (pre-existing empty + emptied, no double-count)", result.CollectionsPruned)
	}
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM recommendation_cache WHERE source_item_id = 'preempty-source'`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("recommendation rows remaining = %d, want 0", remaining)
	}
}
