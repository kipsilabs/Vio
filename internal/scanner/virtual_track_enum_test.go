package scanner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeFakeFFprobe writes an executable shell script that ignores its argv and
// prints body, so a test can drive the bounded track enumeration without a real
// media file or ffprobe install.
func writeFakeFFprobe(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffprobe")
	script := "#!/bin/sh\ncat <<'FFPROBE_JSON'\n" + body + "\nFFPROBE_JSON\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// The bounded enumeration must return every audio and subtitle stream ffprobe
// describes, mapped through the same ProbeData -> models conversion as the full
// probe, so the cold plan can carry the real inventory instead of the declared
// single-track label.
func TestEnumerateVirtualSourceTracksReturnsAllAudioAndSubtitles(t *testing.T) {
	ffprobe := writeFakeFFprobe(t, `{"streams":[
		{"index":0,"codec_type":"video","codec_name":"h264","width":1920,"height":1080},
		{"index":1,"codec_type":"audio","codec_name":"eac3","channels":6,"tags":{"language":"eng"},"disposition":{"default":1}},
		{"index":2,"codec_type":"audio","codec_name":"aac","channels":2,"tags":{"language":"jpn"}},
		{"index":3,"codec_type":"audio","codec_name":"ac3","channels":2,"tags":{"language":"deu"}},
		{"index":4,"codec_type":"subtitle","codec_name":"subrip","tags":{"language":"eng"},"disposition":{"forced":1}},
		{"index":5,"codec_type":"subtitle","codec_name":"hdmv_pgs_subtitle","tags":{"language":"fra"}}
	]}`)

	audio, subtitles, err := EnumerateVirtualSourceTracks(context.Background(), ffprobe, "virtual://movie/tt-multi?result=cand-1")
	if err != nil {
		t.Fatalf("EnumerateVirtualSourceTracks error: %v", err)
	}
	if len(audio) != 3 {
		t.Fatalf("audio tracks = %d (%#v), want 3", len(audio), audio)
	}
	if audio[0].Codec != "eac3" || audio[0].Channels != 6 || audio[0].Language != "en" {
		t.Fatalf("audio[0] = %#v, want eac3/6/en", audio[0])
	}
	if audio[2].Language != "de" {
		t.Fatalf("audio[2] = %#v, want de", audio[2])
	}
	if len(subtitles) != 2 {
		t.Fatalf("subtitle tracks = %d (%#v), want 2", len(subtitles), subtitles)
	}
	if subtitles[0].Codec != "subrip" || subtitles[0].Language != "en" || !subtitles[0].Forced {
		t.Fatalf("subtitles[0] = %#v, want subrip/en/forced", subtitles[0])
	}
}

// A file that legitimately carries video but zero audio and zero subtitle
// streams is a valid empty inventory: the enumeration must succeed with empty
// slices so the cold plan advertises empty, not fall back to a declared label.
func TestEnumerateVirtualSourceTracksReturnsEmptyInventoryForTracklessVideo(t *testing.T) {
	ffprobe := writeFakeFFprobe(t, `{"streams":[
		{"index":0,"codec_type":"video","codec_name":"h264","width":1280,"height":720}
	]}`)

	audio, subtitles, err := EnumerateVirtualSourceTracks(context.Background(), ffprobe, "virtual://movie/tt-silent?result=cand-1")
	if err != nil {
		t.Fatalf("EnumerateVirtualSourceTracks error: %v", err)
	}
	if len(audio) != 0 || len(subtitles) != 0 {
		t.Fatalf("inventory = %d audio / %d subtitle, want empty", len(audio), len(subtitles))
	}
}

// A stream table with no audio and no video at all is inconclusive: a relay can
// return it when the analysis window is too short, so it must surface as
// ErrVirtualProbeNoTracks and let the caller keep the declared inventory.
func TestEnumerateVirtualSourceTracksRejectsAllEmptyStreamTable(t *testing.T) {
	ffprobe := writeFakeFFprobe(t, `{"streams":[]}`)

	_, _, err := EnumerateVirtualSourceTracks(context.Background(), ffprobe, "virtual://movie/tt-empty?result=cand-1")
	if !errors.Is(err, ErrVirtualProbeNoTracks) {
		t.Fatalf("EnumerateVirtualSourceTracks error = %v, want ErrVirtualProbeNoTracks", err)
	}
}

// The enumeration argv must keep the full probe's analysis bounds but omit the
// chapter table, so it never triggers the packet-scan duration fallback that
// makes the full probe multi-second on a remote relay.
func TestEnumerateStreamsArgsOmitsChaptersAndKeepsBounds(t *testing.T) {
	args := enumerateStreamsArgs("virtual://movie/tt1")
	for _, arg := range args {
		if arg == "-show_chapters" {
			t.Fatalf("enumerateStreamsArgs must not request chapters: %v", args)
		}
	}
	for flag, want := range map[string]string{
		"-probesize":       "32M",
		"-analyzeduration": "10M",
	} {
		got, ok := probeArgValue(args, flag)
		if !ok || got != want {
			t.Fatalf("%s = %q (present=%v), want %q", flag, got, ok, want)
		}
	}
	if args[len(args)-1] != "virtual://movie/tt1" {
		t.Fatalf("input path must be last: %v", args)
	}
}
