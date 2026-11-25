package handlers

import (
	"context"
	"testing"

	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// playabilityHandler is a local-execution handler with the baseline
// H.264/AAC transformations and no tone-map executor. That is the measured
// shape behind hdr_transcode_unsupported: the server can encode, but has no
// validated HDR recipe.
func playabilityHandler(t *testing.T) *PlaybackHandler {
	t.Helper()
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	handler.PlaybackConfig = func() config.PlaybackConfig {
		return config.PlaybackConfig{HWAccel: playback.HWAccelNone, TranscodeEnabled: true}
	}
	presetLocalRegistryV3(handler, playback.NewTransformationRegistryV3([]playback.TransformationSpecV3{
		{Name: playback.TransformationAudioToAACV3, RecipeVersion: playback.TransformationAudioToAACRecipeVersionV3, Available: true},
		{Name: playback.TransformationVideoToH264V3, RecipeVersion: playback.TransformationVideoToH264RecipeVersionV3, Available: true},
	}))
	return handler
}

func playabilityClientFromStart(start playback.StartRequestV3) VersionPlayabilityClient {
	return VersionPlayabilityClient{
		ClientFeatures:  start.ClientFeatures,
		Capabilities:    start.Capabilities,
		PlaybackContext: start.ClientPlaybackContext,
	}
}

// hdrPlayabilityFile is a complete, route-plannable HDR10 source. It is not
// playable by a client with no HDR output, and this server has no validated
// HDR transcode recipe, so the start would answer hdr_transcode_unsupported.
func hdrPlayabilityFile(t *testing.T) *models.MediaFile {
	t.Helper()
	file := v3HandlerFixtureFile(t)
	file.Container = "mkv"
	file.CodecVideo = "hevc"
	file.Resolution = "2160p"
	file.Bitrate = 20_000
	file.HDR = true
	file.VideoTracks = []models.VideoTrack{{
		Codec: "hevc", Profile: "Main 10", Level: 153, Width: 3840, Height: 2160,
		FrameRate: "24000/1001", Bitrate: 20_000, BitDepth: 10,
		VideoRange: "HDR", VideoRangeType: "HDR10",
		ColorTransfer: "smpte2084", ColorPrimaries: "bt2020", ColorSpace: "bt2020nc",
	}}
	file.AudioTracks = []models.AudioTrack{{Codec: "aac", Channels: 2, Layout: "stereo", Default: true}}
	return file
}

func TestEvaluateVersionPlayabilityRefusesATerminalHDRRow(t *testing.T) {
	handler := playabilityHandler(t)
	file := hdrPlayabilityFile(t)
	client := playabilityClientFromStart(ladderTVStartRequest())

	got := handler.EvaluateVersionPlayability(context.Background(), []*models.MediaFile{file}, client)
	verdict, ok := got[file.ID]
	if !ok {
		t.Fatalf("no verdict for HDR row %d: %+v", file.ID, got)
	}
	if verdict.Playable {
		t.Fatalf("HDR row reported playable: %+v", verdict)
	}
	if verdict.Reason != playback.TerminalHDRTranscodeUnsupportedV3 {
		t.Fatalf("reason = %q, want %q", verdict.Reason, playback.TerminalHDRTranscodeUnsupportedV3)
	}
	if verdict.Message == "" {
		t.Fatal("terminal verdict carried no message")
	}
}

// A retryable refusal (here the incomplete-metadata terminal) is not a verdict
// about the release. The row must stay selectable, and an anchor-budget expiry
// at a stored resume is the same class: retry, not dead. The file is SDR, so a
// probe can still make it direct-playable; only a range the client can never
// present turns the same incomplete metadata into a terminal (see
// TestEvaluateVersionPlayabilityRefusesAnUnprobedHDRRow).
func TestEvaluateVersionPlayabilityLeavesRetryableRefusalsUnknown(t *testing.T) {
	handler := playabilityHandler(t)
	file := v3HandlerFixtureFile(t)
	// Drop a field the video route requires. The planner answers the
	// retryable source_metadata_incomplete rather than a device verdict.
	file.VideoTracks[0].FrameRate = ""
	client := playabilityClientFromStart(ladderTVStartRequest())

	got := handler.EvaluateVersionPlayability(context.Background(), []*models.MediaFile{file}, client)
	if verdict, ok := got[file.ID]; ok {
		t.Fatalf("retryable refusal was reported as a playability verdict: %+v", verdict)
	}
}

// unprobedHDRDVPlayabilityFile has the shape production rows had: an HDR/DV
// provider row with no probe stamp and no bit depth, frame rate or bitrate.
// This handler has no HDR recipe, so the declared row plans to
// hdr_transcode_unsupported and the evaluator must report that terminal
// instead of leaving the row unknown.
func unprobedHDRDVPlayabilityFile(t *testing.T) *models.MediaFile {
	t.Helper()
	file := v3HandlerFixtureFile(t)
	file.Container = "virtual"
	file.FilePath = "virtual://series/tmdb/259140/1/3?result=d69876f03ff1f128a827038c"
	file.CodecVideo = "hevc"
	file.CodecAudio = "eac3"
	file.Resolution = "1080p"
	file.Bitrate = 0
	file.HDR = true
	file.ProbeUpdatedAt = nil
	file.VideoTracks = []models.VideoTrack{{
		Codec: "hevc", Width: 1920, Height: 1080,
		VideoRange: "DolbyVision", VideoRangeType: "DOVI",
		DVConfigPresent: false, DVBLCompatIDPresent: false,
	}}
	file.AudioTracks = []models.AudioTrack{{Codec: "eac3", Default: true, Language: "DEU"}}
	return file
}

func TestEvaluateVersionPlayabilityRefusesAnUnprobedHDRRow(t *testing.T) {
	handler := playabilityHandler(t)
	file := unprobedHDRDVPlayabilityFile(t)
	client := playabilityClientFromStart(ladderTVStartRequest())

	got := handler.EvaluateVersionPlayability(context.Background(), []*models.MediaFile{file}, client)
	verdict, ok := got[file.ID]
	if !ok {
		t.Fatalf("no verdict for unprobed HDR row %d: %+v", file.ID, got)
	}
	if verdict.Playable {
		t.Fatalf("unprobed HDR row reported playable: %+v", verdict)
	}
	if verdict.Reason != playback.TerminalHDRTranscodeUnsupportedV3 {
		t.Fatalf("reason = %q, want %q", verdict.Reason, playback.TerminalHDRTranscodeUnsupportedV3)
	}
	if verdict.Message == "" {
		t.Fatal("terminal verdict carried no message")
	}
}

func TestEvaluateVersionPlayabilityKeepsAPlayableRowPlayable(t *testing.T) {
	handler := playabilityHandler(t)
	file := v3HandlerFixtureFile(t)
	client := playabilityClientFromStart(v3HandlerStartRequest())

	got := handler.EvaluateVersionPlayability(context.Background(), []*models.MediaFile{file}, client)
	verdict, ok := got[file.ID]
	if !ok || !verdict.Playable {
		t.Fatalf("playable row verdict = %+v, present=%t", verdict, ok)
	}
}

// CheckVersionsWithPlayability is the seam the v2 operation calls: it must
// report the planner's terminal on the same result row the picker renders.
func TestCheckVersionsWithPlayabilityReportsThePlannerVerdict(t *testing.T) {
	file := hdrPlayabilityFile(t)
	catalogHandler := NewCatalogResourceHandler(nil)
	catalogHandler.FileResolver = testPlaybackFileResolver{file: file}
	catalogHandler.ItemAccess = allowAllPlaybackItemAccess{}
	catalogHandler.SetVersionPlayability(playabilityHandler(t))
	client := playabilityClientFromStart(ladderTVStartRequest())

	results := catalogHandler.CheckVersionsWithPlayability(context.Background(), []int{file.ID}, client, false)
	if len(results) != 1 {
		t.Fatalf("results = %+v", results)
	}
	result := results[0]
	if result.Playability == nil {
		t.Fatalf("result carried no playability verdict: %+v", result)
	}
	if result.Playability.Playable || result.Playability.Reason != playback.TerminalHDRTranscodeUnsupportedV3 {
		t.Fatalf("playability = %+v, want the HDR terminal", result.Playability)
	}
}

// The same terminal must survive the versions-check projection for a row that
// has never been probed: the detail picker and the watch-page menu key on this
// result, so an omitted verdict leaves the refused row selectable.
func TestCheckVersionsWithPlayabilityReportsTheUnprobedHDRVerdict(t *testing.T) {
	file := unprobedHDRDVPlayabilityFile(t)
	catalogHandler := NewCatalogResourceHandler(nil)
	catalogHandler.FileResolver = testPlaybackFileResolver{file: file}
	catalogHandler.ItemAccess = allowAllPlaybackItemAccess{}
	catalogHandler.SetVersionPlayability(playabilityHandler(t))
	client := playabilityClientFromStart(ladderTVStartRequest())

	results := catalogHandler.CheckVersionsWithPlayability(context.Background(), []int{file.ID}, client, false)
	if len(results) != 1 {
		t.Fatalf("results = %+v", results)
	}
	result := results[0]
	if result.Playability == nil {
		t.Fatalf("result carried no playability verdict: %+v", result)
	}
	if result.Playability.Playable || result.Playability.Reason != playback.TerminalHDRTranscodeUnsupportedV3 {
		t.Fatalf("playability = %+v, want the HDR terminal", result.Playability)
	}
	if result.Playability.Message == "" {
		t.Fatal("playability verdict carried no message")
	}
}
