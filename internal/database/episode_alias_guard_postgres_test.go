package database

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/migrations"
)

// TestEpisodeAliasIdentityGuardSerializesConcurrentWritesPostgres proves that
// episode_aliases_guard_identity() serializes identity validation per series
// at the database boundary. Two connections insert A -> B and B -> A for the
// same series: the second must wait on the trigger's advisory lock, then fail
// the identity check after the first commits. Without the lock in the trigger,
// the unique indexes do not conflict and both rows commit.
func TestEpisodeAliasIdentityGuardSerializesConcurrentWritesPostgres(t *testing.T) { //nolint:funlen,gocyclo // one DB test covers lock wait, conflict, and a non-conflicting series
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := RunMigrations(ctx, pool, migrations.FS, "sql"); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	prefix := fmt.Sprintf("episode-alias-guard-%d", time.Now().UnixNano())
	seriesCycle := prefix + "-series-cycle"
	seriesOther := prefix + "-series-other"
	episodeA := prefix + "-episode-a"
	episodeB := prefix + "-episode-b"
	episodeOtherA := prefix + "-episode-other-a"
	episodeOtherB := prefix + "-episode-other-b"

	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatalf("seed %q: %v", query, err)
		}
	}
	exec(`INSERT INTO media_items (content_id, type, title) VALUES ($1, 'series', 'Guard cycle'), ($2, 'series', 'Guard other')`, seriesCycle, seriesOther)
	exec(`INSERT INTO episodes (content_id, series_id, season_number, episode_number, title)
		VALUES ($1, $3, 1, 1, 'A'), ($2, $3, 1, 2, 'B'), ($4, $5, 1, 1, 'Other A'), ($6, $5, 1, 2, 'Other B')`,
		episodeA, episodeB, seriesCycle, episodeOtherA, seriesOther, episodeOtherB)
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM episode_aliases WHERE series_id = ANY($1)`, []string{seriesCycle, seriesOther})
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM episodes WHERE series_id = ANY($1)`, []string{seriesCycle, seriesOther})
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM media_items WHERE content_id = ANY($1)`, []string{seriesCycle, seriesOther})
	})

	insertAlias := `INSERT INTO episode_aliases (
			series_id, canonical_episode_id, alias_episode_id,
			provider_installation_id, provider_series_id,
			provider_season_number, provider_episode_number, source
		) VALUES ($1, $2, $3, 0, '', $4, $5, 'db_guard_test')`

	connOne, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect first writer: %v", err)
	}
	t.Cleanup(func() { _ = connOne.Close(context.Background()) })
	connTwo, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect second writer: %v", err)
	}
	// The writer keeps using connTwo after it publishes its outcome: its
	// deferred rollback flushes on the same PgConn the cleanup would close.
	// Closing under that in-flight flush trips pgx's write timer ("BUG: slow
	// write timer already active"), so wait for the goroutine to return first.
	writerDone := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-writerDone:
		case <-time.After(5 * time.Second):
		}
		_ = connTwo.Close(context.Background())
	})

	txOne, err := connOne.Begin(ctx)
	if err != nil {
		t.Fatalf("begin first writer: %v", err)
	}
	defer func() { _ = txOne.Rollback(context.Background()) }()
	if _, err := txOne.Exec(ctx, insertAlias, seriesCycle, episodeA, episodeB, 1, 1); err != nil {
		t.Fatalf("insert A -> B: %v", err)
	}

	type insertOutcome struct {
		err       error
		committed bool
	}
	started := make(chan int32, 1)
	outcome := make(chan insertOutcome, 1)
	go func() {
		defer close(writerDone)
		txTwo, err := connTwo.Begin(ctx)
		if err != nil {
			outcome <- insertOutcome{err: fmt.Errorf("begin second writer: %w", err)}
			return
		}
		defer func() { _ = txTwo.Rollback(context.Background()) }()
		var pid int32
		if err := txTwo.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			outcome <- insertOutcome{err: fmt.Errorf("read second writer pid: %w", err)}
			return
		}
		started <- pid
		if _, err := txTwo.Exec(ctx, insertAlias, seriesCycle, episodeB, episodeA, 2, 2); err != nil {
			outcome <- insertOutcome{err: err}
			return
		}
		if err := txTwo.Commit(ctx); err != nil {
			outcome <- insertOutcome{err: fmt.Errorf("commit second writer: %w", err)}
			return
		}
		outcome <- insertOutcome{committed: true}
	}()

	var secondPID int32
	select {
	case secondPID = <-started:
	case result := <-outcome:
		t.Fatalf("second writer finished before starting the insert: %+v", result)
	case <-ctx.Done():
		t.Fatalf("wait for second writer to start: %v", ctx.Err())
	}

	// Wait for an observable lock wait rather than sleeping for a fixed
	// duration. The trigger takes the same advisory key the repository takes,
	// so the blocked insert owns an ungranted advisory lock while T1 runs.
	waitDeadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_locks
				WHERE pid = $1 AND locktype = 'advisory' AND NOT granted
			)`, secondPID).Scan(&waiting); err != nil {
			t.Fatalf("inspect second writer locks: %v", err)
		}
		if waiting {
			break
		}
		select {
		case result := <-outcome:
			t.Fatalf("second writer returned while the first transaction held the series lock: %+v", result)
		default:
		}
		if time.Now().After(waitDeadline) {
			t.Fatalf("second writer (pid %d) never waited on the series advisory lock", secondPID)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for second writer lock: %v", ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}

	// A different series hashes to a different advisory key and must proceed
	// while the first series lock is still held.
	if _, err := pool.Exec(ctx, insertAlias, seriesOther, episodeOtherA, episodeOtherB, 1, 1); err != nil {
		t.Fatalf("insert a row for a non-conflicting series: %v", err)
	}
	var otherRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM episode_aliases WHERE series_id = $1`, seriesOther).Scan(&otherRows); err != nil {
		t.Fatalf("count non-conflicting series rows: %v", err)
	}
	if otherRows != 1 {
		t.Fatalf("non-conflicting series rows = %d, want 1", otherRows)
	}

	if err := txOne.Commit(ctx); err != nil {
		t.Fatalf("commit first writer: %v", err)
	}

	var result insertOutcome
	select {
	case result = <-outcome:
	case <-ctx.Done():
		t.Fatalf("wait for second writer after first commit: %v", ctx.Err())
	}
	if result.committed {
		t.Fatal("second writer committed B -> A; both members of the cycle are present")
	}
	var pgErr *pgconn.PgError
	if !errors.As(result.err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("second insert error = %v, want SQLSTATE 23514", result.err)
	}
	if !strings.Contains(pgErr.Message, "already an alias row") {
		t.Fatalf("second insert message = %q, want the identity guard's alias error", pgErr.Message)
	}

	var canonicalID string
	var aliasID *string
	if err := pool.QueryRow(ctx, `
		SELECT canonical_episode_id, alias_episode_id
		FROM episode_aliases
		WHERE series_id = $1`, seriesCycle).Scan(&canonicalID, &aliasID); err != nil {
		t.Fatalf("read surviving cycle series row: %v", err)
	}
	if canonicalID != episodeA || aliasID == nil || *aliasID != episodeB {
		t.Fatalf("surviving row = %q -> %v, want %q -> %q", canonicalID, aliasID, episodeA, episodeB)
	}
	var cycleRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM episode_aliases WHERE series_id = $1`, seriesCycle).Scan(&cycleRows); err != nil {
		t.Fatalf("count cycle series rows: %v", err)
	}
	if cycleRows != 1 {
		t.Fatalf("cycle series rows = %d, want only the first committed row", cycleRows)
	}
}
