package handlers

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
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
// resolve skip the provider list. A hit requires all of:
//
//   - a concrete candidate on the row (persistedResultURI): a neutral row has
//     no candidate to bind without a list, so liveness alone cannot serve it;
//   - complete candidate metadata (needsCandidateMetadata false): the listing
//     is the only source of a candidate's declared audio/subtitle labels, so
//     skipping it while metadata is missing makes mergeVirtualCandidateTracks
//     synthesize a codec (e.g. aac) that the provider never declared and that
//     the later inventory push cannot repair — the plan already committed the
//     synthesized recipe. A liveness hit may therefore only ever remove a
//     listing that the row's own evidence already made redundant;
//   - no forced relist, exclusion, or unusable/failed row, so recovery is never
//     blocked.
//
// The metadata guard means a hit can only suppress a listing that
// shouldListVirtualPlaybackCandidates would otherwise run; when the row already
// has complete evidence that gate is already closed, so the memo never removes
// the provider declaration a route needs.
func (h *PlaybackHandler) virtualPreprobeHit(file *fileIdentityV3, persistedResultURI, requestedRowUnusable bool, exclusionPending, forceRelist, needsCandidateMetadata bool, now time.Time) bool {
	if h == nil || file == nil || !persistedResultURI || needsCandidateMetadata || forceRelist || exclusionPending || requestedRowUnusable {
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
	// sessionID is the live session the deferred plan was committed to. It is
	// set by the start path after the transport commits (the resolve runs
	// before the session exists) and carries the probe's terminal outcome back
	// to the session so the inventory reader can leave the loading state.
	sessionID string
	// bindingGeneration is the candidate-binding generation captured when the
	// probe was admitted, and hasBindingGeneration records whether the manager
	// could supply one. The terminal outcome is written with a
	// compare-and-swap on it (see deferOutcomeApplied), so a rotation that lands
	// between probe completion and outcome storage cannot stamp the old probe's
	// verdict onto the replacement binding.
	bindingGeneration    uint64
	hasBindingGeneration bool
	// serviceCtx is the lifecycle context captured at admission. The handler's
	// ServiceContext may be replaced (or nil for a literal-built handler) after
	// a probe is admitted; retaining the captured base keeps the worker's
	// lifecycle and park budget anchored to the request that admitted it.
	serviceCtx context.Context
}

const (
	// virtualDeferredProbeWorkers bounds the dedicated post-commit probe pool.
	// The pool is separate from the aggregate detached gate: a worker may park
	// waiting for a gate slot, and parking must not consume a request-path
	// worker or a gate slot. Small enough that a fleet of cold starts cannot
	// stampede a provider, large enough that a burst of replays drains.
	virtualDeferredProbeWorkers = 8
	// virtualDeferredProbeQueueSize bounds probes waiting for a pool worker.
	// A full queue is backpressure, not an error: the session stays in the
	// pending state and a later trigger (a replay, a rotation, or the inventory
	// poll) re-runs the probe pool rather than the request path probing inline.
	virtualDeferredProbeQueueSize = 64
)

// Probe lifecycle outcomes recorded on a session via SetVirtualProbeOutcome. They
// are the wire-visible inventory_status values for a deferred probe: pending
// while the enumeration is outstanding, then verified or failed. Empty means no
// deferred probe is outstanding.
const (
	probeOutcomePending  = "pending"
	probeOutcomeVerified = "verified"
	probeOutcomeFailed   = "failed"
)

// virtualProbeOutcomeWriter is the optional session-manager capability that
// records a deferred probe's disposition on the live session. A manager without
// it (a minimal test manager) cannot carry the outcome, so the poll path falls
// back to the catalog probe stamp alone.
type virtualProbeOutcomeWriter interface {
	SetVirtualProbeOutcome(sessionID, outcome string) error
}

// setVirtualProbeOutcome records a deferred probe outcome on the live session.
// It is a no-op when the manager cannot carry one or the session has already
// ended, so a probe that completes after its session does not error.
func (h *PlaybackHandler) setVirtualProbeOutcome(sessionID, outcome string) {
	if h == nil || sessionID == "" {
		return
	}
	writer, ok := h.sessionMgr.(virtualProbeOutcomeWriter)
	if !ok {
		return
	}
	if err := writer.SetVirtualProbeOutcome(sessionID, outcome); err != nil && !errors.Is(err, playback.ErrSessionNotFound) {
		slog.Warn("failed to record virtual probe outcome",
			"component", "api", "session", sessionID, "outcome", outcome, "error", err)
	}
}

// startDeferredProbeWorkers lazily starts the fixed post-commit probe pool. The
// pool is bounded, so enqueuing a probe never spawns a goroutine per request.
func (h *PlaybackHandler) startDeferredProbeWorkers() {
	if h == nil {
		return
	}
	h.deferredProbeOnce.Do(func() {
		if h.deferredProbeQueue == nil {
			h.deferredProbeQueue = make(chan *virtualDeferredProbeV3, virtualDeferredProbeQueueSize)
		}
		if h.deferredProbeRetry == nil {
			h.deferredProbeRetry = make(map[string]*virtualDeferredProbeV3)
		}
		if h.deferredProbeSignal == nil {
			h.deferredProbeSignal = make(chan struct{}, 1)
		}
		h.deferredProbeWG.Add(virtualDeferredProbeWorkers)
		for range virtualDeferredProbeWorkers {
			go func() {
				defer h.deferredProbeWG.Done()
				h.runDeferredProbeWorker()
			}()
		}
	})
}

// deferredProbeKey identifies one outstanding deferred probe so a re-admitted
// probe folds onto the same entry instead of stacking duplicates. It binds the
// live session to the exact candidate the enumeration is for.
func deferredProbeKey(deferred *virtualDeferredProbeV3) string {
	if deferred == nil {
		return ""
	}
	return deferred.sessionID + "\x00" + deferred.cand.URI
}

// enqueueDeferredVirtualProbeV3 admits one deferred probe into the bounded pool.
// It never blocks and never probes on the request path. The session is first
// marked pending (conditionally on the binding generation captured at resolve),
// then the probe is handed to the fixed worker pool. When both the queue and the
// bounded retry set are full the probe is shed, but never silently: the session's
// outcome is set to failed and the terminal failure is published, so a client
// leaves its loading state instead of waiting forever for a probe that no worker
// will run. The request path itself is never asked to probe synchronously.
//
// The retry set is drained by the workers themselves, on the signal that follows
// either a fresh admission or a completed probe, so a parked probe completes in
// bounded steps without a second worker pool.
func (h *PlaybackHandler) enqueueDeferredVirtualProbeV3(ctx context.Context, deferred *virtualDeferredProbeV3) {
	if h == nil || deferred == nil || deferred.file == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	h.startDeferredProbeWorkers()
	if deferred.serviceCtx == nil {
		// Anchor the probe's lifecycle to the handler's service context as it is
		// at admission: the request context is canceled the moment the response
		// is written, and the whole point of the deferred probe is to outlive it.
		deferred.serviceCtx = h.ServiceContext
	}
	h.armDeferredProbePending(deferred)
	select {
	case h.deferredProbeQueue <- deferred:
		return
	default:
	}
	// The queue is full. Park the probe in a bounded retry set rather than
	// dropping it: a worker re-admits it when it next drains, so a later
	// completion or the next trigger finishes the enumeration while the request
	// path is never asked to probe synchronously. Beyond the retry bound the
	// probe is shed terminally: the session outcome is marked failed and the
	// failure is published, which ends the client's loading state.
	h.deferredProbeMu.Lock()
	if len(h.deferredProbeRetry) >= virtualDeferredProbeQueueSize {
		h.deferredProbeMu.Unlock()
		slog.WarnContext(ctx, "deferred virtual post-commit probe shed: probe pool saturated; marking inventory failed",
			"component", "api", "candidate_uri", deferred.cand.URI)
		h.shedDeferredVirtualProbe(deferred)
		return
	}
	h.deferredProbeRetry[deferredProbeKey(deferred)] = deferred
	h.deferredProbeMu.Unlock()
	h.signalDeferredProbeRetry()
}

// armDeferredProbePending marks the session's deferred inventory pending, fenced
// on the candidate-binding generation captured at resolve. The generation is
// captured before the CAS so that a rotation landing between resolve and this
// write cannot leave the replacement binding showing the old probe's pending
// state. A manager without the outcome writer (a minimal test manager) is a
// no-op.
func (h *PlaybackHandler) armDeferredProbePending(deferred *virtualDeferredProbeV3) {
	if h == nil || deferred == nil || h.sessionMgr == nil || deferred.sessionID == "" {
		return
	}
	reader, ok := h.sessionMgr.(interface {
		VirtualSourceGeneration(sessionID string) (uint64, error)
	})
	if !ok {
		return
	}
	generation, err := reader.VirtualSourceGeneration(deferred.sessionID)
	if err != nil {
		// No session yet (the caller may set sessionID after the transport
		// commit) or a manager that cannot answer: leave the pending write to
		// the worker, which captures the generation as it runs.
		return
	}
	deferred.bindingGeneration = generation
	deferred.hasBindingGeneration = true
	h.setVirtualProbeOutcomeIfGeneration(deferred.sessionID, generation, probeOutcomePending)
}

// setVirtualProbeOutcomeIfGeneration records an outcome only while the session's
// candidate binding is still the generation the probe was admitted against. It
// is the compare-and-swap half of the fix for a rotation landing between probe
// completion and outcome storage: the old probe's verdict is dropped instead of
// overwriting the new binding's outcome. A manager without the CAS setter (a
// minimal test manager) falls back to the unconditional setter, matching the
// previous best-effort behavior.
func (h *PlaybackHandler) setVirtualProbeOutcomeIfGeneration(sessionID string, generation uint64, outcome string) {
	if h == nil || sessionID == "" || h.sessionMgr == nil {
		return
	}
	writer, ok := h.sessionMgr.(interface {
		SetVirtualProbeOutcomeIfGeneration(sessionID string, generation uint64, outcome string) (bool, error)
	})
	if !ok {
		h.setVirtualProbeOutcome(sessionID, outcome)
		return
	}
	applied, err := writer.SetVirtualProbeOutcomeIfGeneration(sessionID, generation, outcome)
	if err != nil {
		if !errors.Is(err, playback.ErrSessionNotFound) {
			slog.Warn("failed to record virtual probe outcome", "component", "api", "session", sessionID, "outcome", outcome, "error", err)
		}
		return
	}
	if !applied {
		slog.Info("virtual probe outcome dropped: candidate binding moved before outcome storage",
			"component", "api", "session", sessionID, "outcome", outcome, "generation", generation)
	}
}

// shedDeferredVirtualProbe terminally fails a probe that could not be admitted:
// the session outcome is marked failed and the failure published, so a client
// watching either the push or the poll leaves its loading state. The fence on the
// binding generation ensures the shed probe's failure is not written onto a
// replacement binding that never had this probe outstanding; when the binding
// moved, nothing is written or published.
func (h *PlaybackHandler) shedDeferredVirtualProbe(deferred *virtualDeferredProbeV3) {
	if h == nil || deferred == nil {
		return
	}
	if deferred.hasBindingGeneration {
		if !h.deferOutcomeApplied(deferred, deferred.bindingGeneration, true, probeOutcomeFailed) {
			return
		}
	} else {
		// No generation was captured (a manager that cannot answer, or a session
		// that did not exist yet): fall back to the unconditional failed write so
		// the loading state still ends.
		h.setVirtualProbeOutcome(deferred.sessionID, probeOutcomeFailed)
	}
	h.publishDeferredProbeInventory(deferred)
}

// signalDeferredProbeRetry wakes one idle worker to drain the retry set. It
// never blocks: the buffered signal coalesces, so a burst of parked probes is
// drained by whichever workers next become free.
func (h *PlaybackHandler) signalDeferredProbeRetry() {
	if h == nil || h.deferredProbeSignal == nil {
		return
	}
	select {
	case h.deferredProbeSignal <- struct{}{}:
	default:
	}
}

// runDeferredProbeWorker drains admitted probes until the service context ends.
// A worker parks on the aggregate detached gate rather than trying it once:
// parking is what turns saturation into backpressure instead of a synchronous
// fallback, and it happens off the request path.
func (h *PlaybackHandler) runDeferredProbeWorker() {
	var serviceDone <-chan struct{}
	if h.ServiceContext != nil {
		serviceDone = h.ServiceContext.Done()
	}
	for {
		select {
		case <-serviceDone:
			return
		case deferred := <-h.deferredProbeQueue:
			if deferred == nil {
				continue
			}
			h.runDeferredVirtualProbe(deferred)
		case <-h.deferredProbeSignal:
			// A probe that arrived while the queue was full is parked in the
			// retry set; drain one so a saturated burst still completes.
			if next := h.takeDeferredRetry(); next != nil {
				h.runDeferredVirtualProbe(next)
			}
		}
	}
}

// takeDeferredRetry removes and returns one parked probe, if any. A worker calls
// it when the retry signal fires, so a probe parked under queue pressure still
// runs once a worker is free — bounded steps, no second worker pool.
func (h *PlaybackHandler) takeDeferredRetry() *virtualDeferredProbeV3 {
	h.deferredProbeMu.Lock()
	defer h.deferredProbeMu.Unlock()
	for key, deferred := range h.deferredProbeRetry {
		delete(h.deferredProbeRetry, key)
		return deferred
	}
	return nil
}

// runDeferredVirtualProbe runs one admitted post-commit probe under the
// aggregate detached gate and the service lifecycle, then records the terminal
// outcome on the session. The gate is acquired with a blocking wait: a
// saturated gate delays the probe, never converts it to a synchronous one.
func (h *PlaybackHandler) runDeferredVirtualProbe(deferred *virtualDeferredProbeV3) {
	if h == nil || deferred == nil || deferred.file == nil {
		return
	}
	// Capture the binding generation at the moment the probe runs, fenced on the
	// exact candidate, so the terminal outcome only lands on the binding the
	// probe actually enumerated. The generation read and the candidate equality
	// are one read from the manager, so a rotation cannot slip between them.
	generation, hasGeneration := h.deferredProbeGeneration(deferred)
	waitCtx, waitCancel := h.virtualDetachedContext(deferred.serviceCtx, virtualBackgroundProbeBudget)
	defer waitCancel()
	gate := h.detachedGate()
	if !gate.acquire(waitCtx) {
		// The wait ended before a slot freed. If the service is still alive this
		// was only a wait-budget expiry: re-park the probe so the pool keeps
		// retrying it off the request path, and the session's pending state stays
		// honest until it lands. A service shutdown drops it — there is nothing
		// left to serve. A re-park that cannot fit the retry set is shed
		// terminally rather than dropped silently, so the client's loading state
		// still ends.
		if deferred.serviceCtx != nil && deferred.serviceCtx.Err() != nil {
			return
		}
		h.deferredProbeMu.Lock()
		if len(h.deferredProbeRetry) < virtualDeferredProbeQueueSize {
			h.deferredProbeRetry[deferredProbeKey(deferred)] = deferred
			h.deferredProbeMu.Unlock()
			h.signalDeferredProbeRetry()
			return
		}
		h.deferredProbeMu.Unlock()
		slog.WarnContext(waitCtx, "deferred virtual post-commit probe could not be re-parked after gate wait expiry; marking inventory failed",
			"component", "api", "candidate_uri", deferred.cand.URI)
		h.shedDeferredVirtualProbe(deferred)
		return
	}
	defer gate.release()
	bgCtx, bgCancel := h.virtualDetachedContext(deferred.serviceCtx, virtualBackgroundProbeBudget)
	defer bgCancel()
	outcome := h.probeVirtualSourceAndPersistWith(bgCtx, deferred.stickyKey, deferred.file, deferred.streamURL, *deferred.probeTransient, deferred.cand, deferred.expectedRuntimeMinutes, deferred.ownerID, true, h.deferredProbeBindingIntact(deferred.sessionID, deferred.cand.URI))
	switch outcome {
	case probeOutcomeVerified, probeOutcomeFailed:
		// probeVirtualSourceAndPersist has already attempted the evidence write
		// (a foreground writer either landed it or terminally failed it), so the
		// session outcome can now record whether the verified menu is readable
		// or the enumeration terminally failed. The poll and the push then agree
		// and a client can leave its loading state.
		applied := h.deferOutcomeApplied(deferred, generation, hasGeneration, outcome)
		if applied {
			// Now that the session outcome is stored, publish the committed
			// inventory so a push-driven client leaves loading with the same
			// status the poll reports. Publishing after the outcome avoids
			// delivering an event whose status the session does not yet carry.
			h.publishDeferredProbeInventory(deferred)
		}
	default:
		// The binding moved under the probe; the new binding owns its own
		// lifecycle, so no outcome is written.
	}
	// A parked probe may be waiting on the retry set; wake a worker to drain it.
	h.signalDeferredProbeRetry()
}

// deferOutcomeApplied writes the probe's terminal outcome through the
// generation compare-and-swap and reports whether it landed. It returns false
// when a rotation moved the binding while the probe ran, so the caller neither
// records the superseded verdict nor publishes it. A probe without a captured
// generation (a manager that cannot answer) falls back to the unconditional
// write.
func (h *PlaybackHandler) deferOutcomeApplied(deferred *virtualDeferredProbeV3, generation uint64, hasGeneration bool, outcome string) bool {
	if h == nil || deferred == nil || h.sessionMgr == nil {
		return false
	}
	if !hasGeneration {
		h.setVirtualProbeOutcome(deferred.sessionID, outcome)
		return true
	}
	writer, ok := h.sessionMgr.(interface {
		SetVirtualProbeOutcomeIfGeneration(sessionID string, generation uint64, outcome string) (bool, error)
	})
	if !ok {
		// A manager without the CAS setter cannot fence; fall back to the
		// unconditional write (best-effort, as before).
		h.setVirtualProbeOutcome(deferred.sessionID, outcome)
		return true
	}
	applied, err := writer.SetVirtualProbeOutcomeIfGeneration(deferred.sessionID, generation, outcome)
	if err != nil {
		if !errors.Is(err, playback.ErrSessionNotFound) {
			slog.Warn("failed to record virtual probe outcome",
				"component", "api", "session", deferred.sessionID, "outcome", outcome, "error", err)
		}
		return false
	}
	if !applied {
		slog.Info("virtual probe outcome dropped: candidate binding moved before outcome storage",
			"component", "api", "session", deferred.sessionID, "outcome", outcome, "generation", generation)
	}
	return applied
}

// deferredProbeGeneration captures the session's candidate-binding generation and
// reports whether the binding still names the probe's candidate. The two are read
// in one manager call so a rotation cannot interleave between them; a manager
// without the binding reader reports no generation so the caller falls back to
// the unconditional outcome write.
func (h *PlaybackHandler) deferredProbeGeneration(deferred *virtualDeferredProbeV3) (uint64, bool) {
	if h == nil || deferred == nil || deferred.sessionID == "" {
		return 0, false
	}
	reader, ok := h.sessionMgr.(interface {
		VirtualSourceBinding(sessionID string) (playback.VirtualSourceBindingSnapshot, error)
	})
	if !ok {
		return 0, false
	}
	binding, err := reader.VirtualSourceBinding(deferred.sessionID)
	if err != nil {
		return 0, false
	}
	if !sameVirtualCandidate(binding.VirtualURI, deferred.cand.URI) {
		return 0, false
	}
	return binding.Generation, true
}

// deferredProbeBindingIntact reports whether the session is still bound to the
// exact candidate the deferred probe is for. It is the fence the probe worker
// passes to probeVirtualSourceAndPersist: without it, a probe that completes
// after a rotation could write its outcome — and evidence — onto the new
// binding. A manager that cannot supply the binding (a minimal test manager)
// reports intact so the probe proceeds, matching the pre-existing best-effort
// behavior on those managers.
func (h *PlaybackHandler) deferredProbeBindingIntact(sessionID, candidateURI string) func() bool {
	return func() bool {
		if h == nil || sessionID == "" {
			return true
		}
		reader, ok := h.sessionMgr.(interface {
			VirtualSourceBinding(sessionID string) (playback.VirtualSourceBindingSnapshot, error)
		})
		if !ok {
			return true
		}
		binding, err := reader.VirtualSourceBinding(sessionID)
		if err != nil {
			return false
		}
		return sameVirtualCandidate(binding.VirtualURI, candidateURI)
	}
}

// publishDeferredProbeInventory pushes the session's committed deferred-probe
// inventory to live realtime sessions, so a client watching the push path leaves
// its loading state. It runs after the session outcome has been stored, so the
// pushed status (verified with the readable menu, or failed) is exactly what a
// subsequent inventory poll reports; the fan-out skips sessions whose inventory
// has not changed, so only the affected release is pushed. Best-effort: a
// session without a realtime connection receives nothing and the poll carries
// the same terminal status regardless.
func (h *PlaybackHandler) publishDeferredProbeInventory(deferred *virtualDeferredProbeV3) {
	if h == nil || deferred == nil || deferred.file == nil {
		return
	}
	ctx, cancel := h.virtualDetachedContext(deferred.serviceCtx, inventoryUpdatedPublishBudget)
	defer cancel()
	h.logInventoryDelivery(h.PublishInventoryUpdated(ctx, deferred.file.ID), 0)
}
