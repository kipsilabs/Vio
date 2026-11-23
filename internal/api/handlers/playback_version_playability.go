package handlers

import (
	"context"
	"fmt"
	"time"

	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// EvaluateVersionPlayability plans each authorized version row against the
// client capability block the versions check carried. It reuses the production
// planner, planner settings, transformation registry and DV probe seams, so a
// verdict is the same terminal the start would produce instead of a second
// notion of client capability.
//
// Only a non-retryable terminal is a verdict. A retryable terminal, a
// capability probe that did not finish, and an unwired planner all leave the
// row's verdict absent, which callers must read as unknown: the row stays
// selectable and the start retries it. That split keeps an anchor-budget
// expiry or a provider hiccup from hiding a version that may play on the next
// attempt.
func (h *PlaybackHandler) EvaluateVersionPlayability(ctx context.Context, files []*models.MediaFile, client VersionPlayabilityClient) map[int]VersionPlayability {
	out := make(map[int]VersionPlayability, len(files))
	if h == nil || len(files) == 0 {
		return out
	}
	// The settings store drives every terminal policy below it (transcoding,
	// 4K, tone-map, HEVC). A failed read is not evidence about a version, so
	// the whole pass reports unknown rather than a capability refusal.
	settings, err := h.plannerSettingsV3Result(ctx)
	if err != nil {
		return out
	}
	// A probe error means the registry may be missing transformations the
	// server actually has. Planning against it would turn the probe failure
	// into a permanent-looking refusal, so answer unknown.
	registry, registryErr := h.probedTransformationRegistryV3(ctx)
	if registryErr != nil {
		return out
	}
	profileID := apimw.GetProfileID(ctx)
	for _, file := range files {
		if file == nil || file.ID <= 0 {
			continue
		}
		if ctx.Err() != nil {
			// The pass ran out of its own budget. Remaining rows are unknown,
			// not unplayable.
			return out
		}
		start := playback.StartRequestV3{
			ProtocolVersion:   playback.ProtocolV3,
			ClientFeatures:    append([]string(nil), client.ClientFeatures...),
			FileID:            file.ID,
			ProfileID:         profileID,
			PlaybackAttemptID: fmt.Sprintf("version-check-%d", file.ID),
			// Auto quality keeps the verdict about whether the row can play at
			// all. A viewer's resolution cap may still be served by a
			// degradation path, and folding it in here would hide a row the
			// start would play at source quality.
			QualityPreference:          playback.QualityAutoV3,
			SubtitleFidelityPreference: playback.SubtitleFidelityCompatibleV3,
			FileSelection:              playback.FileSelectionAutoV3,
			Capabilities:               client.Capabilities,
			ClientPlaybackContext:      client.PlaybackContext,
		}
		result, capabilityErr := h.planPlaybackWithCapabilitiesV3(ctx, playback.PlannerInputV3{
			Request:       start,
			RequestedFile: file,
			EffectiveFile: file,
			// Auto audio selection: playability is not a verdict about one
			// audio track. A row with any playable track stays playable.
			AudioTrackIndex: -1,
			Settings:        settings,
			Registry:        registry,
			DVRPUStrippable: h.lazyDVRPUStrippableV3(ctx, file),
			Now:             time.Now(),
		})
		if capabilityErr != nil {
			// Tone-map discovery could not complete; the same start would
			// report a retryable capability failure, not this row's defect.
			continue
		}
		if result.Terminal == nil {
			out[file.ID] = VersionPlayability{Playable: true}
			continue
		}
		if result.Terminal.Retryable {
			// Transient by contract (an anchor budget, an incomplete probe, a
			// missing toolchain): the row stays selectable.
			continue
		}
		out[file.ID] = VersionPlayability{
			Playable: false,
			Reason:   result.Terminal.Reason,
			Message:  result.Terminal.Message,
		}
	}
	return out
}
