package playback

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// manifestGapPrefixSeconds sums the EXTINF durations of the EXT-X-GAP entries
// that carry the omitted prefix of a windowed synthetic manifest. It is the
// media time at which the first real segment must land.
func manifestGapPrefixSeconds(t *testing.T, manifest string) float64 {
	t.Helper()
	total := 0.0
	pendingGap := false
	for _, line := range strings.Split(manifest, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "#EXT-X-GAP":
			pendingGap = true
		case strings.HasPrefix(line, "#EXTINF:"):
			value := strings.TrimSuffix(strings.TrimPrefix(line, "#EXTINF:"), ",")
			duration, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err != nil {
				t.Fatalf("parse EXTINF %q: %v", line, err)
			}
			if pendingGap {
				total += duration
			}
		case line == "" || strings.HasPrefix(line, "#"):
			// Non-segment tag: leave the pending gap state untouched so the
			// EXTINF that follows EXT-X-GAP is still attributed to the gap.
		default:
			pendingGap = false
		}
	}
	return total
}

func TestGenerateWindowedManifestCarriesAbsoluteTimelineWithGapPrefix(t *testing.T) {
	session := &TranscodeSession{opts: TranscodeOpts{
		TargetCodecVideo: "h264",
		TargetCodecAudio: "aac",
		SegmentDuration:  2,
		TotalDuration:    3600, // 1800 two-second segments
	}}

	got := string(session.GenerateWindowedManifest("segment/", "token=test", 100, 140))

	for _, want := range []string{
		"#EXT-X-PLAYLIST-TYPE:VOD",
		"#EXT-X-MEDIA-SEQUENCE:0",
		"#EXT-X-ENDLIST",
		"#EXT-X-GAP",
		"segment/seg_00100.ts?token=test",
		"segment/seg_00139.ts?token=test",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("windowed manifest missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "seg_00099.ts") {
		t.Fatalf("windowed manifest listed a segment before the window:\n%s", got)
	}
	if strings.Contains(got, "seg_00140.ts") {
		t.Fatalf("windowed manifest listed a segment after the window:\n%s", got)
	}
	if prefix := manifestGapPrefixSeconds(t, got); prefix != 200 {
		t.Fatalf("gap prefix = %v s, want 200 (windowStart * segmentDuration)", prefix)
	}
	if len(got) >= 16*1024 {
		t.Fatalf("windowed manifest is %d bytes, want a bounded payload", len(got))
	}
}

func TestGenerateWindowedManifestBoundsGapEntriesForALongTitle(t *testing.T) {
	session := &TranscodeSession{opts: TranscodeOpts{
		TargetCodecVideo: "h264",
		SegmentDuration:  2,
		TotalDuration:    36000, // 18000 two-second segments
	}}

	// A window three hours into the title: the omitted prefix alone would be
	// 34,000 s, so the gap run must be aggregated rather than one entry per
	// omitted segment.
	got := string(session.GenerateWindowedManifest("segment/", "", 17000, 17045))

	gaps := strings.Count(got, "#EXT-X-GAP")
	if gaps == 0 {
		t.Fatalf("windowed manifest has no gap prefix:\n%s", got)
	}
	if gaps > syntheticManifestMaxGapEntries {
		t.Fatalf("gap entries = %d, want at most %d", gaps, syntheticManifestMaxGapEntries)
	}
	if prefix := manifestGapPrefixSeconds(t, got); prefix != 17000*2 {
		t.Fatalf("gap prefix = %v s, want %d", prefix, 17000*2)
	}
	if len(got) > 24*1024 {
		t.Fatalf("windowed manifest is %d bytes, want a bounded payload", len(got))
	}
}

func TestGenerateWindowedManifestAdvertisesAMaxDurationsCoveringTheLongestEntry(t *testing.T) {
	session := &TranscodeSession{opts: TranscodeOpts{
		TargetCodecVideo: "h264",
		SegmentDuration:  2,
		TotalDuration:    72000,
	}}
	got := string(session.GenerateWindowedManifest("segment/", "", 35000, 35045))

	target := 0
	longest := 0.0
	for _, line := range strings.Split(got, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#EXT-X-TARGETDURATION:") {
			value, err := strconv.Atoi(strings.TrimPrefix(line, "#EXT-X-TARGETDURATION:"))
			if err != nil {
				t.Fatalf("parse target duration %q: %v", line, err)
			}
			target = value
		}
		if strings.HasPrefix(line, "#EXTINF:") {
			value := strings.TrimSuffix(strings.TrimPrefix(line, "#EXTINF:"), ",")
			duration, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err != nil {
				t.Fatalf("parse EXTINF %q: %v", line, err)
			}
			if duration > longest {
				longest = duration
			}
		}
	}
	if target <= 0 {
		t.Fatalf("windowed manifest has no target duration:\n%s", got)
	}
	if float64(target) < longest {
		t.Fatalf("target duration %d does not cover the longest EXTINF %.3f", target, longest)
	}
}

func TestBuildPlaybackManifestWindowsALongEncodedTranscode(t *testing.T) {
	outputDir := t.TempDir()
	var manifest strings.Builder
	manifest.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:3\n#EXT-X-MEDIA-SEQUENCE:0\n")
	for i := range 4 {
		name := segmentFilename(i, TranscodeOpts{TargetCodecVideo: "h264", SegmentDuration: 2})
		if err := os.WriteFile(filepath.Join(outputDir, name), []byte("segment"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		manifest.WriteString("#EXTINF:2.000000,\n")
		manifest.WriteString(name)
		manifest.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(outputDir, "stream.m3u8"), []byte(manifest.String()), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	session := &TranscodeSession{
		outputDir: outputDir,
		opts: TranscodeOpts{
			TargetCodecVideo: "h264",
			TargetCodecAudio: "aac",
			SegmentDuration:  2,
			TotalDuration:    3600,
		},
	}
	// The client has buffered through segment 500; its window is anchored there.
	session.lastRequestedSegment = 500

	got, err := session.BuildPlaybackManifest("segment/", "token=test&hls_window=1")
	if err != nil {
		t.Fatalf("BuildPlaybackManifest: %v", err)
	}
	text := string(got)

	for _, want := range []string{
		"#EXT-X-PLAYLIST-TYPE:VOD",
		"#EXT-X-MEDIA-SEQUENCE:0",
		"#EXT-X-GAP",
		"#EXT-X-DEFINE:NAME=\"vio_query\"",
		"segment/seg_00410.ts", // 500 - backSegments(90)
		"segment/seg_00544.ts", // 500 + aheadSegments(45) - 1
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("windowed manifest missing %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"seg_00000.ts", "seg_00409.ts", "seg_00545.ts"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("windowed manifest unexpectedly listed %s:\n%s", unwanted, text)
		}
	}
	// The window must be far smaller than the whole-title manifest it replaces.
	full := session.GenerateFullManifest("segment/", "token=test")
	if len(got) >= len(full)/4 {
		t.Fatalf("windowed manifest %d bytes is not markedly smaller than the full %d bytes", len(got), len(full))
	}
}

func TestBuildPlaybackManifestKeepsTheFullManifestWhileItFitsTheWindow(t *testing.T) {
	session := &TranscodeSession{opts: TranscodeOpts{
		TargetCodecVideo: "h264",
		SegmentDuration:  2,
		TotalDuration:    float64(syntheticManifestWindowCapacity) * 2,
	}}

	got, err := session.BuildPlaybackManifest("segment/", "hls_window=1")
	if err != nil {
		t.Fatalf("BuildPlaybackManifest: %v", err)
	}
	if strings.Contains(string(got), "#EXT-X-GAP") {
		t.Fatalf("a title that fits the window capacity must stay a full manifest:\n%s", got)
	}
}

func TestBuildPlaybackManifestLeavesOtherClientsOnTheFullManifest(t *testing.T) {
	session := &TranscodeSession{opts: TranscodeOpts{
		TargetCodecVideo: "h264",
		SegmentDuration:  2,
		TotalDuration:    3600, // 1800 segments, well past the window capacity
	}}

	// A client that has not opted into the windowed manifest must keep
	// receiving the whole-title VOD playlist: it does not reload the playlist
	// at a window edge or reanchor a seek outside one, so a bounded playlist
	// would stop its playback after the first window.
	got, err := session.BuildPlaybackManifest("segment/", "token=test")
	if err != nil {
		t.Fatalf("BuildPlaybackManifest: %v", err)
	}
	text := string(got)
	if strings.Contains(text, "#EXT-X-GAP") {
		t.Fatalf("an opted-out client must not receive a gap prefix:\n%s", text)
	}
	if !strings.Contains(text, "segment/seg_00000.ts") ||
		!strings.Contains(text, "segment/seg_01799.ts") {
		t.Fatalf("an opted-out client must receive the whole-title manifest:\n%s", text)
	}
}

func TestSyntheticManifestWindowClampsToTheTitleAndStartSegment(t *testing.T) {
	tests := []struct {
		name     string
		segCount int
		progress SegmentProgress
		wantFrom int
		wantTo   int
	}{
		{
			name:     "fresh play starts at zero",
			segCount: 1000,
			progress: SegmentProgress{ProducedHead: -1, StartSegmentNumber: 0, LastRequestedSegment: 0},
			wantFrom: 0,
			wantTo:   syntheticManifestWindowAheadSegments,
		},
		{
			name:     "window follows the client buffer",
			segCount: 1000,
			progress: SegmentProgress{ProducedHead: 400, StartSegmentNumber: 0, LastRequestedSegment: 500},
			wantFrom: 410,
			wantTo:   545,
		},
		{
			name:     "window never runs past the title",
			segCount: 500,
			progress: SegmentProgress{ProducedHead: 499, StartSegmentNumber: 0, LastRequestedSegment: 499},
			wantFrom: 409,
			wantTo:   500,
		},
		{
			name:     "resume anchors on the start segment before any download",
			segCount: 1000,
			progress: SegmentProgress{ProducedHead: -1, StartSegmentNumber: 500, LastRequestedSegment: 500},
			wantFrom: 410,
			wantTo:   545,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			from, to := syntheticManifestWindow(tt.segCount, tt.progress)
			if from != tt.wantFrom || to != tt.wantTo {
				t.Fatalf("syntheticManifestWindow() = (%d, %d), want (%d, %d)", from, to, tt.wantFrom, tt.wantTo)
			}
		})
	}
}
