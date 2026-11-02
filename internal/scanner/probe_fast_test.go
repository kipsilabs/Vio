package scanner

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

// cleanFastProbeJSON is a complete candidate answer: a real stream table plus
// both format-level identity fields (duration and size).
const cleanFastProbeJSON = `{"format":{"format_name":"matroska","duration":"1350","size":"3221225472"},"streams":[{"index":0,"codec_name":"hevc","codec_type":"video","width":1920,"height":1080},{"index":1,"codec_name":"eac3","codec_type":"audio","channels":6,"tags":{"language":"eng"}},{"index":2,"codec_name":"subrip","codec_type":"subtitle","tags":{"language":"eng"}}]}`

// TestBuildFastProbeArgs pins the candidate preprobe argv. The fast behavior
// comes from the tiny probesize and zero analysis window plus the flags; there
// must be no -find_stream_info option, because it takes no argument and a
// written value breaks parsing.
func TestBuildFastProbeArgs(t *testing.T) {
	args := buildFastProbeArgs("/media/movie.mkv")
	for _, want := range []string{
		"-v", "error",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		"-show_chapters",
		"-probesize", fastProbeSizeLimit,
		"-analyzeduration", fastProbeAnalyzeDuration,
		fastProbeFFlagsOption, "+discardcorrupt+fastseek+nobuffer",
		"/media/movie.mkv",
	} {
		if !slices.Contains(args, want) {
			t.Fatalf("fast probe args %q missing %q", args, want)
		}
	}
	for _, forbidden := range []string{"-find_stream_info", "32M", "10M"} {
		if slices.Contains(args, forbidden) {
			t.Fatalf("fast probe args %q must not contain %q", args, forbidden)
		}
	}
	if got := args[len(args)-3]; got != fastProbeFFlagsOption {
		t.Fatalf("fast probe args order = %q, want the flags before the input path", args)
	}
	if got := args[len(args)-2]; got != fastProbeFlags {
		t.Fatalf("fast probe flags = %q, want %q", got, fastProbeFlags)
	}
	if got := args[len(args)-1]; got != "/media/movie.mkv" {
		t.Fatalf("fast probe input = %q, want the input path last", got)
	}
}

// TestProbeVirtualSourceFastPreprobeCommitsOnCleanSource pins the win: a source
// whose fast preprobe already carries the identity fields and a complete
// stream table is probed once, by the fast argv, and the result is committed as
// a normal probe.
func TestProbeVirtualSourceFastPreprobeCommitsOnCleanSource(t *testing.T) {
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "ffprobe.args")
	ffprobe := filepath.Join(dir, "ffprobe")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + argsLog + "'\nprintf '%s' '" + cleanFastProbeJSON + "'\n"
	if err := os.WriteFile(ffprobe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	file := &models.MediaFile{ID: 21, FilePath: "virtual://movie/tt21?result=clean", FileSize: 3221225472}
	probed, err := ProbeVirtualSource(context.Background(), ffprobe, "", "http://127.0.0.1/source", file, nil)
	if err != nil {
		t.Fatalf("ProbeVirtualSource error = %v, want nil", err)
	}
	if len(probed.VideoTracks) != 1 || len(probed.AudioTracks) != 1 || len(probed.SubtitleTracks) != 1 {
		t.Fatalf("tracks = %d video / %d audio / %d subtitle, want 1 / 1 / 1",
			len(probed.VideoTracks), len(probed.AudioTracks), len(probed.SubtitleTracks))
	}
	if probed.Duration != 1350 || probed.ProbedFormatSize != 3221225472 {
		t.Fatalf("identity = %ds / %d bytes, want 1350 / 3221225472", probed.Duration, probed.ProbedFormatSize)
	}
	if probed.ProbeUpdatedAt == nil {
		t.Fatal("a usable fast probe must be stamped like any other complete probe")
	}

	logged, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("reading ffprobe invocation log: %v", err)
	}
	invocations := strings.Split(strings.TrimSpace(string(logged)), "\n")
	if len(invocations) != 1 {
		t.Fatalf("ffprobe ran %d times, want only the fast preprobe: %q", len(invocations), logged)
	}
	if !strings.Contains(invocations[0], "-probesize 32 ") {
		t.Fatalf("single invocation was not the fast preprobe: %q", invocations[0])
	}
}

// TestProbeVirtualSourceEscalatesUnusableFastPreprobe pins the fallback: when
// the fast read cannot support the identity or completeness gates, the full
// probe runs and its verdict (including the packet-scan-capable path) is the
// one that commits.
func TestProbeVirtualSourceEscalatesUnusableFastPreprobe(t *testing.T) {
	cases := []struct {
		name        string
		fastJSON    string
		declared    models.MediaFile
		wantTracks  bool
		wantSeconds int
	}{
		{
			name:        "missing duration",
			fastJSON:    `{"format":{"format_name":"matroska","size":"3221225472"},"streams":[{"index":0,"codec_name":"hevc","codec_type":"video","width":1920,"height":1080},{"index":1,"codec_name":"eac3","codec_type":"audio","channels":6}]}`,
			wantTracks:  true,
			wantSeconds: 1350,
		},
		{
			name:        "missing size",
			fastJSON:    `{"format":{"format_name":"matroska","duration":"1350"},"streams":[{"index":0,"codec_name":"hevc","codec_type":"video","width":1920,"height":1080},{"index":1,"codec_name":"eac3","codec_type":"audio","channels":6}]}`,
			wantTracks:  true,
			wantSeconds: 1350,
		},
		{
			name:        "empty stream table",
			fastJSON:    `{"format":{"format_name":"matroska","duration":"1350","size":"3221225472"},"streams":[]}`,
			wantTracks:  true,
			wantSeconds: 1350,
		},
		{
			name:        "partial enumeration of a declared-audio item",
			fastJSON:    `{"format":{"format_name":"matroska","duration":"1350","size":"3221225472"},"streams":[{"index":0,"codec_name":"hevc","codec_type":"video","width":1920,"height":1080}]}`,
			declared:    models.MediaFile{CodecAudio: "eac3"},
			wantTracks:  true,
			wantSeconds: 1350,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			argsLog := filepath.Join(dir, "ffprobe.args")
			ffprobe := filepath.Join(dir, "ffprobe")
			script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + argsLog + "'\n" +
				"case \" $* \" in\n" +
				"  *\" -probesize 32 \"*) printf '%s' '" + tc.fastJSON + "' ;;\n" +
				"  *) printf '%s' '" + cleanFastProbeJSON + "' ;;\n" +
				"esac\n"
			if err := os.WriteFile(ffprobe, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}

			file := tc.declared
			file.ID = 22
			file.FilePath = "virtual://movie/tt22?result=escalate"
			file.FileSize = 3221225472
			probed, err := ProbeVirtualSource(context.Background(), ffprobe, "", "http://127.0.0.1/source", &file, nil)
			if err != nil {
				t.Fatalf("ProbeVirtualSource error = %v, want the full probe's verdict", err)
			}
			if tc.wantTracks && (len(probed.VideoTracks) == 0 || len(probed.AudioTracks) == 0) {
				t.Fatalf("tracks = %d video / %d audio, want the full probe's complete table",
					len(probed.VideoTracks), len(probed.AudioTracks))
			}
			if probed.Duration != tc.wantSeconds {
				t.Fatalf("duration = %d, want %d from the full probe", probed.Duration, tc.wantSeconds)
			}
			if probed.ProbeUpdatedAt == nil {
				t.Fatal("the escalated full probe must be stamped")
			}

			logged, err := os.ReadFile(argsLog)
			if err != nil {
				t.Fatalf("reading ffprobe invocation log: %v", err)
			}
			invocations := strings.Split(strings.TrimSpace(string(logged)), "\n")
			if len(invocations) != 2 {
				t.Fatalf("ffprobe ran %d times, want fast preprobe then full probe: %q", len(invocations), logged)
			}
			if !strings.Contains(invocations[0], "-probesize 32 ") {
				t.Fatalf("first invocation was not the fast preprobe: %q", invocations[0])
			}
			if !strings.Contains(invocations[1], "-probesize 32M ") || !strings.Contains(invocations[1], "-analyzeduration 10M ") {
				t.Fatalf("second invocation was not the full probe: %q", invocations[1])
			}
		})
	}
}
