package virtuallibrary

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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

// An expired entry is pruned on read, and the next empty listing starts a
// fresh streak instead of inheriting the stale count and span.
func TestEmptyTitleTrackerExpiredEntryIsPruned(t *testing.T) {
	tracker := newEmptyTitleTracker()
	base := time.Now()
	tracker.noteEmptyListing("k", time.Minute, base)
	tracker.noteEmptyListing("k", time.Minute, base.Add(2*time.Minute))
	tracker.noteEmptyListing("k", time.Minute, base.Add(3*time.Minute))
	if !tracker.marked("k", time.Minute, base.Add(3*time.Minute)) {
		t.Fatal("sustained emptiness did not mark")
	}
	after := base.Add(3*time.Minute + emptyTitleMarkTTL + time.Second)
	if tracker.marked("k", time.Minute, after) {
		t.Fatal("mark outlived the TTL without fresh evidence")
	}
	tracker.mu.Lock()
	_, retained := tracker.streaks["k"]
	tracker.mu.Unlock()
	if retained {
		t.Fatal("expired tracker entry was not pruned")
	}
	if tracker.noteEmptyListing("k", time.Minute, after) {
		t.Fatal("one empty listing inherited the expired streak's count and re-marked the title")
	}
	if tracker.marked("k", time.Minute, after) {
		t.Fatal("expired streak re-marked the title")
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

// markEmptyTitleForTest drives a Service into the fail-fast marked state
// without waiting out the cache span.
func markEmptyTitleForTest(t *testing.T, svc *Service, virtualPath string) {
	t.Helper()
	key := neutralVirtualPathKey(virtualPath)
	for i := 0; i < emptyTitleMarkStreak; i++ {
		svc.noteEmptyTitleListing(virtualPath)
	}
	now := time.Now()
	svc.emptyTitles.mu.Lock()
	streak := svc.emptyTitles.streaks[key]
	streak.first = now.Add(-11 * time.Minute)
	streak.last = now
	svc.emptyTitles.mu.Unlock()
	if !svc.emptyTitleMarked(virtualPath) {
		t.Fatal("fixture did not mark the title")
	}
}

// recoveryServiceForMarkedTitle serves a manifest and a non-empty stream list,
// so a marked title can be proven recovered without waiting for the TTL.
func recoveryServiceForMarkedTitle(t *testing.T) *Service {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/manifest.json" {
			_, _ = w.Write([]byte(`{"id":"org.stremio.test","resources":["stream"],"types":["movie","series"]}`))
			return
		}
		_, _ = w.Write([]byte(`{"streams":[{"name":"1080p","title":"First","url":"http://192.168.1.100:8080/stream1.mkv"},{"name":"720p","title":"Second","url":"http://192.168.1.100:8080/stream2.mkv"}]}`))
	}))
	t.Cleanup(server.Close)
	svc := New(Config{
		Enabled:             true,
		ManifestURL:         server.URL + "/manifest.json",
		AllowInsecureHTTP:   true,
		AllowPrivateStreams: true,
		CacheTTLMinutes:     10,
	}, nil, nil)
	if svc == nil {
		t.Fatal("expected non-nil service")
	}
	return svc
}

// A forced refresh is an explicit re-list and must bypass the fail-fast mark,
// so a title whose streams returned recovers immediately rather than waiting
// out emptyTitleMarkTTL.
func TestResolveDetailedForceRefreshBypassesMark(t *testing.T) {
	svc := recoveryServiceForMarkedTitle(t)
	key := "virtual://movie/tt-recover"
	markEmptyTitleForTest(t, svc, key)

	resolved, err := svc.ResolveDetailed(context.Background(), key, true, nil, "", false)
	if err != nil {
		t.Fatalf("forced resolve of a marked title = %v, want immediate recovery", err)
	}
	if resolved.URI == "" {
		t.Fatal("forced resolve returned no stream URI")
	}
	if svc.emptyTitleMarked(key) {
		t.Fatal("forced recovery did not clear the fail-fast mark")
	}
	// The ordinary path now sees the cleared mark and the cached listing.
	if _, err := svc.ResolveDetailed(context.Background(), key, false, nil, "", false); err != nil {
		t.Fatalf("resolve after forced recovery = %v, want success", err)
	}
}

// The version-list path observes streams too, so a non-empty listing there must
// clear the mark and let the very next playback resolve without a forced
// refresh.
func TestListStreamsNonEmptyClearsMark(t *testing.T) {
	svc := recoveryServiceForMarkedTitle(t)
	key := "virtual://movie/tt-recover"
	markEmptyTitleForTest(t, svc, key)

	streams, err := svc.ListStreamsFresh(context.Background(), key)
	if err != nil || len(streams) == 0 {
		t.Fatalf("ListStreamsFresh = %d streams, err %v; want a non-empty listing", len(streams), err)
	}
	if svc.emptyTitleMarked(key) {
		t.Fatal("a non-empty list did not clear the fail-fast mark")
	}
	if _, err := svc.ResolveDetailed(context.Background(), key, false, nil, "", false); err != nil {
		t.Fatalf("resolve after a non-empty list = %v, want success", err)
	}
}
