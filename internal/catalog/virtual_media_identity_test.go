package catalog

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"
)

func TestVirtualMediaContentIDPrefersThePinnedIdentity(t *testing.T) {
	withoutPin := VirtualMedia{MediaType: "series", TMDBID: "123", TVDBID: "456"}
	if got := virtualMediaContentID(withoutPin); got != "series-tvdb-456" {
		t.Fatalf("content id = %q, want the derived identity", got)
	}
	withPin := withoutPin
	withPin.ContentID = "series-tmdb-123"
	if got := virtualMediaContentID(withPin); got != "series-tmdb-123" {
		t.Fatalf("content id = %q, want the pinned identity", got)
	}
}

// TestResolveVirtualMediaContentIDFindsTheExistingItem covers the alias
// lookup: a title that was registered under its TMDB id is found again when
// the registration carries an additional provider id, so the item keeps its
// identity instead of being registered a second time.
func TestResolveVirtualMediaContentIDFindsTheExistingItem(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	contentID := fmt.Sprintf("identity-series-%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items (content_id, type, title, tmdb_id, tvdb_id, status)
		VALUES ($1, 'series', 'Identity Series', '123', '456', 'matched')`, contentID); err != nil {
		t.Fatalf("seed media item: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_items WHERE content_id = $1`, contentID)
	})

	reg := NewVirtualMediaRegistrar(pool)

	got, err := reg.ResolveVirtualMediaContentID(ctx, "series", "123", "", "")
	if err != nil {
		t.Fatalf("resolve identity: %v", err)
	}
	if got != contentID {
		t.Fatalf("resolved identity = %q, want %q", got, contentID)
	}

	got, err = reg.ResolveVirtualMediaContentID(ctx, "series", "", "456", "")
	if err != nil {
		t.Fatalf("resolve identity: %v", err)
	}
	if got != contentID {
		t.Fatalf("resolved identity = %q, want %q", got, contentID)
	}

	// An id the catalog does not know resolves to nothing.
	got, err = reg.ResolveVirtualMediaContentID(ctx, "series", "999", "", "")
	if err != nil {
		t.Fatalf("resolve identity: %v", err)
	}
	if got != "" {
		t.Fatalf("resolved identity = %q, want none", got)
	}

	// The media type is part of the lookup: a movie id does not resolve the
	// series row.
	got, err = reg.ResolveVirtualMediaContentID(ctx, "movie", "123", "", "")
	if err != nil {
		t.Fatalf("resolve identity: %v", err)
	}
	if got != "" {
		t.Fatalf("resolved identity = %q, want none", got)
	}
}

// TestVirtualMediaRegistrationKeepsThePinnedIdentity covers the registration
// path: a series that was registered under its TMDB id keeps that identity
// when a later registration carries the TVDB id as well. The provider ids of
// the later registration are aliases of the item, not a new item.
func TestVirtualMediaRegistrationKeepsThePinnedIdentity(t *testing.T) {
	pool := newVirtualMediaTestPool(t)
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	var folderID int
	if err := pool.QueryRow(ctx,
		`INSERT INTO media_folders (type, name) VALUES ('series', $1) RETURNING id`,
		fmt.Sprintf("Identity Series %d", suffix),
	).Scan(&folderID); err != nil {
		t.Fatalf("seed media folder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_items WHERE title = $1`, "Identity Series")
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_folders WHERE id = $1`, folderID)
	})

	reg := NewVirtualMediaRegistrar(pool)
	first := VirtualMedia{
		LibraryID: strconv.Itoa(folderID), MediaType: "series", Title: "Identity Series",
		TMDBID: "123", Source: "identity-test",
		Episodes: []VirtualEpisode{{SeasonNumber: 1, EpisodeNumber: 1, Title: "Pilot", AirDate: time.Now().Add(-24 * time.Hour)}},
	}
	if _, err := reg.Upsert(ctx, first); err != nil {
		t.Fatalf("register series: %v", err)
	}

	// The catalog now holds the series under its TMDB id.
	resolved, err := reg.ResolveVirtualMediaContentID(ctx, "series", "123", "", "")
	if err != nil {
		t.Fatalf("resolve identity: %v", err)
	}
	if resolved != "series-tmdb-123" {
		t.Fatalf("resolved identity = %q, want series-tmdb-123", resolved)
	}

	// A later registration carries the TVDB id as well. The identity is
	// pinned to the row that already exists, so the enriched ids are
	// recorded as aliases of it.
	second := first
	second.TVDBID = "456"
	second.ContentID = resolved
	if _, err := reg.Upsert(ctx, second); err != nil {
		t.Fatalf("register enriched series: %v", err)
	}

	var rows int
	var storedTMDB, storedTVDB string
	if err := pool.QueryRow(ctx,
		`SELECT count(*), COALESCE(max(tmdb_id), ''), COALESCE(max(tvdb_id), '') FROM media_items WHERE title = $1`,
		"Identity Series",
	).Scan(&rows, &storedTMDB, &storedTVDB); err != nil {
		t.Fatalf("read media items: %v", err)
	}
	if rows != 1 {
		t.Fatalf("media items = %d, want 1: the enriched registration split the identity", rows)
	}
	if storedTMDB != "123" || storedTVDB != "456" {
		t.Fatalf("stored ids = tmdb %q tvdb %q, want tmdb 123 and tvdb 456", storedTMDB, storedTVDB)
	}
}
