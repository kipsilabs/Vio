package pgstore

import (
	"context"
	"fmt"
	"time"

	"github.com/Silo-Server/silo-server/internal/userstore"
)

// ListNextUpStatePage reads one account/profile-scoped Next Up state page,
// newest first, using the same keyset order as the SQLite implementation.
func (s *PostgresUserStore) ListNextUpStatePage(ctx context.Context, profileID string, cursor *userstore.NextUpStateCursor, limit int) (userstore.NextUpStatePage, error) {
	if limit <= 0 {
		return userstore.NextUpStatePage{}, userstore.ErrNextUpStateInvalidLimit
	}
	if err := ctx.Err(); err != nil {
		return userstore.NextUpStatePage{}, err
	}

	query := `
		SELECT wp.media_item_id, wp.completed, wp.position_seconds, wp.updated_at
		FROM user_watch_progress wp
		WHERE wp.user_id = $1
		  AND wp.profile_id = $2
		  AND (wp.completed OR wp.position_seconds > 0)
		  AND NOT EXISTS (
			SELECT 1
			FROM user_history_hidden_items hhi
			WHERE hhi.user_id = wp.user_id
			  AND hhi.profile_id = wp.profile_id
			  AND hhi.media_item_id = wp.media_item_id
			  AND hhi.hidden_before >= wp.updated_at
		  )`
	args := []any{s.userID, profileID}
	if cursor != nil {
		query += `
		  AND (wp.updated_at < $3 OR (wp.updated_at = $3 AND wp.media_item_id < $4))`
		args = append(args, cursor.UpdatedAt.UTC(), cursor.MediaItemID)
	}
	limitArg := len(args) + 1
	query += fmt.Sprintf(`
		ORDER BY wp.updated_at DESC, wp.media_item_id DESC
		LIMIT $%d`, limitArg)
	args = append(args, limit+1)

	rows, err := s.pool.Query(ctx, query, profilePlanArgs(args...)...)
	if err != nil {
		return userstore.NextUpStatePage{}, fmt.Errorf("listing next up state page: %w", err)
	}
	defer rows.Close()

	entries := make([]userstore.NextUpStateEntry, 0, limit+1)
	for rows.Next() {
		var entry userstore.NextUpStateEntry
		var updatedAt time.Time
		if err := rows.Scan(&entry.MediaItemID, &entry.Completed, &entry.Position, &updatedAt); err != nil {
			return userstore.NextUpStatePage{}, fmt.Errorf("scanning next up state row: %w", err)
		}
		entry.UpdatedAt = updatedAt.UTC()
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return userstore.NextUpStatePage{}, fmt.Errorf("iterating next up state rows: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return userstore.NextUpStatePage{}, err
	}
	return userstore.NextUpStatePageFromEntries(entries, limit), nil
}

// ListNextUpStateForItems returns the exact visible progress rows for the
// requested items in this account/profile scope. The caller supplies the item
// set, so result ordering is intentionally unspecified.
func (s *PostgresUserStore) ListNextUpStateForItems(ctx context.Context, profileID string, mediaItemIDs []string) ([]userstore.NextUpStateEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(mediaItemIDs) == 0 {
		return nil, nil
	}

	rows, err := s.pool.Query(ctx, `
		SELECT wp.media_item_id, wp.completed, wp.position_seconds, wp.updated_at
		FROM user_watch_progress wp
		WHERE wp.user_id = $1
		  AND wp.profile_id = $2
		  AND wp.media_item_id = ANY($3::text[])
		  AND (wp.completed OR wp.position_seconds > 0)
		  AND NOT EXISTS (
			SELECT 1
			FROM user_history_hidden_items hhi
			WHERE hhi.user_id = wp.user_id
			  AND hhi.profile_id = wp.profile_id
			  AND hhi.media_item_id = wp.media_item_id
			  AND hhi.hidden_before >= wp.updated_at
		  )`,
		profilePlanArgs(s.userID, profileID, mediaItemIDs)...,
	)
	if err != nil {
		return nil, fmt.Errorf("listing next up state for items: %w", err)
	}
	defer rows.Close()

	entries := make([]userstore.NextUpStateEntry, 0, len(mediaItemIDs))
	for rows.Next() {
		var entry userstore.NextUpStateEntry
		var updatedAt time.Time
		if err := rows.Scan(&entry.MediaItemID, &entry.Completed, &entry.Position, &updatedAt); err != nil {
			return nil, fmt.Errorf("scanning next up state item row: %w", err)
		}
		entry.UpdatedAt = updatedAt.UTC()
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating next up state item rows: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}
