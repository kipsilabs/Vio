package resolver

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

// fetchObserver collects ProviderFetchEvents for assertions. It is safe for the
// concurrent fetches the resolver can run under one observed context.
type fetchObserver struct {
	mu     sync.Mutex
	events []ProviderFetchEvent
}

func (o *fetchObserver) observe(ev ProviderFetchEvent) {
	o.mu.Lock()
	o.events = append(o.events, ev)
	o.mu.Unlock()
}

func (o *fetchObserver) snapshot() []ProviderFetchEvent {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]ProviderFetchEvent(nil), o.events...)
}

func oneRelease(name string) []StreamCandidate {
	return []StreamCandidate{{URL: "https://cdn.example/one.mkv", Name: name}}
}

// A cold discovery reports exactly one fetch event with the listing reason and
// the provider's candidate count; a second resolve of the same key is a cache
// hit and reports nothing, so a repeat start can prove zero redundant
// discovery from the observer rather than from a wall-clock guess.
func TestProviderFetchObserverReportsColdDiscoveryThenReuse(t *testing.T) {
	p := newCountingProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeStreams(w, oneRelease("One.Release.1080p"))
	})
	r := New(testConfig(p))
	obs := &fetchObserver{}
	ctx := WithProviderFetchObserver(context.Background(), obs.observe)

	for i := 0; i < 2; i++ {
		if _, _, _, err := r.GetCandidates(ctx, "virtual://movie/tt1"); err != nil {
			t.Fatalf("GetCandidates #%d: %v", i+1, err)
		}
	}

	events := obs.snapshot()
	if len(events) != 1 {
		t.Fatalf("fetch events = %d, want 1 (second resolve must be a cache hit)", len(events))
	}
	ev := events[0]
	if ev.Reason != FetchReasonListing {
		t.Fatalf("reason = %q, want %q", ev.Reason, FetchReasonListing)
	}
	if ev.Count != 1 {
		t.Fatalf("count = %d, want 1", ev.Count)
	}
	if ev.CacheKey != "movie|tt1" {
		t.Fatalf("cache key = %q, want movie|tt1", ev.CacheKey)
	}
	if ev.Err != nil {
		t.Fatalf("err = %v, want nil", ev.Err)
	}
	if ev.Duration <= 0 {
		t.Fatalf("duration = %v, want a positive measurement", ev.Duration)
	}
	if got := p.requests(); got != 1 {
		t.Fatalf("provider requests = %d, want 1", got)
	}
}

// Concurrent identical cold requests join one flight, so the observer must see
// exactly one fetch event even though every caller is served.
func TestProviderFetchObserverCoalescesConcurrentFetches(t *testing.T) {
	release := make(chan struct{})
	firstHit := make(chan struct{})
	var once sync.Once
	p := newCountingProvider(t, func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(firstHit) })
		<-release
		writeStreams(w, oneRelease("One.Release.1080p"))
	})
	r := New(testConfig(p))
	obs := &fetchObserver{}
	ctx := WithProviderFetchObserver(context.Background(), obs.observe)

	const callers = 6
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _, _ = r.GetCandidates(ctx, "virtual://movie/tt1")
		}()
	}
	select {
	case <-firstHit:
	case <-time.After(5 * time.Second):
		t.Fatal("provider was never called")
	}
	close(release)
	wg.Wait()

	if got := len(obs.snapshot()); got != 1 {
		t.Fatalf("fetch events = %d, want 1 coalesced discovery", got)
	}
	if got := p.requests(); got != 1 {
		t.Fatalf("provider requests = %d, want 1", got)
	}
}

// A forced re-list after the cache was aged reports a second event tagged
// forced, so an explicit refresh is distinguishable from a redundant cold
// discovery.
func TestProviderFetchObserverReportsForcedReason(t *testing.T) {
	answer := &mutableStreams{}
	answer.set(oneRelease("One.Release.1080p"))
	p := newCountingProvider(t, func(w http.ResponseWriter, r *http.Request) { writeStreams(w, answer.get()) })
	r := New(testConfig(p))
	obs := &fetchObserver{}
	ctx := WithProviderFetchObserver(context.Background(), obs.observe)

	if _, _, _, err := r.GetCandidates(ctx, "virtual://movie/tt1"); err != nil {
		t.Fatalf("GetCandidates: %v", err)
	}
	answer.set([]StreamCandidate{{URL: "https://cdn.example/two.mkv", Name: "Two.Release.1080p"}})
	// Unbounded bypasses the fresh-serve floor, so the re-list is a genuine
	// forced discovery rather than a cache serve.
	if _, _, _, err := r.GetCandidatesFreshUnbounded(ctx, "virtual://movie/tt1"); err != nil {
		t.Fatalf("GetCandidatesFreshUnbounded: %v", err)
	}

	events := obs.snapshot()
	if len(events) != 2 {
		t.Fatalf("fetch events = %d, want 2", len(events))
	}
	if events[1].Reason != FetchReasonForced {
		t.Fatalf("second reason = %q, want %q", events[1].Reason, FetchReasonForced)
	}
}

// A failed provider fetch reports one event carrying the error, so an operator
// can count discovery failures separately from successful discovery.
func TestProviderFetchObserverReportsFailedFetch(t *testing.T) {
	p := newCountingProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	r := New(testConfig(p))
	obs := &fetchObserver{}
	ctx := WithProviderFetchObserver(context.Background(), obs.observe)

	if _, _, _, err := r.GetCandidates(ctx, "virtual://movie/tt1"); err == nil {
		t.Fatal("GetCandidates succeeded, want a provider error")
	}

	events := obs.snapshot()
	if len(events) != 1 {
		t.Fatalf("fetch events = %d, want 1", len(events))
	}
	if events[0].Err == nil {
		t.Fatal("event err = nil, want the provider failure")
	}
	if events[0].Count != 0 {
		t.Fatalf("count = %d, want 0 on a failed fetch", events[0].Count)
	}
}

// A canceled caller of an in-flight cold fetch must not poison the key: the
// detached flight still lands in the cache, so the next attempt is a cache hit
// with no second discovery.
func TestProviderFetchObserverCancellationDoesNotPoisonLaterAttempt(t *testing.T) {
	release := make(chan struct{})
	firstHit := make(chan struct{})
	var once sync.Once
	p := newCountingProvider(t, func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(firstHit) })
		<-release
		writeStreams(w, oneRelease("One.Release.1080p"))
	})
	r := New(testConfig(p))
	obs := &fetchObserver{}

	callerCtx, cancel := context.WithCancel(WithProviderFetchObserver(context.Background(), obs.observe))
	done := make(chan error, 1)
	go func() {
		_, _, _, err := r.GetCandidates(callerCtx, "virtual://movie/tt1")
		done <- err
	}()
	select {
	case <-firstHit:
	case <-time.After(5 * time.Second):
		t.Fatal("provider was never called")
	}
	cancel()
	close(release)
	if err := <-done; err == nil {
		t.Fatal("canceled caller returned nil error, want ctx cancellation")
	}

	// The flight completed despite the canceled caller; the next attempt is
	// served from the cache with zero new discovery.
	if _, _, _, err := r.GetCandidates(context.Background(), "virtual://movie/tt1"); err != nil {
		t.Fatalf("post-cancel GetCandidates: %v", err)
	}
	if got := p.requests(); got != 1 {
		t.Fatalf("provider requests = %d, want 1 (cancellation must not trigger a refetch)", got)
	}
	if got := len(obs.snapshot()); got != 1 {
		t.Fatalf("fetch events = %d, want 1", got)
	}
}

// An entry that has aged past stale grace is invalidated deterministically: the
// next ordinary resolve re-discovers and reports a listing event again.
func TestProviderFetchObserverFetchesAgainAfterEntryExpires(t *testing.T) {
	answer := &mutableStreams{}
	answer.set(oneRelease("One.Release.1080p"))
	p := newCountingProvider(t, func(w http.ResponseWriter, r *http.Request) { writeStreams(w, answer.get()) })
	r := New(testConfig(p))
	obs := &fetchObserver{}
	ctx := WithProviderFetchObserver(context.Background(), obs.observe)

	if _, _, _, err := r.GetCandidates(ctx, "virtual://movie/tt1"); err != nil {
		t.Fatalf("GetCandidates: %v", err)
	}
	mutateCacheEntry(t, r, "movie|tt1", func(entry *candidateCacheEntry) {
		entry.expiresAt = time.Now().Add(-candidateStaleGrace - time.Minute)
	})
	answer.set([]StreamCandidate{{URL: "https://cdn.example/two.mkv", Name: "Two.Release.1080p"}})

	got, _, _, err := r.GetCandidates(ctx, "virtual://movie/tt1")
	if err != nil || len(got) != 1 {
		t.Fatalf("post-expiry GetCandidates: count=%d err=%v, want 1", len(got), err)
	}
	if got[0].URL != "https://cdn.example/two.mkv" {
		t.Fatalf("post-expiry URL = %q, want the refetched answer", got[0].URL)
	}
	events := obs.snapshot()
	if len(events) != 2 {
		t.Fatalf("fetch events = %d, want 2 (expired entry must re-discover)", len(events))
	}
}

// A resolver with no observer installed must not panic or change behavior.
func TestProviderFetchObserverAbsentIsNoop(t *testing.T) {
	p := newCountingProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeStreams(w, oneRelease("One.Release.1080p"))
	})
	r := New(testConfig(p))
	if _, _, _, err := r.GetCandidates(context.Background(), "virtual://movie/tt1"); err != nil {
		t.Fatalf("GetCandidates without an observer: %v", err)
	}
	if got := p.requests(); got != 1 {
		t.Fatalf("provider requests = %d, want 1", got)
	}
}
