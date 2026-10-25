package catalog

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/userstore"
)

// NextUpQuery controls what the next-up lookup returns.
type NextUpQuery struct {
	UserID           int
	ProfileID        string
	LibraryID        *int
	LibraryIDs       []int
	SeriesID         string // optional: filter to single series
	AccessFilter     AccessFilter
	Limit            int
	EnableResumable  bool       // include in-progress episodes
	EnableRewatching bool       // accepted but deferred (no-op)
	DateCutoff       *time.Time // only series with activity after this date
	// RankByRelease orders results by ActivityAt, so a series whose next
	// episode was released after the profile finished the previous one moves
	// up (the profile's ui.next_up_new_releases_first). Off, results keep the
	// completion-time order and ActivityAt equals CompletedAt.
	RankByRelease bool

	// droppedSeriesIDs are the profile's actively dropped series, which a
	// global lookup excludes. ListNextUp loads them; a series-scoped lookup
	// never excludes its own series.
	droppedSeriesIDs []string
}

// NextUpResult is one row from the next-up query.
type NextUpResult struct {
	ContentID     string
	SeriesID      string
	SeriesTitle   string
	SeasonNumber  int
	EpisodeNumber int
	CompletedAt   time.Time // when the preceding episode was completed
	// ActivityAt orders the results: CompletedAt, or the time this episode was
	// released to the server when that came later. A series the profile had
	// caught up on moves to the front when its next episode arrives.
	ActivityAt  time.Time
	IsResumable bool // true if this is an in-progress item (enableResumable)
}

// NextUpRepository queries for next unwatched episodes per series.
type NextUpRepository struct {
	pool          *pgxpool.Pool
	storeProvider userstore.UserStoreProvider
	// statePageSize overrides nextUpStatePageSize for tests that need to drive
	// page boundaries without seeding hundreds of rows. Zero means the default.
	statePageSize int
}

// NewNextUpRepository creates a NextUpRepository.
func NewNextUpRepository(pool *pgxpool.Pool, storeProvider userstore.UserStoreProvider) *NextUpRepository {
	return &NextUpRepository{pool: pool, storeProvider: storeProvider}
}

// Next Up state paging constants.
const (
	// nextUpStatePageSize is the keyset page size requested from the store. The
	// walk may read several pages; the size only bounds one provider round trip.
	nextUpStatePageSize = 500

	// nextUpResumableScanLimit preserves the shipped in-progress scan window for
	// the resumable-first-episode branch. It is a row cap on the store's
	// in-progress listing, not an anchor cap.
	nextUpResumableScanLimit = 100

	// nextUpRankedCandidates is how many next-up rows are considered when
	// ranking by release, whatever the requested limit, so a newly released
	// episode can lift its series before the list is trimmed.
	nextUpRankedCandidates = 100
)

// statePageLimit returns the page size to request from the store.
func (r *NextUpRepository) statePageLimit() int {
	if r.statePageSize > 0 {
		return r.statePageSize
	}
	return nextUpStatePageSize
}

// nextUpEpisode is the catalog metadata for one episode resolved from the
// profile's Next Up state.
type nextUpEpisode struct {
	ContentID     string
	SeriesID      string
	SeriesTitle   string
	SeasonNumber  int
	EpisodeNumber int
}

// nextUpAnchor is the newest visible completion for one series. "Visible"
// means the store already excluded hidden-history rows before it returned the
// entry; catalog visibility means the episode and its parent series resolve.
type nextUpAnchor struct {
	nextUpEpisode
	UpdatedAt time.Time
}

// newerThan reports whether a should anchor the series instead of b. The
// ordering is the anchor contract: newest updated_at wins, then the highest
// (season, episode), then the highest content id.
func (a nextUpAnchor) newerThan(b nextUpAnchor) bool {
	if !a.UpdatedAt.Equal(b.UpdatedAt) {
		return a.UpdatedAt.After(b.UpdatedAt)
	}
	if a.SeasonNumber != b.SeasonNumber {
		return a.SeasonNumber > b.SeasonNumber
	}
	if a.EpisodeNumber != b.EpisodeNumber {
		return a.EpisodeNumber > b.EpisodeNumber
	}
	return a.ContentID > b.ContentID
}

// ListNextUp returns the next unwatched episode per series for the given user.
//
// The profile's progress lives only behind the user store, never in the
// catalog database, so the repository pages the store's NextUpStateStore
// capability and resolves the returned media item ids against Postgres catalog
// metadata. Anchors and the per-series successor are decided in Go after
// paging; the store's pages are keyset-bounded and the walk stops as soon as it
// has enough correctly ordered results or the state is exhausted. Before
// returning, the resolver reads exact item-scoped state for the candidate
// series (ListNextUpStateForItems) so the result does not depend on how far the
// walk happened to get; see nextUpResolver.
//
// A store that does not implement userstore.NextUpStateStore is a hard error:
// there is no fallback that joins catalog SQL against user-state tables.
func (r *NextUpRepository) ListNextUp(ctx context.Context, q NextUpQuery) ([]NextUpResult, error) {
	if r.storeProvider == nil || q.UserID <= 0 || q.ProfileID == "" {
		return nil, nil
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 20
	}

	store, err := r.storeProvider.ForUser(ctx, q.UserID)
	if err != nil {
		return nil, fmt.Errorf("getting user store: %w", err)
	}
	stateStore, ok := store.(userstore.NextUpStateStore)
	if !ok {
		return nil, fmt.Errorf("next up: user store %T does not implement userstore.NextUpStateStore", store)
	}
	if q.SeriesID == "" {
		dropped, err := activeDroppedSeriesIDs(ctx, r.pool, q.UserID, q.ProfileID)
		if err != nil {
			return nil, err
		}
		q.droppedSeriesIDs = dropped
	}

	walkLimit := limit
	if q.RankByRelease {
		walkLimit = max(limit, nextUpRankedCandidates)
	}

	var results []NextUpResult
	var anchors map[string]nextUpAnchor
	if q.SeriesID != "" {
		results, anchors, err = r.listNextUpForSeries(ctx, q, stateStore)
	} else {
		results, anchors, err = r.listNextUpGlobal(ctx, q, stateStore, walkLimit)
	}
	if err != nil {
		return nil, err
	}

	if q.EnableResumable {
		resumable, rErr := r.listResumableEpisodes(ctx, q, store, stateStore, anchors)
		if rErr != nil {
			return nil, rErr
		}

		if q.SeriesID != "" && len(resumable) > 0 {
			// Show-detail tile: the in-progress episode wins over whatever
			// "next aired" row the completed-episodes branch produced.
			return resumable, nil
		}

		// Global next-up: dedup by series, completed-next row takes priority.
		seen := make(map[string]bool, len(results))
		for _, res := range results {
			seen[res.SeriesID] = true
		}
		for _, res := range resumable {
			if !seen[res.SeriesID] {
				results = append(results, res)
			}
		}
	}

	for i := range results {
		if results[i].ActivityAt.IsZero() {
			results[i].ActivityAt = results[i].CompletedAt
		}
	}
	if q.RankByRelease {
		if err := r.applyNextUpReleaseActivity(ctx, q, results); err != nil {
			return nil, err
		}
		sortNextUpByActivity(results)
	}
	if q.SeriesID == "" && len(results) > limit {
		results = results[:limit]
	}

	return results, nil
}

// sortNextUpByActivity orders next-up rows by ActivityAt, newest first, with
// completion time and then series ID (one row per series) breaking ties.
func sortNextUpByActivity(results []NextUpResult) {
	sort.SliceStable(results, func(i, j int) bool {
		if c := results[j].ActivityAt.Compare(results[i].ActivityAt); c != 0 {
			return c < 0
		}
		if c := results[j].CompletedAt.Compare(results[i].CompletedAt); c != 0 {
			return c < 0
		}
		return results[i].SeriesID < results[j].SeriesID
	})
}

// nextUpReleaseScope limits release lookups to the libraries the request can
// reach, so an episode that arrived where the viewer can't follow never moves
// a series ahead of one they can watch.
func nextUpReleaseScope(q NextUpQuery, argIdx int) (string, []interface{}) {
	var requested []int
	if q.LibraryID != nil {
		requested = []int{*q.LibraryID}
	} else {
		requested = q.LibraryIDs
	}
	ids, none := q.AccessFilter.LibraryScope(requested)
	if none {
		ids = []int{}
	}
	switch {
	case ids != nil:
		return fmt.Sprintf(" AND ea.library_id = ANY($%d::int[])", argIdx), []interface{}{ids}
	case len(q.AccessFilter.DisabledLibraryIDs) > 0:
		return fmt.Sprintf(" AND NOT (ea.library_id = ANY($%d::int[]))", argIdx), []interface{}{q.AccessFilter.DisabledLibraryIDs}
	default:
		return "", nil
	}
}

// applyNextUpReleaseActivity lifts a next-up row's ActivityAt to the time its
// episode was released to the server, following the availability contract:
// released_at counts only when it still equals the row's available_at, so an
// initial scan, imported history, or a merged content ID never moves a series.
func (r *NextUpRepository) applyNextUpReleaseActivity(ctx context.Context, q NextUpQuery, results []NextUpResult) error {
	if len(results) == 0 {
		return nil
	}
	ids := make([]string, len(results))
	byContentID := make(map[string]int, len(results))
	for i := range results {
		ids[i] = results[i].ContentID
		byContentID[results[i].ContentID] = i
	}
	scope, scopeArgs := nextUpReleaseScope(q, 2)
	query := `SELECT DISTINCT ON (ea.episode_id) ea.episode_id,
			CASE WHEN ea.released_at = ea.available_at THEN ea.released_at END
		FROM episode_availability ea
		WHERE ea.episode_id = ANY($1::text[])` + scope + `
		ORDER BY ea.episode_id, ea.available_at, ea.library_id`
	args := append([]any{ids}, scopeArgs...)
	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("querying next-up release times: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var contentID string
		var releasedAt *time.Time
		if err := rows.Scan(&contentID, &releasedAt); err != nil {
			return fmt.Errorf("scanning next-up release time: %w", err)
		}
		if releasedAt == nil {
			continue
		}
		if i, ok := byContentID[contentID]; ok && releasedAt.After(results[i].ActivityAt) {
			results[i].ActivityAt = *releasedAt
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating next-up release times: %w", err)
	}
	return nil
}

// listNextUpGlobal walks the store's state pages newest-first, resolves each
// page against the catalog, and returns the next episode for each eligible
// series in (anchor updated_at DESC, series_id) order.
//
// The walk stops at:
//   - state exhaustion,
//   - the date cutoff (state is updated_at DESC, so every later row is older),
//   - or enough eligible results once no future row can tie the last selected
//     anchor. This replaces the old fixed 960-series ceiling: ineligible series
//     (no successor, suppressed by newer in-progress, hidden, or outside the
//     cutoff) never consume the budget, so an older eligible series is reached.
//
// The prefix of state the walk accumulates is not enough to decide a returned
// series' successor: an out-of-order watch can leave a post-anchor episode's
// state row below the stop threshold, so the walk never sees it. Once the walk
// could stop (or is exhausted) the resolver fetches exact item-scoped state for
// the candidate series, keeping the returned rows exact while the walk stays
// bounded. See nextUpResolver.
func (r *NextUpRepository) listNextUpGlobal(
	ctx context.Context,
	q NextUpQuery,
	stateStore userstore.NextUpStateStore,
	limit int,
) ([]NextUpResult, map[string]nextUpAnchor, error) {
	anchors := make(map[string]nextUpAnchor)
	resolver := &nextUpResolver{
		repo:       r,
		q:          q,
		stateStore: stateStore,
		cache:      make(map[string]nextUpCandidateResolution),
	}

	var cursor *userstore.NextUpStateCursor
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}

		page, err := stateStore.ListNextUpStatePage(ctx, q.ProfileID, cursor, r.statePageLimit())
		if err != nil {
			return nil, nil, err
		}

		if err := r.accumulateNextUpAnchors(ctx, q, page, anchors); err != nil {
			return nil, nil, err
		}

		exhausted := page.Exhausted || page.Next == nil || len(page.Entries) == 0
		cutoffReached := nextUpPageReachedCutoff(q, page)

		// Exact resolution is the expensive part and only matters once the walk
		// could stop: with fewer anchors than the limit it cannot fill a page,
		// so hold off unless the walk is done.
		var results []NextUpResult
		if exhausted || cutoffReached || len(anchors) >= limit {
			results, err = resolver.resolve(ctx, anchors, limit)
			if err != nil {
				return nil, nil, err
			}
		}

		if exhausted || cutoffReached {
			return results, anchors, nil
		}
		if len(results) >= limit && nextUpPageBelowThreshold(page, results[limit-1].CompletedAt) {
			return results, anchors, nil
		}

		cursor = page.Next
	}
}

// droppedSeriesSet builds the exclusion set for global lookups. Nil when the
// query is series-scoped or no series are dropped: dropped series never become
// anchors in a global lookup, so they neither surface nor consume the walk's
// series budget, while series-scoped lookups keep their own series.
func droppedSeriesSet(q NextUpQuery) map[string]struct{} {
	if q.SeriesID != "" || len(q.droppedSeriesIDs) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(q.droppedSeriesIDs))
	for _, id := range q.droppedSeriesIDs {
		if id != "" {
			set[id] = struct{}{}
		}
	}
	return set
}

// accumulateNextUpAnchors folds one state page into the per-series anchor map.
// Entries older than the date cutoff stop contributing anchors; the exact
// resolver owns per-series in-progress and successor state from here.
func (r *NextUpRepository) accumulateNextUpAnchors(
	ctx context.Context,
	q NextUpQuery,
	page userstore.NextUpStatePage,
	anchors map[string]nextUpAnchor,
) error {
	if len(page.Entries) == 0 {
		return nil
	}

	resolved, err := r.resolveNextUpEpisodes(ctx, nextUpStateContentIDs(page.Entries))
	if err != nil {
		return err
	}

	cutoffReached := false
	dropped := droppedSeriesSet(q)
	for _, entry := range page.Entries {
		if q.DateCutoff != nil && entry.UpdatedAt.Before(*q.DateCutoff) {
			cutoffReached = true
		}
		if cutoffReached || !entry.Completed {
			continue
		}
		meta, ok := resolved[entry.MediaItemID]
		if !ok {
			continue
		}
		if _, isDropped := dropped[meta.SeriesID]; isDropped {
			continue
		}
		candidate := nextUpAnchor{nextUpEpisode: meta, UpdatedAt: entry.UpdatedAt}
		if current, ok := anchors[meta.SeriesID]; !ok || candidate.newerThan(current) {
			anchors[meta.SeriesID] = candidate
		}
	}
	return nil
}

// listNextUpForSeries resolves one series' newest visible completion. Unlike
// the global walk it cannot stop at the first eligible result: for the chosen
// series it must page to the end of the profile's state so the successor
// exclusion sees every state row that could follow the anchor. The anchor
// itself is never bounded by recency — a long-idle series still anchors on its
// last completed episode.
//
// This is the deliberate worst case: one series-scoped request reads the
// profile's whole state stream (bounded in memory to that one series' ids).
func (r *NextUpRepository) listNextUpForSeries(
	ctx context.Context,
	q NextUpQuery,
	stateStore userstore.NextUpStateStore,
) ([]NextUpResult, map[string]nextUpAnchor, error) {
	anchors := make(map[string]nextUpAnchor)
	stateIDs := make(map[string]struct{})

	var anchor nextUpAnchor
	haveAnchor := false
	var inProgressMax time.Time
	haveInProgress := false

	var cursor *userstore.NextUpStateCursor
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}

		page, err := stateStore.ListNextUpStatePage(ctx, q.ProfileID, cursor, r.statePageLimit())
		if err != nil {
			return nil, nil, err
		}

		if len(page.Entries) > 0 {
			resolved, err := r.resolveNextUpEpisodes(ctx, nextUpStateContentIDs(page.Entries))
			if err != nil {
				return nil, nil, err
			}
			cutoffReached := false
			for _, entry := range page.Entries {
				if q.DateCutoff != nil && entry.UpdatedAt.Before(*q.DateCutoff) {
					cutoffReached = true
				}
				meta, ok := resolved[entry.MediaItemID]
				if !ok || meta.SeriesID != q.SeriesID {
					continue
				}
				stateIDs[entry.MediaItemID] = struct{}{}
				if cutoffReached {
					continue
				}
				candidate := nextUpAnchor{nextUpEpisode: meta, UpdatedAt: entry.UpdatedAt}
				if entry.Completed {
					if !haveAnchor || candidate.newerThan(anchor) {
						anchor = candidate
						haveAnchor = true
					}
					continue
				}
				if entry.Position > 0 {
					if !haveInProgress || entry.UpdatedAt.After(inProgressMax) {
						inProgressMax = entry.UpdatedAt
						haveInProgress = true
					}
				}
			}
		}

		if page.Exhausted || page.Next == nil {
			break
		}
		cursor = page.Next
	}

	if !haveAnchor {
		return nil, anchors, nil
	}
	anchors[anchor.SeriesID] = anchor
	if !q.EnableResumable && haveInProgress && inProgressMax.After(anchor.UpdatedAt) {
		return nil, anchors, nil
	}

	successors, err := r.lookupNextUpSuccessors(ctx, []nextUpAnchor{anchor}, stateIDs)
	if err != nil {
		return nil, nil, err
	}
	successor, ok := successors[q.SeriesID]
	if !ok {
		return nil, anchors, nil
	}
	return []NextUpResult{nextUpResult(anchor, successor)}, anchors, nil
}

// nextUpResolveBatchSize bounds how many unresolved candidate series one exact
// resolution round trip covers. The catalog and store reads are batched, so a
// larger batch is fewer round trips; a smaller batch avoids fetching state for
// candidates an early stop would never return.
const nextUpResolveBatchSize = 32

// nextUpCandidateResolution is the exact per-series state the resolver needs to
// decide a candidate: whether a successor exists and the newest in-progress
// timestamp. It is cached across page iterations, so a candidate is resolved at
// most once per ListNextUp call.
type nextUpCandidateResolution struct {
	successor     nextUpEpisode
	hasSuccessor  bool
	inProgressMax time.Time
}

// nextUpResolver resolves candidate series against exact item-scoped state.
//
// The page walk only sees a prefix of the profile's state, newest first. A
// post-anchor episode watched out of order can hold an older state row the walk
// never reached, so successor exclusion and the in-progress gate read exact
// state for the candidate series' own episodes instead of trusting the prefix.
// Candidates the walk has not reached cannot be returned, so the walk can still
// stop early.
type nextUpResolver struct {
	repo       *NextUpRepository
	q          NextUpQuery
	stateStore userstore.NextUpStateStore
	cache      map[string]nextUpCandidateResolution
}

// resolve returns up to limit results for the candidate anchors in
// (updated_at DESC, series_id) order. It resolves cache-miss candidates in
// batches and stops resolving once the limit is filled, so an early-stopped
// walk never pays for candidates it will not return.
func (res *nextUpResolver) resolve(ctx context.Context, anchors map[string]nextUpAnchor, limit int) ([]NextUpResult, error) {
	candidates := make([]nextUpAnchor, 0, len(anchors))
	for _, anchor := range anchors {
		candidates = append(candidates, anchor)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].UpdatedAt.Equal(candidates[j].UpdatedAt) {
			return candidates[i].UpdatedAt.After(candidates[j].UpdatedAt)
		}
		return candidates[i].SeriesID < candidates[j].SeriesID
	})

	results := make([]NextUpResult, 0, limit)
	for i := 0; i < len(candidates) && len(results) < limit; {
		end := i + nextUpResolveBatchSize
		if end > len(candidates) {
			end = len(candidates)
		}

		unresolved := make([]nextUpAnchor, 0, end-i)
		for _, candidate := range candidates[i:end] {
			if _, ok := res.cache[candidate.SeriesID]; !ok {
				unresolved = append(unresolved, candidate)
			}
		}
		if len(unresolved) > 0 {
			if err := res.resolveBatch(ctx, unresolved); err != nil {
				return nil, err
			}
		}

		for ; i < end && len(results) < limit; i++ {
			candidate := candidates[i]
			resolution := res.cache[candidate.SeriesID]
			// When resumable items are disabled, a series with in-progress
			// activity newer than its newest completion is mid-watch; surfacing
			// the episode after the completion would skip past the position.
			if !res.q.EnableResumable && !resolution.inProgressMax.IsZero() && resolution.inProgressMax.After(candidate.UpdatedAt) {
				continue
			}
			if !resolution.hasSuccessor {
				continue
			}
			results = append(results, nextUpResult(candidate, resolution.successor))
		}
	}
	return results, nil
}

// resolveBatch fetches exact state for the given candidate series in one
// catalog query and one store query, then resolves their successors in one
// more catalog query.
func (res *nextUpResolver) resolveBatch(ctx context.Context, candidates []nextUpAnchor) error {
	episodeToSeries, err := res.repo.nextUpPostAnchorEpisodes(ctx, candidates)
	if err != nil {
		return err
	}
	episodeIDs := make([]string, 0, len(episodeToSeries))
	for episodeID := range episodeToSeries {
		episodeIDs = append(episodeIDs, episodeID)
	}

	entries, err := res.stateStore.ListNextUpStateForItems(ctx, res.q.ProfileID, episodeIDs)
	if err != nil {
		return err
	}

	stateIDs := make(map[string]struct{}, len(entries))
	inProgressMax := make(map[string]time.Time)
	for _, entry := range entries {
		seriesID, ok := episodeToSeries[entry.MediaItemID]
		if !ok {
			continue
		}
		stateIDs[entry.MediaItemID] = struct{}{}
		if !entry.Completed && entry.Position > 0 {
			if current, ok := inProgressMax[seriesID]; !ok || entry.UpdatedAt.After(current) {
				inProgressMax[seriesID] = entry.UpdatedAt
			}
		}
	}

	successors, err := res.repo.lookupNextUpSuccessors(ctx, candidates, stateIDs)
	if err != nil {
		return err
	}

	for _, candidate := range candidates {
		resolution := nextUpCandidateResolution{}
		if at, ok := inProgressMax[candidate.SeriesID]; ok {
			resolution.inProgressMax = at
		}
		if successor, ok := successors[candidate.SeriesID]; ok {
			resolution.successor = successor
			resolution.hasSuccessor = true
		}
		res.cache[candidate.SeriesID] = resolution
	}
	return nil
}

// nextUpResult builds the response row for an anchor/successor pair.
func nextUpResult(anchor nextUpAnchor, successor nextUpEpisode) NextUpResult {
	return NextUpResult{
		ContentID:     successor.ContentID,
		SeriesID:      anchor.SeriesID,
		SeriesTitle:   anchor.SeriesTitle,
		SeasonNumber:  successor.SeasonNumber,
		EpisodeNumber: successor.EpisodeNumber,
		CompletedAt:   anchor.UpdatedAt,
	}
}

// nextUpStateContentIDs extracts the media item ids from a state page.
func nextUpStateContentIDs(entries []userstore.NextUpStateEntry) []string {
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if id := strings.TrimSpace(entry.MediaItemID); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// nextUpPageReachedCutoff reports whether any entry on the page is older than
// the query's date cutoff. Because the store pages updated_at DESC, that means
// every subsequent page is older too.
func nextUpPageReachedCutoff(q NextUpQuery, page userstore.NextUpStatePage) bool {
	if q.DateCutoff == nil {
		return false
	}
	for _, entry := range page.Entries {
		if entry.UpdatedAt.Before(*q.DateCutoff) {
			return true
		}
	}
	return false
}

// nextUpPageBelowThreshold reports whether the page's oldest timestamp is
// strictly older than threshold. If so, no future entry can tie or outrank the
// result anchored at threshold, so the walk may stop without skipping a
// deterministic tie held on a later page.
func nextUpPageBelowThreshold(page userstore.NextUpStatePage, threshold time.Time) bool {
	if len(page.Entries) == 0 {
		return true
	}
	return page.Entries[len(page.Entries)-1].UpdatedAt.Before(threshold)
}

// nextUpEpisodeLookupQuery resolves a batch of media item ids to episode and
// parent-series metadata. One query per state page; never per episode.
var nextUpEpisodeLookupQuery = `
	SELECT e.content_id, e.series_id, e.season_number, e.episode_number, si.title
	FROM episodes e
	JOIN media_items si ON si.content_id = e.series_id
	WHERE e.content_id = ANY($1::text[])
	  AND ` + episodeAliasExclusionPredicateFor("e")

// resolveNextUpEpisodes batch-resolves media item ids against the catalog.
// Ids that are not episodes (or whose series row is absent) are simply absent
// from the result, matching the old join semantics.
func (r *NextUpRepository) resolveNextUpEpisodes(ctx context.Context, contentIDs []string) (map[string]nextUpEpisode, error) {
	resolved := make(map[string]nextUpEpisode, len(contentIDs))
	if len(contentIDs) == 0 {
		return resolved, nil
	}

	rows, err := r.pool.Query(ctx, nextUpEpisodeLookupQuery, contentIDs)
	if err != nil {
		return nil, fmt.Errorf("resolving next-up episodes: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var episode nextUpEpisode
		if err := rows.Scan(
			&episode.ContentID, &episode.SeriesID, &episode.SeasonNumber,
			&episode.EpisodeNumber, &episode.SeriesTitle,
		); err != nil {
			return nil, fmt.Errorf("scanning next-up episode: %w", err)
		}
		resolved[episode.ContentID] = episode
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating next-up episodes: %w", err)
	}
	return resolved, nil
}

// nextUpPostAnchorEpisodesQuery resolves, for each candidate anchor, the
// content ids of every episode after it. The resolver asks the user store for
// exact state on exactly those episodes, so a post-anchor watch is excluded no
// matter how far down the recency-ordered page walk it sits.
var nextUpPostAnchorEpisodesQuery = `
	SELECT e.content_id, e.series_id
	FROM unnest($1::text[], $2::int[], $3::int[]) AS anchor(series_id, season_number, episode_number)
	JOIN episodes e
	  ON e.series_id = anchor.series_id
	 AND (e.season_number, e.episode_number) > (anchor.season_number, anchor.episode_number)
	WHERE ` + episodeAliasExclusionPredicateFor("e")

// nextUpPostAnchorEpisodes returns episode content id -> parent series id for
// every episode after each candidate's anchor. The caller feeds the ids to the
// user store's exact state lookup.
func (r *NextUpRepository) nextUpPostAnchorEpisodes(ctx context.Context, candidates []nextUpAnchor) (map[string]string, error) {
	episodeToSeries := make(map[string]string, len(candidates))
	if len(candidates) == 0 {
		return episodeToSeries, nil
	}

	seriesIDs := make([]string, len(candidates))
	seasons := make([]int, len(candidates))
	episodes := make([]int, len(candidates))
	for i, candidate := range candidates {
		seriesIDs[i] = candidate.SeriesID
		seasons[i] = candidate.SeasonNumber
		episodes[i] = candidate.EpisodeNumber
	}

	rows, err := r.pool.Query(ctx, nextUpPostAnchorEpisodesQuery, seriesIDs, seasons, episodes)
	if err != nil {
		return nil, fmt.Errorf("resolving next-up post-anchor episodes: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var contentID, seriesID string
		if err := rows.Scan(&contentID, &seriesID); err != nil {
			return nil, fmt.Errorf("scanning next-up post-anchor episode: %w", err)
		}
		episodeToSeries[contentID] = seriesID
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating next-up post-anchor episodes: %w", err)
	}
	return episodeToSeries, nil
}

// nextUpSeriesEpisodesQuery resolves every episode content id for the given
// series. The resumable gate needs a whole-series view: a completion anywhere
// in the series disqualifies it from the resumable branch.
var nextUpSeriesEpisodesQuery = `
	SELECT e.content_id, e.series_id
	FROM episodes e
	WHERE e.series_id = ANY($1::text[])
	  AND ` + episodeAliasExclusionPredicateFor("e")

// nextUpSeriesEpisodes returns episode content id -> parent series id for every
// episode of the given series.
func (r *NextUpRepository) nextUpSeriesEpisodes(ctx context.Context, seriesIDs []string) (map[string]string, error) {
	episodeToSeries := make(map[string]string)
	if len(seriesIDs) == 0 {
		return episodeToSeries, nil
	}

	rows, err := r.pool.Query(ctx, nextUpSeriesEpisodesQuery, seriesIDs)
	if err != nil {
		return nil, fmt.Errorf("resolving next-up series episodes: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var contentID, seriesID string
		if err := rows.Scan(&contentID, &seriesID); err != nil {
			return nil, fmt.Errorf("scanning next-up series episode: %w", err)
		}
		episodeToSeries[contentID] = seriesID
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating next-up series episodes: %w", err)
	}
	return episodeToSeries, nil
}

// nextUpSuccessorQuery resolves, for every candidate anchor, the first canonical
// episode after it with an available (non-missing) file and no state row. A
// canonical episode qualifies when it owns a present file directly or through
// one of its alias rows. The state ids arrive as an array because they live
// behind the user store; the catalog query only supplies the ordering and
// availability.
//
// The episode tie-break is (season, episode, content_id): episodes of one
// series share the first two, and content_id keeps the pick stable when a
// catalog somehow carries duplicate numbering.
var nextUpSuccessorQuery = `
	SELECT anchor.series_id, next_ep.content_id, next_ep.season_number, next_ep.episode_number
	FROM unnest($1::text[], $2::int[], $3::int[]) AS anchor(series_id, season_number, episode_number)
	JOIN media_items si ON si.content_id = anchor.series_id
	JOIN LATERAL (
		SELECT e2.content_id, e2.season_number, e2.episode_number
		FROM episodes e2
		WHERE e2.series_id = anchor.series_id
		  AND (e2.season_number, e2.episode_number) > (anchor.season_number, anchor.episode_number)
		  AND ` + episodeAliasExclusionPredicateFor("e2") + `
		  AND ` + canonicalEpisodeFileExistsSQL("e2.content_id", "mf.missing_since IS NULL") + `
		  AND NOT (e2.content_id = ANY($4::text[]))
		ORDER BY e2.season_number, e2.episode_number, e2.content_id
		LIMIT 1
	) next_ep ON TRUE`

// lookupNextUpSuccessors resolves the successor for each candidate anchor in
// one query and returns them keyed by series id.
func (r *NextUpRepository) lookupNextUpSuccessors(
	ctx context.Context,
	candidates []nextUpAnchor,
	stateIDs map[string]struct{},
) (map[string]nextUpEpisode, error) {
	successors := make(map[string]nextUpEpisode, len(candidates))
	if len(candidates) == 0 {
		return successors, nil
	}

	seriesIDs := make([]string, len(candidates))
	seasons := make([]int, len(candidates))
	episodes := make([]int, len(candidates))
	for i, candidate := range candidates {
		seriesIDs[i] = candidate.SeriesID
		seasons[i] = candidate.SeasonNumber
		episodes[i] = candidate.EpisodeNumber
	}
	excluded := make([]string, 0, len(stateIDs))
	for id := range stateIDs {
		excluded = append(excluded, id)
	}

	rows, err := r.pool.Query(ctx, nextUpSuccessorQuery, seriesIDs, seasons, episodes, excluded)
	if err != nil {
		return nil, fmt.Errorf("resolving next-up successors: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var seriesID string
		var episode nextUpEpisode
		if err := rows.Scan(&seriesID, &episode.ContentID, &episode.SeasonNumber, &episode.EpisodeNumber); err != nil {
			return nil, fmt.Errorf("scanning next-up successor: %w", err)
		}
		episode.SeriesID = seriesID
		successors[seriesID] = episode
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating next-up successors: %w", err)
	}
	return successors, nil
}

// listResumableEpisodes finds in-progress episodes for the profile.
//
// Two callers, two semantics, one listing:
//
//   - Global next-up (q.SeriesID == ""): only contribute series the profile
//     has no completion for. Series with a completion go through the
//     "next unwatched after the last completed" path; this branch fills the gap
//     for series that have no completed anchor yet.
//   - Show-detail tile (q.SeriesID != ""): the in-progress episode wins
//     unconditionally, even when the user already finished earlier episodes of
//     the same series. ListNextUp flips its dedup priority to let this row win.
//
// The in-progress rows come from the user store (ListProgress), not from
// catalog SQL, so a SQLite-backed profile behaves the same as a Postgres one.
func (r *NextUpRepository) listResumableEpisodes(
	ctx context.Context,
	q NextUpQuery,
	store userstore.UserStore,
	stateStore userstore.NextUpStateStore,
	completedSeries map[string]nextUpAnchor,
) ([]NextUpResult, error) {
	if store == nil {
		return nil, nil
	}

	entries, err := store.ListProgress(ctx, q.ProfileID, "in_progress", nextUpResumableScanLimit, 0)
	if err != nil {
		return nil, fmt.Errorf("listing in-progress: %w", err)
	}
	if len(entries) == 0 {
		return nil, nil
	}

	contentIDs := make([]string, 0, len(entries))
	updatedAt := make(map[string]time.Time, len(entries))
	for _, entry := range entries {
		id := strings.TrimSpace(entry.MediaItemID)
		if id == "" {
			continue
		}
		at, parseErr := time.Parse(time.RFC3339, entry.UpdatedAt)
		if parseErr != nil || at.IsZero() {
			continue
		}
		contentIDs = append(contentIDs, id)
		updatedAt[id] = at.UTC()
	}
	if len(contentIDs) == 0 {
		return nil, nil
	}

	resolved, err := r.resolveNextUpEpisodes(ctx, contentIDs)
	if err != nil {
		return nil, err
	}

	newest := make(map[string]nextUpEpisode, len(resolved))
	newestAt := make(map[string]time.Time, len(resolved))
	for _, id := range contentIDs {
		episode, ok := resolved[id]
		if !ok {
			continue
		}
		at := updatedAt[id]
		if current, ok := newestAt[episode.SeriesID]; !ok || at.After(current) {
			newest[episode.SeriesID] = episode
			newestAt[episode.SeriesID] = at
		}
	}

	seriesIDs := make([]string, 0, len(newest))
	for seriesID := range newest {
		seriesIDs = append(seriesIDs, seriesID)
	}
	sort.Strings(seriesIDs)

	// The completed-series gate is global by design: any completion in the
	// series routes it through the completed-next path instead of this branch.
	// The anchor map only holds completions the (possibly early-stopped) walk
	// saw, so fill the rest from exact item-scoped state; otherwise a series
	// whose only completion is older than the stop would be mis-gated into
	// "resumable" even though it has history.
	completed := make(map[string]bool, len(seriesIDs))
	if q.SeriesID == "" {
		unresolved := make([]string, 0, len(seriesIDs))
		for _, seriesID := range seriesIDs {
			if _, ok := completedSeries[seriesID]; ok {
				completed[seriesID] = true
				continue
			}
			unresolved = append(unresolved, seriesID)
		}
		if len(unresolved) > 0 {
			if err := r.markResumableSeriesCompletions(ctx, q, stateStore, unresolved, completed); err != nil {
				return nil, err
			}
		}
	}

	results := make([]NextUpResult, 0, len(seriesIDs))
	dropped := droppedSeriesSet(q)
	for _, seriesID := range seriesIDs {
		if q.SeriesID != "" && seriesID != q.SeriesID {
			continue
		}
		if _, isDropped := dropped[seriesID]; isDropped {
			continue
		}
		if q.SeriesID == "" && completed[seriesID] {
			continue
		}
		episode := newest[seriesID]
		results = append(results, NextUpResult{
			ContentID:     episode.ContentID,
			SeriesID:      episode.SeriesID,
			SeriesTitle:   episode.SeriesTitle,
			SeasonNumber:  episode.SeasonNumber,
			EpisodeNumber: episode.EpisodeNumber,
			CompletedAt:   newestAt[seriesID],
			IsResumable:   true,
		})
	}
	return results, nil
}

// markResumableSeriesCompletions adds every series that has any completion to
// out. It resolves the series' whole episode set, then reads exact item-scoped
// state, so a completion below the anchor walk's stop still disqualifies the
// series from the resumable branch.
func (r *NextUpRepository) markResumableSeriesCompletions(
	ctx context.Context,
	q NextUpQuery,
	stateStore userstore.NextUpStateStore,
	seriesIDs []string,
	out map[string]bool,
) error {
	episodeToSeries, err := r.nextUpSeriesEpisodes(ctx, seriesIDs)
	if err != nil {
		return err
	}
	episodeIDs := make([]string, 0, len(episodeToSeries))
	for episodeID := range episodeToSeries {
		episodeIDs = append(episodeIDs, episodeID)
	}

	entries, err := stateStore.ListNextUpStateForItems(ctx, q.ProfileID, episodeIDs)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.Completed {
			continue
		}
		if seriesID, ok := episodeToSeries[entry.MediaItemID]; ok {
			out[seriesID] = true
		}
	}
	return nil
}
