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
// list.
//
// Identity matching here is deliberately stricter than the preference matcher:
// a transport that is already muxing a stream must keep that stream or fail
// closed, never move to a merely similar one. It matches by absolute container
// stream index first, then by a unique full-signature match, then by a
// conservative fallback that requires the transport facts (language, codec,
// channels, layout) and rejects any conflict in a known identity fact (title,
// embedded title, or MULTi language set).
//
// It returns -1 when the selected track cannot be uniquely re-identified: a
// stream that is genuinely gone, a previously-trustworthy absolute index that is
// absent, duplicate signatures, or an ambiguous fallback. The caller then
// records a server-enforced invalid selection rather than leaving the plan-time
// position in place to name whichever track now occupies it.
func remapAudioSelectionIndex(prev []models.AudioTrack, selectedIndex int, next []models.AudioTrack) int {
	if selectedIndex < 0 || selectedIndex >= len(prev) {
		return -1
	}
	selected := prev[selectedIndex]
	// Absolute stream index is only meaningful in a probed inventory; a
	// synthesized virtual list carries Index 0 for every track, where matching
	// on it would collide every track. When the plan-time inventory was probed
	// and recorded a real index for the selected stream, that index is the
	// strongest identity: if no candidate carries it the stream is gone (or the
	// file changed), and no weaker match may stand in for it.
	if audioInventoryProbed(prev) && selected.Index >= 0 {
		for i, track := range next {
			if track.Index == selected.Index {
				return i
			}
		}
		return -1
	}
	// A unique full-signature match is the next strongest identity.
	if idx := uniqueExactAudioTrack(next, AudioTrackSignatureFromTrack(selected)); idx >= 0 {
		return idx
	}
	// Conservative fallback: transport-fact matched, unique-only, and never a
	// candidate that contradicts a known identity fact. Two matches are
	// ambiguous and no match means the stream is gone; both are -1, never a
	// positional guess.
	match := -1
	for i, track := range next {
		if !trackStringEqual(track.Language, selected.Language) || !trackStringEqual(track.Codec, selected.Codec) {
			continue
		}
		if track.Channels != selected.Channels || !trackStringEqual(track.Layout, selected.Layout) {
			continue
		}
		if audioIdentityConflicts(selected, track) {
			continue
		}
		if match >= 0 {
			return -1
		}
		match = i
	}
	return match
}

// uniqueExactAudioTrack returns the position in tracks that uniquely matches sig
// by full signature (title, embedded title, codec, layout, channels, and MULTi
// language multiset) at the best regional language rank. Unlike
// findExactAudioTrack — which picks the first best-ranked match for preference
// selection — it requires the best-ranked match to be unique, so an overlay
// remap fails closed on duplicate signatures instead of selecting an arbitrary
// track. It returns -1 when nothing matches or when the best rank is shared.
func uniqueExactAudioTrack(tracks []models.AudioTrack, sig *userstore.AudioTrackSignature) int {
	if sig == nil || sig.IsZero() {
		return -1
	}
	best, bestRank, count := -1, lang.RankScriptConflict+1, 0
	for i, track := range tracks {
		if !audioTrackMatchesSignature(track, sig) {
			continue
		}
		rank := langMatchRank(track.Language, sig.Language)
		if rank < 0 {
			continue
		}
		switch {
		case rank < bestRank:
			best, bestRank, count = i, rank, 1
		case rank == bestRank:
			count++
		}
	}
	if count == 1 {
		return best
	}
	return -1
}

// audioIdentityConflicts reports whether a candidate track contradicts a known
// identity fact of the selected track: a title, an embedded title, or a MULTi
// language set that both sides state but disagree on. Facts only one side states
// are unknown, not conflicting, so they never veto a candidate; two stated facts
// that differ mean the candidate is provably a different stream.
func audioIdentityConflicts(selected, candidate models.AudioTrack) bool {
	if selected.Title != "" && candidate.Title != "" && !trackStringEqual(selected.Title, candidate.Title) {
		return true
	}
	if selected.EmbeddedTitle != "" && candidate.EmbeddedTitle != "" && !trackStringEqual(selected.EmbeddedTitle, candidate.EmbeddedTitle) {
		return true
	}
	selectedLanguages := sortedLanguages(selected.Languages)
	candidateLanguages := sortedLanguages(candidate.Languages)
	if len(selectedLanguages) > 0 && len(candidateLanguages) > 0 &&
		!languagesEqualMultiset(selectedLanguages, candidateLanguages) {
		return true
	}
	return false
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
