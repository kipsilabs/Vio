package playback

// copyRemuxRenditionOpts is a copy remux of a source whose embedded SubRip
// track 3 is carried as the rendition.
func copyRemuxRenditionOpts(dir string, mpegts bool) TranscodeOpts {
	opts := subtitleRenditionTestOpts(dir)
	opts.TargetCodecVideo, opts.SourceVideoCodec, opts.CopyVideoMPEGTS = "copy", transcodeCodecHEVC, mpegts
	opts.TotalDuration = 12.5
	return opts
}
