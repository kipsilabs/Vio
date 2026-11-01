package catalog

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestBuildListNextUpQuery_PrefersRecentCompletedOverOlderPartialProgress(t *testing.T) {
	t.Parallel()

// nextUpBackend supplies the user-state backend under test. The catalog is
// always the package's Postgres test database; only progress storage varies.
type nextUpBackend struct {
	name        string
	newProvider func(t *testing.T, pool *pgxpool.Pool) userstore.UserStoreProvider
}

	expectedFragments := []string{
		"eligible_series AS (",
		"uwp_ip.position_seconds > 0",
		"e_ip.series_id = a.series_id",
		"THEN a.anchored_at ELSE a.updated_at END",
		"FROM user_history_hidden_items hhi",
		"uwp.updated_at <= hhi.hidden_before",
		"AND (uwp2.completed = TRUE OR uwp2.position_seconds > 0)",
		"ORDER BY uwp.updated_at DESC, uwp.media_item_id DESC",
		"LIMIT $3",
	}
	for _, fragment := range expectedFragments {
		if !strings.Contains(query, fragment) {
			t.Fatalf("expected query to contain %q, got:\n%s", fragment, query)
		}
	}

	if strings.Contains(query, "uwp.media_item_id = ANY") {
		t.Fatalf("query must not cap completed progress before deriving series anchors, got:\n%s", query)
	}

	if len(args) != 3 {
		t.Fatalf("expected default arg count, got %d", len(args))
	}
}

func TestBuildListNextUpQuery_GlobalBoundsAnchorScan(t *testing.T) {
	t.Parallel()

	q := NextUpQuery{
		UserID:    7,
		ProfileID: "profile-1",
	}
	query, args := buildListNextUpQuery(q, 20, nil)

	expectedFragments := []string{
		"WITH RECURSIVE",
		"(uwp.updated_at, uwp.media_item_id) < (w.updated_at, w.media_item_id)",
		"NOT (e.series_id = ANY(w.seen))",
		fmt.Sprintf("w.n < %d", nextUpAnchorMaxSeries),
		"frontier AS (",
		"LEFT JOIN (",
	}
	for _, fragment := range expectedFragments {
		if !strings.Contains(query, fragment) {
			t.Fatalf("expected global anchor walk to contain %q, got:\n%s", fragment, query)
		}
	}
	// Seed step, recursive step, the anchor lateral attached to each, and the
	// regular-episode lookup that moves a special anchor.
	if got := strings.Count(query, "FROM user_history_hidden_items hhi"); got != 5 {
		t.Fatalf("expected hidden-items filter in every walk step and anchor, got %d occurrences:\n%s", got, query)
	}
	seed := strings.SplitN(query, "UNION ALL", 2)[0]
	if strings.Contains(seed, "(uwp.updated_at, uwp.media_item_id) < ($") {
		t.Fatalf("first batch seed must not contain a continuation cursor, got:\n%s", seed)
	}
	if strings.Contains(seed, "e.series_id = ANY($") {
		t.Fatalf("first batch seed must not contain a seen-series exclusion, got:\n%s", seed)
	}
	if len(args) != 3 {
		t.Fatalf("expected first batch args only, got %v", args)
	}

	cursorAt := time.Now().UTC()
	cursor := &nextUpWalkCursor{
		updatedAt:   cursorAt,
		mediaItemID: "episode-96",
		seen:        []string{"series-1", "series-2"},
	}
	continuedQuery, continuedArgs := buildListNextUpQuery(q, 7, cursor)
	continuedSeed := strings.SplitN(continuedQuery, "UNION ALL", 2)[0]
	continuedFragments := []string{
		"(uwp.updated_at, uwp.media_item_id) < ($4, $5)",
		"NOT (e.series_id = ANY($6))",
		"$6::text[] || pick.series_id AS seen",
		"frontier AS (",
		"LEFT JOIN (",
	}
	for _, fragment := range continuedFragments {
		if !strings.Contains(continuedQuery, fragment) {
			t.Fatalf("expected continued query to contain %q, got:\n%s", fragment, continuedQuery)
		}
	}
	if !strings.Contains(continuedSeed, "(uwp.updated_at, uwp.media_item_id) < ($4, $5)") ||
		!strings.Contains(continuedSeed, "NOT (e.series_id = ANY($6))") {
		t.Fatalf("continued batch seed must contain cursor and seen-series predicates, got:\n%s", continuedSeed)
	}
	if len(continuedArgs) != 6 || continuedArgs[3] != cursorAt || continuedArgs[4] != cursor.mediaItemID {
		t.Fatalf("unexpected continuation args: %v", continuedArgs)
	}
	seenArg, ok := continuedArgs[5].([]string)
	if !ok || len(seenArg) != len(cursor.seen) || seenArg[0] != cursor.seen[0] || seenArg[1] != cursor.seen[1] {
		t.Fatalf("expected cursor seen array verbatim, got %#v", continuedArgs[5])
	}
}

// fixedUserStoreProvider returns one store for every user id. It models the
// per-user SQLite backend, whose progress never appears in Postgres.
type fixedUserStoreProvider struct {
	store userstore.UserStore
}

func (p fixedUserStoreProvider) ForUser(context.Context, int) (userstore.UserStore, error) {
	return p.store, nil
}

func (p fixedUserStoreProvider) Close() error { return nil }

func newNextUpSQLiteProvider(t *testing.T) userstore.UserStoreProvider {
	t.Helper()

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	if err := userdb.InitSchema(db); err != nil {
		t.Fatalf("init sqlite schema: %v", err)
	}
	return fixedUserStoreProvider{store: userdb.NewSQLiteUserStore(db)}
}

func TestBuildListResumableFirstEpisodesQuery_GlobalKeepsCompletedSeriesGate(t *testing.T) {
	t.Parallel()

	// Global /Shows/NextUp?enableResumable=true: the resumable branch must
	// still skip series the user has completed any episode of, otherwise it
	// would double-fire alongside buildListNextUpQuery's main path.
	query, args := buildListResumableFirstEpisodesQuery(NextUpQuery{
		UserID:    7,
		ProfileID: "profile-1",
	}, []string{"ep-1", "ep-2"})

	if !strings.Contains(query, "uwp_c.completed = TRUE") {
		t.Fatalf("global query must keep the completed-series gate, got:\n%s", query)
	}
	return byID
}

// --- Anchor selection ------------------------------------------------------

func TestNextUp_AnchorOrdersByEpisodeNotContentID(t *testing.T) {
	// A bulk mark-watched series writes every row with one timestamp. The anchor
	// must then be the highest (season, episode): production-shape ids can put a
	// season-1 backfill above season 2, and anchoring on content id would
	// surface an already-watched episode's successor.
	runNextUpBackends(t, func(t *testing.T, pool *pgxpool.Pool, provider userstore.UserStoreProvider) {
		f := newNextUpFixture(t, pool, provider)
		seriesID := f.prefix + "-series"
		seedNextUpSeries(t, f.ctx, pool, seriesID, "Ordering")

		// want is s02e04 (highest available episode after the anchor); trap is
		// s01e04, which a content-id-ordered anchor would wrongly pick.
		id := func(base string) string { return f.prefix + "-" + base }
		episodeIDs := []string{
			id("100000000000000001"), id("100000000000000002"),
			id("100000000000000003"), id("100000000000000004"),
			id("900000000000000001"), id("900000000000000002"),
			id("900000000000000003"), id("900000000000000004"),
		}
		seasons := []int{2, 2, 2, 2, 1, 1, 1, 1}
		numbers := []int{1, 2, 3, 4, 1, 2, 3, 4}
		seedNextUpEpisodesSeasoned(t, f.ctx, pool, episodeIDs, repeatString(seriesID, 8), seasons, numbers)
		seedNextUpFiles(t, f.ctx, pool, f.folderID, episodeIDs)

		bulkAt := time.Now().UTC().Truncate(time.Microsecond)
		for _, ep := range episodeIDs[:3:3] {
			f.seedProgress(t, ep, 0, true, bulkAt)
		}
		for _, ep := range episodeIDs[4:7:7] {
			f.seedProgress(t, ep, 0, true, bulkAt)
		}

		results := f.list(t, NextUpQuery{Limit: 20})
		assertNextUpContentIDs(t, results, episodeIDs[3])
	})
}

// --- Starvation / traversal ------------------------------------------------

func TestNextUp_HeavilyWatchedSeriesDoesNotStarveOlderSeries(t *testing.T) {
	runNextUpBackends(t, func(t *testing.T, pool *pgxpool.Pool, provider userstore.UserStoreProvider) {
		f := newNextUpFixture(t, pool, provider)
		floodSeries := f.prefix + "-flood"
		olderSeries := f.prefix + "-older"
		seedNextUpSeries(t, f.ctx, pool, floodSeries, "Flood")
		seedNextUpSeries(t, f.ctx, pool, olderSeries, "Older")

		// >100 newer completions in one series must not consume the budget.
		floodEpisodes := make([]string, 0, 121)
		for i := 1; i <= 121; i++ {
			floodEpisodes = append(floodEpisodes, fmt.Sprintf("%s-e%03d", floodSeries, i))
		}
		seedNextUpEpisodes(t, f.ctx, pool, floodEpisodes, repeatString(floodSeries, 121), sequence(1, 121))
		seedNextUpFiles(t, f.ctx, pool, f.folderID, []string{floodEpisodes[120]})

		olderEpisodes := []string{olderSeries + "-e1", olderSeries + "-e2"}
		seedNextUpEpisodes(t, f.ctx, pool, olderEpisodes, repeatString(olderSeries, 2), []int{1, 2})
		seedNextUpFiles(t, f.ctx, pool, f.folderID, []string{olderEpisodes[1]})

		newest := time.Now().UTC().Truncate(time.Microsecond)
		for i := 0; i < 120; i++ {
			f.seedProgress(t, floodEpisodes[i], 0, true, newest.Add(-time.Duration(i)*time.Second))
		}
		f.seedProgress(t, olderEpisodes[0], 0, true, newest.Add(-time.Hour))

		results := f.list(t, NextUpQuery{Limit: 5})
		assertNextUpContentIDs(t, results, floodEpisodes[120], olderEpisodes[1])
	})
}

func TestNextUp_WalksPastIneligibleSeries(t *testing.T) {
	runNextUpBackends(t, func(t *testing.T, pool *pgxpool.Pool, provider userstore.UserStoreProvider) {
		f := newNextUpFixture(t, pool, provider)

		const ineligibleCount = 100
		seriesIDs := make([]string, 0, ineligibleCount+2)
		episodeIDs := make([]string, 0, ineligibleCount+2)
		for i := 0; i < ineligibleCount; i++ {
			seriesID := fmt.Sprintf("%s-ineligible-%03d", f.prefix, i)
			seriesIDs = append(seriesIDs, seriesID)
			episodeIDs = append(episodeIDs, seriesID+"-e1")
		}
		seedNextUpSeriesBatch(t, f.ctx, pool, seriesIDs, "Ineligible")
		seedNextUpEpisodes(t, f.ctx, pool, episodeIDs, seriesIDs, ones(ineligibleCount))

		olderA := f.prefix + "-older-a"
		olderB := f.prefix + "-older-b"
		seedNextUpSeries(t, f.ctx, pool, olderA, "Older A")
		seedNextUpSeries(t, f.ctx, pool, olderB, "Older B")
		olderEpisodes := []string{olderA + "-e1", olderA + "-e2", olderB + "-e1", olderB + "-e2"}
		seedNextUpEpisodes(t, f.ctx, pool, olderEpisodes, []string{olderA, olderA, olderB, olderB}, []int{1, 2, 1, 2})
		seedNextUpFiles(t, f.ctx, pool, f.folderID, []string{olderEpisodes[1], olderEpisodes[3]})

		newest := time.Now().UTC().Truncate(time.Microsecond)
		for i, id := range episodeIDs {
			f.seedProgress(t, id, 0, true, newest.Add(-time.Duration(i)*time.Minute))
		}
		f.seedProgress(t, olderEpisodes[0], 0, true, newest.Add(-(ineligibleCount+1)*time.Minute))
		f.seedProgress(t, olderEpisodes[2], 0, true, newest.Add(-(ineligibleCount+2)*time.Minute))

		results := f.list(t, NextUpQuery{Limit: 5})
		assertNextUpContentIDs(t, results, olderEpisodes[1], olderEpisodes[3])
	})
}

func TestNextUp_NoStarvationBeyondAnchorSeriesCap(t *testing.T) {
	// The removed 960-series anchor ceiling let an older eligible series starve
	// behind a wall of ineligible ones. Walk past 960 distinct series.
	runNextUpBackends(t, func(t *testing.T, pool *pgxpool.Pool, provider userstore.UserStoreProvider) {
		f := newNextUpFixture(t, pool, provider)

		const ineligibleCount = 970
		seriesIDs := make([]string, 0, ineligibleCount+1)
		episodeIDs := make([]string, 0, ineligibleCount+1)
		for i := 0; i < ineligibleCount; i++ {
			seriesID := fmt.Sprintf("%s-ineligible-%04d", f.prefix, i)
			seriesIDs = append(seriesIDs, seriesID)
			episodeIDs = append(episodeIDs, seriesID+"-e1")
		}
		seedNextUpSeriesBatch(t, f.ctx, pool, seriesIDs, "Ineligible")
		seedNextUpEpisodes(t, f.ctx, pool, episodeIDs, seriesIDs, ones(ineligibleCount))

		olderSeries := f.prefix + "-older"
		seedNextUpSeries(t, f.ctx, pool, olderSeries, "Older")
		olderEpisodes := []string{olderSeries + "-e1", olderSeries + "-e2"}
		seedNextUpEpisodes(t, f.ctx, pool, olderEpisodes, []string{olderSeries, olderSeries}, []int{1, 2})
		seedNextUpFiles(t, f.ctx, pool, f.folderID, []string{olderEpisodes[1]})

		newest := time.Now().UTC().Truncate(time.Microsecond)
		for i, id := range episodeIDs {
			f.seedProgress(t, id, 0, true, newest.Add(-time.Duration(i)*time.Second))
		}
		f.seedProgress(t, olderEpisodes[0], 0, true, newest.Add(-time.Duration(ineligibleCount+1)*time.Second))

		results := f.list(t, NextUpQuery{Limit: 5})
		assertNextUpContentIDs(t, results, olderEpisodes[1])
	})
}

func TestNextUp_ExhaustedHistoryReturnsNothing(t *testing.T) {
	runNextUpBackends(t, func(t *testing.T, pool *pgxpool.Pool, provider userstore.UserStoreProvider) {
		f := newNextUpFixture(t, pool, provider)
		seriesIDs := []string{f.prefix + "-a", f.prefix + "-b", f.prefix + "-c"}
		seedNextUpSeriesBatch(t, f.ctx, pool, seriesIDs, "Finished")
		episodeIDs := []string{seriesIDs[0] + "-e1", seriesIDs[1] + "-e1", seriesIDs[2] + "-e1"}
		seedNextUpEpisodes(t, f.ctx, pool, episodeIDs, seriesIDs, []int{1, 1, 1})

		at := time.Now().UTC().Truncate(time.Microsecond)
		for i, id := range episodeIDs {
			f.seedProgress(t, id, 0, true, at.Add(-time.Duration(i)*time.Minute))
		}

		results := f.list(t, NextUpQuery{Limit: 20})
		if len(results) != 0 {
			t.Fatalf("ListNextUp returned %d rows for finished history: %+v", len(results), results)
		}
	})
}

// --- Ties and pagination ---------------------------------------------------

func TestNextUp_SameTimestampSeriesStayOrdered(t *testing.T) {
	runNextUpBackends(t, func(t *testing.T, pool *pgxpool.Pool, provider userstore.UserStoreProvider) {
		f := newNextUpFixture(t, pool, provider)
		seriesA := f.prefix + "-series-a"
		seriesB := f.prefix + "-series-b"
		seedNextUpSeries(t, f.ctx, pool, seriesA, "Tie A")
		seedNextUpSeries(t, f.ctx, pool, seriesB, "Tie B")
		seedNextUpEpisodes(t, f.ctx, pool,
			[]string{seriesA + "-e1", seriesA + "-e2", seriesB + "-e1", seriesB + "-e2"},
			[]string{seriesA, seriesA, seriesB, seriesB},
			[]int{1, 2, 1, 2},
		)
		seedNextUpFiles(t, f.ctx, pool, f.folderID, []string{seriesA + "-e2", seriesB + "-e2"})

		tiedAt := time.Now().UTC().Truncate(time.Microsecond)
		f.seedProgress(t, seriesA+"-e1", 0, true, tiedAt)
		f.seedProgress(t, seriesB+"-e1", 0, true, tiedAt)

		q := NextUpQuery{Limit: 20}
		results := f.list(t, q)
		assertNextUpOrderedIDs(t, results, seriesA+"-e2", seriesB+"-e2")

		for i := 0; i < 5; i++ {
			repeat := f.list(t, q)
			assertNextUpOrderedIDs(t, repeat, seriesA+"-e2", seriesB+"-e2")
		}
	})
}

func TestNextUp_TiesSpanningPagesStayDeterministic(t *testing.T) {
	runNextUpBackends(t, func(t *testing.T, pool *pgxpool.Pool, provider userstore.UserStoreProvider) {
		f := newNextUpFixture(t, pool, provider)
		// Episode ids sort c > b > a, so with a tiny page size the smallest
		// series (a) lands on a later page than the tie window. The final
		// (updated_at DESC, series_id) order must still put a first.
		seriesIDs := []string{f.prefix + "-a", f.prefix + "-b", f.prefix + "-c"}
		seedNextUpSeriesBatch(t, f.ctx, pool, seriesIDs, "Tie")
		episodeIDs := []string{seriesIDs[0] + "-e1", seriesIDs[1] + "-e1", seriesIDs[2] + "-e1"}
		successors := []string{seriesIDs[0] + "-e2", seriesIDs[1] + "-e2", seriesIDs[2] + "-e2"}
		seedNextUpEpisodes(t, f.ctx, pool,
			append(append([]string{}, episodeIDs...), successors...),
			[]string{seriesIDs[0], seriesIDs[1], seriesIDs[2], seriesIDs[0], seriesIDs[1], seriesIDs[2]},
			[]int{1, 1, 1, 2, 2, 2},
		)
		seedNextUpFiles(t, f.ctx, pool, f.folderID, successors)

		tiedAt := time.Now().UTC().Truncate(time.Microsecond)
		for _, id := range episodeIDs {
			f.seedProgress(t, id, 0, true, tiedAt)
		}

		repo := f.repo(t)
		repo.statePageSize = 2
		results, err := repo.ListNextUp(f.ctx, NextUpQuery{UserID: f.userID, ProfileID: f.profileID, Limit: 2})
		if err != nil {
			t.Fatalf("ListNextUp: %v", err)
		}
		assertNextUpOrderedIDs(t, results, seriesIDs[0]+"-e2", seriesIDs[1]+"-e2")
	})
}

// --- Filters ---------------------------------------------------------------

func TestNextUp_GlobalDateCutoffOnSecondPage(t *testing.T) {
	runNextUpBackends(t, func(t *testing.T, pool *pgxpool.Pool, provider userstore.UserStoreProvider) {
		f := newNextUpFixture(t, pool, provider)
		seriesIDs := []string{f.prefix + "-a", f.prefix + "-b", f.prefix + "-c"}
		seedNextUpSeriesBatch(t, f.ctx, pool, seriesIDs, "Cutoff")
		episodeIDs := []string{seriesIDs[0] + "-e1", seriesIDs[1] + "-e1", seriesIDs[2] + "-e1"}
		successors := []string{seriesIDs[0] + "-e2", seriesIDs[1] + "-e2", seriesIDs[2] + "-e2"}
		seedNextUpEpisodes(t, f.ctx, pool,
			append(append([]string{}, episodeIDs...), successors...),
			[]string{seriesIDs[0], seriesIDs[1], seriesIDs[2], seriesIDs[0], seriesIDs[1], seriesIDs[2]},
			[]int{1, 1, 1, 2, 2, 2},
		)
		seedNextUpFiles(t, f.ctx, pool, f.folderID, successors)

		recent := time.Now().UTC().Truncate(time.Microsecond)
		old := recent.Add(-2 * time.Hour)
		// b and c are recent; a is older than the cutoff and must be dropped.
		f.seedProgress(t, seriesIDs[1]+"-e1", 0, true, recent)
		f.seedProgress(t, seriesIDs[2]+"-e1", 0, true, recent)
		f.seedProgress(t, seriesIDs[0]+"-e1", 0, true, old)

		cutoff := recent.Add(-30 * time.Minute)
		repo := f.repo(t)
		repo.statePageSize = 2
		results, err := repo.ListNextUp(f.ctx, NextUpQuery{
			UserID: f.userID, ProfileID: f.profileID, Limit: 5, DateCutoff: &cutoff,
		})
		if err != nil {
			t.Fatalf("ListNextUp: %v", err)
		}
		assertNextUpContentIDs(t, results, seriesIDs[1]+"-e2", seriesIDs[2]+"-e2")
	})
}

func TestNextUp_SeriesScopedFindsOldestAnchor(t *testing.T) {
	runNextUpBackends(t, func(t *testing.T, pool *pgxpool.Pool, provider userstore.UserStoreProvider) {
		f := newNextUpFixture(t, pool, provider)

		// A wall of newer unrelated completions must not hide the target
		// series' long-idle anchor.
		const noise = 40
		noiseSeries := make([]string, 0, noise)
		noiseEpisodes := make([]string, 0, noise)
		for i := 0; i < noise; i++ {
			seriesID := fmt.Sprintf("%s-noise-%03d", f.prefix, i)
			noiseSeries = append(noiseSeries, seriesID)
			noiseEpisodes = append(noiseEpisodes, seriesID+"-e1")
		}
		seedNextUpSeriesBatch(t, f.ctx, pool, noiseSeries, "Noise")
		seedNextUpEpisodes(t, f.ctx, pool, noiseEpisodes, noiseSeries, ones(noise))

		targetSeries := f.prefix + "-target"
		seedNextUpSeries(t, f.ctx, pool, targetSeries, "Target")
		targetEpisodes := []string{targetSeries + "-e1", targetSeries + "-e2"}
		seedNextUpEpisodes(t, f.ctx, pool, targetEpisodes, repeatString(targetSeries, 2), []int{1, 2})
		seedNextUpFiles(t, f.ctx, pool, f.folderID, []string{targetEpisodes[1]})

		newest := time.Now().UTC().Truncate(time.Microsecond)
		for i, id := range noiseEpisodes {
			f.seedProgress(t, id, 0, true, newest.Add(-time.Duration(i)*time.Minute))
		}
		f.seedProgress(t, targetEpisodes[0], 0, true, newest.Add(-24*time.Hour))

		repo := f.repo(t)
		repo.statePageSize = 5
		results, err := repo.ListNextUp(f.ctx, NextUpQuery{
			UserID: f.userID, ProfileID: f.profileID, SeriesID: targetSeries, Limit: 20,
		})
		if err != nil {
			t.Fatalf("ListNextUp: %v", err)
		}
		assertNextUpContentIDs(t, results, targetEpisodes[1])
	})
}

func TestNextUp_SeriesAndDateFilters(t *testing.T) {
	runNextUpBackends(t, func(t *testing.T, pool *pgxpool.Pool, provider userstore.UserStoreProvider) {
		f := newNextUpFixture(t, pool, provider)
		seriesA := f.prefix + "-a"
		seriesB := f.prefix + "-b"
		seedNextUpSeries(t, f.ctx, pool, seriesA, "A")
		seedNextUpSeries(t, f.ctx, pool, seriesB, "B")
		seedNextUpEpisodes(t, f.ctx, pool,
			[]string{seriesA + "-e1", seriesA + "-e2", seriesB + "-e1", seriesB + "-e2"},
			[]string{seriesA, seriesA, seriesB, seriesB},
			[]int{1, 2, 1, 2},
		)
		seedNextUpFiles(t, f.ctx, pool, f.folderID, []string{seriesA + "-e2", seriesB + "-e2"})

		older := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Microsecond)
		newer := older.Add(time.Hour)
		f.seedProgress(t, seriesA+"-e1", 0, true, older)
		f.seedProgress(t, seriesB+"-e1", 0, true, newer)

		results := f.list(t, NextUpQuery{SeriesID: seriesA, Limit: 20})
		assertNextUpContentIDs(t, results, seriesA+"-e2")

		cutoff := older.Add(30 * time.Minute)
		results = f.list(t, NextUpQuery{DateCutoff: &cutoff, Limit: 20})
		assertNextUpContentIDs(t, results, seriesB+"-e2")

		results = f.list(t, NextUpQuery{SeriesID: seriesA, DateCutoff: &cutoff, Limit: 20})
		if len(results) != 0 {
			t.Fatalf("series+date should exclude the older series, got %+v", results)
		}
	})
}

// --- Hidden history --------------------------------------------------------

func TestNextUp_HiddenHistoryWatermarkAndReappearance(t *testing.T) {
	runNextUpBackends(t, func(t *testing.T, pool *pgxpool.Pool, provider userstore.UserStoreProvider) {
		f := newNextUpFixture(t, pool, provider)
		seriesID := f.prefix + "-series"
		seedNextUpSeries(t, f.ctx, pool, seriesID, "Hidden")
		e1, e2, e3 := seriesID+"-e1", seriesID+"-e2", seriesID+"-e3"
		seedNextUpEpisodes(t, f.ctx, pool, []string{e1, e2, e3}, repeatString(seriesID, 3), []int{1, 2, 3})
		seedNextUpFiles(t, f.ctx, pool, f.folderID, []string{e2, e3})

		older := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
		newer := older.Add(time.Hour)
		f.seedProgress(t, e1, 0, true, older)
		f.seedProgress(t, e2, 0, true, newer)

		assertNextUpContentIDs(t, f.list(t, NextUpQuery{Limit: 20}), e3)

		// Hiding the newest completion deletes it and records the watermark; the
		// older completion anchors the series, so the hidden episode returns.
		hiddenBefore := time.Now().UTC().Add(time.Minute).Truncate(time.Microsecond)
		if err := f.store(t).RemoveHistoryItems(f.ctx, f.profileID, []string{e2}, hiddenBefore); err != nil {
			t.Fatalf("hide progress: %v", err)
		}
		assertNextUpContentIDs(t, f.list(t, NextUpQuery{Limit: 20}), e2)

		// A completion at the watermark is suppressed (inclusive comparison).
		if err := f.store(t).SetProgressAt(f.ctx, f.profileID, e2, 0, 1800, true, hiddenBefore); err != nil {
			t.Fatalf("completion at watermark: %v", err)
		}
		assertNextUpContentIDs(t, f.list(t, NextUpQuery{Limit: 20}), e2)

		// A completion after the watermark restores the newest anchor.
		if err := f.store(t).SetProgressAt(f.ctx, f.profileID, e2, 0, 1800, true, hiddenBefore.Add(time.Second)); err != nil {
			t.Fatalf("completion after watermark: %v", err)
		}
		assertNextUpContentIDs(t, f.list(t, NextUpQuery{Limit: 20}), e3)
	})
}

// --- Profile isolation -----------------------------------------------------

func TestNextUp_SiblingProfilesAreIsolated(t *testing.T) {
	runNextUpBackends(t, func(t *testing.T, pool *pgxpool.Pool, provider userstore.UserStoreProvider) {
		f := newNextUpFixture(t, pool, provider)
		profileA := f.prefix + "-profile-a"
		profileB := f.prefix + "-profile-b"
		seedNextUpProfile(t, f.ctx, pool, f.userID, profileA)
		seedNextUpProfile(t, f.ctx, pool, f.userID, profileB)

		seriesID := f.prefix + "-series"
		seedNextUpSeries(t, f.ctx, pool, seriesID, "Isolation")
		e1, e2 := seriesID+"-e1", seriesID+"-e2"
		seedNextUpEpisodes(t, f.ctx, pool, []string{e1, e2}, repeatString(seriesID, 2), []int{1, 2})
		seedNextUpFiles(t, f.ctx, pool, f.folderID, []string{e2})

		f.seedProgressFor(t, profileA, e1, 0, true, time.Now().UTC().Truncate(time.Microsecond))

		assertNextUpContentIDs(t, f.list(t, NextUpQuery{ProfileID: profileA, Limit: 20}), e2)
		if results := f.list(t, NextUpQuery{ProfileID: profileB, Limit: 20}); len(results) != 0 {
			t.Fatalf("sibling profile inherited progress: %+v", results)
		}
	})
}

// --- Resumable merge -------------------------------------------------------

func TestNextUp_EmptyHistoryResumableEnabled(t *testing.T) {
	runNextUpBackends(t, func(t *testing.T, pool *pgxpool.Pool, provider userstore.UserStoreProvider) {
		f := newNextUpFixture(t, pool, provider)
		results := f.list(t, NextUpQuery{EnableResumable: true, Limit: 20})
		if len(results) != 0 {
			t.Fatalf("empty history must return no anchors, got %+v", results)
		}
	})
}

func TestNextUp_ResumableMergePriority(t *testing.T) {
	runNextUpBackends(t, func(t *testing.T, pool *pgxpool.Pool, provider userstore.UserStoreProvider) {
		f := newNextUpFixture(t, pool, provider)
		seriesInProgress := f.prefix + "-in-progress"
		seriesCompleted := f.prefix + "-completed"
		seedNextUpSeries(t, f.ctx, pool, seriesInProgress, "In Progress")
		seedNextUpSeries(t, f.ctx, pool, seriesCompleted, "Completed")
		seedNextUpEpisodes(t, f.ctx, pool,
			[]string{seriesInProgress + "-e1", seriesCompleted + "-e1", seriesCompleted + "-e2"},
			[]string{seriesInProgress, seriesCompleted, seriesCompleted},
			[]int{1, 1, 2},
		)
		seedNextUpFiles(t, f.ctx, pool, f.folderID, []string{seriesCompleted + "-e2"})

		startedAt := time.Now().UTC().Truncate(time.Microsecond)
		f.seedProgress(t, seriesInProgress+"-e1", 30, false, startedAt)
		f.seedProgress(t, seriesCompleted+"-e1", 0, true, startedAt.Add(-time.Minute))

		results := f.list(t, NextUpQuery{EnableResumable: true, Limit: 20})
		assertNextUpContentIDs(t, results, seriesInProgress+"-e1", seriesCompleted+"-e2")

		byID := nextUpResultByContentID(results)
		if !byID[seriesInProgress+"-e1"].IsResumable {
			t.Fatalf("in-progress episode must be marked resumable: %+v", byID[seriesInProgress+"-e1"])
		}
		if byID[seriesCompleted+"-e2"].IsResumable {
			t.Fatalf("completed-next episode must not be marked resumable: %+v", byID[seriesCompleted+"-e2"])
		}
	})
}

// --- Exact post-anchor state (out-of-order watch) --------------------------

func TestNextUp_OutOfOrderWatchExcludesPostAnchorEpisode(t *testing.T) {
	// A returned series' successor must exclude every post-anchor episode with
	// any state, even when that state row is older than the page walk's stop
	// threshold. Watch e4 first, then e2: e2 is the newest anchor, but e4's
	// state row sits below the stop and the prefix walk never reaches it. e3 is
	// unavailable, so trusting the prefix picks e4; exact state picks e5.
	runNextUpBackends(t, func(t *testing.T, pool *pgxpool.Pool, provider userstore.UserStoreProvider) {
		f := newNextUpFixture(t, pool, provider)
		seriesID := f.prefix + "-series"
		seedNextUpSeries(t, f.ctx, pool, seriesID, "Out Of Order")

		e1, e2, e3, e4, e5 := seriesID+"-e1", seriesID+"-e2", seriesID+"-e3", seriesID+"-e4", seriesID+"-e5"
		seedNextUpEpisodes(t, f.ctx, pool, []string{e1, e2, e3, e4, e5}, repeatString(seriesID, 5), []int{1, 2, 3, 4, 5})
		seedNextUpFiles(t, f.ctx, pool, f.folderID, []string{e4, e5})

		otherA := f.prefix + "-other-a"
		otherB := f.prefix + "-other-b"
		seedNextUpSeries(t, f.ctx, pool, otherA, "Other A")
		seedNextUpSeries(t, f.ctx, pool, otherB, "Other B")
		seedNextUpEpisodes(t, f.ctx, pool,
			[]string{otherA + "-e1", otherA + "-e2", otherB + "-e1", otherB + "-e2"},
			[]string{otherA, otherA, otherB, otherB}, []int{1, 2, 1, 2})
		seedNextUpFiles(t, f.ctx, pool, f.folderID, []string{otherA + "-e2", otherB + "-e2"})

		newest := time.Now().UTC().Truncate(time.Microsecond)
		// Page order (updated_at DESC) with a one-entry page: e2, otherA,
		// otherB, e4. The walk stops at otherA's page, before e4.
		f.seedProgress(t, e2, 0, true, newest)
		f.seedProgress(t, otherA+"-e1", 0, true, newest.Add(-time.Minute))
		f.seedProgress(t, otherB+"-e1", 0, true, newest.Add(-2*time.Minute))
		f.seedProgress(t, e4, 0, true, newest.Add(-3*time.Minute))

		repo := f.repo(t)
		repo.statePageSize = 1
		results, err := repo.ListNextUp(f.ctx, NextUpQuery{UserID: f.userID, ProfileID: f.profileID, Limit: 1})
		if err != nil {
			t.Fatalf("ListNextUp: %v", err)
		}
		assertNextUpOrderedIDs(t, results, e5)
	})
}

// --- Resumable gate uses exact completions ---------------------------------

func TestNextUp_ResumableGateSeesCompletionBelowStop(t *testing.T) {
	// A series with an old completion plus live in-progress must not surface as
	// resumable just because the early-stopped walk never saw its completion.
	// The gate has to read exact state; a genuinely completion-free series with
	// in-progress activity must still surface.
	runNextUpBackends(t, func(t *testing.T, pool *pgxpool.Pool, provider userstore.UserStoreProvider) {
		f := newNextUpFixture(t, pool, provider)

		seriesP1 := f.prefix + "-p1"
		seriesP2 := f.prefix + "-p2"
		seedNextUpSeries(t, f.ctx, pool, seriesP1, "P1")
		seedNextUpSeries(t, f.ctx, pool, seriesP2, "P2")
		seedNextUpEpisodes(t, f.ctx, pool,
			[]string{seriesP1 + "-e1", seriesP1 + "-e2", seriesP2 + "-e1", seriesP2 + "-e2"},
			[]string{seriesP1, seriesP1, seriesP2, seriesP2}, []int{1, 2, 1, 2})
		seedNextUpFiles(t, f.ctx, pool, f.folderID, []string{seriesP1 + "-e2", seriesP2 + "-e2"})

		// Gated series: e1 completed long ago, e2 in progress now.
		gated := f.prefix + "-gated"
		seedNextUpSeries(t, f.ctx, pool, gated, "Gated")
		seedNextUpEpisodes(t, f.ctx, pool, []string{gated + "-e1", gated + "-e2"}, repeatString(gated, 2), []int{1, 2})

		// Genuinely resumable series: in progress, no completion at all.
		openSeries := f.prefix + "-open"
		seedNextUpSeries(t, f.ctx, pool, openSeries, "Open")
		seedNextUpEpisodes(t, f.ctx, pool, []string{openSeries + "-e1"}, []string{openSeries}, []int{1})

		newest := time.Now().UTC().Truncate(time.Microsecond)
		f.seedProgress(t, gated+"-e2", 30, false, newest)
		f.seedProgress(t, seriesP1+"-e1", 0, true, newest.Add(-time.Minute))
		f.seedProgress(t, seriesP2+"-e1", 0, true, newest.Add(-2*time.Minute))
		f.seedProgress(t, openSeries+"-e1", 20, false, newest.Add(-3*time.Minute))
		// The gated series' only completion is below the stop threshold.
		f.seedProgress(t, gated+"-e1", 0, true, newest.Add(-4*time.Minute))

		repo := f.repo(t)
		repo.statePageSize = 1
		resumable, err := repo.ListNextUp(f.ctx, NextUpQuery{
			UserID: f.userID, ProfileID: f.profileID, Limit: 1, EnableResumable: true,
		})
		if err != nil {
			t.Fatalf("ListNextUp(resumable): %v", err)
		}
		byContent := nextUpResultByContentID(resumable)
		if _, ok := byContent[gated+"-e2"]; ok {
			t.Fatalf("gated series surfaced as resumable despite a completion: %+v", resumable)
		}
		if _, ok := byContent[openSeries+"-e1"]; !ok {
			t.Fatalf("completion-free in-progress series missing: %+v", resumable)
		}

		// With resumable disabled the in-progress episode must not surface.
		completedOnly, err := repo.ListNextUp(f.ctx, NextUpQuery{
			UserID: f.userID, ProfileID: f.profileID, Limit: 1,
		})
		if err != nil {
			t.Fatalf("ListNextUp(completed): %v", err)
		}
		if _, ok := nextUpResultByContentID(completedOnly)[gated+"-e2"]; ok {
			t.Fatalf("in-progress surfaced with resumable disabled: %+v", completedOnly)
		}
	})
}

// --- Limits and ordering ---------------------------------------------------

func TestNextUp_MultiSeriesLimit(t *testing.T) {
	runNextUpBackends(t, func(t *testing.T, pool *pgxpool.Pool, provider userstore.UserStoreProvider) {
		f := newNextUpFixture(t, pool, provider)
		seriesIDs := []string{f.prefix + "-a", f.prefix + "-b", f.prefix + "-c"}
		seedNextUpSeriesBatch(t, f.ctx, pool, seriesIDs, "Limit")
		episodeIDs := make([]string, 0, len(seriesIDs)*2)
		episodeSeries := make([]string, 0, len(seriesIDs)*2)
		episodeNumbers := make([]int, 0, len(seriesIDs)*2)
		available := make([]string, 0, len(seriesIDs))
		for _, seriesID := range seriesIDs {
			episodeIDs = append(episodeIDs, seriesID+"-e1", seriesID+"-e2")
			episodeSeries = append(episodeSeries, seriesID, seriesID)
			episodeNumbers = append(episodeNumbers, 1, 2)
			available = append(available, seriesID+"-e2")
		}
		seedNextUpEpisodes(t, f.ctx, pool, episodeIDs, episodeSeries, episodeNumbers)
		seedNextUpFiles(t, f.ctx, pool, f.folderID, available)

		newest := time.Now().UTC().Truncate(time.Microsecond)
		for i, seriesID := range seriesIDs {
			f.seedProgress(t, seriesID+"-e1", 0, true, newest.Add(-time.Duration(i)*time.Minute))
		}

		results := f.list(t, NextUpQuery{Limit: 2})
		assertNextUpOrderedIDs(t, results, seriesIDs[0]+"-e2", seriesIDs[1]+"-e2")
	})
}

// --- Helpers for bulk fixtures --------------------------------------------

func ones(count int) []int {
	values := make([]int, count)
	for i := range values {
		values[i] = 1
	}
	return values
}

func sequence(start, count int) []int {
	values := make([]int, count)
	for i := range values {
		values[i] = start + i
	}
	return values
}

// --- Build-shape guard -----------------------------------------------------

// TestNextUpRepositoryHasNoPostgresUserStateSQL pins the refactor's core
// invariant: the catalog package must not carry Next Up SQL that joins the
// Postgres user-state tables. It scans the package sources so a reintroduced
// join fails the build even if no behavioral test exercises it.
func TestNextUpRepositoryHasNoPostgresUserStateSQL(t *testing.T) {
	raw, err := os.ReadFile("nextup_repo.go")
	if err != nil {
		t.Fatalf("read nextup_repo.go: %v", err)
	}
	source := string(raw)
	for _, forbidden := range []string{"user_watch_progress", "user_history_hidden_items", "nextUpAnchorMax", "buildListNextUpQuery", "buildListNextUpSnapshotQuery"} {
		if strings.Contains(source, forbidden) {
			t.Fatalf("nextup_repo.go still references %q", forbidden)
		}
	}
}

func TestDroppedSeriesSet(t *testing.T) {
	t.Parallel()

	dropped := []string{"series-dropped", "", "series-other"}
	set := droppedSeriesSet(NextUpQuery{UserID: 7, ProfileID: "profile-1", droppedSeriesIDs: dropped})
	if len(set) != 2 {
		t.Fatalf("set = %v, want 2 entries (blank IDs skipped)", set)
	}
	for _, id := range []string{"series-dropped", "series-other"} {
		if _, ok := set[id]; !ok {
			t.Fatalf("set = %v, missing %q", set, id)
		}
	}

	if got := droppedSeriesSet(NextUpQuery{UserID: 7, ProfileID: "profile-1"}); got != nil {
		t.Fatalf("no drops must yield nil, got %v", got)
	}
	// Series-scoped lookups keep their own series: the exclusion applies to
	// global lookups only.
	scoped := droppedSeriesSet(NextUpQuery{UserID: 7, ProfileID: "profile-1", SeriesID: "series-dropped", droppedSeriesIDs: dropped})
	if scoped != nil {
		t.Fatalf("series-scoped query must not exclude drops, got %v", scoped)
	}
}

func TestSortNextUpByActivity_BreaksTiesByCompletionThenSeries(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	results := []NextUpResult{
		{SeriesID: "c", ActivityAt: at, CompletedAt: at.Add(-2 * time.Hour)},
		{SeriesID: "b", ActivityAt: at, CompletedAt: at.Add(-time.Hour)},
		{SeriesID: "z", ActivityAt: at.Add(-time.Minute), CompletedAt: at.Add(-time.Minute)},
		{SeriesID: "a", ActivityAt: at, CompletedAt: at.Add(-2 * time.Hour)},
		{SeriesID: "y", ActivityAt: at.Add(time.Minute), CompletedAt: at.Add(-48 * time.Hour)},
	}
	sortNextUpByActivity(results)
	got := make([]string, len(results))
	for i, res := range results {
		got[i] = res.SeriesID
	}
	if want := "y b a c z"; strings.Join(got, " ") != want {
		t.Fatalf("order = %q, want %q", strings.Join(got, " "), want)
	}
}

func TestNextUpReleaseScope(t *testing.T) {
	t.Parallel()

	five := 5
	cases := []struct {
		name     string
		q        NextUpQuery
		wantCond string
		wantArgs []interface{}
	}{
		{"unrestricted", NextUpQuery{}, "", nil},
		{"disabled", NextUpQuery{AccessFilter: AccessFilter{DisabledLibraryIDs: []int{3}}},
			" AND NOT (ea.library_id = ANY($9::int[]))", []interface{}{[]int{3}}},
		{"allowed", NextUpQuery{AccessFilter: AccessFilter{AllowedLibraryIDs: []int{1, 2}, DisabledLibraryIDs: []int{2}}},
			" AND ea.library_id = ANY($9::int[])", []interface{}{[]int{1}}},
		{"section library", NextUpQuery{LibraryID: &five}, " AND ea.library_id = ANY($9::int[])", []interface{}{[]int{5}}},
		{"no reachable library", NextUpQuery{LibraryIDs: []int{4}, AccessFilter: AccessFilter{AllowedLibraryIDs: []int{1}}},
			" AND ea.library_id = ANY($9::int[])", []interface{}{[]int{}}},
	}
	for _, tc := range cases {
		cond, args := nextUpReleaseScope(tc.q, 9)
		if cond != tc.wantCond || fmt.Sprint(args) != fmt.Sprint(tc.wantArgs) {
			t.Errorf("%s: got %q %v, want %q %v", tc.name, cond, args, tc.wantCond, tc.wantArgs)
		}
	}
}
