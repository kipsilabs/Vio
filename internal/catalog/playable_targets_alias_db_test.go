package catalog

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/userstore/pgstore"
)

// TestPlayableTargetsResolveCanonicalAliasFilesDB pins playable-target
// selection to canonical episode rows even when the canonical owns no
// media_files row and its alias does. Both the backend-neutral path and the
// PostgreSQL winner path must return the canonical ID for series, season, and
// explicit episode cards.
func TestPlayableTargetsResolveCanonicalAliasFilesDB(t *testing.T) { //nolint:funlen // one DB fixture covers backend-neutral and PostgreSQL winner paths
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

	suffix := time.Now().UnixNano()
	prefix := fmt.Sprintf("alias-play-target-%d", suffix)
	seriesID := prefix + "-series"
	seasonID := prefix + "-season"
	coordinatesSeasonID := prefix + "-season-coordinates"
	canonicalID := prefix + "-canonical"
	aliasID := prefix + "-alias"

	var userID, folderID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (email, username, role, enabled)
		VALUES ($1, $1, 'user', TRUE)
		RETURNING id
	`, prefix+"@example.invalid").Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO media_folders (type, name, enabled) VALUES ('series', $1, TRUE) RETURNING id`,
		prefix,
	).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_files WHERE media_folder_id = $1`, folderID)
		_, _ = pool.Exec(ctx, `DELETE FROM episode_aliases WHERE series_id = $1`, seriesID)
		_, _ = pool.Exec(ctx, `DELETE FROM episodes WHERE series_id = $1`, seriesID)
		_, _ = pool.Exec(ctx, `DELETE FROM seasons WHERE series_id = $1`, seriesID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id = $1`, seriesID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id = $1`, folderID)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
	})

	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO media_items (content_id, type, title) VALUES ($1, 'series', 'Alias Play Target')`, []any{seriesID}},
		{`INSERT INTO seasons (content_id, series_id, season_number) VALUES ($1, $2, 1)`, []any{seasonID, seriesID}},
		// The alias row sorts before the canonical (season 1, episode 0), so a
		// missing exclusion would select it for both the series and season cards.
		{`INSERT INTO episodes (content_id, series_id, season_number, episode_number, title)
			VALUES ($1, $3, 1, 1, 'Canonical'), ($2, $3, 1, 0, 'Alias')`,
			[]any{canonicalID, aliasID, seriesID}},
		{`INSERT INTO episode_aliases (
				series_id, canonical_episode_id, alias_episode_id,
				provider_installation_id, provider_series_id,
				provider_season_number, provider_episode_number, source
			) VALUES ($1, $2, $3, 0, '', 1, 0, 'absolute_layout')`,
			[]any{seriesID, canonicalID, aliasID}},
		{`INSERT INTO media_files (content_id, episode_id, media_folder_id, file_path, file_size)
			VALUES ($1, $2, $3, $4, 1)`,
			[]any{seriesID, aliasID, folderID, prefix + "-alias.mkv"}},
	} {
		if _, err := pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("seed %q: %v", statement.sql, err)
		}
	}

	episodes := NewEpisodeRepository(pool)
	hasFiles, err := episodes.HasFilesByIDs(ctx, []string{canonicalID, aliasID, prefix + "-missing"})
	if err != nil {
		t.Fatalf("HasFilesByIDs: %v", err)
	}
	if !hasFiles[canonicalID] {
		t.Fatalf("canonical %q has no file despite an alias-owned file: %v", canonicalID, hasFiles)
	}
	if !hasFiles[aliasID] {
		t.Fatalf("alias %q has no direct file: %v", aliasID, hasFiles)
	}
	if hasFiles[prefix+"-missing"] {
		t.Fatalf("unknown episode reported a file: %v", hasFiles)
	}

	seasonNumber := 1
	inputs := []PlayableTargetInput{
		{ContentID: seriesID, Type: "series"},
		{ContentID: seasonID, Type: "season"},
		{ContentID: coordinatesSeasonID, Type: "season", SeriesID: seriesID, SeasonNumber: &seasonNumber},
		{ContentID: canonicalID, Type: "episode"},
	}
	resolver := NewPlayableTargetResolver(pool)
	store, err := pgstore.NewPostgresProvider(pool).ForUser(ctx, userID)
	if err != nil {
		t.Fatalf("create progress store: %v", err)
	}
	var progressStore PlayableTargetProgressStore = store

	for _, tc := range []struct {
		name          string
		progressStore PlayableTargetProgressStore
	}{
		{name: "backend-neutral"},
		{name: "postgres winners", progressStore: progressStore},
	} {
		t.Run(tc.name, func(t *testing.T) {
			targets, err := resolver.ResolveTargets(ctx, PlayableTargetQuery{
				UserID:        userID,
				ProfileID:     prefix + "-profile",
				Items:         inputs,
				Access:        AccessFilter{AllowedLibraryIDs: []int{folderID}},
				ProgressStore: tc.progressStore,
			})
			if err != nil {
				t.Fatalf("ResolveTargets: %v", err)
			}
			for _, input := range inputs {
				target, ok := targets[input.Key()]
				if !ok {
					t.Fatalf("%s card %+v has no target", input.Type, input)
				}
				if target.ContentID != canonicalID {
					t.Fatalf("%s card target = %q, want canonical %q", input.Type, target.ContentID, canonicalID)
				}
				if input.Type != "episode" && (target.SeasonNumber == nil || *target.SeasonNumber != 1) {
					t.Fatalf("%s card season = %v, want 1", input.Type, target.SeasonNumber)
				}
			}
			if _, ok := targets[PlayableTargetInput{ContentID: aliasID, Type: "episode"}.Key()]; ok {
				t.Fatal("resolver returned a target for an unrequested alias card")
			}
		})
	}
}
