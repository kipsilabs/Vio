package handlers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/jackc/pgx/v5/pgconn"
)

// Probe-evidence persistence is a bounded, coalescing, retrying pipeline
// rather than a fire-and-forget queue.
//
// Admission: an evidence write is keyed by row identity plus the target source
// identity and stamping semantics (see virtualEvidenceKey). A key already
// pending is coalesced only when the new snapshot is at least as new as the
// queued one; a stale writer is rejected outright so it can never overwrite
// newer evidence, selection or failure state. A genuinely distinct key is
// queued until the pending bound is reached, then rejected. Both outcomes are
// returned to the caller, so overload is observable rather than a silent drop.
//
// Execution: a fixed worker pool (virtualEvidenceWorkers) drains the buffer.
// Each write carries its original CAS snapshot (updated_at, probe_updated_at,
// owner, library) so the SQL fence rejects a write whose row has moved on. A
// transient failure is retried with bounded exponential backoff; a permanent
// database error, a CAS miss (rows affected zero), a deterministic adoption
// refusal (errVirtualAdoptIdentityNotPersisted), or exhausted retries is a
// terminal failure that is logged once and dropped. Neither a CAS miss nor an
// adoption refusal is ever retried: a newer writer already committed the row,
// and a refusal is a property of the row's state that a retry cannot change.
//
// Shutdown: closure and admission share the buffer mutex, so the moment
// shutdown begins no further admission can be accepted; a later admit is
// rejected explicitly. Shutdown then awaits every worker (including the task a
// worker already dequeued) and only then drains the remaining accepted work
// under an independent, bounded context. Dequeued work is therefore never
// stranded on the canceled service context and never silently dropped: each
// task is persisted or logged as a terminal failure, and the abandonment
// warning counts both still-queued and still-dequeued work.
//
// The application shutdown sequence is the primary trigger: main calls
// PlaybackHandler.StopVirtualEvidence explicitly and waits for it under its own
// bounded timeout (see cmd/silo). The service-context watcher started by
// evidenceBuffer is only a safety net for callers that never wire the
// lifecycle; it calls the same single-flight path.
//
// Outcomes at shutdown:
//   - Accepted work — queued, or already admitted and dequeued by a worker — is
//     drained within virtualEvidenceDrainGrace (5 s). A write already in flight
//     is awaited and bounded by that same grace. The application-level wait is
//     deliberately longer so the handler can complete rather than be cut off.
//   - Rejected work — refused by admit because the buffer was full or the
//     snapshot was stale, or refused after closure because shutdown won the
//     admission race — is reported to the caller as virtualEvidenceRejected and
//     is never queued.
//   - Work still unfinished when the drain budget expires is abandoned with a
//     warning. Nothing here is durable across process death: accepted evidence
//     lives only in this in-memory buffer, so a crash or a forced termination
//     loses every queued or in-flight write. That is safe because the loss is
//     evidence, not data — the next playback start re-probes and re-admits, and
//     the catalog keeps its last committed snapshot until then.

const (
	// virtualEvidenceQueueSize bounds queued (not yet executing) evidence
	// writes. Active tasks are removed from the pending buffer before they
	// execute, so active + pending is virtualEvidenceQueueSize +
	// virtualEvidenceWorkers.
	virtualEvidenceQueueSize = 256
	// virtualEvidenceWorkers is the number of workers that persist queued
	// evidence. Two keeps a burst moving without competing with probe workers
	// for the aggregate gate.
	virtualEvidenceWorkers = 2
	// virtualEvidencePersistBudget bounds one evidence write attempt.
	virtualEvidencePersistBudget = 5 * time.Second
	// virtualEvidenceMaxAttempts bounds retries for one accepted write.
	virtualEvidenceMaxAttempts = 3
	// virtualEvidenceRetryBaseDelay is the first backoff step; it doubles per
	// attempt up to virtualEvidenceRetryMaxDelay.
	virtualEvidenceRetryBaseDelay = 100 * time.Millisecond
	virtualEvidenceRetryMaxDelay  = time.Second
	// virtualEvidenceDrainGrace bounds the shutdown drain of accepted work.
	virtualEvidenceDrainGrace = 5 * time.Second
	// virtualEvidenceFallbackBudget bounds the last-resort direct evidence write
	// (and, for the detached-gate fallback, the short foreground probe) the
	// start path performs when the bounded pool rejects the write or its
	// detached probe gate is exhausted. Short by design: it is one bounded
	// attempt on the request path for the foreground candidate only, not a
	// second worker pool.
	virtualEvidenceFallbackBudget = 2 * time.Second
)

// errVirtualEvidenceStale marks a CAS-fenced write that matched no row because
// a newer writer (or a candidate rotation) already committed. It is terminal:
// retrying cannot help.
var errVirtualEvidenceStale = errors.New("virtual evidence write superseded by a newer snapshot")

// virtualEvidenceAdmission is the explicit result of admitting one evidence
// write.
type virtualEvidenceAdmission int

const (
	// virtualEvidenceAccepted means the write was queued for execution.
	virtualEvidenceAccepted virtualEvidenceAdmission = iota
	// virtualEvidenceCoalesced means the write was folded onto an equivalent
	// pending write (or was rejected as an older snapshot of one).
	virtualEvidenceCoalesced
	// virtualEvidenceRejected means the write could not be admitted because the
	// pending buffer was full or the snapshot was stale relative to a pending
	// equivalent.
	virtualEvidenceRejected
)

// String forms of virtualEvidenceAdmission. They are the stable textual result
// of an admission, distinct from the autoscan delivery status of the same name.
const (
	virtualEvidenceResultAccepted  = "accepted"
	virtualEvidenceResultCoalesced = "coalesced"
	virtualEvidenceResultRejected  = "rejected"
	virtualEvidenceResultUnknown   = "unknown"
)

// Log attribute keys shared by the evidence pipeline's terminal failure line.
// They are named constants so the repeated literals stay out of the changed
// lines (goconst) and every occurrence cannot drift from the others.
const (
	virtualEvidenceLogKeyComponent = "component"
	virtualEvidenceLogKeyFileID    = "file_id"
	virtualEvidenceLogKeyAttempts  = "attempts"
	virtualEvidenceLogKeyError     = "error"
	virtualEvidenceLogValueAPI     = "api"
)

func (a virtualEvidenceAdmission) String() string {
	switch a {
	case virtualEvidenceAccepted:
		return virtualEvidenceResultAccepted
	case virtualEvidenceCoalesced:
		return virtualEvidenceResultCoalesced
	case virtualEvidenceRejected:
		return virtualEvidenceResultRejected
	default:
		return virtualEvidenceResultUnknown
	}
}

// virtualEvidenceTask is one buffered evidence write plus the ordering
// metadata coalescing uses to decide whether a newer request supersedes it.
type virtualEvidenceTask struct {
	key       string
	seq       uint64
	updatedAt time.Time
	probeAt   *time.Time
	args      models.VirtualFilePersistArgs
	// originFileID is the row the probe was requested for. It differs from
	// args.FileID only when the identity guard rotated the write to a sibling
	// owner row, so the terminal delivery log can name both ends of the
	// rotation.
	originFileID int
}

// virtualEvidenceKey identifies an evidence target. Same row, same expected
// path, same adopted source identity and same stamping semantics may coalesce;
// a different adoption target (a different release), a different stamping mode,
// or a different adoption requirement is a distinct task so coalescing cannot
// drop a probe stamp, write one source's evidence under another's identity, or
// fold a fenced cross-release write (RequireAdopt) into a metadata-only one
// that shares its row, path and adoption target.
func virtualEvidenceKey(args models.VirtualFilePersistArgs) string {
	return fmt.Sprintf("%d\x00%d\x00%s\x00%s\x00%t\x00%t",
		args.FileID, args.OwnerID, args.ExpectedFilePath, args.AdoptPath, args.StampProbe, args.RequireAdopt)
}

func virtualEvidenceProbeTime(p *time.Time) time.Time {
	if p == nil {
		return time.Time{}
	}
	return *p
}

// virtualEvidenceSnapshotNewer reports whether a's snapshot supersedes b's. It
// is the stale-writer guard: coalescing never lets an older snapshot replace a
// newer queued one.
func virtualEvidenceSnapshotNewer(a, b *virtualEvidenceTask) bool {
	if a == nil {
		return false
	}
	if b == nil {
		return true
	}
	if a.updatedAt.After(b.updatedAt) {
		return true
	}
	if a.updatedAt.Before(b.updatedAt) {
		return false
	}
	pa, pb := virtualEvidenceProbeTime(a.probeAt), virtualEvidenceProbeTime(b.probeAt)
	if pa.After(pb) {
		return true
	}
	if pa.Before(pb) {
		return false
	}
	return a.seq >= b.seq
}

// virtualEvidenceBuffer is a bounded FIFO of pending evidence tasks with a
// coalescing index. It is deliberately not a channel: coalescing must replace
// a pending task in place, which a channel cannot express. All methods are safe
// for concurrent use.
//
// The same mutex guards both admission and closure, so "may I enqueue?" and
// "stop accepting" cannot interleave: a task is either admitted (and therefore
// drained) or rejected because shutdown won. shutdownCh wakes workers that are
// parked with an empty buffer, so shutdown does not depend on the service
// context being cancellable in tests.
type virtualEvidenceBuffer struct {
	mu         sync.Mutex
	pending    []*virtualEvidenceTask
	index      map[string]*virtualEvidenceTask
	capacity   int
	seq        uint64
	signalCh   chan struct{}
	shutdownCh chan struct{}
	closed     bool
	drainUntil time.Time
	// drainGrace bounds how long a shutdown drain may spend on accepted work.
	// It defaults to virtualEvidenceDrainGrace; it is a field so tests can
	// exercise an already-expired budget. A zero value falls back to the
	// default, so a negative value is the way to force an expired deadline.
	drainGrace time.Duration
	// inflight counts tasks popped by a worker or drainer that have not yet
	// been persisted and released. It makes already-dequeued work explicit:
	// shutdown awaits workers, and the abandonment warning reports it.
	inflight int
}

func newVirtualEvidenceBuffer(capacity int) *virtualEvidenceBuffer {
	if capacity <= 0 {
		capacity = virtualEvidenceQueueSize
	}
	return &virtualEvidenceBuffer{
		index:      make(map[string]*virtualEvidenceTask),
		capacity:   capacity,
		signalCh:   make(chan struct{}, 1),
		shutdownCh: make(chan struct{}),
		drainGrace: virtualEvidenceDrainGrace,
	}
}

func (b *virtualEvidenceBuffer) signal() {
	if b == nil || b.signalCh == nil {
		return
	}
	select {
	case b.signalCh <- struct{}{}:
	default:
	}
}

func (b *virtualEvidenceBuffer) nextSeq() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	return b.seq
}

// admit attempts to enqueue one task, returning the explicit admission result.
func (b *virtualEvidenceBuffer) admit(t *virtualEvidenceTask) virtualEvidenceAdmission {
	if b == nil || t == nil {
		return virtualEvidenceRejected
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		// Shutdown won the race for the lock: acceptance is over, so the caller
		// gets the explicit rejected result rather than a task that no worker
		// will ever drain.
		return virtualEvidenceRejected
	}
	if existing, ok := b.index[t.key]; ok {
		if !virtualEvidenceSnapshotNewer(t, existing) {
			// A stale writer for an equivalent target: reject rather than let
			// it replace newer evidence.
			return virtualEvidenceRejected
		}
		existing.args = t.args
		existing.updatedAt = t.updatedAt
		existing.probeAt = t.probeAt
		existing.seq = t.seq
		b.signal()
		return virtualEvidenceCoalesced
	}
	if len(b.pending) >= b.capacity {
		return virtualEvidenceRejected
	}
	b.index[t.key] = t
	b.pending = append(b.pending, t)
	b.signal()
	return virtualEvidenceAccepted
}

// pop removes and returns the oldest pending task, or nil when none is queued.
// The key leaves the coalescing index here, so a duplicate arriving while this
// task executes becomes a distinct, sequentially-later write.
func (b *virtualEvidenceBuffer) pop() *virtualEvidenceTask {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pending) == 0 {
		return nil
	}
	t := b.pending[0]
	b.pending = b.pending[1:]
	delete(b.index, t.key)
	b.inflight++
	return t
}

// taskFinished releases the in-flight slot for a task returned by pop. Every
// pop must be paired with it once the task has been persisted or terminally
// logged.
func (b *virtualEvidenceBuffer) taskFinished() {
	if b == nil {
		return
	}
	b.mu.Lock()
	if b.inflight > 0 {
		b.inflight--
	}
	b.mu.Unlock()
}

// close atomically stops admission and wakes the workers. It is idempotent.
// Setting closed and closing shutdownCh under the admission mutex is what makes
// closure and admission atomic: no admit can land after this returns.
func (b *virtualEvidenceBuffer) close() {
	if b == nil {
		return
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	grace := b.drainGrace
	if grace == 0 {
		grace = virtualEvidenceDrainGrace
	}
	b.closed = true
	b.drainUntil = time.Now().Add(grace)
	close(b.shutdownCh)
	b.mu.Unlock()
	b.signal()
}

// drainDeadline returns the wall-clock bound for draining accepted work, or the
// zero time while the buffer is still accepting.
func (b *virtualEvidenceBuffer) drainDeadline() time.Time {
	if b == nil {
		return time.Time{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.drainUntil
}

func (b *virtualEvidenceBuffer) inflightCount() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inflight
}

func (b *virtualEvidenceBuffer) len() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pending)
}

func (b *virtualEvidenceBuffer) signalChannel() <-chan struct{} {
	if b == nil {
		return nil
	}
	return b.signalCh
}

// shutdownChannel is closed by close, waking workers parked on an empty buffer.
func (b *virtualEvidenceBuffer) shutdownChannel() <-chan struct{} {
	if b == nil {
		return nil
	}
	return b.shutdownCh
}

// evidenceBuffer returns the handler's evidence buffer, constructing it and
// starting the bounded worker pool plus the shutdown watcher on first use.
func (h *PlaybackHandler) evidenceBuffer() *virtualEvidenceBuffer {
	if h == nil {
		return nil
	}
	h.virtualEvidenceOnce.Do(func() {
		if h.virtualEvidenceBuffer == nil {
			h.virtualEvidenceBuffer = newVirtualEvidenceBuffer(virtualEvidenceQueueSize)
		}
		buf := h.virtualEvidenceBuffer
		h.virtualEvidenceWG.Add(virtualEvidenceWorkers)
		for range virtualEvidenceWorkers {
			go func() {
				defer h.virtualEvidenceWG.Done()
				h.runVirtualEvidenceWorker(buf)
			}()
		}
		// The watcher is started inside the once, after the workers are
		// registered, so stopVirtualEvidence always has a started pool to await.
		// It is only a safety net: the application shutdown sequence calls
		// StopVirtualEvidence directly (wired in cmd/silo), so shutdown awaits
		// accepted work regardless of whether the service context is ever
		// canceled. Done() is nil for context.Background, which must not leak a
		// watcher.
		if h.ServiceContext != nil && h.ServiceContext.Done() != nil {
			serviceDone := h.ServiceContext.Done()
			go func() {
				<-serviceDone
				h.stopVirtualEvidence()
			}()
		}
	})
	return h.virtualEvidenceBuffer
}

// StopVirtualEvidence is the explicit application-shutdown drain. The
// application lifecycle calls it so shutdown waits for accepted evidence
// instead of relying on a detached lifecycle watcher. It atomically stops
// admission, awaits every worker (including a task one already dequeued), then
// drains the accepted remainder under an independent bounded context. It is
// idempotent and safe to call concurrently or during shutdown: every caller
// blocks until the single closure/await/drain pass has completed, and a caller
// that races the safety-net watcher observes the same terminal state. A handler
// that never admitted evidence has nothing to drain and returns immediately.
func (h *PlaybackHandler) StopVirtualEvidence() {
	h.stopVirtualEvidence()
}

// stopVirtualEvidence is the single shutdown path shared by the explicit
// application call and the lifecycle watcher: it atomically stops admission,
// awaits every worker (including the task one already dequeued), then drains the
// accepted remainder under an independent bounded context. It is single-flight,
// so a caller racing the lifecycle watcher blocks until the one closure has
// fully completed.
func (h *PlaybackHandler) stopVirtualEvidence() {
	if h == nil || h.virtualEvidenceBuffer == nil {
		return
	}
	h.virtualEvidenceStopOnce.Do(func() {
		buf := h.virtualEvidenceBuffer
		buf.close()
		h.virtualEvidenceWG.Wait()
		h.drainVirtualEvidence(buf)
	})
}

// enqueueVirtualProbeEvidence admits one catalog write into the evidence
// buffer. It never blocks: a full buffer rejects instead.
func (h *PlaybackHandler) enqueueVirtualProbeEvidence(ctx context.Context, args models.VirtualFilePersistArgs) virtualEvidenceAdmission {
	return h.enqueueVirtualProbeEvidenceFor(ctx, args, args.FileID)
}

// enqueueVirtualProbeEvidenceFor is enqueueVirtualProbeEvidence for a write that
// may target a row other than the one the probe was requested for. originFileID
// is recorded on the task so the delivery log can name a rotated write's source
// and owner rows; it has no effect on admission, coalescing, or the write.
func (h *PlaybackHandler) enqueueVirtualProbeEvidenceFor(_ context.Context, args models.VirtualFilePersistArgs, originFileID int) virtualEvidenceAdmission {
	if h == nil || h.VirtualFileSaver == nil {
		return virtualEvidenceRejected
	}
	if args.FileID <= 0 {
		return virtualEvidenceRejected
	}
	buf := h.evidenceBuffer()
	task := &virtualEvidenceTask{
		key:          virtualEvidenceKey(args),
		seq:          buf.nextSeq(),
		updatedAt:    args.UpdatedAt,
		probeAt:      args.ProbeUpdatedAt,
		args:         args,
		originFileID: originFileID,
	}
	return buf.admit(task)
}

// persistVirtualEvidenceDirect performs one bounded synchronous evidence write,
// bypassing the bounded worker pool. It is the last-resort fallback for the
// foreground request's own candidate when the pool rejects the write or its
// detached probe gate is exhausted; it never retries and is bounded by
// virtualEvidenceFallbackBudget, so it cannot turn into a second worker pool.
// It returns whether a row was actually updated. background/speculative callers
// must not use it.
func (h *PlaybackHandler) persistVirtualEvidenceDirect(ctx context.Context, args models.VirtualFilePersistArgs) bool {
	if h == nil || args.FileID <= 0 || (h.VirtualFileMetadataSaver == nil && h.VirtualFileSaver == nil) {
		return false
	}
	// The request may already be canceled; a last-resort evidence write should
	// still land, so keep the values but drop the cancellation.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), virtualEvidenceFallbackBudget)
	defer cancel()
	if h.VirtualFileMetadataSaver != nil {
		result, err := h.VirtualFileMetadataSaver(writeCtx, args)
		if err != nil {
			slog.WarnContext(ctx, "virtual probe evidence direct fallback failed",
				"component", "api", "file_id", args.FileID, "error", err)
			return false
		}
		if result.MetadataUpdated {
			h.logInventoryDelivery(h.PublishInventoryUpdated(writeCtx, args.FileID), 0)
			// The row now carries the verified inventory: replay any
			// cold-start default audio selection against the committed order
			// so the executable recipe keeps the preferred language.
			h.reconcileVerifiedDefaultAudio(context.Background(), args.FileID)
		}
		return result.MetadataUpdated
	}
	rows, err := h.VirtualFileSaver(writeCtx, args)
	if err != nil {
		slog.WarnContext(ctx, "virtual probe evidence direct fallback failed",
			"component", "api", "file_id", args.FileID, "error", err)
		return false
	}
	if rows > 0 {
		h.logInventoryDelivery(h.PublishInventoryUpdated(writeCtx, args.FileID), 0)
		h.reconcileVerifiedDefaultAudio(context.Background(), args.FileID)
	}
	return rows > 0
}

// logInventoryDelivery records a successful inventory_updated fan-out: the file
// whose evidence was pushed, how many live sessions received it, the revision,
// and — for a rotated write — the row the probe was originally requested for.
// It emits nothing when no session was notified, so an idle background probe
// does not fill the log.
func (h *PlaybackHandler) logInventoryDelivery(summary inventoryPublishSummary, originFileID int) {
	if summary.SessionsNotified <= 0 {
		return
	}
	attrs := []any{
		virtualEvidenceLogKeyComponent, virtualEvidenceLogValueAPI,
		virtualEvidenceLogKeyFileID, summary.FileID,
		"sessions_notified", summary.SessionsNotified,
		fieldRevision, summary.Revision,
	}
	if originFileID > 0 && originFileID != summary.FileID {
		attrs = append(attrs, "rotated_from_file_id", originFileID)
	}
	slog.Info("virtual probe evidence inventory delivered", attrs...)
}

// runVirtualEvidenceWorker drains accepted work until the buffer is closed by
// shutdown, then exits. It deliberately does not exit on the service context:
// the watcher's stopVirtualEvidence closes the buffer first, so a worker never
// abandons a dequeued task, and admission is already closed by the time any
// worker returns.
func (h *PlaybackHandler) runVirtualEvidenceWorker(buf *virtualEvidenceBuffer) {
	for {
		if task := buf.pop(); task != nil {
			// persistVirtualEvidenceTask logs terminal failures itself, so the
			// worker reports and drops them there and moves to the next task.
			// taskFinished releases the explicit in-flight accounting.
			_ = h.persistVirtualEvidenceTask(task, time.Time{})
			buf.taskFinished()
			continue
		}
		select {
		case <-buf.signalChannel():
		case <-buf.shutdownChannel():
			return
		}
	}
}

// drainVirtualEvidence persists the accepted work left when shutdown began for
// at most the buffer's drain deadline, then abandons the remainder with a
// warning that accounts for both still-queued and still-dequeued work. Drain
// writes use a context independent of the canceled service context so they can
// still complete. It runs after the workers have exited, so anything it pops is
// work no worker had taken.
func (h *PlaybackHandler) drainVirtualEvidence(buf *virtualEvidenceBuffer) {
	deadline := buf.drainDeadline()
	if deadline.IsZero() {
		deadline = time.Now().Add(virtualEvidenceDrainGrace)
	}
	for time.Now().Before(deadline) {
		task := buf.pop()
		if task == nil {
			return
		}
		// persistVirtualEvidenceTask logs terminal failures itself.
		_ = h.persistVirtualEvidenceTask(task, deadline)
		buf.taskFinished()
	}
	if queued, inflight := buf.len(), buf.inflightCount(); queued > 0 || inflight > 0 {
		slog.Warn("virtual probe evidence abandoned at shutdown",
			"component", "api", "queued", queued, "inflight", inflight)
	}
}

// persistVirtualEvidenceTask executes one accepted write with bounded retry.
// drainDeadline, when non-zero, bounds the whole task (including backoff) for
// shutdown draining; when the buffer has been closed its deadline takes
// precedence, so a task dequeued just before shutdown is still drained rather
// than stranded. The return value is the write error, or errVirtualEvidenceStale
// on a CAS miss; both are logged as terminal.
func (h *PlaybackHandler) persistVirtualEvidenceTask(task *virtualEvidenceTask, drainDeadline time.Time) error {
	if task == nil || (h.VirtualFileMetadataSaver == nil && h.VirtualFileSaver == nil) {
		return nil
	}
	// Prefer the explicit adoption-result saver so a metadata-only write is
	// never mistaken for identity adoption; the legacy row-count saver stays
	// as the fallback for wirings that only provide it.
	save := func(ctx context.Context, args models.VirtualFilePersistArgs) (updated bool, err error) {
		if h.VirtualFileMetadataSaver != nil {
			result, err := h.VirtualFileMetadataSaver(ctx, args)
			if err != nil {
				return false, err
			}
			if args.AdoptPath != "" && !result.IdentityAdopted {
				slog.DebugContext(ctx, "virtual probe evidence persisted without identity adoption",
					"component", "api", "file_id", args.FileID, "adopt_path", args.AdoptPath)
			}
			return result.MetadataUpdated, nil
		}
		rows, err := h.VirtualFileSaver(ctx, args)
		return err == nil && rows > 0, err
	}
	var lastErr error
	attempts := 0
	for attempt := 1; attempt <= virtualEvidenceMaxAttempts; attempt++ {
		attempts = attempt
		deadline := h.effectiveEvidenceDeadline(drainDeadline)
		if attempt > 1 {
			delay := virtualEvidenceBackoff(attempt)
			if !deadline.IsZero() {
				remaining := time.Until(deadline)
				if remaining <= 0 {
					lastErr = context.DeadlineExceeded
					break
				}
				if delay > remaining {
					delay = remaining
				}
			}
			// Backoff is bounded (<= virtualEvidenceRetryMaxDelay) and does not
			// abort on the service context: shutdown drains accepted work rather
			// than dropping it.
			time.Sleep(delay)
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			lastErr = context.DeadlineExceeded
			break
		}
		writeCtx, cancel := h.evidenceWriteContext(deadline)
		updated, err := save(writeCtx, task.args)
		cancel()
		if err == nil {
			if !updated {
				lastErr = errVirtualEvidenceStale
				goto terminal
			}
			// The row now carries the probe evidence. Push the verified inventory
			// to any live session bound to it, so a menu stops showing the
			// declared snapshot. The publish bounds its own context and is
			// best-effort: a failure here never affects the committed write.
			//
			// The publish is the single delivery for this revision: the write
			// targets the row that owns the bytes (the owner row after a
			// rotation), and the session-bound lookup reaches every live menu on
			// it. The delivery log below is what makes a rotation's publish
			// observable; it does not publish a second time.
			h.logInventoryDelivery(h.PublishInventoryUpdated(context.Background(), task.args.FileID), task.originFileID)
			// The verified inventory is committed: replay cold-start default
			// audio intent against its order so the executable recipe keeps
			// the preferred language instead of the declared ordinal.
			h.reconcileVerifiedDefaultAudio(context.Background(), task.args.FileID)
			return nil
		}
		lastErr = err
		if !virtualEvidenceRetryable(err) {
			goto terminal
		}
	}
terminal:
	attrs := []any{
		virtualEvidenceLogKeyComponent, virtualEvidenceLogValueAPI,
		virtualEvidenceLogKeyFileID, task.args.FileID,
		virtualEvidenceLogKeyAttempts, attempts,
		virtualEvidenceLogKeyError, lastErr,
	}
	// The candidate identity is the reason an adoption refusal is deterministic:
	// the row could not take this path. Carry it so the one terminal log
	// identifies what was refused instead of only the file it belongs to.
	if task.args.AdoptPath != "" {
		attrs = append(attrs, "candidate", task.args.AdoptPath)
	}
	if errors.Is(lastErr, errVirtualEvidenceStale) {
		// A CAS miss is the expected outcome when a newer snapshot (a re-list, a
		// migration, another probe) lands first. It is not an error condition, so
		// it must not page an operator as one.
		slog.Debug("virtual probe evidence persist skipped: superseded by a newer snapshot", attrs...)
		return lastErr
	}
	slog.Error("virtual probe evidence persist terminal failure", attrs...)
	return lastErr
}

// effectiveEvidenceDeadline resolves the bound for one write attempt. Once the
// buffer has been closed its drain deadline takes precedence over the caller's
// fallback, so a task already in a worker's hands when shutdown begins is
// bounded by the drain grace, not just the per-attempt budget.
func (h *PlaybackHandler) effectiveEvidenceDeadline(fallback time.Time) time.Time {
	if h != nil && h.virtualEvidenceBuffer != nil {
		if d := h.virtualEvidenceBuffer.drainDeadline(); !d.IsZero() {
			return d
		}
	}
	return fallback
}

// evidenceWriteContext builds one write attempt's context. It is independent of
// ServiceContext by design: accepted evidence must be drained at shutdown, so
// canceling the service must not fail an already-admitted write. The attempt is
// bounded by the per-attempt budget, tightened to the remaining drain grace once
// shutdown begins.
func (h *PlaybackHandler) evidenceWriteContext(deadline time.Time) (context.Context, context.CancelFunc) {
	timeout := virtualEvidencePersistBudget
	if !deadline.IsZero() {
		if remaining := time.Until(deadline); remaining > 0 && remaining < timeout {
			timeout = remaining
		}
	}
	return context.WithTimeout(context.Background(), timeout)
}

func virtualEvidenceBackoff(attempt int) time.Duration {
	if attempt < 2 {
		return 0
	}
	delay := virtualEvidenceRetryBaseDelay
	for i := 1; i < attempt-1; i++ {
		delay *= 2
		if delay >= virtualEvidenceRetryMaxDelay {
			return virtualEvidenceRetryMaxDelay
		}
	}
	if delay > virtualEvidenceRetryMaxDelay {
		return virtualEvidenceRetryMaxDelay
	}
	return delay
}

// virtualEvidenceRetryable classifies a write error. Cancellation stops the
// task; a deterministic adoption refusal is not retried because it would fail
// identically every time; integrity and data/syntax-class database errors are
// permanent; a timeout, a connection error or any other transport fault is
// transient and worth a bounded retry.
//
// Determinism of errVirtualAdoptIdentityNotPersisted: the target row's state
// (a sibling owner of the candidate path, a collection-owned row with no
// adoption target, or a live failed verdict) will not change between attempts,
// and the caller supplied a fixed CAS snapshot. Re-running the same fenced
// statement therefore reproduces the refusal rather than racing a fixable
// transposition, so the three-attempt retry was pure noise and is dropped here.
func virtualEvidenceRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, errVirtualAdoptIdentityNotPersisted) {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		for _, prefix := range []string{"22", "23", "42"} {
			if strings.HasPrefix(pgErr.Code, prefix) {
				return false
			}
		}
	}
	return true
}
