package resolver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// tmdbStub is a counting stand-in for the TMDB external_ids endpoint.
type tmdbStub struct {
	server *httptest.Server

	mu    sync.Mutex
	count int
	paths []string
}

func newTMDBStub(t *testing.T, handler http.HandlerFunc) *tmdbStub {
	t.Helper()
	stub := &tmdbStub{}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.mu.Lock()
		stub.count++
		stub.paths = append(stub.paths, r.URL.Path)
		stub.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *tmdbStub) requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

// TestTMDBExternalIDsCachedAcrossResolves pins the translation cache: a
// repeated re-list of the same tmdb id must not pay another TMDB round-trip.
// The provider is re-fetched (unbounded re-list), but the tmdb -> imdb lookup
// is served from the cache.
func TestTMDBExternalIDsCachedAcrossResolves(t *testing.T) {
	tmdb := newTMDBStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"imdb_id":"tt83533"}`))
	})
	answer := []StreamCandidate{{URL: "https://cdn.example/one.mkv", Name: "One.Release.1080p"}}
	p := newCountingProvider(t, func(w http.ResponseWriter, r *http.Request) { writeStreams(w, answer) })
	cfg := testConfig(p)
	cfg.TMDBAPIKey = "testkey"
	cfg.TMDBBaseURL = tmdb.server.URL
	r := New(cfg)
	ctx := context.Background()

	first, _, _, err := r.GetCandidates(ctx, "virtual://movie/tmdb:83533")
	if err != nil || len(first) != 1 {
		t.Fatalf("first GetCandidates: count=%d err=%v, want 1", len(first), err)
	}
	if got := tmdb.requests(); got != 1 {
		t.Fatalf("TMDB requests = %d, want 1", got)
	}
	tmdb.mu.Lock()
	path := tmdb.paths[0]
	tmdb.mu.Unlock()
	if path != "/movie/83533/external_ids" {
		t.Fatalf("TMDB path = %q, want /movie/83533/external_ids", path)
	}

	// Bypass the candidate cache floor so the forced re-list reaches the
	// provider path again, which re-runs the tmdb translation.
	second, _, _, err := r.GetCandidatesFreshUnbounded(ctx, "virtual://movie/tmdb:83533")
	if err != nil || len(second) != 1 {
		t.Fatalf("second GetCandidates: count=%d err=%v, want 1", len(second), err)
	}
	if got := tmdb.requests(); got != 1 {
		t.Fatalf("TMDB requests = %d, want 1 (served from the translation cache)", got)
	}
	if got := p.requests(); got != 2 {
		t.Fatalf("provider requests = %d, want 2", got)
	}
}

// TestTMDBExternalIDsNegativeCached pins the error-path cache: a translation
// that returns no IMDb id is remembered briefly, so a burst of resolves fails
// fast instead of re-paying a doomed TMDB round-trip each time.
func TestTMDBExternalIDsNegativeCached(t *testing.T) {
	tmdb := newTMDBStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tvdb_id":42}`))
	})
	p := newCountingProvider(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("provider must not be contacted when the tmdb translation fails")
	})
	cfg := testConfig(p)
	cfg.TMDBAPIKey = "testkey"
	cfg.TMDBBaseURL = tmdb.server.URL
	r := New(cfg)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, _, _, err := r.GetCandidates(ctx, "virtual://movie/tmdb:83533"); err == nil {
			t.Fatalf("call %d: nil error, want the translation failure", i)
		}
	}
	if got := tmdb.requests(); got != 1 {
		t.Fatalf("TMDB requests = %d, want 1 (negative-cached after the first failure)", got)
	}
	if got := p.requests(); got != 0 {
		t.Fatalf("provider requests = %d, want 0", got)
	}
}

// TestTMDBExternalIDTimeoutBounded pins the dedicated translation timeout: a
// hung TMDB lookup is bounded well under the shared metadata client's 20s, and
// the timeout is a retryable provider failure — never a negative cache entry,
// because a slow TMDB says nothing about whether the title has an IMDb id.
func TestTMDBExternalIDTimeoutBounded(t *testing.T) {
	prev := tmdbExternalIDTimeout
	tmdbExternalIDTimeout = 50 * time.Millisecond
	t.Cleanup(func() { tmdbExternalIDTimeout = prev })

	tmdb := newTMDBStub(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	p := newCountingProvider(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("provider must not be contacted when the tmdb translation times out")
	})
	cfg := testConfig(p)
	cfg.TMDBAPIKey = "testkey"
	cfg.TMDBBaseURL = tmdb.server.URL
	r := New(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if _, _, _, err := r.GetCandidates(ctx, "virtual://movie/tmdb:83533"); err == nil {
		t.Fatal("nil error, want the translation timeout")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("translation blocked for %v, want the dedicated timeout to bound it", elapsed)
	}

	// Our own timeout fired while the caller's context is alive, so the
	// timeout is a genuine provider failure — but an availability-shaped one:
	// the next resolve must retry the lookup instead of serving a cached
	// "no IMDb id" failure.
	if _, _, _, err := r.GetCandidates(ctx, "virtual://movie/tmdb:83533"); err == nil || !errors.Is(err, ErrTMDBExternalIDLookupFailed) {
		t.Fatalf("second call error = %v, want the retryable lookup failure", err)
	}
	if got := tmdb.requests(); got != 2 {
		t.Fatalf("TMDB requests = %d, want 2 (a timeout is not negative-cached)", got)
	}
}

// TestTMDBExternalIDCallerCancellationNotCached pins that a lookup cut short by
// the caller's own deadline is not mistaken for a provider failure. Otherwise
// the watch-detail score path's 1.5s budget would poison the translation cache
// for every later resolve.
func TestTMDBExternalIDCallerCancellationNotCached(t *testing.T) {
	var heal atomic.Bool
	tmdb := newTMDBStub(t, func(w http.ResponseWriter, r *http.Request) {
		if heal.Load() {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"imdb_id":"tt83533"}`))
			return
		}
		<-r.Context().Done()
	})
	answer := []StreamCandidate{{URL: "https://cdn.example/one.mkv", Name: "One.Release.1080p"}}
	p := newCountingProvider(t, func(w http.ResponseWriter, r *http.Request) { writeStreams(w, answer) })
	cfg := testConfig(p)
	cfg.TMDBAPIKey = "testkey"
	cfg.TMDBBaseURL = tmdb.server.URL
	r := New(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, _, _, err := r.GetCandidates(ctx, "virtual://movie/tmdb:83533"); err == nil {
		t.Fatal("nil error, want the caller deadline to fail the lookup")
	}
	if got := tmdb.requests(); got != 1 {
		t.Fatalf("TMDB requests = %d, want 1", got)
	}

	heal.Store(true)
	got, _, _, err := r.GetCandidates(context.Background(), "virtual://movie/tmdb:83533")
	if err != nil || len(got) != 1 {
		t.Fatalf("healed GetCandidates: count=%d err=%v, want 1", len(got), err)
	}
	if requests := tmdb.requests(); requests != 2 {
		t.Fatalf("TMDB requests = %d, want 2 (caller cancellation must not be cached)", requests)
	}
}

// TestProviderFailureBackoffFailsFast pins the outage backoff: after a provider
// failure, a repeat resolve fails fast with the same ErrProviderUnavailable
// instead of re-paying the provider timeout, and a declared outage re-list
// still reaches the provider so recovery is never blocked.
func TestProviderFailureBackoffFailsFast(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	answer := []StreamCandidate{{URL: "https://cdn.example/one.mkv", Name: "One.Release.1080p"}}
	p := newCountingProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "provider down", http.StatusBadGateway)
			return
		}
		writeStreams(w, answer)
	})
	r := New(testConfig(p))
	ctx := context.Background()

	if _, _, _, err := r.GetCandidates(ctx, "virtual://movie/tt1"); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("first GetCandidates error = %v, want ErrProviderUnavailable", err)
	}
	if got := p.requests(); got != 1 {
		t.Fatalf("provider requests = %d, want 1", got)
	}

	// Within the backoff window the resolve fails fast and does not re-list.
	if _, _, _, err := r.GetCandidates(ctx, "virtual://movie/tt1"); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("backoff GetCandidates error = %v, want ErrProviderUnavailable", err)
	}
	if got := p.requests(); got != 1 {
		t.Fatalf("provider requests = %d, want 1 (backoff must not re-list)", got)
	}

	// A declared outage re-list bypasses the backoff and reaches the provider.
	if _, _, _, err := r.GetCandidatesFreshUnbounded(ctx, "virtual://movie/tt1"); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("outage re-list error = %v, want ErrProviderUnavailable", err)
	}
	if got := p.requests(); got != 2 {
		t.Fatalf("provider requests = %d, want 2 (outage re-list must reach the provider)", got)
	}

	// Once the backoff lapses and the provider recovers, the next resolve
	// fetches normally and clears the failure.
	r.cacheMu.Lock()
	r.failures["movie|tt1"] = time.Now().Add(-providerFailureBackoff - time.Second)
	r.cacheMu.Unlock()
	fail.Store(false)
	got, _, _, err := r.GetCandidates(ctx, "virtual://movie/tt1")
	if err != nil || len(got) != 1 {
		t.Fatalf("recovered GetCandidates: count=%d err=%v, want 1", len(got), err)
	}
	if got := p.requests(); got != 3 {
		t.Fatalf("provider requests = %d, want 3", got)
	}
	r.cacheMu.Lock()
	_, stillFailed := r.failures["movie|tt1"]
	r.cacheMu.Unlock()
	if stillFailed {
		t.Fatal("provider failure not cleared after a successful fetch")
	}
}

// TestJoinedFlightFailureDoesNotDoubleFetch pins that a provider error surfaced
// by a joined flight is returned directly, without a second full-timeout fetch
// on the request's context. One resolve must cost at most one provider request.
func TestJoinedFlightFailureDoesNotDoubleFetch(t *testing.T) {
	p := newCountingProvider(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "provider down", http.StatusBadGateway)
	})
	r := New(testConfig(p))
	ctx := context.Background()

	// Non-forced cold path and forced path each join the keyed flight once.
	if _, _, _, err := r.GetCandidates(ctx, "virtual://movie/tt1"); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("GetCandidates error = %v, want ErrProviderUnavailable", err)
	}
	if got := p.requests(); got != 1 {
		t.Fatalf("provider requests = %d, want 1 (no second fetch after the flight error)", got)
	}

	// Reset the backoff so the forced path actually attempts a fetch instead of
	// fast-failing; it must still make exactly one request.
	r.cacheMu.Lock()
	delete(r.failures, "movie|tt1")
	r.cacheMu.Unlock()
	if _, _, _, err := r.GetCandidatesFresh(ctx, "virtual://movie/tt1"); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("GetCandidatesFresh error = %v, want ErrProviderUnavailable", err)
	}
	if got := p.requests(); got != 2 {
		t.Fatalf("provider requests = %d, want 2 (one per forced resolve, no double fetch)", got)
	}
}

// TestConcurrentProviderFailuresCoalesce is the race-checked counterpart: a
// burst of cold resolves against a failing provider coalesces to one provider
// request and every caller observes the same provider error.
func TestConcurrentProviderFailuresCoalesce(t *testing.T) {
	release := make(chan struct{})
	firstHit := make(chan struct{})
	var once sync.Once
	p := newCountingProvider(t, func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(firstHit) })
		<-release
		http.Error(w, "provider down", http.StatusBadGateway)
	})
	r := New(testConfig(p))
	ctx := context.Background()

	const callers = 8
	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, _, errs[i] = r.GetCandidates(ctx, "virtual://movie/tt1")
		}(i)
	}

	select {
	case <-firstHit:
	case <-time.After(5 * time.Second):
		t.Fatal("provider was never called")
	}
	close(release)
	wg.Wait()

	if got := p.requests(); got != 1 {
		t.Fatalf("provider requests = %d, want 1 (coalesced)", got)
	}
	for i, err := range errs {
		if !errors.Is(err, ErrProviderUnavailable) {
			t.Fatalf("caller %d error = %v, want ErrProviderUnavailable", i, err)
		}
	}
}
