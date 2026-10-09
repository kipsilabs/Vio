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

// insertProviderID attaches one media_item_provider_ids row to an item.
func insertProviderID(t *testing.T, f *providerAliasFixture, contentID, itemType, provider, providerID string) {
	t.Helper()
	if _, err := f.pool.Exec(t.Context(), `
		INSERT INTO media_item_provider_ids (content_id, item_type, provider, provider_id)
		VALUES ($1, $2, $3, $4)`, contentID, itemType, provider, providerID); err != nil {
		t.Fatal(err)
	}
}

// TestGetByExternalIDsProviderSlugAndWrongTypeDB pins the provider table as a
// first-class identity source: a provider_id stored in TMDB's "id-slug" URL
// form resolves against the bare numeric id a caller holds, and a provider row
// typed for one media type never answers a lookup of another. Both are
// provider-side cases the SQL-shape tests cannot exercise.
func TestGetByExternalIDsProviderSlugAndWrongTypeDB(t *testing.T) {
	f := newProviderAliasFixture(t)
	ctx := t.Context()
	base := uniqueReleaseSuffix(t)

	// Provider-table-only TMDB id stored in slug form; the media_items column
	// stays empty so only the provider path can find it.
	slugBase := base + "31"
	slugItem := f.item(t, "prov-slug", "movie", "", "", "", f.enabled)
	insertProviderID(t, f, slugItem, "movie", "tmdb", slugBase+"-some-title")

	// A series-typed provider row must not answer a movie lookup, but does
	// answer a series lookup.
	wrongBase := base + "41"
	wrongItem := f.item(t, "prov-wrongtype", "series", "", "", "", f.enabled)
	insertProviderID(t, f, wrongItem, "series", "tmdb", wrongBase)

	got, err := f.repo.GetByExternalIDs(ctx, ExternalIDBatch{TMDBIDs: []string{slugBase}}, "movie")
	if err != nil {
		t.Fatalf("provider-slug movie lookup: %v", err)
	}
	if got.ByTMDB[slugBase] != slugItem {
		t.Errorf("provider slug ByTMDB[%q] = %q, want %q", slugBase, got.ByTMDB[slugBase], slugItem)
	}

	miss, err := f.repo.GetByExternalIDs(ctx, ExternalIDBatch{TMDBIDs: []string{wrongBase}}, "movie")
	if err != nil {
		t.Fatalf("provider wrong-type movie lookup: %v", err)
	}
	if len(miss.ByTMDB) != 0 {
		t.Fatalf("series-typed provider row leaked into movie lookup: %+v", miss)
	}

	hit, err := f.repo.GetByExternalIDs(ctx, ExternalIDBatch{TMDBIDs: []string{wrongBase}}, "series")
	if err != nil {
		t.Fatalf("series lookup by provider id: %v", err)
	}
	if hit.ByTMDB[wrongBase] != wrongItem {
		t.Errorf("series ByTMDB[%q] = %q, want %q", wrongBase, hit.ByTMDB[wrongBase], wrongItem)
	}
}

// TestGetByExternalIDsNumericBoundaryDB pins that an id is matched whole: the
// numeric prefix of one id must not answer a longer id, and a longer id must
// not answer the shorter one, on both the column and provider paths and across
// the "id" vs "id-slug" forms.
func TestGetByExternalIDsNumericBoundaryDB(t *testing.T) {
	f := newProviderAliasFixture(t)
	ctx := t.Context()
	base := uniqueReleaseSuffix(t)

	shortCol := base + "51"
	shortID := f.item(t, "bound-short", "movie", shortCol, "", "", f.enabled)
	longCol := base + "510"
	longID := f.item(t, "bound-long", "movie", longCol, "", "", f.enabled)
	slugCol := base + "52"
	slugID := f.item(t, "bound-slug", "movie", slugCol+"-some-title", "", "", f.enabled)
	slugLongCol := base + "520"
	slugLongID := f.item(t, "bound-slug-long", "movie", slugLongCol, "", "", f.enabled)

	provShortID := f.item(t, "bound-prov-short", "movie", "", "", "", f.enabled)
	insertProviderID(t, f, provShortID, "movie", "tmdb", base+"61-some-title")
	provLongID := f.item(t, "bound-prov-long", "movie", "", "", "", f.enabled)
	insertProviderID(t, f, provLongID, "movie", "tmdb", base+"610-some-title")

	// The bare short id answers only its own item; "510" is a different id.
	short, err := f.repo.GetByExternalIDs(ctx, ExternalIDBatch{TMDBIDs: []string{shortCol}}, "movie")
	if err != nil {
		t.Fatalf("short lookup: %v", err)
	}
	if short.ByTMDB[shortCol] != shortID || len(short.ByTMDB) != 1 {
		t.Fatalf("short id resolved to %+v, want only %q", short.ByTMDB, shortID)
	}

	// The longer id answers only its own item; it must not pick up "51".
	long, err := f.repo.GetByExternalIDs(ctx, ExternalIDBatch{TMDBIDs: []string{longCol}}, "movie")
	if err != nil {
		t.Fatalf("long lookup: %v", err)
	}
	if long.ByTMDB[longCol] != longID || len(long.ByTMDB) != 1 {
		t.Fatalf("long id resolved to %+v, want only %q", long.ByTMDB, longID)
	}

	// A slug whose base is "52" resolves for "52" but not for "520".
	slug, err := f.repo.GetByExternalIDs(ctx, ExternalIDBatch{TMDBIDs: []string{slugCol}}, "movie")
	if err != nil {
		t.Fatalf("slug lookup: %v", err)
	}
	if slug.ByTMDB[slugCol] != slugID || len(slug.ByTMDB) != 1 {
		t.Fatalf("slug base resolved to %+v, want only %q", slug.ByTMDB, slugID)
	}
	slugLong, err := f.repo.GetByExternalIDs(ctx, ExternalIDBatch{TMDBIDs: []string{slugLongCol}}, "movie")
	if err != nil {
		t.Fatalf("slug-long lookup: %v", err)
	}
	if slugLong.ByTMDB[slugLongCol] != slugLongID || len(slugLong.ByTMDB) != 1 {
		t.Fatalf("longer slug base resolved to %+v, want only %q", slugLong.ByTMDB, slugLongID)
	}

	// Provider-side boundary: "61" answers only the "61-" slug, never "610-".
	provShort, err := f.repo.GetByExternalIDs(ctx, ExternalIDBatch{TMDBIDs: []string{base + "61"}}, "movie")
	if err != nil {
		t.Fatalf("provider short lookup: %v", err)
	}
	if provShort.ByTMDB[base+"61"] != provShortID || len(provShort.ByTMDB) != 1 {
		t.Fatalf("provider slug base resolved to %+v, want only %q", provShort.ByTMDB, provShortID)
	}
	provLong, err := f.repo.GetByExternalIDs(ctx, ExternalIDBatch{TMDBIDs: []string{base + "610"}}, "movie")
	if err != nil {
		t.Fatalf("provider long lookup: %v", err)
	}
	if provLong.ByTMDB[base+"610"] != provLongID || len(provLong.ByTMDB) != 1 {
		t.Fatalf("provider longer slug base resolved to %+v, want only %q", provLong.ByTMDB, provLongID)
	}
}
