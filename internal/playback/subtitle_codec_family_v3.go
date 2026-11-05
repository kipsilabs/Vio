package playback

// SubtitleCodecFamilyV3 is the canonical FFmpeg name of the codec family a
// subtitle codec belongs to: subrip for srt, ass for ssa, webvtt for vtt,
// mov_text for tx3g, hdmv_pgs_subtitle for pgs; any other codec is normalized.
//
// Keeping this mapping in playback gives subtitle consumers one shared
// contract while preserving the planner's codec normalization and routing
// behavior for codecs outside the index-served families.
func SubtitleCodecFamilyV3(codec string) string {
	if IsASS(normalizeCodecV3(codec)) {
		return subtitleFormatASS
	}
	return normalizeNativeSubtitleCodecV3(codec)
}
