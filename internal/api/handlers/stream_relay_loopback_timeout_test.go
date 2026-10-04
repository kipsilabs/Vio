package handlers

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

	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/auth"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/remotestream"
)

// These tests are the serve-layer half of the end-to-end timeout proof. The
// loopback relay test (internal/remotestream/relay_loopback_test.go) proves the
// relay itself releases an abandoned open; this test proves the whole
// HandleStream path does. The HTTP caller here is a real client against a real
// httptest server: its context deadline is not handed to the serve handler, so
// the only cancellation signal the server sees is the client's disconnect. The
// assertions are the review's three:
//
//   - bounded elapsed time: the serve handler returns promptly after the client
//     walks away instead of waiting out the delayed provider headers;
//   - no retry after abandonment: one provider open and one resolver call, even
//     after a quiet window past the relay's open-phase backoff;
//   - no durable indictment: the fail marker is never called, so a client
//     walking away is never recorded as a dead release.

// abandonableProvider is a real provider that stalls on response headers until
// its request context is canceled (a downstream disconnect) or the test flips
// serve on. Every open and every observed cancellation is counted.
type abandonableProvider struct {
	server *httptest.Server

	openCount  atomic.Int32
	entered    chan struct{}
	enteredOne sync.Once
	cancelSeen chan struct{}
	cancelOne  sync.Once
	serve      atomic.Bool

	body        string
	headerDelay time.Duration
}

func newAbandonableProvider(body string) *abandonableProvider {
	provider := &abandonableProvider{
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
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", strconv.Itoa(len(provider.body)))
		_, _ = io.WriteString(w, provider.body)
	}))
	return provider
}

func (p *abandonableProvider) Close() { p.server.Close() }

// authorizedStreamRequest builds the request HandleStream expects on the live
// server: it keeps the server request's own (cancelable) context and layers the
// test claims, profile, and route param on top.
func authorizedStreamRequest(r *http.Request, sessionID string) *http.Request {
	ctx := apimw.SetClaims(r.Context(), &auth.Claims{UserID: 1, Role: "user", TokenType: auth.TokenTypeAccess})
	ctx = apimw.SetProfileID(ctx, "profile-1")
	return withPlaybackRouteParam(r.WithContext(ctx), "session_id", sessionID)
}

// TestHandleStreamLoopbackAbandonedOpenIsBoundedAndDoesNotIndict drives the real
// serve handler on a real loopback server while the client abandons a stream
// whose provider is stalled on headers. The handler must stop promptly, open the
// provider once, and never stamp the pinned release.
func TestHandleStreamLoopbackAbandonedOpenIsBoundedAndDoesNotIndict(t *testing.T) {
	const (
		pinnedURI = "virtual://movie/tt-loopback-timeout?result=cand-a"
		body      = "late-media-bytes"
	)
	provider := newAbandonableProvider(body)
	defer provider.Close()

	file := &models.MediaFile{ID: 701, ContentID: "movie-loopback-timeout", FilePath: pinnedURI, VirtualOwnerInstallationID: 5}
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := sessionMgr.SetVirtualSource(session.ID, pinnedURI, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}

	var resolverCalls atomic.Int32
	handler := NewStreamHandler(sessionMgr, testPlaybackFileResolver{file: file})
	handler.RemoteStreamRelay = remotestream.NewRelay()
	t.Cleanup(func() { _ = handler.RemoteStreamRelay.Close(context.Background()) })
	handler.AllowPrivateStreams = func(int) bool { return true }
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
		resolverCalls.Add(1)
		return ResolvedVirtualMedia{URL: provider.server.URL + "/provider/video.mp4", URI: pinnedURI, CandidateID: "cand-a"}, nil
	})
	var (
		stampMu    sync.Mutex
		failStamps []string
	)
	handler.VirtualCandidateFailMarker = func(_ context.Context, _ int, expectedFilePath string, _ *time.Time) error {
		stampMu.Lock()
		failStamps = append(failStamps, expectedFilePath)
		stampMu.Unlock()
		return nil
	}

	var serverElapsed atomic.Int64
	handlerDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		handler.HandleStream(w, authorizedStreamRequest(r, session.ID))
		serverElapsed.Store(int64(time.Since(start)))
		close(handlerDone)
	}))
	defer server.Close()

	// Give up 500ms into an open whose headers arrive only after 30s. The
	// server never receives this deadline directly; it must learn from the
	// disconnect alone.
	const callerBudget = 500 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), callerBudget)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/stream/"+session.ID, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	clientErr := make(chan error, 1)
	go func() {
		response, err := server.Client().Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		clientErr <- err
	}()

	select {
	case err := <-clientErr:
		if err == nil {
			t.Fatal("abandoned request returned without an error")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("abandoned request error = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("abandoned request never returned")
	}

	// The serve handler must let go on its own, not after the delayed headers.
	select {
	case <-handlerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("serve handler did not return after the client disconnected")
	}
	if elapsed := time.Duration(serverElapsed.Load()); elapsed >= 3*time.Second {
		t.Fatalf("serve handler held the abandoned open %v; the %v delayed headers must not be waited out", elapsed, provider.headerDelay)
	}

	select {
	case <-provider.cancelSeen:
	case <-time.After(3 * time.Second):
		t.Fatal("the provider never observed the downstream disconnect")
	}

	// A retry would run the resolver again and open the provider a second time;
	// wait past the relay backoff so any late retry would be visible.
	time.Sleep(relayOpenRetryBackoffForTest + 300*time.Millisecond)
	if opens := provider.openCount.Load(); opens != 1 {
		t.Fatalf("provider opens = %d, want exactly 1 (no retry after the client gave up)", opens)
	}
	if calls := resolverCalls.Load(); calls != 1 {
		t.Fatalf("resolver calls = %d, want exactly 1 (no failover resolve for a client disconnect)", calls)
	}
	stampMu.Lock()
	stamps := append([]string(nil), failStamps...)
	stampMu.Unlock()
	if len(stamps) != 0 {
		t.Fatalf("client disconnect stamped candidate(s) %v; abandonment must never be a durable indictment", stamps)
	}
	if got := handler.RemoteStreamRelay.ActiveRegistrations(); got != 0 {
		t.Fatalf("live relay registrations after abandonment = %d, want 0", got)
	}
	t.Logf("serve handler abandoned open: server_elapsed=%v provider_opens=%d resolver_calls=%d stamps=%d",
		time.Duration(serverElapsed.Load()).Round(time.Millisecond), provider.openCount.Load(), resolverCalls.Load(), len(stamps))
}

// relayOpenRetryBackoffForTest mirrors the relay's unexported
// relayUpstreamOpenRetryBackoff (500ms); the relay-side boundary test pins the
// real value. It is a local constant so this test observes a retry window
// without importing the unexported constant.
const relayOpenRetryBackoffForTest = 500 * time.Millisecond
