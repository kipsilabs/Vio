package playback

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Relay demux admission: bounds concurrent heavy subtitle reads against one
// remote relay source.
//
// A virtual playback session reads the same slow relay pipe with several
// independent consumers: every window extract, the detached full-track warm,
// the subtitle font dump, and layout ffprobes. The video transcode reads it
// too, but that one is never admitted here (playback must not wait on
// subtitle work). Letting all the subtitle reads pile onto the pipe does not
// make any of them faster — the pipe is the bottleneck — it makes each one
// lose to its own timeout instead: a window overruns the client's fetch
// deadline, and the warm's 5 s layout probe times out while the window it
// exists to validate is still reading. The guard admits a small number of
// subtitle demuxes per relay source so those consumers pace each other
// instead of starving each other.
//
// Admission has two classes:
//
//   - Request-path reads (a window serve, a font serve) are what a client is
//     waiting on. They wait a short, bounded time for a slot and, when none
//     frees up, report ErrRelayDemuxBusy so the caller answers the retryable
//     shape it already supports (503 + Retry-After, or the pending empty font
//     bundle). Making a client sit out a 20-70 s demux inside its own fetch
//     deadline is what turns a slow relay into a failed fetch.
//   - Detached reads (full-track warms, font warms, layout probes) are
//     opportunistic. They never queue: when the source is saturated they are
//     dropped and retried by the next trigger (the next windowed miss, warm
//     admission, or client fetch).
//
// Local-file reads are not admitted at all: they do not share the relay pipe
// and are the cheap paths the guard exists to protect.

// ErrRelayDemuxBusy reports that the relay source backing an input is already
// at its concurrent subtitle-demux ceiling. Callers translate it into their
// own retryable shape; it is not a source failure and must not arm a failure
// cooldown.
var ErrRelayDemuxBusy = errors.New("relay demux admission: source saturated")

const (
	// relayDemuxSlotsPerSource caps concurrent subtitle demuxes per remote
	// source. Two slots let one client-facing read and one opportunistic read
	// move at once: a single slot would serialize a household's two
	// simultaneous subtitle requests, and more than two lets the detached
	// warm, font dump, and probe compete with the window the client is waiting
	// on. The video transcode is deliberately outside this budget.
	relayDemuxSlotsPerSource = 2

	// relayDemuxRequestWait bounds how long a request-path read waits for a
	// slot before answering retryable. A read that holds a slot longer than
	// this is a multi-second remote demux; the client is better served by its
	// existing retry loop than by holding the connection inside its own fetch
	// deadline.
	relayDemuxRequestWait = 750 * time.Millisecond
)

// relayDemuxClass selects how an admission request treats a saturated source.
type relayDemuxClass int

const (
	// relayDemuxRequestPath waits the short bound, then reports busy.
	relayDemuxRequestPath relayDemuxClass = iota
	// relayDemuxDetached never waits: it skips a saturated source.
	relayDemuxDetached
)

// relayDemuxClassKey carries the admission class through a context whose
// origin (a request or a detached goroutine) is not otherwise visible to the
// shared font and probe entry points.
type relayDemuxClassKey struct{}

// relayDemuxSlotKey marks a context that already holds a demux slot.
type relayDemuxSlotKey struct{}

// relayDemuxGuard bounds concurrent subtitle demuxes per remote source. It is
// safe for concurrent use.
type relayDemuxGuard struct {
	limit int
	wait  time.Duration

	mu    sync.Mutex
	slots map[string]chan struct{}
}

func newRelayDemuxGuard(limit int, wait time.Duration) *relayDemuxGuard {
	if limit <= 0 {
		limit = 1
	}
	return &relayDemuxGuard{limit: limit, wait: wait, slots: make(map[string]chan struct{})}
}

// relayDemuxGuardState holds the process-wide guard. A server relays through
// its own listener, so production holds one entry in the slot map; test and
// multi-upstream fixtures add one entry per host and never grow unbounded. It
// is an atomic pointer so a test can install a narrower guard without racing
// an in-flight admission.
var relayDemuxGuardState atomic.Pointer[relayDemuxGuard]

// activeRelayDemuxGuard returns the process-wide guard, installing the
// production one on first use.
func activeRelayDemuxGuard() *relayDemuxGuard {
	if guard := relayDemuxGuardState.Load(); guard != nil {
		return guard
	}
	guard := newRelayDemuxGuard(relayDemuxSlotsPerSource, relayDemuxRequestWait)
	if relayDemuxGuardState.CompareAndSwap(nil, guard) {
		return guard
	}
	return relayDemuxGuardState.Load()
}

// semaphore returns the slot channel for a relay source, creating it on first
// use.
func (g *relayDemuxGuard) semaphore(source string) chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	sem, ok := g.slots[source]
	if !ok {
		sem = make(chan struct{}, g.limit)
		g.slots[source] = sem
	}
	return sem
}

// acquire takes one slot for input and returns its release. ok is false when
// the read must be skipped or answered retryable. A local input is always
// admitted with a no-op release.
func (g *relayDemuxGuard) acquire(ctx context.Context, input string, class relayDemuxClass) (func(), bool) {
	source, remote := relayDemuxSource(input)
	if !remote {
		return func() {}, true
	}
	sem := g.semaphore(source)
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, true
	default:
	}
	if class == relayDemuxDetached {
		return nil, false
	}
	timer := time.NewTimer(g.wait)
	defer timer.Stop()
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, true
	case <-timer.C:
		return nil, false
	case <-ctx.Done():
		return nil, false
	}
}

// relayDemuxSource reduces a remote input URL to the relay source the bound
// applies to: scheme and host. A local filesystem path (or a non-HTTP input,
// such as a cached extracted artifact) has no source and is never admitted.
// The relay URL path carries a per-registration token, so it is not part of
// the key: every read a session performs through the relay shares one pipe.
func relayDemuxSource(input string) (string, bool) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "", false
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", false
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", false
	}
	if parsed.Host == "" {
		return "", false
	}
	return scheme + "://" + strings.ToLower(parsed.Host), true
}

// detachedRelayDemux marks ctx as detached admission: opportunistic work that
// must skip a saturated source instead of queueing behind request-path reads.
func detachedRelayDemux(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, relayDemuxClassKey{}, relayDemuxDetached)
}

// relayDemuxClassOf reports how ctx was admitted.
func relayDemuxClassOf(ctx context.Context) relayDemuxClass {
	if ctx == nil {
		return relayDemuxRequestPath
	}
	if class, ok := ctx.Value(relayDemuxClassKey{}).(relayDemuxClass); ok {
		return class
	}
	return relayDemuxRequestPath
}

// acquireRelayDemuxRead admits one heavy read of input and returns a context
// that carries the slot plus its release. The returned context must be the one
// handed to the extraction, so nested work (a layout probe inside an admitted
// warm) is recognized and does not take a second slot: a context that already
// holds one is admitted with a no-op release, which also makes a nested read
// unable to deadlock against its own parent.
func acquireRelayDemuxRead(ctx context.Context, input string) (context.Context, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Value(relayDemuxSlotKey{}) != nil {
		return ctx, func() {}, nil
	}
	if _, remote := relayDemuxSource(input); !remote {
		return ctx, func() {}, nil
	}
	release, ok := activeRelayDemuxGuard().acquire(ctx, input, relayDemuxClassOf(ctx))
	if !ok {
		return ctx, func() {}, ErrRelayDemuxBusy
	}
	return context.WithValue(ctx, relayDemuxSlotKey{}, struct{}{}), release, nil
}
