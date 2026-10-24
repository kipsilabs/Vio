package virtuallibrary

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/metadata"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/monitor"
)

func TestApplySeasonArtworkAttachesPosterToSeasonEpisodes(t *testing.T) {
	item := monitor.MonitoredMedia{
		Episodes: []monitor.VirtualEpisode{
			{Season: 1, Episode: 1},
			{Season: 1, Episode: 2},
			{Season: 2, Episode: 1},
		},
	}

	applySeasonArtwork(&item, []metadata.SeasonResult{
		{
			SeasonNumber:     1,
			PosterPath:       "tvdb://banners/seasons/1.jpg",
			PosterSourcePath: "tvdb://banners/seasons/1.jpg",
			PosterThumbhash:  "season-one-hash",
		},
	})

	for _, episode := range item.Episodes[:2] {
		if episode.SeasonPosterPath != "tvdb://banners/seasons/1.jpg" ||
			episode.SeasonPosterSourcePath != "tvdb://banners/seasons/1.jpg" ||
			episode.SeasonPosterThumbhash != "season-one-hash" {
			t.Fatalf("season 1 episode %d = %+v, want the season 1 poster", episode.Episode, episode)
		}
	}
	if episode := item.Episodes[2]; episode.SeasonPosterPath != "" || episode.SeasonPosterSourcePath != "" {
		t.Fatalf("season 2 episode kept artwork it was not given: %+v", episode)
	}
}

func TestApplySeasonArtworkKeepsExistingPosterWhenLookupHasNone(t *testing.T) {
	item := monitor.MonitoredMedia{
		Episodes: []monitor.VirtualEpisode{{
			Season:                 1,
			Episode:                1,
			SeasonPosterPath:       "existing/poster.jpg",
			SeasonPosterSourcePath: "tvdb://existing.jpg",
		}},
	}

	applySeasonArtwork(&item, []metadata.SeasonResult{{SeasonNumber: 1}})

	if item.Episodes[0].SeasonPosterPath != "existing/poster.jpg" ||
		item.Episodes[0].SeasonPosterSourcePath != "tvdb://existing.jpg" {
		t.Fatalf("existing season artwork was cleared: %+v", item.Episodes[0])
	}
}
