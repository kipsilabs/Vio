package handlers

import (
	"context"
	"log/slog"
	"maps"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/resolver"
)

// The pre-play probe chain closes the gap between discovery persistence and
// the first play attempt. Discovery (the v2 catalog leaf detail path) lists a
// virtual item's provider candidates and persists them, but the persisted
// rows carry declared metadata only: ffprobe evidence — the exact stream
// table — lands the first time the version actually plays. A client that
// opens the detail page and immediately presses Play therefore pays the
// probe latency on its first start even though discovery just ran. Android
// has no background refetch of its own, so that cost always lands on the
// first open there.
//
// The chain re-reads the item's default playback version after persistence,
// requires that its bytes are known exactly (the row must be a persisted
// candidate whose URI appears in the just-persisted listing — a neutral
// group row may resolve to a sibling candidate, so it is never probed
// speculatively), and probes it through the same background evidence path
// the resolve uses (queued, coalescing write; never the foreground direct
// fallback). Nothing here gates playback: the play path is untouched and
// never observes the chain. A chain may finish its probe before a play
// attempt resolves the same row — that attempt then reuses the recorded
// evidence — or play may win first; the queued evidence write follows the
// ordinary persistence rules either way. Every chain is optional best-effort
// work: any failure, including an unexpected panic, may never affect the
// server process serving it.

// virtualDefaultProbeChainBudget bounds one admitted chain's whole life: the
// default-version re-read, the candidate match, and the ffprobe enumeration
// behind the probe call all run under this deadline. probeVirtualSourceAndPersist
// applies its own inner budget on top, so the total cost of a chain is bounded
// twice: once here, by the chain owner, and once inside the probe.
var virtualDefaultProbeChainBudget = 90 * time.Second

// virtualDefaultProbeChainConcurrency bounds how many chains run at once.
// Two keeps a household-wide discovery burst from saturating the detached
// gate while still admitting a genuinely concurrent browse.
const virtualDefaultProbeChainConcurrency = 2

// VirtualDefaultProbeTarget re-reads the item's default (top-sorted) playback
// version after discovery persistence. The chain must probe the row a viewer
// will actually open — the default — and it must re-read those rows after
// persistence instead of trusting the discovery response, because discovery
// may have replaced rows between the request and the persist landing.
type VirtualDefaultProbeTarget interface {
	DefaultVirtualProbeTarget(ctx context.Context, contentID string) (*models.MediaFile, error)
}

// VirtualDefaultProbeTargetFunc allows wiring the default-version lookup as a
// plain function.
type VirtualDefaultProbeTargetFunc func(ctx context.Context, contentID string) (*models.MediaFile, error)

// DefaultVirtualProbeTarget re-reads the item's default playback version.
func (f VirtualDefaultProbeTargetFunc) DefaultVirtualProbeTarget(ctx context.Context, contentID string) (*models.MediaFile, error) {
	return f(ctx, contentID)
}

// defaultProbeChainAdmission deduplicates and bounds pre-play probe chains.
// The map holds one entry per live chain, keyed by the content id the viewer
// opened, and admitted counts live chains against
// virtualDefaultProbeChainConcurrency.
//
// acquire is the whole admission operation: it checks the item key and the
// concurrency bound under one lock and performs no I/O, waits for nothing,
// and spawns nothing. A rejected caller returns immediately; the database
// reads and the goroutine belong to admitted work only. The zero value is
// ready to use.
type defaultProbeChainAdmission struct {
	mu       sync.Mutex
	active   map[string]struct{}
	admitted int
}

// acquire admits one chain for contentID. ok is false when a chain for the
// item is already live (duplicate callbacks coalesce into that chain) or when
// every slot is taken (the callback is dropped; the play path and any later
// discovery run still probe normally). The returned func releases the slot
// and must be called exactly once.
func (a *defaultProbeChainAdmission) acquire(contentID string) (func(), bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active == nil {
		a.active = make(map[string]struct{})
	}
	if _, live := a.active[contentID]; live {
		return nil, false
	}
	if a.admitted >= virtualDefaultProbeChainConcurrency {
		return nil, false
	}
	a.active[contentID] = struct{}{}
	a.admitted++
	return func() { a.release(contentID) }, true
}

func (a *defaultProbeChainAdmission) release(contentID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, live := a.active[contentID]; !live {
		return
	}
	delete(a.active, contentID)
	a.admitted--
}

// ScheduleDefaultFileProbe is the subscription point the discovery services
// call after a successful detached persistence (see
// VirtualCandidatesRefreshService.ScheduleDefaultFileProbe). It is
// non-blocking by contract: admission is a single atomic check, and
// everything else — including the database re-read — happens inside the
// admitted chain, never on the discovery caller.
func (h *PlaybackHandler) ScheduleDefaultFileProbe(contentID string, source *models.MediaFile, streams []VirtualPlaybackStream) {
	if h == nil || contentID == "" || source == nil || len(streams) == 0 {
		return
	}
	if h.DefaultProbeTarget == nil {
		return
	}
	if h.VirtualPlaybackSourceProber == nil && h.VirtualPlaybackSourceProberWithHeaders == nil {
		return
	}
	release, ok := h.defaultProbeChains.acquire(contentID)
	if !ok {
		// A chain for this item is already live, or both slots are taken.
		// Either way the item is covered: the live chain re-reads the same
		// defaults, and a dropped probe still happens on the first play. No
		// queue, no retry, no delay for the caller.
		return
	}
	// Snapshot the exact inputs so a later mutation of the caller's structures
	// cannot change what is probed: the row is copied by value and the
	// listing entries are deep-copied for the mutable members the probe
	// reads.
	probeSource := *source
	streamsSnapshot := make([]VirtualPlaybackStream, len(streams))
	for i := range streams {
		streamsSnapshot[i] = virtualProbeStreamSnapshot(streams[i])
	}
	// The chain detaches from the discovery caller: its context carries the
	// chain budget and ends with the service (virtualDetachedContext cancels
	// on service shutdown), never with the browse request that listed the
	// candidates. The single cleanup defer runs in a fixed order — recover
	// log, admission release, context cancel, completion signal — so a panic
	// anywhere in the chain body is an observation, not a process death
	// unrelated playback would share, and Done() reports the slot truly
	// free: a Wait() caller must be able to re-schedule the same item
	// without racing the previous chain's teardown.
	ctx, cancel := h.virtualDetachedContext(context.Background(), virtualDefaultProbeChainBudget)
	h.probeChainWG.Add(1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.ErrorContext(context.WithoutCancel(ctx), "virtual pre-play probe chain panicked",
					"component", "api", "content_id", contentID, "stack", string(debug.Stack()))
			}
			release()
			cancel()
			h.probeChainWG.Done()
		}()
		h.runDefaultFileProbeChain(ctx, contentID, &probeSource, streamsSnapshot)
	}()
}

// virtualProbeStreamSnapshot deep-copies the mutable members a probe reads
// from one listing entry. Shallow struct copies share the request-header map
// and the declared track slices with the discovery caller, so a later caller
// mutation could change or race a running probe; every member the chain
// consumes is cloned.
func virtualProbeStreamSnapshot(base VirtualPlaybackStream) VirtualPlaybackStream {
	clone := base
	clone.RequestHeaders = maps.Clone(base.RequestHeaders)
	clone.AudioLanguages = slices.Clone(base.AudioLanguages)
	clone.SubtitleLanguages = slices.Clone(base.SubtitleLanguages)
	return clone
}

// runDefaultFileProbeChain is the body of one admitted chain. Every step is
// bounded by the chain context; a failure or an unsafe target skips silently.
func (h *PlaybackHandler) runDefaultFileProbeChain(ctx context.Context, contentID string, source *models.MediaFile, streams []VirtualPlaybackStream) {
	if ctx.Err() != nil {
		return
	}
	// Re-read the persisted default. Discovery may have replaced the rows
	// between the listing and the persistence of this chain's inputs, so the
	// target is never the request-time row: it is whatever the catalog
	// serves now for this item.
	row, err := h.DefaultProbeTarget.DefaultVirtualProbeTarget(ctx, contentID)
	if err != nil || row == nil || ctx.Err() != nil {
		if err != nil && ctx.Err() == nil {
			slog.InfoContext(ctx, "virtual pre-play probe skipped: default version unavailable",
				"component", "api", "content_id", contentID, "error", err)
		}
		return
	}
	if !isVirtualPlaybackFile(row) {
		return
	}
	// Stamp skip and damper check happen inside admitted work — the admission
	// callback only reserves the slot, by design.
	if row.ProbeUpdatedAt != nil {
		return
	}
	oid := row.VirtualOwnerInstallationID
	probeCand := virtualDefaultProbeCandidate(row, source, streams)
	if probeCand == nil {
		// Without an exact byte match there is nothing safe to enumerate: the
		// default may be the neutral group row, and a neutral default may
		// resolve to a sibling candidate. The play path's own probe handles
		// those; a chain must never guess.
		return
	}
	if virtualProbeFailures.recent(virtualProbeFailureKey(probeCand.URI, oid)) {
		return
	}
	// The enumeration runs behind the shared detached gate, the same bounded
	// admission every background virtual probe uses. No slot, no probe.
	gate := h.detachedGate()
	if gate == nil || !gate.tryAcquire() {
		return
	}
	defer gate.release()
	expectedRuntimeMinutes := h.virtualExpectedRuntimeMinutes(ctx, row)
	stickyKey := bestResultCacheKey(contentID, virtualPlaybackNeutralKey(row.FilePath), oid)
	probeTransient := cloneVirtualProbeTransient(*row)
	// Zero the duration so the probe measures the empirical duration instead
	// of inheriting the catalog value.
	probeTransient.Duration = 0
	// The chain probes the default row's own candidate bytes (probeURL = the
	// row's persisted URI) and persists through the queued, coalescing
	// evidence path. No session binding exists here, so the fence is nil and
	// the identity tiers are zero — the same posture the unstamped-row fast
	// path takes, where the row owns its persisted candidate.
	h.probeVirtualSourceAndPersist(
		ctx, stickyKey, row, row.FilePath, probeTransient, *probeCand,
		resolver.PersistedIdentityTiers{}, expectedRuntimeMinutes, oid,
	)
}

// virtualDefaultProbeCandidate matches the default row's exact bytes against
// the listing the discovery run just persisted. An exact URI match is the only
// admissible enumeration target: the row's persisted identity must be the
// bytes the probe opens. The neutral group row itself is never admissible —
// the just-persisted listing necessarily contains it (under the same virtual
// path the group was listed from) without naming any specific release.
func virtualDefaultProbeCandidate(row *models.MediaFile, source *models.MediaFile, streams []VirtualPlaybackStream) *VirtualPlaybackStream {
	if row == nil || source == nil {
		return nil
	}
	target := strings.TrimSpace(row.FilePath)
	if target == "" {
		return nil
	}
	if source.FilePath == target {
		return nil
	}
	for i := range streams {
		if streams[i].URI == target && streams[i].OwnerInstallationID == row.VirtualOwnerInstallationID {
			return &streams[i]
		}
	}
	return nil
}
