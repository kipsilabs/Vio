package jellycompat

func compatAudioOrdinalOrDefault(source PlaybackMediaSource) int {
	return source.SubtitleTrackIndex
}
