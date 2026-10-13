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
//   - leaves a genuinely foreign candidate fenced, records a durable terminal
//     refusal, and reports an actionable terminal instead of looping.
//
// An ownership-lookup failure is not a refusal: it is a recoverable
// infrastructure error that is never counted and never installs the terminal.
//
// A same-release renumber never reaches the bound: the resolver's own reported
// identity proves the row's release in virtualProbeEvidenceSharesRowRelease, so
// the write is metadata-only against the pin before any refusal is counted.
//
// The count is durable and cluster-wide (VirtualAdoptionRefusalStore), so a
// restarted process or a different replica continues the same count instead of
// restarting at zero; the durable terminal decision is therefore reached
// regardless of which replica served the earlier refusals. The disposition it
// produces is durable too: a bounded same-release rotation persists a real probe
// stamp on the owner row, and the terminal refusal stamps the row's failed_at
// verdict through the existing fenced marker, so every replica's resolve gate
// skips the row instead of repeating the refusal.
//
// When a real database and the durable store are wired the evidence write, the
// exact-pin clear, the refusal increment and the terminal decision all run in
// one transaction, serialized on the row being reconciled (see
// execVirtualFileMetadataUpdateResultTx). A successful write always commits its
// clear with it, so a cancel between the two can never leave a stale threshold
// live; the count is re-read in-transaction before the terminal decision; and
// the terminal stamp is fenced on the observed evidence revision, so a newer
// evidence write that advanced the row wins over a stale terminal. Only a
// required adoption that matched no row enters that refusal reconciliation: an
// ordinary metadata CAS miss is a plain no-op and never counts. The same-release
// sibling rotation trigger takes the matching row lock before it increments
// (RecordVirtualAdoptionRefusalLocked), so it, too, serializes on the row.
//
// The in-memory fallback and a store without transaction support keep the
// previous best-effort increment and, because no transaction exists to make the
// count and the revision-fenced stamp atomic, they do not install the durable
// terminal: a deployment without a real database has no durable catalog to
// condemn. A non-transactional database that does carry the durable store keeps
// the best-effort increment for the rotate trigger; the production write path
// always has the transaction.
//
// An ownership-store lookup failure is deliberately NOT a refusal: it is a
// recoverable infrastructure error that must never advance this count or install
// the terminal verdict, so a database outage cannot condemn healthy content. See
// virtualProbeEvidenceRotateTarget.
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
	// virtualAdoptionRefusalMaxEntries caps the in-memory fallback map used when
	// no durable store is wired (tests and non-database deployments). The durable
	// store bounds itself with a TTL and an expired-row sweep.
	virtualAdoptionRefusalMaxEntries = 4096
	// virtualProbeRefusalAdoptionBoundExhausted names a required adoption refused
	// virtualAdoptionRefusalBound or more consecutive times for one row and pin,
	// when the candidate is not the pinned release. It is the terminal disposition
	// the write carries so an operator sees an exhausted refusal rather than
	// another transient miss. The SQL fence itself is unchanged.
	virtualProbeRefusalAdoptionBoundExhausted = "adoption_refusal_bound_exhausted"

	// virtualOwnerLookupBackoffTTL is how long a failed ownership lookup backs
	// off before the next probe re-asks the store. It is short: a database outage
	// that clears quickly resumes immediately after the window, and the backoff
	// never records durable failure.
	virtualOwnerLookupBackoffTTL = 30 * time.Second
	// virtualOwnerLookupBackoffMaxEntries caps the in-memory owner-lookup backoff
	// map so a pathological store outage cannot grow it without bound.
	virtualOwnerLookupBackoffMaxEntries = 4096
)

// VirtualAdoptionRefusalStore is the durable, cluster-wide consecutive-refusal
// state behind the bounded adoption decision. The production implementation is
// the scanner's Postgres-backed FileRepository (an atomic ON CONFLICT upsert
// with a TTL, plus a fenced clear on success); the in-memory
// virtualAdoptionRefusalCache is the fallback for tests and for callers that
// run without a database.
//
// Every method reports a store error rather than a decision: a caller must
// treat an error as a recoverable infrastructure failure, never as a refusal and
// never as a reached bound.
type VirtualAdoptionRefusalStore interface {
	// RecordVirtualAdoptionRefusal atomically increments the live consecutive
	// count for one row and pinned path and returns the new count. An entry older
	// than ttl restarts at one.
	RecordVirtualAdoptionRefusal(ctx context.Context, fileID int, expectedPath string, ttl time.Duration) (int, error)
	// ClearVirtualAdoptionRefusal drops the mark after a write landed, resetting
	// the bound. A missing mark is not an error.
	ClearVirtualAdoptionRefusal(ctx context.Context, fileID int, expectedPath string) error
	// CountVirtualAdoptionRefusal returns the live count without incrementing it,
	// so a caller can decide whether the bound was reached.
	CountVirtualAdoptionRefusal(ctx context.Context, fileID int, expectedPath string) (int, error)
}

// VirtualAdoptionRefusalLockedStore records one consecutive refusal under the
// media_files row lock, so a refusal increment taken outside the evidence
// transaction (the same-release sibling rotation trigger) serializes on the same
// row-first order every replica uses for the evidence write. It reports the live
// count taken under that lock, so a bound decision cannot rest on a count a
// concurrent evidence write or clear has already invalidated. The durable
// Postgres store implements it; a store that does not keeps the previous
// best-effort increment.
type VirtualAdoptionRefusalLockedStore interface {
	RecordVirtualAdoptionRefusalLocked(ctx context.Context, fileID int, expectedPath string, ttl time.Duration) (int, error)
}

// virtualAdoptionRefusalMark is one row/pin's consecutive-refusal state.
type virtualAdoptionRefusalMark struct {
	count     int
	expiresAt time.Time
}

// virtualAdoptionRefusalCache counts consecutive evidence-adoption refusals for
// a row and pin. It is the in-memory fallback store: bounded and expiring. now
// is injectable for tests and defaults to time.Now; maxEntries overrides the
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

// mark records one more consecutive refusal for key with the default TTL and
// returns the new count.
func (c *virtualAdoptionRefusalCache) mark(key string) int {
	return c.markTTL(key, virtualAdoptionRefusalTTL)
}

// markTTL records one more consecutive refusal for key and returns the new
// count. An expired mark restarts the count, so a refusal stream separated by
// more than ttl does not accumulate. The map never exceeds capacity: expired
// marks are dropped first, and if every mark is live the soonest-expiring one is
// evicted to make room, so a burst of distinct rows cannot grow the map past the
// advertised maximum.
func (c *virtualAdoptionRefusalCache) markTTL(key string, ttl time.Duration) int {
	if c == nil || key == "" {
		return 0
	}
	if ttl <= 0 {
		ttl = virtualAdoptionRefusalTTL
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
	c.marks[key] = virtualAdoptionRefusalMark{count: count, expiresAt: now.Add(ttl)}
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

// RecordVirtualAdoptionRefusal implements VirtualAdoptionRefusalStore for the
// in-memory fallback.
func (c *virtualAdoptionRefusalCache) RecordVirtualAdoptionRefusal(_ context.Context, fileID int, expectedPath string, ttl time.Duration) (int, error) {
	return c.markTTL(virtualAdoptionRefusalKey(fileID, expectedPath), ttl), nil
}

// ClearVirtualAdoptionRefusal implements VirtualAdoptionRefusalStore for the
// in-memory fallback.
func (c *virtualAdoptionRefusalCache) ClearVirtualAdoptionRefusal(_ context.Context, fileID int, expectedPath string) error {
	c.clear(virtualAdoptionRefusalKey(fileID, expectedPath))
	return nil
}

// CountVirtualAdoptionRefusal implements VirtualAdoptionRefusalStore for the
// in-memory fallback.
func (c *virtualAdoptionRefusalCache) CountVirtualAdoptionRefusal(_ context.Context, fileID int, expectedPath string) (int, error) {
	return c.count(virtualAdoptionRefusalKey(fileID, expectedPath)), nil
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

// virtualAdoptionRefusals is the process-wide in-memory fallback store: it
// backs the saver path and tests when no durable store is wired. Tests may clear
// entries directly.
var virtualAdoptionRefusals = &virtualAdoptionRefusalCache{marks: make(map[string]virtualAdoptionRefusalMark)}

// adoptionRefusalStore returns the handler's durable store, or the in-memory
// fallback when none is wired.
func (h *PlaybackHandler) adoptionRefusalStore() VirtualAdoptionRefusalStore {
	if h != nil && h.VirtualAdoptionRefusalStore != nil {
		return h.VirtualAdoptionRefusalStore
	}
	return virtualAdoptionRefusals
}

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

// recordAdoptionRefusalStore records one consecutive refusal for a row and pin
// through store and returns the new count. A store error is a recoverable
// infrastructure failure: it is logged and reported as an unknown count (zero),
// never as a reached bound, so a database outage neither advances nor resets the
// durable decision.
func recordAdoptionRefusalStore(ctx context.Context, store VirtualAdoptionRefusalStore, fileID int, expectedPath string) int {
	if store == nil {
		store = virtualAdoptionRefusals
	}
	if fileID <= 0 || strings.TrimSpace(expectedPath) == "" {
		return 0
	}
	count, err := store.RecordVirtualAdoptionRefusal(ctx, fileID, expectedPath, virtualAdoptionRefusalTTL)
	if err != nil {
		slog.WarnContext(ctx, "virtual adoption refusal count was not recorded; treating the store error as recoverable, not as a refusal",
			"component", "api", "file_id", fileID, "expected_path", expectedPath, virtualEvidenceLogKeyError, err)
		return 0
	}
	return count
}

// clearAdoptionRefusalStore drops the refusal mark for a row and pin after a
// write landed. Best-effort: a store error is logged and retried by the next
// successful write, which clears the mark again.
func clearAdoptionRefusalStore(ctx context.Context, store VirtualAdoptionRefusalStore, fileID int, expectedPath string) {
	if store == nil {
		store = virtualAdoptionRefusals
	}
	if fileID <= 0 || strings.TrimSpace(expectedPath) == "" {
		return
	}
	if err := store.ClearVirtualAdoptionRefusal(ctx, fileID, expectedPath); err != nil {
		slog.WarnContext(ctx, "virtual adoption refusal mark was not cleared after a successful write",
			"component", "api", "file_id", fileID, "expected_path", expectedPath, virtualEvidenceLogKeyError, err)
	}
}

// countAdoptionRefusalStore returns the live refusal count for a row and pin, or
// -1 when the store could not be read. The negative sentinel lets a caller
// distinguish "no refusals" from "unknown" so a store error never reads as a
// reached bound.
func countAdoptionRefusalStore(ctx context.Context, store VirtualAdoptionRefusalStore, fileID int, expectedPath string) int {
	if store == nil {
		store = virtualAdoptionRefusals
	}
	if fileID <= 0 || strings.TrimSpace(expectedPath) == "" {
		return 0
	}
	count, err := store.CountVirtualAdoptionRefusal(ctx, fileID, expectedPath)
	if err != nil {
		slog.WarnContext(ctx, "virtual adoption refusal count could not be read; not treating the bound as reached",
			"component", "api", "file_id", fileID, "expected_path", expectedPath, virtualEvidenceLogKeyError, err)
		return -1
	}
	return count
}

// adoptionRefusalBoundReached reports whether this write's row and pin have
// already refused enough consecutive adoptions to treat the pin as stale. A
// store error is not a reached bound.
func (h *PlaybackHandler) adoptionRefusalBoundReached(ctx context.Context, args models.VirtualFilePersistArgs) bool {
	count := countAdoptionRefusalStore(ctx, h.adoptionRefusalStore(), args.FileID, args.ExpectedFilePath)
	return count >= virtualAdoptionRefusalBound
}

// adoptionRefusalStoreCount records one refusal for a row and pin through the
// handler's durable store and returns the new count. When the store can take the
// row lock (the durable Postgres store) the increment is serialized on the same
// media_files row the evidence write locks, so a concurrent successful write or
// clear cannot invalidate the count this decision is made from.
func (h *PlaybackHandler) adoptionRefusalStoreCount(ctx context.Context, fileID int, expectedPath string) int {
	store := h.adoptionRefusalStore()
	if fileID <= 0 || strings.TrimSpace(expectedPath) == "" {
		return 0
	}
	if locked, ok := store.(VirtualAdoptionRefusalLockedStore); ok {
		count, err := locked.RecordVirtualAdoptionRefusalLocked(ctx, fileID, expectedPath, virtualAdoptionRefusalTTL)
		if err != nil {
			slog.WarnContext(ctx, "virtual adoption refusal count was not recorded; treating the store error as recoverable, not as a refusal",
				"component", "api", "file_id", fileID, "expected_path", expectedPath, virtualEvidenceLogKeyError, err)
			return 0
		}
		return count
	}
	return recordAdoptionRefusalStore(ctx, store, fileID, expectedPath)
}

// virtualOwnerLookupBackoff is a bounded, expiring, in-memory marker set for
// failed path-ownership lookups. It backs off the ownership question after a
// store error so a database outage does not re-hit the store on every probe, and
// it NEVER records durable failure: the marker expires on its own, a successful
// lookup clears it immediately, and nothing about it feeds the adoption-refusal
// bound or the failed_at verdict.
type virtualOwnerLookupBackoff struct {
	mu         sync.Mutex
	marks      map[string]time.Time
	now        func() time.Time
	ttl        time.Duration
	maxEntries int
}

func (c *virtualOwnerLookupBackoff) clock() time.Time {
	if c != nil && c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *virtualOwnerLookupBackoff) window() time.Duration {
	if c != nil && c.ttl > 0 {
		return c.ttl
	}
	return virtualOwnerLookupBackoffTTL
}

func (c *virtualOwnerLookupBackoff) capacity() int {
	if c != nil && c.maxEntries > 0 {
		return c.maxEntries
	}
	return virtualOwnerLookupBackoffMaxEntries
}

// recent reports whether key is inside its backoff window. An expired mark is
// dropped on read.
func (c *virtualOwnerLookupBackoff) recent(key string) bool {
	if c == nil || key == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	expiresAt, ok := c.marks[key]
	if !ok {
		return false
	}
	if !c.clock().Before(expiresAt) {
		delete(c.marks, key)
		return false
	}
	return true
}

// mark starts (or restarts) key's backoff window.
func (c *virtualOwnerLookupBackoff) mark(key string) {
	if c == nil || key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.marks == nil {
		c.marks = make(map[string]time.Time)
	}
	now := c.clock()
	capacity := c.capacity()
	for len(c.marks) >= capacity {
		if !c.pruneExpiredLocked(now) {
			c.evictSoonestLocked()
		}
	}
	c.marks[key] = now.Add(c.window())
}

// clear ends key's backoff window after a successful lookup.
func (c *virtualOwnerLookupBackoff) clear(key string) {
	if c == nil || key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.marks, key)
}

// pruneExpiredLocked drops expired marks and reports whether it removed any.
func (c *virtualOwnerLookupBackoff) pruneExpiredLocked(now time.Time) bool {
	removed := false
	for k, expiresAt := range c.marks {
		if !now.Before(expiresAt) {
			delete(c.marks, k)
			removed = true
		}
	}
	return removed
}

// evictSoonestLocked drops the mark closest to expiry. Caller holds c.mu.
func (c *virtualOwnerLookupBackoff) evictSoonestLocked() {
	victim := ""
	var victimAt time.Time
	for k, expiresAt := range c.marks {
		if victim == "" || expiresAt.Before(victimAt) {
			victim, victimAt = k, expiresAt
		}
	}
	if victim != "" {
		delete(c.marks, victim)
	}
}

// virtualOwnerLookups is the process-wide owner-lookup backoff. Tests may
// override its clock or TTL.
var virtualOwnerLookups = &virtualOwnerLookupBackoff{marks: make(map[string]time.Time)}

// virtualOwnerLookupBackoffKey names one row and candidate path for the
// recoverable ownership-lookup backoff.
func virtualOwnerLookupBackoffKey(fileID int, candidateURI string) string {
	if fileID <= 0 {
		return ""
	}
	return strconv.Itoa(fileID) + "\x00" + strings.TrimSpace(candidateURI)
}
