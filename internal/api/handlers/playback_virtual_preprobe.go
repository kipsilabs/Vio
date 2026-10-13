package handlers

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/resolver"
)

// virtualDeferredProbeV3 is a probe scheduled to run after the transport commit
// and the first-byte URL delivery. The fresh-start resolve builds it instead of
// spawning the probe inline, so session creation, recipe persistence, and the
// transport commit are not held up by ffprobe enumeration; the start path admits
// it to the worker pool the moment the URL is handed back. Its fields are value
// copies captured at resolve time, so a later mutation of the request's file
// does not change what is probed.
type virtualDeferredProbeV3 struct {
	stickyKey              string
	file                   *models.MediaFile
	streamURL              string
	probeTransient         *models.MediaFile
	cand                   VirtualPlaybackStream
	candidate              virtualProbeIdentity
	expectedRuntimeMinutes int
	ownerID                int
	// sessionID is the live session the deferred plan was committed to. It is
	// set by the start path after the transport commits (the resolve runs
	// before the session exists) and carries the probe's terminal outcome back
	// to the session so the inventory reader can leave the loading state.
	sessionID string
	// bindingGeneration is the candidate-binding generation captured at
	// admission, when the manager confirmed the session was still bound to this
	// probe's candidate. It is retained for the whole lifecycle — pending write,
	// probe fencing, shed, and terminal compare-and-swap — so a rotation that
	// lands anywhere after admission cannot stamp this probe's verdict onto the
	// replacement binding. A manager that cannot supply one (a minimal test
	// double) leaves it zero; the conditional write is still the only outcome
	// path, so such a job simply never passes a fence against a real manager.
	bindingGeneration uint64
	// binding is the full candidate-binding snapshot captured at admission
	// alongside bindingGeneration. The verified-inventory propagation CASes on
	// it, so a row-id-only mutation (SetEffectiveMediaFileID moves the effective
	// row without bumping the generation) between the release-continuity check
	// and the commit cannot land the probe's inventory on a changed binding. A
	// manager that cannot supply a snapshot leaves it zero; the CAS then falls
	// back to the generation-only fence.
	binding playback.VirtualSourceBindingSnapshot
}

const (
	// virtualDeferredProbeWorkers bounds the dedicated post-commit probe pool.
	// The pool is separate from the aggregate detached gate: a worker may park
	// waiting for a gate slot, and parking must not consume a request-path
	// worker or a gate slot. Small enough that a fleet of cold starts cannot
	// stampede a provider, large enough that a burst of replays drains.
	virtualDeferredProbeWorkers = 8
	// virtualDeferredProbeQueueSize bounds probes waiting for a pool worker.
	// A full queue is backpressure, not an error: the overflow parks in the
	// bounded retry set and a worker re-admits it when it next drains, so the
	// request path never probes inline.
	virtualDeferredProbeQueueSize = 64
	// virtualProbeConvergenceSlack is the extra time the convergence watchdog
	// allows past the probe's own budget before it terminally fails a still-
	// pending deferred probe. It covers the durable evidence write
	// (virtualEvidencePersistBudget) and the outcome/publish handoff, and it is
	// deliberately generous so the watchdog only ever fires for a lifecycle no
	// worker will finish — never for a probe that is merely slow.
	virtualProbeConvergenceSlack = 10 * time.Second
	// virtualProbeWatchdogInterval is the convergence watchdog's scan cadence.
	// The bound itself is what guarantees convergence; the cadence only bounds
	// how late after the bound the failure is observed, so a short interval adds
	// nothing but a finer guarantee.
	virtualProbeWatchdogInterval = 1 * time.Second
	// virtualProbeWatchdogPageSize bounds one scan. A full page is fine: the
	// next tick scans again, and every failed session drops out of the pending
	// set, so the scan converges even when more sessions are pending than fit.
	virtualProbeWatchdogPageSize = 128
)

// Probe lifecycle outcomes recorded on a session via SetVirtualProbeOutcome. They
// are the wire-visible inventory_status values for a deferred probe: pending
// while the enumeration is outstanding, then verified or failed. Empty means no
// deferred probe is outstanding. They alias the playback package's constants,
// the single source of truth.
const (
	probeOutcomePending  = playback.VirtualProbeOutcomePending
	probeOutcomeVerified = playback.VirtualProbeOutcomeVerified
	probeOutcomeFailed   = playback.VirtualProbeOutcomeFailed
	// probeOutcomePartial is the internal, non-wire disposition of a deferred
	// probe whose enumeration was incomplete (scanner.ErrVirtualProbePartial).
	// It is deliberately never written to the session: the session stays
	// pending, so the client keeps its provisional menu until a complete probe
	// verifies it or the convergence watchdog fails it at the bound. Returning
	// it instead of verified is what stops a partial stream table from being
	// published as a final inventory.
	probeOutcomePartial = string(ProbeProvenancePartial)
)

// virtualProbeOutcomeWriter is the session-manager capability that records a
// deferred probe's disposition on the live session conditionally on the binding
// generation. The conditional write is the only outcome path: an unconditional
// write would let a probe that finished after a rotation stamp its verdict onto
// the replacement binding, so there is deliberately no unconditional fallback.
type virtualProbeOutcomeWriter interface {
	SetVirtualProbeOutcomeIfGeneration(sessionID string, generation uint64, outcome string) (bool, error)
}

// virtualProbeInventoryWriter is the session-manager capability that atomically
// records a deferred probe's verified outcome AND replaces the live session's
// carried virtual inventory with the exact tracks the probe committed, fenced on
// the binding generation. It is how verified truth reaches the same session
// without the serve bind swapping in the row's tracks, which would publish a
// selection ordinal space the transport never muxed against. The outcome and the
// inventory move in one write because a poll that observed verified status over
// the unchanged provisional list is the transient this propagation exists to
// close.
type virtualProbeInventoryWriter interface {
	SetVirtualProbeVerifiedInventoryIfGeneration(sessionID string, generation uint64, revision string, audio []models.AudioTrack, subtitles []models.SubtitleTrack, external []models.ExternalSubtitle) (bool, error)
}

// virtualProbeInventoryBindingWriter is the stronger inventory-writer capability
// that fences the verified-inventory commit on the whole candidate binding, not
// just its generation. It exists because SetEffectiveMediaFileID can move the
// session's effective row without advancing virtualSourceGeneration, so a
// generation-only commit could mark the old row's inventory verified onto a
// changed binding. The propagation prefers this form when the manager supplies
// it and falls back to the generation form for a manager that cannot.
type virtualProbeInventoryBindingWriter interface {
	SetVirtualProbeVerifiedInventoryIfBinding(sessionID string, expected playback.VirtualSourceBindingSnapshot, revision string, audio []models.AudioTrack, subtitles []models.SubtitleTrack, external []models.ExternalSubtitle) (bool, error)
}

// virtualCommittedProbeInventory is the exact inventory a deferred probe's
// durable evidence write committed, carried forward from the write itself rather
// than reread from the catalog row. The row is mutable: an in-place adoption
// between the write and a reread can replace its tracks and path with another
// release's while the binding generation is unchanged, so the committed tracks
// and the release identity the probe wrote are the only trustworthy record of
// what the probe verified.
type virtualCommittedProbeInventory struct {
	fileID       int
	candidateURI string
	identity     resolver.PersistedIdentityTiers
	audio        []models.AudioTrack
	subtitles    []models.SubtitleTrack
	external     []models.ExternalSubtitle
}

// committed reports whether the carrier names a real committed catalog row.
func (inv virtualCommittedProbeInventory) committed() bool {
	return inv.fileID > 0
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
	h.startDeferredProbeWatchdog()
}

// probeConvergenceBound is the documented maximum time a cold-start deferred
// probe may remain pending before the convergence watchdog terminally fails it.
// It is the probe's own budget plus a fixed slack for the durable evidence write
// and the outcome/publish handoff. The probe budget is a var so tests can shrink
// the whole bound; a handler-level override replaces it entirely.
func (h *PlaybackHandler) probeConvergenceBound() time.Duration {
	if h != nil && h.probeConvergenceOverride > 0 {
		return h.probeConvergenceOverride
	}
	return virtualBackgroundProbeBudget + virtualProbeConvergenceSlack
}

// probeWatchdogCadence is the effective scan interval, honoring a test override.
func (h *PlaybackHandler) probeWatchdogCadence() time.Duration {
	if h != nil && h.probeWatchdogIntervalOverride > 0 {
		return h.probeWatchdogIntervalOverride
	}
	return virtualProbeWatchdogInterval
}

// startDeferredProbeWatchdog lazily starts the single convergence watchdog. It
// is tied to the service context so it exits at shutdown and never leaks.
func (h *PlaybackHandler) startDeferredProbeWatchdog() {
	if h == nil {
		return
	}
	h.deferredProbeWatchdogOnce.Do(func() {
		go h.runDeferredProbeWatchdog()
	})
}

// runDeferredProbeWatchdog scans for deferred probes that have stayed pending
// past the convergence bound and terminally fails them. It is the guarantee
// that the pending state is bounded: every other lifecycle path can only end a
// probe that a worker actually runs or sheds, so a probe whose worker hangs (a
// provider that ignores its context) or that keeps re-parking behind a
// permanently saturated gate would otherwise stay pending for the session's
// whole life. A failure write is followed by the same terminal-notification
// publish every other failed outcome uses, so the push and the poll agree and a
// client leaves its loading state either way.
func (h *PlaybackHandler) runDeferredProbeWatchdog() {
	base := h.ServiceContext
	if base == nil {
		base = context.Background()
	}
	ticker := time.NewTicker(h.probeWatchdogCadence())
	defer ticker.Stop()
	for {
		select {
		case <-base.Done():
			return
		case <-ticker.C:
			h.failOverdueDeferredProbes(time.Now().Add(-h.probeConvergenceBound()))
		}
	}
}

// failOverdueDeferredProbes fails every session whose deferred probe is still
// pending and was armed at or before cutoff, then parks the terminal
// notification for the dispatcher to deliver. The failure is written as a
// compare-and-swap from pending only, so a probe that verifies between the scan
// and the write is never overwritten, and a rotation that moved the binding is
// left to its own lifecycle. It is exported to the package for the watchdog
// ticker and is directly callable in tests with an explicit cutoff, which keeps
// the bound assertion deterministic instead of wall-clock dependent.
func (h *PlaybackHandler) failOverdueDeferredProbes(cutoff time.Time) {
	if h == nil || h.sessionMgr == nil {
		return
	}
	reader, ok := h.sessionMgr.(interface {
		VirtualProbePendingSessions(time.Time, int) []playback.VirtualProbeTerminalSession
	})
	if !ok {
		return
	}
	failer, ok := h.sessionMgr.(interface {
		FailVirtualProbeIfPendingIfGeneration(string, uint64, time.Time) (bool, error)
	})
	if !ok {
		return
	}
	for _, pending := range reader.VirtualProbePendingSessions(cutoff, virtualProbeWatchdogPageSize) {
		if pending.SessionID == "" || pending.FileID <= 0 {
			continue
		}
		applied, err := failer.FailVirtualProbeIfPendingIfGeneration(pending.SessionID, pending.Generation, cutoff)
		if err != nil || !applied {
			// The session ended, rotated, or reached a terminal outcome between
			// the scan and the write. Nothing is owed: the newer state owns the
			// lifecycle.
			continue
		}
		slog.Warn("deferred virtual probe exceeded the convergence bound; marking inventory failed",
			"component", "api", "session", pending.SessionID, "file_id", pending.FileID,
			"generation", pending.Generation, "bound", h.probeConvergenceBound())
		h.parkDeferredProbePublish(pending.SessionID, pending.FileID, pending.Generation)
	}
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
// It never blocks and never probes on the request path. Admission first captures
// the session's candidate binding — identity and generation in one manager read
// — and rejects the probe as stale when the session no longer names this probe's
// candidate, so a job that rotation already superseded never probes. A live
// binding is marked pending (compare-and-swap on that generation), then the
// probe is handed to the fixed worker pool. When both the queue and the bounded
// retry set are full the probe is shed, but never silently: the session's
// outcome is set to failed and the terminal failure is published off the request
// path, so a client leaves its loading state instead of waiting forever for a
// probe that no worker will run. The request path itself is never asked to probe
// synchronously.
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
	if !h.armDeferredProbePending(ctx, deferred) {
		// Stale job: the session is gone or already bound to a different
		// candidate. Probing would pay a remote ffprobe for a verdict nobody can
		// consume, and the replacement binding owns its own lifecycle, so the
		// job is dropped without an outcome write.
		return
	}
	select {
	case h.deferredProbeQueue <- deferred:
		return
	default:
	}
	// The queue is full. Park the probe in a bounded retry set rather than
	// dropping it: a worker re-admits it when it next drains, so a later
	// completion finishes the enumeration while the request path is never asked
	// to probe synchronously. Beyond the retry bound the probe is shed
	// terminally: the session outcome is marked failed and the failure is
	// published, which ends the client's loading state.
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

// virtualProbePendingArmer is the session-manager capability that transitions a
// binding generation's probe outcome to pending only from empty. It is the
// exclusive ownership fence for deferred-probe admission: a duplicate start that
// adopted a winner's session must not restart a lifecycle that is already armed
// or terminal.
type virtualProbePendingArmer interface {
	ArmVirtualProbePendingIfGeneration(sessionID string, generation uint64) (bool, error)
}

// armDeferredProbePending captures the session's candidate binding and marks the
// deferred inventory pending on it, but only when no lifecycle is already armed
// or terminal for that binding generation. The binding identity and generation
// are one manager read, so a rotation cannot interleave between them: the
// generation is then retained on the job for every later fence, and the pending
// write CASes on it so a rotation landing between this read and the write drops
// the pending mark instead of showing it on the replacement binding. It reports
// false — the job is stale and must not be admitted — when the session is gone,
// already bound to a different candidate, or already carries a probe outcome for
// this binding (armed or terminal). That last case is the duplicate-start fence:
// a concurrent winner's terminal save returns its plan to a loser that then
// re-enters the start path, and the loser must not re-probe and re-arm the
// winner's completed lifecycle. A manager without the binding reader (a minimal
// test manager) admits unfenced, matching the pre-existing best-effort behavior
// on those managers.
func (h *PlaybackHandler) armDeferredProbePending(ctx context.Context, deferred *virtualDeferredProbeV3) bool {
	if h == nil || deferred == nil || h.sessionMgr == nil || deferred.sessionID == "" {
		return true
	}
	reader, ok := h.sessionMgr.(interface {
		VirtualSourceBinding(sessionID string) (playback.VirtualSourceBindingSnapshot, error)
	})
	if !ok {
		return true
	}
	binding, err := reader.VirtualSourceBinding(deferred.sessionID)
	if err != nil {
		slog.DebugContext(ctx, "deferred virtual post-commit probe dropped: session binding unreadable at admission",
			"component", "api", "session", deferred.sessionID, "candidate_uri", deferred.cand.URI, "error", err)
		return false
	}
	if !sameVirtualCandidate(binding.VirtualURI, deferred.cand.URI) {
		slog.DebugContext(ctx, "deferred virtual post-commit probe dropped: candidate binding moved before admission",
			"component", "api", "session", deferred.sessionID, "candidate_uri", deferred.cand.URI, "bound_uri", binding.VirtualURI)
		return false
	}
	deferred.bindingGeneration = binding.Generation
	deferred.binding = binding
	if armer, ok := h.sessionMgr.(virtualProbePendingArmer); ok {
		// Exclusive ownership: arm only from empty, so a duplicate start cannot
		// restart a lifecycle the winner already armed or completed. A manager
		// exposing only the outcome CAS keeps the pre-existing best-effort
		// behavior below.
		applied, armErr := armer.ArmVirtualProbePendingIfGeneration(deferred.sessionID, binding.Generation)
		if armErr != nil {
			slog.DebugContext(ctx, "deferred virtual post-commit probe dropped: pending mark not applied",
				"component", "api", "session", deferred.sessionID, "candidate_uri", deferred.cand.URI, "error", armErr)
			return false
		}
		if !applied {
			slog.DebugContext(ctx, "deferred virtual post-commit probe dropped: lifecycle already armed or terminal for the binding",
				"component", "api", "session", deferred.sessionID, "candidate_uri", deferred.cand.URI, "generation", binding.Generation)
			return false
		}
		return true
	}
	h.setVirtualProbeOutcomeIfGeneration(deferred.sessionID, binding.Generation, probeOutcomePending)
	return true
}

// setVirtualProbeOutcomeIfGeneration records an outcome only while the session's
// candidate binding is still the generation the probe was admitted against
// (compare-and-swap on the binding generation). It is the fence for a rotation
// landing between probe scheduling and outcome storage: the old probe's verdict
// is dropped instead of overwriting the new binding's outcome. It reports
// whether the write landed, so the caller publishes a terminal notification only
// for the binding that actually carries the verdict. A manager without the CAS
// writer (a minimal test double) reports not-applied — there is no unconditional
// write, because that is exactly the unfenced stamp the CAS exists to prevent.
func (h *PlaybackHandler) setVirtualProbeOutcomeIfGeneration(sessionID string, generation uint64, outcome string) bool {
	if h == nil || sessionID == "" || h.sessionMgr == nil {
		return false
	}
	writer, ok := h.sessionMgr.(virtualProbeOutcomeWriter)
	if !ok {
		slog.Debug("virtual probe outcome not recorded: session manager cannot fence on the binding generation",
			"component", "api", "session", sessionID, "outcome", outcome)
		return false
	}
	applied, err := writer.SetVirtualProbeOutcomeIfGeneration(sessionID, generation, outcome)
	if err != nil {
		if !errors.Is(err, playback.ErrSessionNotFound) {
			slog.Warn("failed to record virtual probe outcome", "component", "api", "session", sessionID, "outcome", outcome, "error", err)
		}
		return false
	}
	if !applied {
		slog.Info("virtual probe outcome dropped: candidate binding moved before outcome storage",
			"component", "api", "session", sessionID, "outcome", outcome, "generation", generation)
	}
	return applied
}

// shedDeferredVirtualProbe terminally fails a probe that could not run: the
// session outcome is marked failed and the failure published off the request
// path, so a client watching either the push or the poll leaves its loading
// state. The fence on the retained binding generation ensures the shed probe's
// failure is not written onto a replacement binding that never had this probe
// outstanding; when the CAS rejects the outcome the binding moved, so the
// failure is not published either — the new binding owns its own lifecycle.
func (h *PlaybackHandler) shedDeferredVirtualProbe(deferred *virtualDeferredProbeV3) {
	if h == nil || deferred == nil {
		return
	}
	if !h.setVirtualProbeOutcomeIfGeneration(deferred.sessionID, deferred.bindingGeneration, probeOutcomeFailed) {
		return
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
// The binding generation captured at admission is the fence throughout — the
// worker never recaptures one, so the pending mark, the probe, and the terminal
// outcome all describe exactly the binding the admission check validated.
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
	if !h.deferredProbeBindingCurrent(deferred) {
		gate.release()
		// The binding moved while the probe waited for a slot; the replacement
		// binding owns its own lifecycle, so shed without an outcome write or a
		// publish instead of paying a remote ffprobe for a superseded candidate.
		slog.Debug("deferred virtual post-commit probe shed before probing: candidate binding moved while parked",
			"component", "api", "session", deferred.sessionID, "candidate_uri", deferred.cand.URI)
		h.signalDeferredProbeRetry()
		return
	}
	bgCtx, bgCancel := h.virtualDetachedContext(h.ServiceContext, virtualBackgroundProbeBudget)
	defer bgCancel()
	outcome, committed := h.probeVirtualSourceAndPersistWith(bgCtx, deferred.stickyKey, deferred.file, deferred.streamURL, *deferred.probeTransient, deferred.cand, deferred.candidate.tiers(), deferred.expectedRuntimeMinutes, deferred.ownerID, true, h.deferredProbeBindingIntact(deferred))
	// The probe is done with the shared gate: release this worker's slot before
	// the terminal outcome write and publish so a saturated gate is not held by
	// catalog/realtime fan-out that carries its own budget. Parked terminal
	// notifications need no drain call here — the dispatcher owns wakeups and
	// re-checks its map on its own signal and backstop ticker.
	gate.release()
	// The durable write committed verified evidence on committed.fileID; run the
	// shared cold-start default-audio reconciliation against that owner row (a
	// rotation can move it off deferred.file), with the gate released so its own
	// catalog/session sweep is not charged to this worker's slot. It runs BEFORE
	// the overlay is updated so it compares the plan-time selection against the
	// verified inventory, exactly as the direct and queued evidence paths do.
	// The probe's binding fence already refuses a superseded verdict, and the
	// reconcile sweep re-checks the committed row before issuing any correction.
	if outcome == probeOutcomeVerified && committed.committed() {
		h.reconcileVerifiedDefaultAudio(context.Background(), committed.fileID)
	}
	switch outcome {
	case probeOutcomeVerified:
		// Propagate the committed inventory into the session's carried overlay
		// and record the verified outcome in the SAME generation-fenced write,
		// so a poll can never observe verified status over the unchanged
		// provisional menu. Only a landed propagation is published as verified;
		// a binding that moved owns its own lifecycle, and a committed inventory
		// that provably belongs to another release fails the session closed
		// rather than painting the provisional menu as verified.
		//
		// This deliberately does not go through the generic terminal write: the
		// verified outcome is inseparable from the inventory that backs it, and
		// a recoverable propagation read is retried on a fresh bounded context
		// inside propagateVerifiedProbeInventory.
		switch h.propagateVerifiedProbeInventory(deferred, committed) {
		case probeInventoryApplied:
			h.publishDeferredProbeInventory(deferred)
		case probeInventoryRefused:
			if h.setVirtualProbeOutcomeIfGeneration(deferred.sessionID, deferred.bindingGeneration, probeOutcomeFailed) {
				h.publishDeferredProbeInventory(deferred)
			}
		case probeInventorySuperseded:
			// The binding rotated, the session carries no overlay capability, or
			// a transient identity read never cleared; nothing is owed to this
			// generation and no verified status is published.
		}
	case probeOutcomeFailed:
		// probeVirtualSourceAndPersist has already attempted the durable
		// evidence write (it either landed or terminally failed), so the session
		// outcome can now record that the enumeration terminally failed. The poll
		// and the push then agree and a client can leave its loading state. The
		// terminal write CASes on the admitted generation; a rotation that landed
		// while the probe ran makes the CAS reject the verdict, and then nothing
		// is published — the replacement binding owns its own lifecycle.
		if h.setVirtualProbeOutcomeIfGeneration(deferred.sessionID, deferred.bindingGeneration, outcome) {
			h.publishDeferredProbeInventory(deferred)
		}
	case probeOutcomePartial:
		// The enumeration was structurally incomplete, so the probe returned
		// no verdict about the candidate's bytes. The session stays armed
		// pending (non-verified): a partial inventory is never written to the
		// session and never published as verified. The convergence watchdog
		// fails the still-pending lifecycle at the bound if no complete probe
		// lands first.
		slog.Warn("deferred virtual probe enumeration incomplete; inventory stays pending",
			"component", "api", "session", deferred.sessionID, "candidate_uri", deferred.cand.URI)
	default:
		// The binding moved under the probe; the new binding owns its own
		// lifecycle, so no outcome is written.
	}
	// A parked probe may be waiting on the retry set; wake a worker to drain it.
	h.signalDeferredProbeRetry()
}

// probeInventoryDisposition is the outcome of propagating a deferred probe's
// committed inventory into the live session overlay.
type probeInventoryDisposition int

const (
	// probeInventoryApplied means the verified outcome and the committed
	// inventory landed on the session in one fenced write.
	probeInventoryApplied probeInventoryDisposition = iota
	// probeInventorySuperseded means the binding rotated (or the session cannot
	// carry an overlay, or a transient identity read never cleared); nothing is
	// owed to this generation and no verified status is published.
	probeInventorySuperseded
	// probeInventoryRefused means the committed inventory provably belongs to a
	// different release than the session binding, or an invalid binding id
	// carries no identity to confirm: the session fails closed instead of
	// serving the committed inventory as verified.
	probeInventoryRefused
)

// probePropagationAttempts bounds the fresh-context retries of a recoverable
// propagation decision. The retried step is a catalog read of the session's
// bound row used to prove release continuity; the committed inventory itself is
// already in hand, so a transient read failure is retried, never converted into
// a verified publish.
const probePropagationAttempts = 2

// propagateVerifiedProbeInventory commits the exact inventory a deferred probe
// wrote into the live session's carried overlay AND records the verified outcome
// in one generation-fenced write. It never rereads the committed row: the
// carried inventory is the record of what was written, so an in-place adoption
// between the write and this propagation cannot substitute another release's
// tracks. It returns whether the write landed, was refused as a different
// release, or was superseded by a rotation; a recoverable catalog read while
// proving the bound release is retried on a fresh bounded context.
//
// Transport coherence: replacing the carried list does not rebuild a running
// mux, and none is owed. The transport's audio selector is an audio-only
// ordinal (0:a:N) derived from container stream indices, not array position, so
// an array reorder alone leaves the committed stream's ordinal unchanged and the
// running mux keeps playing the same stream. When a removal shifts that ordinal,
// the shared default-audio reconcile — which runs before this write against the
// plan-time selection, as the direct and queued evidence paths do — issues the
// correction that points 0:a:N back at the committed track. An explicit viewer
// selection is preserved by its track identity and is never overridden. This
// write therefore deliberately leaves the session's committed array position
// untouched; renumbering it here would change the ordinal space the running mux
// was started against.
func (h *PlaybackHandler) propagateVerifiedProbeInventory(deferred *virtualDeferredProbeV3, committed virtualCommittedProbeInventory) probeInventoryDisposition {
	if h == nil || deferred == nil || deferred.sessionID == "" || !committed.committed() {
		return probeInventorySuperseded
	}
	writer, ok := h.sessionMgr.(virtualProbeInventoryWriter)
	if !ok {
		return probeInventorySuperseded
	}
	revision := verifiedProbeInventoryRevision(committed)
	for attempt := 0; attempt < probePropagationAttempts; attempt++ {
		match, retryable := h.committedProbeMatchesSessionBinding(deferred, committed)
		if !match {
			if retryable && attempt < probePropagationAttempts-1 {
				// A transient catalog read of the bound row: retry on a fresh
				// bounded context instead of treating the failure as a refusal.
				continue
			}
			if retryable {
				// Still unreadable after the retry bound: never publish verified
				// over an inventory we could not prove belongs to this binding.
				return probeInventorySuperseded
			}
			return probeInventoryRefused
		}
		// Prefer the full-binding CAS when the manager supplies one: it fences
		// the effective row id too, so a row-id-only mutation between the
		// release-continuity check and this write is rejected rather than
		// committing the validated row's inventory onto the changed binding.
		// The generation-only writer remains the fallback for managers that
		// carry no binding snapshot.
		var applied bool
		var err error
		if bw, ok := h.sessionMgr.(virtualProbeInventoryBindingWriter); ok {
			applied, err = bw.SetVirtualProbeVerifiedInventoryIfBinding(
				deferred.sessionID, deferred.binding, revision,
				committed.audio, committed.subtitles, committed.external,
			)
		} else {
			applied, err = writer.SetVirtualProbeVerifiedInventoryIfGeneration(
				deferred.sessionID, deferred.bindingGeneration, revision,
				committed.audio, committed.subtitles, committed.external,
			)
		}
		if err != nil {
			if errors.Is(err, playback.ErrSessionNotFound) {
				return probeInventorySuperseded
			}
			if attempt < probePropagationAttempts-1 {
				continue
			}
			slog.Warn("verified probe inventory not propagated",
				"component", "api", "session", deferred.sessionID, "file_id", committed.fileID, "error", err)
			return probeInventorySuperseded
		}
		if !applied {
			slog.Debug("verified probe inventory propagation dropped: candidate binding moved",
				"component", "api", "session", deferred.sessionID, "file_id", committed.fileID, "generation", deferred.bindingGeneration)
			return probeInventorySuperseded
		}
		return probeInventoryApplied
	}
	return probeInventorySuperseded
}

// committedProbeMatchesSessionBinding reports whether the inventory a deferred
// probe committed may replace the live session's carried overlay. It fences on
// the session's current candidate binding and on the committed release identity
// the probe itself wrote, and it never rereads the committed row: the carried
// inventory is the record of what was written, so an in-place adoption between
// the write and this check cannot supply another release's tracks.
//
// The candidate URI is asserted explicitly even though the generation CAS
// already proves it, because SetEffectiveMediaFileID moves the session's
// effective row id without advancing the generation, so a row-id-only mutation
// must not let a superseded candidate's inventory through. An unknown effective
// row id (<= 0) carries no identity to confirm and is rejected.
//
// retryable reports that the decision could not be made because the catalog read
// of the session's bound row failed; the caller retries it on a fresh bounded
// context rather than treating a transient read failure as a refusal.
func (h *PlaybackHandler) committedProbeMatchesSessionBinding(deferred *virtualDeferredProbeV3, committed virtualCommittedProbeInventory) (match bool, retryable bool) {
	if h == nil || h.sessionMgr == nil || deferred == nil || !committed.committed() {
		return false, false
	}
	live, err := h.sessionMgr.GetSession(deferred.sessionID)
	if err != nil || live == nil {
		return false, false
	}
	if !sameVirtualCandidate(strings.TrimSpace(live.VirtualSourceURI), deferred.cand.URI) {
		return false, false
	}
	if live.MediaFileID <= 0 {
		// An unknown effective row carries no identity to confirm; never trust
		// it, whatever the committed row id names.
		return false, false
	}
	if live.MediaFileID == committed.fileID {
		// The committed row is the session's own effective row: the probe
		// committed onto exactly the row the session serves. The release identity
		// is carried from that same commit, so no cross-row question remains.
		return true, false
	}
	if h.fileResolver == nil {
		return false, false
	}
	ctx, cancel := h.virtualDetachedContext(h.ServiceContext, virtualEvidencePersistBudget)
	defer cancel()
	bound, err := h.fileResolver.GetByID(ctx, live.MediaFileID)
	if err != nil {
		return false, true
	}
	if bound == nil {
		return false, false
	}
	return boundIdentitySharesRelease(bound, committed.identity), false
}

// boundIdentitySharesRelease reports whether the bound row's durable provider
// identity and a committed release identity prove one release, using the
// asymmetric SharedTier with the bound row authoritative: a stronger stored tier
// the committed identity cannot corroborate is a veto, and an identity whose
// tiers are all empty never matches, so an unprovable pairing fails closed.
func boundIdentitySharesRelease(bound *models.MediaFile, committed resolver.PersistedIdentityTiers) bool {
	if bound == nil {
		return false
	}
	boundTiers := resolver.NewPersistedIdentityTiers(bound.ProviderVideoHash, bound.ProviderGUID, bound.ProviderReleaseName, bound.ProviderReleaseSize)
	_, shared := boundTiers.SharedTier(committed)
	return shared
}

// verifiedProbeInventoryRevision names the verified inventory generation a
// committed deferred probe just produced, derived from the committed tracks so
// the served revision moves on a genuine new inventory and the client's
// conditional poll stops returning 304. It is deliberately keyed on the carried
// inventory, not a reread of the row's stamp: the tracks are what the session
// now serves, and the value is opaque exactly like the plan-time revision it
// replaces.
func verifiedProbeInventoryRevision(committed virtualCommittedProbeInventory) string {
	return reconcileGenerationV3(&models.MediaFile{AudioTracks: committed.audio})
}

// deferredProbeBindingCurrent reports whether the session is still bound to the
// exact candidate and generation this probe was admitted against. It is the one
// fence predicate for the whole lifecycle — the cheap pre-probe re-check and the
// fence passed into the probe — so a rotation is judged the same way before and
// during the remote enumeration. The generation increments monotonically on
// every binding move (see setVirtualSourceLocked), so an A→B→A rotation still
// yields a different generation and is caught here. A manager without the
// binding reader (a minimal test double) reports current so the probe proceeds,
// matching the pre-existing best-effort behavior on those managers.
func (h *PlaybackHandler) deferredProbeBindingCurrent(deferred *virtualDeferredProbeV3) bool {
	if h == nil || deferred == nil || deferred.sessionID == "" || h.sessionMgr == nil {
		return true
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

// deferredProbeBindingIntact is the fence the probe worker passes to
// probeVirtualSourceAndPersist, evaluated after the remote enumeration but
// before the verdict marks the damper or persists evidence. It checks the
// candidate and the retained generation together — a URI-only check would let an
// A→B→A rotation during probing carry superseded work into the persistence
// write, because the second A binding re-uses the first's candidate URI. Without
// the generation a probe that completes after a rotation could write its
// evidence onto a row the session no longer serves.
func (h *PlaybackHandler) deferredProbeBindingIntact(deferred *virtualDeferredProbeV3) func() bool {
	return func() bool {
		return h.deferredProbeBindingCurrent(deferred)
	}
}

// virtualDeferredPublishPendingCap bounds the pending terminal-notification
// set. Saturation is the only time entries accumulate, and each one is a session
// waiting to leave its loading state, so the cap is sized to cover a burst of
// simultaneous cold starts; past it the overflow is recorded in the coalesced
// rescan flag rather than dropped, so the set stays bounded without losing
// intent.
const virtualDeferredPublishPendingCap = 64

// deferredPublishBatchCap bounds how many parked notifications one dispatcher
// wake publishes before re-checking the map and signal, so one wake cannot run
// through an unbounded burst without yielding to a shutdown or a fresh signal.
const deferredPublishBatchCap = 8

// deferredPublishBackstop is the dispatcher's slow ticker. Signal delivery is a
// hint, not the record — the ticker re-checks the pending map even if every
// signal was coalesced away, so no release-site cooperation is ever required.
const deferredPublishBackstop = time.Second

// Retry backoff for a notification whose publish attempt was retryable (a
// transient catalog/plan-store read or a realtime write that did not reach the
// client). The delay grows with the attempt count and is capped, so a flapping
// dependency is retried without a hot loop while a push-only client still leaves
// loading within a bounded time.
const (
	deferredPublishRetryBaseDelay = 250 * time.Millisecond
	deferredPublishRetryMaxDelay  = 10 * time.Second
)

// deferredPublishRetryDelay is the bounded backoff before a retryable
// notification is dispatched again. attempts is the number of retryable attempts
// already made (>= 1 when called).
func deferredPublishRetryDelay(attempts int) time.Duration {
	if attempts <= 1 {
		return deferredPublishRetryBaseDelay
	}
	delay := deferredPublishRetryBaseDelay
	for i := 1; i < attempts; i++ {
		delay *= 2
		if delay >= deferredPublishRetryMaxDelay {
			return deferredPublishRetryMaxDelay
		}
	}
	return delay
}

// publishDeferredProbeInventory pushes the session's committed deferred-probe
// inventory to live realtime sessions, so a client watching the push path leaves
// its loading state. It must only run after the session outcome has been stored,
// so the pushed status (verified with the readable menu, or failed) is exactly
// what a subsequent inventory poll reports.
//
// The fan-out reads the catalog and pushes to every live session on the file, so
// it never runs on the caller: it takes one aggregate detached-gate slot and
// runs under the service lifecycle with the publish budget, keeping the start
// response and the probe pool free of catalog/realtime latency. A saturated gate
// does not drop the push — the session's terminal state is parked and the
// dispatcher is signaled, so overflow backpressure delays the notification
// instead of discarding it.
func (h *PlaybackHandler) publishDeferredProbeInventory(deferred *virtualDeferredProbeV3) {
	if h == nil || deferred == nil || deferred.file == nil {
		return
	}
	// Mark the session in-flight BEFORE touching the gate, so the window where
	// the session is neither parked nor in-flight (and so visible to a
	// concurrent overflow rescan as outstanding) never exists.
	if !h.markDeferredPublishInFlight(deferred.sessionID) {
		// Another worker is already publishing this session's notification. Do
		// NOT drop this (possibly newer) terminal outcome: park it so the
		// dispatcher re-delivers after the in-flight worker acks. The parked
		// entry is keyed by session and coalesces to the latest intent, and the
		// ack is forward-only, so the in-flight worker's ack cannot cover this
		// newer generation — parking is what keeps it from being lost.
		h.parkDeferredProbePublish(deferred.sessionID, deferred.file.ID, deferred.bindingGeneration)
		return
	}
	if !h.detachedGate().tryAcquire() {
		// No slot: unmark and park so the dispatcher owns the retry. The parked
		// entry is the record of the outstanding notification; the in-flight
		// mark must not survive, or the batch that later pops the parked entry
		// would refuse to dispatch it as a duplicate.
		h.deferredPublishMu.Lock()
		delete(h.deferredPublishInFlight, deferred.sessionID)
		h.deferredPublishMu.Unlock()
		h.parkDeferredProbePublish(deferred.sessionID, deferred.file.ID, deferred.bindingGeneration)
		return
	}
	h.runDeferredPublishWorker(deferred.sessionID, deferredPublishIntent{fileID: deferred.file.ID, generation: deferred.bindingGeneration})
}

// deferredPublishIntent is one parked terminal notification: the file whose
// inventory the push re-reads and the binding generation the notification
// carries. The generation flows park → dispatcher → publish worker, so the ack
// after publish (MarkVirtualProbeNotified) marks exactly that binding's outcome
// delivered and never an older or newer one. notBefore, when non-zero, holds a
// retryable notification until its bounded backoff elapses; attempts counts the
// retryable publish attempts so the backoff grows without a hot loop.
type deferredPublishIntent struct {
	fileID     int
	generation uint64
	attempts   int
	notBefore  time.Time
}

// parkDeferredProbePublish records that a session's terminal inventory still
// needs its push once a gate slot frees, then wakes the dispatcher. Keyed by
// session so a later terminal state coalesces onto the earlier one (the push
// re-reads the live session, so only the latest matters). The map is the record;
// the signal is only a hint, sent nonblocking so the admission path never waits.
// When the set is already at its bound the intent is NOT dropped — the coalesced
// rescan flag is set so the dispatcher re-parks the overflow from the session
// manager once the map drains, keeping memory bounded without losing a session.
//
// Coalescing is BY GENERATION, applied under deferredPublishMu: a park replaces
// the parked entry only when the new generation is at least the parked one, so
// a delayed older worker can never overwrite a newer generation's parked
// intent. A park whose generation the session's notified watermark already
// covers is dropped outright — that terminal outcome was already delivered, so
// parking it would schedule a duplicate push. The watermark lives under the
// session lock; the lookup happens while deferredPublishMu is held (the
// documented outer lock — see deferredPublishMu in playback.go), so the check
// and the map write are one atomic step w.r.t. the ack path.
func (h *PlaybackHandler) parkDeferredProbePublish(sessionID string, fileID int, generation uint64) {
	if h == nil || sessionID == "" {
		return
	}
	h.deferredPublishMu.Lock()
	if h.deferredPublishPending == nil {
		h.deferredPublishPending = make(map[string]deferredPublishIntent)
	}
	if h.deferredPublishInFlight == nil {
		h.deferredPublishInFlight = make(map[string]struct{})
	}
	if h.deferredProbeNotifiedCovers(sessionID, generation) {
		// Already delivered: the watermark advanced past this generation while
		// the intent was in flight to the park, so parking it would schedule a
		// duplicate push of an outcome the client already has.
		h.deferredPublishMu.Unlock()
		return
	}
	parked, exists := h.deferredPublishPending[sessionID]
	switch {
	case exists && generation < parked.generation:
		// Stale park: a delayed older worker must not replace the newer
		// generation's parked intent.
	default:
		if !exists && len(h.deferredPublishPending) >= virtualDeferredPublishPendingCap {
			h.deferredPublishRescan = true
		} else {
			h.deferredPublishPending[sessionID] = deferredPublishIntent{fileID: fileID, generation: generation}
		}
	}
	h.deferredPublishMu.Unlock()

	h.signalDeferredPublish()
}

// deferredProbeNotifiedCovers reports whether the session's notified watermark
// already covers generation — i.e. that terminal outcome was already pushed and
// acked. It reads the watermark under deferredPublishMu (the documented outer
// lock), which is what makes the park-time and selection-time checks atomic
// with the ack path: the worker's ack (session lock) always precedes its
// in-flight clear (deferredPublishMu), so a generation observed covered here can
// never belong to a session still owed a push. A manager without the watermark
// reader (a minimal test double) reports not-covered, preserving the
// pre-existing best-effort behavior on those managers.
func (h *PlaybackHandler) deferredProbeNotifiedCovers(sessionID string, generation uint64) bool {
	if h == nil || sessionID == "" || h.sessionMgr == nil {
		return false
	}
	reader, ok := h.sessionMgr.(interface {
		VirtualProbeNotifiedGeneration(sessionID string) (uint64, error)
	})
	if !ok {
		return false
	}
	notified, err := reader.VirtualProbeNotifiedGeneration(sessionID)
	if err != nil {
		return false
	}
	return notified >= generation
}

// signalDeferredPublish wakes the dispatcher. The send is nonblocking and the
// channel is buffered-1: a signal already queued is enough, because the
// dispatcher always re-reads the map after any wake regardless of how many
// signals it coalesced.
func (h *PlaybackHandler) signalDeferredPublish() {
	h.startDeferredPublishDispatcher()
	select {
	case h.deferredPublishSignal <- struct{}{}:
	default:
	}
}

// startDeferredPublishDispatcher lazily starts the single terminal-notification
// dispatcher, tied to the service context so it exits at shutdown and never
// leaks.
func (h *PlaybackHandler) startDeferredPublishDispatcher() {
	if h == nil {
		return
	}
	h.deferredPublishOnce.Do(func() {
		if h.deferredPublishSignal == nil {
			h.deferredPublishSignal = make(chan struct{}, 1)
		}
		if h.deferredPublishDone == nil {
			h.deferredPublishDone = make(chan struct{}, 1)
		}
		go h.deferredPublishDispatcher()
	})
}

// deferredPublishDispatcher is the one goroutine that drains parked terminal
// notifications. It owns all wakeups: there is no release-site drain call, so no
// other detachedGate user's release is required to make progress. It wakes on
// the signal or the backstop ticker, then drains batches until none remain —
// each batch acquires a gate slot under a bounded context, pops a bounded number
// of parked sessions (spawning the existing gated publish worker for each), and
// releases the slot, so a backlog unwinds in bounded steps without pinning a
// slot. The map is always the record — after any wake and after every batch the
// loop re-reads it, so a signal lost to coalescing can never strand a parked
// notification. It sleeps when a batch reports nothing dispatchable — an empty
// map, only in-flight or retry-backoff entries, or a saturated gate — and exits
// when the service context ends; a parked retryable entry is woken by the
// backstop ticker once its delay elapses.
//
// Delivery is acknowledged per binding generation: a publish worker acks the
// generation it carried once its attempt completes, and the overflow rescan only
// re-parks sessions whose terminal outcome is newer than the delivered
// watermark. The ack is forward-only, so a stale worker's late ack — or an ack
// for a binding that was superseded before its push landed — never clears a
// newer binding's outstanding intent; only an actual publish attempt on the
// current binding does. When a batch selects a parked intent it deletes it from
// the map and then claims the gate slot; only after the claim succeeds does it
// mark the session in-flight (deferredPublishInFlight), so the rescan — which
// would otherwise still see it as outstanding — does not re-park it and
// schedule a duplicate push. A failed claim instead reparkDeferredPublishSelected-
// Locked returns the intent to the map under the pending bound. The worker
// clears the marker after its ack and signals deferredPublishDone so the
// dispatcher re-checks immediately instead of spinning on undispatchable
// entries or waiting a full tick.
func (h *PlaybackHandler) deferredPublishDispatcher() {
	base := h.ServiceContext
	if base == nil {
		base = context.Background()
	}
	ticker := time.NewTicker(deferredPublishBackstop)
	defer ticker.Stop()
	for {
		// Drain every batch currently owed before sleeping again: a backlog left
		// by a full gate must not wait a full tick per batch.
		for h.drainDeferredPublishBatch(base) {
			select {
			case <-base.Done():
				return
			default:
			}
		}
		select {
		case <-base.Done():
			return
		case <-h.deferredPublishSignal:
		case <-h.deferredPublishDone:
			// A publish worker cleared its in-flight marker; entries that were
			// undispatchable (all in-flight) on the last pass may now dispatch.
		case <-ticker.C:
		}
	}
}

// drainDeferredPublishBatch publishes up to deferredPublishBatchCap parked
// sessions, claiming one gate slot per publication and handing that slot
// straight to the publish worker. It reports whether more work may remain
// (a full batch was dispatched) so the dispatcher keeps draining a backlog, and
// reports false when nothing is dispatchable — the map is empty, every parked
// entry is mid-publish (sleep until a worker's completion signal or the ticker;
// never spin batches against the in-flight set), an entry is waiting out its
// retry backoff, or the gate is saturated (backpressure — sleep and let the next
// signal or ticker retry rather than hammering acquire). A false return is
// always a sleep decision, never a "rescan is owed" deferral: the rescan runs at
// the top of the next batch and only needs the map to have room, and a
// retryable entry held in backoff still reserves its slot, so returning "more
// work" for an owed rescan would spin the inner loop with nothing to dispatch.
//
// Selection and the slot claim are separate steps with distinct lock
// discipline. Selection (selectDeferredPublishCandidate) validates each
// candidate under one deferredPublishMu section — not in-flight, watermark
// does not already cover the intent's generation, not still in retry backoff —
// and DELETES the winner from the map before returning it, WITHOUT an in-flight
// mark. The claim (takeDeferredPublishSlot) then takes the slot with a
// non-blocking tryAcquire outside the lock and, under the lock section that
// re-validates the intent, marks the session in-flight. The session is
// therefore not parked throughout the claim: between the selection delete and
// the claim's in-flight mark it is briefly neither parked nor in-flight. That
// window is closed by the claim's re-validation — a concurrent park or a
// covering ack makes the selected copy stale, and a same-generation re-park
// still publishes exactly once because the worker acks the generation that
// re-parked entry also carries — and by the failed-acquisition repark
// (reparkDeferredPublishSelectedLocked), which returns the selected intent to
// the map under the pending cap. A saturated gate does not leave the entry
// parked the whole time; it is the repark that puts it back. The watermark read
// calls into the session manager while the mutex is held, which is the
// documented lock order; the worker's ack (session lock) always precedes its
// in-flight clear (deferredPublishMu), so a generation observed covered at
// selection or claim can never belong to a session whose publish is still
// outstanding. Undispatchable entries stay parked; already-delivered ones are
// dropped.
func (h *PlaybackHandler) drainDeferredPublishBatch(base context.Context) bool {
	// Recover overflow intent before deciding there is nothing to do: a map that
	// drained empty with the rescan flag set still owes the overflowed sessions.
	h.rescanDeferredPublishOverflow()

	for popped := 0; popped < deferredPublishBatchCap; popped++ {
		sessionID, intent, found := h.selectDeferredPublishCandidate()
		if !found {
			// Nothing dispatchable this pass. This is the dispatcher's sleep
			// decision, and it must be a sleep even when a rescan is still owed:
			// the rescan runs at the top of the next batch and only needs the
			// map to have room, and a retryable entry held in backoff keeps a
			// slot reserved, so returning "more work" here would spin the inner
			// drain loop with no dispatchable entry to make progress. The
			// backstop ticker (or a retry worker's completion signal) resumes
			// once the backoff elapses, at which point the rescan can recover
			// any overflow. The full-batch return below is the only
			// "keep draining" signal, and it always follows real dispatch.
			return false
		}
		claimed, stale := h.takeDeferredPublishSlot(sessionID, intent)
		if !claimed {
			// The gate is saturated: return the selected intent to the parked
			// map so the notification is not lost, with the same coalescing
			// rule and pending-map bound the other re-park paths use. Sleep
			// afterwards — the next signal or ticker retries; a saturated gate
			// is backpressure, not an error.
			h.deferredPublishMu.Lock()
			h.reparkDeferredPublishSelectedLocked(sessionID, intent)
			h.deferredPublishMu.Unlock()
			return false
		}
		if stale {
			// A covering ack or a newer coalesced park landed between
			// selection and the slot claim; the slot was handed back inside
			// takeDeferredPublishSlot. Re-select rather than publish stale.
			continue
		}
		h.runDeferredPublishWorker(sessionID, intent)
	}
	// A full batch published: the map may still hold more, so keep draining.
	return true
}

// selectDeferredPublishCandidate atomically picks one dispatchable parked
// entry: under a single deferredPublishMu section it validates each candidate
// against the dispatchability predicate, DELETES the winner from the map, and
// returns it. It reports found=false when no parked entry is dispatchable.
// Entries whose generation the watermark already covers are dropped, not
// returned: their push already landed, so dispatching them would duplicate it.
// An entry whose retryable backoff has not yet elapsed is skipped so the batch
// does not spin on it; the dispatcher's backstop ticker wakes it once the delay
// has passed. The in-flight mark is NOT taken here — it moves into
// takeDeferredPublishSlot. Deleting before the gate claim means the selected
// session is briefly neither parked nor in-flight; if the claim fails the
// repark helper puts it back under the pending bound, and if the claim succeeds
// the re-validation there drops any copy a concurrent park made stale.
func (h *PlaybackHandler) selectDeferredPublishCandidate() (sessionID string, intent deferredPublishIntent, found bool) {
	h.deferredPublishMu.Lock()
	defer h.deferredPublishMu.Unlock()
	now := time.Now()
	for id, candidate := range h.deferredPublishPending {
		if _, inFlight := h.deferredPublishInFlight[id]; inFlight {
			// Mid-publish: skip at selection time so the batch never burns a
			// gate-slot acquisition on an entry it cannot dispatch.
			continue
		}
		if h.deferredProbeNotifiedCovers(id, candidate.generation) {
			// Acks that landed while the entry sat parked already delivered
			// this generation; drop it instead of publishing a duplicate.
			delete(h.deferredPublishPending, id)
			continue
		}
		if !candidate.notBefore.IsZero() && now.Before(candidate.notBefore) {
			// A retryable attempt is still in backoff. Leave it parked and
			// wait for the backstop ticker rather than spinning a batch on it.
			continue
		}
		delete(h.deferredPublishPending, id)
		return id, candidate, true
	}
	return "", deferredPublishIntent{}, false
}

// takeDeferredPublishSlot claims a gate slot for a selected candidate and, in
// the same lock section, transitions the session to in-flight. The slot is
// claimed BEFORE the mutex is taken — tryAcquire is safe outside any lock —
// so the lock section only runs with a slot already in hand. The selected
// entry was DELETED from the pending map at selection time, so a failed claim
// leaves the session neither parked nor in-flight until the caller's repark
// helper returns it; a successful claim restores the accounted-for state by
// marking the session in-flight here, so the overflow rescan never sees an
// outstanding session (not parked and not in-flight) that it could duplicate.
// After the slot is claimed the intent is re-validated under the mutex: a
// watermark covering ack, or a coalesced newer park that landed during
// selection, makes the selected copy stale, so the slot is handed back to the
// batch loop and the newer parked intent (or none) is published instead.
func (h *PlaybackHandler) takeDeferredPublishSlot(sessionID string, intent deferredPublishIntent) (claimed bool, stale bool) {
	gate := h.detachedGate()
	if !gate.tryAcquire() {
		return false, false
	}
	h.deferredPublishMu.Lock()
	defer h.deferredPublishMu.Unlock()
	if h.deferredPublishInFlight == nil {
		h.deferredPublishInFlight = make(map[string]struct{})
	}
	// A race at this point is one of: the worker-ack watermark advanced past
	// this intent's generation (already delivered — drop), or a newer intent
	// was parked for this session after selection (the newer entry owns the
	// notification — drop the older one). Either way the slot is not consumed.
	if h.deferredProbeNotifiedCovers(sessionID, intent.generation) {
		gate.release()
		return true, true
	}
	if newer, parked := h.deferredPublishPending[sessionID]; parked && newer.generation > intent.generation {
		gate.release()
		return true, true
	}
	h.deferredPublishInFlight[sessionID] = struct{}{}
	return true, false
}

// rescanDeferredPublishOverflow re-parks sessions that overflowed the bounded
// pending map. It runs when the map has room and the coalesced rescan flag is
// set — not only when the map is empty. Requiring an empty map deadlocked
// overflow recovery behind a retryable entry: a retryable notification stays
// parked while it waits out its backoff, so the map never emptied and the
// overflow sessions were never recovered. The rescan is safe at any occupancy
// because it re-validates each candidate under the lock and only parks a
// session that is neither already parked, nor in-flight, nor already covered by
// the delivered watermark. It pulls a bounded page of sessions with an
// outstanding terminal notification (terminal outcome generation newer than the
// delivered watermark), so the rescan converges: a session whose push landed is
// acked (MarkVirtualProbeNotified) and never returned again. A full page means
// more may remain, so the flag stays set for the next cycle; a short page clears
// it. Bounded both ways: the snapshot and the map are each capped, and a session
// that already ended is absent from the manager's list.
//
// Validation and dispatch eligibility are ATOMIC w.r.t. the ack path: the
// per-candidate re-validation (watermark, parked, in-flight) and the re-park
// all happen while deferredPublishMu is held, following the documented lock
// order (deferredPublishMu outer, session-manager call inner — see
// deferredPublishMu in playback.go). The worker's ack (session lock) always
// precedes its in-flight clear (deferredPublishMu), so while the mutex is held
// no ack can be mid-flight for a session the rescan is about to re-park: either
// the ack already landed (the watermark check skips the candidate) or the
// worker still holds its in-flight marker (the in-flight check skips it). The
// earlier shape — validate outside the mutex, then lock to park — let a worker
// ack and clear between the two steps, re-parking an already-acked generation
// for a duplicate push.
func (h *PlaybackHandler) rescanDeferredPublishOverflow() {
	h.deferredPublishMu.Lock()
	if !h.deferredPublishRescan || len(h.deferredPublishPending) >= virtualDeferredPublishPendingCap {
		// Nothing to recover (flag clear) or no room to park the overflow:
		// leave the flag set and let the dispatcher retry once a slot frees.
		h.deferredPublishMu.Unlock()
		return
	}
	h.deferredPublishRescan = false
	h.deferredPublishMu.Unlock()

	lister, ok := h.sessionMgr.(interface {
		VirtualProbeOutstandingNotifications(limit int) []playback.VirtualProbeTerminalSession
		VirtualProbeNotificationOutstanding(sessionID string) bool
	})
	if !ok {
		return
	}
	page := lister.VirtualProbeOutstandingNotifications(virtualDeferredPublishPendingCap)
	for _, terminal := range page {
		if terminal.SessionID == "" || terminal.FileID <= 0 {
			continue
		}
		h.deferredPublishMu.Lock()
		// Re-validate against the source-of-truth watermark at re-park time,
		// for the SPECIFIC generation this candidate would park: the snapshot
		// was taken outside the publish lock, so a session whose push landed
		// and was acked between the snapshot and now is no longer outstanding,
		// and re-parking it would schedule a duplicate push. A session already
		// parked or mid-publish (in-flight, awaiting its worker's ack) is not
		// re-parked either: re-parking any of these would schedule a second
		// publish of the same notification. Only a session that is neither
		// parked nor in-flight and whose snapshot generation still exceeds the
		// watermark genuinely owes a re-park.
		_, parked := h.deferredPublishPending[terminal.SessionID]
		_, inFlight := h.deferredPublishInFlight[terminal.SessionID]
		outstanding := lister.VirtualProbeNotificationOutstanding(terminal.SessionID)
		if !parked && !inFlight && outstanding && !h.deferredProbeNotifiedCovers(terminal.SessionID, terminal.Generation) {
			if len(h.deferredPublishPending) >= virtualDeferredPublishPendingCap {
				// Still overflowing: keep the flag so the next cycle resumes.
				h.deferredPublishRescan = true
				h.deferredPublishMu.Unlock()
				return
			}
			h.deferredPublishPending[terminal.SessionID] = deferredPublishIntent{fileID: terminal.FileID, generation: terminal.Generation}
		}
		h.deferredPublishMu.Unlock()
	}
	// A full page means the manager may hold more outstanding sessions than fit;
	// keep the flag so the next cycle resumes the rescan. A short page means the
	// outstanding set is drained.
	if len(page) >= virtualDeferredPublishPendingCap {
		h.deferredPublishMu.Lock()
		h.deferredPublishRescan = true
		h.deferredPublishMu.Unlock()
	}
}

// markDeferredPublishInFlight records that a session's terminal notification is
// being published by a worker and has not yet been acked, so the overflow
// rescan does not re-park it into a duplicate push. It reports false when the
// session was already in-flight (another worker is already carrying its
// notification), in which case the caller must not dispatch a second one.
func (h *PlaybackHandler) markDeferredPublishInFlight(sessionID string) bool {
	h.deferredPublishMu.Lock()
	defer h.deferredPublishMu.Unlock()
	if h.deferredPublishInFlight == nil {
		h.deferredPublishInFlight = make(map[string]struct{})
	}
	if _, already := h.deferredPublishInFlight[sessionID]; already {
		return false
	}
	h.deferredPublishInFlight[sessionID] = struct{}{}
	return true
}

// reparkDeferredPublishSelectedLocked returns a selected-but-unclaimed intent
// to the pending map when takeDeferredPublishSlot could not acquire a gate slot.
// It must be called with deferredPublishMu held. It applies the same rules as
// every other park: a same-or-newer intent already parked for the session owns
// the notification; a watermark that already covers this generation means it
// was delivered concurrently, so it is dropped rather than republished; and a
// new session that does not fit the pending bound sets the coalesced rescan
// flag instead of growing the map. Selection deleted the entry before the gate
// claim, so without this bound check a concurrent park could refill the freed
// slot and this reinsert would push the map past virtualDeferredPublishPendingCap.
// It does NOT bump the retry attempt count: a saturated gate is backpressure,
// not a failed publish, so the intent keeps its original schedule.
func (h *PlaybackHandler) reparkDeferredPublishSelectedLocked(sessionID string, intent deferredPublishIntent) {
	if h.deferredPublishPending == nil {
		h.deferredPublishPending = make(map[string]deferredPublishIntent)
	}
	if current, parked := h.deferredPublishPending[sessionID]; parked && current.generation >= intent.generation {
		// A same-or-newer intent was parked while this batch held the selection;
		// it owns the notification.
		return
	}
	if h.deferredProbeNotifiedCovers(sessionID, intent.generation) {
		// The outcome was delivered while the entry was selected; republishing
		// it would duplicate the push.
		return
	}
	if _, parked := h.deferredPublishPending[sessionID]; !parked && len(h.deferredPublishPending) >= virtualDeferredPublishPendingCap {
		// No room for a new session: retain the intent on the overflow path so
		// the rescan recovers it from the session manager once the map has room.
		h.deferredPublishRescan = true
		return
	}
	h.deferredPublishPending[sessionID] = intent
}

// reparkDeferredPublishRetryLocked records a retryable notification for a later
// attempt. It must be called with deferredPublishMu held (the worker's cleanup
// defer owns it). It re-parks only when nothing newer or already-delivered owns
// the session: a covering ack means the outcome landed, and a newer parked
// generation supersedes this one. When the map is full and this is a new session
// it sets the coalesced rescan flag — the same overflow path a full park uses —
// instead of dropping the intent, so the rescan recovers it from the session
// manager once the map has room. The intent is not stored in that case, so it
// cannot pin the map or its backoff; the rescan re-creates the intent with a
// fresh bounded schedule. It reports whether the intent was re-parked.
func (h *PlaybackHandler) reparkDeferredPublishRetryLocked(sessionID string, intent deferredPublishIntent) bool {
	current, parked := h.deferredPublishPending[sessionID]
	if parked && current.generation > intent.generation {
		// A newer park owns the session; this stale attempt is superseded.
		return false
	}
	if h.deferredProbeNotifiedCovers(sessionID, intent.generation) {
		// The outcome was already delivered while this attempt was outstanding.
		return false
	}
	if !parked && len(h.deferredPublishPending) >= virtualDeferredPublishPendingCap {
		// No room for a new session: retain the intent on the overflow path so
		// the rescan recovers it from the session manager once the map has room.
		h.deferredPublishRescan = true
		return false
	}
	intent.attempts++
	intent.notBefore = time.Now().Add(deferredPublishRetryDelay(intent.attempts))
	h.deferredPublishPending[sessionID] = intent
	return true
}

// runDeferredPublishWorker publishes one terminal inventory on a gate slot the
// caller already holds, then releases the slot. It runs under the service
// lifecycle with the publish budget. The publish is SESSION-TARGETED
// (PublishInventoryUpdatedToSession), not the file-wide fan-out: this machinery
// acks delivery per session and per binding generation, so pushing sibling
// sessions on the same file would deliver events they never ack — and ack this
// session against a push its siblings also received, breaking exactly-once for
// both. A session that ended before its slot was granted is simply absent from
// the targeted publish.
//
// The disposition decides the follow-up. A DELIVERED or OBSOLETE attempt acks
// the generation the notification carried, so the overflow rescan stops
// returning this session; the ack is forward-only, so a superseded binding's
// late ack never clears a newer binding's outstanding intent. A RETRYABLE
// attempt (a transient catalog/plan-store read, or a realtime write that did
// not reach the client) must NOT advance the delivered watermark: it re-parks
// the notification with a bounded backoff so the dispatcher retries it. That is
// what keeps a push-only client, which has neither a reconnect nor a poll to
// rescue it, from being left loading forever by a single transient failure.
func (h *PlaybackHandler) runDeferredPublishWorker(sessionID string, intent deferredPublishIntent) {
	gate := h.detachedGate()
	go func() {
		// Deferred cleanup, in order: the gate slot is always returned, and the
		// final disposition is always recorded. The ack/re-park (session lock,
		// watermark forward-only) deliberately precedes the in-flight clear
		// (deferredPublishMu) — that ordering is what lets the rescan and the
		// batch selection treat "in-flight marker absent" as "ack already
		// landed", never observing a session whose ack has not landed as
		// re-parkable. See the lock-order note on deferredPublishMu in
		// playback.go.
		defer gate.release()
		var retry bool
		defer func() {
			h.deferredPublishMu.Lock()
			// The direct (unsaturated) publish path never parks and so never
			// initializes the pending map; a retryable attempt on that path
			// writes the map below, so it must be lazily initialized here under
			// the same lock the park path uses, or the write panics in this
			// background goroutine and takes the process down.
			if h.deferredPublishPending == nil {
				h.deferredPublishPending = make(map[string]deferredPublishIntent)
			}
			if h.deferredPublishInFlight == nil {
				h.deferredPublishInFlight = make(map[string]struct{})
			}
			if retry {
				h.reparkDeferredPublishRetryLocked(sessionID, intent)
			} else {
				h.ackDeferredProbeNotified(sessionID, intent.generation)
			}
			delete(h.deferredPublishInFlight, sessionID)
			h.deferredPublishMu.Unlock()
			// Guarantee the dispatcher exists before waking it. A retryable
			// attempt on the direct path reaches here without the dispatcher
			// ever having started (nothing parked on that path), so a bare
			// non-nil check on deferredPublishDone would leave the re-parked
			// notification with no goroutine to deliver it. startDeferredPublish-
			// Dispatcher is idempotent, so calling it on every completion is
			// free. Buffered-1 and nonblocking: a queued signal is enough because
			// the dispatcher always re-reads the map after any wake.
			h.startDeferredPublishDispatcher()
			if h.deferredPublishDone != nil {
				select {
				case h.deferredPublishDone <- struct{}{}:
				default:
				}
			}
		}()
		ctx, cancel := h.virtualDetachedContext(h.ServiceContext, inventoryUpdatedPublishBudget)
		defer cancel()
		summary, result := h.PublishInventoryUpdatedToSession(ctx, sessionID, intent.fileID)
		h.logInventoryDelivery(summary, 0)
		retry = result == inventoryPublishRetryable
	}()
}

// ackDeferredProbeNotified marks the binding generation whose terminal outcome
// was just pushed, so the overflow rescan converges. It is a no-op when the
// manager cannot track notification progress (a minimal test double) — the
// in-memory map entry was already removed, so the session is not republished
// this cycle regardless.
func (h *PlaybackHandler) ackDeferredProbeNotified(sessionID string, generation uint64) {
	if h == nil || sessionID == "" {
		return
	}
	marker, ok := h.sessionMgr.(interface {
		MarkVirtualProbeNotified(sessionID string, generation uint64) bool
	})
	if !ok {
		return
	}
	marker.MarkVirtualProbeNotified(sessionID, generation)
}

// dropDeferredPublish discards a session's parked terminal notification at
// teardown, so a session that ended before its push could run is not retried
// against a session ID the manager no longer knows.
func (h *PlaybackHandler) dropDeferredPublish(sessionID string) {
	if h == nil || sessionID == "" {
		return
	}
	h.deferredPublishMu.Lock()
	defer h.deferredPublishMu.Unlock()
	delete(h.deferredPublishPending, sessionID)
}
