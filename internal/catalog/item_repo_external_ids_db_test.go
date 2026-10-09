package catalog

import (
	"testing"
)

// TestGetByExternalIDsMatchesProviderTableAndSlugTMDBDB pins the matcher's
// identity sources against a real catalog. It must see ids held only in
// media_item_provider_ids, and must resolve TMDB's "id-slug" URL form in
// media_items.tmdb_id against the bare numeric id a caller holds, the same
// identity the rest of the catalog uses. Without those sources a TMDB-list
// collection import matches zero entries for affected titles (kipsilabs/Vio#255).
// Reuses the provider-alias fixture, which isolates rows by a per-run prefix and
// cascades provider rows on cleanup.
func TestGetByExternalIDsMatchesProviderTableAndSlugTMDBDB(t *testing.T) {
	f := newProviderAliasFixture(t)
	ctx := t.Context()

	// A per-run numeric suffix keeps fixtures collision-free and distinct from
	// the fixed ids other suites seed.
	base := uniqueReleaseSuffix(t)
	tmdbProviderOnly := base + "11"
	tmdbSlug := base + "12"
	tmdbColumn := base + "13"
	imdbColumn := "tt" + base + "14"
	tvdbColumn := base + "15"
	imdbProviderOnly := "tt" + base + "16"
	tvdbProviderOnly := base + "17"

	// Provider-table-only TMDB id: the media_items column stays empty.
	providerOnly := f.item(t, "prov-tmdb", "movie", "", "", "", f.enabled)
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO media_item_provider_ids (content_id, item_type, provider, provider_id)
		VALUES ($1, 'movie', 'tmdb', $2)`, providerOnly, tmdbProviderOnly); err != nil {
		t.Fatal(err)
	}
	// Slug-form column id: TMDB's own URL form ("1931-some-title").
	slugColumn := f.item(t, "slug-col", "movie", tmdbSlug+"-some-title", "", "", f.enabled)
	// Columns-only matches across all three providers.
	colTMDB := f.item(t, "col-tmdb", "movie", tmdbColumn, "", "", f.enabled)
	colIMDb := f.item(t, "col-imdb", "movie", "", imdbColumn, "", f.enabled)
	colTVDB := f.item(t, "col-tvdb", "series", "", "", tvdbColumn, f.enabled)
	// Provider-table-only IMDb and TVDB ids.
	provIMDb := f.item(t, "prov-imdb", "movie", "", "", "", f.enabled)
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO media_item_provider_ids (content_id, item_type, provider, provider_id)
		VALUES ($1, 'movie', 'imdb', $2)`, provIMDb, imdbProviderOnly); err != nil {
		t.Fatal(err)
	}
	provTVDB := f.item(t, "prov-tvdb", "series", "", "", "", f.enabled)
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO media_item_provider_ids (content_id, item_type, provider, provider_id)
		VALUES ($1, 'series', 'tvdb', $2)`, provTVDB, tvdbProviderOnly); err != nil {
		t.Fatal(err)
	}

	movie, err := f.repo.GetByExternalIDs(ctx, ExternalIDBatch{
		TMDBIDs: []string{tmdbProviderOnly, tmdbSlug, tmdbColumn},
		IMDbIDs: []string{imdbColumn, imdbProviderOnly},
	}, "movie")
	if err != nil {
		t.Fatalf("movie lookup: %v", err)
	}
	wantTMDB := map[string]string{
		tmdbProviderOnly: providerOnly,
		tmdbSlug:         slugColumn,
		tmdbColumn:       colTMDB,
	}
	for id, contentID := range wantTMDB {
		if got := movie.ByTMDB[id]; got != contentID {
			t.Errorf("ByTMDB[%q] = %q, want %q", id, got, contentID)
		}
	}
	wantIMDb := map[string]string{imdbColumn: colIMDb, imdbProviderOnly: provIMDb}
	for id, contentID := range wantIMDb {
		if got := movie.ByIMDb[id]; got != contentID {
			t.Errorf("ByIMDb[%q] = %q, want %q", id, got, contentID)
		}
	}

	series, err := f.repo.GetByExternalIDs(ctx, ExternalIDBatch{
		TVDBIDs: []string{tvdbColumn, tvdbProviderOnly},
	}, "series")
	if err != nil {
		t.Fatalf("series lookup: %v", err)
	}
	for id, contentID := range map[string]string{tvdbColumn: colTVDB, tvdbProviderOnly: provTVDB} {
		if got := series.ByTVDB[id]; got != contentID {
			t.Errorf("ByTVDB[%q] = %q, want %q", id, got, contentID)
		}
	}
}

// TestGetByExternalIDsStillMissesUnknownAndWrongTypeDB pins exact-match
// semantics: an id no item carries resolves to nothing, and a known id does not
// leak across the media-type filter.
func TestGetByExternalIDsStillMissesUnknownAndWrongTypeDB(t *testing.T) {
	f := newProviderAliasFixture(t)
	ctx := t.Context()
	base := uniqueReleaseSuffix(t)

	tmdbColumn := base + "21"
	movie := f.item(t, "miss-tmdb", "movie", tmdbColumn, "", "", f.enabled)
	if movie == "" {
		t.Fatal("fixture item not created")
	}

	miss, err := f.repo.GetByExternalIDs(ctx, ExternalIDBatch{
		TMDBIDs: []string{base + "99"},
		IMDbIDs: []string{"tt" + base + "98"},
		TVDBIDs: []string{base + "97"},
	}, "movie")
	if err != nil {
		t.Fatalf("miss lookup: %v", err)
	}
	if len(miss.ByTMDB) != 0 || len(miss.ByIMDb) != 0 || len(miss.ByTVDB) != 0 {
		t.Fatalf("unknown ids resolved to %+v", miss)
	}

	// A movie's TMDB id must not match a series lookup.
	wrongType, err := f.repo.GetByExternalIDs(ctx, ExternalIDBatch{
		TMDBIDs: []string{tmdbColumn},
	}, "series")
	if err != nil {
		t.Fatalf("wrong-type lookup: %v", err)
	}
	if len(wrongType.ByTMDB) != 0 {
		t.Fatalf("movie id leaked into series lookup: %+v", wrongType)
	}
}
