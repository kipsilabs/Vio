package handlers

import (
	"context"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

func findCarryCandidateSessionV3(sessions []*playback.Session, profileID string, fileID int, clientIP, userAgent string) *playback.Session {
	var previous *playback.Session
	matches := 0
	for _, session := range sessions {
		if session == nil || session.ProfileID != profileID {
			continue
		}
		if session.MediaFileID != fileID && session.RequestedMediaFileID != fileID {
			continue
		}
		if clientIP != "" && session.ClientIP != "" && session.ClientIP != clientIP {
			continue
		}
		if userAgent != "" && session.ClientUserAgent != "" && session.ClientUserAgent != userAgent {
			continue
		}
		matches++
		if previous == nil || session.LastActivityAt.After(previous.LastActivityAt) {
			previous = session
		}
	}
	if matches != 1 {
		// Two live sessions from the same client identity are ambiguous (two
		// tabs, or devices behind one NAT); inheriting either one's pick can
		// silently change the other's audio. The client must replay its
		// selection or name the predecessor attempt explicitly.
		return nil
	}
	return previous
}

// remapCarriedAudioIndexV3 re-identifies an explicit audio selection on the
// target inventory. When the inventories are the same layout the ordinal is
// still valid and is reused directly; otherwise the container index is not an
// identity across files, so the strict signature/facts remap decides. Duplicate
// signatures remain ambiguous (-1) and report unavailable.

func remapCarriedAudioIndexV3(previous []models.AudioTrack, selectedIndex int, file *models.MediaFile) int {
	if file == nil {
		return -1
	}
	if playback.AudioLayoutsEqual(previous, file.AudioTracks) &&
		selectedIndex >= 0 && selectedIndex < len(file.AudioTracks) {
		return selectedIndex
	}
	return playback.RemapAudioSelectionIndexBySignature(previous, selectedIndex, file.AudioTracks)
}

func audioTracksFromInventoryV3(items []playback.AudioInventoryItemV3) []models.AudioTrack {
	if len(items) == 0 {
		return nil
	}
	tracks := make([]models.AudioTrack, len(items))
	for i, item := range items {
		tracks[i] = models.AudioTrack{
			Index:         item.Index,
			Title:         item.Title,
			EmbeddedTitle: item.EmbeddedTitle,
			Language:      item.Language,
			Languages:     item.Languages,
			Codec:         item.Codec,
			Layout:        item.Layout,
			Channels:      item.Channels,
		}
	}
	return tracks
}

type subtitleTrackIdentityV3 struct {
	source          string
	language        string
	codec           string
	forced          bool
	hearingImpaired bool
}

func sameSubtitleInventoryV3(a, b *models.MediaFile) bool {
	return playback.SubtitleLayoutsEqualIncludingExternal(
		a.SubtitleTracks, a.ExternalSubtitles, b.SubtitleTracks, b.ExternalSubtitles)
}

// sameMediaInventoryV3 reports whether two files carry identical audio and
// subtitle inventories. It is the "did the effective tracks actually move"
// predicate for a replan: a same-row candidate rotation keeps the file id but
// replaces the probed tracks, so id equality is not a sufficient no-op guard.

func virtualCandidateRotationRecordedV3(ctx context.Context, session *playback.Session, effectiveFile *models.MediaFile, fileResolver FilePathResolver) bool {
	if session == nil || effectiveFile == nil || !isVirtualPlaybackFile(effectiveFile) {
		return false
	}
	if session.VirtualSourceRevision != "" || session.VirtualSourceURI == "" {
		return false
	}
	return session.VirtualSubtitleEvidenceSet && !virtualEvidenceMatchesBoundFile(ctx, effectiveFile, session, fileResolver)
}

// virtualPinnedProfileRowV3 reports whether the catalog row a replan loaded is
// the provider-neutral profile row of the session's pinned concrete candidate:
// the session pin carries a concrete "result=" pick while the row carries none,
// and the two share a provider-neutral profile scope (sameVirtualProfileScope).
//
// The provider mints and churns the per-listing result id for bytes that never
// changed, so on a session serving a concrete pick the reloaded catalog row can
// be the provider-neutral profile row while the session stays bound to the
// concrete candidate. A user-intent replan (track change or seek) must carry
// that pin instead of re-listing: requiring result-id equality would reject the
// very binding the session is serving.
//
// Profile-scope equality does not identify the release: several concrete sibling
// picks can share one profile. This predicate only establishes that the row is
// the pin's profile; whether the carry is trustworthy rests on the session's
// pinned evidence (sessionProvenanceMatchesPinnedRowV3), never on scope equality.
