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
// VirtualRuntimePlausible, missing evidence on either side is not a pass. A
// served-size mismatch needs a positive signal, not the absence of a
// contradiction, before the evidence counts for anything — and even then it
// only keeps the bytes playable, never proves release identity.
func VirtualDurationProvesIdentity(probedSeconds, expectedRuntimeMinutes int) bool {
	return probedSeconds > 0 && expectedRuntimeMinutes > 0 &&
		VirtualRuntimePlausible(probedSeconds, expectedRuntimeMinutes)
}

// VirtualServedEvidenceVerdict classifies probed evidence against the release
// the selected catalog row declares.
type VirtualServedEvidenceVerdict int

const (
	// VirtualServedEvidenceAdmitted means the served container size agrees with
	// the declared release size. The evidence establishes release identity and
	// may be cached, persisted and used for identity-sensitive decisions.
	VirtualServedEvidenceAdmitted VirtualServedEvidenceVerdict = iota
	// VirtualServedEvidenceUnverified means the served size contradicts the
	// declared release size while the measured duration is plausible for the
	// item. The bytes are playable for the current request — a release can be
	// posted short of the indexer's advertised size — but the evidence does not
	// prove which release was served. It must not enter the probe cache as
	// authoritative evidence, be persisted as the row's identity, or feed
	// identity-sensitive decisions such as version death or candidate bounding.
	VirtualServedEvidenceUnverified
	// VirtualServedEvidenceMismatch means the served bytes are not the declared
	// release: the size disagrees and the duration does not prove identity.
	VirtualServedEvidenceMismatch
)

// EstablishesIdentity reports whether the verdict is strong enough to stand as
// the row's release identity.
func (v VirtualServedEvidenceVerdict) EstablishesIdentity() bool {
	return v == VirtualServedEvidenceAdmitted
}

// ClassifyVirtualServedEvidence classifies probed evidence against the declared
// release. Size alone is not an identity verdict. A release can be posted short
// of the indexer's advertised size while still being the right content —
// production observed a 5.57 GB posting of a release an indexer declared at
// 6.33 GB — so a size contradiction with a plausible duration stays playable
// but unverified rather than becoming proof of identity. A genuinely different
// release (a remux of another title, say) fails the duration check
// independently of its size, and evidence with no duration on either side
// keeps the historical rejection.
func ClassifyVirtualServedEvidence(declaredBytes, servedBytes int64, probedSeconds, expectedRuntimeMinutes int) VirtualServedEvidenceVerdict {
	if !VirtualServedSizeMismatch(declaredBytes, servedBytes) {
		return VirtualServedEvidenceAdmitted
	}
	if VirtualDurationProvesIdentity(probedSeconds, expectedRuntimeMinutes) {
		return VirtualServedEvidenceUnverified
	}
	return VirtualServedEvidenceMismatch
}

// VirtualServedEvidenceRejected reports whether probed evidence must be
// rejected as a different release than the catalog row declares: the served
// container size disagrees beyond VirtualServedSizeTolerance and the measured
// duration does not prove identity. A size contradiction whose duration is
// plausible is unverified, not rejected: it still plays, but see
// VirtualServedEvidenceVerdict for what it may not be used for.
func VirtualServedEvidenceRejected(declaredBytes, servedBytes int64, probedSeconds, expectedRuntimeMinutes int) bool {
	return ClassifyVirtualServedEvidence(declaredBytes, servedBytes, probedSeconds, expectedRuntimeMinutes) == VirtualServedEvidenceMismatch
}
