package downloads

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

type RegistryPosition struct {
	CreatedAt time.Time
	ID        string
}

// ListPage preserves the separate account-ephemeral and profile/device modes.
// ID breaks timestamp ties; creation time remains stable through status reports.
func (s *Service) ListPage(ctx context.Context, userID int, profileID, deviceID string, after *RegistryPosition, limit int) ([]*Download, error) {
	if deviceID != "" && profileID == "" {
		return nil, ErrProfileRequired
	}
	rows, err := s.repo.ListPage(ctx, userID, profileID, deviceID, after, limit)
	if err != nil {
		return nil, err
	}
	if after == nil && deviceID != "" {
		// A registry read is the device syncing: it is how a revoked copy
		// reaches the device, and what makes "last seen" mean something.
		if err := s.repo.TouchDeviceSeen(ctx, userID, profileID, deviceID); err != nil {
			slog.WarnContext(ctx, "recording device sync failed", "component", "downloads", "error", err)
		}
	}
	if err := s.repo.attachPreparations(ctx, rows); err != nil {
		// Progress is decoration; the registry is still correct without it.
		slog.WarnContext(ctx, "download preparation progress unavailable", "component", "downloads", "error", err)
	}
	return rows, nil
}
func (r *Repository) ListPage(ctx context.Context, userID int, profileID, deviceID string, after *RegistryPosition, limit int) ([]*Download, error) {
	return r.listRegistryPage(ctx, userID, profileID, deviceID, "", after, limit)
}

// Get returns one registry entry in the same mode listRegistryPage uses: the
// device-managed row when deviceID is set (scoped on profile and device), else
// the account's ephemeral row. A managed row is never reachable through the
// device-less mode, so a caller cannot read another scope's entry by dropping
// the device header. Preparation progress is attached exactly as it is for a
// listed page, so a polling client sees the same shape either way.
func (s *Service) Get(ctx context.Context, userID int, profileID, deviceID, downloadID string) (*Download, error) {
	if downloadID == "" {
		return nil, ErrNotFound
	}
	if deviceID != "" {
		if profileID == "" {
			return nil, ErrProfileRequired
		}
		dl, err := s.repo.GetManagedByID(ctx, downloadID, userID, profileID, deviceID)
		if err != nil {
			return nil, err
		}
		s.attachPreparation(ctx, dl)
		return dl, nil
	}
	dl, err := s.repo.GetByID(ctx, downloadID)
	if err != nil {
		return nil, err
	}
	if dl.UserID != userID || dl.IsManaged() {
		return nil, ErrNotFound
	}
	s.attachPreparation(ctx, dl)
	return dl, nil
}

// attachPreparation decorates one entry with its preparation progress. Progress
// is decoration; a failure to read it must not fail the entry read.
func (s *Service) attachPreparation(ctx context.Context, dl *Download) {
	if dl == nil {
		return
	}
	rows := []*Download{dl}
	if err := s.repo.attachPreparations(ctx, rows); err != nil {
		slog.WarnContext(ctx, "download preparation progress unavailable", "component", "downloads", "download_id", dl.ID, "error", err)
	}
}
func (r *Repository) listRegistryPage(ctx context.Context, userID int, profileID, deviceID, batchID string, after *RegistryPosition, limit int) ([]*Download, error) {
	if limit < 1 || limit > 101 {
		return nil, fmt.Errorf("download page limit must be 1 to 101")
	}
	var at *time.Time
	id := ""
	if after != nil {
		at = &after.CreatedAt
		id = after.ID
	}
	rows, err := r.pool.Query(ctx, `SELECT `+downloadColumns+` FROM downloads WHERE user_id=$1
 AND (($3='' AND device_id IS NULL) OR ($3<>'' AND profile_id=$2 AND device_id=$3))
 AND ($4::timestamptz IS NULL OR (created_at,id)<($4,$5))
 AND ($7='' OR batch_id=$7)
 ORDER BY created_at DESC,id DESC LIMIT $6`, userID, profileID, deviceID, at, id, limit, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDownloads(rows)
}
