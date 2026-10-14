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

// ClientAudioSignatureMatchesTrack reports whether an audio track signature a
// client named (or the start-time snapshot persisted) is consistent with one
// probed track. Unlike the strict track-vs-track comparison
// (audioTrackMatchesSignature, matchExactAudioTrack), every member the caller
// left empty is a wildcard: /api/v2 documents each AudioTrackSignature member
// as optional and absent as equal to its zero value, so a signature that names
// only codec and channels still identifies a probed track that also reports a
// language, title, layout and default flag.
//
// A probed track with no identifying fields at all (a provider placeholder, or
// a row that carries only a synthetic index and default flag) has nothing to
// compare, so it cannot refute the selection: the caller must treat the
// inventory as uninformative rather than absent. A fully populated signature
// reduces to the same member-by-member equality a strict comparison makes.
//
// This is deliberately a separate entry point: the exact matchers still fail
// closed on ambiguity for probe-derived re-identification (see
// remapAudioSelectionIndex), while a client/start signature is only ever
// checked for presence, never used to pick among several streams.
func ClientAudioSignatureMatchesTrack(track models.AudioTrack, sig *userstore.AudioTrackSignature) bool {
	if sig == nil || sig.IsZero() {
		return true
	}
	candidate := AudioTrackSignatureFromTrack(track)
	if candidate == nil {
		return true
	}
	if sig.Language != "" && !trackStringEqual(sig.Language, candidate.Language) {
		return false
	}
	if sig.Title != "" && !trackStringEqual(sig.Title, candidate.Title) {
		return false
	}
	if sig.EmbeddedTitle != "" && !trackStringEqual(sig.EmbeddedTitle, candidate.EmbeddedTitle) {
		return false
	}
	if sig.Codec != "" && !trackStringEqual(sig.Codec, candidate.Codec) {
		return false
	}
	if sig.Layout != "" && !trackStringEqual(sig.Layout, candidate.Layout) {
		return false
	}
	if sig.Channels != 0 && sig.Channels != candidate.Channels {
		return false
	}
	if len(sig.Languages) > 0 && !languagesEqualMultiset(sortedLanguages(sig.Languages), candidate.Languages) {
		return false
	}
	return true
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
// absent, a full-signature match that ties because several candidates are
// canonically equivalent (e.g. "en" and "eng"), or an ambiguous fallback. The
// caller then records a server-enforced invalid selection rather than leaving
// the plan-time position in place to name whichever track now occupies it.
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
	// A unique full-signature match is the next strongest identity. Ambiguity
	// must fail closed here, before the fallback: the exact matcher canonicalizes
	// language tags while the fallback compares raw strings, so an exact tie
	// between e.g. "en" and "eng" would leave the fallback admitting the single
	// raw-string match and silently hide the ambiguity instead of refusing to
	// choose.
	switch idx, match := matchExactAudioTrack(next, AudioTrackSignatureFromTrack(selected)); match {
	case audioExactUnique:
		return idx
	case audioExactAmbiguous:
		return -1
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

// audioExactMatch classifies the outcome of a full-signature lookup so the
// caller can tell "no candidate matched" (safe to try the conservative
// fallback) apart from "several candidates tied at the best language rank"
// (ambiguous, must fail closed without consulting the weaker fallback).
type audioExactMatch int

const (
	// audioExactAbsent means no candidate matched the full signature.
	audioExactAbsent audioExactMatch = iota
	// audioExactUnique means exactly one candidate matched at the best
	// language rank.
	audioExactUnique
	// audioExactAmbiguous means several candidates tied at the best language
	// rank, so no single stream can be re-identified.
	audioExactAmbiguous
)

// matchExactAudioTrack returns the position in tracks that uniquely matches sig
// by full signature (title, embedded title, codec, layout, channels, and MULTi
// language multiset) at the best regional language rank, together with how the
// lookup resolved. Unlike findExactAudioTrack — which picks the first
// best-ranked match for preference selection — it requires the best-ranked match
// to be unique, so an overlay remap fails closed on duplicate signatures instead
// of selecting an arbitrary track. An absent match and an ambiguous one are
// distinct outcomes: the caller may fall back only when nothing matched, because
// the fallback's raw-string language comparison can admit one of an exactly-tied
// set and hide the ambiguity.
func matchExactAudioTrack(tracks []models.AudioTrack, sig *userstore.AudioTrackSignature) (int, audioExactMatch) {
	if sig == nil || sig.IsZero() {
		return -1, audioExactAbsent
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
	switch count {
	case 0:
		return -1, audioExactAbsent
	case 1:
		return best, audioExactUnique
	default:
		return -1, audioExactAmbiguous
	}
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
