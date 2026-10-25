package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/jackc/pgx/v5/pgxpool"
)

type releasedMovieChecker struct{}

func (releasedMovieChecker) HasDigitalRelease(context.Context, int) (bool, error) {
	return true, nil
}

func newReleasedVirtualMediaRegistrar(pool *pgxpool.Pool) *VirtualMediaRegistrar {
	reg := NewVirtualMediaRegistrar(pool)
	reg.TMDBDigitalReleases = releasedMovieChecker{}
	return reg
}

func newVirtualMediaTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to connect to db: %v", err)
	}
	var databaseName string
	if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&databaseName); err != nil {
		t.Fatalf("identify test database: %v", err)
	}
	if !strings.Contains(strings.ToLower(databaseName), "test") && !strings.Contains(strings.ToLower(databaseName), "purge") {
		t.Fatalf("refusing destructive catalog fixture database %q", databaseName)
	}
	lockConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire test database lock: %v", err)
	}
	if _, err := lockConn.Exec(ctx, "SELECT pg_advisory_lock($1)", int64(0x53494c4f5f544553)); err != nil {
		lockConn.Release()
		t.Fatalf("lock test database fixtures: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	t.Cleanup(func() {
		_, _ = lockConn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", int64(0x53494c4f5f544553))
		lockConn.Release()
	})

	// Clean out related tables for isolation. The user/playback tables are
	// cleared explicitly because the retention guards consult them and a
	// leftover row could otherwise retain a file in an unrelated test.
	for _, statement := range []string{
		"DELETE FROM user_watch_progress",
		"DELETE FROM user_watch_history",
		"DELETE FROM playback_v3_attempts",
		"DELETE FROM abs_playback_sessions",
		"DELETE FROM media_files",
		"TRUNCATE public.episodes CASCADE",
		"DELETE FROM seasons",
		"DELETE FROM media_item_libraries",
		"DELETE FROM library_collection_items",
		"DELETE FROM library_collections",
		"DELETE FROM media_items",
		"DELETE FROM media_folders",
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("reset virtual media test database: %v", err)
		}
	}

	return pool
}

// waitForBlockedAdvisoryLock polls pg_locks until another backend holds an
// ungranted advisory lock (optionally matching a specific lockKey), proving a
// concurrent writer actually queued behind held locks rather than running sequentially.
// It captures the intended writer database and lock key, excludes the
// test's own PID, and logs the observed advisory lock holders on timeout
// so a mismatch (wrong key, wrong backend, no queuing) is diagnosable
// instead of a bare timeout.
func waitForBlockedAdvisoryLock(t *testing.T, pool *pgxpool.Pool, lockKey string, timeout time.Duration) {
	t.Helper()
	ctx := context.Background()
	var self int
	var dbName string
	if err := pool.QueryRow(ctx, "SELECT pg_backend_pid(), current_database()").Scan(&self, &dbName); err != nil {
		t.Fatal(err)
	}
	var wantHash int64
	if lockKey != "" {
		if err := pool.QueryRow(ctx, `SELECT hashtextextended($1, 0)`, lockKey).Scan(&wantHash); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var blocked int
		var err error
		if lockKey != "" {
			err = pool.QueryRow(ctx, `
				SELECT count(*) FROM pg_locks
				WHERE locktype='advisory' AND NOT granted AND pid <> $1
				  AND ((classid::bigint << 32) | (objid::bigint & 4294967295)) = hashtextextended($2, 0)`, self, lockKey).Scan(&blocked)
		} else {
			err = pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND NOT granted AND pid <> $1`, self).Scan(&blocked)
		}
		if err != nil {
			t.Fatal(err)
		}
		if blocked > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	rows, qerr := pool.Query(ctx, `
		SELECT l.pid, l.granted,
		       ((l.classid::bigint << 32) | (l.objid::bigint & 4294967295)),
		       COALESCE(a.datname, ''), COALESCE(a.query, ''), COALESCE(a.wait_event_type, ''), COALESCE(a.wait_event, '')
		FROM pg_locks l LEFT JOIN pg_stat_activity a ON a.pid = l.pid
		WHERE l.locktype='advisory' AND l.pid <> $1
		ORDER BY l.pid`, self)
	if qerr != nil {
		t.Fatalf("concurrent writer never queued behind the held release locks (db=%q lockKey=%q wantHash=%d self=%d): could not dump pg_locks: %v", dbName, lockKey, wantHash, self, qerr)
	}
	defer rows.Close()
	for rows.Next() {
		var pid int
		var granted bool
		var keyHash int64
		var datname, query, waitType, waitEvent string
		if err := rows.Scan(&pid, &granted, &keyHash, &datname, &query, &waitType, &waitEvent); err != nil {
			t.Logf("pg_locks dump scan failed: %v", err)
			break
		}
		t.Logf("advisory lock: pid=%d granted=%v keyHash=%d datname=%q query=%q wait=%s/%s (want lockKey=%q wantHash=%d self=%d db=%q)", pid, granted, keyHash, datname, query, waitType, waitEvent, lockKey, wantHash, self, dbName)
	}
	t.Fatalf("concurrent writer never queued behind the held release locks (db=%q lockKey=%q wantHash=%d self=%d)", dbName, lockKey, wantHash, self)
}

var testReleaseSuffixCounter atomic.Uint64

// uniqueReleaseSuffix returns per-run digits for provider identities written
// to append-only tables (override history, metadata queue, provider IDs, and
// content-keyed claims). Canonical numeric schemes accept the result. The
// scheme combines the process ID with an atomic counter so concurrent tests
// and processes get deterministic, collision-free suffixes without
// timestamp-based collisions.
func uniqueReleaseSuffix(t *testing.T) string {
	t.Helper()
	c := testReleaseSuffixCounter.Add(1)
	return fmt.Sprintf("%02d%05d", os.Getpid()%100, c%100000)
}

func TestPresenceRequiresFileInMatchingEnabledLibrary(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled) VALUES(998,'Disabled','movies',false),(999,'Enabled','movies',true);
		INSERT INTO media_items(content_id,type,title,tmdb_id) VALUES('movie-tmdb-1','movie','Presence','1');
		INSERT INTO media_item_libraries(content_id,media_folder_id) VALUES('movie-tmdb-1',998),('movie-tmdb-1',999);
		INSERT INTO media_item_provider_ids(content_id,provider,provider_id,item_type) VALUES('movie-tmdb-1','imdb','tt1','movie');
		INSERT INTO media_files(content_id,media_folder_id,file_path,container) VALUES('movie-tmdb-1',998,'/test/presence.mkv','mkv')`); err != nil {
		t.Fatal(err)
	}
	repo := NewItemRepository(pool)
	for _, candidate := range []ExternalIDLookupCandidate{{TMDBID: "1"}, {TMDBID: "2", IMDbID: "tt1"}} {
		rows, err := repo.LookupExternalIDs(ctx, "movie", []ExternalIDLookupCandidate{candidate})
		if err != nil || len(rows) != 0 {
			t.Fatalf("disabled file counted as enabled presence: rows=%+v err=%v", rows, err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE media_files SET media_folder_id=999 WHERE content_id='movie-tmdb-1'`); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []ExternalIDLookupCandidate{{TMDBID: "1"}, {TMDBID: "2", IMDbID: "tt1"}} {
		rows, err := repo.LookupExternalIDs(ctx, "movie", []ExternalIDLookupCandidate{candidate})
		if err != nil || len(rows) != 1 || rows[0].LibraryID != "999" {
			t.Fatalf("enabled file not found: rows=%+v err=%v", rows, err)
		}
	}
}

// TestVirtualMediaUpsertPersistsSeasonNumberingSource covers the provenance
// that request season joins read: a TMDB-numbered import keeps its canonical
// series-tvdb identity (TVDB wins for identity) but records tmdb numbering.
func TestVirtualMediaUpsertPersistsSeasonNumberingSource(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	reg := newReleasedVirtualMediaRegistrar(pool)

	if _, err := pool.Exec(ctx, "INSERT INTO media_folders(id,name,type,enabled) VALUES(999,'TestVirtual','mixed',true)"); err != nil {
		t.Fatalf("seed virtual media folder: %v", err)
	}
	res, err := reg.UpsertVirtualMedia(ctx, 11, VirtualMedia{
		LibraryID: "999", MediaType: "series", Title: "Numbered Series",
		IMDbID: "tt301", TMDBID: "3", TVDBID: "300", Source: "provider-a",
		SeasonNumberingSource: "tmdb",
		Episodes: []VirtualEpisode{{
			SeasonNumber: 1, EpisodeNumber: 1, Title: "Pilot", AirDate: time.Now().Add(-time.Hour),
			VirtualURI: "virtual://series/tt301/1/1",
		}},
	})
	if err != nil {
		t.Fatalf("upsert series: %v", err)
	}
	if res.MediaID != "series-tvdb-300" {
		t.Fatalf("media id = %q, want the canonical TVDB identity series-tvdb-300", res.MediaID)
	}
	var persisted string
	if err := pool.QueryRow(ctx, "SELECT season_numbering_source FROM media_items WHERE content_id=$1", res.MediaID).Scan(&persisted); err != nil {
		t.Fatalf("load season numbering source: %v", err)
	}
	if persisted != "tmdb" {
		t.Fatalf("season_numbering_source = %q, want tmdb", persisted)
	}

	rows, err := NewItemRepository(pool).LookupExternalIDs(ctx, "series", []ExternalIDLookupCandidate{{TMDBID: "3"}})
	if err != nil {
		t.Fatalf("lookup external ids: %v", err)
	}
	if len(rows) != 1 || rows[0].SeasonNumberingSource != "tmdb" {
		t.Fatalf("lookup rows = %+v, want one row with tmdb season numbering", rows)
	}
}

func TestVirtualMediaVariantsUpsert(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	reg := newReleasedVirtualMediaRegistrar(pool)

	if _, err := pool.Exec(ctx, "INSERT INTO media_folders(id,name,type,enabled) VALUES(999,'TestVirtual','mixed',true)"); err != nil {
		t.Fatalf("seed virtual media folder: %v", err)
	}

	in := VirtualMedia{
		LibraryID: "999", MediaType: "movie", Title: "Test Movie", IMDbID: "tt100", TMDBID: "1", Source: "provider-a",
		Year:           2020,
		RuntimeMinutes: 120,
		Variants: []VirtualMediaVariant{
			{VirtualURI: "virtual://movie/tt100?profile=1080p", Resolution: "1080p", CodecVideo: "h264", RuntimeMinutes: 120},
			{VirtualURI: "virtual://movie/tt100?profile=4k", Resolution: "4k", CodecVideo: "hevc", HDR: "hdr10", RuntimeMinutes: 120},
		},
	}
	res, err := reg.UpsertVirtualMedia(ctx, 11, in)
	if err != nil {
		t.Fatalf("upsert failed: %v", err)
	}

	// Verify two virtual files were inserted and default container is "virtual"
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM media_files WHERE content_id=$1 AND container='virtual'", res.MediaID).Scan(&count); err != nil {
		t.Fatalf("count virtual media files: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 variants, got %d", count)
	}

	// Test variant with supplied container and file size
	inWithSupplied := VirtualMedia{
		LibraryID: "999", MediaType: "movie", Title: "Custom Variant Movie", IMDbID: "tt200", TMDBID: "2", Source: "provider-a",
		Year:           2020,
		RuntimeMinutes: 120,
		Variants: []VirtualMediaVariant{
			{VirtualURI: "virtual://movie/tt200?profile=1080p", Resolution: "1080p", CodecVideo: "h264", Container: "mkv", FileSize: 104857600},
		},
	}
	resSupplied, err := reg.UpsertVirtualMedia(ctx, 11, inWithSupplied)
	if err != nil {
		t.Fatalf("upsert custom variant failed: %v", err)
	}
	var container string
	var fileSize int64
	err = pool.QueryRow(ctx, "SELECT container, file_size FROM media_files WHERE content_id=$1 AND file_path=$2", resSupplied.MediaID, "virtual://movie/tt200?profile=1080p").Scan(&container, &fileSize)
	if err != nil {
		t.Fatalf("failed to query custom variant file: %v", err)
	}
	if container != "virtual" {
		t.Fatalf("expected container 'virtual', got %q", container)
	}
	if fileSize != 104857600 {
		t.Fatalf("expected file_size 104857600, got %d", fileSize)
	}

	// Verify idempotency
	in.Overview = "Updated overview"
	_, err = reg.UpsertVirtualMedia(ctx, 11, in)
	if err != nil {
		t.Fatalf("second upsert failed: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM media_files WHERE content_id=$1", res.MediaID).Scan(&count); err != nil {
		t.Fatalf("count updated virtual media files: %v", err)
	}
	if count != 2 {
		t.Fatalf("idempotent upsert should not add duplicates, got %d", count)
	}
	var overview string
	if err := pool.QueryRow(ctx, "SELECT overview FROM media_items WHERE content_id=$1", res.MediaID).Scan(&overview); err != nil {
		t.Fatalf("load updated virtual media overview: %v", err)
	}
	if overview != "Updated overview" {
		t.Fatalf("expected 'Updated overview', got %q", overview)
	}

	// Test episode variants
	series := VirtualMedia{
		LibraryID: "999", MediaType: "series", Title: "Test Series", IMDbID: "tt300", TMDBID: "3", Source: "provider-a",
		Episodes: []VirtualEpisode{
			{
				SeasonNumber: 1, EpisodeNumber: 1, Title: "Ep 1", AirDate: time.Now().Add(-time.Hour),
				Variants: []VirtualMediaVariant{
					{VirtualURI: "virtual://series/tt300/1/1?profile=1080p", Resolution: "1080p"},
					{VirtualURI: "virtual://series/tt300/1/1?profile=720p", Resolution: "720p"},
				},
			},
		},
	}
	resSeries, err := reg.UpsertVirtualMedia(ctx, 11, series)
	if err != nil {
		t.Fatalf("upsert series failed: %v", err)
	}
	if resSeries.EpisodesUpserted != 1 {
		t.Fatalf("expected 1 episode, got %d", resSeries.EpisodesUpserted)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM media_files WHERE content_id=$1", resSeries.MediaID).Scan(&count); err != nil {
		t.Fatalf("count virtual episode files: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 episode variants, got %d", count)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM virtual_media_file_source_claims
		WHERE plugin_installation_id=11 AND source_key='provider-a'`).Scan(&count); err != nil {
		t.Fatalf("count virtual file claims: %v", err)
	}
	if count != 5 {
		t.Fatalf("expected 5 source-scoped file claims, got %d", count)
	}
}

func TestVirtualMediaUpsertAndReconcileShareSourceLock(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled)
		VALUES(973,'Source Lock','movies',true)`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	registrar := newReleasedVirtualMediaRegistrar(pool)
	input := VirtualMedia{
		LibraryID: "973", MediaType: "movie", Title: "Source Lock",
		IMDbID: "tt208", TMDBID: "208", Source: "source-lock",
		VirtualURI: "virtual://movie/tt208",
	}

	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock holder: %v", err)
	}
	if err := lockVirtualMediaSource(ctx, blocker, 11, input.Source); err != nil {
		t.Fatalf("hold source lock: %v", err)
	}
	upsertDone := make(chan error, 1)
	go func() {
		_, err := registrar.UpsertVirtualMedia(ctx, 11, input)
		upsertDone <- err
	}()
	select {
	case err := <-upsertDone:
		_ = blocker.Rollback(ctx)
		t.Fatalf("upsert bypassed source lock: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatalf("release upsert source lock: %v", err)
	}
	select {
	case err := <-upsertDone:
		if err != nil {
			t.Fatalf("upsert after source unlock: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("upsert did not resume after source unlock")
	}

	blocker, err = pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin reconcile lock holder: %v", err)
	}
	if err := lockVirtualMediaSource(ctx, blocker, 11, input.Source); err != nil {
		t.Fatalf("hold reconcile source lock: %v", err)
	}
	reconcileDone := make(chan error, 1)
	go func() {
		_, err := registrar.ReconcileVirtualMedia(ctx, 11, input.Source, []string{"movie-tmdb-208"}, []int{973})
		reconcileDone <- err
	}()
	select {
	case err := <-reconcileDone:
		_ = blocker.Rollback(ctx)
		t.Fatalf("reconcile bypassed source lock: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatalf("release reconcile source lock: %v", err)
	}
	select {
	case err := <-reconcileDone:
		if err != nil {
			t.Fatalf("reconcile after source unlock: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reconcile did not resume after source unlock")
	}
}

func TestVirtualMediaUpsertPreservesLocalItemAndAddsIndependentVirtualSource(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	reg := newReleasedVirtualMediaRegistrar(pool)

	if _, err := pool.Exec(ctx, "INSERT INTO media_folders(id,name,type,enabled) VALUES(997,'LocalAndVirtual','movies',true)"); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	const contentID = "movie-tmdb-10"
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id,type,title,tmdb_id,status)
		VALUES($1,'movie','Authoritative Local Title','10','matched')`, contentID); err != nil {
		t.Fatalf("seed local item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container)
		VALUES($1,997,'/media/authoritative-local.mkv',1024,'mkv')`, contentID); err != nil {
		t.Fatalf("seed local file: %v", err)
	}

	_, err := reg.UpsertVirtualMedia(ctx, 11, VirtualMedia{
		LibraryID: "997", MediaType: "movie", Title: "Plugin Must Not Replace This",
		IMDbID: "tt10", TMDBID: "10", Source: "provider-a",
		Year:       2020,
		VirtualURI: "virtual://movie/tt10",
	})
	if err != nil {
		t.Fatalf("add virtual source to local item: %v", err)
	}

	var (
		title        string
		legacyOwner  *int64
		localFiles   int
		virtualFiles int
		ownsMetadata bool
	)
	if err := pool.QueryRow(ctx, `
		SELECT title,virtual_owner_installation_id FROM media_items WHERE content_id=$1`,
		contentID).Scan(&title, &legacyOwner); err != nil {
		t.Fatalf("load preserved item: %v", err)
	}
	if title != "Authoritative Local Title" || legacyOwner != nil {
		t.Fatalf("local item was claimed or overwritten: title=%q owner=%v", title, legacyOwner)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER(WHERE file_path='/media/authoritative-local.mkv'),
		       count(*) FILTER(WHERE file_path='virtual://movie/tt10')
		FROM media_files WHERE content_id=$1`, contentID).Scan(&localFiles, &virtualFiles); err != nil {
		t.Fatalf("load coexisting files: %v", err)
	}
	if localFiles != 1 || virtualFiles != 1 {
		t.Fatalf("local+virtual coexistence files local=%d virtual=%d", localFiles, virtualFiles)
	}
	if err := pool.QueryRow(ctx, `
		SELECT owns_item_metadata FROM virtual_media_source_claims
		WHERE plugin_installation_id=11 AND source_key='provider-a' AND content_id=$1`,
		contentID).Scan(&ownsMetadata); err != nil {
		t.Fatalf("load virtual source claim: %v", err)
	}
	if ownsMetadata {
		t.Fatal("virtual source incorrectly claimed local item metadata")
	}

	purgeResult, err := (&ItemRepository{pool: pool}).PurgeVirtualPlaybackItems(
		ctx, VirtualPurgeOptions{InstallationID: 11},
	)
	if err != nil {
		t.Fatalf("purge virtual source from local item: %v", err)
	}
	if purgeResult.FilesDeleted != 1 || purgeResult.ItemsDeleted != 0 {
		t.Fatalf("purge deleted files=%d items=%d, want 1 and 0", purgeResult.FilesDeleted, purgeResult.ItemsDeleted)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM virtual_media_source_claims
		WHERE plugin_installation_id=11 AND content_id=$1`, contentID).Scan(&virtualFiles); err != nil {
		t.Fatalf("count source claims after purge: %v", err)
	}
	if virtualFiles != 0 {
		t.Fatalf("purge left %d ghost virtual source claims", virtualFiles)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM media_files
		WHERE content_id=$1 AND file_path='/media/authoritative-local.mkv'`, contentID).Scan(&localFiles); err != nil {
		t.Fatalf("count local file after purge: %v", err)
	}
	if localFiles != 1 {
		t.Fatalf("purge removed local file, remaining=%d", localFiles)
	}
}

func TestVirtualMediaSourcesDoNotOverwriteEachOtherAndReconcileIndependently(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	reg := newReleasedVirtualMediaRegistrar(pool)
	if _, err := pool.Exec(ctx, "INSERT INTO media_folders(id,name,type,enabled) VALUES(996,'MultiProvider','movies',true)"); err != nil {
		t.Fatalf("seed folder: %v", err)
	}

	first := VirtualMedia{
		LibraryID: "996", MediaType: "movie", Title: "Provider A Title",
		IMDbID: "tt20", TMDBID: "20", Source: "provider-a",
		Year:       2020,
		VirtualURI: "virtual://movie/tt20?result=a",
	}
	result, err := reg.UpsertVirtualMedia(ctx, 11, first)
	if err != nil {
		t.Fatalf("upsert first provider: %v", err)
	}
	second := first
	second.Title = "Provider B Must Not Replace This"
	second.Source = "provider-b"
	second.VirtualURI = "virtual://movie/tt20?result=b"
	if _, err := reg.UpsertVirtualMedia(ctx, 22, second); err != nil {
		t.Fatalf("upsert second provider: %v", err)
	}

	var (
		title       string
		legacyOwner *int64
		claims      int
		files       int
	)
	if err := pool.QueryRow(ctx, `
		SELECT title,virtual_owner_installation_id FROM media_items WHERE content_id=$1`,
		result.MediaID).Scan(&title, &legacyOwner); err != nil {
		t.Fatalf("load shared item: %v", err)
	}
	if title != first.Title || legacyOwner == nil || *legacyOwner != 11 {
		t.Fatalf("second provider overwrote primary metadata: title=%q owner=%v", title, legacyOwner)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM virtual_media_source_claims WHERE content_id=$1", result.MediaID).Scan(&claims); err != nil {
		t.Fatalf("count virtual source claims: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM media_files WHERE content_id=$1", result.MediaID).Scan(&files); err != nil {
		t.Fatalf("count shared virtual files: %v", err)
	}
	if claims != 2 || files != 2 {
		t.Fatalf("multi-provider state claims=%d files=%d, want 2/2", claims, files)
	}

	reconciled, err := reg.ReconcileVirtualMedia(ctx, 11, "provider-a", []string{"movie-tmdb-999"}, []int{996})
	if err != nil {
		t.Fatalf("reconcile first provider: %v", err)
	}
	if reconciled.FilesRemoved != 1 || reconciled.ItemsRemoved != 0 {
		t.Fatalf("reconcile result=%+v, want one file and preserved item", reconciled)
	}
	var remainingOwner int
	if err := pool.QueryRow(ctx, `
		SELECT virtual_owner_installation_id FROM media_files
		WHERE content_id=$1`, result.MediaID).Scan(&remainingOwner); err != nil {
		t.Fatalf("load remaining provider file: %v", err)
	}
	if remainingOwner != 22 {
		t.Fatalf("remaining file owner=%d, want 22", remainingOwner)
	}
	if err := pool.QueryRow(ctx, `
		SELECT virtual_owner_installation_id FROM media_items WHERE content_id=$1`,
		result.MediaID).Scan(&legacyOwner); err != nil {
		t.Fatalf("load compatibility owner: %v", err)
	}
	if legacyOwner == nil || *legacyOwner != 22 {
		t.Fatalf("surviving provider was not promoted after reconciliation: %v", legacyOwner)
	}
}

func TestReplaceCollectionItemsPreservesOtherVirtualSourceClaims(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	reg := newReleasedVirtualMediaRegistrar(pool)

	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled)
		VALUES(994,'ClaimedVirtual','movies',true)`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	result, err := reg.UpsertVirtualMedia(ctx, 11, VirtualMedia{
		LibraryID: "994", MediaType: "movie", Title: "Shared source",
		IMDbID: "tt40", TMDBID: "40", Source: "request",
		Year:       2020,
		VirtualURI: "virtual://movie/tt40",
	})
	if err != nil {
		t.Fatalf("seed request-owned virtual media: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE media_items SET virtual_source='collection'
		WHERE content_id=$1`, result.MediaID); err != nil {
		t.Fatalf("simulate collection scalar ownership: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,library_id,slug,title,collection_type)
		VALUES('collection-claim-test',994,'claim-test','Claim test','manual');
		INSERT INTO library_collection_libraries(collection_id,library_id)
		VALUES('collection-claim-test',994)`); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_items(collection_id,media_item_id)
		VALUES('collection-claim-test',$1)`, result.MediaID); err != nil {
		t.Fatalf("seed collection membership: %v", err)
	}

	repo := NewLibraryCollectionRepository(pool)
	if err := repo.ReplaceItems(ctx, "collection-claim-test", nil); err != nil {
		t.Fatalf("replace collection items: %v", err)
	}
	var items, files, claims int
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM media_items WHERE content_id=$1),
			(SELECT count(*) FROM media_files WHERE content_id=$1),
			(SELECT count(*) FROM virtual_media_source_claims WHERE content_id=$1)`,
		result.MediaID,
	).Scan(&items, &files, &claims); err != nil {
		t.Fatalf("count preserved virtual source: %v", err)
	}
	if items != 1 || files != 1 || claims != 1 {
		t.Fatalf("other virtual source was deleted: items=%d files=%d claims=%d", items, files, claims)
	}
}

func TestCleanupRequestVirtualMediaPreservesOtherPluginClaim(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	reg := newReleasedVirtualMediaRegistrar(pool)
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled)
		VALUES(993,'RequestClaims','movies',true)`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	requestMedia := VirtualMedia{
		LibraryID: "993", MediaType: "movie", Title: "Claim cleanup",
		IMDbID: "tt50", TMDBID: "50", Source: "request:movie:tt50",
		Year:       2020,
		VirtualURI: "virtual://movie/tt50?result=request",
	}
	result, err := reg.UpsertVirtualMedia(ctx, 11, requestMedia)
	if err != nil {
		t.Fatalf("seed request claim: %v", err)
	}
	pluginMedia := requestMedia
	pluginMedia.Source = "provider-b"
	pluginMedia.VirtualURI = "virtual://movie/tt50?result=provider"
	if _, err := reg.UpsertVirtualMedia(ctx, 22, pluginMedia); err != nil {
		t.Fatalf("seed plugin claim: %v", err)
	}

	if err := (&ItemRepository{pool: pool}).CleanupRequestVirtualMedia(ctx, "movie", 50, "", "tt50"); err != nil {
		t.Fatalf("cleanup request media: %v", err)
	}
	var requestFiles, pluginFiles, requestClaims, pluginClaims int
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM media_files WHERE content_id=$1 AND virtual_owner_installation_id=11),
		  (SELECT count(*) FROM media_files WHERE content_id=$1 AND virtual_owner_installation_id=22),
		  (SELECT count(*) FROM virtual_media_source_claims WHERE content_id=$1 AND source_key='request:movie:tt50'),
		  (SELECT count(*) FROM virtual_media_source_claims WHERE content_id=$1 AND source_key='provider-b')`,
		result.MediaID,
	).Scan(&requestFiles, &pluginFiles, &requestClaims, &pluginClaims); err != nil {
		t.Fatalf("inspect request cleanup: %v", err)
	}
	if requestFiles != 0 || requestClaims != 0 {
		t.Fatalf("request ownership remained: files=%d claims=%d", requestFiles, requestClaims)
	}
	if pluginFiles != 1 || pluginClaims != 1 {
		t.Fatalf("other plugin ownership was removed: files=%d claims=%d", pluginFiles, pluginClaims)
	}
}

func seedSharedCollectionAndRequestVirtualFile(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	const (
		contentID    = "movie-tmdb-90"
		collectionID = "shared-virtual-claims"
	)
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled)
		VALUES(989,'SharedClaims','movies',true)`); err != nil {
		t.Fatalf("seed shared collection folder: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,library_id,slug,title,collection_type)
		VALUES($1,989,'shared-virtual-claims','Shared virtual claims','manual')`,
		collectionID); err != nil {
		t.Fatalf("seed shared collection row: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_libraries(collection_id,library_id)
		VALUES($1,989)`, collectionID); err != nil {
		t.Fatalf("seed shared collection library: %v", err)
	}
	repo := NewItemRepository(pool)
	created, err := repo.MaterializeVirtualPlaybackItemWithVariants(ctx, &models.MediaItem{
		ContentID: contentID, Type: "movie", Title: "Shared Claims",
		SortTitle: "Shared Claims", TmdbID: "90", ImdbID: "tt90", Status: "matched",
	}, []int{989}, []VirtualPlaybackVariant{{
		VirtualURI:          "virtual://movie/tt90",
		OwnerInstallationID: 11,
	}})
	if err != nil || !created {
		t.Fatalf("materialize shared collection item: created=%v err=%v", created, err)
	}
	collections := NewLibraryCollectionRepository(pool)
	if err := collections.ReplaceItems(ctx, collectionID, []LibraryCollectionItemInput{{
		MediaItemID: contentID,
	}}); err != nil {
		t.Fatalf("claim collection virtual item: %v", err)
	}
	if _, err := newReleasedVirtualMediaRegistrar(pool).UpsertVirtualMedia(ctx, 11, VirtualMedia{
		LibraryID: "989", MediaType: "movie", Title: "Shared Claims",
		IMDbID: "tt90", TMDBID: "90", Source: "request:movie:tt90",
		Year:       2020,
		VirtualURI: "virtual://movie/tt90",
	}); err != nil {
		t.Fatalf("add shared request claim: %v", err)
	}
	return contentID
}

// Core-owned variants (OwnerInstallationID 0, stamped by the core virtual
// library) persist under the core owner identity now that the virtual-library
// plugin is retired: the base file row carries owner 0 and re-materializing
// is idempotent through the owner-0 unique index.
func TestMaterializeCoreOwnedVirtualPlaybackItem(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	const contentID = "movie-tmdb-core951"
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled)
		VALUES(951,'CoreOwned','movies',true)`); err != nil {
		t.Fatalf("seed core-owned folder: %v", err)
	}
	item := &models.MediaItem{
		ContentID: contentID, Type: "movie", Title: "Core Owned",
		SortTitle: "Core Owned", TmdbID: "951", ImdbID: "tt951", Status: "matched",
	}
	variants := []VirtualPlaybackVariant{{
		VirtualURI:          "virtual://movie/tt951",
		OwnerInstallationID: 0,
	}}
	repo := NewItemRepository(pool)
	created, err := repo.MaterializeVirtualPlaybackItemWithVariants(ctx, item, []int{951}, variants)
	if err != nil || !created {
		t.Fatalf("materialize core-owned item: created=%v err=%v", created, err)
	}
	again, err := repo.MaterializeVirtualPlaybackItemWithVariants(ctx, item, []int{951}, variants)
	if err != nil || again {
		t.Fatalf("re-materialize core-owned item: created=%v err=%v, want idempotent", again, err)
	}
	var files int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM media_files
		WHERE content_id=$1 AND file_path='virtual://movie/tt951'
		  AND virtual_owner_installation_id=0`, contentID).Scan(&files); err != nil {
		t.Fatalf("inspect core-owned file: %v", err)
	}
	if files != 1 {
		t.Fatalf("core-owned files=%d, want 1", files)
	}
}

// Materializing without any variants is still rejected: there is no owner at
// all to persist the file under, core or otherwise.
func TestMaterializeVirtualPlaybackItemRejectsEmptyVariants(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled)
		VALUES(952,'CoreEmpty','movies',true)`); err != nil {
		t.Fatalf("seed core-empty folder: %v", err)
	}
	_, err := NewItemRepository(pool).MaterializeVirtualPlaybackItemWithVariants(ctx, &models.MediaItem{
		ContentID: "movie-tmdb-core952", Type: "movie", Title: "Core Empty",
		SortTitle: "Core Empty", TmdbID: "952", ImdbID: "tt952", Status: "matched",
	}, []int{952}, nil)
	if err == nil || !strings.Contains(err.Error(), "owning provider installation") {
		t.Fatalf("err = %v, want owning-provider-installation rejection", err)
	}
}

func TestCleanupRequestVirtualMediaPreservesSharedCollectionFile(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	contentID := seedSharedCollectionAndRequestVirtualFile(t, pool)

	if err := NewItemRepository(pool).CleanupRequestVirtualMedia(ctx, "movie", 90, "", "tt90"); err != nil {
		t.Fatalf("clean shared request claim: %v", err)
	}
	var files, collectionClaims, requestClaims int
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM media_files
		   WHERE content_id=$1 AND file_path='virtual://movie/tt90'
		     AND virtual_owner_installation_id=11),
		  (SELECT count(*) FROM virtual_media_file_source_claims
		   WHERE content_id=$1 AND source_key='collection:shared-virtual-claims'),
		  (SELECT count(*) FROM virtual_media_file_source_claims
		   WHERE content_id=$1 AND source_key='request:movie:tt90')`,
		contentID,
	).Scan(&files, &collectionClaims, &requestClaims); err != nil {
		t.Fatalf("inspect request cleanup with collection claim: %v", err)
	}
	if files != 1 || collectionClaims != 1 || requestClaims != 0 {
		t.Fatalf("shared request cleanup files=%d collection_claims=%d request_claims=%d",
			files, collectionClaims, requestClaims)
	}
}

func TestReplaceCollectionItemsPreservesSharedRequestFile(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	contentID := seedSharedCollectionAndRequestVirtualFile(t, pool)

	if err := NewLibraryCollectionRepository(pool).ReplaceItems(ctx, "shared-virtual-claims", nil); err != nil {
		t.Fatalf("remove shared item from collection: %v", err)
	}
	var (
		files            int
		collectionClaims int
		requestClaims    int
		memberships      int
	)
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM media_files
		   WHERE content_id=$1 AND file_path='virtual://movie/tt90'
		     AND virtual_owner_installation_id=11),
		  (SELECT count(*) FROM virtual_media_file_source_claims
		   WHERE content_id=$1 AND source_key='collection:shared-virtual-claims'),
		  (SELECT count(*) FROM virtual_media_file_source_claims
		   WHERE content_id=$1 AND source_key='request:movie:tt90'),
		  (SELECT count(*) FROM library_collection_items WHERE media_item_id=$1)`,
		contentID,
	).Scan(&files, &collectionClaims, &requestClaims, &memberships); err != nil {
		t.Fatalf("inspect collection removal with request claim: %v", err)
	}
	if files != 1 || collectionClaims != 0 || requestClaims != 1 || memberships != 0 {
		t.Fatalf("shared collection removal files=%d collection_claims=%d request_claims=%d memberships=%d",
			files, collectionClaims, requestClaims, memberships)
	}
}

func TestCollectionRemovalCleansFileAfterRequestClaimWasCancelled(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	contentID := seedSharedCollectionAndRequestVirtualFile(t, pool)
	if err := NewItemRepository(pool).CleanupRequestVirtualMedia(ctx, "movie", 90, "", "tt90"); err != nil {
		t.Fatalf("cancel shared request claim: %v", err)
	}
	if err := NewLibraryCollectionRepository(pool).ReplaceItems(ctx, "shared-virtual-claims", nil); err != nil {
		t.Fatalf("remove final collection claim: %v", err)
	}
	var items, files, claims int
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM media_items WHERE content_id=$1),
		  (SELECT count(*) FROM media_files WHERE content_id=$1),
		  (SELECT count(*) FROM virtual_media_source_claims WHERE content_id=$1)`,
		contentID,
	).Scan(&items, &files, &claims); err != nil {
		t.Fatalf("inspect final collection cleanup: %v", err)
	}
	if items != 0 || files != 0 || claims != 0 {
		t.Fatalf("orphaned collection virtual media: items=%d files=%d claims=%d", items, files, claims)
	}
}

func TestDeleteCollectionCleansItsUnsharedVirtualMedia(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	contentID := seedSharedCollectionAndRequestVirtualFile(t, pool)
	if err := NewItemRepository(pool).CleanupRequestVirtualMedia(ctx, "movie", 90, "", "tt90"); err != nil {
		t.Fatalf("cancel shared request claim: %v", err)
	}
	if err := NewLibraryCollectionRepository(pool).Delete(ctx, "shared-virtual-claims"); err != nil {
		t.Fatalf("delete collection: %v", err)
	}
	var items, files, claims, collections int
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM media_items WHERE content_id=$1),
		  (SELECT count(*) FROM media_files WHERE content_id=$1),
		  (SELECT count(*) FROM virtual_media_source_claims WHERE content_id=$1),
		  (SELECT count(*) FROM library_collections WHERE id='shared-virtual-claims')`,
		contentID,
	).Scan(&items, &files, &claims, &collections); err != nil {
		t.Fatalf("inspect collection deletion cleanup: %v", err)
	}
	if items != 0 || files != 0 || claims != 0 || collections != 0 {
		t.Fatalf("orphaned deleted collection state: items=%d files=%d claims=%d collections=%d",
			items, files, claims, collections)
	}
}

func TestCollectionMaterializationPreservesCanonicalLocalItem(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled)
		VALUES(992,'CanonicalLocal','movies',true)`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	const contentID = "movie-tmdb-60"
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id,type,title,tmdb_id,status)
		VALUES($1,'movie','Keep Local Metadata','60','matched')`, contentID); err != nil {
		t.Fatalf("seed local item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container)
		VALUES($1,992,'/media/local-60.mkv',2048,'mkv')`, contentID); err != nil {
		t.Fatalf("seed local file: %v", err)
	}
	repo := NewItemRepository(pool)
	created, err := repo.MaterializeVirtualPlaybackItemWithVariants(ctx, &models.MediaItem{
		ContentID: contentID, Type: "movie", Title: "Must Not Replace Local",
		SortTitle: "Must Not Replace Local", TmdbID: "60", ImdbID: "tt60",
		Status: "matched",
	}, []int{992}, []VirtualPlaybackVariant{{
		VirtualURI:          "virtual://movie/tt60?profile=4K+HDR",
		Resolution:          "2160p",
		CodecVideo:          "hevc",
		CodecAudio:          "eac3",
		HDR:                 "hdr10",
		OwnerInstallationID: 11,
	}})
	if err != nil {
		t.Fatalf("materialize canonical local item: %v", err)
	}
	if created {
		t.Fatal("existing canonical local item reported as newly created")
	}
	var title string
	var localFiles, virtualFiles int
	if err := pool.QueryRow(ctx, `
		SELECT title,
		  (SELECT count(*) FROM media_files WHERE content_id=$1 AND file_path='/media/local-60.mkv'),
		  (SELECT count(*) FROM media_files WHERE content_id=$1 AND file_path='virtual://movie/tt60')
		FROM media_items WHERE content_id=$1`, contentID).Scan(&title, &localFiles, &virtualFiles); err != nil {
		t.Fatalf("inspect canonical local item: %v", err)
	}
	if title != "Keep Local Metadata" || localFiles != 1 || virtualFiles != 1 {
		t.Fatalf("local+virtual result title=%q local=%d virtual=%d", title, localFiles, virtualFiles)
	}
	var (
		resolution string
		videoCodec string
		audioCodec string
		hdr        bool
		owner      int
	)
	if err := pool.QueryRow(ctx, `
		SELECT resolution,codec_video,codec_audio,hdr,virtual_owner_installation_id
		FROM media_files
		WHERE content_id=$1 AND file_path='virtual://movie/tt60?profile=4K+HDR'`,
		contentID,
	).Scan(&resolution, &videoCodec, &audioCodec, &hdr, &owner); err != nil {
		t.Fatalf("inspect collection virtual variant metadata: %v", err)
	}
	if resolution != "2160p" || videoCodec != "hevc" || audioCodec != "eac3" || !hdr || owner != 11 {
		t.Fatalf("variant metadata resolution=%q video=%q audio=%q hdr=%v owner=%d",
			resolution, videoCodec, audioCodec, hdr, owner)
	}
}

func TestCleanupUnreferencedCollectionVirtualItemsIsCandidateScoped(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled)
		VALUES(991,'FailedSync','movies',true)`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	repo := NewItemRepository(pool)
	materialize := func(contentID, tmdbID string) {
		t.Helper()
		created, err := repo.MaterializeVirtualPlaybackItemWithVariants(ctx, &models.MediaItem{
			ContentID: contentID, Type: "movie", Title: contentID,
			SortTitle: contentID, TmdbID: tmdbID, Status: "matched",
		}, []int{991}, []VirtualPlaybackVariant{{
			VirtualURI:          "virtual://movie/tmdb/" + tmdbID,
			OwnerInstallationID: 11,
		}})
		if err != nil || !created {
			t.Fatalf("materialize %s: created=%v err=%v", contentID, created, err)
		}
	}
	materialize("movie-tmdb-70", "70")
	materialize("movie-tmdb-71", "71")

	deleted, err := repo.CleanupUnreferencedCollectionVirtualItems(ctx, "collection-70", []string{"movie-tmdb-70"})
	if err != nil {
		t.Fatalf("cleanup failed sync candidates: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted=%d, want 1", deleted)
	}
	var removed, preserved int
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM media_items WHERE content_id='movie-tmdb-70'),
		  (SELECT count(*) FROM media_items WHERE content_id='movie-tmdb-71')`,
	).Scan(&removed, &preserved); err != nil {
		t.Fatalf("inspect failed sync cleanup: %v", err)
	}
	if removed != 0 || preserved != 1 {
		t.Fatalf("candidate scope removed=%d preserved=%d", removed, preserved)
	}
}

func TestReleasedEpisodeReconciliationSkipsFutureEpisodes(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled)
		VALUES(990,'ReleaseSchedule','series',true)`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,slug,title,collection_type,library_id)
		VALUES('release-schedule','release-schedule','Release Schedule','tmdb',990);
		INSERT INTO library_collection_libraries(collection_id,library_id)
		VALUES('release-schedule',990)`); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	repo := NewItemRepository(pool)
	const seriesID = "series-tvdb-80"
	created, err := repo.MaterializeVirtualPlaybackItemWithVariants(ctx, &models.MediaItem{
		ContentID: seriesID, Type: "series", Title: "Scheduled Series",
		SortTitle: "Scheduled Series", TvdbID: "80", Status: "matched",
	}, []int{990}, []VirtualPlaybackVariant{{
		VirtualURI:          "virtual://series/tvdb/80",
		OwnerInstallationID: 11,
	}})
	if err != nil || !created {
		t.Fatalf("materialize series: created=%v err=%v", created, err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO virtual_media_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,owns_item_metadata)
		VALUES(11,'collection:release-schedule',$1,990,false)`, seriesID); err != nil {
		t.Fatalf("seed collection claim: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO virtual_media_file_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,file_path)
		VALUES(11,'collection:release-schedule',$1,990,'virtual://series/tvdb/80')`, seriesID); err != nil {
		t.Fatalf("seed collection file claim: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_items(collection_id,media_item_id,position)
		VALUES('release-schedule',$1,1)`, seriesID); err != nil {
		t.Fatalf("seed collection membership: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO seasons(content_id,series_id,season_number,title)
		VALUES('season-tvdb-80-1',$1,1,'Season 1')
		ON CONFLICT (content_id) DO NOTHING`, seriesID); err != nil {
		t.Fatalf("seed season: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM media_files WHERE content_id=$1 AND episode_id IS NOT NULL`, seriesID); err != nil {
		t.Fatalf("clean initial episode files: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO episodes(
			content_id,series_id,season_id,season_number,episode_number,title,air_date
		) VALUES
			('episode-tvdb-80-1-1',$1,'season-tvdb-80-1',1,1,'Released',CURRENT_DATE),
			('episode-tvdb-80-1-2',$1,'season-tvdb-80-1',1,2,'Future',CURRENT_DATE+1)
		ON CONFLICT (content_id) DO UPDATE SET air_date=EXCLUDED.air_date, title=EXCLUDED.title`,
		seriesID); err != nil {
		t.Fatalf("seed episodes: %v", err)
	}

	reconciled, err := repo.ReconcileReleasedCollectionVirtualEpisodes(ctx, 100)
	if err != nil {
		t.Fatalf("reconcile released episodes: %v", err)
	}
	if reconciled != 1 {
		t.Fatalf("reconciled=%d, want 1", reconciled)
	}
	var released, future int
	if err := pool.QueryRow(ctx, `
		SELECT
		  count(*) FILTER(WHERE episode_id='episode-tvdb-80-1-1'),
		  count(*) FILTER(WHERE episode_id='episode-tvdb-80-1-2')
		FROM media_files WHERE content_id=$1 AND probe_source='virtual_collection'`,
		seriesID,
	).Scan(&released, &future); err != nil {
		t.Fatalf("inspect scheduled episode files: %v", err)
	}
	if released != 1 || future != 0 {
		t.Fatalf("released files=%d future files=%d", released, future)
	}
}

func TestVirtualMediaUpsertRemovesStaleVariantsWithinSourceClaim(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	reg := newReleasedVirtualMediaRegistrar(pool)
	if _, err := pool.Exec(ctx, "INSERT INTO media_folders(id,name,type,enabled) VALUES(995,'VariantReconcile','movies',true)"); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	input := VirtualMedia{
		LibraryID: "995", MediaType: "movie", Title: "Variant Reconcile",
		IMDbID: "tt30", TMDBID: "30", Source: "provider-a",
		Year: 2020,
		Variants: []VirtualMediaVariant{
			{VirtualURI: "virtual://movie/tt30?result=a"},
			{VirtualURI: "virtual://movie/tt30?result=b"},
		},
	}
	result, err := reg.UpsertVirtualMedia(ctx, 11, input)
	if err != nil {
		t.Fatalf("initial variant upsert: %v", err)
	}
	input.Variants = input.Variants[:1]
	if _, err := reg.UpsertVirtualMedia(ctx, 11, input); err != nil {
		t.Fatalf("authoritative variant refresh: %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM media_files WHERE content_id=$1", result.MediaID).Scan(&count); err != nil {
		t.Fatalf("count refreshed variants: %v", err)
	}
	if count != 1 {
		t.Fatalf("stale variant remained: count=%d", count)
	}
}

func TestVirtualMediaUpsertPreservesLocalSeriesEpisodeMetadata(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled) VALUES(984,'LocalSeries','series',true);
		INSERT INTO media_items(content_id,type,title,tvdb_id,status)
		VALUES('series-tvdb-200','series','Local Series','200','matched');
		INSERT INTO seasons(content_id,series_id,season_number,title,metadata_source)
		VALUES('local-season-200-1','series-tvdb-200',1,'Local Season Title','local');
		INSERT INTO episodes(
			content_id,series_id,season_id,season_number,episode_number,title,overview,metadata_source
		) VALUES(
			'local-episode-200-1-1','series-tvdb-200','local-season-200-1',1,1,
			'Local Episode Title','Local overview','local'
		);
		INSERT INTO media_files(
			content_id,episode_id,media_folder_id,file_path,file_size,container
		) VALUES(
			'series-tvdb-200','local-episode-200-1-1',984,'/media/local-series-s01e01.mkv',1024,'mkv'
		)`); err != nil {
		t.Fatalf("seed local series: %v", err)
	}

	_, err := newReleasedVirtualMediaRegistrar(pool).UpsertVirtualMedia(ctx, 11, VirtualMedia{
		LibraryID: "984", MediaType: "series", Title: "Plugin Series", TVDBID: "200",
		Source: "provider-a", Episodes: []VirtualEpisode{{
			SeasonNumber: 1, EpisodeNumber: 1, Title: "Plugin Episode",
			AirDate:  time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
			Overview: "Plugin overview", VirtualURI: "virtual://series/tvdb/200/1/1",
		}},
	})
	if err != nil {
		t.Fatalf("add virtual source to local series: %v", err)
	}
	var seasonTitle, episodeTitle, overview, virtualEpisodeID string
	if err := pool.QueryRow(ctx, `
		SELECT s.title,e.title,COALESCE(e.overview,''),vf.episode_id
		FROM seasons s
		JOIN episodes e ON e.season_id=s.content_id
		JOIN media_files vf ON vf.content_id=e.series_id AND vf.file_path='virtual://series/tvdb/200/1/1'
		WHERE s.series_id='series-tvdb-200' AND s.season_number=1 AND e.episode_number=1`,
	).Scan(&seasonTitle, &episodeTitle, &overview, &virtualEpisodeID); err != nil {
		t.Fatalf("inspect local+virtual episode: %v", err)
	}
	if seasonTitle != "Local Season Title" || episodeTitle != "Local Episode Title" || overview != "Local overview" {
		t.Fatalf("plugin overwrote local episode metadata: season=%q episode=%q overview=%q", seasonTitle, episodeTitle, overview)
	}
	if virtualEpisodeID != "local-episode-200-1-1" {
		t.Fatalf("virtual file episode_id=%q, want existing local episode ID", virtualEpisodeID)
	}
}

func TestVirtualMetadataOwnershipPromotesSurvivingSource(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(983,'OwnerFailover','movies',true)`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	reg := newReleasedVirtualMediaRegistrar(pool)
	first := VirtualMedia{
		LibraryID: "983", MediaType: "movie", Title: "Primary", IMDbID: "tt201", TMDBID: "201",
		Source: "provider-a", VirtualURI: "virtual://movie/tt201?result=a",
	}
	result, err := reg.UpsertVirtualMedia(ctx, 11, first)
	if err != nil {
		t.Fatalf("upsert primary: %v", err)
	}
	second := first
	second.Source = "provider-b"
	second.Title = "Secondary"
	second.VirtualURI = "virtual://movie/tt201?result=b"
	if _, err := reg.UpsertVirtualMedia(ctx, 22, second); err != nil {
		t.Fatalf("upsert secondary: %v", err)
	}
	if _, err := reg.ReconcileVirtualMedia(ctx, 11, "provider-a", []string{"movie-tmdb-999"}, []int{983}); err != nil {
		t.Fatalf("remove primary: %v", err)
	}
	second.Title = "Promoted Secondary"
	if _, err := reg.UpsertVirtualMedia(ctx, 22, second); err != nil {
		t.Fatalf("refresh promoted secondary: %v", err)
	}
	var title string
	var owner int
	var owns bool
	if err := pool.QueryRow(ctx, `
		SELECT mi.title,mi.virtual_owner_installation_id,claim.owns_item_metadata
		FROM media_items mi
		JOIN virtual_media_source_claims claim ON claim.content_id=mi.content_id
		WHERE mi.content_id=$1 AND claim.plugin_installation_id=22 AND claim.source_key='provider-b'`,
		result.MediaID,
	).Scan(&title, &owner, &owns); err != nil {
		t.Fatalf("inspect promoted owner: %v", err)
	}
	if title != "Promoted Secondary" || owner != 22 || !owns {
		t.Fatalf("survivor was not promoted: title=%q owner=%d owns=%v", title, owner, owns)
	}
}

func TestCollectionMaterializationReconcilesProfilesAndProviders(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled) VALUES(982,'ProfileReconcile','movies',true);
		INSERT INTO library_collections(id,library_id,slug,title,collection_type)
		VALUES('profile-reconcile',982,'profile-reconcile','Profile Reconcile','manual');
		INSERT INTO library_collection_libraries(collection_id,library_id)
		VALUES('profile-reconcile',982)`); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	repo := NewItemRepository(pool)
	item := &models.MediaItem{
		ContentID: "movie-tmdb-202", Type: "movie", Title: "Profiles", SortTitle: "Profiles",
		TmdbID: "202", ImdbID: "tt202", Status: "matched",
	}
	initial := []VirtualPlaybackVariant{
		{VirtualURI: "virtual://movie/tt202?profile=1080p", Resolution: "1080p", OwnerInstallationID: 11},
		{VirtualURI: "virtual://movie/tt202?profile=4K", Resolution: "2160p", OwnerInstallationID: 11},
	}
	if _, err := repo.MaterializeVirtualPlaybackItemWithVariants(ctx, item, []int{982}, initial); err != nil {
		t.Fatalf("materialize profiles: %v", err)
	}
	collections := NewLibraryCollectionRepository(pool)
	if err := collections.ReplaceItems(ctx, "profile-reconcile", []LibraryCollectionItemInput{{MediaItemID: item.ContentID}}); err != nil {
		t.Fatalf("claim initial profiles: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO virtual_media_source_claims(
			plugin_installation_id,source_key,content_id,media_folder_id,owns_item_metadata
		) VALUES(11,'collection',$1,982,false)`, item.ContentID); err != nil {
		t.Fatalf("seed default collection source claim: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO virtual_media_file_source_claims(
			plugin_installation_id,source_key,content_id,media_folder_id,file_path
		) VALUES(11,'collection',$1,982,'virtual://movie/tt202?profile=4K')`, item.ContentID); err != nil {
		t.Fatalf("seed default collection file claim: %v", err)
	}
	if _, err := repo.MaterializeVirtualPlaybackItemWithVariants(ctx, item, []int{982}, initial[:1]); err != nil {
		t.Fatalf("remove profile: %v", err)
	}
	var old4K, defaultClaims int
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM media_files WHERE content_id=$1 AND file_path='virtual://movie/tt202?profile=4K'),
		  (SELECT count(*) FROM virtual_media_source_claims WHERE content_id=$1 AND source_key='collection')`,
		item.ContentID,
	).Scan(&old4K, &defaultClaims); err != nil {
		t.Fatalf("count removed profile: %v", err)
	}
	if old4K != 0 || defaultClaims != 0 {
		t.Fatalf("removed profile still has files=%d default_claims=%d", old4K, defaultClaims)
	}
	if _, err := repo.MaterializeVirtualPlaybackItemWithVariants(ctx, item, []int{982}, []VirtualPlaybackVariant{{
		VirtualURI: "virtual://movie/tt202", OwnerInstallationID: 22,
	}}); err != nil {
		t.Fatalf("switch provider: %v", err)
	}
	var owner11, owner22 int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER(WHERE virtual_owner_installation_id=11),
		       count(*) FILTER(WHERE virtual_owner_installation_id=22)
		FROM media_files WHERE content_id=$1 AND probe_source='virtual_collection'`, item.ContentID).Scan(&owner11, &owner22); err != nil {
		t.Fatalf("inspect provider switch: %v", err)
	}
	if owner11 != 0 || owner22 != 1 {
		t.Fatalf("provider switch left old=%d new=%d files", owner11, owner22)
	}
}

func TestCollectionVirtualFilesAreScopedToLibrary(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled) VALUES
			(981,'Movies A','movies',true),(980,'Movies B','movies',true)`); err != nil {
		t.Fatalf("seed folders: %v", err)
	}
	repo := NewItemRepository(pool)
	item := &models.MediaItem{
		ContentID: "movie-tmdb-203", Type: "movie", Title: "Two Libraries", SortTitle: "Two Libraries",
		TmdbID: "203", ImdbID: "tt203", Status: "matched",
	}
	variant := []VirtualPlaybackVariant{{VirtualURI: "virtual://movie/tt203", OwnerInstallationID: 11}}
	if _, err := repo.MaterializeVirtualPlaybackItemWithVariants(ctx, item, []int{981}, variant); err != nil {
		t.Fatalf("materialize first library: %v", err)
	}
	if _, err := repo.MaterializeVirtualPlaybackItemWithVariants(ctx, item, []int{980}, variant); err != nil {
		t.Fatalf("materialize second library: %v", err)
	}
	var count, distinctFolders int
	if err := pool.QueryRow(ctx, `
		SELECT count(*),count(DISTINCT media_folder_id)
		FROM media_files
		WHERE content_id=$1 AND file_path='virtual://movie/tt203' AND virtual_owner_installation_id=11`,
		item.ContentID,
	).Scan(&count, &distinctFolders); err != nil {
		t.Fatalf("inspect library-scoped files: %v", err)
	}
	if count != 2 || distinctFolders != 2 {
		t.Fatalf("virtual files count=%d folders=%d, want 2/2", count, distinctFolders)
	}
}

func TestCollectionClaimsAndCleanupAreScopedToLibrary(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled) VALUES
			(976,'Claim Scope A','movies',true),(975,'Claim Scope B','movies',true);
		INSERT INTO library_collections(id,library_id,slug,title,collection_type) VALUES
			('claim-scope-a',976,'claim-scope-a','Claim Scope A','manual'),
			('claim-scope-b',975,'claim-scope-b','Claim Scope B','manual');
		INSERT INTO library_collection_libraries(collection_id,library_id) VALUES
			('claim-scope-a',976),('claim-scope-b',975)`); err != nil {
		t.Fatalf("seed scoped collections: %v", err)
	}
	repo := NewItemRepository(pool)
	item := &models.MediaItem{
		ContentID: "movie-tmdb-206", Type: "movie", Title: "Scoped Claims",
		SortTitle: "Scoped Claims", TmdbID: "206", ImdbID: "tt206", Status: "matched",
	}
	variant := []VirtualPlaybackVariant{{
		VirtualURI: "virtual://movie/tt206", OwnerInstallationID: 11,
	}}
	if _, err := repo.MaterializeVirtualPlaybackItemWithVariants(ctx, item, []int{976}, variant); err != nil {
		t.Fatalf("materialize first library: %v", err)
	}
	if _, err := repo.MaterializeVirtualPlaybackItemWithVariants(ctx, item, []int{975}, variant); err != nil {
		t.Fatalf("materialize second library: %v", err)
	}
	collections := NewLibraryCollectionRepository(pool)
	if err := collections.ReplaceItems(ctx, "claim-scope-a", []LibraryCollectionItemInput{{MediaItemID: item.ContentID}}); err != nil {
		t.Fatalf("claim first collection: %v", err)
	}
	if err := collections.ReplaceItems(ctx, "claim-scope-b", []LibraryCollectionItemInput{{MediaItemID: item.ContentID}}); err != nil {
		t.Fatalf("claim second collection: %v", err)
	}
	var claimsA, claimsB int
	if err := pool.QueryRow(ctx, `
		SELECT
		  count(*) FILTER(WHERE source_key='collection:claim-scope-a' AND media_folder_id=976),
		  count(*) FILTER(WHERE source_key='collection:claim-scope-b' AND media_folder_id=975)
		FROM virtual_media_file_source_claims WHERE content_id=$1`, item.ContentID,
	).Scan(&claimsA, &claimsB); err != nil {
		t.Fatalf("inspect scoped collection claims: %v", err)
	}
	if claimsA != 1 || claimsB != 1 {
		t.Fatalf("scoped claims first=%d second=%d, want 1/1", claimsA, claimsB)
	}
	var crossed int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM virtual_media_file_source_claims
		WHERE content_id=$1 AND (
		  (source_key='collection:claim-scope-a' AND media_folder_id<>976)
		  OR (source_key='collection:claim-scope-b' AND media_folder_id<>975)
		)`, item.ContentID).Scan(&crossed); err != nil {
		t.Fatalf("inspect crossed collection claims: %v", err)
	}
	if crossed != 0 {
		t.Fatalf("collections claimed %d virtual files outside their libraries", crossed)
	}

	if err := collections.ReplaceItems(ctx, "claim-scope-a", nil); err != nil {
		t.Fatalf("remove first collection item: %v", err)
	}
	var filesA, filesB, owner int
	var ownerSource string
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM media_files WHERE content_id=$1 AND media_folder_id=976),
		  (SELECT count(*) FROM media_files WHERE content_id=$1 AND media_folder_id=975),
		  virtual_owner_installation_id,virtual_source
		FROM media_items WHERE content_id=$1`, item.ContentID,
	).Scan(&filesA, &filesB, &owner, &ownerSource); err != nil {
		t.Fatalf("inspect scoped collection cleanup: %v", err)
	}
	if filesA != 0 || filesB != 1 || owner != 11 || ownerSource != "collection:claim-scope-b" {
		t.Fatalf("cleanup files=%d/%d owner=%d source=%q", filesA, filesB, owner, ownerSource)
	}
}

func TestReleasedEpisodeReconciliationMaintainsCollectionClaims(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled) VALUES(979,'EpisodeClaims','series',true);
		INSERT INTO library_collections(id,library_id,slug,title,collection_type)
		VALUES('episode-claims',979,'episode-claims','Episode Claims','manual');
		INSERT INTO library_collection_libraries(collection_id,library_id)
		VALUES('episode-claims',979)`); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	repo := NewItemRepository(pool)
	const seriesID = "series-tvdb-204"
	if _, err := repo.MaterializeVirtualPlaybackItemWithVariants(ctx, &models.MediaItem{
		ContentID: seriesID, Type: "series", Title: "Episode Claims", SortTitle: "Episode Claims",
		TvdbID: "204", Status: "matched",
	}, []int{979}, []VirtualPlaybackVariant{{VirtualURI: "virtual://series/tvdb/204", OwnerInstallationID: 11}}); err != nil {
		t.Fatalf("materialize series: %v", err)
	}
	if err := NewLibraryCollectionRepository(pool).ReplaceItems(ctx, "episode-claims", []LibraryCollectionItemInput{{MediaItemID: seriesID}}); err != nil {
		t.Fatalf("claim series: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO virtual_media_source_claims(
			plugin_installation_id,source_key,content_id,media_folder_id,owns_item_metadata
		) VALUES(11,'collection',$1,979,false)`, seriesID); err != nil {
		t.Fatalf("seed default collection episode source claim: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO virtual_media_file_source_claims(
			plugin_installation_id,source_key,content_id,media_folder_id,file_path
		) VALUES(11,'collection',$1,979,'virtual://series/tvdb/204')`, seriesID); err != nil {
		t.Fatalf("seed default collection episode file claim: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO seasons(content_id,series_id,season_number,title)
		VALUES('season-tvdb-204-1',$1,1,'Season 1')
		ON CONFLICT (content_id) DO NOTHING`, seriesID); err != nil {
		t.Fatalf("seed season: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO episodes(content_id,series_id,season_id,season_number,episode_number,title,air_date)
		VALUES('episode-tvdb-204-1-1',$1,'season-tvdb-204-1',1,1,'Released',CURRENT_DATE)
		ON CONFLICT (content_id) DO UPDATE SET air_date=EXCLUDED.air_date, title=EXCLUDED.title`, seriesID); err != nil {
		t.Fatalf("seed released episode: %v", err)
	}
	if err := repo.MaterializeVirtualPlaybackEpisodes(ctx, seriesID); err != nil {
		t.Fatalf("materialize released episode: %v", err)
	}
	const episodePath = "virtual://series/tvdb/204/1/1"
	var files, claims, defaultClaims int
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM media_files WHERE content_id=$1 AND file_path=$2),
		  (SELECT count(*) FROM virtual_media_file_source_claims WHERE content_id=$1 AND file_path=$2 AND source_key='collection:episode-claims'),
		  (SELECT count(*) FROM virtual_media_file_source_claims WHERE content_id=$1 AND file_path=$2 AND source_key='collection')`,
		seriesID, episodePath,
	).Scan(&files, &claims, &defaultClaims); err != nil {
		t.Fatalf("inspect released episode claims: %v", err)
	}
	if files != 1 || claims != 1 || defaultClaims != 1 {
		t.Fatalf("released episode files=%d claims=%d default_claims=%d, want 1/1/1", files, claims, defaultClaims)
	}
	if _, err := pool.Exec(ctx, `UPDATE episodes SET air_date=CURRENT_DATE+1 WHERE content_id='episode-tvdb-204-1-1'`); err != nil {
		t.Fatalf("move episode to future: %v", err)
	}
	if err := repo.MaterializeVirtualPlaybackEpisodes(ctx, seriesID); err != nil {
		t.Fatalf("remove future episode: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM media_files WHERE content_id=$1 AND file_path=$2),
		  (SELECT count(*) FROM virtual_media_file_source_claims WHERE content_id=$1 AND file_path=$2)`,
		seriesID, episodePath,
	).Scan(&files, &claims); err != nil {
		t.Fatalf("inspect removed episode claims: %v", err)
	}
	if files != 0 || claims != 0 {
		t.Fatalf("future episode left files=%d claims=%d", files, claims)
	}
}

func TestFailedCollectionCleanupRemovesVirtualAdditionsFromExistingItem(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled) VALUES
			(978,'Local Movies','movies',true),(977,'Virtual Movies','movies',true);
		INSERT INTO media_items(content_id,type,title,tmdb_id,status)
		VALUES('movie-tmdb-205','movie','Existing Local','205','matched');
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container)
		VALUES('movie-tmdb-205',978,'/media/existing-local.mkv',1024,'mkv');
		INSERT INTO media_item_libraries(content_id,media_folder_id)
		VALUES('movie-tmdb-205',978)`); err != nil {
		t.Fatalf("seed existing item: %v", err)
	}
	repo := NewItemRepository(pool)
	if _, err := repo.MaterializeVirtualPlaybackItemWithVariants(ctx, &models.MediaItem{
		ContentID: "movie-tmdb-205", Type: "movie", Title: "Existing Local",
		SortTitle: "Existing Local", TmdbID: "205", ImdbID: "tt205", Status: "matched",
	}, []int{977}, []VirtualPlaybackVariant{{VirtualURI: "virtual://movie/tt205", OwnerInstallationID: 11}}); err != nil {
		t.Fatalf("materialize existing item: %v", err)
	}
	if _, err := repo.CleanupUnreferencedCollectionVirtualItems(ctx, "collection-205", []string{"movie-tmdb-205"}); err != nil {
		t.Fatalf("cleanup failed collection effects: %v", err)
	}
	var localFiles, virtualFiles, localLinks, virtualLinks int
	if err := pool.QueryRow(ctx, `
		SELECT
		  count(*) FILTER(WHERE file_path='/media/existing-local.mkv'),
		  count(*) FILTER(WHERE file_path='virtual://movie/tt205'),
		  (SELECT count(*) FROM media_item_libraries WHERE content_id='movie-tmdb-205' AND media_folder_id=978),
		  (SELECT count(*) FROM media_item_libraries WHERE content_id='movie-tmdb-205' AND media_folder_id=977)
		FROM media_files WHERE content_id='movie-tmdb-205'`,
	).Scan(&localFiles, &virtualFiles, &localLinks, &virtualLinks); err != nil {
		t.Fatalf("inspect failed collection cleanup: %v", err)
	}
	if localFiles != 1 || virtualFiles != 0 || localLinks != 1 || virtualLinks != 0 {
		t.Fatalf("cleanup local=%d virtual=%d local_links=%d virtual_links=%d", localFiles, virtualFiles, localLinks, virtualLinks)
	}
}

func TestPurgeVirtualPlaybackItemsRemovesOwnerZeroClaimsFromLocalItem(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled)
		VALUES(972,'Owner Zero Purge','movies',true);
		INSERT INTO media_items(content_id,type,title,tmdb_id,status)
		VALUES('movie-tmdb-209','movie','Owner Zero Purge','209','matched');
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container)
		VALUES('movie-tmdb-209',972,'/media/owner-zero-local.mkv',1024,'mkv')`); err != nil {
		t.Fatalf("seed local item: %v", err)
	}
	if _, err := newReleasedVirtualMediaRegistrar(pool).Upsert(ctx, VirtualMedia{
		LibraryID: "972", MediaType: "movie", Title: "Owner Zero Purge",
		IMDbID: "tt209", TMDBID: "209", Source: "generic-source",
		Year:       2020,
		VirtualURI: "virtual://movie/tt209",
	}); err != nil {
		t.Fatalf("add generic virtual source: %v", err)
	}
	purgeResult, err := NewItemRepository(pool).PurgeVirtualPlaybackItems(ctx, VirtualPurgeOptions{})
	if err != nil {
		t.Fatalf("purge generic virtual source: %v", err)
	}
	if purgeResult.FilesDeleted != 1 || purgeResult.ItemsDeleted != 0 {
		t.Fatalf("purge files=%d items=%d, want 1/0", purgeResult.FilesDeleted, purgeResult.ItemsDeleted)
	}
	var localFiles, sourceClaims, fileClaims int
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM media_files WHERE content_id='movie-tmdb-209'),
		  (SELECT count(*) FROM virtual_media_source_claims WHERE content_id='movie-tmdb-209'),
		  (SELECT count(*) FROM virtual_media_file_source_claims WHERE content_id='movie-tmdb-209')`,
	).Scan(&localFiles, &sourceClaims, &fileClaims); err != nil {
		t.Fatalf("inspect generic purge: %v", err)
	}
	if localFiles != 1 || sourceClaims != 0 || fileClaims != 0 {
		t.Fatalf("generic purge local=%d source_claims=%d file_claims=%d", localFiles, sourceClaims, fileClaims)
	}
}

func TestScopedPurgePreservesSharedLegacyFileAndPromotesOwner(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled)
		VALUES(971,'Shared Legacy Purge','movies',true);
		INSERT INTO media_items(
			content_id,type,title,tmdb_id,status,
			virtual_owner_installation_id,virtual_source,virtual_last_seen_at
		) VALUES('movie-tmdb-210','movie','Shared Legacy Purge','210','matched',11,'provider-a',NOW());
		INSERT INTO media_item_libraries(content_id,media_folder_id)
		VALUES('movie-tmdb-210',971);
		INSERT INTO media_files(
			content_id,media_folder_id,file_path,file_size,container,
			probe_source,virtual_owner_installation_id
		) VALUES('movie-tmdb-210',971,'virtual://movie/tt210',0,'virtual','virtual',0);
		INSERT INTO virtual_media_source_claims(
			plugin_installation_id,source_key,content_id,media_folder_id,owns_item_metadata
		) VALUES
			(11,'provider-a','movie-tmdb-210',971,true),
			(22,'provider-b','movie-tmdb-210',971,false);
		INSERT INTO virtual_media_file_source_claims(
			plugin_installation_id,source_key,content_id,media_folder_id,file_path
		) VALUES
			(11,'provider-a','movie-tmdb-210',971,'virtual://movie/tt210'),
			(22,'provider-b','movie-tmdb-210',971,'virtual://movie/tt210')`); err != nil {
		t.Fatalf("seed shared legacy file: %v", err)
	}
	purgeResult, err := NewItemRepository(pool).PurgeVirtualPlaybackItems(ctx, VirtualPurgeOptions{InstallationID: 11})
	if err != nil {
		t.Fatalf("purge first legacy owner: %v", err)
	}
	if purgeResult.FilesDeleted != 0 || purgeResult.ItemsDeleted != 0 {
		t.Fatalf("scoped purge files=%d items=%d, want shared file preserved", purgeResult.FilesDeleted, purgeResult.ItemsDeleted)
	}
	var fileCount, claims11, claims22, owner int
	var source string
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM media_files WHERE content_id='movie-tmdb-210'),
		  (SELECT count(*) FROM virtual_media_source_claims WHERE content_id='movie-tmdb-210' AND plugin_installation_id=11),
		  (SELECT count(*) FROM virtual_media_source_claims WHERE content_id='movie-tmdb-210' AND plugin_installation_id=22),
		  virtual_owner_installation_id,virtual_source
		FROM media_items WHERE content_id='movie-tmdb-210'`,
	).Scan(&fileCount, &claims11, &claims22, &owner, &source); err != nil {
		t.Fatalf("inspect shared legacy purge: %v", err)
	}
	if fileCount != 1 || claims11 != 0 || claims22 != 1 || owner != 22 || source != "provider-b" {
		t.Fatalf("shared purge file=%d claims=%d/%d owner=%d source=%q", fileCount, claims11, claims22, owner, source)
	}
}

func TestRemoveVirtualMediaInstallationCleansLegacyOwnerRowsAndPreservesSharedClaims(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled)
		VALUES(970,'Shared Legacy Uninstall','movies',true);
		INSERT INTO media_items(
			content_id,type,title,tmdb_id,status,
			virtual_owner_installation_id,virtual_source,virtual_last_seen_at
		) VALUES('movie-tmdb-211','movie','Shared Legacy Uninstall','211','matched',11,'provider-a',NOW());
		INSERT INTO media_files(
			content_id,media_folder_id,file_path,file_size,container,
			probe_source,virtual_owner_installation_id
		) VALUES
			('movie-tmdb-211',970,'virtual://movie/tt211?result=a',0,'virtual','virtual',0),
			('movie-tmdb-211',970,'virtual://movie/tt211?result=b',0,'virtual','virtual',NULL);
		INSERT INTO virtual_media_source_claims(
			plugin_installation_id,source_key,content_id,media_folder_id,owns_item_metadata
		) VALUES
			(11,'provider-a','movie-tmdb-211',970,true),
			(22,'provider-b','movie-tmdb-211',970,false);
		INSERT INTO virtual_media_file_source_claims(
			plugin_installation_id,source_key,content_id,media_folder_id,file_path
		) VALUES
			(11,'provider-a','movie-tmdb-211',970,'virtual://movie/tt211?result=a'),
			(22,'provider-b','movie-tmdb-211',970,'virtual://movie/tt211?result=a'),
			(11,'provider-a','movie-tmdb-211',970,'virtual://movie/tt211?result=b'),
			(22,'provider-b','movie-tmdb-211',970,'virtual://movie/tt211?result=b')`); err != nil {
		t.Fatalf("seed shared legacy uninstall rows: %v", err)
	}

	result, err := newReleasedVirtualMediaRegistrar(pool).RemoveInstallationVirtualMedia(ctx, 11)
	if err != nil {
		t.Fatalf("remove installation virtual media: %v", err)
	}
	if result.FilesRemoved != 0 || result.ItemsRemoved != 0 {
		t.Fatalf("cleanup removed files=%d items=%d, want 0/0", result.FilesRemoved, result.ItemsRemoved)
	}

	var fileCount, claims11, claims22, owner int
	var source string
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM media_files WHERE content_id='movie-tmdb-211'),
		  (SELECT count(*) FROM virtual_media_source_claims WHERE content_id='movie-tmdb-211' AND plugin_installation_id=11),
		  (SELECT count(*) FROM virtual_media_source_claims WHERE content_id='movie-tmdb-211' AND plugin_installation_id=22),
		  virtual_owner_installation_id,virtual_source
		FROM media_items WHERE content_id='movie-tmdb-211'`,
	).Scan(&fileCount, &claims11, &claims22, &owner, &source); err != nil {
		t.Fatalf("inspect shared legacy uninstall rows: %v", err)
	}
	if fileCount != 2 || claims11 != 0 || claims22 != 1 || owner != 22 || source != "provider-b" {
		t.Fatalf("shared uninstall file=%d claims=%d/%d owner=%d source=%q", fileCount, claims11, claims22, owner, source)
	}

	var ownerZero, owner22 int
	if err := pool.QueryRow(ctx, `
		SELECT
		  count(*) FILTER(WHERE virtual_owner_installation_id=0 OR virtual_owner_installation_id IS NULL),
		  count(*) FILTER(WHERE virtual_owner_installation_id=22)
		FROM media_files WHERE content_id='movie-tmdb-211'`,
	).Scan(&ownerZero, &owner22); err != nil {
		t.Fatalf("inspect promoted legacy file owners: %v", err)
	}
	if ownerZero != 0 || owner22 != 2 {
		t.Fatalf("promoted legacy file owners zero=%d owner22=%d, want 0/2", ownerZero, owner22)
	}
}

// Installation-scoped purges must not delete rows belonging to another
// installation. Ownership currently lives on media_items (one owner per
// content ID); this test pins that invariant until per-file ownership is
// introduced.
func TestPurgeVirtualPlaybackItemsKeepsOtherInstallation(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	const contentID = "movie-tmdb-purge-owner-b"
	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(998,'PurgeTest','mixed',true)`); err != nil {
		t.Fatalf("seed purge test folder: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO media_items(content_id,type,title,virtual_owner_installation_id,virtual_source) VALUES($1,'movie','Purge Test',22,'provider-b')`, contentID); err != nil {
		t.Fatalf("seed other-installation item: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,probe_source) VALUES($1,998,'virtual://movie/purge-owner-b',0,'virtual','virtual')`, contentID); err != nil {
		t.Fatalf("seed other-installation virtual item: %v", err)
	}

	purgeResult, err := (&ItemRepository{pool: pool}).PurgeVirtualPlaybackItems(ctx, VirtualPurgeOptions{InstallationID: 11})
	if err != nil {
		t.Fatalf("scoped purge failed: %v", err)
	}
	if purgeResult.FilesDeleted != 0 || purgeResult.ItemsDeleted != 0 {
		t.Fatalf("purge of installation 11 removed files=%d items=%d owned by installation 22", purgeResult.FilesDeleted, purgeResult.ItemsDeleted)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM media_files WHERE file_path='virtual://movie/purge-owner-b'").Scan(&count); err != nil {
		t.Fatalf("check preserved file: %v", err)
	}
	if count != 1 {
		t.Fatalf("other installation's virtual file count=%d, want 1", count)
	}
}

// A purge must take the per-user consumption state with it: those tables
// reference content by bare ID without foreign keys, so purged titles kept
// showing up in Continue Watching and history after the catalog was clean.
func TestPurgeVirtualPlaybackItemsRemovesDanglingUserState(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	// The shared test pool only resets catalog tables; drop any rows a
	// previous run of this test left behind.
	_, _ = pool.Exec(ctx, `DELETE FROM user_profiles WHERE name = 'Purge Profile'`)
	_, _ = pool.Exec(ctx, `DELETE FROM users WHERE username = 'purge-state-user'`)
	var userID int
	if err := pool.QueryRow(ctx, `INSERT INTO users (username, role) VALUES ('purge-state-user', 'user') RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	const profileID = "profile-purge-1"
	if _, err := pool.Exec(ctx, `INSERT INTO user_profiles (id, user_id, name) VALUES ($1, $2, 'Purge Profile')`, profileID, userID); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{sql: `INSERT INTO media_folders(id,name,type,enabled) VALUES(996,'StatePurge','movies',true)`},
		{sql: `INSERT INTO media_items(content_id,type,title,status) VALUES('movie-tmdb-996','movie','State Purge Movie','matched')`},
		{sql: `INSERT INTO media_item_libraries(content_id,media_folder_id) VALUES('movie-tmdb-996',996)`},
		{sql: `INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,probe_source,virtual_owner_installation_id)
		  VALUES('movie-tmdb-996',996,'virtual://movie/tt996',0,'virtual','virtual',7)`},
		{sql: `INSERT INTO user_watch_progress(user_id,profile_id,media_item_id,position_seconds,duration_seconds)
		  VALUES($1,$2,'movie-tmdb-996',300,3600)`, args: []any{userID, profileID}},
		{sql: `INSERT INTO user_favorites(user_id,profile_id,media_item_id) VALUES($1,$2,'movie-tmdb-996')`, args: []any{userID, profileID}},
		{sql: `INSERT INTO user_ratings(user_id,profile_id,media_item_id,rating,rated_at) VALUES($1,$2,'movie-tmdb-996',4,NOW())`, args: []any{userID, profileID}},
		{sql: `INSERT INTO user_watchlist(user_id,profile_id,media_item_id,added_at) VALUES($1,$2,'movie-tmdb-996',NOW())`, args: []any{userID, profileID}},
		{sql: `INSERT INTO metadata_refresh_debt(content_id,priority,reason_mask,next_refresh_at,target_type)
		  VALUES('movie-tmdb-996',5,1,NOW(),'item')`},
	} {
		if _, err := pool.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed virtual item with user state: %v", err)
		}
	}

	purgeResult, err := (&ItemRepository{pool: pool}).PurgeVirtualPlaybackItems(ctx, VirtualPurgeOptions{})
	if err != nil {
		t.Fatalf("purge failed: %v", err)
	}
	if purgeResult.FilesDeleted != 1 || purgeResult.ItemsDeleted != 1 {
		t.Fatalf("purge files=%d items=%d, want 1/1", purgeResult.FilesDeleted, purgeResult.ItemsDeleted)
	}
	if purgeResult.StateRowsDeleted < 5 {
		t.Fatalf("state_rows_deleted=%d, want at least the five seeded state rows", purgeResult.StateRowsDeleted)
	}
	var progressLeft, favoritesLeft, ratingsLeft, watchlistLeft, debtLeft int
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM user_watch_progress WHERE media_item_id='movie-tmdb-996'),
		  (SELECT count(*) FROM user_favorites WHERE media_item_id='movie-tmdb-996'),
		  (SELECT count(*) FROM user_ratings WHERE media_item_id='movie-tmdb-996'),
		  (SELECT count(*) FROM user_watchlist WHERE media_item_id='movie-tmdb-996'),
		  (SELECT count(*) FROM metadata_refresh_debt WHERE content_id='movie-tmdb-996')`,
	).Scan(&progressLeft, &favoritesLeft, &ratingsLeft, &watchlistLeft, &debtLeft); err != nil {
		t.Fatalf("inspect post-purge state: %v", err)
	}
	if progressLeft+favoritesLeft+ratingsLeft+watchlistLeft+debtLeft != 0 {
		t.Fatalf("dangling state survived: progress=%d favorites=%d ratings=%d watchlist=%d debt=%d",
			progressLeft, favoritesLeft, ratingsLeft, watchlistLeft, debtLeft)
	}
}

// A file-less virtual item must not survive a purge just because a collection
// references it: an admin purge is "gone is gone", and the dangling collection
// link is cleaned with the item.
func TestPurgeVirtualPlaybackItemsRemovesCollectionLinkedShell(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(995,'ShellPurge','movies',true)`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO media_items(content_id,type,title,status) VALUES('movie-tmdb-995','movie','Shell Purge Movie','matched')`); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,probe_source,virtual_owner_installation_id)
		VALUES('movie-tmdb-995',995,'virtual://movie/tt995',0,'virtual','virtual',9)`); err != nil {
		t.Fatalf("seed virtual file: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO library_collections(id,library_id,slug,title,collection_type)
		VALUES('shell-purge-collection',995,'shell-purge','Shell purge','manual')`); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO library_collection_items(collection_id,media_item_id) VALUES('shell-purge-collection','movie-tmdb-995')`); err != nil {
		t.Fatalf("seed collection membership: %v", err)
	}

	purgeResult, err := (&ItemRepository{pool: pool}).PurgeVirtualPlaybackItems(ctx, VirtualPurgeOptions{})
	if err != nil {
		t.Fatalf("purge failed: %v", err)
	}
	if purgeResult.ItemsDeleted != 1 {
		t.Fatalf("items_deleted=%d, want the collection-linked shell removed", purgeResult.ItemsDeleted)
	}
	var itemsLeft, linksLeft, collectionsLeft int
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM media_items WHERE content_id='movie-tmdb-995'),
		  (SELECT count(*) FROM library_collection_items WHERE media_item_id='movie-tmdb-995'),
		  (SELECT count(*) FROM library_collections WHERE id='shell-purge-collection')`,
	).Scan(&itemsLeft, &linksLeft, &collectionsLeft); err != nil {
		t.Fatalf("inspect post-purge state: %v", err)
	}
	if itemsLeft != 0 || linksLeft != 0 {
		t.Fatalf("shell survived: items=%d links=%d", itemsLeft, linksLeft)
	}
	if collectionsLeft != 1 {
		t.Fatalf("the collection itself must survive its members: %d", collectionsLeft)
	}
}

func TestPurgeVirtualPlaybackItemsRemovesSeriesAndOrphanedItems(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled) VALUES(997,'SeriesPurge','series',true);
		INSERT INTO media_items(content_id,type,title,status) VALUES('series-tvdb-999','series','Purge TV Show','matched');
		INSERT INTO media_item_libraries(content_id,media_folder_id) VALUES('series-tvdb-999',997);
		INSERT INTO seasons(content_id,series_id,season_number) VALUES('season-tvdb-999-1','series-tvdb-999',1);
		INSERT INTO episodes(content_id,series_id,season_id,season_number,episode_number,title)
		VALUES('episode-tvdb-999-1-1','series-tvdb-999','season-tvdb-999-1',1,1,'Episode 1');
		INSERT INTO media_files(episode_id,media_folder_id,file_path,file_size,container,probe_source,virtual_owner_installation_id)
		VALUES('episode-tvdb-999-1-1',997,'virtual://series/999/1/1',0,'virtual','virtual',5);
	`); err != nil {
		t.Fatalf("seed series virtual item: %v", err)
	}

	purgeResult, err := (&ItemRepository{pool: pool}).PurgeVirtualPlaybackItems(ctx, VirtualPurgeOptions{})
	if err != nil {
		t.Fatalf("purge series failed: %v", err)
	}
	if purgeResult.FilesDeleted != 1 || purgeResult.ItemsDeleted != 1 {
		t.Fatalf("purge files=%d items=%d, want 1/1", purgeResult.FilesDeleted, purgeResult.ItemsDeleted)
	}

	var seriesCount, epFileCount, libCount int
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM media_items WHERE content_id='series-tvdb-999'),
		  (SELECT count(*) FROM media_files WHERE file_path LIKE 'virtual://series/999%'),
		  (SELECT count(*) FROM media_item_libraries WHERE content_id='series-tvdb-999')
	`).Scan(&seriesCount, &epFileCount, &libCount); err != nil {
		t.Fatalf("check purged series: %v", err)
	}
	if seriesCount != 0 || epFileCount != 0 || libCount != 0 {
		t.Fatalf("after purge: seriesCount=%d epFileCount=%d libCount=%d, want 0/0/0", seriesCount, epFileCount, libCount)
	}
}

func TestVirtualMediaUpcomingEpisodesPreserveMetadataWithoutPlayableFiles(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	reg := newReleasedVirtualMediaRegistrar(pool)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "INSERT INTO media_folders(id,name,type,enabled) VALUES(999,'TestVirtual','mixed',true)"); err != nil {
		t.Fatalf("seed virtual media folder: %v", err)
	}

	now := time.Now()
	series := VirtualMedia{
		LibraryID: "999", MediaType: "series", Title: "Upcoming Test Series", IMDbID: "tt400", TMDBID: "4", Source: "provider-b",
		Episodes: []VirtualEpisode{
			{
				SeasonNumber: 1, EpisodeNumber: 1, Title: "Aired Episode", AirDate: now.Add(-24 * time.Hour),
				VirtualURI: "virtual://series/tt400/1/1?profile=1080p",
			},
			{
				SeasonNumber: 1, EpisodeNumber: 2, Title: "Future Episode (Blank URI)", AirDate: now.Add(24 * time.Hour),
				VirtualURI: "",
			},
			{
				SeasonNumber: 1, EpisodeNumber: 3, Title: "Undated Episode (Blank URI)",
				VirtualURI: "",
			},
		},
	}

	res, err := reg.UpsertVirtualMedia(ctx, 11, series)
	if err != nil {
		t.Fatalf("upsert series failed: %v", err)
	}
	if res.EpisodesUpserted != 1 {
		t.Fatalf("expected 1 playable episode upserted, got %d", res.EpisodesUpserted)
	}

	var episodeRowCount, mediaFileCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM episodes WHERE series_id=$1", res.MediaID).Scan(&episodeRowCount); err != nil {
		t.Fatalf("count episodes: %v", err)
	}
	if episodeRowCount != 3 {
		t.Fatalf("expected all 3 episodes in catalog metadata, got %d", episodeRowCount)
	}

	if err := pool.QueryRow(ctx, "SELECT count(*) FROM media_files WHERE content_id=$1", res.MediaID).Scan(&mediaFileCount); err != nil {
		t.Fatalf("count media files: %v", err)
	}
	if mediaFileCount != 1 {
		t.Fatalf("expected only 1 playable media file for aired episode, got %d", mediaFileCount)
	}

	var playedEpisodeID string
	if err := pool.QueryRow(ctx, "SELECT episode_id FROM media_files WHERE content_id=$1", res.MediaID).Scan(&playedEpisodeID); err != nil {
		t.Fatalf("query media file episode_id: %v", err)
	}
	if !strings.HasSuffix(playedEpisodeID, "-1-1") {
		t.Fatalf("expected media file to belong to S01E01, got %q", playedEpisodeID)
	}

	// Now postpone Episode 1 into the future as well
	series.Episodes[0].AirDate = now.Add(48 * time.Hour)
	resPostponed, err := reg.UpsertVirtualMedia(ctx, 11, series)
	if err != nil {
		t.Fatalf("upsert postponed series failed: %v", err)
	}
	if resPostponed.EpisodesUpserted != 0 {
		t.Fatalf("expected 0 playable episodes after postponement, got %d", resPostponed.EpisodesUpserted)
	}

	if err := pool.QueryRow(ctx, "SELECT count(*) FROM episodes WHERE series_id=$1", res.MediaID).Scan(&episodeRowCount); err != nil {
		t.Fatalf("count episodes after postponement: %v", err)
	}
	if episodeRowCount != 3 {
		t.Fatalf("metadata must still be preserved for 3 episodes, got %d", episodeRowCount)
	}

	if err := pool.QueryRow(ctx, "SELECT count(*) FROM media_files WHERE content_id=$1", res.MediaID).Scan(&mediaFileCount); err != nil {
		t.Fatalf("count media files after postponement: %v", err)
	}
	if mediaFileCount != 0 {
		t.Fatalf("expected 0 playable media files after postponement, got %d", mediaFileCount)
	}
}

func TestUpsertVirtualMedia_FutureAndUndatedMoviesPreserveMetadataWithoutPlayableFiles(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	reg := newReleasedVirtualMediaRegistrar(pool)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "INSERT INTO media_folders(id,name,type,enabled) VALUES(999,'TestVirtual','mixed',true)"); err != nil {
		t.Fatalf("seed virtual media folder: %v", err)
	}
	nowYear := time.Now().UTC().Year()

	// 1. Future movie
	futureMovie := VirtualMedia{
		LibraryID:      "999",
		MediaType:      "movie",
		Title:          "Future Movie 2099",
		Year:           nowYear + 5,
		IMDbID:         "tt8888001",
		Overview:       "A movie from the future",
		VirtualURI:     "virtual://movie/tt8888001",
		RuntimeMinutes: 120,
	}

	resFuture, err := reg.UpsertVirtualMedia(ctx, 11, futureMovie)
	if err != nil {
		t.Fatalf("upsert future movie failed: %v", err)
	}

	var futureItemCount, futureFileCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM media_items WHERE content_id=$1", resFuture.MediaID).Scan(&futureItemCount); err != nil || futureItemCount != 1 {
		t.Fatalf("expected future movie metadata in media_items, count=%d, err=%v", futureItemCount, err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM media_files WHERE content_id=$1", resFuture.MediaID).Scan(&futureFileCount); err != nil || futureFileCount != 0 {
		t.Fatalf("expected 0 media_files for future movie, got %d, err=%v", futureFileCount, err)
	}

	// 2. Undated movie (Year: 0)
	undatedMovie := VirtualMedia{
		LibraryID:      "999",
		MediaType:      "movie",
		Title:          "Undated Movie",
		Year:           0,
		IMDbID:         "tt8888002",
		Overview:       "A movie with no year",
		VirtualURI:     "virtual://movie/tt8888002",
		RuntimeMinutes: 90,
	}
	if _, err := reg.UpsertVirtualMedia(ctx, 11, undatedMovie); err == nil {
		t.Fatal("missing release identity must defer registration")
	}

	// 3. Current year movie without verified digital release fails closed (0 media_files)
	currentYearMovie := VirtualMedia{
		LibraryID:      "999",
		MediaType:      "movie",
		Title:          "Current Year Unverified",
		Year:           nowYear,
		IMDbID:         "tt8888004",
		Overview:       "A current-year movie without release checker",
		VirtualURI:     "virtual://movie/tt8888004",
		RuntimeMinutes: 110,
	}
	if _, err := reg.UpsertVirtualMedia(ctx, 11, currentYearMovie); err == nil {
		t.Fatal("unverified current movie must defer registration")
	}

	// 4. Current year movie WITH verified digital release creates 1 media_file
	regWithChecker := newReleasedVirtualMediaRegistrar(pool)
	regWithChecker.TMDBDigitalReleases = &fakeDigitalReleaseChecker{released: map[int]bool{777: true}}
	currentYearVerifiedMovie := VirtualMedia{
		LibraryID:      "999",
		MediaType:      "movie",
		Title:          "Current Year Verified",
		Year:           nowYear,
		TMDBID:         "777",
		IMDbID:         "tt8888005",
		Overview:       "A current-year movie with verified digital release",
		VirtualURI:     "virtual://movie/tt8888005",
		RuntimeMinutes: 115,
	}
	resCurrentVerified, err := regWithChecker.UpsertVirtualMedia(ctx, 11, currentYearVerifiedMovie)
	if err != nil {
		t.Fatalf("upsert current verified movie failed: %v", err)
	}
	var currentVerifiedFileCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM media_files WHERE content_id=$1", resCurrentVerified.MediaID).Scan(&currentVerifiedFileCount); err != nil || currentVerifiedFileCount != 1 {
		t.Fatalf("expected 1 media_file for verified current year movie, got %d, err=%v", currentVerifiedFileCount, err)
	}

	// 5. Released movie then postponed
	releasedMovie := VirtualMedia{
		LibraryID:      "999",
		MediaType:      "movie",
		Title:          "Postponed Movie",
		Year:           nowYear - 1,
		IMDbID:         "tt8888003",
		TMDBID:         "8888003",
		Overview:       "A movie that gets postponed",
		VirtualURI:     "virtual://movie/tt8888003",
		RuntimeMinutes: 100,
	}

	resReleased, err := reg.UpsertVirtualMedia(ctx, 11, releasedMovie)
	if err != nil {
		t.Fatalf("upsert released movie failed: %v", err)
	}

	var releasedFileCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM media_files WHERE content_id=$1", resReleased.MediaID).Scan(&releasedFileCount); err != nil || releasedFileCount != 1 {
		t.Fatalf("expected 1 media_file for released movie, got %d, err=%v", releasedFileCount, err)
	}

	reg.TMDBDigitalReleases = &fakeDigitalReleaseChecker{err: context.DeadlineExceeded}
	if _, err := reg.UpsertVirtualMedia(ctx, 11, releasedMovie); err == nil {
		t.Fatal("lookup outage must defer registration")
	}
	var retainedClaims int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM virtual_media_file_source_claims WHERE content_id=$1", resReleased.MediaID).Scan(&retainedClaims); err != nil || retainedClaims != 1 {
		t.Fatalf("outage removed file claims: count=%d err=%v", retainedClaims, err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM media_files WHERE content_id=$1", resReleased.MediaID).Scan(&releasedFileCount); err != nil || releasedFileCount != 1 {
		t.Fatalf("outage removed existing file: count=%d err=%v", releasedFileCount, err)
	}
	reg.TMDBDigitalReleases = releasedMovieChecker{}

	// Now update to future year
	releasedMovie.Year = nowYear + 2
	resPostponed, err := reg.UpsertVirtualMedia(ctx, 11, releasedMovie)
	if err != nil {
		t.Fatalf("upsert postponed movie failed: %v", err)
	}

	var postponedFileCount, postponedItemCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM media_files WHERE content_id=$1", resPostponed.MediaID).Scan(&postponedFileCount); err != nil || postponedFileCount != 0 {
		t.Fatalf("expected 0 media_files after movie postponement, got %d, err=%v", postponedFileCount, err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM media_items WHERE content_id=$1", resPostponed.MediaID).Scan(&postponedItemCount); err != nil || postponedItemCount != 1 {
		t.Fatalf("expected metadata preserved after movie postponement, got %d, err=%v", postponedItemCount, err)
	}
}

func TestVirtualMovieRegistrationHonorsStoredAliasOverride(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	sfx := uniqueReleaseSuffix(t)
	tmdbID := "4242" + sfx
	imdbID := "tt424242" + sfx[:6]
	reg := newReleasedVirtualMediaRegistrar(pool)
	if _, err := pool.Exec(ctx, "INSERT INTO media_folders(id,name,type,enabled) VALUES(996,'AliasOverride','movies',true)"); err != nil {
		t.Fatalf("seed virtual media folder: %v", err)
	}

	// Register with both IDs so the catalog row carries a known IMDb alias.
	first := VirtualMedia{
		LibraryID: "996", MediaType: "movie", Title: "Alias Movie", Year: 2020,
		TMDBID: tmdbID, IMDbID: imdbID, Source: "provider-a",
		Variants: []VirtualMediaVariant{
			{VirtualURI: "virtual://movie/" + tmdbID + "?profile=1080p", Resolution: "1080p"},
		},
	}
	res, err := reg.UpsertVirtualMedia(ctx, 11, first)
	if err != nil {
		t.Fatalf("initial upsert failed: %v", err)
	}
	var files int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM media_files WHERE content_id=$1 AND container='virtual'", res.MediaID).Scan(&files); err != nil || files != 1 {
		t.Fatalf("expected 1 file after initial upsert, got %d, err=%v", files, err)
	}

	// Record a future override against the IMDb alias directly.
	if _, err := pool.Exec(ctx, `INSERT INTO verified_release_override_history
		(media_type,provider,provider_id,season_number,episode_number,revision,action,release_at,evidence_note,actor_account_id)
		VALUES('movie','imdb','`+imdbID+`',0,0,1,'set','2099-01-01T00:00:00Z','verified future',1)`); err != nil {
		t.Fatalf("seed future IMDb override: %v", err)
	}

	// Retry registration with the TMDB identity only, omitting the known
	// IMDb alias. The merged identity set must still observe the override
	// and withdraw the files instead of replaying the pin.
	retry := VirtualMedia{
		LibraryID: "996", MediaType: "movie", Title: "Alias Movie", Year: 2020,
		TMDBID: tmdbID, Source: "provider-a",
		Variants: []VirtualMediaVariant{
			{VirtualURI: "virtual://movie/" + tmdbID + "?profile=1080p", Resolution: "1080p"},
		},
	}
	if _, err := reg.UpsertVirtualMedia(ctx, 11, retry); err != nil {
		t.Fatalf("alias-omitting upsert failed: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM media_files WHERE content_id=$1 AND container='virtual'", res.MediaID).Scan(&files); err != nil || files != 0 {
		t.Fatalf("expected 0 files while the alias override blocks, got %d, err=%v", files, err)
	}
	var reason string
	if err := pool.QueryRow(ctx, `SELECT reason FROM release_metadata_queue WHERE media_type='movie' AND provider='imdb' AND provider_id='`+imdbID+`'`).Scan(&reason); err != nil {
		t.Fatalf("blocked alias missing from metadata queue: %v", err)
	}
	if reason != "no_home_release" {
		t.Fatalf("queue reason = %q, want no_home_release", reason)
	}
}

func TestCollectionEpisodeMaterializationObservesCommittedOverride(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled) VALUES(975,'OverrideEpisodes','series',true);
		INSERT INTO library_collections(id,library_id,slug,title,collection_type)
		VALUES('override-episodes',975,'override-episodes','Override Episodes','manual');
		INSERT INTO library_collection_libraries(collection_id,library_id)
		VALUES('override-episodes',975)`); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	repo := NewItemRepository(pool)
	tvdbID := "206" + uniqueReleaseSuffix(t)
	seriesID := "series-tvdb-" + tvdbID
	baseURI := "virtual://series/tvdb/" + tvdbID
	episodePath := baseURI + "/1/1"
	if _, err := repo.MaterializeVirtualPlaybackItemWithVariants(ctx, &models.MediaItem{
		ContentID: seriesID, Type: "series", Title: "Override Series", SortTitle: "Override Series",
		TvdbID: tvdbID, Status: "matched",
	}, []int{975}, []VirtualPlaybackVariant{{VirtualURI: baseURI, OwnerInstallationID: 11}}); err != nil {
		t.Fatalf("materialize series: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO seasons(content_id,series_id,season_number,title)
		VALUES('season-tvdb-206-1',$1,1,'Season 1')
		ON CONFLICT (content_id) DO NOTHING`, seriesID); err != nil {
		t.Fatalf("seed season: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO episodes(content_id,series_id,season_id,season_number,episode_number,title,air_date)
		VALUES('episode-tvdb-206-1-1',$1,'season-tvdb-206-1',1,1,'Blocked',CURRENT_DATE)
		ON CONFLICT (content_id) DO UPDATE SET air_date=EXCLUDED.air_date`, seriesID); err != nil {
		t.Fatalf("seed episode: %v", err)
	}
	// A future verified override committed before materialization must win
	// over the aired date: no playable file may be created.
	if _, err := pool.Exec(ctx, `INSERT INTO verified_release_override_history
		(media_type,provider,provider_id,season_number,episode_number,revision,action,release_at,evidence_note,actor_account_id)
		VALUES('episode','tvdb','`+tvdbID+`',1,1,1,'set','2099-01-01T00:00:00Z','verified future',1)`); err != nil {
		t.Fatalf("seed future episode override: %v", err)
	}
	if err := repo.MaterializeVirtualPlaybackEpisodes(ctx, seriesID); err != nil {
		t.Fatalf("materialize blocked episode: %v", err)
	}
	var files int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_files WHERE content_id=$1 AND file_path='`+episodePath+`'`, seriesID).Scan(&files); err != nil || files != 0 {
		t.Fatalf("blocked episode materialized %d files, err=%v", files, err)
	}
}

func TestVirtualEpisodeRegistrationDefersToFutureOverrideOverLocalFiles(t *testing.T) {
	tvdbID := "207" + uniqueReleaseSuffix(t)
	seriesID := "series-tvdb-" + tvdbID
	episodePath := "virtual://series/tvdb/" + tvdbID + "/1/1"
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled) VALUES(974,'OverridePrecedence','series',true);
		INSERT INTO media_items(content_id,type,title,tvdb_id,status)
		VALUES('`+seriesID+`','series','Precedence Series','`+tvdbID+`','matched');
		INSERT INTO seasons(content_id,series_id,season_number,title,metadata_source)
		VALUES('local-season-207-1','`+seriesID+`',1,'Season 1','local');
		INSERT INTO episodes(content_id,series_id,season_id,season_number,episode_number,title,metadata_source)
		VALUES('local-episode-207-1-1','`+seriesID+`','local-season-207-1',1,1,'Local Ep','local');
		INSERT INTO media_files(content_id,episode_id,media_folder_id,file_path,file_size,container)
		VALUES('`+seriesID+`','local-episode-207-1-1',974,'/media/prec-s01e01.mkv',1024,'mkv');
		INSERT INTO verified_release_override_history
		(media_type,provider,provider_id,season_number,episode_number,revision,action,release_at,evidence_note,actor_account_id)
		VALUES('episode','tvdb','`+tvdbID+`',1,1,1,'set','2099-01-01T00:00:00Z','verified future',1)`); err != nil {
		t.Fatalf("seed local series with future override: %v", err)
	}
	_, err := newReleasedVirtualMediaRegistrar(pool).UpsertVirtualMedia(ctx, 11, VirtualMedia{
		LibraryID: "974", MediaType: "series", Title: "Precedence Series", TVDBID: tvdbID,
		Source: "provider-a", Episodes: []VirtualEpisode{{
			SeasonNumber: 1, EpisodeNumber: 1, Title: "Ep 1",
			AirDate:    time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
			VirtualURI: episodePath,
		}},
	})
	if err != nil {
		t.Fatalf("registration with blocking override failed: %v", err)
	}
	var virtualFiles, physicalFiles int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER(WHERE file_path='`+episodePath+`'),
		       count(*) FILTER(WHERE file_path='/media/prec-s01e01.mkv')
		FROM media_files WHERE content_id='`+seriesID+`'`).Scan(&virtualFiles, &physicalFiles); err != nil {
		t.Fatal(err)
	}
	if virtualFiles != 0 {
		t.Fatal("future override must block new virtual files even for locally proven episodes")
	}
	if physicalFiles != 1 {
		t.Fatal("blocking an override must never remove the existing physical file")
	}
}

func TestVirtualMovieRegistrationUsesStoredPastOverrideWithoutProvider(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	sfx := uniqueReleaseSuffix(t)
	tmdbID := "4243" + sfx
	imdbID := "tt424243" + sfx[:6]
	movieID := "movie-tmdb-" + tmdbID
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled) VALUES(973,'PastAliasPermit','movies',true);
		INSERT INTO media_items(content_id,type,title,tmdb_id,imdb_id,status)
		VALUES('`+movieID+`','movie','Past Alias','`+tmdbID+`','`+imdbID+`','matched');
		INSERT INTO verified_release_override_history
		(media_type,provider,provider_id,season_number,episode_number,revision,action,release_at,evidence_note,actor_account_id)
		VALUES('movie','imdb','`+imdbID+`',0,0,1,'set','2020-01-01T00:00:00Z','verified past',1)`); err != nil {
		t.Fatalf("seed stored alias with past override: %v", err)
	}
	reg := newReleasedVirtualMediaRegistrar(pool)
	reg.TMDBDigitalReleases = &fakeDigitalReleaseChecker{err: errors.New("tmdb down")}
	// TMDB-only registration omits the known IMDb alias while the provider
	// is unreachable. The stored past override must permit regardless.
	res, err := reg.UpsertVirtualMedia(ctx, 11, VirtualMedia{
		LibraryID: "973", MediaType: "movie", Title: "Past Alias", Year: 2020,
		TMDBID: tmdbID, Source: "provider-a",
		Variants: []VirtualMediaVariant{
			{VirtualURI: "virtual://movie/" + tmdbID + "?profile=1080p", Resolution: "1080p"},
		},
	})
	if err != nil {
		t.Fatalf("stored past override must permit without provider evidence: %v", err)
	}
	var files int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM media_files WHERE content_id=$1 AND container='virtual'", res.MediaID).Scan(&files); err != nil || files != 1 {
		t.Fatalf("expected 1 file via stored past override, got %d, err=%v", files, err)
	}
}

func TestCollectionEpisodeMaterializationIgnoresConcurrentlyAddedEpisodes(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled) VALUES(971,'Newcomer','series',true);
		INSERT INTO library_collections(id,library_id,slug,title,collection_type)
		VALUES('newcomer',971,'newcomer','Newcomer','manual');
		INSERT INTO library_collection_libraries(collection_id,library_id)
		VALUES('newcomer',971)`); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	repo := NewItemRepository(pool)
	tvdbID := "208" + uniqueReleaseSuffix(t)
	seriesID := "series-tvdb-" + tvdbID
	baseURI := "virtual://series/tvdb/" + tvdbID
	if _, err := repo.MaterializeVirtualPlaybackItemWithVariants(ctx, &models.MediaItem{
		ContentID: seriesID, Type: "series", Title: "Newcomer", SortTitle: "Newcomer",
		TvdbID: tvdbID, Status: "matched",
	}, []int{971}, []VirtualPlaybackVariant{{VirtualURI: baseURI, OwnerInstallationID: 11}}); err != nil {
		t.Fatalf("materialize series: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO seasons(content_id,series_id,season_number,title)
		VALUES('season-tvdb-208-1',$1,1,'Season 1')
		ON CONFLICT (content_id) DO NOTHING`, seriesID); err != nil {
		t.Fatalf("seed newcomer season: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO virtual_media_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,owns_item_metadata)
		VALUES(11,'collection',$1,971,false)`, seriesID); err != nil {
		t.Fatalf("seed newcomer source claim: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO virtual_media_file_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,file_path)
		VALUES(11,'collection',$1,971,'`+baseURI+`')`, seriesID); err != nil {
		t.Fatalf("seed newcomer file claim: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO episodes(content_id,series_id,season_id,season_number,episode_number,title,air_date)
		VALUES('episode-tvdb-208-1-1',$1,'season-tvdb-208-1',1,1,'First',CURRENT_DATE)
		ON CONFLICT (content_id) DO NOTHING`, seriesID); err != nil {
		t.Fatalf("seed first episode: %v", err)
	}
	// After locks are held, a second episode becomes eligible on a separate
	// connection. It must wait for its own run, not join this one unlocked.
	inserted := false
	materializeVirtualEpisodesEvalHook = func() {
		if inserted {
			return
		}
		inserted = true
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO episodes(content_id,series_id,season_id,season_number,episode_number,title,air_date)
			VALUES('episode-tvdb-208-1-2',$1,'season-tvdb-208-1',1,2,'Second',CURRENT_DATE)`, seriesID); err != nil {
			t.Errorf("insert concurrent episode: %v", err)
		}
	}
	defer func() { materializeVirtualEpisodesEvalHook = nil }()
	if err := repo.MaterializeVirtualPlaybackEpisodes(ctx, seriesID); err != nil {
		t.Fatalf("materialize with concurrent episode: %v", err)
	}
	var first, second int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER(WHERE file_path='`+baseURI+`/1/1'),
		       count(*) FILTER(WHERE file_path='`+baseURI+`/1/2')
		FROM media_files WHERE content_id=$1`, seriesID).Scan(&first, &second); err != nil {
		t.Fatal(err)
	}
	if first != 1 || second != 0 {
		t.Fatalf("locked run created first=%d second=%d, want 1/0", first, second)
	}
	if err := repo.MaterializeVirtualPlaybackEpisodes(ctx, seriesID); err != nil {
		t.Fatalf("follow-up materialization failed: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_files WHERE content_id=$1 AND file_path='`+baseURI+`/1/2'`, seriesID).Scan(&second); err != nil || second != 1 {
		t.Fatalf("follow-up run created second=%d, err=%v", second, err)
	}
}

func TestEpisodeMaterializationSerializesWithConcurrentOverrideWrite(t *testing.T) {
	runTag := uniqueReleaseSuffix(t)
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled) VALUES(970,'Serialize','series',true);
		INSERT INTO library_collections(id,library_id,slug,title,collection_type)
		VALUES('serialize',970,'serialize','Serialize','manual');
		INSERT INTO library_collection_libraries(collection_id,library_id)
		VALUES('serialize',970);
		INSERT INTO users(username,role,enabled) VALUES('override-serialize-`+runTag+`','admin',true)`); err != nil {
		t.Fatalf("seed collection and admin: %v", err)
	}
	var actor int
	if err := pool.QueryRow(ctx, `SELECT id FROM users WHERE username='override-serialize-' || $1`, runTag).Scan(&actor); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, actor) })
	repo := NewItemRepository(pool)
	tvdbID := "209" + runTag
	seriesID := "series-tvdb-" + tvdbID
	baseURI := "virtual://series/tvdb/" + tvdbID
	episodePath := baseURI + "/1/1"
	if _, err := repo.MaterializeVirtualPlaybackItemWithVariants(ctx, &models.MediaItem{
		ContentID: seriesID, Type: "series", Title: "Serialize", SortTitle: "Serialize",
		TvdbID: tvdbID, Status: "matched",
	}, []int{970}, []VirtualPlaybackVariant{{VirtualURI: baseURI, OwnerInstallationID: 11}}); err != nil {
		t.Fatalf("materialize series: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO seasons(content_id,series_id,season_number,title)
		VALUES('season-tvdb-209-1',$1,1,'Season 1')
		ON CONFLICT (content_id) DO NOTHING`, seriesID); err != nil {
		t.Fatalf("seed serialize season: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO episodes(content_id,series_id,season_id,season_number,episode_number,title,air_date)
		VALUES('episode-tvdb-209-1-1',$1,'season-tvdb-209-1',1,1,'First',CURRENT_DATE)
		ON CONFLICT (content_id) DO NOTHING`, seriesID); err != nil {
		t.Fatalf("seed serialize episode: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO virtual_media_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,owns_item_metadata)
		VALUES(11,'collection',$1,970,false)`, seriesID); err != nil {
		t.Fatalf("seed serialize source claim: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO virtual_media_file_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,file_path)
		VALUES(11,'collection',$1,970,'`+baseURI+`')`, seriesID); err != nil {
		t.Fatalf("seed serialize file claim: %v", err)
	}
	// Hold materialization inside its release locks while an administrator
	// commits a future override for the same episode. Exactly one order
	// wins; neither side may deadlock and the loser must observe the winner.
	reached := make(chan struct{})
	releaseHook := make(chan struct{})
	hooked := false
	materializeVirtualEpisodesEvalHook = func() {
		if hooked {
			return
		}
		hooked = true
		close(reached)
		<-releaseHook
	}
	defer func() { materializeVirtualEpisodesEvalHook = nil }()
	matErr := make(chan error, 1)
	go func() { matErr <- repo.MaterializeVirtualPlaybackEpisodes(context.Background(), seriesID) }()
	select {
	case <-reached:
	case <-time.After(60 * time.Second):
		t.Fatal("materialization never reached its release locks")
	}
	mutErr := make(chan error, 1)
	go func() {
		_, err := NewReleaseOverrideRepository(pool).Mutate(context.Background(), actor, ReleaseOverrideMutation{
			ReleaseIdentity: ReleaseIdentity{MediaType: "episode", Provider: "tvdb", ProviderID: tvdbID, SeasonNumber: 1, EpisodeNumber: 1},
			ReleaseAt:       "2099-01-01",
			EvidenceNote:    "verified future",
		}, false)
		mutErr <- err
	}()
	// Prove the writer actually queued behind the held release locks
	// before releasing materialization: otherwise the test could pass
	// with effectively sequential execution.
	overrideLockKey := ReleaseIdentity{MediaType: "episode", Provider: "tvdb", ProviderID: tvdbID, SeasonNumber: 1, EpisodeNumber: 1}.lockKey()
	waitForBlockedAdvisoryLock(t, pool, overrideLockKey, 30*time.Second)
	// Let materialization commit first: its decision predates the override.
	close(releaseHook)
	select {
	case err := <-matErr:
		if err != nil {
			t.Fatalf("materialization failed during concurrent override: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("materialization deadlocked with the override write")
	}
	select {
	case err := <-mutErr:
		if err != nil {
			t.Fatalf("override write failed after materialization: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("override write deadlocked with materialization")
	}
	var files int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_files WHERE content_id=$1 AND file_path=$2`, seriesID, episodePath).Scan(&files); err != nil || files != 1 {
		t.Fatalf("pre-override decision must stand: files=%d err=%v", files, err)
	}
	// A follow-up run observes the committed override and withdraws the file.
	if err := repo.MaterializeVirtualPlaybackEpisodes(ctx, seriesID); err != nil {
		t.Fatalf("follow-up materialization failed: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_files WHERE content_id=$1 AND file_path=$2`, seriesID, episodePath).Scan(&files); err != nil || files != 0 {
		t.Fatalf("committed future override must withdraw the file: files=%d err=%v", files, err)
	}
}

func TestReconciliationDiscoversUndatedLocalEpisode(t *testing.T) {
	tvdbID := "210" + uniqueReleaseSuffix(t)
	seriesID := "series-tvdb-" + tvdbID
	baseURI := "virtual://series/tvdb/" + tvdbID
	episodePath := baseURI + "/1/1"
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled) VALUES(969,'LocalUndated','series',true);
		INSERT INTO library_collections(id,library_id,slug,title,collection_type)
		VALUES('local-undated',969,'local-undated','Local Undated','manual');
		INSERT INTO library_collection_libraries(collection_id,library_id)
		VALUES('local-undated',969)`); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	repo := NewItemRepository(pool)
	if _, err := repo.MaterializeVirtualPlaybackItemWithVariants(ctx, &models.MediaItem{
		ContentID: seriesID, Type: "series", Title: "Local Undated", SortTitle: "Local Undated",
		TvdbID: tvdbID, Status: "matched",
	}, []int{969}, []VirtualPlaybackVariant{{VirtualURI: baseURI, OwnerInstallationID: 11}}); err != nil {
		t.Fatalf("materialize series: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO library_collection_items(collection_id,media_item_id,position)
		VALUES('local-undated',$1,0)`, seriesID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO seasons(content_id,series_id,season_number,title)
		VALUES('season-tvdb-210-1',$1,1,'Season 1')
		ON CONFLICT (content_id) DO NOTHING`, seriesID); err != nil {
		t.Fatalf("seed undated season: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO episodes(content_id,series_id,season_id,season_number,episode_number,title,metadata_source)
		VALUES('episode-tvdb-210-1-1',$1,'season-tvdb-210-1',1,1,'Undated Local','local')`, seriesID); err != nil {
		t.Fatalf("seed undated local episode: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO virtual_media_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,owns_item_metadata)
		VALUES(11,'collection',$1,969,false)`, seriesID); err != nil {
		t.Fatalf("seed undated source claim: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO virtual_media_file_source_claims(plugin_installation_id,source_key,content_id,media_folder_id,file_path)
		VALUES(11,'collection',$1,969,'`+baseURI+`')`, seriesID); err != nil {
		t.Fatalf("seed undated file claim: %v", err)
	}
	// Discovery, materialization, and cleanup must agree: the undated local
	// episode is found, materialized once, and never treated as stale.
	if n, err := repo.ReconcileReleasedCollectionVirtualEpisodes(ctx, 10); err != nil || n < 1 {
		t.Fatalf("reconciliation found n=%d err=%v", n, err)
	}
	var files int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_files WHERE content_id=$1 AND file_path=$2`, seriesID, episodePath).Scan(&files); err != nil || files != 1 {
		t.Fatalf("undated local episode files=%d err=%v", files, err)
	}
	if _, err := repo.ReconcileReleasedCollectionVirtualEpisodes(ctx, 10); err != nil {
		t.Fatalf("second reconciliation failed: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_files WHERE content_id=$1 AND file_path=$2`, seriesID, episodePath).Scan(&files); err != nil || files != 1 {
		t.Fatalf("second run left files=%d err=%v", files, err)
	}
}

func TestVirtualMovieRegistrationPermitsPossessedMovieWithoutProvider(t *testing.T) {
	sfx := uniqueReleaseSuffix(t)
	tmdbID := "4250" + sfx
	movieID := "movie-tmdb-" + tmdbID
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled) VALUES(968,'PossessedMovie','movies',true);
		INSERT INTO media_items(content_id,type,title,tmdb_id,status)
		VALUES('`+movieID+`','movie','Possessed','`+tmdbID+`','matched');
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container)
		VALUES('`+movieID+`',968,'/local/possessed.mkv',1024,'mkv')`); err != nil {
		t.Fatalf("seed possessed movie: %v", err)
	}
	reg := newReleasedVirtualMediaRegistrar(pool)
	reg.TMDBDigitalReleases = &fakeDigitalReleaseChecker{err: errors.New("tmdb down")}
	res, err := reg.UpsertVirtualMedia(ctx, 11, VirtualMedia{
		LibraryID: "968", MediaType: "movie", Title: "Possessed", Year: 2020,
		TMDBID: tmdbID, Source: "provider-a",
		Variants: []VirtualMediaVariant{
			{VirtualURI: "virtual://movie/" + tmdbID + "?profile=1080p", Resolution: "1080p"},
		},
	})
	if err != nil {
		t.Fatalf("possession must permit without provider evidence: %v", err)
	}
	var virtualFiles, physicalFiles int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER(WHERE container='virtual'),
		       count(*) FILTER(WHERE file_path='/local/possessed.mkv')
		FROM media_files WHERE content_id=$1`, res.MediaID).Scan(&virtualFiles, &physicalFiles); err != nil {
		t.Fatal(err)
	}
	if virtualFiles != 1 || physicalFiles != 1 {
		t.Fatalf("virtual=%d physical=%d, want coexistence 1/1", virtualFiles, physicalFiles)
	}
}

// seedVerifiedReconcileClaims inserts a folder, one media item per content ID,
// and a single-source claim for each, returning the content IDs.
func seedVerifiedReconcileClaims(t *testing.T, pool *pgxpool.Pool, folderID int, source string, contentIDs []string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES($1,'Verified','movies',true)`, folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	for _, contentID := range contentIDs {
		if _, err := pool.Exec(ctx, `
			INSERT INTO media_items(content_id,type,title,sort_title,status)
			VALUES($1,'movie',$1,$1,'matched')`, contentID); err != nil {
			t.Fatalf("seed item %s: %v", contentID, err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO virtual_media_source_claims(plugin_installation_id,source_key,content_id,media_folder_id)
			VALUES(11,$1,$2,$3)`, source, contentID, folderID); err != nil {
			t.Fatalf("seed claim %s: %v", contentID, err)
		}
	}
}

// TestReconcileVirtualMediaVerifiedRequiresFullCycleEvidence proves a partial
// enumeration (or no evidence at all) is refused and deletes nothing, while the
// refusal is distinguishable from an ordinary failure.
func TestReconcileVirtualMediaVerifiedRequiresFullCycleEvidence(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	const source = "verified-partial"
	ids := []string{"movie-tmdb-4101", "movie-tmdb-4102", "movie-tmdb-4103"}
	seedVerifiedReconcileClaims(t, pool, 4110, source, ids)

	reg := &VirtualMediaRegistrar{pool: pool}
	// A keep set that omits 4103 would delete live media if accepted. The pass
	// only enumerated a suffix, so it must be refused.
	_, err := reg.ReconcileVirtualMediaVerified(ctx, 11, source, ids[:2], []int{4110}, VirtualReconcileEvidence{
		FullCycle:   false,
		SourceCount: 2,
		QueueCount:  3,
	})
	if !errors.Is(err, ErrVirtualReconcileRefused) {
		t.Fatalf("partial enumeration error = %v, want ErrVirtualReconcileRefused", err)
	}

	// The zero evidence value is likewise not complete.
	if _, err := reg.ReconcileVirtualMediaVerified(ctx, 11, source, ids[:2], []int{4110}, VirtualReconcileEvidence{}); !errors.Is(err, ErrVirtualReconcileRefused) {
		t.Fatalf("zero evidence error = %v, want ErrVirtualReconcileRefused", err)
	}

	var claims int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM virtual_media_source_claims WHERE plugin_installation_id=11 AND source_key=$1`, source).Scan(&claims); err != nil {
		t.Fatal(err)
	}
	if claims != len(ids) {
		t.Fatalf("refused reconciliation changed claims: %d, want %d", claims, len(ids))
	}
	var items int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_items`).Scan(&items); err != nil {
		t.Fatal(err)
	}
	if items != len(ids) {
		t.Fatalf("refused reconciliation removed media: %d items, want %d", items, len(ids))
	}
}

// TestReconcileVirtualMediaVerifiedCompletePassDeletesStaleClaim proves a
// genuine full-cycle enumeration still reconciles and deletes the withdrawal.
func TestReconcileVirtualMediaVerifiedCompletePassDeletesStaleClaim(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	const source = "verified-complete"
	ids := []string{"movie-tmdb-4201", "movie-tmdb-4202", "movie-tmdb-4203"}
	seedVerifiedReconcileClaims(t, pool, 4120, source, ids)

	result, err := (&VirtualMediaRegistrar{pool: pool}).ReconcileVirtualMediaVerified(ctx, 11, source, ids[:2], []int{4120}, VirtualReconcileEvidence{
		FullCycle:   true,
		SourceCount: 3,
		QueueCount:  3,
	})
	if err != nil {
		t.Fatalf("complete pass refused: %v", err)
	}
	if result.ItemsRemoved != 1 {
		t.Fatalf("ItemsRemoved = %d, want 1", result.ItemsRemoved)
	}
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM virtual_media_source_claims WHERE plugin_installation_id=11 AND source_key=$1`, source).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 2 {
		t.Fatalf("remaining claims = %d, want 2", remaining)
	}
	var gone int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_items WHERE content_id='movie-tmdb-4203'`).Scan(&gone); err != nil {
		t.Fatal(err)
	}
	if gone != 0 {
		t.Fatalf("withdrawn item survived: %d", gone)
	}
}

// TestReconcileVirtualMediaVerifiedRefusesImplausibleShrink proves a complete
// attestation cannot authorize a keep set that drops most of the source at
// once, which is the signature of a truncated or replaced queue.
func TestReconcileVirtualMediaVerifiedRefusesImplausibleShrink(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	const source = "verified-truncated"
	ids := []string{
		"movie-tmdb-4301", "movie-tmdb-4302", "movie-tmdb-4303",
		"movie-tmdb-4304", "movie-tmdb-4305", "movie-tmdb-4306",
	}
	seedVerifiedReconcileClaims(t, pool, 4130, source, ids)

	_, err := (&VirtualMediaRegistrar{pool: pool}).ReconcileVirtualMediaVerified(ctx, 11, source, ids[:1], []int{4130}, VirtualReconcileEvidence{
		FullCycle:   true,
		SourceCount: 1,
		QueueCount:  6,
	})
	if !errors.Is(err, ErrVirtualReconcileRefused) {
		t.Fatalf("implausible shrink error = %v, want ErrVirtualReconcileRefused", err)
	}
	var claims int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM virtual_media_source_claims WHERE plugin_installation_id=11 AND source_key=$1`, source).Scan(&claims); err != nil {
		t.Fatal(err)
	}
	if claims != len(ids) {
		t.Fatalf("implausible-shrink refusal changed claims: %d, want %d", claims, len(ids))
	}
}

// TestMissingVirtualMediaContentIDsReportsGenuinelyRemoved confirms the monitor
// existence probe reports only content with no catalog row.
func TestMissingVirtualMediaContentIDsReportsGenuinelyRemoved(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id,type,title,sort_title,status)
		VALUES('movie-tmdb-4401','movie','A','A','matched'),
		      ('movie-tmdb-4402','movie','B','B','matched')`); err != nil {
		t.Fatal(err)
	}
	missing, err := (&VirtualMediaRegistrar{pool: pool}).MissingVirtualMediaContentIDs(ctx, []string{
		"movie-tmdb-4401", "movie-tmdb-4402", "movie-tmdb-4403",
	})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(missing) != 1 {
		t.Fatalf("missing = %v, want only movie-tmdb-4403", missing)
	}
	if _, ok := missing["movie-tmdb-4403"]; !ok {
		t.Fatalf("wrong missing set: %v", missing)
	}
}

// TestVirtualMediaVariantsDeduplicateSubtitleLanguageAliases proves the stored
// JSONB subtitle inventory carries one track per base language: a release that
// declared both "EN-US"/"ENG" and "FRE"/"FR-CA" persists two rows, not four.
func TestVirtualMediaVariantsDeduplicateSubtitleLanguageAliases(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	reg := newReleasedVirtualMediaRegistrar(pool)
	if _, err := pool.Exec(ctx, "INSERT INTO media_folders(id,name,type,enabled) VALUES(998,'SubtitleDedupe','mixed',true)"); err != nil {
		t.Fatalf("seed subtitle dedupe folder: %v", err)
	}
	in := VirtualMedia{
		LibraryID: "998", MediaType: "movie", Title: "Subtitle Dedupe", IMDbID: "tt300", TMDBID: "3", Source: "provider-a",
		Year: 2020, RuntimeMinutes: 100,
		Variants: []VirtualMediaVariant{
			{
				VirtualURI: "virtual://movie/tt300?profile=1080p", Resolution: "1080p", CodecVideo: "h264",
				SubtitleLanguages: []string{"EN-US", "ENG", "FRE", "FR-CA"},
			},
		},
	}
	res, err := reg.UpsertVirtualMedia(ctx, 11, in)
	if err != nil {
		t.Fatalf("upsert subtitle dedupe movie: %v", err)
	}
	var raw []byte
	if err := pool.QueryRow(ctx, `
		SELECT subtitle_tracks FROM media_files
		WHERE content_id=$1 AND file_path=$2`,
		res.MediaID, "virtual://movie/tt300?profile=1080p").Scan(&raw); err != nil {
		t.Fatalf("load stored subtitle tracks: %v", err)
	}
	var tracks []models.SubtitleTrack
	if err := json.Unmarshal(raw, &tracks); err != nil {
		t.Fatalf("decode stored subtitle tracks: %v", err)
	}
	if len(tracks) != 2 {
		t.Fatalf("stored subtitle tracks = %#v, want 2 (one per base language)", tracks)
	}
	got := map[string]bool{}
	for _, track := range tracks {
		got[track.Language] = true
	}
	if !got["EN-US"] || !got["FR-CA"] {
		t.Fatalf("stored subtitle tracks = %#v, want EN-US and FR-CA", tracks)
	}
	if got["ENG"] || got["FRE"] {
		t.Fatalf("stored subtitle tracks = %#v, kept a redundant alias", tracks)
	}
}
