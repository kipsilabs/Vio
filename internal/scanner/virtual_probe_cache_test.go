package scanner

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

func TestVirtualProbeCacheSeparatesResolvedQueriesAndReturnsClones(t *testing.T) {
	cache := NewVirtualProbeCache(time.Minute, 4)
	file := &models.MediaFile{ID: 7, FilePath: "virtual://movie/tt7?profile=1080p"}
	var calls atomic.Int32
	probe := func(_ context.Context, _ string, input *models.MediaFile) (*models.MediaFile, error) {
		calls.Add(1)
		result := *input
		result.CodecAudio = "aac"
		result.AudioTracks = []models.AudioTrack{{Codec: "aac"}}
		return &result, nil
	}
	first, err := cache.Probe(context.Background(), "https://provider.example/video.mkv?token=one", file, probe)
	if err != nil {
		t.Fatal(err)
	}
	first.CodecAudio = "mutated"
	second, err := cache.Probe(context.Background(), "https://provider.example/video.mkv?token=two", file, probe)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || second.CodecAudio != "aac" {
		t.Fatalf("calls=%d cached codec=%q", calls.Load(), second.CodecAudio)
	}
}

func TestVirtualProbeCacheReusesRotatingCredentialsForExactResult(t *testing.T) {
	cache := NewVirtualProbeCache(time.Minute, 4)
	file := &models.MediaFile{ID: 7, FilePath: "virtual://movie/tt7?profile=1080p&result=stable-result", VirtualOwnerInstallationID: 9}
	var calls atomic.Int32
	probe := func(_ context.Context, _ string, input *models.MediaFile) (*models.MediaFile, error) {
		calls.Add(1)
		result := *input
		result.CodecAudio = "aac"
		return &result, nil
	}
	if _, err := cache.Probe(context.Background(), "https://provider.example/video.mkv?token=one", file, probe); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Probe(context.Background(), "https://provider.example/video.mkv?token=two", file, probe); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("probe calls=%d, want 1 for the same exact result", calls.Load())
	}
	if _, err := cache.Probe(context.Background(), "https://provider.example/different.mkv?token=three", file, probe); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("probe calls=%d, want 2 after provider path changed", calls.Load())
	}
}

// TestVirtualProbeCacheSeparatesExpectedAudioExpectations pins the completeness
// gate against a cache replay. A video-with-subtitles-but-no-audio enumeration
// is a complete answer for an item that declares no audio, so it is cached; it
// is a partial read for an item that declares audio, so that expectation must
// re-run the real probe (and see the gate's ErrVirtualProbePartial) instead of
// being served the thin entry. Without the expected-audio signal in the key the
// second probe returns the first result as verified and the gate never runs.
func TestVirtualProbeCacheSeparatesExpectedAudioExpectations(t *testing.T) {
	cache := NewVirtualProbeCache(time.Minute, 8)
	base := &models.MediaFile{ID: 7, FilePath: "virtual://movie/tt7?result=stable-result", VirtualOwnerInstallationID: 9}
	thin := &models.MediaFile{
		ID:             base.ID,
		FilePath:       base.FilePath,
		VideoTracks:    []models.VideoTrack{{Codec: "hevc"}},
		SubtitleTracks: []models.SubtitleTrack{{Index: 2, Codec: "ass", Language: "fre"}},
	}
	var videoOnlyCalls atomic.Int32
	if _, err := cache.Probe(context.Background(), "https://provider.example/video.mkv?token=one", base, func(_ context.Context, _ string, _ *models.MediaFile) (*models.MediaFile, error) {
		videoOnlyCalls.Add(1)
		return cloneVirtualProbeFile(thin), nil
	}); err != nil {
		t.Fatalf("video-only probe: %v", err)
	}
	if videoOnlyCalls.Load() != 1 {
		t.Fatalf("video-only probe calls=%d, want 1", videoOnlyCalls.Load())
	}

	declared := cloneVirtualProbeFile(base)
	declared.CodecAudio = "eac3"
	var declaredCalls atomic.Int32
	_, err := cache.Probe(context.Background(), "https://provider.example/video.mkv?token=two", declared, func(_ context.Context, _ string, _ *models.MediaFile) (*models.MediaFile, error) {
		declaredCalls.Add(1)
		return nil, ErrVirtualProbePartial
	})
	if declaredCalls.Load() != 1 {
		t.Fatalf("declared-audio probe calls=%d, want 1: the cache replayed the video-only result and bypassed the gate", declaredCalls.Load())
	}
	if !errors.Is(err, ErrVirtualProbePartial) {
		t.Fatalf("declared-audio probe error = %v, want ErrVirtualProbePartial", err)
	}

	// The same expectation still reuses its own entry, so fast startup is not
	// traded away for the gate: a complete declared-audio probe is measured once
	// and served from cache afterwards.
	var repeatCalls atomic.Int32
	complete := cloneVirtualProbeFile(thin)
	complete.AudioTracks = []models.AudioTrack{{Index: 1, Codec: "eac3", Channels: 6}}
	reprobe := func(_ context.Context, _ string, _ *models.MediaFile) (*models.MediaFile, error) {
		repeatCalls.Add(1)
		return cloneVirtualProbeFile(complete), nil
	}
	if _, err := cache.Probe(context.Background(), "https://provider.example/video.mkv?token=three", declared, reprobe); err != nil {
		t.Fatalf("declared probe: %v", err)
	}
	if _, err := cache.Probe(context.Background(), "https://provider.example/video.mkv?token=four", declared, reprobe); err != nil {
		t.Fatalf("repeat declared probe: %v", err)
	}
	if repeatCalls.Load() != 1 {
		t.Fatalf("declared-audio probe calls=%d, want 1 for the same expectation (cache disabled fast startup)", repeatCalls.Load())
	}
}

// A probe that read a different release's bytes must not be cached: the
// playback path rejects it, and a cached entry would keep replaying that
// rejection for the cache TTL without ever reaching fresh provider bytes.
func TestVirtualProbeCacheDoesNotStoreServedSizeMismatch(t *testing.T) {
	cache := NewVirtualProbeCache(time.Minute, 4)
	file := &models.MediaFile{ID: 7, FilePath: "virtual://movie/tt7", FileSize: 8_449_999_999}
	var mismatchedCalls atomic.Int32
	mismatched := func(_ context.Context, _ string, input *models.MediaFile) (*models.MediaFile, error) {
		mismatchedCalls.Add(1)
		result := *input
		result.ProbedFormatSize = 7_427_636_945
		return &result, nil
	}
	if _, err := cache.Probe(context.Background(), "https://provider.example/video.mkv", file, mismatched); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Probe(context.Background(), "https://provider.example/video.mkv", file, mismatched); err != nil {
		t.Fatal(err)
	}
	if mismatchedCalls.Load() != 2 {
		t.Fatalf("mismatched probe calls=%d, want 2: the wrong file was cached", mismatchedCalls.Load())
	}
	if cached := cache.Lookup("https://provider.example/video.mkv", file); cached != nil {
		t.Fatalf("wrong-file probe result was cached: %#v", cached)
	}

	// A matching size is cached normally.
	var matchedCalls atomic.Int32
	matched := func(_ context.Context, _ string, input *models.MediaFile) (*models.MediaFile, error) {
		matchedCalls.Add(1)
		result := *input
		result.ProbedFormatSize = 8_449_000_000
		return &result, nil
	}
	if _, err := cache.Probe(context.Background(), "https://provider.example/video.mkv", file, matched); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Probe(context.Background(), "https://provider.example/video.mkv", file, matched); err != nil {
		t.Fatal(err)
	}
	if matchedCalls.Load() != 1 {
		t.Fatalf("matching probe calls=%d, want 1 from the cache", matchedCalls.Load())
	}
}

// A short-posted release can still be the right content, so its bytes stay
// playable for the current request. A plausible duration does not prove which
// release was served, though, so the evidence is unverified and must not be
// cached as the row's inventory: the playback path would then reuse unproven
// tracks and sizing for the whole TTL.
func TestVirtualProbeCacheDoesNotStoreUnverifiedSizeMismatch(t *testing.T) {
	cache := NewVirtualProbeCache(time.Minute, 4)
	file := &models.MediaFile{
		ID: 7, FilePath: "virtual://movie/tt7", FileSize: 6_330_000_000,
		VirtualExpectedRuntimeMinutes: 51,
	}
	var calls atomic.Int32
	probe := func(_ context.Context, _ string, input *models.MediaFile) (*models.MediaFile, error) {
		calls.Add(1)
		result := *input
		result.ProbedFormatSize = 5_570_000_000 // 12% short posting
		result.Duration = 3072                  // 51.2 minutes: plausible, not proof
		return &result, nil
	}
	for i := 0; i < 2; i++ {
		probed, err := cache.Probe(context.Background(), "https://provider.example/video.mkv", file, probe)
		if err != nil {
			t.Fatal(err)
		}
		if probed == nil || probed.ProbedFormatSize != 5_570_000_000 {
			t.Fatalf("probe %d: unverified evidence must still be returned playable, got %+v", i, probed)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("unverified probe calls=%d, want 2: unverified evidence must not be cached", calls.Load())
	}
	if cached := cache.Lookup("https://provider.example/video.mkv", file); cached != nil {
		t.Fatal("unverified size mismatch was cached as the row's inventory")
	}
}

// The cache mirrors the persist path's rejection rule: a size mismatch is only a
// wrong file when the duration cannot prove identity, so unknown or implausible
// duration evidence is never cached.
func TestVirtualProbeCacheDoesNotStoreServedSizeMismatchWithoutDurationProof(t *testing.T) {
	tests := []struct {
		name          string
		expectedMins  int
		probedSeconds int
	}{
		{name: "expected runtime unknown", expectedMins: 0, probedSeconds: 3072},
		{name: "probed duration unknown", expectedMins: 39, probedSeconds: 0},
		{name: "probed duration implausible", expectedMins: 39, probedSeconds: 3072},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache := NewVirtualProbeCache(time.Minute, 4)
			file := &models.MediaFile{
				ID: 7, FilePath: "virtual://movie/tt7", FileSize: 8_449_999_999,
				VirtualExpectedRuntimeMinutes: tt.expectedMins,
			}
			var calls atomic.Int32
			probe := func(_ context.Context, _ string, input *models.MediaFile) (*models.MediaFile, error) {
				calls.Add(1)
				result := *input
				result.ProbedFormatSize = 7_427_636_945
				result.Duration = tt.probedSeconds
				return &result, nil
			}
			if _, err := cache.Probe(context.Background(), "https://provider.example/video.mkv", file, probe); err != nil {
				t.Fatal(err)
			}
			if _, err := cache.Probe(context.Background(), "https://provider.example/video.mkv", file, probe); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 2 {
				t.Fatalf("probe calls=%d, want 2: unproven wrong-file evidence was cached", calls.Load())
			}
			if cached := cache.Lookup("https://provider.example/video.mkv", file); cached != nil {
				t.Fatalf("unproven wrong-file probe result was cached: %#v", cached)
			}
		})
	}
}

func TestVirtualProbeCacheIsBounded(t *testing.T) {
	cache := NewVirtualProbeCache(time.Minute, 2)
	probe := func(_ context.Context, _ string, input *models.MediaFile) (*models.MediaFile, error) {
		return input, nil
	}
	for id := 1; id <= 3; id++ {
		file := &models.MediaFile{ID: id, FilePath: "virtual://movie/tt"}
		if _, err := cache.Probe(context.Background(), "https://provider.example/video", file, probe); err != nil {
			t.Fatal(err)
		}
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.entries) != 2 {
		t.Fatalf("cache entries=%d, want 2", len(cache.entries))
	}
}

func TestVirtualProbeCacheConcurrentSingleflightDistinctClones(t *testing.T) {
	cache := NewVirtualProbeCache(time.Minute, 4)
	boolVal := true
	file := &models.MediaFile{
		ID:       8,
		FilePath: "virtual://movie/tt8",
		VideoTracks: []models.VideoTrack{{
			Codec:               "h264",
			MultiplePPS:         &boolVal,
			DVRPUStrippable:     &boolVal,
			DVProvenanceCurrent: &boolVal,
		}},
	}
	workerStarted := make(chan struct{})
	releaseWorker := make(chan struct{})
	probe := func(_ context.Context, _ string, input *models.MediaFile) (*models.MediaFile, error) {
		close(workerStarted)
		<-releaseWorker
		result := *input
		result.CodecAudio = "aac"
		result.AudioTracks = []models.AudioTrack{{Codec: "aac"}}
		return &result, nil
	}

	type res struct {
		file *models.MediaFile
		err  error
	}
	ch1 := make(chan res, 1)
	ch2 := make(chan res, 1)

	go func() {
		f, err := cache.Probe(context.Background(), "https://provider.example/v.mp4", file, probe)
		ch1 <- res{file: f, err: err}
	}()

	<-workerStarted

	go func() {
		f, err := cache.Probe(context.Background(), "https://provider.example/v.mp4", file, probe)
		ch2 <- res{file: f, err: err}
	}()

	time.Sleep(20 * time.Millisecond)
	close(releaseWorker)

	r1 := <-ch1
	r2 := <-ch2
	if r1.err != nil || r2.err != nil {
		t.Fatalf("unexpected error: r1=%v, r2=%v", r1.err, r2.err)
	}
	if r1.file == r2.file {
		t.Fatal("concurrent singleflight callers received the same pointer instead of distinct clones")
	}
	if len(r1.file.VideoTracks) == 0 || len(r2.file.VideoTracks) == 0 {
		t.Fatal("expected video tracks in both results")
	}
	if r1.file.VideoTracks[0].MultiplePPS == r2.file.VideoTracks[0].MultiplePPS {
		t.Fatal("nested pointer MultiplePPS was shared instead of deeply cloned")
	}
	*r1.file.VideoTracks[0].MultiplePPS = false
	if !*r2.file.VideoTracks[0].MultiplePPS {
		t.Fatal("mutating r1 pointer affected r2 pointer")
	}
}

func TestVirtualProbeCacheLeaderCancellationDoesNotPoisonFollower(t *testing.T) {
	cache := NewVirtualProbeCache(time.Minute, 4)
	file1 := &models.MediaFile{ID: 9, FilePath: "virtual://movie/tt9"}
	file2 := &models.MediaFile{ID: 9, FilePath: "virtual://movie/tt9"}
	workerStarted := make(chan struct{})
	releaseWorker := make(chan struct{})
	probe := func(probeCtx context.Context, _ string, input *models.MediaFile) (*models.MediaFile, error) {
		close(workerStarted)
		select {
		case <-releaseWorker:
		case <-probeCtx.Done():
			return nil, probeCtx.Err()
		}
		result := *input
		result.CodecAudio = "eac3"
		return &result, nil
	}

	ctx1, cancel1 := context.WithCancel(context.Background())
	ch1 := make(chan error, 1)
	go func() {
		_, err := cache.Probe(ctx1, "https://provider.example/v2.mp4", file1, probe)
		ch1 <- err
	}()

	<-workerStarted

	ch2 := make(chan *models.MediaFile, 1)
	ch2Err := make(chan error, 1)
	go func() {
		f, err := cache.Probe(context.Background(), "https://provider.example/v2.mp4", file2, probe)
		ch2 <- f
		ch2Err <- err
	}()

	cancel1()
	err1 := <-ch1
	if !errors.Is(err1, context.Canceled) {
		t.Fatalf("expected caller 1 to receive context.Canceled, got %v", err1)
	}

	file1.CodecAudio = "mutated-caller-input"
	close(releaseWorker)

	f2 := <-ch2
	err2 := <-ch2Err
	if err2 != nil {
		t.Fatalf("caller 2 should not fail when caller 1 cancels: %v", err2)
	}
	if f2.CodecAudio != "eac3" {
		t.Fatalf("f2 codec = %q, want eac3", f2.CodecAudio)
	}
}
