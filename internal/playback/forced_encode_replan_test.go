package playback

import (
	"strconv"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/tonemap"
)

// argValueAfter returns the argument immediately following flag, failing the
// test when the flag is absent.
func argValueAfter(t *testing.T, args []string, flag string) string {
	t.Helper()
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	t.Fatalf("args missing %s: %v", flag, args)
	return ""
}

func filterComplexArg(t *testing.T, args []string) string {
	t.Helper()
	return argValueAfter(t, args, "-filter_complex")
}

// TestFramesPerSegmentUsesSourceFrameRateGOP pins the GOP table: the segment
// must be an integer number of source frames, and an unknown rate keeps the
// historical 30 fps ceiling.
func TestFramesPerSegmentUsesSourceFrameRateGOP(t *testing.T) {
	tests := []struct {
		name string
		fps  float64
		want int
	}{
		{name: "23.976", fps: 24000.0 / 1001.0, want: 48},
		{name: "24", fps: 24, want: 48},
		{name: "25", fps: 25, want: 50},
		{name: "29.97", fps: 30000.0 / 1001.0, want: 60},
		{name: "30", fps: 30, want: 60},
		{name: "60", fps: 60, want: 120},
		{name: "unknown", fps: 0, want: 60},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := appendSegmentBoundaryArgs(nil, TranscodeOpts{
				SegmentDuration: 2, SourceFrameRate: tt.fps, HWAccel: transcodeHWQSV,
			})
			want := strconv.Itoa(tt.want)
			if got := argValueAfter(t, args, "-g"); got != want {
				t.Fatalf("-g = %q, want %q", got, want)
			}
			if got := argValueAfter(t, args, "-keyint_min"); got != want {
				t.Fatalf("-keyint_min = %q, want %q", got, want)
			}
		})
	}
}

// TestBurnInEncodeUsesQualityRateControl pins the burn-in rate-control change:
// the chosen hardware encoder uses a quantizer plus the resolution's hard peak
// cap, and never a -b:v at (or anywhere near) the source bitrate.
func TestBurnInEncodeUsesQualityRateControl(t *testing.T) {
	tests := []struct {
		name          string
		codec         string
		hwAccel       string
		wantEncoder   string
		wantQuantFlag string
		wantQuantVal  string
		wantLookAhead bool
	}{
		{name: "qsv h264", codec: "h264", hwAccel: transcodeHWQSV, wantEncoder: "h264_qsv", wantQuantFlag: "-global_quality", wantQuantVal: "23", wantLookAhead: true},
		{name: "qsv hevc", codec: "hevc", hwAccel: transcodeHWQSV, wantEncoder: "hevc_qsv", wantQuantFlag: "-global_quality", wantQuantVal: "26", wantLookAhead: true},
		{name: "qsv av1 last resort", codec: "av1", hwAccel: transcodeHWQSV, wantEncoder: "av1_qsv", wantQuantFlag: "-global_quality", wantQuantVal: "30"},
		{name: "vaapi h264", codec: "h264", hwAccel: transcodeHWVAAPI, wantEncoder: "h264_vaapi", wantQuantFlag: "-qp", wantQuantVal: "23"},
		{name: "vaapi hevc", codec: "hevc", hwAccel: transcodeHWVAAPI, wantEncoder: "hevc_vaapi", wantQuantFlag: "-qp", wantQuantVal: "26"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := buildFFmpegArgs(TranscodeOpts{
				InputPath: "/media/dv7-p7.mkv", OutputDir: t.TempDir(),
				TargetCodecVideo: tt.codec, TargetCodecAudio: "aac", SourceVideoCodec: "hevc",
				TargetResolution: "2160p", SourceHeight: 2160, SourceFrameRate: 24000.0 / 1001.0,
				// The reported failure: a source-preserving result of ~76 Mbps.
				TargetBitrateKbps: 76_000, SegmentDuration: 2, HWAccel: tt.hwAccel,
				SubtitleBurnIn: true, SubtitleTrackIndex: 0, SubtitleCodec: "hdmv_pgs_subtitle",
			})
			joined := strings.Join(args, " ")
			for _, want := range []string{
				"-c:v " + tt.wantEncoder,
				tt.wantQuantFlag + " " + tt.wantQuantVal,
				"-maxrate 40000k",
				"-bufsize 80000k",
			} {
				if !strings.Contains(joined, want) {
					t.Fatalf("%s args missing %q: %s", tt.name, want, joined)
				}
			}
			if tt.wantLookAhead && !strings.Contains(joined, "-look_ahead 0") {
				t.Fatalf("%s args missing -look_ahead 0: %s", tt.name, joined)
			}
			if strings.Contains(joined, "-b:v") {
				t.Fatalf("%s args must not pin a video bitrate: %s", tt.name, joined)
			}
			if tt.codec != transcodeCodecAV1 && strings.Contains(joined, "av1_qsv") {
				t.Fatalf("%s args used the AV1 last resort: %s", tt.name, joined)
			}
			if strings.Contains(joined, "76000k") {
				t.Fatalf("%s args inherited the source bitrate: %s", tt.name, joined)
			}
		})
	}
}

// TestSelectBurnInTargetVideoCodecPrefersRealtimeCodecs verifies the burn-in
// preference order is H.264 -> HEVC -> AV1, still filtered by the delivery's
// declared codecs, while ordinary transcodes keep the AV1-first order.
func TestSelectBurnInTargetVideoCodecPrefersRealtimeCodecs(t *testing.T) {
	registry := targetCodecRegistryV3(
		TransformationVideoToH264V3, TransformationVideoToHEVCV3, TransformationVideoToAV1V3,
	)
	input := targetCodecInputV3([]string{"av1", "hevc", "h264"}, transcodeHWQSV, nil)

	ordinary, ok := selectTargetVideoCodecV3(input, registry)
	if !ok || ordinary.codec != TargetVideoCodecAV1V3 {
		t.Fatalf("ordinary selection = %+v ok=%v, want av1", ordinary, ok)
	}
	burnIn, ok := selectBurnInTargetVideoCodecV3(input, registry)
	if !ok || burnIn.codec != TargetVideoCodecH264V3 {
		t.Fatalf("burn-in selection = %+v ok=%v, want h264", burnIn, ok)
	}

	// A delivery that only accepts HEVC still gets HEVC, never the H.264 floor.
	hevcDelivery := targetCodecInputV3([]string{"av1", "hevc", "h264"}, transcodeHWQSV, []string{"hevc"})
	if got, ok := selectBurnInTargetVideoCodecV3(hevcDelivery, registry); !ok || got.codec != TargetVideoCodecHEVCV3 {
		t.Fatalf("hevc-only burn-in selection = %+v ok=%v, want hevc", got, ok)
	}

	// AV1 remains available as the last resort when it is the only allowed codec.
	av1Delivery := targetCodecInputV3([]string{"av1"}, transcodeHWQSV, []string{"av1"})
	if got, ok := selectBurnInTargetVideoCodecV3(av1Delivery, registry); !ok || got.codec != TargetVideoCodecAV1V3 {
		t.Fatalf("av1-only burn-in selection = %+v ok=%v, want av1", got, ok)
	}
}

// TestToneMapBitmapGraphDropsRedundantScaleAtSourceHeight verifies the QSV
// tone-map + bitmap burn-in graph maps straight to QSV without a full-frame
// scale when the target already matches the source, while a real reduction
// keeps its scale and the subtitle overlay still runs after the tone-map.
func TestToneMapBitmapGraphDropsRedundantScaleAtSourceHeight(t *testing.T) {
	base := TranscodeOpts{
		InputPath: "/media/dv7-p7.mkv", OutputDir: t.TempDir(),
		SourceVideoCodec: "hevc", SourceVideoProfile: "Main 10", SourceVideoBitDepth: 10,
		TargetCodecVideo: "h264", TargetCodecAudio: "aac",
		TargetResolution: "2160p", SourceHeight: 2160, SourceFrameRate: 24000.0 / 1001.0,
		HWAccel: transcodeHWQSV, ToneMapPolicy: tonemap.PolicyHardwareOnly,
		ToneMapMode: tonemap.ModeHardware, ToneMapSourceKind: tonemap.SourcePQ,
		ToneMapFilter:        tonemap.HardwareFilterOpenCL,
		ToneMapRecipeVersion: TransformationHDRToSDRToneMapRecipeVersionV3,
		SubtitleBurnIn:       true, SubtitleTrackIndex: 0, SubtitleCodec: "hdmv_pgs_subtitle",
	}

	graph := filterComplexArg(t, buildFFmpegArgs(base))
	if strings.Contains(graph, "scale_vaapi=w=-2:h=2160") || strings.Contains(graph, "scale_vaapi=w=-2:h=min(2160") {
		t.Fatalf("same-height tone-map bitmap graph still scales a full frame: %s", graph)
	}
	if !strings.Contains(graph, "hwmap=derive_device=qsv:mode=read+write,format=qsv") {
		t.Fatalf("same-height tone-map bitmap graph is missing the map-only QSV tail: %s", graph)
	}
	toneMapIndex := strings.Index(graph, "tonemap_opencl")
	overlayIndex := strings.Index(graph, "overlay_opencl=eof_action=pass")
	if toneMapIndex < 0 || overlayIndex < toneMapIndex {
		t.Fatalf("subtitle overlay must follow the tone-map: %s", graph)
	}

	reduced := base
	reduced.TargetResolution = "1080p"
	reducedGraph := filterComplexArg(t, buildFFmpegArgs(reduced))
	if !strings.Contains(reducedGraph, "scale_opencl=w=-2:h=1080") {
		t.Fatalf("1080p tone-map bitmap graph lost its real scale: %s", reducedGraph)
	}
}

// TestGenerateFullManifestAdvertisesTargetDurationPlusOne pins the manifest
// cushion: EXTINF keeps the nominal segment duration, segment indexing is
// unchanged, and TARGETDURATION is widened so a frame-accurate GOP that
// slightly overshoots the nominal duration is still legal.
func TestGenerateFullManifestAdvertisesTargetDurationPlusOne(t *testing.T) {
	session := &TranscodeSession{opts: TranscodeOpts{
		TargetCodecVideo: "h264", SegmentDuration: 2, TotalDuration: 5,
	}}
	manifest := string(session.GenerateFullManifest("segment/", ""))
	if !strings.Contains(manifest, "#EXT-X-TARGETDURATION:3\n") {
		t.Fatalf("manifest missing widened target duration:\n%s", manifest)
	}
	if strings.Contains(manifest, "#EXT-X-TARGETDURATION:2\n") {
		t.Fatalf("manifest kept the old target duration:\n%s", manifest)
	}
	if !strings.Contains(manifest, "#EXTINF:2.000000,") {
		t.Fatalf("manifest changed EXTINF:\n%s", manifest)
	}
	if !strings.Contains(manifest, "segment/seg_00000.ts") || !strings.Contains(manifest, "segment/seg_00002.ts") {
		t.Fatalf("manifest changed integer segment indexing:\n%s", manifest)
	}
}

// TestRecipeCardRoundTripsSourceFrameRateAndHeight verifies the new GOP facts
// survive the recipe card, the stream-token claims, and a legacy card decodes
// to the historical 30 fps GOP.
func TestRecipeCardRoundTripsSourceFrameRateAndHeight(t *testing.T) {
	fps := 24000.0 / 1001.0
	opts := TranscodeOpts{
		InputPath: "/media/movie.mkv", SessionID: "session-1",
		TargetCodecVideo: "h264", TargetCodecAudio: "aac",
		SegmentDuration: 2, SourceFrameRate: fps, SourceHeight: 2160,
	}
	card := NewRecipeCard(1, "profile-1", 42, "", opts)
	if card.SourceFrameRate != fps || card.SourceHeight != 2160 {
		t.Fatalf("card = %#v, want fps %v height 2160", card, fps)
	}

	restored := card.TranscodeOpts(t.TempDir(), "", nil)
	if restored.SourceFrameRate != fps || restored.SourceHeight != 2160 {
		t.Fatalf("restored opts = %#v, want fps %v height 2160", restored, fps)
	}

	claims := card.ToClaims()
	if claims.SourceFrameRate != fps || claims.SourceHeight != 2160 {
		t.Fatalf("claims = %#v, want fps %v height 2160", claims, fps)
	}
	roundTripped := RecipeCardFromClaims(&claims)
	if roundTripped.SourceFrameRate != fps || roundTripped.SourceHeight != 2160 {
		t.Fatalf("claims round trip = %#v, want fps %v height 2160", roundTripped, fps)
	}
}

func TestLegacyRecipeCardWithoutSourceFrameRateKeepsHistoricalGOP(t *testing.T) {
	card := RecipeCard{
		SessionID: "legacy", TargetCodecVideo: "h264", TargetCodecAudio: "aac",
		TargetResolution: "1080p", SegmentDuration: 2, HWAccel: transcodeHWQSV,
	}
	opts := card.TranscodeOpts(t.TempDir(), "", nil)
	if opts.SourceFrameRate != 0 || opts.SourceHeight != 0 {
		t.Fatalf("legacy card invented source facts: %#v", opts)
	}
	args := buildFFmpegArgs(opts)
	if got := argValueAfter(t, args, "-g"); got != "60" {
		t.Fatalf("legacy GOP = %q, want the 30 fps fallback 60", got)
	}
}

// TestPlanPlaybackForcedBurnInReplacesSourceBitrate is the planner-level
// regression: a source-preserving plan forced into an encode by bitmap
// burn-in must target the source-class ladder rung, warn, and pick the
// realtime-safe H.264 encoder.
func TestPlanPlaybackForcedBurnInReplacesSourceBitrate(t *testing.T) {
	file := detailedFixtureFileV3()
	file.VideoTracks[0].VideoRange = "SDR"
	file.VideoTracks[0].VideoRangeType = "SDR"
	file.VideoTracks[0].ColorTransfer = "bt709"
	req := pgsBurnRequestV3(file)

	result := PlanPlaybackV3(PlannerInputV3{
		Request: req, RequestedFile: file, EffectiveFile: file, AudioTrackIndex: 0,
		Settings: PlannerSettingsV3{TranscodeEnabled: true, Allow4KTranscode: true},
		Registry: testTransformationRegistryV3(),
	})
	if result.Plan == nil || result.Plan.Delivery != DeliveryTranscodeHLSV3 {
		t.Fatalf("result = %s", ExplainPlannerResultV3(result))
	}
	if result.TargetBitrateKbps != 40_000 {
		t.Fatalf("TargetBitrateKbps = %d, want the 4K source rung 40000", result.TargetBitrateKbps)
	}
	if result.TargetVideoCodec != TargetVideoCodecH264V3 {
		t.Fatalf("TargetVideoCodec = %q, want h264 for burn-in", result.TargetVideoCodec)
	}
	if result.SourceHeight != 2160 || result.SourceFrameRate <= 0 {
		t.Fatalf("source facts = height %d fps %v, want 2160 and a real rate", result.SourceHeight, result.SourceFrameRate)
	}
	if !hasDegradationWarningV3(result.Plan.DegradationWarnings, "quality_source_requires_transcode") {
		t.Fatalf("warnings = %#v, want quality_source_requires_transcode", result.Plan.DegradationWarnings)
	}
	if !hasDegradationWarningV3(result.Plan.DegradationWarnings, "subtitle_burn_in") {
		t.Fatalf("warnings = %#v, want subtitle_burn_in", result.Plan.DegradationWarnings)
	}
}
