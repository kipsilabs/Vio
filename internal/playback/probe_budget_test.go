package playback

import (
	"testing"
	"time"
)

// TestAdaptiveProbeBudgetScalesFromObservedLatency pins the sizing rule every
// remote probe uses: the floor is what a healthy probe needs, a measured slow
// link extends it to twice its slowest recent observation, the cap bounds it,
// and the caller's remaining budget is never exceeded.
func TestAdaptiveProbeBudgetScalesFromObservedLatency(t *testing.T) {
	const (
		floor     = 5 * time.Second
		maxBudget = 15 * time.Second
	)
	for _, tc := range []struct {
		name      string
		observed  time.Duration
		remaining time.Duration
		want      time.Duration
	}{
		{name: "unmeasured keeps the floor", observed: 0, want: floor},
		{name: "healthy observation keeps the floor", observed: 1900 * time.Millisecond, want: floor},
		{name: "spike extends past the old wall", observed: 4800 * time.Millisecond, want: 9600 * time.Millisecond},
		{name: "slow link reaches the cap", observed: 10 * time.Second, want: maxBudget},
		{name: "stall stays inside the cap", observed: 5 * time.Minute, want: maxBudget},
		{name: "caller budget caps the extension", observed: 6 * time.Second, remaining: 8 * time.Second, want: 8 * time.Second},
		{name: "caller budget below the floor still bounds it", observed: 0, remaining: 3 * time.Second, want: 3 * time.Second},
		{name: "no time left yields no probe", observed: 6 * time.Second, remaining: -time.Second, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := AdaptiveProbeBudget(floor, maxBudget, tc.observed, tc.remaining); got != tc.want {
				t.Fatalf("budget for observed=%v remaining=%v = %v, want %v", tc.observed, tc.remaining, got, tc.want)
			}
		})
	}
	// A cap below the floor is raised to the floor: a caller cannot configure a
	// budget smaller than a normal probe.
	if got := AdaptiveProbeBudget(floor, time.Second, 0, 0); got != floor {
		t.Fatalf("budget with a cap below the floor = %v, want the floor %v", got, floor)
	}
	// A zero floor falls back to RemoteProbeFloor.
	if got := AdaptiveProbeBudget(0, time.Minute, 0, 0); got != RemoteProbeFloor {
		t.Fatalf("budget with a zero floor = %v, want %v", got, RemoteProbeFloor)
	}
}
