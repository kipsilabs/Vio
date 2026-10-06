package metadata

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

// sweepCountingRefreshDebtRepo counts series-wide episode debt sweeps, which
// always start by snapshotting the series' episode debt.
type sweepCountingRefreshDebtRepo struct {
	*fakeRefreshDebtRepo
	sweepMu sync.Mutex
	sweeps  map[string]int
}

func newSweepCountingRefreshDebtRepo() *sweepCountingRefreshDebtRepo {
	return &sweepCountingRefreshDebtRepo{
		fakeRefreshDebtRepo: newFakeRefreshDebtRepo(),
		sweeps:              make(map[string]int),
	}
}

func (r *sweepCountingRefreshDebtRepo) SnapshotEpisodeDebts(ctx context.Context, seriesID string) (map[string]string, error) {
	r.sweepMu.Lock()
	r.sweeps[seriesID]++
	r.sweepMu.Unlock()
	return r.fakeRefreshDebtRepo.SnapshotEpisodeDebts(ctx, seriesID)
}

func (r *sweepCountingRefreshDebtRepo) sweepCount(seriesID string) int {
	r.sweepMu.Lock()
	defer r.sweepMu.Unlock()
	return r.sweeps[seriesID]
}

// seedSeriesSyncCounters wires a target-refresh harness whose provider covers
// S05E03 and S05E04 and counts the series-wide link passes and debt sweeps.
func seedSeriesSyncCounters(t *testing.T) (*testHarness, string, string, *atomic.Int32, *sweepCountingRefreshDebtRepo) {
	t.Helper()
	provider := &targetRefreshProvider{
		seasons: []SeasonResult{{SeasonNumber: 5, Title: "Provider Season 5"}},
		episodesBySeason: map[int][]EpisodeResult{
			5: {
				{SeasonNumber: 5, EpisodeNumber: 3, Title: "Provider Episode 3", Overview: "Episode 3 overview"},
				{SeasonNumber: 5, EpisodeNumber: 4, Title: "Provider Episode 4", Overview: "Episode 4 overview"},
			},
		},
	}
	h, seriesID, seasonID, _ := seedTargetRefreshHarness(provider)
	debts := newSweepCountingRefreshDebtRepo()
	h.service.refreshDebtRepo = debts
	linkPasses := &atomic.Int32{}
	h.service.hooks.ensureSeriesEpisodeLinks = func(context.Context, string) error {
		linkPasses.Add(1)
		return nil
	}
	return h, seriesID, seasonID, linkPasses, debts
}

func TestScheduledRefreshBatchRunsSeriesSyncOncePerSeries(t *testing.T) {
	h, seriesID, seasonID, linkPasses, debts := seedSeriesSyncCounters(t)
	ctx := context.Background()

	batchCtx, flush := h.service.BeginScheduledRefreshBatch(ctx)
	targets := []struct{ targetType, contentID string }{
		{RefreshTargetEpisode, "episode-s05e03"},
		{RefreshTargetEpisode, "episode-s05e04"},
		{RefreshTargetSeason, seasonID},
	}
	for _, target := range targets {
		if err := h.service.RefreshScheduledTarget(batchCtx, target.targetType, target.contentID); err != nil {
			t.Fatalf("RefreshScheduledTarget(%s %s): %v", target.targetType, target.contentID, err)
		}
	}

	if got := linkPasses.Load(); got != 0 {
		t.Fatalf("series link passes during the batch = %d, want 0 until the flush", got)
	}
	if got := debts.sweepCount(seriesID); got != 0 {
		t.Fatalf("series debt sweeps during the batch = %d, want 0 until the flush", got)
	}

	flush(ctx)

	if got := linkPasses.Load(); got != 1 {
		t.Fatalf("series link passes after the flush = %d, want 1", got)
	}
	if got := debts.sweepCount(seriesID); got != 1 {
		t.Fatalf("series debt sweeps after the flush = %d, want 1", got)
	}
	updated, err := h.service.episodeRepo.GetBySeriesAndNumber(ctx, seriesID, 5, 4)
	if err != nil {
		t.Fatalf("GetBySeriesAndNumber S05E04: %v", err)
	}
	if updated.Overview != "Episode 4 overview" {
		t.Fatalf("S05E04 overview = %q, want the refreshed provider metadata", updated.Overview)
	}
}

func TestScheduledRefreshWithoutBatchSyncsSeriesAfterEachTarget(t *testing.T) {
	h, seriesID, _, linkPasses, debts := seedSeriesSyncCounters(t)
	ctx := context.Background()

	for _, episodeID := range []string{"episode-s05e03", "episode-s05e04"} {
		if err := h.service.RefreshScheduledTarget(ctx, RefreshTargetEpisode, episodeID); err != nil {
			t.Fatalf("RefreshScheduledTarget(%s): %v", episodeID, err)
		}
	}

	if got := linkPasses.Load(); got != 2 {
		t.Fatalf("series link passes = %d, want one per refreshed target", got)
	}
	if got := debts.sweepCount(seriesID); got != 2 {
		t.Fatalf("series debt sweeps = %d, want one per refreshed target", got)
	}
}

func TestScheduledRefreshBatchKeepsFullSeriesPersistInline(t *testing.T) {
	h, seriesID, _, linkPasses, debts := seedSeriesSyncCounters(t)
	ctx := context.Background()
	series := h.itemRepo.items[seriesID]

	batchCtx, flush := h.service.BeginScheduledRefreshBatch(ctx)
	h.service.persistSeasonsAndEpisodes(
		batchCtx, series, map[string]string{"tmdb": series.TmdbID}, "en", "en",
		[]SeasonResult{{SeasonNumber: 5, Title: "Provider Season 5"}},
		[]EpisodeResult{{SeasonNumber: 5, EpisodeNumber: 3, Title: "Provider Episode 3"}},
		MergeReplaceUnlocked,
	)

	// A whole-series refresh feeds the series' own debt sync straight after it
	// returns, so its link pass and sweep must not wait for the batch flush.
	if got := linkPasses.Load(); got != 1 {
		t.Fatalf("series link passes after a full-series persist = %d, want 1 inline", got)
	}
	if got := debts.sweepCount(seriesID); got != 1 {
		t.Fatalf("series debt sweeps after a full-series persist = %d, want 1 inline", got)
	}

	flush(ctx)

	if got := linkPasses.Load(); got != 1 {
		t.Fatalf("series link passes after the flush = %d, want no deferred repeat", got)
	}
}

func TestScheduledRefreshBatchFlushSkipsCanceledContext(t *testing.T) {
	h, seriesID, _, linkPasses, debts := seedSeriesSyncCounters(t)
	ctx, cancel := context.WithCancel(context.Background())

	batchCtx, flush := h.service.BeginScheduledRefreshBatch(ctx)
	if err := h.service.RefreshScheduledTarget(batchCtx, RefreshTargetEpisode, "episode-s05e03"); err != nil {
		t.Fatalf("RefreshScheduledTarget: %v", err)
	}
	cancel()
	flush(ctx)

	if got := linkPasses.Load(); got != 0 {
		t.Fatalf("series link passes after a canceled flush = %d, want 0", got)
	}
	if got := debts.sweepCount(seriesID); got != 0 {
		t.Fatalf("series debt sweeps after a canceled flush = %d, want 0", got)
	}
}

// failingStaleIDRepo fails every stale provider ID lookup after the first,
// which lets a two-language refresh write its first language and then fail.
type failingStaleIDRepo struct {
	*fakeStaleIDRepo
	calls atomic.Int32
}

func (r *failingStaleIDRepo) GetByContentID(ctx context.Context, contentID string) ([]*models.StaleMediaID, error) {
	if r.calls.Add(1) > 1 {
		return nil, errors.New("stale id lookup failed")
	}
	return r.fakeStaleIDRepo.GetByContentID(ctx, contentID)
}

func TestScheduledRefreshBatchSyncsSeriesWhenALaterLanguageFails(t *testing.T) {
	h, seriesID, _, linkPasses, _ := seedSeriesSyncCounters(t)
	ctx := context.Background()
	h.libraryRepo.setMetadataLanguages(seriesID, []string{"en", "fr"}, nil)
	h.service.staleIDRepo = &failingStaleIDRepo{fakeStaleIDRepo: newFakeStaleIDRepo()}

	batchCtx, flush := h.service.BeginScheduledRefreshBatch(ctx)
	if err := h.service.RefreshScheduledTarget(batchCtx, RefreshTargetEpisode, "episode-s05e04"); err == nil {
		t.Fatal("RefreshScheduledTarget succeeded, want the second language's lookup error")
	}
	updated, err := h.service.episodeRepo.GetBySeriesAndNumber(ctx, seriesID, 5, 4)
	if err != nil {
		t.Fatalf("GetBySeriesAndNumber S05E04: %v", err)
	}
	if updated.Overview != "Episode 4 overview" {
		t.Fatalf("S05E04 overview = %q, want the first language's rows written", updated.Overview)
	}

	flush(ctx)

	if got := linkPasses.Load(); got != 1 {
		t.Fatalf("series link passes after the flush = %d, want 1 for the rows the first language wrote", got)
	}
}

func TestScheduledRefreshBatchFlushSyncsEachRecordedSeriesOnce(t *testing.T) {
	h := newTestHarness()
	h.service.episodeRepo = newFakeEpisodeRepo()
	var mu sync.Mutex
	passes := make(map[string]int)
	h.service.hooks.ensureSeriesEpisodeLinks = func(_ context.Context, seriesID string) error {
		mu.Lock()
		passes[seriesID]++
		mu.Unlock()
		return nil
	}
	ctx := context.Background()

	batchCtx, flush := h.service.BeginScheduledRefreshBatch(ctx)
	seriesIDs := make([]string, 0, 2*scheduledRefreshFlushWorkers)
	for i := range 2 * scheduledRefreshFlushWorkers {
		seriesIDs = append(seriesIDs, fmt.Sprintf("series-%02d", i))
	}
	for range 3 {
		for _, seriesID := range seriesIDs {
			h.service.syncSeriesEpisodeStateOrDefer(batchCtx, seriesID)
		}
	}
	flush(ctx)
	flush(ctx)

	mu.Lock()
	defer mu.Unlock()
	if len(passes) != len(seriesIDs) {
		t.Fatalf("series synced = %d, want %d", len(passes), len(seriesIDs))
	}
	for _, seriesID := range seriesIDs {
		if passes[seriesID] != 1 {
			t.Fatalf("series %s link passes = %d, want 1", seriesID, passes[seriesID])
		}
	}
}

// lockedLogBuffer serializes writes from any goroutine that logs while a test
// has replaced the default logger.
type lockedLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureDefaultLogs(t *testing.T) *lockedLogBuffer {
	t.Helper()
	buf := &lockedLogBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return buf
}

func TestSeriesDebtSweepDoesNotRepeatTerminalWarnings(t *testing.T) {
	h, seriesID, seasonID, _ := seedTargetRefreshHarness(&targetRefreshProvider{})
	ctx := context.Background()
	debts := newFakeRefreshDebtRepo()
	h.service.refreshDebtRepo = debts
	for _, episode := range []struct {
		contentID string
		number    int
	}{{"episode-s05e03", 3}, {"episode-s05e04", 4}} {
		if err := h.service.episodeRepo.Upsert(ctx, &models.Episode{
			ContentID:      episode.contentID,
			SeriesID:       seriesID,
			SeasonID:       seasonID,
			SeasonNumber:   5,
			EpisodeNumber:  episode.number,
			MetadataSource: "scanner_fallback",
		}); err != nil {
			t.Fatalf("seed incomplete episode %s: %v", episode.contentID, err)
		}
		debts.debts[fakeRefreshDebtKey(RefreshTargetEpisode, episode.contentID)] = &models.MetadataRefreshDebt{
			TargetType:   RefreshTargetEpisode,
			ContentID:    episode.contentID,
			ReasonMask:   RefreshDebtReasonEpisodeIncomplete,
			AttemptCount: refreshDebtEpisodeTerminalAttempts,
		}
	}
	logs := captureDefaultLogs(t)

	// Every series-wide sweep re-syncs every incomplete episode. Rows already
	// sitting at the terminal count must not announce the transition again.
	for range 3 {
		h.service.refreshSeriesEpisodeMetadataState(ctx, seriesID, time.Now())
	}
	if got := strings.Count(logs.String(), "reached terminal attempts"); got != 0 {
		t.Fatalf("terminal warnings from series sweeps = %d, want 0\n%s", got, logs.String())
	}

	// The refreshed target itself still reports the claim that took it to the
	// terminal count, once.
	if err := h.service.syncRefreshDebtForEpisode(ctx, "episode-s05e03"); err != nil {
		t.Fatalf("syncRefreshDebtForEpisode: %v", err)
	}
	if got := strings.Count(logs.String(), "reached terminal attempts"); got != 1 {
		t.Fatalf("terminal warnings after the target's own sync = %d, want 1\n%s", got, logs.String())
	}
	if !strings.Contains(logs.String(), "content_id=episode-s05e03") {
		t.Fatalf("terminal warning does not name the refreshed target\n%s", logs.String())
	}
}
