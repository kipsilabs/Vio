package stream

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Silo-Server/silo-server/internal/lang"
)

var streamSizePattern = regexp.MustCompile(`(?i)\b\d+(?:\.\d+)?\s*(?:TB|GB|MB)\b`)
var languagePattern = regexp.MustCompile(`(?i)\b(?:eng|en|fra|fre|fr|deu|ger|de|ita|es|spa|jpn|kor|zho|chi|por|rus|ara)\b`)
var (
	dolbyVisionPattern = regexp.MustCompile(`(?i)(?:\bdolby[ ._-]*vision\b|\bdv\b)`)
	atmosPattern       = regexp.MustCompile(`(?i)\batmos\b`)
	trueHDPattern      = regexp.MustCompile(`(?i)(?:\btrue[ ._-]*hd\b|\bthd\b)`)
	dtsHDPattern       = regexp.MustCompile(`(?i)\bdts[ ._-]*hd\b`)
	dtsPattern         = regexp.MustCompile(`(?i)\bdts\b`)
	eac3Pattern        = regexp.MustCompile(`(?i)(?:\be[ ._-]*ac[ ._-]*3\b|\bdd\+)`)
	ac3Pattern         = regexp.MustCompile(`(?i)(?:\bac[ ._-]*3\b|\bdd\b)`)
	aacPattern         = regexp.MustCompile(`(?i)\baac\b`)
	// audioChannelsPattern tolerates a channel count glued to a codec token
	// ("DDP5.1", "DD+5.1"): \b fails between two word chars (P→5), so the
	// leading edge is "start or a non-digit" and the count is group 1. The
	// trailing edge rejects numeric continuations ("5.10", "5.1080p") while
	// still allowing separator continuations ("5.1Ch", "5.1.2", ".5.1.").
	audioChannelsPattern = regexp.MustCompile(`(?i)(?:^|[^0-9])(7\.1(?:\.4)?|5\.1(?:\.2)?|2\.0|1\.0)(?:[^0-9]|$)`)
	// hevcPattern/h264Pattern tolerate dotted separators ("H.264", "X.265"):
	// release names use dots as spaces, so a plain Contains misses them.
	hevcPattern   = regexp.MustCompile(`(?i)(?:hevc|h[ ._-]*265|x[ ._-]*265)`)
	h264Pattern   = regexp.MustCompile(`(?i)(?:h[ ._-]*264|x[ ._-]*264|avc)`)
	av1Pattern    = regexp.MustCompile(`(?i)\bav1\b`)
	tenBitPattern = regexp.MustCompile(`(?i)\b(?:10[ ._-]?bit|hi10p?)\b`)
	imaxPattern   = regexp.MustCompile(`(?i)\bimax(?:[ ._-]enhanced)?\b`)
	// multiAudioPattern matches audio-MULTI markers: MULTI or DUAL with an
	// explicit audio qualifier (audio, lang, language), the standalone
	// MULTILINGUAL word, or a bare MULTI. Go's regexp has no lookahead, so
	// the SUBS exclusion lives in the match guard below (multiSubsSpan):
	// MULTI SUBS / MULTISUBS advertises subtitle tracks, never audio
	// (setting IsMultiAudio for it lets single-audio releases pass
	// MULTI-audio profiles).
	multiAudioPattern = regexp.MustCompile(`(?i)\b(?:multilingual|dual[ ._-]*audio|multi(?:[ ._-]*(?:audio|lang|language))?)\b`)
	// multiSubsSpan matches the subtitle spans a bare MULTI must not be read
	// from: MULTI SUBS / MULTI-SUB / MULTISUBS (and the bare SUBS/SUB words
	// themselves, so "MULTI … SUBS" with other tokens between still counts
	// as a subtitle span). Stripping these before the audio test leaves only
	// the audio-context MULTI markers behind.
	multiSubsSpanPattern = regexp.MustCompile(`(?i)\bmulti[ ._-]*subs?\b|\bmultisubs?\b|\bsubs?\b`)
	dualPattern          = regexp.MustCompile(`(?i)\bdual[ ._-]*audio\b`)
	releaseGroupPattern  = regexp.MustCompile(`(?i)-([a-zA-Z0-9]+)(?:\[.*?\])?$`)
	// regionalLangPattern covers the release-name regional/script forms the
	// parser observes in the wild. Regional spans are masked before the
	// bare-code pass (see ParseStreamMetadata), so adding a span here can only
	// move that language from bare to regional — never duplicate it.
	regionalLangPattern = regexp.MustCompile(`(?i)\b(pt-br|pt-pt|es-419|es-es|zh-hans|zh-hant|zh-tw|en-us|en-gb|fr-ca)\b`)
	fullNameLangPattern = regexp.MustCompile(`(?i)\b(english|french|german|spanish|italian|japanese|korean|russian|chinese|portuguese|hindi|arabic|dutch|polish|swedish|norwegian|danish|finnish|turkish|ukrainian)\b`)
)

// subtitlePattern matches subtitle-track markers in release metadata: common
// subtitle container extensions (.srt/.ass/.ssa/.sub/.vtt), the "subtitles"
// word, and forced/HI qualifiers. It does not match audio-only tokens so the
// same language code in the audio list does not bleed into subtitles.
var subtitlePattern = regexp.MustCompile(`(?i)\b(?:srt|ass|ssa|sub|vtt|pgs|sup|subtitle[s]?|forced|hi)\b`)
var subtitleLanguagePattern = regexp.MustCompile(`(?i)\b(?:eng|en|fra|fre|fr|deu|ger|de|ita|es|spa|jpn|kor|zho|chi|por|rus|ara)\b`)

type StreamCandidate struct {
	URL         string
	Name        string
	Description string
	Title       string
	// InfoHash is the Stremio stream's torrent info hash. Torrent-style addons
	// (and AltMount proxying one) identify the release by infoHash rather than a
	// behaviorHints.videoHash, and it is stable across a re-listing's result
	// renumbering, so it is a durable identity tier. It is provider-declared
	// payload, not a resolved URL, and never leaves the server process.
	InfoHash      string `json:"infoHash"`
	BehaviorHints struct {
		VideoHash    string         `json:"videoHash"`
		Filename     string         `json:"filename"`
		BingeGroup   string         `json:"bingeGroup"`
		VideoSize    int64          `json:"videoSize"`
		NotWebReady  bool           `json:"notWebReady"`
		ProxyHeaders map[string]any `json:"proxyHeaders"`
	}

	Resolution        string
	CodecVideo        string
	CodecAudio        string
	HasAtmos          bool
	HDR               string
	SourceType        string
	FileSize          int64
	Container         string
	AudioLanguages    []string
	SubtitleLanguages []string
	ExpiresAt         time.Time
	RequestHeaders    map[string]string
	QualityScore      int
	OriginalIndex     int
	AudioChannels     string   `json:"audioChannels,omitempty"`
	Bitrate           int      `json:"bitrate,omitempty"`
	Is10Bit           bool     `json:"is10Bit,omitempty"`
	IsMultiAudio      bool     `json:"isMultiAudio,omitempty"`
	IsDualAudio       bool     `json:"isDualAudio,omitempty"`
	VisualTags        []string `json:"visualTags,omitempty"`
	AudioTags         []string `json:"audioTags,omitempty"`
	// SourceConfirmed marks a candidate whose release the configured source of
	// truth (AltMount's completed/imported state, or Prowlarr as a fallback)
	// has already accepted. SourceFailed marks a release AltMount reports as
	// failed. SourcePending marks a release AltMount reports as actively
	// fetching (SABnzbd queue, not history): neither dead (so failed-drops
	// and the pruner must ignore it) nor ready (so the resolver may hold for
	// it). All three are provider-local derived state, never part of the
	// Stremio payload, so a provider response cannot spoof them.
	SourceConfirmed bool `json:"-"`
	SourceFailed    bool `json:"-"`
	SourcePending   bool `json:"-"`
	// SourceGUID is the stable GUID of the indexed release the classifier tied
	// this candidate to (Prowlarr exposes one per result). It is the dedup
	// identity when the provider carries no content hash, so two variants of
	// one release collapse even when their display names or sizes differ.
	// Like the flags above it is provider-local derived state and never part
	// of the Stremio payload.
	SourceGUID string `json:"-"`
	// CustomFormatRejected marks a candidate a configured custom format
	// rejects (an explicit Reject rule or a score at/below the discard line).
	// It is a transient ranking signal, never persisted: reject means
	// rank-last and last-resort selectable, not a hard drop, so it is
	// recomputed on every resolve/list and carried through device ranking so a
	// rejected candidate can never be promoted to the front by device fit.
	CustomFormatRejected bool `json:"-"`
}

// ParseStreamDetails fills Resolution, CodecVideo, CodecAudio, HasAtmos, HDR,
// SourceType, FileSize, Container, AudioLanguages, SubtitleLanguages,
// AudioChannels, Is10Bit, VisualTags, and AudioTags on the candidate from its
// name/description/title/URL text.
func ParseStreamDetails(s *StreamCandidate) {
	ParseStreamDetailsWithTitle(s, "")
}

// ParseStreamDetailsWithTitle fills derived fields on the candidate, optionally
// masking itemTitle to prevent title words from triggering false positives.
func ParseStreamDetailsWithTitle(s *StreamCandidate, itemTitle string) {
	parseStreamDetailsWithTitle(s, itemTitle)
	ParseStreamMetadata(s)
}

func parseStreamDetails(s *StreamCandidate) {
	parseStreamDetailsWithTitle(s, "")
}

func parseStreamDetailsWithTitle(s *StreamCandidate, itemTitle string) {
	metadataText := strings.ToLower(s.Name + " " + s.Description + " " + s.Title)
	fullText := metadataText + " " + strings.ToLower(s.URL)

	// Resolution
	// Prefer explicit provider metadata. URL tokens can contain unrelated
	// strings such as "4k" in an opaque identifier.
	resolutionText := metadataText
	if !hasResolutionMarker(resolutionText) {
		resolutionText = fullText
	}
	if itemTitle = strings.TrimSpace(itemTitle); len(itemTitle) >= 3 {
		escaped := regexp.QuoteMeta(strings.ToLower(itemTitle))
		titleRe := regexp.MustCompile(`(?i)(?:^|[^a-z0-9])` + strings.ReplaceAll(escaped, `\ `, `[ ._-]+`) + `(?:[^a-z0-9]|$)`)
		resolutionText = titleRe.ReplaceAllString(resolutionText, " ")
	}
	if strings.Contains(resolutionText, "2160p") || strings.Contains(resolutionText, "4k") || strings.Contains(resolutionText, "uhd") {
		s.Resolution = "2160p"
	} else if strings.Contains(resolutionText, "1080p") || strings.Contains(resolutionText, "1080i") {
		s.Resolution = "1080p"
	} else if strings.Contains(resolutionText, "720p") {
		s.Resolution = "720p"
	} else if strings.Contains(resolutionText, "480p") || strings.Contains(resolutionText, "sd") {
		s.Resolution = "480p"
	} else if strings.Contains(resolutionText, "bluray") || strings.Contains(resolutionText, "bdrip") || strings.Contains(resolutionText, "brrip") || strings.Contains(resolutionText, "remux") {
		s.Resolution = "1080p"
	} else if strings.Contains(resolutionText, "dvdrip") || strings.Contains(resolutionText, "dvd") {
		s.Resolution = "480p"
	}

	// Codec Video (separator-tolerant: release dots read as spaces, so plain
	// Contains misses "H.264"/"X.265").
	if hevcPattern.MatchString(fullText) {
		s.CodecVideo = "hevc"
	} else if h264Pattern.MatchString(fullText) {
		s.CodecVideo = "h264"
	} else if av1Pattern.MatchString(fullText) {
		s.CodecVideo = "av1"
	}

	// Codec Audio & Atmos
	s.HasAtmos = atmosPattern.MatchString(fullText)
	if trueHDPattern.MatchString(fullText) {
		s.CodecAudio = "truehd"
		s.AudioTags = append(s.AudioTags, "truehd")
	} else if dtsHDPattern.MatchString(fullText) {
		s.CodecAudio = "dts-hd"
		s.AudioTags = append(s.AudioTags, "dts-hd")
	} else if dtsPattern.MatchString(fullText) {
		s.CodecAudio = "dts"
		s.AudioTags = append(s.AudioTags, "dts")
	} else if eac3Pattern.MatchString(fullText) {
		s.CodecAudio = "eac3"
		s.AudioTags = append(s.AudioTags, "eac3")
	} else if ac3Pattern.MatchString(fullText) {
		s.CodecAudio = "ac3"
		s.AudioTags = append(s.AudioTags, "ac3")
	} else if aacPattern.MatchString(fullText) {
		s.CodecAudio = "aac"
		s.AudioTags = append(s.AudioTags, "aac")
	} else if strings.Contains(fullText, "flac") {
		s.CodecAudio = "flac"
		s.AudioTags = append(s.AudioTags, "flac")
	} else if strings.Contains(fullText, "opus") {
		s.CodecAudio = "opus"
		s.AudioTags = append(s.AudioTags, "opus")
	} else if s.HasAtmos {
		s.CodecAudio = "eac3"
	}
	if s.HasAtmos {
		s.AudioTags = append(s.AudioTags, "atmos")
	}

	// Audio Channels (group 1 carries the bare count: the leading boundary
	// char is not part of the match value, so "DDP5.1" still yields "5.1").
	if match := audioChannelsPattern.FindStringSubmatch(fullText); len(match) > 1 && match[1] != "" {
		ch := strings.ToLower(match[1])
		if strings.HasPrefix(ch, "7.1") {
			s.AudioChannels = "7.1"
		} else if strings.HasPrefix(ch, "5.1") {
			s.AudioChannels = "5.1"
		} else {
			s.AudioChannels = ch
		}
	}

	// HDR & Visual Tags
	if strings.Contains(fullText, "hdr10+") {
		s.HDR = "hdr10+"
		s.VisualTags = append(s.VisualTags, "hdr10+")
	} else if strings.Contains(fullText, "hdr10") {
		s.HDR = "hdr10"
		s.VisualTags = append(s.VisualTags, "hdr10")
	} else if dolbyVisionPattern.MatchString(fullText) {
		s.HDR = "dv"
		s.VisualTags = append(s.VisualTags, "dv")
	} else if strings.Contains(fullText, "hdr") {
		s.HDR = "hdr"
		s.VisualTags = append(s.VisualTags, "hdr")
	}
	if strings.Contains(fullText, "hlg") {
		s.VisualTags = append(s.VisualTags, "hlg")
	}
	if tenBitPattern.MatchString(fullText) {
		s.Is10Bit = true
		s.VisualTags = append(s.VisualTags, "10bit")
	}
	if imaxPattern.MatchString(fullText) {
		s.VisualTags = append(s.VisualTags, "imax")
	}

	// Source Type
	if strings.Contains(fullText, "remux") {
		s.SourceType = "remux"
	} else if strings.Contains(fullText, "web-dl") || strings.Contains(fullText, "webdl") || strings.Contains(fullText, "web") {
		s.SourceType = "web-dl"
	} else if strings.Contains(fullText, "bluray") || strings.Contains(fullText, "blu-ray") || strings.Contains(fullText, "bdrip") {
		s.SourceType = "bluray"
	} else if strings.Contains(fullText, "hdtv") {
		s.SourceType = "hdtv"
	}
}

func hasResolutionMarker(text string) bool {
	return strings.Contains(text, "2160p") || strings.Contains(text, "4k") ||
		strings.Contains(text, "1080p") || strings.Contains(text, "720p") ||
		strings.Contains(text, "480p")
}

func streamSize(s StreamCandidate) string {
	return streamSizePattern.FindString(s.Name + " " + s.Description + " " + s.Title)
}

// CandidateVideoHash returns the candidate's provider-declared content hash:
// the Stremio behaviorHints.videoHash when the addon sends one, else the
// torrent infoHash. Both identify the same bytes across a re-listing's result
// renumbering, so either is a durable hash tier. Empty means the provider
// declared no content hash.
func CandidateVideoHash(candidate StreamCandidate) string {
	if hash := strings.TrimSpace(candidate.BehaviorHints.VideoHash); hash != "" {
		return hash
	}
	return strings.TrimSpace(candidate.InfoHash)
}

// CandidateDeclaredSize returns the candidate's best provider-declared byte
// size: the parsed stream-level FileSize when present, else the Stremio
// behaviorHints.videoSize. Addons that report size only as a hint otherwise
// leave the name tier's size key unknown.
func CandidateDeclaredSize(candidate StreamCandidate) int64 {
	if candidate.FileSize > 0 {
		return candidate.FileSize
	}
	return candidate.BehaviorHints.VideoSize
}

// CandidateVariantID computes the stable 24-character hex candidate identity
// from stable stream fields. It matches the algorithm used by the plugin and
// ensures ?result= identifiers survive between plugin and core.
func CandidateVariantID(candidate StreamCandidate) string {
	urlIdentity := ""
	if parsed, err := url.Parse(strings.TrimSpace(candidate.URL)); err == nil {
		filename := strings.TrimSpace(candidate.BehaviorHints.Filename)
		if filename == "" {
			filename = path.Base(parsed.Path)
		}
		urlIdentity = strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host) + "/" + strings.ToLower(filename)
	}
	fingerprint := strings.Join([]string{
		strings.TrimSpace(candidate.Name), strings.TrimSpace(candidate.Title),
		strconv.FormatInt(candidate.FileSize, 10),
		strings.TrimSpace(candidate.Resolution), strings.TrimSpace(candidate.CodecVideo),
		strings.TrimSpace(candidate.CodecAudio), strings.TrimSpace(candidate.HDR),
		strings.TrimSpace(candidate.SourceType), strings.TrimSpace(candidate.Container),
		strings.Join(candidate.AudioLanguages, ","), strings.Join(candidate.SubtitleLanguages, ","),
		strings.TrimSpace(candidate.BehaviorHints.VideoHash),
		strings.TrimSpace(candidate.BehaviorHints.Filename),
		strings.TrimSpace(candidate.BehaviorHints.BingeGroup),
		strings.TrimSpace(candidate.InfoHash),
		urlIdentity,
	}, "\x00")
	digest := sha256.Sum256([]byte(fingerprint))
	return hex.EncodeToString(digest[:12])
}

// CandidateDisplayName formats a clean display name for a stream candidate.
func CandidateDisplayName(candidate StreamCandidate) string {
	name := strings.TrimSpace(candidate.Name)
	if name == "" {
		name = strings.TrimSpace(candidate.Title)
	}
	if name == "" {
		name = candidate.Resolution
	}
	if size := streamSize(candidate); size != "" && !strings.Contains(strings.ToLower(name), strings.ToLower(size)) {
		name += " · " + size
	}
	clean := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, name)
	return strings.TrimSpace(clean)
}

func canonicalAudioLanguage(token string) string {
	lower := strings.ToLower(strings.TrimSpace(token))
	switch lower {
	case "eng", "en", "english":
		return "ENG"
	case "fre", "fra", "fr", "french":
		return "FRE"
	case "ger", "deu", "de", "german", "deutsch":
		return "DEU"
	case "ita", "it", "italian":
		return "ITA"
	case "spa", "es", "spanish":
		return "SPA"
	case "jpn", "ja", "japanese":
		return "JPN"
	case "kor", "ko", "korean":
		return "KOR"
	case "rus", "ru", "russian":
		return "RUS"
	case "zho", "chi", "zh", "chinese":
		return "ZHO"
	case "por", "pt", "portuguese":
		return "POR"
	case "pt-br", "brazilian portuguese":
		return "PT-BR"
	case "pt-pt", "european portuguese":
		return "PT-PT"
	case "es-419":
		return "ES-419"
	case "es-es", "castilian":
		return "ES-ES"
	case "zh-hans", "simplified chinese":
		return "ZH-HANS"
	case "zh-hant", "zh-tw", "traditional chinese":
		return "ZH-HANT"
	case "en-us":
		return "EN-US"
	case "en-gb":
		return "EN-GB"
	case "fr-ca":
		return "FR-CA"
	case "ara", "ar", "arabic":
		return "ARA"
	case "hin", "hi", "hindi":
		return "HIN"
	case "nld", "dut", "nl", "dutch":
		return "NLD"
	case "pol", "pl", "polish":
		return "POL"
	case "swe", "sv", "swedish":
		return "SWE"
	case "nor", "no", "norwegian":
		return "NOR"
	case "dan", "da", "danish":
		return "DAN"
	case "fin", "fi", "finnish":
		return "FIN"
	case "tur", "tr", "turkish":
		return "TUR"
	case "ukr", "uk", "ukrainian":
		return "UKR"
	default:
		if len(lower) == 3 {
			if c := lang.Canonical(lower); c != "" && c != "und" && c != "mul" {
				return strings.ToUpper(lower)
			}
		}
		return ""
	}
}

// canonicalLanguageBaseCodes folds the canonical codes canonicalAudioLanguage
// emits onto their base (ISO 639-1) language. Bibliographic 3-letter forms
// ("FRE") are not understood by the x/text parser, so they are mapped here
// rather than left to lang.PrimaryLanguage.
var canonicalLanguageBaseCodes = map[string]string{
	"ENG": "en", "FRE": "fr", "DEU": "de", "ITA": "it", "SPA": "es",
	"JPN": "ja", "KOR": "ko", "RUS": "ru", "ZHO": "zh", "POR": "pt",
	"ARA": "ar", "HIN": "hi", "NLD": "nl", "POL": "pl", "SWE": "sv",
	"NOR": "no", "DAN": "da", "FIN": "fi", "TUR": "tr", "UKR": "uk",
}

// CanonicalLanguageBase folds a language token onto a stable base-language
// key. ISO 639-1/2 codes, their bibliographic variants, English display names
// and regional/script forms all collapse to one key per base language:
// "EN-US", "en-GB", "ENG", "en" and "English" yield "en"; "FR-CA", "FRE" and
// "fr" yield "fr". It is the de-duplication key shared by the stream parser
// and the stored/protocol subtitle inventories. Callers that must display a
// language keep the more specific canonical code separately. Unidentifiable
// input yields "".
func CanonicalLanguageBase(value string) string {
	code := canonicalAudioLanguage(value)
	if code == "" {
		return ""
	}
	if base, ok := canonicalLanguageBaseCodes[code]; ok {
		return base
	}
	if index := strings.IndexByte(code, '-'); index > 0 {
		base := strings.ToLower(code[:index])
		if base != "" && base != "und" && base != "mul" {
			return base
		}
	}
	if base := lang.PrimaryLanguage(code); base != "" && base != "und" && base != "mul" {
		return base
	}
	lower := strings.ToLower(code)
	if index := strings.IndexByte(lower, '-'); index > 0 {
		lower = lower[:index]
	}
	return lower
}

// DedupeLanguageAliases collapses language aliases that share a base-language
// key, keeping exactly one entry per base language. The first occurrence's
// position is preserved; when a later alias is more specific (it carries a
// region or script subtag) and the kept entry for that base is not, the more
// specific code replaces it, so a regional tag wins over its bare base. Order
// is otherwise first-seen, which makes the result deterministic. Empty or
// unidentifiable entries are dropped. Returns nil when nothing remains.
func DedupeLanguageAliases(languages []string) []string {
	if len(languages) == 0 {
		return nil
	}
	out := make([]string, 0, len(languages))
	position := make(map[string]int, len(languages))
	for _, language := range languages {
		language = strings.TrimSpace(language)
		if language == "" {
			continue
		}
		base := CanonicalLanguageBase(language)
		if base == "" {
			continue
		}
		index, seen := position[base]
		if !seen {
			position[base] = len(out)
			out = append(out, language)
			continue
		}
		if !strings.Contains(out[index], "-") && strings.Contains(language, "-") {
			out[index] = language
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ParseStreamMetadata fills FileSize, AudioLanguages, SubtitleLanguages,
// Container, ExpiresAt, and RequestHeaders on the candidate.
func ParseStreamMetadata(s *StreamCandidate) {
	text := s.Name + " " + s.Description + " " + s.Title
	if size := streamSizePattern.FindString(text); size != "" {
		parts := strings.Fields(strings.ToUpper(size))
		if len(parts) == 2 {
			if value, err := strconv.ParseFloat(parts[0], 64); err == nil {
				multiplier := float64(1)
				switch parts[1] {
				case "TB":
					multiplier = 1e12
				case "GB":
					multiplier = 1e9
				case "MB":
					multiplier = 1e6
				}
				s.FileSize = int64(value * multiplier)
			}
		}
	}
	// A provider that declares an exact size hint but no parseable size text
	// still gets a size tier, so its name+size identity is re-matchable.
	if s.FileSize <= 0 && s.BehaviorHints.VideoSize > 0 {
		s.FileSize = s.BehaviorHints.VideoSize
	}

	cleanText := text
	if match := releaseGroupPattern.FindStringSubmatch(text); len(match) > 1 {
		if strings.EqualFold(match[1], "ind") {
			cleanText = strings.TrimSuffix(cleanText, match[0])
		}
	}

	// Regional spans are masked before the bare-code pass: in "es-419" the
	// "-" is a word boundary, so a naive bare scan would also emit "SPA"
	// (from "es") and let the wrong regional variant gain bare-language
	// rank. Masking keeps exactly one token per span — the regional code —
	// so es-419 yields ES-419 only, pt-BR yields PT-BR only.
	maskedText := regionalLangPattern.ReplaceAllString(cleanText, " ")
	seen := map[string]bool{}
	for _, match := range regionalLangPattern.FindAllString(cleanText, -1) {
		if code := canonicalAudioLanguage(match); code != "" && !seen[code] {
			seen[code] = true
			s.AudioLanguages = append(s.AudioLanguages, code)
		}
	}
	for _, match := range fullNameLangPattern.FindAllString(maskedText, -1) {
		if code := canonicalAudioLanguage(match); code != "" && !seen[code] {
			seen[code] = true
			s.AudioLanguages = append(s.AudioLanguages, code)
		}
	}
	for _, match := range languagePattern.FindAllString(strings.ToLower(maskedText), -1) {
		if code := canonicalAudioLanguage(match); code != "" && !seen[code] {
			seen[code] = true
			s.AudioLanguages = append(s.AudioLanguages, code)
		}
	}

	// multiAudioPattern alone: the removed `strings.Contains(name, "multi")`
	// fallback matched titles like "Multiplicity" and "The Multiverse" and
	// advertised them as multi-audio releases. A bare word "multi" still
	// matches through the pattern's word boundary — but only outside a
	// subtitle span: strip the SUBS spans first, then test what remains, so
	// "PT-BR.MULTI.TrueHD" (audio MULTI, no SUBS anywhere) keeps the flag
	// while "MULTI.SUBS" (the MULTI sits inside a subtitle span) does not.
	// Explicit audio qualifiers (MULTI.AUDIO, DUAL.AUDIO, MULTILINGUAL) match
	// on the stripped text the same way they match the raw text.
	if multiAudioPattern.MatchString(multiSubsSpanPattern.ReplaceAllString(text, " ")) {
		s.IsMultiAudio = true
	}
	if dualPattern.MatchString(text) {
		s.IsDualAudio = true
		s.IsMultiAudio = true
	}

	// Subtitle languages: only parse when the release text actually carries a
	// subtitle marker (an extension, the "subtitles" word, or a forced/HI
	// qualifier). A bare language token that only appears in the audio context
	// (e.g. "English DD5.1") must not be advertised as a subtitle track.
	if subtitlePattern.MatchString(text) {
		// Collect every canonical match in pass order (regional first, so a
		// specific code precedes its bare base), then collapse aliases of one
		// base language onto a single entry. The previous exact-code seen-set
		// let "…en-US.srt" yield both "EN-US" (regional pass) and "ENG" (base
		// pass), because "-" is a word boundary; downstream surfaces then
		// listed the one language twice.
		var matches []string
		for _, match := range regionalLangPattern.FindAllString(cleanText, -1) {
			if code := canonicalAudioLanguage(match); code != "" {
				matches = append(matches, code)
			}
		}
		for _, match := range fullNameLangPattern.FindAllString(cleanText, -1) {
			if code := canonicalAudioLanguage(match); code != "" {
				matches = append(matches, code)
			}
		}
		for _, match := range subtitleLanguagePattern.FindAllString(strings.ToLower(cleanText), -1) {
			if code := canonicalAudioLanguage(match); code != "" {
				matches = append(matches, code)
			}
		}
		s.SubtitleLanguages = DedupeLanguageAliases(append(s.SubtitleLanguages, matches...))
	}

	// Container resolution: prefer behaviorHints.filename, then URL, then text
	s.Container = inferContainer(s.BehaviorHints.Filename, s.URL, text)

	// URL expiry extraction
	s.ExpiresAt = parseURLExpiration(s.URL)

	// Request headers extraction from behaviorHints.proxyHeaders
	s.RequestHeaders = extractProxyRequestHeaders(s.BehaviorHints.ProxyHeaders)
}

func inferContainer(filename, streamURL, text string) string {
	knownExts := []string{".mkv", ".mp4", ".webm", ".avi", ".mov", ".ts", ".m2ts", ".m4v"}
	if filename != "" {
		ext := strings.ToLower(path.Ext(filename))
		for _, k := range knownExts {
			if ext == k {
				return strings.TrimPrefix(k, ".")
			}
		}
	}
	lowerURL := strings.ToLower(streamURL)
	for _, ext := range knownExts {
		if strings.Contains(lowerURL, ext) {
			return strings.TrimPrefix(ext, ".")
		}
	}
	lowerText := strings.ToLower(text)
	for _, ext := range knownExts {
		if strings.Contains(lowerText, ext) {
			return strings.TrimPrefix(ext, ".")
		}
	}
	return ""
}

func parseURLExpiration(rawURL string) time.Time {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed == nil {
		return time.Time{}
	}
	q := parsed.Query()

	// 1. AWS SigV4 signed URLs: X-Amz-Expires (duration in seconds) + X-Amz-Date.
	// A parseable expiry is always preserved, even when already past: dropping
	// it to zero would make an expired URL look like it never expires.
	amzExpires := strings.TrimSpace(q.Get("X-Amz-Expires"))
	if amzExpires == "" {
		amzExpires = strings.TrimSpace(q.Get("x-amz-expires"))
	}
	if amzExpires != "" {
		if durSec, err := strconv.ParseInt(amzExpires, 10, 64); err == nil && durSec > 0 {
			amzDate := strings.TrimSpace(q.Get("X-Amz-Date"))
			if amzDate == "" {
				amzDate = strings.TrimSpace(q.Get("x-amz-date"))
			}
			if amzDate != "" {
				var baseTime time.Time
				if t, err := time.Parse("20060102T150405Z", amzDate); err == nil {
					baseTime = t.UTC()
				} else if t, err := time.Parse(time.RFC3339, amzDate); err == nil {
					baseTime = t.UTC()
				}
				if !baseTime.IsZero() {
					return baseTime.Add(time.Duration(durSec) * time.Second).Add(-15 * time.Second)
				}
			}
		}
	}

	// 2. Absolute Unix timestamp expiration params. Same rule: a parseable
	// timestamp is preserved even when past, so callers can distinguish
	// expired (past time) from absent/invalid (zero time).
	for _, key := range []string{"expires", "expire", "exp", "Expires"} {
		val := strings.TrimSpace(q.Get(key))
		if val == "" {
			continue
		}
		if sec, err := strconv.ParseInt(val, 10, 64); err == nil && sec > 0 {
			if sec > 1000000000 { // unix timestamp
				return time.Unix(sec, 0).UTC().Add(-15 * time.Second) // safety margin
			}
		}
	}
	return time.Time{}
}

func extractProxyRequestHeaders(proxyHeaders map[string]any) map[string]string {
	if len(proxyHeaders) == 0 {
		return nil
	}
	reqRaw, ok := proxyHeaders["request"]
	if !ok {
		reqRaw = proxyHeaders
	}
	reqMap, ok := reqRaw.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string)
	for k, v := range reqMap {
		sVal, ok := v.(string)
		if !ok || strings.TrimSpace(sVal) == "" {
			continue
		}
		lowerK := strings.ToLower(strings.TrimSpace(k))
		if lowerK == "referer" || lowerK == "origin" || lowerK == "user-agent" {
			out[k] = strings.TrimSpace(sVal)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ResolutionScore ranks a normalized resolution string for quality sorting.
// Higher is better: 2160p=4, 1080p=3, 720p=2, 480p=1, unknown=0.
func ResolutionScore(res string) int {
	switch res {
	case "2160p":
		return 4
	case "1080p":
		return 3
	case "720p":
		return 2
	case "480p":
		return 1
	}
	return 0
}

// SourceScore ranks a source type string for quality sorting.
// Higher is better: remux=4, bluray=3, web-dl=2, hdtv=1, unknown=0.
func SourceScore(src string) int {
	switch src {
	case "remux":
		return 4
	case "bluray":
		return 3
	case "web-dl":
		return 2
	case "hdtv":
		return 1
	}
	return 0
}

// NormalizeResolution canonicalizes a resolution label ("4k" -> "2160p").
func NormalizeResolution(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "4k", "uhd", "2160":
		return "2160p"
	case "2k", "1440":
		return "1440p"
	case "1080":
		return "1080p"
	case "720":
		return "720p"
	case "480":
		return "480p"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

// AudioChannelsScore ranks channel counts for quality sorting.
// Higher is better: 7.1=3, 5.1=2, 2.0=1, unknown=0.
func AudioChannelsScore(ch string) int {
	switch ch {
	case "7.1":
		return 3
	case "5.1":
		return 2
	case "2.0":
		return 1
	default:
		return 0
	}
}

// CandidateLanguageMatchRank ranks how well a candidate matches the preferred
// language tag using upstream lang package semantics:
//
//	0: exact BCP-47 / regional tag match (e.g. pt-BR == pt-BR)
//	1: bare language tag match (e.g. pt matches pt-BR)
//	2: regional variant match (e.g. pt-PT matches pt-BR)
//	3: candidate is MULTi audio (advertises multiple languages)
//
// -1: no match
func CandidateLanguageMatchRank(candidate StreamCandidate, preferred string) int {
	preferred = lang.CompatibleTag(preferred)
	if preferred == "" {
		return -1
	}
	bestRank := -1
	for _, code := range candidate.AudioLanguages {
		rank := candidateLangMatchRank(code, preferred)
		if rank >= 0 && (bestRank == -1 || rank < bestRank) {
			bestRank = rank
		}
		if bestRank == 0 {
			return 0
		}
	}
	if bestRank >= 0 {
		return bestRank
	}
	if candidate.IsMultiAudio || candidate.IsDualAudio {
		return 3
	}
	return -1
}

// CandidateHasDistinctAudioLanguages reports whether the candidate carries
// at least n distinct audio languages by base language (so es-419 beside a
// bare "es" alias counts once, not twice). It is the alias-proof gate for
// RequireMultiAudio: the flag alone is a release-text claim, and the raw
// list length counts aliases.
func CandidateHasDistinctAudioLanguages(candidate StreamCandidate, n int) bool {
	if n <= 0 {
		return true
	}
	seen := make(map[string]struct{}, len(candidate.AudioLanguages))
	for _, language := range candidate.AudioLanguages {
		if base := CanonicalLanguageBase(language); base != "" {
			seen[base] = struct{}{}
			if len(seen) >= n {
				return true
			}
		}
	}
	return false
}

// CandidateHasLanguage reports whether candidate carries or supports the preferred language.
func CandidateHasLanguage(candidate StreamCandidate, preferred string) bool {
	return CandidateLanguageMatchRank(candidate, preferred) >= 0
}

func candidateLangMatchRank(candidate, preferred string) int {
	candidate = lang.CompatibleTag(candidate)
	preferred = lang.CompatibleTag(preferred)
	if candidate == "" || preferred == "" {
		return -1
	}
	if strings.EqualFold(candidate, preferred) {
		return 0
	}
	candidateBase := lang.PrimaryLanguage(candidate)
	preferredBase := lang.PrimaryLanguage(preferred)
	if candidateBase == "" || candidateBase != preferredBase {
		return -1
	}
	if !strings.Contains(candidate, "-") {
		return 1
	}
	return 2
}
