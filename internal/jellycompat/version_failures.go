package jellycompat

import (
	"time"

	"github.com/Silo-Server/silo-server/internal/catalog"
)

// compatLiveVersions removes versions the catalog stamped with a durable failed
// verdict (media_files.failed_at), mirroring the native version pickers: a dead
// row leaves the list instead of being offered again. Two exceptions keep the
// native behavior exact:
//   - the row matching the active/selected version stays, so a verdict that
//     lands mid-session cannot yank the version the client is playing;
//   - an all-failed list is returned whole rather than rendered empty.
//
// A verdict only excludes for catalog.FailedVerdictMaxAge: a transient stamp
// that was never cleared must not hide the version from Jellyfin clients
// forever while the native pickers have already started offering it again.
//
// This is read-only filtering of an already-fetched slice; it never writes to
// the catalog. The Jellyfin protocol has no "failed" field, so exclusion at
// enumeration time is the parity mechanism.
func compatLiveVersions(versions []catalog.FileVersion, selectedFileID int) []catalog.FileVersion {
	return compatLiveVersionsAt(versions, selectedFileID, time.Now())
}

// compatLiveVersionsAt is compatLiveVersions with an explicit clock so the
// verdict window is testable at its boundary.
func compatLiveVersionsAt(versions []catalog.FileVersion, selectedFileID int, now time.Time) []catalog.FileVersion {
	alive := make([]catalog.FileVersion, 0, len(versions))
	for _, version := range versions {
		if !compatVersionDead(version, now) || (selectedFileID > 0 && version.FileID == selectedFileID) {
			alive = append(alive, version)
		}
	}
	if len(alive) > 0 {
		return alive
	}
	return versions
}

// compatVersionDead reports whether the row is currently excluded by a failed
// verdict. A Failed flag without a stamp keeps excluding: only a timestamped
// verdict can age out, so a malformed projection fails safe rather than
// resurrecting a dead row.
func compatVersionDead(version catalog.FileVersion, now time.Time) bool {
	if !version.Failed {
		return false
	}
	if version.FailedAt == nil {
		return true
	}
	return catalog.FailedVerdictActive(version.FailedAt, now, catalog.FailedVerdictMaxAge)
}
