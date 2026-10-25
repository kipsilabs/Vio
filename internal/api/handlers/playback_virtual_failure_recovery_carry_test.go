package handlers

import (
	"math"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// TestCarryFailureRecoveryVirtualEvidenceV3SatisfiesCompletenessRequirement tests
// that a virtual media file with missing frame rate and bitrate, when carried against
// a running plan for the same candidate URI and row with complete fields, satisfies
// the planner's completeness requirement without setting ProbeUpdatedAt.
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

	carried := carryFailureRecoveryVirtualEvidenceV3(record, matchingURI, file)
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

// TestCarryFailureRecoveryVirtualEvidenceV3LeavesMismatchedCandidateUnchanged tests
// that a different resolved URI, a source whose MediaFileID differs from the row id,
// or a conflicting video codec leaves the file returned unchanged.
func TestCarryFailureRecoveryVirtualEvidenceV3LeavesMismatchedCandidateUnchanged(t *testing.T) {
	planURI := "virtual://movie/tt12345?result=cand-alpha"
	file := &models.MediaFile{
		ID:         720,
		FilePath:   planURI,
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
			EffectiveVirtualURI: planURI,
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

	// 1. Different resolved URI
	diffURI := "virtual://movie/tt12345?result=cand-beta"
	gotDiffURI := carryFailureRecoveryVirtualEvidenceV3(record, diffURI, file)
	if gotDiffURI != file {
		t.Fatalf("expected file to be returned unchanged on different URI, got %+v", gotDiffURI)
	}

	// 2. Source whose MediaFileID differs from row id
	diffIDRecord := &playback.AttemptRecordV3{
		CurrentPlan: playback.PlanV3{
			EffectiveVirtualURI: planURI,
			Source: func() playback.SourceDescriptorV3 {
				s := record.CurrentPlan.Source
				s.MediaFileID = 999
				return s
			}(),
		},
	}
	gotDiffID := carryFailureRecoveryVirtualEvidenceV3(diffIDRecord, planURI, file)
	if gotDiffID != file {
		t.Fatalf("expected file to be returned unchanged when MediaFileID differs, got %+v", gotDiffID)
	}

	// 3. Conflicting codec
	conflictCodecRecord := &playback.AttemptRecordV3{
		CurrentPlan: playback.PlanV3{
			EffectiveVirtualURI: planURI,
			Source: func() playback.SourceDescriptorV3 {
				s := record.CurrentPlan.Source
				s.VideoCodec = "h264"
				return s
			}(),
		},
	}
	gotConflictCodec := carryFailureRecoveryVirtualEvidenceV3(conflictCodecRecord, planURI, file)
	if gotConflictCodec != file {
		t.Fatalf("expected file to be returned unchanged when codec conflicts, got %+v", gotConflictCodec)
	}
}

// TestCarryFailureRecoveryVirtualEvidenceV3PerformsNoWrites tests that the carry
// is a pure memory operation: it performs no catalog/store/resolver writes, does not
// mutate the input row in place, and keeps ProbeUpdatedAt nil.
func TestCarryFailureRecoveryVirtualEvidenceV3PerformsNoWrites(t *testing.T) {
	matchingURI := "virtual://movie/tt12345?result=cand-alpha"
	file := &models.MediaFile{
		ID:         720,
		FilePath:   matchingURI,
		Container:  "mkv",
		CodecVideo: "hevc",
		VideoTracks: []models.VideoTrack{{
			Codec:    "hevc",
			BitDepth: 10,
			Width:    3840,
			Height:   2160,
			// FrameRate and Bitrate missing
		}},
	}

	record := &playback.AttemptRecordV3{
		CurrentPlan: playback.PlanV3{
			EffectiveVirtualURI: matchingURI,
			Source: playback.SourceDescriptorV3{
				MediaFileID: 720,
				Container:   "mkv",
				VideoCodec:  "hevc",
				BitDepth:    10,
				Width:       3840,
				Height:      2160,
				FrameRate:   23.976,
				BitrateKbps: 20_000,
			},
		},
	}

	carried := carryFailureRecoveryVirtualEvidenceV3(record, matchingURI, file)
	if carried == file {
		t.Fatal("expected carried file to be a copy")
	}

	// Assert input file was not mutated in place
	if file.Bitrate != 0 || file.VideoTracks[0].Bitrate != 0 || file.VideoTracks[0].FrameRate != "" {
		t.Fatal("input file was mutated in place")
	}
	if file.ProbeUpdatedAt != nil || carried.ProbeUpdatedAt != nil {
		t.Fatal("ProbeUpdatedAt must not be set on input or carried file")
	}
}
