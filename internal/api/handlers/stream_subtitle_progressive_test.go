package handlers

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// A bitmap page that could not be cut from the source at an affordable read cost
// is waiting for the track's progressive artifact, and that artifact only exists
// if the whole-track fill runs. This is the one case where a bitmap fill is
// started while the viewer is playing: the bounded page prefetch would re-read
// the same source for pages the fill is already publishing.
func TestVirtualWaitingBitmapPageWarmsWholeTrackWhilePlaying(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "ffmpeg.args")
	handler, session, file, virtualURI := newVirtualSubtitleWindowFixture(t, dir,
		"#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+argsLog+"'\nprintf 'PG'\n")
	file.SubtitleTracks = []models.SubtitleTrack{{Index: 0, Codec: "hdmv_pgs_subtitle"}}
	session.VirtualSubtitleTracks = file.SubtitleTracks
	session.IsPaused = false
	// A known, large container is what a page cannot be extracted from
	// cheaply; the whole-track fill is the page path for it.
	file.FileSize = virtualSubtitleImplicitWindowMinSourceBytes
	writeSubtitleLayoutProbe(t, dir, file.SubtitleTracks...)
	identity := virtualSubtitleCacheIdentity(file, virtualURI, 0)

	handler.warmVirtualSubtitleForWaitingPage(file, file.SubtitleTracks[0], session, playback.StreamExtractOpts{
		InputPath:       "unused",
		CacheIdentity:   identity,
		SourceSizeBytes: file.FileSize,
		TrackIndex:      0,
		SourceCodec:     "hdmv_pgs_subtitle",
		SeekSeconds:     600,
		DurationSeconds: 120,
		FFmpegPath:      filepath.Join(dir, "ffmpeg"),
	}, nil, true, "")

	waitForWarmInvocation(t, argsLog)
	window, warm := ffmpegLogLines(t, argsLog)
	if warm != 1 {
		t.Fatalf("full-track fills = %d, want exactly 1", warm)
	}
	if window != 0 {
		t.Fatalf("bounded page extracts = %d, want none: the fill is the page path", window)
	}
	waitForCommittedEntry(t, handler.SubtitleCache, identity, 0, "hdmv_pgs_subtitle")
}
