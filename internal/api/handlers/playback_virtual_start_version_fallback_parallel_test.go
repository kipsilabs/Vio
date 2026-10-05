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
	_, _, ok := runVirtualVersionFallbackCandidatesV3(decisionCtx, candidates, workers, virtualStartVersionFallbackListingBudget, resolve)
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
	canceled := make(chan int, len(dead))
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
		canceled <- file.ID
		return resolvedVirtualPlaybackSource{}, false
	}

	winner, winnerIndex, ok := runVirtualVersionFallbackCandidatesV3(context.Background(), candidates, 4, virtualStartVersionFallbackListingBudget, resolve)
	if !ok {
		t.Fatal("a healthy candidate was present; want the winner")
	}
	if winner.File == nil || winner.File.ID != healthy.ID {
		t.Fatalf("winner = %#v, want the healthy candidate %d", winner, healthy.ID)
	}
	// The winner's own index must travel with the result; candidate 1 is the
	// healthy one in the fallback order above.
	if winnerIndex != 1 || candidates[winnerIndex] != healthy {
		t.Fatalf("winner index = %d, want the healthy candidate's index 1", winnerIndex)
	}
	// The three dead candidates must all observe the winner's cancellation.
	for i := 0; i < len(dead); i++ {
		select {
		case <-canceled:
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

	_, _, ok := runVirtualVersionFallbackCandidatesV3(context.Background(), candidates, 2, virtualStartVersionFallbackListingBudget, resolve)
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

// TestRunVirtualVersionFallbackCandidatesCompetingSuccessCarriesWinnerIndex is
// the gated competing-success race test. Two candidates can both resolve; the
// two successes are released together so they race to publish on the winner
// channel. The result must carry the winning source together with the index of
// the candidate that produced it, never a competing worker's index read from a
// shared variable the straggler could overwrite. Run with -race and repeated so
// both publish orders are exercised.
func TestRunVirtualVersionFallbackCandidatesCompetingSuccessCarriesWinnerIndex(t *testing.T) {
	first := &models.MediaFile{ID: 101, FilePath: "virtual://movie/movie-compete?result=first"}
	second := &models.MediaFile{ID: 102, FilePath: "virtual://movie/movie-compete?result=second"}
	candidates := []*models.MediaFile{first, second}

	for i := 0; i < 200; i++ {
		var entered atomic.Int32
		bothIn := make(chan struct{})
		release := make(chan struct{})
		// completed carries every resolver that returned success, so the test
		// can require that BOTH successes ran to completion rather than letting
		// the winner's cancellation make the second return false before it
		// returns. A resolver ignores its context here: after the shared
		// release it must succeed unconditionally.
		completed := make(chan int, len(candidates))
		resolve := func(_ context.Context, file *models.MediaFile) (resolvedVirtualPlaybackSource, bool) {
			if entered.Add(1) == int32(len(candidates)) {
				close(bothIn)
			}
			<-bothIn
			<-release
			completed <- file.ID
			return resolvedVirtualPlaybackSource{URI: file.FilePath, File: file}, true
		}
		// Open the release only once both successes are in their resolve, so
		// the two publish attempts genuinely race rather than arriving serially.
		go func() {
			<-bothIn
			close(release)
		}()

		winner, index, ok := runVirtualVersionFallbackCandidatesV3(
			context.Background(), candidates, len(candidates), virtualStartVersionFallbackListingBudget, resolve,
		)
		if !ok {
			t.Fatalf("iteration %d: both candidates succeeded; want a winner", i)
		}
		if index < 0 || index >= len(candidates) || candidates[index] != winner.File {
			t.Fatalf("iteration %d: winner source %q / file %v was paired with index %d (%v); the originating index must travel with the source",
				i, winner.URI, winner.File, index, candidates[index])
		}
		if winner.URI != candidates[index].FilePath {
			t.Fatalf("iteration %d: winner URI %q does not belong to the returned index %d (%q)",
				i, winner.URI, index, candidates[index].FilePath)
		}
		// Both successes must be observed to have completed: the point of the
		// race is that two successful completions contend for attribution, not
		// that cancellation quietly reduced it to one.
		seen := make(map[int]bool, len(candidates))
		for n := 0; n < len(candidates); n++ {
			select {
			case id := <-completed:
				if seen[id] {
					t.Fatalf("iteration %d: candidate %d completed twice", i, id)
				}
				seen[id] = true
			case <-time.After(2 * time.Second):
				t.Fatalf("iteration %d: observed %d of %d successful completions; want both successes to race",
					i, len(seen), len(candidates))
			}
		}
	}
}

// TestRunVirtualVersionFallbackCandidatesLaterWaveSharesDecisionBudget pins the
// deliberate sliding-wave tradeoff: the first `workers` candidates each get the
// full per-listing budget, and a candidate that only receives a worker slot
// after an earlier wave has timed out inherits only the remainder of the
// decision budget. Four listings that each burn the whole listing budget leave
// a fifth candidate a fraction of it, and the walk terminals on the decision
// budget instead of paying one full listing budget per candidate.
func TestRunVirtualVersionFallbackCandidatesLaterWaveSharesDecisionBudget(t *testing.T) {
	const workers = 4
	candidates := []*models.MediaFile{{ID: 1}, {ID: 2}, {ID: 3}, {ID: 4}, {ID: 5}}
	// The decision budget is deliberately below two listing budgets, so the
	// candidate that only starts after the first wave times out can never be
	// handed a fresh full listing budget.
	listingBudget := 150 * time.Millisecond
	decisionBudget := 250 * time.Millisecond

	var attempted atomic.Int32
	var laterRemaining atomic.Int64
	laterRemaining.Store(-1)
	resolve := func(ctx context.Context, file *models.MediaFile) (resolvedVirtualPlaybackSource, bool) {
		attempted.Add(1)
		if file.ID == 5 {
			if deadline, ok := ctx.Deadline(); ok {
				laterRemaining.Store(int64(time.Until(deadline)))
			}
		}
		// Every candidate times out its own listing, so the first wave consumes
		// the whole listing budget and the second wave only gets the remainder.
		<-ctx.Done()
		return resolvedVirtualPlaybackSource{}, false
	}

	decisionCtx, cancel := context.WithTimeout(context.Background(), decisionBudget)
	defer cancel()
	start := time.Now()
	_, _, ok := runVirtualVersionFallbackCandidatesV3(decisionCtx, candidates, workers, listingBudget, resolve)
	elapsed := time.Since(start)

	if ok {
		t.Fatal("every candidate timed out; want the terminal, not a winner")
	}
	if got := attempted.Load(); got != int32(len(candidates)) {
		t.Fatalf("attempted %d candidate(s), want all %d", got, len(candidates))
	}
	remaining := time.Duration(laterRemaining.Load())
	if remaining < 0 {
		t.Fatal("the later-wave candidate ran without a per-listing deadline")
	}
	if remaining >= listingBudget {
		t.Fatalf("later-wave listing deadline had %s remaining, want less than one listing budget (%s): a later wave must share the decision budget",
			remaining, listingBudget)
	}
	// A serial per-candidate budget would be len(candidates)*listingBudget; the
	// wave must instead land near the decision budget.
	if elapsed >= 3*listingBudget {
		t.Fatalf("walk took %s, want it bounded near the decision budget (%s) rather than one full listing budget per candidate",
			elapsed, decisionBudget)
	}
}

// TestResolveVirtualStartWithVersionFallbackDoesNotJoinBlockedVerdictStamp pins
// the separation of verdict persistence from resolution. The one alternate
// resolves confirmed-dead when its own listing budget expires, which fires the
// decision budget at the same moment; the failed_at marker is then blocked for
// its whole budget. The walk must terminal at the listing/decision deadline
// regardless, because the stamp is detached: a synchronous stamp held the
// worker mid-resolve, so the budget-elapsed join on the worker stretched the
// walk by startCandidateFailStampBudget. The blocked marker is what proves the
// deadline expires rather than the walk waiting out the stamp.
func TestResolveVirtualStartWithVersionFallbackDoesNotJoinBlockedVerdictStamp(t *testing.T) {
	resetVirtualRecoveryRelists(t)
	// Shrink the walk budgets so the test is fast and the relationship is
	// explicit. The listing and decision budgets coincide, so the failed
	// listing and the walk deadline expire together and the blocked marker is
	// the only thing that could stretch the walk.
	listingBudget, decisionBudget, stampBudget := 150*time.Millisecond, 150*time.Millisecond, 1200*time.Millisecond
	prevListing, prevDecision, prevStamp := virtualStartVersionFallbackListingBudget, virtualStartVersionFallbackDecisionBudget, startCandidateFailStampBudget
	virtualStartVersionFallbackListingBudget, virtualStartVersionFallbackDecisionBudget, startCandidateFailStampBudget = listingBudget, decisionBudget, stampBudget
	t.Cleanup(func() {
		virtualStartVersionFallbackListingBudget, virtualStartVersionFallbackDecisionBudget, startCandidateFailStampBudget = prevListing, prevDecision, prevStamp
	})

	const (
		content = "movie-blocked-stamp"
		neutral = "virtual://movie/" + content
	)
	primary := &models.MediaFile{ID: 1, ContentID: content, FilePath: neutral + "?result=A", VirtualOwnerInstallationID: 5}
	alternate := &models.MediaFile{
		ID: 11, ContentID: content, FilePath: neutral + "?result=B",
		VirtualOwnerInstallationID: 5, ProviderVideoHash: "hash-b", ProviderReleaseName: "Movie.2024",
	}

	stampEntered := make(chan struct{}, 1)
	releaseStamp := make(chan struct{})
	h := &PlaybackHandler{
		FileVersionFetcher: testPlaybackFileVersionFetcher{byContent: map[string][]*models.MediaFile{
			content: {primary, alternate},
		}},
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
			return "", errors.New("simple resolver must not be used when the detailed resolver is set")
		}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(ctx context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			if virtualResultCandidateID(uri) == "A" {
				return ResolvedVirtualMedia{}, providerEmptyListing()
			}
			// The alternate burns its whole listing budget, so its failure and
			// the walk deadline land together.
			<-ctx.Done()
			return ResolvedVirtualMedia{}, errors.New("virtual stream provider returned no matching candidate")
		}),
		VirtualCandidateFailMarker: func(ctx context.Context, _ int, _ string, _ *time.Time) error {
			stampEntered <- struct{}{}
			select {
			case <-releaseStamp:
			case <-ctx.Done():
			}
			return nil
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v2/playback/start", nil)

	walkDone := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		_, _ = h.resolveVirtualStartWithVersionFallback(req, primary, "profile-1", playback.StartRequestV3{QualityPreference: "auto"}, false, 0)
		walkDone <- time.Since(start)
	}()

	// The walk must terminal near the deadline while the marker is blocked. A
	// synchronous stamp would hold it for the whole stamp budget.
	select {
	case elapsed := <-walkDone:
		if elapsed >= stampBudget/2 {
			close(releaseStamp)
			t.Fatalf("walk took %s with a blocked verdict marker; want it bounded to the decision budget (%s), not joined to the stamp budget %s",
				elapsed, decisionBudget, stampBudget)
		}
	case <-time.After(stampBudget / 2):
		close(releaseStamp)
		t.Fatal("walk did not terminal while the verdict marker was blocked; the stamp is joined to the walk")
	}

	// The confirmed-dead alternate must still attempt its verdict stamp.
	select {
	case <-stampEntered:
	case <-time.After(time.Second):
		close(releaseStamp)
		t.Fatal("no confirmed-dead alternate stamp was attempted")
	}
	close(releaseStamp)
}

// TestResolveVirtualStartWithVersionFallbackBoundsConcurrentVerdictStamps pins
// the admission cap on the detached verdict stamps. Four alternates resolve
// confirmed-dead together, so four async stamps fire; the handler's detached
// gate is shrunk to one slot, so only one marker write may be in flight while
// the rest are shed. The walk must still terminal at its decision budget while
// that one marker is blocked, proving the stamp is both bounded and detached
// from the walk, and no second write is ever admitted to a full gate.
func TestResolveVirtualStartWithVersionFallbackBoundsConcurrentVerdictStamps(t *testing.T) {
	resetVirtualRecoveryRelists(t)
	listingBudget, decisionBudget, stampBudget := 150*time.Millisecond, 150*time.Millisecond, 1200*time.Millisecond
	prevListing, prevDecision, prevStamp := virtualStartVersionFallbackListingBudget, virtualStartVersionFallbackDecisionBudget, startCandidateFailStampBudget
	virtualStartVersionFallbackListingBudget, virtualStartVersionFallbackDecisionBudget, startCandidateFailStampBudget = listingBudget, decisionBudget, stampBudget
	t.Cleanup(func() {
		virtualStartVersionFallbackListingBudget, virtualStartVersionFallbackDecisionBudget, startCandidateFailStampBudget = prevListing, prevDecision, prevStamp
	})

	const (
		content = "movie-bounded-stamp"
		neutral = "virtual://movie/" + content
	)
	primary := &models.MediaFile{ID: 1, ContentID: content, FilePath: neutral + "?result=A", VirtualOwnerInstallationID: 5}
	const altCount = 4
	alternates := make([]*models.MediaFile, 0, altCount)
	for i := 0; i < altCount; i++ {
		alternates = append(alternates, &models.MediaFile{
			ID: 11 + i, ContentID: content,
			FilePath:                   neutral + "?result=" + string(rune('B'+i)),
			VirtualOwnerInstallationID: 5, ProviderVideoHash: "hash-b", ProviderReleaseName: "Movie.2024",
		})
	}

	var stampsInFlight, maxStampsInFlight, stampsAdmitted atomic.Int32
	var altAttempted atomic.Int32
	stampEntered := make(chan struct{}, altCount)
	releaseStamp := make(chan struct{})
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
			altAttempted.Add(1)
			// Each alternate burns its whole listing budget, so every
			// confirmed-dead stamp fires at roughly the decision deadline.
			<-ctx.Done()
			return ResolvedVirtualMedia{}, errors.New("virtual stream provider returned no matching candidate")
		}),
		VirtualCandidateFailMarker: func(ctx context.Context, _ int, _ string, _ *time.Time) error {
			stampsAdmitted.Add(1)
			cur := stampsInFlight.Add(1)
			for {
				old := maxStampsInFlight.Load()
				if cur <= old || maxStampsInFlight.CompareAndSwap(old, cur) {
					break
				}
			}
			stampEntered <- struct{}{}
			select {
			case <-releaseStamp:
			case <-ctx.Done():
			}
			stampsInFlight.Add(-1)
			return nil
		},
	}
	// One slot: the cap on concurrently admitted verdict stamps is observable,
	// and a saturated gate must shed rather than queue the extras.
	h.detachedWorkGate = newVirtualDetachedGate(1)
	req := httptest.NewRequest(http.MethodPost, "/api/v2/playback/start", nil)

	walkDone := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		_, _ = h.resolveVirtualStartWithVersionFallback(req, primary, "profile-1", playback.StartRequestV3{QualityPreference: "auto"}, false, 0)
		walkDone <- time.Since(start)
	}()

	// Exactly one marker is admitted and blocked; the remaining stamps must be
	// shed, never admitted to the full gate.
	select {
	case <-stampEntered:
	case <-time.After(time.Second):
		close(releaseStamp)
		t.Fatal("no verdict stamp was admitted")
	}
	select {
	case <-stampEntered:
		close(releaseStamp)
		t.Fatal("a second verdict stamp was admitted while the gate held one slot")
	case <-time.After(50 * time.Millisecond):
	}

	// The walk terminals promptly while that admitted stamp is still blocked.
	select {
	case elapsed := <-walkDone:
		if elapsed >= stampBudget/2 {
			close(releaseStamp)
			t.Fatalf("walk took %s with a blocked verdict marker; want it bounded to the decision budget (%s)",
				elapsed, decisionBudget)
		}
	case <-time.After(stampBudget / 2):
		close(releaseStamp)
		t.Fatal("walk did not terminal while its verdict stamp was blocked on the saturated gate")
	}
	// The runner joins its workers before returning, so by now every alternate
	// has attempted its stamp. All four attempted; exactly one was admitted and
	// three were shed by the one-slot gate.
	if got := altAttempted.Load(); got != altCount {
		close(releaseStamp)
		t.Fatalf("attempted %d alternate(s), want all %d to earn a verdict stamp", got, altCount)
	}
	if got := len(stampEntered); got != 0 {
		close(releaseStamp)
		t.Fatalf("a second verdict stamp was queued (%d buffered), want exactly 1 admitted", got)
	}
	if got := stampsAdmitted.Load(); got != 1 {
		close(releaseStamp)
		t.Fatalf("marker entered %d time(s), want exactly 1: the gate has one slot and the rest must be shed", got)
	}
	if got := maxStampsInFlight.Load(); got != 1 {
		close(releaseStamp)
		t.Fatalf("peak in-flight verdict stamps = %d, want 1", got)
	}
	close(releaseStamp)
}

// TestStampStartVirtualCandidateFailedAsyncShedsAtGateCap is the direct unit
// test for the detached stamp's non-blocking admission: a saturated gate must
// shed the write and return at once, and freeing a slot admits the next stamp.
func TestStampStartVirtualCandidateFailedAsyncShedsAtGateCap(t *testing.T) {
	prevStamp := startCandidateFailStampBudget
	startCandidateFailStampBudget = 2 * time.Second
	t.Cleanup(func() { startCandidateFailStampBudget = prevStamp })

	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	h := &PlaybackHandler{
		VirtualCandidateFailMarker: func(ctx context.Context, _ int, _ string, _ *time.Time) error {
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil
		},
	}
	gate := newVirtualDetachedGate(1)
	h.detachedWorkGate = gate

	dead := errors.New("virtual stream provider returned no matching candidate")
	file := func(id int) *models.MediaFile {
		return &models.MediaFile{ID: id, FilePath: "virtual://movie/movie-dead?result=x", ProviderVideoHash: "h", ProviderReleaseName: "Movie.2024"}
	}

	h.stampStartVirtualCandidateFailedAsync(context.Background(), file(1), dead)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("the first confirmed-dead stamp was not admitted")
	}

	// The slot is held by a blocked marker. The next stamp must be shed
	// without spawning a worker and without blocking its caller.
	returned := make(chan struct{})
	go func() {
		h.stampStartVirtualCandidateFailedAsync(context.Background(), file(2), dead)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("a shed verdict stamp blocked the caller on a saturated gate")
	}
	select {
	case <-entered:
		t.Fatal("a second stamp entered while the gate had no free slot")
	case <-time.After(50 * time.Millisecond):
	}

	// Free the slot; the next stamp is admitted again.
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for len(gate.slots) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(gate.slots) != 0 {
		t.Fatal("the admitted stamp did not release its gate slot")
	}
	h.stampStartVirtualCandidateFailedAsync(context.Background(), file(3), dead)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("a verdict stamp was not admitted after the gate freed a slot")
	}
}
