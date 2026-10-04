package remotestream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestRelayRewritesNestedHLSReferencesAndPreservesRange(t *testing.T) {
	var mu sync.Mutex
	var upstreamRequests []*http.Request
	relay := NewRelay()
	relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		upstreamRequests = append(upstreamRequests, request.Clone(context.Background()))
		mu.Unlock()
		switch request.URL.String() {
		case "https://1.1.1.1/master.m3u8?token=top-secret":
			return relayResponse(request, http.StatusOK, "application/vnd.apple.mpegurl", strings.Join([]string{
				"#EXTM3U",
				`#EXT-X-KEY:METHOD=AES-128,URI="https://8.8.8.8/key.bin?key=secret"`,
				"variant/playlist.m3u8?auth=secret",
				"",
			}, "\n")), nil
		case "https://1.1.1.1/variant/playlist.m3u8?auth=secret":
			return relayResponse(request, http.StatusOK, "application/x-mpegURL", strings.Join([]string{
				"#EXTM3U",
				"#EXTINF:6,",
				"https://9.9.9.9/segment.ts?signature=secret",
				"",
			}, "\n")), nil
		case "https://9.9.9.9/segment.ts?signature=secret":
			if request.Header.Get("Range") != "bytes=2-5" {
				t.Errorf("upstream Range = %q", request.Header.Get("Range"))
			}
			response := relayResponse(request, http.StatusPartialContent, "video/mp2t", "2345")
			response.Header.Set("Accept-Ranges", "bytes")
			response.Header.Set("Content-Range", "bytes 2-5/10")
			response.Header.Set("Content-Length", "4")
			return response, nil
		default:
			t.Errorf("unexpected upstream URL %q", request.URL.String())
			return relayResponse(request, http.StatusNotFound, "text/plain", ""), nil
		}
	})}

	masterURL, cleanup := registerRelayForTest(t, relay, "root", "https://1.1.1.1/master.m3u8?token=top-secret")
	defer cleanup()

	master := fetchRelay(t, relay, masterURL, http.MethodGet, "")
	if master.status != http.StatusOK {
		t.Fatalf("master status = %d, body=%q", master.status, master.body)
	}
	for _, secret := range []string{"1.1.1.1", "8.8.8.8", "top-secret", "auth=secret", "key=secret"} {
		if strings.Contains(master.body, secret) {
			t.Fatalf("rewritten master leaked %q: %s", secret, master.body)
		}
	}
	keyPath := quotedURI(t, master.body)
	variantPath := firstMediaURI(t, master.body)
	if !strings.HasPrefix(keyPath, "/source/") || !strings.HasPrefix(variantPath, "/source/") {
		t.Fatalf("master references were not relayed: key=%q variant=%q", keyPath, variantPath)
	}

	variant := fetchRelay(t, relay, variantPath, http.MethodGet, "")
	if strings.Contains(variant.body, "9.9.9.9") || strings.Contains(variant.body, "signature=secret") {
		t.Fatalf("rewritten variant leaked provider URL: %s", variant.body)
	}
	segmentPath := firstMediaURI(t, variant.body)
	segment := fetchRelay(t, relay, segmentPath, http.MethodGet, "bytes=2-5")
	if segment.status != http.StatusPartialContent || segment.body != "2345" {
		t.Fatalf("segment response = status %d body %q", segment.status, segment.body)
	}
	if segment.header.Get("Content-Range") != "bytes 2-5/10" ||
		segment.header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("range headers = %+v", segment.header)
	}

	cleanup()
	if got := fetchRelay(t, relay, masterURL, http.MethodGet, ""); got.status != http.StatusNotFound {
		t.Fatalf("released master status = %d", got.status)
	}
	if got := fetchRelay(t, relay, segmentPath, http.MethodGet, ""); got.status != http.StatusNotFound {
		t.Fatalf("released child status = %d", got.status)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(upstreamRequests) != 3 {
		t.Fatalf("upstream request count = %d, want 3", len(upstreamRequests))
	}
}

func TestRelayHEADAndMethodRestriction(t *testing.T) {
	relay := NewRelay()
	relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodHead {
			t.Errorf("upstream method = %s", request.Method)
		}
		response := relayResponse(request, http.StatusOK, "video/mp4", "must-not-be-forwarded")
		response.Header.Set("Content-Length", "99")
		return response, nil
	})}
	relayURL, cleanup := registerRelayForTest(t, relay, "head", "https://1.1.1.1/movie.mp4")
	defer cleanup()

	head := fetchRelay(t, relay, relayURL, http.MethodHead, "")
	if head.status != http.StatusOK || head.body != "" || head.header.Get("Content-Length") != "99" {
		t.Fatalf("HEAD response = status %d len=%q body=%q", head.status, head.header.Get("Content-Length"), head.body)
	}
	post := fetchRelay(t, relay, relayURL, http.MethodPost, "")
	if post.status != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", post.status)
	}
}

func TestRelayRejectsUnsafeHLSReference(t *testing.T) {
	relay := NewRelay()
	relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return relayResponse(request, http.StatusOK, "application/vnd.apple.mpegurl",
			"#EXTM3U\nhttp://127.0.0.1/private\n"), nil
	})}
	relayURL, cleanup := registerRelayForTest(t, relay, "unsafe", "https://1.1.1.1/master.m3u8")
	defer cleanup()

	response := fetchRelay(t, relay, relayURL, http.MethodGet, "")
	if response.status != http.StatusBadGateway {
		t.Fatalf("unsafe playlist status = %d, body=%q", response.status, response.body)
	}
	if strings.Contains(response.body, "127.0.0.1") {
		t.Fatalf("unsafe target leaked in response: %q", response.body)
	}
}

func TestRelayProxyErrorDoesNotLeakProviderURL(t *testing.T) {
	relay := NewRelay()
	relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return nil, errors.New(`Get "https://1.1.1.1/movie?token=secret": provider failed`)
	})}
	request := httptest.NewRequest(http.MethodGet, "http://silo/stream", nil)
	recorder := httptest.NewRecorder()
	err := relay.Proxy(recorder, request, "https://1.1.1.1/movie?token=secret")
	if err == nil || !RetryableBeforeResponse(err) {
		t.Fatalf("Proxy error = %v, want retryable pre-response failure", err)
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "1.1.1.1") {
		t.Fatalf("Proxy error leaked provider URL: %v", err)
	}
}

func TestProxyRejectsPrivateSourceAndProxyInsecureAttemptsIt(t *testing.T) {
	relay := NewRelay()
	failer := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return nil, errors.New("upstream unreachable")
	})
	relay.client = &http.Client{Transport: failer}
	relay.insecureClient = &http.Client{Transport: failer}

	privateSource := "http://10.0.0.7/movie.mp4"
	secureRecorder := httptest.NewRecorder()
	secureErr := relay.Proxy(secureRecorder, httptest.NewRequest(http.MethodGet, "http://silo/stream", nil), privateSource)
	if secureErr == nil || !strings.Contains(secureErr.Error(), "non-public") {
		t.Fatalf("Proxy error = %v, want non-public address rejection", secureErr)
	}

	insecureRecorder := httptest.NewRecorder()
	insecureErr := relay.ProxyInsecure(insecureRecorder, httptest.NewRequest(http.MethodGet, "http://silo/stream", nil), privateSource)
	var proxyErr *ProxyError
	if !errors.As(insecureErr, &proxyErr) {
		t.Fatalf("ProxyInsecure error = %v, want fetch-attempt ProxyError", insecureErr)
	}
	if proxyErr.Started {
		t.Fatal("ProxyInsecure committed response before upstream failure")
	}
}

func TestRegisterInsecureKeepsProviderURLOutOfFFmpegURL(t *testing.T) {
	relay := NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()

	loopbackURL, release, err := relay.RegisterInsecure(context.Background(), "http://127.0.0.1:65535/private.mp4")
	if err != nil {
		t.Fatalf("RegisterInsecure: %v", err)
	}
	defer release()
	if strings.Contains(loopbackURL, "127.0.0.1:65535") || strings.Contains(loopbackURL, "private.mp4") {
		t.Fatalf("relay URL leaked provider source: %q", loopbackURL)
	}
}

func TestProxyInsecureRejectsStructurallyUnsafeSource(t *testing.T) {
	relay := NewRelay()
	relay.insecureClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatal("transport must not be called for an unsafe source")
		return nil, nil
	})}
	for _, raw := range []string{
		"file:///etc/passwd",
		"https://user:secret@10.0.0.7/stream",
		"https://10.0.0.7/stream\ninjected",
	} {
		err := relay.ProxyInsecure(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://silo/stream", nil), raw)
		if err == nil {
			t.Fatalf("ProxyInsecure(%q) succeeded, want rejection", raw)
		}
	}
}

func TestRegisterInsecureRewritesPrivateHLSPlaylist(t *testing.T) {
	relay := NewRelay()
	relay.insecureClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return relayResponse(request, http.StatusOK, "application/vnd.apple.mpegurl",
			"#EXTM3U\nhttp://192.168.1.50:8080/segment1.ts\nhttp://localhost:3000/segment2.ts\n"), nil
	})}
	relayURL, release, err := relay.RegisterInsecure(context.Background(), "http://192.168.1.50:8080/master.m3u8")
	if err != nil {
		t.Fatalf("RegisterInsecure: %v", err)
	}
	defer release()

	response := fetchRelay(t, relay, strings.TrimPrefix(relayURL, relay.baseURL), http.MethodGet, "")
	if response.status != http.StatusOK {
		t.Fatalf("insecure HLS rewrite failed with status %d body: %s", response.status, response.body)
	}
	if strings.Contains(response.body, "192.168.1.50") || strings.Contains(response.body, "localhost") {
		t.Fatalf("insecure HLS rewrite leaked private targets: %s", response.body)
	}
	if !strings.Contains(response.body, "/source/") || !strings.Contains(response.body, "/resource/") {
		t.Fatalf("insecure HLS rewrite missing relay resource references: %s", response.body)
	}
}

func TestRelayProxyRejectsUnregisteredHLSBeforeResponse(t *testing.T) {
	relay := NewRelay()
	relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return relayResponse(request, http.StatusOK, "application/vnd.apple.mpegurl",
			"#EXTM3U\nhttps://9.9.9.9/segment.ts?token=secret\n"), nil
	})}
	request := httptest.NewRequest(http.MethodGet, "http://silo/stream", nil)
	recorder := httptest.NewRecorder()
	err := relay.Proxy(recorder, request, "https://1.1.1.1/master.m3u8?token=secret")
	if err == nil || !RetryableBeforeResponse(err) {
		t.Fatalf("Proxy error = %v, want retryable pre-response failure", err)
	}
	if recorder.Code != http.StatusOK || recorder.Body.Len() != 0 {
		t.Fatalf("Proxy committed response status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "9.9.9.9") {
		t.Fatalf("Proxy error leaked provider URL: %v", err)
	}
}

func TestRelayStagesEmptyProviderBodyBeforeResponse(t *testing.T) {
	relay := NewRelay()
	relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return relayResponse(request, http.StatusOK, "video/mp4", ""), nil
	})}
	recorder := httptest.NewRecorder()
	err := relay.Proxy(recorder, httptest.NewRequest(http.MethodGet, "http://silo/stream", nil), "https://1.1.1.1/movie.mp4")
	if err == nil || !RetryableBeforeResponse(err) {
		t.Fatalf("Proxy error = %v, want retryable pre-response failure", err)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("provider failure committed body %q", recorder.Body.String())
	}
}

func TestRelayRejectsDASHBeforeResponse(t *testing.T) {
	for name, testCase := range map[string]struct {
		contentType string
		body        string
	}{
		"content-type": {"application/dash+xml", `<MPD></MPD>`},
		"body-sniff":   {"application/octet-stream", `<?xml version="1.0"?><MPD></MPD>`},
	} {
		t.Run(name, func(t *testing.T) {
			relay := NewRelay()
			relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return relayResponse(request, http.StatusOK, testCase.contentType, testCase.body), nil
			})}
			recorder := httptest.NewRecorder()
			err := relay.Proxy(recorder, httptest.NewRequest(http.MethodGet, "http://silo/stream", nil), "https://1.1.1.1/video")
			if err == nil || !RetryableBeforeResponse(err) {
				t.Fatalf("Proxy error = %v, want retryable DASH rejection", err)
			}
			if recorder.Body.Len() != 0 {
				t.Fatalf("DASH rejection committed body %q", recorder.Body.String())
			}
		})
	}
}

func TestRelaySniffsMislabeledHLS(t *testing.T) {
	relay := NewRelay()
	relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return relayResponse(request, http.StatusOK, "application/octet-stream", "#EXTM3U\nhttps://9.9.9.9/segment.ts\n"), nil
	})}
	relayURL, cleanup := registerRelayForTest(t, relay, "sniff", "https://1.1.1.1/video")
	defer cleanup()

	response := fetchRelay(t, relay, relayURL, http.MethodGet, "")
	if response.status != http.StatusOK || !strings.Contains(response.body, "/source/") || strings.Contains(response.body, "9.9.9.9") {
		t.Fatalf("mislabeled HLS response = status %d body %q", response.status, response.body)
	}
}

func TestRelayHLSReferenceTokenIsSecretBoundAndExpiring(t *testing.T) {
	relay := NewRelay()
	now := time.Now()
	const source = "https://1.1.1.1/segment.ts?token=provider-secret"
	opaque, err := relay.sealReference("parent-a", source, now)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(opaque, "provider-secret") || strings.Contains(opaque, "1.1.1.1") {
		t.Fatalf("sealed reference leaked source URL: %q", opaque)
	}
	opened, err := relay.openReference("parent-a", opaque, now)
	if err != nil || opened != source {
		t.Fatalf("openReference = %q, %v", opened, err)
	}
	if _, err := relay.openReference("parent-b", opaque, now); err == nil {
		t.Fatal("reference token opened under a different parent")
	}
	tamperedBytes := []byte(opaque)
	tamperAt := len(tamperedBytes) / 2
	if tamperedBytes[tamperAt] == 'A' {
		tamperedBytes[tamperAt] = 'B'
	} else {
		tamperedBytes[tamperAt] = 'A'
	}
	tampered := string(tamperedBytes)
	if _, err := relay.openReference("parent-a", tampered, now); err == nil {
		t.Fatal("tampered reference token opened")
	}
	if _, err := relay.openReference("parent-a", opaque, now.Add(relayEntryLifetime)); err == nil {
		t.Fatal("expired reference token opened")
	}
}

func TestRelayCloseRevokesEntriesAndRejectsRegistrations(t *testing.T) {
	relay := NewRelay()
	relayURL, cleanup := registerRelayForTest(t, relay, "close", "https://1.1.1.1/movie.mp4")
	defer cleanup()

	if err := relay.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if got := fetchRelay(t, relay, relayURL, http.MethodGet, ""); got.status != http.StatusNotFound {
		t.Fatalf("closed relay response status = %d, want 404", got.status)
	}
	if _, _, err := relay.Register(context.Background(), "https://1.1.1.1/other.mp4"); err == nil {
		t.Fatal("Register succeeded after Close")
	}
	if err := relay.Close(context.Background()); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

// TestRelayFreshRegistrationBypassesContentReuse proves that re-registering
// identical content reuses the live entry's token (one upstream, one range-cache
// scope), while the force-fresh variant mints a new token so a bounded retry
// presents new bytes. Both tokens stay live.
func TestRelayFreshRegistrationBypassesContentReuse(t *testing.T) {
	relay := NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()

	source := "https://1.1.1.1/movie.mp4"
	headers := map[string]string{"Referer": "https://example.test/"}
	tokenOf := func(rawURL string) string {
		t.Helper()
		token, ok := relayTokenFromURL(rawURL)
		if !ok {
			t.Fatalf("relay URL %q has no token", rawURL)
		}
		return token
	}

	first, releaseFirst, err := relay.RegisterWithHeaders(context.Background(), source, headers)
	if err != nil {
		t.Fatalf("first register: %v", err)
	}
	defer releaseFirst()
	reused, releaseReused, err := relay.RegisterWithHeaders(context.Background(), source, headers)
	if err != nil {
		t.Fatalf("reuse register: %v", err)
	}
	defer releaseReused()
	if tokenOf(reused) != tokenOf(first) {
		t.Fatalf("reuse token = %q, want the live token %q", tokenOf(reused), tokenOf(first))
	}

	fresh, releaseFresh, err := relay.RegisterWithHeadersFresh(context.Background(), source, headers)
	if err != nil {
		t.Fatalf("fresh register: %v", err)
	}
	defer releaseFresh()
	if tokenOf(fresh) == tokenOf(first) {
		t.Fatalf("fresh token = %q, want a token distinct from %q", tokenOf(fresh), tokenOf(first))
	}
	if got := relay.RegistrationStatus(first); got != RegistrationLive {
		t.Fatalf("original registration status = %v, want live", got)
	}
	if got := relay.RegistrationStatus(fresh); got != RegistrationLive {
		t.Fatalf("fresh registration status = %v, want live", got)
	}
}

func relayResponse(request *http.Request, status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

type relayFetch struct {
	status int
	header http.Header
	body   string
}

func fetchRelay(t *testing.T, relay *Relay, rawURL, method, byteRange string) relayFetch {
	t.Helper()
	return fetchRelayWithHeaders(t, relay, rawURL, method, byteRange, nil)
}

func fetchRelayWithHeaders(t *testing.T, relay *Relay, rawURL, method, byteRange string, headers map[string]string) relayFetch {
	t.Helper()
	request := httptest.NewRequest(method, "http://relay"+rawURL, nil)
	if byteRange != "" {
		request.Header.Set("Range", byteRange)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	relay.handle(recorder, request)
	response := recorder.Result()
	return relayFetch{status: response.StatusCode, header: response.Header.Clone(), body: recorder.Body.String()}
}

func registerRelayForTest(t *testing.T, relay *Relay, token, rawURL string) (string, func()) {
	t.Helper()
	source, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	baseName := safeRelayBaseName(source)
	relay.mu.Lock()
	relay.entries[token] = &relayEntry{
		source: source, baseName: baseName, createdAt: time.Now(),
	}
	relay.mu.Unlock()
	cleanup := func() {
		relay.mu.Lock()
		relay.deleteEntryLocked(token)
		relay.mu.Unlock()
	}
	return "/source/" + token + "/" + url.PathEscape(baseName), cleanup
}

// registerRelayWithHeadersForTest seeds an entry with forwarded headers, which
// registerRelayForTest does not model.
func registerRelayWithHeadersForTest(t *testing.T, relay *Relay, token, rawURL string, headers map[string]string) (string, func()) {
	t.Helper()
	source, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	baseName := safeRelayBaseName(source)
	relay.mu.Lock()
	relay.entries[token] = &relayEntry{
		source: source, baseName: baseName, createdAt: time.Now(), headers: cloneHeaderMap(headers),
	}
	relay.mu.Unlock()
	cleanup := func() {
		relay.mu.Lock()
		relay.deleteEntryLocked(token)
		relay.mu.Unlock()
	}
	return "/source/" + token + "/" + url.PathEscape(baseName), cleanup
}

// TestRelayEvictsOldestAtCapacity proves the registry bounds itself by dropping
// the oldest registration instead of refusing new playback once relayMaxEntries
// is reached.
func TestRelayEvictsOldestAtCapacity(t *testing.T) {
	relay := NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()

	relay.mu.Lock()
	base := time.Now().Add(-time.Hour)
	for i := 0; i < relayMaxEntries; i++ {
		source, _ := url.Parse("https://1.1.1.1/evict")
		relay.entries[fmt.Sprintf("old-%d", i)] = &relayEntry{
			source: source, baseName: "stream", createdAt: base.Add(time.Duration(i) * time.Second),
		}
	}
	relay.mu.Unlock()

	relayURL, release, err := relay.RegisterInsecure(context.Background(), "https://1.1.1.1/new")
	if err != nil {
		t.Fatalf("RegisterInsecure at capacity: %v", err)
	}
	defer release()

	relay.mu.Lock()
	size := len(relay.entries)
	_, oldestPresent := relay.entries["old-0"]
	_, newestPresent := relay.entries[fmt.Sprintf("old-%d", relayMaxEntries-1)]
	relay.mu.Unlock()
	if size != relayMaxEntries {
		t.Fatalf("entries = %d, want %d", size, relayMaxEntries)
	}
	if oldestPresent {
		t.Fatal("oldest entry was not evicted at capacity")
	}
	if !newestPresent {
		t.Fatal("a newer entry was evicted before the oldest")
	}
	if !strings.Contains(relayURL, "/source/") {
		t.Fatalf("registration URL = %q", relayURL)
	}
}

// TestRelayRejectsExpiredEntryOnPresentation proves token expiry is enforced
// when the token is presented, not only when an unrelated registration happens
// to trigger eviction.
func TestRelayRejectsExpiredEntryOnPresentation(t *testing.T) {
	relay := NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()
	relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response := relayResponse(request, http.StatusOK, "video/mp4", "ok")
		response.Header.Set("Content-Length", "2")
		return response, nil
	})}
	relayURL, cleanup := registerRelayForTest(t, relay, "expired", "https://1.1.1.1/media.mkv")
	defer cleanup()

	if got := fetchRelay(t, relay, relayURL, http.MethodGet, ""); got.status == http.StatusNotFound {
		t.Fatal("fresh entry was rejected")
	}

	relay.mu.Lock()
	relay.entries["expired"].createdAt = time.Now().Add(-relayEntryLifetime - time.Minute)
	relay.mu.Unlock()

	if got := fetchRelay(t, relay, relayURL, http.MethodGet, ""); got.status != http.StatusNotFound {
		t.Fatalf("expired entry status = %d, want 404", got.status)
	}
	relay.mu.Lock()
	_, stillPresent := relay.entries["expired"]
	relay.mu.Unlock()
	if stillPresent {
		t.Fatal("expired entry was not dropped on presentation")
	}
}

// TestRelayRangeCacheKeyIncludesRegistrationHeaders proves two registrations of
// the same URL with different forwarded headers never share cached bytes.
func TestRelayRangeCacheKeyIncludesRegistrationHeaders(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	relay := NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()
	relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		body := "referer:" + request.Header.Get("Referer")
		response := relayResponse(request, http.StatusPartialContent, "application/octet-stream", body)
		response.Header.Set("Content-Range", "bytes 0-4/100")
		response.Header.Set("Content-Length", strconv.Itoa(len(body)))
		return response, nil
	})}

	urlA, cleanupA := registerRelayWithHeadersForTest(t, relay, "hdr-a", "https://1.1.1.1/media.mkv", map[string]string{"Referer": "https://provider-a/"})
	defer cleanupA()
	urlB, cleanupB := registerRelayWithHeadersForTest(t, relay, "hdr-b", "https://1.1.1.1/media.mkv", map[string]string{"Referer": "https://provider-b/"})
	defer cleanupB()

	gotA := fetchRelay(t, relay, urlA, http.MethodGet, "bytes=0-4")
	gotB := fetchRelay(t, relay, urlB, http.MethodGet, "bytes=0-4")
	if gotA.body != "referer:https://provider-a/" {
		t.Fatalf("registration A body = %q", gotA.body)
	}
	if gotB.body != "referer:https://provider-b/" {
		t.Fatalf("registration B body = %q", gotB.body)
	}
	if got := fetchRelay(t, relay, urlA, http.MethodGet, "bytes=0-4"); got.body != gotA.body {
		t.Fatalf("registration A repeat body = %q, want %q", got.body, gotA.body)
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (each registration caches under its own header identity)", got)
	}
}

// TestRelayRangeCacheKeyIncludesRegistrationOrigin proves a differing
// registration Origin alone forces a separate cache entry.
func TestRelayRangeCacheKeyIncludesRegistrationOrigin(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	relay := NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()
	relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		body := "origin:" + request.Header.Get("Origin")
		response := relayResponse(request, http.StatusPartialContent, "application/octet-stream", body)
		response.Header.Set("Content-Range", "bytes 0-4/100")
		response.Header.Set("Content-Length", strconv.Itoa(len(body)))
		return response, nil
	})}
	urlA, cleanupA := registerRelayWithHeadersForTest(t, relay, "origin-a", "https://1.1.1.1/media.mkv", map[string]string{"Origin": "https://origin-a"})
	defer cleanupA()
	urlB, cleanupB := registerRelayWithHeadersForTest(t, relay, "origin-b", "https://1.1.1.1/media.mkv", map[string]string{"Origin": "https://origin-b"})
	defer cleanupB()

	gotA := fetchRelay(t, relay, urlA, http.MethodGet, "bytes=0-4")
	gotB := fetchRelay(t, relay, urlB, http.MethodGet, "bytes=0-4")
	if gotA.body != "origin:https://origin-a" || gotB.body != "origin:https://origin-b" {
		t.Fatalf("bodies = %q / %q", gotA.body, gotB.body)
	}
	if got := fetchRelay(t, relay, urlA, http.MethodGet, "bytes=0-4"); got.body != gotA.body {
		t.Fatalf("registration A repeat body = %q, want %q", got.body, gotA.body)
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (a differing Origin alone forces its own entry)", got)
	}
}

func TestSafeRelayBaseNameDoesNotExposeProviderPath(t *testing.T) {
	source, _ := url.Parse("https://1.1.1.1/provider-secret-token/movie-title.m3u8?token=query-secret")
	if got := safeRelayBaseName(source); got != "stream.m3u8" {
		t.Fatalf("safeRelayBaseName = %q", got)
	}
	source, _ = url.Parse("https://1.1.1.1/provider-secret-token")
	if got := safeRelayBaseName(source); got != "stream" {
		t.Fatalf("safeRelayBaseName without extension = %q", got)
	}
}

func quotedURI(t *testing.T, playlist string) string {
	t.Helper()
	const marker = `URI="`
	start := strings.Index(playlist, marker)
	if start < 0 {
		t.Fatalf("playlist has no URI attribute: %s", playlist)
	}
	start += len(marker)
	end := strings.IndexByte(playlist[start:], '"')
	if end < 0 {
		t.Fatalf("playlist has malformed URI attribute: %s", playlist)
	}
	return playlist[start : start+end]
}

func firstMediaURI(t *testing.T, playlist string) string {
	t.Helper()
	for _, line := range strings.Split(playlist, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			return line
		}
	}
	t.Fatalf("playlist has no media URI: %s", playlist)
	return ""
}

func TestRegisterWithHeadersForwardsAllowedHeaders(t *testing.T) {
	var receivedReferer, receivedOrigin string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedReferer = r.Header.Get("Referer")
		receivedOrigin = r.Header.Get("Origin")
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("mp4-data"))
	}))
	defer server.Close()

	relay := NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()

	headers := map[string]string{
		"Referer": "https://provider.example/player",
		"Origin":  "https://provider.example",
		"Cookie":  "secret-cookie", // must NOT be forwarded
	}
	relayURL, release, err := relay.RegisterInsecureWithHeaders(context.Background(), server.URL+"/stream.mp4", headers)
	if err != nil {
		t.Fatalf("RegisterInsecureWithHeaders failed: %v", err)
	}
	defer release()

	req, _ := http.NewRequest(http.MethodGet, relayURL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do relay request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if receivedReferer != "https://provider.example/player" {
		t.Fatalf("received Referer = %q, want https://provider.example/player", receivedReferer)
	}
	if receivedOrigin != "https://provider.example" {
		t.Fatalf("received Origin = %q, want https://provider.example", receivedOrigin)
	}
}

func TestRelayStalledErrorBodyReturnsBoundedError(t *testing.T) {
	hangServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusServiceUnavailable)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer hangServer.Close()

	relay := NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()

	relayURL, release, err := relay.RegisterInsecure(context.Background(), hangServer.URL+"/video.mp4")
	if err != nil {
		t.Fatalf("RegisterInsecure failed: %v", err)
	}
	defer release()

	start := time.Now()
	req, _ := http.NewRequest(http.MethodGet, relayURL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do relay request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want upstream 503 forwarded", resp.StatusCode)
	}
	if elapsed >= 2500*time.Millisecond {
		t.Fatalf("elapsed = %v, want < 2.5s", elapsed)
	}
}

func TestRelayResponseWriterFlusherAndUnwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	tracked := &relayResponseWriter{ResponseWriter: rec}

	if unwrapped := tracked.Unwrap(); unwrapped != rec {
		t.Fatalf("unwrapped = %v, want %v", unwrapped, rec)
	}

	flusher, ok := any(tracked).(http.Flusher)
	if !ok {
		t.Fatal("expected relayResponseWriter to implement http.Flusher")
	}
	flusher.Flush()
	if !rec.Flushed {
		t.Fatal("expected underlying recorder to be flushed")
	}
}

func TestRelayPostCommitFailureAbortsDownstreamConnection(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("first-256k-chunk-header"))
		if flusher != nil {
			flusher.Flush()
		}
		hj, ok := w.(http.Hijacker)
		if ok {
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
			return
		}
	}))
	defer upstream.Close()

	relay := NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()

	relayURL, release, err := relay.RegisterInsecure(context.Background(), upstream.URL+"/stream.mp4")
	if err != nil {
		t.Fatalf("RegisterInsecure failed: %v", err)
	}
	defer release()

	req, _ := http.NewRequest(http.MethodGet, relayURL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do relay request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	buf := make([]byte, 23)
	n, err := io.ReadFull(resp.Body, buf)
	if err != nil {
		t.Fatalf("reading first chunk failed: %v", err)
	}
	if string(buf[:n]) != "first-256k-chunk-header" {
		t.Fatalf("chunk = %q, want first-256k-chunk-header", string(buf[:n]))
	}

	_, err = io.ReadAll(resp.Body)
	if err == nil {
		t.Fatal("expected read error on aborted stream, but got clean EOF")
	}
}

func TestRelayServesCompleteRangeResponseFromCache(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	relay := NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()
	relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		if got := request.Header.Get("Range"); got != "bytes=100-199" {
			t.Errorf("upstream Range = %q, want bytes=100-199", got)
		}
		response := relayResponse(request, http.StatusPartialContent, "application/octet-stream", strings.Repeat("x", 100))
		response.Header.Set("Accept-Ranges", "bytes")
		response.Header.Set("Content-Range", "bytes 100-199/1000")
		response.Header.Set("Content-Length", "100")
		response.Header.Set("ETag", `"etag-1"`)
		return response, nil
	})}
	relayURL, cleanup := registerRelayForTest(t, relay, "range-cache", "https://1.1.1.1/media.mkv")
	defer cleanup()

	first := fetchRelay(t, relay, relayURL, http.MethodGet, "bytes=100-199")
	if first.status != http.StatusPartialContent || first.body != strings.Repeat("x", 100) {
		t.Fatalf("first response = status %d, %d bytes", first.status, len(first.body))
	}
	second := fetchRelay(t, relay, relayURL, http.MethodGet, "bytes=100-199")
	if second.status != first.status || second.body != first.body {
		t.Fatalf("cached response differs: status %d/%d, body %d/%d",
			first.status, second.status, len(first.body), len(second.body))
	}
	if got := second.header.Get("Content-Range"); got != "bytes 100-199/1000" {
		t.Fatalf("cached Content-Range = %q", got)
	}
	if got := second.header.Get("ETag"); got != `"etag-1"` {
		t.Fatalf("cached ETag = %q", got)
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (second request served from cache)", got)
	}
}

func TestRelayRangeCacheLeavesOversizedAndNoStoreResponsesUpstream(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	relay := NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()
	relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		switch request.URL.Path {
		case "/large":
			body := strings.Repeat("z", relayRangeCacheMaxEntrySize+1)
			response := relayResponse(request, http.StatusPartialContent, "application/octet-stream", body)
			response.Header.Set("Content-Range", "bytes 0-524288/1000000")
			response.Header.Set("Content-Length", strconv.Itoa(len(body)))
			return response, nil
		case "/nostore":
			response := relayResponse(request, http.StatusPartialContent, "application/octet-stream", strings.Repeat("n", 64))
			response.Header.Set("Content-Range", "bytes 0-63/1000")
			response.Header.Set("Content-Length", "64")
			response.Header.Set("Cache-Control", "no-store")
			return response, nil
		default:
			t.Errorf("unexpected upstream path %q", request.URL.Path)
			return relayResponse(request, http.StatusNotFound, "text/plain", ""), nil
		}
	})}

	largeURL, largeCleanup := registerRelayForTest(t, relay, "large", "https://1.1.1.1/large")
	defer largeCleanup()
	noStoreURL, noStoreCleanup := registerRelayForTest(t, relay, "nostore", "https://1.1.1.1/nostore")
	defer noStoreCleanup()

	for i := 0; i < 2; i++ {
		if got := fetchRelay(t, relay, largeURL, http.MethodGet, "bytes=0-524288"); got.status != http.StatusPartialContent {
			t.Fatalf("large fetch %d status = %d", i, got.status)
		}
		if got := fetchRelay(t, relay, noStoreURL, http.MethodGet, "bytes=0-63"); got.status != http.StatusPartialContent {
			t.Fatalf("no-store fetch %d status = %d", i, got.status)
		}
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 4 {
		t.Fatalf("upstream calls = %d, want 4 (oversized and no-store responses are never cached)", got)
	}
}

func TestRelayRangeCacheEvictsAndExpires(t *testing.T) {
	cache := newRelayRangeCache()
	now := time.Unix(1_700_000_000, 0)
	cache.now = func() time.Time { return now }

	cache.put("entry", relayRangeCacheEntry{status: http.StatusPartialContent, body: []byte("body")})
	if entry, ok := cache.get("entry"); !ok || string(entry.body) != "body" {
		t.Fatalf("cache miss right after put: ok=%v body=%q", ok, entry.body)
	}
	now = now.Add(relayRangeCacheTTL + time.Second)
	if _, ok := cache.get("entry"); ok {
		t.Fatal("expired entry was served")
	}

	for i := 0; i < relayRangeCacheMaxEntries+8; i++ {
		cache.put("k"+strconv.Itoa(i), relayRangeCacheEntry{status: http.StatusPartialContent, body: []byte("x")})
	}
	if len(cache.entries) > relayRangeCacheMaxEntries {
		t.Fatalf("cache entries = %d, want <= %d", len(cache.entries), relayRangeCacheMaxEntries)
	}
}

// TestRelayRangeResponseCacheabilityRejectsUnboundedAndNonReusable proves the
// length/status bounds still hold and that every origin directive the review
// called out keeps a response out of the cache. It also pins the new split
// between validity and residency: max-age/s-maxage within the sanity ceiling but
// longer than relayRangeCacheTTL is accepted (and clamped later), while a value
// beyond the 30-day sanity ceiling is still refused.
func TestRelayRangeResponseCacheabilityRejectsUnboundedAndNonReusable(t *testing.T) {
	receivedAt := time.Unix(1_700_000_000, 0)
	cases := []struct {
		name   string
		status int
		length string
		header map[string]string
		want   bool
	}{
		{"complete", http.StatusPartialContent, "128", nil, true},
		{"empty_length", http.StatusPartialContent, "", nil, false},
		{"chunked", http.StatusPartialContent, "", map[string]string{"Transfer-Encoding": "chunked"}, false},
		{"oversized", http.StatusPartialContent, strconv.Itoa(relayRangeCacheMaxEntrySize + 1), nil, false},
		{"zero", http.StatusPartialContent, "0", nil, false},
		{"server_error", http.StatusBadGateway, "128", nil, false},
		{"no_store", http.StatusPartialContent, "128", map[string]string{"Cache-Control": "no-store"}, false},
		{"private", http.StatusPartialContent, "128", map[string]string{"Cache-Control": "private, max-age=60"}, false},
		{"no_cache", http.StatusPartialContent, "128", map[string]string{"Cache-Control": "no-cache"}, false},
		{"max_age_zero", http.StatusPartialContent, "128", map[string]string{"Cache-Control": "max-age=0"}, false},
		{"s_maxage_zero", http.StatusPartialContent, "128", map[string]string{"Cache-Control": "s-maxage=0"}, false},
		{"malformed_max_age", http.StatusPartialContent, "128", map[string]string{"Cache-Control": "max-age=soon"}, false},
		{"age_beyond_sanity_ceiling", http.StatusPartialContent, "128", map[string]string{"Cache-Control": "max-age=60", "Age": strconv.FormatInt(relayMaxFreshnessSeconds+1, 10)}, false},
		{"unrepresentable_age", http.StatusPartialContent, "128", map[string]string{"Cache-Control": "max-age=60", "Age": "999999999999999999999999"}, false},
		{"unrepresentable_max_age", http.StatusPartialContent, "128", map[string]string{"Cache-Control": "max-age=999999999999999999999999"}, false},
		{"max_age_beyond_sanity_ceiling", http.StatusPartialContent, "128", map[string]string{"Cache-Control": "max-age=" + strconv.FormatInt(relayMaxFreshnessSeconds+1, 10)}, false},
		{"s_maxage_beyond_sanity_ceiling", http.StatusPartialContent, "128", map[string]string{"Cache-Control": "s-maxage=" + strconv.FormatInt(relayMaxFreshnessSeconds+1, 10)}, false},
		{"max_age_above_cache_ttl_is_reusable", http.StatusPartialContent, "128", map[string]string{"Cache-Control": "max-age=3600"}, true},
		{"s_maxage_above_cache_ttl_is_reusable", http.StatusPartialContent, "128", map[string]string{"Cache-Control": "s-maxage=3600"}, true},
		{"vary_star", http.StatusPartialContent, "128", map[string]string{"Vary": "*"}, false},
		{"vary_star_list", http.StatusPartialContent, "128", map[string]string{"Vary": "Accept, *"}, false},
		{"vary_accept", http.StatusPartialContent, "128", map[string]string{"Vary": "Accept"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := &http.Response{StatusCode: tc.status, Header: http.Header{}}
			if tc.length != "" {
				response.Header.Set("Content-Length", tc.length)
			}
			for k, v := range tc.header {
				response.Header.Set(k, v)
			}
			_, _, ok := relayRangeResponseCacheability(response, receivedAt, receivedAt)
			if ok != tc.want {
				t.Fatalf("cacheable = %v, want %v", ok, tc.want)
			}
		})
	}
}

// TestRelayRangeResponseCacheabilityFreshness pins the freshness predicate: the
// expiry is measured from the response's own Age (or backdated Date), never
// restarted at receipt, and a response whose freshness is already spent is not
// reusable.
func TestRelayRangeResponseCacheabilityFreshness(t *testing.T) {
	receivedAt := time.Unix(1_700_000_000, 0)
	date := func(offset time.Duration) string {
		return receivedAt.Add(offset).UTC().Format(http.TimeFormat)
	}
	cases := []struct {
		name       string
		header     http.Header
		wantOK     bool
		wantExpiry time.Time
	}{
		{
			name:       "no directive uses bounded relay default",
			header:     http.Header{},
			wantOK:     true,
			wantExpiry: receivedAt.Add(relayRangeCacheTTL),
		},
		{
			name:       "max-age measured from receipt",
			header:     http.Header{"Cache-Control": {"max-age=60"}},
			wantOK:     true,
			wantExpiry: receivedAt.Add(60 * time.Second),
		},
		{
			name:       "age consumes max-age",
			header:     http.Header{"Cache-Control": {"max-age=60"}, "Age": {"55"}},
			wantOK:     true,
			wantExpiry: receivedAt.Add(5 * time.Second),
		},
		{
			name:   "age equal to max-age is not reusable",
			header: http.Header{"Cache-Control": {"max-age=60"}, "Age": {"60"}},
			wantOK: false,
		},
		{
			name:   "age beyond max-age is not reusable",
			header: http.Header{"Cache-Control": {"max-age=60"}, "Age": {"120"}},
			wantOK: false,
		},
		{
			// s-maxage is the shared-cache freshness and wins over a larger
			// max-age; both values only have to stay within the sanity ceiling.
			name:       "s-maxage wins for a shared cache",
			header:     http.Header{"Cache-Control": {"s-maxage=30, max-age=" + strconv.FormatInt(relayMaxFreshnessSeconds, 10)}},
			wantOK:     true,
			wantExpiry: receivedAt.Add(30 * time.Second),
		},
		{
			// If max-age were used the remaining freshness would be zero and
			// the response would not be reusable; s-maxage keeps it fresh.
			name:       "s-maxage precedence survives corrected age",
			header:     http.Header{"Cache-Control": {"s-maxage=120, max-age=30"}, "Age": {"30"}, "Date": {date(-30 * time.Second)}},
			wantOK:     true,
			wantExpiry: receivedAt.Add(90 * time.Second),
		},
		{
			name:       "backdated date counts as age",
			header:     http.Header{"Cache-Control": {"max-age=60"}, "Date": {date(-10 * time.Second)}},
			wantOK:     true,
			wantExpiry: receivedAt.Add(50 * time.Second),
		},
		{
			// A small Age cannot hide an old Date: apparent age (120s) exceeds
			// max-age, so the response is stale even though Age says fresh.
			name:   "old date contradicts modest age",
			header: http.Header{"Cache-Control": {"max-age=60"}, "Age": {"10"}, "Date": {date(-120 * time.Second)}},
			wantOK: false,
		},
		{
			name:       "future expires",
			header:     http.Header{"Date": {date(0)}, "Expires": {date(30 * time.Second)}},
			wantOK:     true,
			wantExpiry: receivedAt.Add(30 * time.Second),
		},
		{
			name:   "past expires",
			header: http.Header{"Date": {date(-60 * time.Second)}, "Expires": {date(-1 * time.Second)}},
			wantOK: false,
		},
		{
			name:       "ordinary long max-age clamps to the cache lifetime",
			header:     http.Header{"Cache-Control": {"max-age=3600"}},
			wantOK:     true,
			wantExpiry: receivedAt.Add(relayRangeCacheTTL),
		},
		{
			// max-age at the sanity ceiling is still a valid response, but its
			// residency is the relay's own cache lifetime, not 30 days.
			name:       "max-age at the sanity ceiling clamps to the cache lifetime",
			header:     http.Header{"Cache-Control": {"max-age=" + strconv.FormatInt(relayMaxFreshnessSeconds, 10)}},
			wantOK:     true,
			wantExpiry: receivedAt.Add(relayRangeCacheTTL),
		},
		{
			// A far-future Expires is a valid freshness lifetime; the relay
			// clamps how long it keeps the bytes, not whether it caches them.
			name:       "far future expires clamps to the cache lifetime",
			header:     http.Header{"Date": {date(0)}, "Expires": {date(100 * 365 * 24 * time.Hour)}},
			wantOK:     true,
			wantExpiry: receivedAt.Add(relayRangeCacheTTL),
		},
		{
			name:   "malformed age cannot establish freshness",
			header: http.Header{"Cache-Control": {"max-age=60"}, "Age": {"not-a-number"}},
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := &http.Response{StatusCode: http.StatusPartialContent, Header: tc.header.Clone()}
			response.Header.Set("Content-Length", "128")
			_, expiry, ok := relayRangeResponseCacheability(response, receivedAt, receivedAt)
			if ok != tc.wantOK {
				t.Fatalf("cacheable = %v, want %v", ok, tc.wantOK)
			}
			if tc.wantOK && !expiry.Equal(tc.wantExpiry) {
				t.Fatalf("expiry = %v, want %v", expiry, tc.wantExpiry)
			}
		})
	}
}

// TestRelayRangeResponseCacheabilityCorrectedAge pins the RFC 9111 §4.2.3
// corrected-age rule: corrected initial age is max(Age + response delay,
// responseReceivedAt - Date), so an old Date or a slow response can only make a
// response staler than Age claims. Every case declares max-age=60.
func TestRelayRangeResponseCacheabilityCorrectedAge(t *testing.T) {
	receivedAt := time.Unix(1_700_000_000, 0)
	date := func(offset time.Duration) string {
		return receivedAt.Add(offset).UTC().Format(http.TimeFormat)
	}
	cases := []struct {
		name         string
		header       http.Header
		requestDelay time.Duration
		wantOK       bool
		wantExpiry   time.Time
	}{
		{
			name:         "response delay added to Age exhausts max-age",
			header:       http.Header{"Cache-Control": {"max-age=60"}, "Age": {"55"}},
			requestDelay: 10 * time.Second,
			wantOK:       false,
		},
		{
			name:         "response delay reduces remaining freshness",
			header:       http.Header{"Cache-Control": {"max-age=60"}, "Age": {"50"}},
			requestDelay: 5 * time.Second,
			wantOK:       true,
			wantExpiry:   receivedAt.Add(5 * time.Second),
		},
		{
			name:   "apparent age dominates an understated Age",
			header: http.Header{"Cache-Control": {"max-age=60"}, "Age": {"10"}, "Date": {date(-120 * time.Second)}},
			wantOK: false,
		},
		{
			name:         "corrected Age dominates a truthful Date",
			header:       http.Header{"Cache-Control": {"max-age=60"}, "Age": {"55"}, "Date": {date(-55 * time.Second)}},
			requestDelay: 2 * time.Second,
			wantOK:       true,
			wantExpiry:   receivedAt.Add(3 * time.Second),
		},
		{
			name:   "already expired by Date is never reusable",
			header: http.Header{"Cache-Control": {"max-age=60"}, "Date": {date(-90 * time.Second)}},
			wantOK: false,
		},
		{
			name:       "fresh Date and Age with no delay stays reusable",
			header:     http.Header{"Cache-Control": {"max-age=60"}, "Age": {"10"}, "Date": {date(-10 * time.Second)}},
			wantOK:     true,
			wantExpiry: receivedAt.Add(50 * time.Second),
		},
		{
			name:   "malformed Date cannot establish freshness",
			header: http.Header{"Cache-Control": {"max-age=60"}, "Age": {"10"}, "Date": {"not-a-date"}},
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := &http.Response{StatusCode: http.StatusPartialContent, Header: tc.header.Clone()}
			response.Header.Set("Content-Length", "128")
			_, expiry, ok := relayRangeResponseCacheability(response, receivedAt.Add(-tc.requestDelay), receivedAt)
			if ok != tc.wantOK {
				t.Fatalf("cacheable = %v, want %v", ok, tc.wantOK)
			}
			if tc.wantOK && !expiry.Equal(tc.wantExpiry) {
				t.Fatalf("expiry = %v, want %v", expiry, tc.wantExpiry)
			}
		})
	}
}

// TestRelayRangeResponseCacheabilityAcceptsUTCSuffixDate is the regression for a
// Date ending in "UTC": http.ParseTime wants the "GMT" abbreviation, so the
// corrected-age half used to reject a Date the freshness-lifetime half accepted,
// collapsing cacheability. Both halves now share relayHTTPTime, so a UTC-suffix
// Date with Expires or max-age and no Age is reusable, while a malformed Date is
// still rejected.
func TestRelayRangeResponseCacheabilityAcceptsUTCSuffixDate(t *testing.T) {
	receivedAt := time.Unix(1_700_000_000, 0)
	utcDate := receivedAt.UTC().Format(time.RFC1123)
	cases := []struct {
		name       string
		header     http.Header
		wantExpiry time.Time
	}{
		{
			name:       "expires relative to a UTC-suffix date",
			header:     http.Header{"Date": {utcDate}, "Expires": {receivedAt.Add(30 * time.Second).UTC().Format(time.RFC1123)}},
			wantExpiry: receivedAt.Add(30 * time.Second),
		},
		{
			name:       "max-age with a UTC-suffix date and no age",
			header:     http.Header{"Date": {utcDate}, "Cache-Control": {"max-age=60"}},
			wantExpiry: receivedAt.Add(60 * time.Second),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := &http.Response{StatusCode: http.StatusPartialContent, Header: tc.header.Clone()}
			response.Header.Set("Content-Length", "128")
			directives := relayCacheControlDirectives(response.Header.Values(headerCacheControl))
			if lifetime, ok := relayFreshnessLifetime(directives, response.Header, receivedAt); !ok || lifetime <= 0 {
				t.Fatalf("freshness-lifetime half rejected the UTC Date: ok=%v lifetime=%v", ok, lifetime)
			}
			if _, ok := relayCorrectedInitialAge(response.Header, receivedAt, receivedAt); !ok {
				t.Fatal("corrected-age half rejected the UTC Date")
			}
			_, expiry, ok := relayRangeResponseCacheability(response, receivedAt, receivedAt)
			if !ok {
				t.Fatal("cacheable = false, want a UTC-suffix Date to be reusable")
			}
			if !expiry.Equal(tc.wantExpiry) {
				t.Fatalf("expiry = %v, want %v", expiry, tc.wantExpiry)
			}
		})
	}

	malformed := &http.Response{StatusCode: http.StatusPartialContent, Header: http.Header{
		"Date": {"not-a-date"}, "Cache-Control": {"max-age=60"},
	}}
	malformed.Header.Set("Content-Length", "128")
	if _, _, ok := relayRangeResponseCacheability(malformed, receivedAt, receivedAt); ok {
		t.Fatal("cacheable = true for a malformed Date, want rejection")
	}
}

// TestRelayRangeResponseCacheabilitySaturatesOverflowingFreshness proves the
// arithmetic that used to be unbounded cannot mark a stale response fresh: an
// Age or max-age/s-maxage above the 30-day sanity ceiling, an Age that does not
// even fit in int64, and a response delay far past every freshness lifetime are
// all non-reusable, and no case returns a far-future expiry.
func TestRelayRangeResponseCacheabilitySaturatesOverflowingFreshness(t *testing.T) {
	receivedAt := time.Unix(1_700_000_000, 0)
	beyondCeiling := strconv.FormatInt(relayMaxFreshnessSeconds+1, 10)
	unrepresentable := "999999999999999999999999"
	cases := []struct {
		name         string
		header       http.Header
		requestDelay time.Duration
	}{
		{
			name:   "age above the sanity ceiling",
			header: http.Header{"Cache-Control": {"max-age=60"}, "Age": {beyondCeiling}},
		},
		{
			name:   "age that overflows int64",
			header: http.Header{"Cache-Control": {"max-age=60"}, "Age": {unrepresentable}},
		},
		{
			name:   "max-age above the sanity ceiling",
			header: http.Header{"Cache-Control": {"max-age=" + beyondCeiling}},
		},
		{
			name:   "max-age that overflows int64",
			header: http.Header{"Cache-Control": {"max-age=" + unrepresentable}},
		},
		{
			name:   "s-maxage above the sanity ceiling",
			header: http.Header{"Cache-Control": {"s-maxage=" + beyondCeiling}},
		},
		{
			name:         "response delay far beyond every lifetime",
			header:       http.Header{"Cache-Control": {"max-age=60"}},
			requestDelay: 100 * 365 * 24 * time.Hour,
		},
		{
			// Age sits exactly at the sanity ceiling and the delay alone would
			// overflow a naive addition; the saturated sum still exceeds max-age.
			name:         "age at the sanity ceiling plus an overflowing delay",
			header:       http.Header{"Cache-Control": {"max-age=60"}, "Age": {strconv.FormatInt(relayMaxFreshnessSeconds, 10)}},
			requestDelay: 100 * 365 * 24 * time.Hour,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := &http.Response{StatusCode: http.StatusPartialContent, Header: tc.header.Clone()}
			response.Header.Set("Content-Length", "128")
			_, expiry, ok := relayRangeResponseCacheability(response, receivedAt.Add(-tc.requestDelay), receivedAt)
			if ok {
				t.Fatalf("cacheable = true with expiry %v, want a stale response to be non-reusable", expiry)
			}
			if !expiry.IsZero() && expiry.After(receivedAt.Add(relayRangeCacheTTL)) {
				t.Fatalf("expiry = %v, want no expiry beyond the cache lifetime %v", expiry, receivedAt.Add(relayRangeCacheTTL))
			}
		})
	}
}

// TestRelayDoesNotCacheLargeOpenEndedReads proves a read larger than the cache
// entry bound streams through on every request and is never served from cache.
// This is the guard against the range cache truncating or replaying a media
// read: a 22 GB body (and any bounded range over 512 KiB) always comes from
// upstream, so the cache cannot shorten it.
func TestRelayDoesNotCacheLargeOpenEndedReads(t *testing.T) {
	body := strings.Repeat("z", relayRangeCacheMaxEntrySize+1)
	var mu sync.Mutex
	calls := 0
	relay := NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()
	relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		response := relayResponse(request, http.StatusPartialContent, "application/octet-stream", body)
		response.Header.Set("Accept-Ranges", "bytes")
		response.Header.Set("Content-Range", "bytes 0-524288/22613433146")
		response.Header.Set("Content-Length", strconv.Itoa(len(body)))
		return response, nil
	})}
	relayURL, cleanup := registerRelayForTest(t, relay, "large-open-ended", "https://1.1.1.1/media.mkv")
	defer cleanup()

	for attempt := 0; attempt < 2; attempt++ {
		got := fetchRelay(t, relay, relayURL, http.MethodGet, "bytes=0-")
		if got.status != http.StatusPartialContent || len(got.body) != len(body) {
			t.Fatalf("attempt %d = status %d, %d bytes, want %d", attempt, got.status, len(got.body), len(body))
		}
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (a large read is never cached)", got)
	}
}

// TestRelayRangeCacheKeyHonorsExactRange proves two different byte ranges of
// the same source are cached independently: the exact Range header is part of
// the key, so a hit can never answer a different offset with the wrong bytes.
func TestRelayRangeCacheKeyHonorsExactRange(t *testing.T) {
	bodies := map[string]string{
		"bytes=100-199": strings.Repeat("A", 100),
		"bytes=200-299": strings.Repeat("B", 100),
	}
	var mu sync.Mutex
	calls := 0
	relay := NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()
	relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		rangeHeader := request.Header.Get("Range")
		response := relayResponse(request, http.StatusPartialContent, "application/octet-stream", bodies[rangeHeader])
		response.Header.Set("Content-Range", strings.Replace(rangeHeader, "=", " ", 1)+"/1000")
		response.Header.Set("Content-Length", strconv.Itoa(len(bodies[rangeHeader])))
		return response, nil
	})}
	relayURL, cleanup := registerRelayForTest(t, relay, "exact-range", "https://1.1.1.1/media.mkv")
	defer cleanup()

	for _, tc := range []struct{ name, body string }{
		{"bytes=100-199", bodies["bytes=100-199"]},
		{"bytes=200-299", bodies["bytes=200-299"]},
		{"bytes=100-199", bodies["bytes=100-199"]},
	} {
		got := fetchRelay(t, relay, relayURL, http.MethodGet, tc.name)
		if got.body != tc.body {
			t.Fatalf("range %s served %d bytes starting %q, want the body for that exact range",
				tc.name, len(got.body), got.body[:min(8, len(got.body))])
		}
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (only the repeated exact range is cached)", got)
	}
}

// TestRelayRangeCacheReusesOnlyRemainingFreshness proves freshness is measured
// from the response's own Age, not restarted at insertion: a max-age=60
// response that arrived already 55s old is a hit at +4s and must be re-fetched
// at +6s.
func TestRelayRangeCacheReusesOnlyRemainingFreshness(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	relay := NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()
	relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		body := strings.Repeat("a", 64)
		response := relayResponse(request, http.StatusPartialContent, "application/octet-stream", body)
		response.Header.Set("Content-Range", "bytes 0-63/1000")
		response.Header.Set("Content-Length", "64")
		response.Header.Set("Cache-Control", "max-age=60")
		response.Header.Set("Age", "55")
		return response, nil
	})}
	relayURL, cleanup := registerRelayForTest(t, relay, "age-freshness", "https://1.1.1.1/media.mkv")
	defer cleanup()

	now := time.Unix(1_700_000_000, 0)
	relay.rangeCache.now = func() time.Time { return now }

	first := fetchRelay(t, relay, relayURL, http.MethodGet, "bytes=0-63")
	if first.status != http.StatusPartialContent || len(first.body) != 64 {
		t.Fatalf("first response = status %d, %d bytes", first.status, len(first.body))
	}
	now = now.Add(4 * time.Second)
	if got := fetchRelay(t, relay, relayURL, http.MethodGet, "bytes=0-63"); got.status != http.StatusPartialContent || got.body != first.body {
		t.Fatalf("within-freshness response = status %d, %d bytes", got.status, len(got.body))
	}
	now = now.Add(2 * time.Second)
	if got := fetchRelay(t, relay, relayURL, http.MethodGet, "bytes=0-63"); got.status != http.StatusPartialContent || got.body != first.body {
		t.Fatalf("past-freshness response = status %d, %d bytes", got.status, len(got.body))
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (one miss, then a re-fetch once Age exhausted max-age)", got)
	}
}

// TestRelayRangeCacheRefetchesWhenDateContradictsAge proves a small Age cannot
// mask an old Date: the response is 120s old by Date with Age: 10 and
// max-age=60, so it is stale on arrival and every request must re-fetch. This is
// the case the age-only predicate treated as fresh.
func TestRelayRangeCacheRefetchesWhenDateContradictsAge(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	relay := NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()

	now := time.Unix(1_700_000_000, 0)
	relay.rangeCache.now = func() time.Time { return now }

	want := strings.Repeat("d", 64)
	relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		response := relayResponse(request, http.StatusPartialContent, "application/octet-stream", want)
		response.Header.Set("Content-Range", "bytes 0-63/1000")
		response.Header.Set("Content-Length", "64")
		response.Header.Set("Cache-Control", "max-age=60")
		response.Header.Set("Age", "10")
		response.Header.Set("Date", now.Add(-120*time.Second).UTC().Format(http.TimeFormat))
		return response, nil
	})}
	relayURL, cleanup := registerRelayForTest(t, relay, "date-contradiction", "https://1.1.1.1/media.mkv")
	defer cleanup()

	for attempt := 0; attempt < 2; attempt++ {
		got := fetchRelay(t, relay, relayURL, http.MethodGet, "bytes=0-63")
		if got.status != http.StatusPartialContent || got.body != want {
			t.Fatalf("attempt %d = status %d, %d bytes; want the origin's %d-byte body",
				attempt, got.status, len(got.body), len(want))
		}
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (old Date makes the response stale despite a modest Age)", got)
	}
}

// TestRelayRangeCacheResponseDelayCountsTowardCorrectedAge proves the time a
// response spends in transit is part of its corrected age: an Age: 55 response
// that took 10s to arrive is 65s old, past max-age=60, and must not be reused.
func TestRelayRangeCacheResponseDelayCountsTowardCorrectedAge(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	relay := NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()

	now := time.Unix(1_700_000_000, 0)
	relay.rangeCache.now = func() time.Time { return now }

	want := strings.Repeat("e", 64)
	relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		// Advance the injected clock to model 10s of transit after the relay
		// recorded the request send time.
		now = now.Add(10 * time.Second)
		response := relayResponse(request, http.StatusPartialContent, "application/octet-stream", want)
		response.Header.Set("Content-Range", "bytes 0-63/1000")
		response.Header.Set("Content-Length", "64")
		response.Header.Set("Cache-Control", "max-age=60")
		response.Header.Set("Age", "55")
		return response, nil
	})}
	relayURL, cleanup := registerRelayForTest(t, relay, "response-delay", "https://1.1.1.1/media.mkv")
	defer cleanup()

	for attempt := 0; attempt < 2; attempt++ {
		got := fetchRelay(t, relay, relayURL, http.MethodGet, "bytes=0-63")
		if got.status != http.StatusPartialContent || got.body != want {
			t.Fatalf("attempt %d = status %d, %d bytes; want the origin's %d-byte body",
				attempt, got.status, len(got.body), len(want))
		}
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (response delay pushes corrected age past max-age)", got)
	}
}

// TestRelayRangeCacheNeverReusesExpiredResponse proves an expired response is
// never stored: Date 90s in the past with no Age and max-age=60 is stale on
// arrival, so both requests hit the origin.
func TestRelayRangeCacheNeverReusesExpiredResponse(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	relay := NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()

	now := time.Unix(1_700_000_000, 0)
	relay.rangeCache.now = func() time.Time { return now }

	want := strings.Repeat("f", 64)
	relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		response := relayResponse(request, http.StatusPartialContent, "application/octet-stream", want)
		response.Header.Set("Content-Range", "bytes 0-63/1000")
		response.Header.Set("Content-Length", "64")
		response.Header.Set("Cache-Control", "max-age=60")
		response.Header.Set("Date", now.Add(-90*time.Second).UTC().Format(http.TimeFormat))
		return response, nil
	})}
	relayURL, cleanup := registerRelayForTest(t, relay, "expired-by-date", "https://1.1.1.1/media.mkv")
	defer cleanup()

	for attempt := 0; attempt < 2; attempt++ {
		got := fetchRelay(t, relay, relayURL, http.MethodGet, "bytes=0-63")
		if got.status != http.StatusPartialContent || got.body != want {
			t.Fatalf("attempt %d = status %d, %d bytes; want the origin's %d-byte body",
				attempt, got.status, len(got.body), len(want))
		}
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (an expired response is never reused)", got)
	}
}

// TestRelayRangeCacheReusesFreshResponseWithCorrectedAge proves the ordinary
// fresh case still hits: Date and Age agree the response is 10s old with
// max-age=60, so the second request is served from cache without an origin call.
func TestRelayRangeCacheReusesFreshResponseWithCorrectedAge(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	relay := NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()

	now := time.Unix(1_700_000_000, 0)
	relay.rangeCache.now = func() time.Time { return now }

	want := strings.Repeat("g", 64)
	relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		response := relayResponse(request, http.StatusPartialContent, "application/octet-stream", want)
		response.Header.Set("Content-Range", "bytes 0-63/1000")
		response.Header.Set("Content-Length", "64")
		response.Header.Set("Cache-Control", "max-age=60")
		response.Header.Set("Age", "10")
		response.Header.Set("Date", now.Add(-10*time.Second).UTC().Format(http.TimeFormat))
		return response, nil
	})}
	relayURL, cleanup := registerRelayForTest(t, relay, "fresh-corrected-age", "https://1.1.1.1/media.mkv")
	defer cleanup()

	first := fetchRelay(t, relay, relayURL, http.MethodGet, "bytes=0-63")
	if first.status != http.StatusPartialContent || first.body != want {
		t.Fatalf("first = status %d, %d bytes; want the origin's %d-byte body", first.status, len(first.body), len(want))
	}
	second := fetchRelay(t, relay, relayURL, http.MethodGet, "bytes=0-63")
	if second.status != http.StatusPartialContent || second.body != want {
		t.Fatalf("second = status %d, %d bytes; want the cached %d-byte body", second.status, len(second.body), len(want))
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (a fresh corrected age is still cached)", got)
	}
}

// TestRelayRangeCacheClampsLongOriginFreshness is the regression for the
// maintainer's tradeoff fix: an origin may declare freshness far longer than the
// relay's two-minute cache lifetime (max-age=3600, or s-maxage above it). That
// response is valid and reusable, so instead of the old reject-over-ceiling
// behavior it is cached with the remaining freshness clamped to
// relayRangeCacheTTL. A lookup inside the window is a byte-exact hit; reopening
// after the window re-fetches the origin's exact bytes rather than replaying a
// stale entry.
func TestRelayRangeCacheClampsLongOriginFreshness(t *testing.T) {
	for _, tc := range []struct {
		name         string
		cacheControl string
	}{
		{"max-age above the clamp", "max-age=3600"},
		{"s-maxage above the clamp", "s-maxage=3600"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			calls := 0
			want := strings.Repeat("h", 64)
			relay := NewRelay()
			defer func() { _ = relay.Close(context.Background()) }()
			now := time.Unix(1_700_000_000, 0)
			relay.rangeCache.now = func() time.Time { return now }
			relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				mu.Lock()
				calls++
				mu.Unlock()
				response := relayResponse(request, http.StatusPartialContent, "application/octet-stream", want)
				response.Header.Set("Content-Range", "bytes 0-63/1000")
				response.Header.Set("Content-Length", "64")
				response.Header.Set("Cache-Control", tc.cacheControl)
				return response, nil
			})}
			relayURL, cleanup := registerRelayForTest(t, relay, "long-origin-freshness", "https://1.1.1.1/media.mkv")
			defer cleanup()

			first := fetchRelay(t, relay, relayURL, http.MethodGet, "bytes=0-63")
			if first.status != http.StatusPartialContent || first.body != want {
				t.Fatalf("first = status %d, %d bytes; want the origin's %d-byte body", first.status, len(first.body), len(want))
			}
			// Still inside the clamped two-minute residency: a byte-exact hit.
			now = now.Add(90 * time.Second)
			hit := fetchRelay(t, relay, relayURL, http.MethodGet, "bytes=0-63")
			if hit.status != http.StatusPartialContent || hit.body != want {
				t.Fatalf("within-clamp = status %d, %d bytes; want the cached %d-byte body", hit.status, len(hit.body), len(want))
			}
			// Past the clamped residency: the origin is consulted again and its
			// exact bytes are served, not the stale cached entry.
			now = now.Add(relayRangeCacheTTL + time.Second)
			late := fetchRelay(t, relay, relayURL, http.MethodGet, "bytes=0-63")
			if late.status != http.StatusPartialContent || late.body != want {
				t.Fatalf("past-clamp = status %d, %d bytes; want the origin's %d-byte body", late.status, len(late.body), len(want))
			}
			mu.Lock()
			got := calls
			mu.Unlock()
			if got != 2 {
				t.Fatalf("upstream calls = %d, want 2 (one miss, then a re-fetch after the clamped residency)", got)
			}
		})
	}
}

// TestRelayRangeCacheNoDirectiveBoundedReuse documents the conservative default
// when the origin sends no freshness directive: reuse is bounded to
// relayRangeCacheTTL and never extended.
func TestRelayRangeCacheNoDirectiveBoundedReuse(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	relay := NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()
	relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		body := strings.Repeat("b", 32)
		response := relayResponse(request, http.StatusPartialContent, "application/octet-stream", body)
		response.Header.Set("Content-Range", "bytes 0-31/1000")
		response.Header.Set("Content-Length", "32")
		return response, nil
	})}
	relayURL, cleanup := registerRelayForTest(t, relay, "no-directive", "https://1.1.1.1/media.mkv")
	defer cleanup()

	now := time.Unix(1_700_000_000, 0)
	relay.rangeCache.now = func() time.Time { return now }

	if got := fetchRelay(t, relay, relayURL, http.MethodGet, "bytes=0-31"); got.status != http.StatusPartialContent {
		t.Fatalf("first status = %d", got.status)
	}
	if got := fetchRelay(t, relay, relayURL, http.MethodGet, "bytes=0-31"); got.status != http.StatusPartialContent {
		t.Fatalf("second status = %d", got.status)
	}
	now = now.Add(relayRangeCacheTTL + time.Second)
	if got := fetchRelay(t, relay, relayURL, http.MethodGet, "bytes=0-31"); got.status != http.StatusPartialContent {
		t.Fatalf("expired status = %d", got.status)
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (default reuse bounded to relayRangeCacheTTL)", got)
	}
}

// TestRelayRangeCacheBypassesOriginNonReusable proves every directive the review
// named keeps the response out of the cache end to end.
func TestRelayRangeCacheBypassesOriginNonReusable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header map[string]string
	}{
		{"no-cache", map[string]string{"Cache-Control": "no-cache"}},
		{"max-age=0", map[string]string{"Cache-Control": "max-age=0"}},
		{"no-store", map[string]string{"Cache-Control": "no-store"}},
		{"private", map[string]string{"Cache-Control": "private"}},
		{"vary-star", map[string]string{"Vary": "*"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			calls := 0
			want := strings.Repeat("n", 32)
			relay := NewRelay()
			defer func() { _ = relay.Close(context.Background()) }()
			relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				mu.Lock()
				calls++
				mu.Unlock()
				response := relayResponse(request, http.StatusPartialContent, "application/octet-stream", want)
				response.Header.Set("Content-Range", "bytes 0-31/1000")
				response.Header.Set("Content-Length", "32")
				for name, value := range tc.header {
					response.Header.Set(name, value)
				}
				return response, nil
			})}
			relayURL, cleanup := registerRelayForTest(t, relay, "non-reusable", "https://1.1.1.1/media.mkv")
			defer cleanup()
			for attempt := 0; attempt < 2; attempt++ {
				got := fetchRelay(t, relay, relayURL, http.MethodGet, "bytes=0-31")
				if got.status != http.StatusPartialContent || got.body != want {
					t.Fatalf("attempt %d = status %d, %d bytes", attempt, got.status, len(got.body))
				}
			}
			mu.Lock()
			got := calls
			mu.Unlock()
			if got != 2 {
				t.Fatalf("upstream calls = %d, want 2 (origin marked the response non-reusable)", got)
			}
		})
	}
}

// TestRelayRangeCacheNeverServesOverflowingFreshness proves end to end that a
// genuinely absurd Age or max-age/s-maxage (beyond the 30-day sanity ceiling),
// and a delay that alone exceeds every freshness lifetime, keep the response out
// of the cache: both requests reach the origin and each receives the full origin
// body, so a saturated value can never be served as a fresh hit.
func TestRelayRangeCacheNeverServesOverflowingFreshness(t *testing.T) {
	beyondSanity := strconv.FormatInt(relayMaxFreshnessSeconds+1, 10)
	cases := []struct {
		name         string
		header       map[string]string
		clockAdvance time.Duration
	}{
		{"age above the sanity ceiling", map[string]string{"Cache-Control": "max-age=60", "Age": beyondSanity}, 0},
		{"age that overflows int64", map[string]string{"Cache-Control": "max-age=60", "Age": "999999999999999999999999"}, 0},
		{"max-age above the sanity ceiling", map[string]string{"Cache-Control": "max-age=" + beyondSanity}, 0},
		{"s-maxage above the sanity ceiling", map[string]string{"Cache-Control": "s-maxage=" + beyondSanity}, 0},
		{"s-maxage that overflows int64", map[string]string{"Cache-Control": "s-maxage=999999999999999999999999"}, 0},
		{"huge response delay", map[string]string{"Cache-Control": "max-age=60"}, 100 * 365 * 24 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			calls := 0
			want := strings.Repeat("o", 32)
			relay := NewRelay()
			defer func() { _ = relay.Close(context.Background()) }()
			now := time.Unix(1_700_000_000, 0)
			relay.rangeCache.now = func() time.Time { return now }
			relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				mu.Lock()
				calls++
				mu.Unlock()
				now = now.Add(tc.clockAdvance)
				response := relayResponse(request, http.StatusPartialContent, "application/octet-stream", want)
				response.Header.Set("Content-Range", "bytes 0-31/1000")
				response.Header.Set("Content-Length", "32")
				for name, value := range tc.header {
					response.Header.Set(name, value)
				}
				return response, nil
			})}
			relayURL, cleanup := registerRelayForTest(t, relay, "overflowing-freshness", "https://1.1.1.1/media.mkv")
			defer cleanup()
			for attempt := 0; attempt < 2; attempt++ {
				got := fetchRelay(t, relay, relayURL, http.MethodGet, "bytes=0-31")
				if got.status != http.StatusPartialContent || got.body != want {
					t.Fatalf("attempt %d = status %d, %d bytes; want the origin's %d-byte body",
						attempt, got.status, len(got.body), len(want))
				}
			}
			mu.Lock()
			got := calls
			mu.Unlock()
			if got != 2 {
				t.Fatalf("upstream calls = %d, want 2 (an overflowing freshness value is never cached)", got)
			}
		})
	}
}

// TestRelayRangeCacheIdentityUsesEffectiveOutboundHeaders proves request-level
// Accept and User-Agent differences produce distinct entries, exactly as they
// change the outbound request.
func TestRelayRangeCacheIdentityUsesEffectiveOutboundHeaders(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	relay := NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()
	relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		body := "accept=" + request.Header.Get("Accept") + ";ua=" + request.Header.Get("User-Agent")
		response := relayResponse(request, http.StatusPartialContent, "application/octet-stream", body)
		response.Header.Set("Content-Range", "bytes 0-0/1")
		response.Header.Set("Content-Length", strconv.Itoa(len(body)))
		return response, nil
	})}
	relayURL, cleanup := registerRelayForTest(t, relay, "effective-headers", "https://1.1.1.1/media.mkv")
	defer cleanup()

	first := fetchRelayWithHeaders(t, relay, relayURL, http.MethodGet, "bytes=0-0", map[string]string{"Accept": "video/mp4", "User-Agent": "player-a"})
	otherUA := fetchRelayWithHeaders(t, relay, relayURL, http.MethodGet, "bytes=0-0", map[string]string{"Accept": "video/mp4", "User-Agent": "player-b"})
	otherAccept := fetchRelayWithHeaders(t, relay, relayURL, http.MethodGet, "bytes=0-0", map[string]string{"Accept": "application/octet-stream", "User-Agent": "player-a"})
	repeat := fetchRelayWithHeaders(t, relay, relayURL, http.MethodGet, "bytes=0-0", map[string]string{"Accept": "video/mp4", "User-Agent": "player-a"})

	if first.body != "accept=video/mp4;ua=player-a" {
		t.Fatalf("first body = %q", first.body)
	}
	if otherUA.body != "accept=video/mp4;ua=player-b" {
		t.Fatalf("other-UA body = %q", otherUA.body)
	}
	if otherAccept.body != "accept=application/octet-stream;ua=player-a" {
		t.Fatalf("other-Accept body = %q", otherAccept.body)
	}
	if repeat.body != first.body {
		t.Fatalf("repeat body = %q, want %q", repeat.body, first.body)
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 3 {
		t.Fatalf("upstream calls = %d, want 3 (only the exact effective-header variant is reused)", got)
	}
}

// TestRelayRangeCacheIdentityUsesRegistrationOverride proves the identity is
// computed after per-registration overrides: two client User-Agent values that
// the registration pins to one value share a single entry.
func TestRelayRangeCacheIdentityUsesRegistrationOverride(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	relay := NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()
	relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		body := "ua=" + request.Header.Get("User-Agent")
		response := relayResponse(request, http.StatusPartialContent, "application/octet-stream", body)
		response.Header.Set("Content-Range", "bytes 0-0/1")
		response.Header.Set("Content-Length", strconv.Itoa(len(body)))
		return response, nil
	})}
	relayURL, cleanup := registerRelayWithHeadersForTest(t, relay, "override-headers", "https://1.1.1.1/media.mkv", map[string]string{"User-Agent": "provider-pinned"})
	defer cleanup()

	first := fetchRelayWithHeaders(t, relay, relayURL, http.MethodGet, "bytes=0-0", map[string]string{"User-Agent": "client-a"})
	second := fetchRelayWithHeaders(t, relay, relayURL, http.MethodGet, "bytes=0-0", map[string]string{"User-Agent": "client-b"})
	if first.body != "ua=provider-pinned" || second.body != "ua=provider-pinned" {
		t.Fatalf("bodies = %q / %q, want the registration override", first.body, second.body)
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (identical effective headers share an entry)", got)
	}
}

// TestRelayRangeCacheConcurrentMissesServeCompleteBodies proves concurrent
// misses each receive a complete body and that only a fully read entry is
// published: the next request after the burst is served from cache.
func TestRelayRangeCacheConcurrentMissesServeCompleteBodies(t *testing.T) {
	const concurrent = 4
	body := strings.Repeat("c", 128)
	var calls int32
	arrived := make(chan struct{}, concurrent)
	release := make(chan struct{})
	relay := NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()
	relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		arrived <- struct{}{}
		<-release
		response := relayResponse(request, http.StatusPartialContent, "application/octet-stream", body)
		response.Header.Set("Content-Range", "bytes 0-127/1000")
		response.Header.Set("Content-Length", strconv.Itoa(len(body)))
		return response, nil
	})}
	relayURL, cleanup := registerRelayForTest(t, relay, "concurrent-miss", "https://1.1.1.1/media.mkv")
	defer cleanup()

	type result struct {
		status int
		body   string
	}
	results := make(chan result, concurrent)
	var wg sync.WaitGroup
	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := fetchRelay(t, relay, relayURL, http.MethodGet, "bytes=0-127")
			results <- result{status: got.status, body: got.body}
		}()
	}
	for i := 0; i < concurrent; i++ {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent upstream requests did not start")
		}
	}
	close(release)
	wg.Wait()
	close(results)
	for got := range results {
		if got.status != http.StatusPartialContent || got.body != body {
			t.Fatalf("concurrent response = status %d, %d bytes, want a complete %d-byte body", got.status, len(got.body), len(body))
		}
	}
	if got := fetchRelay(t, relay, relayURL, http.MethodGet, "bytes=0-127"); got.status != http.StatusPartialContent || got.body != body {
		t.Fatalf("post-concurrency response = status %d, %d bytes, want the cached complete body", got.status, len(got.body))
	}
	if got := atomic.LoadInt32(&calls); got != concurrent {
		t.Fatalf("upstream calls = %d, want %d (a concurrent miss must not serve a partial or stale entry)", got, concurrent)
	}
}

// TestRelayRangeCacheEvictsUnderByteCap proves the total-bytes cap is enforced
// and that accounting stays exact after eviction.
func TestRelayRangeCacheEvictsUnderByteCap(t *testing.T) {
	cache := newRelayRangeCache()
	now := time.Unix(1_700_000_000, 0)
	cache.now = func() time.Time { return now }
	body := make([]byte, relayRangeCacheMaxEntrySize)
	for i := 0; i < 80; i++ {
		cache.put("key-"+strconv.Itoa(i), relayRangeCacheEntry{status: http.StatusPartialContent, body: body})
	}
	if cache.totalBytes > relayRangeCacheMaxTotalSize {
		t.Fatalf("cached bytes = %d, want <= %d", cache.totalBytes, relayRangeCacheMaxTotalSize)
	}
	if want := relayRangeCacheMaxTotalSize / relayRangeCacheMaxEntrySize; len(cache.entries) > want {
		t.Fatalf("entries = %d, want <= %d", len(cache.entries), want)
	}
	sum := 0
	for _, entry := range cache.entries {
		sum += len(entry.body)
	}
	if sum != cache.totalBytes {
		t.Fatalf("totalBytes = %d, want the retained body sum %d", cache.totalBytes, sum)
	}
}

// TestRelayRegistrationStatusLiveExpiredAndEvicted proves the registration
// check distinguishes a live registration from one whose lifetime elapsed and
// from one evicted at capacity. The expired case is enforced when the status is
// queried (like the presentation path), not only when an unrelated registration
// triggers eviction.
func TestRelayRegistrationStatusLiveExpiredAndEvicted(t *testing.T) {
	relay := NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()

	liveURL, cleanup := registerRelayForTest(t, relay, "status-live", "https://1.1.1.1/live.mkv")
	defer cleanup()
	if got := relay.RegistrationStatus(liveURL); got != RegistrationLive {
		t.Fatalf("live status = %v, want RegistrationLive", got)
	}

	relay.mu.Lock()
	relay.entries["status-live"].createdAt = time.Now().Add(-relayEntryLifetime - time.Minute)
	relay.mu.Unlock()
	if got := relay.RegistrationStatus(liveURL); got != RegistrationAbsent {
		t.Fatalf("expired status = %v, want RegistrationAbsent", got)
	}
	relay.mu.Lock()
	_, stillPresent := relay.entries["status-live"]
	relay.mu.Unlock()
	if stillPresent {
		t.Fatal("expired entry was not dropped by the status check")
	}

	// Seed a full table and evict the oldest, then confirm the evicted URL is
	// reported absent while a newer one stays live.
	base := time.Now().Add(-time.Hour)
	var evictedURL string
	relay.mu.Lock()
	for i := 0; i < relayMaxEntries; i++ {
		source, _ := url.Parse("https://1.1.1.1/evict")
		token := "status-old-" + strconv.Itoa(i)
		relay.entries[token] = &relayEntry{source: source, baseName: "stream", createdAt: base.Add(time.Duration(i) * time.Second)}
		if i == 0 {
			evictedURL = "/source/" + token + "/stream"
		}
	}
	relay.evictOldestLocked(relayMaxEntries - 1)
	_, newestPresent := relay.entries["status-old-"+strconv.Itoa(relayMaxEntries-1)]
	relay.mu.Unlock()
	if got := relay.RegistrationStatus(evictedURL); got != RegistrationAbsent {
		t.Fatalf("evicted status = %v, want RegistrationAbsent", got)
	}
	if !newestPresent {
		t.Fatal("the newest entry was evicted before the oldest")
	}
	if got := relay.RegistrationStatus("/source/status-old-" + strconv.Itoa(relayMaxEntries-1) + "/stream"); got != RegistrationLive {
		t.Fatalf("surviving status = %v, want RegistrationLive", got)
	}
}

// TestRelayRegistrationStatusReportsClosed proves a closed relay reports every
// registration absent, so a caller holding a pinned URL learned before shutdown
// does not reuse it against a relay that will refuse the request.
func TestRelayRegistrationStatusReportsClosed(t *testing.T) {
	relay := NewRelay()
	relayURL, cleanup := registerRelayForTest(t, relay, "status-closed", "https://1.1.1.1/closed.mkv")
	defer cleanup()
	if got := relay.RegistrationStatus(relayURL); got != RegistrationLive {
		t.Fatalf("status before close = %v, want RegistrationLive", got)
	}
	if err := relay.Close(context.Background()); err != nil {
		t.Fatalf("close relay: %v", err)
	}
	if got := relay.RegistrationStatus(relayURL); got != RegistrationAbsent {
		t.Fatalf("status after close = %v, want RegistrationAbsent", got)
	}
}

// TestRelayRegistrationStatusReportsUnknownURL proves a URL that is not a relay
// source path — or that names an unknown token — reports absent rather than
// being mistaken for a live registration.
func TestRelayRegistrationStatusReportsUnknownURL(t *testing.T) {
	relay := NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()
	for _, raw := range []string{
		"",
		"not a url",
		"https://example.test/other/path",
		"http://127.0.0.1:1/source/unknown/stream.mkv",
	} {
		if got := relay.RegistrationStatus(raw); got != RegistrationAbsent {
			t.Fatalf("status for %q = %v, want RegistrationAbsent", raw, got)
		}
	}
}

// TestRelayRegistrationStatusReportsUpstreamAuthRejection proves an upstream
// 401 or 403 marks the registration rejected: the proxied request still fails,
// and a later status query reports RegistrationAuthRejected so the caller can
// renew instead of reusing the dead registration.
func TestRelayRegistrationStatusReportsUpstreamAuthRejection(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}))
			defer upstream.Close()

			relay := NewRelay()
			defer func() { _ = relay.Close(context.Background()) }()
			relayURL, release, err := relay.RegisterInsecure(context.Background(), upstream.URL+"/stream.mkv")
			if err != nil {
				t.Fatalf("RegisterInsecure: %v", err)
			}
			defer release()
			if got := relay.RegistrationStatus(relayURL); got != RegistrationLive {
				t.Fatalf("status before request = %v, want RegistrationLive", got)
			}
			parsed, err := url.Parse(relayURL)
			if err != nil {
				t.Fatalf("parse relay URL: %v", err)
			}
			fetchRelay(t, relay, parsed.EscapedPath(), http.MethodGet, "")
			if got := relay.RegistrationStatus(relayURL); got != RegistrationAuthRejected {
				t.Fatalf("status after upstream %d = %v, want RegistrationAuthRejected", status, got)
			}
		})
	}
}

func TestRelayRetriesTransientUpstream5xxOnce(t *testing.T) {
	var calls int
	relay := NewRelay()
	relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return relayResponse(request, http.StatusServiceUnavailable, "text/plain", "flap"), nil
		}
		return relayResponse(request, http.StatusOK, "video/mp4", "0123456789abcdef"), nil
	})}
	relayURL, cleanup := registerRelayForTest(t, relay, "flap", "https://1.1.1.1/video.mp4")
	defer cleanup()
	got := fetchRelay(t, relay, relayURL, http.MethodGet, "")
	if got.status != http.StatusOK {
		t.Fatalf("flapping upstream status = %d, want 200 after one retry", got.status)
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want exactly 2 (one retry, no more)", calls)
	}
}
