package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/mdblist"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/requestlock"
	"github.com/jackc/pgx/v5/pgxpool"
)

func waitCatalogBlocked(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid uint32) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestRemoveItemLocksCollectionThenItemBeforeClaims(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled) VALUES(3903,'Removal','movies',true);
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config) VALUES('remove-race','remove-race','Removal','manual',3903,'{"virtual_playback":true}');
		INSERT INTO library_collection_libraries(collection_id,library_id) VALUES('remove-race',3903);
		INSERT INTO media_items(content_id,type,title,sort_title,status) VALUES('movie-tmdb-3903','movie','Removal','Removal','matched');
		INSERT INTO library_collection_items(collection_id,media_item_id,position) VALUES('remove-race','movie-tmdb-3903',0);
		INSERT INTO virtual_media_source_claims(plugin_installation_id,source_key,content_id,media_folder_id) VALUES(11,'collection:remove-race','movie-tmdb-3903',3903)`); err != nil {
		t.Fatal(err)
	}
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if err := requestlock.LockItem(ctx, blocker, "movie-tmdb-3903"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- NewLibraryCollectionRepository(pool).RemoveItem(ctx, "remove-race", "movie-tmdb-3903") }()
	waitCatalogBlocked(t, ctx, pool, blocker.Conn().PgConn().PID())
	probe, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, lockErr := probe.Exec(ctx, `SELECT id FROM library_collections WHERE id='remove-race' FOR UPDATE NOWAIT`)
	_ = probe.Rollback(ctx)
	if lockErr == nil {
		t.Fatal("removal did not lock collection before item")
	}
	if _, err := blocker.Exec(ctx, `SELECT content_id FROM virtual_media_source_claims WHERE content_id='movie-tmdb-3903' FOR UPDATE NOWAIT`); err != nil {
		t.Fatalf("removal locked claims before item: %v", err)
	}
	if _, err := blocker.Exec(ctx, `INSERT INTO virtual_media_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,owns_item_metadata) VALUES(12,'independent','movie-tmdb-3903',3903,true)`); err != nil {
		t.Fatal(err)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var claims, members, owner int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM virtual_media_source_claims WHERE content_id='movie-tmdb-3903'), (SELECT count(*) FROM library_collection_items WHERE media_item_id='movie-tmdb-3903'), virtual_owner_installation_id FROM media_items WHERE content_id='movie-tmdb-3903'`).Scan(&claims, &members, &owner); err != nil || claims != 1 || members != 0 || owner != 12 {
		t.Fatalf("claims=%d members=%d owner=%d error=%v", claims, members, owner, err)
	}
}

func TestCollectionSchedulerRepairsAcceptedVirtualMember(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled) VALUES(3904,'Recovery','movies',true);
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config,sync_schedule,next_sync_at) VALUES('scheduled-repair','scheduled-repair','Recovery','tmdb',3904,'{"mode":"tmdb_preset","preset":"popular","virtual_playback":true}','0 * * * *',NOW()-INTERVAL '1 hour');
		INSERT INTO library_collection_libraries(collection_id,library_id) VALUES('scheduled-repair',3904);
		INSERT INTO media_items(content_id,type,title,sort_title,tmdb_id,status,year) VALUES('movie-tmdb-3904','movie','Recovery','Recovery','3904','matched',2000);
		INSERT INTO media_item_libraries(content_id,media_folder_id) VALUES('movie-tmdb-3904',3904);
		INSERT INTO library_collection_items(collection_id,media_item_id,position) VALUES('scheduled-repair','movie-tmdb-3904',0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION lose_scheduled_virtual_files() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			DELETE FROM media_files WHERE content_id='movie-tmdb-3904';
			RETURN NEW;
		END $$;
		CREATE TRIGGER lose_scheduled_virtual_files AFTER INSERT ON library_collection_sync_runs FOR EACH ROW EXECUTE FUNCTION lose_scheduled_virtual_files()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TRIGGER lose_scheduled_virtual_files ON library_collection_sync_runs; DROP FUNCTION lose_scheduled_virtual_files()`)
	})
	collections := NewLibraryCollectionRepository(pool)
	service := NewLibraryCollectionService(collections, NewItemRepository(pool), NewLibraryItemRepository(pool), nil)
	service.TMDBDigitalReleases = &fakeDigitalReleaseChecker{released: map[int]bool{3904: true}}
	service.TMDBCollections = &mockTMDBFailSyncFetcher{entries: []TMDBCollectionEntry{{ID: 3904, MediaType: "movie", Title: "Recovery", ReleaseDate: "2000-01-01"}}}
	var calls int
	service.VirtualVariants = func(context.Context, string, string) ([]VirtualPlaybackVariant, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("provider unavailable")
		}
		return []VirtualPlaybackVariant{{OwnerInstallationID: 11}}, nil
	}
	scheduler := NewCollectionSyncScheduler(collections, service, slog.New(slog.DiscardHandler))
	for attempt := range 2 {
		data, err := scheduler.RunOnce(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		var result CollectionSyncResult
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		if result.Due != 1 || (attempt == 0 && result.Failed != 1) || (attempt == 1 && result.Synced != 1) {
			t.Fatalf("attempt %d: %+v", attempt, result)
		}
		if _, err := pool.Exec(ctx, `UPDATE library_collections SET next_sync_at=NOW()-INTERVAL '1 hour' WHERE id='scheduled-repair'`); err != nil {
			t.Fatal(err)
		}
	}
	var files, claims int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM media_files WHERE content_id='movie-tmdb-3904' AND container='virtual'), (SELECT count(*) FROM virtual_media_file_source_claims WHERE content_id='movie-tmdb-3904' AND source_key='collection:scheduled-repair' AND staged_until IS NULL)`).Scan(&files, &claims); err != nil || files != 1 || claims != 1 {
		t.Fatalf("files=%d claims=%d error=%v", files, claims, err)
	}
}

func TestLegacyCleanupRevalidatesAfterAcceptance(t *testing.T) {
	for _, source := range []string{"collection", "collection:cleanup-race"} {
		t.Run(source, func(t *testing.T) {
			pool := newVirtualMediaTestPool(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if _, err := pool.Exec(ctx, `
				INSERT INTO media_folders(id,name,type,enabled) VALUES(3901,'Race','movies',true);
				INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config) VALUES('cleanup-race','cleanup-race','Race','manual',3901,'{"virtual_playback":true}');
				INSERT INTO library_collection_libraries(collection_id,library_id) VALUES('cleanup-race',3901);
				INSERT INTO media_items(content_id,type,title,sort_title,status) VALUES('movie-tmdb-3901','movie','Race','Race','matched')`); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO virtual_media_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,staged_until) VALUES(11,$1,'movie-tmdb-3901',3901,NOW()-INTERVAL '1 hour')`, source); err != nil {
				t.Fatal(err)
			}
			blocker, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = blocker.Rollback(ctx) }()
			if _, err := blocker.Exec(ctx, `SELECT id FROM library_collections WHERE id='cleanup-race' FOR UPDATE`); err != nil {
				t.Fatal(err)
			}
			if err := requestlock.LockItem(ctx, blocker, "movie-tmdb-3901"); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, err := NewItemRepository(pool).CleanupLegacyUnscopedCollectionClaims(ctx, 1); done <- err }()
			waitCatalogBlocked(t, ctx, pool, blocker.Conn().PgConn().PID())
			if _, err := blocker.Exec(ctx, `INSERT INTO library_collection_items(collection_id,media_item_id,position) VALUES('cleanup-race','movie-tmdb-3901',0)`); err != nil {
				t.Fatal(err)
			}
			if _, err := blocker.Exec(ctx, `UPDATE virtual_media_source_claims SET staged_until=NULL WHERE content_id='movie-tmdb-3901'`); err != nil {
				t.Fatal(err)
			}
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			var count int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM virtual_media_source_claims WHERE content_id='movie-tmdb-3901' AND staged_until IS NULL`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("renewed claims=%d error=%v", count, err)
			}
		})
	}
}

func TestReconcileVirtualMediaLocksItemBeforeClaims(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled) VALUES(3902,'Race','movies',true);
		INSERT INTO media_items(content_id,type,title,sort_title,status) VALUES('movie-tmdb-3902','movie','Race','Race','matched');
		INSERT INTO virtual_media_source_claims(plugin_installation_id,source_key,content_id,media_folder_id) VALUES(11,'race-source','movie-tmdb-3902',3902)`); err != nil {
		t.Fatal(err)
	}
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if err := requestlock.LockItem(ctx, blocker, "movie-tmdb-3902"); err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(ctx, `SELECT content_id FROM media_items WHERE content_id='movie-tmdb-3902' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := (&VirtualMediaRegistrar{pool: pool}).ReconcileVirtualMedia(ctx, 11, "race-source", []string{"movie-tmdb-9999"}, []int{3902})
		done <- err
	}()
	waitCatalogBlocked(t, ctx, pool, blocker.Conn().PgConn().PID())
	if _, err := blocker.Exec(ctx, `SELECT content_id FROM virtual_media_source_claims WHERE content_id='movie-tmdb-3902' FOR UPDATE NOWAIT`); err != nil {
		t.Fatalf("reconciliation locked claims before item: %v", err)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM virtual_media_source_claims WHERE content_id='movie-tmdb-3902'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("stale claims=%d error=%v", count, err)
	}
}

func TestCollectionSyncFetchFailureAndNormalAcceptance(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(969,'Sync','movies',true);
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config) VALUES('atomic-sync','atomic-sync','Sync','tmdb',969,'{"mode":"tmdb_preset","preset":"popular","virtual_playback":true}');
		INSERT INTO library_collection_libraries(collection_id,library_id) VALUES('atomic-sync',969)`); err != nil {
		t.Fatal(err)
	}
	service := NewLibraryCollectionService(NewLibraryCollectionRepository(pool), NewItemRepository(pool), NewLibraryItemRepository(pool), nil)
	service.TMDBDigitalReleases = &fakeDigitalReleaseChecker{released: map[int]bool{969: true}}
	fetcher := &mockTMDBFailSyncFetcher{err: errors.New("source unavailable"), entries: []TMDBCollectionEntry{{ID: 969, MediaType: "movie", Title: "Fresh", ReleaseDate: "2000-01-01"}}}
	service.TMDBCollections = fetcher
	service.VirtualVariants = func(context.Context, string, string) ([]VirtualPlaybackVariant, error) {
		return []VirtualPlaybackVariant{{OwnerInstallationID: 11}}, nil
	}
	if _, err := service.SyncCollectionWithOptions(ctx, "atomic-sync", SyncCollectionOptions{SkipCollage: true}); err == nil {
		t.Fatal("expected fetch failure")
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_items`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("source failure wrote catalog: count=%d error=%v", count, err)
	}
	fetcher.err = nil
	for range 2 {
		if _, err := service.SyncCollectionWithOptions(ctx, "atomic-sync", SyncCollectionOptions{SkipCollage: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM library_collection_items member JOIN virtual_media_file_source_claims claim ON claim.content_id=member.media_item_id AND claim.source_key='collection:atomic-sync' WHERE member.collection_id='atomic-sync' AND claim.staged_until IS NULL`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("accepted claims: count=%d error=%v", count, err)
	}
	service.TMDBDigitalReleases = &fakeDigitalReleaseChecker{err: context.DeadlineExceeded}
	if _, err := service.SyncCollectionWithOptions(ctx, "atomic-sync", SyncCollectionOptions{SkipCollage: true}); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("expected release lookup failure, got %v", err)
	}
	for _, query := range []string{
		`SELECT count(*) FROM library_collection_items WHERE collection_id='atomic-sync'`,
		`SELECT count(*) FROM virtual_media_file_source_claims WHERE source_key='collection:atomic-sync'`,
		`SELECT count(*) FROM media_files WHERE content_id='movie-tmdb-969'`,
	} {
		if err := pool.QueryRow(ctx, query).Scan(&count); err != nil || count != 1 {
			t.Fatalf("release outage changed accepted state: count=%d error=%v", count, err)
		}
	}
}

func TestCollectionAtomicAcceptanceRollbackAndRepair(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(970,'Atomic','movies',true);
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config) VALUES('atomic','atomic','Atomic','tmdb',970,'{"virtual_playback":true}');
		INSERT INTO library_collection_libraries(collection_id,library_id) VALUES('atomic',970)`); err != nil {
		t.Fatal(err)
	}
	items := NewItemRepository(pool)
	collections := NewLibraryCollectionRepository(pool)
	collection, err := collections.GetByID(ctx, "atomic")
	if err != nil {
		t.Fatal(err)
	}
	item := &models.MediaItem{ContentID: "movie-tmdb-970", Type: "movie", Title: "Atomic", SortTitle: "Atomic", TmdbID: "970", Status: "matched"}
	prepared := map[string]preparedCollectionItem{item.ContentID: {item: item, variants: []VirtualPlaybackVariant{{OwnerInstallationID: 11}, {OwnerInstallationID: 12, VirtualURI: "virtual://movie/tmdb/970?profile=HD"}}}}
	members := []LibraryCollectionItemInput{{MediaItemID: item.ContentID}, {MediaItemID: "missing-foreign-key"}}
	if err := collections.AcceptPreparedItems(ctx, collection, members, prepared, items); err == nil {
		t.Fatal("expected foreign key failure after materialization")
	}
	for _, table := range []string{"media_items", "media_files", "virtual_media_source_claims", "virtual_media_file_source_claims", "media_item_libraries", "library_collection_items", "metadata_refresh_debt"} {
		column := "content_id"
		if table == "library_collection_items" {
			column = "media_item_id"
		}
		var count int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE "+column+"=$1", item.ContentID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("rollback %s count=%d error=%v", table, count, err)
		}
	}
	members = members[:1]
	if err := collections.AcceptPreparedItems(ctx, collection, members, prepared, items); err != nil {
		t.Fatal(err)
	}
	var before string
	if err := pool.QueryRow(ctx, `SELECT string_agg(id::text,',' ORDER BY id) FROM media_files WHERE content_id=$1`, item.ContentID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM virtual_media_source_claims WHERE content_id=$1 AND plugin_installation_id=12`, item.ContentID); err != nil {
		t.Fatal(err)
	}
	if err := collections.AcceptPreparedItems(ctx, collection, members, prepared, items); err != nil {
		t.Fatal(err)
	}
	var after string
	var claims int
	if err := pool.QueryRow(ctx, `SELECT string_agg(id::text,',' ORDER BY id) FROM media_files WHERE content_id=$1`, item.ContentID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM virtual_media_file_source_claims WHERE content_id=$1 AND source_key='collection:atomic'`, item.ContentID).Scan(&claims); err != nil {
		t.Fatal(err)
	}
	if before != after || claims != 3 {
		t.Fatalf("repair file IDs %s -> %s, claims=%d", before, after, claims)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO virtual_media_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,owns_item_metadata) VALUES(11,'independent',$1,970,true)`, item.ContentID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO virtual_media_file_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,file_path) VALUES(11,'independent',$1,970,'virtual://movie/tmdb/970')`, item.ContentID); err != nil {
		t.Fatal(err)
	}
	if err := collections.AcceptPreparedItems(ctx, collection, nil, nil, items); err != nil {
		t.Fatal(err)
	}
	var independent, remaining int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM virtual_media_source_claims WHERE content_id=$1 AND source_key='independent'), (SELECT count(*) FROM media_files WHERE content_id=$1)`, item.ContentID).Scan(&independent, &remaining); err != nil || independent != 1 || remaining != 1 {
		t.Fatalf("independent ownership: claims=%d files=%d error=%v", independent, remaining, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE library_collections SET source_config='{"virtual_playback":false}' WHERE id='atomic'`); err != nil {
		t.Fatal(err)
	}
	if err := collections.AcceptPreparedItems(ctx, collection, nil, nil, items); !errors.Is(err, ErrCollectionSyncConfigurationChanged) {
		t.Fatalf("configuration fence: %v", err)
	}
}

func TestEnsureCollectionItemMaterialized_CreatesBaseAndReleasedEpisodesWithClaims(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(971,'Virtual Series','series',true)`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}

	const collectionID = "collection-971"
	const seriesID = "series-tvdb-971"

	sourceConfig, _ := json.Marshal(map[string]any{
		"virtual_playback": true,
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
		VALUES($1,$1,'Trending Shows','tmdb',971,$2)`, collectionID, sourceConfig); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_libraries(collection_id,library_id)
		VALUES($1,971)`, collectionID); err != nil {
		t.Fatalf("seed collection library: %v", err)
	}

	item := &models.MediaItem{
		ContentID: seriesID,
		Type:      "series",
		Title:     "Repairable Series",
		SortTitle: "Repairable Series",
		TvdbID:    "971",
		Status:    "matched",
	}
	if err := NewItemRepository(pool).Upsert(ctx, item); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_items(collection_id,media_item_id,position)
		VALUES($1,$2,1)`, collectionID, seriesID); err != nil {
		t.Fatalf("seed collection membership: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO seasons(content_id,series_id,season_number,title)
		VALUES('season-tvdb-971-1',$1,1,'Season 1')`, seriesID); err != nil {
		t.Fatalf("seed season: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO episodes(content_id,series_id,season_id,season_number,episode_number,title,air_date)
		VALUES
			('episode-tvdb-971-1-1',$1,'season-tvdb-971-1',1,1,'Released',CURRENT_DATE),
			('episode-tvdb-971-1-2',$1,'season-tvdb-971-1',1,2,'Future',CURRENT_DATE+1),
			('episode-tvdb-971-1-3',$1,'season-tvdb-971-1',1,3,'Unknown',NULL)`, seriesID); err != nil {
		t.Fatalf("seed episodes: %v", err)
	}

	repo := NewItemRepository(pool)
	collRepo := NewLibraryCollectionRepository(pool)
	service := NewLibraryCollectionService(collRepo, repo, nil, nil)
	service.VirtualVariants = func(_ context.Context, uri, mediaType string) ([]VirtualPlaybackVariant, error) {
		if uri != "virtual://series/tvdb/971" || mediaType != "series" {
			t.Fatalf("variant request = (%q, %q)", uri, mediaType)
		}
		return []VirtualPlaybackVariant{{OwnerInstallationID: 11}}, nil
	}
	collection := &models.LibraryCollection{
		ID:           collectionID,
		LibraryID:    971,
		LibraryIDs:   []int{971},
		SourceConfig: sourceConfig,
	}

	// First run: should create base and 1 released episode.
	res1, err := service.EnsureCollectionItemMaterialized(ctx, collection, item)
	if err != nil {
		t.Fatalf("first materialization: %v", err)
	}
	if res1.FilesCreated != 2 || res1.FilesExisting != 0 || res1.EpisodesMaterialized != 1 {
		t.Fatalf("res1 = %+v, want FilesCreated=2 FilesExisting=0 EpisodesMaterialized=1", res1)
	}

	// Verify database files: released episode present, future and unknown absent.
	var base, released, future, unknown int
	if err := pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER(WHERE episode_id IS NULL),
			count(*) FILTER(WHERE episode_id='episode-tvdb-971-1-1'),
			count(*) FILTER(WHERE episode_id='episode-tvdb-971-1-2'),
			count(*) FILTER(WHERE episode_id='episode-tvdb-971-1-3')
		FROM media_files
		WHERE content_id=$1 AND container='virtual' AND virtual_owner_installation_id=11`, seriesID).Scan(&base, &released, &future, &unknown); err != nil {
		t.Fatalf("inspect materialized files: %v", err)
	}
	if base != 1 || released != 1 || future != 0 || unknown != 0 {
		t.Fatalf("files base=%d released=%d future=%d unknown=%d, want 1/1/0/0", base, released, future, unknown)
	}

	// Verify ownership claims established for collection
	var baseClaims, epClaims int
	if err := pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER(WHERE file_path='virtual://series/tvdb/971'),
			count(*) FILTER(WHERE file_path='virtual://series/tvdb/971/1/1')
		FROM virtual_media_file_source_claims
		WHERE content_id=$1 AND source_key=$2 AND plugin_installation_id=11`, seriesID, "collection:"+collectionID).Scan(&baseClaims, &epClaims); err != nil {
		t.Fatalf("inspect claims: %v", err)
	}
	if baseClaims != 1 || epClaims != 1 {
		t.Fatalf("claims base=%d ep=%d, want 1/1", baseClaims, epClaims)
	}

	// Second run: idempotent, reporting 0 created, 2 existing, 1 episode
	res2, err := service.EnsureCollectionItemMaterialized(ctx, collection, item)
	if err != nil {
		t.Fatalf("idempotent materialization: %v", err)
	}
	if res2.FilesCreated != 0 || res2.FilesExisting != 2 || res2.EpisodesMaterialized != 1 {
		t.Fatalf("res2 = %+v, want FilesCreated=0 FilesExisting=2 EpisodesMaterialized=1", res2)
	}
}

func TestEnsureCollectionItemMaterialized_RejectsNonMember(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(971,'Virtual Series','series',true)`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}

	const collectionID = "collection-971"
	const seriesID = "series-tvdb-972"

	sourceConfig, _ := json.Marshal(map[string]any{"virtual_playback": true})
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
		VALUES($1,$1,'Trending Shows','tmdb',971,$2)`, collectionID, sourceConfig); err != nil {
		t.Fatalf("seed collection: %v", err)
	}

	item := &models.MediaItem{
		ContentID: seriesID,
		Type:      "series",
		Title:     "Non-member Series",
		SortTitle: "Non-member Series",
		TvdbID:    "972",
		Status:    "matched",
	}
	if err := NewItemRepository(pool).Upsert(ctx, item); err != nil {
		t.Fatalf("seed item: %v", err)
	}

	repo := NewItemRepository(pool)
	collRepo := NewLibraryCollectionRepository(pool)
	service := NewLibraryCollectionService(collRepo, repo, nil, nil)
	collection := &models.LibraryCollection{
		ID:           collectionID,
		LibraryID:    971,
		LibraryIDs:   []int{971},
		SourceConfig: sourceConfig,
	}

	service.VirtualVariants = func(context.Context, string, string) ([]VirtualPlaybackVariant, error) {
		return []VirtualPlaybackVariant{{OwnerInstallationID: 11}}, nil
	}
	_, err := service.EnsureCollectionItemMaterialized(ctx, collection, item)
	if err == nil || !errors.Is(err, ErrCollectionItemNotMember) {
		t.Fatalf("expected ErrCollectionItemNotMember, got: %v", err)
	}
}

func TestEnsureCollectionItemMaterialized_RejectsDisabledVirtualPlayback(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(971,'Virtual Series','series',true)`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}

	const collectionID = "collection-disabled"
	const seriesID = "series-tvdb-973"

	sourceConfig, _ := json.Marshal(map[string]any{"virtual_playback": false})
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
		VALUES($1,$1,'Standard Shows','tmdb',971,$2)`, collectionID, sourceConfig); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	item := &models.MediaItem{
		ContentID: seriesID,
		Type:      "series",
		Title:     "Disabled Item",
		TvdbID:    "973",
	}

	repo := NewItemRepository(pool)
	collRepo := NewLibraryCollectionRepository(pool)
	service := NewLibraryCollectionService(collRepo, repo, nil, nil)
	collection := &models.LibraryCollection{
		ID:           collectionID,
		LibraryID:    971,
		LibraryIDs:   []int{971},
		SourceConfig: sourceConfig,
	}

	_, err := service.EnsureCollectionItemMaterialized(ctx, collection, item)
	if err == nil {
		t.Fatal("expected error when virtual playback is disabled on collection, got nil")
	}
}

func TestEnsureCollectionItemMaterialized_RejectsIncompatibleLibrary(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()

	// Only movie folders exist
	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(981,'Movies Only','movies',true)`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}

	const collectionID = "collection-movies-only"
	const seriesID = "series-tvdb-974"

	sourceConfig, _ := json.Marshal(map[string]any{"virtual_playback": true})
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
		VALUES($1,$1,'Movies Collection','tmdb',981,$2)`, collectionID, sourceConfig); err != nil {
		t.Fatalf("seed collection: %v", err)
	}

	item := &models.MediaItem{
		ContentID: seriesID,
		Type:      "series",
		Title:     "Series In Movie Collection",
		TvdbID:    "974",
	}
	if err := NewItemRepository(pool).Upsert(ctx, item); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_items(collection_id,media_item_id,position)
		VALUES($1,$2,1)`, collectionID, seriesID); err != nil {
		t.Fatalf("seed collection membership: %v", err)
	}

	repo := NewItemRepository(pool)
	collRepo := NewLibraryCollectionRepository(pool)
	service := NewLibraryCollectionService(collRepo, repo, nil, nil)
	service.VirtualVariants = func(_ context.Context, _, _ string) ([]VirtualPlaybackVariant, error) {
		return []VirtualPlaybackVariant{{OwnerInstallationID: 11}}, nil
	}
	collection := &models.LibraryCollection{
		ID:           collectionID,
		LibraryID:    981,
		LibraryIDs:   []int{981},
		SourceConfig: sourceConfig,
	}

	_, err := service.EnsureCollectionItemMaterialized(ctx, collection, item)
	if err == nil || !errors.Is(err, ErrIncompatibleLibrary) {
		t.Fatalf("expected ErrIncompatibleLibrary, got: %v", err)
	}
}

func TestReconcileMissingCollectionVirtualItems_AutomaticHeal(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(971,'Virtual Series','series',true)`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}

	const collectionID = "collection-auto-heal"
	const seriesID = "series-tvdb-975"

	sourceConfig, _ := json.Marshal(map[string]any{"virtual_playback": true})
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
		VALUES($1,$1,'Auto Heal Shows','tmdb',971,$2)`, collectionID, sourceConfig); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_libraries(collection_id,library_id)
		VALUES($1,971)`, collectionID); err != nil {
		t.Fatalf("seed collection library: %v", err)
	}

	item := &models.MediaItem{
		ContentID: seriesID,
		Type:      "series",
		Title:     "Auto Heal Series",
		SortTitle: "Auto Heal Series",
		TvdbID:    "975",
		Status:    "matched",
	}
	if err := NewItemRepository(pool).Upsert(ctx, item); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_items(collection_id,media_item_id,position)
		VALUES($1,$2,1)`, collectionID, seriesID); err != nil {
		t.Fatalf("seed collection membership: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO seasons(content_id,series_id,season_number,title)
		VALUES('season-tvdb-975-1',$1,1,'Season 1')`, seriesID); err != nil {
		t.Fatalf("seed season: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO episodes(content_id,series_id,season_id,season_number,episode_number,title,air_date)
		VALUES ('episode-tvdb-975-1-1',$1,'season-tvdb-975-1',1,1,'Released',CURRENT_DATE)`, seriesID); err != nil {
		t.Fatalf("seed episode: %v", err)
	}

	repo := NewItemRepository(pool)
	collRepo := NewLibraryCollectionRepository(pool)
	service := NewLibraryCollectionService(collRepo, repo, nil, nil)
	service.VirtualVariants = func(_ context.Context, uri, mediaType string) ([]VirtualPlaybackVariant, error) {
		return []VirtualPlaybackVariant{{OwnerInstallationID: 11}}, nil
	}
	collection := &models.LibraryCollection{
		ID:           collectionID,
		LibraryID:    971,
		LibraryIDs:   []int{971},
		SourceConfig: sourceConfig,
	}

	// Routine reconciliation should discover the series missing its virtual base and heal it.
	repaired, err := service.ReconcileMissingCollectionVirtualItems(ctx, collection)
	if err != nil {
		t.Fatalf("reconcile missing virtual items: %v", err)
	}
	if repaired != 1 {
		t.Fatalf("repaired = %d, want 1", repaired)
	}

	// Verify base and episode files exist now
	var count int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM media_files
		WHERE content_id=$1 AND container='virtual' AND virtual_owner_installation_id=11`, seriesID).Scan(&count); err != nil {
		t.Fatalf("count files: %v", err)
	}
	if count != 2 {
		t.Fatalf("file count = %d, want 2 (base + 1 episode)", count)
	}

	// Running reconciliation again finds nothing missing
	repaired2, err := service.ReconcileMissingCollectionVirtualItems(ctx, collection)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if repaired2 != 0 {
		t.Fatalf("repaired2 = %d, want 0", repaired2)
	}
}

func TestEnsureCollectionItemMaterialized_PreservesLocalFiles(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(991,'Local Movies','movies',true)`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}

	const collectionID = "collection-preserve-local"
	const movieID = "movie-tmdb-991"

	sourceConfig, _ := json.Marshal(map[string]any{"virtual_playback": true})
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
		VALUES($1,$1,'Local Preservation','tmdb',991,$2)`, collectionID, sourceConfig); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_libraries(collection_id,library_id)
		VALUES($1,991)`, collectionID); err != nil {
		t.Fatalf("seed collection library: %v", err)
	}

	item := &models.MediaItem{
		ContentID: movieID,
		Type:      "movie",
		Title:     "Local Preserved Movie",
		SortTitle: "Local Preserved Movie",
		Year:      2020,
		TmdbID:    "991",
		Status:    "matched",
	}
	if err := NewItemRepository(pool).Upsert(ctx, item); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_items(collection_id,media_item_id,position)
		VALUES($1,$2,1)`, collectionID, movieID); err != nil {
		t.Fatalf("seed collection membership: %v", err)
	}

	// Seed existing local file
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container)
		VALUES($1,991,'/movies/local-991.mkv',100000,'mkv')`, movieID); err != nil {
		t.Fatalf("seed local file: %v", err)
	}

	repo := NewItemRepository(pool)
	collRepo := NewLibraryCollectionRepository(pool)
	service := NewLibraryCollectionService(collRepo, repo, nil, nil)
	service.TMDBDigitalReleases = &fakeDigitalReleaseChecker{released: map[int]bool{991: true}}
	service.VirtualVariants = func(_ context.Context, uri, mediaType string) ([]VirtualPlaybackVariant, error) {
		return []VirtualPlaybackVariant{{OwnerInstallationID: 11}}, nil
	}
	collection := &models.LibraryCollection{
		ID:           collectionID,
		LibraryID:    991,
		LibraryIDs:   []int{991},
		SourceConfig: sourceConfig,
	}

	res, err := service.EnsureCollectionItemMaterialized(ctx, collection, item)
	if err != nil {
		t.Fatalf("ensure materialized: %v", err)
	}
	if res.FilesCreated != 1 {
		t.Fatalf("res.FilesCreated = %d, want 1", res.FilesCreated)
	}

	// Local file must be untouched
	var localCount, virtualCount int
	if err := pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE file_path = '/movies/local-991.mkv' AND container = 'mkv'),
			count(*) FILTER (WHERE file_path = 'virtual://movie/tmdb/991' AND container = 'virtual')
		FROM media_files WHERE content_id=$1`, movieID).Scan(&localCount, &virtualCount); err != nil {
		t.Fatalf("inspect files: %v", err)
	}
	if localCount != 1 || virtualCount != 1 {
		t.Fatalf("files local=%d virtual=%d, want 1/1", localCount, virtualCount)
	}
}

func TestMaterializeVirtualPlaybackEpisodes_RequestOwnedSeries(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(971,'Virtual Series','series',true)`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}

	const seriesID = "series-tvdb-req-1"
	item := &models.MediaItem{
		ContentID: seriesID,
		Type:      "series",
		Title:     "Request Owned Series",
		SortTitle: "Request Owned Series",
		TvdbID:    "9999",
		Status:    "matched",
	}
	if err := NewItemRepository(pool).Upsert(ctx, item); err != nil {
		t.Fatalf("seed item: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO seasons(content_id,series_id,season_number,title)
		VALUES('season-tvdb-req-1-1',$1,1,'Season 1')`, seriesID); err != nil {
		t.Fatalf("seed season: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO episodes(content_id,series_id,season_id,season_number,episode_number,title,air_date)
		VALUES ('episode-tvdb-req-1-1-1',$1,'season-tvdb-req-1-1',1,1,'Pilot',CURRENT_DATE)`, seriesID); err != nil {
		t.Fatalf("seed episode: %v", err)
	}

	// Seed request-owned virtual series base file
	const basePath = "virtual://series/tvdb/9999"
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,probe_source,virtual_owner_installation_id)
		VALUES($1,971,$2,0,'virtual','virtual',11)`, seriesID, basePath); err != nil {
		t.Fatalf("seed base file: %v", err)
	}

	// Seed source claim: request-owned, NOT collection-owned!
	if _, err := pool.Exec(ctx, `
		INSERT INTO virtual_media_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,owns_item_metadata)
		VALUES(11,'request:user-42',$1,971,false)`, seriesID); err != nil {
		t.Fatalf("seed source claim: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO virtual_media_file_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,file_path)
		VALUES(11,'request:user-42',$1,971,$2)`, seriesID, basePath); err != nil {
		t.Fatalf("seed file claim: %v", err)
	}

	repo := NewItemRepository(pool)
	// Now call MaterializeVirtualPlaybackEpisodes (as metadata refresh does!)
	err := repo.MaterializeVirtualPlaybackEpisodes(ctx, seriesID)
	if err != nil {
		t.Fatalf("MaterializeVirtualPlaybackEpisodes failed: %v", err)
	}
}

func TestVirtualCollection_MultiCollectionSharedClaimAndRemoval(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(985,'Movies Folder','movies',true)`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}

	const col1ID = "collection-shared-1"
	const col2ID = "collection-shared-2"
	const movieID = "movie-shared-985"

	cfg, _ := json.Marshal(map[string]any{"virtual_playback": true})
	for _, cID := range []string{col1ID, col2ID} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
			VALUES($1,$1,$1,'tmdb',985,$2)`, cID, cfg); err != nil {
			t.Fatalf("seed collection %s: %v", cID, err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO library_collection_libraries(collection_id,library_id)
			VALUES($1,985)`, cID); err != nil {
			t.Fatalf("seed collection library %s: %v", cID, err)
		}
	}

	item := &models.MediaItem{
		ContentID: movieID,
		Type:      "movie",
		Title:     "Shared Virtual Movie",
		SortTitle: "Shared Virtual Movie",
		Year:      2020,
		TmdbID:    "985",
		Status:    "matched",
	}
	if err := NewItemRepository(pool).Upsert(ctx, item); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	for _, cID := range []string{col1ID, col2ID} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO library_collection_items(collection_id,media_item_id,position)
			VALUES($1,$2,1)`, cID, movieID); err != nil {
			t.Fatalf("seed membership %s: %v", cID, err)
		}
	}

	repo := NewItemRepository(pool)
	collRepo := NewLibraryCollectionRepository(pool)
	service := NewLibraryCollectionService(collRepo, repo, nil, nil)
	service.TMDBDigitalReleases = &fakeDigitalReleaseChecker{released: map[int]bool{985: true}}
	service.VirtualVariants = func(_ context.Context, _, _ string) ([]VirtualPlaybackVariant, error) {
		return []VirtualPlaybackVariant{{OwnerInstallationID: 11}}, nil
	}

	col1 := &models.LibraryCollection{ID: col1ID, LibraryID: 985, LibraryIDs: []int{985}, SourceConfig: cfg}
	col2 := &models.LibraryCollection{ID: col2ID, LibraryID: 985, LibraryIDs: []int{985}, SourceConfig: cfg}

	// Materialize under col1
	res1, err := service.EnsureCollectionItemMaterialized(ctx, col1, item)
	if err != nil {
		t.Fatalf("col1 materialization: %v", err)
	}
	if res1.FilesCreated != 1 {
		t.Fatalf("res1.FilesCreated = %d, want 1", res1.FilesCreated)
	}

	// Materialize under col2
	res2, err := service.EnsureCollectionItemMaterialized(ctx, col2, item)
	if err != nil {
		t.Fatalf("col2 materialization: %v", err)
	}
	if res2.FilesCreated != 0 || res2.FilesExisting != 1 {
		t.Fatalf("res2 = %+v, want FilesCreated=0 FilesExisting=1", res2)
	}

	// Verify claims exist for BOTH col1 and col2
	var col1Claim, col2Claim int
	if err := pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER(WHERE source_key = 'collection:' || $2),
			count(*) FILTER(WHERE source_key = 'collection:' || $3)
		FROM virtual_media_file_source_claims
		WHERE content_id = $1`, movieID, col1ID, col2ID).Scan(&col1Claim, &col2Claim); err != nil {
		t.Fatalf("inspect claims: %v", err)
	}
	if col1Claim != 1 || col2Claim != 1 {
		t.Fatalf("claims col1=%d col2=%d, want 1/1", col1Claim, col2Claim)
	}

	// Remove movieID from col1 using RemoveItem (single-item removal)
	if err := collRepo.RemoveItem(ctx, col1ID, movieID); err != nil {
		t.Fatalf("remove item from col1: %v", err)
	}

	// Verify membership: movieID removed from col1, but col1 itself still exists!
	var col1Exists, col1Member bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM library_collections WHERE id=$1)`, col1ID).Scan(&col1Exists); err != nil {
		t.Fatalf("check col1 exists: %v", err)
	}
	if !col1Exists {
		t.Fatalf("col1 was unexpectedly deleted by single item removal")
	}
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM library_collection_items WHERE collection_id=$1 AND media_item_id=$2)`, col1ID, movieID).Scan(&col1Member); err != nil {
		t.Fatalf("check col1 member: %v", err)
	}
	if col1Member {
		t.Fatalf("movieID still member of col1 after RemoveItem")
	}

	// Check files: media_files row MUST STILL EXIST because col2 still claims it!
	var fileCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_files WHERE content_id = $1`, movieID).Scan(&fileCount); err != nil {
		t.Fatalf("inspect files after col1 removal: %v", err)
	}
	if fileCount != 1 {
		t.Fatalf("fileCount = %d after col1 removal, want 1 (preserved by col2)", fileCount)
	}

	// Check claims: col1 claim gone, col2 claim intact
	if err := pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER(WHERE source_key = 'collection:' || $2),
			count(*) FILTER(WHERE source_key = 'collection:' || $3)
		FROM virtual_media_file_source_claims
		WHERE content_id = $1`, movieID, col1ID, col2ID).Scan(&col1Claim, &col2Claim); err != nil {
		t.Fatalf("inspect claims after col1 removal: %v", err)
	}
	if col1Claim != 0 || col2Claim != 1 {
		t.Fatalf("after col1 removal: col1=%d col2=%d, want 0/1", col1Claim, col2Claim)
	}

	// Now remove movieID from col2 using RemoveItem
	if err := collRepo.RemoveItem(ctx, col2ID, movieID); err != nil {
		t.Fatalf("remove item from col2: %v", err)
	}

	// Now media_files row should be deleted since no claims remain
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_files WHERE content_id = $1`, movieID).Scan(&fileCount); err != nil {
		t.Fatalf("inspect files after col2 removal: %v", err)
	}
	if fileCount != 0 {
		t.Fatalf("fileCount = %d after col2 removal, want 0", fileCount)
	}

	// And media_items row should also be cleaned up as orphaned virtual item
	var itemCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_items WHERE content_id = $1`, movieID).Scan(&itemCount); err != nil {
		t.Fatalf("inspect media_items after col2 removal: %v", err)
	}
	if itemCount != 0 {
		t.Fatalf("itemCount = %d after final removal, want 0 (cleaned up)", itemCount)
	}
}

func TestVirtualCollection_Concurrency_SyncAndRepair(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(986,'Series Folder','series',true)`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}

	const collectionID = "collection-concurrent-sync"
	const seriesID = "series-tvdb-986"

	cfg, _ := json.Marshal(map[string]any{"virtual_playback": true})
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
		VALUES($1,$1,$1,'tmdb',986,$2)`, collectionID, cfg); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_libraries(collection_id,library_id)
		VALUES($1,986)`, collectionID); err != nil {
		t.Fatalf("seed collection library: %v", err)
	}

	item := &models.MediaItem{
		ContentID: seriesID,
		Type:      "series",
		Title:     "Concurrent Series",
		TvdbID:    "986",
		Status:    "matched",
	}
	if err := NewItemRepository(pool).Upsert(ctx, item); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_items(collection_id,media_item_id,position)
		VALUES($1,$2,1)`, collectionID, seriesID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO seasons(content_id,series_id,season_number,title)
		VALUES('season-tvdb-986-1',$1,1,'Season 1')`, seriesID); err != nil {
		t.Fatalf("seed season: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO episodes(content_id,series_id,season_id,season_number,episode_number,title,air_date)
		VALUES ('episode-tvdb-986-1-1',$1,'season-tvdb-986-1',1,1,'Ep 1',CURRENT_DATE)`, seriesID); err != nil {
		t.Fatalf("seed episode: %v", err)
	}

	repo := NewItemRepository(pool)
	collRepo := NewLibraryCollectionRepository(pool)
	service := NewLibraryCollectionService(collRepo, repo, nil, nil)
	service.VirtualVariants = func(_ context.Context, _, _ string) ([]VirtualPlaybackVariant, error) {
		return []VirtualPlaybackVariant{{OwnerInstallationID: 11}}, nil
	}
	collection := &models.LibraryCollection{
		ID:           collectionID,
		LibraryID:    986,
		LibraryIDs:   []int{986},
		SourceConfig: cfg,
	}

	const concurrency = 6
	errs := make(chan error, concurrency)
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := service.EnsureCollectionItemMaterialized(ctx, collection, item)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent materialization failed: %v", err)
		}
	}

	// Verify exact count: 1 base file and 1 episode file (no duplicates)
	var baseCount, epCount int
	if err := pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER(WHERE episode_id IS NULL),
			count(*) FILTER(WHERE episode_id IS NOT NULL)
		FROM media_files WHERE content_id = $1`, seriesID).Scan(&baseCount, &epCount); err != nil {
		t.Fatalf("inspect files: %v", err)
	}
	if baseCount != 1 || epCount != 1 {
		t.Fatalf("base=%d ep=%d, want 1/1", baseCount, epCount)
	}
}

func TestVirtualCollection_Concurrency_RepairAndRemove(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(987,'Movies Folder','movies',true)`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}

	const collectionID = "collection-concurrent-remove"
	const movieID = "movie-tmdb-987"

	cfg, _ := json.Marshal(map[string]any{"virtual_playback": true})
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
		VALUES($1,$1,$1,'tmdb',987,$2)`, collectionID, cfg); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_libraries(collection_id,library_id)
		VALUES($1,987)`, collectionID); err != nil {
		t.Fatalf("seed collection library: %v", err)
	}

	item := &models.MediaItem{
		ContentID: movieID,
		Type:      "movie",
		Title:     "Concurrent Remove Movie",
		Year:      2020,
		TmdbID:    "987",
		Status:    "matched",
	}
	if err := NewItemRepository(pool).Upsert(ctx, item); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_items(collection_id,media_item_id,position)
		VALUES($1,$2,1)`, collectionID, movieID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	repo := NewItemRepository(pool)
	collRepo := NewLibraryCollectionRepository(pool)
	service := NewLibraryCollectionService(collRepo, repo, nil, nil)
	service.TMDBDigitalReleases = &fakeDigitalReleaseChecker{released: map[int]bool{987: true}}
	service.VirtualVariants = func(_ context.Context, _, _ string) ([]VirtualPlaybackVariant, error) {
		return []VirtualPlaybackVariant{{OwnerInstallationID: 11}}, nil
	}
	collection := &models.LibraryCollection{
		ID:           collectionID,
		LibraryID:    987,
		LibraryIDs:   []int{987},
		SourceConfig: cfg,
	}

	var wg sync.WaitGroup
	start := make(chan struct{})

	// Goroutine 1: Repair
	var repairErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		_, repairErr = service.EnsureCollectionItemMaterialized(ctx, collection, item)
	}()

	// Goroutine 2: Remove collection
	var deleteErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		deleteErr = collRepo.Delete(ctx, collectionID)
	}()

	close(start)
	wg.Wait()

	if deleteErr != nil {
		t.Fatalf("delete collection failed: %v", deleteErr)
	}
	// If repair ran after delete, it should fail with ErrCollectionItemNotMember or ErrLibraryCollectionNotFound, never panic or FK error
	if repairErr != nil {
		if !errors.Is(repairErr, ErrCollectionItemNotMember) && !errors.Is(repairErr, ErrLibraryCollectionNotFound) {
			t.Fatalf("unexpected repair error: %v", repairErr)
		}
	}

	// In the end, no orphaned claims should exist for this deleted collection
	var orphanedClaims int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM virtual_media_file_source_claims
		WHERE source_key = 'collection:' || $1`, collectionID).Scan(&orphanedClaims); err != nil {
		t.Fatalf("inspect orphaned claims: %v", err)
	}
	if orphanedClaims != 0 {
		t.Fatalf("orphanedClaims = %d, want 0", orphanedClaims)
	}
}

func TestVirtualCollection_StrictReleaseRules(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(988,'Series Folder','series',true)`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}

	const collectionID = "collection-strict-release"
	const seriesID = "series-tvdb-988"

	cfg, _ := json.Marshal(map[string]any{"virtual_playback": true})
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
		VALUES($1,$1,$1,'tmdb',988,$2)`, collectionID, cfg); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_libraries(collection_id,library_id)
		VALUES($1,988)`, collectionID); err != nil {
		t.Fatalf("seed collection library: %v", err)
	}

	item := &models.MediaItem{
		ContentID: seriesID,
		Type:      "series",
		Title:     "Strict Release Series",
		TvdbID:    "988",
		Status:    "matched",
	}
	if err := NewItemRepository(pool).Upsert(ctx, item); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_items(collection_id,media_item_id,position)
		VALUES($1,$2,1)`, collectionID, seriesID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO seasons(content_id,series_id,season_number,title)
		VALUES
			('season-988-0',$1,0,'Specials'),
			('season-988-1',$1,1,'Season 1')`, seriesID); err != nil {
		t.Fatalf("seed seasons: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO episodes(content_id,series_id,season_id,season_number,episode_number,title,air_date)
		VALUES
			('ep-988-0-1',$1,'season-988-0',0,1,'Special 1',CURRENT_DATE - 10),
			('ep-988-1-0',$1,'season-988-1',1,0,'Ep 0',CURRENT_DATE - 5),
			('ep-988-1-1',$1,'season-988-1',1,1,'Released 1',CURRENT_DATE - 2),
			('ep-988-1-2',$1,'season-988-1',1,2,'Released Today',CURRENT_DATE),
			('ep-988-1-3',$1,'season-988-1',1,3,'Future',CURRENT_DATE + 3),
			('ep-988-1-4',$1,'season-988-1',1,4,'Unknown',NULL)`, seriesID); err != nil {
		t.Fatalf("seed episodes: %v", err)
	}

	repo := NewItemRepository(pool)
	collRepo := NewLibraryCollectionRepository(pool)
	service := NewLibraryCollectionService(collRepo, repo, nil, nil)
	service.VirtualVariants = func(_ context.Context, _, _ string) ([]VirtualPlaybackVariant, error) {
		return []VirtualPlaybackVariant{{OwnerInstallationID: 11}}, nil
	}
	collection := &models.LibraryCollection{
		ID:           collectionID,
		LibraryID:    988,
		LibraryIDs:   []int{988},
		SourceConfig: cfg,
	}

	res, err := service.EnsureCollectionItemMaterialized(ctx, collection, item)
	if err != nil {
		t.Fatalf("ensure materialized: %v", err)
	}
	// Exactly 2 episodes should be materialized: ep-988-1-1 and ep-988-1-2. Plus 1 base file = 3 files.
	if res.FilesCreated != 3 || res.EpisodesMaterialized != 2 {
		t.Fatalf("res = %+v, want FilesCreated=3, EpisodesMaterialized=2", res)
	}

	// Verify exact database rows
	var ep1, ep2, epFuture, epUnknown, epSpecial, epZero int
	if err := pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER(WHERE episode_id='ep-988-1-1'),
			count(*) FILTER(WHERE episode_id='ep-988-1-2'),
			count(*) FILTER(WHERE episode_id='ep-988-1-3'),
			count(*) FILTER(WHERE episode_id='ep-988-1-4'),
			count(*) FILTER(WHERE episode_id='ep-988-0-1'),
			count(*) FILTER(WHERE episode_id='ep-988-1-0')
		FROM media_files WHERE content_id = $1`, seriesID).Scan(&ep1, &ep2, &epFuture, &epUnknown, &epSpecial, &epZero); err != nil {
		t.Fatalf("inspect files: %v", err)
	}
	if ep1 != 1 || ep2 != 1 || epFuture != 0 || epUnknown != 0 || epSpecial != 0 || epZero != 0 {
		t.Fatalf("episodes: ep1=%d ep2=%d epFuture=%d epUnknown=%d epSpecial=%d epZero=%d, want 1/1/0/0/0/0",
			ep1, ep2, epFuture, epUnknown, epSpecial, epZero)
	}

	// Now simulate episode release: ep-988-1-3 air_date arrives
	if _, err := pool.Exec(ctx, `UPDATE episodes SET air_date = CURRENT_DATE WHERE content_id = 'ep-988-1-3'`); err != nil {
		t.Fatalf("update air_date: %v", err)
	}

	// Routine reconciliation should detect missing episode and heal it
	repaired, err := service.ReconcileMissingCollectionVirtualItems(ctx, collection)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if repaired != 1 {
		t.Fatalf("repaired = %d, want 1", repaired)
	}

	// ep-988-1-3 should now exist
	var ep3 int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_files WHERE episode_id='ep-988-1-3'`).Scan(&ep3); err != nil {
		t.Fatalf("query ep3: %v", err)
	}
	if ep3 != 1 {
		t.Fatalf("ep3 = %d, want 1", ep3)
	}

	// Reconciling again converges to 0 repairs
	repaired2, err := service.ReconcileMissingCollectionVirtualItems(ctx, collection)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if repaired2 != 0 {
		t.Fatalf("repaired2 = %d, want 0", repaired2)
	}
}

func TestVirtualCollection_ProviderFailureResilienceAndStarvation(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(989,'Movies Folder','movies',true)`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}

	const collectionID = "collection-starvation"
	cfg, _ := json.Marshal(map[string]any{"virtual_playback": true})
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
		VALUES($1,$1,$1,'tmdb',989,$2)`, collectionID, cfg); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_libraries(collection_id,library_id)
		VALUES($1,989)`, collectionID); err != nil {
		t.Fatalf("seed collection library: %v", err)
	}

	repo := NewItemRepository(pool)
	collRepo := NewLibraryCollectionRepository(pool)

	items := make([]*models.MediaItem, 55)
	membershipInputs := make([]LibraryCollectionItemInput, 55)
	for i := 0; i < 55; i++ {
		cid := fmt.Sprintf("movie-starve-%02d", i+1)
		title := fmt.Sprintf("Starve Movie %02d", i+1)
		tmdbID := fmt.Sprintf("989%02d", i+1)
		it := &models.MediaItem{ContentID: cid, Type: "movie", Title: title, Year: 2020, TmdbID: tmdbID, Status: "matched"}
		items[i] = it
		if err := repo.Upsert(ctx, it); err != nil {
			t.Fatalf("seed item %d: %v", i, err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO library_collection_items(collection_id,media_item_id,position,updated_at)
			VALUES($1,$2,$3,NOW() - INTERVAL '10 minutes' * (60 - $3))`, collectionID, cid, i+1); err != nil {
			t.Fatalf("seed membership %d: %v", i, err)
		}
		membershipInputs[i] = LibraryCollectionItemInput{
			MediaItemID: cid,
			Position:    i,
			SourceRank:  i + 1,
		}
	}

	var providerHealthy atomic.Bool
	providerHealthy.Store(false)

	checker := &fakeDigitalReleaseChecker{released: map[int]bool{}}
	for i := 1; i <= 55; i++ {
		checker.released[98900+i] = true
	}
	service := NewLibraryCollectionService(collRepo, repo, nil, nil)
	service.TMDBDigitalReleases = checker
	service.VirtualVariants = func(_ context.Context, uri, mediaType string) ([]VirtualPlaybackVariant, error) {
		// First 50 items (1..50) fail when providerHealthy is false
		for i := 1; i <= 50; i++ {
			if uri == fmt.Sprintf("virtual://movie/tmdb/989%02d", i) && !providerHealthy.Load() {
				return nil, errors.New("upstream provider timeout")
			}
		}
		return []VirtualPlaybackVariant{{OwnerInstallationID: 11}}, nil
	}
	collection := &models.LibraryCollection{
		ID:           collectionID,
		LibraryID:    989,
		LibraryIDs:   []int{989},
		SourceConfig: cfg,
	}

	// Batch 1: ReconcileMissingCollectionVirtualItems processes first batch of 50 items.
	// All 50 fail, but their durable reconciliation attempt timestamps are touched.
	repaired1, err1 := service.ReconcileMissingCollectionVirtualItems(ctx, collection)
	if err1 == nil {
		t.Fatal("expected error in batch 1 due to 50 failing items, got nil")
	}
	if repaired1 != 0 {
		t.Fatalf("repaired1 = %d, want 0", repaired1)
	}

	// Now simulate periodic collection sync running, which calls ReplaceItems.
	// ReplaceItems resets library_collection_items rows and their updated_at timestamps,
	// but the independent reconciliation attempt timestamps remain intact.
	if err := collRepo.ReplaceItems(ctx, collectionID, membershipInputs); err != nil {
		t.Fatalf("ReplaceItems between batches: %v", err)
	}

	// Batch 2: Should prioritize items 51..55 because they have no reconciliation attempt yet,
	// preventing starvation even after ReplaceItems! Items 51..55 succeed!
	repaired2, err2 := service.ReconcileMissingCollectionVirtualItems(ctx, collection)
	if repaired2 < 5 {
		t.Fatalf("repaired2 = %d, want at least 5 (items 51-55 must not be starved by 50 failing items, err=%v)", repaired2, err2)
	}

	// Verify items 51..55 were materialized
	var healthyCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM media_files
		WHERE content_id IN ('movie-starve-51', 'movie-starve-52', 'movie-starve-53', 'movie-starve-54', 'movie-starve-55')
		  AND container = 'virtual'`).Scan(&healthyCount); err != nil {
		t.Fatalf("inspect healthy files: %v", err)
	}
	if healthyCount != 5 {
		t.Fatalf("healthyCount = %d, want 5", healthyCount)
	}

	// Provider recovers!
	providerHealthy.Store(true)

	// Subsequent reconciliation heals remaining items
	repaired3, err3 := service.ReconcileMissingCollectionVirtualItems(ctx, collection)
	if err3 != nil {
		t.Fatalf("reconcile after recovery: %v", err3)
	}
	if repaired3 != 50 {
		t.Fatalf("repaired3 = %d, want 50 healed items", repaired3)
	}
}

func TestMaterializeVirtualPlaybackEpisodes_RequestOwnedSeriesDoesNotInventCollectionClaim(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()

	// Seed two folders: folder 971 (request) and folder 972 (collection)
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled)
		VALUES (971,'Request Series','series',true), (972,'Collection Series','series',true)
		ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("seed folders: %v", err)
	}

	const seriesID = "series-tvdb-req-no-invent"
	item := &models.MediaItem{
		ContentID: seriesID,
		Type:      "series",
		Title:     "Request Series No Invent",
		SortTitle: "Request Series No Invent",
		TvdbID:    "9888",
		Status:    "matched",
	}
	repo := NewItemRepository(pool)
	if err := repo.Upsert(ctx, item); err != nil {
		t.Fatalf("seed item: %v", err)
	}

	// Seed episode: S01E01 released
	if _, err := pool.Exec(ctx, `
		INSERT INTO seasons(content_id,series_id,season_number,title)
		VALUES('season-tvdb-req-no-invent-1',$1,1,'Season 1')`, seriesID); err != nil {
		t.Fatalf("seed season: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO episodes(content_id,series_id,season_id,season_number,episode_number,title,air_date)
		VALUES ('episode-tvdb-req-no-invent-1-1',$1,'season-tvdb-req-no-invent-1',1,1,'Released Ep',CURRENT_DATE)`, seriesID); err != nil {
		t.Fatalf("seed episode: %v", err)
	}

	// Seed request-owned base in folder 971
	const basePath = "virtual://series/tvdb/9888"
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,probe_source,virtual_owner_installation_id)
		VALUES($1,971,$2,0,'virtual','virtual',11)`, seriesID, basePath); err != nil {
		t.Fatalf("seed request base: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO virtual_media_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,owns_item_metadata)
		VALUES(11,'request:user-99',$1,971,false)`, seriesID); err != nil {
		t.Fatalf("seed request source claim: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO virtual_media_file_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,file_path)
		VALUES(11,'request:user-99',$1,971,$2)`, seriesID, basePath); err != nil {
		t.Fatalf("seed request file claim: %v", err)
	}

	// Now configure collection-972 targeting folder 972 ONLY
	const collectionID = "collection-972"
	sourceConfig, _ := json.Marshal(map[string]any{"virtual_playback": true})
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
		VALUES($1,$1,'Collection 972','tmdb',972,$2)`, collectionID, sourceConfig); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_libraries(collection_id,library_id)
		VALUES($1,972)`, collectionID); err != nil {
		t.Fatalf("seed collection library: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_items(collection_id,media_item_id,position)
		VALUES($1,$2,1)`, collectionID, seriesID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	collRepo := NewLibraryCollectionRepository(pool)
	service := NewLibraryCollectionService(collRepo, repo, nil, nil)
	service.VirtualVariants = func(_ context.Context, _, _ string) ([]VirtualPlaybackVariant, error) {
		return []VirtualPlaybackVariant{{OwnerInstallationID: 11}}, nil
	}
	collection := &models.LibraryCollection{
		ID:           collectionID,
		LibraryID:    972,
		LibraryIDs:   []int{972},
		SourceConfig: sourceConfig,
	}

	// Materialize item for collection-972
	res, err := service.EnsureCollectionItemMaterialized(ctx, collection, item)
	if err != nil {
		t.Fatalf("EnsureCollectionItemMaterialized failed: %v", err)
	}
	if res.FilesCreated != 2 { // base in 972 + ep in 972
		t.Fatalf("FilesCreated = %d, want 2", res.FilesCreated)
	}

	// Verify claims in folder 971: MUST ONLY have request claim, NO collection claim!
	var colClaimsInFolder971 int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM virtual_media_file_source_claims
		WHERE content_id=$1 AND media_folder_id=971 AND source_key=$2`, seriesID, "collection:"+collectionID).Scan(&colClaimsInFolder971); err != nil {
		t.Fatalf("query folder 971 claims: %v", err)
	}
	if colClaimsInFolder971 != 0 {
		t.Fatalf("colClaimsInFolder971 = %d, want 0 (must NOT invent collection claim in request folder)", colClaimsInFolder971)
	}

	// Verify claims in folder 972: MUST have collection claim
	var colClaimsInFolder972 int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM virtual_media_file_source_claims
		WHERE content_id=$1 AND media_folder_id=972 AND source_key=$2`, seriesID, "collection:"+collectionID).Scan(&colClaimsInFolder972); err != nil {
		t.Fatalf("query folder 972 claims: %v", err)
	}
	if colClaimsInFolder972 != 2 { // base + episode
		t.Fatalf("colClaimsInFolder972 = %d, want 2", colClaimsInFolder972)
	}
}

func TestReconcileReleasedCollectionVirtualEpisodes_MultipleFoldersAndStrictRules(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()

	// Seed two folders: 981 and 982
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled)
		VALUES (981,'Multi 1','series',true), (982,'Multi 2','series',true)
		ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("seed folders: %v", err)
	}

	const seriesID = "series-tvdb-multi-strict"
	item := &models.MediaItem{
		ContentID: seriesID,
		Type:      "series",
		Title:     "Multi Strict Series",
		SortTitle: "Multi Strict Series",
		TvdbID:    "9889",
		Status:    "matched",
	}
	repo := NewItemRepository(pool)
	if err := repo.Upsert(ctx, item); err != nil {
		t.Fatalf("seed item: %v", err)
	}

	// Seed seasons and episodes:
	// - S01E01: Released (CURRENT_DATE) -> eligible
	// - S01E02: Future (CURRENT_DATE + 1) -> ineligible
	// - S01E03: Unknown (air_date NULL) -> ineligible
	// - S00E01: Special (season 0) -> ineligible
	if _, err := pool.Exec(ctx, `
		INSERT INTO seasons(content_id,series_id,season_number,title)
		VALUES ('season-multi-s1',$1,1,'Season 1'), ('season-multi-s0',$1,0,'Specials')`, seriesID); err != nil {
		t.Fatalf("seed seasons: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO episodes(content_id,series_id,season_id,season_number,episode_number,title,air_date)
		VALUES
			('ep-multi-1-1',$1,'season-multi-s1',1,1,'Released',CURRENT_DATE),
			('ep-multi-1-2',$1,'season-multi-s1',1,2,'Future',CURRENT_DATE+1),
			('ep-multi-1-3',$1,'season-multi-s1',1,3,'Unknown',NULL),
			('ep-multi-0-1',$1,'season-multi-s0',0,1,'Special',CURRENT_DATE)`, seriesID); err != nil {
		t.Fatalf("seed episodes: %v", err)
	}

	const colID = "col-multi-strict"
	cfg, _ := json.Marshal(map[string]any{"virtual_playback": true})
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
		VALUES($1,$1,$1,'tmdb',981,$2) ON CONFLICT (id) DO NOTHING`, colID, cfg); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_libraries(collection_id,library_id)
		VALUES($1,981), ($1,982) ON CONFLICT DO NOTHING`, colID); err != nil {
		t.Fatalf("seed collection libraries: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_items(collection_id,media_item_id,position)
		VALUES($1,$2,1) ON CONFLICT DO NOTHING`, colID, seriesID); err != nil {
		t.Fatalf("seed collection membership: %v", err)
	}

	const basePath = "virtual://series/tvdb/9889"
	// Seed bases in BOTH folders 981 and 982
	for _, fID := range []int{981, 982} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,probe_source,virtual_owner_installation_id)
			VALUES($1,$2,$3,0,'virtual','virtual_collection',11)`, seriesID, fID, basePath); err != nil {
			t.Fatalf("seed base in %d: %v", fID, err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO virtual_media_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,owns_item_metadata)
			VALUES(11,'collection:' || $3,$1,$2,false)`, seriesID, fID, colID); err != nil {
			t.Fatalf("seed source claim in %d: %v", fID, err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO virtual_media_file_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,file_path)
			VALUES(11,'collection:' || $4,$1,$2,$3)`, seriesID, fID, basePath, colID); err != nil {
			t.Fatalf("seed file claim in %d: %v", fID, err)
		}
	}

	// Also seed a stale special file in folder 981 to ensure cleanup removes it
	const specialPath = "virtual://series/tvdb/9889/0/1"
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id,episode_id,media_folder_id,file_path,file_size,container,probe_source,virtual_owner_installation_id,season_number,episode_number)
		VALUES($1,'ep-multi-0-1',981,$2,0,'virtual','virtual_collection',11,0,1)`, seriesID, specialPath); err != nil {
		t.Fatalf("seed stale special file: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO virtual_media_file_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,file_path)
		VALUES(11,'collection:' || $3,$1,981,$2)`, seriesID, specialPath, colID); err != nil {
		t.Fatalf("seed stale special claim: %v", err)
	}

	// First reconciliation: should reconcile seriesID across both folders
	reconciled, err := repo.ReconcileReleasedCollectionVirtualEpisodes(ctx, 10)
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if reconciled != 1 {
		t.Fatalf("reconciled = %d, want 1", reconciled)
	}

	// Verify files:
	// In folder 981: 1 base + 1 S01E01 file. Special S00E01 must be gone.
	// In folder 982: 1 base + 1 S01E01 file.
	var count981, count982, staleSpecialCount int
	if err := pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER(WHERE media_folder_id=981),
			count(*) FILTER(WHERE media_folder_id=982),
			count(*) FILTER(WHERE episode_id='ep-multi-0-1')
		FROM media_files WHERE content_id=$1 AND container='virtual'`, seriesID).Scan(&count981, &count982, &staleSpecialCount); err != nil {
		t.Fatalf("query file counts: %v", err)
	}
	if count981 != 2 || count982 != 2 || staleSpecialCount != 0 {
		t.Fatalf("count981=%d count982=%d staleSpecial=%d, want 2/2/0", count981, count982, staleSpecialCount)
	}

	// Convergence: second reconciliation must find 0 items needing reconciliation
	reconciled2, err := repo.ReconcileReleasedCollectionVirtualEpisodes(ctx, 10)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if reconciled2 != 0 {
		t.Fatalf("reconciled2 = %d, want 0 (must converge)", reconciled2)
	}
}

func TestCleanupLegacyUnscopedCollectionClaims(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(991,'Legacy Folder','movies',true) ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}

	repo := NewItemRepository(pool)
	item1 := &models.MediaItem{ContentID: "movie-legacy-unref", Type: "movie", Title: "Unreferenced Legacy", TmdbID: "9910", Status: "matched"}
	item2 := &models.MediaItem{ContentID: "movie-legacy-superseded", Type: "movie", Title: "Superseded Legacy", TmdbID: "9911", Status: "matched"}
	if err := repo.Upsert(ctx, item1); err != nil {
		t.Fatalf("seed item1: %v", err)
	}
	if err := repo.Upsert(ctx, item2); err != nil {
		t.Fatalf("seed item2: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE media_items SET virtual_source='collection' WHERE content_id IN ($1, $2)`, item1.ContentID, item2.ContentID); err != nil {
		t.Fatalf("set virtual source: %v", err)
	}

	// Item 1: Legacy claim ('collection'), but NOT a member of any collection targeting folder 991
	const uri1 = "virtual://movie/tmdb/9910"
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,probe_source,virtual_owner_installation_id)
		VALUES($1,991,$2,0,'virtual','virtual_collection',11)`, item1.ContentID, uri1); err != nil {
		t.Fatalf("seed file 1: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO virtual_media_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,owns_item_metadata)
		VALUES(11,'collection',$1,991,false)`, item1.ContentID); err != nil {
		t.Fatalf("seed claim 1: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO virtual_media_file_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,file_path)
		VALUES(11,'collection',$1,991,$2)`, item1.ContentID, uri1); err != nil {
		t.Fatalf("seed file claim 1: %v", err)
	}

	// Item 2: Legacy claim ('collection') AND scoped claim ('collection:col-991') in folder 991
	const uri2 = "virtual://movie/tmdb/9911"
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,probe_source,virtual_owner_installation_id)
		VALUES($1,991,$2,0,'virtual','virtual_collection',11)`, item2.ContentID, uri2); err != nil {
		t.Fatalf("seed file 2: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO virtual_media_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,owns_item_metadata)
		VALUES (11,'collection',$1,991,false), (11,'collection:col-991',$1,991,false)`, item2.ContentID); err != nil {
		t.Fatalf("seed claims 2: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO virtual_media_file_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,file_path)
		VALUES (11,'collection',$1,991,$2), (11,'collection:col-991',$1,991,$2)`, item2.ContentID, uri2); err != nil {
		t.Fatalf("seed file claims 2: %v", err)
	}

	// Run cleanup of legacy unscoped claims
	deleted, err := repo.CleanupLegacyUnscopedCollectionClaims(ctx, 100)
	if err != nil {
		t.Fatalf("CleanupLegacyUnscopedCollectionClaims failed: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2 (both legacy 'collection' claims cleaned)", deleted)
	}

	// Item 1 had no other claims and no collection membership:
	// Virtual file should be deleted and unreferenced item deleted
	var item1Files int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_files WHERE content_id=$1`, item1.ContentID).Scan(&item1Files); err != nil {
		t.Fatalf("query item1 files: %v", err)
	}
	if item1Files != 0 {
		t.Fatalf("item1Files = %d, want 0", item1Files)
	}

	// Item 2 had scoped claim 'collection:col-991':
	// Scoped claim and virtual file MUST be preserved!
	var item2ScopedClaims, item2Files int
	if err := pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER(WHERE source_key='collection:col-991'),
			count(*) FILTER(WHERE source_key='collection')
		FROM virtual_media_source_claims WHERE content_id=$1`, item2.ContentID).Scan(&item2ScopedClaims, &item1Files); err != nil {
		t.Fatalf("query item2 claims: %v", err)
	}
	if item2ScopedClaims != 1 || item1Files != 0 {
		t.Fatalf("item2ScopedClaims=%d item2LegacyClaims=%d, want 1/0", item2ScopedClaims, item1Files)
	}

	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_files WHERE content_id=$1`, item2.ContentID).Scan(&item2Files); err != nil {
		t.Fatalf("query item2 files: %v", err)
	}
	if item2Files != 1 {
		t.Fatalf("item2Files = %d, want 1 (preserved)", item2Files)
	}
}

func TestEnsureCollectionItemMaterialized_DeduplicatesDesiredVariants(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(992,'Variant Folder','movies',true) ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}

	const colID = "col-dedup-variants"
	const itemID = "movie-dedup-variants"
	cfg, _ := json.Marshal(map[string]any{"virtual_playback": true})

	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
		VALUES($1,$1,$1,'tmdb',992,$2) ON CONFLICT (id) DO UPDATE SET source_config=$2`, colID, cfg); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_libraries(collection_id,library_id)
		VALUES($1,992) ON CONFLICT DO NOTHING`, colID); err != nil {
		t.Fatalf("seed collection library: %v", err)
	}

	item := &models.MediaItem{ContentID: itemID, Type: "movie", Title: "Dedup Movie", Year: 2020, TmdbID: "9920", Status: "matched"}
	repo := NewItemRepository(pool)
	if err := repo.Upsert(ctx, item); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_items(collection_id,media_item_id,position)
		VALUES ($1,$2,1) ON CONFLICT DO NOTHING`, colID, itemID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	collRepo := NewLibraryCollectionRepository(pool)
	service := NewLibraryCollectionService(collRepo, repo, nil, nil)
	service.TMDBDigitalReleases = &fakeDigitalReleaseChecker{released: map[int]bool{9920: true}}
	// Return duplicate variants from provider
	service.VirtualVariants = func(_ context.Context, _, _ string) ([]VirtualPlaybackVariant, error) {
		return []VirtualPlaybackVariant{
			{VirtualURI: "virtual://movie/tmdb/9920?profile=4k", OwnerInstallationID: 11, Resolution: "4k"},
			{VirtualURI: "virtual://movie/tmdb/9920?profile=4k", OwnerInstallationID: 11, Resolution: "4k"}, // Duplicate!
		}, nil
	}
	collection := &models.LibraryCollection{
		ID:           colID,
		LibraryID:    992,
		LibraryIDs:   []int{992},
		SourceConfig: cfg,
	}

	res, err := service.EnsureCollectionItemMaterialized(ctx, collection, item)
	if err != nil {
		t.Fatalf("EnsureCollectionItemMaterialized failed: %v", err)
	}

	// Base file (virtual://movie/tmdb/9920) + 1 deduplicated variant = 2 files total
	if res.FilesCreated != 2 || res.FilesExisting != 0 {
		t.Fatalf("res = %+v, want FilesCreated=2 FilesExisting=0 (deduplicated)", res)
	}
}

type mockTMDBFailSyncFetcher struct {
	entries []TMDBCollectionEntry
	err     error
}

func (m *mockTMDBFailSyncFetcher) GetCollectionPreset(ctx context.Context, preset, mediaType, timeWindow string, limit int) ([]TMDBCollectionEntry, error) {
	return m.entries, m.err
}

func TestVirtualCollection_FailedSyncBeforeMembershipLeavesNoOrphanClaimsOrFiles(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(993,'Failed Sync Folder','movies',true) ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}

	const colID = "col-failed-sync"
	const existingID = "movie-existing-993"
	const newID = "movie-tmdb-9931"

	sourceConfig, _ := json.Marshal(map[string]any{
		"virtual_playback": true,
		"preset":           "popular",
		"mode":             "tmdb_preset",
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
		VALUES($1,$1,$1,'tmdb',993,$2)
		ON CONFLICT (id) DO UPDATE SET source_config=$2`, colID, sourceConfig); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_libraries(collection_id,library_id)
		VALUES($1,993) ON CONFLICT DO NOTHING`, colID); err != nil {
		t.Fatalf("seed collection library: %v", err)
	}

	repo := NewItemRepository(pool)
	collRepo := NewLibraryCollectionRepository(pool)

	// Seed pre-existing member
	existingItem := &models.MediaItem{ContentID: existingID, Type: "movie", Title: "Existing Member", TmdbID: "9930", Status: "matched"}
	if err := repo.Upsert(ctx, existingItem); err != nil {
		t.Fatalf("seed existing item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_items(collection_id,media_item_id,position)
		VALUES($1,$2,1) ON CONFLICT DO NOTHING`, colID, existingID); err != nil {
		t.Fatalf("seed existing membership: %v", err)
	}
	const existingPath = "virtual://movie/tmdb/9930"
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,probe_source,virtual_owner_installation_id)
		VALUES($1,993,$2,0,'virtual','virtual_collection',11)`, existingID, existingPath); err != nil {
		t.Fatalf("seed existing file: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO virtual_media_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,owns_item_metadata)
		VALUES(11,'collection:' || $1,$2,993,false)`, colID, existingID); err != nil {
		t.Fatalf("seed existing source claim: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO virtual_media_file_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,file_path)
		VALUES(11,'collection:' || $1,$2,993,$3)`, colID, existingID, existingPath); err != nil {
		t.Fatalf("seed existing file claim: %v", err)
	}

	service := NewLibraryCollectionService(collRepo, repo, nil, nil)
	service.TMDBDigitalReleases = &fakeDigitalReleaseChecker{released: map[int]bool{9931: true, 9932: true}}
	service.TMDBCollections = &mockTMDBFailSyncFetcher{
		entries: []TMDBCollectionEntry{
			{MediaType: "movie", Title: "New Movie 1", ID: 9931, ReleaseDate: "2020-01-01"},
			{MediaType: "movie", Title: "Failing Movie 2", ID: 9932, ReleaseDate: "2020-01-01"},
		},
	}

	syncCtx, cancelSync := context.WithCancel(ctx)
	service.VirtualVariants = func(vCtx context.Context, uri, _ string) ([]VirtualPlaybackVariant, error) {
		if uri == "virtual://movie/tmdb/9931" {
			// First item materializes successfully; cancel context immediately after so
			// the subsequent entry fails acceptance before membership replacement.
			defer cancelSync()
			return []VirtualPlaybackVariant{{VirtualURI: uri, OwnerInstallationID: 11}}, nil
		}
		return []VirtualPlaybackVariant{{VirtualURI: uri, OwnerInstallationID: 11}}, nil
	}

	// Run sync with cancellable context
	_, err := service.SyncCollection(syncCtx, colID)
	if err == nil {
		t.Fatal("expected sync failure due to cancellation, got nil")
	}

	// Verify new item 9931: provisional claims and files MUST BE COMPLETELY CLEANED UP
	var newClaims, newFiles, newItems int
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM virtual_media_file_source_claims WHERE content_id = $1),
			(SELECT count(*) FROM media_files WHERE content_id = $1),
			(SELECT count(*) FROM media_items WHERE content_id = $1)`, newID).Scan(&newClaims, &newFiles, &newItems); err != nil {
		t.Fatalf("query new item cleanup: %v", err)
	}
	if newClaims != 0 || newFiles != 0 || newItems != 0 {
		t.Fatalf("unreferenced new item was NOT cleaned up: claims=%d files=%d items=%d (want 0/0/0)", newClaims, newFiles, newItems)
	}

	// Verify pre-existing member 9930: MUST BE FULLY PRESERVED!
	var existClaims, existFiles, existMembers int
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM virtual_media_file_source_claims WHERE content_id = $1),
			(SELECT count(*) FROM media_files WHERE content_id = $1),
			(SELECT count(*) FROM library_collection_items WHERE collection_id = $2 AND media_item_id = $1)`, existingID, colID).Scan(&existClaims, &existFiles, &existMembers); err != nil {
		t.Fatalf("query existing member state: %v", err)
	}
	if existClaims != 1 || existFiles != 1 || existMembers != 1 {
		t.Fatalf("pre-existing member corrupted by failed sync: claims=%d files=%d members=%d (want 1/1/1)", existClaims, existFiles, existMembers)
	}

	// Collection itself must be intact
	var colExists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM library_collections WHERE id = $1)`, colID).Scan(&colExists); err != nil || !colExists {
		t.Fatalf("collection existence check failed: exists=%v, err=%v", colExists, err)
	}
}

func TestVirtualCollection_IndependentEpisodeVariantsSurviveBackgroundReconciliation(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(994,'Independent Series Folder','series',true) ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}

	const colID = "col-ep-indep"
	const seriesID = "series-tvdb-9940"
	const requestSourceKey = "request:user-req-9940"

	cfg, _ := json.Marshal(map[string]any{"virtual_playback": true})
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
		VALUES($1,$1,$1,'tmdb',994,$2) ON CONFLICT (id) DO NOTHING`, colID, cfg); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_libraries(collection_id,library_id)
		VALUES($1,994) ON CONFLICT DO NOTHING`, colID); err != nil {
		t.Fatalf("seed collection library: %v", err)
	}

	repo := NewItemRepository(pool)
	seriesItem := &models.MediaItem{
		ContentID: seriesID,
		Type:      "series",
		Title:     "Independent Series",
		TvdbID:    "9940",
		Status:    "matched",
	}
	if err := repo.Upsert(ctx, seriesItem); err != nil {
		t.Fatalf("seed series: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_items(collection_id,media_item_id,position)
		VALUES($1,$2,1) ON CONFLICT DO NOTHING`, colID, seriesID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	// Seed season and episodes: S01E01 and S01E02 both released
	if _, err := pool.Exec(ctx, `
		INSERT INTO seasons(content_id,series_id,season_number,title)
		VALUES('season-9940-1',$1,1,'Season 1') ON CONFLICT DO NOTHING`, seriesID); err != nil {
		t.Fatalf("seed season: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO episodes(content_id,series_id,season_id,season_number,episode_number,title,air_date)
		VALUES
			('ep-9940-1-1',$1,'season-9940-1',1,1,'Ep 1',CURRENT_DATE),
			('ep-9940-1-2',$1,'season-9940-1',1,2,'Ep 2',CURRENT_DATE)
		ON CONFLICT DO NOTHING`, seriesID); err != nil {
		t.Fatalf("seed episodes: %v", err)
	}

	// Collection base file in folder 994
	const basePath = "virtual://series/tvdb/9940"
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,probe_source,virtual_owner_installation_id)
		VALUES($1,994,$2,0,'virtual','virtual_collection',11)`, seriesID, basePath); err != nil {
		t.Fatalf("seed collection base: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO virtual_media_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,owns_item_metadata)
		VALUES(11,'collection:' || $1,$2,994,false)`, colID, seriesID); err != nil {
		t.Fatalf("seed collection source claim: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO virtual_media_file_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,file_path)
		VALUES(11,'collection:' || $1,$2,994,$3)`, colID, seriesID, basePath); err != nil {
		t.Fatalf("seed collection base file claim: %v", err)
	}

	// S01E01 collection episode file and claim
	const ep1Path = "virtual://series/tvdb/9940/1/1"
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id,episode_id,media_folder_id,file_path,file_size,container,probe_source,virtual_owner_installation_id,season_number,episode_number)
		VALUES($1,'ep-9940-1-1',994,$2,0,'virtual','virtual_collection',11,1,1)`, seriesID, ep1Path); err != nil {
		t.Fatalf("seed ep1 collection file: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO virtual_media_file_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,file_path)
		VALUES(11,'collection:' || $1,$2,994,$3)`, colID, seriesID, ep1Path); err != nil {
		t.Fatalf("seed ep1 collection claim: %v", err)
	}

	// Now seed INDEPENDENT request-registered episode variants for S01E02 and an extra S01E01 4K variant
	// These have probe_source = 'virtual' and source_key = 'request:user-req-9940'
	const indepEp2Path = "virtual://series/tvdb/9940/1/2"
	const indepEp1VariantPath = "virtual://series/tvdb/9940/1/1?profile=4k"
	if _, err := pool.Exec(ctx, `
		INSERT INTO virtual_media_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,owns_item_metadata)
		VALUES(22,$1,$2,994,false)`, requestSourceKey, seriesID); err != nil {
		t.Fatalf("seed independent source claim: %v", err)
	}
	for _, path := range []string{indepEp2Path, indepEp1VariantPath} {
		epID := "ep-9940-1-2"
		seasonNum, epNum := 1, 2
		if path == indepEp1VariantPath {
			epID = "ep-9940-1-1"
			seasonNum, epNum = 1, 1
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO media_files(content_id,episode_id,media_folder_id,file_path,file_size,container,probe_source,virtual_owner_installation_id,season_number,episode_number)
			VALUES($1,$2,994,$3,0,'virtual','virtual',22,$4,$5)`, seriesID, epID, path, seasonNum, epNum); err != nil {
			t.Fatalf("seed independent file %s: %v", path, err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO virtual_media_file_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,file_path)
			VALUES(22,$1,$2,994,$3)`, requestSourceKey, seriesID, path); err != nil {
			t.Fatalf("seed independent file claim %s: %v", path, err)
		}
	}

	// Run routine background reconciliation for the series with empty collectionID
	if err := repo.MaterializeVirtualPlaybackEpisodes(ctx, seriesID); err != nil {
		t.Fatalf("routine background reconciliation failed: %v", err)
	}

	// Verify that the independent request claims and files SURVIVED!
	var reqClaims, reqFiles int
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM virtual_media_file_source_claims WHERE source_key = $1),
			(SELECT count(*) FROM media_files WHERE virtual_owner_installation_id = 22 AND content_id = $2)`, requestSourceKey, seriesID).Scan(&reqClaims, &reqFiles); err != nil {
		t.Fatalf("query independent request records: %v", err)
	}
	if reqClaims != 2 {
		t.Fatalf("independent request claims = %d, want 2 (must not be purged by background reconciliation)", reqClaims)
	}
	if reqFiles != 2 {
		t.Fatalf("independent request files = %d, want 2 (must not be purged by background reconciliation)", reqFiles)
	}

	// Also verify collection-owned files exist
	var colFiles int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_files WHERE virtual_owner_installation_id = 11 AND content_id = $1`, seriesID).Scan(&colFiles); err != nil {
		t.Fatalf("query collection files: %v", err)
	}
	if colFiles < 3 { // base + S1E1 + S1E2
		t.Fatalf("collection files = %d, want at least 3 (base + S1E1 + S1E2)", colFiles)
	}
}

func TestVirtualCollection_Concurrency_RealSyncAndRepair(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(995,'Concurrent Folder','movies',true) ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}

	const colID = "col-concurrent-real"
	cfg, _ := json.Marshal(map[string]any{"virtual_playback": true})
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
		VALUES($1,$1,$1,'tmdb',995,$2) ON CONFLICT (id) DO NOTHING`, colID, cfg); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_libraries(collection_id,library_id)
		VALUES($1,995) ON CONFLICT DO NOTHING`, colID); err != nil {
		t.Fatalf("seed collection library: %v", err)
	}

	repo := NewItemRepository(pool)
	collRepo := NewLibraryCollectionRepository(pool)
	checker := &fakeDigitalReleaseChecker{released: map[int]bool{}}
	for i := 1; i <= 6; i++ {
		checker.released[9950+i] = true
	}
	service := NewLibraryCollectionService(collRepo, repo, nil, nil)
	service.TMDBDigitalReleases = checker
	service.VirtualVariants = func(_ context.Context, _, _ string) ([]VirtualPlaybackVariant, error) {
		return []VirtualPlaybackVariant{{OwnerInstallationID: 11}}, nil
	}
	collection := &models.LibraryCollection{
		ID:           colID,
		LibraryID:    995,
		LibraryIDs:   []int{995},
		SourceConfig: cfg,
	}

	// Seed 6 items
	items := make([]*models.MediaItem, 6)
	baseInputs := make([]LibraryCollectionItemInput, 6)
	for i := 0; i < 6; i++ {
		cid := fmt.Sprintf("movie-concurrent-%d", i+1)
		items[i] = &models.MediaItem{ContentID: cid, Type: "movie", Title: fmt.Sprintf("Movie %d", i+1), Year: 2020, TmdbID: fmt.Sprintf("995%d", i+1), Status: "matched"}
		if err := repo.Upsert(ctx, items[i]); err != nil {
			t.Fatalf("seed item %d: %v", i, err)
		}
		baseInputs[i] = LibraryCollectionItemInput{
			MediaItemID: cid,
			Position:    i,
			SourceRank:  i + 1,
		}
	}
	if err := collRepo.ReplaceItems(ctx, colID, baseInputs); err != nil {
		t.Fatalf("seed initial items: %v", err)
	}

	// Concurrently run ReplaceItems, EnsureCollectionItemMaterialized, and ReconcileMissingCollectionVirtualItems
	const workers = 9
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	start := make(chan struct{})

	// 3 workers doing ReplaceItems with different subsets / permutations
	for i := 0; i < 3; i++ {
		wID := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// Permute inputs
			subInputs := make([]LibraryCollectionItemInput, 6)
			offset := (wID + 1) % 6
			for j := 0; j < 6; j++ {
				idx := (offset + j) % 6
				subInputs[j] = LibraryCollectionItemInput{
					MediaItemID: items[idx].ContentID,
					Position:    j,
					SourceRank:  j + 1,
				}
			}
			errs <- collRepo.ReplaceItems(ctx, colID, subInputs)
		}()
	}

	// 3 workers doing EnsureCollectionItemMaterialized on different items
	for i := 0; i < 3; i++ {
		targetItem := items[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := service.EnsureCollectionItemMaterialized(ctx, collection, targetItem)
			// ErrCollectionItemNotMember is an acceptable non-fatal concurrency outcome if ReplaceItems removed the item
			if errors.Is(err, ErrCollectionItemNotMember) {
				err = nil
			}
			errs <- err
		}()
	}

	// 3 workers doing ReconcileMissingCollectionVirtualItems
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := service.ReconcileMissingCollectionVirtualItems(ctx, collection)
			errs <- err
		}()
	}

	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent real sync/repair encountered error: %v", err)
		}
	}

	// Post-condition: Ensure final reconciliation converges cleanly
	repaired, err := service.ReconcileMissingCollectionVirtualItems(ctx, collection)
	if err != nil {
		t.Fatalf("post-concurrency reconciliation failed: %v", err)
	}
	_ = repaired

	// Verify no duplicate files for any content_id in folder 995
	var dupCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM (
			SELECT content_id, file_path, count(*)
			FROM media_files
			WHERE media_folder_id = 995
			GROUP BY content_id, file_path
			HAVING count(*) > 1
		) duplicates`).Scan(&dupCount); err != nil {
		t.Fatalf("inspect duplicate files: %v", err)
	}
	if dupCount != 0 {
		t.Fatalf("found %d duplicate files after concurrent sync and repair", dupCount)
	}
}

func TestVirtualCollection_MissingBaseFileClaimAndEpisodeFileClaimHeal(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(996,'Heal Claims Folder','series',true)`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}

	const colID = "col-heal-claims"
	const seriesID = "series-tvdb-heal-1"
	sourceConfig, _ := json.Marshal(map[string]any{"virtual_playback": true})

	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
		VALUES($1,$1,'Heal Claims Collection','tmdb',996,$2)`, colID, sourceConfig); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_libraries(collection_id,library_id)
		VALUES($1,996)`, colID); err != nil {
		t.Fatalf("seed collection library: %v", err)
	}

	item := &models.MediaItem{
		ContentID: seriesID,
		Type:      "series",
		Title:     "Heal Series",
		SortTitle: "Heal Series",
		TvdbID:    "9961",
		Status:    "matched",
	}
	itemRepo := NewItemRepository(pool)
	if err := itemRepo.Upsert(ctx, item); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_items(collection_id,media_item_id,position)
		VALUES($1,$2,1)`, colID, seriesID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO seasons(content_id,series_id,season_number,title)
		VALUES('season-heal-1',$1,1,'Season 1')`, seriesID); err != nil {
		t.Fatalf("seed season: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO episodes(content_id,series_id,season_id,season_number,episode_number,title,air_date)
		VALUES('ep-heal-1-1',$1,'season-heal-1',1,1,'Released Ep 1',CURRENT_DATE)`, seriesID); err != nil {
		t.Fatalf("seed episode: %v", err)
	}

	collRepo := NewLibraryCollectionRepository(pool)
	service := NewLibraryCollectionService(collRepo, itemRepo, nil, nil)
	service.VirtualVariants = func(_ context.Context, _, _ string) ([]VirtualPlaybackVariant, error) {
		return []VirtualPlaybackVariant{{OwnerInstallationID: 11}}, nil
	}
	collection := &models.LibraryCollection{
		ID:           colID,
		LibraryID:    996,
		LibraryIDs:   []int{996},
		SourceConfig: sourceConfig,
	}

	// 1. Initial materialization creates base file, episode file, and both file claims
	if _, err := service.EnsureCollectionItemMaterialized(ctx, collection, item); err != nil {
		t.Fatalf("initial materialization: %v", err)
	}

	// Verify not needing materialization now
	needs, err := itemRepo.CollectionItemNeedsVirtualMaterialization(ctx, colID, seriesID)
	if err != nil || needs {
		t.Fatalf("after materialization needs=%v (err=%v), want false", needs, err)
	}

	// 2. Simulate dropping the base file claim (e.g. from an erroneous or partial external cleanup)
	tag, err := pool.Exec(ctx, `
		DELETE FROM virtual_media_file_source_claims
		WHERE source_key = $1 AND content_id = $2 AND file_path = 'virtual://series/tvdb/9961'`,
		"collection:"+colID, seriesID)
	if err != nil {
		t.Fatalf("delete base file claim: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("expected 1 base file claim deleted, got %d", tag.RowsAffected())
	}

	// Predicates must detect the missing base file claim!
	needs, err = itemRepo.CollectionItemNeedsVirtualMaterialization(ctx, colID, seriesID)
	if err != nil || !needs {
		t.Fatalf("with missing base file claim needs=%v (err=%v), want true", needs, err)
	}
	missing, err := itemRepo.FindCollectionItemsMissingVirtualBase(ctx, colID, 50)
	if err != nil || len(missing) != 1 || missing[0].ContentID != seriesID {
		t.Fatalf("FindCollectionItemsMissingVirtualBase got %v (err=%v), want seriesID", missing, err)
	}

	// ReconcileMissingCollectionVirtualItems must heal the missing base file claim!
	repaired, err := service.ReconcileMissingCollectionVirtualItems(ctx, collection)
	if err != nil || repaired != 1 {
		t.Fatalf("heal base file claim repaired=%d (err=%v), want 1", repaired, err)
	}
	var baseClaims int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM virtual_media_file_source_claims
		WHERE source_key = $1 AND content_id = $2 AND file_path = 'virtual://series/tvdb/9961'`,
		"collection:"+colID, seriesID).Scan(&baseClaims); err != nil || baseClaims != 1 {
		t.Fatalf("base claims count=%d (err=%v), want 1", baseClaims, err)
	}

	// 3. Simulate dropping the episode file claim
	if _, err := pool.Exec(ctx, `
		DELETE FROM virtual_media_file_source_claims
		WHERE source_key = $1 AND content_id = $2 AND file_path LIKE '%/1/1'`,
		"collection:"+colID, seriesID); err != nil {
		t.Fatalf("delete episode file claim: %v", err)
	}

	// Predicates must detect the missing episode file claim!
	needs, err = itemRepo.CollectionItemNeedsVirtualMaterialization(ctx, colID, seriesID)
	if err != nil || !needs {
		t.Fatalf("with missing episode file claim needs=%v (err=%v), want true", needs, err)
	}

	// Reconcile heals the missing episode file claim!
	repaired, err = service.ReconcileMissingCollectionVirtualItems(ctx, collection)
	if err != nil || repaired != 1 {
		t.Fatalf("heal episode file claim repaired=%d (err=%v), want 1", repaired, err)
	}
	var epClaims int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM virtual_media_file_source_claims
		WHERE source_key = $1 AND content_id = $2 AND file_path LIKE '%/1/1'`,
		"collection:"+colID, seriesID).Scan(&epClaims); err != nil || epClaims != 1 {
		t.Fatalf("episode claims count=%d (err=%v), want 1", epClaims, err)
	}
}

func TestVirtualCollection_EqualCountWrongIdentityVariantsReconciles(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(997,'Equal Count Folder','series',true)`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}

	const colID = "col-equal-count"
	const seriesID = "series-tvdb-eq-1"
	sourceConfig, _ := json.Marshal(map[string]any{"virtual_playback": true})

	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
		VALUES($1,$1,'Equal Count Col','tmdb',997,$2)`, colID, sourceConfig); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_libraries(collection_id,library_id)
		VALUES($1,997)`, colID); err != nil {
		t.Fatalf("seed collection library: %v", err)
	}

	item := &models.MediaItem{
		ContentID: seriesID,
		Type:      "series",
		Title:     "Equal Count Series",
		SortTitle: "Equal Count Series",
		TvdbID:    "9971",
		Status:    "matched",
	}
	itemRepo := NewItemRepository(pool)
	if err := itemRepo.Upsert(ctx, item); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_items(collection_id,media_item_id,position)
		VALUES($1,$2,1)`, colID, seriesID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO seasons(content_id,series_id,season_number,title)
		VALUES('season-eq-1',$1,1,'Season 1')`, seriesID); err != nil {
		t.Fatalf("seed season: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO episodes(content_id,series_id,season_id,season_number,episode_number,title,air_date)
		VALUES('ep-eq-1-1',$1,'season-eq-1',1,1,'Released Ep 1',CURRENT_DATE)`, seriesID); err != nil {
		t.Fatalf("seed episode: %v", err)
	}

	collRepo := NewLibraryCollectionRepository(pool)
	service := NewLibraryCollectionService(collRepo, itemRepo, nil, nil)
	// Two distinct providers
	service.VirtualVariants = func(_ context.Context, _, _ string) ([]VirtualPlaybackVariant, error) {
		return []VirtualPlaybackVariant{
			{OwnerInstallationID: 101},
			{OwnerInstallationID: 102},
		}, nil
	}
	collection := &models.LibraryCollection{
		ID:           colID,
		LibraryID:    997,
		LibraryIDs:   []int{997},
		SourceConfig: sourceConfig,
	}

	// Materialize: creates 2 base files and 2 episode files (one for each provider: 101 and 102)
	if _, err := service.EnsureCollectionItemMaterialized(ctx, collection, item); err != nil {
		t.Fatalf("materialize two providers: %v", err)
	}

	// Now corrupt episode files: delete provider 102's episode file, and insert a duplicate variant for 101.
	// Total episode files count is still 2 (equal count), but provider 102 is missing!
	if _, err := pool.Exec(ctx, `
		DELETE FROM media_files
		WHERE episode_id = 'ep-eq-1-1' AND virtual_owner_installation_id = 102`); err != nil {
		t.Fatalf("delete provider 102 episode file: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(
			content_id, episode_id, media_folder_id, file_path, file_size, container,
			probe_source, virtual_owner_installation_id, season_number, episode_number
		) VALUES ($1, 'ep-eq-1-1', 997, 'virtual://series/tvdb/9971/1/1?extra=variant', 0, 'virtual',
			'virtual_collection', 101, 1, 1)`, seriesID); err != nil {
		t.Fatalf("insert extra variant for 101: %v", err)
	}

	// ReconcileReleasedCollectionVirtualEpisodes MUST detect that provider 102 is missing its episode file
	// despite total episode files count being equal to base count (2)!
	reconciled, err := itemRepo.ReconcileReleasedCollectionVirtualEpisodes(ctx, 10)
	if err != nil {
		t.Fatalf("reconcile released episodes failed: %v", err)
	}
	if reconciled != 1 {
		t.Fatalf("expected 1 series reconciled, got %d", reconciled)
	}

	// Verify provider 102 now has its episode file restored
	var has102 bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM media_files
			WHERE episode_id = 'ep-eq-1-1' AND virtual_owner_installation_id = 102
		)`).Scan(&has102); err != nil || !has102 {
		t.Fatalf("provider 102 episode file restored=%v (err=%v), want true", has102, err)
	}
}

func TestVirtualCollection_FailedSyncBeforeMembershipCleansEpisodeLibraries(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(998,'Sync Fail Series Folder','series',true)`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}

	const colID = "col-fail-series"
	const seriesID = "series-tvdb-fail-1"
	sourceConfig, _ := json.Marshal(map[string]any{"virtual_playback": true})

	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
		VALUES($1,$1,'Failed Series Col','tmdb',998,$2)`, colID, sourceConfig); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_libraries(collection_id,library_id)
		VALUES($1,998)`, colID); err != nil {
		t.Fatalf("seed collection library: %v", err)
	}

	item := &models.MediaItem{
		ContentID: seriesID,
		Type:      "series",
		Title:     "Fail Series",
		SortTitle: "Fail Series",
		TvdbID:    "9981",
		Status:    "matched",
	}
	itemRepo := NewItemRepository(pool)
	if err := itemRepo.Upsert(ctx, item); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO seasons(content_id,series_id,season_number,title)
		VALUES('season-fail-1',$1,1,'Season 1')`, seriesID); err != nil {
		t.Fatalf("seed season: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO episodes(content_id,series_id,season_id,season_number,episode_number,title,air_date)
		VALUES('ep-fail-1-1',$1,'season-fail-1',1,1,'Released Ep 1',CURRENT_DATE)`, seriesID); err != nil {
		t.Fatalf("seed episode: %v", err)
	}

	collRepo := NewLibraryCollectionRepository(pool)
	service := NewLibraryCollectionService(collRepo, itemRepo, nil, nil)
	service.VirtualVariants = func(_ context.Context, _, _ string) ([]VirtualPlaybackVariant, error) {
		return []VirtualPlaybackVariant{{OwnerInstallationID: 11}}, nil
	}
	collection := &models.LibraryCollection{
		ID:           colID,
		LibraryID:    998,
		LibraryIDs:   []int{998},
		SourceConfig: sourceConfig,
	}

	tracker := &collectionVirtualCreationTracker{}
	ctx = context.WithValue(ctx, collectionVirtualCreationTrackerKey{}, tracker)
	if _, err := service.EnsureCollectionItemMaterializedWithOptions(ctx, collection, item, VirtualMaterializeOptions{}); err != nil {
		t.Fatalf("prepare series: %v", err)
	}
	if tracker.items[seriesID].item == nil {
		t.Fatal("series was not prepared in memory")
	}

	// Provisional materialization must not publish library links before the
	// collection membership transaction accepts the item.
	var epLibCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM episode_libraries
		WHERE episode_id = 'ep-fail-1-1' AND media_folder_id = 998`).Scan(&epLibCount); err != nil || epLibCount != 0 {
		t.Fatalf("expected no episode_libraries row before acceptance, got %d (err=%v)", epLibCount, err)
	}

	tracker.err = ErrProviderUnavailable
	if _, err := service.acceptCollectionItems(ctx, collection, []LibraryCollectionItemInput{{MediaItemID: seriesID}}); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("failed preparation accepted: %v", err)
	}
	var files, claims, items int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM media_files WHERE content_id=$1),
		(SELECT count(*) FROM virtual_media_source_claims WHERE content_id=$1),
		(SELECT count(*) FROM media_items WHERE content_id=$1)`, seriesID).Scan(&files, &claims, &items); err != nil || files != 0 || claims != 0 || items != 1 {
		t.Fatalf("failed preparation files=%d claims=%d existing items=%d: %v", files, claims, items, err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM episode_libraries
		WHERE episode_id = 'ep-fail-1-1' AND media_folder_id = 998`).Scan(&epLibCount); err != nil {
		t.Fatalf("query episode_libraries after rollback: %v", err)
	}
	if epLibCount != 0 {
		t.Fatalf("expected 0 episode_libraries rows after rollback, got %d", epLibCount)
	}
}

func TestVirtualCollection_RemoveItemCleansUnreferencedFolderLibraries(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()

	// Two separate folders: 999 (Folder A) and 1000 (Folder B)
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled)
		VALUES (999,'Folder A','series',true), (1000,'Folder B','series',true)`); err != nil {
		t.Fatalf("seed folders: %v", err)
	}

	const colA = "col-remove-a"
	const colB = "col-remove-b"
	const seriesID = "series-tvdb-shared-ab"
	cfgA, _ := json.Marshal(map[string]any{"virtual_playback": true})
	cfgB, _ := json.Marshal(map[string]any{"virtual_playback": true})

	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
		VALUES
			($1,$1,'Collection A','manual',999,$3),
			($2,$2,'Collection B','manual',1000,$4)`, colA, colB, cfgA, cfgB); err != nil {
		t.Fatalf("seed collections: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_libraries(collection_id,library_id)
		VALUES ($1,999), ($2,1000)`, colA, colB); err != nil {
		t.Fatalf("seed collection libraries: %v", err)
	}

	item := &models.MediaItem{
		ContentID: seriesID,
		Type:      "series",
		Title:     "Shared AB Series",
		SortTitle: "Shared AB Series",
		TvdbID:    "9991",
		Status:    "matched",
	}
	itemRepo := NewItemRepository(pool)
	if err := itemRepo.Upsert(ctx, item); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_items(collection_id,media_item_id,position)
		VALUES ($1,$3,1), ($2,$3,1)`, colA, colB, seriesID); err != nil {
		t.Fatalf("seed memberships: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO seasons(content_id,series_id,season_number,title)
		VALUES('season-ab-1',$1,1,'Season 1')`, seriesID); err != nil {
		t.Fatalf("seed season: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO episodes(content_id,series_id,season_id,season_number,episode_number,title,air_date)
		VALUES('ep-ab-1-1',$1,'season-ab-1',1,1,'Released Ep 1',CURRENT_DATE)`, seriesID); err != nil {
		t.Fatalf("seed episode: %v", err)
	}

	collRepo := NewLibraryCollectionRepository(pool)
	service := NewLibraryCollectionService(collRepo, itemRepo, nil, nil)
	service.VirtualVariants = func(_ context.Context, _, _ string) ([]VirtualPlaybackVariant, error) {
		return []VirtualPlaybackVariant{{OwnerInstallationID: 11}}, nil
	}

	colObjA := &models.LibraryCollection{ID: colA, LibraryID: 999, LibraryIDs: []int{999}, SourceConfig: cfgA}
	colObjB := &models.LibraryCollection{ID: colB, LibraryID: 1000, LibraryIDs: []int{1000}, SourceConfig: cfgB}

	// Materialize in both collections
	if _, err := service.EnsureCollectionItemMaterialized(ctx, colObjA, item); err != nil {
		t.Fatalf("materialize in colA: %v", err)
	}
	if _, err := service.EnsureCollectionItemMaterialized(ctx, colObjB, item); err != nil {
		t.Fatalf("materialize in colB: %v", err)
	}

	// Verify item exists in media_item_libraries and episode_libraries for both 999 and 1000
	var count999, count1000 int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_item_libraries WHERE content_id=$1 AND media_folder_id=999`, seriesID).Scan(&count999); err != nil || count999 != 1 {
		t.Fatalf("folder 999 media_item_libraries = %d, want 1", count999)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_item_libraries WHERE content_id=$1 AND media_folder_id=1000`, seriesID).Scan(&count1000); err != nil || count1000 != 1 {
		t.Fatalf("folder 1000 media_item_libraries = %d, want 1", count1000)
	}

	// Remove item from colA (folder 999)
	if err := collRepo.RemoveItem(ctx, colA, seriesID); err != nil {
		t.Fatalf("RemoveItem from colA: %v", err)
	}

	// Folder 999 links should be cleaned up!
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_item_libraries WHERE content_id=$1 AND media_folder_id=999`, seriesID).Scan(&count999); err != nil || count999 != 0 {
		t.Fatalf("after RemoveItem, folder 999 media_item_libraries = %d, want 0", count999)
	}
	var ep999 int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM episode_libraries WHERE episode_id='ep-ab-1-1' AND media_folder_id=999`).Scan(&ep999); err != nil || ep999 != 0 {
		t.Fatalf("after RemoveItem, folder 999 episode_libraries = %d, want 0", ep999)
	}

	// Folder 1000 (colB) MUST be preserved intact!
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_item_libraries WHERE content_id=$1 AND media_folder_id=1000`, seriesID).Scan(&count1000); err != nil || count1000 != 1 {
		t.Fatalf("after RemoveItem, folder 1000 media_item_libraries = %d, want 1", count1000)
	}
	var ep1000 int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM episode_libraries WHERE episode_id='ep-ab-1-1' AND media_folder_id=1000`).Scan(&ep1000); err != nil || ep1000 != 1 {
		t.Fatalf("after RemoveItem, folder 1000 episode_libraries = %d, want 1", ep1000)
	}
	var survivingFiles int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_files WHERE content_id=$1 AND media_folder_id=1000`, seriesID).Scan(&survivingFiles); err != nil || survivingFiles != 2 {
		t.Fatalf("after RemoveItem, folder 1000 media_files = %d, want 2", survivingFiles)
	}
}

func TestVirtualCollection_StagedSeriesPreparesEpisodesAtomically(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(3001,'Staged Series','series',true)`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	const colID = "col-staged-series"
	const seriesID = "series-tvdb-3001"
	cfg, _ := json.Marshal(map[string]any{"virtual_playback": true})
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
		VALUES($1,$1,'Staged Series Col','tmdb',3001,$2)`, colID, cfg); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO library_collection_libraries(collection_id,library_id) VALUES($1,3001)`, colID); err != nil {
		t.Fatalf("seed collection library: %v", err)
	}
	item := &models.MediaItem{ContentID: seriesID, Type: "series", Title: "Staged Series", SortTitle: "Staged Series", TvdbID: "3001", Status: "matched"}
	itemRepo := NewItemRepository(pool)
	if err := itemRepo.Upsert(ctx, item); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO seasons(content_id,series_id,season_number,title) VALUES('season-3001-1',$1,1,'Season 1')`, seriesID); err != nil {
		t.Fatalf("seed season: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO episodes(content_id,series_id,season_id,season_number,episode_number,title,air_date)
		VALUES('ep-3001-1-1',$1,'season-3001-1',1,1,'Released',CURRENT_DATE)`, seriesID); err != nil {
		t.Fatalf("seed episode: %v", err)
	}

	collRepo := NewLibraryCollectionRepository(pool)
	service := NewLibraryCollectionService(collRepo, itemRepo, NewLibraryItemRepository(pool), nil)
	service.VirtualVariants = func(_ context.Context, _, _ string) ([]VirtualPlaybackVariant, error) {
		return []VirtualPlaybackVariant{{OwnerInstallationID: 11}}, nil
	}
	collection := &models.LibraryCollection{ID: colID, LibraryID: 3001, LibraryIDs: []int{3001}, SourceConfig: cfg, CollectionType: "tmdb"}
	tracker := &collectionVirtualCreationTracker{}
	ctx = context.WithValue(ctx, collectionVirtualCreationTrackerKey{}, tracker)
	res, err := service.EnsureCollectionItemMaterializedWithOptions(ctx, collection, item, VirtualMaterializeOptions{})
	if err != nil {
		t.Fatalf("stage new series: %v", err)
	}
	if res.FilesCreated != 0 || res.EpisodesMaterialized != 0 || tracker.items[seriesID].item == nil || len(tracker.items[seriesID].variants) != 1 {
		t.Fatalf("preparation published files or omitted candidate: %+v, %+v", res, tracker.items[seriesID])
	}
	var stagedBase, stagedEp, links, epLinks int
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM media_files WHERE content_id=$1 AND episode_id IS NULL),
		  (SELECT count(*) FROM media_files WHERE episode_id='ep-3001-1-1'),
		  (SELECT count(*) FROM media_item_libraries WHERE content_id=$1),
		  (SELECT count(*) FROM episode_libraries WHERE episode_id='ep-3001-1-1')`,
		seriesID).Scan(&stagedBase, &stagedEp, &links, &epLinks); err != nil {
		t.Fatalf("inspect staged state: %v", err)
	}
	if stagedBase != 0 || stagedEp != 0 || links != 0 || epLinks != 0 {
		t.Fatalf("prepared state base=%d ep=%d links=%d epLinks=%d, want 0/0/0/0", stagedBase, stagedEp, links, epLinks)
	}

	if _, err := service.acceptCollectionItems(ctx, collection, []LibraryCollectionItemInput{{MediaItemID: seriesID, Position: 0, SourceRank: 1}}); err != nil {
		t.Fatalf("accept membership: %v", err)
	}
	var activeBase, activeEp int
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM media_files WHERE content_id=$1 AND episode_id IS NULL AND missing_since IS NULL),
		  (SELECT count(*) FROM media_files WHERE episode_id='ep-3001-1-1' AND missing_since IS NULL)`,
		seriesID).Scan(&activeBase, &activeEp); err != nil {
		t.Fatalf("inspect accepted state: %v", err)
	}
	if activeBase != 1 || activeEp != 1 {
		t.Fatalf("accepted state base=%d ep=%d, want 1/1", activeBase, activeEp)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_item_libraries WHERE content_id=$1`, seriesID).Scan(&links); err != nil || links != 1 {
		t.Fatalf("accepted item links = %d, want 1", links)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM episode_libraries WHERE episode_id='ep-3001-1-1'`).Scan(&epLinks); err != nil || epLinks != 1 {
		t.Fatalf("accepted episode links = %d, want 1", epLinks)
	}
}

func TestVirtualCollection_MDBListPrefersInLibraryCandidate(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(3002,'Pref Movies','movies',true)`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	const colID = "col-mdblist-pref"
	const localID = "movie-tmdb-3002"
	cfg, _ := json.Marshal(map[string]any{"mode": "mdblist_json", "url": "https://mdblist.com/lists/testuser/testlist", "limit": 10, "virtual_playback": true})
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
		VALUES($1,$1,'Pref Col','mdblist',3002,$2)`, colID, cfg); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO library_collection_libraries(collection_id,library_id) VALUES($1,3002)`, colID); err != nil {
		t.Fatalf("seed collection library: %v", err)
	}
	// Existing local movie matching the list entry by both TMDB and IMDb IDs.
	local := &models.MediaItem{ContentID: localID, Type: "movie", Title: "Local Pref Movie", SortTitle: "Local Pref Movie", TmdbID: "3002", ImdbID: "tt3002002", Status: "matched"}
	itemRepo := NewItemRepository(pool)
	if err := itemRepo.Upsert(ctx, local); err != nil {
		t.Fatalf("seed local item: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container) VALUES($1,3002,'/movies/local-3002.mkv',1024,NULL)`, localID); err != nil {
		t.Fatalf("seed local file: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO media_item_libraries(content_id,media_folder_id) VALUES($1,3002)`, localID); err != nil {
		t.Fatalf("seed local link: %v", err)
	}

	body := `[{"id":3002,"rank":1,"imdb_id":"tt3002002","mediatype":"movie","title":"Local Pref Movie","release_year":2024,"released":"2024-05-01"}]`
	stub := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	collRepo := NewLibraryCollectionRepository(pool)
	service := NewLibraryCollectionService(collRepo, itemRepo, NewLibraryItemRepository(pool), stub)
	variantsCalled := false
	service.VirtualVariants = func(_ context.Context, _, _ string) ([]VirtualPlaybackVariant, error) {
		variantsCalled = true
		return nil, errors.New("must not resolve provider variants for a resident local match")
	}

	run, err := service.SyncCollection(ctx, colID)
	if err != nil {
		t.Fatalf("sync collection: %v", err)
	}
	if run.Status != "success" {
		t.Fatalf("run status = %q, want success", run.Status)
	}
	var member string
	if err := pool.QueryRow(ctx, `SELECT media_item_id FROM library_collection_items WHERE collection_id=$1`, colID).Scan(&member); err != nil {
		t.Fatalf("inspect membership: %v", err)
	}
	if member != localID {
		t.Fatalf("member = %q, want local item %q", member, localID)
	}
	var virtualFiles, virtualClaims int
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM media_files WHERE content_id=$1 AND (container='virtual' OR file_path LIKE 'virtual://%')),
		  (SELECT count(*) FROM virtual_media_file_source_claims WHERE content_id=$1)`,
		localID).Scan(&virtualFiles, &virtualClaims); err != nil {
		t.Fatalf("inspect virtual state: %v", err)
	}
	if virtualFiles != 0 || virtualClaims != 0 {
		t.Fatalf("virtual state files=%d claims=%d, want 0/0", virtualFiles, virtualClaims)
	}
	if variantsCalled {
		t.Fatal("provider variants were resolved for a resident local match")
	}
}

// TestVirtualCollection_MDBListFetchFailurePreservesMembership proves a fetch
// error leaves existing collection membership untouched and records a failed
// sync run (F9).
func TestVirtualCollection_MDBListFetchFailurePreservesMembership(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(3005,'Fetch Fail','movies',true)`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	const colID = "col-mdblist-fetchfail"
	const existingID = "movie-existing-3005"
	cfg, _ := json.Marshal(map[string]any{"mode": "mdblist_json", "url": "https://mdblist.com/lists/testuser/testlist", "virtual_playback": true})
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
		VALUES($1,$1,'Fetch Fail Col','mdblist',3005,$2)`, colID, cfg); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO library_collection_libraries(collection_id,library_id) VALUES($1,3005)`, colID); err != nil {
		t.Fatalf("seed collection library: %v", err)
	}
	itemRepo := NewItemRepository(pool)
	if err := itemRepo.Upsert(ctx, &models.MediaItem{ContentID: existingID, Type: "movie", Title: "Existing", SortTitle: "Existing", TmdbID: "3005", Status: "matched"}); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO library_collection_items(collection_id,media_item_id,position) VALUES($1,$2,0)`, colID, existingID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	stub := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("mdblist source unavailable")
	})}
	service := NewLibraryCollectionService(NewLibraryCollectionRepository(pool), itemRepo, NewLibraryItemRepository(pool), stub)

	if _, err := service.SyncCollection(ctx, colID); err == nil {
		t.Fatal("expected a fetch error")
	}
	var members int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM library_collection_items WHERE collection_id=$1 AND media_item_id=$2`, colID, existingID).Scan(&members); err != nil {
		t.Fatalf("inspect membership: %v", err)
	}
	if members != 1 {
		t.Fatalf("membership = %d, want 1 (fetch error must not clear membership)", members)
	}
}

// TestVirtualCollection_MDBListEmptyAPIFallsBackWithoutWipe proves the API
// empty-response sentinel falls back to /json and preserves membership (F1/F2
// + F9). The API fetcher returns the sentinel; the /json stub serves the
// pre-existing member so membership is re-accepted rather than cleared.
func TestVirtualCollection_MDBListEmptyAPIFallsBackWithoutWipe(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(3006,'Empty API','movies',true)`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	const colID = "col-mdblist-emptyapi"
	const existingID = "movie-existing-3006"
	cfg, _ := json.Marshal(map[string]any{"mode": "mdblist_json", "url": "https://mdblist.com/lists/testuser/testlist", "limit": 10, "virtual_playback": true})
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
		VALUES($1,$1,'Empty API Col','mdblist',3006,$2)`, colID, cfg); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO library_collection_libraries(collection_id,library_id) VALUES($1,3006)`, colID); err != nil {
		t.Fatalf("seed collection library: %v", err)
	}
	itemRepo := NewItemRepository(pool)
	if err := itemRepo.Upsert(ctx, &models.MediaItem{ContentID: existingID, Type: "movie", Title: "Existing", SortTitle: "Existing", TmdbID: "3006", ImdbID: "tt3006006", Status: "matched"}); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	// A resident physical member so the sync accepts it without needing
	// release evidence (mirrors the in-library-candidate test).
	if _, err := pool.Exec(ctx, `INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container) VALUES($1,3006,'/movies/existing-3006.mkv',1024,NULL)`, existingID); err != nil {
		t.Fatalf("seed local file: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO media_item_libraries(content_id,media_folder_id) VALUES($1,3006)`, existingID); err != nil {
		t.Fatalf("seed local link: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO library_collection_items(collection_id,media_item_id,position) VALUES($1,$2,0)`, colID, existingID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	body := `[{"id":3006,"rank":1,"imdb_id":"tt3006006","mediatype":"movie","title":"Existing","release_year":2024,"released":"2024-05-01"}]`
	stub := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	service := NewLibraryCollectionService(NewLibraryCollectionRepository(pool), itemRepo, NewLibraryItemRepository(pool), stub)
	// The API fetcher reports the empty/incomplete sentinel; the service must
	// fall back to the /json stub instead of accepting zero items.
	service.MDBListAPI = &fakeMDBListAPI{err: fmt.Errorf("%w: total=1", mdblist.ErrEmptyItemsWithTotal)}

	run, err := service.SyncCollection(ctx, colID)
	if err != nil {
		t.Fatalf("sync collection: %v", err)
	}
	if run.Status != "success" {
		t.Fatalf("run status = %q, want success via /json fallback", run.Status)
	}
	var members int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM library_collection_items WHERE collection_id=$1 AND media_item_id=$2`, colID, existingID).Scan(&members); err != nil {
		t.Fatalf("inspect membership: %v", err)
	}
	if members != 1 {
		t.Fatalf("membership = %d, want 1 (empty API must not wipe the collection)", members)
	}
	if !service.MDBListAPI.(*fakeMDBListAPI).called {
		t.Fatal("API fetcher was not attempted before falling back")
	}
}

// TestVirtualCollection_ContextEndRecordsFailedRun proves F4: a sync that ends
// because its context is done still persists a failed sync run, so
// last_sync_status reflects the failure.
func TestVirtualCollection_ContextEndRecordsFailedRun(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(3007,'Context End','movies',true)`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	const colID = "col-mdblist-contextend"
	cfg, _ := json.Marshal(map[string]any{"mode": "mdblist_json", "url": "https://mdblist.com/lists/testuser/testlist", "virtual_playback": true})
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
		VALUES($1,$1,'Context End Col','mdblist',3007,$2)`, colID, cfg); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO library_collection_libraries(collection_id,library_id) VALUES($1,3007)`, colID); err != nil {
		t.Fatalf("seed collection library: %v", err)
	}

	syncCtx, cancel := context.WithCancel(ctx)
	stub := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		// Simulate a deadline arriving mid-fetch: the context ends, then the
		// fetch returns an error.
		cancel()
		return nil, context.DeadlineExceeded
	})}
	service := NewLibraryCollectionService(NewLibraryCollectionRepository(pool), NewItemRepository(pool), NewLibraryItemRepository(pool), stub)

	if _, err := service.SyncCollection(syncCtx, colID); err == nil {
		t.Fatal("expected a fetch error")
	}
	var status string
	if err := pool.QueryRow(ctx, `
		SELECT status FROM library_collection_sync_runs
		WHERE collection_id=$1 ORDER BY created_at DESC LIMIT 1`, colID).Scan(&status); err != nil {
		t.Fatalf("expected a durable failed run row: %v", err)
	}
	if status != "failed" {
		t.Fatalf("run status = %q, want failed", status)
	}
}

func TestVirtualCollection_RemoveItemPreservesOrdinaryMetadataItem(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(3003,'Ordinary Movies','movies',true)`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	const colID = "col-ordinary-remove"
	const itemID = "movie-ordinary-3003"
	cfg, _ := json.Marshal(map[string]any{"virtual_playback": true})
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
		VALUES($1,$1,'Ordinary Col','manual',3003,$2)`, colID, cfg); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	// Ordinary metadata-only catalog row: no files, no claims.
	if err := NewItemRepository(pool).Upsert(ctx, &models.MediaItem{ContentID: itemID, Type: "movie", Title: "Ordinary", SortTitle: "Ordinary", TmdbID: "3003", Status: "matched"}); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO library_collection_items(collection_id,media_item_id,position) VALUES($1,$2,1)`, colID, itemID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	collRepo := NewLibraryCollectionRepository(pool)
	if err := collRepo.RemoveItem(ctx, colID, itemID); err != nil {
		t.Fatalf("remove member: %v", err)
	}
	var items, members int
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM media_items WHERE content_id=$1),
		  (SELECT count(*) FROM library_collection_items WHERE collection_id=$2 AND media_item_id=$1)`,
		itemID, colID).Scan(&items, &members); err != nil {
		t.Fatalf("inspect after removal: %v", err)
	}
	if items != 1 || members != 0 {
		t.Fatalf("after removal items=%d members=%d, want 1/0", items, members)
	}
	if err := collRepo.RemoveItem(ctx, colID, itemID); !errors.Is(err, ErrCollectionItemNotMember) {
		t.Fatalf("second removal err = %v, want ErrCollectionItemNotMember", err)
	}
}

func TestCleanupLegacyPreservesOtherProviderFiles(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(3004,'Legacy Movies','movies',true)`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	const itemID = "movie-legacy-providers-3004"
	if err := NewItemRepository(pool).Upsert(ctx, &models.MediaItem{ContentID: itemID, Type: "movie", Title: "Legacy Providers", SortTitle: "Legacy Providers", TmdbID: "3004", Status: "matched"}); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	const uriA = "virtual://movie/tmdb/3004"
	const uriB = "virtual://movie/tmdb/3004?profile=alt"
	// Provider A holds only legacy unscoped claims; provider B holds scoped claims.
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,probe_source,virtual_owner_installation_id)
		VALUES
			($1,3004,$2,0,'virtual','virtual_collection',11),
			($1,3004,$3,0,'virtual','virtual',12)`, itemID, uriA, uriB); err != nil {
		t.Fatalf("seed files: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO virtual_media_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,owns_item_metadata)
		VALUES (11,'collection',$1,3004,false), (12,'collection:col-keep',$1,3004,false)`, itemID); err != nil {
		t.Fatalf("seed source claims: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO virtual_media_file_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,file_path)
		VALUES (11,'collection',$1,3004,$2), (12,'collection:col-keep',$1,3004,$3)`, itemID, uriA, uriB); err != nil {
		t.Fatalf("seed file claims: %v", err)
	}

	deleted, err := NewItemRepository(pool).CleanupLegacyUnscopedCollectionClaims(ctx, 100)
	if err != nil {
		t.Fatalf("legacy cleanup: %v", err)
	}
	if deleted < 1 {
		t.Fatalf("deleted = %d, want at least 1", deleted)
	}
	var aSource, aFile, aFiles, bSource, bFile, bFiles, items int
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM virtual_media_source_claims WHERE content_id=$1 AND source_key='collection'),
		  (SELECT count(*) FROM virtual_media_file_source_claims WHERE content_id=$1 AND source_key='collection'),
		  (SELECT count(*) FROM media_files WHERE content_id=$1 AND file_path=$2),
		  (SELECT count(*) FROM virtual_media_source_claims WHERE content_id=$1 AND source_key='collection:col-keep'),
		  (SELECT count(*) FROM virtual_media_file_source_claims WHERE content_id=$1 AND source_key='collection:col-keep'),
		  (SELECT count(*) FROM media_files WHERE content_id=$1 AND file_path=$3),
		  (SELECT count(*) FROM media_items WHERE content_id=$1)`,
		itemID, uriA, uriB).Scan(&aSource, &aFile, &aFiles, &bSource, &bFile, &bFiles, &items); err != nil {
		t.Fatalf("inspect after cleanup: %v", err)
	}
	if aSource != 0 || aFile != 0 || aFiles != 0 {
		t.Fatalf("legacy provider state source=%d fileclaims=%d files=%d, want 0/0/0", aSource, aFile, aFiles)
	}
	if bSource != 1 || bFile != 1 || bFiles != 1 || items != 1 {
		t.Fatalf("other provider state source=%d fileclaims=%d files=%d items=%d, want 1/1/1/1", bSource, bFile, bFiles, items)
	}
}

func TestVirtualCollection_FailedCleanupPreservesAcceptedAndForeignClaims(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(3005,'Fence Movies','movies',true)`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	const colX = "col-fence-x"
	const colY = "col-fence-y"
	cfg, _ := json.Marshal(map[string]any{"virtual_playback": true})
	for _, c := range []string{colX, colY} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
			VALUES($1,$1,'Fence Col','tmdb',3005,$2)`, c, cfg); err != nil {
			t.Fatalf("seed collection %s: %v", c, err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO library_collection_libraries(collection_id,library_id) VALUES($1,3005)`, c); err != nil {
			t.Fatalf("seed collection library %s: %v", c, err)
		}
	}
	const stagedID = "movie-fence-staged-3005"
	const keptID = "movie-fence-kept-3005"
	const foreignID = "movie-fence-foreign-3005"
	itemRepo := NewItemRepository(pool)
	collRepo := NewLibraryCollectionRepository(pool)
	service := NewLibraryCollectionService(collRepo, itemRepo, NewLibraryItemRepository(pool), nil)
	service.TMDBDigitalReleases = &fakeDigitalReleaseChecker{released: map[int]bool{3005: true, 3006: true, 3007: true}}
	service.VirtualVariants = func(_ context.Context, _, _ string) ([]VirtualPlaybackVariant, error) {
		return []VirtualPlaybackVariant{{OwnerInstallationID: 11}}, nil
	}
	for _, tc := range []struct {
		id     string
		tmdbID string
	}{
		{stagedID, "3005"},
		{keptID, "3006"},
		{foreignID, "3007"},
	} {
		if err := itemRepo.Upsert(ctx, &models.MediaItem{ContentID: tc.id, Type: "movie", Title: tc.id, SortTitle: tc.id, Year: 2020, TmdbID: tc.tmdbID, Status: "matched"}); err != nil {
			t.Fatalf("seed item %s: %v", tc.id, err)
		}
	}
	colXObj := &models.LibraryCollection{ID: colX, LibraryID: 3005, LibraryIDs: []int{3005}, SourceConfig: cfg}
	colYObj := &models.LibraryCollection{ID: colY, LibraryID: 3005, LibraryIDs: []int{3005}, SourceConfig: cfg}
	tracker := &collectionVirtualCreationTracker{}
	prepareCtx := context.WithValue(ctx, collectionVirtualCreationTrackerKey{}, tracker)
	stagedItem, _ := itemRepo.GetByID(ctx, stagedID)
	if _, err := service.EnsureCollectionItemMaterializedWithOptions(prepareCtx, colXObj, stagedItem, VirtualMaterializeOptions{}); err != nil {
		t.Fatalf("stage item: %v", err)
	}
	// Accepted member of colX and member of colY.
	for _, tc := range []struct {
		col *models.LibraryCollection
		id  string
	}{
		{colXObj, keptID},
		{colYObj, foreignID},
	} {
		it, _ := itemRepo.GetByID(ctx, tc.id)
		tc.col.CollectionType = "tmdb"
		acceptedCtx := context.WithValue(ctx, collectionVirtualCreationTrackerKey{}, &collectionVirtualCreationTracker{})
		if _, err := service.EnsureCollectionItemMaterializedWithOptions(acceptedCtx, tc.col, it, VirtualMaterializeOptions{}); err != nil {
			t.Fatalf("prepare %s: %v", tc.id, err)
		}
		if _, err := service.acceptCollectionItems(acceptedCtx, tc.col, []LibraryCollectionItemInput{{MediaItemID: tc.id}}); err != nil {
			t.Fatalf("activate %s: %v", tc.id, err)
		}
	}

	tracker.err = ErrProviderUnavailable
	if _, err := service.acceptCollectionItems(prepareCtx, colXObj, []LibraryCollectionItemInput{{MediaItemID: stagedID}, {MediaItemID: keptID}, {MediaItemID: foreignID}}); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("failed preparation accepted: %v", err)
	}
	var stagedFiles int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_files WHERE content_id=$1`, stagedID).Scan(&stagedFiles); err != nil || stagedFiles != 0 {
		t.Fatalf("failed preparation files=%d: %v", stagedFiles, err)
	}
	var stagedClaims, keptClaims, foreignClaims, keptFiles, foreignFiles int
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM virtual_media_file_source_claims WHERE content_id=$1),
		  (SELECT count(*) FROM virtual_media_file_source_claims WHERE content_id=$2),
		  (SELECT count(*) FROM virtual_media_file_source_claims WHERE content_id=$3),
		  (SELECT count(*) FROM media_files WHERE content_id=$2),
		  (SELECT count(*) FROM media_files WHERE content_id=$3)`,
		stagedID, keptID, foreignID).Scan(&stagedClaims, &keptClaims, &foreignClaims, &keptFiles, &foreignFiles); err != nil {
		t.Fatalf("inspect after cleanup: %v", err)
	}
	if stagedClaims != 0 {
		t.Fatalf("staged claims = %d, want 0", stagedClaims)
	}
	if keptClaims == 0 || keptFiles == 0 {
		t.Fatalf("accepted state claims=%d files=%d, want both >0", keptClaims, keptFiles)
	}
	if foreignClaims == 0 || foreignFiles == 0 {
		t.Fatalf("foreign collection state claims=%d files=%d, want both >0", foreignClaims, foreignFiles)
	}
}
