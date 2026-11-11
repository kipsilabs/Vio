package playback

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const prefetchWindowVTT = "WEBVTT\n\n00:04:00.000 --> 00:04:01.000\nprefetched\n"

// A playhead prefetch must keep the covering windows in the last-good window
// cache, so a later request for one of them is served without a second extract.
func TestPrefetchWindowsCachesWindowsForLaterServe(t *testing.T) {
	cache, source := newTestCache(t)
	var extractMu sync.Mutex
	var extracts []StreamExtractOpts
	extract := func(_ context.Context, opts StreamExtractOpts) error {
		extractMu.Lock()
		extracts = append(extracts, opts)
		extractMu.Unlock()
		_, err := io.WriteString(opts.Writer, prefetchWindowVTT)
		return err
	}
	opts := StreamExtractOpts{
		InputPath:       source,
		TrackIndex:      0,
		SourceCodec:     "subrip",
		SeekSeconds:     0,
		DurationSeconds: 120,
		WindowRequested: true,
	}
	windows := []SubtitlePrefetchWindow{
		{StartSeconds: 240, DurationSeconds: 120},
		{StartSeconds: 360, DurationSeconds: 120},
	}
	select {
	case <-cache.PrefetchWindowsInBackground(opts, windows, extract, "prefetch-cache"):
	case <-time.After(5 * time.Second):
		t.Fatal("window prefetch did not settle")
	}
	extractMu.Lock()
	if len(extracts) != 2 {
		extractMu.Unlock()
		t.Fatalf("prefetch extracts = %d, want 2", len(extracts))
	}
	for _, got := range extracts {
		if !got.WindowRequested || got.SeekSeconds == 0 || got.DurationSeconds <= 0 {
			extractMu.Unlock()
			t.Fatalf("prefetch extract was not a bounded window: seek=%v duration=%v", got.SeekSeconds, got.DurationSeconds)
		}
	}
	extractMu.Unlock()

	// The exact prefetched window and a smaller request contained in it are both
	// served from the window cache. Neither may spawn another extraction.
	for _, request := range []struct{ seek, duration float64 }{
		{seek: 240, duration: 120},
		{seek: 250, duration: 50},
	} {
		reqOpts := opts
		reqOpts.SeekSeconds = request.seek
		reqOpts.DurationSeconds = request.duration
		reqOpts.DisableBackgroundWarm = true
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		_, err := cache.ServeExtractWithResult(rec, req, reqOpts, func(context.Context, StreamExtractOpts) error {
			t.Errorf("window %.0f+%.0f was re-extracted instead of served from the prefetch", request.seek, request.duration)
			return nil
		})
		if err != nil {
			t.Fatalf("serve prefetched window %.0f+%.0f: %v", request.seek, request.duration, err)
		}
		if !strings.Contains(rec.Body.String(), "prefetched") {
			t.Fatalf("serve prefetched window body = %q", rec.Body.String())
		}
	}
	extractMu.Lock()
	defer extractMu.Unlock()
	if len(extracts) != 2 {
		t.Fatalf("extracts after cached serves = %d, want 2", len(extracts))
	}
	if cache.HasCommittedTextEntry(source, "", 0, "subrip", "") {
		t.Fatal("window prefetch committed a whole-track artifact")
	}
}

// A prefetched tail window must never be published as the complete track: the
// whole-track path still extracts from source zero, and nothing is committed
// under the artifact key.
func TestPrefetchWindowsNeverPublishesTailAsWholeTrack(t *testing.T) {
	cache, source := newTestCache(t)
	var calls atomic.Int32
	extract := func(_ context.Context, opts StreamExtractOpts) error {
		calls.Add(1)
		body := prefetchWindowVTT
		if !opts.windowIntent() {
			body = "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nwhole\n"
		}
		_, err := io.WriteString(opts.Writer, body)
		return err
	}
	opts := StreamExtractOpts{
		InputPath:       source,
		TrackIndex:      0,
		SourceCodec:     "subrip",
		SeekSeconds:     0,
		DurationSeconds: 120,
		WindowRequested: true,
	}
	select {
	case <-cache.PrefetchWindowsInBackground(opts, []SubtitlePrefetchWindow{{StartSeconds: 240, DurationSeconds: 120}}, extract, "prefetch-artifact"):
	case <-time.After(5 * time.Second):
		t.Fatal("window prefetch did not settle")
	}
	if cache.HasCommittedTextEntry(source, "", 0, "subrip", "") {
		t.Fatal("window prefetch committed a whole-track artifact")
	}

	// A whole-track request after the prefetch must extract the complete track,
	// not replay the prefetched tail slice as if it covered source zero.
	whole := opts
	whole.SeekSeconds = 0
	whole.DurationSeconds = 0
	whole.WindowRequested = false
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, err := cache.ServeExtractWithResult(rec, req, whole, extract); err != nil {
		t.Fatalf("serve whole track: %v", err)
	}
	if body := rec.Body.String(); strings.Contains(body, "prefetched") || !strings.Contains(body, "whole") {
		t.Fatalf("whole-track serve reused a prefetched tail slice: body = %q", body)
	}
	if calls.Load() != 2 {
		t.Fatalf("extract calls = %d, want 1 prefetch + 1 whole track", calls.Load())
	}
}

// While request-path admission is shedding, a prefetch must not add a demux to
// the same relay pipe. The next window miss re-attempts it once shedding stops.
func TestPrefetchWindowsSkipsWhileRequestPathShedding(t *testing.T) {
	guard := newRelayDemuxGuardWithDetached(1, 1, 5*time.Millisecond)
	swapRelayDemuxGuardForTest(t, guard)
	const remote = "http://relay-prefetch-shed.test/source/tok/movie.mkv"

	release, ok := guard.acquire(context.Background(), remote, relayDemuxRequestPath)
	if !ok {
		t.Fatal("first request-path read was not admitted")
	}
	if _, ok := guard.acquire(context.Background(), remote, relayDemuxRequestPath); ok {
		t.Fatal("request-path read was admitted to a saturated source")
	}
	release()

	cache := NewSubtitleCache(func() string { return t.TempDir() })
	var calls atomic.Int32
	opts := StreamExtractOpts{
		InputPath:     remote,
		CacheIdentity: "prefetch-shed",
		TrackIndex:    0,
		SourceCodec:   "subrip",
	}
	select {
	case <-cache.PrefetchWindowsInBackground(opts, []SubtitlePrefetchWindow{{StartSeconds: 240, DurationSeconds: 120}}, func(context.Context, StreamExtractOpts) error {
		calls.Add(1)
		return nil
	}, "prefetch-shed"):
	case <-time.After(time.Second):
		t.Fatal("window prefetch did not settle while request-path admission was shedding")
	}
	if calls.Load() != 0 {
		t.Fatalf("window prefetch extracted %d times while request-path admission was shedding, want 0", calls.Load())
	}
}

// A prefetch never queues behind a taken detached budget: it skips, and the
// next window miss re-attempts it.
func TestPrefetchWindowsSkipsWhenDetachedBudgetTaken(t *testing.T) {
	swapRelayDemuxGuardForTest(t, newRelayDemuxGuardWithDetached(2, 1, 5*time.Millisecond))
	const remote = "http://relay-prefetch-saturated.test/source/tok/movie.mkv"
	release := holdDetachedRelaySlot(t, remote)
	defer release()

	cache := NewSubtitleCache(func() string { return t.TempDir() })
	var calls atomic.Int32
	opts := StreamExtractOpts{
		InputPath:     remote,
		CacheIdentity: "prefetch-saturated",
		TrackIndex:    0,
		SourceCodec:   "subrip",
	}
	select {
	case <-cache.PrefetchWindowsInBackground(opts, []SubtitlePrefetchWindow{{StartSeconds: 240, DurationSeconds: 120}}, func(context.Context, StreamExtractOpts) error {
		calls.Add(1)
		return nil
	}, "prefetch-saturated"):
	case <-time.After(2 * time.Second):
		t.Fatal("window prefetch queued behind a taken detached budget")
	}
	if calls.Load() != 0 {
		t.Fatalf("window prefetch extracted %d times with no detached capacity, want 0", calls.Load())
	}
}

// A prefetch kick launched while the request path is already at its slot
// capacity must not start at all. A shed is only recorded after a client read
// has waited out its whole admission window, so the shed gate alone let a kick
// slip in during exactly that window: the client's own read then timed out on a
// pipe the prefetch had just joined (production: one 503 at the full 753ms slot
// wait, then five shed-skipped kicks).
func TestPrefetchWindowsSkipsWhileRequestPathSaturated(t *testing.T) {
	guard := newRelayDemuxGuardWithDetached(2, 1, 5*time.Millisecond)
	swapRelayDemuxGuardForTest(t, guard)
	const remote = "http://relay-prefetch-full.test/source/tok/movie.mkv"

	// Hold every request-path slot without shedding once: this is the state the
	// prefetch used to miss.
	var releases []func()
	for i := 0; i < 2; i++ {
		release, ok := guard.acquire(context.Background(), remote, relayDemuxRequestPath)
		if !ok {
			t.Fatalf("request-path read %d was not admitted to an unsaturated source", i+1)
		}
		releases = append(releases, release)
	}
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	if !relayDemuxRequestPathSaturated(remote) {
		t.Fatal("request path is not saturated after taking every slot")
	}

	cache := NewSubtitleCache(func() string { return t.TempDir() })
	var calls atomic.Int32
	opts := StreamExtractOpts{
		InputPath:     remote,
		CacheIdentity: "prefetch-saturated-request-path",
		TrackIndex:    0,
		SourceCodec:   "subrip",
	}
	select {
	case <-cache.PrefetchWindowsInBackground(opts, []SubtitlePrefetchWindow{{StartSeconds: 240, DurationSeconds: 120}}, func(context.Context, StreamExtractOpts) error {
		calls.Add(1)
		return nil
	}, "prefetch-saturated-request-path"):
	case <-time.After(time.Second):
		t.Fatal("window prefetch did not settle against a saturated request path")
	}
	if calls.Load() != 0 {
		t.Fatalf("window prefetch extracted %d times against a saturated request path, want 0", calls.Load())
	}
}

// A prefetch that is already running must stop adding windows once the request
// path fills: the saturation it must yield to can appear after its kick, and
// each window is another demux on the pipe a client read is now waiting on.
func TestPrefetchWindowsStopsWhenRequestPathFills(t *testing.T) {
	guard := newRelayDemuxGuardWithDetached(2, 1, 5*time.Millisecond)
	swapRelayDemuxGuardForTest(t, guard)
	const remote = "http://relay-prefetch-fills.test/source/tok/movie.mkv"

	cache := NewSubtitleCache(func() string { return t.TempDir() })
	var calls atomic.Int32
	var held []func()
	opts := StreamExtractOpts{
		InputPath:     remote,
		CacheIdentity: "prefetch-fills",
		TrackIndex:    0,
		SourceCodec:   "subrip",
	}
	extract := func(_ context.Context, windowOpts StreamExtractOpts) error {
		calls.Add(1)
		// Two client reads arrive during this window and take every
		// request-path slot.
		for i := 0; i < 2; i++ {
			release, ok := guard.acquire(context.Background(), remote, relayDemuxRequestPath)
			if !ok {
				return nil
			}
			held = append(held, release)
		}
		_, err := io.WriteString(windowOpts.Writer, prefetchWindowVTT)
		return err
	}
	defer func() {
		for _, release := range held {
			release()
		}
	}()

	select {
	case <-cache.PrefetchWindowsInBackground(opts, []SubtitlePrefetchWindow{
		{StartSeconds: 240, DurationSeconds: 120},
		{StartSeconds: 360, DurationSeconds: 120},
	}, extract, "prefetch-fills"):
	case <-time.After(5 * time.Second):
		t.Fatal("window prefetch did not settle")
	}
	if calls.Load() != 1 {
		t.Fatalf("window prefetch extracts = %d, want the first window only", calls.Load())
	}
}
