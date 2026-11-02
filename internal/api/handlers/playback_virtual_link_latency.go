package handlers

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

const (
	// copySeekAnchorResolveLatencyScale multiplies the link's slowest recent
	// provider observation into one copy-video seek-anchor resolve budget. A
	// resolve pays a pinned-relay lookup comparable to a provider listing and
	// then an FFmpeg probe over the same pipe, so twice the observed round trip
	// covers both legs; see copySeekAnchorResolveBudget.
	copySeekAnchorResolveLatencyScale = 2
	// virtualProviderLinkLatencyWindow bounds how long one observation keeps
	// sizing a link's budgets. The relay that was slow a minute ago is the same
	// relay this start will read, so a short window carries the measurement
	// across the client retry that follows a failed start without letting an
	// old stall size budgets indefinitely.
	virtualProviderLinkLatencyWindow = 2 * time.Minute
	// virtualProviderLinkLatencyLimit bounds the tracker. Entries expire on
	// record and read, so a large install cannot accumulate one per virtual
	// source owner forever.
	virtualProviderLinkLatencyLimit = 256
)

// copySeekAnchorSeekResolveTimeoutCap bounds how far a slow link may extend one
// copy-video seek-anchor resolve attempt. It stays well inside the replan
// budget (the caller's own deadline still bounds every attempt), and a relay
// that is stuck rather than slow must not hold the seek past it. The cap is a
// var so tests can shorten it.
var copySeekAnchorSeekResolveTimeoutCap = 20 * time.Second

// copySeekAnchorResolveBudget derives one seek-anchor resolve attempt's bound
// from the link's slowest recent provider observation. The floor is today's
// fixed budget (copySeekAnchorSeekResolveTimeout), so a link that answers
// inside it keeps exactly the old behavior; a healthy-but-slow relay — one
// whose own listings or anchor resolves recently took longer than the fixed
// budget — gets twice that observation instead, capped at maxBudget. The
// budget is never shortened below the floor.
func copySeekAnchorResolveBudget(floor, maxBudget, recent time.Duration) time.Duration {
	if floor <= 0 {
		floor = copySeekAnchorSeekResolveTimeout
	}
	if maxBudget < floor {
		maxBudget = floor
	}
	if recent <= 0 {
		return floor
	}
	scaled := copySeekAnchorResolveLatencyScale * recent
	if scaled <= floor {
		return floor
	}
	if scaled > maxBudget {
		return maxBudget
	}
	return scaled
}

// copySeekAnchorResolveBudgetFor returns the resolve budget for one attempt on
// this file's source. The observation is keyed on the provider link the source
// is served through, so a fast link is unaffected by another link's stall.
func copySeekAnchorResolveBudgetFor(file *models.MediaFile) time.Duration {
	return copySeekAnchorResolveBudget(
		copySeekAnchorSeekResolveTimeout,
		copySeekAnchorSeekResolveTimeoutCap,
		recentVirtualProviderLinkLatency(virtualProviderLinkLatencyKey(file)),
	)
}

// copySeekAnchorAttemptFits reports whether the caller's remaining budget can
// hold one more anchor resolve attempt at budget, plus the scheduling margin
// process startup, probe-slot waits, and result handling need. A retry that
// only half fits would be truncated by the caller deadline and surface that
// deadline instead of the anchor failure, so it is skipped instead. A caller
// with no deadline keeps the retry.
func copySeekAnchorAttemptFits(ctx context.Context, budget time.Duration) (bool, time.Duration) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return true, 0
	}
	remaining := time.Until(deadline)
	return remaining > budget+copySeekProbeRetryMargin, remaining
}

// virtualProviderLinkLatencyKey names the provider link a source is served
// through. The virtual source owner installation is the identity relay
// resolution and provider listing already key on, and one slow owner answers
// slowly for every title and sibling release it lists. A local file has no
// provider link, so it never inherits another link's measurement.
func virtualProviderLinkLatencyKey(file *models.MediaFile) string {
	if file == nil || file.VirtualOwnerInstallationID <= 0 {
		return ""
	}
	return strconv.Itoa(file.VirtualOwnerInstallationID)
}

// virtualProviderLinkLatencyMark is the slowest provider observation a link
// served inside the window, stamped when it was observed.
type virtualProviderLinkLatencyMark struct {
	slowest    time.Duration
	observedAt time.Time
}

// virtualProviderLinkLatency remembers the slowest recent provider round trip
// per link. Two sources feed it: the resolver's provider listing durations (the
// same measurement the start path already attributes) and completed anchor
// resolve attempts on the link. The slow end is kept deliberately — the anchor
// start must fit the pipe the provider just proved it has, and a later faster
// answer must not retract that room. Tests may clear entries directly.
var virtualProviderLinkLatency = struct {
	sync.Mutex
	marks map[string]virtualProviderLinkLatencyMark
}{marks: make(map[string]virtualProviderLinkLatencyMark)}

// recordVirtualProviderLinkLatency records one observed provider round trip for
// key at now. A key with no provider link is ignored, and an observation only
// replaces the mark when it is slower, so the mark's own timestamp keeps the
// window anchored to the measurement that set it.
func recordVirtualProviderLinkLatency(key string, observed time.Duration, now time.Time) {
	if key == "" || observed <= 0 {
		return
	}
	virtualProviderLinkLatency.Lock()
	defer virtualProviderLinkLatency.Unlock()
	mark, ok := virtualProviderLinkLatency.marks[key]
	if ok && now.Sub(mark.observedAt) < virtualProviderLinkLatencyWindow && mark.slowest >= observed {
		return
	}
	if !ok && len(virtualProviderLinkLatency.marks) >= virtualProviderLinkLatencyLimit {
		for k, entry := range virtualProviderLinkLatency.marks {
			if now.Sub(entry.observedAt) >= virtualProviderLinkLatencyWindow {
				delete(virtualProviderLinkLatency.marks, k)
			}
		}
	}
	virtualProviderLinkLatency.marks[key] = virtualProviderLinkLatencyMark{slowest: observed, observedAt: now}
}

// recentVirtualProviderLinkLatency returns the slowest provider observation the
// link served inside the window, or 0 when nothing recent was measured. A
// lapsed mark is dropped on read so the map stays bounded.
func recentVirtualProviderLinkLatency(key string) time.Duration {
	if key == "" {
		return 0
	}
	virtualProviderLinkLatency.Lock()
	defer virtualProviderLinkLatency.Unlock()
	mark, ok := virtualProviderLinkLatency.marks[key]
	if !ok {
		return 0
	}
	if time.Since(mark.observedAt) >= virtualProviderLinkLatencyWindow {
		delete(virtualProviderLinkLatency.marks, key)
		return 0
	}
	return mark.slowest
}
