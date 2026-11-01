package playback

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestStreamExtractSubtitleBoundsWindow(t *testing.T) {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is required to verify subtitle window timestamps")
	}
	source := filepath.Join(t.TempDir(), "captions.srt")
	const input = "1\n00:00:00,000 --> 00:00:05,000\nBefore window\n\n2\n00:13:20,000 --> 00:13:25,000\nInside window\n\n3\n00:16:40,000 --> 00:16:45,000\nAfter window\n"
	if err := os.WriteFile(source, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := StreamExtractSubtitle(t.Context(), StreamExtractOpts{
		InputPath: source, SourceCodec: "subrip", SeekSeconds: 800, DurationSeconds: 100,
		FFmpegPath: bin, Writer: &output,
	}); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.Contains(got, "Inside window") ||
		!strings.Contains(got, "13:20.000 --> 13:25.000") ||
		strings.Contains(got, "Before window") || strings.Contains(got, "After window") {
		t.Fatalf("expected only the in-window cue at its source timestamp, got %q", got)
	}
}

// The playback timeline starts at the container start. A window cut after a
// seek must put each cue where the unseeked extract does, even when the
// container's first timestamp is not zero (TS remuxes), or subtitles run late
// by that start time after every seek.
func TestStreamExtractSubtitleSeekKeepsContainerRelativeTimeline(t *testing.T) {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is required to verify subtitle window timestamps")
	}
	dir := t.TempDir()
	srt := filepath.Join(dir, "captions.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:40,000 --> 00:00:42,000\nForty seconds in\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "offset.mkv")
	mux := exec.CommandContext(t.Context(), bin, "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=black:s=64x64:r=5:d=60", "-i", srt,
		"-map", "0:v", "-map", "1:s", "-c:v", "mpeg4", "-c:s", "srt",
		"-output_ts_offset", "30", source)
	if out, err := mux.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v: %s", err, out)
	}

	extract := func(seek float64) string {
		t.Helper()
		var output bytes.Buffer
		if err := StreamExtractSubtitle(t.Context(), StreamExtractOpts{
			InputPath: source, SourceCodec: "subrip", SeekSeconds: seek, DurationSeconds: 600,
			FFmpegPath: bin, Writer: &output,
		}); err != nil {
			t.Fatal(err)
		}
		return output.String()
	}
	const cue = "00:40.000 --> 00:42.000"
	if got := extract(0); !strings.Contains(got, cue) {
		t.Fatalf("unseeked extract should place the cue at %s, got %q", cue, got)
	}
	if got := extract(35); !strings.Contains(got, cue) {
		t.Fatalf("seeked window should place the cue at %s like the unseeked extract, got %q", cue, got)
	}
}

type failingSubtitleWriter struct{ err error }

func (w failingSubtitleWriter) Write([]byte) (int, error) { return 0, w.err }

func TestStreamExtractSubtitleStopsOnWriterFailure(t *testing.T) {
	// Keep the extractor producing output after the response writer fails.
	// Waiting for it without canceling first leaves it blocked on a full pipe.
	bin := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec cat /dev/zero\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	writeErr := errors.New("response writer disconnected")
	err := StreamExtractSubtitle(ctx, StreamExtractOpts{
		InputPath: "unused.mkv", FFmpegPath: bin, Writer: failingSubtitleWriter{writeErr},
	})
	if !errors.Is(err, writeErr) {
		t.Fatalf("extract error = %v, want writer error %v", err, writeErr)
	}
	if ctx.Err() != nil {
		t.Fatalf("extract required request timeout to stop: %v", ctx.Err())
	}
}

func TestStreamExtractOutput(t *testing.T) {
	cases := []struct {
		codec      string
		target     string
		wantCodec  string
		wantFormat string
	}{
		{"ass", "", "copy", "ass"},
		{"ssa", "", "copy", "ass"},
		{"pgs", "", "copy", "sup"},
		{"hdmv_pgs_subtitle", "", "copy", "sup"},
		{"pgssub", "", "copy", "sup"},
		{"subrip", "", "webvtt", "webvtt"},
		{"mov_text", "", "webvtt", "webvtt"},
		// A forced "vtt" target applies only to text sources: bitmap codecs carry no
		// text for ffmpeg's webvtt encoder, so the override must fall back to the
		// source-driven mapping instead of building a command that always fails.
		{"subrip", "vtt", "webvtt", "webvtt"},
		{"mov_text", "vtt", "webvtt", "webvtt"},
		{"ass", "vtt", "webvtt", "webvtt"},
		{"pgs", "vtt", "copy", "sup"},
		{"hdmv_pgs_subtitle", "vtt", "copy", "sup"},
	}
	for _, tc := range cases {
		var outCodec, outFormat string
		if tc.target == "" {
			outCodec, outFormat = streamExtractOutput(tc.codec)
		} else {
			outCodec, outFormat = streamExtractOutput(tc.codec, tc.target)
		}
		if outCodec != tc.wantCodec || outFormat != tc.wantFormat {
			t.Errorf("streamExtractOutput(%q, %q) = (%q, %q), want (%q, %q)",
				tc.codec, tc.target, outCodec, outFormat, tc.wantCodec, tc.wantFormat)
		}
	}
}

func TestStreamExtractArgs_TextCodecIsWindowed(t *testing.T) {
	args := streamExtractArgs(StreamExtractOpts{
		InputPath:       "/media/movie.mkv",
		TrackIndex:      2,
		SourceCodec:     "subrip",
		SeekSeconds:     120,
		DurationSeconds: 600,
	})

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-ss 120.000") {
		t.Fatalf("text extract should seek the input: %s", joined)
	}
	if !strings.Contains(joined, "-to 720.000") {
		t.Fatalf("text extract should cap the read duration: %s", joined)
	}
	if !strings.Contains(joined, "-copyts") {
		t.Fatalf("seeked extract must preserve source timestamps: %s", joined)
	}
	if !strings.Contains(joined, "-c:s webvtt") || !strings.Contains(joined, "-f webvtt") {
		t.Fatalf("text extract should transmux to WebVTT: %s", joined)
	}
}

// PGS streams are fetched once and consumed whole by their client-side
// renderers, so by default seek/duration windowing must never apply even when
// the handler passes nonzero values. (ASS is windowed only when the caller
// supplies a position; see TestStreamExtractArgs_WindowedASS.)
func TestStreamExtractArgs_WholeTrackPGSIgnoresWindow(t *testing.T) {
	codec := "hdmv_pgs_subtitle"
	args := streamExtractArgs(StreamExtractOpts{
		InputPath:       "/media/movie.mkv",
		TrackIndex:      0,
		SourceCodec:     codec,
		SeekSeconds:     120,
		DurationSeconds: 600,
	})

	if slices.Contains(args, "-ss") {
		t.Errorf("%s extract must not seek the input: %v", codec, args)
	}
	if slices.Contains(args, "-to") {
		t.Errorf("%s extract must not cap the read duration: %v", codec, args)
	}
	if !slices.Contains(args, "copy") {
		t.Errorf("%s extract must copy the source stream: %v", codec, args)
	}
}

// A caller-supplied position makes ASS windowable: the input is seeked with
// -ss, the output capped with -to at the absolute window end, and -copyts
// keeps events on the source timeline. The ASS muxer re-emits the script
// header (styles) from the container extradata on every extraction, so each
// window is a self-contained script and the client's JASSUB timeOffset stays
// correct.
func TestStreamExtractArgs_WindowedASS(t *testing.T) {
	args := streamExtractArgs(StreamExtractOpts{
		InputPath:       "/media/movie.mkv",
		TrackIndex:      1,
		SourceCodec:     "ass",
		SeekSeconds:     120,
		DurationSeconds: 600,
	})

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-ss 120.000") {
		t.Fatalf("windowed ASS extract should seek the input: %s", joined)
	}
	ssIdx := slices.Index(args, "-ss")
	inIdx := slices.Index(args, "-i")
	if ssIdx < 0 || inIdx < 0 || ssIdx > inIdx {
		t.Fatalf("-ss must be an input option (before -i): %s", joined)
	}
	if !strings.Contains(joined, "-to 720.000") {
		t.Fatalf("windowed ASS extract should cap to the absolute window end: %s", joined)
	}
	if !strings.Contains(joined, "-copyts") || !strings.Contains(joined, "-avoid_negative_ts disabled") {
		t.Fatalf("windowed ASS extract must preserve absolute source timestamps: %s", joined)
	}
	if !strings.Contains(joined, "-c:s copy") || !strings.Contains(joined, "-f ass pipe:1") {
		t.Fatalf("windowed ASS extract should still copy the ass stream: %s", joined)
	}
}

// Absent a position, ASS keeps today's whole-track behavior byte-for-byte,
// so native clients that fetch the complete script once are unaffected even
// when a duration is present.
func TestStreamExtractArgs_ASSWithoutPositionIgnoresWindow(t *testing.T) {
	args := streamExtractArgs(StreamExtractOpts{
		InputPath:       "/media/movie.mkv",
		TrackIndex:      0,
		SourceCodec:     "ass",
		DurationSeconds: 600,
	})

	if slices.Contains(args, "-ss") || slices.Contains(args, "-to") {
		t.Fatalf("duration-only ASS extract must not window: %v", args)
	}
	baseline := streamExtractArgs(StreamExtractOpts{
		InputPath:   "/media/movie.mkv",
		TrackIndex:  0,
		SourceCodec: "ass",
	})
	if !slices.Equal(args, baseline) {
		t.Fatalf("no-window ASS args changed:\n got %v\nwant %v", args, baseline)
	}
}

// Explicit position=0 is a valid window start, not a whole-track fetch.
// streamExtractArgs must emit `-ss 0`, `-copyts`, and `-to <duration>` so a
// client that opens at position 0 with a duration gets bounded output on the
// source timeline instead of a full-file demux.
func TestStreamExtractArgs_ExplicitZeroWindowRequested(t *testing.T) {
	for _, codec := range []string{"ass", "subrip"} {
		args := streamExtractArgs(StreamExtractOpts{
			InputPath:       "/media/movie.mkv",
			TrackIndex:      0,
			SourceCodec:     codec,
			WindowRequested: true,
			SeekSeconds:     0,
			DurationSeconds: 600,
		})
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "-ss 0.000") {
			t.Fatalf("%s explicit-zero window must seek to 0: %s", codec, joined)
		}
		if !strings.Contains(joined, "-to 600.000") {
			t.Fatalf("%s explicit-zero window must cap at the window end: %s", codec, joined)
		}
		if !strings.Contains(joined, "-copyts") {
			t.Fatalf("%s explicit-zero window must preserve absolute timestamps: %s", codec, joined)
		}
	}
}

// WindowRequested is the gate for ASS, not DurationSeconds: a legacy direct
// caller that supplies only a duration (no window intent) keeps whole-track
// output.
func TestStreamExtractArgs_ASSWithoutWindowIntentIgnoresDuration(t *testing.T) {
	args := streamExtractArgs(StreamExtractOpts{
		InputPath:       "/media/movie.mkv",
		SourceCodec:     "ass",
		DurationSeconds: 600,
	})
	if slices.Contains(args, "-ss") || slices.Contains(args, "-to") {
		t.Fatalf("window intent absent: ASS must stay whole-track: %v", args)
	}
}

// Explicit position=0 must produce a genuinely bounded extract, not a
// whole-track demux: absolute cue timestamps and the ASS script header must
// survive, and the output must be smaller than the whole script because the
// late cue is outside the window. Argument assertions alone cannot prove the
// bound, so this drives the real ffmpeg binary.
//
//nolint:misspell // the ASS event keyword is spelled "Dialogue:".
func TestStreamExtractSubtitleExplicitZeroWindowBounded(t *testing.T) {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is required to verify explicit-zero windowing")
	}
	source := filepath.Join(t.TempDir(), "captions.ass")
	const fixture = `[Script Info]
Title: Window test
ScriptType: v4.00+
PlayResX: 384
PlayResY: 288

[V4+ Styles]
Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding
Style: Default,Arial,16,&H00FFFFFF,&H000000FF,&H00000000,&H00000000,0,0,0,0,100,100,0,0,1,2,2,2,10,10,10,1

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
Dialogue: 0,0:00:01.00,0:00:03.00,Default,,0,0,0,,Early cue
Dialogue: 0,0:11:00.00,0:11:02.00,Default,,0,0,0,,Late cue
`
	if err := os.WriteFile(source, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}

	extract := func(windowed bool) string {
		t.Helper()
		var output bytes.Buffer
		opts := StreamExtractOpts{
			InputPath:       source,
			SourceCodec:     "ass",
			WindowRequested: windowed,
			DurationSeconds: 600,
			FFmpegPath:      bin,
			Writer:          &output,
		}
		if err := StreamExtractSubtitle(t.Context(), opts); err != nil {
			t.Fatal(err)
		}
		return output.String()
	}

	whole := extract(false)
	windowed := extract(true)

	if !strings.Contains(whole, "Late cue") {
		t.Fatalf("whole-track extract must include the late cue: %q", whole)
	}
	if strings.Contains(windowed, "Late cue") {
		t.Fatalf("explicit-zero window must exclude the cue at 11:00: %q", windowed)
	}
	if !strings.Contains(windowed, "Early cue") {
		t.Fatalf("explicit-zero window must retain the in-window cue: %q", windowed)
	}
	if !strings.Contains(windowed, "0:00:01.00,0:00:03.00") {
		t.Fatalf("window must preserve absolute source timestamps, got %q", windowed)
	}
	if !strings.Contains(windowed, "[Script Info]") || !strings.Contains(windowed, "[V4+ Styles]") {
		t.Fatalf("window must retain the ASS script header/styles, got %q", windowed)
	}
	if len(windowed) >= len(whole) {
		t.Fatalf("explicit-zero window output (%d bytes) must be smaller than the whole script (%d bytes)",
			len(windowed), len(whole))
	}
}

// A client that opts in via AllowWindow gets a seeked, duration-capped PGS
// extract with -copyts keeping cues on the playback timeline — the -ss must
// be an input option (before -i) so ffmpeg uses the container index.
func TestStreamExtractArgs_WindowedPGS(t *testing.T) {
	args := streamExtractArgs(StreamExtractOpts{
		InputPath:       "/media/movie.mkv",
		TrackIndex:      1,
		SourceCodec:     "hdmv_pgs_subtitle",
		SeekSeconds:     1200,
		DurationSeconds: 3600,
		AllowWindow:     true,
	})

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-ss 1200.000") {
		t.Fatalf("windowed PGS extract should seek the input: %s", joined)
	}
	ssIdx := slices.Index(args, "-ss")
	inIdx := slices.Index(args, "-i")
	if ssIdx < 0 || inIdx < 0 || ssIdx > inIdx {
		t.Fatalf("-ss must be an input option (before -i): %s", joined)
	}
	if !strings.Contains(joined, "-to 4800.000") {
		t.Fatalf("windowed PGS extract should cap the read duration: %s", joined)
	}
	if !strings.Contains(joined, "-copyts") {
		t.Fatalf("windowed PGS extract must preserve source timestamps: %s", joined)
	}
	if !strings.Contains(joined, "-c:s copy") || !strings.Contains(joined, "-f sup pipe:1") {
		t.Fatalf("windowed PGS extract should still copy into a sup stream: %s", joined)
	}
}

// A windowed extract whose input is a cached full-track .sup must force the
// sup demuxer (the elementary stream has no container header to probe from
// arbitrary offsets), remap to the file's sole stream regardless of the
// original container's track ordinal, and still seek/window with -copyts so
// the cached stream's stored timestamps survive into the output.
func TestStreamExtractArgs_ExtractedSupInput(t *testing.T) {
	args := streamExtractArgs(StreamExtractOpts{
		InputPath:           "/transcode/subtitle-cache/abc-s3-1-2.sup",
		TrackIndex:          3,
		SourceCodec:         "hdmv_pgs_subtitle",
		SeekSeconds:         1200,
		DurationSeconds:     3600,
		AllowWindow:         true,
		InputIsExtractedSup: true,
	})

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-f sup -i /transcode/subtitle-cache/abc-s3-1-2.sup") {
		t.Fatalf("cached sup input must force the sup demuxer before -i: %s", joined)
	}
	if !strings.Contains(joined, "-map 0:s:0") {
		t.Fatalf("cached sup holds exactly one stream; must map 0:s:0: %s", joined)
	}
	if strings.Contains(joined, "0:s:3") {
		t.Fatalf("original container track ordinal must not leak into sup input mapping: %s", joined)
	}
	// The seek starts early so a display set at the window start is read
	// whole; the end is unchanged.
	if !strings.Contains(joined, "-ss 1198.000") || !strings.Contains(joined, "-to 4800.000") {
		t.Fatalf("cached sup extract must still window the input: %s", joined)
	}
	ssIdx := slices.Index(args, "-ss")
	inIdx := slices.Index(args, "-i")
	if ssIdx < 0 || inIdx < 0 || ssIdx > inIdx {
		t.Fatalf("-ss must be an input option (before -i): %s", joined)
	}
	if !strings.Contains(joined, "-copyts") {
		t.Fatalf("cached sup extract must preserve its stored timestamps: %s", joined)
	}
	// The cached .sup is already on the playback timeline and its start time
	// is its first cue; rebasing it would pull every cue early by that much.
	if slices.Contains(args, "-start_at_zero") {
		t.Fatalf("cached sup extract must not rebase its timestamps: %s", joined)
	}
	if !strings.Contains(joined, "-c:s copy") || !strings.Contains(joined, "-f sup pipe:1") {
		t.Fatalf("cached sup extract should copy into a sup stream: %s", joined)
	}
	// ffmpeg offsets an input seek by the input's start time, which for a
	// cached .sup is its first cue; the seek must be absolute instead.
	if tsIdx := slices.Index(args, "-seek_timestamp"); tsIdx < 0 || tsIdx > inIdx || args[tsIdx+1] != "1" {
		t.Fatalf("cached sup seek must be an absolute timestamp (-seek_timestamp 1 before -i): %s", joined)
	}
}

// An unseeked window from a cached .sup keeps its stored timestamps too:
// without -copyts ffmpeg rebases the stream onto its first cue.
func TestStreamExtractArgs_ExtractedSupInputFromStart(t *testing.T) {
	args := streamExtractArgs(StreamExtractOpts{
		InputPath:           "/transcode/subtitle-cache/abc-s3-1-2.sup",
		SourceCodec:         "hdmv_pgs_subtitle",
		DurationSeconds:     600,
		AllowWindow:         true,
		InputIsExtractedSup: true,
	})
	joined := strings.Join(args, " ")
	if !slices.Contains(args, "-copyts") || slices.Contains(args, "-start_at_zero") {
		t.Fatalf("cached sup extract must keep its stored timestamps: %s", joined)
	}
	if slices.Contains(args, "-ss") || slices.Contains(args, "-seek_timestamp") {
		t.Fatalf("an extract from the start must not seek: %s", joined)
	}
	if !strings.Contains(joined, "-to 600.000") {
		t.Fatalf("the window still ends at its duration: %s", joined)
	}
}

// AllowWindow is a PGS-only opt-in: it must not by itself window ASS. ASS
// windows exactly when the caller supplies a position (SeekSeconds > 0).
func TestStreamExtractArgs_ASSWindowDrivenByPositionNotAllowWindow(t *testing.T) {
	args := streamExtractArgs(StreamExtractOpts{
		InputPath:       "/media/movie.mkv",
		TrackIndex:      0,
		SourceCodec:     "ass",
		DurationSeconds: 600,
		AllowWindow:     true,
	})

	if slices.Contains(args, "-ss") {
		t.Errorf("AllowWindow alone must not seek ASS: %v", args)
	}
	if slices.Contains(args, "-to") {
		t.Errorf("AllowWindow alone must not cap ASS: %v", args)
	}
}

// Absent the explicit ?windowed=1 opt-in the request must not window, no
// matter what other params are present — existing clients (Apple, Android,
// jellycompat) send no param and rely on whole-track extraction.
func TestPGSWindowRequest(t *testing.T) {
	cases := []struct {
		name         string
		query        string
		wantAllow    bool
		wantSeek     float64
		wantDuration float64
	}{
		{"no params", "", false, 0, 0},
		{"position without opt-in", "position=120&duration=600", false, 0, 0},
		{"windowed off", "windowed=0&position=120", false, 0, 0},
		{"opt-in with position and duration", "windowed=1&position=120.5&duration=3600", true, 120.5, 3600},
		{"opt-in without position", "windowed=1", true, 0, 0},
		{"opt-in negative position ignored", "windowed=1&position=-5&duration=600", true, 0, 600},
		{"opt-in duration over cap ignored", "windowed=1&position=10&duration=7200", true, 10, 0},
		{"opt-in invalid values ignored", "windowed=1&position=abc&duration=xyz", true, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatalf("ParseQuery(%q): %v", tc.query, err)
			}
			allow, seek, duration := PGSWindowRequest(q)
			if allow != tc.wantAllow || seek != tc.wantSeek || duration != tc.wantDuration {
				t.Errorf("PGSWindowRequest(%q) = (%v, %v, %v), want (%v, %v, %v)",
					tc.query, allow, seek, duration, tc.wantAllow, tc.wantSeek, tc.wantDuration)
			}
		})
	}
}

// A window from a cached .sup that starts at or before the first cue cannot
// be seeked (ffmpeg fails the seek and loses the first presentation
// segment), so it reads from the start and keeps its absolute end.
func TestWindowFromCachedSup(t *testing.T) {
	for _, tc := range []struct {
		name              string
		seek, duration    float64
		wantSeek, wantDur float64
	}{
		{name: "before the first cue", seek: 35, duration: 60, wantSeek: 0, wantDur: 95},
		{name: "onto the first cue", seek: 40, duration: 60, wantSeek: 0, wantDur: 100},
		{name: "after the first cue", seek: 95, duration: 60, wantSeek: 95, wantDur: 60},
		{name: "from the start", seek: 0, duration: 60, wantSeek: 0, wantDur: 60},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := windowFromCachedSup(StreamExtractOpts{SeekSeconds: tc.seek, DurationSeconds: tc.duration}, 40)
			if got.SeekSeconds != tc.wantSeek || got.DurationSeconds != tc.wantDur {
				t.Fatalf("got seek %v duration %v, want %v %v", got.SeekSeconds, got.DurationSeconds, tc.wantSeek, tc.wantDur)
			}
		})
	}
}

// ClampOpenEndedWindow fills in a missing duration for an explicitly-started
// window, so a position-without-duration request cannot extract to EOF, while
// leaving every authoritative case untouched.
func TestClampOpenEndedWindow(t *testing.T) {
	cases := []struct {
		name    string
		opts    StreamExtractOpts
		limit   float64
		wantSet bool
		wantDur float64
	}{
		{
			name:    "text explicit zero position",
			opts:    StreamExtractOpts{SourceCodec: "subrip", WindowRequested: true, SeekSeconds: 0},
			limit:   600,
			wantSet: true,
			wantDur: 600,
		},
		{
			name:    "text nonzero position",
			opts:    StreamExtractOpts{SourceCodec: "subrip", SeekSeconds: 120},
			limit:   600,
			wantSet: true,
			wantDur: 600,
		},
		{
			name:    "ass windowed zero position",
			opts:    StreamExtractOpts{SourceCodec: "ass", WindowRequested: true, SeekSeconds: 0},
			limit:   600,
			wantSet: true,
			wantDur: 600,
		},
		{
			name:    "pgs explicit window opt-in",
			opts:    StreamExtractOpts{SourceCodec: "hdmv_pgs_subtitle", AllowWindow: true, WindowRequested: true, SeekSeconds: 0},
			limit:   600,
			wantSet: true,
			wantDur: 600,
		},
		{
			name:    "explicit duration authoritative",
			opts:    StreamExtractOpts{SourceCodec: "subrip", WindowRequested: true, SeekSeconds: 0, DurationSeconds: 30},
			limit:   600,
			wantSet: false,
			wantDur: 30,
		},
		{
			name:    "whole-track request untouched",
			opts:    StreamExtractOpts{SourceCodec: "subrip"},
			limit:   600,
			wantSet: false,
			wantDur: 0,
		},
		{
			name:    "pgs without opt-in cannot window",
			opts:    StreamExtractOpts{SourceCodec: "hdmv_pgs_subtitle", WindowRequested: true, SeekSeconds: 0},
			limit:   600,
			wantSet: false,
			wantDur: 0,
		},
		{
			name:    "non-positive cap is a no-op",
			opts:    StreamExtractOpts{SourceCodec: "subrip", WindowRequested: true, SeekSeconds: 0},
			limit:   0,
			wantSet: false,
			wantDur: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			if got := opts.ClampOpenEndedWindow(tc.limit); got != tc.wantSet {
				t.Fatalf("ClampOpenEndedWindow = %v, want %v", got, tc.wantSet)
			}
			if opts.DurationSeconds != tc.wantDur {
				t.Fatalf("duration = %v, want %v", opts.DurationSeconds, tc.wantDur)
			}
			// A clamped window must advertise a bounded range and emit -to.
			if tc.wantSet {
				windowed, _, _, open := opts.WindowCoverage()
				if !windowed || open {
					t.Fatalf("clamped window coverage = windowed:%v open:%v, want bounded", windowed, open)
				}
				args := strings.Join(streamExtractArgs(opts), " ")
				if !strings.Contains(args, "-to ") {
					t.Fatalf("clamped window args lack -to: %s", args)
				}
			}
		})
	}
}

func TestIsSubtitleUpstreamError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{
			name: "relay 5xx",
			err:  errors.New("ffmpeg subtitle stream failed: exit status 8 (stderr: [in#0] Error opening input: Server returned 5XX Server Error reply)"),
			want: true,
		},
		{
			name: "wrapped relay 5xx",
			err:  errors.Join(errors.New("extract"), errors.New("Server returned 503 Service Unavailable")),
			want: true,
		},
		{
			name: "map error is not upstream",
			err:  errors.New("ffmpeg subtitle stream failed: Stream map '0:s:9' matches no streams"),
			want: false,
		},
		{
			name: "generic failure is not upstream",
			err:  errors.New("ffmpeg subtitle stream failed: exit status 1"),
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsSubtitleUpstreamError(tc.err); got != tc.want {
				t.Fatalf("IsSubtitleUpstreamError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestReadSupFirstCue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "track.sup")
	if err := os.WriteFile(path, testSupStream([]float64{91.88, 100}, 0), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readSupFirstCue(path)
	if err != nil || got != 91.88 {
		t.Fatalf("readSupFirstCue = %v, %v; want 91.88", got, err)
	}
	if err := os.WriteFile(path, []byte("not a sup stream"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSupFirstCue(path); err == nil {
		t.Fatal("a file without the PG magic should be refused")
	}
}
