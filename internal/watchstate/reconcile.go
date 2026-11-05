package watchstate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/userstore"
)

// ReconcileStats holds counters from a single reconciliation run.
type ReconcileStats struct {
	OrphansFound int
	Resolved     int
	Unresolvable int
	Errors       int
}

type historyDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// historyTx is the narrow transaction surface used for episode reassignment.
// The lock and the update must be separate statements: a CTE lock would leave
// the update on the statement snapshot taken before it blocked on the lock.
type historyTx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

type historyTxBeginner func(context.Context) (historyTx, error)

const orphanedHistoryRowsQuery = `
		SELECT h.user_id, h.id, h.media_item_id, h.watch_identity::text
		FROM user_watch_history h
		WHERE h.watch_identity <> '{}'::jsonb
		  AND (
		    (
		      h.watch_identity->>'stable_type' = 'movie'
		      AND NOT EXISTS (
		        SELECT 1 FROM media_items mi WHERE mi.content_id = h.media_item_id
		      )
		    )
		    OR (
		      h.watch_identity->>'stable_type' = 'episode'
		      AND NOT EXISTS (
		        SELECT 1 FROM episodes ep WHERE ep.content_id = h.media_item_id
		      )
		    )
		  )
		ORDER BY h.user_id, h.id
	`

// updateHistoryMediaItemQuery moves one row and copies the profile's
// watched-state cutoff for the old item onto the new one. A mark unwatched
// keeps plays in history, so a moved play from before it must not count as
// watched there. The listing cutoff stays the new item's: a history removal
// already deleted the plays behind it. The command tag is the UPDATE's.
const updateHistoryMediaItemQuery = `
		WITH carried AS (
			INSERT INTO user_history_hidden_items (user_id, profile_id, media_item_id, hidden_before, removed_before, updated_at)
			SELECT hhi.user_id, hhi.profile_id, $1, hhi.hidden_before, '-infinity', hhi.updated_at
			FROM user_watch_history h
			JOIN user_history_hidden_items hhi
			  ON hhi.user_id = h.user_id AND hhi.profile_id = h.profile_id AND hhi.media_item_id = h.media_item_id
			WHERE h.user_id = $2
			  AND h.id = $3
			  AND h.media_item_id = $4
			ON CONFLICT (user_id, profile_id, media_item_id) DO UPDATE
			SET hidden_before = GREATEST(user_history_hidden_items.hidden_before, EXCLUDED.hidden_before),
			    removed_before = COALESCE(user_history_hidden_items.removed_before, user_history_hidden_items.hidden_before),
			    updated_at = GREATEST(user_history_hidden_items.updated_at, EXCLUDED.updated_at)
		)
		UPDATE user_watch_history
		SET media_item_id = $1
		WHERE user_id = $2
		  AND id = $3
		  AND media_item_id = $4
	`

const lockEpisodeSeriesQuery = `
		SELECT pg_advisory_xact_lock_shared(hashtextextended(e.series_id, 0))
		FROM episodes e
		WHERE e.content_id = $1
	`

const updateEpisodeHistoryMediaItemQuery = `
		UPDATE user_watch_history
		SET media_item_id = public.canonical_episode_id($1)
		WHERE user_id = $2
		  AND id = $3
		  AND media_item_id = $4
		  AND EXISTS (SELECT 1 FROM episodes WHERE content_id = $1)
	`

// HistoryReconciler repairs orphaned watch-history rows by resolving each row's
// stored watch_identity back to a current media_item_id via the catalog.
type HistoryReconciler struct {
	pool     historyDB
	begin    historyTxBeginner
	resolver *StableIdentityResolver
}

// NewHistoryReconciler creates a HistoryReconciler backed by the given pool and resolver.
func NewHistoryReconciler(pool *pgxpool.Pool, resolver *StableIdentityResolver) *HistoryReconciler {
	var begin historyTxBeginner
	if pool != nil {
		begin = func(ctx context.Context) (historyTx, error) { return pool.Begin(ctx) }
	}
	return &HistoryReconciler{pool: pool, begin: begin, resolver: resolver}
}

// Run performs one reconciliation sweep over all orphaned rows in user_watch_history.
// It respects context cancellation and returns partial stats on early exit.
func (r *HistoryReconciler) Run(ctx context.Context) (ReconcileStats, error) {
	var stats ReconcileStats

	rows, err := r.pool.Query(ctx, orphanedHistoryRowsQuery)
	if err != nil {
		return stats, fmt.Errorf("querying orphaned history rows: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		if ctx.Err() != nil {
			return stats, ctx.Err()
		}

		var userID int
		var id, currentItemID, identityJSON string
		if err := rows.Scan(&userID, &id, &currentItemID, &identityJSON); err != nil {
			stats.Errors++
			continue
		}
		stats.OrphansFound++

		var identity userstore.WatchIdentity
		if err := json.Unmarshal([]byte(identityJSON), &identity); err != nil || strings.TrimSpace(identity.StableType) == "" {
			stats.Unresolvable++
			continue
		}

		newID, err := r.resolve(ctx, identity)
		if err != nil {
			stats.Errors++
			continue
		}
		if newID == "" || newID == currentItemID {
			stats.Unresolvable++
			continue
		}

		if identity.StableType == stableTypeEpisode {
			// The episode update must lock and canonicalize. Use two statements
			// in one transaction: a CTE lock would leave the update on the
			// statement snapshot taken before it waited for the lock.
			resolved, err := r.rebindEpisodeHistory(ctx, newID, userID, id, currentItemID)
			if err != nil {
				stats.Errors++
				continue
			}
			if !resolved {
				stats.Unresolvable++
				continue
			}
			stats.Resolved++
			continue
		}
		tag, err := r.pool.Exec(ctx, updateHistoryMediaItemQuery, newID, userID, id, currentItemID)
		if err != nil {
			stats.Errors++
			continue
		}
		if tag.RowsAffected() == 0 {
			stats.Unresolvable++
			continue
		}
		stats.Resolved++
	}
	if err := rows.Err(); err != nil && ctx.Err() == nil {
		return stats, fmt.Errorf("iterating orphaned history rows: %w", err)
	}
	return stats, ctx.Err()
}

// rebindEpisodeHistory locks the target series, then updates in a second
// statement. Under READ COMMITTED the update's snapshot is taken after the
// lock is granted, so it sees a mapping committed by reconciliation while this
// update waited.
func (r *HistoryReconciler) rebindEpisodeHistory(ctx context.Context, newID string, userID int, id, currentItemID string) (bool, error) {
	if r.begin == nil {
		return false, errors.New("episode history transaction is not configured")
	}
	tx, err := r.begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin episode history update: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	lockTag, err := tx.Exec(ctx, lockEpisodeSeriesQuery, newID)
	if err != nil {
		return false, fmt.Errorf("lock episode history series: %w", err)
	}
	// The lock statement also confirms the target episode exists; a resolved
	// content ID can still be stale if the catalog changed underneath us.
	if lockTag.RowsAffected() == 0 {
		return false, nil
	}
	tag, err := tx.Exec(ctx, updateEpisodeHistoryMediaItemQuery, newID, userID, id, currentItemID)
	if err != nil {
		return false, fmt.Errorf("update episode history: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit episode history update: %w", err)
	}
	return true, nil
}

func (r *HistoryReconciler) resolve(ctx context.Context, identity userstore.WatchIdentity) (string, error) {
	switch identity.StableType {
	case stableTypeMovie:
		if len(identity.ProviderIDs) == 0 {
			return "", nil
		}
		return r.resolver.ResolveMovieContentID(ctx, identity.ProviderIDs)
	case stableTypeEpisode:
		seriesProviderIDs := episodeSeriesProviderIDs(identity)
		if len(seriesProviderIDs) == 0 || identity.Season == nil || identity.Episode == nil {
			return "", nil
		}
		return r.resolver.ResolveEpisodeContentID(ctx, seriesProviderIDs, *identity.Season, *identity.Episode)
	default:
		return "", nil
	}
}

func episodeSeriesProviderIDs(identity userstore.WatchIdentity) map[string]string {
	if len(identity.SeriesProviderIDs) > 0 {
		return identity.SeriesProviderIDs
	}
	return identity.ProviderIDs
}
