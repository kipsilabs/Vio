package playback

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

// stickyDeliveryFixtureV3 is an HEVC 2160p HDR10 source a client can take as
// either a copy remux or an HLS transcode: every delivery it advertises is
// viable at original quality, which is exactly the route space a replan
// re-derives and can thrash within.
func stickyDeliveryFixtureV3() (*models.MediaFile, StartRequestV3) {
	file := detailedFixtureFileV3()
	file.VideoTracks[0].Codec = "hevc"
	file.VideoTracks[0].Profile = "main 10"
	file.VideoTracks[0].BitDepth = 10
	file.VideoTracks[0].VideoRange = "HDR10"
	file.VideoTracks[0].VideoRangeType = "HDR10"
	file.VideoTracks[0].ColorTransfer = "smpte2084"
	file.VideoTracks[0].ColorPrimaries = "bt2020"
	req := validStartRequestV3()
	req.Capabilities.CodecsVideo = []string{"hevc", "h264"}
	req.Capabilities.CodecsVideoHardware = []string{"hevc", "h264"}
	req.Capabilities.Containers = []string{"mkv", "mp4", "hls"}
	req.Capabilities.MaxResolution = "2160p"
	req.Capabilities.HDR = true
	req.Capabilities.HDRDetails = &HDRCapabilitiesV3{HDR10: true}
	req.Capabilities.VideoDecode = []VideoDecodeCapabilityV3{{
		Codec: "hevc", Profiles: []string{"main 10"}, Levels: []int{153}, BitDepths: []int{10},
		MaxWidth: 3840, MaxHeight: 2160, MaxFrameRate: 60, MaxBitrateKbps: 80_000, Hardware: true,
	}}
	req.ClientPlaybackContext.Output.HDRDetails = req.Capabilities.HDRDetails
	return file, req
}

func stickyPlannerInputV3(file *models.MediaFile, req StartRequestV3) PlannerInputV3 {
	return PlannerInputV3{
		Request: req, RequestedFile: file, EffectiveFile: file, AudioTrackIndex: 0,
		Settings: PlannerSettingsV3{TranscodeEnabled: true, Allow4KTranscode: true},
		Registry: testTransformationRegistryV3(),
	}
}

// The delivery-stickiness backstop: when the natural replan would move off the
// running delivery's class and the running class can still serve the request,
// the planner keeps it instead. This is what stops the production pipeline
// ping-pong (transcode-HLS -> remux -> transcode-HLS across two track changes)
// when nothing about the source or intent actually changed.
func TestPlanPlaybackV3StickyDeliveryKeepsRunningClass(t *testing.T) {
	file, req := stickyDeliveryFixtureV3()
	input := stickyPlannerInputV3(file, req)

	natural := PlanPlaybackV3(input)
	if natural.Plan == nil {
		t.Fatalf("natural plan terminal: %#v", natural.Terminal)
	}
	naturalClass := DeliveryClassV3(natural.Plan.Delivery)

	// Ask for a class other than the natural one that can still serve.
	stickyClass := DeliveryClassHLSV3
	sticky := DeliveryRemuxHLSV3
	if naturalClass == stickyClass {
		stickyClass = DeliveryClassProgressiveV3
		sticky = DeliveryRemuxProgressiveV3
	}
	input.StickyDelivery = sticky
	stuck := PlanPlaybackV3(input)
	if stuck.Plan == nil {
		t.Fatalf("sticky plan terminal: %#v", stuck.Terminal)
	}
	if got := DeliveryClassV3(stuck.Plan.Delivery); got != stickyClass {
		t.Fatalf("sticky plan delivery class = %q, want the running class %q (delivery %q)", got, stickyClass, stuck.Plan.Delivery)
	}
	if !stuck.StickyDeliveryKept {
		t.Fatal("sticky retry did not record that it kept the running delivery")
	}
}

// Consecutive track-change replans with no forcing reason keep the running
// delivery class: the previous recipe is excluded (as attempt history does),
// the natural plan would move off the class, and the sticky retry keeps it.
func TestPlanPlaybackV3ConsecutiveTrackChangesKeepRunningClass(t *testing.T) {
	file, req := stickyDeliveryFixtureV3()
	input := stickyPlannerInputV3(file, req)

	first := PlanPlaybackV3(input)
	if first.Plan == nil {
		t.Fatalf("first plan terminal: %#v", first.Terminal)
	}
	// Excluding the first recipe's key pushes the natural planner off it.
	input.AttemptedKeys = []string{PlanAttemptKeyV3(*first.Plan, req.ClientPlaybackContext.Output.OutputContextID, nil)}
	natural := PlanPlaybackV3(input)
	if natural.Plan == nil {
		t.Fatalf("natural second plan terminal: %#v", natural.Terminal)
	}
	runningClass := DeliveryClassHLSV3
	if DeliveryClassV3(natural.Plan.Delivery) == runningClass {
		runningClass = DeliveryClassProgressiveV3
	}

	input.StickyDelivery = deliveryForClassV3(runningClass)
	stuck := PlanPlaybackV3(input)
	if stuck.Plan == nil {
		t.Fatalf("sticky second plan terminal: %#v", stuck.Terminal)
	}
	if got := DeliveryClassV3(stuck.Plan.Delivery); got != runningClass {
		t.Fatalf("consecutive replan class = %q, want the running class %q", got, runningClass)
	}
	if !stuck.StickyDeliveryKept {
		t.Fatal("consecutive track change did not record that it kept the running delivery")
	}
}

// deliveryForClassV3 picks a concrete delivery of the requested class.
func deliveryForClassV3(class string) DeliveryV3 {
	switch class {
	case DeliveryClassProgressiveV3:
		return DeliveryRemuxProgressiveV3
	case DeliveryClassOriginalHTTPV3:
		return DeliveryOriginalHTTPV3
	default:
		return DeliveryRemuxHLSV3
	}
}

// A class that genuinely cannot serve the new request is not kept: the
// constrained retry terminals and the natural plan wins.
func TestPlanPlaybackV3StickyDeliveryFallsBackWhenClassCannotServe(t *testing.T) {
	file, req := stickyDeliveryFixtureV3()
	// An HLS-only client cannot take a progressive remux, so a progressive pin
	// can never be honored.
	delete(req.ClientPlaybackContext.Deliveries, DeliveryClassProgressiveV3)
	input := stickyPlannerInputV3(file, req)
	input.StickyDelivery = DeliveryRemuxProgressiveV3

	result := PlanPlaybackV3(input)
	if result.Plan == nil {
		t.Fatalf("sticky fallback terminal: %#v", result.Terminal)
	}
	if result.Plan.Delivery == DeliveryRemuxProgressiveV3 {
		t.Fatalf("an unavailable sticky delivery was served: %s", result.Plan.Delivery)
	}
	if result.StickyDeliveryKept {
		t.Fatal("fallback to the natural plan reported a kept sticky delivery")
	}
}
