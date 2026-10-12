package handlers

import (
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/resolver"
)

// Bounded evidence-adoption refusals.
//
// A virtual catalog row pinned to a provider ?result= id can keep losing a
// required identity adoption: the provider renumbers result ids per listing and
// the row's pin is stale, so every fresh probe produces the same
// "required identity adoption matched no row" refusal. The refusal is safe on
// its own — a row must never absorb another release's bytes — but it is
// unbounded: nothing counts the refusals, so the row stays empty and the session
// keeps serving provisional inventory forever.
//
// A bounded consecutive-refusal count turns that into a decision. After
// virtualAdoptionRefusalBound refusals for one row and pin, the writer stops
// retrying the same fenced adoption and instead:
//
//   - rotates the evidence onto the catalog row that already owns the live
//     candidate path when the two rows provably carry the same release, or
//     writes it metadata-only against the pinned row when the probed bytes
//     provably carry the pin's own release identity; either lands a real stamp,
//     so the session can verify and a later start stops re-probing; or
//   - leaves a genuinely foreign release fenced and reports an actionable
//     terminal instead of looping.
//
// The bound is deliberately small. The refusals it counts are per row and pin,
// so unrelated rows never share a count, and a refusal stream for one row cannot
// keep a healthy provider hot.

const (
	// virtualAdoptionRefusalBound is the number of consecutive adoption refusals
	// for one row and pin after which the refusal is treated as stale-pin
	// evidence rather than a transient miss. Three refusals is more than a
	// same-snapshot race (which retries once) and less than any plausible
	// "wait for the provider to settle" expectation.
	virtualAdoptionRefusalBound = 3
	// virtualAdoptionRefusalTTL bounds how long a refusal mark survives without a
	// fresh refusal. It is long enough to carry across the close/resume cycle the
	// death loop spans, and short enough that a row whose provider recovers is
	// not permanently treated as stale.
	virtualAdoptionRefusalTTL = 10 * time.Minute
	// virtualAdoptionRefusalMaxEntries caps the process-wide map. Refusals are
	// bounded in practice by the rows currently failing to adopt; the cap keeps a
	// pathological provider from growing the map without bound.
	virtualAdoptionRefusalMaxEntries = 4096
	// virtualProbeRefusalAdoptionBoundExhausted names a required adoption refused
	// virtualAdoptionRefusalBound or more consecutive times for one row and pin,
	// when the candidate is not the pinned release. It is the terminal disposition
	// the write carries so an operator sees an exhausted refusal rather than
	// another transient miss. The SQL fence itself is unchanged.
	virtualProbeRefusalAdoptionBoundExhausted = "adoption_refusal_bound_exhausted"
)

// virtualAdoptionRefusalMark is one row/pin's consecutive-refusal state.
type virtualAdoptionRefusalMark struct {
	count     int
	expiresAt time.Time
}

// virtualAdoptionRefusalCache counts consecutive evidence-adoption refusals for
// a row and pin. It is package-level because the handler is shared across
// requests and the count must survive a session close and the later resume that
// repeats the same refusal; the small map is bounded and expires idle entries.
// now is injectable for tests and defaults to time.Now.
type virtualAdoptionRefusalCache struct {
	mu    sync.Mutex
	marks map[string]virtualAdoptionRefusalMark
	now   func() time.Time
}

func (c *virtualAdoptionRefusalCache) clock() time.Time {
	if c != nil && c.now != nil {
		return c.now()
	}
	return time.Now()
}

// mark records one more consecutive refusal for key and returns the new count.
// An expired mark restarts the count, so a refusal stream separated by more than
// the TTL does not accumulate.
func (c *virtualAdoptionRefusalCache) mark(key string) int {
	if c == nil || key == "" {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.marks == nil {
		c.marks = make(map[string]virtualAdoptionRefusalMark)
	}
	now := c.clock()
	if len(c.marks) >= virtualAdoptionRefusalMaxEntries {
		c.pruneLocked(now)
	}
	count := 1
	if prev, ok := c.marks[key]; ok && now.Before(prev.expiresAt) {
		count = prev.count + 1
	}
	c.marks[key] = virtualAdoptionRefusalMark{count: count, expiresAt: now.Add(virtualAdoptionRefusalTTL)}
	return count
}

// count returns the live consecutive-refusal count for key, or 0 when no live
// mark exists. An expired mark is dropped on read.
func (c *virtualAdoptionRefusalCache) count(key string) int {
	if c == nil || key == "" {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	mark, ok := c.marks[key]
	if !ok {
		return 0
	}
	if !c.clock().Before(mark.expiresAt) {
		delete(c.marks, key)
		return 0
	}
	return mark.count
}

// clear drops the mark for key: a successful write resets the row's bound.
func (c *virtualAdoptionRefusalCache) clear(key string) {
	if c == nil || key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.marks, key)
}

// pruneLocked drops expired marks. Caller holds c.mu.
func (c *virtualAdoptionRefusalCache) pruneLocked(now time.Time) {
	for k, mark := range c.marks {
		if !now.Before(mark.expiresAt) {
			delete(c.marks, k)
		}
	}
}

// virtualAdoptionRefusals is the process-wide bounded refusal counter. Tests may
// clear entries directly.
var virtualAdoptionRefusals = &virtualAdoptionRefusalCache{marks: make(map[string]virtualAdoptionRefusalMark)}

// virtualAdoptionRefusalKey names one row and pin for the refusal bound. The
// expected path is the pin the row carried when the adoption was requested, so a
// rotation that moves the row's path starts a fresh count instead of inheriting
// the old pin's refusals.
func virtualAdoptionRefusalKey(fileID int, expectedPath string) string {
	if fileID <= 0 {
		return ""
	}
	return strconv.Itoa(fileID) + "\x00" + strings.TrimSpace(expectedPath)
}

// virtualAdoptionRefusalKeyForArgs is virtualAdoptionRefusalKey for a persist
// write: the row id and the expected (pinned) path the write was gated on.
func virtualAdoptionRefusalKeyForArgs(args models.VirtualFilePersistArgs) string {
	return virtualAdoptionRefusalKey(args.FileID, args.ExpectedFilePath)
}

// virtualAdoptionRefusalBoundReached reports whether this write's row and pin
// have already refused enough consecutive adoptions to treat the pin as stale.
func virtualAdoptionRefusalBoundReached(args models.VirtualFilePersistArgs) bool {
	return virtualAdoptionRefusals.count(virtualAdoptionRefusalKeyForArgs(args)) >= virtualAdoptionRefusalBound
}

// virtualAdoptionRefusalBoundReachedFor reports the bound for a row and pin that
// is about to be evaluated before any VirtualFilePersistArgs exists (the
// rotation-target check).
func virtualAdoptionRefusalBoundReachedFor(fileID int, expectedPath string) bool {
	return virtualAdoptionRefusals.count(virtualAdoptionRefusalKey(fileID, expectedPath)) >= virtualAdoptionRefusalBound
}

// virtualCandidateSharesPersistedIdentity reports whether a probed candidate
// provably carries the same release as the pinned row, judged by the same
// symmetric durable-identity tiers used for sibling rows. Both sides must carry
// a non-empty tier and one must agree; an identity-less side is not proof, so a
// candidate whose bytes cannot be tied to the pin stays foreign.
//
// The probed file carries the candidate's durable identity when the resolver
// annotated the served transient (see applyResolvedIdentity), which is the
// normal case for a renumbered same-release candidate.
func virtualCandidateSharesPersistedIdentity(pinned, candidate *models.MediaFile) bool {
	if pinned == nil || candidate == nil {
		return false
	}
	pinnedTiers := resolver.NewPersistedIdentityTiers(
		pinned.ProviderVideoHash, pinned.ProviderGUID, pinned.ProviderReleaseName, pinned.ProviderReleaseSize,
	)
	candidateTiers := resolver.NewPersistedIdentityTiers(
		candidate.ProviderVideoHash, candidate.ProviderGUID, candidate.ProviderReleaseName, candidate.ProviderReleaseSize,
	)
	_, shared := resolver.PersistedIdentitiesMatch(pinnedTiers, candidateTiers)
	return shared
}
