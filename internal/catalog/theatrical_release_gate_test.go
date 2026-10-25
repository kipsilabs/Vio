package catalog

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeDigitalReleaseChecker struct {
	released map[int]bool
	err      error
	calls    map[int]int
}

func (f *fakeDigitalReleaseChecker) HasDigitalRelease(_ context.Context, tmdbID int) (bool, error) {
	if f.calls == nil {
		f.calls = map[int]int{}
	}
	f.calls[tmdbID]++
	if f.err != nil {
		return false, f.err
	}
	return f.released[tmdbID], nil
}

// recordingReleaseChecker is a race-safe checker for prefetch concurrency
// tests. It records every distinct id and can block until the test releases
// it, so cancellation can be observed while work is in flight.
type recordingReleaseChecker struct {
	mu       sync.Mutex
	calls    map[int]int
	released map[int]bool
	err      error
	gate     chan struct{}
	started  chan int
}

func (c *recordingReleaseChecker) HasDigitalRelease(ctx context.Context, tmdbID int) (bool, error) {
	c.mu.Lock()
	if c.calls == nil {
		c.calls = map[int]int{}
	}
	c.calls[tmdbID]++
	c.mu.Unlock()
	if c.started != nil {
		select {
		case c.started <- tmdbID:
		default:
		}
	}
	if c.gate != nil {
		select {
		case <-c.gate:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	if c.err != nil {
		return false, c.err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.released[tmdbID], nil
}

func (c *recordingReleaseChecker) callCount(tmdbID int) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[tmdbID]
}

func (c *recordingReleaseChecker) distinctIDs() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

func TestTheatricalReleaseGateSkipsTheatricalOnlyMovies(t *testing.T) {
	checker := &fakeDigitalReleaseChecker{released: map[int]bool{
		100: false, // theatrical-only
		200: true,  // digitally released
	}}
	gate := newTheatricalReleaseGate(checker)
	ctx := context.Background()

	if !gate.skipTheatricalMovie(ctx, 100, "", "Theatrical Movie", 0, "") {
		t.Fatal("a theatrical-only movie must be skipped")
	}
	if gate.skipTheatricalMovie(ctx, 200, "", "Digital Movie", 0, "") {
		t.Fatal("a digitally released movie must not be skipped")
	}
}

func TestTheatricalReleaseGateDefersInconclusiveEvidence(t *testing.T) {
	for _, checker := range []TMDBDigitalReleaseChecker{nil, &fakeDigitalReleaseChecker{err: errors.New("tmdb down")}} {
		gate := newTheatricalReleaseGate(checker)
		tracker := &collectionVirtualCreationTracker{}
		ctx := context.WithValue(context.Background(), collectionVirtualCreationTrackerKey{}, tracker)
		if gate.skipTheatricalMovie(ctx, 300, "", "Unavailable", 0, "") {
			t.Fatal("the prefilter must not reject on inconclusive evidence; the authoritative materialization decision fails closed")
		}
		if tracker.err != nil {
			t.Fatalf("inconclusive prefilter evidence must not poison the sync: %v", tracker.err)
		}
		if _, err := gate.lookup(context.Background(), 300); err == nil {
			t.Fatal("authoritative lookup must still fail closed on missing evidence")
		}
	}
}

func TestTheatricalReleaseGateSkipsLookupWithoutTMDBID(t *testing.T) {
	checker := &fakeDigitalReleaseChecker{released: map[int]bool{}}
	gate := newTheatricalReleaseGate(checker)
	// Without any identity the prefilter cannot decide; the authoritative
	// materialization path fails closed instead of the prefilter dropping
	// the entry.
	if gate.skipTheatricalMovie(context.Background(), 0, "", "No TMDB ID", 0, "") {
		t.Fatal("entries without a TMDB ID must defer to the authoritative decision")
	}
	if len(checker.calls) != 0 {
		t.Fatalf("lookup calls = %d, want 0 for missing TMDB ID", len(checker.calls))
	}
}

func TestTheatricalReleaseGatePrefilterUsesFullIdentitySet(t *testing.T) {
	ctx := context.Background()
	imdb := ReleaseIdentity{MediaType: "movie", Provider: "imdb", ProviderID: "tt1234567"}

	// A past IMDb override unblocks a TMDB entry the provider reports as
	// theatrical-only.
	past := time.Now().UTC().Add(-time.Hour)
	lookup := memoryReleaseOverrides{imdb: {ReleaseIdentity: imdb, ReleaseAt: &past, Revision: 1}}
	gate := newTheatricalReleaseGate(&fakeDigitalReleaseChecker{released: map[int]bool{4242: false}}, lookup)
	if gate.skipTheatricalMovie(ctx, 4242, "tt1234567", "Alias Override", 2020, "") {
		t.Fatal("past IMDb override must unblock the TMDB-listed entry")
	}

	// A future IMDb override blocks a TMDB entry the provider reports as
	// released.
	future := time.Now().UTC().Add(time.Hour)
	lookup[imdb] = ReleaseOverride{ReleaseIdentity: imdb, ReleaseAt: &future, Revision: 2}
	gate = newTheatricalReleaseGate(&fakeDigitalReleaseChecker{released: map[int]bool{4242: true}}, lookup)
	if !gate.skipTheatricalMovie(ctx, 4242, "tt1234567", "Alias Block", 2020, "") {
		t.Fatal("future IMDb override must block the TMDB-listed entry")
	}

	// An IMDb-only entry with a past override defers nothing and is not
	// rejected for the missing TMDB identity.
	pastOnly := memoryReleaseOverrides{imdb: {ReleaseIdentity: imdb, ReleaseAt: &past, Revision: 1}}
	gate = newTheatricalReleaseGate(&fakeDigitalReleaseChecker{err: errors.New("tmdb down")}, pastOnly)
	if gate.skipTheatricalMovie(ctx, 0, "tt1234567", "IMDb Only", 2020, "") {
		t.Fatal("past IMDb override must carry an IMDb-only entry")
	}
}

func TestTheatricalReleaseGateCanonicalConflictFailsClosed(t *testing.T) {
	ctx := context.Background()
	conflict := errors.New("tmdb \"1\" and imdb \"tt1\" resolve to 2 distinct movies")
	gate := newTheatricalReleaseGate(&fakeDigitalReleaseChecker{released: map[int]bool{1: true}})
	gate.canonicalIDs = func(_ context.Context, tmdbID int, imdbID string) (int, string, error) {
		return tmdbID, imdbID, conflict
	}
	tracker := &collectionVirtualCreationTracker{}
	gatedCtx := context.WithValue(ctx, collectionVirtualCreationTrackerKey{}, tracker)
	// Even with provider evidence of release, a canonical conflict must
	// skip the entry and surface the error on the tracker.
	if !gate.skipTheatricalMovie(gatedCtx, 1, "tt1", "Conflict", 2020, "") {
		t.Fatal("canonical conflict must skip the entry")
	}
	if !errors.Is(tracker.err, conflict) && (tracker.err == nil || !strings.Contains(tracker.err.Error(), "distinct movies")) {
		t.Fatalf("canonical conflict must poison the sync: %v", tracker.err)
	}
}

func TestTheatricalReleaseGateMemoizesLookupsPerRun(t *testing.T) {
	checker := &fakeDigitalReleaseChecker{released: map[int]bool{400: false}}
	gate := newTheatricalReleaseGate(checker)
	ctx := context.Background()

	for range 3 {
		if !gate.skipTheatricalMovie(ctx, 400, "", "Memoized", 0, "") {
			t.Fatal("theatrical-only movie must be skipped on every call")
		}
	}
	if checker.calls[400] != 1 {
		t.Fatalf("checker calls = %d, want 1 (memoized per sync run)", checker.calls[400])
	}
}

func TestReleaseLookupFailurePreventsCollectionAcceptance(t *testing.T) {
	for _, tc := range []struct {
		name    string
		checker TMDBDigitalReleaseChecker
		id      int
	}{
		{"outage", &fakeDigitalReleaseChecker{err: errors.New("offline")}, 100},
		{"no checker", nil, 100},
		{"no identity", &fakeDigitalReleaseChecker{}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tracker := &collectionVirtualCreationTracker{}
			ctx := context.WithValue(context.Background(), collectionVirtualCreationTrackerKey{}, tracker)
			gate := newTheatricalReleaseGate(tc.checker)
			if gate.skipTheatricalMovie(ctx, tc.id, "", "Backlog", 2000, "") {
				t.Fatal("inconclusive prefilter evidence must defer, not reject")
			}
			// The authoritative materialization decision still fails closed
			// and poisons the sync through the tracker.
			tracker.err = ErrProviderUnavailable
			service := &LibraryCollectionService{}
			if _, err := service.acceptCollectionItems(ctx, nil, nil); !errors.Is(err, ErrProviderUnavailable) {
				t.Fatalf("acceptance reached storage with incomplete evidence: %v", err)
			}
		})
	}
}

func TestReleaseLookupErrorIsNotMemoizedAsUnreleased(t *testing.T) {
	checker := &fakeDigitalReleaseChecker{err: errors.New("offline"), released: map[int]bool{100: true}}
	gate := newTheatricalReleaseGate(checker)
	if released, err := gate.lookup(context.Background(), 100); released || err == nil {
		t.Fatalf("lookup = %v, %v", released, err)
	}
	checker.err = nil
	if released, err := gate.lookup(context.Background(), 100); !released || err != nil {
		t.Fatalf("retry = %v, %v", released, err)
	}
}

func TestTheatricalReleaseGateChecksBacklogMovies(t *testing.T) {
	checker := &fakeDigitalReleaseChecker{}
	gate := newTheatricalReleaseGate(checker)
	for _, date := range []string{"2010-07-16", time.Now().AddDate(0, 0, -200).Format("2006-01-02")} {
		if !gate.skipTheatricalMovie(context.Background(), 500, "", "Old Movie", 2010, date) {
			t.Fatal("old releases still require home release evidence")
		}
	}
	if checker.calls[500] != 1 {
		t.Fatalf("calls = %v", checker.calls)
	}
}

func TestIsUnreleasedYearOrDate(t *testing.T) {
	currentYear := time.Now().UTC().Year()

	tests := []struct {
		name        string
		year        int
		releaseDate string
		want        bool
	}{
		{
			name:        "future year is unreleased",
			year:        currentYear + 2,
			releaseDate: "",
			want:        true,
		},
		{
			name:        "past year with no date is released",
			year:        currentYear - 2,
			releaseDate: "",
			want:        false,
		},
		{
			name:        "current year with past festival date is unreleased",
			year:        currentYear,
			releaseDate: fmt.Sprintf("%d-09-05", currentYear-1), // TIFF previous year
			want:        true,
		},
		{
			name:        "future year with past festival date is unreleased",
			year:        currentYear + 1,
			releaseDate: fmt.Sprintf("%d-09-05", currentYear-1),
			want:        true,
		},
		{
			name:        "past year with matching past date is released",
			year:        currentYear - 1,
			releaseDate: fmt.Sprintf("%d-05-15", currentYear-1),
			want:        false,
		},
		{
			name:        "future release date is unreleased",
			year:        currentYear,
			releaseDate: fmt.Sprintf("%d-12-31", currentYear+1),
			want:        true,
		},
		{
			name:        "future release date with timestamp is unreleased",
			year:        currentYear,
			releaseDate: fmt.Sprintf("%d-12-31T00:00:00Z", currentYear+1),
			want:        true,
		},
		{
			name:        "past release date earlier this year is released",
			year:        currentYear,
			releaseDate: fmt.Sprintf("%d-01-01", currentYear),
			want:        false,
		},
		{
			name:        "undated movie is unreleased",
			year:        0,
			releaseDate: "",
			want:        true,
		},
		{
			name:        "malformed date with zero year is unreleased",
			year:        0,
			releaseDate: "unknown-date",
			want:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isUnreleasedYearOrDate(tt.year, tt.releaseDate)
			if got != tt.want {
				t.Fatalf("isUnreleasedYearOrDate(%d, %q) = %v, want %v", tt.year, tt.releaseDate, got, tt.want)
			}
		})
	}
}

func TestTMDBEntryIsUnreleased(t *testing.T) {
	currentYear := time.Now().UTC().Year()

	tests := []struct {
		name  string
		entry TMDBCollectionEntry
		want  bool
	}{
		{
			name:  "empty release date is unreleased",
			entry: TMDBCollectionEntry{Title: "Placeholder", ReleaseDate: ""},
			want:  true,
		},
		{
			name:  "future release date is unreleased",
			entry: TMDBCollectionEntry{Title: "Upcoming Film", ReleaseDate: fmt.Sprintf("%d-11-20", currentYear+1)},
			want:  true,
		},
		{
			name:  "future release date with timestamp is unreleased",
			entry: TMDBCollectionEntry{Title: "Upcoming Film", ReleaseDate: fmt.Sprintf("%d-11-20T00:00:00Z", currentYear+1)},
			want:  true,
		},
		{
			name:  "past release date for regular movie is released",
			entry: TMDBCollectionEntry{Title: "Released Film", ReleaseDate: fmt.Sprintf("%d-01-15", currentYear-1)},
			want:  false,
		},
		{
			name: "past festival date with current year in title is unreleased",
			entry: TMDBCollectionEntry{
				Title:       fmt.Sprintf("Fuze (%d)", currentYear),
				ReleaseDate: fmt.Sprintf("%d-09-05", currentYear-1), // TIFF premiere in prior year
			},
			want: true,
		},
		{
			name: "past festival date with future year in title is unreleased",
			entry: TMDBCollectionEntry{
				Title:       fmt.Sprintf("Future Film (%d)", currentYear+1),
				ReleaseDate: fmt.Sprintf("%d-09-05", currentYear-1),
			},
			want: true,
		},
		{
			name:  "unparseable date is unreleased",
			entry: TMDBCollectionEntry{Title: "Corrupt Date", ReleaseDate: "not-a-date"},
			want:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tmdbEntryIsUnreleased(tt.entry)
			if got != tt.want {
				t.Fatalf("tmdbEntryIsUnreleased(%+v) = %v, want %v", tt.entry, got, tt.want)
			}
		})
	}
}

type contextAwareDigitalReleaseChecker struct {
	hasDigitalRelease func(ctx context.Context, tmdbID int) (bool, error)
}

func (c *contextAwareDigitalReleaseChecker) HasDigitalRelease(ctx context.Context, tmdbID int) (bool, error) {
	return c.hasDigitalRelease(ctx, tmdbID)
}

func TestTheatricalReleaseGateLookupContextCanceledFailsClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	gate := newTheatricalReleaseGate(&contextAwareDigitalReleaseChecker{
		hasDigitalRelease: func(ctx context.Context, tmdbID int) (bool, error) {
			return false, ctx.Err()
		},
	})
	if gate.skipTheatricalMovie(ctx, 300, "", "Canceled Context", 0, "") {
		t.Fatal("the prefilter must defer canceled lookups to the authoritative decision")
	}
	if _, err := gate.lookup(ctx, 300); err == nil {
		t.Fatal("a checker context cancellation must fail closed at the authoritative lookup")
	}
}

func TestReleaseLookupHasBoundedContext(t *testing.T) {
	gate := newTheatricalReleaseGate(&contextAwareDigitalReleaseChecker{
		hasDigitalRelease: func(ctx context.Context, _ int) (bool, error) {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 5*time.Second {
				t.Fatal("release lookup has no bounded deadline")
			}
			return true, nil
		},
	})
	if released, err := gate.lookup(context.Background(), 1); !released || err != nil {
		t.Fatalf("lookup = %v, %v", released, err)
	}
}

func TestTheatricalReleaseGateNilContextDoesNotPanic(t *testing.T) {
	gate := newTheatricalReleaseGate(&contextAwareDigitalReleaseChecker{
		hasDigitalRelease: func(ctx context.Context, tmdbID int) (bool, error) {
			return true, nil
		},
	})
	//nolint:staticcheck // documents the resolver's explicit nil-context tolerance
	if gate.skipTheatricalMovie(nil, 300, "", "Nil Context", 0, "") {
		t.Fatal("expected released movie to not be skipped with nil context")
	}
}

func TestTheatricalGatePrefetchWarmsEachDistinctID(t *testing.T) {
	checker := &recordingReleaseChecker{released: map[int]bool{101: true, 102: false, 103: true}}
	gate := newTheatricalReleaseGate(checker)
	ids := []int{101, 102, 103}

	gate.prefetch(context.Background(), ids)

	if got := checker.distinctIDs(); got != 3 {
		t.Fatalf("checker distinct ids = %d, want 3", got)
	}
	for _, id := range ids {
		if got := checker.callCount(id); got != 1 {
			t.Fatalf("checker calls for %d = %d, want 1", id, got)
		}
		// The sequential pass must observe the warmed memo without a new call.
		if _, err := gate.lookupProvider(context.Background(), id); err != nil {
			t.Fatalf("lookupProvider(%d) after prefetch: %v", id, err)
		}
		if got := checker.callCount(id); got != 1 {
			t.Fatalf("checker calls for %d after memo hit = %d, want 1", id, got)
		}
	}
}

func TestTheatricalGatePrefetchDeduplicatesDuplicateIDs(t *testing.T) {
	checker := &recordingReleaseChecker{released: map[int]bool{200: true}}
	gate := newTheatricalReleaseGate(checker)

	gate.prefetch(context.Background(), []int{200, 200, 200})

	if got := checker.distinctIDs(); got != 1 {
		t.Fatalf("checker distinct ids = %d, want 1", got)
	}
	if got := checker.callCount(200); got != 1 {
		t.Fatalf("checker calls for 200 = %d, want 1 (deduped and memoized)", got)
	}
}

func TestTheatricalGatePrefetchFailsOpenOnCheckerError(t *testing.T) {
	checker := &recordingReleaseChecker{err: errors.New("tmdb down")}
	gate := newTheatricalReleaseGate(checker)

	// Prefetch itself must never panic or block on a failing provider.
	gate.prefetch(context.Background(), []int{300, 301})

	// A provider error is not cached as "unreleased": the sequential gate
	// defers (not gated) rather than rejecting the entry.
	if gate.skipTheatricalMovie(context.Background(), 300, "", "Outage", 2000, "") {
		t.Fatal("prefetch error must fail open, not gate the movie")
	}
	entryCalls := checker.callCount(300)
	if entryCalls < 2 {
		t.Fatalf("checker calls for 300 = %d, want a retry after the swallowed prefetch error", entryCalls)
	}
}

func TestTheatricalGatePrefetchStopsOnCancellation(t *testing.T) {
	release := make(chan struct{})
	checker := &recordingReleaseChecker{released: map[int]bool{}, gate: release, started: make(chan int, 1)}
	gate := newTheatricalReleaseGate(checker)

	ctx, cancel := context.WithCancel(context.Background())
	ids := []int{400, 401, 402, 403, 404, 405, 406, 407, 408, 409, 410, 411}
	done := make(chan struct{})
	go func() {
		gate.prefetch(ctx, ids)
		close(done)
	}()

	// Wait until at least one worker is inside the checker, then cancel.
	select {
	case <-checker.started:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("prefetch never issued a lookup")
	}
	cancel()
	close(release)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("prefetch did not return promptly after cancellation")
	}
	if got := checker.distinctIDs(); got >= len(ids) {
		t.Fatalf("prefetch queried %d/%d ids after early cancellation, want fewer", got, len(ids))
	}
}

func TestTheatricalGatePrefetchWithoutCheckerIsNoop(t *testing.T) {
	gate := newTheatricalReleaseGate(nil)
	// Must return immediately and not spin up workers against a nil checker.
	gate.prefetch(context.Background(), []int{1, 2, 3})
}

func TestTheatricalGateMemoConcurrentPrefetchAndLookup(t *testing.T) {
	checker := &recordingReleaseChecker{released: map[int]bool{500: true, 501: false, 502: true}}
	gate := newTheatricalReleaseGate(checker)

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gate.prefetch(context.Background(), []int{500, 501, 502, 503, 504, 505})
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, id := range []int{500, 501, 502} {
				if _, err := gate.lookupProvider(context.Background(), id); err != nil {
					t.Errorf("lookupProvider(%d): %v", id, err)
				}
			}
		}()
	}
	wg.Wait()

	for _, id := range []int{500, 501, 502, 503, 504, 505} {
		if got := checker.callCount(id); got != 1 {
			t.Fatalf("checker calls for %d = %d, want 1 (memo + singleflight)", id, got)
		}
	}
}
