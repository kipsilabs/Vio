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

func newExtraArtworkTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedSeriesWithNullEpisodeStill seeds a series with a poster, one season with
// a poster, and one episode whose still_path is NULL. It returns the series ID
// and the image directories the artwork collection should report for it.
//
// still_path is the one nullable artwork column among those surfaces, and a
// NULL there surfaces under the union's first column name (poster_path), so
// scanning the union raw failed with
// `can't scan into dest[0] (col: poster_path): cannot scan NULL into *string`.
func seedSeriesWithNullEpisodeStill(t *testing.T, pool *pgxpool.Pool) (string, []string) {
	t.Helper()
	ctx := context.Background()
	prefix := fmt.Sprintf("null-image-path-%d", time.Now().UnixNano())
	seriesID, seasonID, episodeID := prefix+"-series", prefix+"-season", prefix+"-episode"
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_items WHERE content_id = $1`, seriesID)
	})

	seriesPoster := "tmdb/series/1/poster/original.webp"
	seasonPoster := "tmdb/series/1/season/1/poster/w500.webp"
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items (content_id, type, title, poster_path)
		VALUES ($1, 'series', 'Null still fixture', $2)`, seriesID, seriesPoster); err != nil {
		t.Fatalf("seed series: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO seasons (content_id, series_id, season_number, poster_path)
		VALUES ($1, $2, 1, $3)`, seasonID, seriesID, seasonPoster); err != nil {
		t.Fatalf("seed season: %v", err)
	}
	// still_path is omitted on purpose: the column has no default and stays NULL.
	if _, err := pool.Exec(ctx, `
		INSERT INTO episodes (content_id, series_id, season_id, season_number, episode_number, title)
		VALUES ($1, $2, $3, 1, 1, 'No still')`, episodeID, seriesID, seasonID); err != nil {
		t.Fatalf("seed episode: %v", err)
	}
	var stillIsNull bool
	if err := pool.QueryRow(ctx, `SELECT still_path IS NULL FROM episodes WHERE content_id = $1`, episodeID).Scan(&stillIsNull); err != nil || !stillIsNull {
		t.Fatalf("episode still_path IS NULL = %v, %v; want the fixture to seed a NULL still", stillIsNull, err)
	}
	return seriesID, []string{imageDeletePrefix(seasonPoster), imageDeletePrefix(seriesPoster)}
}

func assertSameImageDirs(t *testing.T, got, want []string) {
	t.Helper()
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("image dirs = %v, want %v", got, want)
	}
}

// TestItemDeleteToleratesNullStillPathDB pins item deletion against a series
// with an episode whose still_path is NULL: the delete must still remove the
// item and report its artwork directories.
func TestItemDeleteToleratesNullStillPathDB(t *testing.T) {
	pool := newExtraArtworkTestPool(t)
	ctx := context.Background()
	repo := NewItemRepository(pool)
	seriesID, wantDirs := seedSeriesWithNullEpisodeStill(t, pool)

	dirs, err := repo.Delete(ctx, seriesID)
	if err != nil {
		t.Fatalf("Delete with a NULL episode still_path: %v", err)
	}
	assertSameImageDirs(t, dirs, wantDirs)

	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_items WHERE content_id = $1`, seriesID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("media_items after delete = %d, %v; want 0", remaining, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM episodes WHERE series_id = $1`, seriesID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("episodes after delete = %d, %v; want 0", remaining, err)
	}
}

// TestCollectRawImageDirsToleratesNullStillPathDB covers the same union query
// in the folder/collection delete path.
func TestCollectRawImageDirsToleratesNullStillPathDB(t *testing.T) {
	pool := newExtraArtworkTestPool(t)
	seriesID, wantDirs := seedSeriesWithNullEpisodeStill(t, pool)

	dirs, err := collectRawImageDirs(context.Background(), pool, []string{seriesID})
	if err != nil {
		t.Fatalf("collectRawImageDirs with a NULL episode still_path: %v", err)
	}
	assertSameImageDirs(t, dirs, wantDirs)
}
