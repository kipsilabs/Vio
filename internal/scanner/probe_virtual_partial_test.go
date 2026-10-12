package scanner

import (
	"context"
	"errors"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

// TestVirtualProbeEnumerationPartial is the completeness table for the probe
// enumerator: a video-only stream table is a partial read only when the item
// declared audio, an audiovisual one is complete, a video-only source with no
// expected audio is complete, and an empty subtitle set is a valid closed
// answer.
func TestVirtualProbeEnumerationPartial(t *testing.T) {
	cases := []struct {
		name        string
		probe       *ProbeData
		expectAudio bool
		want        bool
	}{
		{
			name:        "video without audio is partial when audio is expected",
			probe:       &ProbeData{VideoTracks: []VideoTrackInfo{{Codec: "hevc"}}},
			expectAudio: true,
			want:        true,
		},
		{
			name: "video with subtitle but no audio is partial when audio is expected",
			probe: &ProbeData{
				VideoTracks:    []VideoTrackInfo{{Codec: "hevc"}},
				SubtitleTracks: []SubtitleTrackInfo{{Index: 1, Codec: "subrip"}},
			},
			expectAudio: true,
			want:        true,
		},
		{
			name:        "video without audio is complete for a video-only source",
			probe:       &ProbeData{VideoTracks: []VideoTrackInfo{{Codec: "hevc"}}},
			expectAudio: false,
			want:        false,
		},
		{
			name: "video with subtitle and no audio is complete for a video-only source",
			probe: &ProbeData{
				VideoTracks:    []VideoTrackInfo{{Codec: "hevc"}},
				SubtitleTracks: []SubtitleTrackInfo{{Index: 1, Codec: "subrip"}},
			},
			expectAudio: false,
			want:        false,
		},
		{
			name: "video with audio is complete",
			probe: &ProbeData{
				VideoTracks: []VideoTrackInfo{{Codec: "hevc"}},
				AudioTracks: []AudioTrackInfo{{Index: 1, Codec: "eac3", Channels: 6}},
			},
			expectAudio: true,
			want:        false,
		},
		{
			name: "video with audio and no subtitles is complete",
			probe: &ProbeData{
				VideoTracks: []VideoTrackInfo{{Codec: "h264"}},
				AudioTracks: []AudioTrackInfo{{Index: 1, Codec: "aac", Channels: 2}},
			},
			expectAudio: true,
			want:        false,
		},
		{
			name:        "audio-only is complete",
			probe:       &ProbeData{AudioTracks: []AudioTrackInfo{{Index: 0, Codec: "aac", Channels: 2}}},
			expectAudio: false,
			want:        false,
		},
		{
			name:        "no streams is not partial (it is no-tracks)",
			probe:       &ProbeData{},
			expectAudio: true,
			want:        false,
		},
		{
			name: "track without a container index is partial",
			probe: &ProbeData{
				VideoTracks: []VideoTrackInfo{{Codec: "hevc"}},
				AudioTracks: []AudioTrackInfo{{Index: -1, Codec: "eac3"}},
			},
			expectAudio: true,
			want:        true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := virtualProbeEnumerationPartial(tc.probe, tc.expectAudio); got != tc.want {
				t.Fatalf("virtualProbeEnumerationPartial = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestVirtualSourceExpectsAudio pins the expected-audio signal: the item's
// declared audio codec or declared audio tracks are the only evidence that a
// missing probe audio stream is an incomplete read. A row that declares neither
// may legitimately be video-only.
func TestVirtualSourceExpectsAudio(t *testing.T) {
	cases := []struct {
		name string
		file *models.MediaFile
		want bool
	}{
		{name: "nil row", file: nil, want: false},
		{name: "no declared audio", file: &models.MediaFile{CodecVideo: "hevc"}, want: false},
		{name: "declared audio codec", file: &models.MediaFile{CodecAudio: "eac3"}, want: true},
		{name: "declared audio track", file: &models.MediaFile{AudioTracks: []models.AudioTrack{{Codec: "aac", Index: -1}}}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := virtualSourceExpectsAudio(tc.file); got != tc.want {
				t.Fatalf("virtualSourceExpectsAudio = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestProbeVirtualSourceFlagsPartialEnumeration drives the real enumerator entry
// point with a probe that returns video and subtitles but no audio for an item
// that declared audio: the exact production signature of a partial relay read.
// The result must be flagged partial and must not carry a probe stamp, so no
// caller can persist or publish it as verified.
func TestProbeVirtualSourceFlagsPartialEnumeration(t *testing.T) {
	ffprobe := writeExecutable(t, "ffprobe", `#!/bin/sh
printf '%s' '{"format":{"format_name":"matroska","duration":"3600"},"streams":[{"index":0,"codec_name":"hevc","codec_type":"video","width":1920,"height":1080},{"index":1,"codec_name":"subrip","codec_type":"subtitle","tags":{"language":"eng"}}]}'
`)
	file := &models.MediaFile{ID: 11, FilePath: "virtual://episode/tt11?result=partial", CodecAudio: "eac3"}
	probed, err := ProbeVirtualSource(context.Background(), ffprobe, "", "http://127.0.0.1/source", file, nil)
	if !errors.Is(err, ErrVirtualProbePartial) {
		t.Fatalf("ProbeVirtualSource error = %v, want ErrVirtualProbePartial", err)
	}
	if probed != file {
		t.Fatal("ProbeVirtualSource must return the original file on a partial probe")
	}
	if probed.ProbeUpdatedAt != nil {
		t.Fatalf("ProbeUpdatedAt = %v, want nil (a partial probe is not evidence)", probed.ProbeUpdatedAt)
	}
}

// TestProbeVirtualSourceAcceptsVideoOnlySource pins the correction: an item
// that declares no audio is allowed to enumerate video only. The result is a
// complete answer, so it is stamped and never held pending as a partial read.
func TestProbeVirtualSourceAcceptsVideoOnlySource(t *testing.T) {
	ffprobe := writeExecutable(t, "ffprobe", `#!/bin/sh
printf '%s' '{"format":{"format_name":"matroska","duration":"3600"},"streams":[{"index":0,"codec_name":"hevc","codec_type":"video","width":1920,"height":1080}]}'
`)
	file := &models.MediaFile{ID: 13, FilePath: "virtual://episode/tt13?result=video-only", CodecVideo: "hevc"}
	probed, err := ProbeVirtualSource(context.Background(), ffprobe, "", "http://127.0.0.1/source", file, nil)
	if err != nil {
		t.Fatalf("ProbeVirtualSource error = %v, want nil for a declared video-only source", err)
	}
	if len(probed.VideoTracks) != 1 || len(probed.AudioTracks) != 0 {
		t.Fatalf("tracks = %d video / %d audio, want 1 / 0", len(probed.VideoTracks), len(probed.AudioTracks))
	}
	if probed.ProbeUpdatedAt == nil {
		t.Fatal("a complete video-only probe must be stamped")
	}
}

// TestProbeVirtualSourceAcceptsCompleteEnumeration pins the other direction: an
// audiovisual stream table with real container indexes enumerates cleanly, so
// the partial guard does not reject genuine complete probes (including one with
// no embedded subtitles).
func TestProbeVirtualSourceAcceptsCompleteEnumeration(t *testing.T) {
	ffprobe := writeExecutable(t, "ffprobe", `#!/bin/sh
printf '%s' '{"format":{"format_name":"matroska","duration":"3600"},"streams":[{"index":0,"codec_name":"hevc","codec_type":"video","width":1920,"height":1080},{"index":1,"codec_name":"eac3","codec_type":"audio","channels":6,"tags":{"language":"eng"}},{"index":2,"codec_name":"subrip","codec_type":"subtitle","tags":{"language":"eng"}}]}'
`)
	file := &models.MediaFile{ID: 12, FilePath: "virtual://episode/tt12?result=complete"}
	probed, err := ProbeVirtualSource(context.Background(), ffprobe, "", "http://127.0.0.1/source", file, nil)
	if err != nil {
		t.Fatalf("ProbeVirtualSource error = %v, want nil for a complete enumeration", err)
	}
	if len(probed.AudioTracks) != 1 || len(probed.SubtitleTracks) != 1 {
		t.Fatalf("tracks = %d audio / %d subtitle, want 1 / 1", len(probed.AudioTracks), len(probed.SubtitleTracks))
	}
	if probed.ProbeUpdatedAt == nil {
		t.Fatal("a complete probe must be stamped")
	}
}
