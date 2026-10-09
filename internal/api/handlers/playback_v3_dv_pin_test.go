package handlers

import (
	"context"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// dv8BaseLayerFileV3 is a single-layer Dolby Vision Profile 8.1 Matroska whose
// HDR10 base layer an ordinary HEVC decoder can play. The fixture file is
// mutated in place by the resolver, so a test can simulate the catalog re-probe
// drift production saw without changing the row identity.
func dv8BaseLayerFileV3(t *testing.T) *models.MediaFile {
	t.Helper()
	file := v3HandlerFixtureFile(t)
	file.Container = "mkv"
	file.CodecVideo = "hevc"
	file.Resolution = "2160p"
	file.HDR = true
	file.VideoTracks[0] = models.VideoTrack{
		Codec: "hevc", Profile: "main 10", Level: 153, Width: 3840, Height: 2160,
		FrameRate: "24000/1001", Bitrate: 32_000, BitDepth: 10, PixelFormat: "yuv420p10le",
		VideoRange: "DolbyVision", VideoRangeType: "DOVI", DVProfile: 8, DVBLCompatID: 1,
		DVConfigPresent: true, DVBLCompatIDPresent: true, DVBLPresent: true,
	}
	return file
}

// dv8BaseLayerHandlerV3 wires a handler whose registry can serve the DV8
// base-layer route and fall back to plain HLS, shared by the DV pin tests.
func dv8BaseLayerHandlerV3(t *testing.T, file *models.MediaFile) *PlaybackHandler {
	t.Helper()
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0), testPlaybackFileResolver{file: file})
	stubCopySeekAnchorV3(handler)
	handler.PlaybackConfig = playbackTestConfig(writePlaybackTestFFmpeg(t), t.TempDir())
	presetLocalRegistryV3(handler, playback.NewTransformationRegistryV3([]playback.TransformationSpecV3{
		{Name: "audio_to_aac", RecipeVersion: "2", Available: true},
		{Name: "video_to_h264", RecipeVersion: "2", Available: true},
		{Name: playback.TransformationServerDV7HDR10V3, RecipeVersion: "1", Available: true},
	}))
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"allow_4k_transcode": "true"}}
	return handler
}

func dv8BaseLayerStartV3(file *models.MediaFile) playback.StartRequestV3 {
	start := v3HandlerStartRequest()
	start.FileID = file.ID
	start.Capabilities.CodecsVideo = []string{"hevc"}
	start.Capabilities.CodecsVideoHardware = []string{"hevc"}
	start.Capabilities.Containers = []string{"mkv"}
	start.Capabilities.MaxResolution = "2160p"
	start.Capabilities.HDR = true
	start.Capabilities.HDRDetails = &playback.HDRCapabilitiesV3{HDR10: true}
	start.Capabilities.VideoDecode = []playback.VideoDecodeCapabilityV3{{
		Codec: "hevc", Profiles: []string{"main 10"}, Levels: []int{153}, BitDepths: []int{10},
		MaxWidth: 3840, MaxHeight: 2160, MaxFrameRate: 60, MaxBitrateKbps: 80_000, Hardware: true,
	}}
	start.ClientPlaybackContext.Output.HDRDetails = start.Capabilities.HDRDetails
	start.ClientPlaybackContext.Deliveries[playback.DeliveryClassOriginalHTTPV3] = playback.DeliveryCapabilityV3{
		Enabled: true, SupportedOnDevice: true,
		Containers: []string{"mkv"}, VideoCodecs: []string{"hevc"}, AudioDecodeCodecs: []string{"aac"},
		HDRDetails:      start.Capabilities.HDRDetails,
		ValidatedClaims: []string{playback.ClaimClientDV8BaseLayerFallbackV3},
	}
	return start
}

// Same-file, same-session Dolby Vision stability: the start verifies Profile 8,
// a later replan's probe read of the same file reports 7, and the session must
// keep 8 throughout. Without the pin the replan would re-key off the drifted 7
// and move off the base-layer route.
func TestReplanKeepsPinnedDolbyVisionProfileOnDriftedProbe(t *testing.T) {
	file := dv8BaseLayerFileV3(t)
	handler := dv8BaseLayerHandlerV3(t, file)
	start := dv8BaseLayerStartV3(file)

	started := sessionStabilityStartV3(t, handler, start)
	plan := started.PlaybackPlan
	if plan.DecisionReason != "client_dv8_base_layer" || plan.Source.DVProfile != 8 {
		t.Fatalf("start plan = %s profile %d, want the DV8 base-layer route at profile 8", plan.DecisionReason, plan.Source.DVProfile)
	}

	// The same file is re-probed and now reports profile 7 (the drift). The row
	// identity is unchanged, so this is exactly a same-file replan re-read.
	file.VideoTracks[0].DVProfile = 7

	trackChange := playback.ReplanRequestV3{
		ProtocolVersion: playback.ProtocolV3, ClientFeatures: start.ClientFeatures,
		Operation: playback.ReplanOperationTrackChangeV3, PlaybackAttemptID: start.PlaybackAttemptID,
		ReplanRequestID: "dv-drift-track-0001", FailedPlanID: plan.PlanID, PlanAttemptID: "dv-drift-track-plan",
		PlanAttemptKey: plan.PlanAttemptKey, AttemptCount: 1,
		PositionSeconds: 10, SelectedTracks: plan.SelectedTracks,
		Capabilities:          start.Capabilities,
		ClientPlaybackContext: start.ClientPlaybackContext,
	}
	response := postPlaybackReplanV3(t, handler, started.SessionID, trackChange)
	if response.Terminal != nil || response.PlaybackPlan == nil {
		t.Fatalf("drifted replan terminal=%#v", response.Terminal)
	}
	if response.PlaybackPlan.Source.DVProfile != 8 {
		t.Fatalf("replan source dv_profile = %d, want the pinned 8", response.PlaybackPlan.Source.DVProfile)
	}
	if response.PlaybackPlan.DecisionReason != "client_dv8_base_layer" {
		t.Fatalf("replan decision = %q, want the base-layer route kept under the pin", response.PlaybackPlan.DecisionReason)
	}

	session, err := handler.sessionMgr.GetSession(started.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if session.DVProfile != 8 || session.DVProfilePin != (playback.DVPinV3{FileID: file.ID, Source: file.FilePath, Profile: 8}) {
		t.Fatalf("session DV = profile %d pin %#v, want profile 8 pin {%d 8}", session.DVProfile, session.DVProfilePin, file.ID)
	}
}

// applyDVPinReplacementV3 builds a replacement stream state through the real
// v3SessionStreamState builder — the same path a committed replan uses — and
// applies it to the session so the pin merge and rollback snapshot are
// exercised end to end.
func applyDVPinReplacementV3(t *testing.T, handler *PlaybackHandler, sessionID string, file *models.MediaFile, profile int) playback.SessionReplacementRollback {
	t.Helper()
	session, err := handler.sessionMgr.GetSession(sessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	result := playback.PlannerResultV3{
		Plan: &playback.PlanV3{Source: playback.SourceDescriptorV3{DVProfile: profile}},
	}
	state := handler.v3SessionStreamState(context.Background(), session, file, result, preparedTransportV3{}, mediaAuthModeV3{})
	rollback, err := handler.sessionMgr.ApplyReplacement(sessionID, playback.SessionReplacement{
		EffectiveMediaFileID: file.ID,
		StreamState:          state,
	})
	if err != nil {
		t.Fatalf("ApplyReplacement: %v", err)
	}
	return rollback
}

// A move from a Dolby Vision file to a different SDR file must re-arm the pin to
// the SDR identity at profile zero. Keeping the old DV8 pin would restore
// profile 8 onto bytes that never carried it.
func TestReplacementToDifferentSDRFileReArmsDVPin(t *testing.T) {
	file := dv8BaseLayerFileV3(t)
	handler := dv8BaseLayerHandlerV3(t, file)
	started := sessionStabilityStartV3(t, handler, dv8BaseLayerStartV3(file))
	if started.PlaybackPlan.Source.DVProfile != 8 {
		t.Fatalf("fixture start dv_profile = %d, want 8", started.PlaybackPlan.Source.DVProfile)
	}

	sdr := &models.MediaFile{ID: file.ID + 1000, FilePath: "/media/movie-sdr.mp4", Container: "mp4", CodecVideo: "h264", Resolution: "1080p"}
	_ = applyDVPinReplacementV3(t, handler, started.SessionID, sdr, 0)

	session, err := handler.sessionMgr.GetSession(started.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if want := (playback.DVPinV3{FileID: sdr.ID, Source: sdr.FilePath, Profile: 0}); session.DVProfilePin != want {
		t.Fatalf("pin after move to SDR = %#v, want %#v", session.DVProfilePin, want)
	}
	if session.DVProfile != 0 {
		t.Fatalf("SDR replacement kept DV profile %d, want 0", session.DVProfile)
	}
}

// The same-row virtual rotation keeps the row id but changes the pinned
// candidate URI: the rotated SDR release must still take the pin, so the old
// release's profile cannot describe the new bytes.
func TestReplacementSameRowSDRRotationReArmsDVPin(t *testing.T) {
	file := dv8BaseLayerFileV3(t)
	handler := dv8BaseLayerHandlerV3(t, file)
	started := sessionStabilityStartV3(t, handler, dv8BaseLayerStartV3(file))

	rotated := &models.MediaFile{ID: file.ID, FilePath: "virtual://movie/tt?result=B", Container: "mp4", CodecVideo: "h264", Resolution: "1080p"}
	_ = applyDVPinReplacementV3(t, handler, started.SessionID, rotated, 0)

	session, err := handler.sessionMgr.GetSession(started.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if want := (playback.DVPinV3{FileID: file.ID, Source: rotated.FilePath, Profile: 0}); session.DVProfilePin != want {
		t.Fatalf("pin after same-row rotation = %#v, want %#v", session.DVProfilePin, want)
	}
	if session.DVProfile != 0 {
		t.Fatalf("same-row SDR rotation kept DV profile %d, want 0", session.DVProfile)
	}
}

// A failed replacement rolls the session back to the previous route. The pin
// and the profile are one fact, so the rollback must restore both exactly; a
// restored profile paired with the withdrawn successor's pin is the mismatch
// this guards. The replacement moves the pin to a different, positively probed
// file so the restore is what returns it, independent of the SDR re-arming.
func TestFailedReplacementRollbackRestoresDVPinExactly(t *testing.T) {
	file := dv8BaseLayerFileV3(t)
	handler := dv8BaseLayerHandlerV3(t, file)
	started := sessionStabilityStartV3(t, handler, dv8BaseLayerStartV3(file))

	dv7 := &models.MediaFile{ID: file.ID + 1000, FilePath: "/media/movie-dv7.mp4", Container: "mkv", CodecVideo: "hevc", Resolution: "2160p"}
	rollback := applyDVPinReplacementV3(t, handler, started.SessionID, dv7, 7)

	if err := handler.sessionMgr.RollbackReplacement(started.SessionID, rollback); err != nil {
		t.Fatalf("RollbackReplacement: %v", err)
	}
	session, err := handler.sessionMgr.GetSession(started.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if session.MediaFileID != file.ID {
		t.Fatalf("rollback effective file = %d, want %d", session.MediaFileID, file.ID)
	}
	if want := (playback.DVPinV3{FileID: file.ID, Source: file.FilePath, Profile: 8}); session.DVProfilePin != want {
		t.Fatalf("rolled-back pin = %#v, want %#v", session.DVProfilePin, want)
	}
	if session.DVProfile != 8 {
		t.Fatalf("rolled-back DV profile = %d, want 8", session.DVProfile)
	}
}
