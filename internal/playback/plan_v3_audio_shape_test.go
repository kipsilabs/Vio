package playback

import (
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

// audioShapeFixtureFileV3 is the field source's audio shape: 2160p HEVC with a
// DTS-HD MA 5.1 track, which the Android TV client over-claims and then cannot
// initialize its audio output for.
func audioShapeFixtureFileV3() *models.MediaFile {
	file := detailedFixtureFileV3()
	file.CodecAudio = "dts"
	file.AudioChannels = 6
	file.AudioTracks = []models.AudioTrack{{Codec: "dts", Channels: 6, Layout: "5.1", Default: true}}
	return file
}

// audioShapeClientRequestV3 is the over-claiming TV client: it reports DTS
// decode and an HLS channel ceiling of 8, so every recipe the planner builds
// keeps 5.1.
func audioShapeClientRequestV3() StartRequestV3 {
	req := validStartRequestV3()
	req.Capabilities.CodecsAudio = []string{"aac", "dts"}
	req.Capabilities.VideoDecode = hevcMain10DecodeV3()
	req.Capabilities.HDRDetails = &HDRCapabilitiesV3{HDR10: true}
	req.ClientPlaybackContext.Output.HDRDetails = req.Capabilities.HDRDetails
	// The failing device had progressive disabled for the whole session.
	delete(req.ClientPlaybackContext.Deliveries, DeliveryClassProgressiveV3)
	hls := req.ClientPlaybackContext.Deliveries[DeliveryClassHLSV3]
	hls.MaxChannels = intPointerV3(8)
	req.ClientPlaybackContext.Deliveries[DeliveryClassHLSV3] = hls
	return req
}

func audioShapePlannerInputV3() PlannerInputV3 {
	file := audioShapeFixtureFileV3()
	return PlannerInputV3{
		Request: audioShapeClientRequestV3(), RequestedFile: file, EffectiveFile: file, AudioTrackIndex: 0,
		Settings: PlannerSettingsV3{TranscodeEnabled: true, Allow4KTranscode: true},
		Registry: testTransformationRegistryV3(),
	}
}

// The surround recipe the device actually failed on. This is the plan the
// handler's loop guard excludes when it asks for the audio-shape retry.
func TestPlanPlaybackV3AudioShapeRetryDownmixesToStereoOnTheSameDelivery(t *testing.T) {
	input := audioShapePlannerInputV3()
	first := PlanPlaybackV3(input)
	if first.Plan == nil || first.Plan.Delivery != DeliveryOriginalHTTPV3 {
		t.Fatalf("first plan = %s, want direct play", ExplainPlannerResultV3(first))
	}
	attempted := []string{PlanAttemptKeyV3(*first.Plan, input.Request.ClientPlaybackContext.Output.OutputContextID, nil)}

	// Without the retry flag the same history still lands on the surround
	// HLS remux: this is the recipe that failed in the field.
	input.AttemptedKeys = attempted
	surround := PlanPlaybackV3(input)
	if surround.Plan == nil || surround.Plan.Delivery != DeliveryRemuxHLSV3 {
		t.Fatalf("surround plan = %s, want the HLS remux", ExplainPlannerResultV3(surround))
	}
	if surround.Plan.EffectiveRecipe.AudioChannels == nil || *surround.Plan.EffectiveRecipe.AudioChannels != 6 {
		t.Fatalf("surround plan audio = %s %v, want 5.1", surround.Plan.EffectiveRecipe.AudioCodec, surround.Plan.EffectiveRecipe.AudioChannels)
	}

	input.AudioShapeRetry = true
	input.AttemptedKeys = append(attempted, PlanAttemptKeyV3(*surround.Plan, input.Request.ClientPlaybackContext.Output.OutputContextID, nil))
	recovered := PlanPlaybackV3(input)
	if recovered.Plan == nil {
		t.Fatalf("audio-shape retry = %s", ExplainPlannerResultV3(recovered))
	}
	if recovered.Plan.Delivery != DeliveryRemuxHLSV3 {
		t.Fatalf("audio-shape retry delivery = %q, want the same HLS remux class", recovered.Plan.Delivery)
	}
	if recovered.Plan.EffectiveRecipe.AudioCodec != audioCodecAACV3 {
		t.Fatalf("audio-shape retry codec = %q, want AAC", recovered.Plan.EffectiveRecipe.AudioCodec)
	}
	if recovered.Plan.EffectiveRecipe.AudioChannels == nil || *recovered.Plan.EffectiveRecipe.AudioChannels != 2 {
		t.Fatalf("audio-shape retry channels = %v, want stereo", recovered.Plan.EffectiveRecipe.AudioChannels)
	}
	if recovered.Plan.EffectiveRecipe.AudioLayout != audioLayoutStereoV3 {
		t.Fatalf("audio-shape retry layout = %q, want stereo", recovered.Plan.EffectiveRecipe.AudioLayout)
	}
	// The stereo recovery must be its own plan, or the loop guard would keep
	// handing back the surround recipe that failed.
	if got := PlanAttemptKeyV3(*recovered.Plan, input.Request.ClientPlaybackContext.Output.OutputContextID, nil); got == PlanAttemptKeyV3(*surround.Plan, input.Request.ClientPlaybackContext.Output.OutputContextID, nil) {
		t.Fatal("stereo retry shares the failed surround plan's attempt key")
	}
}

// A source that already delivers stereo has nothing smaller to try: the flag
// must not force an HLS conversion on a web-shaped client.
func TestPlanPlaybackV3AudioShapeRetryLeavesStereoSourcesAlone(t *testing.T) {
	input := audioShapePlannerInputV3()
	file := input.EffectiveFile
	file.CodecAudio, file.AudioChannels = "aac", 2
	file.AudioTracks = []models.AudioTrack{{Codec: "aac", Channels: 2, Layout: "stereo", Default: true}}
	// A web-shaped client decodes AAC and keeps its own downmix; nothing here
	// declares a surround limit.
	input.Request.Capabilities.CodecsAudio = []string{"aac"}
	input.Request.ClientPlaybackContext.Deliveries[DeliveryClassHLSV3] = DeliveryCapabilityV3{Enabled: true, SupportedOnDevice: true}
	input.AudioShapeRetry = true

	result := PlanPlaybackV3(input)
	if result.Plan == nil || result.Plan.Delivery != DeliveryOriginalHTTPV3 {
		t.Fatalf("plan = %s, want the unchanged direct-play route", ExplainPlannerResultV3(result))
	}
	for _, transformation := range result.Plan.Transformations {
		if transformation.Name == TransformationAudioToAACV3 {
			t.Fatalf("direct plan gained an audio conversion: %+v", transformation)
		}
	}
	if result.Plan.EffectiveRecipe.AudioCodec != "aac" ||
		result.Plan.EffectiveRecipe.AudioChannels == nil || *result.Plan.EffectiveRecipe.AudioChannels != 2 {
		t.Fatalf("direct plan audio = %s %v, want the source's own stereo AAC", result.Plan.EffectiveRecipe.AudioCodec, result.Plan.EffectiveRecipe.AudioChannels)
	}
}

// Every route is gone and the last failure was the audio output failing to
// initialize: name that cause, keep it retryable, and carry the player's own
// code in the detail.
func TestPlanPlaybackV3ReportsAudioOutputUnsupportedInsteadOfAGenericTerminal(t *testing.T) {
	input := audioShapePlannerInputV3()
	first := PlanPlaybackV3(input)
	if first.Plan == nil || first.Plan.Delivery != DeliveryOriginalHTTPV3 {
		t.Fatalf("first plan = %s, want direct play", ExplainPlannerResultV3(first))
	}
	input.AttemptedKeys = []string{PlanAttemptKeyV3(*first.Plan, input.Request.ClientPlaybackContext.Output.OutputContextID, nil)}
	// Nothing is left to adapt: only direct play was advertised.
	input.Request.ClientPlaybackContext.Deliveries = map[string]DeliveryCapabilityV3{
		DeliveryClassOriginalHTTPV3: input.Request.ClientPlaybackContext.Deliveries[DeliveryClassOriginalHTTPV3],
	}

	generic := PlanPlaybackV3(input)
	if generic.Terminal == nil || generic.Terminal.Reason != "adaptation_unavailable" || generic.Terminal.Retryable {
		t.Fatalf("terminal without audio-init evidence = %s", ExplainPlannerResultV3(generic))
	}

	input.AudioInitFailureDetail = "media3 code 5001 ERROR_CODE_AUDIO_TRACK_INIT_FAILED"
	specific := PlanPlaybackV3(input)
	if specific.Terminal == nil || specific.Terminal.Reason != TerminalAudioOutputUnsupportedV3 {
		t.Fatalf("terminal = %s, want %q", ExplainPlannerResultV3(specific), TerminalAudioOutputUnsupportedV3)
	}
	if !specific.Terminal.Retryable {
		t.Fatal("audio_output_unsupported must be retryable: another audio track can play")
	}
	if !strings.Contains(specific.Terminal.Detail, "5001") || !strings.Contains(specific.Terminal.Detail, "ERROR_CODE_AUDIO_TRACK_INIT_FAILED") {
		t.Fatalf("terminal detail = %q, want the failing player code", specific.Terminal.Detail)
	}
	if specific.Terminal.Message == generic.Terminal.Message {
		t.Fatal("audio_output_unsupported shares the generic message; the user-facing wording must name the audio output")
	}
}

func TestAudioInitFailureDetailV3ReadsDiagnosticsAndMessages(t *testing.T) {
	for _, test := range []struct {
		name    string
		failure FailureV3
		want    string
	}{
		{
			name:    "structured code and name",
			failure: FailureV3{Classification: "player_failure", Diagnostics: map[string]string{"error_code": "5001", "error_code_name": "ERROR_CODE_AUDIO_TRACK_INIT_FAILED"}},
			want:    "media3 code 5001 ERROR_CODE_AUDIO_TRACK_INIT_FAILED",
		},
		{
			name:    "structured name only",
			failure: FailureV3{Classification: "player_failure", Diagnostics: map[string]string{"error_code_name": "ERROR_CODE_AUDIO_TRACK_INIT_FAILED"}},
			want:    "ERROR_CODE_AUDIO_TRACK_INIT_FAILED",
		},
		{
			name:    "player message",
			failure: FailureV3{Classification: "player_failure", Message: "AudioTrack init failed: 0, Config(48000, 6, ...)"},
			want:    "AudioTrack init failed",
		},
		{
			name:    "audio renderer classification",
			failure: FailureV3{Classification: FailureClassificationAudioRendererErrorV3},
			want:    FailureClassificationAudioRendererErrorV3,
		},
		{
			name:    "video decoder failure",
			failure: FailureV3{Classification: "player_failure", Message: "Decoder init failed for OMX.MTK.VIDEO.DECODER.DVHE.STH"},
			want:    "",
		},
		{
			name:    "unrelated transport failure",
			failure: FailureV3{Classification: "http_failure", Message: "HTTP 500"},
			want:    "",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := AudioInitFailureDetailV3(test.failure)
			if test.want == "" {
				if got != "" {
					t.Fatalf("detail = %q, want no audio-init evidence", got)
				}
				return
			}
			if !strings.Contains(got, test.want) {
				t.Fatalf("detail = %q, want it to contain %q", got, test.want)
			}
		})
	}
}
