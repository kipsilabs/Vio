package playback

import "github.com/Silo-Server/silo-server/internal/models"

// DVPinV3 pins the Dolby Vision profile a playback session first verified for
// an effective media file. Repeated probe reads of the same file can drift
// (production saw dv_profile 8 at start and 7 seconds later on a replan) while
// the underlying bytes never change. Every replan that re-read the catalog row
// would then key route selection off unstable evidence and thrash the pipeline
// between a DV route and a different one. The first verified value wins for the
// life of the session; a binding move to a different effective file — a
// substitution, or a same-row virtual release rotation that changes the
// candidate URI — does not match the pin's identity and re-arms it from that
// file's first read.
//
// Profile is 0 until a source has been verified and the pin still names the
// file identity it was read from. The identity is carried even at profile zero
// so that a move to an SDR file records the new bytes and disarms the previous
// file's profile; the zero itself is never evidence and never applies to a
// file, so an early row whose profile merely failed to probe does not lock the
// session out of a later, genuine profile.
type DVPinV3 struct {
	FileID  int
	Source  string
	Profile int
}

// dvPinMatchesFileV3 reports whether the pin names exactly this media file.
// Both the row id and the source identity must agree: a virtual catalog row can
// keep its id while its pinned candidate URI rotates, and the rotated release's
// bytes must not inherit the previous release's pinned profile.
func dvPinMatchesFileV3(pin DVPinV3, file *models.MediaFile) bool {
	if file == nil || pin.FileID != file.ID || pin.Profile <= 0 {
		return false
	}
	return pin.Source == file.FilePath
}

// PinDVPinV3 returns the pin to use for file given the session's current pin
// and a freshly read profile. First-verified-wins for the same file identity:
// once a positive profile is pinned for a file, a later read of that same file
// never lowers it. A different file identity always owns the pin outright,
// even when the new file verified no profile — a move from a Dolby Vision file
// to an SDR one must re-arm the pin to the SDR identity at profile zero, or the
// old profile would keep describing bytes it never came from. A nil file
// leaves the pin unchanged.
func PinDVPinV3(pin DVPinV3, file *models.MediaFile, profile int) DVPinV3 {
	if file == nil {
		return pin
	}
	if pin.FileID == file.ID && pin.Source == file.FilePath && pin.Profile > 0 {
		return pin
	}
	return DVPinV3{FileID: file.ID, Source: file.FilePath, Profile: profile}
}

// ApplyDVPinToFileV3 overwrites a loaded media file's primary video-track
// Dolby Vision profile with the session's pinned value when the pin names that
// same file identity. It is how a replan feeds stable DV evidence into the
// planner without touching catalog state: the file is a per-request load,
// never the shared catalog row.
func ApplyDVPinToFileV3(file *models.MediaFile, pin DVPinV3) {
	if !dvPinMatchesFileV3(pin, file) || len(file.VideoTracks) == 0 {
		return
	}
	file.VideoTracks[0].DVProfile = pin.Profile
}
