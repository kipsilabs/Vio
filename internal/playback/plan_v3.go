package playback

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/tonemap"
)

type PlannerSettingsV3 struct {
	TranscodeEnabled       bool
	Allow4KTranscode       bool
	AllowHEVCEncoding      bool
	HardwareToneMapEnabled bool
	SoftwareToneMapEnabled bool
	// VPPToneMapEnabled selects the opt-in Intel media-engine HDR-to-SDR
	// conversion (playback.transcode_vpp_tone_map_enabled). It is honored only
	// when the transcode executes on QSV; every other backend keeps the
	// validated OpenCL/VAAPI/CUDA/VideoToolbox recipe.
	VPPToneMapEnabled bool
	// HWAccel is the configured hardware backend (playback.hw_accel). The
	// planner uses it to keep AV1 encodes on QSV, the only AV1 encoder whose
	// throughput is realtime-safe; HEVC still runs on every validated backend.
	HWAccel string
	// ViewerTranscodeDisabled reflects the viewer's account policy. Session
	// admission enforces it; the planner only uses it to stop advertising
	// quality rungs the viewer cannot start.
	ViewerTranscodeDisabled bool
}

const (
	TerminalMessage4KTranscodeDisabledV3   = "A lower-resolution source is required because 4K transcoding is disabled."
	containerMP4V3                         = "mp4"
	containerMKVV3                         = "mkv"
	containerHLSV3                         = "hls"
	mimeVideoMP4V3                         = "video/mp4"
	mimeVideoMSVideoV3                     = "video/x-msvideo"
	degradationAudioConvertedV3            = "audio_converted"
	audioCodecAACV3                        = "aac"
	codecCopyV3                            = "copy"
	serverAudioAdaptationReasonV3          = "server_audio_adaptation"
	decisionReasonAudioAdaptationV3        = "audio_adaptation"
	hlsAudioAdaptationReasonV3             = "hls_audio_adaptation"
	decisionReasonContainerNormalizationV3 = "container_normalization"
	audioLayoutMonoV3                      = "mono"
	audioLayoutStereoV3                    = "stereo"
	audioLayoutSurround51V3                = "5.1"
)

type PlannerInputV3 struct {
	Request StartRequestV3
	// ServerBitrateCapKbps is an administrator-owned, per-attempt ceiling.
	// Unlike a client's bandwidth preference it must fail closed.
	ServerBitrateCapKbps int
	RequestedFile        *models.MediaFile
	EffectiveFile        *models.MediaFile
	// LowerVersion names the version that serves lower qualities when the
	// requested version is 4K and 4K transcoding is disabled. Nil when the
	// item has no such version or the caller did not look for one.
	LowerVersion    *LowerVersionV3
	AudioTrackIndex int
	Settings        PlannerSettingsV3
	// Registry holds the transformations the local binary can execute.
	Registry *TransformationRegistryV3
	// ProgressiveRemuxRegistry, HLSRemuxRegistry, and HLSVideoRegistry report
	// the transformations executable by policy-eligible routes for each
	// delivery. They are lazy because building a registry can touch pooled
	// nodes. The planner only invokes the producer when that delivery needs a
	// server transformation, and the transport layer re-validates the selected
	// executor. HLSRegistry is the legacy shared fallback used when a caller
	// does not provide workload-specific HLS registries.
	ProgressiveRemuxRegistry func() *TransformationRegistryV3
	HLSRemuxRegistry         func() *TransformationRegistryV3
	HLSVideoRegistry         func() *TransformationRegistryV3
	HLSRegistry              func() *TransformationRegistryV3
	// ToneMapCapabilities and HLSToneMapCapabilities mirror Registry and
	// HLSVideoRegistry for executor variants of hdr_to_sdr_tonemap. The public
	// plan names one stable transformation; these internal capabilities select
	// a validated hardware or software implementation without exposing that
	// deployment policy to clients.
	ToneMapCapabilities    tonemap.Capabilities
	HLSToneMapCapabilities func() tonemap.Capabilities
	// DVRPUStrippable reports whether this particular source survives the
	// Dolby Vision RPU strip. The registries answer whether the executor
	// carries the transformation; this answers whether the file does, which
	// no capability probe can. Nil means "assume it does", preserving the
	// pre-probe behaviour for callers that cannot run one (the shadow
	// planner, tests). Lazy for the same reason as the HLS registries: it shells out
	// to ffmpeg, so it is consulted only once every cheap eligibility gate
	// has already passed and a strip route is genuinely on the table.
	DVRPUStrippable     func() bool
	Now                 time.Time
	AttemptedKeys       []string
	AdditionalSubtitles []SubtitleInventoryEntryV3
	// InventoryProvenance is the virtual resolver's provenance for the
	// effective source (see PlanV3.InventoryProvenance). The handler sets it
	// from the resolve result; it is empty for a local source. It is copied
	// onto the plan verbatim and never affects route selection.
	InventoryProvenance string
	// ForceSoftwareVideoDecode asks the planner to mark the resulting
	// server-transcode plan as a software-decode variant. It is set only by
	// reactive failure recovery after the live hardware decoder rejected the
	// source, so the plan identity and frozen recipe carry the decode mode and
	// a restart cannot put the failing hardware decoder back.
	ForceSoftwareVideoDecode bool
	// DecodeAttemptDetail is a diagnostic line naming the decode modes already
	// attempted when the planner exhausts a server-transcode route after a
	// decoder failure. It is surfaced on the terminal so `detail` is not empty.
	DecodeAttemptDetail string
}

// SourceExecutionMetadataV3 is the immutable source probe snapshot used to
// reopen a frozen playback recipe without consuming later catalog drift.
type SourceExecutionMetadataV3 struct {
	VideoCodec                 string
	VideoProfile               string
	VideoBitDepth              int
	SoftwareVideoDecode        bool
	DurationSeconds            float64
	ToneMapSourceKind          tonemap.SourceKind
	ToneMapPreflightRequired   bool
	ToneMapSourceRevision      tonemap.SourceRevision
	ToneMapDVConfigPresent     bool
	ToneMapDVBLCompatIDPresent bool
	ToneMapDVBLPresent         bool
	ToneMapDVRPUPresent        bool
}

// dvRPUStrippable resolves the per-source strip verdict, defaulting to true
// when no probe is wired in.
func (input PlannerInputV3) dvRPUStrippable() bool {
	return input.DVRPUStrippable == nil || input.DVRPUStrippable()
}

// hlsRegistry resolves the registry HLS deliveries gate on: the widened
// local∪node registry when provided, otherwise the local one. Callers must
// keep it behind short-circuits so transformation-free routes never force
// the lazy producer to run.
func (input PlannerInputV3) hlsRegistry() *TransformationRegistryV3 {
	if input.HLSRegistry != nil {
		if widened := input.HLSRegistry(); widened != nil {
			return widened
		}
	}
	return input.Registry
}

func (input PlannerInputV3) progressiveRemuxRegistry() *TransformationRegistryV3 {
	if input.ProgressiveRemuxRegistry != nil {
		if registry := input.ProgressiveRemuxRegistry(); registry != nil {
			return registry
		}
	}
	return input.Registry
}

func (input PlannerInputV3) hlsRemuxRegistry() *TransformationRegistryV3 {
	if input.HLSRemuxRegistry != nil {
		return input.HLSRemuxRegistry()
	}
	return input.hlsRegistry()
}

func (input PlannerInputV3) hlsVideoRegistry() *TransformationRegistryV3 {
	if input.HLSVideoRegistry != nil {
		return input.HLSVideoRegistry()
	}
	return input.hlsRegistry()
}

type PlannerResultV3 struct {
	Plan           *PlanV3
	Terminal       *TerminalV3
	PlayMethod     PlayMethod
	TranscodeAudio bool
	// RemuxResumeLeadingPictureDrop asks a progressive remux that starts past
	// zero to drop the open-GOP leading pictures macOS Firefox rejects. It is
	// best effort: an executor whose FFmpeg lacks the filter serves the plain
	// copy, so it never narrows where the route may run.
	RemuxResumeLeadingPictureDrop bool
	TargetVideoCodec              string
	TargetAudioCodec              string
	// SourceAudioChannels freezes the selected input track's channel count for
	// source-sensitive encode recipes such as multichannel-to-stereo downmixing.
	SourceAudioChannels int
	// TargetAudioChannels caps the transcode's re-encoded channel count;
	// 0 keeps the historical stereo downmix.
	TargetAudioChannels    int
	TargetAudioBitrateKbps int
	TargetResolution       string
	TargetBitrateKbps      int
	// SourceFrameRate and SourceHeight are the probed source video facts a
	// forced encode needs to keep its GOP aligned with the real frame rate and
	// to detect a no-op scale. They are frozen into the recipe card, stream
	// token, and transcode request so a restart reconstructs the same GOP.
	// Zero means unknown and decodes to the historical 30 fps assumption.
	SourceFrameRate             float64
	SourceHeight                int
	SubtitleTrackIndex          int
	SubtitleTransportTrackIndex int
	SubtitleBurnIn              bool
	SubtitleCodec               string
	// DownloadedSubtitleID comes from the same inventory snapshot used for
	// planning. Freezing must not re-list a mutable ordinal inventory after the
	// route has already been accepted.
	DownloadedSubtitleID int
	// FrozenSourceMetadata is set only when a durable executable recipe is
	// thawed for a seek reanchor. Transport construction must then use this
	// captured source snapshot instead of a freshly probed media row.
	FrozenSourceMetadata     *SourceExecutionMetadataV3
	ToneMapPolicy            tonemap.Policy
	ToneMapMode              tonemap.Mode
	ToneMapSourceKind        tonemap.SourceKind
	ToneMapRecipeVersion     string
	ToneMapPreflightRequired bool
	ToneMapSourceRevision    tonemap.SourceRevision
	// ToneMapVPPEnabled reports the opt-in VPP tone-map policy frozen into this
	// plan. The transport turns it into the VPP filter only when the resolved
	// executor is QSV; see prepareLocalTransportV3.
	ToneMapVPPEnabled bool
}

// hlsToneMapCapabilities resolves the lazy pooled inventory when present and
// otherwise uses the eagerly supplied local capability set.
func (input PlannerInputV3) hlsToneMapCapabilities() tonemap.Capabilities {
	if input.HLSToneMapCapabilities != nil {
		if capabilities := input.HLSToneMapCapabilities(); capabilities != nil {
			return capabilities
		}
	}
	return input.ToneMapCapabilities
}

// PlanPlaybackV3 chooses a playable protocol-v3 route from source and client facts.
func PlanPlaybackV3(input PlannerInputV3) (result PlannerResultV3) {
	if input.RequestedFile == nil {
		return terminalPlannerResultV3("source_unavailable", "The requested media source is unavailable.", false)
	}
	file := input.EffectiveFile
	if file == nil {
		file = input.RequestedFile
	}
	// A file ffprobe already rejected has nothing for any route to validate.
	// Answer before the bitrate-cap wrapper below, which would otherwise
	// rename the refusal after a policy that has nothing to do with it.
	if file.ProbeRejected() {
		return terminalPlannerResultV3(TerminalSourceUnreadableV3, TerminalSourceUnreadableMessageV3, false)
	}
	if input.Now.IsZero() {
		input.Now = time.Now()
	}
	source := SourceDescriptorFromFileV3(file, input.AudioTrackIndex)
	serverBitrateRequiresEncode := false
	if input.ServerBitrateCapKbps > 0 {
		if cap := optionalValueV3(input.Request.BandwidthCapKbps); cap == 0 || input.ServerBitrateCapKbps < cap {
			input.Request.BandwidthCapKbps = &input.ServerBitrateCapKbps
		}
		// The video track rate excludes audio and container overhead. Only a
		// known total rate can establish that a source-preserving route fits.
		cap := optionalValueV3(input.Request.BandwidthCapKbps)
		totalBitrateKbps := normalizeBitrateKbpsV3(file.Bitrate)
		serverBitrateRequiresEncode = totalBitrateKbps <= 0 || totalBitrateKbps > cap || source.BitrateKbps > cap
		if serverBitrateRequiresEncode {
			defer func() {
				compliant := result.Plan != nil &&
					(result.PlayMethod == PlayTranscode || file.IsAudioOnly() && result.TranscodeAudio)
				if result.Terminal != nil || !compliant {
					// Keep a transient blocker (such as a missing toolchain)
					// retryable after naming the bitrate limit as the refusal.
					retryable := result.Terminal != nil && result.Terminal.Retryable
					result = terminalPlannerResultV3(TerminalBitratePolicyUnavailableV3, "This stream exceeds the server bitrate limit, and no compliant transcoding route is available.", retryable)
				}
			}()
		}
	}
	// A source without any video track is audio-only (audiobooks, future
	// music): the video, HDR, and subtitle-burn gates below have nothing to
	// gate, and requiring complete video metadata would terminal a perfectly
	// playable file. It gets its own reduced route family instead.
	if file.IsAudioOnly() {
		return planAudioOnlyV3(input, file, source)
	}
	// Subtitle renderability is delivery-specific, so every candidate route is
	// validated against the capabilities of the delivery class that would
	// execute it. A refusal on original_http must not suppress a viable
	// progressive or HLS subtitle renderer.
	subtitle := ResolveSubtitlePolicyV3(file, input.Request, input.Settings.TranscodeEnabled, DeliveryClassOriginalHTTPV3, input.AdditionalSubtitles)
	remuxSubtitle := ResolveSubtitlePolicyV3(file, input.Request, input.Settings.TranscodeEnabled, DeliveryClassProgressiveV3, input.AdditionalSubtitles)
	hlsSubtitle := ResolveSubtitlePolicyV3(file, input.Request, input.Settings.TranscodeEnabled, DeliveryClassHLSV3, input.AdditionalSubtitles)
	if subtitle.Terminal != nil && remuxSubtitle.Terminal != nil && hlsSubtitle.Terminal != nil {
		return PlannerResultV3{Terminal: subtitle.Terminal, SubtitleTrackIndex: -1, SubtitleTransportTrackIndex: -1}
	}
	// Every policy addresses the same selected source track. Preserve that
	// identity even when the original delivery's refusal carries no selection;
	// each candidate still applies its own rendering decision and claims.
	selectedSubtitle := subtitle
	if selectedSubtitle.Terminal != nil {
		selectedSubtitle = remuxSubtitle
		if selectedSubtitle.Terminal != nil {
			selectedSubtitle = hlsSubtitle
		}
	}
	// A remux route cannot burn subtitles, so it is only viable when its own
	// delivery can present the selected subtitle without one.
	originalSubtitleOK := subtitle.Terminal == nil && !subtitle.RequiresBurn
	remuxSubtitleOK := remuxSubtitle.Terminal == nil && !remuxSubtitle.RequiresBurn
	hlsRemuxSubtitleOK := hlsSubtitle.Terminal == nil && !hlsSubtitle.RequiresBurn
	quality := ResolveQualityPolicyV3(input.Request, source)
	if serverBitrateRequiresEncode {
		// The quality resolver uses the video track rate to choose an encode
		// target, which can undercount a source-preserving route's total rate.
		quality.RequiresTranscode = true
		quality.PreservesSource = false
		cap := optionalValueV3(input.Request.BandwidthCapKbps)
		if quality.BitrateKbps <= 0 {
			quality.BitrateKbps = cap
		} else {
			quality.BitrateKbps = min(quality.BitrateKbps, cap)
		}
	}
	videoOK, videoEvidenceInsufficient := videoEligibleV3(source, input.Request)
	var high10Quirk *AppliedQuirkV3
	if !videoOK {
		if quirk, ok := high10DecodeOverrideV3(source, input.Request); ok {
			videoOK = true
			high10Quirk = quirk
		}
	}
	rangeOK, videoClaims := outputRangeEligibleV3(source, input.Request)
	clientManagedRange := clientManagesOriginalDynamicRangeV3(source, input.Request)
	clientDV8BaseLayerOK, clientDV8BaseRange := clientDV8BaseLayerFallbackV3(source, input.Request)
	originalRangeOK := rangeOK || clientManagedRange || clientDV8BaseLayerOK
	audioOK, passthrough, audioClaims := audioEligibilityV3(source, input.Request)
	originalAudioSelectionOK := audioSelectionUsesContainerDefaultV3(file, input.AudioTrackIndex) ||
		clientSelectsOriginalAudioTrackV3(input.Request)
	noAudioTrack := source.AudioCodec == "" && (file == nil || len(file.AudioTracks) == 0)
	if !audioOK && noAudioTrack {
		// Video-only media has no audio stream to adapt: treating the absence
		// as an unsupported codec would force a pointless AAC conversion — or
		// a terminal when conversion is unavailable — on a playable file. An
		// audio track whose codec merely failed to probe keeps the codec
		// gate: converting unknown audio is safer than copying it.
		audioOK = true
		audioClaims.Reason = "no_audio_track"
	}
	// Apple AVPlayer clients (tvOS, iOS, macOS) advertise "mkv" in their
	// container list but AVPlayer cannot play raw Matroska over HTTP progressive.
	// Detect and correct this before containerOK is evaluated so the planner
	// selects a remux route instead of returning an unplayable direct stream.
	// The quirk is recorded on base below, after base is initialized.
	mkvQuirk, mkvQuirkFired := appleAVPlayerMKVContainerFallback(source, input.Request)
	effectiveContainers := input.Request.Capabilities.Containers
	if mkvQuirkFired {
		effectiveContainers = filterContainersV3(effectiveContainers, "mkv", "matroska")
	}
	containerOK := containsFoldV3(effectiveContainers, source.Container)
	hlsDeliveryOK := deliveryAvailableV3(input.Request, DeliveryClassHLSV3)
	// DV strip eligibility is split by delivery because progressive proxy nodes
	// and HLS transcode nodes are different executor pools. Keep each verdict
	// scoped so one pool cannot authorize a recipe that only the other can run.
	dvStripEligibleProgressive := false
	dvStripEligibleHLS := false
	dvStripPlausible := source.DynamicRange == DynamicRangeDolbyVisionV3 &&
		clientSupportsHDR10V3(input.Request, source) &&
		(source.DVProfile == 7 || source.DVProfile == 8 && source.DVBLCompatID == 1)
	if dvStripPlausible {
		if deliveryAvailableV3(input.Request, DeliveryClassProgressiveV3) {
			dvStripEligibleProgressive = canStripDolbyVisionToHDR10V3(source, input.Request, input.progressiveRemuxRegistry())
		}
		if hlsDeliveryOK {
			dvStripEligibleHLS = canStripDolbyVisionToHDR10V3(source, input.Request, input.hlsRemuxRegistry())
		}
	}
	dvStripEligible := dvStripEligibleProgressive || dvStripEligibleHLS
	// A source whose RPU ffmpeg cannot parse must lose the strip here rather
	// than at the transport, so that the plan's HDR10 promise, the durable
	// session's RemuxDVMode and every restart derived from it stay consistent
	// with what the pipeline can actually produce. Ordered last: the probe
	// only runs once an executor has been found for a strip this client wants.
	dvStripUnsupportedBySource := false
	if dvStripEligible && !input.dvRPUStrippable() {
		dvStripUnsupportedBySource = true
		dvStripEligible = false
		dvStripEligibleProgressive = false
		dvStripEligibleHLS = false
	}
	clientDV81Eligible := canClientTransformDV7ToDV81V3(source, input.Request)
	clientHDR10Eligible := canClientTransformDV7ToHDR10V3(source, input.Request)
	// With the server strip gone, a client that cannot take the source range
	// (either natively or by managing it itself) and cannot run its own DV
	// transformation is out of source-preserving routes. Terminate only when no
	// enabled tone-map route remains either: a validated HDR-to-SDR executor can
	// still decode the compatible base layer without preserving the Dolby Vision
	// metadata. Without one, every remaining branch funnels into
	// planVideoTranscodeV3's hdr_transcode_unsupported, so terminate here
	// instead — the client is then told the actual cause, a source whose Dolby
	// Vision metadata cannot be removed, rather than a generic HDR message that
	// sends the user looking for a missing encoder.
	if dvStripUnsupportedBySource && !originalRangeOK && !clientDV81Eligible && !clientHDR10Eligible {
		_, _, _, _, toneMapEligible := toneMapRecipeV3(input, source)
		if !toneMapEligible || !videoTranscodeExecutableV3(input, source) {
			return terminalPlannerResultV3(TerminalDVConversionUnsupportedV3,
				"This source's Dolby Vision metadata cannot be removed cleanly, and this device cannot play the source as it is.", false)
		}
	}

	base := PlanV3{
		ProtocolVersion:        ProtocolV3,
		ExpiresAt:              NewPlanExpiryV3(input.Now),
		SelectedTracks:         selectedTracksForPlanV3(file, input.AudioTrackIndex, selectedSubtitle),
		EffectiveRecipe:        recipeFromSourceV3(source),
		Claims:                 ValidationClaimsV3{Video: videoClaims, Audio: audioClaims, Subtitles: subtitle.Claims},
		Subtitle:               subtitle.Decision,
		Transformations:        []TransformationV3{},
		AppliedQuirks:          []AppliedQuirkV3{},
		RuntimeCorrections:     []string{},
		DegradationWarnings:    []DegradationWarningV3{},
		RequestedMediaFileID:   input.RequestedFile.ID,
		EffectiveMediaFileID:   file.ID,
		EffectiveVirtualURI:    effectiveVirtualURIV3(input),
		VirtualSourceRevision:  virtualSourceRevisionV3(input),
		Source:                 source,
		SubtitleFidelityPolicy: subtitlePolicyNameV3(input.Request.SubtitleFidelityPreference),
		Timeline:               TimelineV3{SourceStartSeconds: floatOrZeroV3(input.Request.StartPosition), PlayerStartSeconds: floatOrZeroV3(input.Request.StartPosition), CanSeekAnywhere: true, SeekRestoration: "player_position"},
	}
	base.AvailableQualities = availableQualitiesV3(input, source)
	base.Subtitle.Inventory = BuildSubtitleInventoryV3(file, input.AdditionalSubtitles)
	// Authoritative per-track audio inventory of the effective source; clients
	// should prefer it over item metadata, which can be stale after a version
	// fallback rehydrates a different candidate under the same catalog row.
	base.AudioTracks = audioInventoryV3(file)
	invStatus := "declared"
	if file != nil && file.ProbeUpdatedAt != nil {
		invStatus = "verified"
	}
	base.InventoryStatus = invStatus
	base.InventoryRevision = ComputeInventoryRevisionV3(invStatus, base.AudioTracks, base.Subtitle.Inventory)
	// The resolver's own provenance for the effective virtual source, distinct
	// from the coarse stamp-derived status above. Empty for a local source.
	base.InventoryProvenance = input.InventoryProvenance
	base.Claims.Audio.Passthrough = passthrough
	if mkvQuirkFired {
		appendAppliedQuirkV3(&base, *mkvQuirk, "")
	}
	if subtitle.Degraded {
		base.DegradationWarnings = append(base.DegradationWarnings, SubtitleTrackUnavailableWarningV3())
	}
	if source.DynamicRange == DynamicRangeHDRUnknownV3 && (rangeOK || clientManagedRange) {
		base.DegradationWarnings = append(base.DegradationWarnings, DegradationWarningV3{
			Code:    "hdr_range_assumed_hdr10",
			Message: "The source is flagged HDR without precise range metadata; playback treats it as HDR10 unless the client resolves a more precise presentation.",
		})
	}
	if dvStripUnsupportedBySource {
		// Say why the HDR10 route this client is capable of was not taken;
		// otherwise the fallback looks like an unexplained quality drop.
		base.DegradationWarnings = append(base.DegradationWarnings, DegradationWarningV3{
			Code:    "dolby_vision_strip_unsupported_by_source",
			Message: "This source's Dolby Vision metadata cannot be removed cleanly, so the validated HDR10 route is unavailable for it.",
		})
	}
	if !routeVideoMetadataCompleteV3(source) {
		return terminalPlannerResultV3WithDetail("source_metadata_incomplete", "The source is missing video metadata required for a validated playback route.", routeVideoMetadataGapsDetailV3(source), true)
	}
	if !videoOK && videoEvidenceInsufficient {
		// The client's flat codec lists claim this stream, but its evidence
		// tier could not validate it for a direct route. Distinguish that from
		// a device that genuinely cannot play the stream so lower-tier clients
		// see an actionable degradation instead of a mystery transcode.
		base.DegradationWarnings = append(base.DegradationWarnings, DegradationWarningV3{
			Code:    EvidenceInsufficientForDirectV3,
			Message: "The client's capability evidence tier cannot validate this stream for a direct route; an adapted route is used instead.",
		})
	}

	// Automatic quality reductions (device resolution limit, bandwidth
	// estimate/cap, metered fallback) are best-effort. When the only reason to
	// transcode is such a reduction, a validated source-preserving route
	// exists, and the transcode itself cannot execute (HDR sources have no
	// validated reduced-quality recipe yet, or the client/server lacks the
	// transcode route entirely), deliver the source at original quality with a
	// degradation warning instead of refusing playback. Explicit user-selected
	// rungs keep the existing terminals.
	if input.ServerBitrateCapKbps == 0 && quality.RequiresTranscode && !quality.ExplicitRung && (originalSubtitleOK || remuxSubtitleOK || hlsRemuxSubtitleOK) && videoOK &&
		(originalRangeOK || dvStripEligible || clientDV81Eligible || clientHDR10Eligible) &&
		!videoTranscodeExecutableV3(input, source) {
		warnings := append(quality.Warnings, DegradationWarningV3{
			Code:    "quality_reduction_unavailable",
			Message: "Reduced-quality transcoding is unavailable for this source; it is delivered at original quality.",
		})
		quality = originalQualityResultV3(source)
		quality.Warnings = warnings
	}
	base.DegradationWarnings = append(base.DegradationWarnings, quality.Warnings...)

	if quality.RequiresTranscode || !videoOK ||
		(!originalRangeOK && !dvStripEligible && !clientDV81Eligible && !clientHDR10Eligible) ||
		(!originalSubtitleOK && !remuxSubtitleOK && !hlsRemuxSubtitleOK) {
		reasonOverride := ""
		if !quality.RequiresTranscode && !videoOK && videoEvidenceInsufficient {
			// The only reason this route adapts is the evidence tier, not a
			// negative device fact; name that in the decision and in any
			// resulting terminal.
			reasonOverride = EvidenceInsufficientForDirectV3
		}
		// True when the burn requirement is the sole disjunct that fired: every
		// other route condition still permits a source-preserving delivery.
		subtitleForcedAdaptation := !quality.RequiresTranscode && videoOK &&
			(originalRangeOK || dvStripEligible || clientDV81Eligible || clientHDR10Eligible) &&
			!originalSubtitleOK && !remuxSubtitleOK && !hlsRemuxSubtitleOK
		return planVideoTranscodeV3(input, base, source, quality, hlsSubtitle, reasonOverride, subtitleForcedAdaptation)
	}

	// Profile 7 is normalized on the client against the original range-capable
	// source. A decoder profile/max-instance claim alone is not proof of native
	// dual-layer output, so the default Android route mirrors Silo Apple: P8.1
	// base-layer Dolby Vision first, then same-file HDR10.
	if source.DVProfile == 7 && quality.PreservesSource && videoOK && containerOK && audioOK && originalAudioSelectionOK && originalSubtitleOK {
		if clientDV81Eligible {
			plan := base
			plan.Delivery = DeliveryOriginalHTTPV3
			plan.Stream = StreamV3{Protocol: StreamHTTPProgressiveV3, Container: source.Container, MIMEType: MimeFromExtension(file.FilePath), Headers: map[string]string{}, HeaderRefresh: HeaderRefreshNoneV3}
			plan.DecisionReason = "client_dv7_to_dv81"
			plan.EffectiveRecipe.DynamicRange = DynamicRangeDolbyVisionV3
			plan.Claims.Video = VideoClaimsV3{DolbyVision: true, DolbyVisionReason: "client_profile7_to_profile81"}
			plan.Transformations = append(plan.Transformations, TransformationV3{
				Name: ClientDV7ToDV81V3, Executor: ExecutorClientV3, RecipeVersion: ClientDVTransformVersionV3,
				ValidatedClaims: []string{"profile7_rpu_converted_to_profile81", "hdr10_base_layer_preserved", "enhancement_layer_discarded"},
			})
			plan.DegradationWarnings = append(plan.DegradationWarnings, DegradationWarningV3{
				Code:    "dolby_vision_enhancement_layer_discarded",
				Message: "Dolby Vision Profile 7 is played as Profile 8.1 base-layer Dolby Vision; enhancement-layer pixel data is discarded.",
			})
			finalizePlanIdentityV3(&plan, input.Request.PlaybackAttemptID, input.Request.ClientPlaybackContext.Output.OutputContextID)
			if deliverySupportsPlanV3(input.Request, DeliveryClassOriginalHTTPV3, plan) && !planAttemptedV3(plan, input.Request.ClientPlaybackContext.Output.OutputContextID, input.AttemptedKeys) {
				return PlannerResultV3{Plan: &plan, PlayMethod: PlayDirect, SubtitleTrackIndex: subtitle.SelectedIndex, SubtitleTransportTrackIndex: subtitle.TransportIndex, SubtitleCodec: subtitle.Codec, DownloadedSubtitleID: subtitle.DownloadedSubtitleID}
			}
		}
		if clientHDR10Eligible {
			plan := base
			plan.Delivery = DeliveryOriginalHTTPV3
			plan.Stream = StreamV3{Protocol: StreamHTTPProgressiveV3, Container: source.Container, MIMEType: MimeFromExtension(file.FilePath), Headers: map[string]string{}, HeaderRefresh: HeaderRefreshNoneV3}
			plan.DecisionReason = "client_dv7_to_hdr10"
			plan.EffectiveRecipe.DynamicRange = DynamicRangeHDR10V3
			plan.Claims.Video = VideoClaimsV3{HDR10: true}
			plan.Transformations = append(plan.Transformations, TransformationV3{
				Name: ClientDV7ToHDR10V3, Executor: ExecutorClientV3, RecipeVersion: ClientDVTransformVersionV3,
				ValidatedClaims: DV7ToHDR10ClaimsV3(),
			})
			plan.DegradationWarnings = append(plan.DegradationWarnings, DegradationWarningV3{
				Code:    "dolby_vision_removed",
				Message: "Dolby Vision Profile 7 is played from the same 4K file as its HDR10 base layer.",
			})
			finalizePlanIdentityV3(&plan, input.Request.PlaybackAttemptID, input.Request.ClientPlaybackContext.Output.OutputContextID)
			if deliverySupportsPlanV3(input.Request, DeliveryClassOriginalHTTPV3, plan) && !planAttemptedV3(plan, input.Request.ClientPlaybackContext.Output.OutputContextID, input.AttemptedKeys) {
				return PlannerResultV3{Plan: &plan, PlayMethod: PlayDirect, SubtitleTrackIndex: subtitle.SelectedIndex, SubtitleTransportTrackIndex: subtitle.TransportIndex, SubtitleCodec: subtitle.Codec, DownloadedSubtitleID: subtitle.DownloadedSubtitleID}
			}
		}
		if clientManagedRange {
			plan := base
			plan.Delivery = DeliveryOriginalHTTPV3
			plan.Stream = StreamV3{Protocol: StreamHTTPProgressiveV3, Container: source.Container, MIMEType: MimeFromExtension(file.FilePath), Headers: map[string]string{}, HeaderRefresh: HeaderRefreshNoneV3}
			plan.DecisionReason = decisionReasonClientManagedDynamicRangeV3
			finalizePlanIdentityV3(&plan, input.Request.PlaybackAttemptID, input.Request.ClientPlaybackContext.Output.OutputContextID)
			if deliverySupportsPlanV3(input.Request, DeliveryClassOriginalHTTPV3, plan) && !planAttemptedV3(plan, input.Request.ClientPlaybackContext.Output.OutputContextID, input.AttemptedKeys) {
				return PlannerResultV3{Plan: &plan, PlayMethod: PlayDirect, SubtitleTrackIndex: subtitle.SelectedIndex, SubtitleTransportTrackIndex: subtitle.TransportIndex, SubtitleCodec: subtitle.Codec, DownloadedSubtitleID: subtitle.DownloadedSubtitleID}
			}
		}
	}

	if source.DVProfile != 7 && deliveryAvailableV3(input.Request, DeliveryClassOriginalHTTPV3) && containerOK && videoOK && originalRangeOK && audioOK && originalAudioSelectionOK && quality.PreservesSource && originalSubtitleOK {
		plan := base
		plan.Delivery = DeliveryOriginalHTTPV3
		plan.Stream = StreamV3{Protocol: StreamHTTPProgressiveV3, Container: source.Container, MIMEType: MimeFromExtension(file.FilePath), Headers: map[string]string{}, HeaderRefresh: HeaderRefreshNoneV3}
		plan.DecisionReason = "validated_original_playback"
		// A native route always wins when the delivery can actually carry
		// it. When the original_http capability itself refuses the native
		// Dolby Vision plan (an HDR10-only executor on a DV-capable output),
		// fall through to the base-layer route rather than adapting.
		nativeDeliverable := rangeOK
		if rangeOK && clientDV8BaseLayerOK {
			// The probe must carry the same copied-video quirks as the real
			// native candidate, or its attempt key differs and a replan after
			// a native failure re-selects native instead of falling through.
			nativeProbe := plan
			applyCopiedVideoQuirksV3(&nativeProbe, source, input.Request, high10Quirk)
			finalizePlanIdentityV3(&nativeProbe, input.Request.PlaybackAttemptID, input.Request.ClientPlaybackContext.Output.OutputContextID)
			nativeDeliverable = deliverySupportsPlanV3(input.Request, DeliveryClassOriginalHTTPV3, nativeProbe) &&
				!planAttemptedV3(nativeProbe, input.Request.ClientPlaybackContext.Output.OutputContextID, input.AttemptedKeys)
		}
		switch {
		case nativeDeliverable:
		case clientDV8BaseLayerOK:
			// Same bytes, ordinary HEVC decoder, base layer presented. The
			// recipe names the range that actually reaches the output so the
			// per-delivery HDR gate below and the attempt key both see the
			// base range, and the plan never claims Dolby Vision.
			plan.DecisionReason = decisionReasonClientDV8BaseLayerV3
			plan.EffectiveRecipe.DynamicRange = clientDV8BaseRange
			plan.Claims.Video = VideoClaimsV3{
				HDR10:             clientDV8BaseRange == DynamicRangeHDR10V3,
				HLG:               clientDV8BaseRange == DynamicRangeHLGV3,
				DolbyVision:       false,
				DolbyVisionReason: "base_layer_compatible_hevc",
			}
			plan.DegradationWarnings = append(plan.DegradationWarnings, DegradationWarningV3{
				Code:    "dolby_vision_base_layer_only",
				Message: "This output does not carry Dolby Vision; the file plays unchanged through an HEVC decoder as its " + strings.ToUpper(clientDV8BaseRange) + " base layer and the Dolby Vision metadata is not presented.",
			})
		case clientManagedRange:
			plan.DecisionReason = decisionReasonClientManagedDynamicRangeV3
		}
		applyCopiedVideoQuirksV3(&plan, source, input.Request, high10Quirk)
		finalizePlanIdentityV3(&plan, input.Request.PlaybackAttemptID, input.Request.ClientPlaybackContext.Output.OutputContextID)
		if deliverySupportsPlanV3(input.Request, DeliveryClassOriginalHTTPV3, plan) && !planAttemptedV3(plan, input.Request.ClientPlaybackContext.Output.OutputContextID, input.AttemptedKeys) {
			return PlannerResultV3{Plan: &plan, PlayMethod: PlayDirect, SubtitleTrackIndex: subtitle.SelectedIndex, SubtitleTransportTrackIndex: subtitle.TransportIndex, SubtitleCodec: subtitle.Codec, DownloadedSubtitleID: subtitle.DownloadedSubtitleID}
		}
	}

	// A progressive remux maps only the base-layer video stream, so dual-layer
	// Profile 7 can never ship as native Dolby Vision here regardless of the
	// client's decoder claims; the validated HDR10 strip is the only eligible
	// P7 remux recipe.
	remuxRangeOK := rangeOK && source.DVProfile != 7
	// A copy-unsafe source (H.264 with conflicting in-band PPS) must not take a
	// video stream-copy route: the avc1/fMP4 segment would desync strict
	// decoders. Skipping the remux branch drops through to the HLS transcode.
	if videoOK && !source.VideoCopyUnsafe && (remuxRangeOK || dvStripEligible) && (remuxSubtitleOK || hlsRemuxSubtitleOK) {
		// A source with more channels than the delivery's ceiling cannot be
		// copied there even when the codec is supported. Adapt only the audio
		// so the video stays a copy instead of falling through to a transcode.
		progressiveChannelLimited := !noAudioTrack && deliveryExceedsMaxChannelsV3(input.Request, DeliveryClassProgressiveV3, source.AudioChannels)
		hlsChannelLimited := !noAudioTrack && deliveryExceedsMaxChannelsV3(input.Request, DeliveryClassHLSV3, source.AudioChannels)
		progressiveCodecAudioOK := noAudioTrack || deliverySupportsAudioClaimV3(input.Request, DeliveryClassProgressiveV3, source.AudioCodec, audioClaims, audioOK)
		hlsCodecAudioOK := noAudioTrack || hlsNativeAudioCodecV3(source.AudioCodec) &&
			deliverySupportsAudioClaimV3(input.Request, DeliveryClassHLSV3, source.AudioCodec, audioClaims, audioOK)
		// AAC frames in Matroska use a millisecond packet clock while each frame
		// contains 1024 samples. Copying those rounded timestamps into MP4/fMP4
		// produces real sub-frame gaps and overlaps that Firefox renders as
		// crackle. Keep video-copy remuxing, but re-encode the selected AAC track
		// through the versioned timestamp-normalization recipe. Native original
		// playback above remains byte-for-byte direct play.
		firefoxAACTimingQuirk, normalizeMatroskaAAC := firefoxMatroskaAACTimingQuirkV3(source, input.Request)
		progressiveCodecTranscodeAudio := !progressiveCodecAudioOK || normalizeMatroskaAAC
		progressiveTranscodeAudio := progressiveCodecTranscodeAudio || progressiveChannelLimited
		hlsCodecTranscodeAudio := !hlsCodecAudioOK || normalizeMatroskaAAC
		hlsTranscodeAudio := hlsCodecTranscodeAudio || hlsChannelLimited
		hlsAudioQuirk, hlsAudioQuirkOK := hlsEAC3AudioCorrectionV3(source, input.Request)
		// The device quirk owns the conversion of a codec HLS could otherwise
		// copy, including one that only exceeds the channel ceiling, so its
		// validated stereo recipe and applied-quirk record are kept.
		hlsQuirkConvertsAudio := hlsAudioQuirkOK && !hlsCodecTranscodeAudio
		hlsAACChannels := 0
		switch {
		case hlsQuirkConvertsAudio:
			hlsAACChannels = aacOutputChannelsV3(input.Request, DeliveryClassHLSV3, source.AudioChannels, false)
		case hlsTranscodeAudio:
			// HLS packaging cannot safely copy non-native codecs such as DTS,
			// TrueHD, or Opus. Preserve surround when adapting those codecs,
			// and keep as many channels as a channel ceiling allows; a native
			// codec rejected by the scoped client claim keeps the normal
			// compatibility downmix policy.
			hlsAACChannels = aacOutputChannelsV3(input.Request, DeliveryClassHLSV3, source.AudioChannels, hlsChannelLimited || !hlsNativeAudioCodecV3(source.AudioCodec))
		}
		progressiveAudioConvertOK := false
		if progressiveTranscodeAudio && deliveryAvailableV3(input.Request, DeliveryClassProgressiveV3) {
			progressiveAudioConvertOK = input.progressiveRemuxRegistry().Available(TransformationAudioToAACV3)
		}
		if progressiveCodecTranscodeAudio && hlsCodecTranscodeAudio {
			// Each delivery consults only its own eligible executor pool. A
			// progressive proxy may run the conversion without implying that an
			// HLS transcode node can, and vice versa. A conversion forced only by
			// a channel ceiling is not terminal: the video transcode below can
			// still downmix through its own executor pool.
			audioConvertOK := progressiveAudioConvertOK ||
				hlsDeliveryOK && input.hlsRemuxRegistry().Available(TransformationAudioToAACV3)
			// A missing AAC toolchain is the cause only when an eligible remux
			// delivery actually needs the conversion. Both conversion flags
			// above are set whenever audio adaptation is required, so the guard
			// is just "does any remux delivery remain available": when neither
			// delivery exists the failure is delivery exhaustion, and falling
			// through reports that real cause instead of a misleading encoder
			// verdict.
			if !audioConvertOK && (deliveryAvailableV3(input.Request, DeliveryClassProgressiveV3) || hlsDeliveryOK) {
				return terminalPlannerResultV3(TerminalAudioConversionUnsupportedV3, "The required validated AAC conversion toolchain is unavailable.", true)
			}
		}
		// Native Dolby Vision and the HDR10 strip are separate remux recipes. A
		// Profile 8.1 source the output can take natively tries native DV
		// first; once every native-DV remux has been attempted on this output
		// route, the strip is the next source-preserving recipe, ahead of any
		// transcode. Profile 7 and outputs without the source range only ever
		// take the strip.
		dvStripModes := []bool{dvStripEligible && (source.DVProfile == 7 || !rangeOK)}
		if !dvStripModes[0] && dvStripEligible {
			dvStripModes = append(dvStripModes, true)
		}
		hlsAudioConversionUnavailable := ""
		for _, dvStrip := range dvStripModes {
			hlsTranscodeAudio := hlsTranscodeAudio
			remuxBase := cloneRemuxPlanCandidateV3(base)
			if dvStrip {
				remuxBase.Transformations = append(remuxBase.Transformations, TransformationV3{Name: TransformationServerDV7HDR10V3, Executor: ExecutorServerV3, RecipeVersion: TransformationServerDV7HDR10RecipeVersionV3, ValidatedClaims: DV7ToHDR10ClaimsV3()})
				remuxBase.EffectiveRecipe.DynamicRange = DynamicRangeHDR10V3
				remuxBase.Claims.Video = VideoClaimsV3{HDR10: true}
				remuxBase.DegradationWarnings = append(remuxBase.DegradationWarnings, DegradationWarningV3{Code: "dolby_vision_removed", Message: "Dolby Vision metadata is removed and the validated HDR10 base layer is preserved."})
			}

			progressivePlan := cloneRemuxPlanCandidateV3(remuxBase)
			progressivePlan.Delivery = DeliveryRemuxProgressiveV3
			progressivePlan.Stream = StreamV3{Protocol: StreamHTTPProgressiveV3, Container: containerMP4V3, MIMEType: mimeVideoMP4V3, Headers: map[string]string{}, HeaderRefresh: HeaderRefreshNoneV3}
			progressivePlan.DecisionReason = decisionReasonContainerNormalizationV3
			progressiveAudioChannels := 0
			if progressiveTranscodeAudio && progressiveAudioConvertOK {
				progressiveAudioChannels = aacOutputChannelsV3(input.Request, DeliveryClassProgressiveV3, source.AudioChannels, progressiveChannelLimited)
				progressivePlan.EffectiveRecipe.AudioCodec = audioCodecAACV3
				progressivePlan.EffectiveRecipe.AudioChannels = intPointerV3(progressiveAudioChannels)
				progressivePlan.EffectiveRecipe.AudioLayout = audioLayoutForChannelsV3(progressiveAudioChannels)
				progressivePlan.Claims.Audio = AudioClaimsV3{Codec: audioCodecAACV3, Reason: serverAudioAdaptationReasonV3}
				progressivePlan.Transformations = append(progressivePlan.Transformations, TransformationV3{Name: TransformationAudioToAACV3, Executor: ExecutorServerV3, RecipeVersion: TransformationAudioToAACRecipeVersionV3, ValidatedClaims: []string{ClaimAudioDecodeV3}})
				progressivePlan.DegradationWarnings = append(progressivePlan.DegradationWarnings, DegradationWarningV3{Code: degradationAudioConvertedV3, Message: fmt.Sprintf("The selected audio track is converted to AAC %s.", audioLayoutForChannelsV3(progressiveAudioChannels))})
				progressivePlan.DecisionReason = decisionReasonAudioAdaptationV3
			}
			if normalizeMatroskaAAC {
				appendAppliedQuirkV3(&progressivePlan, *firefoxAACTimingQuirk, "")
			}
			if !dvStrip {
				applyCopiedVideoQuirksV3(&progressivePlan, source, input.Request, high10Quirk)
			}
			progressiveExecutable := (!progressiveTranscodeAudio || progressiveAudioConvertOK) && (!dvStrip || dvStripEligibleProgressive)
			tryProgressive := func() (PlannerResultV3, bool) {
				if !remuxSubtitleOK || !progressiveExecutable || progressiveTranscodeAudio && !audioRemuxFitsServerCapV3(input, file, progressiveAudioChannels) {
					return PlannerResultV3{}, false
				}
				candidate := cloneRemuxPlanCandidateV3(progressivePlan)
				applySubtitleDecisionV3(&candidate, remuxSubtitle.Decision)
				candidate.Claims.Subtitles = remuxSubtitle.Claims
				finalizePlanIdentityV3(&candidate, input.Request.PlaybackAttemptID, input.Request.ClientPlaybackContext.Output.OutputContextID)
				if deliverySupportsPlanV3(input.Request, DeliveryClassProgressiveV3, candidate) && !planAttemptedV3(candidate, input.Request.ClientPlaybackContext.Output.OutputContextID, input.AttemptedKeys) {
					return PlannerResultV3{Plan: &candidate, PlayMethod: PlayRemux, TranscodeAudio: progressiveTranscodeAudio, RemuxResumeLeadingPictureDrop: firefoxMacOSHEVCResumeLeadingPictureDropV3(source, input.Request), TargetAudioCodec: candidate.EffectiveRecipe.AudioCodec, SourceAudioChannels: stereoDownmixSourceChannelsV3(source.AudioChannels, progressiveAudioChannels, progressiveTranscodeAudio), TargetAudioChannels: progressiveAudioChannels, SubtitleTrackIndex: remuxSubtitle.SelectedIndex, SubtitleTransportTrackIndex: remuxSubtitle.TransportIndex, SubtitleCodec: remuxSubtitle.Codec, DownloadedSubtitleID: remuxSubtitle.DownloadedSubtitleID}, true
				}
				return PlannerResultV3{}, false
			}
			progressiveFirst := !progressiveTranscodeAudio || hlsTranscodeAudio || hlsAudioQuirkOK
			if progressiveFirst {
				if result, ok := tryProgressive(); ok {
					return result
				}
			}
			hlsRouteOK := deliveryAvailableV3(input.Request, DeliveryClassHLSV3) && hlsRemuxSubtitleOK && (!dvStrip || dvStripEligibleHLS) && (!hlsTranscodeAudio && !hlsAudioQuirkOK || audioRemuxFitsServerCapV3(input, file, hlsAACChannels))
			if hlsRouteOK && (hlsTranscodeAudio || hlsAudioQuirkOK) && !input.hlsRemuxRegistry().Available(TransformationAudioToAACV3) {
				// HLS needs an AAC conversion that no HLS executor offers. Skip
				// only this route: a later recipe (the Dolby Vision HDR10 strip)
				// may still play over progressive. The terminal is reported once
				// every remux recipe is exhausted.
				hlsRouteOK = false
				switch {
				case hlsQuirkConvertsAudio:
					hlsAudioConversionUnavailable = "The device-specific HLS route requires the validated AAC conversion toolchain."
				case hlsCodecTranscodeAudio:
					hlsAudioConversionUnavailable = "The HLS route requires the validated AAC conversion toolchain."
				}
			}
			if hlsRouteOK {
				plan := cloneRemuxPlanCandidateV3(remuxBase)
				plan.Delivery = DeliveryRemuxHLSV3
				plan.Stream = StreamV3{Protocol: StreamHLSV3, Container: containerHLSV3, MIMEType: "application/vnd.apple.mpegurl", Headers: map[string]string{}, HeaderRefresh: HeaderRefreshNoneV3}
				plan.EffectiveRecipe.VideoSampleEntry = hlsVideoSampleEntryV3(source, input.Request, dvStrip)
				hlsAudioChannels := 0
				if hlsTranscodeAudio && !hlsQuirkConvertsAudio {
					hlsAudioChannels = hlsAACChannels
					plan.EffectiveRecipe.AudioCodec = audioCodecAACV3
					plan.EffectiveRecipe.AudioChannels = intPointerV3(hlsAudioChannels)
					plan.EffectiveRecipe.AudioLayout = audioLayoutForChannelsV3(hlsAudioChannels)
					plan.Claims.Audio = AudioClaimsV3{Codec: audioCodecAACV3, Reason: hlsAudioAdaptationReasonV3}
					plan.Transformations = append(plan.Transformations, TransformationV3{Name: TransformationAudioToAACV3, Executor: ExecutorServerV3, RecipeVersion: TransformationAudioToAACRecipeVersionV3, ValidatedClaims: []string{ClaimAudioDecodeV3}})
					message := "The selected audio track is converted to AAC for HLS delivery."
					if !hlsCodecTranscodeAudio {
						message = fmt.Sprintf("The selected audio track is converted to AAC %s to fit this output's channel limit.", audioLayoutForChannelsV3(hlsAudioChannels))
					}
					plan.DegradationWarnings = append(plan.DegradationWarnings, DegradationWarningV3{Code: degradationAudioConvertedV3, Message: message})
				}
				if normalizeMatroskaAAC {
					appendAppliedQuirkV3(&plan, *firefoxAACTimingQuirk, "")
				}
				if hlsQuirkConvertsAudio {
					hlsTranscodeAudio = true
					hlsAudioChannels = hlsAACChannels
					plan.EffectiveRecipe.AudioCodec = audioCodecAACV3
					plan.EffectiveRecipe.AudioChannels = intPointerV3(hlsAudioChannels)
					plan.EffectiveRecipe.AudioLayout = audioLayoutForChannelsV3(hlsAudioChannels)
					plan.Claims.Audio = AudioClaimsV3{Codec: audioCodecAACV3, Reason: "device_hls_audio_adaptation"}
					plan.Transformations = append(plan.Transformations, TransformationV3{Name: TransformationAudioToAACV3, Executor: ExecutorServerV3, RecipeVersion: TransformationAudioToAACRecipeVersionV3, ValidatedClaims: []string{ClaimAudioDecodeV3}})
					plan.DegradationWarnings = append(plan.DegradationWarnings, DegradationWarningV3{Code: degradationAudioConvertedV3, Message: fmt.Sprintf("The selected audio track is converted to AAC %s for this device's HLS route.", audioLayoutForChannelsV3(hlsAudioChannels))})
					appendAppliedQuirkV3(&plan, *hlsAudioQuirk, "")
				}
				if !dvStrip {
					applyCopiedVideoQuirksV3(&plan, source, input.Request, high10Quirk)
				}
				if hlsTranscodeAudio {
					plan.DecisionReason = hlsAudioAdaptationReasonV3
				} else {
					plan.DecisionReason = "hls_packaging_required"
				}
				applySubtitleDecisionV3(&plan, hlsSubtitle.Decision)
				plan.Claims.Subtitles = hlsSubtitle.Claims
				finalizePlanIdentityV3(&plan, input.Request.PlaybackAttemptID, input.Request.ClientPlaybackContext.Output.OutputContextID)
				if deliverySupportsPlanV3(input.Request, DeliveryClassHLSV3, plan) && !planAttemptedV3(plan, input.Request.ClientPlaybackContext.Output.OutputContextID, input.AttemptedKeys) {
					targetAudio := codecCopyV3
					if hlsTranscodeAudio {
						targetAudio = audioCodecAACV3
					}
					return PlannerResultV3{Plan: &plan, PlayMethod: PlayRemux, TranscodeAudio: hlsTranscodeAudio, TargetVideoCodec: codecCopyV3, TargetAudioCodec: targetAudio, SourceAudioChannels: stereoDownmixSourceChannelsV3(source.AudioChannels, hlsAudioChannels, hlsTranscodeAudio), TargetAudioChannels: hlsAudioChannels, TargetResolution: resolutionLabelV3(source.Height), TargetBitrateKbps: source.BitrateKbps, SubtitleTrackIndex: hlsSubtitle.SelectedIndex, SubtitleTransportTrackIndex: hlsSubtitle.TransportIndex, SubtitleCodec: hlsSubtitle.Codec, DownloadedSubtitleID: hlsSubtitle.DownloadedSubtitleID}
				}
			}
			if !progressiveFirst {
				if result, ok := tryProgressive(); ok {
					return result
				}
			}
		}
		if hlsAudioConversionUnavailable != "" {
			return terminalPlannerResultV3(TerminalAudioConversionUnsupportedV3, hlsAudioConversionUnavailable, true)
		}
		if input.ServerBitrateCapKbps > 0 && (progressiveTranscodeAudio || hlsTranscodeAudio || hlsAudioQuirkOK) {
			// The audio-converting remux could not prove it stays inside the cap
			// (or was already attempted). Encode both tracks with an explicit
			// budget.
			return planVideoTranscodeV3(input, base, source, quality, hlsSubtitle, "", false)
		}
	}
	if deliveryAvailableV3(input.Request, DeliveryClassHLSV3) {
		return planVideoTranscodeV3(input, base, source, quality, hlsSubtitle, "copy_routes_exhausted", false)
	}

	return terminalPlannerResultV3("adaptation_unavailable", "No validated playback route is available for this source and output route.", false)
}

// Native HLS consumers use hvc1/dvh1 byte recipes. Web MediaSource clients
// keep the server's existing FFmpeg-default labeling unless they explicitly
// advertise the delivery-scoped native-HLS feature.
func hlsVideoSampleEntryV3(source SourceDescriptorV3, request StartRequestV3, dvStrip bool) string {
	if !strings.EqualFold(source.VideoCodec, "hevc") {
		return ""
	}
	if !deliverySupportsFeatureV3(request, DeliveryClassHLSV3, ClientNativeHLSPlaybackV3) &&
		!usesFirstPartyAndroidMedia3HLSV3(request) {
		return ""
	}
	if !dvStrip && (source.DVProfile == 5 || source.DVProfile == 8) {
		return VideoSampleEntryDVH1
	}
	return VideoSampleEntryHVC1
}

func availableQualitiesV3(input PlannerInputV3, source SourceDescriptorV3) []AvailableQualityV3 {
	return availableQualitiesForRouteV3(input, source)
}

// LowerVersionV3 describes the version of an item that serves lower
// qualities when the requested version is 4K and 4K transcoding is disabled.
type LowerVersionV3 struct {
	// Requested is the version the viewer asked for. The menu's original
	// entry stays this version even while the lower version plays.
	Requested SourceDescriptorV3
	// Lower is the first non-4K version in the handler's fallback order, the
	// one a lower quality moves playback to.
	Lower SourceDescriptorV3
}

// audioAvailableQualitiesV3 is the audio-only menu: quality rungs are a video
// concept, so the only entry is the source itself.
func audioAvailableQualitiesV3(source SourceDescriptorV3) []AvailableQualityV3 {
	return []AvailableQualityV3{{Label: QualityOriginalV3, BitrateKbps: source.BitrateKbps, PreservesSource: true}}
}

const (
	// decisionReasonBandwidthCapV3 marks a plan whose recipe was constrained by
	// the request's bandwidth cap rather than by decode capability.
	decisionReasonBandwidthCapV3 = "quality_bandwidth_cap"

	// decisionReasonClientManagedDynamicRangeV3 marks an original-file plan
	// whose executor owns source-to-output dynamic-range presentation.
	decisionReasonClientManagedDynamicRangeV3 = "client_managed_dynamic_range"

	// decisionReasonClientDV8BaseLayerV3 marks an original-file plan for a
	// Dolby Vision Profile 8 source that the client plays through an ordinary
	// HEVC decoder as its compatible base layer. The recipe's dynamic_range is
	// that base range, not dolby_vision.
	decisionReasonClientDV8BaseLayerV3 = "client_dv8_base_layer"
)

// planAudioOnlyV3 plans sources without a video track (audiobooks, music).
// The route family is deliberately small: the original container over
// progressive HTTP when the client decodes the audio codec, otherwise a
// progressive AAC conversion remux. Video, HDR, quality-ladder, and
// subtitle-burn gates do not apply.
func planAudioOnlyV3(input PlannerInputV3, file *models.MediaFile, source SourceDescriptorV3) PlannerResultV3 {
	request := input.Request
	audioOK, _, audioClaims := audioEligibilityV3(source, request)
	originalAudioSelectionOK := audioSelectionUsesContainerDefaultV3(file, input.AudioTrackIndex) ||
		clientSelectsOriginalAudioTrackV3(request)
	bandwidthCapKbps := optionalValueV3(request.BandwidthCapKbps)
	bandwidthCapExceeded := bandwidthCapKbps > 0 && (source.BitrateKbps > bandwidthCapKbps || input.ServerBitrateCapKbps > 0 && source.BitrateKbps <= 0)
	if source.AudioCodec == "" {
		// A file with neither a video track nor a probed audio codec has no
		// stream the planner can validate a route for.
		return terminalPlannerResultV3("source_metadata_incomplete", "The source is missing audio metadata required for a validated playback route.", true)
	}
	base := PlanV3{
		ProtocolVersion: ProtocolV3,
		ExpiresAt:       NewPlanExpiryV3(input.Now),
		SelectedTracks:  selectedTracksForPlanV3(file, input.AudioTrackIndex, SubtitlePolicyResultV3{SelectedIndex: -1, TransportIndex: -1}),
		EffectiveRecipe: recipeFromSourceV3(source),
		Claims:          ValidationClaimsV3{Audio: audioClaims},
		// Audio-only routes bypass every subtitle gate, so the inventory is
		// empty rather than a list of tracks no route on this plan can deliver.
		Subtitle: SubtitleDecisionV3{Mode: SubtitleOffV3, Inventory: []SubtitleInventoryItemV3{}},
		// The audio inventory is the effective source's probed tracks, exactly
		// as on video plans: clients render the audio menu from it rather than
		// from item metadata that can be stale after a version fallback.
		AudioTracks:            audioInventoryV3(file),
		Transformations:        []TransformationV3{},
		AppliedQuirks:          []AppliedQuirkV3{},
		RuntimeCorrections:     []string{},
		AvailableQualities:     audioAvailableQualitiesV3(source),
		DegradationWarnings:    []DegradationWarningV3{},
		RequestedMediaFileID:   input.RequestedFile.ID,
		EffectiveMediaFileID:   file.ID,
		EffectiveVirtualURI:    effectiveVirtualURIV3(input),
		VirtualSourceRevision:  virtualSourceRevisionV3(input),
		InventoryProvenance:    input.InventoryProvenance,
		Source:                 source,
		SubtitleFidelityPolicy: subtitlePolicyNameV3(request.SubtitleFidelityPreference),
		Timeline:               TimelineV3{SourceStartSeconds: floatOrZeroV3(request.StartPosition), PlayerStartSeconds: floatOrZeroV3(request.StartPosition), CanSeekAnywhere: true, SeekRestoration: "player_position"},
	}
	containerOK := containsFoldV3(request.Capabilities.Containers, source.Container)
	if audioOK && containerOK && !bandwidthCapExceeded && originalAudioSelectionOK && deliveryAvailableV3(request, DeliveryClassOriginalHTTPV3) {
		plan := base
		plan.Delivery = DeliveryOriginalHTTPV3
		plan.Stream = StreamV3{Protocol: StreamHTTPProgressiveV3, Container: source.Container, MIMEType: MimeFromExtension(file.FilePath), Headers: map[string]string{}, HeaderRefresh: HeaderRefreshNoneV3}
		plan.DecisionReason = "validated_original_playback"
		finalizePlanIdentityV3(&plan, request.PlaybackAttemptID, request.ClientPlaybackContext.Output.OutputContextID)
		if deliverySupportsPlanV3(request, DeliveryClassOriginalHTTPV3, plan) && !planAttemptedV3(plan, request.ClientPlaybackContext.Output.OutputContextID, input.AttemptedKeys) {
			return PlannerResultV3{Plan: &plan, PlayMethod: PlayDirect, SubtitleTrackIndex: -1, SubtitleTransportTrackIndex: -1}
		}
	}
	if !deliveryAvailableV3(request, DeliveryClassProgressiveV3) {
		return terminalPlannerResultV3("adaptation_unavailable", "No validated playback route is available for this audio source.", false)
	}
	transcodeAudio := !audioOK || bandwidthCapExceeded
	var progressiveRegistry *TransformationRegistryV3
	if transcodeAudio {
		progressiveRegistry = input.progressiveRemuxRegistry()
		if progressiveRegistry == nil || !progressiveRegistry.Available(TransformationAudioToAACV3) {
			return terminalPlannerResultV3(TerminalAudioConversionUnsupportedV3, "The required validated AAC conversion toolchain is unavailable.", true)
		}
	}
	plan := base
	plan.Delivery = DeliveryRemuxProgressiveV3
	// The remux muxes an audio-only fMP4, so the plan must promise audio/mp4:
	// a declared-tier client probes the advertised MIME with isTypeSupported
	// before it will attach a source buffer, and "video/mp4" with no video
	// track is exactly the mismatch that makes that probe lie.
	plan.Stream = StreamV3{Protocol: StreamHTTPProgressiveV3, Container: containerMP4V3, MIMEType: AudioOnlyRemuxMIMEV3, Headers: map[string]string{}, HeaderRefresh: HeaderRefreshNoneV3}
	plan.DecisionReason = decisionReasonContainerNormalizationV3
	targetAudioChannels := audioOnlyAACOutputChannelsV3(request, source)
	targetAudioBitrateKbps := 0
	if transcodeAudio {
		targetAudioBitrateKbps = audioOnlyAACBitrateKbpsV3(bandwidthCapKbps)
		if input.ServerBitrateCapKbps > 0 {
			targetAudioBitrateKbps = min(targetAudioBitrateKbps, input.ServerBitrateCapKbps*95/100)
			if targetAudioBitrateKbps < 32 {
				return terminalPlannerResultV3(TerminalBitratePolicyUnavailableV3, "The server bitrate limit is too low for a playable audio stream.", false)
			}
		}
		applyAudioOnlyAACConversionV3(&plan, targetAudioChannels, targetAudioBitrateKbps, bandwidthCapExceeded)
	} else if !deliverySupportsPlanV3(request, DeliveryClassProgressiveV3, plan) {
		progressiveRegistry = input.progressiveRemuxRegistry()
		if progressiveRegistry != nil && progressiveRegistry.Available(TransformationAudioToAACV3) {
			converted := plan
			targetAudioBitrateKbps = audioOnlyAACBitrateKbpsV3(bandwidthCapKbps)
			applyAudioOnlyAACConversionV3(&converted, targetAudioChannels, targetAudioBitrateKbps, false)
			if deliverySupportsPlanV3(request, DeliveryClassProgressiveV3, converted) {
				plan = converted
				transcodeAudio = true
			}
		}
	}
	if !deliverySupportsPlanV3(request, DeliveryClassProgressiveV3, plan) {
		return terminalPlannerResultV3("adaptation_unavailable", "The progressive delivery cannot decode the planned audio recipe.", false)
	}
	finalizePlanIdentityV3(&plan, request.PlaybackAttemptID, request.ClientPlaybackContext.Output.OutputContextID)
	if planAttemptedV3(plan, request.ClientPlaybackContext.Output.OutputContextID, input.AttemptedKeys) {
		return terminalPlannerResultV3("adaptation_exhausted", "All compatible playback recipes have already failed for this output route.", false)
	}
	if !transcodeAudio {
		targetAudioChannels = 0
		targetAudioBitrateKbps = 0
	}
	return PlannerResultV3{Plan: &plan, PlayMethod: PlayRemux, TranscodeAudio: transcodeAudio, TargetAudioCodec: plan.EffectiveRecipe.AudioCodec, SourceAudioChannels: stereoDownmixSourceChannelsV3(source.AudioChannels, targetAudioChannels, transcodeAudio), TargetAudioChannels: targetAudioChannels, TargetAudioBitrateKbps: targetAudioBitrateKbps, SubtitleTrackIndex: -1, SubtitleTransportTrackIndex: -1}
}

func stereoDownmixSourceChannelsV3(sourceChannels, targetChannels int, transcodeAudio bool) int {
	if !transcodeAudio || sourceChannels <= 2 || (targetChannels != 0 && targetChannels != 2) {
		return 0
	}
	return sourceChannels
}

func audioOnlyAACOutputChannelsV3(request StartRequestV3, source SourceDescriptorV3) int {
	return aacOutputChannelsV3(request, DeliveryClassProgressiveV3, source.AudioChannels, false)
}

// aacOutputChannelsV3 picks an encoder-supported AAC layout that does not
// exceed the active delivery's ceiling. FFmpeg's planned AAC recipes support
// mono, stereo, and 5.1; an intermediate ceiling therefore falls back from
// 5.1 to stereo rather than advertising an output the encoder never creates.
func aacOutputChannelsV3(request StartRequestV3, deliveryClass string, sourceChannels int, preserveSurround bool) int {
	channels := 2
	if sourceChannels == 1 {
		channels = 1
	} else if preserveSurround && sourceChannels >= 6 {
		channels = 6
	}
	capability, ok := request.ClientPlaybackContext.Deliveries[deliveryClass]
	if !ok || capability.MaxChannels == nil || *capability.MaxChannels <= 0 || channels <= *capability.MaxChannels {
		return channels
	}
	if *capability.MaxChannels == 1 {
		return 1
	}
	return 2
}

func audioLayoutForChannelsV3(channels int) string {
	switch channels {
	case 1:
		return audioLayoutMonoV3
	case 6:
		return audioLayoutSurround51V3
	default:
		return audioLayoutStereoV3
	}
}

func audioOnlyAACBitrateKbpsV3(bandwidthCapKbps int) int {
	const defaultAACBitrateKbps = 192
	if bandwidthCapKbps > 0 && bandwidthCapKbps < defaultAACBitrateKbps {
		return bandwidthCapKbps
	}
	return defaultAACBitrateKbps
}

func applyAudioOnlyAACConversionV3(plan *PlanV3, targetChannels, targetBitrateKbps int, bandwidthCapExceeded bool) {
	layout := audioLayoutStereoV3
	warning := "The selected audio track is converted to AAC stereo."
	if targetChannels == 1 {
		layout = audioLayoutMonoV3
		warning = "The selected audio track is converted to AAC mono."
	}
	plan.EffectiveRecipe.AudioCodec = "aac"
	plan.EffectiveRecipe.AudioChannels = intPointerV3(targetChannels)
	plan.EffectiveRecipe.AudioLayout = layout
	plan.EffectiveRecipe.BitrateKbps = intPointerV3(targetBitrateKbps)
	plan.Claims.Audio = AudioClaimsV3{Codec: "aac", Reason: "server_audio_adaptation"}
	plan.Transformations = append(plan.Transformations, TransformationV3{Name: TransformationAudioToAACV3, Executor: ExecutorServerV3, RecipeVersion: TransformationAudioToAACRecipeVersionV3, ValidatedClaims: []string{ClaimAudioDecodeV3}})
	plan.DegradationWarnings = append(plan.DegradationWarnings, DegradationWarningV3{Code: degradationAudioConvertedV3, Message: warning})
	plan.DecisionReason = "audio_adaptation"
	if bandwidthCapExceeded {
		plan.DegradationWarnings = append(plan.DegradationWarnings, DegradationWarningV3{Code: "bandwidth_cap_applied", Message: "Delivery quality is limited by the configured bandwidth cap."})
		plan.DecisionReason = decisionReasonBandwidthCapV3
	}
}

// targetVideoSelectionV3 describes the video encoder a server transcode will
// emit: the codec exposed on the plan, and the transformation/claim identity
// that matches it.
type targetVideoSelectionV3 struct {
	codec          string
	transformation string
	recipeVersion  string
	claim          string
}

// targetVideoPreferencesV3 is the encoder preference order. AV1 and HEVC are
// upgrades the planner may only take when the client declares hardware decode
// and the installed encoder advertises the matching transformation; H.264 is
// the floor that must always remain available.
var targetVideoPreferencesV3 = []targetVideoSelectionV3{
	{codec: TargetVideoCodecAV1V3, transformation: TransformationVideoToAV1V3, recipeVersion: TransformationVideoToAV1RecipeVersionV3, claim: ClaimAV1DecodeV3},
	{codec: TargetVideoCodecHEVCV3, transformation: TransformationVideoToHEVCV3, recipeVersion: TransformationVideoToHEVCRecipeVersionV3, claim: ClaimHEVCDecodeV3},
}

var targetVideoFloorV3 = targetVideoSelectionV3{codec: TargetVideoCodecH264V3, transformation: TransformationVideoToH264V3, recipeVersion: TransformationVideoToH264RecipeVersionV3, claim: ClaimH264DecodeV3}

// targetVideoBurnInPreferencesV3 is the burn-in encoder order: H.264 first,
// then HEVC, then AV1 only as a last resort. A burn-in encode runs overlays and
// often a tone-map on top of the encode, so the slowest hardware encoder on the
// box (AV1) must not be chosen when a realtime-safe H.264/HEVC path exists.
var targetVideoBurnInPreferencesV3 = []targetVideoSelectionV3{
	targetVideoFloorV3,
	{codec: TargetVideoCodecHEVCV3, transformation: TransformationVideoToHEVCV3, recipeVersion: TransformationVideoToHEVCRecipeVersionV3, claim: ClaimHEVCDecodeV3},
	{codec: TargetVideoCodecAV1V3, transformation: TransformationVideoToAV1V3, recipeVersion: TransformationVideoToAV1RecipeVersionV3, claim: ClaimAV1DecodeV3},
}

// selectTargetVideoCodecV3 chooses the HLS video encoder from the client's
// declared hardware codecs. AV1 and HEVC are eligible only when the client
// lists the codec as hardware-decodable, the HLS delivery accepts it, and the
// registry advertises the matching encoder transformation. Missing or
// unknown capabilities fall through to the H.264 floor.
func selectTargetVideoCodecV3(input PlannerInputV3, registry *TransformationRegistryV3) (targetVideoSelectionV3, bool) {
	return selectTargetVideoCodecForV3(input, registry, false)
}

// selectBurnInTargetVideoCodecV3 is selectTargetVideoCodecV3 for a plan that
// must burn subtitles into the video: the ordered preferences put H.264 ahead
// of the codecs ordinary selection would prefer.
func selectBurnInTargetVideoCodecV3(input PlannerInputV3, registry *TransformationRegistryV3) (targetVideoSelectionV3, bool) {
	return selectTargetVideoCodecForV3(input, registry, true)
}

func selectTargetVideoCodecForV3(input PlannerInputV3, registry *TransformationRegistryV3, burnIn bool) (targetVideoSelectionV3, bool) {
	if registry == nil {
		return targetVideoSelectionV3{}, false
	}
	declared := input.Request.Capabilities.CodecsVideoHardware
	delivery := hlsDeliveryVideoCodecsV3(input.Request)
	preferences := targetVideoPreferencesV3
	if burnIn {
		preferences = targetVideoBurnInPreferencesV3
	}
	for _, candidate := range preferences {
		// H.264 is the universal floor: it never needs the client to declare
		// hardware decode. HEVC and AV1 are upgrades gated on that claim.
		// HEVC additionally needs the server opt-in on top of the client
		// claim; the detailed output-capability check runs at plan time
		// (see planVideoTranscodeV3), where the chosen quality is known.
		if candidate.codec != TargetVideoCodecH264V3 && !containsFoldV3(declared, candidate.codec) {
			continue
		}
		if candidate.codec == TargetVideoCodecHEVCV3 && !input.Settings.AllowHEVCEncoding {
			continue
		}
		// AV1 has no validated software encoder in this pipeline; only take it
		// when the configured backend is QSV.
		if candidate.codec == TargetVideoCodecAV1V3 && !strings.EqualFold(strings.TrimSpace(input.Settings.HWAccel), transcodeHWQSV) {
			continue
		}
		if len(delivery) > 0 && !containsFoldV3(delivery, candidate.codec) {
			continue
		}
		if !registry.Available(candidate.transformation) {
			continue
		}
		return candidate, true
	}
	if registry.Available(targetVideoFloorV3.transformation) {
		return targetVideoFloorV3, true
	}
	return targetVideoSelectionV3{}, false
}

// hlsDeliveryVideoCodecsV3 returns the client's declared HLS video codecs. An
// empty list means the delivery did not constrain codecs and is not an
// affirmative capability claim.
func hlsDeliveryVideoCodecsV3(request StartRequestV3) []string {
	return request.ClientPlaybackContext.Deliveries[DeliveryClassHLSV3].VideoCodecs
}

// planVideoTranscodeV3 always executes on the HLS delivery, so the caller must
// pass the subtitle policy resolved against DeliveryClassHLSV3.
//
// subtitleForcedAdaptation marks the case where only the subtitle burn
// requirement forced this adaptation. Deselecting the subtitle then restores
// playback, so a refusal must name the subtitle: an HDR or version reason
// sends the user chasing a problem that is not blocking them. Retryable
// infrastructure failures and the client-route terminal keep their own reasons.
func planVideoTranscodeV3(input PlannerInputV3, base PlanV3, source SourceDescriptorV3, quality QualityResultV3, subtitle SubtitlePolicyResultV3, reasonOverride string, subtitleForcedAdaptation bool) PlannerResultV3 {
	if !deliveryAvailableV3(input.Request, DeliveryClassHLSV3) {
		return terminalPlannerResultV3("client_hls_unsupported", "The client cannot execute the required HLS adaptation route.", false)
	}
	if subtitle.Terminal != nil {
		return PlannerResultV3{Terminal: subtitle.Terminal, SubtitleTrackIndex: -1, SubtitleTransportTrackIndex: -1}
	}
	if !input.Settings.TranscodeEnabled {
		if subtitleForcedAdaptation {
			return terminalPlannerResultV3("subtitle_conversion_unsupported", "The selected subtitle must be burned into the video, but transcoding is unavailable.", false)
		}
		return terminalPlannerResultV3("transcoding_disabled", "The source requires video adaptation, but transcoding is unavailable.", false)
	}
	var hlsRegistry *TransformationRegistryV3
	toneMapRecipe := resolvedToneMapRecipeV3{}
	if source.DynamicRange != "" && source.DynamicRange != DynamicRangeSDRV3 {
		toneMapRecipe = resolveToneMapRecipeV3(input, source, nil)
		hlsRegistry = toneMapRecipe.hlsRegistry
	}
	if source.DynamicRange != "" && source.DynamicRange != DynamicRangeSDRV3 && !toneMapRecipe.ok {
		if subtitleForcedAdaptation {
			return terminalPlannerResultV3("subtitle_conversion_unsupported", "The selected subtitle must be burned into the video, but this HDR source cannot be re-encoded.", false)
		}
		return terminalPlannerResultV3(TerminalHDRTranscodeUnsupportedV3, "This HDR source requires video encoding, but no validated HDR-preserving or tone-map recipe is installed.", false)
	}
	if is4KSourceV3(input.EffectiveFile, source) && !input.Settings.Allow4KTranscode {
		if subtitleForcedAdaptation {
			return terminalPlannerResultV3("subtitle_conversion_unsupported", "The selected subtitle must be burned into the video, but 4K transcoding is disabled.", false)
		}
		return terminalPlannerResultV3("no_alternate_version", TerminalMessage4KTranscodeDisabledV3, false)
	}
	if hlsRegistry == nil {
		hlsRegistry = input.hlsVideoRegistry()
	}
	if hlsRegistry == nil || !hlsRegistry.Available(TransformationAudioToAACV3) {
		return terminalPlannerResultV3("conversion_tool_unavailable", "The required validated video/AAC conversion toolchain is unavailable.", true)
	}
	var targetVideo targetVideoSelectionV3
	var videoTranscodeOK bool
	if subtitle.RequiresBurn {
		targetVideo, videoTranscodeOK = selectBurnInTargetVideoCodecV3(input, hlsRegistry)
	} else {
		targetVideo, videoTranscodeOK = selectTargetVideoCodecV3(input, hlsRegistry)
	}
	if !videoTranscodeOK {
		return terminalPlannerResultV3("conversion_tool_unavailable", "The required validated video/AAC conversion toolchain is unavailable.", true)
	}
	// HEVC needs the server opt-in plus the detailed output-capability check:
	// flat codec lists can describe source copy support without proving the
	// fMP4 transcode decoder that will receive this recipe. Fall back to the
	// H.264 floor when either fails.
	if targetVideo.codec == TargetVideoCodecHEVCV3 &&
		(!input.Settings.AllowHEVCEncoding || !hlsHEVCOutputSupportedV3(input.Request, quality, source)) {
		targetVideo = targetVideoFloorV3
	}
	// A forced encode triggered by burn-in, HDR handling, or capability
	// evidence must not inherit a source-preserving "original quality" bitrate:
	// that yields a source-bitrate encode no hardware encoder can keep up with.
	// Replace it with the source-class ladder rung.
	sourcePreservingEncodeForced := quality.PreservesSource
	if sourcePreservingEncodeForced {
		quality = forcedEncodeQualityResultV3(source, quality)
	}
	if source.DynamicRange != "" && source.DynamicRange != DynamicRangeSDRV3 {
		base.AvailableQualities = availableQualitiesForRouteV3(input, source)
	}
	plan := base
	if sourcePreservingEncodeForced {
		plan.DegradationWarnings = append(plan.DegradationWarnings, DegradationWarningV3{
			Code:    "quality_source_requires_transcode",
			Message: "This route must re-encode the source; the encode uses the source-class quality ladder rung instead of the source bitrate.",
		})
	}
	plan.Delivery = DeliveryTranscodeHLSV3
	plan.Stream = StreamV3{Protocol: StreamHLSV3, Container: containerHLSV3, MIMEType: "application/vnd.apple.mpegurl", Headers: map[string]string{}, HeaderRefresh: HeaderRefreshNoneV3}
	plan.EffectiveRecipe.VideoCodec = targetVideo.codec
	if targetVideo.codec == transcodeCodecHEVC {
		plan.EffectiveRecipe.VideoSampleEntry = VideoSampleEntryHVC1
	}
	plan.EffectiveRecipe.AudioCodec = "aac"
	// A reactive software-decode retry is a distinct route from the hardware
	// plan it replaces. The flag is frozen into the recipe so restarts and
	// later seek reanchors keep decoding on the CPU.
	plan.EffectiveRecipe.SoftwareVideoDecode = base.EffectiveRecipe.SoftwareVideoDecode || input.ForceSoftwareVideoDecode
	plan.EffectiveRecipe.Width = intPointerV3(quality.Width)
	plan.EffectiveRecipe.Height = intPointerV3(quality.Height)
	targetAudioBitrateKbps := 0
	if input.ServerBitrateCapKbps > 0 {
		channels := aacOutputChannelsV3(input.Request, DeliveryClassHLSV3, source.AudioChannels, true)
		_, defaultAudioBitrateKbps := ResolveAACOutputV3(channels, 0)
		budget := optionalValueV3(input.Request.BandwidthCapKbps) * 95 / 100
		targetAudioBitrateKbps = min(defaultAudioBitrateKbps, max(64, budget/4))
		if budget-targetAudioBitrateKbps < 64 {
			return terminalPlannerResultV3(TerminalBitratePolicyUnavailableV3, "The server bitrate limit is too low for a playable video stream.", false)
		}
		quality.BitrateKbps = min(quality.BitrateKbps, budget-targetAudioBitrateKbps)
	}

	plan.EffectiveRecipe.BitrateKbps = intPointerV3(quality.BitrateKbps)
	// Surround sources keep 5.1 through the AAC re-encode (universal Media3
	// decode); only stereo/mono sources — and unknown layouts — downmix to 2.0.
	targetAudioChannels := aacOutputChannelsV3(input.Request, DeliveryClassHLSV3, source.AudioChannels, true)
	audioLayout := audioLayoutForChannelsV3(targetAudioChannels)
	plan.EffectiveRecipe.AudioChannels = intPointerV3(targetAudioChannels)
	plan.EffectiveRecipe.AudioLayout = audioLayout
	plan.Transformations = append(plan.Transformations,
		TransformationV3{Name: targetVideo.transformation, Executor: ExecutorServerV3, RecipeVersion: targetVideo.recipeVersion, ValidatedClaims: []string{targetVideo.claim}},
		TransformationV3{Name: TransformationAudioToAACV3, Executor: ExecutorServerV3, RecipeVersion: TransformationAudioToAACRecipeVersionV3, ValidatedClaims: []string{ClaimAudioDecodeV3}},
	)
	toneMapPolicy := toneMapRecipe.policy
	toneMapMode := toneMapRecipe.mode
	toneMapResolution := toneMapRecipe.resolution
	toneMapRevision := toneMapRecipe.revision
	toneMapOK := toneMapRecipe.ok
	toneMapSourceKind := toneMapResolution.Kind
	if source.DynamicRange != "" && source.DynamicRange != DynamicRangeSDRV3 {
		plan.Transformations = append(plan.Transformations, TransformationV3{
			Name: TransformationHDRToSDRToneMapV3, Executor: ExecutorServerV3,
			RecipeVersion:   TransformationHDRToSDRToneMapRecipeVersionV3,
			ValidatedClaims: []string{ClaimHDRMetadataRemovedV3, ClaimSDRBT709OutputV3},
		})
		plan.DegradationWarnings = append(plan.DegradationWarnings, DegradationWarningV3{
			Code: DegradationWarningHDRToneMappedV3, Message: "HDR video is tone-mapped to SDR for this playback route.",
		})
	} else {
		// A non-tone-mapped recipe has no execution policy. Keeping PolicyNone
		// here would turn an ordinary SDR encode into a partial frozen recipe at
		// the FFmpeg execution boundary.
		toneMapPolicy = ""
		toneMapSourceKind = ""
		toneMapResolution = tonemap.SourceResolution{}
		toneMapRevision = tonemap.SourceRevision{}
	}
	plan.Claims.Audio = AudioClaimsV3{Codec: "aac", Passthrough: false, AtmosPreserved: false, Reason: "server_audio_adaptation"}
	applySubtitleDecisionV3(&plan, subtitle.Decision)
	plan.Claims.Subtitles = subtitle.Claims
	plan.DecisionReason = quality.Reason
	if reasonOverride != "" {
		plan.DecisionReason = reasonOverride
	}
	if subtitle.RequiresBurn {
		plan.DecisionReason = "subtitle_burn_in_required"
		plan.DegradationWarnings = append(plan.DegradationWarnings, DegradationWarningV3{Code: "subtitle_burn_in", Message: "The selected subtitle is rendered into the video."})
	}
	plan.EffectiveRecipe.DynamicRange = DynamicRangeSDRV3
	plan.Claims.Video = VideoClaimsV3{}
	if !deliverySupportsPlanV3(input.Request, DeliveryClassHLSV3, plan) {
		return terminalPlannerResultV3("adaptation_unavailable", "The HLS delivery cannot decode the planned transcode recipe.", false)
	}
	finalizePlanIdentityV3(&plan, input.Request.PlaybackAttemptID, input.Request.ClientPlaybackContext.Output.OutputContextID)
	// A failed HEVC attempt does not exhaust the H.264 route, and a failed AV1
	// attempt does not exhaust either fallback. Rebuild the candidate before
	// checking exhaustion so recovery can downgrade codecs on the same HLS
	// delivery without changing source/remux behavior.
	if targetVideo.codec != TargetVideoCodecH264V3 && planAttemptedV3(plan, input.Request.ClientPlaybackContext.Output.OutputContextID, input.AttemptedKeys) {
		targetVideo = targetVideoFloorV3
		plan.EffectiveRecipe.VideoCodec = targetVideo.codec
		plan.EffectiveRecipe.VideoSampleEntry = ""
		plan.Transformations = replaceVideoTransformationV3(plan.Transformations, targetVideo)
		if !deliverySupportsPlanV3(input.Request, DeliveryClassHLSV3, plan) {
			return terminalPlannerResultV3("adaptation_unavailable", "The HLS delivery cannot decode the H.264 fallback recipe.", false)
		}
		finalizePlanIdentityV3(&plan, input.Request.PlaybackAttemptID, input.Request.ClientPlaybackContext.Output.OutputContextID)
	}
	if planAttemptedV3(plan, input.Request.ClientPlaybackContext.Output.OutputContextID, input.AttemptedKeys) {
		if input.DecodeAttemptDetail != "" {
			return terminalPlannerResultV3WithDetail("adaptation_exhausted", "All compatible playback recipes have already failed for this output route.", input.DecodeAttemptDetail, false)
		}
		return terminalPlannerResultV3("adaptation_exhausted", "All compatible playback recipes have already failed for this output route.", false)
	}
	return PlannerResultV3{Plan: &plan, PlayMethod: PlayTranscode, TranscodeAudio: true, TargetVideoCodec: targetVideo.codec, TargetAudioCodec: "aac", SourceAudioChannels: stereoDownmixSourceChannelsV3(source.AudioChannels, targetAudioChannels, true), TargetAudioChannels: targetAudioChannels, TargetAudioBitrateKbps: targetAudioBitrateKbps, TargetResolution: quality.Label, TargetBitrateKbps: quality.BitrateKbps, SourceFrameRate: source.FrameRate, SourceHeight: source.Height, SubtitleTrackIndex: subtitle.SelectedIndex, SubtitleTransportTrackIndex: subtitle.TransportIndex, SubtitleBurnIn: subtitle.RequiresBurn, SubtitleCodec: subtitle.Codec, DownloadedSubtitleID: subtitle.DownloadedSubtitleID, ToneMapPolicy: toneMapPolicy, ToneMapMode: toneMapMode, ToneMapSourceKind: toneMapSourceKind, ToneMapRecipeVersion: toneMapRecipeVersionV3(toneMapOK), ToneMapPreflightRequired: toneMapResolution.PreflightRequired, ToneMapSourceRevision: toneMapRevision, ToneMapVPPEnabled: toneMapOK && input.Settings.VPPToneMapEnabled}
}

func videoTransformationForCodecV3(sel targetVideoSelectionV3) TransformationV3 {
	return TransformationV3{Name: sel.transformation, Executor: ExecutorServerV3, RecipeVersion: sel.recipeVersion, ValidatedClaims: []string{sel.claim}}
}

func replaceVideoTransformationV3(transformations []TransformationV3, sel targetVideoSelectionV3) []TransformationV3 {
	out := make([]TransformationV3, 0, len(transformations))
	replaced := false
	for _, transformation := range transformations {
		if !replaced && (transformation.Name == TransformationVideoToH264V3 || transformation.Name == TransformationVideoToHEVCV3 || transformation.Name == TransformationVideoToAV1V3) {
			out = append(out, videoTransformationForCodecV3(sel))
			replaced = true
			continue
		}
		out = append(out, transformation)
	}
	if !replaced {
		out = append(out, videoTransformationForCodecV3(sel))
	}
	return out
}

const hevcMainProfileV3 = "main"

// hlsHEVCOutputSupportedV3 requires the selected HLS executor to name HEVC
// explicitly and verifies its detailed decoder can handle this server's Main
// 8-bit SDR output at the chosen dimensions and bitrate. Flat codec lists are
// insufficient here: they can describe source copy support without proving
// the fMP4 transcode decoder that will receive this recipe.
func hlsHEVCOutputSupportedV3(request StartRequestV3, quality QualityResultV3, source SourceDescriptorV3) bool {
	delivery, ok := request.ClientPlaybackContext.Deliveries[DeliveryClassHLSV3]
	if !ok || !delivery.Enabled || !delivery.SupportedOnDevice || !containsFoldV3(delivery.VideoCodecs, transcodeCodecHEVC) {
		return false
	}
	output := source
	output.VideoCodec = transcodeCodecHEVC
	output.VideoProfile = hevcMainProfileV3
	output.BitDepth = 8
	output.Width = quality.Width
	output.Height = quality.Height
	output.BitrateKbps = quality.BitrateKbps
	output.DynamicRange = DynamicRangeSDRV3
	// The FFmpeg HEVC recipe does not currently pin -level:v, so it must not
	// claim a level to an exact decoder with a bounded level list. A level-bound
	// client stays on H.264 until the target level is selected, frozen, and
	// enforced as part of the executable HEVC recipe.
	for _, decoder := range request.Capabilities.VideoDecode {
		if strings.EqualFold(decoder.Codec, transcodeCodecHEVC) && len(decoder.Levels) > 0 {
			return false
		}
	}
	ok, _ = videoEligibleV3(output, request)
	return ok
}

// applySubtitleDecisionV3 changes the delivery-specific subtitle policy without
// discarding the source inventory already frozen onto the base plan. Adapted
// routes still address the same combined ordinal space as original_http; only
// the rendering decision and claims vary by delivery capability.
func applySubtitleDecisionV3(plan *PlanV3, decision SubtitleDecisionV3) {
	if plan == nil {
		return
	}
	inventory := plan.Subtitle.Inventory
	plan.Subtitle = decision
	plan.Subtitle.Inventory = inventory
}

func canStripDolbyVisionToHDR10V3(source SourceDescriptorV3, request StartRequestV3, registry *TransformationRegistryV3) bool {
	if source.DynamicRange != DynamicRangeDolbyVisionV3 || !clientSupportsHDR10V3(request, source) || registry == nil || !registry.Available(TransformationServerDV7HDR10V3) {
		return false
	}
	// Profile 7 always carries an HDR10-viewable base layer. Profile 8 is
	// safe only when the DOVI compatibility id explicitly identifies HDR10.
	return source.DVProfile == 7 || source.DVProfile == 8 && source.DVBLCompatID == 1
}

func canClientTransformDV7ToDV81V3(source SourceDescriptorV3, request StartRequestV3) bool {
	if source.DynamicRange != DynamicRangeDolbyVisionV3 || source.DVProfile != 7 ||
		!clientTransformationAvailableV3(request, ClientDV7ToDV81V3, ClientDVTransformVersionV3) {
		return false
	}
	// The registered conversion recipe produces Profile 8.1.
	source.DVBLCompatID = 1
	return clientSupportsDVProfileV3(request, source, 8)
}

func canClientTransformDV7ToHDR10V3(source SourceDescriptorV3, request StartRequestV3) bool {
	return source.DynamicRange == DynamicRangeDolbyVisionV3 && source.DVProfile == 7 && clientSupportsHDR10V3(request, source) &&
		clientTransformationAvailableV3(request, ClientDV7ToHDR10V3, ClientDVTransformVersionV3)
}

func clientSupportsDVProfileV3(request StartRequestV3, source SourceDescriptorV3, profile int) bool {
	hdr := nativeOutputHDRV3(request)
	source.DVProfile = profile
	return hdr != nil && hdrSupportsDolbyVisionSourceV3(*hdr, source)
}

func clientTransformationAvailableV3(request StartRequestV3, name, version string) bool {
	if !HasFeatureV3(request.ClientFeatures, FeatureClientVideoTransforms) {
		return false
	}
	delivery, ok := request.ClientPlaybackContext.Deliveries[DeliveryClassOriginalHTTPV3]
	if !ok || !delivery.Enabled || !delivery.SupportedOnDevice {
		return false
	}
	for _, transformation := range delivery.Transformations {
		if transformation.Executor == ExecutorClientV3 && transformation.Name == name && transformation.RecipeVersion == version {
			return true
		}
	}
	return false
}

// Is4KMediaFileV3 reports whether a catalog file is recorded as 4K or higher.
// Scanners and imports write the resolution label in several spellings, and the
// stored primary video track can carry dimensions that disagree with that label,
// so callers use both facts to stay aligned with the planner's 4K policy.
func Is4KMediaFileV3(file *models.MediaFile) bool {
	if file == nil {
		return false
	}
	labelWidth, labelHeight := dimensionsFromResolutionV3(file.Resolution)
	if labelWidth >= 3840 || labelHeight >= 2160 {
		return true
	}
	return len(file.VideoTracks) > 0 && (file.VideoTracks[0].Width >= 3840 || file.VideoTracks[0].Height >= 2160)
}

func is4KSourceV3(file *models.MediaFile, source SourceDescriptorV3) bool {
	return Is4KMediaFileV3(file) || source.Width >= 3840 || source.Height >= 2160
}

type QualityResultV3 struct {
	Label             string
	Width             int
	Height            int
	BitrateKbps       int
	PreservesSource   bool
	RequiresTranscode bool
	// ExplicitRung marks a user-selected fixed rung, as opposed to an
	// automatic reduction from device limits, bandwidth evidence, or caps.
	ExplicitRung bool
	Reason       string
	Warnings     []DegradationWarningV3
}

// ResolveQualityPolicyV3 selects the delivery quality for a plan.
//
// bandwidth_cap_kbps is a hard delivery ceiling and is honored in every
// quality mode: source-preserving delivery is degraded when the source bitrate
// exceeds the cap, fixed rungs are lowered when their ladder bitrate exceeds
// it, and "auto" folds the cap into bandwidth-based rung selection. A metered
// connection with neither a cap nor a bandwidth estimate limits auto
// selection to the conservative 720p rung — the rung auto would pick for a
// mid-range bandwidth estimate — instead of assuming the link can sustain the
// original stream.
func ResolveQualityPolicyV3(request StartRequestV3, source SourceDescriptorV3) QualityResultV3 {
	quality, changed := NormalizeQualityV3(request.QualityPreference)
	var warnings []DegradationWarningV3
	if changed {
		warnings = append(warnings, DegradationWarningV3{Code: "quality_preference_normalized", Message: "Unknown quality preference was normalized to auto."})
	}
	capKbps := optionalValueV3(request.BandwidthCapKbps)
	// The cap is a hard ceiling, so a video source whose bitrate is unknown
	// cannot be shown to fit it, as with the server cap.
	capExceededBySource := capKbps > 0 && (source.BitrateKbps > capKbps || source.BitrateKbps <= 0 && source.VideoCodec != "")
	if quality == QualityOriginalV3 && !capExceededBySource {
		result := originalQualityResultV3(source)
		result.Warnings = warnings
		return result
	}
	if rung, ok := ladderRungForLabelV3(quality); ok {
		return compoundRungQualityResultV3(rung, source, capKbps, warnings)
	}
	targetHeight := source.Height
	reason := "quality_auto_source"
	explicitRung := false
	capApplied := false
	// budgetKbps is the share of a bandwidth estimate or cap that automatic
	// quality plans for; the encode never targets more than it. A source over
	// the estimate's budget is re-encoded at its own size rather than sent
	// as-is; a cap keeps its own source rule below.
	budgetKbps := 0
	overEstimate := false
	// classLimit is the tallest class the preference, device, bandwidth or
	// cap allows, before the source's own height bounds targetHeight; 0 is
	// no limit. It keeps a cropped source in the class it earned.
	var classLimit int
	switch {
	case quality == QualityOriginalV3:
		// Only reached when the source bitrate exceeds the cap: the cap is a
		// hard ceiling and outranks the original preference.
		targetHeight = streamLadderClassV3(capKbps, source)
		classLimit = targetHeight
		capApplied = true
	case quality != "auto":
		targetHeight, _ = strconv.Atoi(strings.TrimSuffix(quality, "p"))
		classLimit = targetHeight
		reason = "quality_fixed_rung"
		explicitRung = true
	default:
		maxHeight := resolutionHeightV3(request.Capabilities.MaxResolution)
		if maxHeight > 0 && (targetHeight == 0 || maxHeight < targetHeight) {
			targetHeight = maxHeight
			reason = "quality_device_limit"
		}
		classLimit = maxHeight
		bandwidth := optionalValueV3(request.BandwidthEstimateKbps)
		fromEstimate := bandwidth > 0
		if capKbps > 0 && (bandwidth == 0 || capKbps < bandwidth) {
			bandwidth = capKbps
			fromEstimate = false
		}
		if bandwidth > 0 {
			earned := streamLadderClassV3(bandwidth, source)
			targetHeight = minPositiveV3(targetHeight, earned)
			classLimit = minPositiveV3(classLimit, earned)
			budgetKbps = streamBudgetKbpsV3(bandwidth)
			// A source of unknown bitrate counts as needing the full bitrate
			// of the class it would be sent at, so it goes as-is only when the
			// budget covers that.
			sourceKbps := source.BitrateKbps
			if sourceKbps <= 0 {
				sourceKbps = ladderClassBitrateKbpsV3(streamClassV3(minPositiveV3(targetHeight, source.Height), classLimit, source))
			}
			overEstimate = fromEstimate && sourceKbps > budgetKbps
			reason = "quality_bandwidth_limit"
		} else if request.Metered {
			classLimit = minPositiveV3(classLimit, 720)
			if capped := minPositiveV3(targetHeight, 720); capped != targetHeight {
				targetHeight = capped
				reason = "quality_metered_limit"
			}
		}
	}
	if targetHeight <= 0 {
		targetHeight = 1080
	}
	if source.Height > 0 && targetHeight > source.Height {
		targetHeight = source.Height
	}
	// The cap also constrains the rung chosen above: a rung that would
	// preserve the source is forced down when the source bitrate exceeds the
	// cap, and a transcode rung whose ladder bitrate exceeds the cap drops to
	// the cap's rung.
	if capKbps > 0 && !capApplied {
		if adjusted, applied := CappedRungHeightV3(targetHeight, source.Height, source.BitrateKbps, capKbps); applied {
			capApplied = true
			targetHeight = adjusted
		}
	}
	if capApplied {
		reason = decisionReasonBandwidthCapV3
		warnings = append(warnings, DegradationWarningV3{Code: "bandwidth_cap_applied", Message: "Delivery quality is limited by the configured bandwidth cap."})
	}
	if source.Height > 0 && targetHeight >= source.Height && !capApplied && !overEstimate {
		label, width, height := sourceFrameV3(source)
		return QualityResultV3{
			Label:           label,
			Width:           width,
			Height:          height,
			BitrateKbps:     source.BitrateKbps,
			PreservesSource: true,
			ExplicitRung:    explicitRung,
			Reason:          reason,
			Warnings:        warnings,
		}
	}
	// Snap to a ladder class and fit the source into that class's box, so a
	// scope film at the 1080p class is 1920x800. The label is the exact height
	// the encoder scales to.
	class := ladderClassesFrom(streamClassV3(targetHeight, classLimit, source))[0]
	width, effectiveHeight := FitLadderBox(source.Width, source.Height, class.Height)
	if effectiveHeight == 0 {
		effectiveHeight = class.Height
	}
	if width == 0 {
		width = class.Width
	}
	// A fit that keeps an odd source frame still encodes an even one.
	width, effectiveHeight = encodedFrame(source.Width, source.Height, width, effectiveHeight)
	label := heightLabel(effectiveHeight)
	bitrate := ladderClassBitrateKbpsV3(class.Height)
	if budgetKbps > 0 && bitrate > budgetKbps {
		// A class is earned at its floor, which can sit below the class
		// bitrate; keep the headroom the class choice assumed.
		bitrate = budgetKbps
	}
	if source.BitrateKbps > 0 {
		// A smaller frame never needs more bits than the whole source used,
		// counted as H.264, the output the planner starts from.
		sourceEquivalent := int(float64(source.BitrateKbps) / codecEfficiency(source.VideoCodec))
		bitrate = min(bitrate, max(sourceEquivalent, 1))
	}
	if capKbps > 0 && bitrate > capKbps {
		// The ladder has no rung below 480p, so a cap under the lowest rung's
		// bitrate is honored by lowering the encode target directly: the cap
		// is a hard delivery ceiling, never advisory.
		bitrate = capKbps
	}
	result := QualityResultV3{Label: label, Width: width, Height: effectiveHeight, BitrateKbps: bitrate, PreservesSource: !capApplied && !overEstimate && source.Height > 0 && effectiveHeight >= source.Height, ExplicitRung: explicitRung, Reason: reason, Warnings: warnings}
	result.RequiresTranscode = !result.PreservesSource
	return result
}

// compoundRungQualityResultV3 resolves one explicit menu step. Its resolution
// class never changes under a bandwidth cap; only the bitrate is clamped. For
// cinema-aspect sources (for example 3840x1540 UHD), a same-class rung keeps
// the probed dimensions instead of upscaling to the class's nominal height.
func compoundRungQualityResultV3(rung ladderRungV3, source SourceDescriptorV3, capKbps int, warnings []DegradationWarningV3) QualityResultV3 {
	sourceClassHeight := sourceLadderHeightV3(source)
	height := rung.Height
	sameResolutionClass := sourceClassHeight == rung.Height
	if source.Height > 0 && (sameResolutionClass || source.Height < height) {
		height = source.Height
	}
	bitrate := rung.BitrateKbps
	if source.BitrateKbps > 0 && source.BitrateKbps < bitrate {
		bitrate = source.BitrateKbps
	}
	capApplied := capKbps > 0 && bitrate > capKbps
	if capApplied {
		bitrate = capKbps
	}
	reason := "quality_fixed_rung"
	if capApplied {
		reason = decisionReasonBandwidthCapV3
		warnings = append(warnings, DegradationWarningV3{Code: "bandwidth_cap_applied", Message: "Delivery quality is limited by the configured bandwidth cap."})
	}
	fitsRung := sourceClassHeight > 0 && sourceClassHeight <= rung.Height && source.BitrateKbps > 0 && source.BitrateKbps <= rung.BitrateKbps
	if fitsRung && !capApplied {
		label, width, height := sourceFrameV3(source)
		return QualityResultV3{
			Label:           label,
			Width:           width,
			Height:          height,
			BitrateKbps:     source.BitrateKbps,
			PreservesSource: true,
			ExplicitRung:    true,
			Reason:          reason,
			Warnings:        warnings,
		}
	}
	width := 0
	if source.Width > 0 && source.Height > 0 {
		width = source.Width * height / source.Height
		width -= width % 2
	}
	if width == 0 {
		width, _ = dimensionsFromResolutionV3(resolutionLabelV3(height))
	}
	// Keep the clamped height exact. The transcoder scales to exactly this
	// height, so a source crop is preserved even on a lower-class rung.
	targetLabel := strconv.Itoa(height) + "p"
	return QualityResultV3{
		Label:             targetLabel,
		Width:             width,
		Height:            height,
		BitrateKbps:       bitrate,
		RequiresTranscode: true,
		ExplicitRung:      true,
		Reason:            reason,
		Warnings:          warnings,
	}
}

// sourceClassLadderRungV3 returns the highest-bitrate ladder rung for a
// resolution class. ladderRungsV3 is ordered by descending class and bitrate,
// so the first matching height is the class's ceiling (2160p -> 40 Mbps,
// 1080p -> 10 Mbps, 720p -> 4 Mbps).
func sourceClassLadderRungV3(classHeight int) (ladderRungV3, bool) {
	if classHeight <= 0 {
		return ladderRungV3{}, false
	}
	for _, rung := range ladderRungsV3 {
		if rung.Height == classHeight {
			return rung, true
		}
	}
	return ladderRungV3{}, false
}

// forcedEncodeQualityResultV3 replaces a source-preserving quality result with
// the source-class ladder rung when an otherwise source-preserving route is
// forced to encode (burn-in, HDR handling, or capability evidence). Without
// this the encode inherits the source bitrate — a ~76 Mbps "original quality"
// result — which no hardware encoder keeps up with in realtime. The compound
// rung machinery preserves the source's exact dimensions and clamps to a lower
// source bitrate when one exists.
// availableQualitiesForRouteV3 keeps source-preserving HDR planning lazy while
// still advertising the choices the configured policy allows. Selecting a
// lower HDR rung performs the executor capability lookup during the replan;
// building the menu itself never probes local or pooled executors.
func availableQualitiesForRouteV3(input PlannerInputV3, source SourceDescriptorV3) []AvailableQualityV3 {
	qualities := []AvailableQualityV3{{
		Label:           QualityOriginalV3,
		Height:          source.Height,
		BitrateKbps:     source.BitrateKbps,
		PreservesSource: true,
	}}
	if source.Height <= 0 {
		// Fixed rungs must sit strictly below a known source height; unknown
		// probe metadata cannot prove that any advertised rung avoids upscaling.
		return qualities
	}
	if !deliveryAvailableV3(input.Request, DeliveryClassHLSV3) || !input.Settings.TranscodeEnabled || input.Settings.ViewerTranscodeDisabled {
		return qualities
	}
	if is4KSourceV3(input.EffectiveFile, source) && !input.Settings.Allow4KTranscode {
		return qualities
	}
	if source.DynamicRange != "" && source.DynamicRange != DynamicRangeSDRV3 &&
		tonemap.NewPolicy(input.Settings.HardwareToneMapEnabled, input.Settings.SoftwareToneMapEnabled) == tonemap.PolicyNone {
		return qualities
	}
	for _, rung := range ladderRungsV3 {
		if !ladderRungPublishableV3(rung, source) {
			continue
		}
		qualities = append(qualities, AvailableQualityV3{
			Label:       rung.Label,
			DisplayName: rung.DisplayName,
			Height:      rung.Height,
			BitrateKbps: rung.BitrateKbps,
		})
	}
	return qualities
}

func forcedEncodeQualityResultV3(source SourceDescriptorV3, quality QualityResultV3) QualityResultV3 {
	rung, ok := sourceClassLadderRungV3(sourceLadderHeightV3(source))
	if !ok {
		return quality
	}
	replaced := compoundRungQualityResultV3(rung, source, 0, quality.Warnings)
	// The encode is happening regardless of whether the source already fit the
	// rung, so the result must not advertise a source-preserving route or a
	// user-selected fixed rung.
	replaced.PreservesSource = false
	replaced.RequiresTranscode = true
	replaced.ExplicitRung = quality.ExplicitRung
	replaced.Reason = quality.Reason
	return replaced
}

// originalQualityResultV3 keeps the source's frame. Its label is the exact
// source height: a class label would scale a 3840x1600 source to 1080 lines
// whenever the original route still needs a video transcode.
func originalQualityResultV3(source SourceDescriptorV3) QualityResultV3 {
	label, width, height := sourceFrameV3(source)
	return QualityResultV3{Label: label, Width: width, Height: height, BitrateKbps: source.BitrateKbps, PreservesSource: true, Reason: "quality_original"}
}

// streamClassV3 is the ladder class an automatic encode snaps into for a
// target height already bounded by the source. A target below the source is
// a downscale into the class at or below it. A target that keeps the source
// height uses the class the source's frame belongs to by both dimensions,
// within limit (0 for none), so a cropped 1920x800 film under a 1080p limit
// re-encodes at its own size in the 1080p class rather than dropping to 720p.
func streamClassV3(targetHeight, limit int, source SourceDescriptorV3) int {
	class := ladderClassesFrom(targetHeight)[0].Height
	if source.Height > 0 && targetHeight >= source.Height {
		if own := ladderClassForSize(source.Width, source.Height); own > 0 {
			class = own
			if limit > 0 {
				class = min(class, ladderClassesFrom(limit)[0].Height)
			}
		}
	}
	return class
}

// sourceFrameV3 is the frame a route that keeps the source size encodes when
// it still converts (a codec change): the source frame with an odd height
// rounded down to the even one 4:2:0 output needs, the width scale=-2 then
// gives, and that height as the label. An unknown height has no label.
func sourceFrameV3(source SourceDescriptorV3) (label string, width, height int) {
	if source.Height <= 0 {
		return "", source.Width, source.Height
	}
	width, height = encodedFrame(source.Width, source.Height, source.Width, source.Height)
	return heightLabel(height), width, height
}

// hlsNativeAudioCodecV3 reports whether an audio codec can be stream-copied
// into an HLS delivery. The allowlist follows the HLS authoring spec (AAC,
// AC-3, E-AC-3, MP3); everything else — DTS, TrueHD, PCM, Opus — must be
// converted even when the client can decode it in a progressive container.
func hlsNativeAudioCodecV3(codec string) bool {
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case "aac", "ac3", "eac3", "mp3":
		return true
	}
	return false
}

// hdrTranscodeUnavailableV3 mirrors planVideoTranscodeV3's terminal
// condition: no validated HDR-preserving or tone-map transcode recipe exists.
func hdrTranscodeUnavailableV3(input PlannerInputV3, source SourceDescriptorV3) bool {
	if source.DynamicRange == "" || source.DynamicRange == DynamicRangeSDRV3 {
		return false
	}
	return !resolveToneMapRecipeV3(input, source, nil).ok
}

type resolvedToneMapRecipeV3 struct {
	policy      tonemap.Policy
	mode        tonemap.Mode
	resolution  tonemap.SourceResolution
	revision    tonemap.SourceRevision
	hlsRegistry *TransformationRegistryV3
	ok          bool
}

// toneMapRecipeV3 freezes the policy, preferred validated executor, safe source
// resolution, and source revision required by an HDR video transcode plan.
func toneMapRecipeV3(input PlannerInputV3, source SourceDescriptorV3) (tonemap.Policy, tonemap.Mode, tonemap.SourceResolution, tonemap.SourceRevision, bool) {
	recipe := resolveToneMapRecipeV3(input, source, nil)
	return recipe.policy, recipe.mode, recipe.resolution, recipe.revision, recipe.ok
}

func resolveToneMapRecipeV3(input PlannerInputV3, source SourceDescriptorV3, hlsRegistry *TransformationRegistryV3) resolvedToneMapRecipeV3 {
	policy := tonemap.NewPolicy(input.Settings.HardwareToneMapEnabled, input.Settings.SoftwareToneMapEnabled)
	file := input.EffectiveFile
	if file == nil {
		file = input.RequestedFile
	}
	resolution := toneMapSourceResolutionV3(file, source)
	revision := tonemap.RevisionForFile(file)
	recipe := resolvedToneMapRecipeV3{policy: policy, resolution: resolution, revision: revision}
	if policy == tonemap.PolicyNone || resolution.Kind == "" {
		return recipe
	}
	if hlsRegistry == nil {
		hlsRegistry = input.hlsVideoRegistry()
	}
	recipe.hlsRegistry = hlsRegistry
	if hlsRegistry == nil || !hlsRegistry.Available(TransformationHDRToSDRToneMapV3) {
		return recipe
	}
	recipe.mode = input.hlsToneMapCapabilities().PreferredMode(policy, resolution.Kind)
	recipe.ok = recipe.mode != ""
	return recipe
}

// toneMapSourceResolutionV3 combines protocol source facts with the scanner's
// richer primary-track metadata before resolving a safe base signal.
func toneMapSourceResolutionV3(file *models.MediaFile, source SourceDescriptorV3) tonemap.SourceResolution {
	metadata := tonemap.SourceMetadata{
		DynamicRange: source.DynamicRange,
		DVProfile:    source.DVProfile,
		DVBLCompatID: source.DVBLCompatID,
	}
	if file != nil && len(file.VideoTracks) > 0 {
		track := file.VideoTracks[0]
		metadata.DVConfigPresent = track.DVConfigPresent
		metadata.DVBLCompatIDPresent = track.DVBLCompatIDPresent
		metadata.DVBLPresent = track.DVBLPresent
		metadata.DVRPUPresent = track.DVRPUPresent
		metadata.ColorRange = track.ColorRange
		metadata.ColorPrimaries = track.ColorPrimaries
		metadata.ColorTransfer = track.ColorTransfer
		metadata.ColorSpace = track.ColorSpace
	}
	return tonemap.ResolveSource(metadata)
}

// toneMapRecipeVersionV3 includes the transformation recipe version only when
// tone mapping is part of the executable plan.
func toneMapRecipeVersionV3(enabled bool) string {
	if enabled {
		return TransformationHDRToSDRToneMapRecipeVersionV3
	}
	return ""
}

// videoTranscodeExecutableV3 mirrors planVideoTranscodeV3's terminal
// preconditions: it reports whether a validated video transcode of this
// source could actually run for this client and configuration.
func videoTranscodeExecutableV3(input PlannerInputV3, source SourceDescriptorV3) bool {
	if !deliveryAvailableV3(input.Request, DeliveryClassHLSV3) || !input.Settings.TranscodeEnabled {
		return false
	}
	if is4KSourceV3(input.EffectiveFile, source) && !input.Settings.Allow4KTranscode {
		return false
	}
	if hdrTranscodeUnavailableV3(input, source) {
		return false
	}
	registry := input.hlsVideoRegistry()
	if registry == nil || !registry.Available(TransformationAudioToAACV3) {
		return false
	}
	_, videoOK := selectTargetVideoCodecV3(input, registry)
	return videoOK
}

func recipeFromSourceV3(source SourceDescriptorV3) EffectiveRecipeV3 {
	return EffectiveRecipeV3{VideoCodec: source.VideoCodec, AudioCodec: source.AudioCodec, Width: intPointerV3(source.Width), Height: intPointerV3(source.Height), FrameRate: floatPointerV3(source.FrameRate), BitrateKbps: intPointerV3(source.BitrateKbps), DynamicRange: source.DynamicRange, AudioChannels: intPointerV3(source.AudioChannels), AudioLayout: source.AudioLayout}
}

func selectedTracksForPlanV3(file *models.MediaFile, audioIndex int, subtitle SubtitlePolicyResultV3) SelectedTracksV3 {
	selected := SelectedTracksV3{}
	if file != nil && audioIndex >= 0 && audioIndex < len(file.AudioTracks) {
		index := audioIndex
		selected.Audio = &TrackIdentityV3{ID: TrackIDV3(file.ID, "audio", audioIndex), Index: &index}
	}
	if file != nil && subtitle.SelectedIndex >= 0 {
		index := subtitle.SelectedIndex
		selected.Subtitle = &TrackIdentityV3{ID: TrackIDV3(file.ID, "subtitle", index), Index: &index}
	}
	return selected
}

// audioInventoryV3 builds the plan's audio inventory at its canonical selection
// ordinals. The raw container stream index is preserved on each entry (it is
// what the serve path and the ffmpeg mapping use), while track_id and
// selection_index name the value a client must echo to select the track; they
// always agree with selected_tracks.audio and the audio_track_index request
// field. The input slice is never mutated; the catalog keeps its own tracks.
// AudioInventoryV3 builds the authoritative per-track audio inventory for file.
func AudioInventoryV3(file *models.MediaFile) []AudioInventoryItemV3 {
	return audioInventoryV3(file)
}

func audioInventoryV3(file *models.MediaFile) []AudioInventoryItemV3 {
	if file == nil || len(file.AudioTracks) == 0 {
		return nil
	}
	items := make([]AudioInventoryItemV3, len(file.AudioTracks))
	for i, track := range file.AudioTracks {
		items[i] = AudioInventoryItemV3{
			Index:          track.Index,
			Title:          track.Title,
			EmbeddedTitle:  track.EmbeddedTitle,
			Language:       track.Language,
			Languages:      track.Languages,
			Codec:          track.Codec,
			Profile:        track.Profile,
			Layout:         track.Layout,
			Channels:       track.Channels,
			Bitrate:        track.Bitrate,
			SampleRate:     track.SampleRate,
			BitDepth:       track.BitDepth,
			Default:        track.Default,
			TrackID:        TrackIDV3(file.ID, "audio", i),
			SelectionIndex: i,
		}
	}
	return items
}

// audioSelectionUsesContainerDefaultV3 reports whether an untouched source
// stream can realize the selected audio track without client-side selection.
// Clients that explicitly claim client_selected_audio_track_v1 on
// original_http may select another stream after probing the complete source.
func audioSelectionUsesContainerDefaultV3(file *models.MediaFile, audioIndex int) bool {
	if file == nil || len(file.AudioTracks) == 0 {
		return true
	}
	defaultIndex := 0
	for index, track := range file.AudioTracks {
		if track.Default {
			defaultIndex = index
			break
		}
	}
	if audioIndex < 0 || audioIndex >= len(file.AudioTracks) {
		audioIndex = defaultIndex
	}
	return audioIndex == defaultIndex
}

// effectiveVirtualURIV3 exposes the substituted virtual candidate URI on the
// plan. A neutral catalog row (virtual://movie/ttNNNN with no result=) is
// replaced by a probed candidate during planning; the plan's effective file ID
// already points at that candidate, but clients that key their version menu on
// the requested row need the URI too so a first play adopts the working
// version. It returns "" when the effective file is not a virtual candidate.
func effectiveVirtualURIV3(input PlannerInputV3) string {
	if input.EffectiveFile != nil && strings.HasPrefix(input.EffectiveFile.FilePath, "virtual://") {
		return input.EffectiveFile.FilePath
	}
	return ""
}

// virtualSourceRevisionPrefix domain-separates the revision hash so it can
// never collide with another hash of the same candidate identity.
const virtualSourceRevisionPrefix = "silo.virtual-source-revision.v2\x00"

// virtualSourceRevisionV3 returns an opaque, non-secret revision of the
// resolved virtual source candidate. It names the media generation: the
// `?result=` candidate pick plus the track-evidence generation that describes
// the bytes behind it (probe stamp, version, and a canonical fingerprint of
// every audio/subtitle/video track — never a provider URL, token, header, or
// refresh timestamp). Re-planning the same candidate with unchanged evidence
// yields the same revision; resolving a different candidate — or repairing the
// inventory under the same candidate id — yields a different one. Clients key
// source-change recovery on it, so a corrected inventory re-arms exactly like
// a rotation while a signed-URL renewal alone stays silent.
// It returns "" when the effective file is not a virtual candidate or carries
// no candidate identity (for example a neutral requested row that was planned
// without a substitution).
func virtualSourceRevisionV3(input PlannerInputV3) string {
	candidateIdentity := effectiveVirtualCandidateIdentityV3(input.EffectiveFile)
	if candidateIdentity == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(virtualSourceRevisionPrefix + candidateIdentity + "\x00" + virtualSourceEvidenceGenerationV3(input.EffectiveFile)))
	return hex.EncodeToString(sum[:12])
}

// virtualSourceEvidenceGenerationV3 folds the effective file's track-evidence
// generation into the source revision. A rematch adoption clears the probe
// stamp and rewrites the declared track inventory under the same `?result=`
// id (see adoptRematchedVirtualResolution), so the candidate identity alone
// cannot tell a corrected inventory from a stale one. The generation carries
// only stable, provider-neutral evidence: the probe timestamp (unix seconds,
// so sub-second DB rounding cannot flap the hash), the probe schema version,
// and a canonical fingerprint of every track that moves the plan — audio
// codec/layout/languages, subtitle language/codec/codec-equivalence/flags,
// video codec/profile/range/bit-depth/dimensions. Signed-URL renewals, refresh
// timestamps, and provider credentials are deliberately excluded.
func virtualSourceEvidenceGenerationV3(file *models.MediaFile) string {
	if file == nil {
		return ""
	}
	var b strings.Builder
	if file.ProbeUpdatedAt != nil && !file.ProbeUpdatedAt.IsZero() {
		b.WriteString(strconv.FormatInt(file.ProbeUpdatedAt.UTC().Unix(), 10))
	}
	b.WriteString("\x00")
	b.WriteString(strconv.Itoa(file.ProbeVersion))
	b.WriteString("\x00")
	for _, vt := range file.VideoTracks {
		b.WriteString(strings.ToLower(strings.TrimSpace(vt.Codec)))
		b.WriteString("\x00")
		b.WriteString(strings.ToLower(strings.TrimSpace(vt.Profile)))
		b.WriteString("\x00")
		b.WriteString(strings.ToLower(strings.TrimSpace(vt.VideoRange)))
		b.WriteString("\x00")
		b.WriteString(strings.ToLower(strings.TrimSpace(vt.VideoRangeType)))
		b.WriteString("\x00")
		b.WriteString(strconv.Itoa(vt.BitDepth))
		b.WriteString("\x00")
		b.WriteString(strconv.Itoa(vt.Width))
		b.WriteString("x")
		b.WriteString(strconv.Itoa(vt.Height))
		b.WriteString("\x00")
	}
	for _, at := range file.AudioTracks {
		b.WriteString(strings.ToLower(strings.TrimSpace(at.Codec)))
		b.WriteString("\x00")
		b.WriteString(strings.ToLower(strings.TrimSpace(at.Layout)))
		b.WriteString("\x00")
		b.WriteString(strconv.Itoa(at.Channels))
		b.WriteString("\x00")
		b.WriteString(strings.ToLower(strings.TrimSpace(at.Language)))
		b.WriteString("\x00")
		langs := append([]string(nil), at.Languages...)
		sort.Strings(langs)
		b.WriteString(strings.ToLower(strings.TrimSpace(strings.Join(langs, ","))))
		b.WriteString("\x00")
		b.WriteString(strconv.Itoa(at.Index))
		b.WriteString("\x00")
	}
	for _, st := range file.SubtitleTracks {
		b.WriteString(strings.ToLower(strings.TrimSpace(st.Language)))
		b.WriteString("\x00")
		b.WriteString(normalizeCodecV3(st.Codec))
		b.WriteString("\x00")
		b.WriteString(strings.ToLower(strings.TrimSpace(st.Title)))
		b.WriteString("\x00")
		b.WriteString(strings.ToLower(strings.TrimSpace(st.EmbeddedTitle)))
		b.WriteString("\x00")
		b.WriteString(path.Base(strings.TrimSpace(st.FileName)))
		b.WriteString("\x00")
		b.WriteString(strconv.FormatBool(st.Forced))
		b.WriteString("\x00")
		b.WriteString(strconv.FormatBool(st.HearingImpaired))
		b.WriteString("\x00")
		b.WriteString(strconv.Itoa(st.Index))
		b.WriteString("\x00")
	}
	for _, es := range file.ExternalSubtitles {
		b.WriteString(strings.ToLower(strings.TrimSpace(es.Language)))
		b.WriteString("\x00")
		b.WriteString(normalizeCodecV3(es.Format))
		b.WriteString("\x00")
		b.WriteString(path.Base(strings.TrimSpace(es.Path)))
		b.WriteString("\x00")
		b.WriteString(strconv.FormatBool(es.Forced))
		b.WriteString("\x00")
		b.WriteString(strconv.FormatBool(es.HearingImpaired))
		b.WriteString("\x00")
	}
	return b.String()
}

// effectiveVirtualCandidateIdentityV3 extracts the provider-neutral candidate
// fingerprint from a virtual effective file. The fingerprint is the resolver's
// `?result=` pick, which is itself an opaque hash of the candidate's stable
// stream fields (see virtuallibrary/stream.CandidateVariantID). A row without a
// pick has no identity to distinguish one candidate from another.
func effectiveVirtualCandidateIdentityV3(file *models.MediaFile) string {
	if file == nil || !strings.HasPrefix(strings.TrimSpace(file.FilePath), "virtual://") {
		return ""
	}
	parsed, err := url.Parse(file.FilePath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(parsed.Query().Get("result"))
}

func finalizePlanIdentityV3(plan *PlanV3, attemptID string, outputContextID string) {
	plan.PlanID = DeterministicPlanIDV3(attemptID, plan.RequestedMediaFileID, plan.EffectiveMediaFileID, *plan)
	plan.PlanAttemptKey = PlanAttemptKeyV3(*plan, outputContextID, nil)
}

// planAttemptedV3 compares FNV-hex attempt keys exactly after trimming
// whitespace; the keys are case-sensitive hashes, not free-form labels.
func planAttemptedV3(plan PlanV3, outputContextID string, attempted []string) bool {
	wanted := PlanAttemptKeyV3(plan, outputContextID, nil)
	for _, key := range attempted {
		if strings.TrimSpace(key) == wanted {
			return true
		}
	}
	return false
}

func terminalPlannerResultV3(reason, message string, retryable bool) PlannerResultV3 {
	return PlannerResultV3{Terminal: &TerminalV3{Reason: reason, Message: message, Retryable: retryable}, SubtitleTrackIndex: -1, SubtitleTransportTrackIndex: -1}
}

// terminalPlannerResultV3WithDetail is terminalPlannerResultV3 with a
// diagnostic detail line naming the probe fields that blocked the route.
func terminalPlannerResultV3WithDetail(reason, message, detail string, retryable bool) PlannerResultV3 {
	return PlannerResultV3{Terminal: &TerminalV3{Reason: reason, Message: message, Retryable: retryable, Detail: detail}, SubtitleTrackIndex: -1, SubtitleTransportTrackIndex: -1}
}

func subtitlePolicyNameV3(f SubtitleFidelityV3) string {
	if f == SubtitleFidelityPreserveV3 {
		return "require_authored_fidelity"
	}
	return "allow_simplified_rendering"
}
func floatOrZeroV3(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}
func intPointerV3(v int) *int {
	if v <= 0 {
		return nil
	}
	value := v
	return &value
}
func floatPointerV3(v float64) *float64 {
	if v <= 0 {
		return nil
	}
	value := v
	return &value
}
func optionalValueV3(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}
func resolutionHeightV3(v string) int {
	value, _ := strconv.Atoi(strings.TrimSuffix(strings.ToLower(v), "p"))
	if strings.EqualFold(v, "4k") {
		return 2160
	}
	if _, h, ok := parseWxHResolution(v); ok {
		return h
	}
	return value
}
func resolutionLabelV3(h int) string {
	switch {
	case h >= 2160:
		return "2160p"
	case h >= 1080:
		return "1080p"
	case h >= 720:
		return "720p"
	default:
		return "480p"
	}
}

// streamLadderClassV3 is the ladder class a streaming bandwidth budget
// earns for this source. Automatic quality keeps a fifth of the measured
// bandwidth as headroom and plans H.264, the output every route can decode.
func streamLadderClassV3(kbps int, source SourceDescriptorV3) int {
	return LadderClassForBitrate(streamBudgetKbpsV3(kbps), source.FrameRate, transcodeCodecH264)
}

// streamBudgetKbpsV3 is the part of a bandwidth figure automatic quality
// plans to use.
func streamBudgetKbpsV3(kbps int) int {
	return int(float64(kbps) * 0.8)
}
func ladderHeightForBandwidthV3(kbps int) int {
	switch {
	case kbps >= 20_000:
		return 2160
	case kbps >= 8_000:
		return 1080
	case kbps >= 4_000:
		return 720
	default:
		return 480
	}
}

func minPositiveV3(a, b int) int {
	if a <= 0 {
		return b
	}
	if b <= 0 || a < b {
		return a
	}
	return b
}

// CappedRungHeightV3 returns an explicit rung height after a bandwidth cap is
// applied exactly as ResolveQualityPolicyV3 applies it to a fixed rung: a rung
// that would preserve the source is lowered only when the source bitrate
// exceeds the cap, and a lower (transcode) rung is lowered only when its own
// ladder bitrate exceeds the cap. It returns the adjusted height and whether
// the cap constrained the rung.
//
// The virtual candidate picker shares this derivation so a native
// source-preserving stream under the cap is never displaced by a lower rung,
// while a stream whose bitrate exceeds the cap still falls to the cap's rung.
func CappedRungHeightV3(rungHeight, sourceHeight, sourceBitrateKbps, capKbps int) (int, bool) {
	targetHeight := rungHeight
	if sourceHeight > 0 && targetHeight > sourceHeight {
		targetHeight = sourceHeight
	}
	if capKbps <= 0 {
		return targetHeight, false
	}
	wouldPreserve := sourceHeight > 0 && targetHeight >= sourceHeight
	if (!wouldPreserve || sourceBitrateKbps <= capKbps) &&
		(wouldPreserve || ladderBitrateKbpsV3(targetHeight) <= capKbps) {
		return targetHeight, false
	}
	if capHeight := ladderHeightForBandwidthV3(int(float64(capKbps) * 0.8)); capHeight < targetHeight {
		targetHeight = capHeight
	}
	return targetHeight, true
}

// ladderBitrateKbpsV3 retains the established bitrate for each plain
// resolution preference. Explicit menu steps use ladderRungsV3 below.
func ladderBitrateKbpsV3(height int) int {
	bitrates := map[int]int{480: 1_500, 720: 2_000, 1080: 6_000, 2160: 20_000}
	return bitrates[resolutionHeightV3(resolutionLabelV3(height))]
}

// ladderClassBitrateKbpsV3 is the encode bitrate automatic quality and the
// plain resolution preferences use for a ladder class; a target between
// classes takes the class below it. Explicit menu steps use ladderRungsV3.
func ladderClassBitrateKbpsV3(height int) int {
	switch ladderClassesFrom(height)[0].Height {
	case 2160:
		return 20_000
	case 1080:
		return 6_000
	case 720:
		return 2_000
	case 540:
		return 1_800
	default:
		return 1_500
	}
}

type ladderRungV3 struct {
	Label       string
	DisplayName string
	Height      int
	BitrateKbps int
}

// ladderRungsV3 is the canonical menu ladder in descending resolution and
// bitrate order. The medium values retain the existing plain-rung bitrates;
// their compound labels make the bitrate constraint explicit at same height.
var ladderRungsV3 = []ladderRungV3{
	{Label: QualityRung2160pHighV3, DisplayName: "4K High", Height: 2160, BitrateKbps: 40_000},
	{Label: QualityRung2160pMediumV3, DisplayName: "4K Medium", Height: 2160, BitrateKbps: 20_000},
	{Label: QualityRung2160pLowV3, DisplayName: "4K Low", Height: 2160, BitrateKbps: 10_000},
	{Label: QualityRung1080pHighV3, DisplayName: "1080p High", Height: 1080, BitrateKbps: 10_000},
	{Label: QualityRung1080pMediumV3, DisplayName: "1080p Medium", Height: 1080, BitrateKbps: 6_000},
	{Label: QualityRung1080pLowV3, DisplayName: "1080p Low", Height: 1080, BitrateKbps: 3_000},
	{Label: QualityRung720pHighV3, DisplayName: "720p High", Height: 720, BitrateKbps: 4_000},
	{Label: QualityRung720pMediumV3, DisplayName: "720p Medium", Height: 720, BitrateKbps: 2_000},
	{Label: QualityRung720pLowV3, DisplayName: "720p Low", Height: 720, BitrateKbps: 1_500},
	{Label: "480p", DisplayName: "480p", Height: 480, BitrateKbps: 1_500},
}

func ladderRungForLabelV3(label string) (ladderRungV3, bool) {
	normalized := strings.ToLower(strings.TrimSpace(label))
	for _, rung := range ladderRungsV3 {
		if rung.Label == normalized {
			return rung, true
		}
	}
	return ladderRungV3{}, false
}

// sourceLadderHeightV3 classifies a source by the smallest class whose bounds
// contain both dimensions, the scanner's buckets for a file's resolution label
// (scanner.mapResolution). Cropped and cinema-aspect encodes therefore land in
// the class the catalog shows: 1918x872 is 1080p and 3840x1540 is 2160p. An 8K
// source is 4320p, above every rung, so its 4K rungs scale down to 2160 lines.
func sourceLadderHeightV3(source SourceDescriptorV3) int {
	switch {
	case source.Width <= 0 && source.Height <= 0:
		return 0
	case source.Width <= 854 && source.Height <= 480:
		return 480
	case source.Width <= 1280 && source.Height <= 962:
		return 720
	case source.Width <= 2560 && source.Height <= 1440:
		return 1080
	case source.Width <= 4096 && source.Height <= 3072:
		return 2160
	case source.Width <= 8192 && source.Height <= 6144:
		return 4320
	default:
		return 2160
	}
}

// ladderRungPublishableV3 avoids upscaling and pointless same-resolution
// re-encodes. Every lower resolution-class rung is useful; a same-class rung
// appears only when its bitrate is a real reduction from the source.
func ladderRungPublishableV3(rung ladderRungV3, source SourceDescriptorV3) bool {
	sourceHeight := sourceLadderHeightV3(source)
	if sourceHeight <= 0 {
		return false
	}
	if rung.Height < sourceHeight {
		return true
	}
	if rung.Height > sourceHeight || rung.Label == "480p" {
		return false
	}
	return source.BitrateKbps > 0 && rung.BitrateKbps < source.BitrateKbps
}

func SortedTransformationNamesV3(values []TransformationV3) []string {
	result := make([]string, 0, len(values))
	for _, v := range values {
		result = append(result, v.Name)
	}
	sort.Strings(result)
	return result
}

func deliveryAvailableV3(request StartRequestV3, deliveryClass string) bool {
	capability, ok := request.ClientPlaybackContext.Deliveries[deliveryClass]
	if !ok {
		return false
	}
	return capability.Enabled && capability.SupportedOnDevice
}

// cloneRemuxPlanCandidateV3 keeps progressive and HLS candidate mutations
// independent while preserving the common source and dynamic-range recipe.
func cloneRemuxPlanCandidateV3(plan PlanV3) PlanV3 {
	plan.Transformations = append([]TransformationV3{}, plan.Transformations...)
	plan.AppliedQuirks = append([]AppliedQuirkV3{}, plan.AppliedQuirks...)
	plan.RuntimeCorrections = append([]string{}, plan.RuntimeCorrections...)
	plan.DegradationWarnings = append([]DegradationWarningV3{}, plan.DegradationWarnings...)
	return plan
}

// deliveryAudioCodecSupportedV3 is the single scoped-audio rule shared by the
// pre-plan claim check and the post-recipe delivery check, so both agree on
// exactly which codec a delivery accepts for a given claim.
//
// A delivery that declares no audio lists at all keeps the caller's legacy
// fallback, because older clients use an absent list to mean "unspecified."
// When it does declare lists, they are authoritative subsets of the top-level
// device capabilities — with one deliberate exception for passthrough. A
// validated passthrough claim is a property of the sink (proven by exact
// evidence, a matching entry, and the layout-aware feature), not of the
// delivery: a delivery that declares decode-only lists says nothing about
// passthrough, so its empty passthrough list must not revoke the validated
// claim. Re-reading that silence as "no passthrough" forced an E-AC-3 sink
// that the server had just validated into an unnecessary E-AC-3 to AAC
// conversion on the remux route. When the delivery does declare passthrough
// codecs, they stay authoritative.
func deliveryAudioCodecSupportedV3(capability DeliveryCapabilityV3, codec string, claim AudioClaimsV3, fallback bool) bool {
	hasAudioConstraints := len(capability.AudioDecodeCodecs) > 0 || len(capability.AudioPassthroughCodecs) > 0
	if !hasAudioConstraints {
		return fallback
	}
	if claim.Passthrough {
		if len(capability.AudioPassthroughCodecs) == 0 {
			return true
		}
		return containsFoldV3(capability.AudioPassthroughCodecs, codec)
	}
	return containsFoldV3(capability.AudioDecodeCodecs, codec)
}

// deliverySupportsAudioClaimV3 narrows a device-wide audio claim to the active
// delivery when the client supplies scoped decode or passthrough lists.
func deliverySupportsAudioClaimV3(request StartRequestV3, deliveryClass, codec string, claim AudioClaimsV3, fallback bool) bool {
	capability, ok := request.ClientPlaybackContext.Deliveries[deliveryClass]
	if !ok || !capability.Enabled || !capability.SupportedOnDevice {
		return false
	}
	return deliveryAudioCodecSupportedV3(capability, codec, claim, fallback)
}

// deliveryExceedsMaxChannelsV3 reports whether an audio stream with the given
// channel count exceeds the delivery's max_channels ceiling. Planning and the
// finished-plan check in deliverySupportsPlanV3 share it; a ceiling of zero or
// less means unset, matching aacOutputChannelsV3.
func deliveryExceedsMaxChannelsV3(request StartRequestV3, deliveryClass string, channels int) bool {
	capability, ok := request.ClientPlaybackContext.Deliveries[deliveryClass]
	return ok && capability.MaxChannels != nil && *capability.MaxChannels > 0 && channels > *capability.MaxChannels
}

// audioRemuxFitsServerCapV3 reports whether a video-copy remux that converts
// the selected audio to AAC stays inside the administrator's bitrate cap, or
// the client's bandwidth cap when that is lower: the planner has already folded
// the two into Request.BandwidthCapKbps. The file's total bitrate already
// counts the audio track the AAC output replaces, so adding the full AAC rate
// is an upper bound. The source descriptor's rate
// can be the video track's alone and would undercount. An unknown total cannot
// be bounded and needs the budgeted transcode.
func audioRemuxFitsServerCapV3(input PlannerInputV3, file *models.MediaFile, aacChannels int) bool {
	if input.ServerBitrateCapKbps <= 0 {
		return true
	}
	totalBitrateKbps := 0
	if file != nil {
		totalBitrateKbps = normalizeBitrateKbpsV3(file.Bitrate)
	}
	if totalBitrateKbps <= 0 {
		return false
	}
	_, aacKbps := ResolveAACOutputV3(aacChannels, 0)
	return totalBitrateKbps+aacKbps <= optionalValueV3(input.Request.BandwidthCapKbps)
}

// deliverySupportsPlanV3 applies the capability limits scoped to the delivery
// class after a concrete recipe has been built. Empty lists preserve clients
// that only advertise class availability; non-empty lists are authoritative
// subsets of the top-level device capabilities.
func deliverySupportsPlanV3(request StartRequestV3, deliveryClass string, plan PlanV3) bool {
	capability, ok := request.ClientPlaybackContext.Deliveries[deliveryClass]
	if !ok || !capability.Enabled || !capability.SupportedOnDevice {
		return false
	}
	if len(capability.Containers) > 0 && !containsFoldV3(capability.Containers, plan.Stream.Container) {
		return false
	}
	if codec := strings.TrimSpace(plan.EffectiveRecipe.VideoCodec); codec != "" && len(capability.VideoCodecs) > 0 && !containsFoldV3(capability.VideoCodecs, codec) {
		return false
	}
	if codec := strings.TrimSpace(plan.EffectiveRecipe.AudioCodec); codec != "" {
		// A delivery that declares no audio lists is permissive here: the
		// classic behavior skips the check entirely. fallback=true preserves
		// that, while the scoped codec rule is shared with the pre-plan claim
		// check so the two cannot drift.
		if !deliveryAudioCodecSupportedV3(capability, codec, plan.Claims.Audio, true) {
			return false
		}
	}
	if plan.EffectiveRecipe.AudioChannels != nil && deliveryExceedsMaxChannelsV3(request, deliveryClass, *plan.EffectiveRecipe.AudioChannels) {
		return false
	}
	clientManagedOriginalRange := deliveryClass == DeliveryClassOriginalHTTPV3 &&
		len(plan.Transformations) == 0 &&
		containsFoldV3(capability.ValidatedClaims, ClaimClientManagedDynamicRangeV3)
	if capability.HDRDetails != nil && !hdrDetailsSupportPlanV3(*capability.HDRDetails, plan) && !clientManagedOriginalRange {
		return false
	}
	return true
}

func hdrDetailsSupportPlanV3(hdr HDRCapabilitiesV3, plan PlanV3) bool {
	switch plan.EffectiveRecipe.DynamicRange {
	case "", DynamicRangeSDRV3:
		return true
	case DynamicRangeHDR10V3, DynamicRangeHDRUnknownV3:
		return hdr.HDR10 && hdr10LimitsSupportPlanV3(hdr, plan)
	case DynamicRangeHDR10PlusV3:
		return hdr.HDR10Plus
	case DynamicRangeHLGV3:
		return hdr.HLG
	case DynamicRangeDolbyVisionV3:
		candidate := plan.Source
		for _, transformation := range plan.Transformations {
			if transformation.Name == ClientDV7ToDV81V3 {
				candidate.DVProfile = 8
				candidate.DVBLCompatID = 1
				break
			}
		}
		return hdrSupportsDolbyVisionSourceV3(hdr, candidate)
	default:
		return false
	}
}

func hdr10LimitsSupportPlanV3(hdr HDRCapabilitiesV3, plan PlanV3) bool {
	return !(hdr.HDR10MaxWidth > 0 && (plan.EffectiveRecipe.Width == nil || *plan.EffectiveRecipe.Width > hdr.HDR10MaxWidth) ||
		hdr.HDR10MaxHeight > 0 && (plan.EffectiveRecipe.Height == nil || *plan.EffectiveRecipe.Height > hdr.HDR10MaxHeight) ||
		hdr.HDR10MaxFrameRate > 0 && (plan.EffectiveRecipe.FrameRate == nil || *plan.EffectiveRecipe.FrameRate > hdr.HDR10MaxFrameRate) ||
		hdr.HDR10MaxBitrateKbps > 0 && (plan.EffectiveRecipe.BitrateKbps == nil || *plan.EffectiveRecipe.BitrateKbps > hdr.HDR10MaxBitrateKbps))
}

func hdrSupportsDolbyVisionSourceV3(hdr HDRCapabilitiesV3, source SourceDescriptorV3) bool {
	if !containsIntV3(hdr.DolbyVisionProfiles, source.DVProfile) {
		return false
	}
	var matchedCapability DolbyVisionProfileCapabilityV3
	foundCapability := false
	for _, capability := range hdr.DolbyVisionProfileLevels {
		if capability.Profile == source.DVProfile {
			matchedCapability = capability
			foundCapability = true
			break
		}
	}
	if !foundCapability {
		// Existing clients predate level-bounded Dolby Vision claims. Once a
		// client sends any bounds, every advertised profile must have one.
		return len(hdr.DolbyVisionProfileLevels) == 0
	}
	if len(matchedCapability.BLCompatibilityIDs) > 0 &&
		!containsIntV3(matchedCapability.BLCompatibilityIDs, source.DVBLCompatID) {
		return false
	}
	if source.DVLevel > 0 {
		return source.DVLevel <= matchedCapability.MaxLevel
	}
	return dolbyVisionSourceFitsLevelV3(source, matchedCapability.MaxLevel)
}

func dolbyVisionSourceFitsLevelV3(source SourceDescriptorV3, maxLevel int) bool {
	// Dolby Vision Version 2.0 defines levels by maximum pixel rate, decoded
	// width, and high-tier bitrate. Use those physical bounds for legacy rows
	// scanned before Silo persisted ffprobe's exact dv_level.
	type levelLimit struct {
		pixelRate   float64
		width       int
		bitrateKbps int
	}
	limits := map[int]levelLimit{
		1: {22_118_400, 1280, 50_000}, 2: {27_648_000, 1280, 50_000},
		3: {49_766_400, 1920, 70_000}, 4: {62_208_000, 2560, 70_000},
		5: {124_416_000, 3840, 70_000}, 6: {199_065_600, 3840, 130_000},
		7: {248_832_000, 3840, 130_000}, 8: {398_131_200, 3840, 130_000},
		9: {497_664_000, 3840, 130_000}, 10: {995_328_000, 3840, 240_000},
		11: {995_328_000, 7680, 240_000}, 12: {1_990_656_000, 7680, 480_000},
		13: {3_981_312_000, 7680, 800_000},
	}
	limit, ok := limits[maxLevel]
	if !ok || source.Width <= 0 || source.Height <= 0 || source.FrameRate <= 0 || source.BitrateKbps <= 0 {
		return false
	}
	return source.Width <= limit.width &&
		float64(source.Width*source.Height)*source.FrameRate <= limit.pixelRate &&
		source.BitrateKbps <= limit.bitrateKbps
}
func ExplainPlannerResultV3(result PlannerResultV3) string {
	if result.Plan != nil {
		return fmt.Sprintf("%s:%s", result.Plan.Delivery, result.Plan.DecisionReason)
	}
	if result.Terminal != nil {
		return "terminal:" + result.Terminal.Reason
	}
	return "invalid"
}
