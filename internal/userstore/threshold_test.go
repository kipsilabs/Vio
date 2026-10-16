package userstore

import "testing"

func ptrInt(v int) *int { return &v }

func TestResolveProgressThresholds(t *testing.T) {
	cases := []struct {
		name      string
		global    ProgressThresholds
		overrides LibraryProgressThresholdOverrides
		want      ProgressThresholds
	}{
		{
			name: "no global or overrides falls back to defaults",
			want: ProgressThresholds{WatchedPct: 90, MinResumePct: 5},
		},
		{
			name:   "global settings win over defaults",
			global: ProgressThresholds{WatchedPct: 80, MinResumePct: 10},
			want:   ProgressThresholds{WatchedPct: 80, MinResumePct: 10},
		},
		{
			name:      "watched override wins and inherits the global resume",
			global:    ProgressThresholds{WatchedPct: 80, MinResumePct: 10},
			overrides: LibraryProgressThresholdOverrides{WatchedPct: ptrInt(70)},
			want:      ProgressThresholds{WatchedPct: 70, MinResumePct: 10},
		},
		{
			name:      "resume override wins and inherits the global watched",
			global:    ProgressThresholds{WatchedPct: 80, MinResumePct: 10},
			overrides: LibraryProgressThresholdOverrides{MinResumePct: ptrInt(20)},
			want:      ProgressThresholds{WatchedPct: 80, MinResumePct: 20},
		},
		{
			name:      "consistent override pair is kept",
			global:    ProgressThresholds{WatchedPct: 80, MinResumePct: 10},
			overrides: LibraryProgressThresholdOverrides{WatchedPct: ptrInt(60), MinResumePct: ptrInt(20)},
			want:      ProgressThresholds{WatchedPct: 60, MinResumePct: 20},
		},
		{
			name:      "nil override fields inherit even when the other side is set",
			global:    ProgressThresholds{WatchedPct: 95, MinResumePct: 15},
			overrides: LibraryProgressThresholdOverrides{MinResumePct: ptrInt(3)},
			want:      ProgressThresholds{WatchedPct: 95, MinResumePct: 3},
		},
		{
			name:      "resume override at the watched override falls the resume side back",
			global:    ProgressThresholds{WatchedPct: 90, MinResumePct: 5},
			overrides: LibraryProgressThresholdOverrides{WatchedPct: ptrInt(50), MinResumePct: ptrInt(60)},
			want:      ProgressThresholds{WatchedPct: 50, MinResumePct: 5},
		},
		{
			name:      "resume override equal to watched falls the resume side back",
			global:    ProgressThresholds{WatchedPct: 90, MinResumePct: 5},
			overrides: LibraryProgressThresholdOverrides{WatchedPct: ptrInt(40), MinResumePct: ptrInt(40)},
			want:      ProgressThresholds{WatchedPct: 40, MinResumePct: 5},
		},
		{
			name:      "lone resume override above the global watched is discarded",
			global:    ProgressThresholds{WatchedPct: 80, MinResumePct: 10},
			overrides: LibraryProgressThresholdOverrides{MinResumePct: ptrInt(85)},
			want:      ProgressThresholds{WatchedPct: 80, MinResumePct: 10},
		},
		{
			name:      "watched override below the inherited resume floor collapses to global",
			global:    ProgressThresholds{WatchedPct: 90, MinResumePct: 5},
			overrides: LibraryProgressThresholdOverrides{WatchedPct: ptrInt(3)},
			want:      ProgressThresholds{WatchedPct: 90, MinResumePct: 5},
		},
		{
			name:      "boundary pair resume one below watched is kept",
			global:    ProgressThresholds{WatchedPct: 90, MinResumePct: 5},
			overrides: LibraryProgressThresholdOverrides{WatchedPct: ptrInt(40), MinResumePct: ptrInt(39)},
			want:      ProgressThresholds{WatchedPct: 40, MinResumePct: 39},
		},
		{
			name:      "non-positive override percentages are ignored",
			global:    ProgressThresholds{WatchedPct: 75, MinResumePct: 8},
			overrides: LibraryProgressThresholdOverrides{WatchedPct: ptrInt(0), MinResumePct: ptrInt(-4)},
			want:      ProgressThresholds{WatchedPct: 75, MinResumePct: 8},
		},
		{
			name:   "inconsistent global settings collapse to the defaults",
			global: ProgressThresholds{WatchedPct: 40, MinResumePct: 60},
			want:   ProgressThresholds{WatchedPct: 90, MinResumePct: 5},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveProgressThresholds(tc.global, tc.overrides)
			if got != tc.want {
				t.Fatalf("ResolveProgressThresholds(%+v, %+v) = %+v, want %+v", tc.global, tc.overrides, got, tc.want)
			}
			if got.MinResumePct >= got.WatchedPct {
				t.Fatalf("resolved pair is inconsistent: %+v", got)
			}
		})
	}
}
