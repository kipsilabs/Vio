package catalog

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestEpisodeAliasReconcileRepairsWatchStateDB pins the activation sequence
// that starts with an empty alias table: the migration's repair is a no-op, and
// reconciliation must move state for the mappings it inserts. The merge is
// deterministic across multiple aliases, existing canonical state, and
// profiles, and a second reconciliation changes nothing.
func TestEpisodeAliasReconcileRepairsWatchStateDB(t *testing.T) { //nolint:funlen,gocyclo // one fixture pins migration, merge policy, idempotence, and profile scope
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

	prefix := fmt.Sprintf("alias-reconcile-%d", time.Now().UnixNano())
	seriesID := prefix + "-series"
	canonicalID := prefix + "-canonical"
	aliasOneID := prefix + "-alias-1"
	aliasTwoID := prefix + "-alias-2"
	const (
		profileOne = "alias-profile-1"
		profileTwo = "alias-profile-2"
	)

	var userID, folderID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (email, username, role, enabled)
		VALUES ($1, $1, 'user', TRUE)
		RETURNING id
	`, prefix+"@example.invalid").Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO media_folders (type, name, enabled) VALUES ('series', $1, TRUE) RETURNING id`,
		prefix,
	).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	t.Cleanup(func() {
		cleanup := context.Background()
		_, _ = pool.Exec(cleanup, `DELETE FROM user_history_hidden_items WHERE user_id = $1`, userID)
		_, _ = pool.Exec(cleanup, `DELETE FROM user_watch_history WHERE user_id = $1`, userID)
		_, _ = pool.Exec(cleanup, `DELETE FROM user_watch_progress WHERE user_id = $1`, userID)
		_, _ = pool.Exec(cleanup, `DELETE FROM episode_aliases WHERE series_id = $1`, seriesID)
		_, _ = pool.Exec(cleanup, `DELETE FROM media_files WHERE media_folder_id = $1`, folderID)
		_, _ = pool.Exec(cleanup, `DELETE FROM episodes WHERE series_id = $1`, seriesID)
		_, _ = pool.Exec(cleanup, `DELETE FROM media_items WHERE content_id = $1`, seriesID)
		_, _ = pool.Exec(cleanup, `DELETE FROM media_folders WHERE id = $1`, folderID)
		_, _ = pool.Exec(cleanup, `DELETE FROM users WHERE id = $1`, userID)
	})

	airDate := time.Date(2024, 3, 7, 0, 0, 0, 0, time.UTC)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items (content_id, type, title)
		VALUES ($1, 'series', 'Alias Reconcile Fixture')`, seriesID); err != nil {
		t.Fatalf("seed series: %v", err)
	}
	// The canonical row has no file. Both layout aliases own one file at
	// different season coordinates and share the canonical title and air date,
	// which is what the reconciler matches on.
	if _, err := pool.Exec(ctx, `
		INSERT INTO episodes (content_id, series_id, season_number, episode_number, title, air_date)
		VALUES ($1, $4, 1, 1, 'Alias Reconcile Episode', $5),
		       ($2, $4, 2, 1, 'Alias Reconcile Episode', $5),
		       ($3, $4, 3, 1, 'Alias Reconcile Episode', $5)`,
		canonicalID, aliasOneID, aliasTwoID, seriesID, airDate); err != nil {
		t.Fatalf("seed episodes: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files (content_id, episode_id, media_folder_id, file_path, file_size)
		VALUES ($1, $2, $4, $5, 1), ($1, $3, $4, $6, 1)`,
		seriesID, aliasOneID, aliasTwoID, folderID,
		prefix+"-alias-1.mkv", prefix+"-alias-2.mkv"); err != nil {
		t.Fatalf("seed alias files: %v", err)
	}

	// Profile one already has canonical state plus one completed and one
	// newer incomplete alias. Profile two has only an alias row.
	t0 := base
	t1 := base.Add(time.Minute)
	t2 := base.Add(2 * time.Minute)
	t3 := base.Add(3 * time.Minute)
	events := []struct {
		profile   string
		item      string
		position  float64
		duration  float64
		completed bool
		updated   time.Time
		hidden    bool
	}{
		{profileOne, canonicalID, 50, 900, false, t0, false},
		{profileOne, aliasOneID, 300, 1000, false, t2, true},
		{profileOne, aliasTwoID, 0, 1200, true, t1, false},
		{profileTwo, aliasOneID, 60, 600, false, t3, false},
	}
	for _, event := range events {
		if _, err := pool.Exec(ctx, `
			INSERT INTO user_watch_progress (
				user_id, profile_id, media_item_id, position_seconds, duration_seconds,
				completed, updated_at, event_at, hide_from_continue
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $7, $8)`,
			userID, event.profile, event.item, event.position, event.duration,
			event.completed, event.updated, event.hidden,
		); err != nil {
			t.Fatalf("seed progress: %v", err)
		}
	}

	historyIDs := map[string]string{}
	for index, event := range []struct {
		id       string
		profile  string
		item     string
		watched  time.Time
		duration float64
	}{
		{prefix + "-history-canonical", profileOne, canonicalID, t0, 900},
		{prefix + "-history-alias-1", profileOne, aliasOneID, t1, 1000},
		{prefix + "-history-alias-2", profileOne, aliasTwoID, t2, 1200},
		{prefix + "-history-profile-2", profileTwo, aliasOneID, t3, 600},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO user_watch_history (
				id, user_id, profile_id, media_item_id, watched_at, duration_seconds, completed, source
			) VALUES ($1, $2, $3, $4, $5, $6, TRUE, 'legacy')`,
			event.id, userID, event.profile, event.item, event.watched, event.duration,
		); err != nil {
			t.Fatalf("seed history %d: %v", index, err)
		}
		historyIDs[event.id] = event.item
	}

	// A canonical and an alias watermark for the same episode merge to the later
	// one, so a removal under either identity keeps the episode hidden.
	if _, err := pool.Exec(ctx, `
		INSERT INTO user_history_hidden_items (user_id, profile_id, media_item_id, hidden_before, updated_at)
		VALUES ($1, $2, $3, $4, $4), ($1, $2, $5, $6, $6)`,
		userID, profileOne, canonicalID, t0, aliasTwoID, t3); err != nil {
		t.Fatalf("seed hidden history: %v", err)
	}

	repo := NewEpisodeAliasRepository(pool)
	result, err := repo.ReconcileSeries(ctx, seriesID, false)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Applied != 2 || len(result.Candidates) != 2 {
		t.Fatalf("reconcile applied %d candidates %d, want 2", result.Applied, len(result.Candidates))
	}

	assertProgress := func(t *testing.T, profile string, position, duration float64, completed, hidden bool) {
		t.Helper()
		var (
			gotPosition  float64
			gotDuration  float64
			gotCompleted bool
			gotHidden    bool
		)
		if err := pool.QueryRow(ctx, `
			SELECT position_seconds, duration_seconds, completed, hide_from_continue
			FROM user_watch_progress
			WHERE user_id = $1 AND profile_id = $2 AND media_item_id = $3`,
			userID, profile, canonicalID,
		).Scan(&gotPosition, &gotDuration, &gotCompleted, &gotHidden); err != nil {
			t.Fatalf("%s canonical progress: %v", profile, err)
		}
		if gotPosition != position || gotDuration != duration || gotCompleted != completed || gotHidden != hidden {
			t.Fatalf("%s canonical progress = position %v duration %v completed %v hidden %v, want position %v duration %v completed %v hidden %v",
				profile, gotPosition, gotDuration, gotCompleted, gotHidden,
				position, duration, completed, hidden)
		}
		var aliasRows int
		if err := pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM user_watch_progress
			WHERE user_id = $1 AND profile_id = $2 AND media_item_id = ANY($3::text[])`,
			userID, profile, []string{aliasOneID, aliasTwoID},
		).Scan(&aliasRows); err != nil {
			t.Fatalf("%s alias progress count: %v", profile, err)
		}
		if aliasRows != 0 {
			t.Fatalf("%s alias progress rows = %d, want 0", profile, aliasRows)
		}
	}

	assertProgress(t, profileOne, 300, 1200, true, true)
	assertProgress(t, profileTwo, 60, 600, false, false)

	for id, originalItem := range historyIDs {
		var item string
		if err := pool.QueryRow(ctx, `
			SELECT media_item_id FROM user_watch_history WHERE user_id = $1 AND id = $2`,
			userID, id,
		).Scan(&item); err != nil {
			t.Fatalf("history %s: %v", id, err)
		}
		want := originalItem
		if originalItem == aliasOneID || originalItem == aliasTwoID {
			want = canonicalID
		}
		if item != want {
			t.Fatalf("history %s media item = %q, want %q", id, item, want)
		}
	}

	var hiddenBefore time.Time
	if err := pool.QueryRow(ctx, `
		SELECT hidden_before FROM user_history_hidden_items
		WHERE user_id = $1 AND profile_id = $2 AND media_item_id = $3`,
		userID, profileOne, canonicalID,
	).Scan(&hiddenBefore); err != nil {
		t.Fatalf("canonical hidden marker: %v", err)
	}
	if !hiddenBefore.Equal(t3) {
		t.Fatalf("canonical hidden_before = %s, want %s", hiddenBefore, t3)
	}
	var aliasHidden int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM user_history_hidden_items
		WHERE user_id = $1 AND media_item_id = ANY($2::text[])`,
		userID, []string{aliasOneID, aliasTwoID},
	).Scan(&aliasHidden); err != nil {
		t.Fatalf("alias hidden count: %v", err)
	}
	if aliasHidden != 0 {
		t.Fatalf("alias hidden markers = %d, want 0", aliasHidden)
	}

	// Repeated reconciliation re-applies the same mappings and runs the repair
	// again. Alias rows are already gone, so canonical state must be unchanged.
	type snapshot struct {
		position  float64
		duration  float64
		completed bool
		hidden    bool
		updatedAt time.Time
	}
	snapshots := map[string]snapshot{}
	for _, profile := range []string{profileOne, profileTwo} {
		var snap snapshot
		if err := pool.QueryRow(ctx, `
			SELECT position_seconds, duration_seconds, completed, hide_from_continue, updated_at
			FROM user_watch_progress
			WHERE user_id = $1 AND profile_id = $2 AND media_item_id = $3`,
			userID, profile, canonicalID,
		).Scan(&snap.position, &snap.duration, &snap.completed, &snap.hidden, &snap.updatedAt); err != nil {
			t.Fatalf("snapshot %s: %v", profile, err)
		}
		snapshots[profile] = snap
	}
	historyCount := func() int {
		var count int
		if err := pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM user_watch_history WHERE user_id = $1`, userID,
		).Scan(&count); err != nil {
			t.Fatalf("history count: %v", err)
		}
		return count
	}
	beforeHistory := historyCount()

	if _, err := repo.ReconcileSeries(ctx, seriesID, false); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	for _, profile := range []string{profileOne, profileTwo} {
		var snap snapshot
		if err := pool.QueryRow(ctx, `
			SELECT position_seconds, duration_seconds, completed, hide_from_continue, updated_at
			FROM user_watch_progress
			WHERE user_id = $1 AND profile_id = $2 AND media_item_id = $3`,
			userID, profile, canonicalID,
		).Scan(&snap.position, &snap.duration, &snap.completed, &snap.hidden, &snap.updatedAt); err != nil {
			t.Fatalf("second snapshot %s: %v", profile, err)
		}
		if snap != snapshots[profile] {
			t.Fatalf("repeated reconcile changed %s progress: %+v -> %+v", profile, snapshots[profile], snap)
		}
	}
	if got := historyCount(); got != beforeHistory {
		t.Fatalf("repeated reconcile history count = %d, want %d", got, beforeHistory)
	}
	var mappingCount int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM episode_aliases WHERE series_id = $1`, seriesID,
	).Scan(&mappingCount); err != nil {
		t.Fatalf("mapping count: %v", err)
	}
	if mappingCount != 2 {
		t.Fatalf("mapping count = %d, want 2", mappingCount)
	}
}
