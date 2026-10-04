package remotestream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The relay's open-phase retry is the one place a provider flap is absorbed.
// These tests pin its exact attempt budget and the transport-temporary marker
// it hands the serve layer, so a later change cannot silently turn a transport
// error into a durable dead-candidate verdict.

// TestRelayExhausted5xxRetryIsMarkedTemporary proves a permanently 5xx upstream
// is retried exactly once (two attempts total) and the exhausted result carries
// the temporary marker instead of an unmarked verdict. Two 500s cannot declare
// a release dead.
func TestRelayExhausted5xxRetryIsMarkedTemporary(t *testing.T) {
	var calls atomic.Int32
	relay := NewRelay()
	relay.rangeCache.now = time.Now
	relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		return relayResponse(request, http.StatusServiceUnavailable, "text/plain", "flap"), nil
	})}
	relayURL, cleanup := registerRelayForTest(t, relay, "always-5xx", "https://1.1.1.1/video.mp4")
	defer cleanup()

	got := fetchRelay(t, relay, relayURL, http.MethodGet, "")
	if got.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want the upstream 503 preserved", got.status)
	}
	if got.header.Get(relayTemporaryFailureHeader) == "" {
		t.Fatalf("exhausted 5xx response missing %s; a transport error must be marked temporary", relayTemporaryFailureHeader)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want exactly 2 (one open retry, no more)", n)
	}
}

// TestRelayDialFailureRetriesOnceThenIsMarkedTemporary proves an unreachable
// upstream is retried once and then reported temporary: a dial failure is a
// transport signal, never a durable verdict.
func TestRelayDialFailureRetriesOnceThenIsMarkedTemporary(t *testing.T) {
	var calls atomic.Int32
	relay := NewRelay()
	relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("dial tcp: connection refused")
	})}
	relayURL, cleanup := registerRelayForTest(t, relay, "dial-fail", "https://1.1.1.1/video.mp4")
	defer cleanup()

	got := fetchRelay(t, relay, relayURL, http.MethodGet, "")
	if got.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 for an unreachable upstream", got.status)
	}
	if got.header.Get(relayTemporaryFailureHeader) == "" {
		t.Fatalf("dial-failure response missing %s", relayTemporaryFailureHeader)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want exactly 2 (one open retry)", n)
	}
}

// TestRelayCancelDuringBackoffAbortsWithoutRetry proves a caller that cancels
// during the retry pause is not made to wait it out: the pause observes the
// context and returns the transient failure immediately, with exactly one
// upstream attempt. This is the cancel-during-backoff leg of the matrix.
func TestRelayCancelDuringBackoffAbortsWithoutRetry(t *testing.T) {
	var calls atomic.Int32
	firstAttempt := make(chan struct{})
	relay := NewRelay()
	relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(firstAttempt)
			return relayResponse(request, http.StatusServiceUnavailable, "text/plain", "flap"), nil
		}
		t.Error("relay retried after the caller canceled during backoff")
		return relayResponse(request, http.StatusOK, "video/mp4", "late"), nil
	})}
	relayURL, cleanup := registerRelayForTest(t, relay, "cancel-backoff", "https://1.1.1.1/video.mp4")
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-firstAttempt
		cancel()
	}()

	started := time.Now()
	got := fetchRelayWithContext(t, relay, ctx, relayURL, http.MethodGet, "")
	elapsed := time.Since(started)
	if got.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want the first 503", got.status)
	}
	if elapsed >= relayUpstreamOpenRetryBackoff {
		t.Fatalf("canceled request waited %v, want an immediate return (< %v)", elapsed, relayUpstreamOpenRetryBackoff)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 after cancellation", n)
	}
}

// TestRelayCancelDuringOpenAbortsWithoutRetry proves a blocked dial that is
// canceled before headers returns immediately without consuming the retry
// pause, with exactly one attempt.
func TestRelayCancelDuringOpenAbortsWithoutRetry(t *testing.T) {
	var calls atomic.Int32
	relay := NewRelay()
	relay.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	relayURL, cleanup := registerRelayForTest(t, relay, "cancel-open", "https://1.1.1.1/video.mp4")
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	started := time.Now()
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	got := fetchRelayWithContext(t, relay, ctx, relayURL, http.MethodGet, "")
	elapsed := time.Since(started)
	if got.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 for a canceled open", got.status)
	}
	if got.header.Get(relayTemporaryFailureHeader) == "" {
		t.Fatalf("canceled open missing %s", relayTemporaryFailureHeader)
	}
	if elapsed >= relayUpstreamOpenRetryBackoff {
		t.Fatalf("canceled open waited %v, want an immediate return", elapsed)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 after cancellation", n)
	}
}

// TestRelayStatusCodesDoNotRetryOrMarkTemporary pins the verdict statuses:
// 401/403 mark the registration auth-rejected, 404 collapses to 502, and 416
// is forwarded, each with exactly one upstream attempt and no temporary marker.
func TestRelayStatusCodesDoNotRetryOrMarkTemporary(t *testing.T) {
	cases := []struct {
		name       string
		upstream   int
		wantStatus int
		wantAuth   bool
	}{
		{"unauthorized", http.StatusUnauthorized, http.StatusBadGateway, true},
		{"forbidden", http.StatusForbidden, http.StatusBadGateway, true},
		{"not-found", http.StatusNotFound, http.StatusBadGateway, false},
		{"range-not-satisfiable", http.StatusRequestedRangeNotSatisfiable, http.StatusRequestedRangeNotSatisfiable, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			relay := NewRelay()
			// Auth-rejection marking is keyed by the live registration token,
			// so drive the real Registration path rather than a seeded entry.
			// RegisterInsecure selects the insecure client, so both clients
			// must point at the injected transport.
			transport := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls.Add(1)
				return relayResponse(request, tc.upstream, "text/plain", ""), nil
			})}
			relay.client = transport
			relay.insecureClient = transport
			relayURL, release, err := relay.RegisterInsecure(context.Background(), "https://1.1.1.1/video.mp4")
			if err != nil {
				t.Fatalf("RegisterInsecure: %v", err)
			}
			defer release()

			path := strings.TrimPrefix(relayURL, relay.baseURL)
			got := fetchRelay(t, relay, path, http.MethodGet, "")
			if got.status != tc.wantStatus {
				t.Fatalf("status = %d, want %d", got.status, tc.wantStatus)
			}
			if got.header.Get(relayTemporaryFailureHeader) != "" {
				t.Fatalf("verdict status %d was marked temporary", tc.upstream)
			}
			if n := calls.Load(); n != 1 {
				t.Fatalf("upstream calls = %d, want 1 (verdict statuses never retry)", n)
			}
			wantRegistration := RegistrationLive
			if tc.wantAuth {
				wantRegistration = RegistrationAuthRejected
			}
			if status := relay.RegistrationStatus(relayURL); status != wantRegistration {
				t.Fatalf("registration status = %v, want %v", status, wantRegistration)
			}
		})
	}
}

// TestRelayPartialBodyIsNeverRetried proves an interrupted body after bytes
// already flowed is not retried: a partial body cannot be resumed, and the
// committed response is aborted rather than replayed. Exactly one upstream
// attempt reaches the server.
func TestRelayPartialBodyIsNeverRetried(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "video/mp4")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("partial-bytes"))
		if flusher != nil {
			flusher.Flush()
		}
		hj, ok := w.(http.Hijacker)
		if ok {
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		}
	}))
	defer upstream.Close()

	relay := NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()
	relayURL, release, err := relay.RegisterInsecure(context.Background(), upstream.URL+"/stream.mp4")
	if err != nil {
		t.Fatalf("RegisterInsecure: %v", err)
	}
	defer release()

	resp, err := http.Get(relayURL)
	if err != nil {
		t.Fatalf("GET relay: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, readErr := io.ReadAll(resp.Body)
	if readErr == nil {
		t.Fatal("partial-body stream ended without a read error; the abort was swallowed")
	}
	if len(body) == 0 {
		t.Fatal("no partial bytes were delivered before the abort")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no retry after partial bytes)", n)
	}
}

// TestRelayOpenRetryFitsCallerBudget pins the slow-open boundary directly: the
// pause is skipped when the caller's remaining deadline cannot absorb it, and
// attempted when it can. Timeout values are not changed here; this is the
// evidence that the fixed 500ms retry cannot on its own exhaust a caller whose
// budget is the smaller of the two boundaries.
func TestRelayOpenRetryFitsCallerBudget(t *testing.T) {
	if !relayOpenRetryFitsBudget(context.Background()) {
		t.Fatal("a caller with no deadline should retry")
	}
	short, cancelShort := context.WithTimeout(context.Background(), relayUpstreamOpenRetryBackoff/2)
	defer cancelShort()
	if relayOpenRetryFitsBudget(short) {
		t.Fatal("a caller with less than the backoff remaining must not sleep it out")
	}
	long, cancelLong := context.WithTimeout(context.Background(), 2*relayUpstreamOpenRetryBackoff)
	defer cancelLong()
	if !relayOpenRetryFitsBudget(long) {
		t.Fatal("a caller with room for the backoff should retry")
	}
}

// TestRelaySlowOpenSkipsRetryInsideCallerBudget measures the real slow-open
// boundary: when an upstream takes most of the caller's remaining deadline to
// open, the relay must not add the 500ms backoff on top, or the caller's
// deadline fires before relay recovery. The operation must finish inside the
// caller's deadline with exactly one upstream attempt, and the failure must
// carry the temporary marker rather than a verdict. This is the measured
// evidence behind not changing the fixed timeout values.
func TestRelaySlowOpenSkipsRetryInsideCallerBudget(t *testing.T) {
	const (
		openDelay      = 120 * time.Millisecond
		callerDeadline = 400 * time.Millisecond
	)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case <-time.After(openDelay):
		case <-r.Context().Done():
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	relay := NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()
	relayURL, release, err := relay.RegisterInsecure(context.Background(), server.URL+"/slow.mp4")
	if err != nil {
		t.Fatalf("RegisterInsecure: %v", err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), callerDeadline)
	defer cancel()
	started := time.Now()
	got := fetchRelayWithContext(t, relay, ctx, strings.TrimPrefix(relayURL, relay.baseURL), http.MethodGet, "")
	elapsed := time.Since(started)

	// The first open already consumed openDelay; a 500ms retry pause could not
	// fit the deadline minus the open, so it must be skipped. The total
	// therefore stays close to one open, not one open plus the backoff.
	if elapsed >= relayUpstreamOpenRetryBackoff {
		t.Fatalf("slow open took %v, want no backoff pause (< %v) inside the %v caller deadline", elapsed, relayUpstreamOpenRetryBackoff, callerDeadline)
	}
	if elapsed > callerDeadline {
		t.Fatalf("slow open took %v, past the caller's %v deadline", elapsed, callerDeadline)
	}
	if got.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want the upstream 503", got.status)
	}
	if got.header.Get(relayTemporaryFailureHeader) == "" {
		t.Fatalf("slow-open failure missing %s", relayTemporaryFailureHeader)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (backoff skipped when it would not fit)", n)
	}
	t.Logf("slow-open boundary: elapsed=%v caller_deadline=%v open_delay=%v upstream_calls=%d backoff=%v",
		elapsed.Round(time.Millisecond), callerDeadline, openDelay, calls.Load(), relayUpstreamOpenRetryBackoff)
}

// fetchRelayWithContext drives handle directly with a caller-supplied context,
// which is what lets the cancellation legs observe a canceled request without a
// live listener.
func fetchRelayWithContext(t *testing.T, relay *Relay, ctx context.Context, rawURL, method, byteRange string) relayFetch {
	t.Helper()
	request := httptest.NewRequest(method, "http://relay"+rawURL, nil).WithContext(ctx)
	if byteRange != "" {
		request.Header.Set("Range", byteRange)
	}
	recorder := httptest.NewRecorder()
	relay.handle(recorder, request)
	response := recorder.Result()
	return relayFetch{status: response.StatusCode, header: response.Header.Clone(), body: recorder.Body.String()}
}
