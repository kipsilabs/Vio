package handlers

import (
	"context"
	"log/slog"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// The plan mints selected track identities against the effective media file
// row, while a client re-sends the requested row id as start/replan file_id and
// echoes the plan's identities unchanged on a restart. Virtual candidate
// resolution can also serve a row other than the requested one. Validating an
// echoed identity against the requested id alone therefore discards a perfectly
// valid selection as "stale", which silently falls back to the default track.
//
// The helpers below normalize such an identity onto the row the request actually
// validates against, but only when the embedding row provably names the same
// candidate (or is one of the same session's committed rows for a restart).
// A row naming a different release stays foreign and is still dropped by the
// stale-identity guard.

// rekeyedTrackSelectionV3 records one explicit track identity whose ordinal was
// minted against source. Resolution may serve a candidate with a different
// inventory, so the ordinal is only reusable once it has been checked against
// the served tracks; active is false for a malformed identity or a foreign row
// the stale guard must still drop.
type rekeyedTrackSelectionV3 struct {
	active  bool
	ordinal int
	source  *models.MediaFile
}

// startTrackRekeysV3 is the per-kind state captured before virtual resolution
// replaces the requested row with the served candidate.
type startTrackRekeysV3 struct {
	audio    rekeyedTrackSelectionV3
	subtitle rekeyedTrackSelectionV3
}

func (r startTrackRekeysV3) empty() bool {
	return !r.audio.active && !r.subtitle.active
}

// trackIdentityRekeyV3 rewrites a well-formed track identity of kind to fileID
// while preserving its ordinal. It reports false for a malformed identity or one
// of another kind, leaving the caller's value untouched.
func trackIdentityRekeyV3(fileID int, kind, trackID string) (string, bool) {
	if fileID <= 0 || trackID == "" {
		return trackID, false
	}
	embeddedFileID, embeddedKind, ordinal, ok := playback.ParseTrackIDV3(trackID)
	if !ok || embeddedKind != kind {
		return trackID, false
	}
	if embeddedFileID == fileID {
		return trackID, true
	}
	return playback.TrackIDV3(fileID, kind, ordinal), true
}

// virtualRowsNameSameReleaseV3 reports whether two catalog rows may describe
// bytes of the same release. Exact candidate equality covers a plan row and its
// requested sibling; a provider-neutral requested row names no concrete pick, so
// any concrete result candidate under its profile scope is a possible effective
// row for it. Two concrete rows with different result ids are different
// candidates and stay foreign.
func virtualRowsNameSameReleaseV3(requested, candidate *models.MediaFile) bool {
	if requested == nil || candidate == nil {
		return false
	}
	if !isVirtualPlaybackFile(requested) || !isVirtualPlaybackFile(candidate) {
		return false
	}
	if sameVirtualCandidate(requested.FilePath, candidate.FilePath) {
		return true
	}
	return virtualResultCandidateID(requested.FilePath) == "" &&
		sameVirtualProfileScope(requested.FilePath, candidate.FilePath)
}

// restartCommittedRowIDsV3 collects the catalog rows a same-client restart may
// legitimately echo in a track identity: the predecessor attempt's committed
// requested/effective rows, and the uniquely matching live session's effective,
// requested and evidence rows. An ambiguous live match carries nothing, exactly
// like the restart selection carry. The matching session is returned so the
// re-key can read the inventory its plan-time selection was minted against.
func (h *PlaybackHandler) restartCommittedRowIDsV3(ctx context.Context, req *playback.StartRequestV3, userID int, profileID, clientIP, userAgent string) (map[int]bool, *playback.Session) {
	rows := map[int]bool{}
	if h == nil || req == nil {
		return rows, nil
	}
	h.addPredecessorCommittedRowIDsV3(ctx, req, userID, profileID, rows)
	lister, ok := h.sessionMgr.(interface {
		GetUserSessions(userID int) []*playback.Session
	})
	if !ok {
		return rows, nil
	}
	previous := findCarryCandidateSessionV3(lister.GetUserSessions(userID), profileID, req.FileID, clientIP, userAgent)
	if previous == nil {
		return rows, nil
	}
	for _, id := range []int{previous.MediaFileID, previous.RequestedMediaFileID, previous.VirtualSubtitleEvidenceFileID} {
		if id > 0 {
			rows[id] = true
		}
	}
	return rows, previous
}

// addPredecessorCommittedRowIDsV3 adds the committed requested and effective
// rows of the predecessor attempt this request names, when that attempt belongs
// to the same user, profile and requested file.
func (h *PlaybackHandler) addPredecessorCommittedRowIDsV3(ctx context.Context, req *playback.StartRequestV3, userID int, profileID string, rows map[int]bool) {
	predecessorID := req.PreviousPlaybackAttemptID
	if predecessorID == "" || h.PlanStoreV3 == nil {
		return
	}
	record, err := h.PlanStoreV3.GetAttemptByPlaybackAttemptID(ctx, predecessorID)
	if err != nil || record == nil ||
		record.UserID != userID || record.ProfileID != profileID ||
		record.RequestedMediaFileID != req.FileID {
		return
	}
	if record.RequestedMediaFileID > 0 {
		rows[record.RequestedMediaFileID] = true
	}
	if record.EffectiveMediaFileID > 0 {
		rows[record.EffectiveMediaFileID] = true
	}
}

// rekeySourceFileV3 loads the inventory a track identity's ordinal was minted
// against. The catalog row is the base; when the id names the session's own
// effective or evidence row, the session's retained inventory is preferred
// because an in-place re-probe can rewrite the row under the same id.
func (h *PlaybackHandler) rekeySourceFileV3(ctx context.Context, fileID int, session *playback.Session) *models.MediaFile {
	if fileID <= 0 {
		return nil
	}
	var source *models.MediaFile
	if h.fileResolver != nil {
		if row, err := h.fileResolver.GetByID(ctx, fileID); err == nil && row != nil {
			source = row
		}
	}
	if session == nil || !session.VirtualSubtitleEvidenceSet {
		return source
	}
	if fileID != session.MediaFileID && fileID != session.VirtualSubtitleEvidenceFileID {
		return source
	}
	evidence := &models.MediaFile{ID: fileID}
	if source != nil {
		cloned := *source
		evidence = &cloned
	}
	evidence.AudioTracks = session.VirtualAudioTracks
	evidence.SubtitleTracks = session.VirtualSubtitleTracks
	evidence.ExternalSubtitles = session.VirtualExternalSubtitles
	return evidence
}

// rekeyStartTrackIdentitiesV3 normalizes a start request's selected track
// identities onto req.FileID when the embedded row names the same virtual
// candidate, or is one of the same client's committed restart rows. It must run
// before NormalizeAndValidate, whose stale-identity drop otherwise erases the
// plan's own echoed identities. The returned state carries the inventory each
// echoed ordinal belongs to so constrainRekeyedStartTracksV3 can re-check it
// once virtual resolution has installed the served candidate's tracks.
func (h *PlaybackHandler) rekeyStartTrackIdentitiesV3(ctx context.Context, req *playback.StartRequestV3, userID int, profileID, clientIP, userAgent string) startTrackRekeysV3 {
	var captured startTrackRekeysV3
	if h == nil || h.fileResolver == nil || req == nil || req.FileID <= 0 {
		return captured
	}
	if req.AudioTrackID == "" && req.SubtitleTrackID == "" {
		return captured
	}
	requested, err := h.fileResolver.GetByID(ctx, req.FileID)
	if err != nil || requested == nil || !isVirtualPlaybackFile(requested) {
		return captured
	}
	committed, previous := h.restartCommittedRowIDsV3(ctx, req, userID, profileID, clientIP, userAgent)
	rekey := func(kind, trackID string) (string, rekeyedTrackSelectionV3) {
		embeddedFileID, embeddedKind, ordinal, ok := playback.ParseTrackIDV3(trackID)
		if !ok || embeddedKind != kind {
			return trackID, rekeyedTrackSelectionV3{}
		}
		// The requested row itself is the target of any later normalization, but
		// its inventory still names what the ordinal means: a resolver
		// substitution can serve another candidate under this row, and the
		// ordinal must be re-checked against the served tracks.
		if embeddedFileID == req.FileID {
			return trackID, rekeyedTrackSelectionV3{active: true, ordinal: ordinal, source: requested}
		}
		source := h.rekeySourceFileV3(ctx, embeddedFileID, previous)
		accepted := committed[embeddedFileID]
		if !accepted && source != nil {
			accepted = virtualRowsNameSameReleaseV3(requested, source)
		}
		if !accepted {
			return trackID, rekeyedTrackSelectionV3{}
		}
		rekeyed, rekeyedOK := trackIdentityRekeyV3(req.FileID, kind, trackID)
		if !rekeyedOK {
			return trackID, rekeyedTrackSelectionV3{}
		}
		slog.InfoContext(ctx, "selected track identity re-keyed onto the requested row",
			"component", "playback", "kind", kind, "sent_track_id", trackID,
			"requested_file_id", req.FileID, "effective_file_id", embeddedFileID)
		return rekeyed, rekeyedTrackSelectionV3{active: true, ordinal: ordinal, source: source}
	}
	req.AudioTrackID, captured.audio = rekey("audio", req.AudioTrackID)
	req.SubtitleTrackID, captured.subtitle = rekey("subtitle", req.SubtitleTrackID)
	return captured
}

// constrainRekeyedStartTracksV3 re-checks the identities captured before
// resolution against the candidate the start actually serves. The carried
// ordinal belongs to source's inventory; reusing it unchanged when resolution
// substitutes a candidate that orders its tracks differently silently plays
// another stream. The ordinal is therefore kept only for a proven matching
// inventory, remapped by identity otherwise, and reported unavailable (cleared,
// with a degradation warning added by the caller) when it cannot be proven.
func (h *PlaybackHandler) constrainRekeyedStartTracksV3(ctx context.Context, req *playback.StartRequestV3, target *models.MediaFile, captured startTrackRekeysV3) (audioUnavailable, subtitleUnavailable bool) {
	if req == nil || target == nil || captured.empty() {
		return false, false
	}
	if captured.audio.active {
		index, available := resolvedRekeyedAudioOrdinalV3(captured.audio.source, target, captured.audio.ordinal)
		if !available {
			slog.InfoContext(ctx, "re-keyed audio selection is unavailable on the resolved candidate; using the default track",
				"component", "playback", "from_audio_index", captured.audio.ordinal,
				"resolved_file_id", target.ID)
			req.AudioTrackID = ""
			req.AudioTrackIndex = nil
			audioUnavailable = true
		} else {
			req.AudioTrackID = playback.TrackIDV3(target.ID, "audio", index)
		}
	}
	if captured.subtitle.active {
		subtitleUnavailable = h.constrainRekeyedSubtitleSelectionV3(ctx, req, captured.subtitle.source, target)
	}
	return audioUnavailable, subtitleUnavailable
}

// resolvedRekeyedAudioOrdinalV3 returns the ordinal in target that names the
// same audio stream the source ordinal named. An identical inventory proves the
// ordinal still addresses the selected track; a different inventory is
// re-identified by signature and fails closed when no unique equivalent exists.
func resolvedRekeyedAudioOrdinalV3(source, target *models.MediaFile, ordinal int) (int, bool) {
	if source == nil || target == nil || ordinal < 0 {
		return 0, false
	}
	if playback.AudioLayoutsEqual(source.AudioTracks, target.AudioTracks) {
		return ordinal, true
	}
	if len(source.AudioTracks) == 0 || len(target.AudioTracks) == 0 {
		return 0, false
	}
	remapped := playback.RemapAudioSelectionIndexBySignature(source.AudioTracks, ordinal, target.AudioTracks)
	if remapped < 0 {
		return 0, false
	}
	return remapped, true
}

// constrainRekeyedSubtitleSelectionV3 keeps a re-keyed subtitle ordinal only
// when the served inventory matches the one it was minted against. Unlike
// audio, a changed inventory is remapped by the existing edition-switch
// language/format matcher; when no equivalent exists the selection is cleared
// and reported unavailable rather than reinterpreted against the new order.
func (h *PlaybackHandler) constrainRekeyedSubtitleSelectionV3(ctx context.Context, req *playback.StartRequestV3, source, target *models.MediaFile) (unavailable bool) {
	if req == nil {
		return false
	}
	if source == nil {
		req.SubtitleTrackID = ""
		req.SubtitleTrackIndex = nil
		return true
	}
	if sameSubtitleInventoryV3(source, target) {
		return false
	}
	if _, err := h.remapSubtitleSelectionV3(ctx, source, target, req); err != nil {
		slog.InfoContext(ctx, "re-keyed subtitle selection is unavailable on the resolved candidate; clearing it",
			"component", "playback", "source_file_id", source.ID, "resolved_file_id", target.ID, "error", err)
	}
	if req.SubtitleTrackID == "" {
		req.SubtitleTrackIndex = nil
		return true
	}
	return false
}

// rekeyReplanTrackIdentitiesV3 is the replan counterpart: a serve association or
// candidate rotation can move the session's effective row without updating the
// durable record the replan seeds from, so the client's echoed plan identities
// may embed the session's row rather than the record's. Accept both when they
// name the same candidate or are rows the session itself carries, but re-check
// the carried ordinal against the effective row's inventory exactly like the
// start path: row acceptance alone does not prove the ordinal addresses the
// same stream, because a substituted candidate can reorder its tracks.
func (h *PlaybackHandler) rekeyReplanTrackIdentitiesV3(ctx context.Context, start *playback.StartRequestV3, file *models.MediaFile, session *playback.Session) (audioUnavailable, subtitleUnavailable bool) {
	if h == nil || h.fileResolver == nil || start == nil || file == nil {
		return false, false
	}
	if start.AudioTrackID == "" && start.SubtitleTrackID == "" {
		return false, false
	}
	// Committed session rows are only interchangeable for a virtual binding:
	// two local editions are different files even when a serve association
	// once pointed the session at one of them.
	committed := map[int]bool{file.ID: true}
	virtualBinding := isVirtualPlaybackFile(file)
	if virtualBinding && session != nil {
		for _, id := range []int{session.MediaFileID, session.RequestedMediaFileID, session.VirtualSubtitleEvidenceFileID} {
			if id > 0 {
				committed[id] = true
			}
		}
	}
	acceptedSource := func(kind, trackID string) (*models.MediaFile, int, bool) {
		embeddedFileID, embeddedKind, ordinal, ok := playback.ParseTrackIDV3(trackID)
		if !ok || embeddedKind != kind || embeddedFileID == file.ID {
			return nil, 0, false
		}
		source := h.rekeySourceFileV3(ctx, embeddedFileID, session)
		accepted := committed[embeddedFileID]
		if !accepted && source != nil {
			accepted = virtualRowsNameSameReleaseV3(file, source)
		}
		if !accepted {
			return nil, 0, false
		}
		return source, ordinal, true
	}
	if source, ordinal, ok := acceptedSource("audio", start.AudioTrackID); ok {
		sentTrackID := start.AudioTrackID
		if index, available := resolvedRekeyedAudioOrdinalV3(source, file, ordinal); available {
			start.AudioTrackID = playback.TrackIDV3(file.ID, "audio", index)
			slog.InfoContext(ctx, "replan selected audio track identity re-keyed onto the effective row",
				"component", "playback", "sent_track_id", sentTrackID,
				"effective_file_id", file.ID, "from_audio_index", ordinal, "to_audio_index", index)
		} else {
			slog.InfoContext(ctx, "replan audio selection is unavailable on the effective row; using the default track",
				"component", "playback", "from_audio_index", ordinal, "effective_file_id", file.ID)
			start.AudioTrackID = ""
			start.AudioTrackIndex = nil
			audioUnavailable = true
		}
	}
	if source, ordinal, ok := acceptedSource("subtitle", start.SubtitleTrackID); ok {
		// Normalize to the target row first so the edition-switch matcher reads
		// the carried ordinal regardless of which committed row minted it.
		start.SubtitleTrackID = playback.TrackIDV3(file.ID, "subtitle", ordinal)
		subtitleUnavailable = h.constrainRekeyedSubtitleSelectionV3(ctx, start, source, file)
		if !subtitleUnavailable {
			slog.InfoContext(ctx, "replan selected subtitle track identity re-keyed onto the effective row",
				"component", "playback", "effective_file_id", file.ID)
		}
	}
	return audioUnavailable, subtitleUnavailable
}
