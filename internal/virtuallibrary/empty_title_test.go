package virtuallibrary

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A single cached answer repeated in a burst must never mark the title: the
// 2.6 s storm that marked 50 of 57 rows failed is exactly this shape.
func TestEmptyTitleTrackerBurstDoesNotMark(t *testing.T) {
	tracker := newEmptyTitleTracker()
	base := time.Now()
	for i := 0; i < 10; i++ {
		tracker.noteEmptyListing("virtual://series/tt1/2/3", 10*time.Minute, base.Add(time.Duration(i)*100*time.Millisecond))
	}
	if tracker.marked("virtual://series/tt1/2/3", 10*time.Minute, base.Add(time.Second)) {
		t.Fatal("burst of empties inside one cache entry marked the title")
	}
}

// Sustained emptiness across more than one cache TTL marks the title.
func TestEmptyTitleTrackerSustainedEmptinessMarks(t *testing.T) {
	tracker := newEmptyTitleTracker()
	base := time.Now()
	tracker.noteEmptyListing("k", 10*time.Minute, base)
	tracker.noteEmptyListing("k", 10*time.Minute, base.Add(6*time.Minute))
	if tracker.marked("k", 10*time.Minute, base.Add(6*time.Minute)) {
		t.Fatal("two empties marked the title")
	}
	if !tracker.noteEmptyListing("k", 10*time.Minute, base.Add(11*time.Minute)) {
		t.Fatal("third empty spanning the cache TTL did not mark the title")
	}
	if !tracker.marked("k", 10*time.Minute, base.Add(11*time.Minute)) {
		t.Fatal("marked() disagrees with the marking note")
	}
}

// A non-empty listing clears the streak immediately.
func TestEmptyTitleTrackerNonEmptyClears(t *testing.T) {
	tracker := newEmptyTitleTracker()
	base := time.Now()
	tracker.noteEmptyListing("k", 10*time.Minute, base)
	tracker.noteEmptyListing("k", 10*time.Minute, base.Add(6*time.Minute))
	tracker.noteNonEmptyListing("k")
	tracker.noteEmptyListing("k", 10*time.Minute, base.Add(12*time.Minute))
	if tracker.marked("k", 10*time.Minute, base.Add(12*time.Minute)) {
		t.Fatal("streak survived a non-empty listing")
	}
}

// The mark expires: a title that stays empty is re-proven, not pinned.
func TestEmptyTitleTrackerMarkExpires(t *testing.T) {
	tracker := newEmptyTitleTracker()
	base := time.Now()
	tracker.noteEmptyListing("k", time.Minute, base)
	tracker.noteEmptyListing("k", time.Minute, base.Add(2*time.Minute))
	tracker.noteEmptyListing("k", time.Minute, base.Add(3*time.Minute))
	if !tracker.marked("k", time.Minute, base.Add(3*time.Minute)) {
		t.Fatal("sustained emptiness did not mark")
	}
	if tracker.marked("k", time.Minute, base.Add(9*time.Minute)) {
		t.Fatal("mark outlived the TTL without fresh evidence")
	}
}

// Keys are per title: one title's outage must not mark another.
func TestEmptyTitleTrackerKeysArePerTitle(t *testing.T) {
	tracker := newEmptyTitleTracker()
	base := time.Now()
	for i := 0; i < 3; i++ {
		tracker.noteEmptyListing("a", time.Minute, base.Add(time.Duration(i*2)*time.Minute))
	}
	tracker.noteEmptyListing("b", time.Minute, base)
	if tracker.marked("b", time.Minute, base) {
		t.Fatal("empty listing for one title marked another")
	}
}

// A nil tracker never marks: zero-value Services stay safe.
func TestEmptyTitleTrackerNilSafe(t *testing.T) {
	var tracker *emptyTitleTracker
	if tracker.marked("k", time.Minute, time.Now()) {
		t.Fatal("nil tracker marked")
	}
	if tracker.noteEmptyListing("k", time.Minute, time.Now()) {
		t.Fatal("nil tracker reported a mark")
	}
	tracker.noteNonEmptyListing("k")
	if neutralVirtualPathKey("virtual://series/tt1/2/3?result=abc") != "virtual://series/tt1/2/3" {
		t.Fatal("neutral key keeps the result token")
	}
}

// A marked title fails fast without touching the provider.
func TestResolveDetailedMarkedTitleSkipsProvider(t *testing.T) {
	svc := &Service{cfg: Config{CacheTTLMinutes: 10}, emptyTitles: newEmptyTitleTracker()}
	base := time.Now()
	key := "virtual://series/tt1/2/3"
	for i := 0; i < 3; i++ {
		svc.noteEmptyTitleListing(key + "?result=abc")
	}
	// Backdate the streak so it spans the cache TTL without waiting.
	svc.emptyTitles.mu.Lock()
	st := svc.emptyTitles.streaks[key]
	st.first = base.Add(-11 * time.Minute)
	st.last = base
	svc.emptyTitles.mu.Unlock()
	if !svc.emptyTitleMarked(key + "?result=xyz") {
		t.Fatal("query string should not shard the mark")
	}
	_, err := svc.ResolveDetailed(context.Background(), key+"?result=xyz", false, nil, "", false)
	if !errors.Is(err, ErrTitleWithoutStreams) {
		t.Fatalf("marked resolve = %v, want %v", err, ErrTitleWithoutStreams)
	}
}

// A non-empty listing clears the mark so recovery is immediate.
func TestResolveDetailedNonEmptyClearsMark(t *testing.T) {
	svc := &Service{cfg: Config{CacheTTLMinutes: 10}, emptyTitles: newEmptyTitleTracker()}
	svc.noteNonEmptyTitleListing("virtual://series/tt1/2/3?result=abc")
	if svc.emptyTitleMarked("virtual://series/tt1/2/3") {
		t.Fatal("fresh service reports a mark")
	}
}
