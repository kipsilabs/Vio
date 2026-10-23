package pgstore

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/userstore"
)

// TestPostgresWatchStateWritesCanonicalEpisodeDB pins the storage write
// boundary: a caller that still carries a cached alias id may write progress,
// hints, resume state, and history, and every row lands on the canonical
// episode. No alias-keyed watch state is created.
func TestPostgresWatchStateWritesCanonicalEpisodeDB(t *testing.T) {
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

	prefix := fmt.Sprintf("alias-write-%d", time.Now().UnixNano())
	seriesID := prefix + "-series"
	canonicalID := prefix + "-canonical"
	aliasID := prefix + "-alias"
	const (
		profileOne = "alias-write-profile-1"
		profileTwo = "alias-write-profile-2"
	)

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
		cleanup := context.Background()
		_, _ = pool.Exec(cleanup, `DELETE FROM user_watch_history WHERE user_id = $1`, userID)
		_, _ = pool.Exec(cleanup, `DELETE FROM user_watch_progress WHERE user_id = $1`, userID)
		_, _ = pool.Exec(cleanup, `DELETE FROM episode_aliases WHERE series_id = $1`, seriesID)
		_, _ = pool.Exec(cleanup, `DELETE FROM episodes WHERE series_id = $1`, seriesID)
		_, _ = pool.Exec(cleanup, `DELETE FROM media_items WHERE content_id = $1`, seriesID)
		_, _ = pool.Exec(cleanup, `DELETE FROM media_folders WHERE id = $1`, folderID)
		_, _ = pool.Exec(cleanup, `DELETE FROM users WHERE id = $1`, userID)
	})

	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO media_items (content_id, type, title) VALUES ($1, 'series', 'Alias Write Fixture')`, []any{seriesID}},
		{`INSERT INTO episodes (content_id, series_id, season_number, episode_number, title)
			VALUES ($1, $3, 1, 1, 'Canonical'), ($2, $3, 2, 1, 'Alias')`,
			[]any{canonicalID, aliasID, seriesID}},
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

	store := newStore(pool, userID)
	thresholds := userstore.ProgressThresholds{}
	now := time.Now().UTC()
	if _, err := store.SetProgressIfNewer(ctx, profileOne, aliasID, 240, 1000, false, now); err != nil {
		t.Fatalf("SetProgressIfNewer: %v", err)
	}
	if err := store.UpdateProgressHints(ctx, profileOne, aliasID, userstore.VersionHints{
		FileID: 7, Resolution: "1080p", HDR: true, CodecVideo: "h264", EditionKey: "edition-a",
	}); err != nil {
		t.Fatalf("UpdateProgressHints: %v", err)
	}
	if err := store.SetProgress(ctx, profileOne, aliasID, 300, 1000, thresholds); err != nil {
		t.Fatalf("SetProgress: %v", err)
	}
	if _, err := store.ClearResumePosition(ctx, profileOne, aliasID, now.Add(time.Minute)); err != nil {
		t.Fatalf("ClearResumePosition: %v", err)
	}
	if err := store.MarkWatched(ctx, profileOne, aliasID, 1000); err != nil {
		t.Fatalf("MarkWatched: %v", err)
	}
	if err := store.AddHistory(ctx, userstore.WatchHistoryEntry{
		ID: prefix + "-history-direct", ProfileID: profileOne, MediaItemID: aliasID,
		WatchedAt: now.Format(time.RFC3339), DurationSeconds: 1000, Completed: true,
	}); err != nil {
		t.Fatalf("AddHistory: %v", err)
	}
	if _, err := store.AddVisibleHistory(ctx, userstore.WatchHistoryEntry{
		ID: prefix + "-history-visible", ProfileID: profileOne, MediaItemID: aliasID,
		WatchedAt: now.Add(time.Minute).Format(time.RFC3339), DurationSeconds: 1000, Completed: true,
	}); err != nil {
		t.Fatalf("AddVisibleHistory: %v", err)
	}
	// A second profile writing the same alias stays separate.
	if _, err := store.SetProgressIfNewer(ctx, profileTwo, aliasID, 120, 800, false, now); err != nil {
		t.Fatalf("profile two SetProgressIfNewer: %v", err)
	}

	var canonicalRows, aliasRows int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM user_watch_progress
		WHERE user_id = $1 AND media_item_id = $2`, userID, canonicalID,
	).Scan(&canonicalRows); err != nil {
		t.Fatalf("canonical progress count: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM user_watch_progress
		WHERE user_id = $1 AND media_item_id = $2`, userID, aliasID,
	).Scan(&aliasRows); err != nil {
		t.Fatalf("alias progress count: %v", err)
	}
	if canonicalRows != 2 || aliasRows != 0 {
		t.Fatalf("progress rows canonical=%d alias=%d, want 2 and 0", canonicalRows, aliasRows)
	}

	var (
		position   float64
		duration   float64
		completed  bool
		resolution *string
		hdr        *bool
		codecVideo *string
		editionKey *string
		lastFileID *int
	)
	if err := pool.QueryRow(ctx, `
		SELECT position_seconds, duration_seconds, completed, last_resolution, last_hdr,
		       last_codec_video, last_edition_key, last_file_id
		FROM user_watch_progress
		WHERE user_id = $1 AND profile_id = $2 AND media_item_id = $3`,
		userID, profileOne, canonicalID,
	).Scan(&position, &duration, &completed, &resolution, &hdr, &codecVideo, &editionKey, &lastFileID); err != nil {
		t.Fatalf("canonical progress: %v", err)
	}
	if position != 0 || duration != 1000 || !completed {
		t.Fatalf("canonical progress = position %v duration %v completed %v, want 0, 1000, true", position, duration, completed)
	}
	if resolution == nil || *resolution != "1080p" || hdr == nil || !*hdr ||
		codecVideo == nil || *codecVideo != "h264" || editionKey == nil || *editionKey != "edition-a" ||
		lastFileID == nil || *lastFileID != 7 {
		t.Fatalf("canonical hints = resolution %v hdr %v codec %v edition %v file %v", resolution, hdr, codecVideo, editionKey, lastFileID)
	}

	var canonicalHistory, aliasHistory int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM user_watch_history
		WHERE user_id = $1 AND media_item_id = $2`, userID, canonicalID,
	).Scan(&canonicalHistory); err != nil {
		t.Fatalf("canonical history count: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM user_watch_history
		WHERE user_id = $1 AND media_item_id = $2`, userID, aliasID,
	).Scan(&aliasHistory); err != nil {
		t.Fatalf("alias history count: %v", err)
	}
	if canonicalHistory != 2 || aliasHistory != 0 {
		t.Fatalf("history rows canonical=%d alias=%d, want 2 and 0", canonicalHistory, aliasHistory)
	}

	var profileTwoPosition float64
	if err := pool.QueryRow(ctx, `
		SELECT position_seconds FROM user_watch_progress
		WHERE user_id = $1 AND profile_id = $2 AND media_item_id = $3`,
		userID, profileTwo, canonicalID,
	).Scan(&profileTwoPosition); err != nil {
		t.Fatalf("profile two progress: %v", err)
	}
	if profileTwoPosition != 120 {
		t.Fatalf("profile two position = %v, want 120", profileTwoPosition)
	}
}
