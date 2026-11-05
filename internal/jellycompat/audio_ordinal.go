package jellycompat

func compatAudioOrdinalOrDefault(source PlaybackMediaSource) int {
	if ordinal, ok := compatAudioOrdinal(source); ok {
		return ordinal
	}
	return 0
}
