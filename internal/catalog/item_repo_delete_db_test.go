package catalog

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestDeleteItemWithNullEpisodeStillPathDB is a regression test for the admin
// item delete: episodes.still_path is nullable, and scanning a NULL into string
// from that UNION arm aborted the whole delete. Every artwork column the delete
// collects is coalesced, so a series with an episode that has no still deletes
// together with the rest of its artwork.
func TestDeleteItemWithNullEpisodeStillPathDB(t *testing.T) {
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
	contentPrefix := fmt.Sprintf("delnull/%d", suffix)
	seriesID := fmt.Sprintf("delnull-series-%d", suffix)
	seasonID := fmt.Sprintf("delnull-season-%d", suffix)
	episodeWithoutStill := fmt.Sprintf("delnull-episode-nostill-%d", suffix)
	episodeWithStill := fmt.Sprintf("delnull-episode-still-%d", suffix)

	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatalf("exec %q: %v", query, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_items WHERE content_id = $1`, seriesID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM artwork_revision_gc_candidates WHERE original_path LIKE $1`, contentPrefix+"/%")
	})

	// Every other artwork arm holds a real path, so the only NULL the delete
	// query can meet is the first episode's still_path.
	exec(`INSERT INTO media_items (content_id, type, title, poster_path, backdrop_path, logo_path)
		VALUES ($1, 'series', 'Delete With Null Still', $2, $3, $4)`,
		seriesID, contentPrefix+"/poster/original.webp", contentPrefix+"/backdrop/original.webp", contentPrefix+"/logo/original.webp")
	exec(`INSERT INTO seasons (content_id, series_id, season_number, title, poster_path)
		VALUES ($1, $2, 1, 'Season 1', $3)`, seasonID, seriesID, contentPrefix+"/s1/poster/original.webp")
	exec(`INSERT INTO episodes (content_id, series_id, season_id, season_number, episode_number, title, still_path) VALUES
		($1, $3, $4, 1, 1, 'No Still', NULL),
		($2, $3, $4, 1, 2, 'With Still', $5)`,
		episodeWithoutStill, episodeWithStill, seriesID, seasonID, contentPrefix+"/e2/still/original.webp")
	exec(`INSERT INTO media_item_backdrops (content_id, ordinal, path) VALUES ($1, 1, $2)`,
		seriesID, contentPrefix+"/extra/original.webp")

	repo := NewItemRepository(pool)
	imageDirs, err := repo.Delete(ctx, seriesID)
	if err != nil {
		t.Fatalf("Delete with a NULL episode still_path: %v", err)
	}

	var items, episodes, seasons int
	if err := pool.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM media_items WHERE content_id = $1),
			(SELECT count(*) FROM episodes WHERE series_id = $1),
			(SELECT count(*) FROM seasons WHERE series_id = $1)`, seriesID).Scan(&items, &episodes, &seasons); err != nil {
		t.Fatalf("count surviving rows: %v", err)
	}
	if items != 0 || episodes != 0 || seasons != 0 {
		t.Fatalf("surviving rows after delete: items=%d episodes=%d seasons=%d", items, episodes, seasons)
	}

	// The NULL still contributes no directory; every other path still does.
	for _, want := range []string{
		contentPrefix + "/poster/",
		contentPrefix + "/backdrop/",
		contentPrefix + "/logo/",
		contentPrefix + "/s1/poster/",
		contentPrefix + "/e2/still/",
		contentPrefix + "/extra/",
	} {
		if !slices.Contains(imageDirs, want) {
			t.Fatalf("image dirs after delete = %q, want %q", imageDirs, want)
		}
	}
}
