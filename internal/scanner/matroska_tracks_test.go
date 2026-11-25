package scanner

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/Silo-Server/silo-server/internal/mediaprobe"
)

const (
	mkvVideo    = mediaprobe.MatroskaTrackTypeVideo
	mkvAudio    = mediaprobe.MatroskaTrackTypeAudio
	mkvSubtitle = mediaprobe.MatroskaTrackTypeSubtitle
)

// A virtual source is probed over HTTP through a provider relay URL, so the
// local Matroska reader cannot open it. The scanner must leave FFprobe's
// subtitle metadata in place instead of trying the URL as a path.
func TestApplyMatroskaSubtitleTracksSkipsLocalReadForRemoteSources(t *testing.T) {
	sources := []string{
		"http://127.0.0.1:9100/source/hash/stream",
		"https://provider.example/source/hash/stream",
		"virtual://movie/tt123?result=abc",
	}
	for _, source := range sources {
		t.Run(source, func(t *testing.T) {
			reads := 0
			reader := func(context.Context, string) ([]mediaprobe.MatroskaTrack, os.FileInfo, error) {
				reads++
				return nil, nil, errors.New("local read must not run for a remote source")
			}
			probe := &ProbeData{
				Container:   "mkv",
				VideoTracks: []VideoTrackInfo{{}},
				SubtitleTracks: []SubtitleTrackInfo{{
					Index: 1, Codec: "subrip", Language: "en",
				}},
			}
			applyMatroskaSubtitleTracksWithReader(context.Background(), source, probe, reader)
			if reads != 0 {
				t.Fatalf("local Matroska reads = %d, want 0 for %s", reads, source)
			}
			got := probe.SubtitleTracks[0]
			if got.Index != 1 || got.Codec != "subrip" || got.Language != "en" || got.ContainerTrackID != "" || got.IndexOnly {
				t.Fatalf("probe subtitle = %+v, want the FFprobe metadata unchanged", got)
			}
		})
	}
}

// The remote-source guard must not turn off the enrichment for a local file.
func TestApplyMatroskaSubtitleTracksReadsLocalPaths(t *testing.T) {
	const localPath = "/library/movie.mkv"
	reads := 0
	reader := func(_ context.Context, path string) ([]mediaprobe.MatroskaTrack, os.FileInfo, error) {
		reads++
		if path != localPath {
			t.Fatalf("read path = %q, want %q", path, localPath)
		}
		return []mediaprobe.MatroskaTrack{
			{Number: 1, Type: mkvVideo, CodecID: "V_MPEG4/ISO/AVC"},
			{Number: 2, Type: mkvSubtitle, CodecID: "S_TEXT/UTF8", LanguageBCP47: "pt-BR"},
		}, nil, nil
	}
	probe := &ProbeData{
		Container:      "mkv",
		VideoTracks:    []VideoTrackInfo{{}},
		SubtitleTracks: []SubtitleTrackInfo{{Index: 1, Codec: "subrip", Language: "por"}},
	}
	applyMatroskaSubtitleTracksWithReader(context.Background(), localPath, probe, reader)
	if reads != 1 {
		t.Fatalf("local Matroska reads = %d, want 1", reads)
	}
	got := probe.SubtitleTracks[0]
	if got.ContainerTrackID != "2" || got.Language != "pt-BR" {
		t.Fatalf("probe subtitle = %+v, want local track id 2 with language pt-BR", got)
	}
}
