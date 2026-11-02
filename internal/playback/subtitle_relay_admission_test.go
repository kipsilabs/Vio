package playback

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// swapRelayDemuxGuardForTest installs a narrower guard for the duration of a
// test and restores the process-wide one afterwards.
func swapRelayDemuxGuardForTest(t *testing.T, guard *relayDemuxGuard) {
	t.Helper()
	previous := relayDemuxGuardState.Swap(guard)
	t.Cleanup(func() { relayDemuxGuardState.Store(previous) })
}

func TestRelayDemuxSourceKeysByRelayHost(t *testing.T) {
	source, remote := relayDemuxSource("http://127.0.0.1:9100/source/tok-123/movie.mkv")
	if !remote || source != "http://127.0.0.1:9100" {
		t.Fatalf("relay source = %q (remote %v), want the scheme and host only", source, remote)
	}
	// Every registration a session opens goes through the same relay listener,
	// so the per-registration token must not split the budget.
	other, _ := relayDemuxSource("http://127.0.0.1:9100/source/tok-456/other.mkv")
	if other != source {
		t.Fatalf("second registration keyed as %q, want %q", other, source)
	}
	for _, local := range []string{"/media/library/movie.mkv", "", "virtual://movie/tt-1"} {
		if _, ok := relayDemuxSource(local); ok {
			t.Fatalf("local input %q was treated as a relay read", local)
		}
	}
}

func TestRelayDemuxGuardSkipsDetachedWhenItsBudgetIsTaken(t *testing.T) {
	guard := newRelayDemuxGuard(1, 5*time.Millisecond)
	const remote = "http://relay-saturated.test/source/tok/movie.mkv"

	release, ok := guard.acquire(context.Background(), remote, relayDemuxDetached)
	if !ok {
		t.Fatal("first detached read was not admitted to an idle source")
	}
	if _, ok := guard.acquire(context.Background(), remote, relayDemuxDetached); ok {
		t.Fatal("detached read was admitted while the detached budget was taken")
	}
	// Detached work never consumes a request-path slot: a foreground read is
	// admitted even while the background budget is exhausted.
	if held, ok := guard.acquire(context.Background(), remote, relayDemuxRequestPath); !ok {
		t.Fatal("request-path read was blocked by a detached holder")
	} else {
		held()
	}
	// A different relay source has its own budgets.
	if _, ok := guard.acquire(context.Background(), "http://other-relay.test/source/tok/movie.mkv", relayDemuxDetached); !ok {
		t.Fatal("detached read was refused on an unrelated relay source")
	}
	// A local path is never admitted at all.
	if _, ok := guard.acquire(context.Background(), "/media/library/movie.mkv", relayDemuxDetached); !ok {
		t.Fatal("local read was gated by the relay demux guard")
	}
	release()
	// Releasing returns the slot to the pool.
	if _, ok := guard.acquire(context.Background(), remote, relayDemuxDetached); !ok {
		t.Fatal("detached read was refused after the slot was released")
	}
}

// A foreground window must be admitted while only detached work holds the
// source, and request-path saturation must still be bounded by its own pool.
func TestRelayDemuxGuardRequestPathIsNotStarvedByDetachedWork(t *testing.T) {
	guard := newRelayDemuxGuardWithDetached(2, 1, 20*time.Millisecond)
	const remote = "http://relay-priority.test/source/tok/movie.mkv"

	releaseDetached, ok := guard.acquire(context.Background(), remote, relayDemuxDetached)
	if !ok {
		t.Fatal("detached holder was not admitted")
	}
	defer releaseDetached()

	for slot := 0; slot < 2; slot++ {
		if _, ok := guard.acquire(context.Background(), remote, relayDemuxRequestPath); !ok {
			t.Fatalf("foreground slot %d was blocked by detached work", slot)
		}
	}
	// The foreground pool is now full; a third request-path read waits its
	// bound and answers busy without touching the detached holder.
	start := time.Now()
	if _, ok := guard.acquire(context.Background(), remote, relayDemuxRequestPath); ok {
		t.Fatal("request-path read exceeded its own slot budget")
	}
	if waited := time.Since(start); waited < 15*time.Millisecond {
		t.Fatalf("request-path read gave up after %v, want it to wait its short bound", waited)
	}
}

func TestRelayDemuxGuardRequestPathWaitsThenAnswersBusy(t *testing.T) {
	guard := newRelayDemuxGuard(1, 30*time.Millisecond)
	const remote = "http://relay-wait.test/source/tok/movie.mkv"

	release, ok := guard.acquire(context.Background(), remote, relayDemuxRequestPath)
	if !ok {
		t.Fatal("first read was not admitted")
	}
	start := time.Now()
	if _, ok := guard.acquire(context.Background(), remote, relayDemuxRequestPath); ok {
		t.Fatal("request-path read was admitted to a saturated source")
	}
	if waited := time.Since(start); waited < 20*time.Millisecond {
		t.Fatalf("request-path read gave up after %v, want it to wait the short bound", waited)
	}

	// A slot that frees inside the bound is taken instead of answering busy.
	free := time.AfterFunc(5*time.Millisecond, release)
	defer free.Stop()
	if held, ok := guard.acquire(context.Background(), remote, relayDemuxRequestPath); !ok {
		t.Fatal("request-path read did not take a slot freed inside its wait bound")
	} else {
		held()
	}
}

func TestAcquireRelayDemuxReadNestedSlotIsFree(t *testing.T) {
	swapRelayDemuxGuardForTest(t, newRelayDemuxGuard(1, 5*time.Millisecond))
	const remote = "http://relay-nested.test/source/tok/movie.mkv"

	admitted, release, err := acquireRelayDemuxRead(context.Background(), remote)
	if err != nil {
		t.Fatalf("outer admission failed: %v", err)
	}
	defer release()

	// A probe inside an admitted warm must not take a second slot (with one
	// slot that would deadlock against its own parent).
	if _, nestedRelease, err := acquireRelayDemuxRead(admitted, remote); err != nil {
		t.Fatalf("nested admission failed: %v", err)
	} else {
		nestedRelease()
	}
	if _, _, err := acquireRelayDemuxRead(context.Background(), remote); !errors.Is(err, ErrRelayDemuxBusy) {
		t.Fatalf("second independent admission error = %v, want ErrRelayDemuxBusy", err)
	}
}

func TestAcquireRelayDemuxReadAdmitsLocalWithoutSlot(t *testing.T) {
	swapRelayDemuxGuardForTest(t, newRelayDemuxGuard(1, 5*time.Millisecond))
	const remote = "http://relay-local.test/source/tok/movie.mkv"
	_, release, err := acquireRelayDemuxRead(context.Background(), remote)
	if err != nil {
		t.Fatalf("outer admission failed: %v", err)
	}
	defer release()

	admitted, localRelease, err := acquireRelayDemuxRead(context.Background(), "/media/library/movie.mkv")
	if err != nil {
		t.Fatalf("local read was gated: %v", err)
	}
	localRelease()
	if admitted.Value(relayDemuxSlotKey{}) != nil {
		t.Fatal("local read was decorated with a relay slot")
	}
}

// A request-path window serve that cannot get a relay slot must answer the
// existing retryable shape instead of stacking a second demux on a saturated
// pipe, and it must do so before any response bytes are written.
func TestServeExtractWindowAnswersRetryableWhenRelaySaturated(t *testing.T) {
	dir := t.TempDir()
	swapRelayDemuxGuardForTest(t, newRelayDemuxGuard(1, 5*time.Millisecond))
	const remote = "http://relay-window-saturated.test/source/tok/movie.mkv"
	holdRelaySlot(t, remote)

	cache := NewSubtitleCache(func() string { return dir })
	var calls atomic.Int32
	extract := func(_ context.Context, opts StreamExtractOpts) error {
		calls.Add(1)
		_, err := io.WriteString(opts.Writer, "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nwindow\n")
		return err
	}
	opts := StreamExtractOpts{
		InputPath:             remote,
		CacheIdentity:         "relay-window-saturated",
		TrackIndex:            0,
		SourceCodec:           "subrip",
		SeekSeconds:           600,
		DurationSeconds:       600,
		WindowRequested:       true,
		DisableBackgroundWarm: true,
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/subtitle?position=600&duration=600", nil)

	_, err := cache.ServeExtractWithResult(rec, req, opts, extract)
	var retry *SubtitleExtractionRetryError
	if !errors.As(err, &retry) {
		t.Fatalf("saturated window serve error = %v, want a retryable answer", err)
	}
	if calls.Load() != 0 {
		t.Fatal("saturated window serve spawned an extraction")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("saturated window serve wrote %d body bytes", rec.Body.Len())
	}

	// A local input is outside the guard and still serves.
	local := opts
	local.InputPath = filepath.Join(dir, "movie.mkv")
	local.CacheIdentity = "relay-window-local"
	rec = httptest.NewRecorder()
	if _, err := cache.ServeExtractWithResult(rec, req, local, extract); err != nil {
		t.Fatalf("local window serve failed: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("local window extracts = %d, want 1", calls.Load())
	}
	if !strings.Contains(rec.Body.String(), "WEBVTT") {
		t.Fatalf("local window body = %q", rec.Body.String())
	}
}

// A detached warm never queues behind a taken detached budget: it skips, and
// the next trigger re-attempts it once the background budget frees up.
func TestWarmTrackInBackgroundSkipsWhenRelaySaturated(t *testing.T) {
	dir := t.TempDir()
	swapRelayDemuxGuardForTest(t, newRelayDemuxGuard(1, 5*time.Millisecond))
	const remote = "http://relay-warm-saturated.test/source/tok/movie.mkv"
	release := holdDetachedRelaySlot(t, remote)

	cache := NewSubtitleCache(func() string { return dir })
	var calls atomic.Int32
	extract := func(_ context.Context, opts StreamExtractOpts) error {
		calls.Add(1)
		_, err := io.WriteString(opts.Writer, "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nwarm\n")
		return err
	}
	opts := StreamExtractOpts{
		InputPath:     remote,
		CacheIdentity: "relay-warm-saturated",
		TrackIndex:    0,
		SourceCodec:   "subrip",
	}

	select {
	case <-cache.WarmTrackInBackground(opts, extract, ""):
	case <-time.After(2 * time.Second):
		t.Fatal("detached warm queued behind a saturated relay instead of skipping")
	}
	if calls.Load() != 0 {
		t.Fatal("detached warm extracted on a saturated relay source")
	}
	if cache.HasCommittedTextEntry(remote, opts.CacheIdentity, 0, "subrip", "") {
		t.Fatal("skipped warm committed an entry")
	}

	// The next trigger after the pipe frees extracts and commits.
	release()
	select {
	case <-cache.WarmTrackInBackground(opts, extract, ""):
	case <-time.After(5 * time.Second):
		t.Fatal("detached warm never settled after the relay freed up")
	}
	if calls.Load() != 1 {
		t.Fatalf("warm extracts after the slot was released = %d, want 1", calls.Load())
	}
	if !cache.HasCommittedTextEntry(remote, opts.CacheIdentity, 0, "subrip", "") {
		t.Fatal("warm did not commit after the relay freed up")
	}
}

// A font extraction is admitted like any other relay read, and a local font
// source is not gated at all.
func TestFontExtractionSkipsWhenRelaySaturated(t *testing.T) {
	swapRelayDemuxGuardForTest(t, newRelayDemuxGuard(1, 5*time.Millisecond))
	const remote = "http://relay-fonts-saturated.test/source/tok/movie.mkv"
	holdDetachedRelaySlot(t, remote)

	_, err := ExtractAttachedSubtitleFonts(detachedRelayDemux(context.Background()), remote, filepath.Join(t.TempDir(), "ffmpeg-missing"))
	if !errors.Is(err, ErrRelayDemuxBusy) {
		t.Fatalf("saturated detached font read error = %v, want ErrRelayDemuxBusy", err)
	}

	local := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(local, []byte("not a container"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = ExtractAttachedSubtitleFonts(detachedRelayDemux(context.Background()), local, filepath.Join(t.TempDir(), "ffmpeg-missing"))
	if errors.Is(err, ErrRelayDemuxBusy) {
		t.Fatal("local font read was gated by the relay demux guard")
	}
}

// The param-less whole-track probe draws from the detached budget: even with a
// foreground slot already serving another window, the probe is admitted and a
// concurrent window still gets its own slot. The probe can never deny a window.
func TestParamlessProbeExtractNeverDeniesAWindow(t *testing.T) {
	dir := t.TempDir()
	swapRelayDemuxGuardForTest(t, newRelayDemuxGuardWithDetached(2, 1, 5*time.Millisecond))
	const remote = "http://relay-probe-window.test/source/tok/movie.mkv"

	// One foreground slot already serves another client's window.
	_, releaseWindow, err := acquireRelayDemuxRead(context.Background(), remote)
	if err != nil {
		t.Fatalf("hold foreground slot: %v", err)
	}
	defer releaseWindow()

	cache := NewSubtitleCache(func() string { return dir })
	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	var probeCalls, windowCalls atomic.Int32
	probeExtract := func(_ context.Context, opts StreamExtractOpts) error {
		probeCalls.Add(1)
		close(probeStarted)
		<-releaseProbe
		_, writeErr := io.WriteString(opts.Writer, "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nprobe\n")
		return writeErr
	}
	probeOpts := StreamExtractOpts{
		InputPath:          remote,
		CacheIdentity:      "relay-probe-window",
		TrackIndex:         0,
		SourceCodec:        "subrip",
		DetachedRelayDemux: true,
	}
	probeDone := make(chan error, 1)
	go func() {
		_, err := cache.ServeExtractWithResult(
			httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "/subtitle", nil),
			probeOpts, probeExtract,
		)
		probeDone <- err
	}()
	select {
	case <-probeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("param-less probe never reached its extract while a foreground slot was busy")
	}

	// The probe is holding the detached budget; a window still gets a slot.
	windowOpts := StreamExtractOpts{
		InputPath:       remote,
		CacheIdentity:   "relay-probe-window",
		TrackIndex:      0,
		SourceCodec:     "subrip",
		SeekSeconds:     600,
		DurationSeconds: 600,
		WindowRequested: true,
	}
	windowExtract := func(_ context.Context, opts StreamExtractOpts) error {
		windowCalls.Add(1)
		_, writeErr := io.WriteString(opts.Writer, "WEBVTT\n\n00:10:00.000 --> 00:10:01.000\nwindow\n")
		return writeErr
	}
	windowRec := httptest.NewRecorder()
	if _, err := cache.ServeExtractWithResult(
		windowRec,
		httptest.NewRequest(http.MethodGet, "/subtitle?position=600&duration=600", nil),
		windowOpts, windowExtract,
	); err != nil {
		t.Fatalf("window serve denied while the probe held the detached budget: %v", err)
	}
	if windowCalls.Load() != 1 {
		t.Fatalf("window extracts = %d, want 1", windowCalls.Load())
	}

	close(releaseProbe)
	if err := <-probeDone; err != nil {
		t.Fatalf("admitted probe serve failed: %v", err)
	}
	if probeCalls.Load() != 1 {
		t.Fatalf("probe extracts = %d, want 1", probeCalls.Load())
	}
}

// When the detached budget is taken, the param-less probe is skipped for this
// round (the client's probe cadence re-issues it) rather than waiting on or
// occupying a foreground slot.
func TestParamlessProbeSkipsWhenDetachedBudgetTaken(t *testing.T) {
	dir := t.TempDir()
	swapRelayDemuxGuardForTest(t, newRelayDemuxGuardWithDetached(2, 1, 5*time.Millisecond))
	const remote = "http://relay-probe-skip.test/source/tok/movie.mkv"
	holdDetachedRelaySlot(t, remote)

	cache := NewSubtitleCache(func() string { return dir })
	var calls atomic.Int32
	extract := func(_ context.Context, opts StreamExtractOpts) error {
		calls.Add(1)
		_, err := io.WriteString(opts.Writer, "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nprobe\n")
		return err
	}
	opts := StreamExtractOpts{
		InputPath:          remote,
		CacheIdentity:      "relay-probe-skip",
		TrackIndex:         0,
		SourceCodec:        "subrip",
		DetachedRelayDemux: true,
	}
	rec := httptest.NewRecorder()
	_, err := cache.ServeExtractWithResult(rec, httptest.NewRequest(http.MethodGet, "/subtitle", nil), opts, extract)
	var retry *SubtitleExtractionRetryError
	if !errors.As(err, &retry) {
		t.Fatalf("probe on a taken detached budget error = %v, want a retryable answer", err)
	}
	if calls.Load() != 0 {
		t.Fatal("probe spawned an extraction while the detached budget was taken")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("skipped probe wrote %d body bytes", rec.Body.Len())
	}
}

// holdRelaySlot occupies a relay-source slot for the remainder of the test and
// returns a release that is safe to call early.
func holdRelaySlot(t *testing.T, input string) func() {
	t.Helper()
	_, release, err := acquireRelayDemuxRead(context.Background(), input)
	if err != nil {
		t.Fatalf("hold relay slot: %v", err)
	}
	var released atomic.Bool
	return func() {
		if released.CompareAndSwap(false, true) {
			release()
		}
	}
}

// holdDetachedRelaySlot occupies the detached relay budget for the remainder of
// the test.
func holdDetachedRelaySlot(t *testing.T, input string) func() {
	t.Helper()
	_, release, err := acquireRelayDemuxRead(detachedRelayDemux(context.Background()), input)
	if err != nil {
		t.Fatalf("hold detached relay slot: %v", err)
	}
	var released atomic.Bool
	return func() {
		if released.CompareAndSwap(false, true) {
			release()
		}
	}
}
