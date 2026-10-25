package playback

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

func managedDynamicRangeRequestV3(claims ...string) StartRequestV3 {
	req := validStartRequestV3()
	req.QualityPreference = QualityOriginalV3
	req.Capabilities.VideoEvidence = EvidenceDeclaredV3
	req.Capabilities.AudioEvidence = EvidenceDeclaredV3
	req.Capabilities.CodecsVideo = []string{"hevc"}
	req.Capabilities.CodecsAudio = []string{"aac"}
	req.Capabilities.Containers = []string{"mkv"}
	req.Capabilities.HDR = false
	req.Capabilities.HDRDetails = &HDRCapabilitiesV3{}
	req.ClientPlaybackContext.Output.HDRDetails = &HDRCapabilitiesV3{}
	direct := req.ClientPlaybackContext.Deliveries[DeliveryClassOriginalHTTPV3]
	direct.Containers = []string{"mkv"}
	direct.VideoCodecs = []string{"hevc"}
	direct.AudioDecodeCodecs = []string{"aac"}
	direct.HDRDetails = &HDRCapabilitiesV3{}
	direct.ValidatedClaims = append([]string(nil), claims...)
	req.ClientPlaybackContext.Deliveries[DeliveryClassOriginalHTTPV3] = direct
	return req
}

func managedDynamicRangeFileV3(profile, compatID int, proven bool) *models.MediaFile {
	file := detailedFixtureFileV3()
	file.Container = "mkv"
	file.VideoTracks[0].Profile = "Main 10"
	file.VideoTracks[0].VideoRange = "HDR"
	file.VideoTracks[0].VideoRangeType = "DOVI"
	file.VideoTracks[0].DVProfile = profile
	file.VideoTracks[0].DVBLCompatID = compatID
	if proven {
		file.VideoTracks[0].DVConfigPresent = true
		file.VideoTracks[0].DVBLCompatIDPresent = true
		file.VideoTracks[0].DVBLPresent = true
	}
	return file
}

func planManagedDynamicRangeV3(t *testing.T, file *models.MediaFile, req StartRequestV3) PlannerResultV3 {
	t.Helper()
	return PlanPlaybackV3(PlannerInputV3{
		Request: req, RequestedFile: file, EffectiveFile: file, AudioTrackIndex: 0,
		Settings: PlannerSettingsV3{TranscodeEnabled: true, Allow4KTranscode: true},
		Registry: testTransformationRegistryV3(),
	})
}

func TestClientManagedDVBaseLayerClaimV3(t *testing.T) {
	if got := ClientManagedDVBaseLayerClaimV3(2); got != "client_managed_dv_bl_compat_2_v1" {
		t.Fatalf("ClientManagedDVBaseLayerClaimV3(2) = %q", got)
	}
	for _, absent := range []int{0, -1} {
		if got := ClientManagedDVBaseLayerClaimV3(absent); got != "" {
			t.Fatalf("ClientManagedDVBaseLayerClaimV3(%d) = %q, want empty", absent, got)
		}
	}
}

func TestDeliveryManagesOriginalDynamicRangeV3DolbyVisionNeedsMatchingClaim(t *testing.T) {
	source := SourceDescriptorFromFileV3(managedDynamicRangeFileV3(8, 2, true), 0)
	tests := []struct {
		name   string
		source SourceDescriptorV3
		claims []string
		want   bool
	}{
		{
			name:   "generic claim covers non-DV HDR",
			source: SourceDescriptorV3{DynamicRange: DynamicRangeHDR10V3},
			claims: []string{ClaimClientManagedDynamicRangeV3},
			want:   true,
		},
		{
			name:   "generic claim does not cover Dolby Vision",
			source: source,
			claims: []string{ClaimClientManagedDynamicRangeV3},
			want:   false,
		},
		{
			name:   "matching per-id claim covers a proven base layer",
			source: source,
			claims: []string{ClaimClientManagedDynamicRangeV3, ClientManagedDVBaseLayerClaimV3(2)},
			want:   true,
		},
		{
			name:   "a different id's claim does not count",
			source: source,
			claims: []string{ClientManagedDVBaseLayerClaimV3(4)},
			want:   false,
		},
		{
			name:   "an unproven base layer fails closed",
			source: SourceDescriptorFromFileV3(managedDynamicRangeFileV3(8, 2, false), 0),
			claims: []string{ClientManagedDVBaseLayerClaimV3(2)},
			want:   false,
		},
		{
			name:   "an absent id has no claim",
			source: SourceDescriptorFromFileV3(managedDynamicRangeFileV3(8, 0, true), 0),
			claims: []string{ClaimClientManagedDynamicRangeV3, ClientManagedDVBaseLayerClaimV3(1)},
			want:   false,
		},
		{
			name:   "an unknown id has no matching claim",
			source: SourceDescriptorFromFileV3(managedDynamicRangeFileV3(8, 5, true), 0),
			claims: []string{ClientManagedDVBaseLayerClaimV3(1), ClientManagedDVBaseLayerClaimV3(2)},
			want:   false,
		},
		{
			name:   "profile 5 has no compatible base layer",
			source: SourceDescriptorFromFileV3(managedDynamicRangeFileV3(5, 1, true), 0),
			claims: []string{ClientManagedDVBaseLayerClaimV3(1)},
			want:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			delivery := DeliveryCapabilityV3{ValidatedClaims: tt.claims}
			if got := deliveryManagesOriginalDynamicRangeV3(delivery, tt.source); got != tt.want {
				t.Fatalf("deliveryManagesOriginalDynamicRangeV3() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPlanPlaybackV3DoesNotInferDolbyVisionBaseLayerForAbsentCompatID(t *testing.T) {
	file := managedDynamicRangeFileV3(8, 0, false)
	req := managedDynamicRangeRequestV3(
		ClaimClientManagedDynamicRangeV3,
		ClientManagedDVBaseLayerClaimV3(1),
		ClientManagedDVBaseLayerClaimV3(6),
		ClientManagedDVBaseLayerClaimV3(4),
		ClientManagedDVBaseLayerClaimV3(2),
	)
	result := planManagedDynamicRangeV3(t, file, req)
	if result.Plan != nil && result.Plan.Delivery == DeliveryOriginalHTTPV3 {
		t.Fatalf("absent compatibility id was planned as original HTTP: %s", ExplainPlannerResultV3(result))
	}
}

func TestPlanPlaybackV3ManagedDynamicRangeUsesProvenDVCompatID(t *testing.T) {
	file := managedDynamicRangeFileV3(8, 2, true)
	req := managedDynamicRangeRequestV3(
		ClaimClientManagedDynamicRangeV3,
		ClientManagedDVBaseLayerClaimV3(2),
	)
	result := planManagedDynamicRangeV3(t, file, req)
	if result.Plan == nil || result.Plan.Delivery != DeliveryOriginalHTTPV3 || result.PlayMethod != PlayDirect {
		t.Fatalf("proven compatibility id did not reach original HTTP: %s", ExplainPlannerResultV3(result))
	}
	if result.Plan.DecisionReason != decisionReasonClientManagedDynamicRangeV3 {
		t.Fatalf("decision reason = %q, want %q", result.Plan.DecisionReason, decisionReasonClientManagedDynamicRangeV3)
	}
}
