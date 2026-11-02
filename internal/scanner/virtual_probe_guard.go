package scanner

import (
	"math"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

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
	// The first served-size observation a duration-proven probe records is also
	// unverified: one serve cannot distinguish the release from a wrong file of
	// the same runtime.
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

// VirtualServedSizeSnapshot is the served byte size observed for one pinned
// candidate after a probe whose measured duration proved the content, despite a
// served-vs-declared size gap. Providers routinely advertise a rounded release
// size (an indexer declaration of 2.04 GB for bytes that measure 1.78 GB, ~13%
// off), so the declared size alone can never clear the 5% evidence gate for
// that pin.
//
// One duration-proven serve is an observation, not identity: a wrong file that
// happens to carry the item's runtime would look the same. A snapshot therefore
// only becomes the pin's comparison baseline once a later serve lands on it;
// see RecordVirtualServedSizeSnapshotIfProven and
// ClassifyVirtualServedEvidenceForPinWithSnapshot.
//
// DeclaredBytes is kept so a re-declared release invalidates the snapshot; it is
// never used to replace the row's declared size. FileSize stays the release's
// advertised size for the AltMount-completion comparison and every other
// declared-size decision, so a snapshot can never fake a SourceConfirmed
// verdict.
type VirtualServedSizeSnapshot struct {
	DeclaredBytes  int64
	ObservedBytes  int64
	InventoryToken string
}

// virtualServedSizeSnapshotMaxEntries bounds the snapshot store. Entries are
// per pinned candidate, so the bound tracks the live candidate set the way the
// probe-failure damper does; the oldest snapshot is evicted first.
const virtualServedSizeSnapshotMaxEntries = 512

// virtualServedSizeSnapshotStore is the process-wide snapshot store. It is
// package-level because the guard has no owner object: probes arrive from the
// playback handler and from the probe cache, and both must see each other's
// observations for the same pin.
type virtualServedSizeSnapshotStore struct {
	mu       sync.Mutex
	entries  map[string]virtualServedSizeSnapshotEntry
	sequence int64
}

type virtualServedSizeSnapshotEntry struct {
	snapshot VirtualServedSizeSnapshot
	sequence int64
}

var virtualServedSizeSnapshots = &virtualServedSizeSnapshotStore{entries: make(map[string]virtualServedSizeSnapshotEntry)}

// VirtualServedSizePin names the pinned candidate a served-size snapshot
// belongs to: its provider-neutral virtual path, its owner installation and its
// concrete ?result= pick. A path with no concrete pick names a release rather
// than one candidate's bytes, so it has no pin and returns "": an observed size
// recorded for whatever candidate an auto-pick happened to serve must never
// become the baseline for a sibling.
func VirtualServedSizePin(virtualPath string, ownerInstallationID int) string {
	trimmed := strings.TrimSpace(virtualPath)
	if trimmed == "" {
		return ""
	}
	neutral := trimmed
	resultID := ""
	if parsed, err := url.Parse(trimmed); err == nil && parsed != nil {
		query := parsed.Query()
		resultID = strings.TrimSpace(query.Get("result"))
		if resultID != "" {
			query.Del("result")
			parsed.RawQuery = query.Encode()
			neutral = parsed.String()
		}
	}
	if resultID == "" {
		return ""
	}
	return neutral + "\x00" + strconv.Itoa(ownerInstallationID) + "\x00" + resultID
}

// VirtualServedSizeSnapshotFor returns the snapshot recorded for pin when it
// still describes the pin: the release is still declared at the same size and
// the probed subtitle inventory is unchanged. Either difference invalidates the
// snapshot, because a layout change means the bytes named by the pin's ordinals
// are not the bytes the size was observed for.
func VirtualServedSizeSnapshotFor(pin string, declaredBytes int64, inventoryToken string) (VirtualServedSizeSnapshot, bool) {
	c := virtualServedSizeSnapshots
	if c == nil || pin == "" || declaredBytes <= 0 {
		return VirtualServedSizeSnapshot{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[pin]
	if !ok {
		return VirtualServedSizeSnapshot{}, false
	}
	if entry.snapshot.DeclaredBytes != declaredBytes || entry.snapshot.InventoryToken != inventoryToken || entry.snapshot.ObservedBytes <= 0 {
		delete(c.entries, pin)
		return VirtualServedSizeSnapshot{}, false
	}
	return entry.snapshot, true
}

// RecordVirtualServedSizeSnapshotIfProven records one served-size observation
// for pin when, and only when, a size gap had to be excused by a measured
// duration that proves the content is the item's. It reports whether this call
// recorded the pin's first observation.
//
// Recording an observation is not admission. The observation exists so a later
// serve of the same pin can be compared against it: identical bytes confirm the
// pin, divergent bytes keep it unverified. See
// ClassifyVirtualServedEvidenceForPinWithSnapshot for the verdict side.
//
// It is deliberately the only writer:
//   - a size within tolerance needs no snapshot, so the declared size stays the
//     baseline;
//   - a probe whose duration does not prove identity is unverified or a
//     mismatch and must never teach the gate to accept those bytes;
//   - a pin's existing observation is never overwritten by a divergent serve.
//     A wrong file that once passed the duration check would otherwise keep
//     moving the pin's baseline to whatever it serves, which is exactly how one
//     plausible-looking wrong serve becomes identity. A re-declared release or a
//     changed subtitle inventory invalidates the observation instead
//     (VirtualServedSizeSnapshotFor).
func RecordVirtualServedSizeSnapshotIfProven(pin string, declaredBytes, servedBytes int64, probedSeconds, expectedRuntimeMinutes int, inventoryToken string) bool {
	c := virtualServedSizeSnapshots
	if c == nil || pin == "" || declaredBytes <= 0 || servedBytes <= 0 {
		return false
	}
	if !VirtualServedSizeMismatch(declaredBytes, servedBytes) {
		return false
	}
	if !VirtualDurationProvesIdentity(probedSeconds, expectedRuntimeMinutes) {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]virtualServedSizeSnapshotEntry)
	}
	if existing, ok := c.entries[pin]; ok &&
		existing.snapshot.DeclaredBytes == declaredBytes &&
		existing.snapshot.InventoryToken == inventoryToken &&
		existing.snapshot.ObservedBytes > 0 {
		// The pin already has an observation for this declaration and layout.
		// Keep it: a second, divergent serve is not evidence that replaces the
		// first one, and letting it would turn a duration-plausible wrong file
		// into the baseline by repetition.
		return false
	}
	if _, exists := c.entries[pin]; !exists && len(c.entries) >= virtualServedSizeSnapshotMaxEntries {
		c.evictOldestLocked()
	}
	c.sequence++
	c.entries[pin] = virtualServedSizeSnapshotEntry{
		snapshot: VirtualServedSizeSnapshot{
			DeclaredBytes:  declaredBytes,
			ObservedBytes:  servedBytes,
			InventoryToken: inventoryToken,
		},
		sequence: c.sequence,
	}
	return true
}

// evictOldestLocked drops the least recently recorded snapshot. Caller holds
// c.mu.
func (c *virtualServedSizeSnapshotStore) evictOldestLocked() {
	victim := ""
	var oldest int64
	for pin, entry := range c.entries {
		if victim == "" || entry.sequence < oldest {
			victim, oldest = pin, entry.sequence
		}
	}
	if victim != "" {
		delete(c.entries, victim)
	}
}

// ResetVirtualServedSizeSnapshots drops every recorded snapshot. Tests use it to
// isolate cases that share a pin; production never clears the store.
func ResetVirtualServedSizeSnapshots() {
	c := virtualServedSizeSnapshots
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]virtualServedSizeSnapshotEntry)
}

// ClassifyVirtualServedEvidenceForPin is ClassifyVirtualServedEvidence with the
// pin's recorded observed size as the comparison baseline when a snapshot is
// still valid for it. It is the read-only view: it neither records nor confirms
// an observation, so the returned verdict carries no admission weight by itself.
// A pin with no snapshot (or one invalidated by a declared size or
// subtitle-inventory change) keeps comparing against the declared size, so the
// gate behaves exactly as before for every unproven pin.
func ClassifyVirtualServedEvidenceForPin(pin string, declaredBytes, servedBytes int64, probedSeconds, expectedRuntimeMinutes int, inventoryToken string) VirtualServedEvidenceVerdict {
	baseline := declaredBytes
	if snapshot, ok := VirtualServedSizeSnapshotFor(pin, declaredBytes, inventoryToken); ok {
		baseline = snapshot.ObservedBytes
	}
	return ClassifyVirtualServedEvidence(baseline, servedBytes, probedSeconds, expectedRuntimeMinutes)
}

// VirtualServedSizeSubtitleToken is the subtitle inventory token a snapshot is
// keyed on. It is the same token the subtitle cache uses, so a layout change
// that invalidates warmed subtitle artifacts invalidates the size snapshot too.
func VirtualServedSizeSubtitleToken(tracks []models.SubtitleTrack) string {
	return playback.SubtitleInventoryToken(tracks)
}

// ClassifyVirtualServedEvidenceForPinWithSnapshot returns the evidence verdict
// for a probe of pin and records the pin's first served-size observation when
// the only reason the evidence is not admitted is a size gap that a measured
// duration proves is legitimate.
//
// A single duration-proven serve is an observation, not identity: a wrong file
// of the right runtime and a plausible size would produce exactly the same
// probe. So the serve that records the observation stays Unverified — playable
// for this request, but not cached, persisted as the row's identity, or used to
// bound candidates — and only a later serve that lands on the recorded size is
// Admitted. After that, the observation is the pin's baseline and the cost is
// one extra probe per pin lifetime.
//
// A mismatch (no duration proof) is returned unchanged and records nothing: a
// different release must never teach the gate to accept its bytes. A serve that
// diverges from an existing observation is Unverified as well and leaves the
// observation untouched, so a later wrong file cannot displace the bytes the pin
// was proven to serve.
func ClassifyVirtualServedEvidenceForPinWithSnapshot(pin string, declaredBytes, servedBytes int64, probedSeconds, expectedRuntimeMinutes int, inventoryToken string) VirtualServedEvidenceVerdict {
	snapshot, hasSnapshot := VirtualServedSizeSnapshotFor(pin, declaredBytes, inventoryToken)
	baseline := declaredBytes
	if hasSnapshot {
		baseline = snapshot.ObservedBytes
	}
	verdict := ClassifyVirtualServedEvidence(baseline, servedBytes, probedSeconds, expectedRuntimeMinutes)
	if verdict == VirtualServedEvidenceMismatch {
		return verdict
	}
	if hasSnapshot {
		// A serve inside the observation's tolerance is the second observation
		// that admits the pin. Anything else stays unverified without moving
		// the baseline (RecordVirtualServedSizeSnapshotIfProven refuses to
		// overwrite it).
		return verdict
	}
	if verdict == VirtualServedEvidenceAdmitted {
		// The served size clears the declaration on its own; no observation is
		// needed to excuse a gap, so the pin is admitted without one.
		return verdict
	}
	RecordVirtualServedSizeSnapshotIfProven(pin, declaredBytes, servedBytes, probedSeconds, expectedRuntimeMinutes, inventoryToken)
	return VirtualServedEvidenceUnverified
}
