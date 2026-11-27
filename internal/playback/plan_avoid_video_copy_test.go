package playback

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

// TestPlanPlaybackAvoidVideoCopyPlansDecodedHLS proves the planner flag the
// seek fallback uses really produces the seekable delivery. A copy-compatible
// source plans the progressive remux normally, and the same input with
// AvoidVideoCopy plans server_transcode_hls instead, named in the decision
// reason so the client can tell a seek-driven delivery change from a quality or
// compatibility one.
func TestPlanPlaybackAvoidVideoCopyPlansDecodedHLS(t *testing.T) {
	file := &models.MediaFile{
		ID: 42, FilePath: "/media/movie.mkv", Container: "mkv", CodecVideo: "h264", CodecAudio: "aac",
		Resolution: "1080p", Bitrate: 8_000, AudioChannels: 2,
		VideoTracks: []models.VideoTrack{{Codec: "h264", Profile: "high", Level: 41, Width: 1920, Height: 1080, FrameRate: "24", Bitrate: 8_000, BitDepth: 8, VideoRange: "SDR", VideoRangeType: "SDR"}},
		AudioTracks: []models.AudioTrack{{Codec: "aac", Channels: 2, Layout: "stereo"}},
	}
	req := validStartRequestV3()
	req.Capabilities.Containers = []string{"mp4"}
	req.Capabilities.CodecsVideo = []string{"h264"}
	req.Capabilities.CodecsAudio = []string{"aac"}
	req.Capabilities.VideoDecode = []VideoDecodeCapabilityV3{{Codec: "h264", Profiles: []string{"high"}, Levels: []int{41}, BitDepths: []int{8}, MaxWidth: 1920, MaxHeight: 1080, MaxFrameRate: 60, MaxBitrateKbps: 20_000, Hardware: true}}
	input := PlannerInputV3{
		Request: req, RequestedFile: file, EffectiveFile: file, AudioTrackIndex: 0,
		Settings: PlannerSettingsV3{TranscodeEnabled: true},
		Registry: testTransformationRegistryV3(),
	}

	natural := PlanPlaybackV3(input)
	if natural.Plan == nil || natural.Plan.Delivery != DeliveryRemuxProgressiveV3 {
		t.Fatalf("natural plan = %#v, want the progressive copy control", natural.Plan)
	}

	input.AvoidVideoCopy = true
	decoded := PlanPlaybackV3(input)
	if decoded.Plan == nil || decoded.Plan.Delivery != DeliveryTranscodeHLSV3 || decoded.PlayMethod != PlayTranscode {
		t.Fatalf("avoid-copy plan = %#v, want server_transcode_hls", decoded.Plan)
	}
	if decoded.Plan.DecisionReason != decisionReasonCopySeekAnchorUnusableV3 {
		t.Fatalf("decision_reason = %q, want %q", decoded.Plan.DecisionReason, decisionReasonCopySeekAnchorUnusableV3)
	}
}
