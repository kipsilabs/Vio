package scanner

import (
	"context"

	"github.com/Silo-Server/silo-server/internal/models"
)

// virtualTrackEnumSource labels the scratch probe used by the bounded track
// enumeration. It never reaches a catalog row: the enumeration returns only the
// track slices, and the real ProbeVirtualSource remains the persisted verifier.
const virtualTrackEnumSource = "virtual_tracks"

// enumerateStreamsArgs returns the bounded, metadata-only argv for start-path
// track discovery. It asks for the stream table (audio and subtitles) but omits
// the chapter table, so ffprobe never runs the packet-scan duration fallback the
// full probe uses. It keeps the same -probesize/-analyzeduration window as the
// full probe so the streams it enumerates are the streams the verifier would
// find; only the analysis work around them is dropped.
func enumerateStreamsArgs(filePath string) []string {
	return []string{
		"-v", "quiet",
		"-print_format", "json",
		"-show_streams",
		"-probesize", probeSizeLimit,
		"-analyzeduration", probeAnalyzeDuration,
		filePath,
	}
}

// EnumerateVirtualSourceTracks runs a bounded, metadata-only ffprobe against an
// already-resolved source and returns the enumerated audio and subtitle tracks.
//
// It is the virtual start path's fast track-discovery. The fresh-start resolve
// defers the real probe past the transport commit, so without this the cold plan
// is built from the provider's declared metadata: a multi-audio release whose
// declaration collapses to one language-less track reaches the planner with a
// single audio stream and no subtitle streams. Enumerating here, before the plan
// is built, lets the plan carry the real inventory. It deliberately does NOT run
// the packet-scan duration fallback, the H.264 copy-safety scan, or the Dolby
// Vision strip probe, and it persists nothing — the deferred ProbeVirtualSource
// is still the verifier that stamps and publishes the row.
//
// A successful ffprobe with no audio and no video stream at all is
// inconclusive (a relay can return an empty table when the analysis window is
// too short), so it is reported as ErrVirtualProbeNoTracks and the caller falls
// back to the declared inventory. A file that legitimately carries video but
// zero audio and zero subtitle streams is a valid empty inventory and returns
// successfully with empty slices.
func EnumerateVirtualSourceTracks(ctx context.Context, ffprobePath, sourceURL string) ([]models.AudioTrack, []models.SubtitleTrack, error) {
	probe, _, err := runProbeFFprobe(ctx, ffprobePath, enumerateStreamsArgs(sourceURL), sourceURL)
	if err != nil {
		return nil, nil, err
	}
	if probe == nil || (len(probe.AudioTracks) == 0 && len(probe.VideoTracks) == 0) {
		return nil, nil, ErrVirtualProbeNoTracks
	}
	// Reuse the scanner's one authoritative ProbeData -> models mapping so the
	// discovered tracks are identical in shape to a full probe's.
	var scratch models.MediaFile
	applyProbeData(&scratch, probe, virtualTrackEnumSource)
	return scratch.AudioTracks, scratch.SubtitleTracks, nil
}
