package handlers

import (
	"context"
	"testing"
	"time"
)

// The catalog discovery budget and the completed-release retry budget are both
// 20 s. One start must not be able to spend both in full: every stage derives
// from a single start-phase deadline, so the retry only sees what is left and
// is skipped entirely once too little remains.
func TestVirtualStartPhaseBudgetBoundsCombinedStart(t *testing.T) {
	start, cancel := withVirtualStartPhaseDeadline(context.Background(), virtualStartPhaseBudget)
	defer cancel()
	startDeadline, ok := start.Deadline()
	if !ok {
		t.Fatal("start phase has no deadline")
	}

	// Candidate resolve derives from the start deadline (the real path wraps
	// r.Context() with virtualStartupBudget).
	resolve, resolveCancel := context.WithTimeout(start, virtualStartupBudget)
	defer resolveCancel()
	resolveDeadline, _ := resolve.Deadline()
	if resolveDeadline.After(startDeadline) {
		t.Fatalf("resolve deadline %v outlives the start deadline %v", resolveDeadline, startDeadline)
	}

	// Transport startup and the pin retry derive from the same resolve
	// context, so neither can hand a stage a fresh full budget.
	transport, transportCancel := context.WithTimeout(resolve, virtualStartupBudget)
	defer transportCancel()
	transportDeadline, _ := transport.Deadline()
	if transportDeadline.After(startDeadline) {
		t.Fatalf("transport deadline %v outlives the start deadline %v", transportDeadline, startDeadline)
	}
	if _, allowed := virtualPinRetryBudget(transport); !allowed {
		t.Fatal("the pin retry was skipped before any budget was spent")
	}
	if budget, allowed := virtualPinRetryBudget(transport); !allowed || budget > virtualCompletedReleaseRetryBudget {
		t.Fatalf("pin retry budget = %v allowed=%v, want a bounded retry", budget, allowed)
	}
}

func TestVirtualPinRetrySkippedWhenStartBudgetExhausted(t *testing.T) {
	// A catalog resolve that consumed the start leaves less than the minimum
	// retry budget; the retry must be skipped rather than open another
	// provider round-trip the deadline would cut off.
	spent, cancel := context.WithDeadline(context.Background(), time.Now().Add(virtualPinRetryMinBudget/2))
	defer cancel()
	if budget, allowed := virtualPinRetryBudget(spent); allowed {
		t.Fatalf("pin retry allowed with %v left, want it skipped below %v", budget, virtualPinRetryMinBudget)
	}

	// A stage with more than the minimum but less than the historical retry
	// budget is shortened to what remains.
	partial, partialCancel := context.WithDeadline(context.Background(), time.Now().Add(virtualPinRetryMinBudget+time.Second))
	defer partialCancel()
	budget, allowed := virtualPinRetryBudget(partial)
	if !allowed {
		t.Fatal("pin retry should run with more than the minimum remaining")
	}
	if budget > virtualPinRetryMinBudget+time.Second || budget <= virtualPinRetryMinBudget {
		t.Fatalf("pin retry budget = %v, want the remaining start budget", budget)
	}
}
