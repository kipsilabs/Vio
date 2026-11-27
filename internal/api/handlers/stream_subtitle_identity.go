package handlers

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/url"
	"strconv"
	"strings"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

var (
	errSubtitleIdentityInvalid     = errors.New("invalid subtitle identity")
	errSubtitleIdentityUnavailable = errors.New("the selected subtitle identity is unavailable")
	// errSubtitleSourceChangedAfterRotation reports a subtitle URL minted before
	// a virtual candidate rotation whose carried ordinal the rotation could not
	// preserve. The serve path answers the existing retryable
	// subtitle_source_changed 409 without resolving the dead pin or spawning
	// ffmpeg; the client replans and re-selects from the rotated inventory.
	errSubtitleSourceChangedAfterRotation = errors.New("the selected subtitle changed after a candidate rotation")
)

// resolveSubtitleEditionSwitch re-maps a subtitle request that names the
// session's requested (old) edition after an edition switch moved the effective
// file. The requested edition's inventory is not what is playing: interpreting
// its ordinal against the effective file can serve a different language.
//
// The inventory translation itself is remapSubtitleInventoryIdentityV3. The
// named file is treated as foreign unless the request's pinned identity proves
// it exists in the current plan's inventory. A pin that resolves against the
// effective file does prove it, and the effective file with that resolved
// ordinal is returned. A named row with no subtitle inventory is a catalog
// placeholder with nothing to remap FROM, so its plan-time ordinal already
// names the effective inventory. Anything else is translated from the named
// file's segment identity (external path/language or embedded
// language/codec/flags) onto the effective file's inventory; a miss returns
// errSubtitleIdentityUnavailable so the client degrades to subtitles-off
// instead of being served a wrong track.
func (h *StreamHandler) resolveSubtitleEditionSwitch(
	ctx context.Context,
	namedFile, effectiveFile *models.MediaFile,
	index int,
	query url.Values,
) (*models.MediaFile, int, error) {
	if namedFile == nil || effectiveFile == nil {
		return namedFile, index, nil
	}
	// The unchanged-edition shortcut lives here, at the edition boundary, and
	// never in the inventory remapper below. A virtual rotation can re-probe the
	// same catalog row in place (same row id, different release), so an
	// equal-id check inside the remapper would turn every same-row rotation
	// into a no-op and serve the stale ordinal.
	if namedFile.ID == effectiveFile.ID {
		return namedFile, index, nil
	}
	return h.remapSubtitleInventory(ctx, namedFile, effectiveFile, index, query)
}

// remapSubtitleInventory translates a selection minted against namedFile's
// inventory onto effectiveFile's. It is the rotation path and must be called
// only when the two inventories genuinely differ, whatever their row ids: a
// same-row re-probe has different track layouts behind an identical id, and
// short-circuiting on the id would serve the new release at the old ordinal.
func (h *StreamHandler) remapSubtitleInventory(
	ctx context.Context,
	namedFile, effectiveFile *models.MediaFile,
	index int,
	query url.Values,
) (*models.MediaFile, int, error) {
	// Pin shape is validated on every entry to this remapper, not only on the
	// normal serving path: a malformed combination (mixed pins, repeated pins)
	// must be rejected even when the named inventory is nonempty. Without this
	// check the pinned fast path below would discard the shape error and fall
	// through to the identity translation, which ignores the query and can map
	// the stale ordinal onto an equivalent track as if the pins were valid.
	if err := validateSubtitleIdentityPinShape(query); err != nil {
		return nil, 0, err
	}
	// A validated downloaded pin is file-bound: resolve it against the
	// effective file's own downloaded list and return its published ordinal.
	// It must never reach the generic pin shortcut below, which has no
	// downloaded-row lookup and would return the historical ordinal unchanged
	// — or succeed even when the effective file does not own that row.
	if pin := query.Get(playback.DownloadedSubtitleIDParamV3); pin != "" {
		return h.resolveDownloadedSubtitle(ctx, effectiveFile, pin)
	}
	// A pinned identity that resolves against the effective file is the
	// current plan's own track: honor it directly. Only a shape-invalid pin
	// returns here; an unavailable pin falls through to the identity
	// translation below so a stale pin can still be remapped by language.
	if pinned := hasSubtitleIdentityPin(query); pinned {
		if resolved, err := subtitleRouteIndex(effectiveFile, index, query); err == nil {
			return effectiveFile, resolved, nil
		} else if errors.Is(err, errSubtitleIdentityInvalid) {
			return nil, 0, err
		}
	}
	// A named row with no subtitle inventory has nothing to remap FROM: it is
	// a catalog placeholder, and any selection the client made was against a
	// resolved candidate's track list. The plan-time ordinal already names the
	// effective inventory.
	//
	// Callers pass only a genuine placeholder here. Captured-empty evidence is
	// rejected before it gets this far, so the fallback can never reinterpret
	// an explicitly captured empty inventory against the replacement.
	if len(namedFile.ExternalSubtitles) == 0 && len(namedFile.SubtitleTracks) == 0 {
		if resolved, err := subtitleRouteIndex(effectiveFile, index, query); err == nil {
			return effectiveFile, resolved, nil
		} else if errors.Is(err, errSubtitleIdentityInvalid) {
			return nil, 0, err
		}
		return nil, 0, errSubtitleIdentityUnavailable
	}
	return h.remapSubtitleInventoryIdentityV3(ctx, namedFile, effectiveFile, index)
}

// remapSubtitleInventoryIdentityV3 translates a selection minted against
// oldFile's inventory onto the bound candidate's inventory and returns the
// bound file with the mapped published ordinal. Identity is decided by track
// layout, never by file id: a same-row candidate rotation re-probes the catalog
// row in place (same id, different release), so id equality proves nothing
// here. The returned file is always the bound candidate, and a target with no
// equivalent returns errSubtitleIdentityUnavailable rather than an unrelated
// ordinal.
func (h *StreamHandler) remapSubtitleInventoryIdentityV3(
	ctx context.Context,
	oldFile, bound *models.MediaFile,
	index int,
) (*models.MediaFile, int, error) {
	if oldFile == nil || bound == nil {
		return bound, index, nil
	}
	location, ok := classifySubtitleIndexV3(oldFile, index)
	if !ok {
		return nil, 0, errSubtitleIdentityUnavailable
	}
	switch location.source {
	case playback.SubtitleSourceExternalV3:
		wanted := oldFile.ExternalSubtitles[location.offset]
		for candidateIndex, candidate := range bound.ExternalSubtitles {
			equivalent := strings.EqualFold(candidate.Language, wanted.Language) &&
				strings.EqualFold(candidate.Format, wanted.Format) &&
				candidate.Forced == wanted.Forced &&
				candidate.HearingImpaired == wanted.HearingImpaired
			if candidate.Path != wanted.Path && !equivalent {
				continue
			}
			if published, mapped := playback.SubtitleInventoryOwnPublishedIndexV3(bound, candidateIndex); mapped {
				return bound, published, nil
			}
		}
	case playback.SubtitleSourceEmbeddedV3:
		wanted := oldFile.SubtitleTracks[location.offset]
		if ordinal, _, matched := playback.MatchEmbeddedSubtitleTrack(wanted, bound.SubtitleTracks); matched {
			if published, mapped := playback.SubtitleInventoryOwnPublishedIndexV3(bound, len(bound.ExternalSubtitles)+ordinal); mapped {
				return bound, published, nil
			}
		}
	default:
		// Downloaded rows are file-bound. Only a stable row-id match against the
		// bound file's own downloaded list may survive.
		if h.SubtitleRepo != nil {
			wantedList, wantedErr := h.SubtitleRepo.ListDownloadedSubtitles(ctx, oldFile.ID)
			targetList, targetErr := h.SubtitleRepo.ListDownloadedSubtitles(ctx, bound.ID)
			if wantedErr == nil && targetErr == nil && location.offset >= 0 && location.offset < len(wantedList) {
				wanted := wantedList[location.offset]
				base := len(playback.BuildSubtitleInventoryV3(bound, nil))
				for candidateIndex, candidate := range targetList {
					if wanted.ID > 0 && candidate.ID == wanted.ID {
						return bound, base + candidateIndex, nil
					}
				}
			}
		}
	}
	slog.InfoContext(ctx, "subtitle inventory remap has no equivalent track on the bound file",
		"component", "api", "old_file_id", oldFile.ID, "bound_file_id", bound.ID, "track_index", index)
	return nil, 0, errSubtitleIdentityUnavailable
}

// remapSubtitleFromEvidenceV3 resolves a request's ordinal and any stale
// identity pin against the plan-time evidence, then translates the named track
// onto the bound candidate. It is the rotation-entry counterpart of
// resolveSubtitleEditionSwitch: the evidence, not the bound row, is the
// inventory the request was minted against, so the pin/ordinal is resolved
// there before the identity is mapped. The bound candidate is always the
// returned file, and captured-empty evidence yields unavailable rather than an
// ordinal reinterpreted against the replacement.
//
// A downloaded pin never reaches the evidence inventory: downloaded rows are
// file-bound, so the pin is resolved against the bound file's own downloaded
// list independently of evidence emptiness and the historical ordinal. Pin
// shape is validated first so malformed combinations are rejected on every
// path, not only when the evidence is nonempty.
func (h *StreamHandler) remapSubtitleFromEvidenceV3(
	ctx context.Context,
	evidence, bound *models.MediaFile,
	index int,
	query url.Values,
) (*models.MediaFile, int, error) {
	if err := validateSubtitleIdentityPinShape(query); err != nil {
		return nil, 0, err
	}
	if pin := query.Get(playback.DownloadedSubtitleIDParamV3); pin != "" {
		return h.resolveDownloadedSubtitle(ctx, bound, pin)
	}
	evidenceIndex, err := subtitleRouteIndex(evidence, index, query)
	if err != nil {
		return nil, 0, err
	}
	return h.remapSubtitleInventoryIdentityV3(ctx, evidence, bound, evidenceIndex)
}

// resolveDownloadedSubtitle serves a validated downloaded-subtitle row id
// against one file's own downloaded list, returning the published combined
// ordinal. Downloaded rows are file-bound rather than plan-bound, so a rotation
// that replaced the plan-time embedded/external inventory does not retire them:
// the row id keeps identifying the same artifact.
//
// The edition switch resolves a validated downloaded pin the same way, against
// the effective file's own downloaded list via remapSubtitleInventory before
// the generic pin shortcut. A downloaded pin never falls through to the
// ordinal passthrough: only a row-id match on the effective file may serve it,
// and the published ordinal comes from that file's own list.
func (h *StreamHandler) resolveDownloadedSubtitle(ctx context.Context, file *models.MediaFile, rowID string) (*models.MediaFile, int, error) {
	if h.SubtitleRepo == nil || file == nil {
		return nil, 0, errSubtitleIdentityUnavailable
	}
	id, err := strconv.Atoi(strings.TrimSpace(rowID))
	if err != nil || id <= 0 {
		return nil, 0, errSubtitleIdentityInvalid
	}
	targetList, err := h.SubtitleRepo.ListDownloadedSubtitles(ctx, file.ID)
	if err != nil {
		return nil, 0, err
	}
	base := len(playback.BuildSubtitleInventoryV3(file, nil))
	for candidateIndex, candidate := range targetList {
		if candidate.ID == id {
			return file, base + candidateIndex, nil
		}
	}
	return nil, 0, errSubtitleIdentityUnavailable
}

// resolveSubtitleSourceRequest resolves the media file and combined ordinal a
// subtitle request serves from. It first binds the session's planned virtual
// candidate, then handles two ordinal-space mismatches:
//
//   - After an edition switch the session's effective file differs from the
//     requested edition. A request naming the requested (old) edition must not
//     interpret its ordinal against that stale inventory.
//   - After a provider candidate rotation the catalog row was re-probed in
//     place (same id, different release). The bound file is the new candidate,
//     but the request's ordinal was minted against the plan-time evidence the
//     session carried. The ordinal must be re-mapped from that evidence onto
//     the bound file rather than interpreted against the new inventory.
func (h *StreamHandler) resolveSubtitleSourceRequest(
	ctx context.Context,
	namedFile *models.MediaFile,
	session *playback.Session,
	index int,
	query url.Values,
) (*models.MediaFile, int, error) {
	if namedFile == nil || session == nil {
		return namedFile, index, nil
	}
	// Pin shape is validated before the edition branch so the edition-switch
	// fast path can never accept a malformed combination (mixed pins, repeated
	// pins) the normal path rejects. The remapper re-validates on entry for
	// independently callable use; invalid pins are returned, never remapped.
	if err := validateSubtitleIdentityPinShape(query); err != nil {
		return nil, 0, err
	}
	// A committed candidate rotation carried the viewer's subtitle selection to
	// a new ordinal. A URL minted before the rotation still names the old one,
	// so it is answered on the carried ordinal; when the rotation could not
	// preserve the selection, that exact ordinal is terminal and answers the
	// retryable source-changed verdict instead of resolving the dead candidate.
	// A stable identity pin is a fresh address minted from the current
	// inventory, so it is never rewritten by a committed carry.
	if !hasSubtitleIdentityPin(query) {
		if session.VirtualSubtitleCarryLostSet && index == session.VirtualSubtitleCarryLostIndex {
			return nil, 0, errSubtitleSourceChangedAfterRotation
		}
		if session.VirtualSubtitleCarrySet && index == session.VirtualSubtitleCarryFromIndex &&
			session.VirtualSubtitleCarryFromIndex != session.VirtualSubtitleCarryToIndex {
			index = session.VirtualSubtitleCarryToIndex
		}
	}
	// After an edition switch the session's effective file differs from the
	// requested edition. A request naming the requested (old) edition must not
	// interpret its ordinal against that stale inventory.
	if session.MediaFileID > 0 && session.MediaFileID != namedFile.ID {
		effective, err := h.fileResolver.GetByID(ctx, session.MediaFileID)
		if err != nil || effective == nil {
			return nil, 0, errors.New("media file not found")
		}
		effective = bindSessionVirtualSourceWithTracks(ctx, effective, session, h.fileResolver)
		// Different row ids, so the edition boundary does not apply: remap the
		// inventory directly.
		resolvedFile, resolvedIndex, switchErr := h.remapSubtitleInventory(ctx, namedFile, effective, index, query)
		if switchErr != nil {
			return nil, 0, switchErr
		}
		return resolvedFile, resolvedIndex, nil
	}

	bound := bindSessionVirtualSourceWithTracks(ctx, namedFile, session, h.fileResolver)
	// A virtual session whose carried evidence names a different candidate than
	// the bound file: the request's ordinal (and any container-index pin) was
	// minted against that evidence's inventory. Resolve it there first, then
	// translate the evidence-local ordinal onto the bound release by
	// language/class instead of serving the bound release at a stale ordinal.
	// Captured-empty evidence is genuine evidence with no old track to name, so
	// it yields unavailable rather than reinterpreting an ordinal against the
	// replacement.
	if isVirtualPlaybackFile(bound) && session.VirtualSubtitleEvidenceSet &&
		!virtualEvidenceMatchesBoundFile(ctx, bound, session, h.fileResolver) {
		// The evidence file shares the bound file's row id, so the edition
		// shortcut would discard the remap entirely. Call the remapper: this
		// is exactly the same-row rotation case it exists for.
		if evidence := virtualEvidenceFileV3(bound, session); evidence != nil {
			// Captured-empty evidence says the old release had no subtitle
			// tracks. There is no identity to carry onto the replacement, so
			// report unavailable rather than let the ordinal land on whatever
			// track the replacement happens to publish at that position.
			//
			// A validated downloaded pin is the exception and is not
			// plan-bound: its identity survives a rotation that replaced the
			// plan-time inventory, so it is resolved against the bound row's
			// own downloaded list instead of being refused here. Pin shape is
			// validated first so the exception cannot accept a malformed
			// combination (mixed pins, repeated pins) the normal path rejects.
			if len(evidence.ExternalSubtitles) == 0 && len(evidence.SubtitleTracks) == 0 {
				if err := validateSubtitleIdentityPinShape(query); err != nil {
					return nil, 0, err
				}
				if downloadedSubtitlePin := query.Get(playback.DownloadedSubtitleIDParamV3); downloadedSubtitlePin != "" {
					return h.resolveDownloadedSubtitle(ctx, bound, downloadedSubtitlePin)
				}
				return nil, 0, errSubtitleIdentityUnavailable
			}
			// The query is threaded through rather than dropped: a validated
			// downloaded pin is file-bound and must survive the translation,
			// and an embedded/external pin must be resolved against the
			// evidence inventory the ordinal was minted against.
			resolvedFile, resolvedIndex, remapErr := h.remapSubtitleFromEvidenceV3(ctx, evidence, bound, index, query)
			if remapErr != nil {
				return nil, 0, remapErr
			}
			return resolvedFile, resolvedIndex, nil
		}
	}
	// Captured-empty evidence is authoritative on the serving path too, not
	// only when provenance mismatches. The plan promised this release has no
	// subtitle tracks, so a request naming one cannot be satisfied: falling
	// through to the bound inventory would serve a track the client never
	// selected. Absent evidence is the case where the live row may speak.
	//
	// Downloaded subtitles are file-bound rather than plan-bound, so they are
	// excluded from the evidence substitution above and stay resolvable against
	// the live row; only the plan-bound embedded/external inventories are gated.
	// A downloaded pin appended to an embedded/external ordinal never names a
	// plan track: it bypasses the download list's current offsets and resolves
	// the pinned row directly, so it is resolved first and exactly.
	if err := validateSubtitleIdentityPinShape(query); err != nil {
		return nil, 0, err
	}
	if pin := query.Get(playback.DownloadedSubtitleIDParamV3); pin != "" {
		return h.resolveDownloadedSubtitle(ctx, bound, pin)
	}
	if isVirtualPlaybackFile(bound) && session.VirtualSubtitleEvidenceSet &&
		virtualEvidenceMatchesBoundFile(ctx, bound, session, h.fileResolver) &&
		!hasApplicableSubtitleEvidence(session, query) {
		return nil, 0, errSubtitleIdentityUnavailable
	}

	resolvedIndex, err := subtitleRouteIndex(bound, index, query)
	if err != nil {
		return nil, 0, err
	}
	return bound, resolvedIndex, nil
}

// virtualEvidenceFileV3 builds the file whose inventory the session's carried
// subtitle evidence describes, sharing the bound file's identity/path. It is
// the remap source for a rotated candidate; nil only when no evidence was
// captured at all.
//
// The evidence-set flag, not slice length, is the signal. A candidate probed
// with no subtitle tracks is captured-empty evidence, and it must still
// describe the old release: falling back to the newly bound row would mint the
// request's ordinal against the replacement's inventory and serve a track the
// client never selected. Absence (flag unset) is the only case with nothing to
// remap from.
func virtualEvidenceFileV3(bound *models.MediaFile, session *playback.Session) *models.MediaFile {
	if bound == nil || session == nil || !session.VirtualSubtitleEvidenceSet {
		return nil
	}
	evidence := *bound
	evidence.SubtitleTracks = session.VirtualSubtitleTracks
	evidence.ExternalSubtitles = session.VirtualExternalSubtitles
	evidence.AudioTracks = session.VirtualAudioTracks
	return &evidence
}

// hasSubtitleIdentityPin reports whether the request carries one of the stable
// subtitle identity pins (embedded stream index, external path key, downloaded
// row id).
func hasSubtitleIdentityPin(query url.Values) bool {
	for _, key := range []string{
		playback.EmbeddedSubtitleStreamIndexParamV3,
		playback.ExternalSubtitleKeyParamV3,
		playback.DownloadedSubtitleIDParamV3,
	} {
		if query.Get(key) != "" {
			return true
		}
	}
	return false
}

// hasApplicableSubtitleEvidence reports whether the session captured a
// plan-bound subtitle inventory the request can be resolved against.
//
// Downloaded subtitles are excluded deliberately: they are bound to the file
// row rather than to the plan-time probe, so a candidate that probed with no
// embedded or external tracks may still legitimately have downloaded subtitles
// to serve. Only the embedded and external inventories travel as captured
// evidence, and only their absence makes a request unsatisfiable.
func hasApplicableSubtitleEvidence(session *playback.Session, query url.Values) bool {
	if session == nil {
		return false
	}
	if len(session.VirtualSubtitleTracks) > 0 || len(session.VirtualExternalSubtitles) > 0 {
		return true
	}
	// A downloaded-row pin is not plan-bound evidence; let it resolve against
	// the live file's downloaded list.
	return strings.TrimSpace(query.Get(playback.DownloadedSubtitleIDParamV3)) != ""
}

// validateSubtitleIdentityPinShape rejects malformed identity-pin
// combinations: a pin key repeated or blank, or pins from different identity
// types combined on one request. It is shared by every resolution path — the
// normal route, the evidence remap, and the captured-empty downloaded
// exception — and runs before any exception or early return, so a path that
// resolves a downloaded pin directly can never accept a combination the
// normal path rejects.
func validateSubtitleIdentityPinShape(query url.Values) error {
	pins := 0
	for _, key := range []string{
		playback.EmbeddedSubtitleStreamIndexParamV3,
		playback.ExternalSubtitleKeyParamV3,
		playback.DownloadedSubtitleIDParamV3,
	} {
		if values, ok := query[key]; ok {
			if len(values) != 1 || values[0] == "" {
				return errSubtitleIdentityInvalid
			}
			pins++
		}
	}
	if pins > 1 {
		return errSubtitleIdentityInvalid
	}
	return nil
}

// subtitleRouteIndex resolves a pinned identity before applying the combined
// ordinal dispatch. Old URLs without a pin retain their original behavior.
func subtitleRouteIndex(file *models.MediaFile, index int, query url.Values) (int, error) {
	if err := validateSubtitleIdentityPinShape(query); err != nil {
		return 0, err
	}
	if value := query.Get(playback.EmbeddedSubtitleStreamIndexParamV3); value != "" {
		streamIndex, err := strconv.Atoi(value)
		if err != nil || streamIndex < 0 {
			return 0, errSubtitleIdentityInvalid
		}
		match := -1
		for ordinal, track := range file.SubtitleTracks {
			if track.Index == streamIndex {
				if match >= 0 {
					return 0, errSubtitleIdentityUnavailable
				}
				match = len(file.ExternalSubtitles) + ordinal
			}
		}
		if match < 0 {
			// Virtual sources plan against provider-declared placeholder
			// tracks (Index 0, no codec) and then inherit the real probed
			// tracks at stream time, whose container indices differ. The
			// combined ordinal is the stable identity across that transition,
			// so fall back to it rather than 404ing the pinned URL. Local
			// files keep strict matching: a pin that no longer resolves means
			// the inventory changed and serving a different track would be
			// wrong.
			if isVirtualPlaybackFile(file) {
				return index, nil
			}
			return 0, errSubtitleIdentityUnavailable
		}
		return match, nil
	}
	if key := query.Get(playback.ExternalSubtitleKeyParamV3); key != "" {
		if _, err := hex.DecodeString(key); len(key) != 64 || err != nil {
			return 0, errSubtitleIdentityInvalid
		}
		match := -1
		for ordinal, subtitle := range file.ExternalSubtitles {
			if playback.ExternalSubtitlePathKeyV3(subtitle.Path) == key {
				if match >= 0 {
					return 0, errSubtitleIdentityUnavailable
				}
				match = ordinal
			}
		}
		if match < 0 {
			return 0, errSubtitleIdentityUnavailable
		}
		return match, nil
	}
	return index, nil
}
