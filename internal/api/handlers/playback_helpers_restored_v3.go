// Restored playback helpers.

package handlers

import (
	"context"
	"errors"
	"slices"
	"strings"

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

func appendWarningOnceV3(warnings []playback.DegradationWarningV3, warning playback.DegradationWarningV3) []playback.DegradationWarningV3 {
	if slices.ContainsFunc(warnings, func(existing playback.DegradationWarningV3) bool { return existing.Code == warning.Code }) {
		return warnings
	}
	return append(warnings, warning)
}

func virtualPinnedProfileRowV3(session *playback.Session, row *models.MediaFile) bool {
	if session == nil || row == nil {
		return false
	}
	pinned := strings.TrimSpace(session.VirtualSourceURI)
	rowPath := strings.TrimSpace(row.FilePath)
	if pinned == "" || rowPath == "" {
		return false
	}
	// The session pin must be the concrete candidate; the loaded row must be its
	// provider-neutral profile, not another concrete pick. A different concrete
	// result is a release substitution candidate and must keep re-resolving.
	if virtualResultCandidateID(pinned) == "" || virtualResultCandidateID(rowPath) != "" {
		return false
	}
	return sameVirtualProfileScope(rowPath, pinned)
}

func sessionHasVirtualProvenanceV3(session *playback.Session) bool {
	if session == nil {
		return false
	}
	return session.VirtualSubtitleEvidenceSet ||
		strings.TrimSpace(session.VirtualSubtitleEvidenceURI) != "" ||
		session.VirtualSubtitleEvidenceFileID > 0
}

func sessionProvenanceMatchesPinnedRowV3(ctx context.Context, session *playback.Session, row *models.MediaFile, fileResolver FilePathResolver) bool {
	if !sessionHasVirtualProvenanceV3(session) || row == nil {
		return false
	}
	bound := *row
	bound.FilePath = session.VirtualSourceURI
	return virtualEvidenceMatchesBoundFile(ctx, &bound, session, fileResolver)
}

func (h *PlaybackHandler) frozenSeekCarrierV3(ctx context.Context, session *playback.Session, record *playback.AttemptRecordV3, row *models.MediaFile) (*models.MediaFile, error) {
	if session == nil || record == nil || row == nil {
		return nil, errFrozenSeekEvidenceUnavailable
	}
	candidate := strings.TrimSpace(record.CurrentPlan.EffectiveVirtualURI)
	if candidate == "" {
		return nil, errFrozenSeekEvidenceUnavailable
	}
	rowNamesCandidate := sameVirtualCandidate(row.FilePath, candidate)
	// Evaluate the session's evidence exactly as the binder does, but against
	// the frozen candidate rather than the session's (possibly moved) pin.
	bound := *row
	bound.FilePath = candidate
	sessionEvidence := virtualEvidenceMatchesBoundFile(ctx, &bound, session, h.fileResolver)
	if !rowNamesCandidate && !sessionEvidence {
		return nil, errFrozenSeekEvidenceUnavailable
	}
	owner := 0
	if sameVirtualCandidate(session.VirtualSourceURI, candidate) {
		owner = session.VirtualSourceOwnerInstallationID
	}
	if owner <= 0 && sessionEvidence {
		owner = session.VirtualSubtitleEvidenceOwnerInstallationID
	}
	if owner <= 0 && rowNamesCandidate {
		owner = row.VirtualOwnerInstallationID
	}
	if owner <= 0 {
		return nil, errFrozenSeekEvidenceUnavailable
	}
	carrier := *row
	carrier.FilePath = candidate
	carrier.VirtualOwnerInstallationID = owner
	if !rowNamesCandidate {
		// The row describes another release: its inventory and probe stamp are
		// not evidence for the frozen candidate. Carry the session's retained
		// inventory for the candidate and drop the row's stamp so the state
		// writer cannot stamp the foreign inventory as verified under the
		// candidate's URI.
		carrier.AudioTracks = session.VirtualAudioTracks
		carrier.SubtitleTracks = session.VirtualSubtitleTracks
		carrier.ExternalSubtitles = session.VirtualExternalSubtitles
		carrier.ProbeUpdatedAt = nil
	}
	return carryValidatedVirtualVideoEvidenceV3(&carrier, record.CurrentPlan.Source), nil
}

func carryValidatedVirtualVideoEvidenceV3(file *models.MediaFile, source playback.SourceDescriptorV3) *models.MediaFile {
	if file == nil || !isVirtualPlaybackFile(file) {
		return file
	}
	if completeVirtualVideoEvidenceV3(file) {
		return file
	}
	if source.MediaFileID != file.ID || source.VideoCodec == "" ||
		source.BitDepth <= 0 || source.Width <= 0 || source.Height <= 0 ||
		source.FrameRate <= 0 || source.BitrateKbps <= 0 {
		return file
	}
	// Never mix a row's declared codec with descriptor evidence for a different
	// one. A row that declares h264 while the running plan validated hevc must
	// not receive the descriptor's geometry: that is franken-evidence the
	// planner would route against. Agreement uses the planner's own
	// normalization, so "h265"/"hevc" and "h264"/"avc" compare equal.
	if !virtualCodecAgreesV3(file, source.VideoCodec) {
		return file
	}
	carried := *file
	// Clone the video-track slice before the in-place top-up below. A shallow
	// struct copy shares the backing array, so writing carried.VideoTracks[0]
	// would mutate the caller's inventory and race any other reader of it.
	carried.VideoTracks = append([]models.VideoTrack(nil), file.VideoTracks...)
	if carried.CodecVideo == "" {
		carried.CodecVideo = source.VideoCodec
	}
	if carried.Container == "" || strings.EqualFold(carried.Container, "virtual") {
		if source.Container != "" {
			carried.Container = source.Container
		}
	}
	if carried.Bitrate == 0 {
		// MediaFile.Bitrate is the probe's kbps figure (scanner/probe.go stores
		// ffprobe bit_rate/1000), the same unit as
		// SourceDescriptorV3.BitrateKbps, so the assignment is unit-preserving.
		carried.Bitrate = source.BitrateKbps
	}
	if source.DynamicRange == playback.DynamicRangeHDRUnknownV3 {
		// Unknown HDR carries no classifying range text: tonemap's classifier
		// promotes any "hdr"-bearing VideoRangeType to HDR10, so the unknown
		// grade survives only through the file-level HDR flag with no range text
		// to classify. Setting it keeps the carried row's descriptor equal to
		// the running plan's.
		carried.HDR = true
	}
	if len(carried.VideoTracks) == 0 {
		carried.VideoTracks = []models.VideoTrack{virtualVideoTrackFromDescriptorV3(source)}
		return &carried
	}
	// The existing-track path fills the exact same field set the empty-track
	// reconstruction does — both call fillVirtualVideoTrackFromDescriptorV3 —
	// so a partial row is never left reading copy-safe on a source the running
	// plan proved is not.
	fillVirtualVideoTrackFromDescriptorV3(&carried.VideoTracks[0], source)
	return &carried
}

func virtualCandidateRotationRecordedV3(ctx context.Context, session *playback.Session, effectiveFile *models.MediaFile, fileResolver FilePathResolver) bool {
	if session == nil || effectiveFile == nil || !isVirtualPlaybackFile(effectiveFile) {
		return false
	}
	if session.VirtualSourceRevision != "" || session.VirtualSourceURI == "" {
		return false
	}
	return session.VirtualSubtitleEvidenceSet && !virtualEvidenceMatchesBoundFile(ctx, effectiveFile, session, fileResolver)
}

var errFrozenSeekEvidenceUnavailable = errors.New("the frozen seek candidate's own evidence is unavailable")

func virtualCodecAgreesV3(file *models.MediaFile, codec string) bool {
	if file == nil {
		return false
	}
	want := strings.TrimSpace(codec)
	if want == "" {
		return false
	}
	if strings.EqualFold(file.CodecVideo, want) {
		return true
	}
	for _, t := range file.VideoTracks {
		if strings.EqualFold(t.Codec, want) {
			return true
		}
	}
	return false
}

func virtualVideoTrackFromDescriptorV3(source playback.SourceDescriptorV3) models.VideoTrack {
	t := models.VideoTrack{}
	fillVirtualVideoTrackFromDescriptorV3(&t, source)
	return t
}

func fillVirtualVideoTrackFromDescriptorV3(t *models.VideoTrack, source playback.SourceDescriptorV3) {
	if t == nil {
		return
	}
	t.Codec = source.VideoCodec
	t.Profile = source.VideoProfile
	t.Level = source.VideoLevel
	t.BitDepth = source.BitDepth
	t.Width = source.Width
	t.Height = source.Height
	t.Bitrate = source.BitrateKbps
	t.ColorRange = source.ColorRange
}
