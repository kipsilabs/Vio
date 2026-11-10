package playback

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// labeledCounterValue reads one labeled child of a counter vec without
// touching package state, so a test can assert a delta even though the
// process-wide registry accumulates across tests.
func labeledCounterValue(t testing.TB, vec *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	metric := &dto.Metric{}
	if err := vec.WithLabelValues(labels...).(prometheus.Metric).Write(metric); err != nil {
		t.Fatalf("read counter %v: %v", labels, err)
	}
	return metric.GetCounter().GetValue()
}

// delta wraps a counter read: it returns a function that reports how much the
// counter grew since the read.
func delta(t testing.TB, vec *prometheus.CounterVec, labels ...string) func() float64 {
	t.Helper()
	before := labeledCounterValue(t, vec, labels...)
	return func() float64 { return labeledCounterValue(t, vec, labels...) - before }
}

// TestSubtitleServeSourceMetricsTextArtifact covers the two whole-track text
// paths: the first serve extracts from the source, the second is served from
// the committed artifact. Neither is attributed to a window path.
func TestSubtitleServeSourceMetricsTextArtifact(t *testing.T) {
	c, source := newTestCache(t)
	extract := func(_ context.Context, opts StreamExtractOpts) error {
		_, err := io.WriteString(opts.Writer, "WEBVTT\n\n00:00.000 --> 00:01.000\nhello\n")
		return err
	}
	opts := StreamExtractOpts{InputPath: source, SourceCodec: "subrip"}
	sourceDelta := delta(t, subtitleServeSource, subtitleServeSourceExtract)
	artifactDelta := delta(t, subtitleServeSource, subtitleServeArtifact)
	memoryDelta := delta(t, subtitleServeSource, subtitleServeMemoryWindow)
	warmDelta := delta(t, subtitleServeSource, subtitleServeWarmPrefix)

	for range 2 {
		rec := httptest.NewRecorder()
		if _, err := c.ServeExtractWithResult(rec, httptest.NewRequest(http.MethodGet, "/subtitle", nil), opts, extract); err != nil {
			t.Fatalf("serve: %v", err)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
	}
	if got := sourceDelta(); got != 1 {
		t.Fatalf("%s delta = %v, want 1", subtitleServeSourceExtract, got)
	}
	if got := artifactDelta(); got != 1 {
		t.Fatalf("%s delta = %v, want 1", subtitleServeArtifact, got)
	}
	if got := memoryDelta(); got != 0 {
		t.Fatalf("%s delta = %v, want 0", subtitleServeMemoryWindow, got)
	}
	if got := warmDelta(); got != 0 {
		t.Fatalf("%s delta = %v, want 0", subtitleServeWarmPrefix, got)
	}
}

// TestSubtitleServeSourceMetricsLastGoodWindow covers a repeated bounded
// window: the first serve extracts, the repeat is answered from the last-good
// memory window.
func TestSubtitleServeSourceMetricsLastGoodWindow(t *testing.T) {
	c, source := newTestCache(t)
	extract := func(_ context.Context, opts StreamExtractOpts) error {
		_, err := io.WriteString(opts.Writer, "[Script Info]\n"+subtitleASSEventDialogue+" 0,0:00:12.00,0:00:18.00,Default,,0,0,0,,hi\n")
		return err
	}
	opts := StreamExtractOpts{
		InputPath:             source,
		SourceCodec:           "ass",
		TrackIndex:            0,
		SeekSeconds:           10,
		DurationSeconds:       20,
		DisableBackgroundWarm: true,
	}
	sourceDelta := delta(t, subtitleServeSource, subtitleServeSourceExtract)
	memoryDelta := delta(t, subtitleServeSource, subtitleServeMemoryWindow)
	serve := func() {
		t.Helper()
		rec := httptest.NewRecorder()
		if _, err := c.ServeExtractWithResult(rec, httptest.NewRequest(http.MethodGet, "/subtitle", nil), opts, extract); err != nil {
			t.Fatalf("serve: %v", err)
		}
	}
	serve()
	serve()
	if got := sourceDelta(); got != 1 {
		t.Fatalf("%s delta = %v, want 1", subtitleServeSourceExtract, got)
	}
	if got := memoryDelta(); got != 1 {
		t.Fatalf("%s delta = %v, want 1", subtitleServeMemoryWindow, got)
	}
}

// TestSubtitleServeSourceMetricsWarmPrefix covers a window the in-progress ASS
// warm has already passed: it is rendered from the retained prefix and never
// extracts.
func TestSubtitleServeSourceMetricsWarmPrefix(t *testing.T) {
	c, source := newTestCache(t)
	opts := StreamExtractOpts{
		InputPath:             source,
		SourceCodec:           "ass",
		TrackIndex:            0,
		SeekSeconds:           10,
		DurationSeconds:       20,
		DisableBackgroundWarm: true,
	}
	key, ok := c.formatEntryKey(source, "", 0, subtitleFormatASS)
	if !ok {
		t.Fatal("warm text key")
	}
	progress := c.beginWarmTextProgress(key)
	defer c.endWarmTextProgress(key, progress)
	progress.observe([]byte("[Script Info]\n[Events]\n" +
		"Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n" +
		subtitleASSEventDialogue + " 0,0:00:05.00,0:00:15.00,Default,,0,0,0,,hello\n" +
		subtitleASSEventDialogue + " 0,0:01:00.00,0:01:10.00,Default,,0,0,0,,later\n"))

	warmDelta := delta(t, subtitleServeSource, subtitleServeWarmPrefix)
	extract := func(_ context.Context, _ StreamExtractOpts) error {
		t.Fatal("a covered warm prefix must not extract")
		return nil
	}
	rec := httptest.NewRecorder()
	if _, err := c.ServeExtractWithResult(rec, httptest.NewRequest(http.MethodGet, "/subtitle", nil), opts, extract); err != nil {
		t.Fatalf("serve: %v", err)
	}
	if body := rec.Body.String(); !strings.Contains(body, "hello") {
		t.Fatalf("warm-prefix body = %q", body)
	}
	if got := warmDelta(); got != 1 {
		t.Fatalf("%s delta = %v, want 1", subtitleServeWarmPrefix, got)
	}
}

// TestSubtitleServeSourceMetricsPGS covers the PGS artifact/source split.
func TestSubtitleServeSourceMetricsPGS(t *testing.T) {
	c, source := newTestCache(t)
	fillEntry(t, c, source, 0, "SUP PAYLOAD")

	artifactDelta := delta(t, subtitleServeSource, subtitleServePGSSupArtifact)
	if err := c.ServeSUPExtract(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/subtitle.sup", nil),
		supExtractOpts(source, 0), func(context.Context, StreamExtractOpts) error {
			t.Fatal("a committed PGS entry must not extract")
			return nil
		}); err != nil {
		t.Fatalf("artifact serve: %v", err)
	}
	if got := artifactDelta(); got != 1 {
		t.Fatalf("%s delta = %v, want 1", subtitleServePGSSupArtifact, got)
	}

	cold, coldSource := newTestCache(t)
	sourceDelta := delta(t, subtitleServeSource, subtitleServePGSSupSource)
	if err := cold.ServeSUPExtract(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/subtitle.sup", nil),
		supExtractOpts(coldSource, 0), func(_ context.Context, opts StreamExtractOpts) error {
			_, err := io.WriteString(opts.Writer, "SUP")
			return err
		}); err != nil {
		t.Fatalf("cold serve: %v", err)
	}
	if got := sourceDelta(); got != 1 {
		t.Fatalf("%s delta = %v, want 1", subtitleServePGSSupSource, got)
	}
}

// TestSubtitleLayoutProbeMetricsCountsRunsAndReuse keeps the probe counter
// paired with the probe: a real spawn counts once, a memo reuse counts on the
// reuse hook, and a zero-budget skip counts neither.
func TestSubtitleLayoutProbeMetricsCountsRunsAndReuse(t *testing.T) {
	bin := writeSubtitleProbeScript(t, `printf '%s' '{"streams":[{"index":0,"codec_name":"subrip","codec_type":"subtitle","tags":{}}]}'
`)
	ranDelta := delta(t, subtitleLayoutProbes, subtitleProbeRan)
	reusedDelta := delta(t, subtitleLayoutProbes, subtitleProbeReused)

	if _, err := ProbeSubtitleLayoutWithin(context.Background(), bin, "/media/movie.mkv", 5*time.Second); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if got := ranDelta(); got != 1 {
		t.Fatalf("ran delta = %v, want 1", got)
	}

	ObserveSubtitleLayoutProbeReused()
	ObserveSubtitleLayoutProbeReused()
	if got := reusedDelta(); got != 2 {
		t.Fatalf("reused delta = %v, want 2", got)
	}

	// No budget: skipped before ffprobe spawns, so it is not a run.
	if _, err := ProbeSubtitleLayoutWithin(context.Background(), bin, "/media/movie.mkv", 0); err == nil {
		t.Fatal("zero-budget probe reported success")
	}
	if got := ranDelta(); got != 1 {
		t.Fatalf("ran delta after skip = %v, want 1", got)
	}
}

// TestSubtitleRelayAdmissionMetricsCountsShedPerClass proves each admission
// class reports its own admitted/shed outcome and that a local read, which is
// never admitted, records nothing.
func TestSubtitleRelayAdmissionMetricsCountsShedPerClass(t *testing.T) {
	cases := []struct {
		name  string
		class relayDemuxClass
		label string
	}{
		{name: "request", class: relayDemuxRequestPath, label: relayDemuxClassRequestLabel},
		{name: "detached", class: relayDemuxDetached, label: relayDemuxClassDetachedLabel},
		{name: "font", class: relayDemuxFont, label: relayDemuxClassFontLabel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newRelayDemuxGuardWithPools(1, 1, 1, time.Millisecond)
			source := "https://" + tc.name + ".relay.example"
			input := source + "/session/token"
			admittedDelta := delta(t, subtitleRelayAdmissions, tc.label, "admitted", source)
			shedDelta := delta(t, subtitleRelayAdmissions, tc.label, "shed", source)

			release, ok := g.acquire(context.Background(), input, tc.class)
			if !ok {
				t.Fatal("first acquire was not admitted")
			}
			if _, ok := g.acquire(context.Background(), input, tc.class); ok {
				release()
				t.Fatal("second acquire was admitted past a one-slot budget")
			}
			release()

			if got := admittedDelta(); got != 1 {
				t.Fatalf("admitted delta = %v, want 1", got)
			}
			if got := shedDelta(); got != 1 {
				t.Fatalf("shed delta = %v, want 1", got)
			}

			// A local input is outside the relay budget and is not counted.
			if _, ok := g.acquire(context.Background(), "/media/movie.mkv", tc.class); !ok {
				t.Fatal("local read was not admitted")
			}
			if got := admittedDelta(); got != 1 {
				t.Fatalf("local read counted as an admission: delta = %v", got)
			}
		})
	}
}

// TestSubtitleWarmPageOverlapMetricTimesTheCollision makes a page window
// extract run while a background warm for the same identity is filling and
// asserts the overlap is attributed once.
func TestSubtitleWarmPageOverlapMetricTimesTheCollision(t *testing.T) {
	c, source := newTestCache(t)
	opts := StreamExtractOpts{
		InputPath:             source,
		SourceCodec:           "subrip",
		TrackIndex:            0,
		SeekSeconds:           10,
		DurationSeconds:       20,
		DisableBackgroundWarm: true,
	}
	format := subtitleCacheFormat("subrip", "")
	fill, _ := c.reserveFill(source, "", 0, format, true)
	if fill == nil {
		t.Fatal("reserve background fill")
	}
	defer fill.Discard()
	c.mu.Lock()
	fill.background = true
	c.mu.Unlock()

	overlapDelta := delta(t, subtitleWarmPageOverlaps, format)
	rec := httptest.NewRecorder()
	if _, err := c.ServeExtractWithResult(rec, httptest.NewRequest(http.MethodGet, "/subtitle", nil), opts,
		func(_ context.Context, o StreamExtractOpts) error {
			_, err := io.WriteString(o.Writer, "WEBVTT\n\n00:00.000 --> 00:01.000\nhello\n")
			return err
		}); err != nil {
		t.Fatalf("serve: %v", err)
	}
	if got := overlapDelta(); got != 1 {
		t.Fatalf("warm/page overlap delta = %v, want 1", got)
	}
}
