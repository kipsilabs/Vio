package playback

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fontBundleTestKey is a local-file key with a stable mtime/size.
func fontBundleTestKey() FontBundleKey {
	return FontBundleKey{
		FileID:        7,
		Size:          1234,
		MtimeUnixNano: 1_700_000_000_000_000_000,
		FFmpegPath:    "/usr/bin/ffmpeg",
	}
}

// newFontCache builds a cache rooted under one temp transcode dir captured at
// construction, mirroring newTestCache.
func newFontCache(t *testing.T) *SubtitleCache {
	t.Helper()
	base := t.TempDir()
	return NewSubtitleCache(func() string { return base })
}

func TestExtractFontBundleCachesHit(t *testing.T) {
	c := newFontCache(t)
	key := fontBundleTestKey()

	var calls atomic.Int64
	extract := func(context.Context) ([]byte, error) {
		calls.Add(1)
		return []byte(`{"name":"bundle"}`), nil
	}

	first, err := c.ExtractFontBundle(t.Context(), key, extract)
	if err != nil {
		t.Fatalf("first extract: %v", err)
	}
	second, err := c.ExtractFontBundle(t.Context(), key, extract)
	if err != nil {
		t.Fatalf("second extract: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("extractor ran %d times, want 1", got)
	}
	if string(first) != `{"name":"bundle"}` {
		t.Fatalf("first payload = %q", first)
	}
	if string(second) != string(first) {
		t.Fatalf("second payload = %q, want %q", second, first)
	}
	// The entry must be committed to disk, not just held in flight.
	if _, ok := c.LookupFontBundle(key); !ok {
		t.Fatal("committed entry not found on disk")
	}
}

func TestExtractFontBundleKeyStability(t *testing.T) {
	c := newFontCache(t)
	extract := func(context.Context) ([]byte, error) { return []byte("payload"), nil }

	base := fontBundleTestKey()
	if _, err := c.ExtractFontBundle(t.Context(), base, extract); err != nil {
		t.Fatalf("seed local entry: %v", err)
	}
	if _, ok := c.LookupFontBundle(base); !ok {
		t.Fatal("same local key missed")
	}
	sizeKey := base
	sizeKey.Size++
	if _, ok := c.LookupFontBundle(sizeKey); ok {
		t.Fatal("changed Size still hit")
	}
	mtimeKey := base
	mtimeKey.MtimeUnixNano++
	if _, ok := c.LookupFontBundle(mtimeKey); ok {
		t.Fatal("changed MtimeUnixNano still hit")
	}
	idKey := base
	idKey.FileID++
	if _, ok := c.LookupFontBundle(idKey); ok {
		t.Fatal("changed FileID still hit")
	}
	// A local row without a usable mtime/size is uncacheable: extraction runs,
	// nothing is committed, and the next call runs again.
	unkeyable := FontBundleKey{FileID: 7, FFmpegPath: "/usr/bin/ffmpeg"}
	var calls atomic.Int64
	uncachedExtract := func(context.Context) ([]byte, error) { calls.Add(1); return []byte("payload"), nil }
	if _, err := c.ExtractFontBundle(t.Context(), unkeyable, uncachedExtract); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ExtractFontBundle(t.Context(), unkeyable, uncachedExtract); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("unkeyable extractor ran %d times, want 2 (never cached)", got)
	}

	// Virtual keys: same pinned result stays stable, a different pinned result
	// (rotation to another candidate) misses.
	virtualKey := FontBundleKey{FileID: 7, PinnedResult: "result-abc", FFmpegPath: "/usr/bin/ffmpeg"}
	if _, err := c.ExtractFontBundle(t.Context(), virtualKey, extract); err != nil {
		t.Fatalf("seed virtual entry: %v", err)
	}
	if _, ok := c.LookupFontBundle(virtualKey); !ok {
		t.Fatal("same virtual key missed")
	}
	other := virtualKey
	other.PinnedResult = "result-def"
	if _, ok := c.LookupFontBundle(other); ok {
		t.Fatal("different PinnedResult still hit")
	}
}

func TestExtractFontBundleSingleFlight(t *testing.T) {
	c := newFontCache(t)
	key := FontBundleKey{FileID: 7, PinnedResult: "result-sf", FFmpegPath: "/usr/bin/ffmpeg"}

	const n = 16
	var calls atomic.Int64
	release := make(chan struct{})
	extractEntered := make(chan struct{}, 1)
	extract := func(context.Context) ([]byte, error) {
		calls.Add(1)
		// Signal that the singleflight leader has entered the extract
		// function, so the test can release the barrier without a fixed
		// sleep that is flaky under CI load.
		select {
		case extractEntered <- struct{}{}:
		default:
		}
		<-release
		return []byte("shared bundle"), nil
	}

	var started, finished sync.WaitGroup
	started.Add(n)
	finished.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer finished.Done()
			started.Done()
			data, err := c.ExtractFontBundle(context.Background(), key, extract)
			if err != nil {
				t.Errorf("stampede extract: %v", err)
				return
			}
			if string(data) != "shared bundle" {
				t.Errorf("stampede payload = %q", data)
			}
		}()
	}

	// Give every goroutine time to reach the singleflight flight before letting
	// the single in-flight extractor complete. The extract function signals
	// via extractEntered so we wait for the actual singleflight entry, not a
	// fixed sleep that is flaky under CI load.
	started.Wait()
	<-extractEntered
	close(release)
	finished.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("extractor ran %d times, want exactly 1", got)
	}
}

func TestExtractFontBundleLeaderCancellationDoesNotFailWaiters(t *testing.T) {
	c := newFontCache(t)
	key := FontBundleKey{FileID: 9, PinnedResult: "result-cancel", FFmpegPath: "/usr/bin/ffmpeg"}

	started := make(chan struct{})
	release := make(chan struct{})
	extract := func(context.Context) ([]byte, error) {
		close(started)
		<-release
		return []byte("survived cancellation"), nil
	}

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		data, err := c.ExtractFontBundle(leaderCtx, key, extract)
		if err == nil && string(data) != "survived cancellation" {
			err = context.DeadlineExceeded // sentinel: wrong payload
		}
		leaderDone <- err
	}()
	<-started

	waiterDone := make(chan error, 1)
	go func() {
		data, err := c.ExtractFontBundle(context.Background(), key, extract)
		if err == nil && string(data) != "survived cancellation" {
			err = context.DeadlineExceeded // sentinel: wrong payload
		}
		waiterDone <- err
	}()
	// Let the waiter join the leader's flight before unblocking extraction.
	// The extract function closes `started` on first entry; the waiter
	// needs a brief yield to reach the singleflight Do() call. A short
	// sleep is used because the singleflight's internal state (whether
	// the waiter has joined the leader's flight) is not observable from
	// the test.
	<-started
	time.Sleep(50 * time.Millisecond)

	// Canceling the leader's request must not kill the shared extraction the
	// waiter is blocked on.
	cancelLeader()
	close(release)

	if err := <-waiterDone; err != nil {
		t.Fatalf("waiter failed after leader cancellation: %v", err)
	}
	if err := <-leaderDone; err != nil {
		t.Fatalf("leader failed after cancellation: %v", err)
	}
}

// waitForFontBundle polls until key has a committed entry, failing the test
// if it never lands. Polling on observable cache state keeps the wait free of
// fixed sleeps that are flaky under load.
func waitForFontBundle(t *testing.T, c *SubtitleCache, key FontBundleKey) []byte {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if data, ok := c.LookupFontBundle(key); ok {
			return data
		}
		if time.Now().After(deadline) {
			t.Fatal("font bundle was never committed")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A request whose extraction is still in flight must return promptly so the
// handler can serve an empty bundle instead of holding a 60s connection. The
// detached extraction must keep running and commit for the next fetch.
func TestExtractFontBundleWithinReturnsPendingWhileInFlight(t *testing.T) {
	c := newFontCache(t)
	key := FontBundleKey{FileID: 7, PinnedResult: "result-inflight", FFmpegPath: "/usr/bin/ffmpeg"}

	release := make(chan struct{})
	var calls atomic.Int64
	extract := func(context.Context) ([]byte, error) {
		calls.Add(1)
		<-release
		return []byte("late bundle"), nil
	}

	start := time.Now()
	data, ready, err := c.ExtractFontBundleWithin(t.Context(), key, 40*time.Millisecond, extract)
	if err != nil {
		t.Fatalf("pending extraction returned error: %v", err)
	}
	if ready {
		t.Fatalf("pending extraction reported ready with %q", data)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("bounded wait took %s, want prompt return", elapsed)
	}

	close(release)
	if got := string(waitForFontBundle(t, c, key)); got != "late bundle" {
		t.Fatalf("detached extraction committed %q, want late bundle", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("extractor ran %d times, want 1", got)
	}
}

// The pending/fast-empty path must still register the shared flight so
// concurrent requests coalesce onto a single extraction.
func TestExtractFontBundleWithinCoalescesParallelRequests(t *testing.T) {
	c := newFontCache(t)
	key := FontBundleKey{FileID: 7, PinnedResult: "result-stampede", FFmpegPath: "/usr/bin/ffmpeg"}

	release := make(chan struct{})
	var calls atomic.Int64
	extract := func(context.Context) ([]byte, error) {
		calls.Add(1)
		<-release
		return []byte("shared"), nil
	}

	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			data, ready, err := c.ExtractFontBundleWithin(context.Background(), key, 30*time.Millisecond, extract)
			if err != nil || ready || data != nil {
				t.Errorf("parallel pending request = (%q, %v, %v), want pending", data, ready, err)
			}
		}()
	}
	wg.Wait()
	close(release)

	if got := string(waitForFontBundle(t, c, key)); got != "shared" {
		t.Fatalf("committed bundle = %q, want shared", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("extractor ran %d times, want exactly 1", got)
	}
}

// A definitive, non-timeout failure is the only case that should surface to
// the caller (which turns it into a 500); ambiguous timeouts are handled as
// pending so the client degrades to default fonts.
func TestExtractFontBundleWithinReportsDefinitiveFailure(t *testing.T) {
	c := newFontCache(t)
	key := FontBundleKey{FileID: 7, PinnedResult: "result-fail", FFmpegPath: "/usr/bin/ffmpeg"}
	want := errors.New("malformed container")

	data, ready, err := c.ExtractFontBundleWithin(t.Context(), key, time.Second, func(context.Context) ([]byte, error) {
		return nil, want
	})
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	if ready || data != nil {
		t.Fatalf("failed extraction returned (%q, ready=%v)", data, ready)
	}
}

func TestExtractFontBundleWithinTreatsTimeoutAsPending(t *testing.T) {
	c := newFontCache(t)
	key := FontBundleKey{FileID: 7, PinnedResult: "result-timeout", FFmpegPath: "/usr/bin/ffmpeg"}

	data, ready, err := c.ExtractFontBundleWithin(t.Context(), key, time.Second, func(context.Context) ([]byte, error) {
		return nil, context.DeadlineExceeded
	})
	if err != nil {
		t.Fatalf("timeout surfaced as an error: %v", err)
	}
	if ready || data != nil {
		t.Fatalf("timed-out extraction returned (%q, ready=%v), want pending", data, ready)
	}
}

// The pre-warm path shares the disk cache and single-flight with the HTTP
// path, and its done channel lets the caller release a request-scoped relay
// registration exactly once after the extraction settles.
func TestWarmFontBundleInBackgroundFillsCache(t *testing.T) {
	c := newFontCache(t)
	key := FontBundleKey{FileID: 7, PinnedResult: "result-warm", FFmpegPath: "/usr/bin/ffmpeg"}

	var calls atomic.Int64
	done := c.WarmFontBundleInBackground(key, func(context.Context) ([]byte, error) {
		calls.Add(1)
		return []byte("warmed"), nil
	})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("warm did not settle")
	}
	if got := string(waitForFontBundle(t, c, key)); got != "warmed" {
		t.Fatalf("warmed bundle = %q", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("extractor ran %d times, want 1", got)
	}
}

func TestWarmFontBundleInBackgroundSkipsCached(t *testing.T) {
	c := newFontCache(t)
	key := FontBundleKey{FileID: 7, PinnedResult: "result-warm-cached", FFmpegPath: "/usr/bin/ffmpeg"}
	if _, err := c.ExtractFontBundle(t.Context(), key, func(context.Context) ([]byte, error) {
		return []byte("already"), nil
	}); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	var calls atomic.Int64
	done := c.WarmFontBundleInBackground(key, func(context.Context) ([]byte, error) {
		calls.Add(1)
		return []byte("re-extract"), nil
	})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cached warm did not settle")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("cached warm re-ran extractor %d times, want 0", got)
	}
}

// The warm must join the same flight an HTTP request would start, so a warm
// racing a request cannot fan out into two ffmpeg runs.
func TestWarmFontBundleSharesFlightWithExtractWithin(t *testing.T) {
	c := newFontCache(t)
	key := FontBundleKey{FileID: 7, PinnedResult: "result-warm-race", FFmpegPath: "/usr/bin/ffmpeg"}

	release := make(chan struct{})
	var calls atomic.Int64
	entered := make(chan struct{}, 1)
	extract := func(context.Context) ([]byte, error) {
		calls.Add(1)
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return []byte("raced"), nil
	}

	warmDone := c.WarmFontBundleInBackground(key, extract)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("warm never entered the extractor")
	}
	// A request arriving while the warm leads waits (briefly) and then returns
	// pending; it must not start a second extraction.
	_, ready, err := c.ExtractFontBundleWithin(t.Context(), key, 20*time.Millisecond, extract)
	if err != nil || ready {
		t.Fatalf("racing request = (ready=%v, err=%v), want pending", ready, err)
	}
	close(release)
	select {
	case <-warmDone:
	case <-time.After(5 * time.Second):
		t.Fatal("warm did not settle")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("extractor ran %d times, want exactly 1", got)
	}
}

func TestExtractFontBundleNilCacheRunsExtractor(t *testing.T) {
	var c *SubtitleCache // nil receiver
	calls := 0
	data, err := c.ExtractFontBundle(t.Context(), fontBundleTestKey(), func(context.Context) ([]byte, error) {
		calls++
		return []byte("uncached"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "uncached" || calls != 1 {
		t.Fatalf("nil cache path returned %q with %d calls", data, calls)
	}
	if _, ok := c.LookupFontBundle(fontBundleTestKey()); ok {
		t.Fatal("nil cache LookupFontBundle reported a hit")
	}
}
