package handlers

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/Silo-Server/silo-server/internal/lang"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/settingscontract"
	"github.com/Silo-Server/silo-server/internal/settingskeys"
	"github.com/Silo-Server/silo-server/internal/settingsresolve"
	"github.com/Silo-Server/silo-server/internal/userstore"
)

// playbackSubtitlePreferenceV3 is the effective subtitle preference for one
// playback context, resolved through the same canonical settings manifest the
// catalog uses to publish effective_subtitle_mode/effective_subtitle_language.
type playbackSubtitlePreferenceV3 struct {
	Language      string
	HasLanguage   bool
	Mode          string
	HasMode       bool
	ShowForced    bool
	HasShowForced bool
}

// resolvedPlaybackSubtitlePreferencesV3 resolves playback.subtitle_language,
// playback.subtitle_mode and playback.show_forced_subtitles for one settings
// context. A value that resolved to the contract default is reported as unset
// through the Has* flags, so "no preference" never reads as the user choosing
// an empty language. Resolve errors are returned so playback does not silently
// substitute a different selection.
func resolvedPlaybackSubtitlePreferencesV3(ctx context.Context, store userstore.UserStore, rc settingsresolve.Context) (playbackSubtitlePreferenceV3, error) {
	var out playbackSubtitlePreferenceV3
	if store == nil || rc.ProfileID == "" {
		return out, nil
	}
	contract, err := settingscontract.Load()
	if err != nil {
		return out, err
	}
	resolved, err := settingsresolve.New(contract).Resolve(ctx, store, rc, []string{
		settingskeys.PlaybackSubtitleLanguage,
		settingskeys.PlaybackSubtitleMode,
		settingskeys.PlaybackShowForcedSubtitles,
	}, nil)
	if err != nil {
		return out, err
	}
	for _, eff := range resolved {
		stored := eff.Source != settingscontract.ScopeDefault
		switch eff.Key {
		case settingskeys.PlaybackSubtitleLanguage:
			var language string
			if json.Unmarshal(eff.Value, &language) == nil && stored {
				out.Language, out.HasLanguage = strings.TrimSpace(language), true
			}
		case settingskeys.PlaybackSubtitleMode:
			var mode string
			if json.Unmarshal(eff.Value, &mode) == nil && stored {
				out.Mode, out.HasMode = strings.TrimSpace(mode), true
			}
		case settingskeys.PlaybackShowForcedSubtitles:
			var forced bool
			if json.Unmarshal(eff.Value, &forced) == nil && stored {
				out.ShowForced, out.HasShowForced = forced, true
			}
		}
	}
	return out, nil
}

// resolvePreferredSubtitleSelectionV3 answers what an omitted subtitle
// selection means when the profile's persisted subtitle mode is "always".
// Planning with subtitles off and then discovering the burn-in requirement
// after the first spawn forces a second plan and a second FFmpeg start (the
// AV1-then-H.264 restart the fast-start work targets). Resolving the selection
// here lets the very first plan see the burn-in requirement and pick the
// burn-in codec. Only an explicitly stored "always" mode is honored: "auto"
// depends on the audio track and is the client's contextual decision, and an
// omitted mode leaves selection to the client. An explicit per-series track
// signature wins over language matching, mirroring the audio path.
//
// Returns ok=false when there is no preference, the mode is not "always", the
// inventory is empty, or nothing matches; the request is then planned exactly as
// before.
func (h *PlaybackHandler) resolvePreferredSubtitleSelectionV3(
	ctx context.Context,
	userID int,
	profileID, deviceID string,
	file *models.MediaFile,
	additional []playback.SubtitleInventoryEntryV3,
) (index int, ok bool) {
	if h == nil || h.StoreProvider == nil || file == nil || profileID == "" {
		return 0, false
	}
	inventory := playback.BuildSubtitleInventoryV3(file, additional)
	if len(inventory) == 0 {
		return 0, false
	}
	store, err := h.StoreProvider.ForUser(ctx, userID)
	if err != nil || store == nil {
		slog.ErrorContext(ctx, "protocol v3 start: subtitle preference store lookup failed", "component", "api", "user_id", userID, "error", err)
		return 0, false
	}
	rc := settingsresolve.Context{ProfileID: profileID, DeviceID: deviceID}
	if file.MediaFolderID > 0 {
		rc.LibraryIDs = []int{file.MediaFolderID}
	}
	seriesID := h.resolveSeriesID(ctx, file)
	if seriesID != "" {
		rc.SeriesIDs = []string{seriesID}
	}
	prefs, err := resolvedPlaybackSubtitlePreferencesV3(ctx, store, rc)
	if err != nil {
		slog.ErrorContext(ctx, "protocol v3 start: subtitle preference resolve failed", "component", "api", "profile_id", profileID, "error", err)
		return 0, false
	}
	if !prefs.HasMode || !strings.EqualFold(prefs.Mode, "always") {
		return 0, false
	}
	var seriesSignature *userstore.SubtitleTrackSignature
	if seriesID != "" {
		if stored, prefErr := store.GetSubtitlePreference(ctx, profileID, seriesID); prefErr == nil && stored != nil {
			seriesSignature = stored.TrackSignature
		}
	}
	selected, found := selectPreferredSubtitleV3(inventory, prefs, seriesSignature)
	if !found {
		return 0, false
	}
	return selected.CombinedIndex, true
}

// selectPreferredSubtitleV3 picks the inventory entry the stored preference
// names: an exact track signature when one is stored, otherwise the closest
// language match. Ties break toward a non-forced track and then toward the
// published inventory order, which is the order the client renders. A forced
// track is selected only when the preference opts into forced subtitles;
// otherwise the selection is left to the client rather than silently swapping
// in a different deliverable.
func selectPreferredSubtitleV3(
	inventory []playback.SubtitleInventoryItemV3,
	prefs playbackSubtitlePreferenceV3,
	signature *userstore.SubtitleTrackSignature,
) (playback.SubtitleInventoryItemV3, bool) {
	if signature != nil && !signature.IsZero() {
		if item, ok := matchSubtitleSignatureV3(inventory, signature); ok {
			return item, true
		}
	}
	preferred := strings.TrimSpace(prefs.Language)
	if !prefs.HasLanguage || preferred == "" {
		return playback.SubtitleInventoryItemV3{}, false
	}
	bestIndex := -1
	bestRank := -1
	// Track the best non-forced match separately: a forced track can outrank an
	// available non-forced one (an exact forced tag beats a regional non-forced
	// variant), and without the fallback the whole selection is abandoned even
	// though a legitimate non-forced deliverable exists.
	bestNonForcedIndex := -1
	bestNonForcedRank := -1
	for i, item := range inventory {
		rank := subtitleLanguageRankV3(item.Language, preferred)
		if rank < 0 || rank >= lang.RankScriptConflict {
			continue
		}
		forced := item.Forced
		if bestIndex < 0 ||
			rank < bestRank ||
			(rank == bestRank && inventory[bestIndex].Forced && !forced) {
			bestIndex, bestRank = i, rank
		}
		if !forced && (bestNonForcedIndex < 0 || rank < bestNonForcedRank) {
			bestNonForcedIndex, bestNonForcedRank = i, rank
		}
	}
	if bestIndex < 0 {
		return playback.SubtitleInventoryItemV3{}, false
	}
	item := inventory[bestIndex]
	if item.Forced && (!prefs.HasShowForced || !prefs.ShowForced) {
		// A forced track is a different deliverable from the full track and is
		// only selected when the preference opts in. Fall back to the closest
		// non-forced match rather than silently forcing a different subtitle or
		// abandoning a selectable one.
		if bestNonForcedIndex >= 0 {
			return inventory[bestNonForcedIndex], true
		}
		return playback.SubtitleInventoryItemV3{}, false
	}
	return item, true
}

// matchSubtitleSignatureV3 finds the inventory entry a stored per-series track
// signature names, matching only the traits the signature actually carries.
// Language is ranked rather than required to be equal, but a script conflict is
// rejected exactly as the language branch of selectPreferredSubtitleV3 rejects
// it: a positive lang.RankScriptConflict (e.g. zh-Hans against zh-Hant) is not a
// compatible match, and the exact tag is preferred over a regional variant.
func matchSubtitleSignatureV3(inventory []playback.SubtitleInventoryItemV3, signature *userstore.SubtitleTrackSignature) (playback.SubtitleInventoryItemV3, bool) {
	bestIndex := -1
	bestRank := -1
	for i, item := range inventory {
		if signature.Source != "" && !strings.EqualFold(item.Source, signature.Source) {
			continue
		}
		if signature.Codec != "" && !strings.EqualFold(item.Codec, signature.Codec) {
			continue
		}
		if signature.Label != "" && !strings.EqualFold(item.Label, signature.Label) {
			continue
		}
		if signature.Forced != item.Forced {
			continue
		}
		if signature.HearingImpaired != item.HearingImpaired {
			continue
		}
		if signature.Language == "" {
			return item, true
		}
		rank := subtitleLanguageRankV3(item.Language, signature.Language)
		if rank < 0 || rank >= lang.RankScriptConflict {
			continue
		}
		if bestIndex < 0 || rank < bestRank {
			bestIndex, bestRank = i, rank
		}
	}
	if bestIndex < 0 {
		return playback.SubtitleInventoryItemV3{}, false
	}
	return inventory[bestIndex], true
}

// subtitleLanguageRankV3 ranks a track language against the preference, or -1
// when there is no usable tag. Empty candidate tags carry no evidence and never
// match.
func subtitleLanguageRankV3(candidate, preferred string) int {
	if strings.TrimSpace(candidate) == "" || strings.TrimSpace(preferred) == "" {
		return -1
	}
	return lang.MatchRank(candidate, preferred)
}
