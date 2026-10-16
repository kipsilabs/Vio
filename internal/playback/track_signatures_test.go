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

// TestClientAudioSignatureMatchesTrackLanguage pins the language member against
// MULTi lists and regional/canonical spellings. A signature language the client
// actually named must be found as the track's primary Language or as any MULTi
// member, compared with the strict matcher's canonical/regional rank logic, while
// a genuinely different language or a conflicting script still refutes.
func TestClientAudioSignatureMatchesTrackLanguage(t *testing.T) {
	cases := []struct {
		name  string
		track models.AudioTrack
		sig   *userstore.AudioTrackSignature
		want  bool
	}{
		{
			name:  "multilist carries the signature language",
			track: models.AudioTrack{Language: "mul", Languages: []string{"de", "en"}, Codec: "aac", Channels: 2},
			sig:   &userstore.AudioTrackSignature{Language: "en", Codec: "aac", Channels: 2},
			want:  true,
		},
		{
			name:  "regional track carries a bare signature language",
			track: models.AudioTrack{Language: "en-US", Codec: "aac", Channels: 2},
			sig:   &userstore.AudioTrackSignature{Language: "en", Codec: "aac", Channels: 2},
			want:  true,
		},
		{
			name:  "bare track carries a regional signature language",
			track: models.AudioTrack{Language: "en", Codec: "aac", Channels: 2},
			sig:   &userstore.AudioTrackSignature{Language: "en-US", Codec: "aac", Channels: 2},
			want:  true,
		},
		{
			name:  "canonical ISO alias matches",
			track: models.AudioTrack{Language: "eng", Codec: "aac", Channels: 2},
			sig:   &userstore.AudioTrackSignature{Language: "en", Codec: "aac", Channels: 2},
			want:  true,
		},
		{
			name:  "case and surrounding space variants match",
			track: models.AudioTrack{Language: "en", Codec: "aac", Channels: 2},
			sig:   &userstore.AudioTrackSignature{Language: "  EN  ", Codec: "aac", Channels: 2},
			want:  true,
		},
		{
			name:  "a different language still refutes",
			track: models.AudioTrack{Language: "en", Codec: "aac", Channels: 2},
			sig:   &userstore.AudioTrackSignature{Language: "de", Codec: "aac", Channels: 2},
			want:  false,
		},
		{
			name:  "a MULTi list without the signature language still refutes",
			track: models.AudioTrack{Language: "mul", Languages: []string{"de", "fr"}, Codec: "aac", Channels: 2},
			sig:   &userstore.AudioTrackSignature{Language: "en", Codec: "aac", Channels: 2},
			want:  false,
		},
		{
			name:  "a conflicting script still refutes",
			track: models.AudioTrack{Language: "zh-Hant", Codec: "aac", Channels: 2},
			sig:   &userstore.AudioTrackSignature{Language: "zh-Hans", Codec: "aac", Channels: 2},
			want:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClientAudioSignatureMatchesTrack(tc.track, tc.sig); got != tc.want {
				t.Fatalf("ClientAudioSignatureMatchesTrack(%+v, %+v) = %v, want %v", tc.track, tc.sig, got, tc.want)
			}
		})
	}
}

// TestClientAudioSignatureMatchesTrackRegionalRuleMatrix pins the ONE regional
// rule the predicate applies to both language fields. The primary Language
// member and every MULTi Languages member must be carried under the same
// region/script discipline, so a constraint gets the same verdict whichever
// field expressed it: a stated explicit region is refused by a different
// explicit region, a bare stated language stays permissive, a matching region
// and a bare track tag accept, and a script conflict refuses. A placeholder
// track (nothing comparable) and a stated sentinel that carries no concrete
// language are neutral.
func TestClientAudioSignatureMatchesTrackRegionalRuleMatrix(t *testing.T) {
	track := func(language string, languages ...string) models.AudioTrack {
		return models.AudioTrack{Language: language, Languages: languages, Codec: "aac", Channels: 2}
	}
	cases := []struct {
		name  string
		track models.AudioTrack
		sig   *userstore.AudioTrackSignature
		want  bool
	}{
		// Primary Language member.
		{
			name:  "primary: stated en-US refuses an en-GB-only track",
			track: track("en-GB"),
			sig:   &userstore.AudioTrackSignature{Language: "en-US", Codec: "aac", Channels: 2},
			want:  false,
		},
		{
			name:  "primary: bare stated en accepts an en-GB track",
			track: track("en-GB"),
			sig:   &userstore.AudioTrackSignature{Language: "en", Codec: "aac", Channels: 2},
			want:  true,
		},
		{
			name:  "primary: matching region accepts",
			track: track("en-US"),
			sig:   &userstore.AudioTrackSignature{Language: "en-US", Codec: "aac", Channels: 2},
			want:  true,
		},
		{
			name:  "primary: stated region is carried by a bare track tag",
			track: track("en"),
			sig:   &userstore.AudioTrackSignature{Language: "en-US", Codec: "aac", Channels: 2},
			want:  true,
		},
		{
			name:  "primary: script conflict refuses",
			track: track("zh-Hant"),
			sig:   &userstore.AudioTrackSignature{Language: "zh-Hans", Codec: "aac", Channels: 2},
			want:  false,
		},
		// MULTi Languages member: the same rule, expressed in the other field.
		{
			name:  "multi: stated en-US refuses an en-GB-only track",
			track: track("mul", "en-GB"),
			sig:   &userstore.AudioTrackSignature{Codec: "aac", Channels: 2, Languages: []string{"en-US"}},
			want:  false,
		},
		{
			name:  "multi: bare stated en accepts an en-GB track",
			track: track("mul", "en-GB"),
			sig:   &userstore.AudioTrackSignature{Codec: "aac", Channels: 2, Languages: []string{"en"}},
			want:  true,
		},
		{
			name:  "multi: matching region accepts",
			track: track("mul", "en-US"),
			sig:   &userstore.AudioTrackSignature{Codec: "aac", Channels: 2, Languages: []string{"en-US"}},
			want:  true,
		},
		{
			name:  "multi: stated region is carried by a bare track tag",
			track: track("mul", "en"),
			sig:   &userstore.AudioTrackSignature{Codec: "aac", Channels: 2, Languages: []string{"en-US"}},
			want:  true,
		},
		{
			name:  "multi: script conflict refuses",
			track: track("mul", "zh-Hant"),
			sig:   &userstore.AudioTrackSignature{Codec: "aac", Channels: 2, Languages: []string{"zh-Hans"}},
			want:  false,
		},
		// Neutral cases.
		{
			name:  "placeholder track has nothing to compare",
			track: models.AudioTrack{Index: -1, Default: true},
			sig:   &userstore.AudioTrackSignature{Language: "en-US", Codec: "aac", Channels: 2, Languages: []string{"en-US"}},
			want:  true,
		},
		{
			name:  "sentinel stated language is neutral on the primary field",
			track: track("en"),
			sig:   &userstore.AudioTrackSignature{Language: "mul", Codec: "aac", Channels: 2},
			want:  true,
		},
		{
			name:  "sentinel stated language is neutral on the MULTi field",
			track: track("en"),
			sig:   &userstore.AudioTrackSignature{Codec: "aac", Channels: 2, Languages: []string{"und"}},
			want:  true,
		},
		// The original regression: a partial snapshot is not refuted by a
		// richer probed track that agrees on every member it named.
		{
			name:  "partial signature survives a richer track",
			track: models.AudioTrack{Index: 0, Language: "ja", Codec: "aac", Channels: 2, Layout: "stereo", Title: "[Erai-raws]_AAC_CR", EmbeddedTitle: "[Erai-raws]_AAC_CR", Default: true},
			sig:   &userstore.AudioTrackSignature{Codec: "aac", Channels: 2},
			want:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClientAudioSignatureMatchesTrack(tc.track, tc.sig); got != tc.want {
				t.Fatalf("ClientAudioSignatureMatchesTrack(%+v, %+v) = %v, want %v", tc.track, tc.sig, got, tc.want)
			}
		})
	}
}

// TestClientAudioSignatureMatchesTrackStrictMatchersUntouched pins that the
// non-refutation predicate did not relax probe-derived re-identification: a
// remap whose only candidates tie because "en" and "eng" are canonically equal
// still fails closed.
func TestClientAudioSignatureMatchesTrackStrictMatchersUntouched(t *testing.T) {
	plan := []models.AudioTrack{{Index: 0, Language: "en", Codec: "aac", Channels: 2}}
	verified := []models.AudioTrack{
		{Index: 0, Language: "en", Codec: "aac", Channels: 2},
		{Index: 0, Language: "eng", Codec: "aac", Channels: 2},
	}
	if got := remapAudioSelectionIndex(plan, 0, verified); got != -1 {
		t.Fatalf("remapped position = %d, want -1: probe-derived re-identification must stay fail-closed", got)
	}
}

// TestClientAudioSignatureMatchesTrackLanguagesMembership pins the Languages
// member as a per-member membership test rather than an exact multiset: a
// snapshot that enumerated only some of a probed track's MULTi languages is not
// refuted by the fuller demux, while a stated member the track does not carry
// (including a different explicit region) still refutes.
func TestClientAudioSignatureMatchesTrackLanguagesMembership(t *testing.T) {
	cases := []struct {
		name  string
		track models.AudioTrack
		sig   *userstore.AudioTrackSignature
		want  bool
	}{
		{
			name:  "a partial stated list is carried by the fuller probed list",
			track: models.AudioTrack{Language: "mul", Languages: []string{"en", "de"}, Codec: "aac", Channels: 2},
			sig:   &userstore.AudioTrackSignature{Codec: "aac", Channels: 2, Languages: []string{"en"}},
			want:  true,
		},
		{
			name:  "every stated member of a matching list is carried",
			track: models.AudioTrack{Language: "mul", Languages: []string{"de", "en"}, Codec: "aac", Channels: 2},
			sig:   &userstore.AudioTrackSignature{Codec: "aac", Channels: 2, Languages: []string{"en", "de"}},
			want:  true,
		},
		{
			name:  "a stated member absent from the track refutes",
			track: models.AudioTrack{Language: "mul", Languages: []string{"en", "de"}, Codec: "aac", Channels: 2},
			sig:   &userstore.AudioTrackSignature{Codec: "aac", Channels: 2, Languages: []string{"fr"}},
			want:  false,
		},
		{
			name:  "a stated member partially absent refutes",
			track: models.AudioTrack{Language: "mul", Languages: []string{"en", "de"}, Codec: "aac", Channels: 2},
			sig:   &userstore.AudioTrackSignature{Codec: "aac", Channels: 2, Languages: []string{"en", "fr"}},
			want:  false,
		},
		{
			name:  "a bare stated member is carried by a regional variant",
			track: models.AudioTrack{Language: "mul", Languages: []string{"en-US"}, Codec: "aac", Channels: 2},
			sig:   &userstore.AudioTrackSignature{Codec: "aac", Channels: 2, Languages: []string{"en"}},
			want:  true,
		},
		{
			name:  "a stated explicit region is carried by a bare track tag",
			track: models.AudioTrack{Language: "mul", Languages: []string{"en"}, Codec: "aac", Channels: 2},
			sig:   &userstore.AudioTrackSignature{Codec: "aac", Channels: 2, Languages: []string{"en-US"}},
			want:  true,
		},
		{
			name:  "a stated explicit region is not carried by a different region",
			track: models.AudioTrack{Language: "en-GB", Languages: []string{"en-GB"}, Codec: "aac", Channels: 2},
			sig:   &userstore.AudioTrackSignature{Codec: "aac", Channels: 2, Languages: []string{"en-US"}},
			want:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClientAudioSignatureMatchesTrack(tc.track, tc.sig); got != tc.want {
				t.Fatalf("ClientAudioSignatureMatchesTrack(%+v, %+v) = %v, want %v", tc.track, tc.sig, got, tc.want)
			}
		})
	}
}

// TestClientAudioSignatureMatchesTrackDefaultOnlyIsOverLenient pins the
// documented over-leniency: the signature's Default flag is never compared, so a
// signature that carries Default and no other member constrains nothing and
// matches every track. That is acceptable only because this predicate answers a
// presence question and is never a selector, so an under-constrained positive
// cannot move a transport onto the wrong stream.
func TestClientAudioSignatureMatchesTrackDefaultOnlyIsOverLenient(t *testing.T) {
	sig := &userstore.AudioTrackSignature{Default: true}
	for _, track := range []models.AudioTrack{
		{Index: 0, Language: "en", Codec: "aac", Channels: 2},
		{Index: 1, Language: "de", Codec: "eac3", Channels: 6, Layout: "5.1", Default: true},
		{Index: -1, Default: true},
	} {
		if !ClientAudioSignatureMatchesTrack(track, sig) {
			t.Fatalf("a Default-only signature must not refute track %+v", track)
		}
	}
}
