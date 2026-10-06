package metadata

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const (
	// scheduledRefreshSeriesSyncTimeout bounds each series' deferred link and
	// debt pass when a scheduled refresh batch is flushed. It matches the
	// per-target timeout of the scheduled refresh task.
	scheduledRefreshSeriesSyncTimeout = 2 * time.Minute
	// scheduledRefreshFlushWorkers caps how many series a flush syncs at once.
	// It matches the scheduled refresh task's worker count, which is how many
	// of these passes could run at once before they were batched.
	scheduledRefreshFlushWorkers = 12
)

type scheduledRefreshBatchKey struct{}

// failedEpisodeDebtKey carries the episode targets whose refresh failure was
// recorded during the batch into the flush's series debt sweep.
type failedEpisodeDebtKey struct{}

// scheduledRefreshBatch collects the series whose seasons or episodes were
// refreshed during one scheduled refresh batch. The series-wide link and debt
// passes run once per collected series when the batch is flushed, instead of
// once per refreshed season or episode.
type scheduledRefreshBatch struct {
	mu             sync.Mutex
	series         []string
	seen           map[string]struct{}
	failedEpisodes map[string]struct{}
}

func (b *scheduledRefreshBatch) add(seriesID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.seen[seriesID]; ok {
		return
	}
	b.seen[seriesID] = struct{}{}
	b.series = append(b.series, seriesID)
}

func (b *scheduledRefreshBatch) markEpisodeFailed(episodeID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failedEpisodes[episodeID] = struct{}{}
}

func (b *scheduledRefreshBatch) take() ([]string, map[string]struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	series, failed := b.series, b.failedEpisodes
	b.series = nil
	b.seen = make(map[string]struct{})
	b.failedEpisodes = make(map[string]struct{})
	return series, failed
}

// BeginScheduledRefreshBatch starts a scheduled refresh batch. Season and
// episode refreshes run with the returned context record their series instead
// of relinking and re-syncing the whole series each time; the returned flush
// runs those series-wide passes once per series and must be called after every
// refresh in the batch has returned. Whole-series refreshes keep their passes
// inline because their own debt sync reads the result.
//
// An episode target whose failure was recorded during the batch keeps that
// failure: the flush's series sweep leaves its debt row alone, as the sweep
// that ran before the failure was recorded did when the passes were inline.
//
// A flush whose context is done skips the series it has not started. Their
// links and episode debt catch up on the next refresh or scan of the series.
func (s *MetadataService) BeginScheduledRefreshBatch(ctx context.Context) (context.Context, func(context.Context)) {
	batch := &scheduledRefreshBatch{seen: make(map[string]struct{}), failedEpisodes: make(map[string]struct{})}
	return context.WithValue(ctx, scheduledRefreshBatchKey{}, batch), func(flushCtx context.Context) {
		s.flushScheduledRefreshBatch(flushCtx, batch)
	}
}

func scheduledRefreshBatchFromContext(ctx context.Context) *scheduledRefreshBatch {
	batch, _ := ctx.Value(scheduledRefreshBatchKey{}).(*scheduledRefreshBatch)
	return batch
}

// syncSeriesEpisodeStateOrDefer runs the series-wide link and debt passes now,
// or records the series for the enclosing scheduled refresh batch.
func (s *MetadataService) syncSeriesEpisodeStateOrDefer(ctx context.Context, seriesID string) {
	if batch := scheduledRefreshBatchFromContext(ctx); batch != nil {
		batch.add(seriesID)
		return
	}
	s.syncSeriesEpisodeState(ctx, seriesID)
}

// noteScheduledRefreshFailure records an episode target whose refresh failure
// was written to its debt row, so the enclosing batch's series sweep keeps it.
// Canceled and timed-out refreshes record no failure, so there is nothing to keep.
func noteScheduledRefreshFailure(ctx context.Context, episodeID string, refreshErr error) {
	batch := scheduledRefreshBatchFromContext(ctx)
	episodeID = strings.TrimSpace(episodeID)
	if batch == nil || episodeID == "" || refreshErr == nil ||
		errors.Is(refreshErr, context.Canceled) || errors.Is(refreshErr, context.DeadlineExceeded) {
		return
	}
	batch.markEpisodeFailed(episodeID)
}

// failedEpisodeDebtFromContext returns the episode targets whose recorded
// failure a series debt sweep must leave in place.
func failedEpisodeDebtFromContext(ctx context.Context) map[string]struct{} {
	failed, _ := ctx.Value(failedEpisodeDebtKey{}).(map[string]struct{})
	return failed
}

func (s *MetadataService) flushScheduledRefreshBatch(ctx context.Context, batch *scheduledRefreshBatch) {
	series, failedEpisodes := batch.take()
	if len(failedEpisodes) > 0 {
		ctx = context.WithValue(ctx, failedEpisodeDebtKey{}, failedEpisodes)
	}
	slots := make(chan struct{}, scheduledRefreshFlushWorkers)
	var wg sync.WaitGroup
	for i, seriesID := range series {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
		}
		if err := ctx.Err(); err != nil {
			slog.InfoContext(ctx, "metadata: skipped deferred series sync for a canceled refresh batch", "component", "metadata",
				"skipped_series", len(series)-i, "error", err)
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			seriesCtx, cancel := context.WithTimeout(ctx, scheduledRefreshSeriesSyncTimeout)
			defer cancel()
			s.syncSeriesEpisodeState(seriesCtx, seriesID)
		}()
	}
	wg.Wait()
}
