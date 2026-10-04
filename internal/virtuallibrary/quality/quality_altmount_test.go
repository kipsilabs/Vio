package quality

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary/stream"
)

func candidateWithTitle(title string) stream.StreamCandidate {
	return stream.StreamCandidate{Name: title, Title: title}
}

func TestLegacyCustomFormatDefaultsEnabled(t *testing.T) {
	var f CustomFormat
	if err := json.Unmarshal([]byte(`{"name":"English","regex":"eng","score":100}`), &f); err != nil {
		t.Fatalf("unmarshal legacy format: %v", err)
	}
	if !f.IsEnabled() {
		t.Fatal("legacy format without enabled key must default to enabled")
	}
	if f.EffectivePattern() != "eng" {
		t.Fatalf("effective pattern = %q, want regex fallback", f.EffectivePattern())
	}
	score, rejected := CustomFormatScore(candidateWithTitle("Movie.2024.ENG.1080p"), []CustomFormat{f})
	if rejected || score != 100 {
		t.Fatalf("legacy score = %d rejected %t, want 100 false", score, rejected)
	}
}

func TestLegacyGoLiteralStillScores(t *testing.T) {
	// Zero-value Enabled (pre-parity Go literal) must keep scoring so old
	// preset tables work without migration.
	f := CustomFormat{Name: "English", Regex: `(?i)\beng\b`, Score: 100}
	score, rejected := CustomFormatScore(candidateWithTitle("Movie ENG"), []CustomFormat{f})
	if rejected || score != 100 {
		t.Fatalf("legacy literal score = %d rejected %t, want 100 false", score, rejected)
	}
}

func TestTokenPatternAvoidsSubstringFalsePositives(t *testing.T) {
	f := CustomFormat{Name: "TS", Pattern: "TS", PatternType: "token", Score: -10, Enabled: true}
	if _, rejected := CustomFormatScore(candidateWithTitle("Movie.2024.TS.1080p"), []CustomFormat{f}); rejected {
		t.Fatal("token TS must match Movie.2024.TS.1080p")
	}
	score, _ := CustomFormatScore(candidateWithTitle("Movie.2024.TS.1080p"), []CustomFormat{f})
	if score != -10 {
		t.Fatalf("token TS score = %d, want -10", score)
	}
	for _, title := range []string{"Movie.DTS-HD.MA.2024", "Knights.Tale.2024.1080p"} {
		score, _ := CustomFormatScore(candidateWithTitle(title), []CustomFormat{f})
		if score != 0 {
			t.Fatalf("token TS must not match %q (score %d)", title, score)
		}
	}
}

func TestDiscardThresholdRejectsWithoutRejectFlag(t *testing.T) {
	f := CustomFormat{Name: "CAM", Pattern: `\b(cam|camrip)\b`, Score: -2000, Enabled: true}
	_, rejected := CustomFormatScore(candidateWithTitle("Movie.2024.CAM.x264"), []CustomFormat{f})
	if !rejected {
		t.Fatal("score -2000 must reject even without Reject=true (AltMount parity)")
	}
}

func TestInvertAndDisabled(t *testing.T) {
	inv := CustomFormat{Name: "Not-Dubbed", Pattern: `(?i)\bdubbed\b`, Score: 50, Invert: true, Enabled: true}
	if score, _ := CustomFormatScore(candidateWithTitle("Movie.2024.1080p"), []CustomFormat{inv}); score != 50 {
		t.Fatalf("inverted non-match score = %d, want 50", score)
	}
	if score, _ := CustomFormatScore(candidateWithTitle("Movie Dubbed 2024"), []CustomFormat{inv}); score != 0 {
		t.Fatalf("inverted match score = %d, want 0", score)
	}
	off := CustomFormat{Name: "Off", Pattern: `(?i)1080p`, Score: 999, Enabled: false, PatternType: "regex", ID: "x"}
	if score, _ := CustomFormatScore(candidateWithTitle("Movie 1080p"), []CustomFormat{off}); score != 0 {
		t.Fatalf("disabled format score = %d, want 0", score)
	}
}

func TestAltmountRecommendedRanksRemuxOverCam(t *testing.T) {
	var qc QualityConfig
	qc.CustomFormatPreset = "altmount-recommended"
	qc.ApplyPreset()
	if len(qc.CustomFormats) != 11 {
		t.Fatalf("altmount-recommended formats = %d, want 11", len(qc.CustomFormats))
	}
	if err := qc.Validate(); err != nil {
		t.Fatalf("validate altmount-recommended: %v", err)
	}
	remux := candidateWithTitle("Dune.Part.Two.2024.2160p.UHD.BluRay.REMUX.x265.TrueHD.Atmos.7.1-FLUX")
	webdl := candidateWithTitle("Dune.Part.Two.2024.1080p.WEB-DL.x264-NoGroup")
	cam := candidateWithTitle("Gladiator.II.2024.1080p.CAM.x264-NoGroup")
	remuxScore, _ := CustomFormatScore(remux, qc.CustomFormats)
	webScore, _ := CustomFormatScore(webdl, qc.CustomFormats)
	if remuxScore <= webScore {
		t.Fatalf("remux score %d must beat webdl %d", remuxScore, webScore)
	}
	if _, rejected := CustomFormatScore(cam, qc.CustomFormats); !rejected {
		t.Fatal("CAM title must be rejected by altmount-recommended")
	}
}

func TestAltmountPresetNamesAcceptUnderscores(t *testing.T) {
	var qc QualityConfig
	qc.CustomFormatPreset = "altmount_recommended"
	qc.ApplyPreset()
	if len(qc.CustomFormats) == 0 {
		t.Fatal("underscore preset ID must resolve like the hyphenated key")
	}
}

func TestAltmountRemuxAndCompatibilityPresetsValidate(t *testing.T) {
	for _, preset := range []string{"altmount-remux", "altmount-compatibility"} {
		var qc QualityConfig
		qc.CustomFormatPreset = preset
		qc.ApplyPreset()
		if len(qc.CustomFormats) == 0 {
			t.Fatalf("%s produced no formats", preset)
		}
		if err := qc.Validate(); err != nil {
			t.Fatalf("validate %s: %v", preset, err)
		}
	}
}

func TestLegacyTrashRecommendedUntouched(t *testing.T) {
	var qc QualityConfig
	qc.CustomFormatPreset = "trash-recommended"
	qc.ApplyPreset()
	foundGermanReject := false
	for _, f := range qc.CustomFormats {
		if strings.EqualFold(f.Name, "German") && f.Reject {
			foundGermanReject = true
		}
	}
	if !foundGermanReject {
		t.Fatal("legacy trash-recommended must keep its German reject rule")
	}
}

func TestValidateRejectsBadPatternType(t *testing.T) {
	qc := QualityConfig{CustomFormats: []CustomFormat{
		{Name: "Bad", Pattern: "x", PatternType: "fuzzy", Enabled: true},
	}}
	if err := qc.Validate(); err == nil {
		t.Fatal("expected pattern_type validation error")
	}
}

func TestQualityProfileMatchingWithAudioChannelsAndLanguage(t *testing.T) {
	p := QualityProfile{
		Label:         "4K Surround Portuguese",
		Resolution:    "2160p",
		AudioChannels: "5.1",
		Language:      "pt-BR",
	}
	cMatch := stream.StreamCandidate{
		Resolution:     "2160p",
		AudioChannels:  "5.1",
		AudioLanguages: []string{"PT-BR"},
	}
	cWrongChannel := stream.StreamCandidate{
		Resolution:     "2160p",
		AudioChannels:  "2.0",
		AudioLanguages: []string{"PT-BR"},
	}
	cWrongLang := stream.StreamCandidate{
		Resolution:     "2160p",
		AudioChannels:  "5.1",
		AudioLanguages: []string{"ENG"},
	}

	if !MatchProfile(cMatch, p) {
		t.Fatal("cMatch should match profile")
	}
	if MatchProfile(cWrongChannel, p) {
		t.Fatal("cWrongChannel should not match profile")
	}
	if MatchProfile(cWrongLang, p) {
		t.Fatal("cWrongLang should not match profile")
	}
}

func TestSortCandidatesForProfileLanguageAndChannels(t *testing.T) {
	p := QualityProfile{
		Label:    "Portuguese Profile",
		Language: "pt-BR",
	}
	c1 := stream.StreamCandidate{
		OriginalIndex:  0,
		Resolution:     "1080p",
		AudioLanguages: []string{"ENG"},
	}
	c2 := stream.StreamCandidate{
		OriginalIndex:  1,
		Resolution:     "1080p",
		AudioLanguages: []string{"PT-BR"},
	}
	candidates := []stream.StreamCandidate{c1, c2}
	SortCandidatesForProfile(candidates, p, nil)

	if candidates[0].OriginalIndex != 1 {
		t.Fatalf("expected candidate 1 (PT-BR) to sort ahead of candidate 0 (ENG), got candidate %d", candidates[0].OriginalIndex)
	}
}

// TestValidateLenientSkipsBadRegex proves one uncompilable custom format no
// longer fails the whole config: the source pattern stays intact (so
// repetition is stable and strict Validate still sees the original text),
// the compiled matcher is cleared, the skip is reported with a pattern-free
// reason, the good format still compiles, and structural problems still
// error.
func TestValidateLenientSkipsBadRegex(t *testing.T) {
	const badPattern = `(?:(?<=^)MULTI)`
	newConfig := func() QualityConfig {
		return QualityConfig{CustomFormats: []CustomFormat{
			{Name: "Good", Pattern: `\b1080p\b`, PatternType: "regex", Enabled: true},
			{Name: "Bad", Pattern: badPattern, PatternType: "regex", Enabled: true},
		}}
	}
	qc := newConfig()
	if err := qc.Validate(); err == nil {
		t.Fatal("strict Validate must still fail on the bad pattern")
	}
	for attempt := 1; attempt <= 2; attempt++ {
		skipped, err := qc.ValidateLenient()
		if err != nil {
			t.Fatalf("ValidateLenient attempt %d: %v", attempt, err)
		}
		if len(skipped) != 1 || skipped[0].Name != "Bad" {
			t.Fatalf("attempt %d: skipped = %+v, want [{Bad ...}]", attempt, skipped)
		}
		if skipped[0].Reason == "" || strings.Contains(skipped[0].Reason, "MULTI") {
			t.Fatalf("attempt %d: reason %q must be non-empty and pattern-free", attempt, skipped[0].Reason)
		}
	}
	if got := qc.CustomFormats[1].EffectivePattern(); got != badPattern {
		t.Fatalf("bad source pattern not preserved: %q", got)
	}
	if qc.CustomFormats[0].Compiled() == nil {
		t.Fatal("good format lost its compiled matcher")
	}
	// Strict validation still rejects the original bad regex afterward.
	if err := qc.Validate(); err == nil {
		t.Fatal("strict Validate must still fail after lenient validation")
	}
}

// TestValidateLenientBadRuleNeverScores proves an isolated bad rule
// contributes neither score nor rejection for ordinary, inverted, and
// reject rules, while a good rule keeps scoring.
func TestValidateLenientBadRuleNeverScores(t *testing.T) {
	qc := QualityConfig{CustomFormats: []CustomFormat{
		{Name: "Good", Pattern: `\b1080p\b`, PatternType: "regex", Score: 10, Enabled: true},
		{Name: "BadPlain", Pattern: `(?:(?<=^)MULTI)`, PatternType: "regex", Score: 10, Enabled: true},
		{Name: "BadInvert", Pattern: `(?!uncompilable`, PatternType: "regex", Score: 10, Invert: true, Enabled: true},
		{Name: "BadReject", Pattern: `(?<!broken`, PatternType: "regex", Reject: true, Enabled: true},
	}}
	if _, err := qc.ValidateLenient(); err != nil {
		t.Fatalf("ValidateLenient: %v", err)
	}
	candidate := stream.StreamCandidate{Name: "Show.S01E01.1080p.WEB-DL-GRP"}
	score, rejected := CustomFormatScore(candidate, qc.CustomFormats)
	if rejected {
		t.Fatal("isolated bad reject rule must not reject the candidate")
	}
	if score != 10 {
		t.Fatalf("score = %d, want 10 (good rule only)", score)
	}
}

// TestValidateLenientKeepsStructuralErrors proves leniency covers only
// regex compilability: duplicate names, empty patterns, and bad types still
// fail activation the same way.
func TestValidateLenientKeepsStructuralErrors(t *testing.T) {
	for name, formats := range map[string][]CustomFormat{
		"duplicate": {{Name: "X", Pattern: "x"}, {Name: "X", Pattern: "y"}},
		"empty":     {{Name: "X", Pattern: "   "}},
	} {
		qc := QualityConfig{CustomFormats: formats}
		if _, err := qc.ValidateLenient(); err == nil {
			t.Fatalf("%s: expected a structural error", name)
		}
	}
}
