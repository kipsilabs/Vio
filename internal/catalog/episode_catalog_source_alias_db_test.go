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

// TestEpisodeCatalogBaseRelationAliasDB executes the episode catalog base
// relation against an alias fixture. A canonical episode with no direct
// episode_libraries row and no file must hydrate through its alias row's
// membership, while the alias row itself must never hydrate. The scoped
// relations must apply the same rule to allowed and disabled libraries.
func TestEpisodeCatalogBaseRelationAliasDB(t *testing.T) { //nolint:funlen // one DB fixture covers base and scoped relations
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

	prefix := fmt.Sprintf("episode-catalog-alias-%d", time.Now().UnixNano())
	seriesOne, seriesTwo := prefix+"-series-1", prefix+"-series-2"
	canonicalOne, aliasOne := prefix+"-canonical-1", prefix+"-alias-1"
	canonicalTwo := prefix + "-canonical-2"
	canonicalThree, aliasThree := prefix+"-canonical-3", prefix+"-alias-3"

	var folderOne, folderTwo int
	for _, id := range []*int{&folderOne, &folderTwo} {
		if err := pool.QueryRow(ctx,
			`INSERT INTO media_folders (type, name, enabled) VALUES ('series', $1, true) RETURNING id`,
			prefix,
		).Scan(id); err != nil {
			t.Fatalf("seed folder: %v", err)
		}
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM episode_aliases WHERE series_id = ANY($1)`, []string{seriesOne, seriesTwo})
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM episode_libraries WHERE media_folder_id = ANY($1)`, []int{folderOne, folderTwo})
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM episodes WHERE series_id = ANY($1)`, []string{seriesOne, seriesTwo})
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM media_item_libraries WHERE media_folder_id = ANY($1)`, []int{folderOne, folderTwo})
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM media_items WHERE content_id = ANY($1)`, []string{seriesOne, seriesTwo})
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM media_folders WHERE id = ANY($1)`, []int{folderOne, folderTwo})
	})

	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO media_items (content_id, type, title) VALUES
				($1, 'series', 'Alias Catalog One'), ($2, 'series', 'Alias Catalog Two')`,
			[]any{seriesOne, seriesTwo}},
		// Series one is visible in either folder; series two only in folder
		// one, so a folder-two disabled scope exercises the episode-level
		// alias exclusion rather than the series-level one.
		{`INSERT INTO media_item_libraries (content_id, media_folder_id) VALUES
				($1, $3), ($1, $4), ($2, $3)`,
			[]any{seriesOne, seriesTwo, folderOne, folderTwo}},
		{`INSERT INTO episodes (content_id, series_id, season_number, episode_number, title) VALUES
				($1, $5, 1, 1, 'Canonical One'), ($2, $5, 2, 1, 'Alias One'),
				($3, $6, 1, 2, 'Canonical Two'),
				($4, $6, 1, 1, 'Canonical Three'), ($7, $6, 2, 1, 'Alias Three')`,
			[]any{canonicalOne, aliasOne, canonicalTwo, canonicalThree, seriesOne, seriesTwo, aliasThree}},
		{`INSERT INTO episode_aliases (
				series_id, canonical_episode_id, alias_episode_id,
				provider_installation_id, provider_series_id,
				provider_season_number, provider_episode_number, source
			) VALUES
				($1, $2, $3, 0, '', 2, 1, 'absolute_layout'),
				($4, $5, $6, 0, '', 2, 1, 'absolute_layout')`,
			[]any{seriesOne, canonicalOne, aliasOne, seriesTwo, canonicalThree, aliasThree}},
		// Canonical one and three have no direct membership; only their alias
		// rows own one, and no media file exists anywhere.
		{`INSERT INTO episode_libraries (episode_id, media_folder_id) VALUES
				($1, $5), ($2, $4), ($3, $5)`,
			[]any{aliasOne, canonicalTwo, aliasThree, folderOne, folderTwo}},
	} {
		if _, err := pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("seed %q: %v", statement.sql, err)
		}
	}

	queryIDs := func(t *testing.T, relation string, args ...any) []string {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT mi.content_id FROM `+relation+` ORDER BY mi.content_id`, args...)
		if err != nil {
			t.Fatalf("query episode catalog relation: %v", err)
		}
		defer rows.Close()
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan episode catalog id: %v", err)
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate episode catalog ids: %v", err)
		}
		return ids
	}

	t.Run("base relation", func(t *testing.T) {
		got := queryIDs(t, episodeCatalogBaseRelation)
		want := sortedStrings(canonicalOne, canonicalTwo, canonicalThree)
		if !slices.Equal(got, want) {
			t.Fatalf("base relation ids = %v, want %v", got, want)
		}
	})

	t.Run("allowed library follows alias membership", func(t *testing.T) {
		relation, args, handled := episodeCatalogBaseRelationForLibraries([]int{folderTwo}, nil, 1)
		if !handled {
			t.Fatal("expected episode library scope to be handled")
		}
		got := queryIDs(t, relation, args...)
		if want := []string{canonicalOne}; !slices.Equal(got, want) {
			t.Fatalf("folder-two allowed ids = %v, want %v", got, want)
		}
	})

	t.Run("allowed library keeps direct membership", func(t *testing.T) {
		relation, args, handled := episodeCatalogBaseRelationForLibraries([]int{folderOne}, nil, 1)
		if !handled {
			t.Fatal("expected episode library scope to be handled")
		}
		got := queryIDs(t, relation, args...)
		if want := []string{canonicalTwo}; !slices.Equal(got, want) {
			t.Fatalf("folder-one allowed ids = %v, want %v", got, want)
		}
	})

	t.Run("disabled library rejects alias membership", func(t *testing.T) {
		relation, args, handled := episodeCatalogBaseRelationForLibraries(nil, []int{folderTwo}, 1)
		if !handled {
			t.Fatal("expected disabled episode library scope to be handled")
		}
		got := queryIDs(t, relation, args...)
		if want := []string{canonicalTwo}; !slices.Equal(got, want) {
			t.Fatalf("folder-two disabled ids = %v, want %v", got, want)
		}
	})
}

func sortedStrings(values ...string) []string {
	slices.Sort(values)
	return values
}
