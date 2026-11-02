package handlers

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/remotestream"
)

// clearVirtualProviderLinkLatency drops one link's observation so a test starts
// from an unmeasured link and leaves nothing behind for the next test.
func clearVirtualProviderLinkLatency(t *testing.T, key string) {
	t.Helper()
	virtualProviderLinkLatency.Lock()
	delete(virtualProviderLinkLatency.marks, key)
	virtualProviderLinkLatency.Unlock()
	t.Cleanup(func() {
		virtualProviderLinkLatency.Lock()
		delete(virtualProviderLinkLatency.marks, key)
		virtualProviderLinkLatency.Unlock()
	})
}

// TestCopySeekAnchorResolveBudgetScalesFromRecentLinkLatency proves the anchor
// resolve budget keeps today's fixed floor for an unmeasured or fast link and
// extends only from a recent observation that already outran it, up to the cap.
func TestCopySeekAnchorResolveBudgetScalesFromRecentLinkLatency(t *testing.T) {
	floor, maxBudget := 5*time.Second, 20*time.Second
	for _, tc := range []struct {
		name   string
		recent time.Duration
		want   time.Duration
	}{
		{name: "unmeasured keeps the floor", recent: 0, want: floor},
		{name: "healthy listing keeps the floor", recent: 1900 * time.Millisecond, want: floor},
		{name: "fastest extension stays above the floor", recent: 2600 * time.Millisecond, want: 5200 * time.Millisecond},
		{name: "slow relay extends", recent: 6 * time.Second, want: 12 * time.Second},
		{name: "long listing reaches the cap", recent: 11 * time.Second, want: maxBudget},
		{name: "stall stays inside the cap", recent: 5 * time.Minute, want: maxBudget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := copySeekAnchorResolveBudget(floor, maxBudget, tc.recent); got != tc.want {
				t.Fatalf("budget for recent=%v = %v, want %v", tc.recent, got, tc.want)
			}
		})
	}
	// The floor is the configured budget, never a shortened one, even when a
	// caller passes a cap below it.
	if got := copySeekAnchorResolveBudget(floor, time.Second, 6*time.Second); got != floor {
		t.Fatalf("budget with a cap below the floor = %v, want the floor %v", got, floor)
	}
}

// TestVirtualProviderLinkLatencyKeepsSlowestRecentObservation proves the tracker
// keeps the slow end a link recently showed, ignores a later faster answer, and
// drops a lapsed observation so it cannot size budgets indefinitely.
func TestVirtualProviderLinkLatencyKeepsSlowestRecentObservation(t *testing.T) {
	const key = "test-link"
	clearVirtualProviderLinkLatency(t, key)
	now := time.Now()
	recordVirtualProviderLinkLatency(key, 6*time.Second, now)
	recordVirtualProviderLinkLatency(key, 900*time.Millisecond, now)
	if got := recentVirtualProviderLinkLatency(key); got != 6*time.Second {
		t.Fatalf("recent latency = %v, want the slowest recent observation %v", got, 6*time.Second)
	}
	if got := recentVirtualProviderLinkLatency("other-link"); got != 0 {
		t.Fatalf("recent latency for an unmeasured link = %v, want 0", got)
	}
	recordVirtualProviderLinkLatency("lapsed-link", 8*time.Second, now.Add(-virtualProviderLinkLatencyWindow-time.Second))
	t.Cleanup(func() { clearVirtualProviderLinkLatency(t, "lapsed-link") })
	if got := recentVirtualProviderLinkLatency("lapsed-link"); got != 0 {
		t.Fatalf("recent latency for a lapsed observation = %v, want 0", got)
	}
}

// slowLinkAnchorFixture builds the virtual seek-anchor path for a pinned
// release owned by one provider link, with the probe seam installed so a test
// observes the budget each attempt grants instead of reading bytes.
func slowLinkAnchorFixture(t *testing.T, ownerInstallationID int) (*PlaybackHandler, *models.MediaFile, *playback.Session, *playback.PlanV3) {
	t.Helper()
	const (
		neutralURI = "virtual://movie/tt-slow-link"
		pinnedURI  = neutralURI + "?result=pinned"
	)
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	handler.AllowPrivateStreams = func(int) bool { return true }
	handler.RemoteStreamRelay = remotestream.NewRelay()
	t.Cleanup(func() { _ = handler.RemoteStreamRelay.Close(context.Background()) })
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
		return ResolvedVirtualMedia{
			URL: "http://127.0.0.1:9/pinned.mkv", URI: pinnedURI, CandidateID: "pinned",
			ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024",
		}, nil
	})
	file := &models.MediaFile{
		ID: 21, ContentID: "movie-slow-link", FilePath: pinnedURI,
		VirtualOwnerInstallationID: ownerInstallationID, ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024",
	}
	session := &playback.Session{ID: "session-slow-link", UserID: 1, ProfileID: "profile-1"}
	plan := &playback.PlanV3{
		PlanID:   "plan:slow-link",
		Delivery: playback.DeliveryRemuxProgressiveV3,
		Timeline: playback.TimelineV3{SourceStartSeconds: 30, PlayerStartSeconds: 30},
	}
	return handler, file, session, plan
}

// TestPrepareTransportTimelineSlowLinkExtendsAnchorResolveBudget proves the
// resolve a slow link is granted is derived from that link's recent provider
// latency: the same call that would have expired at the fixed floor gets room
// to open, and the probe still runs. The deadline is observed, not slept on.
func TestPrepareTransportTimelineSlowLinkExtendsAnchorResolveBudget(t *testing.T) {
	previousFloor, previousCap := copySeekAnchorSeekResolveTimeout, copySeekAnchorSeekResolveTimeoutCap
	copySeekAnchorSeekResolveTimeout, copySeekAnchorSeekResolveTimeoutCap = 5*time.Second, 20*time.Second
	defer func() {
		copySeekAnchorSeekResolveTimeout, copySeekAnchorSeekResolveTimeoutCap = previousFloor, previousCap
	}()

	const ownerInstallationID = 7
	clearVirtualProviderLinkLatency(t, "7")
	recordVirtualProviderLinkLatency("7", 6*time.Second, time.Now())
	handler, file, session, plan := slowLinkAnchorFixture(t, ownerInstallationID)

	var mu sync.Mutex
	var budgets []time.Duration
	handler.copySeekAnchor = func(ctx context.Context, _ string, _ string, requested float64, segmentDuration int) (float64, int, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Errorf("anchor probe ctx has no deadline; the attempt budget must bound it")
		} else {
			mu.Lock()
			budgets = append(budgets, time.Until(deadline))
			mu.Unlock()
		}
		return requested - 1, computeStartSegment(requested-1, segmentDuration), nil
	}

	timeline, transportErr := handler.prepareTransportTimelineV3(context.Background(), session, file, playback.PlannerResultV3{Plan: plan, PlayMethod: playback.PlayRemux})
	if transportErr != nil {
		t.Fatalf("prepareTransportTimelineV3: %v", transportErr)
	}
	if !timeline.copySeekAnchorResolved {
		t.Fatalf("timeline = %#v, want a resolved copy anchor", timeline)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(budgets) != 1 {
		t.Fatalf("anchor probes = %d, want exactly 1", len(budgets))
	}
	// Twice the 6s listing the link just served, well above the 5s floor and
	// still inside the cap.
	if budgets[0] < 11*time.Second || budgets[0] > 13*time.Second {
		t.Fatalf("anchor resolve budget = %v, want about 12s from the 6s link observation", budgets[0])
	}
}

// TestPrepareTransportTimelineFastLinkKeepsAnchorResolveBudget proves a link
// whose recent listing fits the fixed budget keeps exactly the old behavior:
// the extension is skipped and the attempt gets the floor.
func TestPrepareTransportTimelineFastLinkKeepsAnchorResolveBudget(t *testing.T) {
	previousFloor := copySeekAnchorSeekResolveTimeout
	copySeekAnchorSeekResolveTimeout = 5 * time.Second
	defer func() { copySeekAnchorSeekResolveTimeout = previousFloor }()

	const ownerInstallationID = 8
	clearVirtualProviderLinkLatency(t, "8")
	recordVirtualProviderLinkLatency("8", 1900*time.Millisecond, time.Now())
	handler, file, session, plan := slowLinkAnchorFixture(t, ownerInstallationID)

	var mu sync.Mutex
	var budgets []time.Duration
	handler.copySeekAnchor = func(ctx context.Context, _ string, _ string, requested float64, segmentDuration int) (float64, int, error) {
		if deadline, ok := ctx.Deadline(); ok {
			mu.Lock()
			budgets = append(budgets, time.Until(deadline))
			mu.Unlock()
		}
		return requested - 1, computeStartSegment(requested-1, segmentDuration), nil
	}

	timeline, transportErr := handler.prepareTransportTimelineV3(context.Background(), session, file, playback.PlannerResultV3{Plan: plan, PlayMethod: playback.PlayRemux})
	if transportErr != nil {
		t.Fatalf("prepareTransportTimelineV3: %v", transportErr)
	}
	if !timeline.copySeekAnchorResolved {
		t.Fatalf("timeline = %#v, want a resolved copy anchor", timeline)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(budgets) != 1 {
		t.Fatalf("anchor probes = %d, want exactly 1", len(budgets))
	}
	if budgets[0] > copySeekAnchorSeekResolveTimeout || budgets[0] < copySeekAnchorSeekResolveTimeout-500*time.Millisecond {
		t.Fatalf("anchor resolve budget = %v, want the unchanged floor %v for a fast link", budgets[0], copySeekAnchorSeekResolveTimeout)
	}
}

// TestPrepareTransportTimelineAnchorTimeoutRetriesSamePinWithFreshBudget proves
// a slow link whose pinned-relay lookup exhausts its first budget is retried
// once on the same release under a budget derived from that observation, and
// that the retry's success serves the start instead of the retryable seek error.
func TestPrepareTransportTimelineAnchorTimeoutRetriesSamePinWithFreshBudget(t *testing.T) {
	previousFloor := copySeekAnchorSeekResolveTimeout
	copySeekAnchorSeekResolveTimeout = 100 * time.Millisecond
	defer func() { copySeekAnchorSeekResolveTimeout = previousFloor }()

	const ownerInstallationID = 9
	clearVirtualProviderLinkLatency(t, "9")
	handler, file, session, plan := slowLinkAnchorFixture(t, ownerInstallationID)

	var mu sync.Mutex
	var resolvedURIs []string
	var resolveBudgets []time.Duration
	firstResolve := true
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(ctx context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		mu.Lock()
		resolvedURIs = append(resolvedURIs, uri)
		if deadline, ok := ctx.Deadline(); ok {
			resolveBudgets = append(resolveBudgets, time.Until(deadline))
		}
		fail := firstResolve
		firstResolve = false
		mu.Unlock()
		if fail {
			// The provider answers only after the first attempt's budget is
			// gone: the shape a healthy but slow relay has.
			<-ctx.Done()
			return ResolvedVirtualMedia{}, ctx.Err()
		}
		return ResolvedVirtualMedia{
			URL: "http://127.0.0.1:9/pinned.mkv", URI: file.FilePath, CandidateID: "pinned",
			ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024",
		}, nil
	})
	var probes atomic.Int32
	handler.copySeekAnchor = func(_ context.Context, _ string, _ string, requested float64, segmentDuration int) (float64, int, error) {
		probes.Add(1)
		return requested - 1, computeStartSegment(requested-1, segmentDuration), nil
	}

	timeline, transportErr := handler.prepareTransportTimelineV3(context.Background(), session, file, playback.PlannerResultV3{Plan: plan, PlayMethod: playback.PlayRemux})
	if transportErr != nil {
		t.Fatalf("prepareTransportTimelineV3: %v, want the retried attempt to serve, got %#v", transportErr, transportErr)
	}
	if !timeline.copySeekAnchorResolved {
		t.Fatalf("timeline = %#v, want the retried attempt's resolved copy anchor", timeline)
	}
	if got := probes.Load(); got != 1 {
		t.Fatalf("anchor probes = %d, want 1: the first attempt never reached the probe", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(resolvedURIs) < 2 {
		t.Fatalf("provider resolves = %v, want the timed-out attempt and its same-pin retry", resolvedURIs)
	}
	for _, uri := range resolvedURIs {
		if uri != file.FilePath {
			t.Fatalf("resolved URIs = %v, want every attempt to resolve the same pinned release %q", resolvedURIs, file.FilePath)
		}
	}
	firstBudget := resolveBudgets[0]
	var widened time.Duration
	for _, budget := range resolveBudgets {
		if budget > widened {
			widened = budget
		}
	}
	if widened < firstBudget*3/2 {
		t.Fatalf("resolve budgets = %v, want the retry widened from the first attempt's ~%v budget", resolveBudgets, firstBudget)
	}
}
