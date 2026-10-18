package quality

import (
	"reflect"
	"testing"
)

// TestMatchedFormatNamesFiltersAndOrders pins the whole contract: only enabled,
// matched, non-rejecting rules are named, and the result is ordered
// score-descending then name-ascending regardless of the configured order.
func TestMatchedFormatNamesFiltersAndOrders(t *testing.T) {
	formats := []CustomFormat{
		{Name: "Low Web", Category: "source", Regex: `(?i)\bweb\b`, Score: 50, Enabled: true},
		{Name: "Remux", Category: "source", Regex: `(?i)\bremux\b`, Score: 200, Enabled: true},
		{Name: "Alpha HDR", Category: "hdr", Regex: `(?i)\bhdr\b`, Score: 200, Enabled: true},
		{Name: "Disabled Remux", Category: "custom", Regex: `(?i)\bremux\b`, Score: 900, Enabled: false},
		{Name: "Reject Flag", Category: "source", Regex: `(?i)\bremux\b`, Score: 500, Enabled: true, Reject: true},
		{Name: "Discard Line", Category: "source", Regex: `(?i)\bremux\b`, Score: discardScoreThreshold, Enabled: true},
		{Name: "Unmatched Audio", Category: "audio", Regex: `(?i)\btruehd\b`, Score: 300, Enabled: true},
	}

	got := MatchedFormatNames(candidateWithTitle("Movie.2024.2160p.REMUX.HDR.WEB-DL"), formats)
	// Remux and Alpha HDR tie on score; Alpha HDR comes first by name.
	want := []string{"Alpha HDR", "Remux", "Low Web"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MatchedFormatNames = %v, want %v", got, want)
	}
}

// TestMatchedFormatNamesInvertAndToken covers the two matching modes that are
// not plain substrings: an inverted rule scores on a non-match, and a token
// rule holds word boundaries so a short keyword cannot match inside a longer
// token.
func TestMatchedFormatNamesInvertAndToken(t *testing.T) {
	formats := []CustomFormat{
		{Name: "10bit", Category: "custom", Pattern: "10bit", PatternType: patternTypeToken, Score: 100, Enabled: true},
		{Name: "Not Commentary", Category: "custom", Pattern: "commentary", PatternType: patternTypeToken, Invert: true, Score: 25, Enabled: true},
		{Name: "TS", Category: "source", Pattern: "ts", PatternType: patternTypeToken, Score: -100, Enabled: true},
	}

	got := MatchedFormatNames(candidateWithTitle("Movie.2024.1080p.10bit.WEB"), formats)
	want := []string{"10bit", "Not Commentary"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MatchedFormatNames = %v, want %v", got, want)
	}

	// "TS" is a token, so it must not match the "ts" inside DTS-HD, but it must
	// match a standalone TS marker.
	if got := MatchedFormatNames(candidateWithTitle("Movie.2024.DTS-HD.1080p"), []CustomFormat{{Name: "TS", Pattern: "ts", PatternType: patternTypeToken, Score: -100, Enabled: true}}); got != nil {
		t.Fatalf("token TS matched inside DTS-HD: %v", got)
	}
	if got := MatchedFormatNames(candidateWithTitle("Movie.2024.TS.1080p"), []CustomFormat{{Name: "TS", Pattern: "ts", PatternType: patternTypeToken, Score: -100, Enabled: true}}); !reflect.DeepEqual(got, []string{"TS"}) {
		t.Fatalf("token TS = %v, want [TS]", got)
	}
}

// TestMatchedFormatNamesEmpty covers the unscored presentation: no matching,
// enabled, non-rejecting rule means no names, so the caller can omit the field.
func TestMatchedFormatNamesEmpty(t *testing.T) {
	formats := []CustomFormat{
		{Name: "CAM", Category: "source", Regex: `(?i)\bcam\b`, Score: -2000, Enabled: true},
		{Name: "Disabled", Category: "custom", Regex: `(?i)\bweb\b`, Score: 100, Enabled: false},
	}
	if got := MatchedFormatNames(candidateWithTitle("Movie.2024.CAM.WEB"), formats); got != nil {
		t.Fatalf("MatchedFormatNames = %v, want nil", got)
	}
}

// TestMatchedFormatNamesMixedRejectAndKeep pins the boundary between this
// display projection and the scoring verdict: a candidate that matches both a
// rejecting rule and non-rejecting ones still lists the non-rejecting names,
// even though CustomFormatScore rejects it and reports zero.
func TestMatchedFormatNamesMixedRejectAndKeep(t *testing.T) {
	formats := []CustomFormat{
		{Name: "Keep Alpha", Category: "custom", Regex: `(?i)\bremux\b`, Score: 100, Enabled: true},
		{Name: "Keep Beta", Category: "custom", Regex: `(?i)\bremux\b`, Score: 50, Enabled: true},
		{Name: "Reject Cam", Category: "source", Regex: `(?i)\bremux\b`, Score: 500, Enabled: true, Reject: true},
	}
	candidate := candidateWithTitle("Movie.2024.2160p.REMUX")

	if score, rejected := CustomFormatScore(candidate, formats); !rejected || score != 0 {
		t.Fatalf("CustomFormatScore = %d rejected %t, want 0 true", score, rejected)
	}

	got := MatchedFormatNames(candidate, formats)
	want := []string{"Keep Alpha", "Keep Beta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MatchedFormatNames = %v, want %v", got, want)
	}
}

// TestMatchedFormatNamesZeroTotalStillNamed pins the other boundary: two
// non-rejecting rules whose scores cancel to zero still match, so this
// projection names them even though the summed score is zero and the api score
// adapter later drops the row as unscored. An absent matched_formats field
// therefore does not by itself mean no formats matched.
func TestMatchedFormatNamesZeroTotalStillNamed(t *testing.T) {
	formats := []CustomFormat{
		{Name: "Plus", Category: "custom", Regex: `(?i)\bremux\b`, Score: 100, Enabled: true},
		{Name: "Minus", Category: "custom", Regex: `(?i)\bremux\b`, Score: -100, Enabled: true},
	}
	candidate := candidateWithTitle("Movie.2024.2160p.REMUX")

	if score, rejected := CustomFormatScore(candidate, formats); rejected || score != 0 {
		t.Fatalf("CustomFormatScore = %d rejected %t, want 0 false", score, rejected)
	}

	got := MatchedFormatNames(candidate, formats)
	want := []string{"Plus", "Minus"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MatchedFormatNames = %v, want %v", got, want)
	}
}
