package handlers

import (
	"context"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// seekAnchorRefusalWindow is how long a retryable copy-video seek-anchor
	// refusal names the next start for the same profile, requested file and
	// target as the viewer's retry. The retry card is the one affordance the
	// viewer is offered, and the client re-issues a fresh start at the refused
	// target. There is no wire field that says "this is the retry", so the
	// server remembers the refusal it just answered. The window matches the
	// anchor cache TTL: a retry older than the anchor evidence it would reuse
	// is no longer the same attempt.
	seekAnchorRefusalWindow = 10 * time.Minute
	// seekAnchorRefusalTargetTolerance accepts the small drift between the
	// timeline target the server recorded on the failed replan and the
	// start_position the client re-sends for it. A different position is a new
	// seek, not a retry of the refused one.
	seekAnchorRefusalTargetTolerance = 1.0
	// seekAnchorRefusalLimit bounds the tracker. Entries expire on read and
	// record, so a large installation cannot accumulate one per refused seek
	// forever.
	seekAnchorRefusalLimit = 256
)

// seekAnchorRefusalMark is the target one (profile, requested file) pair was
// last refused a copy-video seek anchor at.
type seekAnchorRefusalMark struct {
	target     float64
	observedAt time.Time
}

// seekAnchorRefusalTracker remembers which requested seek targets were just
// refused a copy-video seek anchor with a retryable verdict. The next start
// for the same profile, requested file and target is the viewer's retry: it
// gets one extended probe window instead of repeating the first attempt's
// budget. The target must match, so a later track change, quality change or
// ordinary start at another position never inherits the retry.
type seekAnchorRefusalTracker struct {
	mu    sync.Mutex
	marks map[string]seekAnchorRefusalMark
	now   func() time.Time
}

func (t *seekAnchorRefusalTracker) clock() time.Time {
	if t != nil && t.now != nil {
		return t.now()
	}
	return time.Now()
}

func seekAnchorRefusalKey(profileID string, requestedFileID int) string {
	if requestedFileID <= 0 {
		return ""
	}
	return profileID + "\x00" + strconv.Itoa(requestedFileID)
}

// record notes the target a retryable seek-anchor refusal was answered for.
// The latest refusal wins: the retry card names one target, so an older one
// must not be escalated by a later unrelated retry.
func (t *seekAnchorRefusalTracker) record(key string, target float64) {
	if t == nil || key == "" || target <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.marks == nil {
		t.marks = make(map[string]seekAnchorRefusalMark)
	}
	now := t.clock()
	for mark, entry := range t.marks {
		if now.Sub(entry.observedAt) >= seekAnchorRefusalWindow {
			delete(t.marks, mark)
		}
	}
	if len(t.marks) >= seekAnchorRefusalLimit {
		for mark := range t.marks {
			delete(t.marks, mark)
			break
		}
	}
	t.marks[key] = seekAnchorRefusalMark{target: target, observedAt: now}
}

// take consumes the refusal for key when target is the position that was
// refused. A consumed mark cannot escalate a second time: one refusal gets
// one extended attempt, and a third start for the same target is an ordinary
// first attempt again.
func (t *seekAnchorRefusalTracker) take(key string, target float64) bool {
	if t == nil || key == "" || target <= 0 {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	entry, ok := t.marks[key]
	if !ok {
		return false
	}
	if t.clock().Sub(entry.observedAt) >= seekAnchorRefusalWindow {
		delete(t.marks, key)
		return false
	}
	if math.Abs(entry.target-target) > seekAnchorRefusalTargetTolerance {
		return false
	}
	delete(t.marks, key)
	return true
}

// seekAnchorRetryGrantV3 is the once-only permission a start carries when it
// answers a recorded refusal. prepareTransportTimelineV3 may run for several
// candidates in one start (an alternate-version walk); only the first one may
// spend the extended window.
type seekAnchorRetryGrantV3 struct {
	consumed atomic.Bool
}

func (g *seekAnchorRetryGrantV3) consume() bool {
	return g != nil && g.consumed.CompareAndSwap(false, true)
}

type seekAnchorRetryGrantKeyV3 struct{}

func withSeekAnchorRetryGrantV3(ctx context.Context, grant *seekAnchorRetryGrantV3) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, seekAnchorRetryGrantKeyV3{}, grant)
}

func seekAnchorRetryGrantFromContextV3(ctx context.Context) *seekAnchorRetryGrantV3 {
	if ctx == nil {
		return nil
	}
	grant, _ := ctx.Value(seekAnchorRetryGrantKeyV3{}).(*seekAnchorRetryGrantV3)
	return grant
}

// recordCopySeekAnchorRefusalV3 remembers a retryable copy-video seek-anchor
// refusal so the viewer's next start for the same requested file and target is
// treated as the retry it is. Only the existing transcode_start_failed seek
// card is recorded: other failures have their own remedies and must not
// inherit this one.
func (h *PlaybackHandler) recordCopySeekAnchorRefusalV3(profileID string, requestedFileID int, target float64, failure *transportErrorV3) {
	if h == nil || h.copySeekAnchorRefusals == nil {
		return
	}
	if failure == nil || !failure.retryable || !failure.seekFailure || failure.reason != transcodeStartFailedReasonV3 {
		return
	}
	h.copySeekAnchorRefusals.record(seekAnchorRefusalKey(profileID, requestedFileID), target)
}

// copySeekAnchorRetryContextV3 turns a viewer's retry into a context grant for
// prepareTransportTimelineV3. The retry is identified by the refusal this
// handler just recorded: the same profile, requested file and target, matched
// within the tolerance and consumed once. It deliberately does not require
// force_relink. That flag asks the provider for a fresh pin, which is the last
// thing a seek retry needs: the refused seek is on the source already playing,
// and a forced provider re-query can fail in resolve before the anchor probe
// this grant exists to extend. A fresh start at the refused target is the
// retry, and the consumed mark keeps an unrelated later start from inheriting
// it.
func (h *PlaybackHandler) copySeekAnchorRetryContextV3(ctx context.Context, profileID string, requestedFileID int, target float64) (context.Context, bool) {
	if h == nil || h.copySeekAnchorRefusals == nil || target <= 0 {
		return ctx, false
	}
	if !h.copySeekAnchorRefusals.take(seekAnchorRefusalKey(profileID, requestedFileID), target) {
		return ctx, false
	}
	return withSeekAnchorRetryGrantV3(ctx, &seekAnchorRetryGrantV3{}), true
}
