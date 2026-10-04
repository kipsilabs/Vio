package playback

import (
	"slices"
	"strings"

	"github.com/Silo-Server/silo-server/internal/lang"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/userstore"
)

// AudioTrackSignatureFromTrack converts a probed audio track into the stable
// signature persisted for series-level sticky audio preferences.
func AudioTrackSignatureFromTrack(track models.AudioTrack) *userstore.AudioTrackSignature {
	sig := &userstore.AudioTrackSignature{
		Language:      strings.TrimSpace(track.Language),
		Title:         strings.TrimSpace(track.Title),
		EmbeddedTitle: strings.TrimSpace(track.EmbeddedTitle),
		Codec:         strings.TrimSpace(track.Codec),
		Layout:        strings.TrimSpace(track.Layout),
		Channels:      track.Channels,
		Languages:     sortedLanguages(track.Languages),
	}
	if sig.IsZero() {
		return nil
	}
	return sig
}

// sortedLanguages returns a sorted, canonicalized copy of a track's MULTi
// language list so signatures compare as multisets regardless of the order a
// container listed them in. Empty/undecodable entries are dropped.
func sortedLanguages(languages []string) []string {
	if len(languages) == 0 {
		return nil
	}
	out := make([]string, 0, len(languages))
	for _, code := range languages {
		if canonical := lang.Canonical(strings.TrimSpace(code)); canonical != "" {
			out = append(out, canonical)
		}
	}
	if len(out) == 0 {
		return nil
	}
	slices.Sort(out)
	return out
}

func findExactAudioTrack(tracks []models.AudioTrack, sig *userstore.AudioTrackSignature) int {
	if sig == nil || sig.IsZero() {
		return -1
	}
	// Several tracks can share a signature apart from their regional tag, so
	// rank language matches instead of taking the first compatible hit: an
	// exact tag wins over another variant, and a legacy bare-language
	// signature still matches a regional track when nothing closer exists.
	best, bestRank := -1, lang.RankScriptConflict+1
	for i, track := range tracks {
		if !audioTrackMatchesSignature(track, sig) {
			continue
		}
		if rank := langMatchRank(track.Language, sig.Language); rank >= 0 && rank < bestRank {
			best, bestRank = i, rank
		}
	}
	return best
}

func audioTrackMatchesSignature(track models.AudioTrack, sig *userstore.AudioTrackSignature) bool {
	if sig == nil || sig.IsZero() {
		return false
	}
	return langMatch(track.Language, sig.Language) &&
		trackStringEqual(track.Title, sig.Title) &&
		trackStringEqual(track.EmbeddedTitle, sig.EmbeddedTitle) &&
		trackStringEqual(track.Codec, sig.Codec) &&
		trackStringEqual(track.Layout, sig.Layout) &&
		track.Channels == sig.Channels &&
		languagesEqualMultiset(sortedLanguages(track.Languages), sortedLanguages(sig.Languages))
}

// languagesEqualMultiset reports whether two language-code lists are the same
// multiset. Both sides are normalized through sortedLanguages (sorted,
// canonicalized) before the element-wise comparison, so container ordering and
// code spelling differences never produce a false mismatch.
func languagesEqualMultiset(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func trackStringEqual(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}
