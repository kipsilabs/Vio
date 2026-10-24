package catalog

import (
	"strings"
	"testing"
)

// TestEpisodeRollupCompletionSQLExcludesAliases pins alias exclusion in both
// child-episode EXISTS branches, so a series/season completion rollup counts the
// same canonical episodes as ListBySeriesIDs/ListBySeasonIDs.
func TestEpisodeRollupCompletionSQLExcludesAliases(t *testing.T) {
	sql := NewQueryBuilder("mi").episodeRollupCompletionSQL("series_id", "mi", 1)
	if got := strings.Count(sql, episodeAliasExclusionPredicate); got != 2 {
		t.Fatalf("completion SQL alias-exclusion count = %d, want 2:\n%s", got, sql)
	}
	if got := strings.Count(sql, episodeAvailabilityPredicate); got != 2 {
		t.Fatalf("completion SQL availability count = %d, want 2:\n%s", got, sql)
	}
}

func TestNextUpQueriesExcludeAliasesAndFallBackToAliasFiles(t *testing.T) {
	aliasE := episodeAliasExclusionPredicateFor("e")
	for name, query := range map[string]string{
		"episode lookup":  nextUpEpisodeLookupQuery,
		"post-anchor":     nextUpPostAnchorEpisodesQuery,
		"series episodes": nextUpSeriesEpisodesQuery,
	} {
		if !strings.Contains(query, aliasE) {
			t.Fatalf("%s query does not exclude alias rows:\n%s", name, query)
		}
	}

	successor := nextUpSuccessorQuery
	if !strings.Contains(successor, episodeAliasExclusionPredicateFor("e2")) {
		t.Fatalf("successor query does not exclude alias rows:\n%s", successor)
	}
	if !strings.Contains(successor, "ea.canonical_episode_id = e2.content_id") {
		t.Fatalf("successor query does not fall back to an alias-owned file:\n%s", successor)
	}
	if !strings.Contains(successor, "mf.missing_since IS NULL") {
		t.Fatalf("successor query does not require a present file:\n%s", successor)
	}
}

func TestPlayableTargetSQLExcludesAliasesAndFallsBackToAliasFiles(t *testing.T) {
	const fileConditions = "mf.missing_since IS NULL"

	first := firstPlayableEpisodeSQL(fileConditions, "")
	if got := strings.Count(first, episodeAliasExclusionPredicateFor("episode")); got != 2 {
		t.Fatalf("first-playable alias-exclusion count = %d, want 2:\n%s", got, first)
	}
	if !strings.Contains(first, "ea.canonical_episode_id = episode.content_id") {
		t.Fatalf("first-playable does not fall back to alias-owned files:\n%s", first)
	}

	generic := buildPlayableTargetsQuery([]string{fileConditions})
	if got := strings.Count(generic, episodeAliasExclusionPredicateFor("episode")); got != 3 {
		t.Fatalf("generic play-target alias-exclusion count = %d, want 3:\n%s", got, generic)
	}
	for _, want := range []string{
		"ea.canonical_episode_id = candidate.content_id",
		"ea.canonical_episode_id = requested.content_id",
		"mf.content_id = requested.content_id",
	} {
		if !strings.Contains(generic, want) {
			t.Fatalf("generic play-target SQL missing %q:\n%s", want, generic)
		}
	}

	postgres := buildPostgresPlayableTargetsQuery(fileConditions, "profile_progress", "AND TRUE")
	if got := strings.Count(postgres, episodeAliasExclusionPredicateFor("episode")); got < 4 {
		t.Fatalf("postgres play-target alias-exclusion count = %d, want at least 4:\n%s", got, postgres)
	}
	for _, want := range []string{
		"ea.canonical_episode_id = episode.content_id",
		"ea.canonical_episode_id = requested.content_id",
	} {
		if !strings.Contains(postgres, want) {
			t.Fatalf("postgres play-target SQL missing %q:\n%s", want, postgres)
		}
	}
}

func TestCanonicalEpisodeFileExistsSQLShape(t *testing.T) {
	sql := canonicalEpisodeFileExistsSQL("episode.content_id", "mf.missing_since IS NULL")
	for _, want := range []string{
		"mf.episode_id = episode.content_id",
		"ea.canonical_episode_id = episode.content_id",
		"ea.alias_episode_id IS NOT NULL",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("canonicalEpisodeFileExistsSQL missing %q:\n%s", want, sql)
		}
	}
	if got := strings.Count(sql, "mf.missing_since IS NULL"); got != 2 {
		t.Fatalf("canonicalEpisodeFileExistsSQL file-condition count = %d, want 2:\n%s", got, sql)
	}
}
