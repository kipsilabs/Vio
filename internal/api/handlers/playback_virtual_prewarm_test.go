package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/auth"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/plugins"
)

// prewarmFile is a virtual row with no probe evidence and no persisted
// candidate, so a start would normally have to list the provider.
func prewarmFile(path string) *models.MediaFile {
	return &models.MediaFile{
		ID:                         882,
		ContentID:                  "movie-prewarm",
		FilePath:                   path,
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
	}
}

// A valid best-result cache hit already carries the candidate metadata the
// provider list exists to fetch, so a start with incomplete stored probe
// evidence must not pay the provider round-trip again. The lister fails the
// test if invoked, proving the cache short-circuits it.
func TestResolveVirtualBestResultCacheHitSkipsProviderList(t *testing.T) {
	detailedCalls, legacyCalls := 0, 0
	h := virtualRepeatPlayHandler(&detailedCalls, &legacyCalls)
	h.BestResultCache = NewVirtualBestResultCache(time.Hour, 16)

	file := prewarmFile("virtual://movie/tt-cache-skip")
	neutral := virtualPlaybackNeutralKey(file.FilePath)
	alternatives := []VirtualPlaybackStream{{
		URI: neutral + "?result=cand-cached", Resolution: "1080p",
		CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
	}}
	h.BestResultCache.setWithDetails(
		bestResultCacheKey(file.ContentID, neutral, file.VirtualOwnerInstallationID),
		file.ContentID, neutral, file.VirtualOwnerInstallationID,
		alternatives, time.Now(),
	)

	listerCalls := 0
	h.VirtualPlaybackStreamLister = VirtualPlaybackStreamListerFunc(
		func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			listerCalls++
			return nil, errors.New("provider list must not run on a best-result cache hit")
		})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if listerCalls != 0 {
		t.Fatalf("provider lister called %d times, want 0 on a best-result cache hit", listerCalls)
	}
	if resolved.URI != alternatives[0].URI {
		t.Fatalf("resolved URI = %q, want cached candidate %q", resolved.URI, alternatives[0].URI)
	}
}

// A device-keyed lookup that misses must fall back to the device-neutral
// listing warmed by prefetch; the cached set is device-neutral and only the
// ranking is per-device, so the provider list is still unnecessary.
func TestResolveVirtualBestResultNeutralFallbackForDevice(t *testing.T) {
	detailedCalls, legacyCalls := 0, 0
	h := virtualRepeatPlayHandler(&detailedCalls, &legacyCalls)
	h.BestResultCache = NewVirtualBestResultCache(time.Hour, 16)
	h.DeviceCapabilitySource = staticDeviceCapabilitySource{caps: plugins.DeviceCapabilities{
		CodecsVideo:   []string{"h264"},
		CodecsAudio:   []string{"aac"},
		Containers:    []string{"mkv"},
		MaxResolution: "1080p",
	}}

	file := prewarmFile("virtual://movie/tt-neutral-fallback")
	neutral := virtualPlaybackNeutralKey(file.FilePath)
	alternatives := []VirtualPlaybackStream{{
		URI: neutral + "?result=cand-neutral", Resolution: "1080p",
		CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
	}}
	// Only the device-neutral (empty-fingerprint) key is populated.
	h.BestResultCache.setWithDetails(
		bestResultCacheKey(file.ContentID, neutral, file.VirtualOwnerInstallationID),
		file.ContentID, neutral, file.VirtualOwnerInstallationID,
		alternatives, time.Now(),
	)

	listerCalls := 0
	h.VirtualPlaybackStreamLister = VirtualPlaybackStreamListerFunc(
		func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			listerCalls++
			return nil, errors.New("provider list must not run on a neutral-cache fallback")
		})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	req.Header.Set("X-Vio-Device-Id", "device-1")
	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if listerCalls != 0 {
		t.Fatalf("provider lister called %d times, want 0 after a neutral-cache fallback", listerCalls)
	}
	if resolved.URI != alternatives[0].URI {
		t.Fatalf("resolved URI = %q, want cached candidate %q", resolved.URI, alternatives[0].URI)
	}
}

// Prefetch must warm the handler best-result cache under the neutral key, not
// just the resolver's provider cache, so a first click can skip the provider
// list. The pre-warm is asynchronous; poll for the entry.
func TestPrefetchVirtualPlaybackWarmsBestResultCache(t *testing.T) {
	h := &PlaybackHandler{}
	h.BestResultCache = NewVirtualBestResultCache(time.Hour, 16)
	h.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(
		func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
			return "http://provider.example/stream?path=" + path, nil
		})

	listed := make(chan struct{}, 1)
	h.VirtualPlaybackStreamLister = VirtualPlaybackStreamListerFunc(
		func(_ context.Context, path string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			select {
			case listed <- struct{}{}:
			default:
			}
			return []VirtualPlaybackStream{{
				URI: path + "?result=cand-prewarm", Resolution: "1080p",
				CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
			}}, nil
		})

	file := prewarmFile("virtual://movie/tt-prewarm")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/prefetch", nil)
	req = req.WithContext(apimw.SetClaims(req.Context(), &auth.Claims{UserID: 7, Role: "user", TokenType: auth.TokenTypeAccess}))
	h.PrefetchVirtualPlayback(req.Context(), []*models.MediaFile{file}, "profile-1")

	select {
	case <-listed:
	case <-time.After(2 * time.Second):
		t.Fatal("prefetch never listed provider candidates")
	}

	neutral := virtualPlaybackNeutralKey(file.FilePath)
	key := bestResultCacheKey(file.ContentID, neutral, file.VirtualOwnerInstallationID)
	deadline := time.Now().Add(2 * time.Second)
	for {
		cached := h.BestResultCache.get(key, time.Now())
		if len(cached) > 0 {
			if cached[0].URI != neutral+"?result=cand-prewarm" {
				t.Fatalf("warmed candidate = %q, want %q", cached[0].URI, neutral+"?result=cand-prewarm")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("prefetch did not warm the best-result cache under the neutral key")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestPrefetchVirtualPlaybackNeverProbes proves prefetch is list-only: it
// warms the listing cache but never resolves, probes, or opens a candidate
// media URL. Opening a ProviderURL can activate the provider, so any
// bytes-touching warm must wait for an explicit Play.
//
// The negative assertion runs prefetchOne synchronously (not through the
// worker pool) so a delayed forbidden call cannot escape the assertion: when
// prefetchOne returns, all of its work is done.
func TestPrefetchVirtualPlaybackNeverProbes(t *testing.T) {
	h := &PlaybackHandler{}
	h.BestResultCache = NewVirtualBestResultCache(time.Hour, 16)
	candURI := "virtual://movie/tt-preprobe?result=cand-preprobe-top"
	var resolveCalls, probeCalls atomic.Int64
	h.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(
		func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			resolveCalls.Add(1)
			return ResolvedVirtualMedia{URL: "http://provider.example/stream?path=" + uri, URI: uri}, nil
		})
	h.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(
		func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			resolveCalls.Add(1)
			return "", nil
		})

	h.VirtualPlaybackStreamLister = VirtualPlaybackStreamListerFunc(
		func(_ context.Context, path string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{{
				URI: candURI, Resolution: "1080p",
				CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
			}}, nil
		})

	h.VirtualPlaybackSourceProber = func(_ context.Context, url string, f *models.MediaFile) (*models.MediaFile, error) {
		probeCalls.Add(1)
		return f, nil
	}

	file := prewarmFile("virtual://movie/tt-preprobe")
	neutral := virtualPlaybackNeutralKey(file.FilePath)
	task := virtualPrefetchTask{
		file:       *file,
		neutralURI: neutral,
		userID:     7,
		profileID:  "profile-1",
		key:        virtualPrefetchKey(file.ContentID, neutral, file.VirtualOwnerInstallationID, 7, "profile-1"),
		generation: nextVirtualCacheGeneration(),
	}
	h.prefetchOne(task)

	// The listing cache must still warm so the first click skips the provider
	// round-trip.
	key := bestResultCacheKey(file.ContentID, neutral, file.VirtualOwnerInstallationID)
	if cached := h.BestResultCache.get(key, time.Now()); len(cached) == 0 {
		t.Fatal("prefetch did not warm the best-result cache under the neutral key")
	}

	if got := resolveCalls.Load(); got != 0 {
		t.Fatalf("resolver calls = %d, want 0: prefetch must never resolve a media URL", got)
	}
	if got := probeCalls.Load(); got != 0 {
		t.Fatalf("probe calls = %d, want 0: prefetch must never open a media URL", got)
	}
}
