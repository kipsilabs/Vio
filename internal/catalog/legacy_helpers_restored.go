package catalog

// Restored fork helpers. Upstream retired the /api/v1 catalog surface these
// supported; fork catalog reads still call them.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Silo-Server/silo-server/internal/requestlock"
)

func (r *CatalogResolver) SearchFacetV1(ctx context.Context, req CatalogRequest, access AccessFilter, facet string, prefix string, limit int) ([]string, bool, error) {
	if r == nil || r.browseRepo == nil {
		return nil, false, errors.New("catalog resolver requires a browse repository")
	}
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return []string{}, false, nil
	}
	scope, err := r.facetSearchScope(ctx, req, access)
	if err != nil {
		return nil, false, err
	}
	if scope.empty {
		return []string{}, false, nil
	}
	matches, hasMore, err := searchFacetV1(ctx, r.browseRepo.pool, facet, scope.filters, scope.baseRelation, scope.mediaScope, prefix, clampFacetSearchLimit(limit))
	if err != nil {
		return nil, false, err
	}
	if matches == nil {
		matches = []string{}
	}
	return matches, hasMore, nil
}

func CalendarLocation(name string) *time.Location {
	if strings.TrimSpace(name) == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(strings.TrimSpace(name))
	if err != nil {
		return time.UTC
	}
	return loc
}

func (r *LibraryCollectionRepository) ListItemIDsPage(ctx context.Context, collectionID string, limit, offset int) ([]string, bool, error) {
	if limit <= 0 {
		return []string{}, false, nil
	}
	offset = max(offset, 0)

	rows, err := r.pool.Query(ctx, `
		SELECT media_item_id
		FROM library_collection_items
		WHERE collection_id = $1
		ORDER BY position ASC, source_rank ASC, media_item_id ASC
		LIMIT $2 OFFSET $3
	`, collectionID, limit+1, offset)
	if err != nil {
		return nil, false, fmt.Errorf("listing collection item page: %w", err)
	}
	defer rows.Close()

	ids := make([]string, 0, limit+1)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, false, fmt.Errorf("scanning collection item page: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterating collection item page: %w", err)
	}

	hasMore := len(ids) > limit
	if hasMore {
		ids = ids[:limit]
	}
	return ids, hasMore, nil
}

func (r *LibraryCollectionRepository) RemoveItem(ctx context.Context, collectionID, mediaItemID string) error {
	collectionSourceKey := "collection:" + collectionID
	if len([]byte(collectionSourceKey)) > 128 {
		return errors.New("collection ID is too long for virtual source ownership")
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning remove collection item: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock 1: Lock collection first to coordinate with concurrent sync/repair/delete.
	var lockedCollectionID string
	if err := tx.QueryRow(ctx, `
		SELECT id FROM library_collections WHERE id=$1 FOR UPDATE`,
		collectionID,
	).Scan(&lockedCollectionID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLibraryCollectionNotFound
		}
		return fmt.Errorf("locking library collection: %w", err)
	}

	if err := requestlock.LockItem(ctx, tx, mediaItemID); err != nil {
		return fmt.Errorf("locking removed collection item: %w", err)
	}

	// Capture previous collection virtual files for this item before claims are removed.
	if _, err := tx.Exec(ctx, `
		CREATE TEMP TABLE previous_collection_virtual_files(
			plugin_installation_id bigint,
			content_id text,
			media_folder_id int,
			file_path text
		) ON COMMIT DROP`); err != nil {
		return fmt.Errorf("create previous collection virtual files: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO previous_collection_virtual_files(
			plugin_installation_id, content_id, media_folder_id, file_path
		)
		SELECT plugin_installation_id, content_id, media_folder_id, file_path
		FROM virtual_media_file_source_claims
		WHERE source_key = $1 AND content_id = $2`, collectionSourceKey, mediaItemID); err != nil {
		return fmt.Errorf("capturing previous collection virtual files: %w", err)
	}

	// Capture affected item for metadata ownership reconciliation, but only
	// when this collection provably owned virtual state. Ordinary
	// metadata-only members must survive removal as catalog rows.
	if _, err := tx.Exec(ctx, `
		CREATE TEMP TABLE affected_collection_virtual_items(
			content_id text PRIMARY KEY
		) ON COMMIT DROP`); err != nil {
		return fmt.Errorf("create affected collection virtual items: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO affected_collection_virtual_items(content_id)
		SELECT $1
		WHERE EXISTS (
			SELECT 1 FROM virtual_media_source_claims
			WHERE source_key = $2 AND content_id = $1
		) OR EXISTS (
			SELECT 1 FROM virtual_media_file_source_claims
			WHERE source_key = $2 AND content_id = $1
		) ON CONFLICT DO NOTHING`, mediaItemID, collectionSourceKey); err != nil {
		return fmt.Errorf("capturing affected collection virtual item: %w", err)
	}

	// Delete collection membership. A non-member removal is a no-op and must
	// never trigger cleanup of an unrelated catalog item.
	tag, err := tx.Exec(ctx, `
		DELETE FROM library_collection_items
		WHERE collection_id = $1 AND media_item_id = $2
	`, collectionID, mediaItemID)
	if err != nil {
		return fmt.Errorf("deleting collection item: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrCollectionItemNotMember
	}

	// Delete this collection's file claims and source claims for this item.
	if _, err := tx.Exec(ctx, `
		DELETE FROM virtual_media_file_source_claims
		WHERE source_key = $1 AND content_id = $2`, collectionSourceKey, mediaItemID); err != nil {
		return fmt.Errorf("deleting collection virtual file claims: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM virtual_media_source_claims
		WHERE source_key = $1 AND content_id = $2`, collectionSourceKey, mediaItemID); err != nil {
		return fmt.Errorf("deleting collection virtual source claims: %w", err)
	}

	// Reconcile metadata ownership and clean unclaimed virtual media.
	if err := reconcileAffectedVirtualMetadataOwnership(ctx, tx); err != nil {
		return err
	}
	if err := cleanupUnclaimedCollectionVirtualMedia(ctx, tx); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, "UPDATE library_collections SET updated_at = NOW() WHERE id = $1", collectionID); err != nil {
		return fmt.Errorf("touching library collection: %w", err)
	}
	return tx.Commit(ctx)
}

func IntersectCollectionLibraryIDs(existing, required []int) []int {
	if len(required) == 0 {
		return append([]int(nil), existing...)
	}
	if len(existing) == 0 {
		return append([]int(nil), required...)
	}

	allowed := make(map[int]struct{}, len(required))
	for _, id := range required {
		allowed[id] = struct{}{}
	}

	result := make([]int, 0, len(existing))
	seen := make(map[int]struct{}, len(existing))
	for _, id := range existing {
		if _, ok := allowed[id]; !ok {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result
}
