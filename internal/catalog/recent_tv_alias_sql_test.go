package catalog

import (
	"strings"
	"testing"
	"time"
)

// TestRecentTVConditionsUseCanonicalIdentity pins the availability row scope to
// the canonical episode: a membership row resolves through its alias mapping,
// and a file owned by either the canonical row or an alias row keeps it
// available.
func TestRecentTVConditionsUseCanonicalIdentity(t *testing.T) {
	c := buildRecentTVConditions(RecentTVQuery{LibraryIDs: []int{2}, Limit: 5})
	for _, want := range []string{
		"el.media_folder_id = ANY($1)",
		recentTVCanonicalEpisodeIDSQL,
		"ea.alias_episode_id IS NOT NULL",
		"mf.media_folder_id = el.media_folder_id",
		"mf.missing_since IS NULL",
	} {
		if !strings.Contains(c.episodeRows, want) {
			t.Fatalf("episode conditions missing %q:\n%s", want, c.episodeRows)
		}
	}
}

// TestRecentTVArrivalRowsCanonicalizeAndDeduplicate covers the narrow arrival
// rows every query shape groups from: an alias membership resolves to its
// canonical episode, and canonical/alias memberships in one folder collapse to
// a single row with the earliest arrival.
func TestRecentTVArrivalRowsCanonicalizeAndDeduplicate(t *testing.T) {
	c := buildRecentTVConditions(RecentTVQuery{LibraryIDs: []int{2}, Limit: 5})
	rows := recentTVArrivalRowsSQL(c.episodeRows)
	for _, want := range []string{
		recentTVMembershipJoinSQL,
		recentTVCanonicalEpisodeIDSQL + " AS episode_id",
		"MIN(el.first_seen_at) AS first_seen_at",
		"GROUP BY e.series_id, " + recentTVCanonicalEpisodeIDSQL + ", el.media_folder_id",
		"e.title AS episode_title",
	} {
		if !strings.Contains(rows, want) {
			t.Fatalf("arrival rows missing %q:\n%s", want, rows)
		}
	}
	if strings.Contains(rows, "JOIN episodes e ON e.content_id = el.episode_id") {
		t.Fatalf("arrival rows still join the membership identity, not the canonical episode:\n%s", rows)
	}
}

// TestRecentTVEventAggregateCountsCanonicalIDs guards the plain List shape's
// per-event aggregate: episode counts and anchors read canonical ids from the
// deduped per-folder availability pass.
func TestRecentTVEventAggregateCountsCanonicalIDs(t *testing.T) {
	c := buildRecentTVConditions(RecentTVQuery{LibraryIDs: []int{2}, Limit: 5})
	deduped := recentTVDedupedEpisodeRowsSQL(c.episodeRows)
	for _, want := range []string{
		recentTVMembershipJoinSQL,
		recentTVCanonicalEpisodeIDSQL + " AS episode_id",
		"MIN(el.first_seen_at) AS first_seen_at",
		"JOIN episodes e ON e.content_id = " + recentTVCanonicalEpisodeIDSQL,
		"GROUP BY e.series_id, " + recentTVCanonicalEpisodeIDSQL + ", el.media_folder_id",
	} {
		if !strings.Contains(deduped, want) {
			t.Fatalf("deduped availability missing %q:\n%s", want, deduped)
		}
	}

	agg := recentTVEpisodeEventAggregateSQL()
	for _, want := range []string{
		"COUNT(DISTINCT der.episode_id)",
		"FROM deduped_episode_rows der",
		"der.first_seen_at BETWEEN pk.event_start AND pk.added_at",
	} {
		if !strings.Contains(agg, want) {
			t.Fatalf("event aggregate missing %q:\n%s", want, agg)
		}
	}
	if strings.Contains(agg, "el.episode_id") {
		t.Fatalf("event aggregate still reads membership identity:\n%s", agg)
	}
}

// TestRecentTVNewestQueryCanonicalizesEveryShape covers the ListNewest window:
// the active-series cutoff, the history lateral, and the series-without-
// episodes anti-join all resolve memberships to their canonical episode.
func TestRecentTVNewestQueryCanonicalizesEveryShape(t *testing.T) {
	q := RecentTVQuery{LibraryIDs: []int{2}, Limit: 5}
	c := buildRecentTVConditions(q)
	sqlText, args := buildRecentTVNewestQuery(q, time.Date(2026, 8, 7, 8, 0, 0, 0, time.UTC))
	if len(args) != c.argIdx+2 {
		t.Fatalf("newest args = %d, want %d", len(args), c.argIdx+2)
	}
	if got := strings.Count(sqlText, "LEFT JOIN episode_aliases ea_recent"); got < 4 {
		t.Fatalf("membership canonicalization joins = %d, want at least 4 (active, history, anti-join, anchor):\n%s", got, sqlText)
	}
	if got := strings.Count(sqlText, recentTVCanonicalEpisodeIDSQL); got < 5 {
		t.Fatalf("canonical identity count = %d, want at least 5:\n%s", got, sqlText)
	}
	if strings.Contains(sqlText, "JOIN episodes e ON e.content_id = el.episode_id") {
		t.Fatalf("newest query still joins the membership identity:\n%s", sqlText)
	}
	if !strings.Contains(sqlText, "GROUP BY "+recentTVCanonicalEpisodeIDSQL+", el.media_folder_id") {
		t.Fatalf("newest history lateral does not dedupe canonical memberships per folder:\n%s", sqlText)
	}
}

// TestRecentTVPlayTargetLooksThroughAliases guards the page's play hint: it
// rejects alias rows as targets, accepts alias membership, and accepts files
// owned by an alias of the canonical episode.
func TestRecentTVPlayTargetLooksThroughAliases(t *testing.T) {
	sqlText := buildRecentTVResultQuery("", "FROM page")
	for _, want := range []string{
		EpisodeAliasExclusionSQL("e_play.content_id"),
		"JOIN episode_libraries el_play ON el_play.episode_id = ea.alias_episode_id",
		"ea.canonical_episode_id = e_play.content_id",
		"mf.missing_since IS NULL",
		"el_play.media_folder_id = ANY($1)",
	} {
		if !strings.Contains(sqlText, want) {
			t.Fatalf("result query missing %q:\n%s", want, sqlText)
		}
	}
}
