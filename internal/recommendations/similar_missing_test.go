package recommendations

import (
	"strings"
	"testing"
)

func TestDropMissingItemsDropsOrphansAndPreservesOrder(t *testing.T) {
	items := []ScoredItem{
		{MediaItemID: "movie-missing", Score: 0.9},
		{MediaItemID: "movie-a", Score: 0.8},
		{MediaItemID: "movie-missing-2", Score: 0.75},
		{MediaItemID: "movie-b", Score: 0.7},
	}
	existing := map[string]struct{}{"movie-a": {}, "movie-b": {}}

	got := dropMissingItems(items, existing)

	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (%+v)", len(got), got)
	}
	if got[0].MediaItemID != "movie-a" || got[1].MediaItemID != "movie-b" {
		t.Fatalf("surviving order changed: %+v", got)
	}
	if got[0].Score != 0.8 || got[1].Score != 0.7 {
		t.Fatalf("scores changed: %+v", got)
	}
}

func TestDropMissingItemsAllMissingReturnsEmptyNotNil(t *testing.T) {
	items := []ScoredItem{{MediaItemID: "movie-gone", Score: 0.9}}

	got := dropMissingItems(items, map[string]struct{}{})

	if got == nil {
		t.Fatal("filtered result must be a non-nil empty slice so it serializes as []")
	}
	if len(got) != 0 {
		t.Fatalf("len = %d, want 0", len(got))
	}
}

func TestDropMissingItemsEmptyInputStaysEmpty(t *testing.T) {
	if got := dropMissingItems(nil, map[string]struct{}{"movie-a": {}}); len(got) != 0 {
		t.Fatalf("len = %d, want 0", len(got))
	}
}

func TestCowatchNeighborsQueryJoinsMediaItems(t *testing.T) {
	query := strings.Join(strings.Fields(cowatchNeighborsQuery), " ")

	// The neighbor id must be resolved against media_items so a co-watch row
	// for a deleted or relinked item is never returned. Embedding candidates
	// already get this from FindSimilar's join.
	assertQueryTermsInOrder(t, query,
		"FROM item_cowatch c",
		"JOIN media_items mi ON mi.content_id = c.similar_item_id",
		"WHERE c.item_id = $1",
		"ORDER BY c.jaccard_score DESC",
	)
}

func TestItemWatchersQueryOnlyIncludesExistingItems(t *testing.T) {
	query := strings.Join(strings.Fields(itemWatchersQuery), " ")

	// Co-watch pairs are built from this result; joining media_items keeps
	// deleted items out of the matrix so the co-watch table stops accumulating
	// references that no longer resolve.
	assertQueryTermsInOrder(t, query,
		"FROM user_watches",
		"JOIN media_items mi ON mi.content_id = user_watches.media_item_id",
	)
}

func assertQueryTermsInOrder(t *testing.T, query string, terms ...string) {
	t.Helper()
	pos := 0
	for _, term := range terms {
		i := strings.Index(query[pos:], term)
		if i < 0 {
			t.Fatalf("query term %q missing or out of order in query:\n%s", term, query)
		}
		pos += i + len(term)
	}
}
