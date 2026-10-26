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
// like the restart selection carry.
func (h *PlaybackHandler) restartCommittedRowIDsV3(ctx context.Context, req *playback.StartRequestV3, userID int, profileID, clientIP, userAgent string) map[int]bool {
	rows := map[int]bool{}
	if h == nil || req == nil {
		return rows
	}
	if predecessorID := req.PreviousPlaybackAttemptID; predecessorID != "" && h.PlanStoreV3 != nil {
		if record, err := h.PlanStoreV3.GetAttemptByPlaybackAttemptID(ctx, predecessorID); err == nil && record != nil &&
			record.UserID == userID && record.ProfileID == profileID && record.RequestedMediaFileID == req.FileID {
			if record.RequestedMediaFileID > 0 {
				rows[record.RequestedMediaFileID] = true
			}
			if record.EffectiveMediaFileID > 0 {
				rows[record.EffectiveMediaFileID] = true
			}
		}
	}
	lister, ok := h.sessionMgr.(interface {
		GetUserSessions(userID int) []*playback.Session
	})
	if !ok {
		return rows
	}
	previous := findCarryCandidateSessionV3(lister.GetUserSessions(userID), profileID, req.FileID, clientIP, userAgent)
	if previous == nil {
		return rows
	}
	for _, id := range []int{previous.MediaFileID, previous.RequestedMediaFileID, previous.VirtualSubtitleEvidenceFileID} {
		if id > 0 {
			rows[id] = true
		}
	}
	return rows
}

// rekeyStartTrackIdentitiesV3 normalizes a start request's selected track
// identities onto req.FileID when the embedded row names the same virtual
// candidate, or is one of the same client's committed restart rows. It must run
// before NormalizeAndValidate, whose stale-identity drop otherwise erases the
// plan's own echoed identities.
func (h *PlaybackHandler) rekeyStartTrackIdentitiesV3(ctx context.Context, req *playback.StartRequestV3, userID int, profileID, clientIP, userAgent string) {
	if h == nil || h.fileResolver == nil || req == nil || req.FileID <= 0 {
		return
	}
	if req.AudioTrackID == "" && req.SubtitleTrackID == "" {
		return
	}
	requested, err := h.fileResolver.GetByID(ctx, req.FileID)
	if err != nil || requested == nil || !isVirtualPlaybackFile(requested) {
		return
	}
	committed := h.restartCommittedRowIDsV3(ctx, req, userID, profileID, clientIP, userAgent)
	rekey := func(kind, trackID string) string {
		embeddedFileID, embeddedKind, _, ok := playback.ParseTrackIDV3(trackID)
		if !ok || embeddedKind != kind || embeddedFileID == req.FileID {
			return trackID
		}
		accepted := committed[embeddedFileID]
		if !accepted {
			if row, lookupErr := h.fileResolver.GetByID(ctx, embeddedFileID); lookupErr == nil && row != nil {
				accepted = virtualRowsNameSameReleaseV3(requested, row)
			}
		}
		if !accepted {
			return trackID
		}
		rekeyed, ok := trackIdentityRekeyV3(req.FileID, kind, trackID)
		if !ok {
			return trackID
		}
		slog.InfoContext(ctx, "selected track identity re-keyed onto the requested row",
			"component", "playback", "kind", kind, "sent_track_id", trackID,
			"requested_file_id", req.FileID, "effective_file_id", embeddedFileID)
		return rekeyed
	}
	req.AudioTrackID = rekey("audio", req.AudioTrackID)
	req.SubtitleTrackID = rekey("subtitle", req.SubtitleTrackID)
}

// rekeyReplanTrackIdentitiesV3 is the replan counterpart: a serve association or
// candidate rotation can move the session's effective row without updating the
// durable record the replan seeds from, so the client's echoed plan identities
// may embed the session's row rather than the record's. Accept both when they
// name the same candidate or are rows the session itself carries.
func (h *PlaybackHandler) rekeyReplanTrackIdentitiesV3(ctx context.Context, start *playback.StartRequestV3, file *models.MediaFile, session *playback.Session) {
	if h == nil || h.fileResolver == nil || start == nil || file == nil {
		return
	}
	if start.AudioTrackID == "" && start.SubtitleTrackID == "" {
		return
	}
	// Committed session rows are only interchangeable for a virtual binding:
	// two local editions are different files even when a serve association
	// once pointed the session at one of them.
	committed := map[int]bool{file.ID: true}
	if isVirtualPlaybackFile(file) && session != nil {
		for _, id := range []int{session.MediaFileID, session.RequestedMediaFileID, session.VirtualSubtitleEvidenceFileID} {
			if id > 0 {
				committed[id] = true
			}
		}
	}
	rekey := func(kind, trackID string) string {
		embeddedFileID, embeddedKind, _, ok := playback.ParseTrackIDV3(trackID)
		if !ok || embeddedKind != kind || embeddedFileID == file.ID {
			return trackID
		}
		accepted := committed[embeddedFileID]
		if !accepted {
			if row, lookupErr := h.fileResolver.GetByID(ctx, embeddedFileID); lookupErr == nil && row != nil {
				accepted = virtualRowsNameSameReleaseV3(file, row)
			}
		}
		if !accepted {
			return trackID
		}
		rekeyed, ok := trackIdentityRekeyV3(file.ID, kind, trackID)
		if !ok {
			return trackID
		}
		slog.InfoContext(ctx, "replan selected track identity re-keyed onto the effective row",
			"component", "playback", "kind", kind, "sent_track_id", trackID,
			"effective_file_id", file.ID, "sent_file_id", embeddedFileID)
		return rekeyed
	}
	start.AudioTrackID = rekey("audio", start.AudioTrackID)
	start.SubtitleTrackID = rekey("subtitle", start.SubtitleTrackID)
}
