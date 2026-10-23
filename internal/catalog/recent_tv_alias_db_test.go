package catalog

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestRecentTVAliasMembershipDB pins canonical identity in the recently-added
// TV reads. A canonical episode with no direct membership or file arrives
// through its alias row, an alias row never becomes its own card, and the
// canonical episode's own membership plus its alias's membership collapse into
// one arrival per folder.
func TestRecentTVAliasMembershipDB(t *testing.T) { //nolint:funlen // one DB fixture covers list, prefix, and newest queries
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

	prefix := fmt.Sprintf("recent-tv-alias-%d", time.Now().UnixNano())
	seriesA, seriesB := prefix+"-series-a", prefix+"-series-b"
	canonicalA1, aliasA1 := prefix+"-canonical-a1", prefix+"-alias-a1"
	canonicalA2, aliasA2 := prefix+"-canonical-a2", prefix+"-alias-a2"
	canonicalB1, aliasB1 := prefix+"-canonical-b1", prefix+"-alias-b1"

	var folderID int
	if err := pool.QueryRow(ctx,
		`INSERT INTO media_folders (type, name, enabled) VALUES ('series', $1, true) RETURNING id`,
		prefix,
	).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM episode_aliases WHERE series_id = ANY($1)`, []string{seriesA, seriesB})
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM media_files WHERE media_folder_id = $1`, folderID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM episode_libraries WHERE media_folder_id = $1`, folderID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM episodes WHERE series_id = ANY($1)`, []string{seriesA, seriesB})
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM media_item_libraries WHERE media_folder_id = $1`, folderID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM media_items WHERE content_id = ANY($1)`, []string{seriesA, seriesB})
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM media_folders WHERE id = $1`, folderID)
	})

	base := time.Date(2026, 8, 7, 8, 0, 0, 0, time.UTC)
	arrivalA1 := base
	arrivalA2 := base.Add(10 * time.Minute)
	// A1's alias membership arrives long after A1's canonical membership, so
	// folding it in must not create a second event for the same folder.
	arrivalAliasA1 := base.Add(5 * time.Hour)
	arrivalB := base.Add(10 * time.Hour)

	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO media_items (content_id, type, title) VALUES
				($1, 'series', 'Event Show'), ($2, 'series', 'Single Show')`,
			[]any{seriesA, seriesB}},
		{`INSERT INTO media_item_libraries (content_id, media_folder_id, first_seen_at) VALUES
				($1, $3, $4), ($2, $3, $5)`,
			[]any{seriesA, seriesB, folderID, arrivalA1, arrivalB}},
		{`INSERT INTO episodes (content_id, series_id, season_number, episode_number, title) VALUES
				($1, $3, 1, 1, 'Canonical A1'), ($2, $3, 2, 1, 'Alias A1'),
				($4, $3, 1, 2, 'Canonical A2'), ($5, $3, 2, 2, 'Alias A2'),
				($6, $7, 1, 1, 'Canonical B1'), ($8, $7, 2, 1, 'Alias B1')`,
			[]any{canonicalA1, aliasA1, seriesA, canonicalA2, aliasA2, canonicalB1, seriesB, aliasB1}},
		{`INSERT INTO episode_aliases (
				series_id, canonical_episode_id, alias_episode_id,
				provider_installation_id, provider_series_id,
				provider_season_number, provider_episode_number, source
			) VALUES
				($1, $2, $3, 0, '', 2, 1, 'absolute_layout'),
				($1, $4, $5, 0, '', 2, 2, 'absolute_layout'),
				($6, $7, $8, 0, '', 2, 1, 'absolute_layout')`,
			[]any{seriesA, canonicalA1, aliasA1, canonicalA2, aliasA2, seriesB, canonicalB1, aliasB1}},
		// A1 has a direct membership and file; A2 and B1 exist only through
		// their alias rows.
		{`INSERT INTO episode_libraries (episode_id, media_folder_id, first_seen_at) VALUES
				($1, $5, $6), ($2, $5, $7), ($3, $5, $8), ($4, $5, $9)`,
			[]any{canonicalA1, aliasA1, aliasA2, aliasB1, folderID, arrivalA1, arrivalAliasA1, arrivalA2, arrivalB}},
		{`INSERT INTO media_files (content_id, episode_id, media_folder_id, file_path, file_size, container) VALUES
				($1, $2, $5, '/recent-tv-alias/a1-canonical.mkv', 1, 'mkv'),
				($1, $3, $5, '/recent-tv-alias/a1-alias.mkv', 1, 'mkv'),
				($1, $4, $5, '/recent-tv-alias/a2-alias.mkv', 1, 'mkv'),
				($6, $7, $5, '/recent-tv-alias/b1-alias.mkv', 1, 'mkv')`,
			[]any{seriesA, canonicalA1, aliasA1, aliasA2, folderID, seriesB, aliasB1}},
	} {
		if _, err := pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("seed %q: %v", statement.sql, err)
		}
	}

	repo := NewRecentTVRepository(pool)
	assertTargets := func(t *testing.T, label string, targets []RecentTVTarget, want []RecentTVTarget) {
		t.Helper()
		if len(targets) != len(want) {
			t.Fatalf("%s targets = %+v, want %+v", label, targets, want)
		}
		for i := range want {
			got := targets[i]
			if got.ContentID != want[i].ContentID ||
				got.EventID != want[i].EventID ||
				got.Type != want[i].Type ||
				!got.AddedAt.Equal(want[i].AddedAt) ||
				got.PlayContentID != want[i].PlayContentID {
				t.Fatalf("%s target %d = %+v, want %+v", label, i, got, want[i])
			}
		}
	}

	eventID := func(at time.Time) string {
		return at.UTC().Format("2006-01-02T15:04:05.000000Z")
	}
	want := []RecentTVTarget{
		// B1 is a single-episode event, so the card targets the canonical id
		// even though only the alias row owns its membership and file.
		{ContentID: canonicalB1, EventID: eventID(arrivalB), Type: "episode", AddedAt: arrivalB, PlayContentID: canonicalB1},
		// A's two canonical episodes stay one series event; the alias
		// membership for A1 folds into A1's earlier arrival instead of
		// starting a second event.
		{ContentID: seriesA, EventID: eventID(arrivalA1), Type: "series", AddedAt: arrivalA2, PlayContentID: canonicalA1},
	}

	t.Run("List", func(t *testing.T) {
		targets, total, hasMore, err := repo.List(ctx, RecentTVQuery{LibraryIDs: []int{folderID}, Limit: 20})
		if err != nil {
			t.Fatalf("list recent TV: %v", err)
		}
		assertTargets(t, "List", targets, want)
		if total != len(want) || hasMore {
			t.Fatalf("List total = %d hasMore = %v, want %d and false", total, hasMore, len(want))
		}
	})

	t.Run("ListNamePrefix", func(t *testing.T) {
		targets, total, _, err := repo.List(ctx, RecentTVQuery{
			LibraryIDs: []int{folderID}, NamePrefix: "Single", Limit: 20,
		})
		if err != nil {
			t.Fatalf("list recent TV with prefix: %v", err)
		}
		assertTargets(t, "ListNamePrefix", targets, want[:1])
		if total != 1 {
			t.Fatalf("ListNamePrefix total = %d, want 1", total)
		}
	})

	t.Run("ListNewest", func(t *testing.T) {
		targets, hasMore, err := repo.ListNewest(ctx, RecentTVQuery{LibraryIDs: []int{folderID}, Limit: 20})
		if err != nil {
			t.Fatalf("list newest recent TV: %v", err)
		}
		assertTargets(t, "ListNewest", targets, want)
		if hasMore {
			t.Fatal("ListNewest hasMore = true, want false")
		}
	})
}
