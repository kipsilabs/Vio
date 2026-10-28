package playback

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/Silo-Server/silo-server/internal/models"
)

type SubtitlePolicyResultV3 struct {
	Decision             SubtitleDecisionV3
	Claims               SubtitleClaimsV3
	RequiresBurn         bool
	SelectedIndex        int
	TransportIndex       int
	Codec                string
	Source               string
	DownloadedSubtitleID int
	Terminal             *TerminalV3
	// Degraded reports that a positive selection could not be honored and
	// the policy fell back to subtitles-off instead of refusing playback.
	Degraded bool
}

// SubtitleInventoryEntryV3 is one subtitle track that exists for the file but
// is not carried on models.MediaFile — today, downloaded and AI-generated
// tracks. Callers supply them in the order that defines their ordinals (see
// BuildSubtitleInventoryV3); CombinedIndex records the entry's source-space
// ordinal so a caller holding an entry alone can still address it. The
// published ordinal BuildSubtitleInventoryV3 assigns may be lower once
// duplicates are suppressed; subtitleEntryAtCombinedIndexV3 translates between
// the two.
type SubtitleInventoryEntryV3 struct {
	CombinedIndex        int
	Codec                string
	Source               string
	Language             string
	Label                string
	Forced               bool
	HearingImpaired      bool
	DownloadedSubtitleID int `json:"-"`
}

// ResolveSubtitlePolicyV3 decides how the selected subtitle is delivered when
// the plan executes on the given delivery class. Renderability is
// delivery-specific, so callers must resolve the policy against the delivery
// class that will actually run the plan rather than assuming the
// original_http capabilities.
func ResolveSubtitlePolicyV3(file *models.MediaFile, request StartRequestV3, transcodeAllowed bool, deliveryClass string, additional []SubtitleInventoryEntryV3) SubtitlePolicyResultV3 {
	index := -1
	if request.SubtitleTrackIndex != nil {
		index = *request.SubtitleTrackIndex
	} else if request.SubtitleTrackID != "" {
		fileID, kind, ordinal, ok := ParseTrackIDV3(request.SubtitleTrackID)
		if !ok || kind != "subtitle" {
			return subtitleTerminalV3("subtitle_track_invalid", "The selected subtitle identity is invalid.")
		}
		if file != nil && fileID != file.ID {
			// A carried identity still bound to the version that produced it
			// names no track on the effective file. Degrade like any other
			// stale selection rather than terminalling the start.
			if deliveryClass == DeliveryClassOriginalHTTPV3 {
				slog.Info("carried subtitle identity belongs to a different file; subtitles disabled",
					"component", "playback", "file_id", file.ID, "carried_file_id", fileID)
			}
			return SubtitlePolicyResultV3{Decision: SubtitleDecisionV3{Mode: SubtitleOffV3}, SelectedIndex: -1, TransportIndex: -1}
		}
		index = ordinal
	}
	if index < 0 {
		return SubtitlePolicyResultV3{Decision: SubtitleDecisionV3{Mode: SubtitleOffV3}, SelectedIndex: -1, TransportIndex: -1}
	}
	if file == nil {
		return subtitleTerminalV3("subtitle_track_unavailable", "The selected subtitle inventory is unavailable.")
	}
	entry, ok := subtitleEntryAtCombinedIndexV3(file, index, additional)
	if !ok {
		if deliveryClass == DeliveryClassOriginalHTTPV3 {
			slog.Info("subtitle selection out of range for effective file; subtitles disabled",
				"component", "playback", "file_id", file.ID, "subtitle_track_index", index)
		}
		return SubtitlePolicyResultV3{
			Decision:       SubtitleDecisionV3{Mode: SubtitleOffV3},
			SelectedIndex:  -1,
			TransportIndex: -1,
			Degraded:       true,
		}
	}
	codec, source := entry.Codec, entry.Source
	trackID := TrackIDV3(file.ID, "subtitle", index)
	transportIndex := -1
	if source == "embedded" {
		// entry.CombinedIndex is the source-space ordinal; the transport index
		// is the embedded track's position within file.SubtitleTracks. The
		// request's index is the dense published one, which can differ when the
		// inventory de-duplicated an earlier track.
		transportIndex = entry.CombinedIndex - len(file.ExternalSubtitles)
	}
	deliveryCaps := request.ClientPlaybackContext.Deliveries[deliveryClass]
	text := isTextSubtitleV3(codec)
	ass := IsASS(codec)
	clientBitmap := isClientRenderableBitmapSubtitleV3(codec)
	burnInBitmap := clientBitmap || normalizeCodecV3(codec) == "dvb_teletext"
	if !text && !burnInBitmap {
		// Unknown codecs (arib_caption, ...) have no validated client render
		// path and no validated burn-in filter; promising a silent burn-in
		// would produce a plan the transcoder cannot honor.
		return subtitleTerminalV3("subtitle_codec_unsupported", fmt.Sprintf("Subtitle format %s has no validated rendering or burn-in route.", codec))
	}
	if source == SubtitleSourceEmbeddedV3 && (text || clientBitmap) {
		if native, nativeCaps, ok := nativeEmbeddedSubtitleV3(file, request, deliveryClass, transportIndex); ok {
			return SubtitlePolicyResultV3{
				Decision:      SubtitleDecisionV3{Mode: SubtitleRenderV3, TrackID: trackID, Embedded: native},
				Claims:        SubtitleClaimsV3{ASSStylingPreserved: !ass || nativeCaps.ASSStyling, Reason: "client_embedded_subtitle_supported"},
				SelectedIndex: index, TransportIndex: transportIndex, Codec: codec, Source: source,
			}
		}
	}
	if text {
		renderable := deliveryCaps.Subtitles.SidecarText
		if ass && request.SubtitleFidelityPreference == SubtitleFidelityPreserveV3 {
			renderable = renderable && deliveryCaps.Subtitles.ASSStyling && deliveryCaps.Subtitles.FontAttachments
		}
		if renderable {
			return SubtitlePolicyResultV3{
				Decision:      SubtitleDecisionV3{Mode: SubtitleRenderV3, TrackID: trackID},
				Claims:        SubtitleClaimsV3{ASSStylingPreserved: !ass || deliveryCaps.Subtitles.ASSStyling, Reason: "client_render_supported"},
				SelectedIndex: index, TransportIndex: transportIndex, Codec: codec, Source: source,
				DownloadedSubtitleID: entry.DownloadedSubtitleID,
			}
		}
		if request.SubtitleFidelityPreference == SubtitleFidelityCompatibleV3 && deliveryCaps.Subtitles.SidecarText {
			return SubtitlePolicyResultV3{
				Decision:      SubtitleDecisionV3{Mode: SubtitleConvertV3, TrackID: trackID},
				Claims:        SubtitleClaimsV3{Reason: "server_text_conversion"},
				SelectedIndex: index, TransportIndex: transportIndex, Codec: codec, Source: source,
				DownloadedSubtitleID: entry.DownloadedSubtitleID,
			}
		}
	}
	// The stream handler raw-serves exactly one bitmap sidecar shape: an
	// embedded PGS track as .sup. External/downloaded bitmap tracks go through
	// WebVTT conversion (impossible for bitmaps) and embedded DVD/DVB have no
	// client-renderable representation at all, so promising a sidecar for
	// anything broader publishes an artifact URL that always fails at fetch.
	// Everything else falls through to burn-in or its terminal.
	if clientBitmap && source == "embedded" && IsPGS(codec) && deliveryCaps.Subtitles.EmbeddedBitmap {
		return SubtitlePolicyResultV3{
			Decision:      SubtitleDecisionV3{Mode: SubtitleRenderV3, TrackID: trackID},
			Claims:        SubtitleClaimsV3{BitmapSidecar: true, Reason: "client_bitmap_render_supported"},
			SelectedIndex: index, TransportIndex: transportIndex, Codec: codec, Source: source,
			DownloadedSubtitleID: entry.DownloadedSubtitleID,
		}
	}
	if transcodeAllowed {
		if source != "embedded" {
			return subtitleTerminalV3("subtitle_burn_in_source_unsupported", "The selected subtitle source cannot be burned in by the installed transport.")
		}
		return SubtitlePolicyResultV3{
			Decision:     SubtitleDecisionV3{Mode: SubtitleBurnInV3, TrackID: trackID},
			Claims:       SubtitleClaimsV3{BitmapOverlay: burnInBitmap, Reason: "server_burn_in_required"},
			RequiresBurn: true, SelectedIndex: index, TransportIndex: transportIndex, Codec: codec, Source: source,
			DownloadedSubtitleID: entry.DownloadedSubtitleID,
		}
	}
	return subtitleTerminalV3("subtitle_conversion_unsupported", fmt.Sprintf("Subtitle format %s cannot meet the selected fidelity policy.", codec))
}

// subtitleEntryAtCombinedIndexV3 resolves a published combined ordinal to the
// source-space entry it names. The ordinal the client echoes is the dense one
// BuildSubtitleInventoryV3 publishes, but the file's own arrays and the
// additional entries keep their source ordinals, so the published ordinal is
// translated back through the same de-duplication before the segment lookup.
// The returned entry carries the source-space ordinal in CombinedIndex, which
// the policy uses for the embedded transport index and the extraction pins.
func subtitleEntryAtCombinedIndexV3(file *models.MediaFile, index int, additional []SubtitleInventoryEntryV3) (SubtitleInventoryEntryV3, bool) {
	if file == nil || index < 0 {
		return SubtitleInventoryEntryV3{}, false
	}
	sourceIndex, ok := subtitleInventorySourceIndexV3(file, additional, index)
	if !ok {
		return SubtitleInventoryEntryV3{}, false
	}
	if sourceIndex < len(file.ExternalSubtitles) {
		return SubtitleInventoryEntryV3{CombinedIndex: sourceIndex, Codec: normalizeCodecV3(file.ExternalSubtitles[sourceIndex].Format), Source: "external"}, true
	}
	embedded := sourceIndex - len(file.ExternalSubtitles)
	if embedded >= 0 && embedded < len(file.SubtitleTracks) {
		return SubtitleInventoryEntryV3{CombinedIndex: sourceIndex, Codec: normalizeCodecV3(file.SubtitleTracks[embedded].Codec), Source: "embedded"}, true
	}
	// Match the additional entry by the same source ordinal the builder
	// assigned it: CombinedIndex when it is positive, otherwise the entry's
	// position after the file's own ranges.
	base := len(file.ExternalSubtitles) + len(file.SubtitleTracks)
	for position, entry := range additional {
		assigned := entry.CombinedIndex
		if assigned <= 0 {
			assigned = base + position
		}
		if assigned == sourceIndex {
			entry.Codec = normalizeCodecV3(entry.Codec)
			return entry, true
		}
	}
	return SubtitleInventoryEntryV3{}, false
}

// SubtitleEvaluationKeyV3 names one subtitle evaluation: the selection a request
// carries and the effective inventory that selection was placed against. A replan
// that reproduces both is re-evaluating an outcome its predecessor plan already
// published, so it can reuse that outcome instead of re-running the policy.
type SubtitleEvaluationKeyV3 struct {
	// SelectionOrdinal is the combined ordinal the request selected, or -1 when
	// it selects nothing (subtitles off or no subtitle identity).
	SelectionOrdinal int
	// EffectiveFileID is the row whose inventory the ordinal was placed against.
	// Including it keeps a candidate rotation re-evaluating even when the sibling
	// publishes an identical list.
	EffectiveFileID int
	// InventoryDigest is the canonical digest of that effective inventory,
	// including the downloaded tail.
	InventoryDigest string
}

// SubtitleEvaluationV3 is a predecessor plan's published subtitle outcome for one
// key. Only the degraded (out-of-range) outcome is carried: it is the one whose
// warning is logged, so replaying it on an unchanged replan would duplicate a
// warning the first plan already recorded. A successful selection resolves
// silently anyway and needs no reuse.
type SubtitleEvaluationV3 struct {
	Key      SubtitleEvaluationKeyV3
	Disabled bool
}

// PreviousSubtitleEvaluationV3 builds the reuse record of a plan's degraded
// subtitle outcome for the request that produced it. It returns nil when the plan
// published a subtitle, selected subtitles off deliberately, or carries no
// subtitle-unavailable warning: none of those is the out-of-range disable this
// reuse exists to suppress.
func PreviousSubtitleEvaluationV3(request StartRequestV3, plan PlanV3) *SubtitleEvaluationV3 {
	if plan.Subtitle.Mode != SubtitleOffV3 ||
		!slices.ContainsFunc(plan.DegradationWarnings, func(warning DegradationWarningV3) bool {
			return warning.Code == DegradationWarningSubtitleTrackUnavailableV3
		}) {
		return nil
	}
	ordinal := subtitleEvaluationSelectionOrdinalV3(request)
	if ordinal < 0 {
		return nil
	}
	return &SubtitleEvaluationV3{
		Key: SubtitleEvaluationKeyV3{
			SelectionOrdinal: ordinal,
			EffectiveFileID:  plan.EffectiveMediaFileID,
			InventoryDigest:  SubtitleInventoryDigestV3(plan.Subtitle.Inventory),
		},
		Disabled: true,
	}
}

// subtitleEvaluationSelectionOrdinalV3 is the combined ordinal a request
// selects, normalized from either identity form so an index-only selection and
// the equivalent track_id compare equal. -1 means no selection.
func subtitleEvaluationSelectionOrdinalV3(request StartRequestV3) int {
	if request.SubtitleTrackIndex != nil {
		if *request.SubtitleTrackIndex < 0 {
			return -1
		}
		return *request.SubtitleTrackIndex
	}
	if request.SubtitleTrackID == "" {
		return -1
	}
	_, kind, ordinal, ok := ParseTrackIDV3(request.SubtitleTrackID)
	if !ok || kind != "subtitle" || ordinal < 0 {
		return -1
	}
	return ordinal
}

// SubtitleInventoryDigestV3 is a deterministic digest of the selectable subtitle
// inventory. It covers every field a selection or a carry can key on, plus the
// downloaded row identity, and deliberately excludes session URLs: those change
// with the session, not with the inventory, so including them would defeat the
// comparison.
func SubtitleInventoryDigestV3(items []SubtitleInventoryItemV3) string {
	digest := sha256.New()
	for _, item := range items {
		// Each entry is self-delimiting: the trailing marker separates it from a
		// following entry, and every field is fixed for a given shape.
		fmt.Fprintf(digest, "%d\x00%s\x00%s\x00%s\x00%s\x00%t\x00%t\x00%t\x00%s\x00%d\x00%s\x00\x01",
			item.CombinedIndex, item.Source, item.Codec, item.Language, item.Label,
			item.Forced, item.Default, item.HearingImpaired, item.Delivery,
			item.downloadedSubtitleID, item.SyncKey)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// reusedSubtitleDegradeV3 reports whether the predecessor plan's out-of-range
// disable already covers this request, and returns the outcome to publish
// silently if so. It requires the identical selection against an identical
// effective inventory on the identical effective row: a changed pick, a changed
// inventory, or a candidate rotation re-evaluates through the normal policy.
func reusedSubtitleDegradeV3(input PlannerInputV3, file *models.MediaFile, inventory []SubtitleInventoryItemV3) (SubtitlePolicyResultV3, bool) {
	previous := input.PreviousSubtitleEvaluation
	if previous == nil || !previous.Disabled || file == nil {
		return SubtitlePolicyResultV3{}, false
	}
	if previous.Key.SelectionOrdinal < 0 ||
		subtitleEvaluationSelectionOrdinalV3(input.Request) != previous.Key.SelectionOrdinal {
		return SubtitlePolicyResultV3{}, false
	}
	if previous.Key.EffectiveFileID != file.ID ||
		previous.Key.InventoryDigest != SubtitleInventoryDigestV3(inventory) {
		return SubtitlePolicyResultV3{}, false
	}
	return SubtitlePolicyResultV3{
		Decision:       SubtitleDecisionV3{Mode: SubtitleOffV3},
		SelectedIndex:  -1,
		TransportIndex: -1,
		Degraded:       true,
	}, true
}

// isTextSubtitleV3 reports whether a codec delivers as text the client can
// render (or the server can convert) without burn-in.
func isTextSubtitleV3(codec string) bool {
	switch normalizeCodecV3(codec) {
	case "srt", "subrip", "vtt", "webvtt", "ass", "ssa", "mov_text", "tx3g",
		"eia_608", "eia608", "cea_608", "cea608":
		return true
	default:
		return false
	}
}

// SubtitleFormatDeliverableV3 reports whether the v3 subtitle policy can
// deliver a track in this format at all: as text, or by burning in a bitmap.
func SubtitleFormatDeliverableV3(codec string) bool {
	return isTextSubtitleV3(codec) || NeedsBurnIn(codec) || normalizeCodecV3(codec) == "dvb_teletext"
}

func isClientRenderableBitmapSubtitleV3(codec string) bool {
	// normalizeCodecV3 centralizes the short spellings carried by older rows;
	// the bitmap policy can then use the same canonical set as burn-in.
	return NeedsBurnIn(codec)
}

func subtitleTerminalV3(reason, message string) SubtitlePolicyResultV3 {
	return SubtitlePolicyResultV3{Decision: SubtitleDecisionV3{Mode: SubtitleOffV3}, SelectedIndex: -1, TransportIndex: -1, Terminal: &TerminalV3{Reason: reason, Message: message}}
}

func nativeEmbeddedSubtitleV3(file *models.MediaFile, request StartRequestV3, deliveryClass string, ordinal int) (*EmbeddedSubtitleV3, NativeEmbeddedSubtitleCapabilityV3, bool) {
	var none NativeEmbeddedSubtitleCapabilityV3
	if deliveryClass != DeliveryClassOriginalHTTPV3 || !HasFeatureV3(request.ClientFeatures, FeatureEmbeddedSubtitlesV3) || ordinal < 0 || ordinal >= len(file.SubtitleTracks) {
		return nil, none, false
	}
	delivery := request.ClientPlaybackContext.Deliveries[deliveryClass]
	if !delivery.Enabled || !delivery.SupportedOnDevice {
		return nil, none, false
	}
	track := file.SubtitleTracks[ordinal]
	if track.Index < 0 {
		return nil, none, false
	}
	// Reject ambiguous source metadata instead of selecting a different stream.
	for i, other := range file.SubtitleTracks {
		if i != ordinal && other.Index == track.Index {
			return nil, none, false
		}
	}
	for _, capability := range delivery.Subtitles.NativeEmbedded {
		if !strings.EqualFold(capability.Container, file.Container) {
			continue
		}
		if !slices.ContainsFunc(capability.Codecs, func(codec string) bool {
			return normalizeNativeSubtitleCodecV3(codec) == normalizeNativeSubtitleCodecV3(track.Codec)
		}) {
			continue
		}
		if IsASS(track.Codec) && request.SubtitleFidelityPreference == SubtitleFidelityPreserveV3 && (!capability.ASSStyling || !capability.FontAttachments) {
			continue
		}
		switch capability.TrackIdentity {
		case subtitleIdentityFFmpegV3:
		case subtitleIdentityContainerV3:
			id, err := strconv.ParseUint(track.ContainerTrackID, 10, 32)
			if err != nil || id == 0 || strconv.FormatUint(id, 10) != track.ContainerTrackID {
				continue
			}
			ambiguous := false
			for i, other := range file.SubtitleTracks {
				if i != ordinal && other.ContainerTrackID == track.ContainerTrackID {
					ambiguous = true
				}
			}
			if ambiguous {
				continue
			}
		default:
			continue
		}
		return &EmbeddedSubtitleV3{StreamIndex: track.Index, ContainerTrackID: track.ContainerTrackID}, capability, true
	}
	return nil, none, false
}

func normalizeNativeSubtitleCodecV3(codec string) string {
	switch codec = normalizeCodecV3(codec); codec {
	case "srt":
		return "subrip"
	case "vtt":
		return "webvtt"
	case "tx3g":
		return "mov_text"
	case subtitleCodecPGS:
		return subtitleCodecPGSFFmpeg
	default:
		return codec
	}
}
