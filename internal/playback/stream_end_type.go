package playback

import "time"

// StreamEnd is where a transcode found a media file's media ends. The
// transcode-node control surface reports it (internal/nodecontrol); the
// learning that shortens a stored duration is upstream's stream-end adoption,
// deferred with the rest of the playback engine rework.
type StreamEnd struct {
	Seconds        float64
	PlannedSeconds float64
	ReadSince      time.Time
}
