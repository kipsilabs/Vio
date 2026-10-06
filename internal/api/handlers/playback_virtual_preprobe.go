package handlers

import (
	"context"
	"errors"
	"log/slog"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// virtualDeferredProbeV3 is a probe scheduled to run after the transport commit
// and the first-byte URL delivery. The fresh-start resolve builds it instead of
// spawning the probe inline, so session creation, recipe persistence, and the
// transport commit are not held up by ffprobe enumeration; the start path spawns
// it the moment the URL is handed back. Its probe inputs are value copies
// captured at resolve time, so a later mutation of the request's file does not
// change what is probed; its session identity and binding generation are filled
// in by the start path after the transport commits.
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
	// before the session exists).
	sessionID string
	// bindingGeneration is the candidate-binding generation captured atomically
	// with the candidate identity when the probe was admitted, and retained for
	// the probe's whole lifecycle. It fences the pending write, the shed, the
	// in-probe evidence fence, and the terminal outcome compare-and-swap. A
	// worker never recaptures it: the job is bound to the exact binding it was
	// admitted for, so a rotation cannot let a stale probe act on a binding it
	// does not own. A probe is never admitted without it (see
	// bindDeferredProbeToSession).
	bindingGeneration uint64
}

const (
	// virtualDeferredProbeWorkers bounds the dedicated post-commit probe pool.
	// The pool is separate from the aggregate detached gate: a worker may park
	// waiting for a gate slot, and parking must not consume a request-path
	// worker or a gate slot. Small enough that a fleet of cold starts cannot
	// stampede a provider, large enough that a burst of replays drains.
	virtualDeferredProbeWorkers = 8
	// virtualDeferredProbeQueueSize bounds probes waiting for a pool worker. A
	// full queue is backpressure, not an error: the session stays in the pending
	// state and the probe is parked in the bounded re-park set, which a worker
	// drains once it next becomes free. The request path never probes inline.
	virtualDeferredProbeQueueSize = 64
	// virtualDeferredPublishWorkers bounds the pool that delivers a shed
	// probe's terminal inventory after the response has returned. Publication is
	// off the request path: the failed session outcome is stored immediately, and
	// the catalog read plus realtime fan-out run on this bounded pool so a slow
	// catalog or realtime write cannot delay the response.
	virtualDeferredPublishWorkers = 2
	// virtualDeferredPublishQueueSize bounds both the ready queue and the
	// coalescing retry set of that publisher.
	virtualDeferredPublishQueueSize = 64
)

// Probe lifecycle outcomes recorded on a session by the deferred probe. They are
// the wire-visible inventory_status values: pending while the enumeration is
// outstanding, then verified or failed. Empty means no deferred probe is
// outstanding.
const (
	probeOutcomePending  = "pending"
	probeOutcomeVerified = "verified"
	probeOutcomeFailed   = "failed"
)

// virtualSessionProbeOutcome returns the disposition of the session's deferred
// track-inventory probe, or "" when the session carries none (no deferred probe
// was scheduled, a manager that cannot answer, or the session is gone). It is
// read by the replan deferred-verdict guard: a replan must not terminal on
// incomplete metadata while this is still "pending", because the metadata it
// cannot see is what the outstanding probe is about to persist.
func virtualSessionProbeOutcome(session *playback.Session) string {
	if session == nil {
		return ""
	}
	return session.VirtualProbeOutcome
}

// replanDefersIncompleteMetadataVerdict reports whether a replan terminal must
// be deferred because the session's deferred track-inventory probe is still
// pending. It is the single predicate the replan terminal path consults: a
// source_metadata_incomplete verdict while the probe outcome is "pending" is
// deferred (the probe will persist exactly the metadata the planner could not
// see), and every other terminal, or a probe that has already landed
// (verified/failed) or was never scheduled (""), passes through unchanged.
func replanDefersIncompleteMetadataVerdict(terminal *playback.TerminalV3, session *playback.Session) bool {
	if terminal == nil {
		return false
	}
	if terminal.Reason != sourceMetadataIncompleteReasonV3 {
		return false
	}
	return virtualSessionProbeOutcome(session) == probeOutcomePending
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

// bindDeferredProbeToSession captures the session's candidate-binding identity
// and generation in one manager read and retains the generation on the probe.
// It returns false — dropping the job — when the session is gone, the manager
// cannot supply the binding, or the session is bound to a different candidate
// than the probe enumerated. A probe is never admitted without this ownership,
// so every later pending write, shed, fence, and terminal outcome compare-and-
// swap uses the one generation captured here.
func (h *PlaybackHandler) bindDeferredProbeToSession(deferred *virtualDeferredProbeV3) bool {
	if h == nil || deferred == nil || deferred.sessionID == "" || h.sessionMgr == nil {
		return false
	}
	reader, ok := h.sessionMgr.(interface {
		VirtualSourceBinding(sessionID string) (playback.VirtualSourceBindingSnapshot, error)
	})
	if !ok {
		return false
	}
	binding, err := reader.VirtualSourceBinding(deferred.sessionID)
	if err != nil {
		return false
	}
	if !sameVirtualCandidate(binding.VirtualURI, deferred.cand.URI) {
		return false
	}
	deferred.bindingGeneration = binding.Generation
	return true
}

// enqueueDeferredVirtualProbeV3 admits one deferred probe into the bounded pool.
// It never blocks and never probes on the request path. Ownership is bound
// first: the probe is dropped unless the session's current candidate binding is
// the exact one the probe enumerated, and the binding generation captured there
// is retained for the probe's whole lifecycle. The session is then marked
// pending through that generation, and the probe is handed to the fixed worker
// pool. When both the queue and the bounded retry set are full the probe is shed,
// but never silently: the session's outcome is set to failed immediately and the
// terminal failure is published from bounded background work, so a client leaves
// its loading state instead of waiting forever for a probe that no worker will
// run. The request path itself is never asked to probe or publish synchronously.
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
	if !h.bindDeferredProbeToSession(deferred) {
		slog.InfoContext(ctx, "deferred virtual post-commit probe dropped: session binding does not own the probe candidate",
			"component", "api", "session", deferred.sessionID, "candidate_uri", deferred.cand.URI)
		return
	}
	h.startDeferredProbeWorkers()
	if !h.armDeferredProbePending(deferred) {
		slog.InfoContext(ctx, "deferred virtual post-commit probe dropped: session binding moved before the pending write",
			"component", "api", "session", deferred.sessionID, "candidate_uri", deferred.cand.URI)
		return
	}
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

// armDeferredProbePending marks the session's deferred inventory pending through
// the binding generation retained at admission and reports whether the write
// landed. A manager that cannot fence the write, or one whose binding moved
// between admission and this write, is refused: the worker's terminal
// compare-and-swap would drop the outcome anyway, and a probe whose pending
// never registered must not be enqueued for a binding it no longer owns.
func (h *PlaybackHandler) armDeferredProbePending(deferred *virtualDeferredProbeV3) bool {
	if deferred == nil {
		return false
	}
	return h.applyDeferredProbeOutcome(deferred, probeOutcomePending)
}

// applyDeferredProbeOutcome records a deferred probe lifecycle outcome with a
// compare-and-swap on the generation retained at admission. It never writes
// unconditionally: when the manager cannot fence, the session is gone, or the
// binding moved, the outcome is dropped rather than stamped onto a binding the
// probe does not own. It returns whether the outcome landed, so a caller only
// publishes the inventory the session actually carries.
func (h *PlaybackHandler) applyDeferredProbeOutcome(deferred *virtualDeferredProbeV3, outcome string) bool {
	if h == nil || deferred == nil || deferred.sessionID == "" || h.sessionMgr == nil {
		return false
	}
	writer, ok := h.sessionMgr.(interface {
		SetVirtualProbeOutcomeIfGeneration(sessionID string, generation uint64, outcome string) (bool, error)
	})
	if !ok {
		return false
	}
	applied, err := writer.SetVirtualProbeOutcomeIfGeneration(deferred.sessionID, deferred.bindingGeneration, outcome)
	if err != nil {
		if !errors.Is(err, playback.ErrSessionNotFound) {
			slog.Warn("failed to record virtual probe outcome",
				"component", "api", "session", deferred.sessionID, "outcome", outcome, "error", err)
		}
		return false
	}
	if !applied {
		slog.Info("virtual probe outcome dropped: candidate binding moved before outcome storage",
			"component", "api", "session", deferred.sessionID, "outcome", outcome, "generation", deferred.bindingGeneration)
	}
	return applied
}

// shedDeferredVirtualProbe terminally fails a probe that could not be admitted:
// the session outcome is marked failed immediately, so a client's next inventory
// poll leaves its loading state. Delivery to a push-driven client is handed to
// the bounded background publisher rather than performed here, because a shed
// happens on the start path and its catalog read plus realtime fan-out must not
// delay the response. The fence on the retained binding generation ensures the
// shed probe's failure is not written or published for a replacement binding that
// never had this probe outstanding; when the binding moved, nothing is written
// or published.
func (h *PlaybackHandler) shedDeferredVirtualProbe(deferred *virtualDeferredProbeV3) {
	if h == nil || deferred == nil {
		return
	}
	if !h.applyDeferredProbeOutcome(deferred, probeOutcomeFailed) {
		return
	}
	h.scheduleDeferredProbeInventoryPublish(deferred)
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
// saturated gate delays the probe, never converts it to a synchronous one. The
// binding generation is the one captured at admission and is never recaptured:
// the probe fence and the terminal outcome compare-and-swap both use it, so a
// rotation that landed after admission makes the fence fail and the outcome is
// dropped.
func (h *PlaybackHandler) runDeferredVirtualProbe(deferred *virtualDeferredProbeV3) {
	if h == nil || deferred == nil || deferred.file == nil {
		return
	}
	waitCtx, waitCancel := h.virtualDetachedContext(h.ServiceContext, virtualBackgroundProbeBudget)
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
		if h.ServiceContext != nil && h.ServiceContext.Err() != nil {
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
	bgCtx, bgCancel := h.virtualDetachedContext(h.ServiceContext, virtualBackgroundProbeBudget)
	defer bgCancel()
	outcome := h.probeVirtualSourceAndPersistWith(bgCtx, deferred.stickyKey, deferred.file, deferred.streamURL, *deferred.probeTransient, deferred.cand, deferred.expectedRuntimeMinutes, deferred.ownerID, true, h.deferredProbeBindingIntact(deferred))
	switch outcome {
	case probeOutcomeVerified, probeOutcomeFailed:
		// probeVirtualSourceAndPersistWith has already performed the durable
		// evidence write (a foreground writer either committed it or terminally
		// failed it), so the session outcome can now record whether the verified
		// menu is committed or the enumeration terminally failed. The poll and
		// the push then agree and a client can leave its loading state.
		if h.applyDeferredProbeOutcome(deferred, outcome) {
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

// deferredProbeBindingIntact reports whether the session still carries the exact
// candidate-binding generation the probe was admitted against. It is the fence
// the probe worker passes to probeVirtualSourceAndPersistWith: a rotation that
// lands while the probe runs makes the fence fail, so the stale probe neither
// persists evidence nor records a verdict. A manager that cannot supply the
// binding reports intact; such a manager also cannot fence the terminal outcome,
// so the verdict is dropped and only best-effort evidence remains.
func (h *PlaybackHandler) deferredProbeBindingIntact(deferred *virtualDeferredProbeV3) func() bool {
	return func() bool {
		if h == nil || deferred == nil || deferred.sessionID == "" {
			return false
		}
		reader, ok := h.sessionMgr.(interface {
			VirtualSourceBinding(sessionID string) (playback.VirtualSourceBindingSnapshot, error)
		})
		if !ok {
			return true
		}
		binding, err := reader.VirtualSourceBinding(deferred.sessionID)
		if err != nil {
			return false
		}
		return binding.Generation == deferred.bindingGeneration &&
			sameVirtualCandidate(binding.VirtualURI, deferred.cand.URI)
	}
}

// publishDeferredProbeInventory pushes the session's committed deferred-probe
// inventory to live realtime sessions, so a client watching the push path leaves
// its loading state. It runs after the session outcome has been stored, so the
// pushed status (verified with the committed menu, or failed) is exactly what a
// subsequent inventory poll reports; the fan-out skips sessions whose inventory
// has not changed, so only the affected release is pushed. Best-effort: a
// session without a realtime connection receives nothing and the poll carries
// the same terminal status regardless.
func (h *PlaybackHandler) publishDeferredProbeInventory(deferred *virtualDeferredProbeV3) {
	if h == nil || deferred == nil || deferred.file == nil {
		return
	}
	ctx, cancel := h.virtualDetachedContext(h.ServiceContext, inventoryUpdatedPublishBudget)
	defer cancel()
	h.logInventoryDelivery(h.PublishInventoryUpdated(ctx, deferred.file.ID), 0)
}

// scheduleDeferredProbeInventoryPublish delivers a shed probe's terminal
// inventory from bounded background work instead of on the start path. It never
// blocks: a queue with room is used directly, and a full queue parks the probe in
// a coalescing retry set (one entry per affected file, so duplicate sheds cannot
// grow it without bound) that a worker drains. Beyond the retry bound the push is
// dropped with a warning; the failed session outcome is already stored, so the
// inventory poll still reports the terminal status.
func (h *PlaybackHandler) scheduleDeferredProbeInventoryPublish(deferred *virtualDeferredProbeV3) {
	if h == nil || deferred == nil || deferred.file == nil || deferred.file.ID <= 0 {
		return
	}
	h.startDeferredProbePublishWorkers()
	select {
	case h.deferredPublishQueue <- deferred:
		return
	default:
	}
	h.deferredPublishMu.Lock()
	if _, dup := h.deferredPublishRetry[deferred.file.ID]; !dup &&
		len(h.deferredPublishRetry) >= virtualDeferredPublishQueueSize {
		h.deferredPublishMu.Unlock()
		slog.Warn("deferred virtual post-commit probe inventory publish dropped: publish pool saturated",
			"component", "api", "file_id", deferred.file.ID)
		return
	}
	h.deferredPublishRetry[deferred.file.ID] = deferred
	h.deferredPublishMu.Unlock()
	h.signalDeferredPublishRetry()
}

// startDeferredProbePublishWorkers lazily starts the fixed shed-publication
// pool. It is separate from the probe pool so delivering a terminal failure can
// never be starved by probes still parked or running.
func (h *PlaybackHandler) startDeferredProbePublishWorkers() {
	if h == nil {
		return
	}
	h.deferredPublishOnce.Do(func() {
		if h.deferredPublishQueue == nil {
			h.deferredPublishQueue = make(chan *virtualDeferredProbeV3, virtualDeferredPublishQueueSize)
		}
		if h.deferredPublishRetry == nil {
			h.deferredPublishRetry = make(map[int]*virtualDeferredProbeV3)
		}
		if h.deferredPublishSignal == nil {
			h.deferredPublishSignal = make(chan struct{}, 1)
		}
		for range virtualDeferredPublishWorkers {
			go h.runDeferredProbePublishWorker()
		}
	})
}

// runDeferredProbePublishWorker drains admitted publications until the service
// context ends. Queued work at shutdown is abandoned with the process; the
// session outcome is already stored, so a poll still reports the terminal status.
func (h *PlaybackHandler) runDeferredProbePublishWorker() {
	var serviceDone <-chan struct{}
	if h.ServiceContext != nil {
		serviceDone = h.ServiceContext.Done()
	}
	for {
		select {
		case <-serviceDone:
			return
		case deferred := <-h.deferredPublishQueue:
			if deferred != nil {
				h.publishDeferredProbeInventory(deferred)
			}
		case <-h.deferredPublishSignal:
			if next := h.takeDeferredPublishRetry(); next != nil {
				h.publishDeferredProbeInventory(next)
			}
		}
	}
}

// takeDeferredPublishRetry removes and returns one parked publication, if any.
func (h *PlaybackHandler) takeDeferredPublishRetry() *virtualDeferredProbeV3 {
	h.deferredPublishMu.Lock()
	defer h.deferredPublishMu.Unlock()
	for fileID, deferred := range h.deferredPublishRetry {
		delete(h.deferredPublishRetry, fileID)
		return deferred
	}
	return nil
}

// signalDeferredPublishRetry wakes one idle publish worker to drain the retry
// set. It never blocks: the buffered signal coalesces.
func (h *PlaybackHandler) signalDeferredPublishRetry() {
	if h == nil || h.deferredPublishSignal == nil {
		return
	}
	select {
	case h.deferredPublishSignal <- struct{}{}:
	default:
	}
}
