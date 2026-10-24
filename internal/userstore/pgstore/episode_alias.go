package pgstore

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// watchStateRowQuerier is the read side shared by the pool and a transaction.
type watchStateRowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type watchStateQuerier interface {
	watchStateRowQuerier
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// lockWatchStateSeries serializes a watch-state write against episode-alias
// reconciliation for every episode series named by mediaItemIDs. It takes the
// shared form of the per-series lock reconciliation takes exclusively
// (lockEpisodeAliasSeries in internal/catalog, and the episode_aliases identity
// guard trigger), so canonical resolution and the write cannot interleave with
// a repair: reconciliation either commits before the resolver runs, so the
// write sees the mapping and lands on the canonical episode, or it starts only
// after the write commits, so its repair sees the row and moves it.
//
// The lock is transaction-scoped and must be acquired before resolving the
// canonical id, because the resolution statement needs a snapshot taken after
// the lock is granted. Series ids are locked in ascending order so a batch
// write spanning several series cannot deadlock against another batch writer
// that takes the same series in a different input order.
//
// Items that are not episodes (movies, series rows) have no series to lock and
// are already canonical. Aliases are only defined between episodes that share
// one series, so the submitted id's own episodes row names the series that
// owns any mapping it can match.
func lockWatchStateSeries(ctx context.Context, tx pgx.Tx, mediaItemIDs []string) error {
	mediaItemIDs = compactMediaItemIDs(mediaItemIDs)
	if len(mediaItemIDs) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT e.series_id
		FROM episodes e
		WHERE e.content_id = ANY($1::text[])
		ORDER BY e.series_id`, mediaItemIDs)
	if err != nil {
		return fmt.Errorf("listing watch-state series: %w", err)
	}
	seriesIDs := make([]string, 0, len(mediaItemIDs))
	for rows.Next() {
		var seriesID string
		if err := rows.Scan(&seriesID); err != nil {
			rows.Close()
			return fmt.Errorf("scanning watch-state series: %w", err)
		}
		seriesIDs = append(seriesIDs, seriesID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating watch-state series: %w", err)
	}
	for _, seriesID := range seriesIDs {
		if _, err := tx.Exec(ctx,
			`SELECT pg_advisory_xact_lock_shared(hashtextextended($1, 0))`, seriesID,
		); err != nil {
			return fmt.Errorf("locking watch-state series %q: %w", seriesID, err)
		}
	}
	return nil
}

// canonicalWatchStateID maps one watch-state target to the canonical episode
// through the same SQL function the progress migration and series
// reconciliation use. An id with no alias mapping is returned unchanged, so
// movies, series, and canonical episodes cost the same one indexed probe.
//
// A lookup failure is returned to the caller. Watch-state writes must not fall
// back to the submitted id: a cached alias would otherwise recreate the hidden
// progress row the canonical read paths ignore.
func canonicalWatchStateID(ctx context.Context, db watchStateRowQuerier, mediaItemID string) (string, error) {
	if strings.TrimSpace(mediaItemID) == "" {
		return mediaItemID, nil
	}
	var canonicalID string
	if err := db.QueryRow(ctx,
		`SELECT public.canonical_episode_id($1)`, mediaItemID,
	).Scan(&canonicalID); err != nil {
		return "", fmt.Errorf("resolving canonical watch-state episode %q: %w", mediaItemID, err)
	}
	return canonicalID, nil
}

// canonicalWatchStateIDMap resolves a batch of targets in one round trip. The
// returned map is keyed by the submitted id.
func canonicalWatchStateIDMap(ctx context.Context, db watchStateQuerier, mediaItemIDs []string) (map[string]string, error) {
	mediaItemIDs = compactMediaItemIDs(mediaItemIDs)
	if len(mediaItemIDs) == 0 {
		return map[string]string{}, nil
	}
	rows, err := db.Query(ctx, `
		SELECT input.media_item_id, public.canonical_episode_id(input.media_item_id)
		FROM unnest($1::text[]) AS input(media_item_id)`, mediaItemIDs)
	if err != nil {
		return nil, fmt.Errorf("resolving canonical watch-state episodes: %w", err)
	}
	defer rows.Close()

	canonicalByID := make(map[string]string, len(mediaItemIDs))
	for rows.Next() {
		var mediaItemID, canonicalID string
		if err := rows.Scan(&mediaItemID, &canonicalID); err != nil {
			return nil, fmt.Errorf("scanning canonical watch-state episode: %w", err)
		}
		if canonicalID == "" {
			// A function that returned an empty id is a contract failure, not
			// an unmapped id: fail the write rather than guess at identity.
			return nil, fmt.Errorf("canonical watch-state episode missing for %q", mediaItemID)
		}
		canonicalByID[mediaItemID] = canonicalID
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating canonical watch-state episodes: %w", err)
	}
	for _, mediaItemID := range mediaItemIDs {
		if _, ok := canonicalByID[mediaItemID]; !ok {
			return nil, fmt.Errorf("canonical watch-state episode missing for %q", mediaItemID)
		}
	}
	return canonicalByID, nil
}

// canonicalWatchStateIDs is canonicalWatchStateIDMap in input order with
// aliases of one canonical episode collapsed to one write target.
func canonicalWatchStateIDs(ctx context.Context, db watchStateQuerier, mediaItemIDs []string) ([]string, error) {
	mediaItemIDs = compactMediaItemIDs(mediaItemIDs)
	canonicalByID, err := canonicalWatchStateIDMap(ctx, db, mediaItemIDs)
	if err != nil {
		return nil, err
	}
	canonicalIDs := make([]string, 0, len(mediaItemIDs))
	seen := make(map[string]struct{}, len(mediaItemIDs))
	for _, mediaItemID := range mediaItemIDs {
		canonicalID := canonicalByID[mediaItemID]
		if _, ok := seen[canonicalID]; ok {
			continue
		}
		seen[canonicalID] = struct{}{}
		canonicalIDs = append(canonicalIDs, canonicalID)
	}
	return canonicalIDs, nil
}
