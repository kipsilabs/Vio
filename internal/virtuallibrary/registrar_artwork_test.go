package virtuallibrary

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary/monitor"
)

func TestSeasonImageInputsDeduplicatesBySeason(t *testing.T) {
	inputs := seasonImageInputs("series-tvdb-42", monitor.MonitoredMedia{
		TVDBID: "42",
		Episodes: []monitor.VirtualEpisode{
			{Season: 1, Episode: 1, SeasonPosterPath: "tvdb://seasons/1.jpg", SeasonPosterSourcePath: "tvdb://seasons/1.jpg"},
			{Season: 1, Episode: 2, SeasonPosterPath: "tvdb://seasons/1.jpg", SeasonPosterSourcePath: "tvdb://seasons/1.jpg"},
			{Season: 2, Episode: 1, SeasonPosterPath: "tvdb://seasons/2.jpg", SeasonPosterSourcePath: "tvdb://seasons/2.jpg"},
		},
	}, map[int]string{
		1: "legacy-season-one",
		2: "season-tvdb-42-2",
	})

	if len(inputs) != 2 {
		t.Fatalf("inputs = %+v, want one job per season", inputs)
	}
	if inputs[0].TargetContentID != "legacy-season-one" || inputs[0].ProviderID != "tvdb" || inputs[0].ProviderContentID != "42" {
		t.Fatalf("season 1 input = %+v", inputs[0])
	}
	if inputs[1].TargetContentID != "season-tvdb-42-2" || inputs[1].SourcePath != "tvdb://seasons/2.jpg" {
		t.Fatalf("season 2 input = %+v", inputs[1])
	}
}

func TestVirtualMediaRegistrationCarriesSeasonPoster(t *testing.T) {
	payload := virtualMediaRegistration(monitor.MonitoredMedia{
		MediaType: "series",
		Title:     "Example",
		TVDBID:    "42",
		Episodes: []monitor.VirtualEpisode{{
			Season:                 1,
			Episode:                1,
			Title:                  "Pilot",
			SeasonPosterPath:       "tvdb://banners/seasons/1.jpg",
			SeasonPosterSourcePath: "tvdb://banners/seasons/1.jpg",
			SeasonPosterThumbhash:  "season-one-hash",
		}},
	}, "2")

	if len(payload.Episodes) != 1 {
		t.Fatalf("episodes = %d, want one", len(payload.Episodes))
	}
	episode := payload.Episodes[0]
	if episode.SeasonPosterPath != "tvdb://banners/seasons/1.jpg" ||
		episode.SeasonPosterSourcePath != "tvdb://banners/seasons/1.jpg" ||
		episode.SeasonPosterThumbhash != "season-one-hash" {
		t.Fatalf("episode artwork = %+v, want the season 1 poster", episode)
	}
}
