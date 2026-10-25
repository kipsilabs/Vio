package catalog

import (
	"strings"
	"testing"
)

// TestEpisodeAliasExclusionPredicateShape pins the shared predicate's table
// reference. Queries that alias episodes must use the alias-aware form so the
// NOT EXISTS compares against the row being filtered, not an unrelated
// episodes table reference.
func TestEpisodeAliasExclusionPredicateShape(t *testing.T) {
	for _, want := range []string{
		"NOT EXISTS",
		"FROM episode_aliases ea",
		"ea.alias_episode_id = episodes.content_id",
	} {
		if !strings.Contains(episodeAliasExclusionPredicate, want) {
			t.Fatalf("episodeAliasExclusionPredicate missing %q:\n%s", want, episodeAliasExclusionPredicate)
		}
	}

	aliased := episodeAliasExclusionPredicateFor("e")
	if !strings.Contains(aliased, "ea.alias_episode_id = e.content_id") {
		t.Fatalf("aliased predicate does not reference e.content_id:\n%s", aliased)
	}
	if strings.Contains(aliased, "episodes.content_id") {
		t.Fatalf("aliased predicate must not reference the unaliased episodes table:\n%s", aliased)
	}
}

func TestListBySeriesGroupedBySeasonQueryExcludesAliasRows(t *testing.T) {
	repo := &EpisodeRepository{}
	sql, _ := repo.buildListBySeriesGroupedBySeasonQuery("series-abc")
	if !strings.Contains(sql, episodeAliasExclusionPredicate) {
		t.Fatalf("grouped-by-season query does not exclude alias episodes:\n%s", sql)
	}
}
