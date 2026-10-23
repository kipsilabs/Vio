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

// TestEpisodeCatalogEntriesCanonicalDB pins the canonical-by-construction rule
// for episode_catalog_entries: membership and file statistics owned by an alias
// episode produce exactly one entry keyed by the canonical episode.
func TestEpisodeCatalogEntriesCanonicalDB(t *testing.T) { //nolint:funlen // one DB fixture covers the canonical entry lifecycle
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

	prefix := fmt.Sprintf("ece-canonical-%d", time.Now().UnixNano())
	seriesID := prefix + "-series"
	canonicalID := prefix + "-canonical"
	aliasID := prefix + "-alias"
	addedAt := time.Date(2024, 4, 10, 12, 0, 0, 0, time.UTC)

	var folderID int
	if err := pool.QueryRow(ctx,
		`INSERT INTO media_folders (type, name, enabled) VALUES ('series', $1, true) RETURNING id`,
		prefix,
	).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_files WHERE content_id = $1`, seriesID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM episodes WHERE series_id = $1`, seriesID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_items WHERE content_id = $1`, seriesID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_folders WHERE id = $1`, folderID)
	})

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	exec(`INSERT INTO media_items (content_id, type, title) VALUES ($1, 'series', 'Canonical ECE Fixture')`, seriesID)
	exec(`INSERT INTO episodes (content_id, series_id, season_number, episode_number, title, air_date)
		VALUES ($1, $3, 1, 1, 'Canonical episode', '2024-04-10'),
		       ($2, $3, 2, 1, 'Alias episode', '2024-04-10')`,
		canonicalID, aliasID, seriesID)

	// The canonical episode owns no membership and no file. The alias owns both.
	exec(`INSERT INTO episode_libraries (episode_id, media_folder_id, first_seen_at)
		VALUES ($1, $2, $3)`, aliasID, folderID, addedAt)
	exec(`INSERT INTO media_files (
			content_id, episode_id, media_folder_id, file_path, file_size, container,
			resolution, bitrate, hdr, video_tracks, audio_tracks, subtitle_tracks
		) VALUES ($1, $2, $3, $4, 1, 'mkv', '1080p', 5000, true,
			'[{"dolby_vision":"dvhe.05"}]'::jsonb,
			'[{"language":"eng"}]'::jsonb,
			'[{"language":"spa"}]'::jsonb)`,
		seriesID, aliasID, folderID, prefix+"-alias.mkv")

	// Insert the mapping last, as reconciliation does, so the alias trigger has
	// to move the entry from the alias id to the canonical id.
	exec(`INSERT INTO episode_aliases (
			series_id, canonical_episode_id, alias_episode_id,
			provider_installation_id, provider_series_id,
			provider_season_number, provider_episode_number, source
		) VALUES ($1, $2, $3, 0, '', 2, 1, 'absolute_layout')`,
		seriesID, canonicalID, aliasID)

	fetchEntries := func(t *testing.T) []eceRow {
		t.Helper()
		rows, err := pool.Query(ctx, `
			SELECT episode_id, series_id, added_at,
			       COALESCE(max_resolution_rank, 0), resolution_codes,
			       COALESCE(max_bitrate, 0), COALESCE(min_bitrate, 0),
			       has_hdr, has_non_hdr, has_dolby_vision, has_non_dolby_vision,
			       audio_language_codes, subtitle_language_codes
			FROM episode_catalog_entries
			WHERE media_folder_id = $1
			ORDER BY episode_id`, folderID)
		if err != nil {
			t.Fatalf("query episode_catalog_entries: %v", err)
		}
		defer rows.Close()

		var entries []eceRow
		for rows.Next() {
			var entry eceRow
			if err := rows.Scan(
				&entry.episodeID, &entry.seriesID, &entry.addedAt,
				&entry.maxResolutionRank, &entry.resolutionCodes,
				&entry.maxBitrate, &entry.minBitrate,
				&entry.hasHDR, &entry.hasNonHDR, &entry.hasDolbyVision, &entry.hasNonDolbyVision,
				&entry.audioLanguageCodes, &entry.subtitleLanguageCodes,
			); err != nil {
				t.Fatalf("scan episode_catalog_entries: %v", err)
			}
			entries = append(entries, entry)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate episode_catalog_entries: %v", err)
		}
		return entries
	}

	entries := fetchEntries(t)
	if len(entries) != 1 {
		t.Fatalf("episode_catalog_entries rows = %d, want 1: %+v", len(entries), entries)
	}
	got := entries[0]
	if got.episodeID != canonicalID {
		t.Fatalf("entry episode_id = %q, want canonical %q", got.episodeID, canonicalID)
	}
	if got.seriesID != seriesID {
		t.Fatalf("entry series_id = %q, want %q", got.seriesID, seriesID)
	}
	if !got.addedAt.Equal(addedAt) {
		t.Fatalf("entry added_at = %s, want alias membership %s", got.addedAt, addedAt)
	}
	assertECEStats(t, got, eceStats{
		maxResolutionRank:     3,
		resolutionCodes:       []string{"1080p"},
		maxBitrate:            5000,
		minBitrate:            5000,
		hasHDR:                true,
		hasNonHDR:             false,
		hasDolbyVision:        true,
		hasNonDolbyVision:     false,
		audioLanguageCodes:    []string{"en"},
		subtitleLanguageCodes: []string{"es"},
	})

	// A direct canonical file is aggregated with the alias file.
	exec(`INSERT INTO media_files (
			content_id, episode_id, media_folder_id, file_path, file_size, container,
			resolution, bitrate, hdr, video_tracks, audio_tracks, subtitle_tracks
		) VALUES ($1, $2, $3, $4, 2, 'mkv', '4K', 12000, false,
			'[]'::jsonb,
			'[{"language":"fra"}]'::jsonb,
			'[{"language":"deu"}]'::jsonb)`,
		seriesID, canonicalID, folderID, prefix+"-canonical.mkv")

	entries = fetchEntries(t)
	if len(entries) != 1 {
		t.Fatalf("episode_catalog_entries rows after canonical file = %d, want 1: %+v", len(entries), entries)
	}
	assertECEStats(t, entries[0], eceStats{
		maxResolutionRank:     4,
		resolutionCodes:       []string{"1080p", "2160p"},
		maxBitrate:            12000,
		minBitrate:            5000,
		hasHDR:                true,
		hasNonHDR:             true,
		hasDolbyVision:        true,
		hasNonDolbyVision:     true,
		audioLanguageCodes:    []string{"en", "fr"},
		subtitleLanguageCodes: []string{"de", "es"},
	})
	if entries[0].episodeID != canonicalID {
		t.Fatalf("entry episode_id after canonical file = %q, want %q", entries[0].episodeID, canonicalID)
	}

	// Removing the only membership removes the canonical entry.
	exec(`DELETE FROM episode_libraries WHERE episode_id = $1 AND media_folder_id = $2`, aliasID, folderID)
	if entries = fetchEntries(t); len(entries) != 0 {
		t.Fatalf("episode_catalog_entries rows after alias membership removal = %d, want 0: %+v", len(entries), entries)
	}

	var aliasEntries int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM episode_catalog_entries WHERE episode_id = $1`, aliasID,
	).Scan(&aliasEntries); err != nil {
		t.Fatalf("count alias entries: %v", err)
	}
	if aliasEntries != 0 {
		t.Fatalf("episode_catalog_entries rows keyed by alias = %d, want 0", aliasEntries)
	}
}

type eceRow struct {
	episodeID             string
	seriesID              string
	addedAt               time.Time
	maxResolutionRank     int
	resolutionCodes       []string
	maxBitrate            int
	minBitrate            int
	hasHDR                bool
	hasNonHDR             bool
	hasDolbyVision        bool
	hasNonDolbyVision     bool
	audioLanguageCodes    []string
	subtitleLanguageCodes []string
}

type eceStats struct {
	maxResolutionRank     int
	resolutionCodes       []string
	maxBitrate            int
	minBitrate            int
	hasHDR                bool
	hasNonHDR             bool
	hasDolbyVision        bool
	hasNonDolbyVision     bool
	audioLanguageCodes    []string
	subtitleLanguageCodes []string
}

func assertECEStats(t *testing.T, got eceRow, want eceStats) {
	t.Helper()
	if got.maxResolutionRank != want.maxResolutionRank {
		t.Fatalf("max_resolution_rank = %d, want %d", got.maxResolutionRank, want.maxResolutionRank)
	}
	if !slices.Equal(got.resolutionCodes, want.resolutionCodes) {
		t.Fatalf("resolution_codes = %v, want %v", got.resolutionCodes, want.resolutionCodes)
	}
	if got.maxBitrate != want.maxBitrate {
		t.Fatalf("max_bitrate = %d, want %d", got.maxBitrate, want.maxBitrate)
	}
	if got.minBitrate != want.minBitrate {
		t.Fatalf("min_bitrate = %d, want %d", got.minBitrate, want.minBitrate)
	}
	if got.hasHDR != want.hasHDR {
		t.Fatalf("has_hdr = %v, want %v", got.hasHDR, want.hasHDR)
	}
	if got.hasNonHDR != want.hasNonHDR {
		t.Fatalf("has_non_hdr = %v, want %v", got.hasNonHDR, want.hasNonHDR)
	}
	if got.hasDolbyVision != want.hasDolbyVision {
		t.Fatalf("has_dolby_vision = %v, want %v", got.hasDolbyVision, want.hasDolbyVision)
	}
	if got.hasNonDolbyVision != want.hasNonDolbyVision {
		t.Fatalf("has_non_dolby_vision = %v, want %v", got.hasNonDolbyVision, want.hasNonDolbyVision)
	}
	if !slices.Equal(got.audioLanguageCodes, want.audioLanguageCodes) {
		t.Fatalf("audio_language_codes = %v, want %v", got.audioLanguageCodes, want.audioLanguageCodes)
	}
	if !slices.Equal(got.subtitleLanguageCodes, want.subtitleLanguageCodes) {
		t.Fatalf("subtitle_language_codes = %v, want %v", got.subtitleLanguageCodes, want.subtitleLanguageCodes)
	}
}
