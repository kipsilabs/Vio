package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// TestRunVirtualVersionFallbackCandidatesDeadCandidatesResolveInParallel proves
// the walk fans out instead of resolving dead candidates one at a time. Every
// resolver blocks until all workers have entered, so a serial runner could never
// release them and would burn the per-listing budget on the first candidate.
// The gate is pure synchronization: the test never sleeps to order the workers.
func TestRunVirtualVersionFallbackCandidatesDeadCandidatesResolveInParallel(t *testing.T) {
	const workers = 4
	candidates := []*models.MediaFile{{ID: 1}, {ID: 2}, {ID: 3}, {ID: 4}}

	var started atomic.Int32
	var missingDeadline atomic.Int32
	var overBudgetDeadline atomic.Int32
	release := make(chan struct{})
	resolve := func(ctx context.Context, _ *models.MediaFile) (resolvedVirtualPlaybackSource, bool) {
		deadline, ok := ctx.Deadline()
		switch {
		case !ok:
			missingDeadline.Add(1)
		case time.Until(deadline) > virtualStartVersionFallbackListingBudget:
			overBudgetDeadline.Add(1)
		}
		if started.Add(1) == workers {
			close(release)
		}
		select {
		case <-release:
		case <-ctx.Done():
			return resolvedVirtualPlaybackSource{}, false
		}
		return resolvedVirtualPlaybackSource{}, false
	}

	decisionCtx, cancel := context.WithTimeout(context.Background(), virtualStartVersionFallbackDecisionBudget)
	defer cancel()
	start := time.Now()
	_, ok := runVirtualVersionFallbackCandidatesV3(decisionCtx, candidates, workers, virtualStartVersionFallbackListingBudget, resolve)
	elapsed := time.Since(start)

	if ok {
		t.Fatal("every candidate failed; want the all-failed terminal, not a winner")
	}
	if elapsed >= virtualStartVersionFallbackListingBudget/2 {
		t.Fatalf("dead-candidate walk took %s; want parallel fan-out well under one listing budget (%s)", elapsed, virtualStartVersionFallbackListingBudget)
	}
	if missingDeadline.Load() != 0 {
		t.Fatalf("%d resolve(s) ran without a per-listing deadline", missingDeadline.Load())
	}
	if overBudgetDeadline.Load() != 0 {
		t.Fatalf("%d resolve(s) ran with a deadline beyond the listing budget %s", overBudgetDeadline.Load(), virtualStartVersionFallbackListingBudget)
	}
}

// TestRunVirtualVersionFallbackCandidatesFirstHealthyWinsCancelsRest proves the
// walk returns the first healthy candidate and cancels the stragglers already
// in flight: each dead candidate observes context cancellation after the winner
// instead of running to its own listing timeout. The winner waits on a gate
// that opens only once every dead candidate has entered its resolve, so the
// cancellation is observed for every straggler and the test needs no sleeps to
// order the workers.
func TestRunVirtualVersionFallbackCandidatesFirstHealthyWinsCancelsRest(t *testing.T) {
	healthy := &models.MediaFile{ID: 7}
	dead := []*models.MediaFile{{ID: 1}, {ID: 2}, {ID: 3}}
	candidates := []*models.MediaFile{dead[0], healthy, dead[1], dead[2]}

	var deadStarted atomic.Int32
	allDeadStarted := make(chan struct{})
	cancelled := make(chan int, len(dead))
	resolve := func(ctx context.Context, file *models.MediaFile) (resolvedVirtualPlaybackSource, bool) {
		if file.ID == healthy.ID {
			// Hold the winner until the whole fan-out wave is in flight, so
			// every dead candidate is a straggler that must be canceled.
			<-allDeadStarted
			return resolvedVirtualPlaybackSource{URI: "virtual://movie/movie-tt-wins?result=healthy", File: file}, true
		}
		if deadStarted.Add(1) == int32(len(dead)) {
			close(allDeadStarted)
		}
		<-ctx.Done()
		cancelled <- file.ID
		return resolvedVirtualPlaybackSource{}, false
	}

	winner, ok := runVirtualVersionFallbackCandidatesV3(context.Background(), candidates, 4, virtualStartVersionFallbackListingBudget, resolve)
	if !ok {
		t.Fatal("a healthy candidate was present; want the winner")
	}
	if winner.File == nil || winner.File.ID != healthy.ID {
		t.Fatalf("winner = %#v, want the healthy candidate %d", winner, healthy.ID)
	}
	// The three dead candidates must all observe the winner's cancellation.
	for i := 0; i < len(dead); i++ {
		select {
		case <-cancelled:
		case <-time.After(time.Second):
			t.Fatal("a straggler was not canceled after the first healthy candidate won")
		}
	}
}

// TestRunVirtualVersionFallbackCandidatesTerminalOnlyAfterAllFail proves the
// terminal verdict is reached only once every candidate has been attempted. A
// runner that returned after the first failure would leave later candidates
// untried and drop a recoverable start.
func TestRunVirtualVersionFallbackCandidatesTerminalOnlyAfterAllFail(t *testing.T) {
	candidates := []*models.MediaFile{{ID: 1}, {ID: 2}, {ID: 3}, {ID: 4}, {ID: 5}}
	var attempted atomic.Int32
	resolve := func(context.Context, *models.MediaFile) (resolvedVirtualPlaybackSource, bool) {
		attempted.Add(1)
		return resolvedVirtualPlaybackSource{}, false
	}

	_, ok := runVirtualVersionFallbackCandidatesV3(context.Background(), candidates, 2, virtualStartVersionFallbackListingBudget, resolve)
	if ok {
		t.Fatal("every candidate failed; want the terminal, not a winner")
	}
	if got := attempted.Load(); got != int32(len(candidates)) {
		t.Fatalf("attempted %d candidate(s), want all %d before the terminal", got, len(candidates))
	}
}

// TestResolveVirtualStartWithVersionFallbackDeadPinsResolveInParallel exercises
// the fan-out through the real walk with four dead pinned versions. Each
// alternate's provider call blocks until all four have entered, so a serial
// walk could not release them and would spend a full listing budget on the
// first; the parallel walk terminals well inside half a listing budget.
func TestResolveVirtualStartWithVersionFallbackDeadPinsResolveInParallel(t *testing.T) {
	resetVirtualRecoveryRelists(t)
	const (
		content = "movie-tt-parallel-walk"
		neutral = "virtual://movie/" + content
	)
	primary := &models.MediaFile{ID: 1, ContentID: content, FilePath: neutral + "?result=A", VirtualOwnerInstallationID: 5}
	const altCount = 4
	alternates := make([]*models.MediaFile, 0, altCount)
	for i := 0; i < altCount; i++ {
		alternates = append(alternates, &models.MediaFile{
			ID: 10 + i, ContentID: content,
			FilePath:                   neutral + "?result=" + string(rune('B'+i)),
			VirtualOwnerInstallationID: 5,
		})
	}

	var altStarted atomic.Int32
	release := make(chan struct{})
	h := &PlaybackHandler{
		FileVersionFetcher: testPlaybackFileVersionFetcher{byContent: map[string][]*models.MediaFile{
			content: append([]*models.MediaFile{primary}, alternates...),
		}},
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
			return "", errors.New("simple resolver must not be used when the detailed resolver is set")
		}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(ctx context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			if virtualResultCandidateID(uri) == "A" {
				return ResolvedVirtualMedia{}, providerEmptyListing()
			}
			if altStarted.Add(1) == altCount {
				close(release)
			}
			select {
			case <-release:
			case <-ctx.Done():
				return ResolvedVirtualMedia{}, ctx.Err()
			}
			return ResolvedVirtualMedia{}, providerEmptyListing()
		}),
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v2/playback/start", nil)

	start := time.Now()
	_, err := h.resolveVirtualStartWithVersionFallback(req, primary, "profile-1", playback.StartRequestV3{QualityPreference: "auto"}, false, 0)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("every version's listing was empty; want the honest primary failure")
	}
	if elapsed >= virtualStartVersionFallbackListingBudget/2 {
		t.Fatalf("dead-pin version-fallback walk took %s; want parallel fan-out well under one listing budget (%s)", elapsed, virtualStartVersionFallbackListingBudget)
	}
	if got := altStarted.Load(); got != altCount {
		t.Fatalf("started %d alternate resolve(s), want all %d", got, altCount)
	}
}
