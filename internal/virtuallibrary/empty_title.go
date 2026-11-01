package virtuallibrary

import (
	"errors"
	"strings"
	"sync"
	"time"
)

// ErrTitleWithoutStreams marks a resolve that was refused without contacting
// the provider because consecutive fresh listings for the title came back
// empty. It is a dependency condition, not a verdict about any release: the
// provider may simply have nothing cached for the title right now. Callers
// must treat it like a transient provider-listing outage (retryable, no
// verdicts, no rotation), never as a dead pin.
var ErrTitleWithoutStreams = errors.New("virtual title has no provider streams")

// emptyTitleMarkStreak is how many consecutive empty listings declare a title
// currently unservable. Counting consecutive answers (rather than failures)
// keeps one hiccup from marking anything; the span rule below keeps a single
// cached answer repeated in a burst from counting as consecutive evidence.
// The 2.6 s burst that marked 50 of 57 rows failed is exactly what this
// pairing refuses to repeat: bursts share one cache entry and one span.
const emptyTitleMarkStreak = 3

// emptyTitleMarkTTL bounds how long a title stays marked after its last empty
// listing. A listing that observes streams again clears the streak immediately,
// and an explicit forceRefresh bypasses the mark to re-list; the TTL only caps
// how long a mark can outlive its evidence when nothing else revisits the
// title.
var emptyTitleMarkTTL = 5 * time.Minute

// maxEmptyTitleEntries caps the tracker map so a long-lived server cannot
// accumulate cold entries indefinitely.
const maxEmptyTitleEntries = 2048

// emptyTitleStreak records consecutive empty provider listings for one title.
type emptyTitleStreak struct {
	count int
	first time.Time
	last  time.Time
}

// emptyTitleTracker tracks consecutive empty listings per title so a title
// whose provider answers keep coming back empty fails fast instead of paying
// a full alternate walk per start. All methods are safe for concurrent use;
// a nil receiver is a no-op tracker that never marks. The tracker is node-local
// in-process state with no shared admission: another node, or a restart,
// re-lists the title normally.
type emptyTitleTracker struct {
	mu      sync.Mutex
	streaks map[string]*emptyTitleStreak
}

func (t *emptyTitleTracker) sweepExpired(now time.Time) {
	for k, s := range t.streaks {
		if s == nil || s.last.IsZero() || now.Sub(s.last) > emptyTitleMarkTTL {
			delete(t.streaks, k)
		}
	}
}

func newEmptyTitleTracker() *emptyTitleTracker {
	return &emptyTitleTracker{streaks: make(map[string]*emptyTitleStreak)}
}

// neutralVirtualPathKey reduces a virtual:// URI to the title it lists so
// per-candidate result= tokens do not shard one title's streak.
func neutralVirtualPathKey(virtualPath string) string {
	if i := strings.IndexByte(virtualPath, '?'); i >= 0 {
		return virtualPath[:i]
	}
	return virtualPath
}

func (streak *emptyTitleStreak) expired(now time.Time) bool {
	return streak.count >= emptyTitleMarkStreak && (streak.last.IsZero() || now.Sub(streak.last) > emptyTitleMarkTTL)
}

func (t *emptyTitleTracker) ensureCapacity(now time.Time) {
	if len(t.streaks) < maxEmptyTitleEntries {
		return
	}
	t.sweepExpired(now)
	if len(t.streaks) < maxEmptyTitleEntries {
		return
	}
	var oldestKey string
	var oldestTime time.Time
	for k, s := range t.streaks {
		if oldestKey == "" || (s != nil && s.last.Before(oldestTime)) {
			oldestKey = k
			if s != nil {
				oldestTime = s.last
			}
		}
	}
	if oldestKey != "" {
		delete(t.streaks, oldestKey)
	}
}

// noteEmptyListing records one empty provider listing for the title and
// reports whether the title is now marked without streams.
func (t *emptyTitleTracker) noteEmptyListing(key string, cacheTTL time.Duration, now time.Time) bool {
	if t == nil || key == "" {
		return false
	}
	required := cacheTTL
	if required < time.Minute {
		required = time.Minute
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	streak, ok := t.streaks[key]
	// An expired MARK is no evidence for the new listing: start a fresh streak
	// instead of letting its old count and span re-mark the title from one
	// observation. An unmarked streak is still accumulating evidence.
	if !ok || streak.expired(now) {
		if !ok {
			t.ensureCapacity(now)
		}
		streak = &emptyTitleStreak{}
		t.streaks[key] = streak
	}
	streak.count++
	if streak.count == 1 {
		streak.first = now
	}
	streak.last = now
	return streak.count >= emptyTitleMarkStreak && !streak.first.IsZero() && streak.last.Sub(streak.first) >= required
}

// noteNonEmptyListing clears any streak: the provider has streams again.
func (t *emptyTitleTracker) noteNonEmptyListing(key string) {
	if t == nil || key == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.streaks, key)
}

// marked reports whether the title is currently marked without streams: a
// qualifying streak whose last empty listing is still inside the mark TTL.
func (t *emptyTitleTracker) marked(key string, cacheTTL time.Duration, now time.Time) bool {
	if t == nil || key == "" {
		return false
	}
	required := cacheTTL
	if required < time.Minute {
		required = time.Minute
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	streak, ok := t.streaks[key]
	if !ok {
		return false
	}
	// Expiry-on-read prunes both completed marks and stale partial streaks
	// whose evidence has lapsed, so cold titles are not retained indefinitely.
	if streak == nil || streak.last.IsZero() || now.Sub(streak.last) > emptyTitleMarkTTL {
		delete(t.streaks, key)
		return false
	}
	if streak.count < emptyTitleMarkStreak {
		return false
	}
	return !streak.first.IsZero() && streak.last.Sub(streak.first) >= required
}

// listingCacheTTL returns the provider listing cache TTL that bounds how fast
// genuine emptiness can be re-proven: answers inside one cache entry cannot
// corroborate each other.
func (s *Service) listingCacheTTL() time.Duration {
	if s == nil {
		return 0
	}
	return time.Duration(s.cfg.CacheTTLMinutes) * time.Minute
}

// emptyTitleMarked reports whether the title currently has no servable
// streams: enough consecutive empty listings spanning more than one cache
// entry. Safe on a nil or untracked service (never marked).
func (s *Service) emptyTitleMarked(virtualPath string) bool {
	if s == nil {
		return false
	}
	return s.emptyTitles.marked(neutralVirtualPathKey(virtualPath), s.listingCacheTTL(), time.Now())
}

// noteEmptyTitleListing records one empty provider listing for the title.
// It returns true when this listing newly marks the title, which is logged
// once; repeat marks stay silent.
func (s *Service) noteEmptyTitleListing(virtualPath string) bool {
	if s == nil {
		return false
	}
	key := neutralVirtualPathKey(virtualPath)
	ttl := s.listingCacheTTL()
	now := time.Now()
	was := s.emptyTitles.marked(key, ttl, now)
	marked := s.emptyTitles.noteEmptyListing(key, ttl, now)
	if marked && !was && s.logger != nil {
		s.logger.Info("virtual title has no provider streams; failing fast until streams return",
			"title", key)
	}
	return marked
}

// noteNonEmptyTitleListing clears any empty streak: the provider has streams
// again (or a listing finally answered).
func (s *Service) noteNonEmptyTitleListing(virtualPath string) {
	if s == nil {
		return
	}
	s.emptyTitles.noteNonEmptyListing(neutralVirtualPathKey(virtualPath))
}
