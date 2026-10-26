package playback

import (
	"github.com/Silo-Server/silo-server/internal/lang"
	"github.com/Silo-Server/silo-server/internal/models"
)

// BackfillAudioTrackLanguages fills a track's Languages array (and a placeholder
// primary Language) from its EmbeddedTitle when a probe predates MULTI/DUAL
// language support. The catalog picker and the playback start path both resolve
// "which track would play" against this same backfilled view, so the marker on
// the detail page and the track the planner selects cannot disagree about a
// title whose language only lives in the embedded title.
//
// The slice is modified in place and returned for convenient chaining; callers
// that must not mutate the catalog row pass a copy.
func BackfillAudioTrackLanguages(tracks []models.AudioTrack) []models.AudioTrack {
	for i := range tracks {
		track := &tracks[i]
		if len(track.Languages) > 0 {
			continue
		}
		if track.Language != "" && track.Language != "und" && track.Language != "mul" {
			continue
		}
		if languages := lang.ParseLanguages(track.EmbeddedTitle); len(languages) > 0 {
			track.Languages = languages
			if track.Language == "" || track.Language == "und" || track.Language == "mul" {
				track.Language = languages[0]
			}
		}
	}
	return tracks
}
