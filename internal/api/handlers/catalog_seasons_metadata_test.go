package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/access"
	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/scanner"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestSeriesSeasonsKeepsMetadataOnlySeasons pins two rules with one fixture
// set. First, season metadata is the list's authority: a season whose
// candidates were never materialized stays visible with its metadata episode
// count, and a fileless episode serializes without a "files" key. Second, every
// season read selects episodes by (series_id, season_number), not by the
// season row's season_id, so a fully or partly unlinked season answers the same
// set from the series count, season page, ItemEpisodes, SeasonEpisodes, and the
// season detail. The partly linked season also proves the by-number read keeps
// unlinked-but-available materialized episodes and their files.
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
	episodeID := func(number, episodeNumber int) string {
		return fmt.Sprintf("%s-E%02d", seasonID(number), episodeNumber)
	}

	type fixture struct {
		number       int
		episodes     int
		linked       int // episodes carrying this season row's season_id
		materialized bool
	}
	fixtures := []fixture{
		// Fully linked and materialized: the ordinary file-backed season.
		{number: 1, episodes: 2, linked: 2, materialized: true},
		// Partly linked and materialized: the season-id read would answer
		// only episodes 1-2 and drop episodes 3-4 with their files.
		{number: 2, episodes: 4, linked: 2, materialized: true},
		// Fully unlinked and metadata-only: no candidate materialized.
		{number: 3, episodes: 2, linked: 0, materialized: false},
		// Fully unlinked but materialized: the by-number rollback keeps the
		// files even though no episode points at the season row.
		{number: 4, episodes: 2, linked: 0, materialized: true},
		// Linked but metadata-only: episodes point at the season row, yet no
		// candidate materialized. The rollup has no entry, so the reads must
		// still answer the metadata count and rows.
		{number: 5, episodes: 2, linked: 2, materialized: false},
	}

	for _, f := range fixtures {
		sid := seasonID(f.number)
		if err := seasons.Upsert(ctx, &models.Season{
			ContentID: sid, SeriesID: prefix, SeasonNumber: f.number,
			Title: fmt.Sprintf("Season %d", f.number), DefaultMetadataLanguage: "en",
		}); err != nil {
			t.Fatal(err)
		}
		for episodeNumber := 1; episodeNumber <= f.episodes; episodeNumber++ {
			id := episodeID(f.number, episodeNumber)
			episode := &models.Episode{
				ContentID: id, SeriesID: prefix, SeasonNumber: f.number,
				EpisodeNumber: episodeNumber, Title: "Episode", DefaultMetadataLanguage: "en",
			}
			if episodeNumber <= f.linked {
				episode.SeasonID = sid
			}
			if err := episodes.Upsert(ctx, episode); err != nil {
				t.Fatal(err)
			}
			if !f.materialized {
				continue
			}
			if _, err := pool.Exec(ctx, `INSERT INTO episode_libraries(episode_id,media_folder_id) VALUES($1,$2)`, id, library); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,resolution,episode_id) VALUES($1,$2,$3,1024,'1080p',$4)`,
				id, library, fmt.Sprintf("/synthetic/%s.mkv", id), id); err != nil {
				t.Fatal(err)
			}
		}
	}

	itemsRepo := catalog.NewItemRepository(pool)
	files := scanner.NewFileRepository(pool)
	detailSvc := catalog.NewDetailService(itemsRepo, episodes, seasons, catalog.NewPersonRepository(pool), files)
	items := &ItemsHandler{
		itemRepo:    itemsRepo,
		seasonRepo:  seasons,
		episodeRepo: episodes,
		fileRepo:    files,
		detailSvc:   detailSvc,
	}
	h := NewCatalogResourceHandler(items)
	viewer := ItemViewer{}

	router := chi.NewRouter()
	router.Get("/series/{id}/seasons/{num}/episodes", h.HandleGetEpisodes)
	router.Get("/items/{id}/episodes", h.HandleGetItemEpisodes)

	got, err := h.SeriesSeasons(ctx, viewer, prefix, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(fixtures) {
		t.Fatalf("seasons = %d, want %d", len(got), len(fixtures))
	}
	for i, f := range fixtures {
		season := got[i]
		if season.SeasonNumber != f.number {
			t.Fatalf("season[%d] = %d, want %d (ordered)", i, season.SeasonNumber, f.number)
		}
		if season.EpisodeCount != f.episodes {
			t.Fatalf("season %d episode_count = %d, want %d", f.number, season.EpisodeCount, f.episodes)
		}
	}

	for _, f := range fixtures {
		t.Run(fmt.Sprintf("season-%02d", f.number), func(t *testing.T) {
			season, err := h.SeriesSeason(ctx, viewer, prefix, f.number)
			if err != nil {
				t.Fatalf("season page: %v", err)
			}
			byNumber, err := h.SeasonEpisodes(ctx, viewer, prefix, f.number)
			if err != nil {
				t.Fatalf("season episodes: %v", err)
			}
			byItem, err := h.ItemEpisodes(ctx, viewer, seasonID(f.number))
			if err != nil {
				t.Fatalf("item episodes: %v", err)
			}
			detail, err := detailSvc.GetItemDetail(ctx, seasonID(f.number), catalog.AccessFilter{})
			if err != nil {
				t.Fatalf("season detail: %v", err)
			}

			counts := map[string]int{
				"season page":     season.EpisodeCount,
				"season episodes": len(byNumber),
				"item episodes":   len(byItem),
				"season detail":   episodeCountOf(detail),
			}
			for name, count := range counts {
				if count != f.episodes {
					t.Fatalf("%s count = %d, want %d: every season read must select the same set", name, count, f.episodes)
				}
			}

			for name, views := range map[string][]EpisodeView{
				"season episodes": byNumber,
				"item episodes":   byItem,
			} {
				for i, view := range views {
					if view.EpisodeNumber != i+1 {
						t.Fatalf("%s[%d] = episode %d, want %d", name, i, view.EpisodeNumber, i+1)
					}
					fields := episodeWireFields(t, view)
					rawFiles, hasFiles := fields["files"]
					if !f.materialized {
						if hasFiles {
							t.Fatalf("%s episode %s serializes a files key for a metadata-only episode: %s", name, view.ContentID, rawFiles)
						}
						continue
					}
					if !hasFiles {
						t.Fatalf("%s episode %s has no files key; unlinked-but-available episodes must keep their files", name, view.ContentID)
					}
					var files []episodeFileResponse
					if err := json.Unmarshal(rawFiles, &files); err != nil {
						t.Fatal(err)
					}
					if len(files) != 1 || files[0].FileID == 0 {
						t.Fatalf("%s episode %s files = %s, want one materialized file", name, view.ContentID, rawFiles)
					}
				}
			}

			// The v1 routes share these handlers, so pin the status and wire
			// body as well: a 200 with the episodes array and, for a fileless
			// episode, no "files" key at all.
			for _, route := range []struct{ name, path string }{
				{"season episodes http", fmt.Sprintf("/series/%s/seasons/%d/episodes", prefix, f.number)},
				{"item episodes http", fmt.Sprintf("/items/%s/episodes", seasonID(f.number))},
			} {
				req := httptest.NewRequest(http.MethodGet, route.path, nil).WithContext(access.SetScope(t.Context(), access.Scope{}))
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Fatalf("%s status = %d: %s", route.name, rec.Code, rec.Body.String())
				}
				var body struct {
					Episodes []map[string]json.RawMessage `json:"episodes"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if len(body.Episodes) != f.episodes {
					t.Fatalf("%s episodes = %d, want %d", route.name, len(body.Episodes), f.episodes)
				}
				for i, episode := range body.Episodes {
					rawFiles, hasFiles := episode["files"]
					if hasFiles != f.materialized {
						t.Fatalf("%s episode[%d] files key present = %v, want %v: %s", route.name, i, hasFiles, f.materialized, rec.Body.String())
					}
					if !f.materialized {
						continue
					}
					var files []episodeFileResponse
					if err := json.Unmarshal(rawFiles, &files); err != nil {
						t.Fatal(err)
					}
					if len(files) != 1 || files[0].FileID == 0 {
						t.Fatalf("%s episode[%d] files = %s, want one materialized file", route.name, i, rawFiles)
					}
				}
			}
		})
	}

	if _, err := h.SeriesSeason(ctx, viewer, prefix, 9); err == nil {
		t.Fatal("a season with no metadata should still be not found")
	}
}

func episodeCountOf(detail *catalog.ItemDetail) int {
	if detail == nil || detail.EpisodeCount == nil {
		return 0
	}
	return *detail.EpisodeCount
}

// episodeWireFields is the serialized shape a client receives. It is the
// contract the fileless fallback must keep: omitempty drops an empty files
// list instead of sending "files": [].
func episodeWireFields(t *testing.T, view EpisodeView) map[string]json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}
