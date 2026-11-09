package playback

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/playback/keyframes"
)

func stubKeyframeIndex(t *testing.T, idx keyframes.Index, err error) {
	t.Helper()
	previous := loadKeyframeIndex
	loadKeyframeIndex = func(string) (keyframes.Index, error) { return idx, err }
	t.Cleanup(func() { loadKeyframeIndex = previous })
}

func stubHTTPKeyframeIndex(t *testing.T, idx keyframes.Index, err error) {
	t.Helper()
	previous := loadHTTPKeyframeIndex
	loadHTTPKeyframeIndex = func(context.Context, *http.Client, string) (keyframes.Index, error) { return idx, err }
	t.Cleanup(func() { loadHTTPKeyframeIndex = previous })
}

func plannedOpts() TranscodeOpts {
	return TranscodeOpts{
		InputPath:        "/media/episode.mkv",
		SessionID:        "planned",
		TargetCodecVideo: "copy",
		TargetCodecAudio: "copy",
		SegmentDuration:  2,
		TotalDuration:    10,
		KeyframePlaylist: true,
	}
}

func TestPlanCopySegments(t *testing.T) {
	// The container runs to 12s, but FFmpeg's last segment ends with the
	// video at 10s.
	stubKeyframeIndex(t, keyframes.Index{Keyframes: []float64{0, 1, 2.6, 7.0, 7.4, 9.0}, Duration: 12, VideoEnd: 10}, nil)

	plan, err := planCopySegments(t.Context(), plannedOpts())
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil {
		t.Fatal("no plan for an eligible session")
	}
	// Starts 0, 2.6, 7.0, 7.4 (catching up after the long gap), 9.0; the
	// last segment runs to the video's end.
	want := []float64{2.6, 4.4, 0.4, 1.6, 1.0}
	if len(plan.durations) != len(want) {
		t.Fatalf("durations = %v, want %v", plan.durations, want)
	}
	for i := range want {
		if math.Abs(plan.durations[i]-want[i]) > 1e-9 {
			t.Fatalf("durations = %v, want %v", plan.durations, want)
		}
	}
}

// A source whose timestamps start later than zero plans the same segments.
func TestPlanCopySegmentsForALaterStart(t *testing.T) {
	stubKeyframeIndex(t, keyframes.Index{Keyframes: []float64{7, 8, 9.6, 14.0, 14.4, 16.0}, VideoEnd: 17}, nil)
	plan, err := planCopySegments(t.Context(), plannedOpts())
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil {
		t.Fatal("no plan for a source starting at 7s")
	}
	if plan.origin != 7 {
		t.Fatalf("origin = %v, want the first keyframe at 7s", plan.origin)
	}
	want := []float64{2.6, 4.4, 0.4, 1.6, 1.0}
	for i := range want {
		if math.Abs(plan.durations[i]-want[i]) > 1e-9 {
			t.Fatalf("durations = %v, want %v", plan.durations, want)
		}
	}
}

func TestPlanCopySegmentsKeepsFFmpegPlaylist(t *testing.T) {
	stubKeyframeIndex(t, keyframes.Index{Keyframes: []float64{0, 2, 4, 6, 8}, VideoEnd: 10}, nil)
	for name, change := range map[string]func(*TranscodeOpts){
		"setting off":      func(o *TranscodeOpts) { o.KeyframePlaylist = false },
		"encoded video":    func(o *TranscodeOpts) { o.TargetCodecVideo = "h264" },
		"unknown duration": func(o *TranscodeOpts) { o.TotalDuration = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			opts := plannedOpts()
			change(&opts)
			if plan, _ := planCopySegments(t.Context(), opts); plan != nil {
				t.Fatalf("plan = %v, want none", plan.durations)
			}
		})
	}

	// A start at a seek or mid-stream still plans the whole file: the served
	// playlist begins at segment zero and FFmpeg starts at the requested
	// segment, so a complete timeline is exactly what makes the seek native.
	for name, change := range map[string]func(*TranscodeOpts){
		"starts at a seek":  func(o *TranscodeOpts) { o.SeekSeconds = 4 },
		"starts mid-stream": func(o *TranscodeOpts) { o.StartSegmentNumber = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			opts := plannedOpts()
			change(&opts)
			plan, err := planCopySegments(t.Context(), opts)
			if err != nil || plan == nil || len(plan.durations) != 5 {
				t.Fatalf("plan = %+v, %v; want the full five-segment plan", plan, err)
			}
		})
	}

	t.Run("unknown video end falls back to the container duration", func(t *testing.T) {
		stubKeyframeIndex(t, keyframes.Index{Keyframes: []float64{0, 2, 4, 6, 8}, Duration: 10}, nil)
		plan, err := planCopySegments(t.Context(), plannedOpts())
		if err != nil || plan == nil || !slices.Equal(plan.durations, []float64{2, 2, 2, 2, 2}) {
			t.Fatalf("plan = %+v, %v; want the container-bounded plan", plan, err)
		}
	})
	t.Run("no index", func(t *testing.T) {
		stubKeyframeIndex(t, keyframes.Index{}, keyframes.ErrNoIndex)
		if plan, _ := planCopySegments(t.Context(), plannedOpts()); plan != nil {
			t.Fatalf("plan = %v, want none", plan.durations)
		}
	})
	t.Run("unreadable source", func(t *testing.T) {
		stubKeyframeIndex(t, keyframes.Index{}, errors.New("permission denied"))
		if plan, _ := planCopySegments(t.Context(), plannedOpts()); plan != nil {
			t.Fatalf("plan = %v, want none", plan.durations)
		}
	})
	t.Run("transient remote read is retryable", func(t *testing.T) {
		stubHTTPKeyframeIndex(t, keyframes.Index{}, fmt.Errorf("%w: relay 503", keyframes.ErrTransient))
		opts := plannedOpts()
		opts.InputPath = "http://127.0.0.1:1234/source/token/stream"
		plan, err := planCopySegments(t.Context(), opts)
		if plan != nil || !errors.Is(err, keyframes.ErrTransient) {
			t.Fatalf("plan = %+v, err = %v; want a retryable transient error", plan, err)
		}
	})
}

// A remote relay source is read through the HTTP range reader, and its quick
// index has no VideoEnd, so the plan is bounded by the container duration.
func TestPlanCopySegmentsReadsRemoteRelay(t *testing.T) {
	stubHTTPKeyframeIndex(t, keyframes.Index{Keyframes: []float64{0, 2.6, 7.0, 9.0}, Duration: 10}, nil)
	opts := plannedOpts()
	opts.InputPath = "http://127.0.0.1:1234/source/token/stream"
	plan, err := planCopySegments(t.Context(), opts)
	if err != nil || plan == nil {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	if !slices.Equal(plan.durations, []float64{2.6, 4.4, 2.0, 1.0}) {
		t.Fatalf("durations = %v, want the keyframe plan bounded by the container", plan.durations)
	}
}

// A session rebuilt from its recipe serves the playlist its player has, even
// if the file's index became available since it started.
func TestResolveCopyPlanFollowsTheRecordedDecision(t *testing.T) {
	opts := plannedOpts()
	opts.OutputDir = t.TempDir()

	stubKeyframeIndex(t, keyframes.Index{}, keyframes.ErrUnverified)
	if plan := resolveCopyPlan(t.Context(), opts); plan != nil {
		t.Fatal("planned while the index is being checked")
	}
	verified := keyframes.Index{Keyframes: []float64{0, 1, 2.6, 7.0, 7.4, 9.0}, VideoEnd: 10}
	stubKeyframeIndex(t, verified, nil)
	if plan := resolveCopyPlan(t.Context(), opts); plan != nil {
		t.Fatal("a rebuilt session switched to a keyframe playlist")
	}

	planned := plannedOpts()
	planned.OutputDir = t.TempDir()
	first := resolveCopyPlan(t.Context(), planned)
	if first == nil {
		t.Fatal("no plan with a verified index")
	}
	// The record stands in for the index, which a cold process may not have
	// verified yet.
	stubKeyframeIndex(t, keyframes.Index{}, keyframes.ErrUnverified)
	again := resolveCopyPlan(t.Context(), planned)
	if again == nil || !slices.Equal(again.durations, first.durations) {
		t.Fatalf("rebuilt plan = %+v, want the recorded %+v", again, first)
	}
}

// A clean shutdown removes the output directory and its plan record. The
// rebuilt session plans again from the verified index, waiting for the check
// a cold process hasn't run yet, and gets the same plan.
func TestResolveCopyPlanReplansAfterTheRecordIsGone(t *testing.T) {
	verified := keyframes.Index{Keyframes: []float64{0, 1, 2.6, 7.0, 7.4, 9.0}, VideoEnd: 10}
	stubKeyframeIndex(t, verified, nil)
	first := plannedOpts()
	first.OutputDir = t.TempDir()
	original := resolveCopyPlan(t.Context(), first)
	if original == nil {
		t.Fatal("no plan with a verified index")
	}

	// A cold process: the cache has no result yet, and the full check gives
	// the same index.
	stubKeyframeIndex(t, keyframes.Index{}, keyframes.ErrUnverified)
	previous := loadVerifiedKeyframeIndex
	verifiedLoads := 0
	loadVerifiedKeyframeIndex = func(context.Context, string) (keyframes.Index, error) { verifiedLoads++; return verified, nil }
	t.Cleanup(func() { loadVerifiedKeyframeIndex = previous })

	rebuilt := plannedOpts()
	rebuilt.OutputDir = t.TempDir()
	rebuilt.Reconstructed = true
	again := resolveCopyPlan(t.Context(), rebuilt)
	if again == nil || !slices.Equal(again.durations, original.durations) || verifiedLoads != 1 {
		t.Fatalf("rebuilt plan = %+v after %d verified loads, want the original %+v", again, verifiedLoads, original)
	}

	// A first start still doesn't wait for the check.
	fresh := plannedOpts()
	fresh.OutputDir = t.TempDir()
	if plan := resolveCopyPlan(t.Context(), fresh); plan != nil || verifiedLoads != 1 {
		t.Fatalf("a first start planned (%v) or waited for the check (%d loads)", plan != nil, verifiedLoads)
	}
}

// A rebuild whose request goes away during the keyframe check records nothing,
// so the next rebuild plans instead of following a fallback.
func TestResolveCopyPlanLeavesNoRecordWhenTheRebuildIsCanceled(t *testing.T) {
	verified := keyframes.Index{Keyframes: []float64{0, 1, 2.6, 7.0, 7.4, 9.0}, VideoEnd: 10}
	stubKeyframeIndex(t, keyframes.Index{}, keyframes.ErrUnverified)
	previous := loadVerifiedKeyframeIndex
	loadVerifiedKeyframeIndex = func(ctx context.Context, _ string) (keyframes.Index, error) {
		if err := ctx.Err(); err != nil {
			return keyframes.Index{}, err
		}
		return verified, nil
	}
	t.Cleanup(func() { loadVerifiedKeyframeIndex = previous })

	rebuilt := plannedOpts()
	rebuilt.OutputDir = t.TempDir()
	rebuilt.Reconstructed = true
	gone, cancel := context.WithCancel(t.Context())
	cancel()
	if plan := resolveCopyPlan(gone, rebuilt); plan != nil {
		t.Fatal("planned without the keyframe check")
	}
	if _, err := os.Stat(filepath.Join(rebuilt.OutputDir, copyPlanRecordFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a canceled rebuild left a plan record: %v", err)
	}
	if plan := resolveCopyPlan(t.Context(), rebuilt); plan == nil {
		t.Fatal("the next rebuild didn't plan")
	}
}

func TestCopySegmentPlanManifest(t *testing.T) {
	plan := &copySegmentPlan{durations: []float64{2.6, 5.2, 0.4}}
	got := string(plan.manifest(plannedOpts(), "segment/", "token=abc"))
	want := "#EXTM3U\n" +
		"#EXT-X-VERSION:7\n" +
		// The 5.2s segment, from a long keyframe gap, sets the target.
		"#EXT-X-TARGETDURATION:5\n" +
		"#EXT-X-MEDIA-SEQUENCE:0\n" +
		"#EXT-X-PLAYLIST-TYPE:VOD\n" +
		"#EXT-X-MAP:URI=\"segment/init.mp4?token=abc\"\n" +
		"#EXTINF:2.600000,\nsegment/seg_00000.m4s?token=abc\n" +
		"#EXTINF:5.200000,\nsegment/seg_00001.m4s?token=abc\n" +
		"#EXTINF:0.400000,\nsegment/seg_00002.m4s?token=abc\n" +
		"#EXT-X-ENDLIST\n"
	if got != want {
		t.Fatalf("manifest:\n%s\nwant:\n%s", got, want)
	}
}

func TestPlannedSessionServesPlanForEveryManifest(t *testing.T) {
	plan := &copySegmentPlan{durations: []float64{2, 2, 1}}
	s := &TranscodeSession{opts: plannedOpts(), copyPlan: plan}
	// The manifest carries the session's generation token on every segment
	// URI (see BuildPlaybackManifest), so the expected plan is scoped the same
	// way.
	scoped := s.generationScopedQuery("")
	want := plan.manifest(s.opts, "", scoped)
	for name, build := range map[string]func(string, string) ([]byte, error){
		"playback":       s.BuildPlaybackManifest,
		"source aligned": s.BuildSourceAlignedPlaybackManifest,
	} {
		got, err := build("", "")
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s manifest = %q, %v; want the plan", name, got, err)
		}
	}
}

func TestPlannedSegmentRecoveryDecision(t *testing.T) {
	s := &TranscodeSession{opts: plannedOpts(), copyPlan: &copySegmentPlan{durations: []float64{2}}, lastRequestedSegment: 3}
	decide := func(segNum int, progress SegmentProgress) SegmentRecoveryDecision {
		return s.plannedSegmentRecoveryDecision(segNum, SegmentRecoveryDecision{RestartOnTimeout: true, Progress: progress})
	}

	// A segment inside the client's read-ahead window is sequential playback,
	// not a seek: wait for it and record the request so the throttler lets
	// FFmpeg reach it.
	near := decide(20+plannedSegmentPrefetchSegments, SegmentProgress{Running: true, ProducedHead: 20})
	if !near.Wait || near.RestartOnTimeout || near.WaitTimeout != plannedSegmentWait {
		t.Fatalf("segment near the head: %+v, want a %s wait without restart", near, plannedSegmentWait)
	}
	if got := s.LastRequestedSegment(); got != 20+plannedSegmentPrefetchSegments {
		t.Fatalf("last requested segment = %d, want %d", got, 20+plannedSegmentPrefetchSegments)
	}

	// A far segment is a native seek: restart the copy process at the plan's
	// boundary now instead of waiting for it to copy the whole prefix.
	far := decide(20+plannedSegmentPrefetchSegments+1, SegmentProgress{Running: true, ProducedHead: 20})
	if far.Wait || !far.RestartOnTimeout || far.Reason != "planned_segment_seek" {
		t.Fatalf("segment far ahead: %+v, want an immediate planned restart", far)
	}

	for name, progress := range map[string]SegmentProgress{
		"pruned behind the head": {Running: true, ProducedHead: 500},
		"FFmpeg stopped":         {Running: false, ProducedHead: 20},
	} {
		got := decide(10, progress)
		if got.Wait || !got.RestartOnTimeout {
			t.Fatalf("%s: %+v, want a restart", name, got)
		}
	}

	restarting := decide(10, SegmentProgress{Restarting: true})
	if !restarting.Wait || restarting.RestartOnTimeout {
		t.Fatalf("restarting: %+v, want a wait", restarting)
	}
}

func TestPlannedSegmentRecoveryTargetUsesThePlanBoundary(t *testing.T) {
	durations := make([]float64, 100)
	for i := range durations {
		durations[i] = 2
	}
	biases := make([]float64, 100)
	for i := range biases {
		biases[i] = 0.5
	}
	s := &TranscodeSession{opts: plannedOpts(), copyPlan: &copySegmentPlan{durations: durations, origin: 1, seekBias: biases}}
	target, ok, err := s.ResolveSegmentRecoveryTarget(context.Background(), 40)
	if err != nil || !ok {
		t.Fatalf("ResolveSegmentRecoveryTarget = %v, %v", ok, err)
	}
	// Segment 40 starts at origin 1 + 40×2s and keeps its playlist number;
	// the client's complete VOD playlist does not change. The seek request
	// carries the bias that lands the demuxer on that boundary keyframe.
	if target.SeekSeconds != 81.5 || target.StreamOriginSeconds != 81 || target.StartSegmentNumber != 40 || !target.CopySeekAnchorResolved {
		t.Fatalf("target = %+v, want segment 40 at 81s (+0.5s seek bias)", target)
	}
}

func TestCopySegmentPlanSeekTarget(t *testing.T) {
	plan := &copySegmentPlan{durations: []float64{2.6, 5.2, 0.4}, origin: 7}
	for name, test := range map[string]struct {
		position float64
		start    float64
		segment  int
	}{
		"before the origin": {position: 1, start: 7, segment: 0},
		"inside segment 0":  {position: 8, start: 7, segment: 0},
		"inside segment 1":  {position: 10, start: 9.6, segment: 1},
		"inside segment 2":  {position: 15, start: 14.8, segment: 2},
		"at the end":        {position: 100, start: 14.8, segment: 2},
	} {
		t.Run(name, func(t *testing.T) {
			start, segment, ok := plan.seekTarget(test.position)
			if !ok || start != test.start || segment != test.segment {
				t.Fatalf("seekTarget(%v) = %v, %d, %v; want %v, %d", test.position, start, segment, ok, test.start, test.segment)
			}
		})
	}
	if _, _, ok := (*copySegmentPlan)(nil).seekTarget(1); ok {
		t.Fatal("a nil plan has a seek target")
	}
}

func TestCopyVODPlanReportsTheCompleteTimeline(t *testing.T) {
	s := &TranscodeSession{copyPlan: &copySegmentPlan{durations: []float64{2.6, 5.2, 0.4}, origin: 7}}
	vod, ok := s.CopyVODPlan()
	if !ok || vod.Origin != 7 || math.Abs(vod.Duration-8.2) > 1e-9 || vod.Segments != 3 {
		t.Fatalf("CopyVODPlan = %+v, %v", vod, ok)
	}
	if _, ok := (&TranscodeSession{}).CopyVODPlan(); ok {
		t.Fatal("an unplanned session reports a VOD plan")
	}
}

// A planned session's FFmpeg must write exactly the segments its playlist
// lists, from the start and after a restart.
func TestPlannedCopySessionMatchesFFmpeg(t *testing.T) {
	if testing.Short() {
		t.Skip("real FFmpeg integration test")
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	dir := t.TempDir()
	source := filepath.Join(dir, "irregular.mkv")
	// A second audio track the session doesn't select runs 20s past the
	// video, so the container's duration does too.
	out, err := exec.CommandContext(ctx, ffmpeg, "-v", "error",
		"-f", "lavfi", "-i", "testsrc2=size=96x54:rate=25:duration=40",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=40",
		"-f", "lavfi", "-i", "sine=frequency=660:sample_rate=48000:duration=60",
		"-map", "0", "-map", "1", "-map", "2",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "1000", "-bf", "2",
		"-sc_threshold", "0", "-force_key_frames", "0,0.9,2.2,2.6,5.2,7.2,7.6,8.9,12.6,13,15,17.6,18.5,25,26.1,31,33.3,38",
		"-c:a", "aac", source).CombinedOutput()
	if err != nil {
		t.Skipf("ffmpeg can't make the test file (%v): %s", err, out)
	}

	// Planned sessions use a fully checked index; check it now rather than
	// in the background.
	if _, err := keyframes.LoadVerified(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	opts := TranscodeOpts{
		InputPath:        source,
		OutputDir:        filepath.Join(dir, "out"),
		SessionID:        "planned-integration",
		SourceVideoCodec: "h264",
		TargetCodecVideo: "copy",
		TargetCodecAudio: "copy",
		SegmentDuration:  2,
		TotalDuration:    60,
		KeyframePlaylist: true,
		FFmpegPath:       ffmpeg,
		HWAccel:          HWAccelNone,
	}
	s, err := StartTranscode(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if s.copyPlan == nil {
		t.Fatal("no plan for a Matroska source with Cues")
	}
	manifest, err := s.BuildPlaybackManifest("", "")
	if err != nil {
		t.Fatal(err)
	}
	// The plan is served as complete VOD: every segment boundary is known
	// before FFmpeg produces anything, so the client can seek by requesting
	// the target segment instead of following a growing live edge.
	if !bytes.Contains(manifest, []byte("#EXT-X-PLAYLIST-TYPE:VOD")) ||
		!bytes.Contains(manifest, []byte("#EXT-X-ENDLIST")) {
		t.Fatalf("a planned playlist must be complete VOD for native seeking:\n%s", manifest)
	}
	planned := extinfDurations(t, manifest)
	if len(planned) != len(s.copyPlan.durations) {
		t.Fatalf("playlist lists %d segments, plan has %d", len(planned), len(s.copyPlan.durations))
	}

	// FFmpeg must write exactly the segments the playlist lists, so a client
	// that asks for segment N gets the planned N. from is the plan's first
	// segment after a seek restart.
	checkWritten := func(label string, from int) {
		t.Helper()
		select {
		case <-s.Done():
		case <-ctx.Done():
			t.Fatalf("%s: FFmpeg didn't finish", label)
		}
		written, err := os.ReadFile(filepath.Join(opts.OutputDir, "stream.m3u8"))
		if err != nil {
			t.Fatal(err)
		}
		got := extinfDurations(t, written)
		if len(got) != len(planned)-from {
			t.Fatalf("%s: FFmpeg wrote %d segments %v, playlist lists %d from segment %d", label, len(got), got, len(planned)-from, from)
		}
		for i := range got {
			tolerance := 0.002
			if from+i == len(planned)-1 {
				// FFmpeg measures the last segment to a late packet,
				// within a frame of the video's end.
				tolerance = 0.041
			}
			if math.Abs(got[i]-planned[from+i]) > tolerance {
				t.Fatalf("%s: segment %d is %.3fs, playlist says %.3fs", label, from+i, got[i], planned[from+i])
			}
		}
		// Per-segment tolerances could hide drift that adds up.
		var wrote, listed float64
		for i := range got {
			wrote += got[i]
			listed += planned[from+i]
		}
		if math.Abs(wrote-listed) > 0.041 {
			t.Fatalf("%s: FFmpeg wrote %.3fs in all, the playlist lists %.3fs", label, wrote, listed)
		}
	}
	checkWritten("from the start", 0)

	// A seek to segment 9 is a planned boundary: FFmpeg restarts there under
	// segment 9's own number, while the VOD playlist the player holds keeps
	// listing every segment.
	if _, ok, err := s.RestartSegment(ctx, 9); err != nil || !ok {
		t.Fatalf("RestartSegment = %v, %v", ok, err)
	}
	wantSeek, ok := s.copyPlan.segmentStart(9)
	if !ok {
		t.Fatal("plan has no segment 9")
	}
	wantRequest, ok := s.copyPlan.segmentSeekPosition(9)
	if !ok || wantRequest <= wantSeek {
		t.Fatalf("segment 9 seek position = %v, want a request past the boundary %v", wantRequest, wantSeek)
	}
	if o := s.Opts(); math.Abs(o.SeekSeconds-wantRequest) > 1e-9 || o.StartSegmentNumber != 9 || math.Abs(o.StreamOriginSeconds-wantSeek) > 1e-9 {
		t.Fatalf("restart request %.3f from origin %.3f at segment %d, want %.3f from %.3f at segment 9", o.SeekSeconds, o.StreamOriginSeconds, o.StartSegmentNumber, wantRequest, wantSeek)
	}
	if got := s.LastRequestedSegment(); got != 9 {
		t.Fatalf("last requested segment after restart = %d, want 9", got)
	}
	checkWritten("after a seek restart", 9)
}

func extinfDurations(t *testing.T, playlist []byte) []float64 {
	t.Helper()
	var durations []float64
	scanner := bufio.NewScanner(bytes.NewReader(playlist))
	for scanner.Scan() {
		if v, ok := strings.CutPrefix(scanner.Text(), "#EXTINF:"); ok {
			d, err := strconv.ParseFloat(strings.TrimSuffix(v, ","), 64)
			if err != nil {
				t.Fatal(err)
			}
			durations = append(durations, d)
		}
	}
	return durations
}

// A planned boundary's seek request moves past the boundary keyframe but never
// reaches the next one, so FFmpeg lands on the boundary and cuts the plan's
// segment there instead of an earlier keyframe.
func TestCopySeekBiasesStayInsideTheBoundaryInterval(t *testing.T) {
	sourceKeyframes := []float64{0, 0.9, 2.2, 2.6, 5.2, 12.6, 12.64, 15}
	starts := []float64{0, 2.2, 5.2, 12.6}
	biases := copySeekBiases(sourceKeyframes, starts)
	want := []float64{0.45, 0.2, 0.5, 0.02}
	for i := range want {
		if math.Abs(biases[i]-want[i]) > 1e-9 {
			t.Fatalf("bias[%d] = %v, want %v", i, biases[i], want[i])
		}
	}
	for i, bias := range biases {
		next := sourceKeyframes[indexOfFirstGreater(sourceKeyframes, starts[i])]
		if starts[i]+bias >= next {
			t.Fatalf("segment %d seek request %.3f reaches the next keyframe %.3f", i, starts[i]+bias, next)
		}
	}
}

func indexOfFirstGreater(values []float64, pivot float64) int {
	for i, v := range values {
		if v > pivot+1e-6 {
			return i
		}
	}
	return len(values) - 1
}
