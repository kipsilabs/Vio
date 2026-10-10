package handlers

import (
	"context"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// TestVirtualNonIndictingReplanKeepsPinnedCandidateAndStripsDV7 pins the
// same-file invariant for a display-driven fallback: a failure_recovery on a
// pinned virtual Dolby Vision Profile 7 candidate that does not indict the
// release must keep that candidate and advance to the server DV7->HDR10 strip
// rung, never substitute a different release.
func TestVirtualNonIndictingReplanKeepsPinnedCandidateAndStripsDV7(t *testing.T) {
	source := v3HandlerFixtureFile(t)
	source.ID = 710
	source.ContentID = "movie-dv7"
	// Neutral catalog row: the provider listing binds the session to the
	// result=-suffixed pin, so the replan takes the fall-through rehydration
	// path where the release swap used to happen.
	source.FilePath = "virtual://movie/tt-dv7"
	source.VirtualOwnerInstallationID = 5
	source.Container = "virtual"
	source.CodecVideo = "hevc"
	source.Resolution = "2160p"
	source.Bitrate = 32_000
	source.HDR = true
	source.VideoTracks = []models.VideoTrack{{
		Codec: "hevc", Profile: "main 10", Level: 153, Width: 3840, Height: 2160,
		FrameRate: "23.976", Bitrate: 32_000, BitDepth: 10, PixelFormat: "yuv420p10le",
		VideoRange: "DolbyVision", VideoRangeType: "DOVIWithEL", DVProfile: 7,
		DVBLCompatID: 1, DVConfigPresent: true, DVBLCompatIDPresent: true, DVBLPresent: true, DVRPUPresent: true,
	}}

	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0), mapPlaybackFileResolver{files: map[int]*models.MediaFile{source.ID: source}})
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"allow_4k_transcode": "true"}}
	handler.PlaybackConfig = playbackTestConfig(writePlaybackTestFFmpeg(t), t.TempDir())
	stubCopySeekAnchorV3(handler)
	presetLocalRegistryV3(handler, playback.NewTransformationRegistryV3([]playback.TransformationSpecV3{
		{Name: playback.TransformationAudioToAACV3, RecipeVersion: playback.TransformationAudioToAACRecipeVersionV3, Available: true},
		{Name: playback.TransformationServerDV7HDR10V3, RecipeVersion: playback.TransformationServerDV7HDR10RecipeVersionV3, Available: true},
	}))

	pinnedURI := "virtual://movie/tt-dv7?result=pinned"
	handler.VirtualPlaybackStreamLister = VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
		return []VirtualPlaybackStream{{
			ID: "pinned", URI: pinnedURI,
			Resolution: source.Resolution, CodecVideo: source.CodecVideo, CodecAudio: source.CodecAudio, Container: "mkv",
		}}, nil
	})
	type resolveCall struct {
		virtualURI string
		excluded   []string
		preferred  string
		candidate  string
	}
	var resolveCalls []resolveCall
	handler.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
		return "http://127.0.0.1:9/stream?path=" + path, nil
	})
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(_ context.Context, virtualURI string, _ int, _ int, _ string, _ bool, excluded []string, preferred string) (ResolvedVirtualMedia, error) {
		resolveCalls = append(resolveCalls, resolveCall{
			virtualURI: virtualURI,
			excluded:   append([]string(nil), excluded...),
			preferred:  preferred,
			candidate:  "pinned",
		})
		return ResolvedVirtualMedia{URL: "http://127.0.0.1:9/stream", URI: virtualURI, CandidateID: "pinned"}, nil
	})
	handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, file *models.MediaFile) (*models.MediaFile, error) {
		file.VideoTracks = source.VideoTracks
		file.AudioTracks = source.AudioTracks
		file.CodecVideo, file.CodecAudio, file.Resolution, file.Container, file.HDR = "hevc", "aac", "2160p", "mkv", true
		return file, nil
	}

	start := v3HandlerStartRequest()
	start.FileID = source.ID
	start.QualityPreference = "original"
	start.ClientFeatures = append(start.ClientFeatures, playback.FeatureClientVideoTransforms)
	start.Capabilities.CodecsVideo = []string{"hevc", "h264"}
	start.Capabilities.CodecsVideoHardware = []string{"hevc", "h264"}
	start.Capabilities.Containers = []string{"mp4", "mkv"}
	start.Capabilities.MaxResolution = "2160p"
	start.Capabilities.VideoDecode = []playback.VideoDecodeCapabilityV3{{
		Codec: "hevc", Profiles: []string{"main 10"}, Levels: []int{153}, BitDepths: []int{10},
		MaxWidth: 3840, MaxHeight: 2160, MaxFrameRate: 60, MaxBitrateKbps: 80_000, Hardware: true,
	}}
	start.Capabilities.HDR = true
	start.Capabilities.HDRDetails = &playback.HDRCapabilitiesV3{HDR10: true}
	start.ClientPlaybackContext.Output.HDRDetails = start.Capabilities.HDRDetails
	direct := start.ClientPlaybackContext.Deliveries[playback.DeliveryClassOriginalHTTPV3]
	direct.Transformations = []playback.TransformationV3{{
		Name: playback.ClientDV7ToHDR10V3, Executor: playback.ExecutorClientV3, RecipeVersion: playback.ClientDVTransformVersionV3,
	}}
	start.ClientPlaybackContext.Deliveries[playback.DeliveryClassOriginalHTTPV3] = direct
	start.ClientPlaybackContext.Deliveries[playback.DeliveryClassProgressiveV3] = playback.DeliveryCapabilityV3{
		Enabled: true, SupportedOnDevice: true,
		Containers: []string{"mp4"}, VideoCodecs: []string{"hevc", "h264"}, AudioDecodeCodecs: []string{"aac"},
	}
	start.ClientPlaybackContext.Deliveries[playback.DeliveryClassHLSV3] = playback.DeliveryCapabilityV3{
		Enabled: true, SupportedOnDevice: true,
		Containers: []string{"hls"}, VideoCodecs: []string{"h264"}, AudioDecodeCodecs: []string{"aac"},
	}

	started := startV3PlaybackForHandlerTest(t, handler, start)
	if started.PlaybackPlan == nil || started.PlaybackPlan.DecisionReason != "client_dv7_to_hdr10" {
		t.Fatalf("start plan = %#v (reason %q), want the client DV7->HDR10 direct fallback", started.PlaybackPlan, started.PlaybackPlan.DecisionReason)
	}

	resolveCalls = nil
	failedKey := playback.PlanAttemptKeyV3(*started.PlaybackPlan, start.ClientPlaybackContext.Output.OutputContextID, nil)
	response := postPlaybackReplanV3(t, handler, started.SessionID, playback.ReplanRequestV3{
		ProtocolVersion:       playback.ProtocolV3,
		Operation:             playback.ReplanOperationFailureRecoveryV3,
		PlaybackAttemptID:     start.PlaybackAttemptID,
		ReplanRequestID:       "dv7-same-file-0001",
		FailedPlanID:          started.PlaybackPlan.PlanID,
		PlanAttemptID:         "dv7-same-file-0001",
		PlanAttemptKey:        failedKey,
		AttemptedPlanKeys:     []string{failedKey},
		AttemptCount:          1,
		QualityPreference:     "original",
		SelectedTracks:        started.PlaybackPlan.SelectedTracks,
		Failure:               playback.FailureV3{Classification: "player_failure"},
		Capabilities:          start.Capabilities,
		ClientPlaybackContext: start.ClientPlaybackContext,
	})
	if response.Terminal != nil || response.PlaybackPlan == nil {
		t.Fatalf("replan terminal=%#v", response.Terminal)
	}
	if response.PlaybackPlan.EffectiveMediaFileID != source.ID {
		t.Fatalf("effective file = %d, want the unchanged file %d", response.PlaybackPlan.EffectiveMediaFileID, source.ID)
	}
	hasStrip := false
	for _, transformation := range response.PlaybackPlan.Transformations {
		if transformation.Name == playback.TransformationServerDV7HDR10V3 {
			hasStrip = true
		}
	}
	if !hasStrip {
		t.Fatalf("replan transformations = %#v, want the server DV7->HDR10 strip", response.PlaybackPlan.Transformations)
	}

	// The rehydration must keep the pinned session candidate and must not
	// exclude it: a non-indicting failure is a transformation change, not a
	// release substitution.
	found := false
	for _, call := range resolveCalls {
		if call.preferred != "pinned" {
			continue
		}
		found = true
		if len(call.excluded) != 0 {
			t.Fatalf("rehydration excluded %v for a non-indicting failure; want none", call.excluded)
		}
		if call.candidate != "pinned" {
			t.Fatalf("rehydration substituted candidate %q for the pinned one", call.candidate)
		}
	}
	if !found {
		t.Fatal("rehydration never resolved with the session-bound candidate preferred")
	}
}
