package virtuallibrary

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary/monitor"
)

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
