package handlers

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/scanner"
)

// TestSeasonEpisodesMergePartialAvailabilityDB pins the season read rule for a
// partly available season: every canonical metadata episode is listed, the
// file-backed subset carries files, fileless rows carry no files, and an alias
// layout row never appears.
func TestSeasonEpisodesMergePartialAvailabilityDB(t *testing.T) { //nolint:funlen // one DB fixture covers persisted and partial-availability season reads
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	prefix := fmt.Sprintf("season-partial-%d", time.Now().UnixNano())
	seriesID := prefix + "-series"
	seasonID := prefix + "-S01"
	aliasID := prefix + "-alias"
	var library int
	if err := pool.QueryRow(ctx, `INSERT INTO media_folders(type,name) VALUES('tv',$1) RETURNING id`, prefix).Scan(&library); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_files WHERE media_folder_id = $1`, library)
		_, _ = pool.Exec(context.Background(), `DELETE FROM episode_aliases WHERE series_id = $1`, seriesID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM episodes WHERE series_id = $1`, seriesID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM seasons WHERE series_id = $1`, seriesID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_items WHERE content_id = $1`, seriesID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_folders WHERE id = $1`, library)
	})
	if _, err := pool.Exec(ctx, `INSERT INTO media_items(content_id,type,title,genres,default_metadata_language) VALUES($1,'series','Partial Season','{}','en')`, seriesID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO media_item_libraries(content_id,media_folder_id) VALUES($1,$2)`, seriesID, library); err != nil {
		t.Fatal(err)
	}

	episodesRepo := catalog.NewEpisodeRepository(pool)
	seasonsRepo := catalog.NewSeasonRepository(pool)
	if err := seasonsRepo.Upsert(ctx, &models.Season{
		ContentID: seasonID, SeriesID: seriesID, SeasonNumber: 1,
		Title: "Season 1", DefaultMetadataLanguage: "en",
	}); err != nil {
		t.Fatal(err)
	}
	for episodeNumber := 1; episodeNumber <= 4; episodeNumber++ {
		id := fmt.Sprintf("%s-E%02d", seasonID, episodeNumber)
		if err := episodesRepo.Upsert(ctx, &models.Episode{
			ContentID: id, SeriesID: seriesID, SeasonID: seasonID, SeasonNumber: 1,
			EpisodeNumber: episodeNumber, Title: fmt.Sprintf("Episode %d", episodeNumber),
			DefaultMetadataLanguage: "en",
		}); err != nil {
			t.Fatal(err)
		}
		if episodeNumber > 2 {
			continue
		}
		if _, err := pool.Exec(ctx, `INSERT INTO episode_libraries(episode_id,media_folder_id) VALUES($1,$2)`, id, library); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,resolution,episode_id) VALUES($1,$2,$3,1024,'1080p',$1)`,
			id, library, fmt.Sprintf("/partial/%s.mkv", id)); err != nil {
			t.Fatal(err)
		}
	}
	// An absolute-layout duplicate of episode 1 that would sort last inside the
	// same season if alias exclusion failed.
	if err := episodesRepo.Upsert(ctx, &models.Episode{
		ContentID: aliasID, SeriesID: seriesID, SeasonNumber: 1, EpisodeNumber: 99,
		Title: "Alias duplicate", DefaultMetadataLanguage: "en",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO episode_libraries(episode_id,media_folder_id) VALUES($1,$2)`, aliasID, library); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,resolution,episode_id) VALUES($1,$2,$3,1024,'1080p',$1)`,
		aliasID, library, fmt.Sprintf("/partial/%s.mkv", aliasID)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO episode_aliases (
			series_id, canonical_episode_id, alias_episode_id,
			provider_installation_id, provider_series_id,
			provider_season_number, provider_episode_number, source
		) VALUES ($1, $2, $3, 0, '', 1, 99, 'absolute_layout')`,
		seriesID, seasonID+"-E01", aliasID); err != nil {
		t.Fatal(err)
	}

	itemsRepo := catalog.NewItemRepository(pool)
	files := scanner.NewFileRepository(pool)
	detailSvc := catalog.NewDetailService(itemsRepo, episodesRepo, seasonsRepo, catalog.NewPersonRepository(pool), files)
	items := &ItemsHandler{
		itemRepo:    itemsRepo,
		seasonRepo:  seasonsRepo,
		episodeRepo: episodesRepo,
		fileRepo:    files,
		detailSvc:   detailSvc,
	}
	h := NewCatalogResourceHandler(items)
	viewer := ItemViewer{}

	seasons, err := h.SeriesSeasons(ctx, viewer, seriesID, false)
	if err != nil {
		t.Fatalf("series seasons: %v", err)
	}
	if len(seasons) != 1 || seasons[0].EpisodeCount != 4 {
		t.Fatalf("series seasons = %+v, want season 1 with 4 episodes", seasons)
	}

	season, err := h.SeriesSeason(ctx, viewer, seriesID, 1)
	if err != nil {
		t.Fatalf("season page: %v", err)
	}
	if season.EpisodeCount != 4 {
		t.Fatalf("season page count = %d, want 4", season.EpisodeCount)
	}

	for name, views := range map[string][]EpisodeView{
		"season episodes": mustSeasonEpisodes(t, h, ctx, viewer, seriesID, 1),
		"item episodes":   mustItemEpisodes(t, h, ctx, viewer, seasonID),
	} {
		if len(views) != 4 {
			t.Fatalf("%s = %d episodes, want 4", name, len(views))
		}
		for i, view := range views {
			if view.ContentID == aliasID {
				t.Fatalf("%s exposed alias row %q", name, aliasID)
			}
			if view.EpisodeNumber != i+1 {
				t.Fatalf("%s[%d] episode number = %d, want %d", name, i, view.EpisodeNumber, i+1)
			}
			fields := episodeWireFields(t, view)
			rawFiles, hasFiles := fields["files"]
			if wantFiles := i < 2; hasFiles != wantFiles {
				t.Fatalf("%s[%d] files key present = %v, want %v: %s", name, i, hasFiles, wantFiles, rawFiles)
			}
		}
	}

	detail, err := detailSvc.GetItemDetail(ctx, seasonID, catalog.AccessFilter{})
	if err != nil {
		t.Fatalf("season detail: %v", err)
	}
	if got := episodeCountOf(detail); got != 4 {
		t.Fatalf("season detail count = %d, want 4", got)
	}

}

func mustSeasonEpisodes(t *testing.T, h *CatalogResourceHandler, ctx context.Context, viewer ItemViewer, seriesID string, num int) []EpisodeView {
	t.Helper()
	views, err := h.SeasonEpisodes(ctx, viewer, seriesID, num)
	if err != nil {
		t.Fatalf("season episodes: %v", err)
	}
	return views
}

func mustItemEpisodes(t *testing.T, h *CatalogResourceHandler, ctx context.Context, viewer ItemViewer, seasonID string) []EpisodeView {
	t.Helper()
	views, err := h.ItemEpisodes(ctx, viewer, seasonID)
	if err != nil {
		t.Fatalf("item episodes: %v", err)
	}
	return views
}
