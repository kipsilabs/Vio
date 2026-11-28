package nodepool

import (
	"strings"
)

// JobKind classifies the work a transcode node runs, so the planner can spread
// each kind evenly across nodes instead of only the total job count. Two jobs
// of different kinds cost very different amounts: a video encode holds a GPU
// encoder for the length of a stream, while a remux that copies both streams
// costs a little disk and network. Balancing the total alone can leave one
// node's GPU saturated with encodes while another runs only remuxes.
type JobKind string

// A playback stream and a prepared download are classed by the work they do,
// not by what they are for: a download that tone-maps is a video encode on the
// GPU like any playback encode, and must count against the same kind.
const (
	// JobKindVideoTranscode re-encodes the video stream (tone mapping and
	// subtitle burn-in included), whatever happens to the audio.
	JobKindVideoTranscode JobKind = "video_transcode"
	// JobKindAudioTranscode copies the video stream, or has none, and
	// re-encodes the audio: a remux with audio conversion, or a theme
	// conversion.
	JobKindAudioTranscode JobKind = "audio_transcode"
	// JobKindRemux copies every stream into a new container.
	JobKindRemux JobKind = "remux"
	// JobKindTrickplay extracts trickplay thumbnails.
	JobKindTrickplay JobKind = "trickplay"
)

// JobKinds lists every job kind, in the order a node reports them.
var JobKinds = []JobKind{JobKindVideoTranscode, JobKindAudioTranscode, JobKindRemux, JobKindTrickplay}

// StreamJobKind classifies a stream or download from what it re-encodes.
func StreamJobKind(transcodeVideo, transcodeAudio bool) JobKind {
	switch {
	case transcodeVideo:
		return JobKindVideoTranscode
	case transcodeAudio:
		return JobKindAudioTranscode
	default:
		return JobKindRemux
	}
}

// CodecJobKind classifies a stream or download from its target codecs, where
// "copy" keeps the source stream and anything else re-encodes it.
func CodecJobKind(targetVideoCodec, targetAudioCodec string) JobKind {
	return StreamJobKind(!isCopyCodec(targetVideoCodec), !isCopyCodec(targetAudioCodec))
}

func isCopyCodec(codec string) bool {
	return strings.EqualFold(strings.TrimSpace(codec), "copy")
}
