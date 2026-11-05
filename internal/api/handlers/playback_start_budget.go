package handlers

import (
	"context"
	"time"
)

// virtualStartPhaseBudget bounds one user-visible virtual start: candidate
// listing/resolve and transport startup observe the same deadline. Provider
// listings measured 17–25 s in production, so 30 s admits the slow band with
// margin while keeping the catalog discovery budget (20 s, detached from the
// start request) from stacking on top of it. The budget is a var so tests can
// shrink it.
var virtualStartPhaseBudget = 30 * time.Second

// withVirtualStartPhaseDeadline attaches the shared start-phase deadline. The
// caller owns the returned cancel and must defer it for the duration of the
// start, so transport startup and every recovery stage see the same bound.
func withVirtualStartPhaseDeadline(ctx context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	if budget <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, budget)
}
