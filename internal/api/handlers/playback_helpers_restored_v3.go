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

func sessionProvenanceMatchesPinnedRowV3(session *playback.Session, row *models.MediaFile) bool {
	if !sessionHasVirtualProvenanceV3(session) || row == nil {
		return false
	}
	bound := *row
	bound.FilePath = session.VirtualSourceURI
	return virtualEvidenceMatchesBoundFile(ctx, &bound, session, h.fileResolver)
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
