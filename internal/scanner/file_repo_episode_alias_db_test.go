package scanner

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestFileRepositoryFallsBackToAliasEpisodeFilesDB pins the read-time alias
// fallback: a canonical episode with no present files resolves the files owned
// by its alias rows, while a canonical episode with its own files ignores the
// alias side.
func TestFileRepositoryFallsBackToAliasEpisodeFilesDB(t *testing.T) { //nolint:funlen // one DB fixture covers direct, alias-only, duplicate, batch, and missing-file cases
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

	prefix := fmt.Sprintf("episode-alias-files-%d", time.Now().UnixNano())
	seriesID := prefix + "-series"
	canonicalFallbackID := prefix + "-canonical-fallback"
	aliasFallbackID := prefix + "-alias-fallback"
	canonicalOwnedID := prefix + "-canonical-owned"
	aliasShadowedID := prefix + "-alias-shadowed"

	var folderID int
	if err := pool.QueryRow(ctx,
		`INSERT INTO media_folders (type, name, enabled) VALUES ('series', $1, true) RETURNING id`,
		prefix,
	).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_files WHERE media_folder_id = $1`, folderID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM episode_aliases WHERE series_id = $1`, seriesID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM episodes WHERE series_id = $1`, seriesID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_items WHERE content_id = $1`, seriesID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_folders WHERE id = $1`, folderID)
	})

	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO media_items (content_id, type, title) VALUES ($1, 'series', 'Alias File Fixture')`, []any{seriesID}},
		{`INSERT INTO episodes (content_id, series_id, season_number, episode_number, title)
			VALUES ($1, $5, 1, 1, 'Canonical fallback'),
			       ($2, $5, 2, 1, 'Alias fallback'),
			       ($3, $5, 1, 2, 'Canonical owned'),
			       ($4, $5, 2, 2, 'Alias shadowed')`,
			[]any{canonicalFallbackID, aliasFallbackID, canonicalOwnedID, aliasShadowedID, seriesID}},
		{`INSERT INTO episode_aliases (
				series_id, canonical_episode_id, alias_episode_id,
				provider_installation_id, provider_series_id,
				provider_season_number, provider_episode_number, source
			) VALUES ($1, $2, $3, 0, '', 2, 1, 'absolute_layout'),
			         ($1, $4, $5, 0, '', 2, 2, 'absolute_layout')`,
			[]any{seriesID, canonicalFallbackID, aliasFallbackID, canonicalOwnedID, aliasShadowedID}},
	} {
		if _, err := pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("seed %q: %v", statement.sql, err)
		}
	}

	insertFile := func(episodeID, path string, missing bool) int {
		t.Helper()
		var missingSince any
		if missing {
			missingSince = time.Now()
		}
		var id int
		if err := pool.QueryRow(ctx, `
			INSERT INTO media_files (content_id, episode_id, media_folder_id, file_path, file_size, missing_since)
			VALUES ($1, $2, $3, $4, 1, $5)
			RETURNING id`, seriesID, episodeID, folderID, path, missingSince).Scan(&id); err != nil {
			t.Fatalf("seed media file %s: %v", path, err)
		}
		return id
	}

	aliasFirst := insertFile(aliasFallbackID, "/media/alias-fallback-1.mkv", false)
	insertFile(aliasFallbackID, "/media/alias-fallback-missing.mkv", true)
	aliasSecond := insertFile(aliasFallbackID, "/media/alias-fallback-2.mkv", false)
	canonicalOwnedFile := insertFile(canonicalOwnedID, "/media/canonical-owned.mkv", false)
	insertFile(aliasShadowedID, "/media/alias-shadowed.mkv", false)

	repo := NewFileRepository(pool)

	t.Run("canonical without files falls back to alias files in id order", func(t *testing.T) {
		files, err := repo.GetByEpisodeID(ctx, canonicalFallbackID)
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 2 {
			t.Fatalf("GetByEpisodeID returned %d files, want 2", len(files))
		}
		if files[0].ID != aliasFirst || files[1].ID != aliasSecond {
			t.Fatalf("file ids = [%d %d], want [%d %d]", files[0].ID, files[1].ID, aliasFirst, aliasSecond)
		}
		for _, file := range files {
			if file.EpisodeID != aliasFallbackID {
				t.Fatalf("file.EpisodeID = %q, want stored alias id %q", file.EpisodeID, aliasFallbackID)
			}
		}
	})

	t.Run("canonical with files does not duplicate alias files", func(t *testing.T) {
		files, err := repo.GetByEpisodeID(ctx, canonicalOwnedID)
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 1 || files[0].ID != canonicalOwnedFile {
			t.Fatalf("GetByEpisodeID returned %+v, want only canonical file %d", files, canonicalOwnedFile)
		}
	})

	t.Run("batch fallback keys by canonical episode and keeps stored alias ids", func(t *testing.T) {
		grouped, err := repo.ListByEpisodeIDs(ctx, []string{canonicalFallbackID, canonicalOwnedID, "unknown-episode"})
		if err != nil {
			t.Fatal(err)
		}
		fallback := grouped[canonicalFallbackID]
		if len(fallback) != 2 || fallback[0].ID != aliasFirst || fallback[1].ID != aliasSecond {
			t.Fatalf("batch fallback files = %+v, want alias files [%d %d]", fallback, aliasFirst, aliasSecond)
		}
		for _, file := range fallback {
			if file.EpisodeID != aliasFallbackID {
				t.Fatalf("batch fallback file.EpisodeID = %q, want stored alias id %q", file.EpisodeID, aliasFallbackID)
			}
		}
		owned := grouped[canonicalOwnedID]
		if len(owned) != 1 || owned[0].ID != canonicalOwnedFile {
			t.Fatalf("batch canonical-owned files = %+v, want only file %d", owned, canonicalOwnedFile)
		}
		if _, ok := grouped["unknown-episode"]; ok {
			t.Fatalf("unknown episode unexpectedly has a key: %+v", grouped["unknown-episode"])
		}
	})

	t.Run("empty input stays empty", func(t *testing.T) {
		grouped, err := repo.ListByEpisodeIDs(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(grouped) != 0 {
			t.Fatalf("ListByEpisodeIDs(nil) = %v, want empty", grouped)
		}
	})
}
