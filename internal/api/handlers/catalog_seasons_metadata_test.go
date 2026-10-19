package handlers

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestSeriesSeasonsKeepsMetadataOnlySeasons pins the virtual-series rule: the
// seasons list comes from metadata, so a season whose candidates were never
// materialized stays visible with its metadata episode count. Its season and
// episode reads use the metadata episodes as a coherent fileless fallback.
func TestSeriesSeasonsKeepsMetadataOnlySeasons(t *testing.T) {
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	prefix := fmt.Sprintf("season-metadata-%d", time.Now().UnixNano())
	var library int
	if err := pool.QueryRow(ctx, `INSERT INTO media_folders(type,name) VALUES('tv',$1) RETURNING id`, prefix).Scan(&library); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_folders WHERE id=$1`, library)
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_items WHERE content_id LIKE $1`, prefix+"%")
	})
	if _, err := pool.Exec(ctx, `INSERT INTO media_items(content_id,type,title,genres,default_metadata_language) VALUES($1,'series','Synthetic Series','{}','en')`, prefix); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO media_item_libraries(content_id,media_folder_id) VALUES($1,$2)`, prefix, library); err != nil {
		t.Fatal(err)
	}

	seasons := catalog.NewSeasonRepository(pool)
	episodes := catalog.NewEpisodeRepository(pool)
	seasonID := func(number int) string { return fmt.Sprintf("%s-S%02d", prefix, number) }
	for number := 1; number <= 3; number++ {
		if err := seasons.Upsert(ctx, &models.Season{
			ContentID: seasonID(number), SeriesID: prefix, SeasonNumber: number,
			Title: fmt.Sprintf("Season %d", number), DefaultMetadataLanguage: "en",
		}); err != nil {
			t.Fatal(err)
		}
		for episodeNumber := 1; episodeNumber <= 2; episodeNumber++ {
			id := fmt.Sprintf("%s-E%02d", seasonID(number), episodeNumber)
			if err := episodes.Upsert(ctx, &models.Episode{
				ContentID: id, SeriesID: prefix, SeasonID: seasonID(number),
				SeasonNumber: number, EpisodeNumber: episodeNumber, Title: "Episode",
				DefaultMetadataLanguage: "en",
			}); err != nil {
				t.Fatal(err)
			}
			// Only season 1 has materialized candidates. Seasons 2 and 3 are
			// metadata-only, as an undiscovered virtual season is.
			if number == 1 {
				if _, err := pool.Exec(ctx, `INSERT INTO episode_libraries(episode_id,media_folder_id) VALUES($1,$2)`, id, library); err != nil {
					t.Fatal(err)
				}
			}
		}
	}

	items := &ItemsHandler{
		itemRepo:    catalog.NewItemRepository(pool),
		seasonRepo:  seasons,
		episodeRepo: episodes,
	}
	h := NewCatalogResourceHandler(items)
	viewer := ItemViewer{}

	got, err := h.SeriesSeasons(ctx, viewer, prefix, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("seasons = %d, want 3 metadata seasons", len(got))
	}
	wantNumbers := []int{1, 2, 3}
	for i, season := range got {
		if season.SeasonNumber != wantNumbers[i] {
			t.Fatalf("season[%d] = %d, want %d (ordered)", i, season.SeasonNumber, wantNumbers[i])
		}
		if season.EpisodeCount != 2 {
			t.Fatalf("season %d episode_count = %d, want the metadata count 2", season.SeasonNumber, season.EpisodeCount)
		}
	}
	if got[1].UserData != nil || got[2].UserData != nil {
		t.Fatalf("metadata-only seasons have user_data: %+v %+v", got[1].UserData, got[2].UserData)
	}

	season, err := h.SeriesSeason(ctx, viewer, prefix, 2)
	if err != nil {
		t.Fatalf("metadata-only season by number: %v", err)
	}
	if season.EpisodeCount != 2 {
		t.Fatalf("metadata-only season episode_count = %d, want 2", season.EpisodeCount)
	}

	byNumber, err := h.SeasonEpisodes(ctx, viewer, prefix, 2)
	if err != nil {
		t.Fatalf("metadata-only season episodes: %v", err)
	}
	if len(byNumber) != 2 {
		t.Fatalf("metadata-only season episodes = %d, want 2", len(byNumber))
	}
	for _, episode := range byNumber {
		if len(episode.Files) != 0 {
			t.Fatalf("metadata-only episode %s has files %+v", episode.ContentID, episode.Files)
		}
	}

	byItem, err := h.ItemEpisodes(ctx, viewer, seasonID(2))
	if err != nil {
		t.Fatalf("metadata-only season item episodes: %v", err)
	}
	if len(byItem) != 2 {
		t.Fatalf("metadata-only season item episodes = %d, want 2", len(byItem))
	}

	if _, err := h.SeriesSeason(ctx, viewer, prefix, 9); err == nil {
		t.Fatal("a season with no metadata should still be not found")
	}
}
