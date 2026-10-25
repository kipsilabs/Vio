package handlers

import (
	"context"
	"math"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

func TestCarryFailureRecoveryVirtualEvidenceV3SatisfiesCompletenessRequirement(t *testing.T) {
	matchingURI := "virtual://movie/tt12345?result=cand-alpha"
	file := &models.MediaFile{
		ID:         720,
		FilePath:   matchingURI,
		Container:  "mkv",
		CodecVideo: "hevc",
		VideoTracks: []models.VideoTrack{{
			Codec:    "hevc",
			Profile:  "main10",
			Level:    153,
			BitDepth: 10,
			Width:    3840,
			Height:   2160,
			// FrameRate and Bitrate missing (gaps)
		}},
	}

	if playback.VirtualRouteVideoMetadataCompleteV3(file) {
		t.Fatal("initial file unexpectedly satisfies planner completeness")
	}

	record := &playback.AttemptRecordV3{
		CurrentPlan: playback.PlanV3{
			EffectiveVirtualURI: matchingURI,
			Source: playback.SourceDescriptorV3{
				MediaFileID:  720,
				Container:    "mkv",
				VideoCodec:   "hevc",
				VideoProfile: "main10",
				VideoLevel:   153,
				BitDepth:     10,
				Width:        3840,
				Height:       2160,
				FrameRate:    24.0,
				BitrateKbps:  15_000,
			},
		},
	}

	session := &playback.Session{
		VirtualSourceURI:                 matchingURI,
		VirtualSourceOwnerInstallationID: 11,
		VirtualSubtitleEvidenceURI:       matchingURI,
		VirtualSubtitleEvidenceFileID:    720,
	}

	h := &PlaybackHandler{}
	ctx := context.Background()

	carried := h.carryFailureRecoveryVirtualEvidenceV3(ctx, session, record, playback.ReplanOperationFailureRecoveryV3, matchingURI, 11, file)
	if carried == file {
		t.Fatal("expected carried file to be a topped-up copy, got unmodified pointer")
	}
	if !playback.VirtualRouteVideoMetadataCompleteV3(carried) {
		t.Fatal("carried file does not satisfy the planner's completeness requirement")
	}
	if len(carried.VideoTracks) != 1 {
		t.Fatalf("video tracks = %d, want 1", len(carried.VideoTracks))
	}
	track := carried.VideoTracks[0]
	if track.BitDepth != 10 || track.Width != 3840 || track.Height != 2160 {
		t.Fatalf("track geometry = %dx%d %d-bit, want 3840x2160 10-bit", track.Width, track.Height, track.BitDepth)
	}
	if math.Abs(virtualFrameRateV3(track.FrameRate)-24.0) > 1e-6 {
		t.Fatalf("track frame rate = %q, want 24", track.FrameRate)
	}
	if track.Bitrate != 15_000 {
		t.Fatalf("track bitrate = %d, want 15000", track.Bitrate)
	}
	if carried.Bitrate != 15_000 {
		t.Fatalf("file bitrate = %d, want 15000", carried.Bitrate)
	}
	if carried.ProbeUpdatedAt != nil {
		t.Fatalf("ProbeUpdatedAt was set: %+v, must remain nil to preserve inventory_status declared", carried.ProbeUpdatedAt)
	}
}

func TestCarryFailureRecoveryVirtualEvidenceV3FencingAndProvenance(t *testing.T) {
	matchingURI := "virtual://movie/tt12345?result=cand-alpha"
	file := &models.MediaFile{
		ID:         720,
		FilePath:   matchingURI,
		Container:  "mkv",
		CodecVideo: "hevc",
		VideoTracks: []models.VideoTrack{{
			Codec:    "hevc",
			Profile:  "main10",
			Level:    153,
			BitDepth: 10,
			Width:    3840,
			Height:   2160,
		}},
	}

	record := &playback.AttemptRecordV3{
		CurrentPlan: playback.PlanV3{
			EffectiveVirtualURI: matchingURI,
			Source: playback.SourceDescriptorV3{
				MediaFileID:  720,
				Container:    "mkv",
				VideoCodec:   "hevc",
				VideoProfile: "main10",
				VideoLevel:   153,
				BitDepth:     10,
				Width:        3840,
				Height:       2160,
				FrameRate:    24.0,
				BitrateKbps:  15_000,
			},
		},
	}

	session := &playback.Session{
		VirtualSourceURI:                 matchingURI,
		VirtualSourceOwnerInstallationID: 11,
		VirtualSubtitleEvidenceURI:       matchingURI,
		VirtualSubtitleEvidenceFileID:    720,
	}

	h := &PlaybackHandler{}
	ctx := context.Background()

	// 1. Operation not failure_recovery
	gotNonRecovery := h.carryFailureRecoveryVirtualEvidenceV3(ctx, session, record, playback.ReplanOperationTrackChangeV3, matchingURI, 11, file)
	if gotNonRecovery != file {
		t.Fatal("expected carry to be refused for non-failure-recovery operation")
	}

	// 2. Different owner installation ID
	gotDiffOwner := h.carryFailureRecoveryVirtualEvidenceV3(ctx, session, record, playback.ReplanOperationFailureRecoveryV3, matchingURI, 99, file)
	if gotDiffOwner != file {
		t.Fatal("expected carry to be refused when owner installation ID differs")
	}

	// 3. Different resolved candidate URI
	diffURI := "virtual://movie/tt12345?result=cand-beta"
	gotDiffURI := h.carryFailureRecoveryVirtualEvidenceV3(ctx, session, record, playback.ReplanOperationFailureRecoveryV3, diffURI, 11, file)
	if gotDiffURI != file {
		t.Fatal("expected carry to be refused when resolved URI differs from plan candidate")
	}

	// 4. Session pin differs from plan candidate
	diffSession := &playback.Session{
		VirtualSourceURI:                 "virtual://movie/tt12345?result=cand-gamma",
		VirtualSourceOwnerInstallationID: 11,
		VirtualSubtitleEvidenceURI:       matchingURI,
		VirtualSubtitleEvidenceFileID:    720,
	}
	gotDiffPin := h.carryFailureRecoveryVirtualEvidenceV3(ctx, diffSession, record, playback.ReplanOperationFailureRecoveryV3, matchingURI, 11, file)
	if gotDiffPin != file {
		t.Fatal("expected carry to be refused when session pin differs from plan candidate")
	}

	// 5. Stale session provenance (e.g. evidence captured for another file ID)
	staleProvenanceSession := &playback.Session{
		VirtualSourceURI:                 matchingURI,
		VirtualSourceOwnerInstallationID: 11,
		VirtualSubtitleEvidenceURI:       matchingURI,
		VirtualSubtitleEvidenceFileID:    999, // mismatch
	}
	gotStaleProv := h.carryFailureRecoveryVirtualEvidenceV3(ctx, staleProvenanceSession, record, playback.ReplanOperationFailureRecoveryV3, matchingURI, 11, file)
	if gotStaleProv != file {
		t.Fatal("expected carry to be refused when session provenance does not match row")
	}

	// 6. Conflicting codec in source descriptor
	conflictCodecRecord := &playback.AttemptRecordV3{
		CurrentPlan: playback.PlanV3{
			EffectiveVirtualURI: matchingURI,
			Source: func() playback.SourceDescriptorV3 {
				s := record.CurrentPlan.Source
				s.VideoCodec = "h264"
				return s
			}(),
		},
	}
	gotConflictCodec := h.carryFailureRecoveryVirtualEvidenceV3(ctx, session, conflictCodecRecord, playback.ReplanOperationFailureRecoveryV3, matchingURI, 11, file)
	if gotConflictCodec != file {
		t.Fatal("expected carry to be refused when source codec conflicts with row")
	}
}
