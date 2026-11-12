package playback

import "time"

const (
	// RemoteProbeFloor is the least time one probe attempt against a remote
	// (provider-backed) source receives. Production reads a container head in
	// 0.7-1.9s, so 5s is a normal probe with headroom: it is the amount a probe
	// needs, not a budget it may be truncated below.
	RemoteProbeFloor = 5 * time.Second

	// remoteProbeLatencyScale multiplies the slowest recent observation of a
	// source into its probe budget. A probe pays the same upstream first-byte
	// cost the provider just paid, so twice that observation covers the request
	// plus the read without making the budget unbounded.
	remoteProbeLatencyScale = 2
)

// AdaptiveProbeBudget sizes one probe attempt against a remote source from the
// link's observed latency and the caller's remaining budget.
//
//   - floor is what a healthy probe needs; a non-positive floor means
//     RemoteProbeFloor.
//   - maxBudget caps the result. A cap below the floor is raised to the floor,
//     so a caller cannot configure a budget smaller than a normal probe.
//   - observed is the slowest recent provider observation for the link, or 0
//     when the link is unmeasured. A measured link gets twice its observation
//     when that exceeds the floor.
//   - remaining is the caller's remaining budget, 0 when the caller has no
//     deadline, or negative when the deadline has already passed. A bounded
//     caller is never promised more than it has.
//
// A result of 0 means no time is left and the caller must not start a probe.
// The result is always bounded by maxBudget, so a stalled link cannot hold the
// request open.
func AdaptiveProbeBudget(floor, maxBudget, observed, remaining time.Duration) time.Duration {
	if floor <= 0 {
		floor = RemoteProbeFloor
	}
	if maxBudget < floor {
		maxBudget = floor
	}
	budget := floor
	if observed > 0 {
		if scaled := remoteProbeLatencyScale * observed; scaled > budget {
			budget = scaled
		}
	}
	if budget > maxBudget {
		budget = maxBudget
	}
	if remaining > 0 && remaining < budget {
		budget = remaining
	}
	if remaining < 0 || budget <= 0 {
		return 0
	}
	return budget
}
