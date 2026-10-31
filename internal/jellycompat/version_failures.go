package jellycompat

import "github.com/Silo-Server/silo-server/internal/catalog"

// compatLiveVersions removes versions the catalog stamped with a durable failed
// verdict (media_files.failed_at), mirroring the native version pickers: a dead
// row leaves the list instead of being offered again. Two exceptions keep the
// native behavior exact:
//   - the row matching the active/selected version stays, so a verdict that
//     lands mid-session cannot yank the version the client is playing;
//   - an all-failed list is returned whole rather than rendered empty.
//
// This is read-only filtering of an already-fetched slice; it never writes to
// the catalog. The Jellyfin protocol has no "failed" field, so exclusion at
// enumeration time is the parity mechanism.
func compatLiveVersions(versions []catalog.FileVersion, selectedFileID int) []catalog.FileVersion {
	alive := make([]catalog.FileVersion, 0, len(versions))
	for _, version := range versions {
		if !version.Failed || (selectedFileID > 0 && version.FileID == selectedFileID) {
			alive = append(alive, version)
		}
	}
	if len(alive) > 0 {
		return alive
	}
	return versions
}
