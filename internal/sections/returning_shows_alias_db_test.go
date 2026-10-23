package sections

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/catalog"
)

// TestReturningShowsAliasDB pins the alias rules in the Returning Shows season
// check: a layout alias row is not itself a new season, and a canonical
// episode's present file still counts when its alias row owns it. Ordering
// ignores alias rows too, so a recent alias does not move its series up.
func TestReturningShowsAliasDB(t *testing.T) {
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)

	suffix := time.Now().UnixNano()
	id := func(name string) string { return fmt.Sprintf("returning-alias-%s-%d", name, suffix) }
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}

	var folderID, userID int
	if err := pool.QueryRow(ctx, `INSERT INTO media_folders (type, name, enabled) VALUES ('series', $1, true) RETURNING id`, id("folder")).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO users (username, email, password_hash, role) VALUES ($1, $2, '', 'user') RETURNING id`, id("user"), id("user")+"@example.test").Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	profileID := id("profile")

	// Each series has a watched S1E1 and a canonical S2E1. The alias series
	// also have an alias row under a different layout (S3E1), because episode
	// coordinates are unique per series.
	aliasFile, aliasOnlySeason, aliasOrder := id("alias-file"), id("alias-only-season"), id("alias-order")
	seriesIDs := []string{aliasFile, aliasOnlySeason, aliasOrder}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM episode_aliases WHERE series_id = ANY($1)`, seriesIDs)
		_, _ = pool.Exec(ctx, `DELETE FROM user_watch_history WHERE user_id = $1`, userID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_files WHERE media_folder_id = $1`, folderID)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
		_, _ = pool.Exec(ctx, `DELETE FROM episodes WHERE series_id = ANY($1)`, seriesIDs)
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id = ANY($1)`, seriesIDs)
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id = $1`, folderID)
	})

	type aliasSpec struct {
		createdAgo time.Duration
		ownsFile   bool
	}
	series := []struct {
		id           string
		canonicalAge time.Duration
		canonicalHas bool
		alias        *aliasSpec
	}{
		// The canonical new episode has no direct file; its alias owns the
		// present file. The alias is old, so it cannot qualify on its own.
		{aliasFile, 2 * time.Hour, false, &aliasSpec{400 * 24 * time.Hour, true}},
		// Only the alias row looks like a new season with a present file. The
		// canonical S2E1 is old, so the series must not surface.
		{aliasOnlySeason, 400 * 24 * time.Hour, false, &aliasSpec{2 * time.Hour, true}},
		// The canonical new episode qualifies on its own; the newer alias row
		// must not become the ordering timestamp.
		{aliasOrder, 48 * time.Hour, true, &aliasSpec{time.Hour, true}},
	}

	for _, s := range series {
		s1, s2 := s.id+"-s1e1", s.id+"-s2e1"
		exec(`INSERT INTO media_items (content_id, type, title, status, genres) VALUES ($1, 'series', $1, 'matched', '{}'::text[])`, s.id)
		exec(`INSERT INTO episodes (content_id, series_id, season_number, episode_number, title, overview, runtime, rating_imdb, created_at)
			VALUES ($1, $3, 1, 1, 'One', '', 30, 0, NOW() - interval '400 days'), ($2, $3, 2, 1, 'Two', '', 30, 0, NOW() - make_interval(secs => $4))`,
			s1, s2, s.id, s.canonicalAge.Seconds())
		exec(`INSERT INTO media_files (episode_id, media_folder_id, file_path, duration) VALUES ($1, $2, '/returning-alias/' || $1, 1800)`, s1, folderID)
		if s.canonicalHas {
			exec(`INSERT INTO media_files (episode_id, media_folder_id, file_path, duration) VALUES ($1, $2, '/returning-alias/' || $1, 1800)`, s2, folderID)
		}
		if s.alias != nil {
			aliasID := s.id + "-alias"
			exec(`INSERT INTO episodes (content_id, series_id, season_number, episode_number, title, overview, runtime, rating_imdb, created_at)
				VALUES ($1, $2, 3, 1, 'Alias', '', 30, 0, NOW() - make_interval(secs => $3))`,
				aliasID, s.id, s.alias.createdAgo.Seconds())
			if s.alias.ownsFile {
				exec(`INSERT INTO media_files (episode_id, media_folder_id, file_path, duration) VALUES ($1, $2, '/returning-alias/' || $1, 1800)`, aliasID, folderID)
			}
			exec(`INSERT INTO episode_aliases (
					series_id, canonical_episode_id, alias_episode_id,
					provider_installation_id, provider_series_id,
					provider_season_number, provider_episode_number, source
				) VALUES ($1, $2, $3, 0, '', 2, 1, 'absolute_layout')`,
				s.id, s2, aliasID)
		}
		exec(`INSERT INTO user_watch_history (id, user_id, profile_id, media_item_id, watched_at) VALUES ($1, $2, $3, $4, NOW() - interval '200 days')`,
			id("history-"+s.id), userID, profileID, s1)
	}
	exec(`INSERT INTO media_item_libraries (content_id, media_folder_id) SELECT unnest($1::text[]), $2`, seriesIDs, folderID)

	section := ResolvedSection{ID: id("section"), SectionType: SectionReturningShows, ItemLimit: 20}
	filter := catalog.AccessFilter{AllowedLibraryIDs: []int{folderID}}
	got, err := NewFetcher(pool).FetchOne(ctx, section, nil, []int{folderID}, userID, profileID, filter)
	if err != nil {
		t.Fatalf("FetchOne: %v", err)
	}
	if ids, want := contentIDs(got.Items), []string{aliasFile, aliasOrder}; strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("row = %v, want %v", ids, want)
	}
}
