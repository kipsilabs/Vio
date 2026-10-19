package playback

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// An oversized window is still streamed but must be marked truncated so the
// prefix is never stored and served as a complete last-good slice.
func TestSubtitleWindowCaptureMarksTruncation(t *testing.T) {
	var dst bytes.Buffer
	capture := &subtitleWindowCapture{dst: &dst, remaining: 4}
	if _, err := capture.Write([]byte("abcd")); err != nil {
		t.Fatal(err)
	}
	if capture.truncated {
		t.Fatal("a response that exactly fills the bound is not truncated")
	}
	if _, err := capture.Write([]byte("e")); err != nil {
		t.Fatal(err)
	}
	if !capture.truncated {
		t.Fatal("a response past the bound must be marked truncated")
	}
	if got := dst.String(); got != "abcde" {
		t.Fatalf("streamed bytes = %q, want the full response", got)
	}
	if got := capture.buf.String(); got != "abcd" {
		t.Fatalf("captured bytes = %q, want the bounded prefix", got)
	}
}

func windowResilienceOpts(source string) StreamExtractOpts {
	return StreamExtractOpts{
		InputPath:             source,
		SourceCodec:           "ass",
		TrackIndex:            0,
		SeekSeconds:           120,
		DurationSeconds:       600,
		WindowRequested:       true,
		DisableBackgroundWarm: true,
	}
}

func subtitleWindowRequest() *http.Request {
	return httptest.NewRequest(http.MethodGet, "/subtitle.ass?position=120&duration=600", nil)
}

func upstreamSubtitleError() error {
	return errors.New(`ffmpeg subtitle stream failed: exit status 8 (stderr: [http @ 0x1] Server returned 5XX Server Error reply)`)
}

// A repeated identical window request must serve the last-good copy instead of
// re-running ffmpeg, including after the provider starts failing with a 5xx.
func TestServeExtractWindowedRepeatServesLastGoodThroughUpstreamFailure(t *testing.T) {
	c, source := newTestCache(t)
	opts := windowResilienceOpts(source)
	//nolint:misspell // "Dialogue" is the ASS cue keyword, not prose.
	payload := "[Script Info]\nDialogue: 0,0:02:00.00,0:02:01.00,Default,,0,0,0,,hello\n"

	calls := 0
	extract := func(_ context.Context, o StreamExtractOpts) error {
		calls++
		_, err := io.WriteString(o.Writer, payload)
		return err
	}
	for attempt := 0; attempt < 2; attempt++ {
		rec := httptest.NewRecorder()
		if err := c.ServeExtract(rec, subtitleWindowRequest(), opts, extract); err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		if rec.Body.String() != payload {
			t.Fatalf("attempt %d: body = %q, want cached window", attempt, rec.Body.String())
		}
	}
	if calls != 1 {
		t.Fatalf("extract calls = %d, want 1 for a repeated window", calls)
	}

	// The upstream now 5xxes; the exact repeat still serves the last-good copy.
	failing := func(context.Context, StreamExtractOpts) error {
		calls++
		return upstreamSubtitleError()
	}
	rec := httptest.NewRecorder()
	if err := c.ServeExtract(rec, subtitleWindowRequest(), opts, failing); err != nil {
		t.Fatalf("cached serve during upstream failure: %v", err)
	}
	if rec.Body.String() != payload {
		t.Fatalf("cached body during upstream failure = %q", rec.Body.String())
	}
	if calls != 1 {
		t.Fatalf("extract calls = %d after upstream failure, want the cached copy (still 1)", calls)
	}
}

// Without a cached copy, an upstream 5xx is retried a bounded number of times
// inside the request, then the identity enters a backoff so an immediate client
// retry does not open the provider again.
func TestServeExtractWindowedUpstreamFailureBacksOff(t *testing.T) {
	oldDelays := subtitleExtractRetryDelays
	subtitleExtractRetryDelays = []time.Duration{time.Millisecond}
	defer func() { subtitleExtractRetryDelays = oldDelays }()

	c, source := newTestCache(t)
	opts := windowResilienceOpts(source)
	calls := 0
	extract := func(context.Context, StreamExtractOpts) error {
		calls++
		return upstreamSubtitleError()
	}

	err := c.ServeExtract(httptest.NewRecorder(), subtitleWindowRequest(), opts, extract)
	if !IsSubtitleUpstreamError(err) {
		t.Fatalf("first error = %v, want the upstream failure", err)
	}
	wantCalls := 1 + len(subtitleExtractRetryDelays)
	if calls != wantCalls {
		t.Fatalf("extract calls = %d, want %d bounded attempts", calls, wantCalls)
	}

	// The next request is answered from the backoff without extracting.
	err = c.ServeExtract(httptest.NewRecorder(), subtitleWindowRequest(), opts, extract)
	var retry *SubtitleExtractionRetryError
	if !errors.As(err, &retry) {
		t.Fatalf("second error = %v, want SubtitleExtractionRetryError", err)
	}
	if retry.RetryAfter <= 0 {
		t.Fatalf("retry after = %s, want a positive backoff", retry.RetryAfter)
	}
	if calls != wantCalls {
		t.Fatalf("extract calls = %d after a retry inside the backoff, want %d", calls, wantCalls)
	}

	// Once the backoff lapses the next request attempts extraction again.
	key, ok := c.formatEntryKey(opts.InputPath, opts.CacheIdentity, opts.TrackIndex, subtitleFormatASS)
	if !ok {
		t.Fatal("failed to compute the extraction key")
	}
	c.failureMu.Lock()
	guard := c.failureGuard[key]
	guard.retryAt = time.Now().Add(-time.Second)
	c.failureGuard[key] = guard
	c.failureMu.Unlock()

	if err := c.ServeExtract(httptest.NewRecorder(), subtitleWindowRequest(), opts, extract); !IsSubtitleUpstreamError(err) {
		t.Fatalf("post-backoff error = %v, want a fresh upstream attempt", err)
	}
	if calls != wantCalls*2 {
		t.Fatalf("post-backoff extract calls = %d, want %d", calls, wantCalls*2)
	}
}

// Concurrent identical window requests must coalesce onto one extraction and
// all serve its bytes.
func TestServeExtractWindowedCoalescesConcurrentRepeats(t *testing.T) {
	c, source := newTestCache(t)
	opts := windowResilienceOpts(source)
	payload := "[Script Info]\n"

	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })

	var (
		mu    sync.Mutex
		calls int
	)
	extract := func(_ context.Context, o StreamExtractOpts) error {
		mu.Lock()
		calls++
		leader := calls == 1
		mu.Unlock()
		if leader {
			close(started)
			<-release
		}
		_, err := io.WriteString(o.Writer, payload)
		return err
	}

	type result struct {
		body string
		err  error
	}
	const requests = 4
	begin := make(chan struct{})
	results := make(chan result, requests)
	for i := 0; i < requests; i++ {
		go func() {
			<-begin
			rec := httptest.NewRecorder()
			err := c.ServeExtract(rec, subtitleWindowRequest(), opts, extract)
			results <- result{body: rec.Body.String(), err: err}
		}()
	}
	close(begin)

	// The leader holds the extraction open; the others either join its flight
	// or, if they arrive after it stores the slice, serve that stored copy.
	<-started
	releaseOnce.Do(func() { close(release) })

	for i := 0; i < requests; i++ {
		got := <-results
		if got.err != nil {
			t.Fatalf("request %d: %v", i, got.err)
		}
		if got.body != payload {
			t.Fatalf("request %d body = %q, want %q", i, got.body, payload)
		}
	}
	if calls != 1 {
		t.Fatalf("extract calls = %d, want 1 coalesced extraction", calls)
	}
}
