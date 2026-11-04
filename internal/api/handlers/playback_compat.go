package handlers

import (
	"context"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"net/http"
	"time"
)

func audioStreamOrdinalV3(file *models.MediaFile, selected int) int {
	if file == nil || selected < 0 || selected >= len(file.AudioTracks) {
		return -1
	}
	tracks := file.AudioTracks
	known := false
	for _, t := range tracks {
		if t.Index > 0 {
			known = true
			break
		}
	}
	if !known {
		return selected
	}
	ordinal := 0
	for i, t := range tracks {
		if i == selected {
			return ordinal
		}
		if t.Index > 0 {
			ordinal++
		}
	}
	return -1
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

func (h *PlaybackHandler) carrySubtitleFromPlanV3(ctx context.Context, req *playback.StartRequestV3, plan playback.PlanV3, file *models.MediaFile) bool {
	if req == nil || file == nil || plan.SelectedTracks.Subtitle == nil {
		return false
	}
	item := plan.SelectedTracks.Subtitle
	if item.Index != nil {
		inv := playback.BuildSubtitleInventoryV3(file, nil)
		if *item.Index >= 0 && *item.Index < len(inv) && inv[*item.Index].Source == playback.SubtitleSourceEmbeddedV3 {
			req.SubtitleTrackIndex = item.Index
			return false
		}
	}
	return true
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
