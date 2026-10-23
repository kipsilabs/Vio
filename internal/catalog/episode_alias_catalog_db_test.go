package catalog

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/models"
)

// TestEpisodeAliasExcludedFromCanonicalCatalogDB pins the read-time alias rule
// for every canonical episode list and count: the row named by
// episode_aliases.alias_episode_id never appears, and the canonical row does.
func TestEpisodeAliasExcludedFromCanonicalCatalogDB(t *testing.T) {
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

	prefix := fmt.Sprintf("episode-alias-list-%d", time.Now().UnixNano())
	seriesID := prefix + "-series"
	seasonOneID := prefix + "-season-1"
	seasonTwoID := prefix + "-season-2"
	canonicalID := prefix + "-episode-canonical"
	aliasID := prefix + "-episode-alias"
	airDate := time.Date(2024, 3, 7, 0, 0, 0, 0, time.UTC)

	var folderID int
	if err := pool.QueryRow(ctx,
		`INSERT INTO media_folders (type, name, enabled) VALUES ('series', $1, true) RETURNING id`,
		prefix,
	).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM episode_aliases WHERE series_id = $1`, seriesID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_files WHERE content_id = $1`, seriesID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM episodes WHERE series_id = $1`, seriesID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM seasons WHERE series_id = $1`, seriesID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_items WHERE content_id = $1`, seriesID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_folders WHERE id = $1`, folderID)
	})

	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO media_items (content_id, type, title) VALUES ($1, 'series', 'Alias Fixture')`, []any{seriesID}},
		{`INSERT INTO seasons (content_id, series_id, season_number) VALUES ($1, $2, 1), ($3, $2, 2)`, []any{seasonOneID, seriesID, seasonTwoID}},
		// Both rows share the air date. The canonical row is a named-season
		// episode; the alias row is the absolute-layout duplicate.
		{`INSERT INTO episodes (content_id, series_id, season_id, season_number, episode_number, title, air_date)
			VALUES ($1, $3, $4, 1, 1, 'Canonical', $5), ($2, $3, $6, 2, 1, 'Alias duplicate', $5)`,
			[]any{canonicalID, aliasID, seriesID, seasonOneID, airDate, seasonTwoID}},
		// The canonical row has no direct episode_libraries link; its alias owns
		// both the library link and the file. Availability must follow the alias.
		{`INSERT INTO episode_libraries (episode_id, media_folder_id) VALUES ($1, $2)`, []any{aliasID, folderID}},
		{`INSERT INTO media_files (content_id, episode_id, media_folder_id, file_path, file_size, container)
			VALUES ($1, $2, $3, '/tmp/alias-fixture.mkv', 1, 'mkv')`, []any{seriesID, aliasID, folderID}},
		{`INSERT INTO episode_aliases (
				series_id, canonical_episode_id, alias_episode_id,
				provider_installation_id, provider_series_id,
				provider_season_number, provider_episode_number, source
			) VALUES ($1, $2, $3, 0, '', 2, 1, 'absolute_layout')`,
			[]any{seriesID, canonicalID, aliasID}},
	} {
		if _, err := pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("seed %q: %v", statement.sql, err)
		}
	}

	repo := NewEpisodeRepository(pool)
	canonicalOnly := []string{canonicalID}

	assertEpisodeIDs := func(t *testing.T, label string, episodes []*models.Episode, want []string) {
		t.Helper()
		got := make([]string, 0, len(episodes))
		for _, episode := range episodes {
			got = append(got, episode.ContentID)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("%s episode ids = %v, want %v", label, got, want)
		}
	}

	t.Run("ListBySeries", func(t *testing.T) {
		episodes, err := repo.ListBySeries(ctx, seriesID)
		if err != nil {
			t.Fatal(err)
		}
		assertEpisodeIDs(t, "ListBySeries", episodes, canonicalOnly)
	})

	t.Run("ListBySeriesIDs", func(t *testing.T) {
		grouped, err := repo.ListBySeriesIDs(ctx, []string{seriesID})
		if err != nil {
			t.Fatal(err)
		}
		assertEpisodeIDs(t, "ListBySeriesIDs", grouped[seriesID], canonicalOnly)
	})

	t.Run("ListBySeriesGroupedBySeason", func(t *testing.T) {
		grouped, err := repo.ListBySeriesGroupedBySeason(ctx, seriesID)
		if err != nil {
			t.Fatal(err)
		}
		if len(grouped) != 1 {
			t.Fatalf("ListBySeriesGroupedBySeason seasons = %v, want only season 1", grouped)
		}
		assertEpisodeIDs(t, "ListBySeriesGroupedBySeason", grouped[1], canonicalOnly)
	})

	t.Run("ListBySeason", func(t *testing.T) {
		episodes, err := repo.ListBySeason(ctx, seriesID, 1)
		if err != nil {
			t.Fatal(err)
		}
		assertEpisodeIDs(t, "ListBySeason season 1", episodes, canonicalOnly)

		episodes, err = repo.ListBySeason(ctx, seriesID, 2)
		if err != nil {
			t.Fatal(err)
		}
		assertEpisodeIDs(t, "ListBySeason season 2", episodes, nil)
	})

	t.Run("ListBySeasonID", func(t *testing.T) {
		episodes, err := repo.ListBySeasonID(ctx, seasonOneID)
		if err != nil {
			t.Fatal(err)
		}
		assertEpisodeIDs(t, "ListBySeasonID season 1", episodes, canonicalOnly)

		episodes, err = repo.ListBySeasonID(ctx, seasonTwoID)
		if err != nil {
			t.Fatal(err)
		}
		assertEpisodeIDs(t, "ListBySeasonID season 2", episodes, nil)
	})

	t.Run("ListBySeasonIDs", func(t *testing.T) {
		grouped, err := repo.ListBySeasonIDs(ctx, []string{seasonOneID, seasonTwoID})
		if err != nil {
			t.Fatal(err)
		}
		assertEpisodeIDs(t, "ListBySeasonIDs season 1", grouped[seasonOneID], canonicalOnly)
		assertEpisodeIDs(t, "ListBySeasonIDs season 2", grouped[seasonTwoID], nil)
	})

	t.Run("ListSeasons", func(t *testing.T) {
		seasons, err := repo.ListSeasons(ctx, seriesID)
		if err != nil {
			t.Fatal(err)
		}
		want := []SeasonSummary{{SeasonNumber: 1, EpisodeCount: 1}}
		if !reflect.DeepEqual(seasons, want) {
			t.Fatalf("ListSeasons = %v, want %v", seasons, want)
		}
	})

	t.Run("ListMetadataSeasons", func(t *testing.T) {
		seasons, err := repo.ListMetadataSeasons(ctx, seriesID)
		if err != nil {
			t.Fatal(err)
		}
		want := []SeasonSummary{{SeasonNumber: 1, EpisodeCount: 1}}
		if !reflect.DeepEqual(seasons, want) {
			t.Fatalf("ListMetadataSeasons = %v, want %v", seasons, want)
		}
	})

	t.Run("ListMetadataBySeason", func(t *testing.T) {
		episodes, err := repo.ListMetadataBySeason(ctx, seriesID, 1)
		if err != nil {
			t.Fatal(err)
		}
		assertEpisodeIDs(t, "ListMetadataBySeason season 1", episodes, canonicalOnly)

		episodes, err = repo.ListMetadataBySeason(ctx, seriesID, 2)
		if err != nil {
			t.Fatal(err)
		}
		assertEpisodeIDs(t, "ListMetadataBySeason season 2", episodes, nil)
	})

	t.Run("ListIDsByParent", func(t *testing.T) {
		seriesIDs, err := repo.ListIDsBySeriesIDs(ctx, []string{seriesID})
		if err != nil {
			t.Fatal(err)
		}
		if got := seriesIDs[seriesID]; !slices.Equal(got, canonicalOnly) {
			t.Fatalf("ListIDsBySeriesIDs = %v, want %v", got, canonicalOnly)
		}
		seasonIDs, err := repo.ListIDsBySeasonIDs(ctx, []string{seasonOneID, seasonTwoID})
		if err != nil {
			t.Fatal(err)
		}
		if got := seasonIDs[seasonOneID]; !slices.Equal(got, canonicalOnly) {
			t.Fatalf("ListIDsBySeasonIDs season 1 = %v, want %v", got, canonicalOnly)
		}
		if got := seasonIDs[seasonTwoID]; len(got) != 0 {
			t.Fatalf("ListIDsBySeasonIDs season 2 = %v, want no alias rows", got)
		}
	})

	t.Run("PartitionIDs", func(t *testing.T) {
		assertPartition := func(t *testing.T, label string, withFile, fileless []string) {
			t.Helper()
			if !slices.Equal(withFile, canonicalOnly) || len(fileless) != 0 {
				t.Fatalf("%s = withFile %v fileless %v, want canonical only", label, withFile, fileless)
			}
		}
		withFile, fileless, err := repo.PartitionIDsBySeries(ctx, seriesID)
		if err != nil {
			t.Fatal(err)
		}
		assertPartition(t, "PartitionIDsBySeries", withFile, fileless)

		withFile, fileless, err = repo.PartitionIDsBySeason(ctx, seasonOneID)
		if err != nil {
			t.Fatal(err)
		}
		assertPartition(t, "PartitionIDsBySeason", withFile, fileless)

		withFile, fileless, err = repo.PartitionIDsBySeriesSeason(ctx, seriesID, 1)
		if err != nil {
			t.Fatal(err)
		}
		assertPartition(t, "PartitionIDsBySeriesSeason", withFile, fileless)
	})

	t.Run("ListAdjacentInSeries", func(t *testing.T) {
		episodes, err := repo.ListAdjacentInSeries(ctx, seriesID, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		assertEpisodeIDs(t, "ListAdjacentInSeries", episodes, canonicalOnly)
	})

	t.Run("ListAvailableIDsBySeriesPage", func(t *testing.T) {
		ids, err := repo.ListAvailableIDsBySeriesPage(ctx, seriesID, 10, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(ids, canonicalOnly) {
			t.Fatalf("ListAvailableIDsBySeriesPage = %v, want %v", ids, canonicalOnly)
		}
	})

	t.Run("CountBySeries", func(t *testing.T) {
		count, err := repo.CountBySeries(ctx, seriesID)
		if err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("CountBySeries = %d, want 1", count)
		}
	})

	t.Run("ListBySeriesAndAirDates", func(t *testing.T) {
		grouped, err := repo.ListBySeriesAndAirDates(ctx, seriesID, []string{"2024-03-07"})
		if err != nil {
			t.Fatal(err)
		}
		assertEpisodeIDs(t, "ListBySeriesAndAirDates", grouped["2024-03-07"], canonicalOnly)
	})

	t.Run("ListUpcoming", func(t *testing.T) {
		episodes, total, err := repo.ListUpcoming(ctx, time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), seriesID, "", 0, 10, 0, AccessFilter{}, true)
		if err != nil {
			t.Fatal(err)
		}
		assertEpisodeIDs(t, "ListUpcoming", episodes, canonicalOnly)
		if total != 1 {
			t.Fatalf("ListUpcoming total = %d, want 1", total)
		}
	})

	t.Run("BrowseEpisodes", func(t *testing.T) {
		episodes, total, err := repo.BrowseEpisodes(ctx, seriesID, "", nil, "", BrowseFilters{Limit: 10}, AccessFilter{}, true)
		if err != nil {
			t.Fatal(err)
		}
		assertEpisodeIDs(t, "BrowseEpisodes", episodes, canonicalOnly)
		if total != 1 {
			t.Fatalf("BrowseEpisodes total = %d, want 1", total)
		}
	})
}
