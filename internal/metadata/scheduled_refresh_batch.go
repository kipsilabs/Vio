package metadata

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// scheduledRefreshSeriesSyncTimeout bounds each series' deferred link and
// debt pass when a scheduled refresh batch is flushed. It matches the
// per-target timeout of the scheduled refresh task.
const scheduledRefreshSeriesSyncTimeout = 2 * time.Minute

type scheduledRefreshBatchKey struct{}

// scheduledRefreshBatch collects the series whose seasons or episodes were
// refreshed during one scheduled refresh batch. The series-wide link and debt
// passes run once per collected series when the batch is flushed, instead of
// once per refreshed season or episode.
type scheduledRefreshBatch struct {
	mu     sync.Mutex
	series []string
	seen   map[string]struct{}
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

func (b *scheduledRefreshBatch) take() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	series := b.series
	b.series = nil
	b.seen = make(map[string]struct{})
	return series
}

// BeginScheduledRefreshBatch starts a scheduled refresh batch. Season and
// episode refreshes run with the returned context record their series instead
// of relinking and re-syncing the whole series each time; the returned flush
// runs those series-wide passes once per series and must be called after every
// refresh in the batch has returned. Whole-series refreshes keep their passes
// inline because their own debt sync reads the result.
//
// A flush whose context is already done skips the remaining series. Their
// links and episode debt catch up on the next refresh or scan of the series.
func (s *MetadataService) BeginScheduledRefreshBatch(ctx context.Context) (context.Context, func(context.Context)) {
	batch := &scheduledRefreshBatch{seen: make(map[string]struct{})}
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

func (s *MetadataService) flushScheduledRefreshBatch(ctx context.Context, batch *scheduledRefreshBatch) {
	series := batch.take()
	for i, seriesID := range series {
		if err := ctx.Err(); err != nil {
			slog.InfoContext(ctx, "metadata: skipped deferred series sync for a cancelled refresh batch", "component", "metadata",
				"skipped_series", len(series)-i, "error", err)
			return
		}
		seriesCtx, cancel := context.WithTimeout(ctx, scheduledRefreshSeriesSyncTimeout)
		s.syncSeriesEpisodeState(seriesCtx, seriesID)
		cancel()
	}
}
