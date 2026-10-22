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

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/access"
	"github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/auth"
	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/scanner"
	"github.com/Silo-Server/silo-server/internal/userstore/pgstore"
	"github.com/Silo-Server/silo-server/internal/watchstate"
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
//
//nolint:funlen,gocyclo // One DB fixture matrix covers linked, partly linked, unlinked, metadata-only, synthetic, and mixed seasons; splitting it would duplicate the migration-backed setup.
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

	// A series without season rows exercises the synthetic season list. Season
	// 1 has ten metadata episodes but only two materialized, so its count must
	// be the available two; season 2 has two metadata episodes and no files, so
	// its count must be the metadata total.
	syntheticPrefix := prefix + "-synthetic"
	if _, err := pool.Exec(ctx, `INSERT INTO media_items(content_id,type,title,genres,default_metadata_language) VALUES($1,'series','Synthetic Series Without Season Rows','{}','en')`, syntheticPrefix); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO media_item_libraries(content_id,media_folder_id) VALUES($1,$2)`, syntheticPrefix, library); err != nil {
		t.Fatal(err)
	}
	syntheticEpisodeID := func(season, episode int) string {
		return fmt.Sprintf("%s-E%02d%02d", syntheticPrefix, season, episode)
	}
	for episodeNumber := 1; episodeNumber <= 10; episodeNumber++ {
		id := syntheticEpisodeID(1, episodeNumber)
		if err := episodes.Upsert(ctx, &models.Episode{
			ContentID: id, SeriesID: syntheticPrefix, SeasonNumber: 1,
			EpisodeNumber: episodeNumber, Title: "Episode", DefaultMetadataLanguage: "en",
		}); err != nil {
			t.Fatal(err)
		}
		if episodeNumber > 2 {
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
	for episodeNumber := 1; episodeNumber <= 2; episodeNumber++ {
		if err := episodes.Upsert(ctx, &models.Episode{
			ContentID: syntheticEpisodeID(2, episodeNumber), SeriesID: syntheticPrefix, SeasonNumber: 2,
			EpisodeNumber: episodeNumber, Title: "Episode", DefaultMetadataLanguage: "en",
		}); err != nil {
			t.Fatal(err)
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

	synthetic, err := h.SeriesSeasons(ctx, viewer, syntheticPrefix, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(synthetic) != 2 {
		t.Fatalf("synthetic seasons = %d, want 2", len(synthetic))
	}
	syntheticCounts := make(map[int]int, len(synthetic))
	for _, season := range synthetic {
		syntheticCounts[season.SeasonNumber] = season.EpisodeCount
	}
	if syntheticCounts[1] != 2 {
		t.Fatalf("synthetic season 1 episode_count = %d, want 2 available episodes", syntheticCounts[1])
	}
	if syntheticCounts[2] != 2 {
		t.Fatalf("synthetic season 2 episode_count = %d, want 2 metadata episodes", syntheticCounts[2])
	}
	syntheticSeason1, err := h.SeriesSeason(ctx, viewer, syntheticPrefix, 1)
	if err != nil {
		t.Fatal(err)
	}
	if syntheticSeason1.EpisodeCount != 2 {
		t.Fatalf("synthetic season 1 page count = %d, want 2", syntheticSeason1.EpisodeCount)
	}
	syntheticSeason1Episodes, err := h.SeasonEpisodes(ctx, viewer, syntheticPrefix, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(syntheticSeason1Episodes) != 2 {
		t.Fatalf("synthetic season 1 episodes = %d, want 2", len(syntheticSeason1Episodes))
	}
	syntheticSeason2, err := h.SeriesSeason(ctx, viewer, syntheticPrefix, 2)
	if err != nil {
		t.Fatal(err)
	}
	if syntheticSeason2.EpisodeCount != 2 {
		t.Fatalf("synthetic season 2 page count = %d, want 2", syntheticSeason2.EpisodeCount)
	}
	syntheticSeason2Episodes, err := h.SeasonEpisodes(ctx, viewer, syntheticPrefix, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(syntheticSeason2Episodes) != 2 {
		t.Fatalf("synthetic season 2 episodes = %d, want 2", len(syntheticSeason2Episodes))
	}

	// The public item-detail enrichment must agree with the season reads for
	// linked, partly linked, unlinked, metadata-only, and synthetic seasons.
	var userID int
	if err := pool.QueryRow(ctx, `INSERT INTO users (username,role) VALUES ($1,'user') RETURNING id`, prefix+"-viewer").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, userID) })
	profileID := fmt.Sprintf("00000000-0000-4000-8002-%012d", time.Now().UnixNano()%1_000_000_000_000)
	if _, err := pool.Exec(ctx, `INSERT INTO user_profiles (id,user_id,name) VALUES ($1,$2,'Season metadata viewer')`, profileID, userID); err != nil {
		t.Fatal(err)
	}
	provider := pgstore.NewPostgresProvider(pool)
	store, err := provider.ForUser(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	watched := func(id string) {
		t.Helper()
		if err := store.SetProgressAt(ctx, profileID, id, 1500, 1500, true, at); err != nil {
			t.Fatal(err)
		}
	}
	resuming := func(id string) {
		t.Helper()
		if err := store.SetProgressAt(ctx, profileID, id, 300, 1500, false, at); err != nil {
			t.Fatal(err)
		}
	}
	// Season 2 is partly linked: these episodes have files but no season_id,
	// so the old season-row rollup would report no state.
	watched(episodeID(2, 3))
	resuming(episodeID(2, 4))
	// Metadata-only seasons have no availability rollup entry.
	watched(episodeID(3, 1))
	resuming(episodeID(3, 2))
	// Linked metadata-only season: the row rollup has no entry, so this
	// branch must fold the linked metadata episodes.
	watched(episodeID(5, 1))
	resuming(episodeID(5, 2))
	// Fully unlinked but materialized.
	watched(episodeID(4, 1))
	watched(episodeID(4, 2))
	// Synthetic season without a season row: one available episode watched.
	watched(syntheticEpisodeID(1, 1))
	// Synthetic metadata-only season: one metadata episode watched.
	watched(syntheticEpisodeID(2, 1))

	items.storeProvider = provider
	authCtx := middleware.SetClaims(ctx, &auth.Claims{UserID: userID, Role: "user"})
	authViewer := ItemViewer{ProfileID: profileID}
	for _, tc := range []struct {
		name       string
		id         string
		watched    int
		inProgress int
		unplayed   int
		played     bool
	}{
		{"partly linked detail", seasonID(2), 1, 1, 3, false},
		{"metadata-only detail", seasonID(3), 1, 1, 1, false},
		{"linked metadata-only detail", seasonID(5), 1, 1, 1, false},
		{"unlinked materialized detail", seasonID(4), 2, 0, 0, true},
		{"synthetic available detail", syntheticPrefix + "-S01", 1, 0, 1, false},
		{"synthetic metadata-only detail", syntheticPrefix + "-S02", 1, 0, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			detail, err := h.ItemDetail(authCtx, authViewer, tc.id)
			if err != nil {
				t.Fatalf("item detail: %v", err)
			}
			if detail.SeasonUserData == nil {
				t.Fatal("season user_data is nil")
			}
			got := *detail.SeasonUserData
			if got.WatchedCount != tc.watched || got.InProgressCount != tc.inProgress || got.UnplayedCount != tc.unplayed || got.Played != tc.played {
				t.Fatalf("season user_data = %+v, want watched=%d in_progress=%d unplayed=%d played=%v",
					got, tc.watched, tc.inProgress, tc.unplayed, tc.played)
			}
		})
	}

	// Marking a partly linked season watched must act on every episode the
	// season reads expose, not only the season-row-linked subset.
	items.watchState = watchstate.NewService(provider)
	authFilter := catalog.AccessFilter{UserID: userID, ProfileID: profileID}
	if _, err := items.SetWatchedState(authCtx, userID, profileID, seasonID(2), true, authFilter); err != nil {
		t.Fatalf("mark partly linked season watched: %v", err)
	}
	marked, err := h.ItemDetail(authCtx, authViewer, seasonID(2))
	if err != nil {
		t.Fatalf("detail after watched mutation: %v", err)
	}
	if marked.SeasonUserData == nil || marked.SeasonUserData.WatchedCount != 4 || !marked.SeasonUserData.Played {
		t.Fatalf("partly linked season watched state = %+v, want all 4 episodes watched", marked.SeasonUserData)
	}
	unwatchView, err := items.SetWatchedState(authCtx, userID, profileID, seasonID(2), false, authFilter)
	if err != nil {
		t.Fatalf("mark partly linked season unwatched: %v", err)
	}
	if unwatchView.AffectedCount != 4 {
		t.Fatalf("partly linked season unwatch affected_count = %d, want 4 distinct episodes", unwatchView.AffectedCount)
	}
	unmarked, err := h.ItemDetail(authCtx, authViewer, seasonID(2))
	if err != nil {
		t.Fatalf("detail after unwatched mutation: %v", err)
	}
	if unmarked.SeasonUserData == nil || unmarked.SeasonUserData.WatchedCount != 0 || unmarked.SeasonUserData.InProgressCount != 0 || unmarked.SeasonUserData.Played {
		t.Fatalf("partly linked season unwatched state = %+v, want no progress", unmarked.SeasonUserData)
	}

	// Synthetic seasons have no season row; watched/unwatched must still
	// resolve their displayed available or metadata episodes.
	for _, tc := range []struct{ name, id string }{
		{"synthetic available season", syntheticPrefix + "-S01"},
		{"synthetic metadata-only season", syntheticPrefix + "-S02"},
	} {
		t.Run(tc.name+" mutation", func(t *testing.T) {
			if _, err := items.SetWatchedState(authCtx, userID, profileID, tc.id, true, authFilter); err != nil {
				t.Fatalf("mark synthetic season watched: %v", err)
			}
			marked, err := h.ItemDetail(authCtx, authViewer, tc.id)
			if err != nil {
				t.Fatalf("synthetic detail after watched mutation: %v", err)
			}
			if marked.SeasonUserData == nil || marked.SeasonUserData.WatchedCount != 2 || !marked.SeasonUserData.Played {
				t.Fatalf("synthetic season watched state = %+v, want both episodes watched", marked.SeasonUserData)
			}
			if _, err := items.SetWatchedState(authCtx, userID, profileID, tc.id, false, authFilter); err != nil {
				t.Fatalf("mark synthetic season unwatched: %v", err)
			}
			unmarked, err := h.ItemDetail(authCtx, authViewer, tc.id)
			if err != nil {
				t.Fatalf("synthetic detail after unwatched mutation: %v", err)
			}
			if unmarked.SeasonUserData == nil || unmarked.SeasonUserData.WatchedCount != 0 || unmarked.SeasonUserData.InProgressCount != 0 || unmarked.SeasonUserData.Played {
				t.Fatalf("synthetic season unwatched state = %+v, want no progress", unmarked.SeasonUserData)
			}
		})
	}

	// A persisted season row and a metadata-only season can coexist. Both
	// must appear in the series list; only the missing row is synthesized.
	mixedPrefix := prefix + "-mixed"
	if _, err := pool.Exec(ctx, `INSERT INTO media_items(content_id,type,title,genres,default_metadata_language) VALUES($1,'series','Mixed Series','{}','en')`, mixedPrefix); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO media_item_libraries(content_id,media_folder_id) VALUES($1,$2)`, mixedPrefix, library); err != nil {
		t.Fatal(err)
	}
	if err := seasons.Upsert(ctx, &models.Season{ContentID: mixedPrefix + "-S01", SeriesID: mixedPrefix, SeasonNumber: 1, Title: "Season 1", DefaultMetadataLanguage: "en"}); err != nil {
		t.Fatal(err)
	}
	if err := episodes.Upsert(ctx, &models.Episode{ContentID: mixedPrefix + "-S01E01", SeriesID: mixedPrefix, SeasonID: mixedPrefix + "-S01", SeasonNumber: 1, EpisodeNumber: 1, Title: "One", DefaultMetadataLanguage: "en"}); err != nil {
		t.Fatal(err)
	}
	if err := episodes.Upsert(ctx, &models.Episode{ContentID: mixedPrefix + "-S02E01", SeriesID: mixedPrefix, SeasonNumber: 2, EpisodeNumber: 1, Title: "Two", DefaultMetadataLanguage: "en"}); err != nil {
		t.Fatal(err)
	}
	mixed, err := h.SeriesSeasons(ctx, viewer, mixedPrefix, false)
	if err != nil {
		t.Fatalf("mixed series seasons: %v", err)
	}
	if len(mixed) != 2 || mixed[0].SeasonNumber != 1 || mixed[1].SeasonNumber != 2 {
		t.Fatalf("mixed series seasons = %+v, want persisted season 1 plus synthesized season 2", mixed)
	}
	if mixed[1].EpisodeCount != 1 {
		t.Fatalf("mixed synthesized season count = %d, want 1", mixed[1].EpisodeCount)
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
