package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// gatedPlaybackProbeEnsurer blocks the copy-safety call until it is released or
// its context is done, standing in for the multi-second remote reads a cold
// probe performs on a remote library. It records whether the detached repair
// was canceled, which is what proves a client disconnect cannot poison it.
type gatedPlaybackProbeEnsurer struct {
	started  chan struct{}
	release  chan struct{}
	calls    atomic.Int32
	canceled atomic.Bool
	err      error
}

func newGatedPlaybackProbeEnsurer() *gatedPlaybackProbeEnsurer {
	return &gatedPlaybackProbeEnsurer{started: make(chan struct{}, 8), release: make(chan struct{})}
}

func (e *gatedPlaybackProbeEnsurer) EnsureProbeOnly(_ context.Context, file *models.MediaFile) (*models.MediaFile, error) {
	return file, nil
}

func (e *gatedPlaybackProbeEnsurer) EnsureCopySafetyCached(ctx context.Context, file *models.MediaFile) (*models.MediaFile, error) {
	e.calls.Add(1)
	select {
	case e.started <- struct{}{}:
	default:
	}
	if e.err != nil {
		return file, e.err
	}
	select {
	case <-e.release:
		return file, nil
	case <-ctx.Done():
		e.canceled.Store(true)
		return file, ctx.Err()
	}
}

// immediateRepairProbeEnsurer returns a repaired row synchronously, standing in
// for the cached/fast repair a start must still be able to consume before
// planning.
type immediateRepairProbeEnsurer struct {
	repaired *models.MediaFile
}

func (e *immediateRepairProbeEnsurer) EnsureProbeOnly(_ context.Context, file *models.MediaFile) (*models.MediaFile, error) {
	return file, nil
}

func (e *immediateRepairProbeEnsurer) EnsureCopySafetyCached(_ context.Context, _ *models.MediaFile) (*models.MediaFile, error) {
	return e.repaired, nil
}

// A start must not wait unbounded for a slow probe: it serves the row's known
// metadata after the bounded budget while the repair continues in the
// background.
func TestEnsurePlaybackProbeStartReturnsBoundedOnSlowProbe(t *testing.T) {
	ensurer := newGatedPlaybackProbeEnsurer()
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	h.ProbeEnsurer = ensurer
	h.probeStartBudget = 40 * time.Millisecond
	file := &models.MediaFile{ID: 77, FilePath: "/library/slow.mkv", FileSize: 1024}

	start := time.Now()
	got := h.ensurePlaybackProbeStart(context.Background(), file)
	elapsed := time.Since(start)

	if got != file {
		t.Fatalf("slow probe changed the served file: got %p want %p", got, file)
	}
	if elapsed > time.Second {
		t.Fatalf("start probe blocked %s, want at most the %s budget", elapsed, h.probeStartBudget)
	}

	select {
	case <-ensurer.started:
	case <-time.After(2 * time.Second):
		t.Fatal("background probe repair never started")
	}
	close(ensurer.release)
	h.probeRefreshWG.Wait()
	if got := ensurer.calls.Load(); got != 1 {
		t.Fatalf("background probe called %d times, want 1", got)
	}
}

// A fast repair still lands before planning: the bounded wait honors the
// ensurer's repaired row instead of always discarding it.
func TestEnsurePlaybackProbeStartHonorsFastRepair(t *testing.T) {
	file := &models.MediaFile{ID: 78, FilePath: "/library/fast.mkv", FileSize: 1024}
	repaired := *file
	repaired.Duration = 999
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	h.ProbeEnsurer = &immediateRepairProbeEnsurer{repaired: &repaired}

	got := h.ensurePlaybackProbeStart(context.Background(), file)
	if got != &repaired {
		t.Fatalf("start probe = %+v, want the repaired row", got)
	}
	h.probeRefreshWG.Wait()
}

// A probe error must fail open: start still succeeds from known metadata.
func TestEnsurePlaybackProbeStartFailsOpenOnProbeError(t *testing.T) {
	ensurer := newGatedPlaybackProbeEnsurer()
	ensurer.err = errors.New("probe failed")
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	h.ProbeEnsurer = ensurer
	h.probeStartBudget = time.Second
	file := &models.MediaFile{ID: 79, FilePath: "/library/fail.mkv", FileSize: 1024}

	if got := h.ensurePlaybackProbeStart(context.Background(), file); got != file {
		t.Fatalf("probe error changed the served file: got %p want %p", got, file)
	}
	h.probeRefreshWG.Wait()
	if got := ensurer.calls.Load(); got != 1 {
		t.Fatalf("background probe called %d times, want 1", got)
	}

	// The failed refresh dropped its claim, so a later start retries.
	if got := h.ensurePlaybackProbeStart(context.Background(), file); got != file {
		t.Fatalf("retry changed the served file: got %p want %p", got, file)
	}
	h.probeRefreshWG.Wait()
	if got := ensurer.calls.Load(); got != 2 {
		t.Fatalf("failed refresh suppressed the retry: probe called %d times, want 2", got)
	}
}

// Repeated and concurrent starts for one unchanged file schedule one repair,
// and a later start of the prepared generation does not re-queue it.
func TestEnsurePlaybackProbeStartSchedulesOneRefreshPerFile(t *testing.T) {
	ensurer := newGatedPlaybackProbeEnsurer()
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	h.ProbeEnsurer = ensurer
	h.probeStartBudget = 30 * time.Millisecond
	file := &models.MediaFile{ID: 80, FilePath: "/library/once.mkv", FileSize: 1024}

	h.ensurePlaybackProbeStart(context.Background(), file)
	select {
	case <-ensurer.started:
	case <-time.After(2 * time.Second):
		t.Fatal("background probe repair never started")
	}

	h.ensurePlaybackProbeStart(context.Background(), file)
	if got := ensurer.calls.Load(); got != 1 {
		t.Fatalf("two starts called the probe %d times, want 1", got)
	}

	close(ensurer.release)
	h.probeRefreshWG.Wait()

	h.ensurePlaybackProbeStart(context.Background(), file)
	if got := ensurer.calls.Load(); got != 1 {
		t.Fatalf("start of the prepared generation re-queued the probe: called %d times, want 1", got)
	}
}

// A client that disconnects while the probe runs must not cancel the detached
// repair the next start depends on.
func TestEnsurePlaybackProbeStartBackgroundRepairSurvivesClientCancel(t *testing.T) {
	ensurer := newGatedPlaybackProbeEnsurer()
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	h.ProbeEnsurer = ensurer
	h.probeStartBudget = 30 * time.Millisecond
	file := &models.MediaFile{ID: 81, FilePath: "/library/cancel.mkv", FileSize: 1024}

	ctx, cancel := context.WithCancel(context.Background())
	if got := h.ensurePlaybackProbeStart(ctx, file); got != file {
		t.Fatalf("slow probe changed the served file: got %p want %p", got, file)
	}
	select {
	case <-ensurer.started:
	case <-time.After(2 * time.Second):
		t.Fatal("background probe repair never started")
	}

	// The viewer goes away while the repair is still in flight.
	cancel()
	close(ensurer.release)
	h.probeRefreshWG.Wait()

	if ensurer.canceled.Load() {
		t.Fatal("client disconnect canceled the background probe repair")
	}
	if got := ensurer.calls.Load(); got != 1 {
		t.Fatalf("background probe called %d times, want 1", got)
	}
}

// A concurrent start that joins an in-flight refresh must not wait on it.
// Only the refresh owner pays the bounded wait.
func TestEnsurePlaybackProbeStartJoinerReturnsWithoutWaiting(t *testing.T) {
	ensurer := newGatedPlaybackProbeEnsurer()
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	h.ProbeEnsurer = ensurer
	// Long enough that a joiner entering the bounded wait would be obvious.
	h.probeStartBudget = 5 * time.Second
	file := &models.MediaFile{ID: 82, FilePath: "/library/join.mkv", FileSize: 1024}

	// The owner starts the detached refresh and parks on the bounded wait.
	ownerDone := make(chan struct{})
	go func() {
		h.ensurePlaybackProbeStart(context.Background(), file)
		close(ownerDone)
	}()
	select {
	case <-ensurer.started:
	case <-time.After(2 * time.Second):
		t.Fatal("background probe repair never started")
	}

	start := time.Now()
	if got := h.ensurePlaybackProbeStart(context.Background(), file); got != file {
		t.Fatalf("joiner changed the served file: got %p want %p", got, file)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("joiner waited %s on the owner's in-flight refresh, want an immediate return", elapsed)
	}

	close(ensurer.release)
	<-ownerDone
	h.probeRefreshWG.Wait()
	if got := ensurer.calls.Load(); got != 1 {
		t.Fatalf("two starts called the probe %d times, want 1", got)
	}
}

// At the memo ceiling with every entry still live, a new claim is refused: the
// memo does not grow and no detached probe is scheduled.
func TestClaimPlaybackProbeRefreshRefusesWhenMemoFull(t *testing.T) {
	ensurer := newGatedPlaybackProbeEnsurer()
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	h.ProbeEnsurer = ensurer
	h.probeStartBudget = 20 * time.Millisecond

	done := make(chan struct{})
	close(done)
	now := time.Now()
	h.probeRefreshed = make(map[int]*playbackProbeRefresh, playbackProbePreparedMaxEntries)
	for i := 0; i < playbackProbePreparedMaxEntries; i++ {
		h.probeRefreshed[i] = &playbackProbeRefresh{fingerprint: "live", preparedAt: now, done: done}
	}

	file := &models.MediaFile{ID: playbackProbePreparedMaxEntries, FilePath: "/library/new.mkv", FileSize: 1}
	if got := h.ensurePlaybackProbeStart(context.Background(), file); got != file {
		t.Fatalf("saturated memo changed the served file: got %p want %p", got, file)
	}
	if got := len(h.probeRefreshed); got != playbackProbePreparedMaxEntries {
		t.Fatalf("memo size = %d after a claim at the ceiling, want %d", got, playbackProbePreparedMaxEntries)
	}
	if got := ensurer.calls.Load(); got != 0 {
		t.Fatalf("refused claim scheduled %d probe repairs, want 0", got)
	}
}

// Expiry still frees slots at the ceiling: the sweep admits one new claim and
// the memo stays within its bound.
func TestClaimPlaybackProbeRefreshPrunesExpiredAtCeiling(t *testing.T) {
	ensurer := newGatedPlaybackProbeEnsurer()
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	h.ProbeEnsurer = ensurer
	h.probeStartBudget = 20 * time.Millisecond

	done := make(chan struct{})
	close(done)
	stale := time.Now().Add(-2 * playbackProbePreparedTTL)
	h.probeRefreshed = make(map[int]*playbackProbeRefresh, playbackProbePreparedMaxEntries)
	for i := 0; i < playbackProbePreparedMaxEntries; i++ {
		h.probeRefreshed[i] = &playbackProbeRefresh{fingerprint: "stale", preparedAt: stale, done: done}
	}

	file := &models.MediaFile{ID: playbackProbePreparedMaxEntries, FilePath: "/library/retry.mkv", FileSize: 1}
	h.ensurePlaybackProbeStart(context.Background(), file)
	if got := len(h.probeRefreshed); got > playbackProbePreparedMaxEntries {
		t.Fatalf("memo size = %d after admitting past the ceiling, want at most %d", got, playbackProbePreparedMaxEntries)
	}
	select {
	case <-ensurer.started:
	case <-time.After(2 * time.Second):
		t.Fatal("expired-ceiling claim did not schedule a probe")
	}
	close(ensurer.release)
	h.probeRefreshWG.Wait()
}

// The full start handler proves the bound end to end: a blocking probe does not
// hold the POST /playback/start response past its budget.
func TestHandleStartPlaybackV3ReturnsBoundedOnSlowProbe(t *testing.T) {
	sessionMgr := playback.NewSessionManager(0, 0)
	file := v3HandlerFixtureFile(t)
	ensurer := newGatedPlaybackProbeEnsurer()

	handler := NewPlaybackHandler(sessionMgr, testPlaybackFileResolver{file: file})
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{}}
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	handler.ProbeEnsurer = ensurer
	handler.probeStartBudget = 40 * time.Millisecond

	req := httptest.NewRequest(http.MethodPost, "/api/v2/playback/start",
		strings.NewReader(marshalV3StartRequest(t, v3HandlerStartRequest()))).WithContext(newAuthorizedPlaybackContext())
	rr := httptest.NewRecorder()
	start := time.Now()
	handler.HandleStartPlayback(rr, req)
	elapsed := time.Since(start)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if elapsed > time.Second {
		t.Fatalf("start blocked %s on a slow probe, want at most the %s budget", elapsed, handler.probeStartBudget)
	}
	select {
	case <-ensurer.started:
	case <-time.After(2 * time.Second):
		t.Fatal("background probe repair never started")
	}
	close(ensurer.release)
	handler.probeRefreshWG.Wait()
}

// Safe metadata allows immediate startup without waiting on probeStartBudget:
// the start path returns known metadata instantly while repair runs detached.
func TestEnsurePlaybackProbeStartReturnsImmediatelyWhenSafe(t *testing.T) {
	ensurer := newGatedPlaybackProbeEnsurer()
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	h.ProbeEnsurer = ensurer
	h.probeStartBudget = 5 * time.Second

	file := &models.MediaFile{
		ID:         99,
		FilePath:   "/library/safe.mkv",
		FileSize:   1024,
		Duration:   120,
		Container:  "mkv",
		CodecVideo: "h264",
		CodecAudio: "aac",
		Resolution: "1080p",
		Bitrate:    5000,
		VideoTracks: []models.VideoTrack{{
			Codec:     "h264",
			Width:     1920,
			Height:    1080,
			BitDepth:  8,
			Bitrate:   5000,
			FrameRate: "24",
		}},
		AudioTracks: []models.AudioTrack{{
			Codec:    "aac",
			Channels: 2,
		}},
	}

	start := time.Now()
	got := h.ensurePlaybackProbeStart(context.Background(), file)
	elapsed := time.Since(start)

	if got != file {
		t.Fatalf("safe start changed the served file: got %p want %p", got, file)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("safe start waited %s on in-flight refresh, want immediate return", elapsed)
	}

	select {
	case <-ensurer.started:
	case <-time.After(2 * time.Second):
		t.Fatal("background probe repair never started")
	}
	close(ensurer.release)
	h.probeRefreshWG.Wait()
	if got := ensurer.calls.Load(); got != 1 {
		t.Fatalf("background probe called %d times, want 1", got)
	}
}

// When video metadata is complete but audio is unknown on an unprobed file,
// ensurePlaybackProbeStart must not bypass repair: it must wait for the probe
// so the audio codec can be verified before planning.
func TestEnsurePlaybackProbeStartAwaitsWhenAudioUnknown(t *testing.T) {
	ensurer := newGatedPlaybackProbeEnsurer()
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	h.ProbeEnsurer = ensurer
	h.probeStartBudget = 50 * time.Millisecond

	// Video is complete, but audio is completely unknown and unprobed
	file := &models.MediaFile{
		ID:         101,
		FilePath:   "/library/unknown-audio.mkv",
		FileSize:   1024,
		Duration:   120,
		Container:  "mkv",
		CodecVideo: "h264",
		Resolution: "1080p",
		Bitrate:    5000,
		VideoTracks: []models.VideoTrack{{
			Codec:     "h264",
			Width:     1920,
			Height:    1080,
			BitDepth:  8,
			Bitrate:   5000,
			FrameRate: "24",
		}},
		ProbeUpdatedAt: nil,
	}

	start := time.Now()
	got := h.ensurePlaybackProbeStart(context.Background(), file)
	elapsed := time.Since(start)

	if got != file {
		t.Fatalf("unknown-audio start changed file: got %p want %p", got, file)
	}
	// Must have waited for the budget (50ms) rather than returning immediately (<10ms)
	if elapsed < 40*time.Millisecond {
		t.Fatalf("unknown audio returned in %s, must wait for probe budget", elapsed)
	}

	select {
	case <-ensurer.started:
	case <-time.After(2 * time.Second):
		t.Fatal("background probe repair never started")
	}
	close(ensurer.release)
	h.probeRefreshWG.Wait()
}

// When an in-flight repair completes, a joiner immediately honors the repaired
// row without waiting or re-running the probe.
func TestEnsurePlaybackProbeStartJoinerHonorsCompletedRepair(t *testing.T) {
	file := &models.MediaFile{ID: 102, FilePath: "/library/join-complete.mkv", FileSize: 1024}
	repaired := *file
	repaired.Duration = 120
	repaired.CodecVideo = "h264"
	repaired.CodecAudio = "aac"

	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	h.ProbeEnsurer = &immediateRepairProbeEnsurer{repaired: &repaired}

	// Prime the memo with completed repair
	gotFirst := h.ensurePlaybackProbeStart(context.Background(), file)
	if gotFirst != &repaired {
		t.Fatalf("first start = %+v, want repaired %+v", gotFirst, &repaired)
	}

	// Second start is a joiner/subsequent start on the same generation:
	// must immediately return repaired without waiting
	gotSecond := h.ensurePlaybackProbeStart(context.Background(), file)
	if gotSecond != &repaired {
		t.Fatalf("second start = %+v, want repaired %+v", gotSecond, &repaired)
	}
}
