package scanner

import "math"

// VirtualRuntimeTolerance is the maximum fraction a probed duration may deviate
// from the catalog runtime before the candidate is treated as mislabeled
// content rather than a legitimate release.
//
// 0.30 accommodates anime and international content where fansub groups trim
// OP/ED sequences and streaming cuts differ from broadcast metadata by 3–5
// minutes on a typical 24-minute episode. Genuinely wrong content (a trailer, a
// sample, or a different show) differs by far more than this.
const VirtualRuntimeTolerance = 0.30

// VirtualRuntimePlausible reports whether a probed duration is consistent with
// the item's catalog runtime. Unknown values pass: the gate only rejects when
// both sides are known and clearly disagree.
func VirtualRuntimePlausible(probedSeconds, runtimeMinutes int) bool {
	if probedSeconds <= 0 || runtimeMinutes <= 0 {
		return true
	}
	expected := float64(runtimeMinutes) * 60
	diff := math.Abs(float64(probedSeconds) - expected)
	return diff <= expected*VirtualRuntimeTolerance
}

// VirtualDurationProvesIdentity reports whether a measured duration is a known,
// plausible match for the item's expected runtime. Unlike
// VirtualRuntimePlausible, missing evidence on either side is not a pass: the
// caller uses this to overrule a served-size mismatch, which needs a positive
// identity signal, not the absence of a contradiction.
func VirtualDurationProvesIdentity(probedSeconds, expectedRuntimeMinutes int) bool {
	return probedSeconds > 0 && expectedRuntimeMinutes > 0 &&
		VirtualRuntimePlausible(probedSeconds, expectedRuntimeMinutes)
}

// VirtualServedEvidenceRejected reports whether probed evidence must be
// rejected as a different release than the catalog row declares: the served
// container size disagrees beyond VirtualServedSizeTolerance and the measured
// duration does not prove identity.
//
// Size alone is not an identity verdict. A release can be posted short of the
// indexer's advertised size while still being the right content — production
// observed a 5.57 GB posting of a release an indexer declared at 6.33 GB. When
// the probed duration matches the item's runtime that posting is accepted; the
// size disagreement stays visible in the caller's warning. A genuinely
// different release (a remux of another title, say) fails the duration check
// independently of its size, and evidence with no duration on either side
// keeps the historical rejection.
func VirtualServedEvidenceRejected(declaredBytes, servedBytes int64, probedSeconds, expectedRuntimeMinutes int) bool {
	if !VirtualServedSizeMismatch(declaredBytes, servedBytes) {
		return false
	}
	return !VirtualDurationProvesIdentity(probedSeconds, expectedRuntimeMinutes)
}
