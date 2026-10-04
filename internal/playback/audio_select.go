package playback

import (
	"strings"

	"github.com/Silo-Server/silo-server/internal/lang"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/userstore"
)

// OriginalLanguageSentinel is the value stored in audio language preference
// columns to mean "use the media item's original language." It is resolved
// to a concrete language code in the playback handler before reaching
// SelectAudioTrack.
const OriginalLanguageSentinel = "original"

// OriginalLanguageTag is the settings-contract spelling of the same choice for
// playback.audio_language. The contract only stores language tags, so it uses
// the private-use tag catalog.metadata_language already defines for "each
// item's original language".
const OriginalLanguageTag = "x-silo-original"

// IsOriginalLanguagePreference reports whether a stored audio language
// preference means the media item's original language.
func IsOriginalLanguagePreference(preference string) bool {
	preference = strings.TrimSpace(preference)
	return strings.EqualFold(preference, OriginalLanguageSentinel) || strings.EqualFold(preference, OriginalLanguageTag)
}

// AudioTrackPreference holds a per-series audio track preference.
type AudioTrackPreference struct {
	AudioTrackIndex int
	AudioLanguage   string
	TrackSignature  *userstore.AudioTrackSignature
}

// langMatch accepts compatible languages for previously saved track selections,
// rejecting explicitly conflicting scripts.
func langMatch(a, b string) bool {
	rank := langMatchRank(a, b)
	return rank >= 0 && rank < lang.RankScriptConflict
}

// trackLanguageRank returns the best language rank for a track against the
// preferred language, considering both the primary code and the MULTi
// language list. -1 when nothing matches.
func trackLanguageRank(track models.AudioTrack, preferred string) int {
	best := -1
	if rank := langMatchRank(track.Language, preferred); rank >= 0 {
		best = rank
		if best == 0 {
			return best
		}
	}
	for _, code := range track.Languages {
		if rank := langMatchRank(code, preferred); rank >= 0 && (best < 0 || rank < best) {
			best = rank
			if best == 0 {
				break
			}
		}
	}
	return best
}

// trackHasLanguage reports whether the track carries the preferred language,
// either as its primary code or anywhere in its MULTi language list,
// without an explicit script conflict.
func trackHasLanguage(track models.AudioTrack, preferred string) bool {
	rank := trackLanguageRank(track, preferred)
	return rank >= 0 && rank < lang.RankScriptConflict
}

// langMatchRank delegates to lang.MatchRank to rank language closeness:
// exact (0) > matching script/region (1-2) > bare tag (3) >
// regional variant (4) > conflicting script (5).
func langMatchRank(candidate, preferred string) int {
	return lang.MatchRank(candidate, preferred)
}

// SelectAudioTrack determines which audio track to use based on preferences.
//
// Priority:
// 1. Series preference exact track signature
// 2. Series preference index (if track exists at that index with matching language)
// 3. Series preference language (best language match)
// 4. Profile preferred language (best language match)
// 5. File's default track (first track with Default: true)
// 6. First track (index 0)
//
// Language matches rank exact tag > matching script/region > bare language >
// another variant of the same language > conflicting script. Track order breaks
// ties within a language rank. Saved signatures and compatible saved indices
// take precedence over language-only preferences.
func SelectAudioTrack(tracks []models.AudioTrack, preferredLang string, seriesPref *AudioTrackPreference) int {
	if len(tracks) == 0 {
		return 0
	}

	// 1. Series preference: try exact signature match first.
	if seriesPref != nil {
		if idx := findExactAudioTrack(tracks, seriesPref.TrackSignature); idx >= 0 {
			return idx
		}

		// 2. Series preference: honor the saved index when its track is still
		// the same language. Saved preferences may carry a bare tag while the
		// scanner now preserves regional subtags, so any compatible match keeps
		// the index rather than falling through to a different track.
		if seriesPref.AudioTrackIndex >= 0 && seriesPref.AudioTrackIndex < len(tracks) {
			if trackHasLanguage(tracks[seriesPref.AudioTrackIndex], seriesPref.AudioLanguage) {
				return seriesPref.AudioTrackIndex
			}
		}

		// 3. Series preference: fall back to language match.
		if seriesPref.AudioLanguage != "" {
			if idx := bestLanguageTrack(tracks, seriesPref.AudioLanguage); idx >= 0 {
				return idx
			}
		}
	}

	// 4. Profile language preference.
	if preferredLang != "" {
		if idx := bestLanguageTrack(tracks, preferredLang); idx >= 0 {
			return idx
		}
	}

	// 5. File's default track.
	for i, t := range tracks {
		if t.Default {
			return i
		}
	}

	// 6. First track.
	return 0
}

func bestLanguageTrack(tracks []models.AudioTrack, preferred string) int {
	best, bestRank := -1, lang.RankScriptConflict+1
	for i, track := range tracks {
		if rank := trackLanguageRank(track, preferred); rank >= 0 && rank < bestRank {
			best, bestRank = i, rank
			if bestRank == 0 {
				break
			}
		}
	}
	return best
}

// MatchAudioTrackAcrossVersions maps a selection made against one file's
// audio inventory onto another version of the same content. Track ordering is
// not stable across encodes, so carrying the raw ordinal can select a different
// language. Prefer the stable signature, then the selected language, and
// finally the effective file's default track.
func MatchAudioTrackAcrossVersions(
	requestedTracks []models.AudioTrack,
	effectiveTracks []models.AudioTrack,
	requestedIndex int,
) int {
	if len(effectiveTracks) == 0 {
		return 0
	}
	if len(requestedTracks) == 0 {
		return SelectAudioTrack(effectiveTracks, "", nil)
	}
	if requestedIndex < 0 || requestedIndex >= len(requestedTracks) {
		requestedIndex = SelectAudioTrack(requestedTracks, "", nil)
	}

	selected := requestedTracks[requestedIndex]
	signature := AudioTrackSignatureFromTrack(selected)
	if idx := findExactAudioTrack(effectiveTracks, signature); idx >= 0 {
		return idx
	}
	for _, code := range crossVersionAudioLanguages(selected) {
		candidate := SelectAudioTrack(effectiveTracks, "", &AudioTrackPreference{
			AudioTrackIndex: requestedIndex,
			AudioLanguage:   code,
			TrackSignature:  signature,
		})
		if trackHasLanguage(effectiveTracks[candidate], code) {
			return candidate
		}
	}
	return SelectAudioTrack(effectiveTracks, "", nil)
}

// crossVersionAudioLanguages returns the concrete languages a track carries,
// primary code first, then its MULTi language list, deduplicated by canonical
// tag form. The "und"/"mul" sentinels are placeholders and are skipped.
func crossVersionAudioLanguages(track models.AudioTrack) []string {
	codes := make([]string, 0, len(track.Languages)+1)
	if primary := track.Language; !isPlaceholderLanguage(primary) {
		codes = append(codes, primary)
	}
	for _, code := range track.Languages {
		if isPlaceholderLanguage(code) {
			continue
		}
		duplicate := false
		for _, existing := range codes {
			if lang.CompatibleTag(existing) == lang.CompatibleTag(code) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			codes = append(codes, code)
		}
	}
	return codes
}

// isPlaceholderLanguage reports whether a language code carries no concrete
// language intent: empty, "und"/"mul", or their long forms.
func isPlaceholderLanguage(code string) bool {
	canonical := lang.Canonical(code)
	return canonical == "" || canonical == "und" || canonical == "mul"
}

// BrowserSupportsAudioCodec returns true if the given audio codec can be
// played natively by web browsers without transcoding.
func BrowserSupportsAudioCodec(codec string) bool {
	switch strings.ToLower(codec) {
	case "aac", "mp3", "opus", "vorbis", "flac":
		return true
	default:
		return false
	}
}
