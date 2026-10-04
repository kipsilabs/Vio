package quality

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"regexp/syntax"
	"sort"
	"strings"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary/stream"
)

const (
	maxQualityProfiles       = 10
	maxCustomFormats         = 50
	maxProfileLabelBytes     = 128
	maxProfileRegexBytes     = 1024
	maxProfileAttributeBytes = 64
	maxPreferredOrder        = 10000
	// maxSortCriteria bounds one profile's sort list. Sixteen ordered keys is
	// far past any usable UI while still bounding the per-comparison work.
	maxSortCriteria = 16

	// patternTypeToken selects word-boundary keyword matching instead of
	// regex for a custom format (AltMount parity).
	patternTypeToken = "token"
	patternTypeRegex = "regex"

	// HDR10 classifier values emitted by stream.ParseStreamDetails. The generic
	// "hdr" requirement and exclusion cover this family, so its members are
	// named once rather than repeated as literals.
	hdrValueHDR10     = "hdr10"
	hdrValueHDR10Plus = "hdr10+"
	hdrValueDV        = "dv"
)

type CustomFormat struct {
	Name   string `json:"name"`
	Regex  string `json:"regex"`
	Score  int    `json:"score"`
	Reject bool   `json:"reject"`

	// AltMount TRaSH parity fields (all optional, additive-only).
	// ID is a stable rule identifier, Category groups rules in the admin
	// UI (source/hdr/audio/release_group/resolution/custom), Pattern is
	// the primary match expression (Regex kept as legacy alias),
	// PatternType is "regex" (default) or "token" (word-boundary keyword
	// match that avoids substring false positives like TS in DTS-HD),
	// Enabled skips the rule when false (defaults true when absent),
	// IsCustom marks user-defined rules preserved across preset switches,
	// Invert flips the match (rule scores when the pattern does NOT match).
	ID          string `json:"id,omitempty"`
	Category    string `json:"category,omitempty"`
	Pattern     string `json:"pattern,omitempty"`
	PatternType string `json:"pattern_type,omitempty"`
	Enabled     bool   `json:"enabled"`
	IsCustom    bool   `json:"is_custom,omitempty"`
	Invert      bool   `json:"invert,omitempty"`

	match *regexp.Regexp
}

// Compiled returns the compiled match regex, or nil if unset.
func (f *CustomFormat) Compiled() *regexp.Regexp { return f.match }

// discardScoreThreshold mirrors AltMount's hard-discard line: any matching
// format scoring at or below this rejects the candidate even without
// Reject=true.
const discardScoreThreshold = -1500

// EffectivePattern prefers Pattern (AltMount field) over Regex (legacy Vio
// field) so one rule representation serves both.
func (f *CustomFormat) EffectivePattern() string {
	if strings.TrimSpace(f.Pattern) != "" {
		return strings.TrimSpace(f.Pattern)
	}
	return strings.TrimSpace(f.Regex)
}

// IsEnabled reports whether the rule participates in scoring. Legacy rules
// built before the Enabled field existed (zero value, no AltMount fields)
// default to enabled so old configs keep working without migration.
func (f *CustomFormat) IsEnabled() bool {
	if f.Enabled {
		return true
	}
	return f.PatternType == "" && f.ID == "" && f.Category == "" &&
		strings.TrimSpace(f.Pattern) == "" && !f.Invert && !f.IsCustom
}

// UnmarshalJSON accepts both legacy (regex) and AltMount (pattern,
// pattern_type/patternType, enabled, is_custom/isCustom, category, id)
// spellings. Enabled defaults to true when the key is absent so legacy
// configs without the field keep scoring.
func (f *CustomFormat) UnmarshalJSON(data []byte) error {
	type rawFormat struct {
		Name         string `json:"name"`
		Regex        string `json:"regex"`
		Pattern      string `json:"pattern"`
		Score        int    `json:"score"`
		Reject       bool   `json:"reject"`
		ID           string `json:"id"`
		Category     string `json:"category"`
		PatternType  string `json:"pattern_type"`
		PatternTypeC string `json:"patternType"`
		IsCustom     bool   `json:"is_custom"`
		IsCustomC    bool   `json:"isCustom"`
		Invert       bool   `json:"invert"`
		Enabled      *bool  `json:"enabled"`
	}
	var raw rawFormat
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	f.Name = raw.Name
	f.Regex = raw.Regex
	f.Pattern = raw.Pattern
	f.Score = raw.Score
	f.Reject = raw.Reject
	f.ID = raw.ID
	f.Category = raw.Category
	f.PatternType = raw.PatternType
	if f.PatternType == "" {
		f.PatternType = raw.PatternTypeC
	}
	f.IsCustom = raw.IsCustom || raw.IsCustomC
	f.Invert = raw.Invert
	if raw.Enabled == nil {
		f.Enabled = true
	} else {
		f.Enabled = *raw.Enabled
	}
	return nil
}

// SortCriterion is one ordered ranking key in a quality profile. Attribute
// names a candidate field; Direction is "desc" (largest first, numeric
// attributes) or "asc" (best/lowest rank first, ordinal attributes). An
// absent direction uses the attribute's default.
type SortCriterion struct {
	Attribute string `json:"attribute"` // size|bitrate|resolution|audio_channels|bit_depth|hdr|source|score|confirmed|language
	Direction string `json:"direction"` // desc|asc
}

type QualityProfile struct {
	Label             string  `json:"label"`
	Resolution        string  `json:"resolution"`
	IncludeRegex      string  `json:"include_regex"`
	ExcludeRegex      string  `json:"exclude_regex"`
	PreferredOrder    int     `json:"preferred_order"`
	CodecVideo        string  `json:"codec_video"`
	CodecAudio        string  `json:"codec_audio"`
	HDR               string  `json:"hdr"`
	ExcludeHDR        string  `json:"exclude_hdr"`
	AudioChannels     string  `json:"audio_channels,omitempty"`
	Language          string  `json:"language,omitempty"`
	VisualTag         string  `json:"visual_tag,omitempty"`
	MinSizeGB         float64 `json:"min_size_gb,omitempty"`
	MaxSizeGB         float64 `json:"max_size_gb,omitempty"`
	MinSize           int64   `json:"min_size,omitempty"`
	MaxSize           int64   `json:"max_size,omitempty"`
	RequireMultiAudio bool    `json:"require_multi_audio,omitempty"`
	// Sort is the ordered ranking key list, top-down. Absent (the zero value)
	// keeps the package's default ranking, so a stored profile without it
	// ranks exactly as it did before Sort existed.
	Sort []SortCriterion `json:"sort,omitempty"`

	include *regexp.Regexp
	exclude *regexp.Regexp
}

// IncludeCompiled returns the compiled include regex, or nil if unset.
func (p *QualityProfile) IncludeCompiled() *regexp.Regexp { return p.include }

// ExcludeCompiled returns the compiled exclude regex, or nil if unset.
func (p *QualityProfile) ExcludeCompiled() *regexp.Regexp { return p.exclude }

type QualityConfig struct {
	Preset                   string           `json:"quality_preset"`
	CustomFormatPreset       string           `json:"custom_format_preset"`
	EnableProfiles           bool             `json:"enable_quality_profiles"`
	Profiles                 []QualityProfile `json:"quality_profiles"`
	CustomFormats            []CustomFormat   `json:"custom_formats"`
	FallbackToAnyStream      bool             `json:"fallback_to_any_stream"`
	SingleStreamWithFailover bool             `json:"single_stream_with_failover"`
}

const defaultQualityPreset = "custom"

// qualityPresetProfiles keeps the common choices understandable in the admin
// form while leaving regex-level control available through Custom.
func qualityPresetProfiles(preset string) []QualityProfile {
	switch strings.ToLower(strings.TrimSpace(preset)) {
	case "balanced":
		return []QualityProfile{{Label: "1080p", Resolution: "1080p", PreferredOrder: 1}}
	case "4k-hdr":
		return []QualityProfile{{Label: "4K HDR", Resolution: "2160p", HDR: "hdr", PreferredOrder: 1}, {Label: "1080p", Resolution: "1080p", PreferredOrder: 2}}
	case "4k-dolby-vision":
		return []QualityProfile{{Label: "4K Dolby Vision", Resolution: "2160p", HDR: "dv", PreferredOrder: 1}, {Label: "1080p", Resolution: "1080p", PreferredOrder: 2}}
	case "no-dolby-vision":
		return []QualityProfile{{Label: "4K HDR10", Resolution: "2160p", ExcludeHDR: "dv", ExcludeRegex: `(?i)(dolby[ ._-]*vision|\bdv\b|dovi)`, PreferredOrder: 1}, {Label: "1080p", Resolution: "1080p", ExcludeHDR: "dv", ExcludeRegex: `(?i)(dolby[ ._-]*vision|\bdv\b|dovi)`, PreferredOrder: 2}}
	case "no-hdr":
		return []QualityProfile{{Label: "4K SDR", Resolution: "2160p", ExcludeHDR: "*", ExcludeRegex: `(?i)(hdr|dolby[ ._-]*vision|\bdv\b|dovi|hlg)`, PreferredOrder: 1}, {Label: "1080p SDR", Resolution: "1080p", ExcludeHDR: "*", ExcludeRegex: `(?i)(hdr|dolby[ ._-]*vision|\bdv\b|dovi|hlg)`, PreferredOrder: 2}}
	case "compatibility":
		return []QualityProfile{{Label: "1080p Compatible", Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac", PreferredOrder: 1}, {Label: "720p Compatible", Resolution: "720p", CodecVideo: "h264", CodecAudio: "aac", PreferredOrder: 2}}
	case "anime":
		return []QualityProfile{{Label: "Anime 1080p", Resolution: "1080p", IncludeRegex: `(?i)(anime|web-dl|web)`, PreferredOrder: 1}, {Label: "Anime 720p", Resolution: "720p", IncludeRegex: `(?i)(anime|web-dl|web)`, PreferredOrder: 2}}
	default:
		return nil
	}
}

func customFormatPresets(preset string) []CustomFormat {
	// Underscores and hyphens are equivalent so AltMount preset IDs
	// (trash_recommended) resolve the same as Vio keys (trash-recommended).
	normalized := strings.ToLower(strings.TrimSpace(preset))
	normalized = strings.ReplaceAll(normalized, "_", "-")
	switch normalized {
	case "english-original":
		return []CustomFormat{
			{Name: "English", Regex: `(?i)\b(?:eng|en|english)\b`, Score: 100},
			{Name: "German", Regex: `(?i)\b(?:deu|ger|de|german|deutsch)\b`, Reject: true},
			{Name: "Dubbed", Regex: `(?i)\b(?:dub|dubbed|dublado|synchroni[sz]ed)\b`, Reject: true},
			{Name: "Multi Audio", Regex: `(?i)\b(?:multi|dual|multi[ ._-]*audio)\b`, Score: -25},
		}
	case "english-strict":
		return []CustomFormat{
			{Name: "English", Regex: `(?i)\b(?:eng|en|english)\b`, Score: 100},
			{Name: "German", Regex: `(?i)\b(?:deu|ger|de|german|deutsch)\b`, Reject: true},
			{Name: "Non-English Language", Regex: `(?i)\b(?:fra|fre|fr|ita|spa|jpn|kor|zho|chi|por|rus|ara|nld|dut|pol|swe|nor|dan|fin)\b`, Reject: true},
			{Name: "Dubbed", Regex: `(?i)\b(?:dub|dubbed|dublado|synchroni[sz]ed)\b`, Reject: true},
			{Name: "Multi Audio", Regex: `(?i)\b(?:multi|dual|multi[ ._-]*audio)\b`, Reject: true},
		}
	case "original-or-english":
		return []CustomFormat{
			{Name: "English", Regex: `(?i)\b(?:eng|en|english)\b`, Score: 100},
			{Name: "German Dub", Regex: `(?i)\b(?:german|deutsch|deu|ger)\b`, Reject: true},
			{Name: "Dubbed", Regex: `(?i)\b(?:dub|dubbed|dublado|synchroni[sz]ed)\b`, Reject: true},
			{Name: "Multi Audio", Regex: `(?i)\b(?:multi|dual|multi[ ._-]*audio)\b`, Score: -10},
		}
	case "clean-quality":
		return []CustomFormat{
			{Name: "Cam/TS/HDCam", Regex: `(?i)\b(?:cam|hdcam|telecine|tc|tele-sync|ts|hd-ts|pdvd)\b`, Reject: true},
			{Name: "3D", Regex: `(?i)\b(?:3d|sbs|tab|hsbs|htab)\b`, Reject: true},
			{Name: "BR-DISK / Iso", Regex: `(?i)\b(?:br[ ._-]*disk|iso|bdmv)\b`, Reject: true},
			{Name: "Extras / Sample", Regex: `(?i)\b(?:sample|trailer|extras)\b`, Reject: true},
		}
	case "audio-hd":
		return []CustomFormat{
			{Name: "TrueHD Atmos / DTS:X", Regex: `(?i)(?:truehd[ ._-]*atmos|dts[ ._-]*x)`, Score: 500},
			{Name: "ATMOS", Regex: `(?i)\batmos\b`, Score: 300},
			{Name: "TrueHD / DTS-HD MA", Regex: `(?i)(?:truehd|dts[ ._-]*hd[ ._-]*ma)`, Score: 250},
		}
	case "repack-proper":
		return []CustomFormat{
			{Name: "Repack / Proper", Regex: `(?i)\b(?:repack[0-9]?|proper[0-9]?|rerip)\b`, Score: 200},
		}
	case "top-web-sources":
		return []CustomFormat{
			{Name: "Top Tier WEB (NF/ATVP/AMZN/DSNP/MAX)", Regex: `(?i)\b(?:nf|atvp|atv|amzn|dsnp|hmax|max|bcore)\b`, Score: 150},
		}
	case "anime-enhanced":
		return []CustomFormat{
			{Name: "Dual Audio", Regex: `(?i)\b(?:dual[ ._-]*audio|multi[ ._-]*audio)\b`, Score: 200},
			{Name: "Uncensored", Regex: `(?i)\buncensored\b`, Score: 150},
			{Name: "10-bit Color", Regex: `(?i)\b10bit\b`, Score: 100},
		}
	case "web-tier-01":
		return []CustomFormat{
			{Name: "WEB Tier 1 Groups", Regex: `(?i)-(?:NTb|FLUX|Kitsune|ETHEL|playWEB|NOGRP|DECiBEL|DON|CtrlHD|TrollHD|KiNGS|hallowed|CRiMSON|BLUTONiUM|NIMA)\b`, Score: 300},
		}
	case "web-tier-02":
		return []CustomFormat{
			{Name: "WEB Tier 2 Groups", Regex: `(?i)-(?:QxR|SiNNERS|CasStudio|CMRG|LAZY|W4NK3R|HQMUX|GHOSTS)\b`, Score: 150},
		}
	case "remux-tier-01":
		return []CustomFormat{
			{Name: "Remux Tier 1 Groups", Regex: `(?i)-(?:FraMeSToR|EPSiLON|KRaLiMARK|HDC|DON|CtrlHD|TrollHD|BHDStudio)\b`, Score: 500},
		}
	case "remux-tier-02":
		return []CustomFormat{
			{Name: "Remux Tier 2 Groups", Regex: `(?i)-(?:BLUTONiUM|PmP|TRiToN|SNAKE|W4NK3R)\b`, Score: 250},
		}
	case "trash-recommended":
		return []CustomFormat{
			{Name: "English", Regex: `(?i)\b(?:eng|en|english)\b`, Score: 100},
			{Name: "German", Regex: `(?i)\b(?:deu|ger|de|german|deutsch)\b`, Reject: true},
			{Name: "Dubbed", Regex: `(?i)\b(?:dub|dubbed|dublado|synchroni[sz]ed)\b`, Reject: true},
			{Name: "Cam/TS/HDCam", Regex: `(?i)\b(?:cam|hdcam|telecine|tc|tele-sync|ts|hd-ts|pdvd)\b`, Reject: true},
			{Name: "3D", Regex: `(?i)\b(?:3d|sbs|tab|hsbs|htab)\b`, Reject: true},
			{Name: "BR-DISK / Iso", Regex: `(?i)\b(?:br[ ._-]*disk|iso|bdmv)\b`, Reject: true},
			{Name: "Extras / Sample", Regex: `(?i)\b(?:sample|trailer|extras)\b`, Reject: true},
			{Name: "Repack / Proper", Regex: `(?i)\b(?:repack[0-9]?|proper[0-9]?|rerip)\b`, Score: 200},
			{Name: "Remux Tier 1", Regex: `(?i)-(?:FraMeSToR|EPSiLON|KRaLiMARK|HDC|DON|CtrlHD|TrollHD|BHDStudio)\b`, Score: 500},
			{Name: "WEB Tier 1", Regex: `(?i)-(?:NTb|FLUX|Kitsune|ETHEL|playWEB|NOGRP|DECiBEL|DON|CtrlHD|TrollHD|KiNGS|hallowed|CRiMSON|BLUTONiUM|NIMA)\b`, Score: 300},
			{Name: "TrueHD Atmos / DTS:X", Regex: `(?i)(?:truehd[ ._-]*atmos|dts[ ._-]*x)`, Score: 250},
		}
	case "altmount-recommended":
		return altmountRecommendedFormats()
	case "altmount-remux":
		return altmountRemuxEnthusiastFormats()
	case "altmount-compatibility":
		return altmountCompatibilityFormats()
	default:
		return nil
	}
}

// altmountTrashFormat builds a CustomFormat from AltMount's TRaSH preset
// definitions (frontend scoringPresets.ts). Pattern and Regex carry the same
// expression so legacy Regex-only readers keep working.
func altmountTrashFormat(id, name, category, pattern string, score int) CustomFormat {
	return CustomFormat{
		ID: id, Name: name, Category: category,
		Pattern: pattern, Regex: pattern, PatternType: "regex",
		Score: score, Enabled: true,
	}
}

// altmountRecommendedFormats is a 1:1 port of AltMount's TRASH_RECOMMENDED
// preset. Unlike Vio's legacy "trash-recommended" (which bundles language
// rejects), language preference stays out of custom formats — AltMount
// scores languages separately via preferred_languages.
func altmountRecommendedFormats() []CustomFormat {
	return []CustomFormat{
		altmountTrashFormat("remux-4k", "4K UHD Remux (Disc)", "source", `\b(2160p|4k)\b.*\b(remux|bdremux|uhd\.remux)\b|\b(remux|bdremux|uhd\.remux)\b.*\b(2160p|4k)\b`, 500),
		altmountTrashFormat("remux-1080p", "1080p Remux (Disc)", "source", `\b1080p\b.*\b(remux|bdremux)\b|\b(remux|bdremux)\b.*\b1080p\b`, 350),
		altmountTrashFormat("dv-hdr", "Dolby Vision (P7/P8)", "hdr", `\b(dv|dovi|dolby[ ._-]?vision)\b`, 350),
		altmountTrashFormat("hdr10-plus", "HDR10+ / HDR10", "hdr", `\b(hdr10\+|hdr10|hdr)\b`, 200),
		altmountTrashFormat("lossless-atmos", "Lossless Atmos / TrueHD", "audio", `\b(truehd[ ._-]?atmos|truehd|atmos)\b`, 250),
		altmountTrashFormat("dts-hd-ma", "DTS-HD MA / DTS:X", "audio", `\b(dts[ ._-]hd([ ._-]ma)?|dts[ ._-]?x)\b`, 200),
		altmountTrashFormat("webdl-4k", "4K WEB-DL / WEBRip", "source", `\b(2160p|4k)\b.*\b(web[ ._-]?dl|webrip)\b`, 180),
		altmountTrashFormat("tier1-groups", "Tier 1 High-Quality Release Groups", "release_group", `-(FLUX|FraMeSToR|EPSiLON|DON|playBD|CtrlHD|ZQ|TayTO|BHDStudio|SURCODE)\b`, 150),
		altmountTrashFormat("webdl-1080p", "1080p WEB-DL", "source", `\b1080p\b.*\b(web[ ._-]?dl|webrip)\b`, 120),
		altmountTrashFormat("aac-stereo-demote", "Low Bitrate Stereo Audio", "audio", `\b(aac[ ._-]?2\.0|stereo|mp3)\b`, -100),
		altmountTrashFormat("cam-ts-discard", "CAM / TeleSync / Screener", "source", `\b(cam|camrip|telesync|ts|hdcam|hdts|screener|scr|dvdscr)\b`, -2000),
	}
}

// altmountRemuxEnthusiastFormats ports AltMount's remux_enthusiast preset:
// disc-first weighting with a WEB-DL demotion.
func altmountRemuxEnthusiastFormats() []CustomFormat {
	return []CustomFormat{
		altmountTrashFormat("remux-4k", "4K UHD Remux (Disc)", "source", `\b(2160p|4k)\b.*\b(remux|bdremux|uhd\.remux)\b|\b(remux|bdremux|uhd\.remux)\b.*\b(2160p|4k)\b`, 800),
		altmountTrashFormat("remux-1080p", "1080p Remux (Disc)", "source", `\b1080p\b.*\b(remux|bdremux)\b|\b(remux|bdremux)\b.*\b1080p\b`, 500),
		altmountTrashFormat("dv-hdr", "Dolby Vision (P7/P8)", "hdr", `\b(dv|dovi|dolby[ ._-]?vision)\b`, 400),
		altmountTrashFormat("hdr10-plus", "HDR10+ / HDR10", "hdr", `\b(hdr10\+|hdr10|hdr)\b`, 250),
		altmountTrashFormat("lossless-atmos", "Lossless Atmos / TrueHD", "audio", `\b(truehd[ ._-]?atmos|truehd|atmos)\b`, 350),
		altmountTrashFormat("dts-hd-ma", "DTS-HD MA / DTS:X", "audio", `\b(dts[ ._-]hd([ ._-]ma)?|dts[ ._-]?x)\b`, 300),
		altmountTrashFormat("tier1-groups", "Tier 1 High-Quality Release Groups", "release_group", `-(FLUX|FraMeSToR|EPSiLON|DON|playBD|CtrlHD|ZQ|TayTO|BHDStudio|SURCODE)\b`, 200),
		altmountTrashFormat("webdl-demote", "Compressed WEB-DL Demotion", "source", `\b(web[ ._-]?dl|webrip)\b`, -200),
		altmountTrashFormat("cam-ts-discard", "CAM / TeleSync / Screener", "source", `\b(cam|camrip|telesync|ts|hdcam|hdts|screener|scr|dvdscr)\b`, -2000),
	}
}

// altmountCompatibilityFormats ports AltMount's compatibility preset:
// universal direct-play weighting (H.264, E-AC-3, AAC) with a remux demotion.
func altmountCompatibilityFormats() []CustomFormat {
	return []CustomFormat{
		altmountTrashFormat("webdl-1080p", "1080p WEB-DL (High Compatibility)", "source", `\b1080p\b.*\b(web[ ._-]?dl|webrip)\b`, 400),
		altmountTrashFormat("webdl-4k", "4K WEB-DL (SDR / Standard HDR)", "source", `\b(2160p|4k)\b.*\b(web[ ._-]?dl|webrip)\b`, 300),
		altmountTrashFormat("h264-avc", "H.264 / AVC (Universal Playback)", "source", `\b(h[ ._-]?264|x264|avc)\b`, 250),
		altmountTrashFormat("eac3-ddp", "Dolby Digital Plus (E-AC-3 / DDP)", "audio", `\b(e[ ._-]?ac[ ._-]?3|ddp|dd\+)\b`, 200),
		altmountTrashFormat("aac-stereo", "AAC Audio", "audio", `\baac\b`, 150),
		altmountTrashFormat("remux-demote", "Heavy High-Bitrate Remux Demotion", "source", `\b(remux|bdremux)\b`, -100),
		altmountTrashFormat("cam-ts-discard", "CAM / TeleSync / Screener", "source", `\b(cam|camrip|telesync|ts|hdcam|hdts|screener|scr|dvdscr)\b`, -2000),
	}
}

func (q *QualityConfig) ApplyPreset() {
	if profiles := qualityPresetProfiles(q.Preset); len(profiles) > 0 {
		q.Profiles = profiles
	}
	if formats := customFormatPresets(q.CustomFormatPreset); len(formats) > 0 {
		q.CustomFormats = formats
	}
}

// decodeQualityProfiles accepts the typed array declared by the plugin
// manifest. Keeping one representation prevents configuration drift between
// the UI, JSON Schema validation, and the runtime.
func decodeQualityProfiles(raw any) ([]QualityProfile, error) {
	if raw == nil {
		return nil, nil
	}
	if _, ok := raw.([]any); !ok {
		return nil, errors.New("must be an array")
	}

	data, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var profiles []QualityProfile
	if err := json.Unmarshal(data, &profiles); err != nil {
		return nil, err
	}
	return profiles, nil
}

func (q *QualityConfig) Validate() error {
	return q.validate(false, nil)
}

// SkippedFormat names a custom format isolated during lenient validation
// along with a pattern-free compile-failure reason for diagnostics. Reason
// never carries the user-supplied pattern text.
type SkippedFormat struct {
	Name   string
	Reason string
}

// ValidateLenient validates like Validate but isolates regex-uncompilable
// custom formats instead of failing: the broken rule keeps its source
// pattern (so a later fix or strict Validate still sees it), its compiled
// matcher is cleared so it never matches, and its name plus reason are
// returned. Every structural problem still errors. ValidateLenient is
// stable under repetition: revalidating the same config succeeds with the
// same skip report. Boot activation uses this so one broken custom format
// cannot take down the whole virtual library; settings save uses strict
// Validate so the operator fixes the pattern before persisting it.
func (q *QualityConfig) ValidateLenient() ([]SkippedFormat, error) {
	skipped := []SkippedFormat{}
	if err := q.validate(true, &skipped); err != nil {
		return skipped, err
	}
	return skipped, nil
}

// regexFailureReason summarizes a compile failure without echoing the
// pattern: Go's syntax errors embed the offending expression, which must
// not reach logs.
func regexFailureReason(err error) string {
	var syntaxErr *syntax.Error
	if errors.As(err, &syntaxErr) {
		return "invalid regex: " + syntaxErr.Code.String()
	}
	return "invalid regex: uncompilable pattern"
}

func (q *QualityConfig) validate(isolateBadPatterns bool, skipped *[]SkippedFormat) error {
	if len(q.CustomFormats) > maxCustomFormats {
		return fmt.Errorf("maximum %d custom formats allowed", maxCustomFormats)
	}
	seenFormats := make(map[string]bool)
	for i := range q.CustomFormats {
		format := &q.CustomFormats[i]
		format.Name = strings.TrimSpace(format.Name)
		format.Regex = strings.TrimSpace(format.Regex)
		format.Pattern = strings.TrimSpace(format.Pattern)
		format.PatternType = strings.ToLower(strings.TrimSpace(format.PatternType))
		format.Category = strings.TrimSpace(format.Category)
		format.ID = strings.TrimSpace(format.ID)
		if format.Name == "" {
			return errors.New("custom format name cannot be empty")
		}
		pattern := format.EffectivePattern()
		if pattern == "" {
			return fmt.Errorf("custom format %s regex cannot be empty", format.Name)
		}
		if len(format.Name) > maxProfileLabelBytes || len(pattern) > maxProfileRegexBytes {
			return fmt.Errorf("custom format %s exceeds its size limit", format.Name)
		}
		if len(format.ID) > maxProfileLabelBytes || len(format.Category) > maxProfileAttributeBytes {
			return fmt.Errorf("custom format %s exceeds its size limit", format.Name)
		}
		if format.PatternType != "" && format.PatternType != patternTypeRegex && format.PatternType != patternTypeToken {
			return fmt.Errorf("custom format %s has invalid pattern_type %q: must be regex or token", format.Name, format.PatternType)
		}
		// Legacy rules predate the Enabled flag (zero value, no AltMount
		// fields): normalize to enabled so old configs validate as before.
		// Explicitly disabled modern rules keep Enabled=false.
		if !format.Enabled && format.IsEnabled() {
			format.Enabled = true
		}
		key := strings.ToLower(format.Name)
		if seenFormats[key] {
			return fmt.Errorf("duplicate custom format name: %s", format.Name)
		}
		seenFormats[key] = true
		if format.PatternType == patternTypeToken {
			format.match = nil
			continue
		}
		compiled, err := compileFormatRegex(pattern)
		if err != nil {
			if isolateBadPatterns && skipped != nil {
				// Keep the source pattern intact so repetition is stable
				// and strict Validate still sees the original text; only
				// the compiled matcher is cleared. formatMatchesText
				// recompiles on a nil matcher and returns false before
				// applying Invert, so the broken rule contributes neither
				// score nor rejection. The caller warns with name+reason.
				*skipped = append(*skipped, SkippedFormat{Name: format.Name, Reason: regexFailureReason(err)})
				format.match = nil
				continue
			}
			return fmt.Errorf("invalid regex in custom format %s: %w", format.Name, err)
		}
		format.match = compiled
	}
	if !q.EnableProfiles {
		return nil
	}
	if len(q.Profiles) == 0 {
		return errors.New("at least one quality profile is required when profiles are enabled")
	}
	if len(q.Profiles) > maxQualityProfiles {
		return fmt.Errorf("maximum %d quality profiles allowed", maxQualityProfiles)
	}
	seen := make(map[string]bool)
	for i := range q.Profiles {
		p := &q.Profiles[i]
		p.Label = strings.TrimSpace(p.Label)
		if p.Label == "" {
			return fmt.Errorf("profile label cannot be empty")
		}
		if len(p.Label) > maxProfileLabelBytes {
			return fmt.Errorf("profile label exceeds %d bytes", maxProfileLabelBytes)
		}
		for name, value := range map[string]string{
			"resolution":     p.Resolution,
			"codec_video":    p.CodecVideo,
			"codec_audio":    p.CodecAudio,
			"hdr":            p.HDR,
			"exclude_hdr":    p.ExcludeHDR,
			"audio_channels": p.AudioChannels,
			"language":       p.Language,
			"visual_tag":     p.VisualTag,
		} {
			if len(value) > maxProfileAttributeBytes {
				return fmt.Errorf("%s in profile %s exceeds %d bytes", name, p.Label, maxProfileAttributeBytes)
			}
		}
		if len(p.IncludeRegex) > maxProfileRegexBytes || len(p.ExcludeRegex) > maxProfileRegexBytes {
			return fmt.Errorf("profile %s regex exceeds %d bytes", p.Label, maxProfileRegexBytes)
		}
		if p.PreferredOrder < 0 || p.PreferredOrder > maxPreferredOrder {
			return fmt.Errorf("preferred_order in profile %s must be between 0 and %d", p.Label, maxPreferredOrder)
		}
		if len(p.Sort) > maxSortCriteria {
			return fmt.Errorf("profile %s has more than %d sort criteria", p.Label, maxSortCriteria)
		}
		for i := range p.Sort {
			criterion := &p.Sort[i]
			attribute := strings.ToLower(strings.TrimSpace(criterion.Attribute))
			if !sortAttributes[attribute] {
				return fmt.Errorf("profile %s has unknown sort attribute %q", p.Label, criterion.Attribute)
			}
			direction := strings.ToLower(strings.TrimSpace(criterion.Direction))
			if direction != "" && direction != sortDirectionAsc && direction != sortDirectionDesc {
				return fmt.Errorf("profile %s has invalid sort direction %q: must be asc or desc", p.Label, criterion.Direction)
			}
			criterion.Attribute = attribute
			criterion.Direction = direction
		}
		lower := strings.ToLower(p.Label)
		if seen[lower] {
			return fmt.Errorf("duplicate profile label: %s", p.Label)
		}
		seen[lower] = true

		if p.IncludeRegex != "" {
			r, err := regexp.Compile(p.IncludeRegex)
			if err != nil {
				return fmt.Errorf("invalid include_regex in profile %s: %w", p.Label, err)
			}
			p.include = r
		}
		if p.ExcludeRegex != "" {
			r, err := regexp.Compile(p.ExcludeRegex)
			if err != nil {
				return fmt.Errorf("invalid exclude_regex in profile %s: %w", p.Label, err)
			}
			p.exclude = r
		}
	}
	sort.SliceStable(q.Profiles, func(i, j int) bool {
		left, right := q.Profiles[i].PreferredOrder, q.Profiles[j].PreferredOrder
		switch {
		case left == 0 && right == 0:
			return false
		case left == 0:
			return false
		case right == 0:
			return true
		default:
			return left < right
		}
	})
	return nil
}

// hdrSatisfies reports whether a candidate's classifier HDR value satisfies a
// profile's HDR requirement. The classifier's emitted values are the source of
// truth: hdr, hdr10, hdr10+, dv (see stream.ParseStreamDetails).
//
// The generic "hdr" requirement means "any high dynamic range", so it accepts
// the whole family: the HDR10 variants and Dolby Vision. A profile named for
// HDR content must match HDR content; treating DV as a non-match made the
// "4K HDR" preset reject exactly the releases it exists to select (a DV-only
// listing then matched zero candidates and hard-failed playback). The
// requirement is a selection preference, so a broad match is the safe side.
// A profile that wants only Dolby Vision still says so explicitly with HDR
// "dv" (the 4K Dolby Vision preset does) and keeps the exact match. Every
// other value matches case-insensitively and exactly.
func hdrSatisfies(required, candidate string) bool {
	required = strings.ToLower(strings.TrimSpace(required))
	candidate = strings.ToLower(strings.TrimSpace(candidate))
	if required == candidate {
		return true
	}
	if required == "hdr" {
		switch candidate {
		case hdrValueHDR10, hdrValueHDR10Plus, hdrValueDV:
			return true
		}
	}
	return false
}

// resolutionSatisfies reports whether a candidate's parsed resolution meets a
// profile's resolution requirement. A known resolution must match exactly; an
// unknown one (the parser found no marker) does not. The resolution is a
// selection preference, not a proof requirement, so a candidate whose
// resolution could not be parsed must not be treated as a proven mismatch and
// filtered out. Doing so turned a listing whose releases carried no explicit
// resolution marker into a zero-match hard failure for an auto-picked profile.
// Known resolutions still gate exactly, and the ranking keeps a known
// higher-resolution candidate ahead of an unknown one.
func resolutionSatisfies(required, candidate string) bool {
	required = stream.NormalizeResolution(required)
	candidate = stream.NormalizeResolution(candidate)
	if candidate == "" {
		return true
	}
	return candidate == required
}

// MatchProfile reports whether a candidate satisfies a quality profile's
// include/exclude regex, resolution, codecs, and HDR constraints.
func MatchProfile(c stream.StreamCandidate, p QualityProfile) bool {
	fullText := c.Name + " " + c.Description + " " + c.Title + " " + c.URL
	if p.ExcludeCompiled() != nil && p.ExcludeCompiled().MatchString(fullText) {
		return false
	}
	if p.IncludeCompiled() != nil && !p.IncludeCompiled().MatchString(fullText) {
		return false
	}
	if p.Resolution != "" && !resolutionSatisfies(p.Resolution, c.Resolution) {
		return false
	}
	if p.CodecVideo != "" && c.CodecVideo != p.CodecVideo {
		return false
	}
	if p.CodecAudio != "" && c.CodecAudio != p.CodecAudio {
		return false
	}
	if p.HDR != "" && !hdrSatisfies(p.HDR, c.HDR) {
		return false
	}
	if p.ExcludeHDR == "*" && c.HDR != "" {
		return false
	}
	// The same sibling rule as the requirement side: ExcludeHDR "hdr" excludes
	// the whole HDR family (hdr, hdr10, hdr10+, dv) just as HDR "hdr" accepts
	// it, while ExcludeHDR "dv" excludes only DV. The two sides stay symmetric,
	// so "exclude HDR" means SDR-only rather than leaving DV behind.
	if p.ExcludeHDR != "" && p.ExcludeHDR != "*" && hdrSatisfies(p.ExcludeHDR, c.HDR) {
		return false
	}
	if p.AudioChannels != "" && c.AudioChannels != "" && !strings.EqualFold(c.AudioChannels, p.AudioChannels) {
		return false
	}
	minSize := p.MinSize
	if minSize <= 0 && p.MinSizeGB > 0 {
		minSize = int64(p.MinSizeGB * 1e9)
	}
	maxSize := p.MaxSize
	if maxSize <= 0 && p.MaxSizeGB > 0 {
		maxSize = int64(p.MaxSizeGB * 1e9)
	}
	// Size bounds are deliberately asymmetric for an unknown (0) size. A
	// minimum is a requirement the candidate must prove it meets, so an
	// unknown size fails it; a maximum is a ceiling only a known size can
	// exceed, so an unknown size passes it. A min-size profile therefore never
	// admits a release whose size could not be parsed, while a max-size profile
	// never rejects purely for missing metadata.
	if minSize > 0 && (c.FileSize <= 0 || c.FileSize < minSize) {
		return false
	}
	if maxSize > 0 && c.FileSize > maxSize {
		return false
	}
	// RequireMultiAudio accepts a candidate with two distinct parsed audio
	// languages, an explicit MULTI/DUAL marker, or (fallback) two raw list
	// entries the parser could not identify: unrecognized languages must not
	// fail the gate purely for being unparseable.
	if p.RequireMultiAudio && !stream.CandidateHasDistinctAudioLanguages(c, 2) && !c.IsMultiAudio && !c.IsDualAudio && len(c.AudioLanguages) <= 1 {
		return false
	}
	if p.Language != "" && !stream.CandidateHasLanguage(c, p.Language) {
		return false
	}
	if p.VisualTag != "" {
		matchedTag := false
		for _, vt := range c.VisualTags {
			if strings.EqualFold(strings.TrimSpace(vt), strings.TrimSpace(p.VisualTag)) {
				matchedTag = true
				break
			}
		}
		// The text fallback uses the package's token/boundary matcher, not a
		// substring contains: a plain Contains made "dv" match "DVD-Rip".
		// Exact parsed VisualTags matching stays authoritative.
		if !matchedTag && !matchKeywordOrPattern(fullText, p.VisualTag) {
			return false
		}
	}
	return true
}

// CustomFormatScore sums the scores of matching custom formats for a
// candidate. Returns the score and whether any matching format rejects.
func CustomFormatScore(candidate stream.StreamCandidate, formats []CustomFormat) (int, bool) {
	return customFormatScore(candidate, formats)
}

// customFormatMatchText is the text a custom format is matched against: the
// display fields plus the filename and URL in their decoded spellings, so an
// encoded release name still matches.
func customFormatMatchText(candidate stream.StreamCandidate) string {
	text := candidate.Name + " " + candidate.Description + " " + candidate.Title + " " + candidate.URL
	if candidate.BehaviorHints.Filename != "" {
		text += " " + candidate.BehaviorHints.Filename
		if u, err := url.QueryUnescape(candidate.BehaviorHints.Filename); err == nil && u != "" && u != candidate.BehaviorHints.Filename {
			text += " " + u
		}
		if u, err := url.PathUnescape(candidate.BehaviorHints.Filename); err == nil && u != "" && u != candidate.BehaviorHints.Filename {
			text += " " + u
		}
	}
	if u, err := url.QueryUnescape(candidate.URL); err == nil && u != "" {
		text += " " + u
	}
	if u, err := url.PathUnescape(candidate.URL); err == nil && u != "" {
		text += " " + u
	}
	return text
}

// formatMatchesText reports whether one custom format matches the candidate
// text, applying the rule's Invert flag.
func formatMatchesText(format CustomFormat, text string) bool {
	pattern := format.EffectivePattern()
	if pattern == "" {
		return false
	}
	var matched bool
	if strings.ToLower(strings.TrimSpace(format.PatternType)) == patternTypeToken {
		matched = matchKeywordOrPattern(text, pattern)
	} else {
		matcher := format.Compiled()
		if matcher == nil {
			var err error
			matcher, err = compileFormatRegex(pattern)
			if err != nil {
				return false
			}
		}
		matched = matcher.MatchString(text)
	}
	if format.Invert {
		matched = !matched
	}
	return matched
}

func customFormatScore(candidate stream.StreamCandidate, formats []CustomFormat) (int, bool) {
	text := customFormatMatchText(candidate)
	score := 0
	for _, format := range formats {
		if !format.IsEnabled() || !formatMatchesText(format, text) {
			continue
		}
		// AltMount parity: scores at or below the discard line reject the
		// candidate even without an explicit Reject flag.
		if format.Reject || format.Score <= discardScoreThreshold {
			return 0, true
		}
		score += format.Score
	}
	return score, false
}

// RejectingFormatNames returns the names of the enabled custom formats that
// reject a candidate (an explicit Reject rule or a score at/below the discard
// line). It is diagnostic only: the resolver logs it when every candidate in a
// set is rejected and the best rejected one has to be used.
func RejectingFormatNames(candidate stream.StreamCandidate, formats []CustomFormat) []string {
	text := customFormatMatchText(candidate)
	var names []string
	for _, format := range formats {
		if !format.IsEnabled() || !formatMatchesText(format, text) {
			continue
		}
		if format.Reject || format.Score <= discardScoreThreshold {
			names = append(names, strings.TrimSpace(format.Name))
		}
	}
	return names
}

// compileFormatRegex compiles a custom-format regex case-insensitively,
// mirroring AltMount's regexcache (which prefixes (?i) unless present) so
// ported TRaSH patterns match release titles regardless of case.
func compileFormatRegex(pattern string) (*regexp.Regexp, error) {
	if !strings.HasPrefix(pattern, "(?i)") {
		pattern = "(?i)" + pattern
	}
	return regexp.Compile(pattern)
}

// Token/keyword matching ported from AltMount's prowlarr.MatchKeywordOrPattern
// so token-typed custom formats agree with the Stremio addon backend.
// Plain keywords match on token boundaries (TS matches "Movie.TS.1080p" but
// not "DTS-HD" or "Knights"); explicit regex constructs and /pattern/flags
// forms match as regex.

var (
	reExplicitRegexConstruct = regexp.MustCompile(`\\b|\\[dwsDWS]|\(\?|[|*+?^$]`)
	reWhitespaceSplit        = regexp.MustCompile(`\s+`)
)

// slashPatternExpr builds a case-insensitive regex expression from a
// slash-delimited pattern body and trailing flags. Only Go-supported inline
// flags i, m, s are honored; unknown letters are dropped.
func slashPatternExpr(raw, flags string) string {
	var b strings.Builder
	b.WriteString("(?i")
	for _, f := range flags {
		if f == 'm' || f == 's' {
			b.WriteRune(f)
		}
	}
	b.WriteString(")")
	return b.String() + raw
}

// buildKeywordRegex matches a keyword phrase across release-name delimiters
// (".", "_", "-", spaces) with token boundaries on both ends.
func buildKeywordRegex(keyword string) string {
	clean := strings.Trim(strings.Trim(keyword, "._- \t"), "._- \t")
	if clean == "" {
		return ""
	}
	parts := reWhitespaceSplit.Split(clean, -1)
	escaped := make([]string, len(parts))
	for i, p := range parts {
		escaped[i] = regexp.QuoteMeta(p)
	}
	return `(?i)(?:^|[^a-zA-Z0-9])` + strings.Join(escaped, `[ ._\-]+`) + `(?:[^a-zA-Z0-9]|$)`
}

// matchKeywordOrPattern matches release text against a token keyword or an
// explicit regex, mirroring AltMount (Go prowlarr.MatchKeywordOrPattern and
// scoringPresets.ts matchKeywordOrPattern).
func matchKeywordOrPattern(title, pattern string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" || title == "" {
		return false
	}
	if strings.HasPrefix(pattern, "/") && len(pattern) >= 2 {
		if lastSlash := strings.LastIndex(pattern, "/"); lastSlash > 0 {
			expr := slashPatternExpr(pattern[1:lastSlash], pattern[lastSlash+1:])
			if re, err := regexp.Compile(expr); err == nil && re != nil {
				return re.MatchString(title)
			}
			return false
		}
	}
	if reExplicitRegexConstruct.MatchString(pattern) {
		if re, err := regexp.Compile(pattern); err == nil && re != nil {
			return re.MatchString(title)
		}
	}
	tokenPattern := buildKeywordRegex(pattern)
	if tokenPattern == "" {
		return false
	}
	if re, err := regexp.Compile(tokenPattern); err == nil && re != nil {
		return re.MatchString(title)
	}
	return false
}

// Sort attribute and direction names accepted by Validate and the ranking
// evaluator. They are the wire vocabulary of QualityProfile.Sort.
const (
	sortAttributeSize          = "size"
	sortAttributeBitrate       = "bitrate"
	sortAttributeResolution    = "resolution"
	sortAttributeAudioChannels = "audio_channels"
	sortAttributeBitDepth      = "bit_depth"
	sortAttributeHDR           = "hdr"
	sortAttributeSource        = "source"
	sortAttributeScore         = "score"
	sortAttributeConfirmed     = "confirmed"
	sortAttributeLanguage      = "language"

	sortDirectionAsc  = "asc"
	sortDirectionDesc = "desc"
)

// sortAttributes is the closed attribute set. A name outside it is a config
// error rather than a silently ignored key.
var sortAttributes = map[string]bool{
	sortAttributeSize:          true,
	sortAttributeBitrate:       true,
	sortAttributeResolution:    true,
	sortAttributeAudioChannels: true,
	sortAttributeBitDepth:      true,
	sortAttributeHDR:           true,
	sortAttributeSource:        true,
	sortAttributeScore:         true,
	sortAttributeConfirmed:     true,
	sortAttributeLanguage:      true,
}

// defaultSortCriteria reproduces the ranking tail that existed before profiles
// could declare their own sort: confirmation, then custom-format score, then a
// preferred-language match, resolution, source, audio channels and size. It is
// applied whenever profile.Sort is empty so stored profiles rank exactly as
// they always did.
//
// Ordinal attributes (confirmed, language, source, hdr) use ascending
// direction because their evaluator returns a preference rank where 0 is best;
// numeric attributes (score, resolution, audio channels, size, bitrate,
// bit_depth) use descending direction because a larger value is better.
var defaultSortCriteria = []SortCriterion{
	{Attribute: sortAttributeConfirmed, Direction: sortDirectionAsc},
	{Attribute: sortAttributeScore, Direction: sortDirectionDesc},
	{Attribute: sortAttributeLanguage, Direction: sortDirectionAsc},
	{Attribute: sortAttributeResolution, Direction: sortDirectionDesc},
	{Attribute: sortAttributeSource, Direction: sortDirectionAsc},
	{Attribute: sortAttributeAudioChannels, Direction: sortDirectionDesc},
	{Attribute: sortAttributeSize, Direction: sortDirectionDesc},
}

// EffectiveSortCriteria returns the ordered ranking keys a profile applies: its
// own Sort when set, else the package's built-in default order. The result is a
// copy the caller may retain; it is the read-only projection of
// sortCandidatesForProfile's key list.
func EffectiveSortCriteria(p QualityProfile) []SortCriterion {
	if len(p.Sort) > 0 {
		return append([]SortCriterion(nil), p.Sort...)
	}
	return append([]SortCriterion(nil), defaultSortCriteria...)
}

// sortAttributeDefaultDirection is the direction a criterion uses when it omits
// one: descending for the numeric attributes (largest first), ascending for the
// ordinal attributes (best rank first).
func sortAttributeDefaultDirection(attribute string) string {
	switch attribute {
	case sortAttributeSize, sortAttributeBitrate, sortAttributeResolution,
		sortAttributeAudioChannels, sortAttributeBitDepth, sortAttributeScore:
		return sortDirectionDesc
	default:
		return sortDirectionAsc
	}
}

// sortCriterionDirection resolves a criterion's effective direction.
func sortCriterionDirection(criterion SortCriterion) string {
	switch direction := strings.ToLower(strings.TrimSpace(criterion.Direction)); direction {
	case sortDirectionAsc, sortDirectionDesc:
		return direction
	default:
		return sortAttributeDefaultDirection(criterion.Attribute)
	}
}

// maxSourceScore is stream.SourceScore's best value; sourcePreferenceRank
// inverts the score into a best-first rank for the ordinal "source" criterion.
const maxSourceScore = 4

// sourcePreferenceRank orders source types best-first (0 is best): remux,
// bluray, web-dl, hdtv, then an unknown type. It is stream.SourceScore
// inverted, so the ranking vocabulary and the resolver's source classification
// cannot drift apart.
func sourcePreferenceRank(sourceType string) int {
	return maxSourceScore - stream.SourceScore(sourceType)
}

// hdrPreferenceRank orders HDR families best-first (0 is best): Dolby Vision,
// HDR10+, HDR10, generic HDR, then SDR or unlabelled. Every candidate yields a
// rank, so the criterion never reports an unknown value.
func hdrPreferenceRank(hdr string) int {
	switch strings.ToLower(strings.TrimSpace(hdr)) {
	case "dv":
		return 0
	case hdrValueHDR10Plus:
		return 1
	case hdrValueHDR10:
		return 2
	case "hdr":
		return 3
	default:
		return 4
	}
}

// sortValueFor evaluates one sort criterion for a candidate. known is false
// when the attribute carries no value for this candidate; an unknown value
// sorts after every known value within the criterion and then falls through to
// the next key.
func sortValueFor(attribute string, c stream.StreamCandidate, p QualityProfile) (value int64, known bool) {
	switch attribute {
	case sortAttributeSize:
		return c.FileSize, c.FileSize > 0
	case sortAttributeBitrate:
		return int64(c.Bitrate), c.Bitrate > 0
	case sortAttributeResolution:
		score := stream.ResolutionScore(c.Resolution)
		return int64(score), score > 0
	case sortAttributeAudioChannels:
		score := stream.AudioChannelsScore(c.AudioChannels)
		return int64(score), score > 0
	case sortAttributeBitDepth:
		// The classifier exposes only Is10Bit, so an 8-bit candidate and an
		// unlabelled one both read 0. bit_depth cannot distinguish "not 10-bit"
		// from "unknown" and is therefore always a known value (10-bit sorts
		// ahead of everything else under the descending default).
		if c.Is10Bit {
			return 1, true
		}
		return 0, true
	case sortAttributeScore:
		return int64(c.QualityScore), true
	case sortAttributeHDR:
		return int64(hdrPreferenceRank(c.HDR)), true
	case sortAttributeSource:
		return int64(sourcePreferenceRank(c.SourceType)), true
	case sortAttributeConfirmed:
		if c.SourceConfirmed {
			return 0, true
		}
		return 1, true
	case sortAttributeLanguage:
		rank := stream.CandidateLanguageMatchRank(c, p.Language)
		if rank < 0 {
			return 0, false
		}
		return int64(rank), true
	}
	return 0, false
}

// SortCandidatesForProfile ranks candidates in place: non-rejected first,
// then profile-matching candidates, then by custom-format score, resolution,
// source type, and original order. Ordering a profile-matching candidate ahead
// of a profile-removed one is what keeps the auto picker's candidate list in
// agreement with the resolver's profile filter; a zero profile (profiles
// disabled or an unknown label) imposes no such ordering.
func SortCandidatesForProfile(candidates []stream.StreamCandidate, p QualityProfile, formats []CustomFormat) {
	sortCandidatesForProfile(candidates, p, formats)
}

func sortCandidatesForProfile(candidates []stream.StreamCandidate, p QualityProfile, formats []CustomFormat) {
	if len(candidates) == 0 {
		return
	}
	if len(candidates) == 1 {
		candidates[0].QualityScore, candidates[0].CustomFormatRejected = customFormatScore(candidates[0], formats)
		return
	}
	type scoredCandidate struct {
		candidate stream.StreamCandidate
		rejected  bool
		// profileMatched is true when the candidate satisfies the active
		// profile. It is computed once per candidate so the comparator does not
		// re-run the profile's regexes, and it orders matching candidates
		// ahead of non-matching ones so the shared ranking (used by the auto
		// picker's candidate list) agrees with the resolver's profile filter.
		profileMatched bool
	}
	profileActive := strings.TrimSpace(p.Label) != ""
	scored := make([]scoredCandidate, len(candidates))
	for idx := range candidates {
		score, reject := customFormatScore(candidates[idx], formats)
		candidates[idx].QualityScore = score
		candidates[idx].CustomFormatRejected = reject
		scored[idx] = scoredCandidate{
			candidate:      candidates[idx],
			rejected:       reject,
			profileMatched: !profileActive || MatchProfile(candidates[idx], p),
		}
	}
	sort.SliceStable(scored, func(i, j int) bool {
		c1, c2 := scored[i].candidate, scored[j].candidate
		if scored[i].rejected != scored[j].rejected {
			return !scored[i].rejected
		}
		// Only an active profile contributes profile ordering. With a zero
		// profile every candidate is profileMatched, but gating the comparison
		// on profileActive makes the no-op structural rather than incidental.
		if profileActive && scored[i].profileMatched != scored[j].profileMatched {
			return scored[i].profileMatched
		}
		// The ranked tail is the profile's ordered criteria, or the default
		// tail when it declares none. Each criterion is a total key: an
		// unknown value sorts after every known value and then falls through.
		criteria := p.Sort
		if len(criteria) == 0 {
			criteria = defaultSortCriteria
		}
		for _, criterion := range criteria {
			v1, known1 := sortValueFor(criterion.Attribute, c1, p)
			v2, known2 := sortValueFor(criterion.Attribute, c2, p)
			if known1 != known2 {
				return known1
			}
			if !known1 || v1 == v2 {
				continue
			}
			if sortCriterionDirection(criterion) == sortDirectionAsc {
				return v1 < v2
			}
			return v1 > v2
		}
		if c1.OriginalIndex != c2.OriginalIndex {
			return c1.OriginalIndex < c2.OriginalIndex
		}
		// A stable final tie-break, so two candidates that tie on every key
		// (including a shared OriginalIndex) always order the same way.
		return stream.CandidateVariantID(c1) < stream.CandidateVariantID(c2)
	})
	for idx := range scored {
		candidates[idx] = scored[idx].candidate
	}
}
