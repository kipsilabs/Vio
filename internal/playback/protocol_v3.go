package playback

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	ProtocolV3                               = 3
	FeaturePlaybackPlanV3                    = "playback_plan_v3"
	FeatureServerRemoteStreamBitratePolicyV3 = "server_remote_stream_bitrate_policy_v1"
	FeatureServerLocalStreamBitratePolicyV3  = "server_local_stream_bitrate_policy_v1"
	FeatureEmbeddedSubtitlesV3               = "embedded_subtitles_v1"
	FeatureNeutralContractV3                 = "neutral_playback_v3_contract_v1"
	FeatureLayoutPassthrough                 = "layout_aware_passthrough"
	FeatureClientVideoTransforms             = "client_video_transformations_v1"
	FeatureRouteDiagnostics                  = "playback_route_diagnostics"
	FeatureDeviceQuirksV3                    = "device_quirks_v1"
	FeatureSeekReanchorV3                    = "seek_reanchor_v1"
	FeatureOutputChangeV3                    = "output_change_v1"
	// FeatureOutputDisplayEvidenceV3 tells a client this server understands
	// output.display and its hdr_evidence tier. A pre-feature server ignores
	// the field and falls back from a missing output.hdr_details to the
	// device-level HDR facts, so a client that sends output.display must
	// always send output.hdr_details (decoder ∩ display) as well; it may
	// rely on the evidence tier being honored only when this token is
	// advertised.
	FeatureOutputDisplayEvidenceV3 = "output_display_evidence_v1"
	FeatureDirectStreamResumeV3    = "direct_stream_resume_v1"
	FeaturePlanSourceDurationV3    = "plan_source_duration_v1"
	// FeatureSoftwareVideoDecodeV3 lets a strict evidence-tier client opt into
	// bounded hardware:false video_decode entries. Without the feature, exact
	// and platform_attested retain their historical hardware-only direct-play
	// policy, so older clients cannot become software-decoding candidates by
	// accident.
	FeatureSoftwareVideoDecodeV3 = "software_video_decode_v1"
	// FeatureHeaderAuthenticatedMediaV3 advertises an opt-in transport mode
	// whose client-visible stream and subtitle URLs carry no signed playback
	// credential. A client that sends this token promises to attach its normal
	// access-token Authorization header to every media request, including HLS
	// manifests/segments and sidecar subtitle/font requests.
	FeatureHeaderAuthenticatedMediaV3 = "header_authenticated_media_v1"
	// FeatureAuthorizedMediaOriginsV3 is the client's promise to fetch media
	// from the absolute URLs a plan returns on server-designated origins (proxy
	// nodes), attaching its normal access-token Authorization header to those
	// requests exactly as it does to the API origin. It is meaningful only
	// together with header_authenticated_media_v1: on its own there is nothing
	// to designate, because a legacy attempt already receives signed proxy URLs
	// that authenticate themselves.
	//
	// Without it a header-authenticated attempt stays entirely on the API
	// origin, so every byte egresses from the API server; with it the plan may
	// hand out credential-free proxy origins and distributed egress is restored.
	FeatureAuthorizedMediaOriginsV3 = "authorized_media_origins_v1"
	// FeaturePlanInvalidatedV3 is the client's promise to handle the realtime
	// plan_invalidated command: ack it, replan off the named plan with operation
	// failure_recovery and the invalidated plan's attempt key in
	// attempted_plan_keys, then report the result.
	//
	// It exists because the server may only learn a route is wrong after the
	// plan is already playing — the H.264 copy-safety scan now runs
	// asynchronously so playback never waits on it. A session that did not
	// negotiate the token, or has no realtime connection, is stopped instead;
	// the client's ordinary recovery then mints a fresh attempt that plans
	// against the now-persisted verdict.
	FeaturePlanInvalidatedV3 = "plan_invalidated_v1"
	// FeatureDefaultAudioReconcileResponseV3 is the client's promise that it
	// can (a) answer a default_audio_reconciliation withdrawal through the
	// dedicated answers_plan_invalidation correlation instead of the ordinary
	// failure_recovery semantics, and (b) name that withdrawal's reason back
	// to the server on its replan. A client that advertises only
	// plan_invalidated_v1 treats every plan_invalidated as a route failure and
	// folds the withdrawn attempt key into attempted_plan_keys, so a
	// default-audio withdrawal would exclude a perfectly healthy route from
	// its own replacement: the server withholds the withdrawal from such
	// clients and lands the correction on the next start/reconnect instead.
	FeatureDefaultAudioReconcileResponseV3 = "default_audio_reconcile_response_v1"
	// FeatureSubripSidecarV3 is the client's statement that it parses SubRip
	// itself, including {\anN} placement. An opted-in client receives
	// external and downloaded SRT tracks as the original .srt bytes instead
	// of the WebVTT conversion, which cannot carry every SRT feature. Embedded
	// SRT tracks keep their existing delivery. It exists only on /api/v2 (see
	// NativeServerFeaturesV3).
	FeatureSubripSidecarV3 = "subrip_sidecar_v1"
	FeatureLiveInventoryV3 = "live_inventory_refresh_v1"
	// FeatureSourceCommittedV3 is the client's promise to handle the realtime
	// source_committed event: re-key its version menu to the effective source
	// the transport committed to and adopt the declared audio inventory without
	// a replan. It only appears on /api/v2, like the other realtime-v3
	// capabilities.
	FeatureSourceCommittedV3 = "source_committed_event_v1"
	// FeatureInventoryUpdatedV3 is the client's promise to handle the realtime
	// inventory_updated event: replace the declared track menu it took from
	// source_committed or its plan with the probe-verified audio and subtitle
	// inventory, gating on inventory_revision. It only appears on /api/v2, like
	// the other realtime-v3 capabilities.
	FeatureInventoryUpdatedV3 = "inventory_updated_event_v1"
	// FeatureDeferredTrackInventoryV3 is the client's promise to handle a plan
	// that defers its full track enumeration: it reads playback_plan.tracks_pending
	// and keeps the provisional menu until the follow-up inventory_updated push
	// (or the inventory poll named by playback_plan.inventory_url) lands. It
	// only appears on /api/v2, like the other realtime-v3 capabilities.
	FeatureDeferredTrackInventoryV3 = "deferred_track_inventory_v1"
	PlanRecipeVersionV3             = "v3.4"
	ClientDV7ToDV81V3               = "client_dv7_to_dv81"
	ClientDV7ToHDR10V3              = "client_dv7_to_hdr10"
	ClientDVTransformVersionV3      = "1"
	// ClaimClientManagedDynamicRangeV3 is scoped to the original_http
	// delivery. A client that advertises it accepts responsibility for mapping
	// any source dynamic range it declares decodable onto the active output;
	// the server must not require native sink-HDR support before delivering the
	// original bytes. Packaged server deliveries remain output-gated.
	ClaimClientManagedDynamicRangeV3 = "client_managed_dynamic_range_v1"
	ClaimClientSelectedAudioTrackV3  = "client_selected_audio_track_v1"
	// ClaimClientDV8BaseLayerFallbackV3 is scoped to the original_http
	// delivery. The executor decodes a single-layer Dolby Vision Profile 8
	// stream through an ordinary HEVC decoder and presents its
	// standards-compatible base layer when the active output lacks native
	// Dolby Vision. It is deliberately narrower than
	// ClaimClientManagedDynamicRangeV3: the server still decides which base
	// range the plan promises (from dv_bl_compat_id), still requires the
	// active output to carry that range, and never claims Dolby Vision output.
	ClaimClientDV8BaseLayerFallbackV3 = "client_dv8_base_layer_fallback_v1"
	ClientDV8HDR10PlusSanitizerV3     = "client_dv8_hdr10plus_sanitizer_v1"
	ClientNativeHLSPlaybackV3         = "native_hls_playback_v1"
	ClientPostResumeRecoveryV3        = "client_post_resume_video_recovery_v1"
	ClientSurfaceRecoveryV3           = "client_surface_recovery_v1"
	DeviceQuirkRegistryRevisionV3     = "2026-07-13.1"
)

// Worker transport features are protocol capabilities rather than media
// transformations. They let route planning exclude an older worker before a
// client is handed a URL that worker cannot serve.
const (
	TransportFeatureProgressiveRemuxExecutionV1 = "progressive_remux_execution_v1"
	TransportFeatureProgressiveRemuxRelayV1     = "progressive_remux_relay_v1"
	// Theme audio markers: a proxy that serves /stream/theme, and a transcode
	// node that approves theme files as progressive AAC inputs.
	TransportFeatureThemeAudioEgressV1    = "theme_audio_egress_v1"
	TransportFeatureThemeAudioExecutionV1 = "theme_audio_execution_v1"
	// A transcode node that executes the multi-track prepared-download layout
	// (PreparedTracksRecipeVersion).
	TransportFeaturePreparedTracksV1 = "prepared_tracks_v1"
	// A transcode node that makes trickplay sheets (POST /trickplay/extract).
	TransportFeatureTrickplayExtractV1 = "trickplay_extract_v1"
)

// Degradation warning codes reported by playback plans.
const (
	DegradationWarningHDRToneMappedV3 = "hdr_tone_mapped"
	// A carried-over audio selection is not on the effective file, so the plan
	// plays the file's default audio track instead of refusing to start.
	DegradationWarningAudioTrackUnavailableV3 = "audio_track_unavailable"
	// A carried-over subtitle selection has no equivalent on the effective
	// file, so the plan starts with subtitles off instead of refusing to start.
	DegradationWarningSubtitleTrackUnavailableV3 = "subtitle_track_unavailable"
)

// AudioTrackUnavailableWarningV3 reports an audio selection that fell back to
// the file's default track.
func AudioTrackUnavailableWarningV3() DegradationWarningV3 {
	return DegradationWarningV3{Code: DegradationWarningAudioTrackUnavailableV3, Message: "The selected audio track is not on this file; playing its default instead."}
}

// SubtitleTrackUnavailableWarningV3 reports a subtitle selection that was
// dropped because the effective file has no equivalent track.
func SubtitleTrackUnavailableWarningV3() DegradationWarningV3 {
	return DegradationWarningV3{Code: DegradationWarningSubtitleTrackUnavailableV3, Message: "The selected subtitle track is not on this file; starting without it."}
}

// ServerFeaturesV3 returns the complete feature set advertised by protocol-v3
// capability and decision responses. A fresh slice prevents callers from
// mutating the shared contract.
func ServerFeaturesV3() []string {
	return []string{
		FeaturePlaybackPlanV3,
		FeatureServerRemoteStreamBitratePolicyV3,
		FeatureServerLocalStreamBitratePolicyV3,
		FeatureNeutralContractV3,
		FeatureEmbeddedSubtitlesV3,
		FeatureLayoutPassthrough,
		FeatureRouteDiagnostics,
		FeatureDeviceQuirksV3,
		FeatureSeekReanchorV3,
		FeatureOutputChangeV3,
		FeatureOutputDisplayEvidenceV3,
		FeatureDirectStreamResumeV3,
		FeatureHeaderAuthenticatedMediaV3,
		FeatureAuthorizedMediaOriginsV3,
		FeatureSoftwareVideoDecodeV3,
		FeaturePlanInvalidatedV3,
		// Advertised so the withdrawal side and the response side of
		// default-audio reconciliation can be negotiated independently:
		// a client that only handles route-failure invalidations never
		// receives a default-audio withdrawal (§6.1.1).
		FeatureDefaultAudioReconcileResponseV3,
		// Advertised so a client can tell "this server does not populate
		// source.duration_seconds" apart from "this server knows the runtime
		// is genuinely unknown". Without the distinction both look like an
		// absent field, and a client cannot decide whether its own catalog
		// fallback is still required.
		FeaturePlanSourceDurationV3,
	}
}

// NativeServerFeaturesV3 is ServerFeaturesV3 plus the features the server
// advertises and honors only on /api/v2. They postdate the /api/v1 freeze, so
// the frozen surface neither advertises nor negotiates them.
func NativeServerFeaturesV3() []string {
	return append(ServerFeaturesV3(), FeatureSubripSidecarV3, FeatureLiveInventoryV3, FeatureSourceCommittedV3, FeatureInventoryUpdatedV3, FeatureDeferredTrackInventoryV3)
}

// WithoutFeatureV3 returns features with every spelling of feature removed.
func WithoutFeatureV3(features []string, feature string) []string {
	return slices.DeleteFunc(slices.Clone(features), func(candidate string) bool {
		return strings.EqualFold(strings.TrimSpace(candidate), feature)
	})
}

type DecisionOutcomeV3 string

const (
	OutcomePlayableV3              DecisionOutcomeV3 = "playable"
	OutcomeAdaptationUnavailableV3 DecisionOutcomeV3 = "adaptation_unavailable"
)

type DeliveryV3 string

const (
	DeliveryOriginalHTTPV3     DeliveryV3 = "original_http"
	DeliveryRemuxHLSV3         DeliveryV3 = "server_remux_hls"
	DeliveryRemuxProgressiveV3 DeliveryV3 = "server_remux_progressive"
	DeliveryTranscodeHLSV3     DeliveryV3 = "server_transcode_hls"
)

// Delivery classes are the client-side negotiation unit: a client advertises
// the delivery classes it can execute in ClientPlaybackContextV3.Deliveries,
// and each client maps them onto its own player internally. The server-side
// DeliveryV3 values above are the finer-grained plan outcomes; DeliveryClassV3
// folds them onto the negotiation keys.
const (
	DeliveryClassOriginalHTTPV3 = "original_http"
	DeliveryClassProgressiveV3  = "progressive"
	DeliveryClassHLSV3          = "hls"
)

// DeliveryClassV3 maps a plan delivery to the capability class the client
// advertises for it.
func DeliveryClassV3(d DeliveryV3) string {
	switch d {
	case DeliveryOriginalHTTPV3:
		return DeliveryClassOriginalHTTPV3
	case DeliveryRemuxProgressiveV3:
		return DeliveryClassProgressiveV3
	case DeliveryRemuxHLSV3, DeliveryTranscodeHLSV3:
		return DeliveryClassHLSV3
	default:
		return string(d)
	}
}

type StreamProtocolV3 string

const (
	StreamHTTPProgressiveV3 StreamProtocolV3 = "http_progressive"
	StreamHLSV3             StreamProtocolV3 = "hls"
)

type HeaderRefreshModeV3 string

const (
	HeaderRefreshNoneV3    HeaderRefreshModeV3 = "none"
	HeaderRefreshSessionV3 HeaderRefreshModeV3 = "session"
)

// AudioOnlyRemuxMIMEV3 is the content type of the fragmented MP4 the remux
// pipeline produces for a source with no video track. The transport serves the
// same value, so a client that gates attachment on the advertised MIME sees a
// promise the response keeps.
const AudioOnlyRemuxMIMEV3 = "audio/mp4"

// Dynamic-range vocabulary shared by source descriptors, effective recipes and
// the range a transformation promises to produce.
const (
	DynamicRangeSDRV3         = "sdr"
	DynamicRangeHDR10V3       = "hdr10"
	DynamicRangeHDR10PlusV3   = "hdr10_plus"
	DynamicRangeHLGV3         = "hlg"
	DynamicRangeDolbyVisionV3 = "dolby_vision"
	DynamicRangeHDRUnknownV3  = "hdr_unknown"
)

// Server transformation names. A plan names the transformations its serving
// executor must run; the registry keys availability by the same names.
const (
	TransformationAudioToAACV3      = "audio_to_aac"
	TransformationVideoToH264V3     = "video_to_h264"
	TransformationVideoToHEVCV3     = "video_to_hevc"
	TransformationVideoToAV1V3      = "video_to_av1"
	TransformationServerDV7HDR10V3  = "server_dv7_to_hdr10"
	TransformationHDRToSDRToneMapV3 = "hdr_to_sdr_tonemap"

	TransformationVideoToH264RecipeVersionV3     = "2"
	TransformationVideoToHEVCRecipeVersionV3     = "1"
	TransformationVideoToAV1RecipeVersionV3      = "1"
	TransformationAudioToAACRecipeVersionV3      = "4"
	TransformationHDRToSDRToneMapRecipeVersionV3 = "1"
	// TransformationServerDV7HDR10RecipeVersionV3 2 also removes the Profile 7
	// enhancement-layer NAL units (see DV7ToHDR10BitstreamFilter), so an
	// executor still on version 1 cannot claim the corrected output.
	TransformationServerDV7HDR10RecipeVersionV3 = "2"
)

// Target video codecs a server HLS transcode may emit. H.264 is the floor:
// the planner selects AV1 or HEVC only when the client declares hardware
// decode for that codec, the HLS delivery accepts it, and the installed
// encoder toolchain advertises the matching transformation.
const (
	TargetVideoCodecAV1V3  = "av1"
	TargetVideoCodecHEVCV3 = "hevc"
	TargetVideoCodecH264V3 = "h264"
)

// Transformation executors: who runs the transformation. A "server"
// transformation is performed by the serving executor before the bytes leave
// the server; a "client" one is the client's own responsibility.
const (
	ExecutorServerV3 = "server"
	ExecutorClientV3 = "client"
)

// Validated claims a server transformation asserts about its output: neutral
// statements about the bytes, not client-framework decoder names.
const (
	ClaimAudioDecodeV3                = "audio_decode"
	ClaimH264DecodeV3                 = "h264_decode"
	ClaimHEVCDecodeV3                 = "hevc_decode"
	ClaimAV1DecodeV3                  = "av1_decode"
	ClaimDolbyVisionMetadataRemovedV3 = "dolby_vision_metadata_removed"
	ClaimHDR10BaseLayerPreservedV3    = "hdr10_base_layer_preserved"
	ClaimEnhancementLayerDiscardedV3  = "enhancement_layer_discarded"
	ClaimHDRMetadataRemovedV3         = "hdr_metadata_removed"
	ClaimSDRBT709OutputV3             = "sdr_bt709_output"
)

// DV7ToHDR10ClaimsV3 returns the claims the server DV7→HDR10 transformation
// asserts. A fresh slice keeps callers from mutating the shared contract.
func DV7ToHDR10ClaimsV3() []string {
	return []string{ClaimDolbyVisionMetadataRemovedV3, ClaimHDR10BaseLayerPreservedV3, ClaimEnhancementLayerDiscardedV3}
}

// Terminal reasons reported when a required conversion toolchain is absent.
const (
	TerminalAudioConversionUnsupportedV3 = "audio_conversion_unsupported"
	TerminalVideoConversionUnsupportedV3 = "video_conversion_unsupported"
	TerminalHDRTranscodeUnsupportedV3    = "hdr_transcode_unsupported"
	TerminalDVConversionUnsupportedV3    = "dv_conversion_unsupported"
)

// TerminalSourceUnreadableV3 reports that ffprobe rejected the effective file
// (zero-byte, corrupt, truncated) and no stream metadata exists for it. Unlike
// source_metadata_incomplete, which covers a file that has not been probed
// yet, retrying cannot help until the file is replaced and rescanned.
const TerminalSourceUnreadableV3 = "source_unreadable"

// TerminalSourceUnreadableMessageV3 is the planner message for
// TerminalSourceUnreadableV3.
const TerminalSourceUnreadableMessageV3 = "The source file could not be read; it appears to be empty or damaged."

// TerminalBitratePolicyUnavailableV3 reports that no route fits the
// administrator's local or remote per-stream bitrate limit for this version.
const TerminalBitratePolicyUnavailableV3 = "bitrate_policy_unavailable"

type SubtitleModeV3 string

const (
	SubtitleOffV3     SubtitleModeV3 = "off"
	SubtitleRenderV3  SubtitleModeV3 = "render"
	SubtitleConvertV3 SubtitleModeV3 = "convert"
	SubtitleBurnInV3  SubtitleModeV3 = "burn_in"
)

type SubtitleFidelityV3 string

const (
	SubtitleFidelityPreserveV3   SubtitleFidelityV3 = "preserve"
	SubtitleFidelityCompatibleV3 SubtitleFidelityV3 = "compatible"
)

type EnhancementLayerV3 string

const (
	EnhancementNoneV3    EnhancementLayerV3 = "none"
	EnhancementMELV3     EnhancementLayerV3 = "mel"
	EnhancementFELV3     EnhancementLayerV3 = "fel"
	EnhancementUnknownV3 EnhancementLayerV3 = "unknown"
)

type HDRCapabilitiesV3 struct {
	HDR10                    bool                             `json:"hdr10"`
	HDR10Plus                bool                             `json:"hdr10_plus"`
	HLG                      bool                             `json:"hlg"`
	HDR10MaxWidth            int                              `json:"hdr10_max_width,omitempty"`
	HDR10MaxHeight           int                              `json:"hdr10_max_height,omitempty"`
	HDR10MaxFrameRate        float64                          `json:"hdr10_max_frame_rate,omitempty"`
	HDR10MaxBitrateKbps      int                              `json:"hdr10_max_bitrate_kbps,omitempty"`
	DolbyVisionProfiles      []int                            `json:"dolby_vision_profiles"`
	DolbyVisionProfileLevels []DolbyVisionProfileCapabilityV3 `json:"dolby_vision_profile_levels,omitempty"`
}

type DolbyVisionProfileCapabilityV3 struct {
	Profile            int   `json:"profile"`
	MaxLevel           int   `json:"max_level"`
	BLCompatibilityIDs []int `json:"bl_compatibility_ids,omitempty"`
}

type AudioPassthroughV3 struct {
	PassthroughCodecs  []string                  `json:"passthrough_codecs"`
	SpatializerEnabled bool                      `json:"spatializer_enabled"`
	MaxChannels        int                       `json:"max_channels"`
	Entries            []AudioPassthroughEntryV3 `json:"entries,omitempty"`
}

type AudioPassthroughEntryV3 struct {
	Codec         string   `json:"codec"`
	ChannelCounts []int    `json:"channel_counts,omitempty"`
	Layouts       []string `json:"layouts,omitempty"`
}

type VideoDecodeCapabilityV3 struct {
	Codec          string   `json:"codec"`
	DecoderName    string   `json:"decoder_name,omitempty"`
	Profiles       []string `json:"profiles,omitempty"`
	Levels         []int    `json:"levels,omitempty"`
	BitDepths      []int    `json:"bit_depths,omitempty"`
	MaxWidth       int      `json:"max_width,omitempty"`
	MaxHeight      int      `json:"max_height,omitempty"`
	MaxFrameRate   float64  `json:"max_frame_rate,omitempty"`
	MaxBitrateKbps int      `json:"max_bitrate_kbps,omitempty"`
	Hardware       bool     `json:"hardware"`
}

// Capability evidence tiers. Each area (video, audio) declares how its
// capability facts were produced, and planner strictness follows the tier:
//
//   - exact: per-codec profiles/levels/bit-depths/bounds from a real platform
//     probe (Android MediaCodecList). Full strict validation.
//   - platform_attested: platform-level decoder-stack attestation without
//     profile/level enumeration (for example Apple's VideoToolbox plus a
//     pinned software stack). Codec, resolution, bit depth, frame rate, and
//     dynamic range are validated; profile/level matching is skipped instead
//     of failing conservative. Software entries require an explicit feature.
//   - declared: boolean support statements (web MediaSource.isTypeSupported).
//     Copy routes are granted on codec+container+range match from the flat
//     codec lists; no strict direct claims are made.
type CapabilityEvidenceV3 string

const (
	EvidenceExactV3            CapabilityEvidenceV3 = "exact"
	EvidencePlatformAttestedV3 CapabilityEvidenceV3 = "platform_attested"
	EvidenceDeclaredV3         CapabilityEvidenceV3 = "declared"
)

func validCapabilityEvidenceV3(v CapabilityEvidenceV3) bool {
	return v == EvidenceExactV3 || v == EvidencePlatformAttestedV3 || v == EvidenceDeclaredV3
}

// validateVideoCapabilitiesStructureV3 performs the non-mutating structural
// checks on the video half of the shared capability payload. It is the
// validation half of the normalize/validate split: a replan is rejected for
// malformed values, but nothing is rewritten or dropped, because the durable
// request is normalized exactly once — after its features are merged — and the
// degradation warnings from that single normalization must remain
// discoverable. Length checks measure the trimmed value, because normalization
// trims before checking.
func validateVideoCapabilitiesStructureV3(c *ClientCodecCapabilitiesV3, features []string) error {
	if !validCapabilityEvidenceV3(c.VideoEvidence) {
		return errors.New("video_evidence is required and must be exact, platform_attested, or declared")
	}
	if len(features) > 64 || len(c.CodecsVideo) > 64 || len(c.CodecsVideoHardware) > 64 || len(c.VideoDecode) > 64 {
		return errors.New("video capability list exceeds supported size")
	}
	for _, feature := range features {
		if len(feature) > 128 {
			return errors.New("client feature exceeds supported size")
		}
	}
	for _, values := range [][]string{c.CodecsVideo, c.CodecsVideoHardware} {
		for _, value := range values {
			if len(strings.TrimSpace(value)) > 128 {
				return errors.New("capability value exceeds supported size")
			}
		}
	}
	for i := range c.VideoDecode {
		entry := &c.VideoDecode[i]
		if strings.TrimSpace(entry.Codec) == "" || len(entry.DecoderName) > 128 || entry.MaxWidth < 0 || entry.MaxHeight < 0 || entry.MaxFrameRate < 0 || entry.MaxBitrateKbps < 0 {
			return errors.New("invalid detailed video capability")
		}
		if len(entry.Profiles) > 64 || len(entry.Levels) > 64 || len(entry.BitDepths) > 64 {
			return errors.New("detailed video capability exceeds supported size")
		}
		for _, profile := range entry.Profiles {
			if len(profile) > 64 {
				return errors.New("detailed video capability value exceeds supported size")
			}
		}
	}
	return nil
}

// normalizeVideoCapabilitiesV3 rewrites the video half of the payload into its
// canonical form. Structural validation must have run first.
func normalizeVideoCapabilitiesV3(c *ClientCodecCapabilitiesV3) {
	for _, values := range [][]string{c.CodecsVideo, c.CodecsVideoHardware} {
		for i := range values {
			values[i] = strings.ToLower(strings.TrimSpace(values[i]))
		}
	}
	for i := range c.VideoDecode {
		c.VideoDecode[i].Codec = strings.ToLower(strings.TrimSpace(c.VideoDecode[i].Codec))
	}
}

type ClientCodecCapabilitiesV3 struct {
	// VideoEvidence and AudioEvidence are required closed enums declaring the
	// provenance of the respective capability facts.
	VideoEvidence       CapabilityEvidenceV3      `json:"video_evidence"`
	AudioEvidence       CapabilityEvidenceV3      `json:"audio_evidence"`
	CodecsVideo         []string                  `json:"codecs_video"`
	CodecsVideoHardware []string                  `json:"codecs_video_hardware"`
	CodecsAudio         []string                  `json:"codecs_audio"`
	Containers          []string                  `json:"containers"`
	MaxResolution       string                    `json:"max_resolution,omitempty"`
	HDR                 bool                      `json:"hdr"`
	HDRDetails          *HDRCapabilitiesV3        `json:"hdr_details,omitempty"`
	AudioPassthrough    *AudioPassthroughV3       `json:"audio_passthrough,omitempty"`
	VideoDecode         []VideoDecodeCapabilityV3 `json:"video_decode,omitempty"`
}

// DeviceContextV3 is platform-neutral device identity. Manufacturer and model
// stay first-class because the quirk registry matches on them; everything
// platform-specific (Android sdk_int, soc_model, build fields, …) travels in
// PlatformDetails as opaque bounded strings.
type DeviceContextV3 struct {
	Platform        string            `json:"platform,omitempty"`
	OSVersion       string            `json:"os_version,omitempty"`
	Manufacturer    string            `json:"manufacturer,omitempty"`
	Model           string            `json:"model,omitempty"`
	PlatformDetails map[string]string `json:"platform_details,omitempty"`
}

type OutputContextV3 struct {
	HDRDetails       *HDRCapabilitiesV3  `json:"hdr_details,omitempty"`
	AudioPassthrough *AudioPassthroughV3 `json:"audio_passthrough,omitempty"`
	CurrentSink      string              `json:"current_sink,omitempty"`
	SinkType         string              `json:"sink_type,omitempty"`
	// Display carries the raw active-display HDR facts and how they were
	// obtained. It is additive: HDRDetails above remains the native-output
	// authority (decoder ∩ display) that older servers already read. When a
	// client sends Display at all, the server never falls back from a missing
	// output HDRDetails to the device-level capability, and an unknown
	// evidence tier disables native HDR/DV output claims.
	Display *OutputDisplayV3 `json:"display,omitempty"`
	// OutputContextID is an optional opaque token identifying the current
	// output route. The server only ever compares it for equality — in attempt
	// keys and plan invalidation — so any stable platform-native identity
	// works: Android supplies its route generation stringified, Apple its
	// synthetic sink hash, web omits it.
	OutputContextID string `json:"output_context_id,omitempty"`
}

// OutputDisplayV3 is the raw display probe with its evidence tier.
type OutputDisplayV3 struct {
	// HDREvidence is OutputHDREvidenceExactV3 for a successful probe (an
	// empty HDRTypes then means a confirmed SDR panel) or
	// OutputHDREvidenceUnknownV3 when the platform could not answer.
	HDREvidence string             `json:"hdr_evidence"`
	HDRTypes    *HDRCapabilitiesV3 `json:"hdr_types,omitempty"`
	DisplayID   string             `json:"display_id,omitempty"`
}

const (
	OutputHDREvidenceExactV3   = "exact"
	OutputHDREvidenceUnknownV3 = "unknown"
)

const (
	subtitleIdentityFFmpegV3    = "ffmpeg_stream_index"
	subtitleIdentityContainerV3 = "container_track_id"
)

// NativeEmbeddedSubtitleCapabilityV3 attests native selection for one container
// and codec set. Stream indexes and container track IDs are distinct namespaces.
type NativeEmbeddedSubtitleCapabilityV3 struct {
	Container       string   `json:"container"`
	Codecs          []string `json:"codecs"`
	TrackIdentity   string   `json:"track_identity"`
	ASSStyling      bool     `json:"ass_styling"`
	FontAttachments bool     `json:"font_attachments"`
}

type DeliverySubtitleCapabilitiesV3 struct {
	NativeEmbedded  []NativeEmbeddedSubtitleCapabilityV3 `json:"native_embedded,omitempty"`
	EmbeddedText    bool                                 `json:"embedded_text"`
	SidecarText     bool                                 `json:"sidecar_text"`
	ASSStyling      bool                                 `json:"ass_styling"`
	EmbeddedBitmap  bool                                 `json:"embedded_bitmap"`
	SidecarBitmap   bool                                 `json:"sidecar_bitmap"`
	FontAttachments bool                                 `json:"font_attachments"`
}

type DeliveryCapabilityV3 struct {
	Enabled                bool     `json:"enabled"`
	SupportedOnDevice      bool     `json:"supported_on_device"`
	FailureReason          string   `json:"failure_reason,omitempty"`
	Containers             []string `json:"containers"`
	VideoCodecs            []string `json:"video_codecs"`
	AudioDecodeCodecs      []string `json:"audio_decode_codecs"`
	AudioPassthroughCodecs []string `json:"audio_passthrough_codecs"`
	// MaxChannels caps the channel count of the audio stream this class
	// delivers: the most channels the client can play from it. A client whose
	// player downmixes surround itself omits it. A source track above it is
	// converted to AAC within the ceiling, on a video-copy remux when the
	// video can be copied. Zero or less means unset.
	MaxChannels       *int                           `json:"max_channels,omitempty"`
	HDRDetails        *HDRCapabilitiesV3             `json:"hdr_details,omitempty"`
	Subtitles         DeliverySubtitleCapabilitiesV3 `json:"subtitles"`
	Features          []string                       `json:"features"`
	AuthHeaderRefresh bool                           `json:"auth_header_refresh"`
	ValidatedClaims   []string                       `json:"validated_claims"`
	Transformations   []TransformationV3             `json:"transformations"`
}

// ClientPlaybackContextV3 carries the client's execution context. Feature
// advertisement lives exclusively in the request's top-level client_features
// list; there is deliberately no second features location here.
type ClientPlaybackContextV3 struct {
	ProtocolVersion int    `json:"protocol_version"`
	FormFactor      string `json:"form_factor"`
	AppVersion      string `json:"app_version"`
	// AppBuild and AppChannel are the request-body fallback for the
	// X-Vio-Client-Build / X-Vio-Client-Channel headers. Both are opaque
	// strings the server stores verbatim.
	AppBuild   string                          `json:"app_build,omitempty"`
	AppChannel string                          `json:"app_channel,omitempty"`
	Device     DeviceContextV3                 `json:"device"`
	Output     OutputContextV3                 `json:"output"`
	Deliveries map[string]DeliveryCapabilityV3 `json:"deliveries"`
}

type StartRequestV3 struct {
	ProtocolVersion   int      `json:"protocol_version"`
	ClientFeatures    []string `json:"client_features"`
	FileID            int      `json:"file_id"`
	ProfileID         string   `json:"profile_id"`
	PlaybackAttemptID string   `json:"playback_attempt_id"`
	QualityPreference string   `json:"quality_preference"`
	// False pins the source file through start and every replan. Encoding and
	// delivery can still adapt to the viewer without changing the timeline.
	AllowAlternateVersions     *bool                 `json:"allow_alternate_versions,omitempty"`
	SubtitleFidelityPreference SubtitleFidelityV3    `json:"subtitle_fidelity_preference"`
	StartPosition              *float64              `json:"start_position,omitempty"`
	ProgressPersistence        ProgressPersistenceV3 `json:"progress_persistence,omitempty"`
	AudioTrackID               string                `json:"audio_track_id,omitempty"`
	AudioTrackIndex            *int                  `json:"audio_track_index,omitempty"`
	// CarriedAudioTrackID is the file-bound audio identity (file:<id>:audio:<ordinal>)
	// the viewer had selected on a previously playing version. The server re-resolves
	// it onto the requested file's inventory by track family instead of raw ordinal.
	// Optional and additive: absent means the server owns audio selection (profile or
	// series preference), exactly as before.
	CarriedAudioTrackID string `json:"carried_audio_track_id,omitempty"`
	SubtitleTrackID     string `json:"subtitle_track_id,omitempty"`
	SubtitleTrackIndex  *int   `json:"subtitle_track_index,omitempty"`
	// FileSelection distinguishes an explicit user version pick from an
	// auto/default selection. Absent means auto, preserving existing client
	// behavior; an explicit pick pins the requested file and disables the
	// start-path alternate-file fallback.
	FileSelection FileSelectionV3 `json:"file_selection,omitempty"`
	// ForceRelink asks the server to fetch a fresh provider listing for an
	// explicitly re-selected version instead of trusting the cached or pinned
	// candidate. It is only meaningful alongside an explicit file_selection;
	// absent means the server keeps its existing listing/cache behavior.
	ForceRelink           bool                      `json:"force_relink,omitempty"`
	Metered               bool                      `json:"metered"`
	BandwidthEstimateKbps *int                      `json:"bandwidth_estimate_kbps,omitempty"`
	BandwidthCapKbps      *int                      `json:"bandwidth_cap_kbps,omitempty"`
	Capabilities          ClientCodecCapabilitiesV3 `json:"client_capabilities"`
	ClientPlaybackContext ClientPlaybackContextV3   `json:"client_playback_context"`
}

func (r StartRequestV3) AllowsAlternateVersions() bool {
	return r.AllowAlternateVersions == nil || *r.AllowAlternateVersions
}

// ProgressPersistenceV3 declares which side owns durable item resume/history.
// Session progress is still reported in both modes so live playback state and
// diagnostics remain accurate.
type ProgressPersistenceV3 string

const (
	ProgressPersistenceServerV3 ProgressPersistenceV3 = "server"
	ProgressPersistenceClientV3 ProgressPersistenceV3 = "client"
)

// FileSelectionV3 declares whether the requested file_id is an explicit user
// version pick or an auto/default selection. An explicit pick must not be
// silently swapped for an alternate version when the plan is terminal; the
// server refuses instead and points the client at the version list.
type FileSelectionV3 string

const (
	FileSelectionAutoV3     FileSelectionV3 = "auto"
	FileSelectionExplicitV3 FileSelectionV3 = "explicit"
)

type TrackIdentityV3 struct {
	ID    string `json:"id"`
	Index *int   `json:"index,omitempty"`
}

type SelectedTracksV3 struct {
	Audio    *TrackIdentityV3 `json:"audio,omitempty"`
	Subtitle *TrackIdentityV3 `json:"subtitle,omitempty"`
}

type FailureV3 struct {
	Classification string `json:"classification"`
	Message        string `json:"message,omitempty"`
	DecoderName    string `json:"decoder_name,omitempty"`
}

type ReplanOperationV3 string

const (
	ReplanOperationFailureRecoveryV3     ReplanOperationV3 = "failure_recovery"
	ReplanOperationSeekReanchorV3        ReplanOperationV3 = "seek_reanchor"
	ReplanOperationSeekFailureRecoveryV3 ReplanOperationV3 = "seek_failure_recovery"
	// ReplanOperationTrackChangeV3 replaces the legacy audio PATCH: the client
	// sends new selected_tracks and no failure classification. It runs through
	// the same replan transaction as failure recovery, inheriting idempotency
	// and restart safety.
	ReplanOperationTrackChangeV3 ReplanOperationV3 = "track_change"
	// ReplanOperationQualityChangeV3 replaces the client-recipe half of the
	// legacy transcode start: the client sends a quality_preference chosen from
	// the plan's available_qualities and no failure classification.
	ReplanOperationQualityChangeV3 ReplanOperationV3 = "quality_change"
	// ReplanOperationOutputChangeV3 refreshes output capabilities without
	// declaring the active route failed, so an unchanged route stays eligible.
	ReplanOperationOutputChangeV3 ReplanOperationV3 = "output_change"
)

type ReplanRequestV3 struct {
	ProtocolVersion int `json:"protocol_version"`
	// ClientFeatures is the single feature-advertisement location; the
	// playback context deliberately carries no second features list.
	ClientFeatures    []string          `json:"client_features,omitempty"`
	Operation         ReplanOperationV3 `json:"operation,omitempty"`
	PlaybackAttemptID string            `json:"playback_attempt_id"`
	ReplanRequestID   string            `json:"replan_request_id"`
	FailedPlanID      string            `json:"failed_plan_id"`
	PlanAttemptID     string            `json:"plan_attempt_id"`
	PlanAttemptKey    string            `json:"plan_attempt_key"`
	AttemptedPlanKeys []string          `json:"attempted_plan_keys"`
	// LocalMutations reports client-applied local plan mutations (for example
	// a PCM recovery route) so the server can fold them into the attempt key it
	// computes for the failed plan. Clients never hash anything themselves.
	LocalMutations    []string `json:"local_mutations,omitempty"`
	AttemptCount      int      `json:"attempt_count"`
	QualityPreference string   `json:"quality_preference"`
	PositionSeconds   float64  `json:"position_seconds"`
	Metered           bool     `json:"metered"`
	// AutoFallback re-negotiates the session's version-fallback intent. A
	// viewer who re-arms Auto mid-session states it here so a later dead-source
	// recovery rotates even though the session started on an explicit pick.
	// Omitted leaves the start-time intent unchanged, so clients that predate
	// the field keep their existing behavior. It never authorizes a healthy
	// mid-play switch: only a dead or unplayable source advances it.
	AutoFallback          *bool `json:"auto_fallback,omitempty"`
	BandwidthEstimateKbps *int  `json:"bandwidth_estimate_kbps,omitempty"`
	BandwidthCapKbps      *int  `json:"bandwidth_cap_kbps,omitempty"`
	// AnswersPlanInvalidation carries the `reason` string from the
	// plan_invalidated command this replan is answering. It is the
	// authoritative correlation between a withdrawal and its response:
	// present only on the replan that answers a withdrawn plan, never on
	// ordinary failure recovery or intent replans. It is NOT trust-sensitive
	// the way Automatic is: at worst it can make the server apply a
	// correction the server itself already decided and announced, so the
	// ingress boundary does not strip it (contrast
	// stripClientSuppliedAutomatic, which drops a marker the client has no
	// right to set because forging it would impersonate server
	// reconciliation).
	AnswersPlanInvalidation string                    `json:"answers_plan_invalidation,omitempty"`
	SelectedTracks          SelectedTracksV3          `json:"selected_tracks"`
	Failure                 FailureV3                 `json:"failure,omitzero"`
	Capabilities            ClientCodecCapabilitiesV3 `json:"client_capabilities"`
	ClientPlaybackContext   ClientPlaybackContextV3   `json:"client_playback_context"`
	// Automatic marks a replan request the server built itself (no client
	// gesture behind it). It is server-side only: omitempty keeps it off the
	// client wire in practice, Validate ignores it, and clients never send
	// it. The replan application uses it to suppress user-preference
	// persistence for automatic corrections; explicit user track changes
	// keep persisting. Empty means a client-issued request.
	Automatic string `json:"automatic,omitempty"`
}

// ReplanAutomaticV3 is the Automatic marker for server-built reconciliation
// replans (see ReplanRequestV3.Automatic).
const ReplanAutomaticV3 = "automatic"

const (
	RouteEventPlanSelectedV3               = "plan_selected"
	RouteEventPlanInvalidatedV3            = "plan_invalidated"
	RouteEventPlanFailedV3                 = "plan_failed"
	RouteEventFirstFrameV3                 = "first_frame"
	RouteEventTerminalV3                   = "terminal"
	RouteEventStoppedV3                    = "stopped"
	RouteEventRuntimeCorrectionAppliedV3   = "runtime_correction_applied"
	RouteEventRuntimeCorrectionSucceededV3 = "runtime_correction_succeeded"
	RouteEventRuntimeCorrectionFailedV3    = "runtime_correction_failed"
	RouteEventSeekReanchorRequestedV3      = "seek_reanchor_requested"
	RouteEventSeekReanchoredV3             = "seek_reanchored"
)

var routeEventNamesV3 = []string{
	RouteEventPlanSelectedV3,
	RouteEventPlanInvalidatedV3,
	RouteEventPlanFailedV3,
	RouteEventFirstFrameV3,
	RouteEventTerminalV3,
	RouteEventStoppedV3,
	RouteEventRuntimeCorrectionAppliedV3,
	RouteEventRuntimeCorrectionSucceededV3,
	RouteEventRuntimeCorrectionFailedV3,
	RouteEventSeekReanchorRequestedV3,
	RouteEventSeekReanchoredV3,
}

// RouteEventNamesV3 returns the complete protocol-v3 telemetry event contract.
func RouteEventNamesV3() []string {
	return append([]string(nil), routeEventNamesV3...)
}

// ValidRouteEventNameV3 reports whether name is part of the protocol-v3
// telemetry contract shared by handlers, persistence, and clients.
func ValidRouteEventNameV3(name string) bool {
	return slices.Contains(routeEventNamesV3, name)
}

// EffectiveOperation keeps clients which predate the explicit operation field
// on the ordinary failure-recovery path. Seek operations are deliberately
// opt-in because both pin the current media version and user intent; an exact
// reanchor also preserves the current route instead of walking the ladder.
func (r ReplanRequestV3) EffectiveOperation() ReplanOperationV3 {
	if r.Operation == "" {
		return ReplanOperationFailureRecoveryV3
	}
	return r.Operation
}

type RouteEventV3 struct {
	ProtocolVersion       int               `json:"protocol_version"`
	PlaybackAttemptID     string            `json:"playback_attempt_id"`
	SessionID             string            `json:"session_id,omitempty"`
	PlanID                string            `json:"plan_id,omitempty"`
	PlanAttemptID         string            `json:"plan_attempt_id,omitempty"`
	PlanAttemptKey        string            `json:"plan_attempt_key,omitempty"`
	Event                 string            `json:"event"`
	FailureClassification string            `json:"failure_classification,omitempty"`
	FallbackReason        string            `json:"fallback_reason,omitempty"`
	AppliedQuirkIDs       []string          `json:"applied_quirk_ids,omitempty"`
	QuirkRegistryRevision string            `json:"quirk_registry_revision,omitempty"`
	OutputContextID       string            `json:"output_context_id,omitempty"`
	Diagnostics           map[string]string `json:"diagnostics"`
}

type StreamV3 struct {
	URL              string              `json:"url"`
	Protocol         StreamProtocolV3    `json:"protocol"`
	Container        string              `json:"container,omitempty"`
	MIMEType         string              `json:"mime_type,omitempty"`
	Headers          map[string]string   `json:"headers"`
	HeaderRefresh    HeaderRefreshModeV3 `json:"header_refresh"`
	HeaderRefreshURL string              `json:"header_refresh_url,omitempty"`
}

type TimelineV3 struct {
	SourceStartSeconds     float64  `json:"source_start_seconds"`
	StreamOriginSeconds    float64  `json:"stream_origin_seconds"`
	PlayerStartSeconds     float64  `json:"player_start_seconds"`
	TimelineOffsetSeconds  float64  `json:"timeline_offset_seconds"`
	SeekWindowStartSeconds *float64 `json:"seek_window_start_seconds,omitempty"`
	SeekWindowEndSeconds   *float64 `json:"seek_window_end_seconds,omitempty"`
	CanSeekAnywhere        bool     `json:"can_seek_anywhere"`
	SeekRestoration        string   `json:"seek_restoration"`
}

type EffectiveRecipeV3 struct {
	VideoCodec       string   `json:"video_codec,omitempty"`
	VideoSampleEntry string   `json:"video_sample_entry,omitempty"`
	AudioCodec       string   `json:"audio_codec,omitempty"`
	Width            *int     `json:"width,omitempty"`
	Height           *int     `json:"height,omitempty"`
	FrameRate        *float64 `json:"frame_rate,omitempty"`
	BitrateKbps      *int     `json:"bitrate_kbps,omitempty"`
	DynamicRange     string   `json:"dynamic_range,omitempty"`
	AudioChannels    *int     `json:"audio_channels,omitempty"`
	AudioLayout      string   `json:"audio_layout,omitempty"`
	// SoftwareVideoDecode marks a route that keeps its hardware encoder but
	// decodes the source on the CPU. It is set only by reactive failure
	// recovery after the executed hardware decoder rejected the source, and it
	// participates in plan identity so the software retry is a distinct
	// attempt from the hardware plan it replaces.
	SoftwareVideoDecode bool `json:"software_video_decode,omitempty"`
}

type SourceDescriptorV3 struct {
	MediaFileID int `json:"media_file_id"`
	// DurationSeconds is the full runtime of this source, independent of where
	// the delivery's timeline is anchored: never `total - source_start`, and
	// never adjusted by timeline_offset_seconds.
	//
	// Absent means the server does not know the runtime. It is omitted rather
	// than sent as null: clients that coerce null to a numeric default would
	// read it as zero, which is the value this field exists to stop them
	// inventing. A client must not substitute the playback engine's reported
	// duration for it — on an HLS copy remux the engine reports the length
	// produced so far, not the runtime.
	DurationSeconds *float64 `json:"duration_seconds,omitempty"`
	Container       string   `json:"container,omitempty"`
	VideoCodec      string   `json:"video_codec,omitempty"`
	VideoProfile    string   `json:"video_profile,omitempty"`
	VideoLevel      int      `json:"video_level,omitempty"`
	BitDepth        int      `json:"bit_depth,omitempty"`
	ColorRange      string   `json:"color_range,omitempty"`
	Width           int      `json:"width,omitempty"`
	Height          int      `json:"height,omitempty"`
	FrameRate       float64  `json:"frame_rate,omitempty"`
	BitrateKbps     int      `json:"bitrate_kbps,omitempty"`
	DynamicRange    string   `json:"dynamic_range,omitempty"`
	HDR10Plus       bool     `json:"hdr10_plus"`
	DVProfile       int      `json:"dolby_vision_profile,omitempty"`
	DVLevel         int      `json:"dolby_vision_level,omitempty"`
	DVBLCompatID    int      `json:"dv_bl_compat_id,omitempty"`
	// DVBaseLayerProven is true only when the scan recorded a Dolby Vision
	// configuration record, an explicit compatibility id, and a present base
	// layer. A legacy or contradictory row keeps the numeric id but cannot
	// prove a decodable compatible base signal.
	DVBaseLayerProven  bool               `json:"dv_base_layer_proven,omitempty"`
	DVEnhancementLayer EnhancementLayerV3 `json:"dv_enhancement_layer"`
	AudioCodec         string             `json:"audio_codec,omitempty"`
	AudioChannels      int                `json:"audio_channels,omitempty"`
	AudioLayout        string             `json:"audio_layout,omitempty"`
	// VideoCopyUnsafe marks a source whose video stream cannot be safely
	// stream-copied into an avc1/fMP4 segment (H.264 with conflicting in-band
	// PPS). Copy/remux routes are disqualified for it; a real encode is used.
	VideoCopyUnsafe bool `json:"video_copy_unsafe,omitempty"`
}

type VideoClaimsV3 struct {
	HDR10             bool   `json:"hdr10"`
	HDR10Plus         bool   `json:"hdr10_plus"`
	HLG               bool   `json:"hlg"`
	DolbyVision       bool   `json:"dolby_vision"`
	DolbyVisionReason string `json:"dolby_vision_reason,omitempty"`
}

type AudioClaimsV3 struct {
	Codec          string `json:"codec,omitempty"`
	Passthrough    bool   `json:"passthrough"`
	AtmosPreserved bool   `json:"atmos_preserved"`
	DTSVariant     string `json:"dts_variant,omitempty"`
	Reason         string `json:"reason,omitempty"`
}

type SubtitleClaimsV3 struct {
	ASSStylingPreserved bool   `json:"ass_styling_preserved"`
	BitmapOverlay       bool   `json:"bitmap_overlay"`
	BitmapSidecar       bool   `json:"bitmap_sidecar"`
	Reason              string `json:"reason,omitempty"`
}

type ValidationClaimsV3 struct {
	Video     VideoClaimsV3    `json:"video"`
	Audio     AudioClaimsV3    `json:"audio"`
	Subtitles SubtitleClaimsV3 `json:"subtitles"`
}

type SubtitleArtifactV3 struct {
	URL                 string  `json:"url"`
	MIMEType            string  `json:"mime_type"`
	Format              string  `json:"format"`
	TimingOriginSeconds float64 `json:"timing_origin_seconds"`
}

// EmbeddedSubtitleV3 selects a track in the unmodified media source. The
// container identifier is canonical decimal when the probe supplies one.
type EmbeddedSubtitleV3 struct {
	StreamIndex      int    `json:"stream_index"`
	ContainerTrackID string `json:"container_track_id,omitempty"`
}

type SubtitleDecisionV3 struct {
	Embedded *EmbeddedSubtitleV3 `json:"embedded,omitempty"`
	Mode     SubtitleModeV3      `json:"mode"`
	TrackID  string              `json:"track_id,omitempty"`
	// Artifact is the selected sidecar, mutually exclusive with Embedded. It exists only under
	// SubtitleRenderV3 and SubtitleConvertV3; SubtitleOffV3 and
	// SubtitleBurnInV3 have no client-fetchable artifact and must publish none,
	// including on a plan derived from an earlier plan of the same session.
	// SubtitleOffV3 carries no TrackID either. Inventory URLs are independent
	// of the selection and stay published in every mode.
	Artifact *SubtitleArtifactV3 `json:"artifact,omitempty"`
	// Inventory is the complete, gap-free combined-ordinal subtitle track list
	// for the effective source. It is authoritative: a client selects a track
	// by echoing an entry's track_id or combined_index and never derives an
	// ordinal by counting, summing track arrays, or taking max(index)+1.
	Inventory []SubtitleInventoryItemV3 `json:"inventory"`
}

type TransformationV3 struct {
	Name            string   `json:"name"`
	Executor        string   `json:"executor"`
	RecipeVersion   string   `json:"recipe_version"`
	ValidatedClaims []string `json:"validated_claims"`
}

type AppliedQuirkV3 struct {
	ID               string `json:"id"`
	RegistryRevision string `json:"registry_revision"`
	Action           string `json:"action"`
	Reason           string `json:"reason,omitempty"`
}

type DegradationWarningV3 struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// QualityOriginalV3 is the quality preference that pins the source ladder
// rung: the plan must preserve the source's own height and bitrate rather than
// pick a transcode rung. Distinct from OriginalLanguageSentinel, which selects
// a track's original language.
const QualityOriginalV3 = "original"

// Compound ladder rung labels pin a bitrate as well as a resolution class.
// The plain resolution labels remain valid for stored/default preferences;
// menu selections use these explicit variants so every visible entry has
// unambiguous quality semantics, including at the source's own resolution.
const (
	QualityRung2160pHighV3   = "2160p-high"
	QualityRung2160pMediumV3 = "2160p-medium"
	QualityRung2160pLowV3    = "2160p-low"
	QualityRung1080pHighV3   = "1080p-high"
	QualityRung1080pMediumV3 = "1080p-medium"
	QualityRung1080pLowV3    = "1080p-low"
	QualityRung720pHighV3    = "720p-high"
	QualityRung720pMediumV3  = "720p-medium"
	QualityRung720pLowV3     = "720p-low"
)

// DeliveryChangeV3 records a mid-session serving-route change on a replan. It
// is the explicit marker for a delivery and/or play-method swap so a client can
// distinguish a planned route change from a silent one. It carries wire tokens
// only (no URLs, tokens, or headers).
type DeliveryChangeV3 struct {
	// PreviousDelivery is the delivery the session was serving before this plan.
	PreviousDelivery DeliveryV3 `json:"previous_delivery,omitempty"`
	// Delivery is the delivery this plan will serve. It always differs from
	// PreviousDelivery when the marker is present.
	Delivery DeliveryV3 `json:"delivery,omitempty"`
	// PreviousPlayMethod is the serve path before this plan (direct, remux, or
	// transcode).
	PreviousPlayMethod PlayMethod `json:"previous_play_method,omitempty"`
	// PlayMethod is the serve path this plan will use.
	PlayMethod PlayMethod `json:"play_method,omitempty"`
	// DeliveryChanged and PlayMethodChanged name which of the two actually
	// moved, so a client can react to a route-shape change without diffing the
	// tokens itself.
	DeliveryChanged   bool `json:"delivery_changed,omitempty"`
	PlayMethodChanged bool `json:"play_method_changed,omitempty"`
}

// AvailableQualityV3 is one server-ladder rung valid for this source and
// client, published on the plan so clients can render a quality menu without
// owning a bitrate table. The QualityOriginalV3 entry preserves the source;
// DisplayName gives compound rungs their human-facing High/Medium/Low label.
type AvailableQualityV3 struct {
	Label           string `json:"label"`
	DisplayName     string `json:"display_name,omitempty"`
	Height          int    `json:"height,omitempty"`
	BitrateKbps     int    `json:"bitrate_kbps,omitempty"`
	PreservesSource bool   `json:"preserves_source"`
}

type PlanV3 struct {
	ProtocolVersion int    `json:"protocol_version"`
	PlanID          string `json:"plan_id"`
	// PlanAttemptKey is the server-computed opaque loop-prevention token for
	// this plan. Clients store the keys of attempted plans and echo them in
	// attempted_plan_keys on replan; they never compute keys themselves.
	PlanAttemptKey       string                 `json:"plan_attempt_key"`
	SessionID            string                 `json:"session_id,omitempty"`
	ExpiresAt            string                 `json:"expires_at,omitempty"`
	Delivery             DeliveryV3             `json:"delivery"`
	Stream               StreamV3               `json:"stream"`
	Timeline             TimelineV3             `json:"timeline"`
	SelectedTracks       SelectedTracksV3       `json:"selected_tracks"`
	EffectiveRecipe      EffectiveRecipeV3      `json:"effective_recipe"`
	Claims               ValidationClaimsV3     `json:"claims"`
	Subtitle             SubtitleDecisionV3     `json:"subtitle"`
	Transformations      []TransformationV3     `json:"transformations"`
	AppliedQuirks        []AppliedQuirkV3       `json:"applied_quirks"`
	RuntimeCorrections   []string               `json:"runtime_corrections"`
	AvailableQualities   []AvailableQualityV3   `json:"available_qualities"`
	DegradationWarnings  []DegradationWarningV3 `json:"degradation_warnings"`
	DecisionReason       string                 `json:"decision_reason"`
	RequestedMediaFileID int                    `json:"requested_media_file_id"`
	EffectiveMediaFileID int                    `json:"effective_media_file_id"`
	// SubstitutedFromFileID names the catalog row the client asked for when the
	// effective release differs from it; it is the requested row id and is
	// omitted when the effective row is the requested one. It exists so a
	// client can surface an honest substitution notice without first diffing
	// the requested/effective ids, and carries SubstitutionReason alongside.
	// UI-only, like the inventory hints below: it is set after plan identity is
	// finalized and is deliberately excluded from plan identity hashing.
	SubstitutedFromFileID int `json:"substituted_from_file_id,omitempty"`
	// SubstitutionReason is the machine-readable cause of a version
	// substitution, present only with SubstitutedFromFileID: a dead release, a
	// provider listing failure, a transport failure, or a decode rejection.
	// The set is additive; clients treat an unknown value as a generic
	// substitution rather than failing to render.
	SubstitutionReason string `json:"substitution_reason,omitempty"`
	// DeliveryChange, when present, records that this plan changed the serving
	// route mid-session: the delivery and/or play method differs from the plan
	// the session was previously serving. A failure-recovery replan may
	// legitimately move from one delivery to another (for example
	// server_transcode_hls to server_remux_progressive), and without an explicit
	// marker the client only sees the new plan body and cannot tell a silent
	// mid-play route swap from a planned change. It is a UI-only signal, set
	// after plan identity is finalized and deliberately excluded from plan
	// identity hashing, like the substitution and inventory hints beside it.
	DeliveryChange *DeliveryChangeV3 `json:"delivery_change,omitempty"`
	// EffectiveVirtualURI is the provider-neutral virtual:// candidate URI the
	// planner selected and probed when it substituted a real candidate for a
	// neutral catalog row. UI-only: clients use it to keep the version menu in
	// sync with the version that actually played. It is deliberately excluded
	// from plan identity hashing, like the inventory fields above.
	EffectiveVirtualURI string `json:"effective_virtual_uri,omitempty"`
	// VirtualSourceRevision is an opaque, non-secret revision of the resolved
	// virtual source media generation. It changes when the planner resolves a
	// different candidate (a release rotation) and when the track evidence is
	// repaired under the same candidate, and stays fixed while the same
	// generation is served, so clients can re-arm source-change recovery on a
	// rotation even when the published effective_media_file_id and
	// effective_virtual_uri are unchanged. Its inputs are the provider-neutral
	// candidate identity (the `?result=` fingerprint) plus the evidence
	// generation (probe stamp, probe version, canonical track fingerprint) — never a provider
	// URL, token, or header; the value is a domain-separated SHA-256. Empty
	// for a non-virtual source. Like EffectiveVirtualURI it is a UI hint and
	// is deliberately excluded from plan identity hashing.
	VirtualSourceRevision  string             `json:"virtual_source_revision,omitempty"`
	Source                 SourceDescriptorV3 `json:"source"`
	SubtitleFidelityPolicy string             `json:"subtitle_fidelity_policy"`
	// AudioTracks is the authoritative per-track audio inventory of the
	// effective source, mirroring the subtitle inventory. Clients should
	// prefer it over item metadata: after a version fallback the effective
	// file can differ from the requested catalog row, and only this list
	// reflects the tracks the plan actually plays. Select a track by echoing an
	// entry's track_id or selection_index, never by its raw index.
	AudioTracks []AudioInventoryItemV3 `json:"audio_tracks,omitempty"`
	// InventoryRevision is an opaque, deterministic revision of the plan's
	// authoritative audio and subtitle inventories. It advances when the
	// background probe populates or repairs tracks under a live virtual session.
	InventoryRevision string `json:"inventory_revision,omitempty"`
	// InventoryStatus indicates whether the track inventory is "declared"
	// (placeholder from candidate metadata) or "verified" (probed by ffprobe).
	InventoryStatus string `json:"inventory_status,omitempty"`
	// InventoryProvenance qualifies InventoryStatus with the resolver's own
	// provenance for a virtual source: "verified" (this resolve probed the
	// served bytes), "declared" (provider metadata only), "pending" (a probe
	// is deferred), or "failed" (a probe ran and failed, so declared metadata
	// is served). It is empty for a non-virtual source. InventoryStatus is
	// inferred from the row's probe stamp alone, which a stale stamp or a
	// served declared fallback can make look "verified"; this field reports
	// what the resolve that produced the plan actually did. Like the inventory
	// fields beside it, it is a UI hint and is deliberately excluded from plan
	// identity hashing; it never changes selection.
	InventoryProvenance string `json:"inventory_provenance,omitempty"`
	// InventoryURL is the relative URL clients can fetch (with ETag/If-None-Match)
	// to retrieve refreshed audio and subtitle inventories without replanning.
	InventoryURL string `json:"inventory_url,omitempty"`
	// TracksPending marks a plan whose audio and subtitle inventory is
	// deliberately provisional: the first-byte URL was handed back with the
	// minimal executable recipe (video plus the default audio) and the full
	// track enumeration is still being probed. It is additive and omitted on a
	// plan whose inventory is already complete. A client shows its track menu
	// as loading until the follow-up `inventory_updated` push (or the existing
	// inventory poll) replaces the provisional list; the value never changes
	// stream selection or the executable recipe, and it is excluded from plan
	// identity hashing like the inventory fields beside it.
	TracksPending bool `json:"tracks_pending,omitempty"`
}

// PlaybackInventoryV3 is the live audio and subtitle inventory of an active
// playback session, returned by GET /api/v2/playback/{session_id}/inventory.
type PlaybackInventoryV3 struct {
	SessionID         string                    `json:"session_id"`
	InventoryRevision string                    `json:"inventory_revision"`
	InventoryStatus   string                    `json:"inventory_status"`
	AudioTracks       []AudioInventoryItemV3    `json:"audio_tracks"`
	SubtitleInventory []SubtitleInventoryItemV3 `json:"subtitle_inventory"`
	// EffectiveMediaFileID is the catalog row the transport is committed to.
	// It moves when a serve-layer rotation rebinds the session to a sibling
	// release, so a client polling this endpoint can follow the streamed
	// version instead of the plan's stale identity. It equals the plan's
	// effective_media_file_id on an un-rotated session.
	EffectiveMediaFileID int `json:"effective_media_file_id,omitempty"`
	// EffectiveVirtualURI is the provider-neutral candidate URI the session is
	// bound to. Like the plan's effective_virtual_uri it lets a client re-key
	// its version menu to the release actually being served; unlike the plan it
	// is read from the live session, so it survives a rotation.
	EffectiveVirtualURI string `json:"effective_virtual_uri,omitempty"`
	// VirtualSourceRevision is the opaque media-generation revision the plan
	// published for the bound candidate. A serve-layer rotation clears it (the
	// new release is not probed yet), which is itself a signal that the
	// inventory is declared rather than verified. Empty for a non-virtual
	// source.
	VirtualSourceRevision string `json:"virtual_source_revision,omitempty"`
}

type inventoryRevisionEnvelope struct {
	Status      string                    `json:"status"`
	AudioTracks []AudioInventoryItemV3    `json:"audio_tracks"`
	Subtitles   []SubtitleInventoryItemV3 `json:"subtitles"`
	// EffectiveMediaFileID, EffectiveVirtualURI and VirtualSourceRevision name
	// the effective source. They are omitted for the zero identity, so a plan's
	// inventory-only revision keeps its historical digest; a live-session
	// reader supplies them so a rotation to a sibling with an identical track
	// list still changes the revision (and therefore the ETag).
	EffectiveMediaFileID  int    `json:"effective_media_file_id,omitempty"`
	EffectiveVirtualURI   string `json:"effective_virtual_uri,omitempty"`
	VirtualSourceRevision string `json:"virtual_source_revision,omitempty"`
}

// InventorySourceIdentityV3 names the effective source an inventory revision
// describes. The zero value is valid and omits every field from the digest.
type InventorySourceIdentityV3 struct {
	EffectiveMediaFileID  int
	EffectiveVirtualURI   string
	VirtualSourceRevision string
}

// ComputeInventoryRevisionV3 returns a deterministic opaque digest of the full
// audio and subtitle inventories, suitable for ETag generation and inventory
// caching. It serializes the exact response-visible inventory to canonical JSON
// before hashing, preventing delimiter collisions and ensuring every field
// mutation produces a distinct revision. A caller that reads a live session may
// pass one source identity so the effective version is part of the digest: a
// rotation to a sibling with an identical inventory then changes the revision
// instead of returning 304. Callers that only revise a plan's inventory pass no
// identity and keep the historical digest.
func ComputeInventoryRevisionV3(status string, audio []AudioInventoryItemV3, subs []SubtitleInventoryItemV3, source ...InventorySourceIdentityV3) string {
	var identity InventorySourceIdentityV3
	if len(source) > 0 {
		identity = source[0]
	}
	payload, err := json.Marshal(inventoryRevisionEnvelope{
		Status:                status,
		AudioTracks:           audio,
		Subtitles:             subs,
		EffectiveMediaFileID:  identity.EffectiveMediaFileID,
		EffectiveVirtualURI:   identity.EffectiveVirtualURI,
		VirtualSourceRevision: identity.VirtualSourceRevision,
	})
	if err != nil {
		h := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%d:%d:%s:%s", status, len(audio), len(subs), identity.EffectiveMediaFileID, identity.EffectiveVirtualURI, identity.VirtualSourceRevision)))
		return fmt.Sprintf("inv:%x", h[:16])
	}
	h := sha256.Sum256(payload)
	return fmt.Sprintf("inv:%x", h[:16])
}

// AudioInventoryItemV3 is one selectable audio track of the effective source at
// its canonical selection ordinal. It mirrors SubtitleInventoryItemV3 so a
// client selects a track by echoing track_id or selection_index instead of the
// raw container stream index, which lives in a different domain: index is the
// absolute ffprobe stream index (video 0, first audio 1, subtitles
// interleaved), while selection_index is the 0-based inventory position the
// start/replan request's audio_track_index and audio_track_id must carry.
type AudioInventoryItemV3 struct {
	Index         int      `json:"index,omitempty"`
	Title         string   `json:"title,omitempty"`
	EmbeddedTitle string   `json:"embedded_title,omitempty"`
	Language      string   `json:"language,omitempty"`
	Languages     []string `json:"languages,omitempty"`
	Codec         string   `json:"codec,omitempty"`
	Profile       string   `json:"profile,omitempty"`
	Layout        string   `json:"layout,omitempty"`
	Channels      int      `json:"channels,omitempty"`
	Bitrate       int      `json:"bitrate,omitempty"`
	SampleRate    int      `json:"sample_rate,omitempty"`
	BitDepth      int      `json:"bit_depth,omitempty"`
	Default       bool     `json:"default"`
	// TrackID is the canonical selection identity, file:<id>:audio:<selection_index>.
	TrackID string `json:"track_id"`
	// SelectionIndex is this track's 0-based position in the inventory, the
	// value the selected_tracks.audio identity and the audio_track_index
	// request field use.
	SelectionIndex int `json:"selection_index"`
}

type TerminalV3 struct {
	Reason    string `json:"reason"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	Detail    string `json:"detail,omitempty"`
}

type DecisionResponseV3 struct {
	ProtocolVersion int               `json:"protocol_version"`
	ServerFeatures  []string          `json:"server_features"`
	Outcome         DecisionOutcomeV3 `json:"outcome"`
	SessionID       string            `json:"session_id,omitempty"`
	PlaybackPlan    *PlanV3           `json:"playback_plan,omitempty"`
	Terminal        *TerminalV3       `json:"terminal,omitempty"`
}

type CapabilityResponseV3 struct {
	Enabled          bool               `json:"enabled"`
	ProtocolVersions []int              `json:"protocol_versions"`
	Features         []string           `json:"features"`
	Deliveries       []DeliveryV3       `json:"deliveries"`
	Transformations  []TransformationV3 `json:"transformations"`
	Reason           string             `json:"reason,omitempty"`
}

var boundedIdentifierV3 = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$`)

func (r *StartRequestV3) NormalizeAndValidate() ([]DegradationWarningV3, error) {
	if r.ProtocolVersion != ProtocolV3 {
		return nil, fmt.Errorf("protocol_version must be %d", ProtocolV3)
	}
	if r.FileID <= 0 || strings.TrimSpace(r.ProfileID) == "" {
		return nil, errors.New("file_id and profile_id are required")
	}
	if !boundedIdentifierV3.MatchString(r.PlaybackAttemptID) {
		return nil, errors.New("playback_attempt_id is invalid")
	}
	if r.ClientPlaybackContext.ProtocolVersion != ProtocolV3 {
		return nil, errors.New("client_playback_context.protocol_version must be 3")
	}
	if r.StartPosition != nil && (!isFiniteV3(*r.StartPosition) || *r.StartPosition < 0 || *r.StartPosition > 31_536_000) {
		return nil, errors.New("start_position is outside the supported range")
	}
	if r.ProgressPersistence == "" {
		r.ProgressPersistence = ProgressPersistenceServerV3
	}
	if r.ProgressPersistence != ProgressPersistenceServerV3 && r.ProgressPersistence != ProgressPersistenceClientV3 {
		return nil, errors.New("progress_persistence is invalid")
	}
	if r.ProgressPersistence == ProgressPersistenceClientV3 && r.StartPosition == nil {
		return nil, errors.New("start_position is required when progress_persistence is client")
	}
	if r.FileSelection == "" {
		r.FileSelection = FileSelectionAutoV3
	}
	if r.FileSelection != FileSelectionAutoV3 && r.FileSelection != FileSelectionExplicitV3 {
		return nil, errors.New("file_selection is invalid")
	}
	if err := validateOptionalBoundedIntV3(r.BandwidthEstimateKbps, 100, 1_000_000, "bandwidth_estimate_kbps"); err != nil {
		return nil, err
	}
	if err := validateOptionalBoundedIntV3(r.BandwidthCapKbps, 100, 1_000_000, "bandwidth_cap_kbps"); err != nil {
		return nil, err
	}
	if r.SubtitleFidelityPreference != SubtitleFidelityPreserveV3 && r.SubtitleFidelityPreference != SubtitleFidelityCompatibleV3 {
		return nil, errors.New("subtitle_fidelity_preference is invalid")
	}
	if len(r.ClientFeatures) > 64 {
		return nil, errors.New("client_features exceeds supported size")
	}
	for _, feature := range r.ClientFeatures {
		if len(feature) > 128 {
			return nil, errors.New("client feature exceeds supported size")
		}
	}
	warnings, err := validateCapabilitiesV3(&r.Capabilities, &r.ClientPlaybackContext, r.ClientFeatures)
	if err != nil {
		return nil, err
	}
	// A selected track identity can name the effective file of an earlier
	// attempt, which virtual candidate rotation may have replaced. Drop such a
	// stale identity before the file-bound id/index check so the start degrades
	// to the default track pipeline instead of failing 400; the replan path
	// re-keys the equivalent identity before its own validation. A same-file
	// id/index disagreement is not stale and is still rejected below.
	r.DropStaleSelectedTrackIdentitiesV3()
	if err := validateTrackPairV3(r.FileID, "audio", r.AudioTrackID, r.AudioTrackIndex); err != nil {
		return nil, err
	}
	if len(r.CarriedAudioTrackID) > 128 {
		return nil, errors.New("carried_audio_track_id is too long")
	}
	if err := validateTrackPairV3(r.FileID, "subtitle", r.SubtitleTrackID, r.SubtitleTrackIndex); err != nil {
		return nil, err
	}
	quality, changed := NormalizeQualityV3(r.QualityPreference)
	r.QualityPreference = quality
	if changed {
		warnings = append(warnings, DegradationWarningV3{Code: "quality_preference_normalized", Message: "Unknown quality preference was normalized to auto."})
	}
	return warnings, nil
}

func NormalizeQualityV3(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "auto":
		return "auto", false
	case QualityOriginalV3, "source", "max":
		return QualityOriginalV3, false
	case "2160p", "4k", "uhd":
		return "2160p", false
	case "1080p", "fhd", "1080p-8":
		// 1080p-8 is a legacy 1080p variant (8-bit encode); it is not a
		// distinct ladder rung, so it normalizes to the plain 1080p rung.
		return "1080p", false
	case "720p", "hd":
		return "720p", false
	case "480p", "sd":
		return "480p", false
	case "420p":
		return "420p", false
	case "328p":
		return "328p", false
	default:
		if rung, ok := ladderRungForLabelV3(value); ok {
			return rung.Label, false
		}
		return "auto", true
	}
}

func (r *ReplanRequestV3) Validate() error {
	if r == nil {
		return errors.New("replan request is required")
	}
	if r.ProtocolVersion != ProtocolV3 || !boundedIdentifierV3.MatchString(r.PlaybackAttemptID) || !boundedIdentifierV3.MatchString(r.ReplanRequestID) {
		return errors.New("invalid replan identity")
	}
	if len(r.FailedPlanID) < 8 || len(r.FailedPlanID) > 128 || len(r.PlanAttemptID) < 8 || len(r.PlanAttemptID) > 128 || len(r.PlanAttemptKey) < 8 || len(r.PlanAttemptKey) > 128 || !strings.HasPrefix(r.PlanAttemptKey, "v3:") || r.AttemptCount < 1 || r.AttemptCount > 8 {
		return errors.New("invalid replan attempt")
	}
	if len(r.AttemptedPlanKeys) > 16 || len(r.Failure.Classification) > 64 || len(r.Failure.Message) > 512 || len(r.Failure.DecoderName) > 128 || !isFiniteV3(r.PositionSeconds) || r.PositionSeconds < 0 || r.PositionSeconds > 31_536_000 {
		return errors.New("replan bounds exceeded")
	}
	if len(r.LocalMutations) > 8 {
		return errors.New("local_mutations exceeds supported size")
	}
	for _, mutation := range r.LocalMutations {
		if mutation == "" || len(mutation) > 64 {
			return errors.New("invalid local mutation")
		}
	}
	if err := validateOptionalBoundedIntV3(r.BandwidthEstimateKbps, 100, 1_000_000, "bandwidth_estimate_kbps"); err != nil {
		return err
	}
	if err := validateOptionalBoundedIntV3(r.BandwidthCapKbps, 100, 1_000_000, "bandwidth_cap_kbps"); err != nil {
		return err
	}
	if err := validateSelectedTrackIdentityV3("audio", r.SelectedTracks.Audio); err != nil {
		return err
	}
	if err := validateSelectedTrackIdentityV3("subtitle", r.SelectedTracks.Subtitle); err != nil {
		return err
	}
	switch r.EffectiveOperation() {
	case ReplanOperationFailureRecoveryV3, ReplanOperationSeekFailureRecoveryV3:
		if r.Failure.Classification == "" {
			return errors.New("failure recovery requires a failure classification")
		}
	case ReplanOperationSeekReanchorV3:
		// An exact seek reanchor is a timeline operation, not a failed recipe.
		// Classification remains accepted for older callers but is not required
		// and never selects seek semantics.
	case ReplanOperationTrackChangeV3:
		// A user track change is not a failure; no classification is required.
		if r.Failure != (FailureV3{}) {
			return errors.New("track_change must not include failure")
		}
	case ReplanOperationQualityChangeV3:
		// A user quality change is not a failure either, but it must actually
		// name the wanted rung: an empty preference would silently mean "auto",
		// which is a different user intent than the menu selection this
		// operation models.
		if strings.TrimSpace(r.QualityPreference) == "" {
			return errors.New("quality_change requires a quality_preference")
		}
		if r.Failure != (FailureV3{}) {
			return errors.New("quality_change must not include failure")
		}
	case ReplanOperationOutputChangeV3:
		// Output capability refreshes are intent changes, not route failures.
		if r.Failure != (FailureV3{}) {
			return errors.New("output_change must not include failure")
		}
	default:
		return errors.New("invalid replan operation")
	}
	for _, key := range r.AttemptedPlanKeys {
		if len(key) > 128 || !strings.HasPrefix(key, "v3:") {
			return errors.New("invalid attempted plan key")
		}
	}
	if len(r.ClientFeatures) > 64 {
		return errors.New("client_features exceeds supported size")
	}
	for _, feature := range r.ClientFeatures {
		if len(feature) > 128 {
			return errors.New("client feature exceeds supported size")
		}
	}
	// Replan validation is structural only. It must not normalize or drop
	// anything: the handler merges the attempt's durable features into the
	// replan's, and the execution path then normalizes the merged start request
	// exactly once and surfaces the resulting degradation warnings on the plan.
	// A normalizing Validate here would drop an un-negotiated transformation
	// before that merge, and the single execution-time normalization could no
	// longer rediscover it to warn the client.
	if err := validateCapabilitiesStructureV3(&r.Capabilities, &r.ClientPlaybackContext, r.ClientFeatures); err != nil {
		return err
	}
	return nil
}

func validateSelectedTrackIdentityV3(kind string, track *TrackIdentityV3) error {
	if track == nil {
		return nil
	}
	if len(track.ID) > 128 {
		return fmt.Errorf("%s track id exceeds supported size", kind)
	}
	if track.Index != nil && (*track.Index < 0 || *track.Index > 10_000) {
		return fmt.Errorf("%s track index is invalid", kind)
	}
	return nil
}

// validateCapabilitiesStructureV3 performs the non-mutating structural checks
// on the shared capability payload and is the validation half of the
// normalize/validate split. features carries the request's top-level
// client_features — the only feature-advertisement location in the contract —
// and is used only for its bound. Nothing here rewrites or drops a value, so it
// cannot consume the evidence a later single normalization needs to report a
// degradation warning.
func validateCapabilitiesStructureV3(c *ClientCodecCapabilitiesV3, ctx *ClientPlaybackContextV3, features []string) error {
	if c == nil || ctx == nil {
		return errors.New("client capabilities are required")
	}
	if err := validateVideoCapabilitiesStructureV3(c, features); err != nil {
		return err
	}
	if !validCapabilityEvidenceV3(c.AudioEvidence) {
		return errors.New("audio_evidence is required and must be exact, platform_attested, or declared")
	}
	if len(c.CodecsVideo) > 64 || len(c.CodecsVideoHardware) > 64 || len(c.CodecsAudio) > 64 || len(c.Containers) > 64 || len(c.VideoDecode) > 64 || len(ctx.Deliveries) > 16 || len(ctx.Device.Platform) > 32 || len(ctx.FormFactor) > 32 {
		return errors.New("capability list exceeds supported size")
	}
	deviceValues := []string{
		ctx.Device.OSVersion, ctx.Device.Manufacturer, ctx.Device.Model,
		ctx.Output.CurrentSink, ctx.Output.SinkType, ctx.Output.OutputContextID,
	}
	for _, value := range deviceValues {
		if len(value) > 128 {
			return errors.New("device capability value exceeds supported size")
		}
	}
	if len(ctx.Device.PlatformDetails) > 16 {
		return errors.New("platform_details exceeds supported size")
	}
	for key, value := range ctx.Device.PlatformDetails {
		if key == "" || len(key) > 128 || len(value) > 128 {
			return errors.New("platform_details entry exceeds supported size")
		}
	}
	for _, values := range [][]string{c.CodecsAudio, c.Containers} {
		for _, value := range values {
			// Normalization trims before enforcing the bound, so measure the
			// trimmed value here too; a heavily padded string must not be
			// rejected by structural validation only to be accepted later.
			if len(strings.TrimSpace(value)) > 128 {
				return errors.New("capability value exceeds supported size")
			}
		}
	}
	for _, hdr := range []*HDRCapabilitiesV3{c.HDRDetails, ctx.Output.HDRDetails} {
		if err := validateHDRCapabilitiesV3(hdr); err != nil {
			return err
		}
	}
	if display := ctx.Output.Display; display != nil {
		evidence := strings.ToLower(strings.TrimSpace(display.HDREvidence))
		switch evidence {
		case OutputHDREvidenceExactV3, OutputHDREvidenceUnknownV3:
		default:
			return errors.New("output display hdr_evidence must be exact or unknown")
		}
		if len(display.DisplayID) > 64 {
			return errors.New("output display id exceeds supported size")
		}
		if err := validateHDRCapabilitiesV3(display.HDRTypes); err != nil {
			return err
		}
		// An exact display record is the raw panel fact; output.hdr_details
		// is supposed to be its intersection with the decoder. Reject a
		// contradiction where hdr_details claims a range the panel does not
		// carry rather than let planning pick whichever one it reads first.
		if evidence == OutputHDREvidenceExactV3 && ctx.Output.HDRDetails != nil {
			panel := display.HDRTypes
			if panel == nil {
				panel = &HDRCapabilitiesV3{}
			}
			out := ctx.Output.HDRDetails
			if out.HDR10 && !panel.HDR10 || out.HDR10Plus && !panel.HDR10Plus || out.HLG && !panel.HLG {
				return errors.New("output hdr_details claims a range the exact display record does not carry")
			}
			for _, profile := range out.DolbyVisionProfiles {
				if !containsIntV3(panel.DolbyVisionProfiles, profile) {
					return errors.New("output hdr_details claims a dolby vision profile the exact display record does not carry")
				}
			}
			// Numeric bounds are ceilings: hdr_details may be tighter than
			// the panel (the decoder narrows it) but never looser.
			if boundExceedsV3(out.HDR10MaxWidth, panel.HDR10MaxWidth) || boundExceedsV3(out.HDR10MaxHeight, panel.HDR10MaxHeight) ||
				boundExceedsFloatV3(out.HDR10MaxFrameRate, panel.HDR10MaxFrameRate) || boundExceedsV3(out.HDR10MaxBitrateKbps, panel.HDR10MaxBitrateKbps) {
				return errors.New("output hdr_details hdr10 limits exceed the exact display record")
			}
			for _, capability := range out.DolbyVisionProfileLevels {
				for _, panelCapability := range panel.DolbyVisionProfileLevels {
					if panelCapability.Profile == capability.Profile && capability.MaxLevel > panelCapability.MaxLevel {
						return errors.New("output hdr_details dolby vision level exceeds the exact display record")
					}
				}
			}
		}
	}
	for name, delivery := range ctx.Deliveries {
		if len(name) > 64 || len(delivery.Containers) > 64 || len(delivery.VideoCodecs) > 64 || len(delivery.AudioDecodeCodecs) > 64 || len(delivery.AudioPassthroughCodecs) > 64 || len(delivery.Features) > 64 || len(delivery.ValidatedClaims) > 64 || len(delivery.Transformations) > 16 {
			return errors.New("delivery capability exceeds supported size")
		}
		if err := validateHDRCapabilitiesV3(delivery.HDRDetails); err != nil {
			return err
		}
		for _, values := range [][]string{delivery.Containers, delivery.VideoCodecs, delivery.AudioDecodeCodecs, delivery.AudioPassthroughCodecs, delivery.Features, delivery.ValidatedClaims} {
			for _, value := range values {
				if len(value) > 64 {
					return errors.New("delivery capability value exceeds supported size")
				}
			}
		}
		if len(delivery.Subtitles.NativeEmbedded) > 16 {
			return errors.New("native subtitle capability list exceeds supported size")
		}
		for i := range delivery.Subtitles.NativeEmbedded {
			native := &delivery.Subtitles.NativeEmbedded[i]
			container := strings.ToLower(strings.TrimSpace(native.Container))
			if container == "" || len(container) > 32 || len(native.Codecs) == 0 || len(native.Codecs) > 32 {
				return errors.New("invalid native subtitle capability")
			}
			if native.TrackIdentity != subtitleIdentityFFmpegV3 && native.TrackIdentity != subtitleIdentityContainerV3 {
				return errors.New("invalid native subtitle track identity")
			}
			for _, codec := range native.Codecs {
				if strings.TrimSpace(codec) == "" || len(codec) > 64 {
					return errors.New("invalid native subtitle codec")
				}
			}
		}
		if err := validateDeliveryTransformationsStructureV3(&delivery); err != nil {
			return err
		}
	}
	for _, passthrough := range []*AudioPassthroughV3{c.AudioPassthrough, ctx.Output.AudioPassthrough} {
		if passthrough == nil {
			continue
		}
		if len(passthrough.PassthroughCodecs) > 64 || len(passthrough.Entries) > 64 || passthrough.MaxChannels < 0 || passthrough.MaxChannels > 64 {
			return errors.New("audio passthrough capability exceeds supported size")
		}
		for _, entry := range passthrough.Entries {
			if len(entry.Codec) > 64 || len(entry.ChannelCounts) > 32 || len(entry.Layouts) > 32 {
				return errors.New("audio passthrough entry exceeds supported size")
			}
		}
	}
	return nil
}

// normalizeCapabilitiesV3 is the mutation half of the
// structural-validate/normalize split. It canonicalizes the shared capability
// payload in place and returns the degradation warnings produced while dropping
// a client transformation the merged feature list did not negotiate.
// validateCapabilitiesStructureV3 must have accepted the payload first.
func normalizeCapabilitiesV3(c *ClientCodecCapabilitiesV3, ctx *ClientPlaybackContextV3, features []string) ([]DegradationWarningV3, error) {
	normalizeVideoCapabilitiesV3(c)
	// Version, build, and channel are diagnostic labels, so an over-long value is
	// worth clamping and never worth refusing playback over. The header route
	// (X-Vio-Client-Version / -Build / -Channel) clamps with the same helper; rejecting
	// here would mean the same string plays from a header and 400s from the
	// body.
	ctx.AppVersion = normalizeClientMetadataValue(ctx.AppVersion, 64)
	ctx.AppBuild = normalizeClientMetadataValue(ctx.AppBuild, 64)
	ctx.AppChannel = normalizeClientMetadataValue(ctx.AppChannel, 32)
	for _, values := range [][]string{c.CodecsAudio, c.Containers} {
		for i := range values {
			values[i] = strings.ToLower(strings.TrimSpace(values[i]))
		}
	}
	if display := ctx.Output.Display; display != nil {
		display.HDREvidence = strings.ToLower(strings.TrimSpace(display.HDREvidence))
	}
	var warnings []DegradationWarningV3
	for name, delivery := range ctx.Deliveries {
		for i := range delivery.Subtitles.NativeEmbedded {
			native := &delivery.Subtitles.NativeEmbedded[i]
			native.Container = strings.ToLower(strings.TrimSpace(native.Container))
			for j, codec := range native.Codecs {
				native.Codecs[j] = normalizeNativeSubtitleCodecV3(codec)
			}
		}
		kept, transformWarnings, err := normalizeDeliveryTransformationsV3(&delivery, features)
		if err != nil {
			return nil, err
		}
		warnings = append(warnings, transformWarnings...)
		if len(delivery.Transformations) > 0 {
			delivery.Transformations = kept
		}
		ctx.Deliveries[name] = delivery
	}
	return warnings, nil
}

// validateCapabilitiesV3 validates and normalizes the shared capability payload
// in one call, returning the degradation warnings normalization produced. It is
// the start-request entry point; replan validation calls the structural half
// alone so the merged start request owns the single normalization that drops
// un-negotiated transformations.
func validateCapabilitiesV3(c *ClientCodecCapabilitiesV3, ctx *ClientPlaybackContextV3, features []string) ([]DegradationWarningV3, error) {
	if err := validateCapabilitiesStructureV3(c, ctx, features); err != nil {
		return nil, err
	}
	return normalizeCapabilitiesV3(c, ctx, features)
}

// validateDeliveryTransformationsStructureV3 is the non-mutating structural
// half of delivery-transformation handling. Structurally malformed or duplicate
// transformations are hard errors; nothing is rewritten or dropped here.
func validateDeliveryTransformationsStructureV3(delivery *DeliveryCapabilityV3) error {
	seenTransformations := make(map[string]struct{}, len(delivery.Transformations))
	for i := range delivery.Transformations {
		transformation := delivery.Transformations[i]
		name := strings.ToLower(strings.TrimSpace(transformation.Name))
		executor := strings.ToLower(strings.TrimSpace(transformation.Executor))
		recipeVersion := strings.TrimSpace(transformation.RecipeVersion)
		if name == "" || len(name) > 64 ||
			(executor != ExecutorClientV3 && executor != ExecutorServerV3) ||
			recipeVersion == "" || len(recipeVersion) > 32 ||
			len(transformation.ValidatedClaims) > 32 {
			return errors.New("invalid delivery transformation capability")
		}
		key := executor + ":" + name + ":" + recipeVersion
		if _, exists := seenTransformations[key]; exists {
			return errors.New("duplicate delivery transformation capability")
		}
		seenTransformations[key] = struct{}{}
		for _, claim := range transformation.ValidatedClaims {
			if len(claim) > 128 {
				return errors.New("transformation claim exceeds supported size")
			}
		}
	}
	return nil
}

// normalizeDeliveryTransformationsV3 normalizes one delivery's transformations
// and drops, with a degradation warning, a well-formed client transformation
// the merged feature list did not negotiate. The drop is deliberately a
// normalization step, not a validation step: a replan's structural validation
// must leave the transformation in place so this single call can rediscover it
// and report the warning.
func normalizeDeliveryTransformationsV3(delivery *DeliveryCapabilityV3, features []string) ([]TransformationV3, []DegradationWarningV3, error) {
	if err := validateDeliveryTransformationsStructureV3(delivery); err != nil {
		return nil, nil, err
	}
	kept := make([]TransformationV3, 0, len(delivery.Transformations))
	var warnings []DegradationWarningV3
	for i := range delivery.Transformations {
		transformation := delivery.Transformations[i]
		transformation.Name = strings.ToLower(strings.TrimSpace(transformation.Name))
		transformation.Executor = strings.ToLower(strings.TrimSpace(transformation.Executor))
		transformation.RecipeVersion = strings.TrimSpace(transformation.RecipeVersion)
		if transformation.Executor == ExecutorClientV3 &&
			(!delivery.Enabled || !delivery.SupportedOnDevice || !HasFeatureV3(features, FeatureClientVideoTransforms)) {
			warnings = append(warnings, DegradationWarningV3{
				Code:    "client_transformation_not_negotiated",
				Message: fmt.Sprintf("Client video transformation %s was not negotiated and is skipped.", transformation.Name),
			})
			continue
		}
		kept = append(kept, transformation)
	}
	return kept, warnings, nil
}

// CloneDeliveryCapabilitiesV3 returns a deep copy of a client's negotiated
// delivery map. A plain struct copy shares every backing slice, so a caller
// overlaying one request's deliveries onto a durable one must clone first:
// normalization writes those slices in place, and a mutation through the
// durable request must not reach the request that supplied them (or vice
// versa). A nil map clones to nil, preserving "no deliveries advertised".
func CloneDeliveryCapabilitiesV3(deliveries map[string]DeliveryCapabilityV3) map[string]DeliveryCapabilityV3 {
	if deliveries == nil {
		return nil
	}
	cloned := make(map[string]DeliveryCapabilityV3, len(deliveries))
	for name, delivery := range deliveries {
		cloned[name] = cloneDeliveryCapabilityV3(delivery)
	}
	return cloned
}

func cloneDeliveryCapabilityV3(delivery DeliveryCapabilityV3) DeliveryCapabilityV3 {
	delivery.Containers = append([]string(nil), delivery.Containers...)
	delivery.VideoCodecs = append([]string(nil), delivery.VideoCodecs...)
	delivery.AudioDecodeCodecs = append([]string(nil), delivery.AudioDecodeCodecs...)
	delivery.AudioPassthroughCodecs = append([]string(nil), delivery.AudioPassthroughCodecs...)
	delivery.Features = append([]string(nil), delivery.Features...)
	delivery.ValidatedClaims = append([]string(nil), delivery.ValidatedClaims...)
	delivery.HDRDetails = cloneHDRCapabilitiesV3(delivery.HDRDetails)
	if delivery.MaxChannels != nil {
		maxChannels := *delivery.MaxChannels
		delivery.MaxChannels = &maxChannels
	}
	delivery.Transformations = append([]TransformationV3(nil), delivery.Transformations...)
	for i := range delivery.Transformations {
		delivery.Transformations[i].ValidatedClaims = append([]string(nil), delivery.Transformations[i].ValidatedClaims...)
	}
	delivery.Subtitles.NativeEmbedded = append([]NativeEmbeddedSubtitleCapabilityV3(nil), delivery.Subtitles.NativeEmbedded...)
	for i := range delivery.Subtitles.NativeEmbedded {
		delivery.Subtitles.NativeEmbedded[i].Codecs = append([]string(nil), delivery.Subtitles.NativeEmbedded[i].Codecs...)
	}
	return delivery
}

func cloneHDRCapabilitiesV3(hdr *HDRCapabilitiesV3) *HDRCapabilitiesV3 {
	if hdr == nil {
		return nil
	}
	cloned := *hdr
	cloned.DolbyVisionProfiles = append([]int(nil), hdr.DolbyVisionProfiles...)
	cloned.DolbyVisionProfileLevels = append([]DolbyVisionProfileCapabilityV3(nil), hdr.DolbyVisionProfileLevels...)
	for i := range cloned.DolbyVisionProfileLevels {
		cloned.DolbyVisionProfileLevels[i].BLCompatibilityIDs = append([]int(nil), hdr.DolbyVisionProfileLevels[i].BLCompatibilityIDs...)
	}
	return &cloned
}

func validateHDRCapabilitiesV3(hdr *HDRCapabilitiesV3) error {
	if hdr == nil {
		return nil
	}
	if hdr.HDR10MaxWidth < 0 || hdr.HDR10MaxHeight < 0 || hdr.HDR10MaxFrameRate < 0 || hdr.HDR10MaxBitrateKbps < 0 {
		return errors.New("invalid hdr10 capability limit")
	}
	if !hdr.HDR10 && (hdr.HDR10MaxWidth > 0 || hdr.HDR10MaxHeight > 0 || hdr.HDR10MaxFrameRate > 0 || hdr.HDR10MaxBitrateKbps > 0) {
		return errors.New("hdr10 capability limits require hdr10 support")
	}
	if len(hdr.DolbyVisionProfiles) > 16 || len(hdr.DolbyVisionProfileLevels) > 16 {
		return errors.New("dolby vision profile list exceeds supported size")
	}
	seenProfiles := make(map[int]struct{}, len(hdr.DolbyVisionProfileLevels))
	for _, capability := range hdr.DolbyVisionProfileLevels {
		if capability.Profile <= 0 || capability.MaxLevel < 1 || capability.MaxLevel > 13 {
			return errors.New("invalid dolby vision profile level capability")
		}
		if _, exists := seenProfiles[capability.Profile]; exists {
			return errors.New("duplicate dolby vision profile level capability")
		}
		seenProfiles[capability.Profile] = struct{}{}
		if len(capability.BLCompatibilityIDs) > 16 {
			return errors.New("dolby vision base-layer compatibility list exceeds supported size")
		}
		seenCompatibilityIDs := make(map[int]struct{}, len(capability.BLCompatibilityIDs))
		for _, compatibilityID := range capability.BLCompatibilityIDs {
			if compatibilityID < 0 || compatibilityID > 15 {
				return errors.New("invalid dolby vision base-layer compatibility id")
			}
			if _, exists := seenCompatibilityIDs[compatibilityID]; exists {
				return errors.New("duplicate dolby vision base-layer compatibility id")
			}
			seenCompatibilityIDs[compatibilityID] = struct{}{}
		}
	}
	return nil
}

func validateTrackPairV3(fileID int, kind, id string, index *int) error {
	if len(id) > 128 {
		return fmt.Errorf("%s_track_id exceeds supported size", kind)
	}
	if index != nil && (*index < 0 || *index > 10_000) {
		return fmt.Errorf("%s_track_index is invalid", kind)
	}
	if id == "" || index == nil {
		return nil
	}
	want := TrackIDV3(fileID, kind, *index)
	if id != want {
		return fmt.Errorf("%s track id and index disagree", kind)
	}
	return nil
}

// StaleTrackIdentityV3 reports whether a well-formed selected track identity of
// kind is bound to a media file other than fileID. Virtual candidate rotation
// replaces the media_files row under a cached selection, so a client replaying
// an earlier plan can carry an identity minted against a rotated-out effective
// file; that identity is stale, not malformed. A malformed identity, an
// identity of another kind, or one bound to fileID itself is not stale.
func StaleTrackIdentityV3(kind string, fileID int, trackID string) bool {
	if fileID <= 0 || trackID == "" {
		return false
	}
	embeddedFileID, embeddedKind, _, ok := ParseTrackIDV3(trackID)
	return ok && embeddedKind == kind && embeddedFileID != fileID
}

// DropStaleSelectedTrackIdentitiesV3 clears the request's selected audio and
// subtitle identities when they name a file other than the requested file, and
// reports which kinds it cleared. A rotated-out selection degrades to the
// default track pipeline instead of failing the whole start; a same-file
// id/index disagreement is not stale and is left for the pair check to reject.
func (r *StartRequestV3) DropStaleSelectedTrackIdentitiesV3() (droppedAudio, droppedSubtitle bool) {
	if r == nil {
		return false, false
	}
	if StaleTrackIdentityV3("audio", r.FileID, r.AudioTrackID) {
		r.AudioTrackID = ""
		r.AudioTrackIndex = nil
		droppedAudio = true
	}
	if StaleTrackIdentityV3("subtitle", r.FileID, r.SubtitleTrackID) {
		r.SubtitleTrackID = ""
		r.SubtitleTrackIndex = nil
		droppedSubtitle = true
	}
	return droppedAudio, droppedSubtitle
}

func validateOptionalBoundedIntV3(v *int, min, max int, name string) error {
	if v != nil && (*v < min || *v > max) {
		return fmt.Errorf("%s is outside the supported range", name)
	}
	return nil
}

func isFiniteV3(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func HasFeatureV3(features []string, wanted string) bool {
	return slices.ContainsFunc(features, func(v string) bool { return strings.EqualFold(strings.TrimSpace(v), wanted) })
}

// AttemptStickyFeaturesV3 lists the client features that are negotiated once,
// at start, and are fixed for the lifetime of the playback attempt. Each of
// them selects a contract the durable plan and its live transport are built
// around rather than a per-plan preference:
//
//   - header_authenticated_media_v1 picks the media security contract. A legacy
//     signed URL from an earlier plan can outlive the plan that minted it, so
//     switching mid-attempt would leave two contracts alive for one session.
//   - authorized_media_origins_v1 selects which origins may serve the attempt's
//     media. The trust set a client honors must not change mid-attempt: a plan
//     that already handed out a proxy origin outlives the replan that would
//     revoke it, so the client would be left holding a URL it no longer trusts.
//   - software_video_decode_v1 widens the direct-play evidence tiers. Dropping
//     it on a replan silently converts a direct route into a transcode and
//     persists that downgrade into the durable normalized request.
//   - subrip_sidecar_v1 picks the representation of every SRT sidecar URL.
//     Switching it mid-attempt would publish one track under two URLs, and a
//     seek reanchor must reproduce the frozen plan's artifact exactly.
//
// Stop/start is the explicit boundary for changing any of them.
func AttemptStickyFeaturesV3() []string {
	return []string{FeatureHeaderAuthenticatedMediaV3, FeatureAuthorizedMediaOriginsV3, FeatureSoftwareVideoDecodeV3, FeatureSubripSidecarV3, FeatureDefaultAudioReconcileResponseV3}
}

// PinAttemptStickyFeaturesV3 returns requested with every attempt-sticky
// feature forced back to the state the start negotiation established: a replan
// can neither add nor remove one, whatever its own client_features list says.
// Non-sticky features are passed through untouched and in order.
func PinAttemptStickyFeaturesV3(requested, negotiated []string) []string {
	sticky := AttemptStickyFeaturesV3()
	pinned := make([]string, 0, len(requested)+len(sticky))
	for _, feature := range requested {
		if slices.ContainsFunc(sticky, func(candidate string) bool {
			return strings.EqualFold(strings.TrimSpace(feature), candidate)
		}) {
			continue
		}
		pinned = append(pinned, feature)
	}
	for _, feature := range sticky {
		if HasFeatureV3(negotiated, feature) {
			pinned = append(pinned, feature)
		}
	}
	return pinned
}

func NewTerminalResponseV3(reason, message string, retryable bool) DecisionResponseV3 {
	return DecisionResponseV3{
		ProtocolVersion: ProtocolV3,
		ServerFeatures:  ServerFeaturesV3(),
		Outcome:         OutcomeAdaptationUnavailableV3,
		Terminal:        &TerminalV3{Reason: reason, Message: message, Retryable: retryable},
	}
}

// NewTerminalResponseFromTerminalV3 builds a decision response from an existing
// terminal, preserving the diagnostic detail line.
func NewTerminalResponseFromTerminalV3(terminal *TerminalV3) DecisionResponseV3 {
	response := NewTerminalResponseV3(terminal.Reason, terminal.Message, terminal.Retryable)
	response.Terminal.Detail = terminal.Detail
	return response
}

func NewPlanExpiryV3(now time.Time) string { return now.Add(MaxTokenTTL).UTC().Format(time.RFC3339) }

// boundExceedsV3 reports whether an output ceiling is looser than the panel's.
// Zero means "no ceiling declared" on either side.
func boundExceedsV3(output, panel int) bool {
	return panel > 0 && (output == 0 || output > panel)
}

func boundExceedsFloatV3(output, panel float64) bool {
	return panel > 0 && (output == 0 || output > panel)
}
