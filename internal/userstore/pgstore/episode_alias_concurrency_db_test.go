package pgstore

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/userstore"
)

// aliasConcurrencyFixture is the smallest catalog shape reconciliation can
// repair: one fileless canonical episode and one file-backed duplicate layout
// row that share a title and air date.
type aliasConcurrencyFixture struct {
	userID      int
	folderID    int
	seriesID    string
	canonicalID string
	aliasID     string
}

func seedAliasConcurrencyFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool, prefix string) aliasConcurrencyFixture {
	t.Helper()
	fixture := aliasConcurrencyFixture{
		seriesID:    prefix + "-series",
		canonicalID: prefix + "-canonical",
		aliasID:     prefix + "-alias",
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (email, username, role, enabled)
		VALUES ($1, $1, 'user', TRUE)
		RETURNING id
	`, prefix+"@example.invalid").Scan(&fixture.userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO media_folders (type, name, enabled) VALUES ('series', $1, TRUE) RETURNING id`,
		prefix,
	).Scan(&fixture.folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	t.Cleanup(func() {
		cleanup := context.Background()
		_, _ = pool.Exec(cleanup, `DELETE FROM user_history_hidden_items WHERE user_id = $1`, fixture.userID)
		_, _ = pool.Exec(cleanup, `DELETE FROM user_watch_history WHERE user_id = $1`, fixture.userID)
		_, _ = pool.Exec(cleanup, `DELETE FROM user_watch_progress WHERE user_id = $1`, fixture.userID)
		_, _ = pool.Exec(cleanup, `DELETE FROM episode_aliases WHERE series_id = $1`, fixture.seriesID)
		_, _ = pool.Exec(cleanup, `DELETE FROM media_files WHERE media_folder_id = $1`, fixture.folderID)
		_, _ = pool.Exec(cleanup, `DELETE FROM episodes WHERE series_id = $1`, fixture.seriesID)
		_, _ = pool.Exec(cleanup, `DELETE FROM media_items WHERE content_id = $1`, fixture.seriesID)
		_, _ = pool.Exec(cleanup, `DELETE FROM media_folders WHERE id = $1`, fixture.folderID)
		_, _ = pool.Exec(cleanup, `DELETE FROM users WHERE id = $1`, fixture.userID)
	})

	airDate := time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items (content_id, type, title)
		VALUES ($1, 'series', 'Alias Concurrency Fixture')`, fixture.seriesID); err != nil {
		t.Fatalf("seed series: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO episodes (content_id, series_id, season_number, episode_number, title, air_date)
		VALUES ($1, $3, 1, 1, 'Alias Concurrency Episode', $4),
		       ($2, $3, 2, 1, 'Alias Concurrency Episode', $4)`,
		fixture.canonicalID, fixture.aliasID, fixture.seriesID, airDate); err != nil {
		t.Fatalf("seed episodes: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files (content_id, episode_id, media_folder_id, file_path, file_size)
		VALUES ($1, $2, $3, $4, 1)`,
		fixture.seriesID, fixture.aliasID, fixture.folderID, prefix+"-alias.mkv"); err != nil {
		t.Fatalf("seed alias file: %v", err)
	}
	return fixture
}

// observedReconcilePool gives the reconciler a distinct application_name so the
// test can see its ungranted advisory-lock request in pg_locks.
func observedReconcilePool(t *testing.T, ctx context.Context, dsn, applicationName string) *pgxpool.Pool {
	t.Helper()
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse test database config: %v", err)
	}
	config.ConnConfig.RuntimeParams["application_name"] = applicationName
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect reconcile pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// waitForQueuedAdvisoryLock waits until the named backend has a queued advisory
// lock request. A queued request, not a sleep, proves the writer still holds
// the shared series lock and reconciliation has not started its snapshot.
func waitForQueuedAdvisoryLock(t *testing.T, ctx context.Context, observer *pgxpool.Pool, applicationName string) {
	t.Helper()
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting int
		if err := observer.QueryRow(ctx, `
			SELECT count(*)
			FROM pg_locks l
			JOIN pg_stat_activity a ON a.pid = l.pid
			WHERE l.locktype = 'advisory'
			  AND NOT l.granted
			  AND a.application_name = $1`, applicationName,
		).Scan(&waiting); err != nil {
			t.Fatalf("inspect advisory lock waiters: %v", err)
		}
		if waiting > 0 {
			return
		}
		select {
		case <-ticker.C:
		case <-timeout.C:
			t.Fatal("reconciliation never queued on the episode-series advisory lock")
		}
	}
}

// heldWatchStateWrite runs one watch-state write through the store's
// transaction wrapper and lets the test pause between canonical resolution and
// the actual row write.
type heldWatchStateWrite struct {
	resolved chan string
	release  chan struct{}
	done     chan error
}

func beginHeldWatchStateWrite(ctx context.Context, store *PostgresUserStore, mediaItemID string, write func(context.Context, pgx.Tx, string) error) *heldWatchStateWrite {
	held := &heldWatchStateWrite{
		resolved: make(chan string, 1),
		release:  make(chan struct{}),
		done:     make(chan error, 1),
	}
	go func() {
		held.done <- store.withWatchStateSeries(ctx, []string{mediaItemID}, func(tx pgx.Tx) error {
			targetID, err := canonicalWatchStateID(ctx, tx, mediaItemID)
			if err != nil {
				held.resolved <- ""
				return err
			}
			held.resolved <- targetID
			select {
			case <-held.release:
			case <-ctx.Done():
				return ctx.Err()
			}
			return write(ctx, tx, targetID)
		})
	}()
	return held
}

func (h *heldWatchStateWrite) waitResolved(t *testing.T) string {
	t.Helper()
	select {
	case targetID := <-h.resolved:
		return targetID
	case <-time.After(10 * time.Second):
		t.Fatal("watch-state write never resolved a canonical id")
		return ""
	}
}

// TestPostgresAliasWriteSerializedWithReconcileDB proves the resolve-and-write
// path cannot straddle a reconciliation. The writer resolves an unmapped alias,
// reconciliation queues on the exclusive series lock, and only then does the
// writer store state under the alias id it resolved. The repair that follows
// the writer's commit moves that state to the canonical episode, so no hidden
// alias-keyed progress row can survive.
func TestPostgresAliasWriteSerializedWithReconcileDB(t *testing.T) { //nolint:funlen,gocyclo // alias resurrection needs one fixture plus a queued lock check
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)

	prefix := fmt.Sprintf("alias-race-revive-%d", time.Now().UnixNano())
	fixture := seedAliasConcurrencyFixture(t, ctx, pool, prefix)
	store := newStore(pool, fixture.userID)
	const profileID = "alias-race-profile"
	reconcilePool := observedReconcilePool(t, ctx, dsn, prefix+"-reconcile")
	repo := catalog.NewEpisodeAliasRepository(reconcilePool)

	writer := beginHeldWatchStateWrite(ctx, store, fixture.aliasID, func(ctx context.Context, tx pgx.Tx, targetID string) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO user_watch_progress
				(user_id, profile_id, media_item_id, position_seconds, duration_seconds, completed, updated_at)
			VALUES ($1, $2, $3, 500, 1000, TRUE, $4)
			ON CONFLICT (user_id, profile_id, media_item_id) DO UPDATE SET
				position_seconds = EXCLUDED.position_seconds,
				duration_seconds = EXCLUDED.duration_seconds,
				completed = user_watch_progress.completed OR EXCLUDED.completed,
				updated_at = EXCLUDED.updated_at,
				event_at = EXCLUDED.updated_at`,
			fixture.userID, profileID, targetID, time.Now().UTC())
		return err
	})
	if resolved := writer.waitResolved(t); resolved != fixture.aliasID {
		t.Fatalf("unmapped alias resolved to %q, want the submitted id %q", resolved, fixture.aliasID)
	}

	reconciled := make(chan error, 1)
	go func() {
		_, err := repo.ReconcileSeries(ctx, fixture.seriesID, false)
		reconciled <- err
	}()

	waitForQueuedAdvisoryLock(t, ctx, pool, prefix+"-reconcile")
	close(writer.release)
	if err := <-writer.done; err != nil {
		t.Fatalf("alias write: %v", err)
	}
	if err := <-reconciled; err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var canonicalRows, aliasRows int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM user_watch_progress
		WHERE user_id = $1 AND media_item_id = $2`, fixture.userID, fixture.canonicalID,
	).Scan(&canonicalRows); err != nil {
		t.Fatalf("canonical progress count: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM user_watch_progress
		WHERE user_id = $1 AND media_item_id = $2`, fixture.userID, fixture.aliasID,
	).Scan(&aliasRows); err != nil {
		t.Fatalf("alias progress count: %v", err)
	}
	if canonicalRows != 1 || aliasRows != 0 {
		t.Fatalf("progress rows canonical=%d alias=%d, want 1 and 0", canonicalRows, aliasRows)
	}
	var position float64
	var completed bool
	if err := pool.QueryRow(ctx, `
		SELECT position_seconds, completed FROM user_watch_progress
		WHERE user_id = $1 AND profile_id = $2 AND media_item_id = $3`,
		fixture.userID, profileID, fixture.canonicalID,
	).Scan(&position, &completed); err != nil {
		t.Fatalf("canonical progress: %v", err)
	}
	if position != 500 || !completed {
		t.Fatalf("canonical progress = position %v completed %v, want 500 and true", position, completed)
	}
	var mappings int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM episode_aliases WHERE series_id = $1`, fixture.seriesID,
	).Scan(&mappings); err != nil {
		t.Fatalf("mapping count: %v", err)
	}
	if mappings != 1 {
		t.Fatalf("mapping count = %d, want 1", mappings)
	}
}

// TestPostgresCanonicalStateSurvivesReconcileSnapshotDB proves the repair cannot
// overwrite a canonical write it did not see. The writer holds the shared series
// lock and has written newer position, completion, and hints, but has not
// committed; reconciliation queues instead of taking its snapshot. Once the
// writer commits, the repair aggregates the newer row and leaves it in place.
func TestPostgresCanonicalStateSurvivesReconcileSnapshotDB(t *testing.T) { //nolint:funlen,gocyclo // repair snapshot loss needs one fixture plus a queued lock check
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)

	prefix := fmt.Sprintf("alias-race-newer-%d", time.Now().UnixNano())
	fixture := seedAliasConcurrencyFixture(t, ctx, pool, prefix)
	store := newStore(pool, fixture.userID)
	const profileID = "alias-newer-profile"

	// Map the alias first; the repair still has alias state to merge.
	if _, err := pool.Exec(ctx, `
		INSERT INTO episode_aliases (
			series_id, canonical_episode_id, alias_episode_id,
			provider_installation_id, provider_series_id,
			provider_season_number, provider_episode_number, source
		) VALUES ($1, $2, $3, 0, '', 2, 1, 'absolute_layout')`,
		fixture.seriesID, fixture.canonicalID, fixture.aliasID); err != nil {
		t.Fatalf("seed alias mapping: %v", err)
	}
	t0 := time.Now().UTC().Add(-2 * time.Minute)
	t1 := t0.Add(time.Minute)
	if _, err := pool.Exec(ctx, `
		INSERT INTO user_watch_progress
			(user_id, profile_id, media_item_id, position_seconds, duration_seconds, completed, updated_at, event_at)
		VALUES ($1, $2, $3, 100, 900, FALSE, $4, $4),
		       ($1, $2, $5, 50, 950, FALSE, $6, $6)`,
		fixture.userID, profileID, fixture.aliasID, t1, fixture.canonicalID, t0); err != nil {
		t.Fatalf("seed old progress: %v", err)
	}

	t2 := t1.Add(time.Minute)
	writer := beginHeldWatchStateWrite(ctx, store, fixture.canonicalID, func(ctx context.Context, tx pgx.Tx, targetID string) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO user_watch_progress
				(user_id, profile_id, media_item_id, position_seconds, duration_seconds, completed,
				 updated_at, event_at, last_file_id, last_resolution, last_hdr, last_codec_video, last_edition_key)
			VALUES ($1, $2, $3, 700, 1000, TRUE, $4, $4, 9, '2160p', TRUE, 'hevc', 'new-edition')
			ON CONFLICT (user_id, profile_id, media_item_id) DO UPDATE SET
				position_seconds = EXCLUDED.position_seconds,
				duration_seconds = EXCLUDED.duration_seconds,
				completed = user_watch_progress.completed OR EXCLUDED.completed,
				updated_at = EXCLUDED.updated_at,
				event_at = EXCLUDED.event_at,
				last_file_id = EXCLUDED.last_file_id,
				last_resolution = EXCLUDED.last_resolution,
				last_hdr = EXCLUDED.last_hdr,
				last_codec_video = EXCLUDED.last_codec_video,
				last_edition_key = EXCLUDED.last_edition_key`,
			fixture.userID, profileID, targetID, t2)
		return err
	})
	if resolved := writer.waitResolved(t); resolved != fixture.canonicalID {
		t.Fatalf("canonical id resolved to %q, want %q", resolved, fixture.canonicalID)
	}

	reconcilePool := observedReconcilePool(t, ctx, dsn, prefix+"-reconcile")
	repo := catalog.NewEpisodeAliasRepository(reconcilePool)
	reconciled := make(chan error, 1)
	go func() {
		_, err := repo.ReconcileSeries(ctx, fixture.seriesID, false)
		reconciled <- err
	}()

	waitForQueuedAdvisoryLock(t, ctx, pool, prefix+"-reconcile")
	close(writer.release)
	if err := <-writer.done; err != nil {
		t.Fatalf("newer canonical write: %v", err)
	}
	if err := <-reconciled; err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var (
		position   float64
		duration   float64
		completed  bool
		resolution *string
		hdr        *bool
		codecVideo *string
		editionKey *string
		lastFileID *int
	)
	if err := pool.QueryRow(ctx, `
		SELECT position_seconds, duration_seconds, completed, last_resolution, last_hdr,
		       last_codec_video, last_edition_key, last_file_id
		FROM user_watch_progress
		WHERE user_id = $1 AND profile_id = $2 AND media_item_id = $3`,
		fixture.userID, profileID, fixture.canonicalID,
	).Scan(&position, &duration, &completed, &resolution, &hdr, &codecVideo, &editionKey, &lastFileID); err != nil {
		t.Fatalf("canonical progress: %v", err)
	}
	if position != 700 || duration != 1000 || !completed {
		t.Fatalf("canonical progress = position %v duration %v completed %v, want 700, 1000, true",
			position, duration, completed)
	}
	if resolution == nil || *resolution != "2160p" || hdr == nil || !*hdr ||
		codecVideo == nil || *codecVideo != "hevc" || editionKey == nil || *editionKey != "new-edition" ||
		lastFileID == nil || *lastFileID != 9 {
		t.Fatalf("canonical hints = resolution %v hdr %v codec %v edition %v file %v",
			resolution, hdr, codecVideo, editionKey, lastFileID)
	}
	var aliasRows int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM user_watch_progress
		WHERE user_id = $1 AND media_item_id = $2`, fixture.userID, fixture.aliasID,
	).Scan(&aliasRows); err != nil {
		t.Fatalf("alias progress count: %v", err)
	}
	if aliasRows != 0 {
		t.Fatalf("alias progress rows = %d, want 0", aliasRows)
	}
}

// TestPostgresWatchStateWritesLockSeriesDB pins the store boundary itself: each
// write path that can receive a cached alias id takes the shared series lock in
// its own transaction, including profile-separated progress and history.
func TestPostgresWatchStateWritesLockSeriesDB(t *testing.T) { //nolint:funlen,gocyclo // one fixture covers every write path and profile separation
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)

	prefix := fmt.Sprintf("alias-write-lock-%d", time.Now().UnixNano())
	fixture := seedAliasConcurrencyFixture(t, ctx, pool, prefix)
	writerPool := observedReconcilePool(t, ctx, dsn, prefix+"-writer")
	store := newStore(writerPool, fixture.userID)
	if _, err := pool.Exec(ctx, `
		INSERT INTO episode_aliases (
			series_id, canonical_episode_id, alias_episode_id,
			provider_installation_id, provider_series_id,
			provider_season_number, provider_episode_number, source
		) VALUES ($1, $2, $3, 0, '', 2, 1, 'absolute_layout')`,
		fixture.seriesID, fixture.canonicalID, fixture.aliasID); err != nil {
		t.Fatalf("seed alias mapping: %v", err)
	}

	const lockProfile = "alias-lock-all"
	const profileA, profileB = "alias-lock-a", "alias-lock-b"
	now := time.Now().UTC()
	thresholds := userstore.ProgressThresholds{}
	operations := []struct {
		name string
		run  func() error
	}{
		{"UpdateProgress", func() error {
			return store.UpdateProgress(ctx, lockProfile, fixture.aliasID, 100, 1000, thresholds)
		}},
		{"SetProgress", func() error {
			return store.SetProgress(ctx, lockProfile, fixture.aliasID, 110, 1000, thresholds)
		}},
		{"SetProgressAt", func() error {
			return store.SetProgressAt(ctx, lockProfile, fixture.aliasID, 120, 1000, false, now)
		}},
		{"SetProgressIfNewer", func() error {
			_, err := store.SetProgressIfNewer(ctx, lockProfile, fixture.aliasID, 130, 1000, false, now.Add(time.Second))
			return err
		}},
		{"MarkWatched", func() error {
			return store.MarkWatched(ctx, lockProfile, fixture.aliasID, 1000)
		}},
		{"ClearProgress", func() error {
			return store.ClearProgress(ctx, lockProfile, fixture.aliasID)
		}},
		{"ClearResumePosition", func() error {
			_, err := store.ClearResumePosition(ctx, lockProfile, fixture.aliasID, now)
			return err
		}},
		{"UpdateProgressHints", func() error {
			return store.UpdateProgressHints(ctx, lockProfile, fixture.aliasID, userstore.VersionHints{FileID: 7, Resolution: "1080p"})
		}},
		{"AddHistory", func() error {
			return store.AddHistory(ctx, userstore.WatchHistoryEntry{
				ID: prefix + "-history-direct", ProfileID: lockProfile, MediaItemID: fixture.aliasID,
				WatchedAt: now.Format(time.RFC3339), DurationSeconds: 1000, Completed: true,
			})
		}},
		{"AddVisibleHistory", func() error {
			_, err := store.AddVisibleHistory(ctx, userstore.WatchHistoryEntry{
				ID: prefix + "-history-visible", ProfileID: lockProfile, MediaItemID: fixture.aliasID,
				WatchedAt: now.Format(time.RFC3339), DurationSeconds: 1000, Completed: true,
			})
			return err
		}},
		{"AddHistoryIfMissing", func() error {
			_, err := store.AddHistoryIfMissing(ctx, userstore.WatchHistoryEntry{
				ID: prefix + "-history-import", ProfileID: lockProfile, MediaItemID: fixture.aliasID,
				WatchedAt: now.Format(time.RFC3339Nano), DurationSeconds: 1000, Completed: true,
			})
			return err
		}},
		{"MarkProgressBatch", func() error {
			return store.MarkProgressBatch(ctx, lockProfile, []string{fixture.aliasID}, now)
		}},
		{"ClearProgressBatch", func() error {
			return store.ClearProgressBatch(ctx, lockProfile, []string{fixture.aliasID}, now)
		}},
		{"DeleteHistoryBySource", func() error {
			return store.DeleteHistoryBySource(ctx, lockProfile, []string{fixture.aliasID}, userstore.WatchHistorySourceLegacy)
		}},
		{"MarkWatchedBatch", func() error {
			_, err := store.MarkWatchedBatch(ctx, lockProfile,
				[]userstore.MarkWatchedTarget{{MediaItemID: fixture.aliasID, DurationSeconds: 1000}},
				[]userstore.WatchHistoryEntry{{ID: prefix + "-history-batch", ProfileID: lockProfile, MediaItemID: fixture.aliasID, Completed: true}},
			)
			return err
		}},
		{"RemoveHistoryItems", func() error {
			return store.RemoveHistoryItems(ctx, lockProfile, []string{fixture.aliasID}, now)
		}},
		{"ApplyJellycompatProgress", func() error {
			return store.ApplyJellycompatProgress(ctx, lockProfile, userstore.JellycompatProgressEdit{
				MediaItemID: fixture.aliasID, PositionSeconds: 140, DurationSeconds: 1000, EventAt: now,
			})
		}},
	}

	for _, operation := range operations {
		operation := operation
		t.Run(operation.name, func(t *testing.T) {
			// Hold the exclusive form of the same key from another connection.
			// The write must queue on the shared request instead of resolving and
			// writing while reconciliation could run.
			blocker, err := pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin blocker: %v", err)
			}
			defer func() { _ = blocker.Rollback(context.Background()) }()
			if _, err := blocker.Exec(ctx,
				`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, fixture.seriesID); err != nil {
				t.Fatalf("take exclusive series lock: %v", err)
			}

			done := make(chan error, 1)
			go func() { done <- operation.run() }()
			waitForQueuedAdvisoryLock(t, ctx, pool, prefix+"-writer")
			if err := blocker.Rollback(ctx); err != nil {
				t.Fatalf("release blocker: %v", err)
			}
			if err := <-done; err != nil {
				t.Fatalf("%s: %v", operation.name, err)
			}
		})
	}

	// Profile separation still holds on the canonical episode after every write
	// path above ran against the alias id.
	if _, err := store.SetProgressIfNewer(ctx, profileA, fixture.aliasID, 240, 1000, false, now.Add(2*time.Second)); err != nil {
		t.Fatalf("profile A SetProgressIfNewer: %v", err)
	}
	if _, err := store.SetProgressIfNewer(ctx, profileB, fixture.aliasID, 120, 800, false, now.Add(2*time.Second)); err != nil {
		t.Fatalf("profile B SetProgressIfNewer: %v", err)
	}
	var profileAPosition, profileBPosition float64
	if err := pool.QueryRow(ctx, `
		SELECT position_seconds FROM user_watch_progress
		WHERE user_id = $1 AND profile_id = $2 AND media_item_id = $3`,
		fixture.userID, profileA, fixture.canonicalID,
	).Scan(&profileAPosition); err != nil {
		t.Fatalf("profile A progress: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT position_seconds FROM user_watch_progress
		WHERE user_id = $1 AND profile_id = $2 AND media_item_id = $3`,
		fixture.userID, profileB, fixture.canonicalID,
	).Scan(&profileBPosition); err != nil {
		t.Fatalf("profile B progress: %v", err)
	}
	if profileAPosition != 240 || profileBPosition != 120 {
		t.Fatalf("profile positions = %v and %v, want 240 and 120", profileAPosition, profileBPosition)
	}
	var aliasProgress, aliasHistory int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM user_watch_progress
		WHERE user_id = $1 AND media_item_id = $2`, fixture.userID, fixture.aliasID,
	).Scan(&aliasProgress); err != nil {
		t.Fatalf("alias progress count: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM user_watch_history
		WHERE user_id = $1 AND media_item_id = $2`, fixture.userID, fixture.aliasID,
	).Scan(&aliasHistory); err != nil {
		t.Fatalf("alias history count: %v", err)
	}
	if aliasProgress != 0 || aliasHistory != 0 {
		t.Fatalf("alias rows progress=%d history=%d, want 0 and 0", aliasProgress, aliasHistory)
	}
}
