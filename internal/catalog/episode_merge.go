package catalog

import (
	"sort"

	"github.com/Silo-Server/silo-server/internal/models"
)

// MergeSeasonEpisodesByContentID returns the canonical episode set a season
// read displays: library-available episodes plus metadata-only episodes,
// deduplicated by content_id. The available row wins when both lists carry the
// same episode, so a file-backed row keeps its identity. Metadata-only rows
// stay in the result as fileless episodes, which renders without a Play target.
//
// The result keeps natural season/episode order, with content_id as a stable
// tie-break. Alias layout rows never reach this helper because both repository
// reads already exclude them.
func MergeSeasonEpisodesByContentID(available, metadata []*models.Episode) []*models.Episode {
	if len(available) == 0 {
		return metadata
	}
	if len(metadata) == 0 {
		return available
	}

	merged := make([]*models.Episode, 0, len(available)+len(metadata))
	index := make(map[string]int, len(available)+len(metadata))
	appendEpisode := func(episode *models.Episode, replace bool) {
		if episode == nil {
			return
		}
		if existing, ok := index[episode.ContentID]; ok {
			if replace {
				merged[existing] = episode
			}
			return
		}
		index[episode.ContentID] = len(merged)
		merged = append(merged, episode)
	}

	for _, episode := range metadata {
		appendEpisode(episode, false)
	}
	// Available rows replace their metadata counterparts so file-backed rows
	// win the identity when the same content ID appears in both reads.
	for _, episode := range available {
		appendEpisode(episode, true)
	}

	sort.SliceStable(merged, func(i, j int) bool {
		a, b := merged[i], merged[j]
		if a.SeasonNumber != b.SeasonNumber {
			return a.SeasonNumber < b.SeasonNumber
		}
		if a.EpisodeNumber != b.EpisodeNumber {
			return a.EpisodeNumber < b.EpisodeNumber
		}
		return a.ContentID < b.ContentID
	})
	return merged
}
