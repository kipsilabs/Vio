package remotestream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These tests are the end-to-end timeout proof for the relay's open phase. They
// drive the real loopback listener with a provider that delays response headers
// and abandon the request the way an HTTP caller does: by canceling its request
// context or hitting a deadline, which the loopback server only observes as a
// disconnect (its own request context is not handed the caller's deadline). The
// assertions are the three the review asked for:
//
//   - bounded elapsed time: an abandoned open returns promptly instead of
//     waiting out the delayed provider headers;
//   - no retry after abandonment: exactly one provider open, even after a quiet
//     window longer than the open-phase backoff;
//   - no durable indictment: the registration stays live and serveable, so a
//     transport flap can never look like a verdict about the release.
//
// The unit fault-injection matrix in relay_fault_injection_test.go drives
// handle() directly with a supplied context. These tests exist because that
// shape cannot observe the disconnect path: a caller deadline is not
// automatically visible to the loopback server.

// loopbackDelayedProvider is a real HTTP provider whose response headers are
// delayed until its request context is canceled, or until the test flips serve
// on. Every open is counted and every observed cancellation is recorded, so a
// test can prove both boundedness and the disconnect's propagation.
type loopbackDelayedProvider struct {
	server *httptest.Server

	openCount atomic.Int32
	// entered closes on the first open, letting the test cancel only after the
	// provider is actually stalling on headers.
	entered    chan struct{}
	enteredOne sync.Once
	// cancelSeen closes once a stalled open observed its request context
	// canceled: the downstream disconnect reached the provider.
	cancelSeen chan struct{}
	cancelOne  sync.Once

	// serve makes the provider answer immediately. A test flips it after an
	// abandonment to prove the same registration is still serveable.
	serve atomic.Bool

	body string
	// headerDelay bounds how long a stalled open waits before giving up on its
	// own, so a broken cancellation chain fails the test instead of hanging the
	// suite.
	headerDelay time.Duration
}

func newLoopbackDelayedProvider(body string) *loopbackDelayedProvider {
	provider := &loopbackDelayedProvider{
		entered:     make(chan struct{}),
		cancelSeen:  make(chan struct{}),
		body:        body,
		headerDelay: 30 * time.Second,
	}
	provider.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provider.openCount.Add(1)
		provider.enteredOne.Do(func() { close(provider.entered) })
		if !provider.serve.Load() {
			select {
			case <-r.Context().Done():
				provider.cancelOne.Do(func() { close(provider.cancelSeen) })
				return
			case <-time.After(provider.headerDelay):
				return
			}
		}
		w.Header().Set(headerContentType, "video/mp4")
		w.Header().Set(headerContentLength, strconv.Itoa(len(provider.body)))
		_, _ = io.WriteString(w, provider.body)
	}))
	return provider
}

func (p *loopbackDelayedProvider) Close() { p.server.Close() }

func (p *loopbackDelayedProvider) sourceURL() string {
	return p.server.URL + "/provider/video.mp4"
}

// relayLoopbackRegistration registers the delayed provider on a real relay and
// returns the loopback URL plus its release.
func relayLoopbackRegistration(t *testing.T, provider *loopbackDelayedProvider) (*Relay, string, func()) {
	t.Helper()
	relay := NewRelay()
	t.Cleanup(func() { _ = relay.Close(context.Background()) })
	relayURL, release, err := relay.RegisterInsecure(context.Background(), provider.sourceURL())
	if err != nil {
		t.Fatalf("RegisterInsecure: %v", err)
	}
	return relay, relayURL, release
}

// TestRelayLoopbackAbandonedDelayedHeaderOpenIsBoundedAndNotRetried cancels a
// caller whose open is stalled before headers. The caller must return promptly,
// the provider must see the disconnect, no second open may occur, and the
// registration must stay live and serveable.
func TestRelayLoopbackAbandonedDelayedHeaderOpenIsBoundedAndNotRetried(t *testing.T) {
	provider := newLoopbackDelayedProvider("late-media-bytes")
	defer provider.Close()
	relay, relayURL, release := relayLoopbackRegistration(t, provider)
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-provider.entered:
		case <-time.After(5 * time.Second):
			t.Errorf("provider never received the relay open")
		}
		cancel()
	}()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, relayURL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	started := time.Now()
	response, err := http.DefaultClient.Do(request)
	elapsed := time.Since(started)
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil {
		t.Fatalf("abandoned open returned HTTP %d, want a canceled request", response.StatusCode)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("abandoned open error = %v, want context.Canceled", err)
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("abandoned open waited %v; the %v delayed provider headers must not be waited out", elapsed, provider.headerDelay)
	}

	select {
	case <-provider.cancelSeen:
	case <-time.After(3 * time.Second):
		t.Fatal("the provider never observed the downstream disconnect")
	}

	// Quiet window past the open-phase backoff: a retry would have opened a
	// second connection by now.
	time.Sleep(relayUpstreamOpenRetryBackoff + 200*time.Millisecond)
	if opens := provider.openCount.Load(); opens != 1 {
		t.Fatalf("provider opens = %d, want exactly 1 (no retry after abandonment)", opens)
	}
	if status := relay.RegistrationStatus(relayURL); status != RegistrationLive {
		t.Fatalf("registration status after abandonment = %v, want live", status)
	}

	// The same registration still serves once the provider recovers, proving
	// nothing durable (auth-rejected, evicted) was written by the abandonment.
	provider.serve.Store(true)
	recovery, err := http.Get(relayURL)
	if err != nil {
		t.Fatalf("recovery request: %v", err)
	}
	defer func() { _ = recovery.Body.Close() }()
	body, err := io.ReadAll(recovery.Body)
	if err != nil {
		t.Fatalf("recovery read: %v", err)
	}
	if recovery.StatusCode != http.StatusOK || string(body) != provider.body {
		t.Fatalf("recovery response = %d %q, want 200 %q", recovery.StatusCode, body, provider.body)
	}
	if opens := provider.openCount.Load(); opens != 2 {
		t.Fatalf("provider opens after recovery = %d, want 2", opens)
	}
	t.Logf("abandoned open: elapsed=%v provider_opens=%d backoff=%v",
		elapsed.Round(time.Millisecond), provider.openCount.Load(), relayUpstreamOpenRetryBackoff)
}

// TestRelayLoopbackCallerDeadlineReachesProviderAsDisconnect uses a caller
// context deadline instead of an explicit cancel. The loopback server is never
// handed that deadline directly; it learns about it only when the client
// disconnects. The open must still be bounded, unretried, and non-indicting.
func TestRelayLoopbackCallerDeadlineReachesProviderAsDisconnect(t *testing.T) {
	provider := newLoopbackDelayedProvider("deadline-media-bytes")
	defer provider.Close()
	relay, relayURL, release := relayLoopbackRegistration(t, provider)
	defer release()

	const callerDeadline = 300 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), callerDeadline)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, relayURL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	started := time.Now()
	response, err := http.DefaultClient.Do(request)
	elapsed := time.Since(started)
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil {
		t.Fatalf("deadline open returned HTTP %d, want a deadline failure", response.StatusCode)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline open error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("deadline open waited %v; the %v delayed provider headers must not be waited out", elapsed, provider.headerDelay)
	}

	select {
	case <-provider.cancelSeen:
	case <-time.After(3 * time.Second):
		t.Fatal("the provider never observed the downstream disconnect")
	}

	time.Sleep(relayUpstreamOpenRetryBackoff + 200*time.Millisecond)
	if opens := provider.openCount.Load(); opens != 1 {
		t.Fatalf("provider opens = %d, want exactly 1 (no retry after the caller gave up)", opens)
	}
	if status := relay.RegistrationStatus(relayURL); status != RegistrationLive {
		t.Fatalf("registration status after deadline = %v, want live", status)
	}
	t.Logf("deadline open: elapsed=%v caller_deadline=%v provider_opens=%d",
		elapsed.Round(time.Millisecond), callerDeadline, provider.openCount.Load())
}
