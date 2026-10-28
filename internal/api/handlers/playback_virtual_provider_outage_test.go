package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/remotestream"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/resolver"
)

// providerOutageRow builds a session-bound virtual catalog row that is inside
// the trust window: durable identity plus persisted transport evidence (a
// stored URL and a delivery stamp), the precondition for the bounded outage
// retry.
func providerOutageRow(id int, pinned string) *models.MediaFile {
	delivered := time.Now().Add(-time.Minute)
	return &models.MediaFile{
		ID:                         id,
		ContentID:                  "movie-provider-outage",
		FilePath:                   pinned,
		VirtualOwnerInstallationID: 5,
		ResolvedURL:                "https://93.184.216.34/stream/token=stored",
		ResolvedURLExpiresAt:       new(time.Now().Add(2 * time.Hour)),
		LastDeliveredAt:            &delivered,
		UpdatedAt:                  time.Now(),
		ProviderVideoHash:          "hash-a",
		ProviderReleaseName:        "Movie.2024",
	}
}

// providerEmptyListing returns the provider's "title not found" shape: a
// successful answer with no candidates. This is what altmount's Stremio
// listing did during the live incident's 40s blackout.
func providerEmptyListing() error {
	return errors.New("no streams available from provider")
}

// provider502 wraps the resolver's transient provider sentinel the way a real
// upstream 5xx does, so callers can branch on resolver.ErrProviderUnavailable.
func provider502() error {
	return fmt.Errorf("%w: streaming provider returned status 502", resolver.ErrProviderUnavailable)
}

// TestVirtualTransportStartupServesStoredURLDuringProviderBlackout is the live
// incident replay: a transport (re)start for an existing session reaches
// getPlaybackMedia, and the provider listing is empty while the row still holds
// a persisted URL inside the trust window. The startup must serve the stored
// URL (zero provider resolves) instead of failing the transport.
func TestVirtualTransportStartupServesStoredURLDuringProviderBlackout(t *testing.T) {
	tempDir := t.TempDir()
	pinned := "virtual://movie/tt-outage-stored?result=pinned"
	row := providerOutageRow(101, pinned)

	calls := 0
	h := &PlaybackHandler{
		fileResolver:      &fakePinFileResolver{file: row},
		VirtualFileLookup: storedURLLookup(row),
		sessionMgr:        playback.NewSessionManager(0, 0),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
			calls++
			return ResolvedVirtualMedia{}, providerEmptyListing()
		}),
		StartTranscodeFunc: func(_ context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
			opts.OutputDir = tempDir
			return playback.NewReadyTranscodeSessionForTesting(tempDir, opts)
		},
	}
	opts := playback.TranscodeOpts{
		MediaFileID:                      101,
		InputPath:                        pinned,
		VirtualSourceOwnerInstallationID: 5,
		SessionID:                        "outage-stored",
		OutputDir:                        tempDir,
	}
	session, err := h.startLocalPlaybackTransportOnce(context.Background(), opts)
	if err != nil {
		t.Fatalf("startLocalPlaybackTransportOnce failed during provider blackout: %v", err)
	}
	defer func() { _ = session.Close() }()
	if calls != 0 {
		t.Fatalf("provider resolves = %d, want 0: the stored URL must serve without a listing", calls)
	}
}

// TestVirtualTransportStartupRetriesTransientProviderFailure proves the bounded
// retry half: the provider fails twice with a 502 then recovers, and the
// startup serves the recovered candidate instead of returning an error. The
// row is inside the trust window so the retry is authorized.
func TestVirtualTransportStartupRetriesTransientProviderFailure(t *testing.T) {
	tempDir := t.TempDir()
	pinned := "virtual://movie/tt-outage-retry?result=pinned"
	row := providerOutageRow(102, pinned)

	// No stored URL: force the provider resolve path so the retry is exercised.
	row.ResolvedURL = ""

	calls := 0
	h := &PlaybackHandler{
		fileResolver:                &fakePinFileResolver{file: row},
		VirtualFileLookup:           storedURLLookup(row),
		VirtualCandidateTrustWindow: func() time.Duration { return 720 * time.Hour },
		sessionMgr:                  playback.NewSessionManager(0, 0),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
			calls++
			if calls <= 2 {
				return ResolvedVirtualMedia{}, provider502()
			}
			return ResolvedVirtualMedia{URL: "http://127.0.0.1:9/recovered.mp4", URI: pinned, CandidateID: "pinned"}, nil
		}),
		StartTranscodeFunc: func(_ context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
			opts.OutputDir = tempDir
			return playback.NewReadyTranscodeSessionForTesting(tempDir, opts)
		},
	}
	opts := playback.TranscodeOpts{
		MediaFileID:                      102,
		InputPath:                        pinned,
		VirtualSourceOwnerInstallationID: 5,
		SessionID:                        "outage-retry",
		OutputDir:                        tempDir,
	}
	session, err := h.startLocalPlaybackTransportOnce(context.Background(), opts)
	if err != nil {
		t.Fatalf("startLocalPlaybackTransportOnce failed after the provider recovered: %v", err)
	}
	defer func() { _ = session.Close() }()
	// Exactly the initial attempt plus the two bounded retries: the failover
	// loop's neutral substitution must not have run.
	if calls != 3 {
		t.Fatalf("provider resolves = %d, want exactly 3 (initial + 2 bounded retries), not a failover substitution", calls)
	}
}

// TestVirtualTransportStartupProviderOutageIsRetryable503 proves the final
// classification: a provider that stays dead yields the retryable
// provider_unavailable classification (not a bare permanent failure), so the
// client keeps retrying the release it picked.
func TestVirtualTransportStartupProviderOutageIsRetryable503(t *testing.T) {
	pinned := "virtual://movie/tt-outage-dead?result=pinned"
	row := providerOutageRow(103, pinned)
	row.ResolvedURL = ""

	trusted := virtualCandidateTrustedForOutageRetry(row, 720*time.Hour)
	if !trusted {
		t.Fatal("row inside the trust window with delivery evidence must be outage-retry trusted")
	}
	err := classifyVirtualProviderOutage(provider502(), trusted)
	if !errors.Is(err, errVirtualProviderUnavailable) {
		t.Fatalf("classified error = %v, want errVirtualProviderUnavailable", err)
	}
	if !virtualProviderListingOutage(err) {
		t.Fatalf("classified error = %v, want it still recognized as a provider outage", err)
	}

	// A row outside the window must not be classified as retryable: it keeps
	// the pre-existing resolve failure.
	untrusted := providerOutageRow(104, pinned)
	untrusted.UpdatedAt = time.Now().Add(-30 * 24 * time.Hour)
	untrustedTrusted := virtualCandidateTrustedForOutageRetry(untrusted, 720*time.Hour)
	if untrustedTrusted {
		t.Fatal("a row outside the trust window must not be outage-retry trusted")
	}
	if errors.Is(classifyVirtualProviderOutage(provider502(), untrustedTrusted), errVirtualProviderUnavailable) {
		t.Fatal("a row outside the trust window must not be classified as a retryable provider outage")
	}
}

// TestHandleStreamProviderOutageAnswersProviderUnavailable proves the serve
// layer answers 503 provider_unavailable for a trusted session-bound row when
// the provider listing stays empty, instead of the 502 virtual_resolve_failed
// that would be read as a permanent release failure.
func TestHandleStreamProviderOutageAnswersProviderUnavailable(t *testing.T) {
	const pinned = "virtual://movie/tt-serve-outage?result=cand-a"
	row := providerOutageRow(105, pinned)
	row.ResolvedURL = ""

	file := &models.MediaFile{
		ID:                         row.ID,
		ContentID:                  row.ContentID,
		FilePath:                   pinned,
		VirtualOwnerInstallationID: 5,
	}
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := sessionMgr.SetVirtualSource(session.ID, pinned, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}

	handler := NewStreamHandler(sessionMgr, &storedURLStreamFileResolver{row: row})
	handler.RemoteStreamRelay = remotestream.NewRelay()
	defer func() { _ = handler.RemoteStreamRelay.Close(context.Background()) }()
	handler.AllowPrivateStreams = func(int) bool { return true }
	handler.VirtualCandidateTrustWindow = func() time.Duration { return 720 * time.Hour }
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
		return ResolvedVirtualMedia{}, providerEmptyListing()
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID, nil)
	req = req.WithContext(newAuthorizedPlaybackContext())
	req = withPlaybackRouteParam(req, "session_id", session.ID)
	rr := httptest.NewRecorder()
	handler.HandleStream(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 provider_unavailable, body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "provider_unavailable") {
		t.Fatalf("body = %q, want the provider_unavailable error code", rr.Body.String())
	}
}

// TestVirtualTransportStartupRefusesSilentSiblingOnExistingSession proves
// requirement (c): the startup fallback loop for an existing session must not
// silently swap to a different release. A sibling that does not re-match the
// row's durable identity is refused rather than served under the viewer's pin.
func TestVirtualTransportStartupRefusesSilentSiblingOnExistingSession(t *testing.T) {
	tempDir := t.TempDir()
	neutral := "virtual://movie/tt-outage-swap"
	pinned := neutral + "?result=pinned"
	sibling := neutral + "?result=sibling"
	row := providerOutageRow(106, pinned)
	row.ResolvedURL = ""

	// The first resolve fails with a genuine absent-pin cause (not a transient
	// outage), so the loop advances to the substitution attempt; that attempt
	// returns a different release with no identity match.
	h := &PlaybackHandler{
		fileResolver:      &fakePinFileResolver{file: row},
		VirtualFileLookup: storedURLLookup(row),
		sessionMgr:        playback.NewSessionManager(0, 0),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			if strings.Contains(uri, "result=pinned") {
				return ResolvedVirtualMedia{}, absentSessionPinError("pinned")
			}
			return ResolvedVirtualMedia{URL: "http://127.0.0.1:9/sibling.mp4", URI: sibling, CandidateID: "sibling"}, nil
		}),
		StartTranscodeFunc: func(_ context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
			opts.OutputDir = tempDir
			return playback.NewReadyTranscodeSessionForTesting(tempDir, opts)
		},
	}
	// Bind an existing session so sessionVirtualURI is non-empty.
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", row.ID, playback.PlayRemux, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := sessionMgr.SetVirtualSource(session.ID, pinned, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	h.sessionMgr = sessionMgr

	opts := playback.TranscodeOpts{
		MediaFileID:                      106,
		InputPath:                        pinned,
		VirtualSourceOwnerInstallationID: 5,
		SessionID:                        session.ID,
		OutputDir:                        tempDir,
	}
	_, err = h.startLocalPlaybackTransportOnce(context.Background(), opts)
	if err == nil {
		t.Fatal("startup served a sibling release; an existing session must refuse a silent swap")
	}
	if !strings.Contains(err.Error(), "different release") {
		t.Fatalf("startup error = %v, want the silent-release-swap refusal", err)
	}
}

// TestVirtualProviderListingOutageClassification pins the outage predicate so a
// future resolver error rename cannot silently disable the retry.
func TestVirtualProviderListingOutageClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"provider sentinel", provider502(), true},
		{"empty listing", providerEmptyListing(), true},
		{"trusted persisted absent", fmt.Errorf("no longer listed: %w", virtuallibrary.ErrPersistedCandidateTrusted), true},
		{"title without streams", fmt.Errorf("resolve: %w", virtuallibrary.ErrTitleWithoutStreams), true},
		{"session-bound absent renumber artifact", absentSessionPinError("pinned"), true},
		{"nil", nil, false},
		{"other", errors.New("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := virtualProviderListingOutage(tc.err); got != tc.want {
				t.Fatalf("virtualProviderListingOutage(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// outageStartupProvider drives a transport startup against a session-bound row
// whose provider fails every listing, counting resolves. The resolver always
// reports a transient 5xx, so the only thing that can stop the retry loop early
// is the loop's own context observation — exactly what the prompt-cancellation
// test needs to prove.
type outageStartupProvider struct {
	calls  atomic.Int64
	onCall func(n int64)
}

func (p *outageStartupProvider) resolver() VirtualMediaDetailedResolver {
	return VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		n := p.calls.Add(1)
		if p.onCall != nil {
			p.onCall(n)
		}
		return ResolvedVirtualMedia{}, provider502()
	})
}

// outageStartupCatalog is a race-free catalog double for the concurrent outage
// test: reads hand back a fresh copy and the dead-pin write is an inert no-op,
// so N goroutines sharing one handler never touch the same mutable row. The
// pre-existing fakePinFileResolver mutates its row in place, which is fine for
// single-threaded tests but a data race under concurrent startups.
type outageStartupCatalog struct{ row *models.MediaFile }

func (c *outageStartupCatalog) GetByID(context.Context, int) (*models.MediaFile, error) {
	copy := *c.row
	return &copy, nil
}

func (c *outageStartupCatalog) ReplaceVirtualResultPin(context.Context, int, string, string) (bool, error) {
	return true, nil
}

func (c *outageStartupCatalog) lookup(_ context.Context, path string) (*models.MediaFile, error) {
	if c.row == nil || c.row.FilePath != path {
		return nil, ErrVirtualCandidateNotFound
	}
	copy := *c.row
	return &copy, nil
}

// outageStartupHandler builds a handler whose transport startup resolves through
// provider. The failover cap is pinned to one attempt so the startup's provider
// calls are exactly the retry machinery's bound (initial + two retries); the
// pre-existing neutral-failover attempt would otherwise add one more call and
// hide a runaway retry.
func outageStartupHandler(t *testing.T, row *models.MediaFile, provider *outageStartupProvider) *PlaybackHandler {
	t.Helper()
	catalog := &outageStartupCatalog{row: row}
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	handler.fileResolver = catalog
	handler.VirtualFileLookup = catalog.lookup
	handler.VirtualCandidateTrustWindow = func() time.Duration { return 720 * time.Hour }
	handler.PlaybackConfig = func() config.PlaybackConfig {
		return config.PlaybackConfig{MaxVirtualFailoverAttempts: 1, TranscodeEnabled: true}
	}
	handler.VirtualMediaDetailedResolver = provider.resolver()
	handler.StartTranscodeFunc = func(_ context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
		opts.OutputDir = t.TempDir()
		return playback.NewReadyTranscodeSessionForTesting(opts.OutputDir, opts)
	}
	return handler
}

// outageStartupSession binds session id to row's pinned candidate so the startup
// treats it as an existing session (sessionVirtualURI non-empty), which is the
// path the outage retry protects.
func outageStartupSession(t *testing.T, handler *PlaybackHandler, fileID int, pinned string) string {
	t.Helper()
	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", fileID, playback.PlayRemux, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := manager.SetVirtualSource(session.ID, pinned, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	handler.sessionMgr = manager
	return session.ID
}

// TestVirtualTransportConcurrentProviderOutageCallBounds drives N concurrent
// transport startups against the same session-bound row while the provider
// fails every listing. It asserts the retry never amplifies calls (at most one
// initial attempt plus two retries per startup, so ≤ N×3), and that once every
// startup has returned no provider call is still in flight.
func TestVirtualTransportConcurrentProviderOutageCallBounds(t *testing.T) {
	const concurrency = 8
	// Bound the waits: the backoff schedule is 1s + 2s, so a startup that walks
	// both retries takes ≤ ~3s. A cancellable context per startup keeps the slow
	// path bounded without depending on wall-clock luck.
	pinned := "virtual://movie/tt-concurrent-outage?result=pinned"
	row := providerOutageRow(201, pinned)
	row.ResolvedURL = ""

	provider := &outageStartupProvider{}
	handler := outageStartupHandler(t, row, provider)
	sessionID := outageStartupSession(t, handler, row.ID, pinned)

	baselineGoroutines := runtime.NumGoroutine()

	var wg sync.WaitGroup
	started := make(chan struct{}, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started <- struct{}{}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, _ = handler.startLocalPlaybackTransportOnce(ctx, playback.TranscodeOpts{
				MediaFileID:                      row.ID,
				InputPath:                        pinned,
				VirtualSourceOwnerInstallationID: 5,
				SessionID:                        sessionID,
			})
		}()
	}
	for i := 0; i < concurrency; i++ {
		<-started
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent outage startups did not finish")
	}

	if got := provider.calls.Load(); got > concurrency*3 {
		t.Fatalf("provider resolves = %d, want ≤ %d (N×3: initial + 2 retries per startup), no unbounded amplification", got, concurrency*3)
	}
	if got := provider.calls.Load(); got == 0 {
		t.Fatal("provider resolves = 0; the outage path was never exercised")
	} else {
		t.Logf("concurrent provider resolves = %d across %d startups (bound %d)", got, concurrency, concurrency*3)
	}

	// No leaked work: after every startup returned, the call count must stop
	// growing. Wait on observable stability, not a fixed sleep.
	if grew := waitForProviderCallsToQuiesce(&provider.calls, 250*time.Millisecond, 3*time.Second); grew {
		t.Fatalf("provider calls kept growing after startups finished (%d); a retry loop leaked", provider.calls.Load())
	}

	// A returned startup owns no retry goroutine: goroutine count settles back
	// near its pre-start baseline. A settle wait keeps scheduler/GC noise from
	// flaking the assertion while still catching a leaked retry loop.
	if goroutinesLeaked := goroutinesExceedBaseline(baselineGoroutines, 8, 3*time.Second); goroutinesLeaked {
		t.Fatalf("goroutines = %d, want within 8 of the pre-start baseline %d; a retry goroutine leaked", runtime.NumGoroutine(), baselineGoroutines)
	}
}

// goroutinesExceedBaseline reports whether the goroutine count fails to return
// to within slack of target before timeout. It polls the observable runtime
// count so a leaked retry goroutine is caught while transient scheduler growth
// is not falsely flagged.
func goroutinesExceedBaseline(target, slack int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if runtime.NumGoroutine() <= target+slack {
			return false
		}
		if time.Now().After(deadline) {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// waitForProviderCallsToQuiesce reports whether calls kept growing after a
// quiet window. It returns false once the count is stable across the window or
// the timeout elapses, so a leaked goroutine that stopped is not falsely
// reported and a live one is caught.
func waitForProviderCallsToQuiesce(calls *atomic.Int64, quiet, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	last := calls.Load()
	stableSince := time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(quiet / 4)
		now := calls.Load()
		if now != last {
			last = now
			stableSince = time.Now()
			continue
		}
		if time.Since(stableSince) >= quiet {
			return false
		}
	}
	return calls.Load() != last
}

// TestVirtualTransportProviderOutageCancellationIsPrompt proves a startup whose
// context is canceled mid-retry stops promptly: the retry loop observes the
// canceled context on its next resolve and returns, and the elapsed time stays
// well under the full backoff budget (1s + 2s).
func TestVirtualTransportProviderOutageCancellationIsPrompt(t *testing.T) {
	pinned := "virtual://movie/tt-outage-cancel?result=pinned"
	row := providerOutageRow(202, pinned)
	row.ResolvedURL = ""

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var once sync.Once
	provider := &outageStartupProvider{}
	// Cancel from within the second resolve (call 2). The provider always
	// returns a transient failure, so only the retry loop's own context
	// observation can stop it: the next backoff (1s, then 2s) must see the
	// canceled context and return instead of sleeping the full 2s and issuing
	// a third resolve.
	provider.onCall = func(n int64) {
		if n == 2 {
			once.Do(cancel)
		}
	}
	handler := outageStartupHandler(t, row, provider)
	sessionID := outageStartupSession(t, handler, row.ID, pinned)

	start := time.Now()
	_, _ = handler.startLocalPlaybackTransportOnce(ctx, playback.TranscodeOpts{
		MediaFileID:                      row.ID,
		InputPath:                        pinned,
		VirtualSourceOwnerInstallationID: 5,
		SessionID:                        sessionID,
	})
	elapsed := time.Since(start)

	// The full budget is len(backoff) waits = 3s. A prompt cancellation is
	// observed by sleepWithContext on the very next backoff, so the startup
	// returns well before the full budget.
	budget := time.Duration(0)
	for _, d := range virtualProviderOutageBackoff {
		budget += d
	}
	if elapsed >= budget {
		t.Fatalf("canceled startup took %s, want < the full backoff budget %s", elapsed, budget)
	}
	if got := provider.calls.Load(); got > 2 {
		t.Fatalf("provider resolves after cancellation = %d, want ≤ 2 (no final retry after cancel)", got)
	}
	t.Logf("canceled startup returned in %s (full budget %s) after %d provider calls", elapsed, budget, provider.calls.Load())
}
