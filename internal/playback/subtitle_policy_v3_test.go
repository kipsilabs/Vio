package playback

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

func TestResolveSubtitlePolicyV3RendersCEA608AsTextArtifact(t *testing.T) {
	file := detailedFixtureFileV3()
	file.ExternalSubtitles = nil
	file.SubtitleTracks = []models.SubtitleTrack{{Codec: "eia_608"}}
	req := validStartRequestV3()
	index := 0
	req.SubtitleTrackIndex = &index

	result := ResolveSubtitlePolicyV3(file, req, true, DeliveryClassOriginalHTTPV3, nil)

	if result.Terminal != nil || result.RequiresBurn || result.Decision.Mode != SubtitleRenderV3 {
		t.Fatalf("CEA-608 should render as a client-styled text artifact: %#v", result)
	}
	if result.Claims.Reason != "client_render_supported" || result.Claims.BitmapOverlay {
		t.Fatalf("CEA-608 claims = %#v", result.Claims)
	}
}

func TestResolveSubtitlePolicyV3DoesNotOfferDVBTeletextAsClientBitmap(t *testing.T) {
	file := detailedFixtureFileV3()
	file.ExternalSubtitles = nil
	file.SubtitleTracks = []models.SubtitleTrack{{Codec: "dvb_teletext"}}
	req := validStartRequestV3()
	index := 0
	req.SubtitleTrackIndex = &index

	result := ResolveSubtitlePolicyV3(file, req, true, DeliveryClassOriginalHTTPV3, nil)

	if result.Terminal != nil || !result.RequiresBurn || result.Decision.Mode != SubtitleBurnInV3 {
		t.Fatalf("DVB teletext must stay on the server fallback: %#v", result)
	}
	if !result.Claims.BitmapOverlay {
		t.Fatalf("DVB teletext burn-in must retain bitmap overlay semantics: %#v", result.Claims)
	}
}

func TestResolveSubtitlePolicyV3UnknownCodecIsExplicitlyUnsupported(t *testing.T) {
	file := detailedFixtureFileV3()
	file.ExternalSubtitles = nil
	file.SubtitleTracks = []models.SubtitleTrack{{Codec: "arib_caption"}}
	req := validStartRequestV3()
	index := 0
	req.SubtitleTrackIndex = &index

	for _, transcodeAllowed := range []bool{true, false} {
		result := ResolveSubtitlePolicyV3(file, req, transcodeAllowed, DeliveryClassOriginalHTTPV3, nil)
		if result.Terminal == nil || result.Terminal.Reason != "subtitle_codec_unsupported" {
			t.Fatalf("transcodeAllowed=%v: unknown codec must be explicitly unsupported, got %#v", transcodeAllowed, result)
		}
	}
}

func TestResolveSubtitlePolicyV3BurnsFFmpegBitmapAliases(t *testing.T) {
	file := detailedFixtureFileV3()
	file.ExternalSubtitles = nil
	file.SubtitleTracks = []models.SubtitleTrack{{Codec: "dvdsub"}}
	req := validStartRequestV3()
	index := 0
	req.SubtitleTrackIndex = &index

	result := ResolveSubtitlePolicyV3(file, req, true, DeliveryClassOriginalHTTPV3, nil)
	if result.Terminal != nil || !result.RequiresBurn || result.Decision.Mode != SubtitleBurnInV3 || !result.Claims.BitmapOverlay {
		t.Fatalf("dvdsub alias must burn in like dvd_subtitle: %#v", result)
	}
}

func TestPlanPlaybackV3TranscodeUsesHLSDeliverySubtitleCapabilities(t *testing.T) {
	file := detailedFixtureFileV3()
	file.VideoTracks[0].VideoRange = "SDR"
	file.VideoTracks[0].VideoRangeType = "SDR"
	file.VideoTracks[0].ColorTransfer = "bt709"
	file.SubtitleTracks = []models.SubtitleTrack{{Codec: "srt"}}
	req := validStartRequestV3()
	// A fixed rung forces the HLS transcode route; the fixture's direct
	// original HTTP renders embedded text while the HLS delivery cannot.
	req.QualityPreference = "1080p"
	req.Capabilities.VideoDecode = []VideoDecodeCapabilityV3{{Codec: "hevc", Profiles: []string{"main 10"}, Levels: []int{153}, BitDepths: []int{10}, MaxWidth: 3840, MaxHeight: 2160, MaxFrameRate: 60, MaxBitrateKbps: 80_000, Hardware: true}}
	index := 0
	req.SubtitleTrackIndex = &index
	req.SubtitleTrackID = TrackIDV3(file.ID, "subtitle", index)

	result := PlanPlaybackV3(PlannerInputV3{Request: req, RequestedFile: file, EffectiveFile: file, AudioTrackIndex: 0, Settings: PlannerSettingsV3{TranscodeEnabled: true, Allow4KTranscode: true}, Registry: testTransformationRegistryV3()})
	if result.Plan == nil || result.Plan.Delivery != DeliveryTranscodeHLSV3 {
		t.Fatalf("result = %s", ExplainPlannerResultV3(result))
	}
	if result.Plan.Subtitle.Mode == SubtitleRenderV3 {
		t.Fatalf("HLS transcode plan claims client render the HLS engine cannot honor: %#v", result.Plan.Subtitle)
	}
	if result.Plan.Subtitle.Mode != SubtitleBurnInV3 || result.Plan.Claims.Subtitles.Reason != "server_burn_in_required" {
		t.Fatalf("subtitle decision = %#v claims = %#v", result.Plan.Subtitle, result.Plan.Claims.Subtitles)
	}
	if result.SubtitleTrackIndex != index {
		t.Fatalf("subtitle track index = %d", result.SubtitleTrackIndex)
	}
}

// A carried ordinal from a richer version (the prod case: a 35-38 track
// version resumed onto a 3-track edition) must not hard-fail the start. There
// is no carried-vs-explicit wire flag, so the policy degrades the selection to
// subtitles-off instead of terminalling the whole plan.
func TestResolveSubtitlePolicyV3OutOfRangeDegradesToOff(t *testing.T) {
	file := detailedFixtureFileV3()
	file.ExternalSubtitles = nil
	file.SubtitleTracks = []models.SubtitleTrack{{Codec: "subrip"}, {Codec: "subrip"}, {Codec: "subrip"}}
	req := validStartRequestV3()
	index := 20
	req.SubtitleTrackIndex = &index
	req.SubtitleTrackID = TrackIDV3(file.ID, "subtitle", index)

	result := ResolveSubtitlePolicyV3(file, req, true, DeliveryClassOriginalHTTPV3, nil)

	if result.Terminal != nil {
		t.Fatalf("out-of-range carried selection must not terminal the start: %#v", result.Terminal)
	}
	if result.Decision.Mode != SubtitleOffV3 || result.SelectedIndex != -1 || result.TransportIndex != -1 {
		t.Fatalf("out-of-range selection = %#v, want subtitles off", result)
	}
}

func TestResolveSubtitlePolicyV3InRangeSelectionStillResolves(t *testing.T) {
	file := detailedFixtureFileV3()
	file.ExternalSubtitles = nil
	file.SubtitleTracks = []models.SubtitleTrack{{Codec: "subrip"}}
	req := validStartRequestV3()
	index := 0
	req.SubtitleTrackIndex = &index

	result := ResolveSubtitlePolicyV3(file, req, true, DeliveryClassOriginalHTTPV3, nil)

	if result.Terminal != nil || result.Decision.Mode != SubtitleRenderV3 || result.SelectedIndex != 0 {
		t.Fatalf("in-range selection = %#v, want a render of track 0", result)
	}
}

// A subtitle identity still bound to a different (previous) file names no
// track on the effective file; it must degrade to off, not terminal.
func TestResolveSubtitlePolicyV3ForeignIdentityDegradesToOff(t *testing.T) {
	file := detailedFixtureFileV3()
	file.SubtitleTracks = []models.SubtitleTrack{{Codec: "subrip"}}
	req := validStartRequestV3()
	req.SubtitleTrackID = TrackIDV3(999, "subtitle", 0)

	result := ResolveSubtitlePolicyV3(file, req, true, DeliveryClassOriginalHTTPV3, nil)

	if result.Terminal != nil || result.Decision.Mode != SubtitleOffV3 || result.SelectedIndex != -1 {
		t.Fatalf("stale carried identity = %#v, want subtitles off", result)
	}
}

func TestPlanPlaybackV3OutOfRangeSubtitleDegradesToOff(t *testing.T) {
	file := detailedFixtureFileV3()
	file.VideoTracks[0].VideoRange = "SDR"
	file.VideoTracks[0].VideoRangeType = "SDR"
	file.VideoTracks[0].ColorTransfer = "bt709"
	file.SubtitleTracks = []models.SubtitleTrack{{Codec: "subrip"}, {Codec: "subrip"}, {Codec: "subrip"}}
	req := validStartRequestV3()
	req.Capabilities.VideoDecode = []VideoDecodeCapabilityV3{{
		Codec: "hevc", Profiles: []string{"main 10"}, Levels: []int{153},
		BitDepths: []int{10}, MaxWidth: 3840, MaxHeight: 2160, MaxFrameRate: 60,
		MaxBitrateKbps: 80_000, Hardware: true,
	}}
	index := 20
	req.SubtitleTrackIndex = &index
	req.SubtitleTrackID = TrackIDV3(file.ID, "subtitle", index)

	result := PlanPlaybackV3(PlannerInputV3{
		Request: req, RequestedFile: file, EffectiveFile: file, AudioTrackIndex: 0,
		Settings: PlannerSettingsV3{TranscodeEnabled: false}, Registry: testTransformationRegistryV3(),
	})

	if result.Terminal != nil {
		t.Fatalf("out-of-range carried selection terminalled the plan: %s", ExplainPlannerResultV3(result))
	}
	if result.Plan == nil || result.Plan.Subtitle.Mode != SubtitleOffV3 || result.Plan.SelectedTracks.Subtitle != nil {
		t.Fatalf("plan = %#v, want subtitles off and no selected subtitle", result.Plan)
	}
}

// The plan's audio and subtitle inventories must be the effective file's, not
// the requested edition's, so a version substitution cannot advertise tracks
// the playing source does not have.
func TestPlanPlaybackV3InventoriesFollowEffectiveFile(t *testing.T) {
	requested := detailedFixtureFileV3()
	requested.AudioTracks = []models.AudioTrack{{Codec: "eac3", Language: "eng", Channels: 6}}
	requested.SubtitleTracks = []models.SubtitleTrack{{Language: "eng", Codec: "subrip"}}

	effective := detailedFixtureFileV3()
	effective.ID = 99
	effective.AudioTracks = []models.AudioTrack{{Codec: "aac", Language: "jpn", Channels: 2, Default: true}}
	effective.SubtitleTracks = []models.SubtitleTrack{{Language: "jpn", Codec: "ass"}}
	for i := range effective.VideoTracks {
		effective.VideoTracks[i].VideoRange = "SDR"
		effective.VideoTracks[i].VideoRangeType = "SDR"
		effective.VideoTracks[i].ColorTransfer = "bt709"
	}
	req := validStartRequestV3()
	req.Capabilities.VideoDecode = []VideoDecodeCapabilityV3{{
		Codec: "hevc", Profiles: []string{"main 10"}, Levels: []int{153},
		BitDepths: []int{10}, MaxWidth: 3840, MaxHeight: 2160, MaxFrameRate: 60,
		MaxBitrateKbps: 80_000, Hardware: true,
	}}

	result := PlanPlaybackV3(PlannerInputV3{
		Request: req, RequestedFile: requested, EffectiveFile: effective, AudioTrackIndex: 0,
		Settings: PlannerSettingsV3{TranscodeEnabled: false}, Registry: testTransformationRegistryV3(),
	})
	if result.Plan == nil {
		t.Fatalf("plan failed: %s", ExplainPlannerResultV3(result))
	}
	if len(result.Plan.AudioTracks) != 1 || result.Plan.AudioTracks[0].Codec != "aac" || result.Plan.AudioTracks[0].Language != "jpn" {
		t.Fatalf("audio tracks leaked the requested file: %#v", result.Plan.AudioTracks)
	}
	if len(result.Plan.Subtitle.Inventory) != 1 || result.Plan.Subtitle.Inventory[0].Codec != "ass" ||
		result.Plan.Subtitle.Inventory[0].Language != "jpn" ||
		result.Plan.Subtitle.Inventory[0].TrackID != TrackIDV3(effective.ID, "subtitle", 0) {
		t.Fatalf("subtitle inventory leaked the requested file: %#v", result.Plan.Subtitle.Inventory)
	}
}

func TestClientRenderableBitmapSubtitleV3UsesExactCodecFamilies(t *testing.T) {
	for _, codec := range []string{"pgs", "pgssub", "hdmv_pgs_subtitle", "dvd_subtitle", "dvdsub", "dvb_subtitle", "dvbsub", "vobsub"} {
		if !isClientRenderableBitmapSubtitleV3(codec) {
			t.Errorf("expected client-renderable bitmap codec: %s", codec)
		}
	}
	for _, codec := range []string{"dvb_teletext", "hdmv_text_subtitle", "arib_caption", "eia_608"} {
		if isClientRenderableBitmapSubtitleV3(codec) {
			t.Errorf("must not advertise unsupported client bitmap codec: %s", codec)
		}
	}
}

// A selection carried over from another file — the next episode routinely has
// a different track layout — must degrade to subtitles-off, not refuse
// playback. This is the direct-start path; the version-switch remap in the
// handlers degrades the same way.
func TestResolveSubtitlePolicyV3DegradesOutOfRangeSelectionToOff(t *testing.T) {
	file := detailedFixtureFileV3()
	file.ExternalSubtitles = nil
	file.SubtitleTracks = []models.SubtitleTrack{{Codec: "subrip"}}
	req := validStartRequestV3()
	index := 7
	req.SubtitleTrackIndex = &index

	result := ResolveSubtitlePolicyV3(file, req, true, DeliveryClassOriginalHTTPV3, nil)

	if result.Terminal != nil {
		t.Fatalf("an out-of-range subtitle selection must not refuse playback: %#v", result.Terminal)
	}
	if result.Decision.Mode != SubtitleOffV3 || result.SelectedIndex != -1 {
		t.Fatalf("expected subtitles-off fallback: %#v", result)
	}
	if !result.Degraded {
		t.Fatal("the fallback was not reported as a degradation")
	}
}

// Malformed selections stay terminal: a bad identity is a client bug, and
// swallowing it would make that bug harder to find.
func TestResolveSubtitlePolicyV3StillRejectsInvalidIdentity(t *testing.T) {
	file := detailedFixtureFileV3()
	req := validStartRequestV3()
	req.SubtitleTrackID = "not-a-track-id"

	result := ResolveSubtitlePolicyV3(file, req, true, DeliveryClassOriginalHTTPV3, nil)

	if result.Terminal == nil || result.Terminal.Reason != "subtitle_track_invalid" {
		t.Fatalf("a malformed identity must stay terminal: %#v", result)
	}
}
