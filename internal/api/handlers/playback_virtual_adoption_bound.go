package handlers

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
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
//     candidate path when the two rows provably carry the same release, landing
//     a real stamp so the session can verify; or
//   - leaves a genuinely foreign or unanswerable candidate fenced, records a
//     durable terminal refusal, and reports an actionable terminal instead of
//     looping.
//
// A same-release renumber never reaches the bound: the resolver's own reported
// identity proves the row's release in virtualProbeEvidenceSharesRowRelease, so
// the write is metadata-only against the pin before any refusal is counted.
//
// The per-replica count is only the trigger that reaches the bound. The
// disposition it produces is durable and cross-replica: a bounded same-release
// rotation persists a real probe stamp on the owner row, and the terminal
// refusal stamps the row's failed_at verdict through the existing fenced marker,
// so a restarted process or a different replica's resolve gate skips the row
// instead of repeating the refusal.
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
	// bounded in practice by the rows currently failing to adopt; the cap is
	// enforced, not merely advertised, so a pathological provider cannot grow the
	// map without bound.
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
// now is injectable for tests and defaults to time.Now; maxEntries overrides the
// default cap for tests that prove the enforcement.
type virtualAdoptionRefusalCache struct {
	mu         sync.Mutex
	marks      map[string]virtualAdoptionRefusalMark
	now        func() time.Time
	maxEntries int
}

func (c *virtualAdoptionRefusalCache) clock() time.Time {
	if c != nil && c.now != nil {
		return c.now()
	}
	return time.Now()
}

// capacity is the hard cap on live marks. It falls back to the default constant
// so a zero-value cache (and the package-wide instance) is still bounded.
func (c *virtualAdoptionRefusalCache) capacity() int {
	if c != nil && c.maxEntries > 0 {
		return c.maxEntries
	}
	return virtualAdoptionRefusalMaxEntries
}

// mark records one more consecutive refusal for key and returns the new count.
// An expired mark restarts the count, so a refusal stream separated by more than
// the TTL does not accumulate. The map never exceeds capacity: expired marks are
// dropped first, and if every mark is live the soonest-expiring one is evicted to
// make room, so a burst of distinct rows cannot grow the map past the advertised
// maximum.
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
	capacity := c.capacity()
	if len(c.marks) >= capacity {
		c.pruneLocked(now)
	}
	if _, exists := c.marks[key]; !exists {
		for len(c.marks) >= capacity {
			c.evictSoonestLocked()
		}
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

// evictSoonestLocked drops the live mark closest to expiry so a new key always
// fits under the hard cap. Caller holds c.mu and must guarantee len(c.marks) > 0.
func (c *virtualAdoptionRefusalCache) evictSoonestLocked() {
	oldestKey := ""
	var oldest time.Time
	for k, mark := range c.marks {
		if oldestKey == "" || mark.expiresAt.Before(oldest) {
			oldestKey, oldest = k, mark.expiresAt
		}
	}
	if oldestKey != "" {
		delete(c.marks, oldestKey)
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

// markVirtualAdoptionTerminalRefusal installs the durable, cross-replica terminal
// refusal for a row whose pinned adoption has been refused
// virtualAdoptionRefusalBound times without a candidate the resolver can tie to
// the pin. It stamps the row's failed_at verdict through the same fenced marker
// the transport-failure path uses, so every replica's resolve and probe gate
// skips the row and a restarted process does not restart the loop. The
// in-process count is only the trigger; this durable stamp is what actually stops
// subsequent attempts. It returns whether a marker was installed.
func (h *PlaybackHandler) markVirtualAdoptionTerminalRefusal(ctx context.Context, row *models.MediaFile, reason string) bool {
	if h == nil || row == nil || row.ID <= 0 || h.VirtualCandidateFailMarker == nil {
		return false
	}
	if err := h.VirtualCandidateFailMarker(ctx, row.ID, row.FilePath, row.FailedAt); err != nil {
		slog.WarnContext(ctx, "virtual adoption terminal refusal was not persisted; the row keeps its own failed_at state",
			"component", "api", "file_id", row.ID, "candidate_path", row.FilePath,
			virtualEvidenceLogKeyReason, reason, virtualEvidenceLogKeyError, err)
		return false
	}
	slog.ErrorContext(ctx, "virtual adoption terminal refusal persisted",
		"component", "api", "file_id", row.ID, "candidate_path", row.FilePath, virtualEvidenceLogKeyReason, reason)
	return true
}
