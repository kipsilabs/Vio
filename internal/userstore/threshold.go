package userstore

import "context"

// Defaults for the progress thresholds, matching the server settings
// playback.watched_threshold and playback.min_resume_threshold.
const (
	DefaultWatchedPct   = 90
	DefaultMinResumePct = 5
)

// ProgressThresholds bundles the watched and min-resume threshold percentages.
// Zero values mean "use defaults" (90% watched, 5% min-resume).
type ProgressThresholds struct {
	WatchedPct   int // mark completed above this % (default 90)
	MinResumePct int // discard progress below this % (default 5)
}

// LibraryProgressThresholdOverrides holds one library's stored percent
// overrides of the server progress thresholds (media_folders.watched_threshold
// and min_resume_threshold). A nil field inherits the server setting, or its
// default when the setting is unset.
type LibraryProgressThresholdOverrides struct {
	WatchedPct   *int
	MinResumePct *int
}

// LibraryProgressThresholdLookup resolves one library folder's stored progress
// threshold overrides. catalog.FolderRepository implements it; callers hold the
// interface so a nil lookup simply means every library inherits the server
// settings.
type LibraryProgressThresholdLookup interface {
	ProgressThresholdOverrides(ctx context.Context, folderID int) (LibraryProgressThresholdOverrides, error)
}

// ResolveProgressThresholds resolves the effective thresholds for one library.
// A library override wins over the server setting (itself defaulted when unset).
// If the resulting pair would put min-resume at or above watched, the resume
// side falls back to the server/default value first; if that still conflicts
// (for example a watched override below the inherited resume floor), the
// watched side falls back too, so the resolved pair is the server/default one.
// The invariant is strict: resolved min-resume is always below resolved watched.
func ResolveProgressThresholds(global ProgressThresholds, overrides LibraryProgressThresholdOverrides) ProgressThresholds {
	watched := normalizeWatchedPct(global.WatchedPct)
	resume := normalizeMinResumePct(global.MinResumePct)
	if overrides.WatchedPct != nil && *overrides.WatchedPct > 0 {
		watched = *overrides.WatchedPct
	}
	if overrides.MinResumePct != nil && *overrides.MinResumePct > 0 {
		resume = *overrides.MinResumePct
	}
	if resume >= watched {
		resume = normalizeMinResumePct(global.MinResumePct)
		if resume >= watched {
			watched = normalizeWatchedPct(global.WatchedPct)
			resume = normalizeMinResumePct(global.MinResumePct)
		}
	}
	if resume >= watched {
		// The server settings themselves are inconsistent, which validation
		// normally prevents. Fall back to the compiled defaults, which are
		// always consistent, so the invariant holds regardless.
		watched = DefaultWatchedPct
		resume = DefaultMinResumePct
	}
	return ProgressThresholds{WatchedPct: watched, MinResumePct: resume}
}

// normalizeWatchedPct maps an unset or non-positive percentage to the default.
func normalizeWatchedPct(pct int) int {
	if pct <= 0 {
		return DefaultWatchedPct
	}
	return pct
}

// normalizeMinResumePct maps an unset or non-positive percentage to the default.
func normalizeMinResumePct(pct int) int {
	if pct <= 0 {
		return DefaultMinResumePct
	}
	return pct
}

// WatchedFraction converts a watched-threshold percentage (e.g. 90) to a
// fraction (0.9). If pct <= 0, returns the default of 0.9 (90%).
func WatchedFraction(pct int) float64 {
	return float64(normalizeWatchedPct(pct)) / 100.0
}

// MinResumeFraction converts a min-resume percentage (e.g. 5) to a fraction
// (0.05). If pct <= 0, returns the default of 0.05 (5%).
func MinResumeFraction(pct int) float64 {
	return float64(normalizeMinResumePct(pct)) / 100.0
}

// ResolveProgressState classifies one progress event against the thresholds:
// skip (below the min-resume floor — discard entirely), completed (above the
// watched threshold — the returned position resets to 0 so completed rows
// hold no resume point, matching MarkWatched), or a plain in-progress update.
// The single home of the rule shared by both store backends and the
// offline-sync ingest; `completed` remains a one-way latch at the write site.
func ResolveProgressState(position, duration float64, t ProgressThresholds) (pos float64, completed, skip bool) {
	// Clamp malformed client input so negative values can never classify — or
	// persist — as real progress on any backend.
	if position < 0 {
		position = 0
	}
	if duration < 0 {
		duration = 0
	}
	if duration > 0 && position > 0 && position/duration < MinResumeFraction(t.MinResumePct) {
		return position, false, true
	}
	completed = duration > 0 && position/duration > WatchedFraction(t.WatchedPct)
	if completed {
		position = 0
	}
	return position, completed, false
}
