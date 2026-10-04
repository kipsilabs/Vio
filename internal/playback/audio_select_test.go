package playback_test

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/userstore"
)

func TestSelectAudioTrack(t *testing.T) {
	tracks := []models.AudioTrack{
		{Language: "en", Codec: "aac", Default: true, Channels: 2},
		{Language: "ja", Codec: "aac", Channels: 2},
		{Language: "ja", Codec: "flac", Channels: 6, Layout: "5.1"},
		{Language: "de", Codec: "aac", Channels: 2},
	}

	tests := []struct {
		name           string
		preferredLang  string
		seriesPrefIdx  int
		seriesPrefLang string
		hasSeriesPref  bool
		want           int
	}{
		{
			name:          "series pref index+lang match",
			preferredLang: "en",
			seriesPrefIdx: 2, seriesPrefLang: "ja", hasSeriesPref: true,
			want: 2,
		},
		{
			name:          "series pref index out of bounds falls back to lang",
			preferredLang: "en",
			seriesPrefIdx: 99, seriesPrefLang: "ja", hasSeriesPref: true,
			want: 1,
		},
		{
			name:          "series pref index wrong lang falls back to lang match",
			preferredLang: "en",
			seriesPrefIdx: 0, seriesPrefLang: "ja", hasSeriesPref: true,
			want: 1,
		},
		{
			name:          "no series pref uses profile language",
			preferredLang: "de",
			hasSeriesPref: false,
			want:          3,
		},
		{
			name:          "no matching language uses default track",
			preferredLang: "fr",
			hasSeriesPref: false,
			want:          0,
		},
		{
			name:          "empty preferred lang uses default track",
			preferredLang: "",
			hasSeriesPref: false,
			want:          0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seriesPref *playback.AudioTrackPreference
			if tt.hasSeriesPref {
				seriesPref = &playback.AudioTrackPreference{
					AudioTrackIndex: tt.seriesPrefIdx,
					AudioLanguage:   tt.seriesPrefLang,
				}
			}
			got := playback.SelectAudioTrack(tracks, tt.preferredLang, seriesPref)
			if got != tt.want {
				t.Errorf("SelectAudioTrack() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestSelectAudioTrack_NoTracks(t *testing.T) {
	got := playback.SelectAudioTrack(nil, "en", nil)
	if got != 0 {
		t.Errorf("SelectAudioTrack(nil) = %d, want 0", got)
	}
}

func TestSelectAudioTrack_PrefersExactRegionalTag(t *testing.T) {
	tracks := []models.AudioTrack{
		{Language: "en-GB"},
		{Language: "en"},
		{Language: "en-US"},
	}
	if got := playback.SelectAudioTrack(tracks, "en-US", nil); got != 2 {
		t.Fatalf("SelectAudioTrack(en-US) = %d, want exact en-US track 2", got)
	}
	if got := playback.SelectAudioTrack(tracks, "en-AU", nil); got != 1 {
		t.Fatalf("SelectAudioTrack(en-AU) = %d, want generic en track 1", got)
	}
}

func TestSelectAudioTrack_RegionalPrecedenceExactBareVariant(t *testing.T) {
	tracks := []models.AudioTrack{
		{Language: "en", Codec: "aac", Channels: 2},
		{Language: "fr-BE", Codec: "aac", Channels: 2},
		{Language: "fr", Codec: "aac", Channels: 2},
		{Language: "fr-CA", Codec: "aac", Channels: 2},
	}
	if got := playback.SelectAudioTrack(tracks, "fr-CA", nil); got != 3 {
		t.Fatalf("SelectAudioTrack(fr-CA) = %d, want exact track 3", got)
	}
	withoutExact := []models.AudioTrack{tracks[0], tracks[1], tracks[2]}
	if got := playback.SelectAudioTrack(withoutExact, "fr-CA", nil); got != 2 {
		t.Fatalf("SelectAudioTrack(fr-CA) without exact = %d, want bare track 2", got)
	}
	withoutBare := []models.AudioTrack{tracks[0], tracks[1]}
	if got := playback.SelectAudioTrack(withoutBare, "fr-CA", nil); got != 1 {
		t.Fatalf("SelectAudioTrack(fr-CA) variant-only = %d, want variant track 1", got)
	}
}

func TestSelectAudioTrack_ChineseScripts(t *testing.T) {
	tracks := []models.AudioTrack{
		{Language: "zh-Hant", Codec: "aac", Channels: 2},
		{Language: "zh-Hans", Codec: "aac", Channels: 2},
	}
	if got := playback.SelectAudioTrack(tracks, "zh-Hans", nil); got != 1 {
		t.Fatalf("SelectAudioTrack(zh-Hans) = %d, want simplified track 1", got)
	}
	if got := playback.SelectAudioTrack(tracks, "zh-Hant", nil); got != 0 {
		t.Fatalf("SelectAudioTrack(zh-Hant) = %d, want traditional track 0", got)
	}
}

func TestSelectAudioTrack_ScriptAndRegionalFidelity(t *testing.T) {
	// Matching script beats conflicting script even when conflicting script is index 0
	tracksWithScript := []models.AudioTrack{
		{Language: "zh-Hant", Codec: "aac", Channels: 2},
		{Language: "zh-Hans-CN", Codec: "aac", Channels: 2},
	}
	if got := playback.SelectAudioTrack(tracksWithScript, "zh-Hans", nil); got != 1 {
		t.Fatalf("zh-Hans preference on [zh-Hant, zh-Hans-CN] = %d, want 1", got)
	}

	// Matching script with extra subtag beats bare language tag
	tracksWithBare := []models.AudioTrack{
		{Language: "zh", Codec: "aac", Channels: 2},
		{Language: "zh-Hans-CN", Codec: "aac", Channels: 2},
	}
	if got := playback.SelectAudioTrack(tracksWithBare, "zh-Hans", nil); got != 1 {
		t.Fatalf("zh-Hans preference on [zh, zh-Hans-CN] = %d, want 1", got)
	}

	// Bare language tag beats conflicting script
	tracksBareVsConflict := []models.AudioTrack{
		{Language: "zh-Hant", Codec: "aac", Channels: 2},
		{Language: "zh", Codec: "aac", Channels: 2},
	}
	if got := playback.SelectAudioTrack(tracksBareVsConflict, "zh-Hans", nil); got != 1 {
		t.Fatalf("zh-Hans preference on [zh-Hant, zh] = %d, want bare track 1", got)
	}
}

func TestSelectAudioTrack_SavedIndexRejectsConflictingScript(t *testing.T) {
	tracks := []models.AudioTrack{
		{Language: "zh-Hant", Codec: "aac", Channels: 2},
		{Language: "zh-Hans", Codec: "aac", Channels: 2},
	}
	// Saved index points to index 0 (zh-Hant), but AudioLanguage is zh-Hans.
	// Since zh-Hant has a conflicting script with zh-Hans, it must NOT retain index 0,
	// and should instead fall through to select track 1 (zh-Hans).
	pref := &playback.AudioTrackPreference{
		AudioTrackIndex: 0,
		AudioLanguage:   "zh-Hans",
	}
	if got := playback.SelectAudioTrack(tracks, "", pref); got != 1 {
		t.Fatalf("saved index on conflicting script = %d, want language match 1", got)
	}

	// Unregistered variant on conflicting script (e.g. zh-Hant-foobar) must also
	// be rejected as a conflicting script, not accepted via parser error fallback.
	tracksUnregistered := []models.AudioTrack{
		{Language: "zh-Hant-foobar", Codec: "aac", Channels: 2},
		{Language: "zh-Hans", Codec: "aac", Channels: 2},
	}
	prefUnregistered := &playback.AudioTrackPreference{
		AudioTrackIndex: 0,
		AudioLanguage:   "zh-Hans",
	}
	if got := playback.SelectAudioTrack(tracksUnregistered, "", prefUnregistered); got != 1 {
		t.Fatalf("saved index on unregistered conflicting script = %d, want language match 1", got)
	}
}

func TestSelectAudioTrack_RegionDerivedScripts(t *testing.T) {
	// A bare language tag (zh) beats another regional variant (zh-HK) for zh-TW,
	// maintaining the standard exact > bare > variant precedence chain.
	tracks := []models.AudioTrack{
		{Language: "zh-HK", Codec: "aac", Channels: 2},
		{Language: "zh", Codec: "aac", Channels: 2},
	}
	if got := playback.SelectAudioTrack(tracks, "zh-TW", nil); got != 1 {
		t.Fatalf("zh-TW preference on [zh-HK, zh] = %d, want bare zh 1", got)
	}

	// Compatible regional variant (zh-HK, Hant) beats conflicting script (zh-CN, Hans).
	tracksCompatibleVsConflict := []models.AudioTrack{
		{Language: "zh-CN", Codec: "aac", Channels: 2},
		{Language: "zh-HK", Codec: "aac", Channels: 2},
	}
	if got := playback.SelectAudioTrack(tracksCompatibleVsConflict, "zh-TW", nil); got != 1 {
		t.Fatalf("zh-TW preference on [zh-CN, zh-HK] = %d, want compatible zh-HK 1", got)
	}

	// zh-CN implies Simplified (Hans) while zh-TW implies Traditional (Hant).
	// A zh-TW preference on [zh-CN, zh] picks bare zh over conflicting zh-CN.
	conflictTracks := []models.AudioTrack{
		{Language: "zh-CN", Codec: "aac", Channels: 2},
		{Language: "zh", Codec: "aac", Channels: 2},
	}
	if got := playback.SelectAudioTrack(conflictTracks, "zh-TW", nil); got != 1 {
		t.Fatalf("zh-TW preference on [zh-CN, zh] = %d, want bare zh 1", got)
	}
}

func TestSelectAudioTrack_MULTiLanguageList(t *testing.T) {
	multi := []models.AudioTrack{
		{Language: "en", Languages: []string{"en", "fr", "de"}, Codec: "eac3", Channels: 6, Default: true},
		{Language: "ja", Codec: "aac", Channels: 2},
	}
	if got := playback.SelectAudioTrack(multi, "fr", nil); got != 0 {
		t.Fatalf("profile fr = %d, want MULTi track 0", got)
	}
	single := []models.AudioTrack{
		{Language: "ja", Codec: "aac", Channels: 2, Default: true},
		{Language: "fr", Codec: "aac", Channels: 2},
	}
	if got := playback.SelectAudioTrack(single, "fr", nil); got != 1 {
		t.Fatalf("single-language fr = %d, want 1", got)
	}
	noDe := []models.AudioTrack{
		{Language: "en", Languages: []string{"en", "fr"}, Codec: "eac3", Channels: 6, Default: true},
		{Language: "ja", Codec: "aac", Channels: 2},
	}
	if got := playback.SelectAudioTrack(noDe, "de", nil); got != 0 {
		t.Fatalf("unmatched de = %d, want default 0", got)
	}
	seriesIndex := &playback.AudioTrackPreference{AudioTrackIndex: 1, AudioLanguage: "fr"}
	if got := playback.SelectAudioTrack(multi, "", seriesIndex); got != 0 {
		t.Fatalf("series index 1 fr = %d, want MULTi track 0", got)
	}
	seriesFallback := &playback.AudioTrackPreference{AudioTrackIndex: 99, AudioLanguage: "fr"}
	if got := playback.SelectAudioTrack(multi, "", seriesFallback); got != 0 {
		t.Fatalf("series language fallback fr = %d, want MULTi track 0", got)
	}
}

func TestSelectAudioTrack_MULTiPrimaryMul(t *testing.T) {
	tracks := []models.AudioTrack{
		{Language: "mul", Languages: []string{"en", "fr"}, Codec: "eac3", Channels: 6},
		{Language: "ja", Codec: "aac", Channels: 2, Default: true},
	}
	if got := playback.SelectAudioTrack(tracks, "fr", nil); got != 0 {
		t.Fatalf("profile fr on mul primary = %d, want MULTi track 0", got)
	}
	pref := &playback.AudioTrackPreference{AudioTrackIndex: 0, AudioLanguage: "en"}
	if got := playback.SelectAudioTrack(tracks, "", pref); got != 0 {
		t.Fatalf("saved mul index = %d, want 0", got)
	}
}

func TestSelectAudioTrack_MULTiRegionalRanking(t *testing.T) {
	tracks := []models.AudioTrack{
		{Language: "mul", Languages: []string{"fr-BE"}, Codec: "aac", Channels: 2},
		{Language: "fr", Codec: "aac", Channels: 2},
		{Language: "fr-CA", Codec: "aac", Channels: 2},
	}
	if got := playback.SelectAudioTrack(tracks, "fr-CA", nil); got != 2 {
		t.Fatalf("SelectAudioTrack(fr-CA) = %d, want exact track 2", got)
	}
}

// TestSelectAudioTrack_MULTiMembershipAcceptance pins G2: a track's MULTi
// `.Languages` list carries the language for both saved-index compatibility and
// ranking. Before the fix only `.Language` was consulted, so a saved index onto
// a MULTi track was discarded and any MULTi entry lost to a single-language
// track, regardless of rank.
func TestSelectAudioTrack_MULTiMembershipAcceptance(t *testing.T) {
	cases := []struct {
		name          string
		tracks        []models.AudioTrack
		preferredLang string
		seriesPref    *playback.AudioTrackPreference
		want          int
	}{
		{
			// The saved index lands on a MULTi track. It must be kept even
			// though the bare single-language track would win the language
			// ranking; without list membership the saved index is dropped.
			name: "saved index keeps MULTi track over ranked language",
			tracks: []models.AudioTrack{
				{Language: "en", Languages: []string{"en", "fr"}, Codec: "eac3", Channels: 6, Default: true},
				{Language: "fr", Codec: "aac", Channels: 2},
			},
			seriesPref: &playback.AudioTrackPreference{AudioTrackIndex: 0, AudioLanguage: "fr"},
			want:       0,
		},
		{
			// An exact regional entry in the MULTi list (rank 0) must beat a
			// bare primary code (rank 1).
			name: "exact list entry beats bare primary",
			tracks: []models.AudioTrack{
				{Language: "en", Languages: []string{"fr-CA"}, Codec: "eac3", Channels: 6},
				{Language: "fr", Codec: "aac", Channels: 2, Default: true},
			},
			preferredLang: "fr-CA",
			want:          0,
		},
		{
			// A bare entry in the MULTi list (rank 1) must beat a regional
			// primary variant (rank 2).
			name: "bare list entry beats regional primary variant",
			tracks: []models.AudioTrack{
				{Language: "mul", Languages: []string{"fr"}, Codec: "eac3", Channels: 6},
				{Language: "fr-BE", Codec: "aac", Channels: 2, Default: true},
			},
			preferredLang: "fr-FR",
			want:          0,
		},
		{
			// Equal variant rank across a MULTi list entry and a primary code is
			// broken by track order: the earlier track wins.
			name: "equal variant rank keeps first track",
			tracks: []models.AudioTrack{
				{Language: "mul", Languages: []string{"fr-BE"}, Codec: "eac3", Channels: 6},
				{Language: "fr-CA", Codec: "aac", Channels: 2, Default: true},
			},
			preferredLang: "fr-FR",
			want:          0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := playback.SelectAudioTrack(tc.tracks, tc.preferredLang, tc.seriesPref); got != tc.want {
				t.Fatalf("SelectAudioTrack() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestSelectAudioTrack_NoDefaultFallsToFirst(t *testing.T) {
	tracks := []models.AudioTrack{
		{Language: "ja", Codec: "aac"},
		{Language: "en", Codec: "aac"},
	}
	got := playback.SelectAudioTrack(tracks, "fr", nil)
	if got != 0 {
		t.Errorf("SelectAudioTrack() = %d, want 0", got)
	}
}

// TestSelectAudioTrack_ISO639CrossFormat tests that 2-letter profile
// preferences match 3-letter FFmpeg track codes and vice versa.
func TestSelectAudioTrack_ISO639CrossFormat(t *testing.T) {
	// Real-world FFmpeg track languages use 3-letter ISO 639-2 codes.
	tracks := []models.AudioTrack{
		{Language: "spa", Codec: "aac", Default: true, Channels: 2},
		{Language: "eng", Codec: "aac", Channels: 6, Layout: "5.1"},
		{Language: "jpn", Codec: "flac", Channels: 2},
	}

	tests := []struct {
		name          string
		preferredLang string
		want          int
	}{
		{"2-letter en matches 3-letter eng", "en", 1},
		{"2-letter es matches 3-letter spa", "es", 0},
		{"2-letter ja matches 3-letter jpn", "ja", 2},
		{"3-letter eng matches 3-letter eng", "eng", 1},
		{"unmatched falls to default", "fr", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := playback.SelectAudioTrack(tracks, tt.preferredLang, nil)
			if got != tt.want {
				t.Errorf("SelectAudioTrack() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestSelectAudioTrack_SeriesPrefCrossFormat(t *testing.T) {
	tracks := []models.AudioTrack{
		{Language: "spa", Codec: "aac", Default: true, Channels: 2},
		{Language: "eng", Codec: "aac", Channels: 6},
		{Language: "jpn", Codec: "flac", Channels: 2},
	}

	// Series pref stored with 3-letter code (from track), profile uses 2-letter.
	pref := &playback.AudioTrackPreference{
		AudioTrackIndex: 2,
		AudioLanguage:   "jpn",
	}
	got := playback.SelectAudioTrack(tracks, "en", pref)
	if got != 2 {
		t.Errorf("series pref jpn at index 2: got %d, want 2", got)
	}

	// Series pref language fallback with 2-letter stored code.
	pref2 := &playback.AudioTrackPreference{
		AudioTrackIndex: 99,   // out of bounds
		AudioLanguage:   "ja", // 2-letter
	}
	got = playback.SelectAudioTrack(tracks, "en", pref2)
	if got != 2 {
		t.Errorf("series pref ja fallback: got %d, want 2", got)
	}
}

func TestSelectAudioTrack_SeriesPrefIndexKeepsRegionalVariant(t *testing.T) {
	tracks := []models.AudioTrack{
		{Language: "en", Codec: "aac", Channels: 2, Title: "Commentary"},
		{Language: "en-US", Codec: "eac3", Channels: 6, Title: "English 5.1", Default: true},
	}

	// Preference saved before regional subtags were preserved: bare "en" for
	// the track at index 1. The saved index must still win over the bare
	// "en" commentary track at index 0.
	pref := &playback.AudioTrackPreference{
		AudioTrackIndex: 1,
		AudioLanguage:   "en",
	}
	if got := playback.SelectAudioTrack(tracks, "", pref); got != 1 {
		t.Fatalf("SelectAudioTrack() = %d, want saved index 1", got)
	}
}

func TestSelectAudioTrack_SignaturePrefersExactRegionalTag(t *testing.T) {
	tracks := []models.AudioTrack{
		{Language: "en-GB", Codec: "eac3", Channels: 6, Layout: "5.1", Title: "English 5.1"},
		{Language: "en-US", Codec: "eac3", Channels: 6, Layout: "5.1", Title: "English 5.1"},
	}
	sig := func(language string) *userstore.AudioTrackSignature {
		return &userstore.AudioTrackSignature{Language: language, Title: "English 5.1", Codec: "eac3", Layout: "5.1", Channels: 6}
	}

	// A regional signature must not settle for the earlier variant.
	pref := &playback.AudioTrackPreference{AudioTrackIndex: 0, AudioLanguage: "en-US", TrackSignature: sig("en-US")}
	if got := playback.SelectAudioTrack(tracks, "", pref); got != 1 {
		t.Fatalf("en-US signature: SelectAudioTrack() = %d, want 1", got)
	}

	// A legacy bare-language signature still matches a regional track.
	pref = &playback.AudioTrackPreference{AudioTrackIndex: 0, AudioLanguage: "en", TrackSignature: sig("en")}
	if got := playback.SelectAudioTrack(tracks, "", pref); got != 0 {
		t.Fatalf("bare en signature: SelectAudioTrack() = %d, want 0", got)
	}
}

func TestSelectAudioTrack_PrefersExactTrackSignatureOverIndexFallback(t *testing.T) {
	tracks := []models.AudioTrack{
		{Language: "eng", Codec: "aac", Channels: 2, Layout: "stereo", Title: "English Stereo", Default: true},
		{Language: "eng", Codec: "flac", Channels: 6, Layout: "5.1", Title: "English 5.1"},
	}

	pref := &playback.AudioTrackPreference{
		AudioTrackIndex: 0,
		AudioLanguage:   "en",
		TrackSignature: &userstore.AudioTrackSignature{
			Language: "eng",
			Title:    "English 5.1",
			Codec:    "flac",
			Layout:   "5.1",
			Channels: 6,
			Default:  false,
		},
	}

	got := playback.SelectAudioTrack(tracks, "en", pref)
	if got != 1 {
		t.Fatalf("SelectAudioTrack() = %d, want 1", got)
	}
}

func TestMatchAudioTrackAcrossVersionsRemapsReorderedLanguage(t *testing.T) {
	requested := []models.AudioTrack{
		{Language: "ja", Codec: "aac", Channels: 2, Title: "Japanese"},
		{Language: "en", Codec: "eac3", Channels: 6, Title: "English 5.1"},
	}
	effective := []models.AudioTrack{
		{Language: "en", Codec: "eac3", Channels: 6, Title: "English 5.1"},
		{Language: "ja", Codec: "aac", Channels: 2, Title: "Japanese"},
	}

	if got := playback.MatchAudioTrackAcrossVersions(requested, effective, 1); got != 0 {
		t.Fatalf("MatchAudioTrackAcrossVersions() = %d, want English track 0", got)
	}
}

func TestMatchAudioTrackAcrossVersionsFallsBackToLanguageAcrossCodecs(t *testing.T) {
	requested := []models.AudioTrack{
		{Language: "ja", Codec: "aac", Channels: 2},
		{Language: "en", Codec: "truehd", Channels: 8},
	}
	effective := []models.AudioTrack{
		{Language: "en", Codec: "eac3", Channels: 6},
		{Language: "es", Codec: "aac", Channels: 2, Default: true},
	}

	if got := playback.MatchAudioTrackAcrossVersions(requested, effective, 1); got != 0 {
		t.Fatalf("MatchAudioTrackAcrossVersions() = %d, want English track 0", got)
	}
}

func TestMatchAudioTrackAcrossVersions_MULTiCrossFile(t *testing.T) {
	requested := []models.AudioTrack{
		{Language: "es", Codec: "ac3", Channels: 2},
		{Language: "ja", Codec: "aac", Channels: 2},
		{Language: "en", Languages: []string{"en", "fr", "de"}, Codec: "eac3", Channels: 6, Title: "MULTi"},
	}
	target := []models.AudioTrack{
		{Language: "es", Codec: "ac3", Channels: 2},
		{Language: "en", Languages: []string{"fr", "de", "en"}, Codec: "eac3", Channels: 6, Title: "MULTi"},
		{Language: "ja", Codec: "aac", Channels: 2, Default: true},
	}
	if got := playback.MatchAudioTrackAcrossVersions(requested, target, 2); got != 1 {
		t.Fatalf("MULTi remap = %d, want target MULTi track 1", got)
	}
	if got := playback.MatchAudioTrackAcrossVersions(requested, target, 0); got != 0 {
		t.Fatalf("es remap = %d, want 0", got)
	}
	if got := playback.MatchAudioTrackAcrossVersions(requested, target, 1); got != 2 {
		t.Fatalf("ja remap = %d, want target track 2", got)
	}
	absent := []models.AudioTrack{
		{Language: "es", Codec: "ac3", Channels: 2},
		{Language: "it", Codec: "ac3", Channels: 2},
		{Language: "en", Languages: []string{"en", "fr", "de"}, Codec: "eac3", Channels: 6},
	}
	if got := playback.MatchAudioTrackAcrossVersions(absent, target, 1); got != 2 {
		t.Fatalf("absent-track remap = %d, want target default 2", got)
	}
}

func TestMatchAudioTrackAcrossVersionsTriesEveryLanguage(t *testing.T) {
	requested := []models.AudioTrack{
		{Language: "es", Codec: "ac3", Channels: 2, Default: true},
		{Language: "mul", Languages: []string{"en", "fr"}, Codec: "eac3", Channels: 6, Title: "MULTi"},
	}
	effective := []models.AudioTrack{
		{Language: "es", Codec: "ac3", Channels: 2, Default: true},
		{Language: "fr", Codec: "eac3", Channels: 6, Title: "French"},
	}
	if got := playback.MatchAudioTrackAcrossVersions(requested, effective, 1); got != 1 {
		t.Fatalf("second-language MULTi remap = %d, want target French track 1", got)
	}
}

func TestMatchAudioTrackAcrossVersionsTriesLanguageListAfterPrimary(t *testing.T) {
	requested := []models.AudioTrack{
		{Language: "en", Languages: []string{"en", "de"}, Codec: "eac3", Channels: 6, Title: "MULTi"},
	}
	effective := []models.AudioTrack{
		{Language: "es", Codec: "ac3", Channels: 2, Default: true},
		{Language: "de", Codec: "eac3", Channels: 6},
	}
	if got := playback.MatchAudioTrackAcrossVersions(requested, effective, 0); got != 1 {
		t.Fatalf("language-list fallback = %d, want target German track 1", got)
	}
}

func TestMatchAudioTrackAcrossVersionsFallsBackToDefaultWhenNoLanguageMatches(t *testing.T) {
	requested := []models.AudioTrack{
		{Language: "mul", Languages: []string{"en", "fr"}, Codec: "eac3", Channels: 6},
	}
	effective := []models.AudioTrack{
		{Language: "es", Codec: "ac3", Channels: 2, Default: true},
		{Language: "ja", Codec: "aac", Channels: 2},
	}
	if got := playback.MatchAudioTrackAcrossVersions(requested, effective, 0); got != 0 {
		t.Fatalf("no-language-match remap = %d, want target default 0", got)
	}
}

func TestMatchAudioTrackAcrossVersionsSkipsPlaceholderListEntries(t *testing.T) {
	requested := []models.AudioTrack{
		{Language: "mul", Languages: []string{"mul", "und", "fr"}, Codec: "eac3", Channels: 6, Title: "MULTi"},
	}
	effective := []models.AudioTrack{
		{Language: "mul", Languages: []string{"mul"}, Codec: "eac3", Channels: 6, Title: "Other"},
		{Language: "fr", Codec: "eac3", Channels: 6, Title: "French", Default: true},
	}
	if got := playback.MatchAudioTrackAcrossVersions(requested, effective, 0); got != 1 {
		t.Fatalf("placeholder-skip remap = %d, want target French track 1", got)
	}
}

func TestIsOriginalLanguagePreference(t *testing.T) {
	for value, want := range map[string]bool{
		"original": true, "Original": true, "x-silo-original": true, " X-Silo-Original ": true,
		"": false, "en": false, "ja": false, "x-silo-other": false,
	} {
		if got := playback.IsOriginalLanguagePreference(value); got != want {
			t.Errorf("IsOriginalLanguagePreference(%q) = %v, want %v", value, got, want)
		}
	}
}
