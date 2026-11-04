package handlers

import (
	"context"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

func subtitleInventoryWaitBlockerV3(session *playback.Session, file *models.MediaFile) string {
	if session == nil || file == nil {
		return subtitleWaitBlockerRelay
	}
	if file.ProbeUpdatedAt == nil {
		return subtitleWaitBlockerUnprobed
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
