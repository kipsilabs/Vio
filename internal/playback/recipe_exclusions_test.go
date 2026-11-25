package playback

import (
	"strconv"
	"testing"
)

// TestUnionRecipeExclusions keeps the durable recipe-exclusion chain ordered,
// deduplicated, blank-free, and inside its bound, and proves the candidate
// union preserves it: the Postgres writer round-trips the whole state through
// the candidate merge, so a union that dropped this list would silently forget
// every abandoned recipe.
func TestUnionRecipeExclusions(t *testing.T) {
	keys := func(count int) []string {
		out := make([]string, 0, count)
		for i := 0; i < count; i++ {
			out = append(out, "v3:"+strconv.Itoa(i))
		}
		return out
	}
	merged := UnionRecipeExclusions([]string{"v3:a", " v3:b ", "", "v3:a"}, []string{"v3:b", "v3:c"})
	if len(merged) != 3 || merged[0] != "v3:a" || merged[1] != "v3:b" || merged[2] != "v3:c" {
		t.Fatalf("merged = %v, want the trimmed, deduplicated first-seen order", merged)
	}
	if UnionRecipeExclusions(nil, nil) != nil {
		t.Fatal("an empty union must stay nil so the persisted state omits the field")
	}
	if got := UnionRecipeExclusions(keys(RecipeExclusionLimitV3), []string{"v3:new"}); len(got) != RecipeExclusionLimitV3 || got[RecipeExclusionLimitV3-1] == "v3:new" {
		t.Fatalf("the chain exceeded its bound or evicted its oldest entry: %d", len(got))
	}

	state := UnionRecoveryExclusions(RecoveryStateV3{RecipeExclusions: merged}, []RecoveryExclusionV3{{CandidateID: "candidate-1"}})
	if len(state.Exclusions) != 1 || len(state.RecipeExclusions) != 3 {
		t.Fatalf("candidate union = %+v, want the recipe chain preserved", state)
	}
}
