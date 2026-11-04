package playback

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const warmTextASSHeader = `[Script Info]
ScriptType: v4.00+
PlayResX: 1920
PlayResY: 1080

[V4+ Styles]
Format: Name, Fontname, Fontsize
Style: Default,Arial,48

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
`

func TestSubtitleWarmTextProgressFiltersAndCovers(t *testing.T) {
	p := newSubtitleWarmTextProgress()
	// Feed in chunks that split a line and the header, mirroring ffmpeg's
	// 32 KiB pipe reads rather than one complete document.
	chunks := []string{
		warmTextASSHeader[:40],
		warmTextASSHeader[40:] + subtitleASSEventDialogue + " 0,0:00:10.00,0:02:10.00,Default,,0,0,0,,Hello",
		"\n" + subtitleASSEventDialogue + " 0,0:01:50.00,0:02:20.00,Default,,0,0,0,,Straddles\n",
		subtitleASSEventComment + " 0,0:03:00.00,0:03:10.00,Default,,0,0,0,,Later\n",
	}
	for _, chunk := range chunks {
		p.observe([]byte(chunk))
	}

	// maxEnd is 190 s (the Comment), so [0,120] is covered.
	data, ok := p.render(0, 120)
	if !ok {
		t.Fatal("window [0,120] reported uncovered")
	}
	body := string(data)
	if !strings.HasPrefix(body, "[Script Info]") || !strings.Contains(body, "Format: Layer, Start, End") {
		t.Fatalf("rendered window has no header: %q", body)
	}
	if !strings.Contains(body, "Hello") {
		t.Fatalf("rendered window dropped an overlapping event: %q", body)
	}
	if !strings.Contains(body, "Straddles") {
		t.Fatalf("rendered window dropped a straddling event: %q", body)
	}
	if strings.Contains(body, "Later") {
		t.Fatalf("rendered window leaked an event outside the range: %q", body)
	}

	// [160,200] is only covered once the 190 s event has been seen; the
	// requested end is 200, so this must miss.
	if _, ok := p.render(160, 200); ok {
		t.Fatal("window [160,200] reported covered before the watermark reached it")
	}

	// A later event extends the watermark; [160,200] then resolves and keeps
	// only the events that overlap it.
	p.observe([]byte(subtitleASSEventDialogue + " 0,0:03:20.00,0:03:40.00,Default,,0,0,0,,Tail\n"))
	data, ok = p.render(160, 200)
	if !ok {
		t.Fatal("window [160,200] still reported uncovered")
	}
	body = string(data)
	if strings.Contains(body, "Hello") || strings.Contains(body, "Tail") {
		t.Fatalf("window [160,200] leaked a non-overlapping event: %q", body)
	}
}

func TestSubtitleWarmTextProgressDisablesOnMalformedEvent(t *testing.T) {
	before := subtitleWarmTextDisables.Load()
	p := newSubtitleWarmTextProgress()
	p.observe([]byte(warmTextASSHeader))
	p.observe([]byte(subtitleASSEventDialogue + " 0,not-a-time,0:02:10.00,Default,,0,0,0,,Broken\n"))
	if _, ok := p.render(0, 120); ok {
		t.Fatal("progress served a window after a malformed event")
	}
	// The disable is signaled once per warm: another malformed chunk must not
	// count or log the same fallback again.
	p.observe([]byte(subtitleASSEventDialogue + " 0,also-not-a-time,0:02:10.00,Default,,0,0,0,,Broken\n"))
	if got := subtitleWarmTextDisables.Load() - before; got != 1 {
		t.Fatalf("disable signal count = %d, want exactly one per warm", got)
	}
}

func TestSubtitleWarmTextProgressHonorsFormatColumns(t *testing.T) {
	p := newSubtitleWarmTextProgress()
	p.observe([]byte("[Events]\nFormat: Start, End, Style, Text\n"))
	p.observe([]byte(subtitleASSEventDialogue + " 0:00:01.00,0:00:02.00,Default,Zero-order\n"))
	// Advance the watermark past the requested end without overlapping it.
	p.observe([]byte(subtitleASSEventDialogue + " 0:00:05.00,0:00:06.00,Default,Later\n"))
	data, ok := p.render(0, 2)
	if !ok {
		t.Fatal("custom Format columns were not honored")
	}
	if !strings.Contains(string(data), "Zero-order") {
		t.Fatalf("rendered window dropped the event: %q", data)
	}
}

// WarmTrackInBackground retains the full-track ASS output as it streams, so a
// window whose range the warm has passed is served without a second extract.
func TestServeExtractWindowServedFromInProgressASSWarm(t *testing.T) {
	c, source := newTestCache(t)
	warmOpts := StreamExtractOpts{InputPath: source, SourceCodec: "ass", TrackIndex: 0}

	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	extract := func(_ context.Context, opts StreamExtractOpts) error {
		calls.Add(1)
		if _, err := io.WriteString(opts.Writer,
			warmTextASSHeader+
				subtitleASSEventDialogue+" 0,0:00:10.00,0:02:10.00,Default,,0,0,0,,Hello\n"+
				// Advances the watermark past the first window's end so the
				// [0,120] range is provably complete.
				subtitleASSEventDialogue+" 0,0:02:10.00,0:02:20.00,Default,,0,0,0,,After\n"); err != nil {
			return err
		}
		close(started)
		<-release
		_, err := io.WriteString(opts.Writer,
			subtitleASSEventDialogue+" 0,0:03:00.00,0:03:10.00,Default,,0,0,0,,Later\n")
		return err
	}
	done := c.WarmTrackInBackground(warmOpts, extract, "")
	<-started

	windowOpts := StreamExtractOpts{
		InputPath:             source,
		SourceCodec:           "ass",
		TrackIndex:            0,
		WindowRequested:       true,
		SeekSeconds:           0,
		DurationSeconds:       120,
		DisableBackgroundWarm: true,
	}
	rec := httptest.NewRecorder()
	err := c.ServeExtract(rec, httptest.NewRequest(http.MethodGet, "/subtitle.ass?position=0&duration=120", nil), windowOpts, extract)
	if err != nil {
		t.Fatalf("serve window: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Hello") {
		t.Fatalf("window body = %q, want the warm's overlapping event", body)
	}
	if strings.Contains(body, "Later") {
		t.Fatalf("window body leaked an event past its end: %q", body)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("extractor calls = %d, want only the warm's extract", got)
	}

	close(release)
	<-done
}

// A window the warm has not reached yet must fall back to its own extract.
func TestServeExtractWindowFallsBackBeforeWarmCoverage(t *testing.T) {
	c, source := newTestCache(t)
	warmOpts := StreamExtractOpts{InputPath: source, SourceCodec: "ass", TrackIndex: 0}

	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	extract := func(_ context.Context, opts StreamExtractOpts) error {
		if calls.Add(1) == 1 {
			if _, err := io.WriteString(opts.Writer,
				warmTextASSHeader+subtitleASSEventDialogue+" 0,0:00:01.00,0:00:02.00,Default,,0,0,0,,Early\n"); err != nil {
				return err
			}
			close(started)
			<-release
			return nil
		}
		return nil
	}
	done := c.WarmTrackInBackground(warmOpts, extract, "")
	<-started

	windowOpts := StreamExtractOpts{
		InputPath:             source,
		SourceCodec:           "ass",
		TrackIndex:            0,
		WindowRequested:       true,
		SeekSeconds:           200,
		DurationSeconds:       120,
		DisableBackgroundWarm: true,
	}
	rec := httptest.NewRecorder()
	err := c.ServeExtract(rec, httptest.NewRequest(http.MethodGet, "/subtitle.ass?position=200&duration=120", nil), windowOpts, extract)
	if err != nil {
		t.Fatalf("serve window: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("extractor calls = %d, want the warm plus the window's own extract", got)
	}

	close(release)
	<-done
}

// A leader whose request context is canceled mid-extract must still commit the
// window for whoever coalesced onto it. Detaching the leader's extract from the
// request context is what stops a client re-anchor from discarding a demux that
// was about to land.
func TestServeExtractWindowedLeaderCancellationStillCommits(t *testing.T) {
	c, source := newTestCache(t)
	opts := StreamExtractOpts{
		InputPath:             source,
		SourceCodec:           "ass",
		TrackIndex:            0,
		WindowRequested:       true,
		SeekSeconds:           0,
		DurationSeconds:       120,
		DisableBackgroundWarm: true,
	}
	const payload = "[Script Info]\n" + subtitleASSEventDialogue + " 0,0:00:01.00,0:00:02.00,Default,,0,0,0,,Window\n"

	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	extract := func(ctx context.Context, extractOpts StreamExtractOpts) error {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			// The leader's request is canceled by now. A request-owned extract
			// would see that here and discard the work.
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		_, err := io.WriteString(extractOpts.Writer, payload)
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	leaderRec := httptest.NewRecorder()
	leaderDone := make(chan error, 1)
	go func() {
		leaderDone <- c.ServeExtract(
			leaderRec,
			httptest.NewRequest(http.MethodGet, "/subtitle.ass?position=0&duration=120", nil).WithContext(ctx),
			opts,
			extract,
		)
	}()
	<-started
	cancel()
	close(release)

	if err := <-leaderDone; err != nil {
		t.Fatalf("leader error = %v, want the detached extract to commit", err)
	}
	followerRec := httptest.NewRecorder()
	if err := c.ServeExtract(followerRec, httptest.NewRequest(http.MethodGet, "/subtitle.ass?position=0&duration=120", nil), opts, extract); err != nil {
		t.Fatalf("follower error = %v", err)
	}
	if got := followerRec.Body.String(); got != payload {
		t.Fatalf("follower body = %q, want %q", got, payload)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("extractor calls = %d, want 1 coalesced extraction", got)
	}
}
