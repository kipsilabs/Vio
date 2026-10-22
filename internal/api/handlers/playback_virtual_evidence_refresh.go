package handlers

import (
	"context"

	"github.com/Silo-Server/silo-server/internal/models"
)

// refreshVirtualCatalogFileForEvidence re-reads the row a probe is about to
// write. The probe snapshot may predate a detail read's catalog revision or a
// provider re-list; the evidence write is fenced on the row's current
// updated_at/probe stamp, so writing against the old snapshot can be refused
// even when the row still names the same item and source. Keep the old row when
// the lookup is unavailable or the row identity moved, preserving the existing
// fail-closed behavior.
//
// The identity fence deliberately includes EpisodeID and the provider-neutral
// path. ContentID alone is not enough: a row can be reassigned to another
// episode or profile group while retaining the same content and library.
func (h *PlaybackHandler) refreshVirtualCatalogFileForEvidence(ctx context.Context, file *models.MediaFile) *models.MediaFile {
	if h == nil || h.fileResolver == nil || file == nil || file.ID <= 0 {
		return file
	}
	fresh, err := h.fileResolver.GetByID(ctx, file.ID)
	if err != nil || fresh == nil || fresh.ID != file.ID {
		return file
	}
	if fresh.ContentID != file.ContentID ||
		fresh.EpisodeID != file.EpisodeID ||
		fresh.MediaFolderID != file.MediaFolderID ||
		fresh.VirtualOwnerInstallationID != file.VirtualOwnerInstallationID ||
		virtualPlaybackNeutralKey(fresh.FilePath) != virtualPlaybackNeutralKey(file.FilePath) {
		return file
	}
	// Do not let a stale probe overwrite evidence another probe already
	// committed. Refresh the CAS snapshot for revision-only or path churn, but
	// keep the old snapshot when the probe stamp moved; that case must take the
	// normal superseded-write path rather than regress newer evidence.
	if !sameVirtualProbeStamp(file.ProbeUpdatedAt, fresh.ProbeUpdatedAt) {
		return file
	}
	return fresh
}
