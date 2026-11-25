package playback

import "math"

// The window grid a windowed subtitle client's request path walks. The server
// mirrors it so that three things name the same window: the page an explicit
// bitmap request is extracted as, the page a windowed serve stores, and the page
// a detached prefetch produces. Any of those anchored to the live playhead
// instead is a range no later request computes, so the covering-window cache
// correctly rejects it and every request pays another container read.
//
// The other half of this contract is the web player's
// web/src/player/utils/subtitleWindows.ts (SUBTITLE_WINDOW_SECONDS,
// BITMAP_SUBTITLE_PAGE_SECONDS, WINDOW_ALIGN_SECONDS, SEEK_BACKOFF); the two
// ship together, so a change here that is not mirrored there re-opens the
// miss-every-prefetch bug this grid exists to close.
const (
	// SubtitleBitmapPageSeconds is one bitmap page. A bitmap page can only be
	// produced by demuxing the container, so its cost scales with its length:
	// 120 s measured ~13 s on a 50 GB / 2h52m release, fast enough to finish
	// inside a client fetch, while a 600 s page measured 65-105 s and was
	// abandoned before it completed (an abandoned page stores nothing).
	SubtitleBitmapPageSeconds = 120.0
	// SubtitleTextWindowSeconds is the window length a grid-aligned text client
	// asks for.
	SubtitleTextWindowSeconds = 120.0
	// SubtitleTextWindowCellSeconds is the grid a text client's window starts
	// fall on. Its windows overlap, so text is not partitioned into pages: the
	// cell only mirrors the request a prefetch should produce.
	SubtitleTextWindowCellSeconds = 30.0
	// SubtitleTextWindowBackoffSeconds is how far behind the playhead a text
	// client starts a window, so a small scrub back stays covered.
	SubtitleTextWindowBackoffSeconds = 2.0
)

// SubtitleRequestGrid is the window grid one subtitle class's request path
// walks: the cell its starts fall on, the window length it asks for, and the
// backoff it pulls the anchor back by.
type SubtitleRequestGrid struct {
	CellSeconds    float64
	PageSeconds    float64
	BackoffSeconds float64
}

// SubtitleRequestGridFor returns the request grid for a subtitle codec.
//
// A bitmap client asks for whole pages that start on the source-zero page grid,
// so one page can be stored and re-served for every later request inside it. A
// text client slides a window on a coarser cell and overlaps consecutive
// windows, so it has no page partition; its grid is used only to mirror the
// request a prefetch produces.
func SubtitleRequestGridFor(codec string) SubtitleRequestGrid {
	if IsPGS(codec) {
		return SubtitleRequestGrid{
			CellSeconds: SubtitleBitmapPageSeconds,
			PageSeconds: SubtitleBitmapPageSeconds,
		}
	}
	return SubtitleRequestGrid{
		CellSeconds:    SubtitleTextWindowCellSeconds,
		PageSeconds:    SubtitleTextWindowSeconds,
		BackoffSeconds: SubtitleTextWindowBackoffSeconds,
	}
}

// Start returns the start of the window a grid-aligned request asks for at
// position: the cell that contains the backoff-adjusted playhead. It never
// returns a negative start.
func (g SubtitleRequestGrid) Start(position float64) float64 {
	if g.CellSeconds <= 0 {
		return math.Max(0, position)
	}
	anchored := math.Max(0, position-g.BackoffSeconds)
	return math.Floor(anchored/g.CellSeconds) * g.CellSeconds
}

// NextStart returns the start of the window a grid-aligned request asks for
// after the one starting at start: the next cell on the same grid.
func (g SubtitleRequestGrid) NextStart(start float64) float64 {
	if start < 0 {
		start = 0
	}
	if g.CellSeconds <= 0 {
		return start
	}
	return start + g.CellSeconds
}
