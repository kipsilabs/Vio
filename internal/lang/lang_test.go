package lang

import (
	"slices"
	"testing"
)

func TestCanonicalTag(t *testing.T) {
	cases := map[string]string{
		"": "", "  ": "", "en": "en", "EN": "en", "eng": "en", "ara": "ar", "AR": "ar",
		"Arabic": "", "en_US": "en-US", "pt-BR": "pt-BR", "pt_br": "pt-BR",
		"zh-Hant": "zh-Hant", "ZH-hant-TW": "zh-Hant-TW", "iw": "he", "not a language": "",
		"Klingon": "", "x-private": "x-private",
		"qaa": "qaa", "en-abcde-abcde": "", "en-a-foo-a-bar": "",
		"x-abcde-abcde": "x-abcde-abcde", "en-x-abcde-abcde": "en-x-abcde-abcde",
		"en-a-abcde-abcde": "en-a-abcde-abcde",
		// Grandfathered forms resolve to their registered replacements.
		"i-klingon": "tlh", "en-GB-oed": "en-GB-oxendict", "sgn-BE-FR": "sfb",
		"i-navajo": "nv", "i-notreal": "", "Klingon (TNG)": "",
	}
	for in, want := range cases {
		if got := CanonicalTag(in); got != want {
			t.Errorf("CanonicalTag(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPrimaryLanguage(t *testing.T) {
	for in, want := range map[string]string{"pt-BR": "pt", "zh-Hant": "zh", "eng": "en", "Arabic": "ar", "": "", "unknown": ""} {
		if got := PrimaryLanguage(in); got != want {
			t.Errorf("PrimaryLanguage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestISO6392(t *testing.T) {
	for in, want := range map[string]string{"en": "eng", "eng": "eng", "pt-BR": "por", "zh-Hant": "zho", "fr": "fra", "fil": "fil", "": "", "und": "", "x-private": "", "unknown": ""} {
		if got := ISO6392(in); got != want {
			t.Errorf("ISO6392(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCanonical(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"   ", ""},
		{"en", "en"},
		{"EN", "en"},
		{" en ", "en"},
		{"eng", "en"},
		{"ENG", "en"},
		{"jpn", "ja"},
		{"ja", "ja"},
		{"fra", "fr"},
		{"fre", "fr"},
		{"fr", "fr"},
		{"deu", "de"},
		{"ger", "de"},
		{"zho", "zh"},
		{"chi", "zh"},
		{"nor", "no"},
		{"nob", "nb"},
		{"nb", "nb"},
		{"nn", "nn"},
		{"fr-CA", "fr"},
		{"en-US", "en"},
		{"pt-BR", "pt"},
		// Languages without a 2-letter form keep their 3-letter code.
		{"fil", "fil"},
		// Unrecognized inputs preserved as lowercase+trimmed.
		{"english", "english"},
		{"klingon", "klingon"},
		{"  Ja  ", "ja"},
		// "und"/"mul" are preserved verbatim, never remapped to English.
		{"und", "und"},
		{"mul", "mul"},
		{"UND", "und"},
		{"undetermined", "und"},
		{"multiple", "mul"},
	}
	for _, tc := range cases {
		got := Canonical(tc.in)
		if got != tc.want {
			t.Errorf("Canonical(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseLanguages(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"English / French / Spanish", []string{"en", "fr", "es"}},
		{"Eng/Fra/Deu", []string{"en", "fr", "de"}},
		{"AC3 5.1 English+French+Spanish", []string{"en", "fr", "es"}},
		{"English", []string{"en"}},
		{"SyncUP", nil},
		{"Audio Commentary", nil},
		{"DTS 5.1", nil},
		{"", nil},
		{"   ", nil},
		// Uppercase 2-letter codes are unambiguous.
		{"EN/FR", []string{"en", "fr"}},
		// Lowercase 2-letter tokens are rejected to avoid English-word
		// false positives ("it", "no", "hi").
		{"it/no", nil},
		// Junk alone yields nil even when it looks code-like.
		{"7.1", nil},
		{"German DTS-HD 5.1", []string{"de"}},
	}
	for _, tc := range cases {
		got := ParseLanguages(tc.in)
		if (got == nil) != (tc.want == nil) {
			t.Errorf("ParseLanguages(%q) nil mismatch: got %v, want %v", tc.in, got, tc.want)
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("ParseLanguages(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Errorf("ParseLanguages(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

func TestCanonicalCountry(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"  ", ""},
		{"US", "US"},
		{"us", "US"},
		{" us ", "US"},
		{"GB", "GB"},
		{"JP", "JP"},
		// Three-letter codes get canonicalized to alpha-2.
		{"USA", "US"},
		{"GBR", "GB"},
		{"JPN", "JP"},
		// Unrecognized stays uppercase+trimmed.
		{"United States", "UNITED STATES"},
		{"ZZ", "ZZ"},
	}
	for _, tc := range cases {
		got := CanonicalCountry(tc.in)
		if got != tc.want {
			t.Errorf("CanonicalCountry(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCanonicalCountries(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
	}{
		{nil, nil},
		{[]string{}, []string{}},
		{[]string{"us", "GBR", "", "  jp  ", "ZZ"}, []string{"US", "GB", "JP", "ZZ"}},
		{[]string{"  ", ""}, []string{}},
	}
	for _, tc := range cases {
		got := CanonicalCountries(tc.in)
		if (got == nil) != (tc.want == nil) {
			t.Errorf("CanonicalCountries(%v) nil mismatch: got %v, want %v", tc.in, got, tc.want)
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("CanonicalCountries(%v) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Errorf("CanonicalCountries(%v)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

func TestCodeAliases(t *testing.T) {
	for input, want := range map[string][]string{
		"es":    {"es", "spa"},
		"spa":   {"es", "spa"},
		"de":    {"de", "deu", "ger"},
		"zh":    {"zh", "zho", "chi"},
		"eng":   {"en", "eng"},
		"fil":   {"fil"},
		"pt-BR": {"pt-BR"},
		"":      nil,
	} {
		if got := CodeAliases(input); !slices.Equal(got, want) {
			t.Errorf("CodeAliases(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestMatchRank(t *testing.T) {
	cases := []struct {
		candidate string
		preferred string
		want      int
	}{
		// Exact matches (0)
		{"en", "en", RankExactMatch},
		{"en-US", "en-US", RankExactMatch},
		{"zh-Hans", "zh-Hans", RankExactMatch},
		{"zh-Hant", "zh-Hant", RankExactMatch},
		{"fr-CA", "fr-CA", RankExactMatch},

		// Matching script or region (1 or 2)
		{"zh-Hans-CN", "zh-Hans", RankMatchingScript},
		{"zh-Hant-TW", "zh-Hant", RankMatchingScript},
		{"zh-CN", "zh-Hans", RankMatchingScript},
		{"zh-Hans-foobar", "zh-Hans", RankMatchingScript},

		// Bare language candidate (3)
		{"fr", "fr-CA", RankBareLanguage},
		{"en", "en-US", RankBareLanguage},
		{"zh", "zh-Hans", RankBareLanguage},
		{"zh", "zh-Hant", RankBareLanguage},
		{"zh", "zh-TW", RankBareLanguage},

		// Regional variant with compatible script (4)
		{"fr-BE", "fr-CA", RankRegionalVariant},
		{"en-GB", "en-US", RankRegionalVariant},
		{"zh-Hans", "zh", RankRegionalVariant},
		{"zh-Hant", "zh", RankRegionalVariant},
		{"zh-HK", "zh-TW", RankRegionalVariant},

		// Conflicting script (5)
		{"zh-Hant", "zh-Hans", RankScriptConflict},
		{"zh-Hans", "zh-Hant", RankScriptConflict},
		{"zh-TW", "zh-Hans", RankScriptConflict},
		{"zh-CN", "zh-Hant", RankScriptConflict},
		{"zh-CN", "zh-TW", RankScriptConflict},
		{"zh-Hant-foobar", "zh-Hans", RankScriptConflict},

		// Mismatch (-1)
		{"de", "fr-CA", -1},
		{"", "en", -1},
		{"en", "", -1},
		{"und", "en", -1},
	}

	for _, tc := range cases {
		t.Run(tc.candidate+"_vs_"+tc.preferred, func(t *testing.T) {
			got := MatchRank(tc.candidate, tc.preferred)
			if got != tc.want {
				t.Errorf("MatchRank(%q, %q) = %d, want %d", tc.candidate, tc.preferred, got, tc.want)
			}
		})
	}
}
