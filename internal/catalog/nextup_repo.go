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

// nextUpAnchorsCTE places each series' anchor for the successor lookup.
// Specials carry no airing position and season 0 sorts first, so a special
// anchor would only ever lead to the next special. When the anchor is a
// special and the series has regular seasons, it moves to the profile's most
// recent completed regular episode, or, when the profile has watched only
// specials, to just before the first regular season. A series of specials
// alone keeps stepping through them. updated_at stays the completion that
// picked the series, so ordering and CompletedAt don't change; anchored_at is
// the completion the anchor now stands on, -infinity before any regular one.
const nextUpAnchorsCTE = `,
		anchors AS (
			SELECT
				ce.series_id,
				ce.updated_at,
				COALESCE(regular.season_number, ce.season_number) AS season_number,
				COALESCE(regular.episode_number, ce.episode_number) AS episode_number,
				regular.season_number IS NOT NULL AS moved,
				CASE
					WHEN regular.season_number IS NULL THEN ce.updated_at
					ELSE COALESCE(regular.completed_at, '-infinity'::timestamptz)
				END AS anchored_at
			FROM completed_episodes ce
			LEFT JOIN LATERAL (
				SELECT
					COALESCE(done.season_number, 0) AS season_number,
					COALESCE(done.episode_number, 2147483647) AS episode_number,
					done.updated_at AS completed_at
				FROM episodes e_reg
				LEFT JOIN LATERAL (
					SELECT e_d.season_number, e_d.episode_number, uwp_d.updated_at
					FROM episodes e_d
					JOIN user_watch_progress uwp_d
					  ON uwp_d.media_item_id = e_d.content_id
					 AND uwp_d.user_id = $1
					 AND uwp_d.profile_id = $2
					 AND uwp_d.completed = TRUE
					WHERE e_d.series_id = ce.series_id
					  AND e_d.season_number > 0
					  AND NOT EXISTS (
						  SELECT 1
						  FROM user_history_hidden_items hhi
						  WHERE hhi.user_id = uwp_d.user_id
						    AND hhi.profile_id = uwp_d.profile_id
						    AND hhi.media_item_id = uwp_d.media_item_id
						    AND uwp_d.updated_at <= hhi.hidden_before
					  )
					ORDER BY uwp_d.updated_at DESC, e_d.season_number DESC, e_d.episode_number DESC
					LIMIT 1
				) done ON true
				WHERE ce.season_number = 0
				  AND e_reg.series_id = ce.series_id
				  AND e_reg.season_number > 0
				LIMIT 1
			) regular ON true
		)`

func buildListNextUpQuery(q NextUpQuery, limit int, cursor *nextUpWalkCursor) (string, []interface{}) {
	args := []interface{}{q.UserID, q.ProfileID, limit}
	argIdx := 4

	seriesFilter := ""
	if q.SeriesID != "" {
		seriesFilter = fmt.Sprintf(" AND e.series_id = $%d", argIdx)
		args = append(args, q.SeriesID)
		argIdx++
	}
	ids := make([]string, len(results))
	byContentID := make(map[string]int, len(results))
	for i := range results {
		ids[i] = results[i].ContentID
		byContentID[results[i].ContentID] = i
	}

	// seedSeen is projected alongside the seed's picked series, which the global
	// walk exposes as `pick`; the series-scoped branch has no walk and no seed.
	cursorFilter := ""
	seedSeen := "ARRAY[pick.series_id]"
	if q.SeriesID == "" && cursor != nil {
		cursorUpdatedAtArg := argIdx
		cursorMediaItemIDArg := argIdx + 1
		cursorSeenArg := argIdx + 2
		cursorFilter = fmt.Sprintf(`
				  AND (uwp.updated_at, uwp.media_item_id) < ($%d, $%d)
				  AND NOT (e.series_id = ANY($%d))`, cursorUpdatedAtArg, cursorMediaItemIDArg, cursorSeenArg)
		seedSeen = fmt.Sprintf("$%d::text[] || pick.series_id", cursorSeenArg)
		args = append(args, cursor.updatedAt, cursor.mediaItemID, cursor.seen)
		argIdx += 3
	}

	// Dropped series are excluded inside every walk step, like hidden rows, so
	// they neither become anchors nor spend the walk's series budget. Without
	// drops the query text is unchanged.
	droppedFilter := ""
	if q.SeriesID == "" && len(q.droppedSeriesIDs) > 0 {
		droppedFilter = fmt.Sprintf(`
					  AND NOT (e.series_id = ANY($%d::text[]))`, argIdx)
		args = append(args, q.droppedSeriesIDs)
		argIdx++
	}

	// Without RankByRelease the release lookup is skipped and a row's
	// activity is its completion time.
	releaseLateral, activityExpr := "", "es.updated_at"
	if q.RankByRelease {
		releaseScope, releaseArgs := nextUpReleaseScope(q, argIdx)
		args = append(args, releaseArgs...)
		releaseLateral = fmt.Sprintf(nextUpReleaseLateral, releaseScope)
		activityExpr = "GREATEST(es.updated_at, released.released_at)"
	}

	// When resumable items are disabled, suppress next-up only if the series has
	// newer in-progress activity than the most recent completed episode. Older
	// partial watches should not block the user's current progression. Once the
	// anchor has moved off a special, a regular episode is measured against the
	// regular completion the anchor moved to instead (any time, before the
	// first one).
	inProgressExclusion := ""
	if !q.EnableResumable {
		inProgressExclusion = `
		,
		eligible_series AS (
			SELECT a.*
			FROM anchors a
			WHERE NOT EXISTS (
				SELECT 1 FROM user_watch_progress uwp_ip
				JOIN episodes e_ip ON e_ip.content_id = uwp_ip.media_item_id
				WHERE uwp_ip.user_id = $1
				  AND uwp_ip.profile_id = $2
				  AND uwp_ip.position_seconds > 0
				  AND e_ip.series_id = a.series_id
				  AND uwp_ip.updated_at > CASE WHEN a.moved AND e_ip.season_number > 0 THEN a.anchored_at ELSE a.updated_at END
			)
		)`
	}

	sourceTable := "anchors"
	if !q.EnableResumable {
		sourceTable = "eligible_series"
	}

	// Global queries use a recursive loose-index scan to find at most
	// nextUpAnchorMaxSeries distinct series without letting one series' rows
	// consume the budget. The compound cursor is the total order required when
	// bulk updates give many rows the same timestamp; because media_item_id is
	// unique it can only order the walk, never decide which episode of a series
	// anchors it — see the anchor lateral below.
	// Series-scoped queries keep the unbounded shape so the anchor is the
	// series' last completed episode regardless of age. Hidden rows and the date
	// cutoff are excluded inside every walk step so they neither become anchors
	// nor advance the cursor.
	var completedEpisodesCTE string
	if q.SeriesID != "" {
		completedEpisodesCTE = fmt.Sprintf(`completed_episodes AS (
			SELECT DISTINCT ON (e.series_id)
				e.series_id,
				e.season_number,
				e.episode_number,
				uwp.updated_at
			FROM user_watch_progress uwp
			JOIN episodes e ON e.content_id = uwp.media_item_id
			WHERE uwp.user_id = $1
			  AND uwp.profile_id = $2
			  AND uwp.completed = TRUE
			  AND NOT EXISTS (
				  SELECT 1
				  FROM user_history_hidden_items hhi
				  WHERE hhi.user_id = uwp.user_id
				    AND hhi.profile_id = uwp.profile_id
				    AND hhi.media_item_id = uwp.media_item_id
				    AND uwp.updated_at <= hhi.hidden_before
			  )
			  %s
			  %s
			ORDER BY e.series_id, uwp.updated_at DESC, e.season_number DESC, e.episode_number DESC
		)`, seriesFilter, dateCutoffFilter)
	} else {
		// Picking the series and picking which of its episodes to anchor on are
		// two different orderings, so they are two different steps.
		//
		// The walk advances on (updated_at, media_item_id), the total order that
		// guarantees forward progress. media_item_id is unique, so no two rows
		// ever tie on that pair and any season/episode clause appended behind it
		// is unreachable. A bulk mark-watched series — every row written with one
		// timestamp — would then anchor on whichever content_id sorts highest,
		// which is an arbitrary season once IDs are not scan-ordered (season 2
		// scanned before a season-1 backfill), and the rail would surface an
		// already-watched episode's successor.
		//
		// So `pick` chooses the series and the cursor position, and the anchor
		// lateral then chooses the episode by (season, episode) among that
		// series' rows sharing pick.updated_at — its newest completed timestamp.
		// Pinning updated_at rather than re-sorting keeps this an index probe,
		// and both rows carry the same updated_at, so cursor order and the
		// reported CompletedAt are unchanged by the split.
		anchorLateral := `JOIN LATERAL (
				SELECT e_a.season_number, e_a.episode_number
				FROM episodes e_a
				JOIN user_watch_progress uwp_a
				  ON uwp_a.media_item_id = e_a.content_id
				 AND uwp_a.user_id = $1
				 AND uwp_a.profile_id = $2
				 AND uwp_a.completed = TRUE
				 AND uwp_a.updated_at = pick.updated_at
				WHERE e_a.series_id = pick.series_id
				  AND NOT EXISTS (
					  SELECT 1
					  FROM user_history_hidden_items hhi
					  WHERE hhi.user_id = uwp_a.user_id
					    AND hhi.profile_id = uwp_a.profile_id
					    AND hhi.media_item_id = uwp_a.media_item_id
					    AND uwp_a.updated_at <= hhi.hidden_before
				  )
				ORDER BY e_a.season_number DESC, e_a.episode_number DESC
				LIMIT 1
			) anchor ON TRUE`

		completedEpisodesCTE = fmt.Sprintf(`RECURSIVE walk AS (
			(
				SELECT
					pick.series_id,
					anchor.season_number,
					anchor.episode_number,
					pick.updated_at,
					pick.media_item_id,
					%s AS seen,
					1 AS n
				FROM (
					SELECT e.series_id, uwp.updated_at, uwp.media_item_id
					FROM user_watch_progress uwp
					JOIN episodes e ON e.content_id = uwp.media_item_id
					WHERE uwp.user_id = $1
					  AND uwp.profile_id = $2
					  AND uwp.completed = TRUE
					  AND NOT EXISTS (
						  SELECT 1
						  FROM user_history_hidden_items hhi
						  WHERE hhi.user_id = uwp.user_id
						    AND hhi.profile_id = uwp.profile_id
						    AND hhi.media_item_id = uwp.media_item_id
						    AND uwp.updated_at <= hhi.hidden_before
					  )
					  %s
					  %s
					ORDER BY uwp.updated_at DESC, uwp.media_item_id DESC
					LIMIT 1
				) pick
				%s
			)
			UNION ALL
			SELECT
				pick.series_id,
				anchor.season_number,
				anchor.episode_number,
				pick.updated_at,
				pick.media_item_id,
				w.seen || pick.series_id,
				w.n + 1
			FROM walk w
			JOIN LATERAL (
				SELECT e.series_id, uwp.updated_at, uwp.media_item_id
				FROM user_watch_progress uwp
				JOIN episodes e ON e.content_id = uwp.media_item_id
				WHERE uwp.user_id = $1
				  AND uwp.profile_id = $2
				  AND uwp.completed = TRUE
				  AND (uwp.updated_at, uwp.media_item_id) < (w.updated_at, w.media_item_id)
				  AND NOT (e.series_id = ANY(w.seen))
				  AND NOT EXISTS (
					  SELECT 1
					  FROM user_history_hidden_items hhi
					  WHERE hhi.user_id = uwp.user_id
					    AND hhi.profile_id = uwp.profile_id
					    AND hhi.media_item_id = uwp.media_item_id
					    AND uwp.updated_at <= hhi.hidden_before
				  )
				  %s
				ORDER BY uwp.updated_at DESC, uwp.media_item_id DESC
				LIMIT 1
			) pick ON true
			%s
			WHERE w.n < %d
		),
		completed_episodes AS (
			SELECT series_id, season_number, episode_number, updated_at
			FROM walk
		)`, seedSeen, cursorFilter, dateCutoffFilter+droppedFilter, anchorLateral,
			dateCutoffFilter+droppedFilter, anchorLateral, nextUpAnchorMaxSeries)
	}

	if q.SeriesID != "" {
		query := fmt.Sprintf(`
		WITH %s%s%s
		SELECT
			next_ep.content_id,
			next_ep.series_id,
			si.title,
			next_ep.season_number,
			next_ep.episode_number,
			es.updated_at,
			%s
		FROM %s es
		JOIN media_items si ON si.content_id = es.series_id
		JOIN LATERAL (
			SELECT e2.content_id, e2.series_id, e2.season_number, e2.episode_number
			FROM episodes e2
			WHERE e2.series_id = es.series_id
			  AND (e2.season_number, e2.episode_number) > (es.season_number, es.episode_number)
			  AND EXISTS (SELECT 1 FROM media_files mf WHERE mf.episode_id = e2.content_id AND mf.missing_since IS NULL)
			  AND NOT EXISTS (
				  SELECT 1 FROM user_watch_progress uwp2
				  WHERE uwp2.user_id = $1
				    AND uwp2.profile_id = $2
				    AND uwp2.media_item_id = e2.content_id
				    AND (uwp2.completed = TRUE OR uwp2.position_seconds > 0)
			  )
			ORDER BY e2.season_number, e2.episode_number
			LIMIT 1
		) next_ep ON true
		%s
		ORDER BY es.updated_at DESC
		LIMIT $3`, completedEpisodesCTE, nextUpAnchorsCTE, inProgressExclusion, activityExpr, sourceTable, releaseLateral)
		return query, args
	}

	resultQuery := fmt.Sprintf(`
		SELECT
			next_ep.content_id,
			next_ep.series_id,
			si.title,
			next_ep.season_number,
			next_ep.episode_number,
			es.updated_at AS completed_at,
			%s AS activity_at
		FROM %s es
		JOIN media_items si ON si.content_id = es.series_id
		JOIN LATERAL (
			SELECT e2.content_id, e2.series_id, e2.season_number, e2.episode_number
			FROM episodes e2
			WHERE e2.series_id = es.series_id
			  AND (e2.season_number, e2.episode_number) > (es.season_number, es.episode_number)
			  AND EXISTS (SELECT 1 FROM media_files mf WHERE mf.episode_id = e2.content_id AND mf.missing_since IS NULL)
			  AND NOT EXISTS (
				  SELECT 1 FROM user_watch_progress uwp2
				  WHERE uwp2.user_id = $1
				    AND uwp2.profile_id = $2
				    AND uwp2.media_item_id = e2.content_id
				    AND (uwp2.completed = TRUE OR uwp2.position_seconds > 0)
			  )
			ORDER BY e2.season_number, e2.episode_number
			LIMIT 1
		) next_ep ON true
		%s
		ORDER BY es.updated_at DESC
		LIMIT $3`, activityExpr, sourceTable, releaseLateral)

	query := fmt.Sprintf(`
		WITH %s%s%s
		,
		frontier AS (
			SELECT w.updated_at, w.media_item_id, w.seen, w.n
			FROM walk w
			ORDER BY w.n DESC
			LIMIT 1
		)
		SELECT
			f.updated_at AS frontier_updated_at,
			f.media_item_id AS frontier_media_item_id,
			f.seen AS frontier_seen,
			f.n AS frontier_n,
			r.content_id,
			r.series_id,
			r.title,
			r.season_number,
			r.episode_number,
			r.completed_at,
			r.activity_at
		FROM frontier f
		LEFT JOIN (
			%s
		) r ON true
		ORDER BY r.completed_at DESC NULLS LAST`, completedEpisodesCTE, nextUpAnchorsCTE, inProgressExclusion, resultQuery)

	return query, args
}

// buildListResumableFirstEpisodesQuery builds the SQL + args for the
// resumable-first-episode lookup. Pulled out of listResumableFirstEpisodes
// so the SeriesID-driven gate-drop is unit-testable without a live database.
//
// When q.SeriesID is set the WHERE clause is scoped to that series and the
// "no completed episodes for this series" exclusion is dropped: the
// show-detail tile is supposed to surface the in-progress episode even when
// the user already finished earlier episodes of the same show. Without this
// gate-drop the endpoint silently falls through to buildListNextUpQuery's
// "next aired" row, which is the bug Codex flagged on PR #64.
//
// When q.SeriesID is empty (global /Shows/NextUp) the gate is preserved —
// series with completed episodes go through the main query, and this branch
// only contributes "started but no episode finished yet" series.
func buildListResumableFirstEpisodesQuery(q NextUpQuery, inProgressIDs []string) (string, []interface{}) {
	args := []interface{}{q.UserID, q.ProfileID, inProgressIDs}
	seriesFilter := ""
	if q.SeriesID != "" {
		args = append(args, q.SeriesID)
		seriesFilter = fmt.Sprintf(" AND e.series_id = $%d", len(args))
	}

	completedSeriesGate := `
		  AND NOT EXISTS (
			  SELECT 1 FROM user_watch_progress uwp_c
			  JOIN episodes e_c ON e_c.content_id = uwp_c.media_item_id
			  WHERE uwp_c.user_id = $1
			    AND uwp_c.profile_id = $2
			    AND uwp_c.completed = TRUE
			    AND e_c.series_id = e.series_id
		  )`
	if q.SeriesID != "" {
		completedSeriesGate = ""
	}
	if q.SeriesID == "" && len(q.droppedSeriesIDs) > 0 {
		args = append(args, q.droppedSeriesIDs)
		seriesFilter = fmt.Sprintf(" AND NOT (e.series_id = ANY($%d::text[]))", len(args))
	}

	query := fmt.Sprintf(`
		SELECT DISTINCT ON (e.series_id)
			e.content_id,
			e.series_id,
			si.title,
			e.season_number,
			e.episode_number,
			uwp.updated_at
		FROM user_watch_progress uwp
		JOIN episodes e ON e.content_id = uwp.media_item_id
		JOIN media_items si ON si.content_id = e.series_id
		WHERE uwp.user_id = $1
		  AND uwp.profile_id = $2
		  AND uwp.position_seconds > 0
		  AND uwp.media_item_id = ANY($3)%s%s
		ORDER BY e.series_id, uwp.updated_at DESC`, seriesFilter, completedSeriesGate)

	return query, args
}

// listResumableFirstEpisodes finds in-progress episodes for the user.
//
// Two callers, two semantics, one query:
//
//   - Global next-up (q.SeriesID == ""): only contribute series the user has
//     never completed any episode of. Series with completed episodes go
//     through buildListNextUpQuery's "next unwatched after the last
//     completed" path; the resumable branch fills the gap for series that
//     don't have a completed-episode anchor yet.
//   - Show-detail tile (q.SeriesID != ""): the in-progress episode wins
//     unconditionally, even when the user has already completed earlier
//     episodes in the same series. The completed-then-mid-watching-the-
//     next-one case is exactly what the show-detail "continue watching"
//     tile is for, so the no-completed-episodes gate would defeat the
//     endpoint's purpose. ListNextUp also flips its dedup priority to let
//     this row win over the completed-next row from the main query.
func (r *NextUpRepository) listResumableFirstEpisodes(ctx context.Context, q NextUpQuery) ([]NextUpResult, error) {
	store, err := r.storeProvider.ForUser(ctx, q.UserID)
	if err != nil {
		return nil, fmt.Errorf("getting user store: %w", err)
	}

	inProgressEntries, err := store.ListProgress(ctx, q.ProfileID, "in_progress", 100, 0)
	if err != nil {
		return nil, fmt.Errorf("listing in-progress: %w", err)
	}
	if len(inProgressEntries) == 0 {
		return nil, nil
	}

	inProgressIDs := make([]string, 0, len(inProgressEntries))
	for _, entry := range inProgressEntries {
		inProgressIDs = append(inProgressIDs, entry.MediaItemID)
	}

	query, args := buildListResumableFirstEpisodesQuery(q, inProgressIDs)

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
