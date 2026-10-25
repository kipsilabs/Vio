package playback

import (
	"fmt"
	"testing"
)

func TestParseWxHResolution(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		wantW  int
		wantH  int
		wantOK bool
	}{
		{name: "1080p WxH", input: "1920x1080", wantW: 1920, wantH: 1080, wantOK: true},
		{name: "4K WxH", input: "3840x2160", wantW: 3840, wantH: 2160, wantOK: true},
		{name: "720p WxH", input: "1280x720", wantW: 1280, wantH: 720, wantOK: true},
		{name: "uppercase X", input: "1920X1080", wantW: 1920, wantH: 1080, wantOK: true},
		{name: "surrounding whitespace", input: "  1920x1080  ", wantW: 1920, wantH: 1080, wantOK: true},
		{name: "trailing junk rejected", input: "1920x1080garbage"},
		{name: "decimal rejected", input: "1920x1080.5"},
		{name: "zero rejected", input: "0x0"},
		{name: "negative rejected", input: "-1x-1"},
		{name: "overflow rejected", input: "99999x99999"},
		{name: "empty", input: ""},
		{name: "standard label rejected", input: "1080p"},
		{name: "single number rejected", input: "1920"},
		{name: "missing width", input: "x1080"},
		{name: "missing height", input: "1920x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, h, ok := parseWxHResolution(tt.input)
			if w != tt.wantW || h != tt.wantH || ok != tt.wantOK {
				t.Fatalf("parseWxHResolution(%q) = (%d, %d, %v), want (%d, %d, %v)",
					tt.input, w, h, ok, tt.wantW, tt.wantH, tt.wantOK)
			}
		})
	}

	dimensions := []struct {
		name  string
		input string
		wantW int
		wantH int
	}{
		{name: "WxH input", input: "1920x1080", wantW: 1920, wantH: 1080},
		{name: "label still works", input: "1080p", wantW: 1920, wantH: 1080},
	}
	for _, tt := range dimensions {
		t.Run("dimensions/"+tt.name, func(t *testing.T) {
			w, h := dimensionsFromResolutionV3(tt.input)
			if w != tt.wantW || h != tt.wantH {
				t.Fatalf("dimensionsFromResolutionV3(%q) = (%d, %d), want (%d, %d)",
					tt.input, w, h, tt.wantW, tt.wantH)
			}
		})
	}
}

func TestCrossCompatibleDVBaseLayerV3(t *testing.T) {
	tests := []struct {
		compatID int
		want     bool
	}{
		{compatID: 0, want: false},
		{compatID: 1, want: true},
		{compatID: 2, want: true},
		{compatID: 4, want: true},
		{compatID: 5, want: false},
		{compatID: 6, want: true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("compat_%d", tt.compatID), func(t *testing.T) {
			got := crossCompatibleDVBaseLayerV3(tt.compatID)
			if got != tt.want {
				t.Fatalf("crossCompatibleDVBaseLayerV3(%d) = %v, want %v", tt.compatID, got, tt.want)
			}
		})
	}
}

func TestClientManagesOriginalDynamicRangeV3DVCompatIDs(t *testing.T) {
	req := validStartRequestV3()
	direct := req.ClientPlaybackContext.Deliveries[DeliveryClassOriginalHTTPV3]
	direct.ValidatedClaims = []string{ClaimClientManagedDynamicRangeV3}
	req.ClientPlaybackContext.Deliveries[DeliveryClassOriginalHTTPV3] = direct

	tests := []struct {
		compatID int
		want     bool
	}{
		{compatID: 0, want: false},
		{compatID: 1, want: true},
		{compatID: 2, want: true},
		{compatID: 4, want: true},
		{compatID: 5, want: false},
		{compatID: 6, want: true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("compat_%d", tt.compatID), func(t *testing.T) {
			source := SourceDescriptorV3{
				DynamicRange: DynamicRangeDolbyVisionV3,
				DVProfile:    8,
				DVBLCompatID: tt.compatID,
			}
			got := clientManagesOriginalDynamicRangeV3(source, req)
			if got != tt.want {
				t.Fatalf("clientManagesOriginalDynamicRangeV3(DV, compat=%d) = %v, want %v", tt.compatID, got, tt.want)
			}
		})
	}
}

func TestClientManagesOriginalDynamicRangeV3SourceRanges(t *testing.T) {
	req := validStartRequestV3()
	direct := req.ClientPlaybackContext.Deliveries[DeliveryClassOriginalHTTPV3]
	direct.ValidatedClaims = []string{ClaimClientManagedDynamicRangeV3}
	req.ClientPlaybackContext.Deliveries[DeliveryClassOriginalHTTPV3] = direct

	// HDR10 source with the claim -> still accepted
	hdr10Source := SourceDescriptorV3{
		DynamicRange: DynamicRangeHDR10V3,
	}
	if !clientManagesOriginalDynamicRangeV3(hdr10Source, req) {
		t.Fatalf("expected HDR10 source with claim to be accepted")
	}

	// SDR source with the claim -> refused
	sdrSource := SourceDescriptorV3{
		DynamicRange: DynamicRangeSDRV3,
	}
	if clientManagesOriginalDynamicRangeV3(sdrSource, req) {
		t.Fatalf("expected SDR source with claim to be refused")
	}

	// Empty dynamic range -> refused
	emptySource := SourceDescriptorV3{
		DynamicRange: "",
	}
	if clientManagesOriginalDynamicRangeV3(emptySource, req) {
		t.Fatalf("expected empty dynamic range to be refused")
	}
}

func TestPlanPlaybackV3ClientManagedDynamicRangeDVCompatGate(t *testing.T) {
	req := validStartRequestV3()
	req.Capabilities.VideoDecode = []VideoDecodeCapabilityV3{{
		Codec: "hevc", Profiles: []string{"main 10"}, Levels: []int{153}, BitDepths: []int{10},
		MaxWidth: 3840, MaxHeight: 2160, MaxFrameRate: 60, MaxBitrateKbps: 80_000, Hardware: true,
	}}
	// Non-DV output (SDR display)
	req.Capabilities.HDRDetails = &HDRCapabilitiesV3{}
	req.ClientPlaybackContext.Output.HDRDetails = &HDRCapabilitiesV3{}
	req.ClientPlaybackContext.Output.Display = &OutputDisplayV3{HDREvidence: OutputHDREvidenceExactV3, HDRTypes: &HDRCapabilitiesV3{}}
	direct := req.ClientPlaybackContext.Deliveries[DeliveryClassOriginalHTTPV3]
	direct.Containers = []string{"mkv"}
	direct.VideoCodecs = []string{"hevc"}
	direct.AudioDecodeCodecs = []string{"aac"}
	direct.HDRDetails = &HDRCapabilitiesV3{}
	direct.ValidatedClaims = []string{ClaimClientManagedDynamicRangeV3}
	req.ClientPlaybackContext.Deliveries[DeliveryClassOriginalHTTPV3] = direct

	// DV profile 8 with compat 0: claim present, does not produce client_managed_dynamic_range, does not panic
	fileCompat0 := dv8BaseLayerFixtureV3(0)
	resultCompat0 := PlanPlaybackV3(PlannerInputV3{
		Request:         req,
		RequestedFile:   fileCompat0,
		EffectiveFile:   fileCompat0,
		AudioTrackIndex: 0,
		Settings:        PlannerSettingsV3{TranscodeEnabled: true, Allow4KTranscode: true},
		Registry:        testTransformationRegistryV3(),
	})
	if resultCompat0.Plan != nil && resultCompat0.Plan.DecisionReason == decisionReasonClientManagedDynamicRangeV3 {
		t.Fatalf("DV Profile 8 compat 0 must not produce %q decision reason", decisionReasonClientManagedDynamicRangeV3)
	}

	// DV profile 8 with compat 1: claim present, does produce client_managed_dynamic_range
	fileCompat1 := dv8BaseLayerFixtureV3(1)
	resultCompat1 := PlanPlaybackV3(PlannerInputV3{
		Request:         req,
		RequestedFile:   fileCompat1,
		EffectiveFile:   fileCompat1,
		AudioTrackIndex: 0,
		Settings:        PlannerSettingsV3{TranscodeEnabled: true, Allow4KTranscode: true},
		Registry:        testTransformationRegistryV3(),
	})
	if resultCompat1.Plan == nil || resultCompat1.Plan.Delivery != DeliveryOriginalHTTPV3 || resultCompat1.PlayMethod != PlayDirect {
		t.Fatalf("expected original_http direct play for compat 1: %s", ExplainPlannerResultV3(resultCompat1))
	}
	if resultCompat1.Plan.DecisionReason != decisionReasonClientManagedDynamicRangeV3 {
		t.Fatalf("decision reason = %q, want %q", resultCompat1.Plan.DecisionReason, decisionReasonClientManagedDynamicRangeV3)
	}
}
