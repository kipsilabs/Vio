package playback

import "testing"

// A bitmap page must land on the source-zero page grid, so the page an explicit
// request is extracted as is the page every later request inside it asks for.
// The production bug this pins: requests at 1944.6 and 1939.6 both produced
// 229,103 bytes and shared nothing, because each was anchored on the playhead.
func TestSubtitleRequestGridBitmapPagesAlignToSourceZero(t *testing.T) {
	grid := SubtitleRequestGridFor("hdmv_pgs_subtitle")
	if grid.CellSeconds != SubtitleBitmapPageSeconds || grid.PageSeconds != SubtitleBitmapPageSeconds {
		t.Fatalf("bitmap grid = %+v, want one %vs page", grid, SubtitleBitmapPageSeconds)
	}
	if grid.BackoffSeconds != 0 {
		t.Fatalf("bitmap backoff = %v, want 0: the page already starts at or before the playhead", grid.BackoffSeconds)
	}
	for _, tc := range []struct {
		position float64
		want     float64
	}{
		{position: 0, want: 0},
		{position: 119.999, want: 0},
		{position: 120, want: 120},
		{position: 1939.6, want: 1920},
		{position: 1944.6, want: 1920},
		{position: 2040, want: 2040},
	} {
		if got := grid.Start(tc.position); got != tc.want {
			t.Errorf("page start at %v = %v, want %v", tc.position, got, tc.want)
		}
	}
	if got := grid.NextStart(1920); got != 2040 {
		t.Fatalf("next page after 1920 = %v, want 2040", got)
	}
}

// A text client slides a 120 s window on a 30 s grid, pulled 2 s back from the
// playhead, so its next request at the playhead is the cell containing
// playhead-2. That is what the prefetch mirrors; it is not a page partition.
func TestSubtitleRequestGridTextMirrorsTheSlidingWindow(t *testing.T) {
	grid := SubtitleRequestGridFor("subrip")
	if grid.CellSeconds != SubtitleTextWindowCellSeconds || grid.PageSeconds != SubtitleTextWindowSeconds {
		t.Fatalf("text grid = %+v, want a %vs window on a %vs cell", grid, SubtitleTextWindowSeconds, SubtitleTextWindowCellSeconds)
	}
	for _, tc := range []struct {
		position float64
		want     float64
	}{
		{position: 0, want: 0},
		{position: 1350, want: 1320},
		{position: 1358, want: 1350},
		{position: 300, want: 270},
	} {
		if got := grid.Start(tc.position); got != tc.want {
			t.Errorf("text window start at %v = %v, want %v", tc.position, got, tc.want)
		}
	}
	if got := grid.NextStart(1320); got != 1350 {
		t.Fatalf("next text window after 1320 = %v, want 1350", got)
	}
}

// ASS is a text class, so it mirrors the text window grid; a non-windowable
// grid (a zero cell) must never divide by zero or return a negative start.
func TestSubtitleRequestGridDegenerateInputs(t *testing.T) {
	if got := (SubtitleRequestGrid{}).Start(-10); got != 0 {
		t.Fatalf("empty grid start(-10) = %v, want 0", got)
	}
	if got := (SubtitleRequestGrid{}).NextStart(-10); got != 0 {
		t.Fatalf("empty grid next start(-10) = %v, want 0", got)
	}
	if got := SubtitleRequestGridFor("ass").Start(95); got != 90 {
		t.Fatalf("ASS window start at 95 = %v, want 90", got)
	}
}
