package playback

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/userstore"
)

// TestRemapAudioSelectionIndexReidentifiesCommittedTrack pins the overlay-swap
// remap: the selected track's array position may move when the verified list
// reorders, so the remap must return the position that names the same stream.
func TestRemapAudioSelectionIndexReidentifiesCommittedTrack(t *testing.T) {
	plan := []models.AudioTrack{
		{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
		{Index: 3, Language: "pt", Codec: "eac3", Channels: 6, Layout: "5.1"},
	}
	// pt's absolute container index is stable across the reorder, so it is
	// re-identified at its new array position.
	verified := []models.AudioTrack{
		{Index: 3, Language: "pt", Codec: "eac3", Channels: 6, Layout: "5.1"},
		{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
	}
	if got := remapAudioSelectionIndex(plan, 1, verified); got != 0 {
		t.Fatalf("remapped position = %d, want 0 (the reordered pt track)", got)
	}
}

// TestRemapAudioSelectionIndexMissingProbedIndexFailsClosed pins that a
// previously-trustworthy absolute index is the strongest identity: when the
// selected stream's probed index is absent, the remap fails closed even if a
// different-index candidate carries identical transport facts. A renumbered
// stream is a different stream, and no weaker match may stand in.
func TestRemapAudioSelectionIndexMissingProbedIndexFailsClosed(t *testing.T) {
	plan := []models.AudioTrack{
		{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
		{Index: 3, Language: "pt", Codec: "eac3", Channels: 6, Layout: "5.1"},
	}
	// The pt stream is present by facts but its container index moved 3 -> 4.
	verified := []models.AudioTrack{
		{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
		{Index: 4, Language: "pt", Codec: "eac3", Channels: 6, Layout: "5.1"},
	}
	if got := remapAudioSelectionIndex(plan, 1, verified); got != -1 {
		t.Fatalf("remapped position = %d, want -1; a missing probed index must not fall through to another index", got)
	}
}

// TestRemapAudioSelectionIndexRemovalReturnsInvalid pins the removed case: a
// committed stream that is absent from the verified inventory has no position to
// return, and the caller must see -1 rather than the old position naming another
// track.
func TestRemapAudioSelectionIndexRemovalReturnsInvalid(t *testing.T) {
	plan := []models.AudioTrack{
		{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
		{Index: 3, Language: "pt", Codec: "eac3", Channels: 6, Layout: "5.1"},
	}
	verified := []models.AudioTrack{
		{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
	}
	if got := remapAudioSelectionIndex(plan, 1, verified); got != -1 {
		t.Fatalf("remapped position = %d, want -1 for a removed committed track", got)
	}
}

// TestRemapAudioSelectionIndexDuplicateSignatureFailsClosed pins the
// exact-signature ambiguity guard: two verified tracks that match the selected
// signature equally well make the identity ambiguous, and the remap must fail
// closed instead of picking one arbitrarily. This is the path that bypassed the
// unique-only fallback.
func TestRemapAudioSelectionIndexDuplicateSignatureFailsClosed(t *testing.T) {
	// A synthesized plan-time list: every Index is 0, so only the signature can
	// identify the stream.
	plan := []models.AudioTrack{{Index: 0, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"}}
	verified := []models.AudioTrack{
		{Index: 0, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
		{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
	}
	if got := remapAudioSelectionIndex(plan, 0, verified); got != -1 {
		t.Fatalf("remapped position = %d, want -1 for a duplicate signature", got)
	}
}

// TestRemapAudioSelectionIndexAmbiguousExactMatchFailsClosed pins the ambiguity
// guard against canonicalized language tags. Two verified tracks whose language
// tags ("en" and "eng") canonicalize to the same language match the synthesized
// selection equally well, so the exact lookup is ambiguous. The conservative
// fallback compares raw strings and would admit exactly one of them (the raw
// "en"), silently hiding the ambiguity and moving the transport onto a stream
// the exact matcher refused to choose. An ambiguous exact match must therefore
// return -1 without consulting the fallback.
func TestRemapAudioSelectionIndexAmbiguousExactMatchFailsClosed(t *testing.T) {
	plan := []models.AudioTrack{{Index: 0, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"}}
	verified := []models.AudioTrack{
		{Index: 0, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
		{Index: 0, Language: "eng", Codec: "aac", Channels: 2, Layout: "stereo"},
	}
	if got := remapAudioSelectionIndex(plan, 0, verified); got != -1 {
		t.Fatalf("remapped position = %d, want -1: en/eng are canonically equal, so the exact match is ambiguous and must not fall through to the raw-string fallback", got)
	}
}

// TestRemapAudioSelectionIndexUniqueSignatureMatches pins the positive
// exact-signature path: one verified track carries the selected track's full
// signature (including its known title), so it is re-identified.
func TestRemapAudioSelectionIndexUniqueSignatureMatches(t *testing.T) {
	plan := []models.AudioTrack{
		{Index: 0, Language: "en", Codec: "eac3", Channels: 6, Layout: "5.1", Title: "Director"},
		{Index: 0, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo", Title: "Commentary"},
	}
	verified := []models.AudioTrack{
		{Index: 2, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo", Title: "Commentary"},
		{Index: 4, Language: "en", Codec: "eac3", Channels: 6, Layout: "5.1", Title: "Director"},
	}
	if got := remapAudioSelectionIndex(plan, 0, verified); got != 1 {
		t.Fatalf("remapped position = %d, want 1 (the Director track)", got)
	}
}

// TestRemapAudioSelectionIndexFallbackRejectsConflictingIdentity pins the
// conservative fallback: when the exact signature misses, a candidate may only
// stand in if it agrees with every known identity fact. A conflicting title,
// embedded title, or MULTi language set is a different stream and is rejected,
// even when it is the only transport-fact match.
func TestRemapAudioSelectionIndexFallbackRejectsConflictingIdentity(t *testing.T) {
	base := models.AudioTrack{Index: 0, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"}

	cases := []struct {
		name      string
		selected  models.AudioTrack
		verified  []models.AudioTrack
		wantIndex int
	}{
		{
			name:      "conflicting known title is rejected",
			selected:  withTitle(base, "Director"),
			verified:  []models.AudioTrack{withTitle(base, "Main")},
			wantIndex: -1,
		},
		{
			name:      "conflicting embedded title is rejected",
			selected:  withEmbeddedTitle(base, "Director"),
			verified:  []models.AudioTrack{withEmbeddedTitle(base, "Main")},
			wantIndex: -1,
		},
		{
			name:      "conflicting language set is rejected",
			selected:  withLanguages(base, "en", "fr"),
			verified:  []models.AudioTrack{withLanguages(base, "en", "de")},
			wantIndex: -1,
		},
		{
			name:      "unknown selected title is not a conflict",
			selected:  base,
			verified:  []models.AudioTrack{withTitle(base, "Main")},
			wantIndex: 0,
		},
		{
			name:      "ambiguous non-conflicting set is invalid",
			selected:  base,
			verified:  []models.AudioTrack{withTitle(base, "Main"), withTitle(base, "Descriptive")},
			wantIndex: -1,
		},
		{
			name:      "different layout is not a substitute",
			selected:  models.AudioTrack{Index: 0, Language: "en", Codec: "aac", Channels: 6, Layout: "5.1", Title: "Director"},
			verified:  []models.AudioTrack{base},
			wantIndex: -1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := remapAudioSelectionIndex([]models.AudioTrack{tc.selected}, 0, tc.verified); got != tc.wantIndex {
				t.Fatalf("remapped position = %d, want %d (selected %+v, verified %+v)", got, tc.wantIndex, tc.selected, tc.verified)
			}
		})
	}
}

func withTitle(track models.AudioTrack, title string) models.AudioTrack {
	track.Title = title
	return track
}

func withEmbeddedTitle(track models.AudioTrack, title string) models.AudioTrack {
	track.EmbeddedTitle = title
	return track
}

func withLanguages(track models.AudioTrack, languages ...string) models.AudioTrack {
	track.Languages = languages
	return track
}

// TestClientAudioSignatureMatchesTrackWildcards pins the presence-check
// semantics: a client/start signature constrains only the members it actually
// carried, so a partial snapshot matches a richer probed track instead of being
// falsely reported gone. The strict re-identification matchers are separate and
// keep failing closed (see the remap tests above).
func TestClientAudioSignatureMatchesTrackWildcards(t *testing.T) {
	rich := models.AudioTrack{
		Index: 0, Language: "ja", Codec: "aac", Channels: 2, Layout: "stereo",
		Title: "[Erai-raws]_AAC_CR", EmbeddedTitle: "[Erai-raws]_AAC_CR", Default: true,
	}
	partial := &userstore.AudioTrackSignature{Codec: "aac", Channels: 2}
	if !ClientAudioSignatureMatchesTrack(rich, partial) {
		t.Fatal("a codec+channels signature must match a richer track that agrees on both")
	}
	// Every named member still has to agree.
	for name, sig := range map[string]*userstore.AudioTrackSignature{
		"wrong codec":      {Codec: "eac3", Channels: 2},
		"wrong channels":   {Codec: "aac", Channels: 6},
		"wrong language":   {Language: "en", Codec: "aac", Channels: 2},
		"wrong layout":     {Codec: "aac", Channels: 2, Layout: "5.1"},
		"wrong title":      {Codec: "aac", Channels: 2, Title: "commentary"},
		"wrong multi list": {Codec: "aac", Channels: 2, Languages: []string{"en"}},
	} {
		if ClientAudioSignatureMatchesTrack(rich, sig) {
			t.Fatalf("%s must not match the probed track", name)
		}
	}
	// A fully populated signature reduces to strict member equality.
	if !ClientAudioSignatureMatchesTrack(rich, AudioTrackSignatureFromTrack(rich)) {
		t.Fatal("the track's own full signature must match it")
	}
	// A placeholder track (index/default only) has nothing comparable and must
	// not refute the selection.
	placeholder := models.AudioTrack{Index: -1, Default: true}
	if !ClientAudioSignatureMatchesTrack(placeholder, partial) {
		t.Fatal("a placeholder track must never refute a selection")
	}
	if !ClientAudioSignatureMatchesTrack(rich, nil) || !ClientAudioSignatureMatchesTrack(rich, &userstore.AudioTrackSignature{}) {
		t.Fatal("a nil or zero signature carries no constraint")
	}
}
