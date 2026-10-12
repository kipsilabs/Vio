package handlers

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/auth"
	"github.com/Silo-Server/silo-server/internal/models"
)

func prefetchTestContext(userID int) context.Context {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/prefetch", nil)
	return apimw.SetClaims(req.Context(), &auth.Claims{UserID: userID, Role: "user", TokenType: auth.TokenTypeAccess})
}

func prefetchTestFile(id int, path string) *models.MediaFile {
	return &models.MediaFile{
		ID: id, ContentID: "movie-prefetch", FilePath: path,
		Container: "virtual", VirtualOwnerInstallationID: 5,
	}
}

// TestPrefetchDeduplicatesEquivalentRequests pins the equivalence-key rule: two
// requests for the same source and profile collapse to one provider list while
// the first is still in flight, and a second request for a distinct source does
// not get suppressed by the first's key.
func TestPrefetchDeduplicatesEquivalentRequests(t *testing.T) {
	started := make(chan string, 4)
	release := make(chan struct{})
	h := &PlaybackHandler{BestResultCache: NewVirtualBestResultCache(time.Hour, 16),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(_ context.Context, path string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			started <- path
			<-release
			return nil, nil
		}),
	}
	ctx := prefetchTestContext(7)
	a := prefetchTestFile(1, "virtual://movie/dup?result=a")
	b := prefetchTestFile(2, "virtual://movie/other?result=b")

	// Same source/profile twice: the second must dedupe against the first.
	h.PrefetchVirtualPlayback(ctx, []*models.MediaFile{a}, "profile-1")
	h.PrefetchVirtualPlayback(ctx, []*models.MediaFile{a}, "profile-1")
	// Distinct source: its own admission and provider list.
	h.PrefetchVirtualPlayback(ctx, []*models.MediaFile{b}, "profile-1")

	deadline := time.Now().Add(2 * time.Second)
	var paths []string
	for len(paths) < 2 && time.Now().Before(deadline) {
		select {
		case p := <-started:
			paths = append(paths, p)
		case <-time.After(5 * time.Millisecond):
		}
	}
	close(release)
	if len(paths) != 2 {
		t.Fatalf("provider lists = %v, want exactly one per distinct source", paths)
	}
	seen := map[string]int{}
	for _, p := range paths {
		seen[p]++
	}
	if seen[virtualPlaybackNeutralKey(a.FilePath)] != 1 {
		t.Fatalf("equivalent request listed %d times, want 1", seen[virtualPlaybackNeutralKey(a.FilePath)])
	}
	if seen[virtualPlaybackNeutralKey(b.FilePath)] != 1 {
		t.Fatalf("distinct request listed %d times, want 1", seen[virtualPlaybackNeutralKey(b.FilePath)])
	}
}

// TestPrefetchDistinctKeyFloodIsBounded pins that a storm of distinct
// source/profile requests cannot grow in-flight or pending state without
// bound: the dedup set stays at or below queue+workers and the submit loop
// never blocks.
func TestPrefetchDistinctKeyFloodIsBounded(t *testing.T) {
	release := make(chan struct{})
	h := &PlaybackHandler{BestResultCache: NewVirtualBestResultCache(time.Hour, 16),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			<-release
			return nil, nil
		}),
	}
	ctx := prefetchTestContext(7)
	bound := virtualPrefetchQueueSize + virtualPrefetchWorkers

	start := time.Now()
	for i := 0; i < bound+32; i++ {
		file := prefetchTestFile(1000+i, "virtual://movie/flood-"+strconv.Itoa(i)+"?result=r")
		h.PrefetchVirtualPlayback(ctx, []*models.MediaFile{file}, "profile-1")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("prefetch submission blocked for %v under a distinct-key flood", elapsed)
	}

	h.prefetchMu.Lock()
	got := len(h.prefetchInFlight)
	h.prefetchMu.Unlock()
	if got > bound {
		t.Fatalf("in-flight prefetch keys = %d, want <= bound %d", got, bound)
	}
	if queued := len(h.prefetchQueue); queued > virtualPrefetchQueueSize {
		t.Fatalf("pending prefetch tasks = %d, want <= %d", queued, virtualPrefetchQueueSize)
	}
	close(release)
}

// TestPrefetchStalledProviderShedsAndDoesNotBlock pins the skip/reject
// behavior: when a stalled provider holds both workers and the queue fills,
// further requests are shed immediately rather than piling up.
func TestPrefetchStalledProviderShedsAndDoesNotBlock(t *testing.T) {
	release := make(chan struct{})
	h := &PlaybackHandler{BestResultCache: NewVirtualBestResultCache(time.Hour, 16),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			<-release
			return nil, nil
		}),
	}
	ctx := prefetchTestContext(7)
	for i := 0; i < virtualPrefetchQueueSize+virtualPrefetchWorkers+8; i++ {
		file := prefetchTestFile(2000+i, "virtual://movie/stall-"+strconv.Itoa(i)+"?result=r")
		h.PrefetchVirtualPlayback(ctx, []*models.MediaFile{file}, "profile-1")
	}

	shedStart := time.Now()
	for i := 0; i < 8; i++ {
		file := prefetchTestFile(3000+i, "virtual://movie/shed-"+strconv.Itoa(i)+"?result=r")
		h.PrefetchVirtualPlayback(ctx, []*models.MediaFile{file}, "profile-1")
	}
	if elapsed := time.Since(shedStart); elapsed > time.Second {
		t.Fatalf("shedding under a stalled provider took %v, want non-blocking", elapsed)
	}
	close(release)
}

// TestPrefetchShutdownStopsWorkersAndClearsState pins service-shutdown binding:
// canceling the service context stops the workers, clears the dedup set, and
// leaves no work that could still contact a provider.
func TestPrefetchShutdownStopsWorkersAndClearsState(t *testing.T) {
	serviceCtx, serviceCancel := context.WithCancel(context.Background())
	var lists int64
	release := make(chan struct{})
	h := &PlaybackHandler{BestResultCache: NewVirtualBestResultCache(time.Hour, 16),
		ServiceContext: serviceCtx,
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(ctx context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			atomic.AddInt64(&lists, 1)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-release:
				return nil, nil
			}
		}),
	}
	ctx := prefetchTestContext(7)
	for i := 0; i < 4; i++ {
		file := prefetchTestFile(4000+i, "virtual://movie/shutdown-"+strconv.Itoa(i)+"?result=r")
		h.PrefetchVirtualPlayback(ctx, []*models.MediaFile{file}, "profile-1")
	}
	time.Sleep(30 * time.Millisecond)
	serviceCancel()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.prefetchMu.Lock()
		n := len(h.prefetchInFlight)
		h.prefetchMu.Unlock()
		if n == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.prefetchMu.Lock()
	remaining := len(h.prefetchInFlight)
	h.prefetchMu.Unlock()
	close(release)
	if remaining != 0 {
		t.Fatalf("shutdown left %d prefetch key(s) behind", remaining)
	}
	before := atomic.LoadInt64(&lists)
	time.Sleep(30 * time.Millisecond)
	if after := atomic.LoadInt64(&lists); after != before {
		t.Fatalf("provider lists ran after shutdown: %d -> %d", before, after)
	}
}

// TestPrefetchDoesNotCreateStickyEvidence pins the metadata-only rule: warming
// the best-result cache must not pin sticky playback evidence.
func TestPrefetchDoesNotCreateStickyEvidence(t *testing.T) {
	h := &PlaybackHandler{BestResultCache: NewVirtualBestResultCache(time.Hour, 16)}
	listed := make(chan struct{}, 1)
	h.VirtualPlaybackStreamLister = VirtualPlaybackStreamListerFunc(func(_ context.Context, path string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
		select {
		case listed <- struct{}{}:
		default:
		}
		return []VirtualPlaybackStream{{URI: path + "?result=cand"}}, nil
	})
	file := prefetchTestFile(5, "virtual://movie/sticky?result=x")
	neutral := virtualPlaybackNeutralKey(file.FilePath)
	key := bestResultCacheKey(file.ContentID, neutral, file.VirtualOwnerInstallationID)
	ctx := prefetchTestContext(7)
	h.PrefetchVirtualPlayback(ctx, []*models.MediaFile{file}, "profile-1")

	select {
	case <-listed:
	case <-time.After(2 * time.Second):
		t.Fatal("prefetch never listed")
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(h.BestResultCache.get(key, time.Now())) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if cached := h.BestResultCache.get(key, time.Now()); len(cached) == 0 {
		t.Fatal("prefetch did not warm the metadata cache")
	}
	if pin := h.peekVirtualSticky(key); pin != "" {
		t.Fatalf("prefetch created sticky playback evidence %q", pin)
	}
}

// TestPrefetchSaturationDoesNotBlockForegroundResolve pins that a saturated
// prefetch pipeline cannot stall a foreground start: the resolve path is
// synchronous and does not wait on prefetch capacity.
func TestPrefetchSaturationDoesNotBlockForegroundResolve(t *testing.T) {
	release := make(chan struct{})
	h := &PlaybackHandler{
		BestResultCache: NewVirtualBestResultCache(time.Hour, 16),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(_ context.Context, path string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			// Only the saturating prefetch sources stall. A foreground start
			// (a different source) must not wait behind prefetch capacity.
			if strings.Contains(path, "/sat-") {
				<-release
				return nil, nil
			}
			return []VirtualPlaybackStream{{URI: path + "?result=fg", Resolution: "1080p"}}, nil
		}),
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
			return "http://provider.example/stream?path=" + path, nil
		}),
	}
	ctx := prefetchTestContext(7)
	for i := 0; i < virtualPrefetchQueueSize+virtualPrefetchWorkers+4; i++ {
		file := prefetchTestFile(5000+i, "virtual://movie/sat-"+strconv.Itoa(i)+"?result=r")
		h.PrefetchVirtualPlayback(ctx, []*models.MediaFile{file}, "profile-1")
	}

	// A foreground resolve with no pinned candidate and no cached listing must
	// still complete; prefetch saturation must not be on its path.
	foreground := prefetchTestFile(9, "virtual://movie/foreground?result=fg")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	req = req.WithContext(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := h.resolveVirtualPlaybackSource(req, foreground, "profile-1", false, nil, "", "", 0, false)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("foreground resolve failed under prefetch saturation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("foreground resolve blocked behind saturated prefetch")
	}
	close(release)
}

// TestPrefetchRejectionsAreObservable pins that overload is observable at the
// expected log level rather than silent.
func TestPrefetchRejectionsAreObservable(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	release := make(chan struct{})
	h := &PlaybackHandler{BestResultCache: NewVirtualBestResultCache(time.Hour, 16),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			<-release
			return nil, nil
		}),
	}
	ctx := prefetchTestContext(7)
	for i := 0; i < virtualPrefetchQueueSize+virtualPrefetchWorkers+4; i++ {
		file := prefetchTestFile(6000+i, "virtual://movie/observe-"+strconv.Itoa(i)+"?result=r")
		h.PrefetchVirtualPlayback(ctx, []*models.MediaFile{file}, "profile-1")
	}
	close(release)
	if !strings.Contains(logs.String(), "prefetch rejected") {
		t.Fatalf("overload rejection was not logged: %s", logs.String())
	}
}

// TestPrefetchOutcomeMetricsAreObservable pins that every admission and
// completion outcome lands on the prefetch metric with a bounded label. The
// production drops are Debug logs, invisible at the default Info level; the
// metric is what sizes the convertible share from /metrics without scraping.
func TestPrefetchOutcomeMetricsAreObservable(t *testing.T) {
	outcomeSamples := func() map[string]float64 {
		families, err := prometheus.DefaultGatherer.Gather()
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]float64{}
		for _, family := range families {
			if family.GetName() != "silo_virtual_prefetch_outcomes_total" {
				continue
			}
			for _, metric := range family.GetMetric() {
				for _, label := range metric.GetLabel() {
					if label.GetName() == "outcome" {
						got[label.GetValue()] = metric.GetCounter().GetValue()
					}
				}
			}
		}
		return got
	}

	// Duplicate: the same source/profile admitted twice collapses to one list.
	dupRelease := make(chan struct{})
	h := &PlaybackHandler{BestResultCache: NewVirtualBestResultCache(time.Hour, 16),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			<-dupRelease
			return nil, nil
		}),
	}
	ctx := prefetchTestContext(7)
	dup := prefetchTestFile(7001, "virtual://movie/metric-dup?result=r")
	h.PrefetchVirtualPlayback(ctx, []*models.MediaFile{dup}, "profile-1")
	h.PrefetchVirtualPlayback(ctx, []*models.MediaFile{dup}, "profile-1")
	close(dupRelease)
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := outcomeSamples()
		if got[virtualPrefetchDuplicate] > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no duplicate outcome observed: %v", got)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Completed: a prefetch whose provider answers warms the cache.
	h2 := &PlaybackHandler{BestResultCache: NewVirtualBestResultCache(time.Hour, 16),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(_ context.Context, path string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{{
				URI: path + "?result=done", Resolution: "1080p",
				CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
			}}, nil
		}),
	}
	warm := prefetchTestFile(7002, "virtual://movie/metric-done")
	h2.PrefetchVirtualPlayback(ctx, []*models.MediaFile{warm}, "profile-1")
	deadline = time.Now().Add(5 * time.Second)
	for {
		got := outcomeSamples()
		if got[virtualPrefetchCompleted] > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no completed outcome observed: %v", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
	key := bestResultCacheKey("movie-prefetch", virtualPlaybackNeutralKey("virtual://movie/metric-done"), 5)
	if cached := h2.BestResultCache.get(key, time.Now()); len(cached) == 0 {
		t.Fatal("completed prefetch did not warm the best-result cache")
	}
}
