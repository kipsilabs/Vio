package catalog

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

func TestMergeSeasonEpisodesByContentID(t *testing.T) {
	available := []*models.Episode{
		{ContentID: "e1", SeasonNumber: 1, EpisodeNumber: 1, Title: "available one"},
	}
	metadata := []*models.Episode{
		{ContentID: "e1", SeasonNumber: 1, EpisodeNumber: 1, Title: "metadata one"},
		{ContentID: "e2", SeasonNumber: 1, EpisodeNumber: 2, Title: "metadata two"},
		{ContentID: "e3", SeasonNumber: 1, EpisodeNumber: 3, Title: "metadata three"},
	}

	merged := MergeSeasonEpisodesByContentID(available, metadata)
	if len(merged) != 3 {
		t.Fatalf("merged = %d episodes, want 3", len(merged))
	}
	if merged[0].Title != "available one" {
		t.Fatalf("available row did not win the duplicate: %+v", merged[0])
	}
	for i, episode := range merged {
		if episode.EpisodeNumber != i+1 {
			t.Fatalf("merged[%d] episode number = %d, want %d", i, episode.EpisodeNumber, i+1)
		}
	}

	if got := MergeSeasonEpisodesByContentID(nil, metadata); len(got) != 3 {
		t.Fatalf("metadata-only merge = %d episodes, want 3", len(got))
	}
	if got := MergeSeasonEpisodesByContentID(available, nil); len(got) != 1 {
		t.Fatalf("available-only merge = %d episodes, want 1", len(got))
	}
}
