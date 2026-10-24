package database

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/migrations"
)

// TestEpisodeAliasChainValidationPrecedesWatchStateRepairPostgres proves the
// canonicalize migration is fail-closed on a malformed alias graph. The repair
// merges an alias episode's watch state into its canonical row and deletes the
// alias row; on a pre-existing A -> B -> C chain that moves A's state onto B
// and then deletes B's own state. The chain validation must therefore run
// before the repair, in the same transaction, so a failed migration leaves
// every watch-state and alias row untouched.
//
// The test owns its database: rewinding a shared test database would
// invalidate other packages' fixtures. The test role needs CREATEDB. It stops
// at 20261023125706, the schema immediately before the repair migration, and
// seeds the chain through the real race the later guard migration closes:
// two transactions insert A -> B and B -> C before either commits, so the
// per-row guard cannot see the other row.
func TestEpisodeAliasChainValidationPrecedesWatchStateRepairPostgres(t *testing.T) { //nolint:funlen,gocyclo // one fixture pins migration order, unchanged state, and recovery
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}

	ctx := t.Context()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect maintenance database: %v", err)
	}
	defer admin.Close()

	name := fmt.Sprintf("silo_alias_chain_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create disposable database: %v", err)
	}
	defer func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("drop disposable database: %v", err)
		}
	}()

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse test database config: %v", err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect disposable database: %v", err)
	}
	defer pool.Close()

	provider, err := newMigrationProvider(pool, migrations.FS, "sql")
	if err != nil {
		t.Fatalf("create migration provider: %v", err)
	}
	defer func() { _ = provider.Close() }()

	const (
		preRepairVersion = int64(20261023125706)
		repairVersion    = int64(20261023130437)
	)
	if _, err := provider.UpTo(ctx, preRepairVersion); err != nil {
		t.Fatalf("migrate to %d: %v", preRepairVersion, err)
	}

	prefix := fmt.Sprintf("alias-chain-%d", time.Now().UnixNano())
	seriesID := prefix + "-series"
	episodeA := prefix + "-episode-a"
	episodeB := prefix + "-episode-b"
	episodeC := prefix + "-episode-c"
	const (
		profileOne = "alias-chain-profile-1"
		profileTwo = "alias-chain-profile-2"
	)
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatalf("seed %q: %v", query, err)
		}
	}

	var userID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (email, username, role, enabled)
		VALUES ($1, $1, 'user', TRUE)
		RETURNING id
	`, prefix+"@example.invalid").Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	exec(`INSERT INTO media_items (content_id, type, title) VALUES ($1, 'series', 'Alias chain fixture')`, seriesID)
	exec(`INSERT INTO episodes (content_id, series_id, season_number, episode_number, title)
		VALUES ($1, $4, 1, 1, 'A'), ($2, $4, 1, 2, 'B'), ($3, $4, 1, 3, 'C')`,
		episodeA, episodeB, episodeC, seriesID)

	episodes := []string{episodeA, episodeB, episodeC}
	for profileIndex, profileID := range []string{profileOne, profileTwo} {
		for episodeIndex, episodeID := range episodes {
			exec(`INSERT INTO user_watch_progress (
					user_id, profile_id, media_item_id, position_seconds, duration_seconds,
					completed, updated_at, event_at, hide_from_continue, last_file_id,
					last_resolution, last_hdr, last_codec_video, last_edition_key
				) VALUES ($1, $2, $3, $4, 1800, $5, $6, $6, $7, $8, $9, $10, 'h264', $11)`,
				userID, profileID, episodeID,
				float64(100*(episodeIndex+1)+10*profileIndex),
				episodeIndex == 2,
				base.Add(time.Duration(profileIndex*10+episodeIndex)*time.Minute),
				episodeIndex == 1 && profileIndex == 1,
				profileIndex*10+episodeIndex+1,
				[]string{"1080p", "720p"}[profileIndex],
				profileIndex == 0,
				fmt.Sprintf("edition-%d-%d", profileIndex, episodeIndex),
			)
		}
	}
	exec(`INSERT INTO user_watch_history (id, user_id, profile_id, media_item_id, watched_at, duration_seconds, completed)
		VALUES ($1, $2, $3, $4, $5, 1800, TRUE), ($6, $2, $7, $8, $5, 1800, TRUE)`,
		prefix+"-history-b", userID, profileOne, episodeB, base.Add(30*time.Minute),
		prefix+"-history-c", profileTwo, episodeC)
	exec(`INSERT INTO user_history_hidden_items (user_id, profile_id, media_item_id, hidden_before, updated_at)
		VALUES ($1, $2, $3, $4, $4)`,
		userID, profileOne, episodeB, base.Add(20*time.Minute))

	// Seed the pre-existing chain through the identity guard's race: hold
	// A -> B uncommitted while B -> C is inserted on another connection, then
	// commit both. The guard added in 20261023125706 checks committed rows
	// only, so it accepts both; 20261023175027 adds the advisory lock that
	// would have serialized them.
	connOne, err := pgx.ConnectConfig(ctx, cfg.ConnConfig.Copy())
	if err != nil {
		t.Fatalf("connect first chain writer: %v", err)
	}
	defer func() { _ = connOne.Close(context.Background()) }()
	connTwo, err := pgx.ConnectConfig(ctx, cfg.ConnConfig.Copy())
	if err != nil {
		t.Fatalf("connect second chain writer: %v", err)
	}
	defer func() { _ = connTwo.Close(context.Background()) }()

	insertAlias := `INSERT INTO episode_aliases (
			series_id, canonical_episode_id, alias_episode_id,
			provider_installation_id, provider_series_id,
			provider_season_number, provider_episode_number, source
		) VALUES ($1, $2, $3, 0, '', $4, $5, 'chain_repair_test')`
	txOne, err := connOne.Begin(ctx)
	if err != nil {
		t.Fatalf("begin first chain writer: %v", err)
	}
	defer func() { _ = txOne.Rollback(context.Background()) }()
	if _, err := txOne.Exec(ctx, insertAlias, seriesID, episodeA, episodeB, 1, 1); err != nil {
		t.Fatalf("insert A -> B: %v", err)
	}
	txTwo, err := connTwo.Begin(ctx)
	if err != nil {
		t.Fatalf("begin second chain writer: %v", err)
	}
	defer func() { _ = txTwo.Rollback(context.Background()) }()
	if _, err := txTwo.Exec(ctx, insertAlias, seriesID, episodeB, episodeC, 2, 2); err != nil {
		t.Fatalf("insert B -> C: %v", err)
	}
	if err := txOne.Commit(ctx); err != nil {
		t.Fatalf("commit A -> B: %v", err)
	}
	if err := txTwo.Commit(ctx); err != nil {
		t.Fatalf("commit B -> C: %v", err)
	}

	progressSnapshot := func() string {
		t.Helper()
		var snapshot string
		if err := pool.QueryRow(ctx, `
			SELECT COALESCE(jsonb_agg(to_jsonb(progress) ORDER BY progress.profile_id, progress.media_item_id), '[]'::jsonb)::text
			FROM (
				SELECT user_id, profile_id, media_item_id, position_seconds, duration_seconds,
				       completed, updated_at, event_at, hide_from_continue, last_file_id,
				       last_resolution, last_hdr, last_codec_video, last_edition_key
				FROM user_watch_progress
				WHERE user_id = $1
			) progress`, userID).Scan(&snapshot); err != nil {
			t.Fatalf("snapshot watch progress: %v", err)
		}
		return snapshot
	}
	historySnapshot := func() string {
		t.Helper()
		var snapshot string
		if err := pool.QueryRow(ctx, `
			SELECT COALESCE(jsonb_agg(to_jsonb(history) ORDER BY history.id), '[]'::jsonb)::text
			FROM (
				SELECT id, user_id, profile_id, media_item_id, watched_at, duration_seconds, completed
				FROM user_watch_history
				WHERE user_id = $1
			) history`, userID).Scan(&snapshot); err != nil {
			t.Fatalf("snapshot watch history: %v", err)
		}
		return snapshot
	}
	hiddenSnapshot := func() string {
		t.Helper()
		var snapshot string
		if err := pool.QueryRow(ctx, `
			SELECT COALESCE(jsonb_agg(to_jsonb(hidden) ORDER BY hidden.profile_id, hidden.media_item_id), '[]'::jsonb)::text
			FROM (
				SELECT user_id, profile_id, media_item_id, hidden_before, updated_at
				FROM user_history_hidden_items
				WHERE user_id = $1
			) hidden`, userID).Scan(&snapshot); err != nil {
			t.Fatalf("snapshot hidden items: %v", err)
		}
		return snapshot
	}
	aliasSnapshot := func() string {
		t.Helper()
		var snapshot string
		if err := pool.QueryRow(ctx, `
			SELECT COALESCE(jsonb_agg(to_jsonb(alias) ORDER BY alias.id), '[]'::jsonb)::text
			FROM (
				SELECT id, series_id, canonical_episode_id, alias_episode_id,
				       provider_installation_id, provider_series_id,
				       provider_season_number, provider_episode_number,
				       source, confidence, created_at, updated_at
				FROM episode_aliases
				WHERE series_id = $1
			) alias`, seriesID).Scan(&snapshot); err != nil {
			t.Fatalf("snapshot alias rows: %v", err)
		}
		return snapshot
	}

	var progressRows, aliasRows, chainRows int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_watch_progress WHERE user_id = $1`, userID).Scan(&progressRows); err != nil {
		t.Fatalf("count seeded progress rows: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM episode_aliases WHERE series_id = $1`, seriesID).Scan(&aliasRows); err != nil {
		t.Fatalf("count seeded alias rows: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM episode_aliases
		WHERE series_id = $1
		  AND ((canonical_episode_id = $2 AND alias_episode_id = $3)
		    OR (canonical_episode_id = $3 AND alias_episode_id = $4))`,
		seriesID, episodeA, episodeB, episodeC,
	).Scan(&chainRows); err != nil {
		t.Fatalf("count seeded chain rows: %v", err)
	}
	if progressRows != 6 || aliasRows != 2 || chainRows != 2 {
		t.Fatalf("seeded progress=%d alias=%d chain=%d, want 6, 2, and 2", progressRows, aliasRows, chainRows)
	}
	beforeProgress := progressSnapshot()
	beforeHistory := historySnapshot()
	beforeHidden := hiddenSnapshot()
	beforeAliases := aliasSnapshot()

	_, err = provider.UpTo(ctx, repairVersion)
	if err == nil {
		t.Fatal("canonicalize migration applied over a chain; want a graph-validation failure before the watch-state repair")
	}
	if !strings.Contains(err.Error(), "chain or cycle") || !strings.Contains(err.Error(), "not installable") {
		t.Fatalf("canonicalize migration error = %v; want the alias graph-validation error", err)
	}

	if got := progressSnapshot(); got != beforeProgress {
		t.Fatalf("watch progress changed after the rejected migration:\nbefore %s\nafter  %s", beforeProgress, got)
	}
	if got := historySnapshot(); got != beforeHistory {
		t.Fatalf("watch history changed after the rejected migration:\nbefore %s\nafter  %s", beforeHistory, got)
	}
	if got := hiddenSnapshot(); got != beforeHidden {
		t.Fatalf("hidden watch-state rows changed after the rejected migration:\nbefore %s\nafter  %s", beforeHidden, got)
	}
	if got := aliasSnapshot(); got != beforeAliases {
		t.Fatalf("alias rows changed after the rejected migration:\nbefore %s\nafter  %s", beforeAliases, got)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_watch_progress WHERE user_id = $1`, userID).Scan(&progressRows); err != nil {
		t.Fatalf("count progress rows after rejection: %v", err)
	}
	if progressRows != 6 {
		t.Fatalf("progress rows after rejection = %d, want all 6 original rows", progressRows)
	}

	// The failed migration transaction must also leave no partial schema
	// behind, and must not be recorded as applied.
	var repairFunctionsExist, repairVersionApplied bool
	if err := pool.QueryRow(ctx, `SELECT to_regprocedure('public.canonicalize_episode_watch_state(text)') IS NOT NULL`).Scan(&repairFunctionsExist); err != nil {
		t.Fatalf("inspect repair function: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM public.goose_db_version
			WHERE version_id = $1 AND is_applied
		)`, repairVersion).Scan(&repairVersionApplied); err != nil {
		t.Fatalf("inspect migration version: %v", err)
	}
	if repairFunctionsExist || repairVersionApplied {
		t.Fatalf("rejected migration left state behind: function=%t version_applied=%t", repairFunctionsExist, repairVersionApplied)
	}

	// Removing the invalid row leaves a valid one-hop mapping; the same
	// migration must now apply and run the repair.
	exec(`DELETE FROM episode_aliases WHERE series_id = $1 AND canonical_episode_id = $2`, seriesID, episodeB)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("canonicalize migration after removing the invalid row: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT to_regprocedure('public.canonicalize_episode_watch_state(text)') IS NOT NULL`).Scan(&repairFunctionsExist); err != nil {
		t.Fatalf("inspect repair function after success: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM public.goose_db_version
			WHERE version_id = $1 AND is_applied
		)`, repairVersion).Scan(&repairVersionApplied); err != nil {
		t.Fatalf("inspect migration version after success: %v", err)
	}
	if !repairFunctionsExist || !repairVersionApplied {
		t.Fatalf("migration after recovery: function=%t version_applied=%t, want both", repairFunctionsExist, repairVersionApplied)
	}
	var aliasProgressRows int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_watch_progress WHERE user_id = $1 AND media_item_id = $2`, userID, episodeB).Scan(&aliasProgressRows); err != nil {
		t.Fatalf("count repaired alias progress: %v", err)
	}
	if aliasProgressRows != 0 {
		t.Fatalf("alias progress rows after repair = %d, want the one-hop merge to delete them", aliasProgressRows)
	}
}
