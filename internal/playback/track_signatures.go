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

// remapAudioSelectionIndex returns the position in next that names the same
// audio track the session had selected at prev[selectedIndex], so replacing the
// carried inventory cannot silently move the transport's derived 0:a:N ordinal
// onto a different stream when the verified list reorders or trims the plan-time
// list. It matches by absolute container stream index first (the probe's
// recorded position, stable across a reorder), then by the persisted track
// signature, then by language, codec, and the transport-relevant channel facts.
//
// It returns -1 when the selected track cannot be uniquely re-identified - a
// genuinely removed stream, or one whose weak language/codec fallback matches
// more than one track (distinct commentary or layout tracks of the same
// language). The caller then records a server-enforced invalid selection rather
// than leaving the plan-time position in place to name whichever track now
// occupies it.
func remapAudioSelectionIndex(prev []models.AudioTrack, selectedIndex int, next []models.AudioTrack) int {
	if selectedIndex < 0 || selectedIndex >= len(prev) {
		return -1
	}
	selected := prev[selectedIndex]
	// Absolute stream index is only meaningful in a probed inventory; a
	// synthesized virtual list carries Index 0 for every track, where matching
	// on it would collide every track.
	if audioInventoryProbed(prev) && selected.Index >= 0 {
		for i, track := range next {
			if track.Index == selected.Index {
				return i
			}
		}
	}
	if idx := findExactAudioTrack(next, AudioTrackSignatureFromTrack(selected)); idx >= 0 {
		return idx
	}
	// The weak fallback is deliberately transport-fact matched and unique-only:
	// language and codec alone can substitute a distinct commentary or layout
	// track of the same language, which would point the running mux at the wrong
	// stream. Two matches are ambiguous and no match means the stream is gone;
	// both are -1, never a positional guess.
	match := -1
	for i, track := range next {
		if !(trackStringEqual(track.Language, selected.Language) && trackStringEqual(track.Codec, selected.Codec)) {
			continue
		}
		if track.Channels != selected.Channels || !trackStringEqual(track.Layout, selected.Layout) {
			continue
		}
		if match >= 0 {
			return -1
		}
		match = i
	}
	return match
}

// audioInventoryProbed reports whether an audio inventory came from a real
// probe: any track carrying a positive absolute container stream index. It
// mirrors the classification playback.AudioStreamOrdinal uses, so the two index
// domains never disagree about which mapping applies.
func audioInventoryProbed(tracks []models.AudioTrack) bool {
	for _, track := range tracks {
		if track.Index > 0 {
			return true
		}
	}
	return false
}
