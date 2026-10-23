package catalog

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

func TestEpisodeAliasValidate(t *testing.T) {
	aliasID := "episode-tvdb-1-2-21"
	valid := &models.EpisodeAlias{
		SeriesID:              "series-tvdb-1",
		CanonicalEpisodeID:    "episode-tvdb-1-2-1",
		AliasEpisodeID:        &aliasID,
		ProviderSeasonNumber:  1,
		ProviderEpisodeNumber: 21,
		Source:                "air_date",
		Confidence:            95,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid alias rejected: %v", err)
	}
	valid.AliasEpisodeID = &valid.CanonicalEpisodeID
	if err := valid.Validate(); err == nil {
		t.Fatal("alias pointing at its canonical episode must be rejected")
	}
	valid.AliasEpisodeID = nil
	valid.Confidence = 101
	if err := valid.Validate(); err == nil {
		t.Fatal("confidence above 100 must be rejected")
	}
}
