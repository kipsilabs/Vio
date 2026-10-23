package pgstore

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestEpisodeAliasWatchCountsCanonicalSeasonsDB pins the canonical episode set
// used by the series and season watch-count rollups: duplicate absolute-layout
// rows do not inflate a season, and a canonical episode with no direct file
// still counts when its alias row owns a present file.
func TestEpisodeAliasWatchCountsCanonicalSeasonsDB(t *testing.T) {
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

	suffix := time.Now().UnixNano()
	prefix := fmt.Sprintf("alias-watch-counts-%d", suffix)
	seriesID := prefix + "-series"
	seasonOneID := prefix + "-season-1"
	seasonTwoID := prefix + "-season-2"
	canonicalAliasFileID := prefix + "-canonical-alias-file"
	canonicalDirectID := prefix + "-canonical-direct"
	aliasID := prefix + "-alias"
	const profileID = "alias-profile"

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
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_files WHERE media_folder_id = $1`, folderID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM episode_aliases WHERE series_id = $1`, seriesID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM episodes WHERE series_id = $1`, seriesID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM seasons WHERE series_id = $1`, seriesID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_items WHERE content_id = $1`, seriesID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_folders WHERE id = $1`, folderID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	})

	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO media_items (content_id, type, title) VALUES ($1, 'series', 'Alias Watch Counts')`, []any{seriesID}},
		{`INSERT INTO seasons (content_id, series_id, season_number) VALUES ($1, $3, 1), ($2, $3, 2)`, []any{seasonOneID, seasonTwoID, seriesID}},
		// The alias row lives in the absolute-layout season and is backed by an
		// episode_libraries row, so it would inflate season 2 without exclusion.
		{`INSERT INTO episodes (content_id, series_id, season_id, season_number, episode_number, title)
			VALUES ($1, $4, $5, 1, 1, 'Canonical via alias file'),
			       ($2, $4, $5, 1, 2, 'Canonical direct'),
			       ($3, $4, $6, 2, 1, 'Alias duplicate')`,
			[]any{canonicalAliasFileID, canonicalDirectID, aliasID, seriesID, seasonOneID, seasonTwoID}},
		{`INSERT INTO episode_libraries (episode_id, media_folder_id) VALUES ($1, $3), ($2, $3)`, []any{aliasID, canonicalDirectID, folderID}},
		{`INSERT INTO episode_aliases (
				series_id, canonical_episode_id, alias_episode_id,
				provider_installation_id, provider_series_id,
				provider_season_number, provider_episode_number, source
			) VALUES ($1, $2, $3, 0, '', 2, 1, 'absolute_layout')`,
			[]any{seriesID, canonicalAliasFileID, aliasID}},
		// The canonical has no direct media_files row; only its alias owns one.
		{`INSERT INTO media_files (content_id, episode_id, media_folder_id, file_path, file_size)
			VALUES ($1, $2, $3, $4, 1), ($1, $5, $3, $6, 1)`,
			[]any{seriesID, aliasID, folderID, prefix + "-alias.mkv", canonicalDirectID, prefix + "-direct.mkv"}},
		{`INSERT INTO user_watch_progress (user_id, profile_id, media_item_id, position_seconds, duration_seconds, completed)
			VALUES ($1, $2, $3, 0, 1200, TRUE)`,
			[]any{userID, profileID, canonicalDirectID}},
	} {
		if _, err := pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("seed %q: %v", statement.sql, err)
		}
	}

	store := newStore(pool, userID)
	seasons, err := store.SeriesSeasonWatchCounts(ctx, profileID, seriesID)
	if err != nil {
		t.Fatalf("SeriesSeasonWatchCounts: %v", err)
	}
	if got := seasons[1]; got.TotalEpisodes != 2 || got.WatchedCount != 1 {
		t.Fatalf("season 1 counts = %+v, want total=2 watched=1", got)
	}
	if _, ok := seasons[2]; ok {
		t.Fatalf("season 2 contains alias rows: %+v", seasons[2])
	}

	seriesCounts, err := store.SeriesEpisodeWatchCounts(ctx, profileID, []string{seriesID})
	if err != nil {
		t.Fatalf("SeriesEpisodeWatchCounts: %v", err)
	}
	if got := seriesCounts[seriesID]; got.TotalEpisodes != 2 || got.WatchedCount != 1 {
		t.Fatalf("series counts = %+v, want total=2 watched=1", got)
	}

	seasonCounts, err := store.SeasonEpisodeWatchCounts(ctx, profileID, []string{seasonOneID, seasonTwoID})
	if err != nil {
		t.Fatalf("SeasonEpisodeWatchCounts: %v", err)
	}
	if got := seasonCounts[seasonOneID]; got.TotalEpisodes != 2 || got.WatchedCount != 1 {
		t.Fatalf("season 1 row counts = %+v, want total=2 watched=1", got)
	}
	if _, ok := seasonCounts[seasonTwoID]; ok {
		t.Fatalf("season 2 row contains alias rows: %+v", seasonCounts[seasonTwoID])
	}
}
