package handlers

import (
	"net/http"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// audioStreamOrdinalV3 translates a selected audio track's array position in
// file.AudioTracks to the audio-only stream ordinal ffmpeg's `0:a:N` expects.
// It delegates to the shared playback.AudioStreamOrdinal so v3 and jellycompat
// convert through one implementation; see that function for the two index
// domains and the synthesized-track fallback.
func audioStreamOrdinalV3(file *models.MediaFile, selected int) int {
	if file == nil {
		return selected
	}
	return playback.AudioStreamOrdinal(file.AudioTracks, selected)
}

func subtitleInventoryWaitBlockerV3(session *playback.Session, file *models.MediaFile) string {
	if session == nil || file == nil {
		return subtitleWaitBlockerRelay
	}
	// An unprobed row is only an enumeration gate once the session has an
	// armed deferred probe. Before that, the relay is the authority holding
	// inventory back; do not infer a probe from the effective row alone.
	if file.ProbeUpdatedAt == nil {
		if session.VirtualProbeOutcome == playback.VirtualProbeOutcomePending {
			return subtitleWaitBlockerUnprobed
		}
		return subtitleWaitBlockerRelay
	}
	return subtitleWaitBlockerStale
}

func (h *PlaybackHandler) degradeStartSubtitleInPlaceV3(r *http.Request, req playback.StartRequestV3, requested, effective *models.MediaFile, audio int, settings playback.PlannerSettingsV3, provenance ProbeProvenance) (playback.StartRequestV3, playback.PlannerResultV3, error, bool) {
	degraded := req
	degraded.SubtitleTrackIndex = nil
	degraded.SubtitleTrackID = ""
	result, err := h.planPlaybackWithCapabilitiesV3(r.Context(), playback.PlannerInputV3{Request: degraded, RequestedFile: requested, EffectiveFile: effective, AudioTrackIndex: audio, Settings: settings, Now: time.Now(), InventoryProvenance: string(provenance), AdditionalSubtitles: h.downloadedSubtitleInventoryV3(r.Context(), effective)})
	if result.Terminal != nil {
		return req, result, err, false
	}
	return degraded, result, err, true
}
