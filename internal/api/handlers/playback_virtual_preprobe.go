package handlers

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

// The preprobe pin-liveness cache is a lightweight, content-keyed memo that a
// provider listing for this content answered recently. It exists so a cold
// virtual start whose row already names a concrete ?result= candidate can skip
// the full provider listing while that liveness observation is fresh, instead
// of paying a listing round-trip on every start. A miss falls through to the
// ordinary path (the item-1 bounded parallel version walk covers a dead pin),
// so the cache can only remove a listing that a live observation already
// justified; it never substitutes a release or hides an outage.
//
// The key is (content_id, owner installation), not the candidate URI: liveness
// is a property of the content's provider listing at that installation, and
// the observation is made exactly when a listing answers with candidates. The
// TTL is deliberately short (30-60s) because upstream listings are volatile —
// a liveness hit from a minute ago is not evidence the pin is still listed now,
// and the serving path's own re-resolve/rotation still owns the release truth.
const (
	// virtualPreprobeDefaultTTL is how long a successful listing observation
	// suppresses the next full listing for the same content. It sits in the
	// issue's 30-60s window: long enough to cover a burst of replays and
	// failover hops, short enough that a volatile provider is re-listed soon.
	virtualPreprobeDefaultTTL = 45 * time.Second
	// virtualPreprobeDefaultMaxEntries bounds the memo. A later admission past
	// the ceiling evicts the oldest-expired entry it can find; when every entry
	// is live it admits anyway after dropping one arbitrary entry, so the cache
	// never grows without bound and never wedges.
	virtualPreprobeDefaultMaxEntries = 4096
)

// virtualPreprobeTTL is a test seam for the liveness window; a non-positive
// value keeps the production default so a handler built outside the router is
// unaffected.
var virtualPreprobeTTL = virtualPreprobeDefaultTTL

// virtualPreprobeKey is the liveness equivalence key: the content identity plus
// the owning installation, because two installations list independently and a
// hit for one must not suppress the other's listing.
func virtualPreprobeKey(contentID string, ownerInstallationID int) string {
	if contentID == "" {
		return ""
	}
	return contentID + "\x00" + strconv.Itoa(ownerInstallationID)
}

// virtualPreprobeCache is a bounded, TTL'd memo of recent successful listings.
// It is safe for concurrent use.
type virtualPreprobeCache struct {
	mu      sync.Mutex
	entries map[string]time.Time
	max     int
}

func newVirtualPreprobeCache(max int) *virtualPreprobeCache {
	if max <= 0 {
		max = virtualPreprobeDefaultMaxEntries
	}
	return &virtualPreprobeCache{entries: make(map[string]time.Time), max: max}
}

// record stamps a successful listing observation for key as of now.
func (c *virtualPreprobeCache) record(key string, now time.Time) {
	if c == nil || key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]time.Time)
	}
	if _, exists := c.entries[key]; !exists && len(c.entries) >= c.max {
		c.evictOldestLocked(now)
	}
	c.entries[key] = now
}

// hit reports whether key carries an unexpired listing observation. It also
// drops the entry once it has expired, so a stale key is not retried forever.
func (c *virtualPreprobeCache) hit(key string, now time.Time) bool {
	if c == nil || key == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	at, ok := c.entries[key]
	if !ok {
		return false
	}
	if now.Sub(at) >= virtualPreprobeWindow() {
		delete(c.entries, key)
		return false
	}
	return true
}

// evictOldestLocked drops one entry so a new key can be admitted at the
// ceiling. It prefers the oldest expired entry; when every entry is still live
// it drops the globally oldest, trading one liveness observation for the bound.
func (c *virtualPreprobeCache) evictOldestLocked(now time.Time) {
	var oldestKey string
	var oldest time.Time
	first := true
	for key, at := range c.entries {
		if now.Sub(at) >= virtualPreprobeWindow() {
			delete(c.entries, key)
			return
		}
		if first || at.Before(oldest) {
			oldestKey, oldest, first = key, at, false
		}
	}
	if oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}

// virtualPreprobeWindow resolves the active liveness window, honoring the test
// seam.
func virtualPreprobeWindow() time.Duration {
	if virtualPreprobeTTL > 0 {
		return virtualPreprobeTTL
	}
	return virtualPreprobeDefaultTTL
}

// preprobeCache lazily builds the handler's liveness memo for handlers built
// as literals (tests) as well as through the router.
func (h *PlaybackHandler) preprobeCache() *virtualPreprobeCache {
	if h == nil {
		return nil
	}
	h.virtualPreprobeOnce.Do(func() {
		if h.virtualPreprobeCache == nil {
			h.virtualPreprobeCache = newVirtualPreprobeCache(virtualPreprobeDefaultMaxEntries)
		}
	})
	return h.virtualPreprobeCache
}

// recordVirtualPreprobeLiveness stamps a successful listing for the file's
// content so the next start can skip the listing while it is fresh.
func (h *PlaybackHandler) recordVirtualPreprobeLiveness(file *fileIdentityV3, now time.Time) {
	if h == nil || file == nil {
		return
	}
	h.preprobeCache().record(virtualPreprobeKey(file.contentID, file.ownerID), now)
}

// virtualPreprobeHit reports whether a fresh listing observation lets this
// resolve skip the provider list. It requires the row to already name a
// concrete candidate (persistedResultURI): a neutral row has no candidate to
// bind without a list, so liveness alone cannot serve it. A forced relist, an
// exclusion, or an unusable/failed row all bypass the memo so recovery is never
// blocked.
func (h *PlaybackHandler) virtualPreprobeHit(file *fileIdentityV3, persistedResultURI, requestedRowUnusable bool, exclusionPending, forceRelist bool, now time.Time) bool {
	if h == nil || file == nil || !persistedResultURI || forceRelist || exclusionPending || requestedRowUnusable {
		return false
	}
	return h.preprobeCache().hit(virtualPreprobeKey(file.contentID, file.ownerID), now)
}

// fileIdentityV3 is the minimal content identity a liveness observation keys
// on, so the preprobe helpers do not take a full *models.MediaFile.
type fileIdentityV3 struct {
	contentID string
	ownerID   int
}

// virtualDeferredProbeV3 is a probe scheduled to run after the transport commit
// and the first-byte URL delivery. The fresh-start resolve builds it instead of
// spawning the probe inline, so session creation, recipe persistence, and the
// transport commit are not held up by ffprobe enumeration; the start path spawns
// it the moment the URL is handed back. Its fields are value copies captured at
// resolve time, so a later mutation of the request's file does not change what
// is probed.
type virtualDeferredProbeV3 struct {
	stickyKey              string
	file                   *models.MediaFile
	streamURL              string
	probeTransient         *models.MediaFile
	cand                   VirtualPlaybackStream
	expectedRuntimeMinutes int
	ownerID                int
}

// spawnDeferredVirtualProbeV3 runs a probe deferred past the transport commit.
// It uses the same detached gate and bounded context the inline start-path probe
// used, so scheduling it later changes only when the ffprobe runs, never the
// concurrency budget or the persistence guarantees. When the gate is exhausted
// it falls back to the bounded foreground probe so the served row is not left
// unprobed on every later play.
func (h *PlaybackHandler) spawnDeferredVirtualProbeV3(ctx context.Context, deferred *virtualDeferredProbeV3) {
	if h == nil || deferred == nil || deferred.file == nil {
		return
	}
	gate := h.detachedGate()
	if !gate.tryAcquire() {
		slog.WarnContext(ctx, "deferred virtual post-commit probe skipped: detached worker budget exhausted",
			"component", "api", "candidate_uri", deferred.cand.URI)
		h.probeVirtualCandidateForegroundFallback(ctx, deferred.stickyKey, deferred.file, deferred.streamURL, *deferred.probeTransient, deferred.cand, deferred.expectedRuntimeMinutes, deferred.ownerID)
		return
	}
	bgCtx, bgCancel := h.virtualDetachedContext(ctx, virtualBackgroundProbeBudget)
	go func() {
		defer gate.release()
		defer bgCancel()
		h.probeVirtualSourceAndPersist(bgCtx, deferred.stickyKey, deferred.file, deferred.streamURL, *deferred.probeTransient, deferred.cand, deferred.expectedRuntimeMinutes, deferred.ownerID)
	}()
}
