package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/auth"
	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/plugins"
	"github.com/Silo-Server/silo-server/internal/remotestream"
	"github.com/Silo-Server/silo-server/internal/scanner"
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

func TestSelectedVirtualPreparationCandidate(t *testing.T) {
	selected := "virtual://movie/tt-preparation?result=selected"
	base := VirtualPlaybackStream{
		URI: selected, SourceType: "usenet", ProviderURL: "https://provider.example/selected",
	}
	for _, test := range []struct {
		name      string
		path      string
		candidate VirtualPlaybackStream
		want      bool
	}{
		{name: "exact selected usenet release", path: selected, candidate: base, want: true},
		{name: "placeholder", path: "virtual://movie/tt-preparation", candidate: base},
		{name: "different selected release", path: "virtual://movie/tt-preparation?result=other", candidate: base},
		{name: "nonvirtual source", path: "https://provider.example/selected?result=selected", candidate: base},
		{name: "nonusenet source", path: selected, candidate: VirtualPlaybackStream{URI: selected, SourceType: "torrent", ProviderURL: base.ProviderURL}},
		{name: "rejected release", path: selected, candidate: VirtualPlaybackStream{URI: selected, SourceType: base.SourceType, ProviderURL: base.ProviderURL, Rejected: true}},
		{name: "hidden release", path: selected, candidate: VirtualPlaybackStream{URI: selected, SourceType: base.SourceType, ProviderURL: base.ProviderURL, VisibilitySpecified: true}},
		{name: "missing provider URL", path: selected, candidate: VirtualPlaybackStream{URI: selected, SourceType: base.SourceType}},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate, ok := selectedVirtualPreparationCandidate(&models.MediaFile{FilePath: test.path}, []VirtualPlaybackStream{test.candidate})
			if ok != test.want {
				t.Fatalf("preparation eligibility = %v, want %v", ok, test.want)
			}
			if ok && candidate.URI != selected {
				t.Fatalf("prepared candidate = %q, want %q", candidate.URI, selected)
			}
		})
	}
	if _, ok := selectedVirtualPreparationCandidate(nil, []VirtualPlaybackStream{base}); ok {
		t.Fatal("a missing file must not prepare a candidate")
	}
}

func TestVirtualPreparationRequiresEnabledAndAuthorizedVersion(t *testing.T) {
	for _, test := range []struct {
		name    string
		enabled bool
		denied  bool
	}{
		{name: "disabled"},
		{name: "access denied", enabled: true, denied: true},
		{name: "file not in authorized detail", enabled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			detail := &fakeRefreshDetail{detail: &catalog.WatchDetail{Type: itemTypeMovie}}
			if test.denied {
				detail.err = apiError(http.StatusForbidden, "forbidden", "Forbidden")
			}
			service := &VirtualPlaybackPreparationService{
				Enabled: func(context.Context) bool { return test.enabled },
				Detail:  detail,
				Probe: func(context.Context, string, *models.MediaFile, map[string]string) (*models.MediaFile, error) {
					t.Fatal("must not probe without authorization")
					return nil, nil
				},
				FileByID: func(context.Context, int) (*models.MediaFile, error) {
					t.Fatal("must authorize the selected file before loading it")
					return nil, nil
				},
				List: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
					t.Fatal("must not list providers without authorization")
					return nil, nil
				}),
			}
			if err := service.PrepareVirtualPlayback(context.Background(), 7, "profile-1", "movie-1", 42, catalog.AccessFilter{}); err == nil {
				t.Fatal("expected preparation to be rejected")
			}
		})
	}
}

func TestVirtualPreparationUsesBoundedRangeAndPrivateOptIn(t *testing.T) {
	for _, allowPrivate := range []bool{false, true} {
		t.Run(fmt.Sprintf("allow_private_%t", allowPrivate), func(t *testing.T) {
			var requests atomic.Int32
			requestHeaders := make(chan http.Header, 1)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requests.Add(1)
				requestHeaders <- request.Header.Clone()
				writer.WriteHeader(http.StatusPartialContent)
				_, _ = writer.Write([]byte{1})
			}))
			defer server.Close()
			file := prewarmFile("virtual://movie/tt-preparation?result=selected")
			service := &VirtualPlaybackPreparationService{
				Enabled:      func(context.Context) bool { return true },
				AllowPrivate: func(context.Context) bool { return allowPrivate },
				publicHTTP:   remotestream.NewSafeClient(), privateHTTP: remotestream.NewInsecureClient(),
				List: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
					return []VirtualPlaybackStream{{
						URI: file.FilePath, SourceType: "usenet", ProviderURL: server.URL,
						RequestHeaders: map[string]string{"Referer": "https://provider.example", "Range": "bytes=0-"},
					}}, nil
				}),
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			service.prepare(ctx, file, 7, "profile-1")
			if !allowPrivate {
				if requests.Load() != 0 {
					t.Fatal("private source contacted without the explicit network opt-in")
				}
				return
			}
			select {
			case headers := <-requestHeaders:
				if headers.Get("Range") != "bytes=0-0" || headers.Get("Referer") != "https://provider.example" {
					t.Fatalf("unexpected provider headers: %v", headers)
				}
			default:
				t.Fatal("selected provider was not contacted")
			}
		})
	}
}

func TestVirtualPreparationCooldown(t *testing.T) {
	service := &VirtualPlaybackPreparationService{recent: make(map[string]time.Time)}
	for attempt := range 2 {
		claimed, err := service.claim(context.Background(), "selected")
		if err != nil || claimed != (attempt == 0) {
			t.Fatalf("attempt %d: claimed=%v error=%v", attempt, claimed, err)
		}
	}
	service.recent["selected"] = time.Now().Add(-time.Second)
	if claimed, err := service.claim(context.Background(), "selected"); err != nil || !claimed {
		t.Fatalf("expired preparation claim: claimed=%v error=%v", claimed, err)
	}
}

func TestVirtualPreparationWarmsPlaybackProbeCache(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "complete"},
		{name: "partial", err: scanner.ErrVirtualProbePartial},
		{name: "failed", err: errors.New("provider not ready")},
	} {
		t.Run(test.name, func(t *testing.T) {
			var rangeRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				rangeRequests.Add(1)
				if request.Header.Get("Range") != "bytes=0-0" || request.Header.Get("Referer") != "https://provider.example" {
					t.Errorf("unexpected warm-up request headers: %v", request.Header)
				}
				writer.WriteHeader(http.StatusInternalServerError)
			}))
			defer server.Close()
			file := prewarmFile("virtual://movie/tt-probe-preparation?result=selected")
			file.Duration = 3600
			candidate := VirtualPlaybackStream{
				URI: file.FilePath, SourceType: "usenet", ProviderURL: server.URL + "/selected?token=first",
				CodecAudio: "aac", RequestHeaders: map[string]string{"Referer": "https://provider.example"},
			}
			cache := scanner.NewVirtualProbeCache(time.Minute, 8)
			probeCalls := 0
			prober := func(ctx context.Context, sourceURL string, probeFile *models.MediaFile, headers map[string]string) (*models.MediaFile, error) {
				if headers["Referer"] != candidate.RequestHeaders["Referer"] {
					t.Errorf("provider headers were not preserved: %v", headers)
				}
				return cache.Probe(ctx, sourceURL, probeFile, func(_ context.Context, _ string, input *models.MediaFile) (*models.MediaFile, error) {
					probeCalls++
					if input.FilePath != file.FilePath || input.ID != file.ID || input.VirtualOwnerInstallationID != file.VirtualOwnerInstallationID || input.CodecAudio != "aac" || input.Duration != 0 {
						t.Errorf("unexpected early probe input: %+v", input)
					}
					input.Duration = 3598
					input.Container = "mkv"
					return input, test.err
				})
			}
			service := &VirtualPlaybackPreparationService{
				Enabled:      func(context.Context) bool { return true },
				AllowPrivate: func(context.Context) bool { return true },
				Probe:        prober,
				privateHTTP:  remotestream.NewInsecureClient(),
				List: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
					return []VirtualPlaybackStream{candidate}, nil
				}),
			}
			service.prepare(context.Background(), file, 7, "profile-1")
			if rangeRequests.Load() != 1 {
				t.Fatal("preparation must send the one-byte request even when probing fails")
			}
			if test.err == nil {
				service.prepare(context.Background(), file, 7, "profile-1")
				if rangeRequests.Load() != 2 || probeCalls != 1 {
					t.Fatalf("cache hit suppressed warm-up or repeated probe: requests=%d probes=%d", rangeRequests.Load(), probeCalls)
				}
			}
			playbackFile := cloneVirtualProbeTransient(*file)
			playbackFile.Duration = 0
			forwardVirtualCandidateDeclaredAudio(&playbackFile, candidate)
			rotatedURL := server.URL + "/selected?token=second"
			cached := cache.Lookup(rotatedURL, &playbackFile)
			if (cached != nil) != (test.err == nil) {
				t.Fatalf("cached probe = %v, probe error = %v", cached, test.err)
			}
			handler := &PlaybackHandler{VirtualPlaybackSourceProberWithHeaders: prober}
			probed, err := handler.probeVirtualSource(context.Background(), rotatedURL, &playbackFile, candidate.RequestHeaders)
			wantCalls := 1
			if test.err != nil {
				wantCalls = 2
			}
			if probeCalls != wantCalls || !errors.Is(err, test.err) {
				t.Fatalf("playback probe calls=%d want=%d error=%v", probeCalls, wantCalls, err)
			}
			if test.err == nil && (probed == nil || probed.Duration != 3598 || probed.Container != "mkv") {
				t.Fatalf("playback did not reuse early metadata: %+v", probed)
			}
			other := playbackFile
			other.FilePath = "virtual://movie/tt-probe-preparation?result=other"
			if cache.Lookup(rotatedURL, &other) != nil {
				t.Fatal("probe evidence leaked to another selected version")
			}
			withoutAudio := playbackFile
			withoutAudio.CodecAudio = ""
			if cache.Lookup(rotatedURL, &withoutAudio) != nil {
				t.Fatal("probe evidence reused with a different expected-audio signal")
			}
			if file.Duration != 3600 || file.Container != "virtual" || file.CodecAudio != "" || file.FailedAt != nil {
				t.Fatalf("preparation mutated the catalog row: %+v", file)
			}
		})
	}
}

func TestVirtualPreparationProbeRequiresPrivateOptIn(t *testing.T) {
	file := prewarmFile("virtual://movie/tt-probe-network?result=selected")
	for _, allowed := range []bool{false, true} {
		t.Run(fmt.Sprintf("allowed_%t", allowed), func(t *testing.T) {
			calls := 0
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				writer.WriteHeader(http.StatusPartialContent)
				_, _ = writer.Write([]byte{1})
			}))
			defer server.Close()
			service := &VirtualPlaybackPreparationService{
				Enabled:      func(context.Context) bool { return true },
				AllowPrivate: func(context.Context) bool { return allowed },
				publicHTTP:   remotestream.NewSafeClient(),
				privateHTTP:  remotestream.NewInsecureClient(),
				Probe: func(context.Context, string, *models.MediaFile, map[string]string) (*models.MediaFile, error) {
					calls++
					return nil, nil
				},
				List: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
					return []VirtualPlaybackStream{{URI: file.FilePath, SourceType: "usenet", ProviderURL: server.URL}}, nil
				}),
			}
			service.prepare(context.Background(), file, 7, "profile-1")
			if (calls == 1) != allowed || (requests.Load() == 1) != allowed {
				t.Fatalf("probe calls=%d requests=%d allow_private=%v", calls, requests.Load(), allowed)
			}
		})
	}
}

func TestVirtualPreparationProbeIsAsynchronousAndDeduplicated(t *testing.T) {
	for _, kind := range []string{itemTypeMovie, itemTypeEpisode} {
		t.Run(kind, func(t *testing.T) {
			file := prewarmFile("virtual://movie/tt-probe-async?result=selected")
			started := make(chan struct{}, 4)
			finished := make(chan struct{}, 4)
			warmup := make(chan string, 4)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				warmup <- request.Header.Get("Range")
				writer.WriteHeader(http.StatusPartialContent)
				_, _ = writer.Write([]byte{1})
			}))
			defer server.Close()
			serviceCtx, stop := context.WithCancel(context.Background())
			defer stop()
			requestCtx, cancelRequest := context.WithCancel(context.Background())
			defer cancelRequest()
			service := &VirtualPlaybackPreparationService{
				Context:      serviceCtx,
				Enabled:      func(context.Context) bool { return true },
				AllowPrivate: func(context.Context) bool { return true },
				Detail: &fakeRefreshDetail{detail: &catalog.WatchDetail{
					Type: kind, Versions: []catalog.FileVersion{{FileID: file.ID, FilePath: file.FilePath}},
				}},
				FileByID: func(context.Context, int) (*models.MediaFile, error) { return file, nil },
				List: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
					return []VirtualPlaybackStream{{URI: file.FilePath, SourceType: "usenet", ProviderURL: server.URL}}, nil
				}),
				Probe: func(ctx context.Context, _ string, input *models.MediaFile, _ map[string]string) (*models.MediaFile, error) {
					started <- struct{}{}
					<-ctx.Done()
					finished <- struct{}{}
					return input, ctx.Err()
				},
			}
			admitted := make(chan error, 1)
			go func() {
				admitted <- service.PrepareVirtualPlayback(requestCtx, 7, "profile-1", file.ContentID, file.ID, catalog.AccessFilter{})
			}()
			select {
			case err := <-admitted:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("preparation admission waited for ffprobe")
			}
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("background probe did not start")
			}
			select {
			case header := <-warmup:
				if header != "bytes=0-0" {
					t.Fatalf("warm-up Range = %q", header)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("warm-up waited for the blocked probe")
			}
			if len(service.active) != 1 {
				t.Fatal("preparation released its slot while the probe was still running")
			}
			cancelRequest()
			if err := service.PrepareVirtualPlayback(context.Background(), 7, "profile-1", file.ContentID, file.ID, catalog.AccessFilter{}); err != nil {
				t.Fatal(err)
			}
			stop()
			select {
			case <-finished:
			case <-time.After(2 * time.Second):
				t.Fatal("shutdown did not cancel background preparation")
			}
			select {
			case <-started:
				t.Fatal("the same selected version started a second probe")
			default:
			}
			select {
			case <-warmup:
				t.Fatal("the same selected version started a second warm-up")
			default:
			}
		})
	}
}
