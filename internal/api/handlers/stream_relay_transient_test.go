package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/remotestream"
)

// These tests pin the serve-layer contract for an exhausted relay 5xx: a
// transport outage must never become a durable dead-candidate verdict. Two
// 503s half a second apart cannot distinguish a provider outage from a dead
// release, so the pinned candidate is not stamped; bounded sibling rotation for
// the current attempt is preserved.
//
// The relay marks an exhausted 5xx / unreachable-upstream response with
// X-Silo-Relay-Temporary; the serve layer reads it through
// remotestream.RelayTemporaryFailure.

// relayTransientServer returns an httptest server that answers every media
// request with the given status until `succeedAfter` requests have been seen,
// then serves a small media body. succeedAfter = 0 means never succeed.
func relayTransientServer(status int, succeedAfter int32, body string) (*httptest.Server, *atomic.Int32) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if succeedAfter > 0 && n >= succeedAfter {
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte(body))
			return
		}
		w.WriteHeader(status)
	}))
	return server, &calls
}

// relayTransientFixture builds a live direct-play session bound to a virtual
// pin whose resolver returns a URL per call, with a real relay so the loopback
// direct-play path runs end to end.
func relayTransientFixture(t *testing.T, file *models.MediaFile, resolve func(call int, forceRefresh, rotate bool, excluded []string) ResolvedVirtualMedia) (*StreamHandler, *playback.Session, *int, *[]bool) {
	t.Helper()
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := sessionMgr.SetVirtualSource(session.ID, file.FilePath, file.VirtualOwnerInstallationID); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	handler := NewStreamHandler(sessionMgr, testPlaybackFileResolver{file: file})
	handler.RemoteStreamRelay = remotestream.NewRelay()
	t.Cleanup(func() { _ = handler.RemoteStreamRelay.Close(context.Background()) })
	handler.AllowPrivateStreams = func(int) bool { return true }
	calls := new(int)
	rotateFlags := new([]bool)
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(ctx context.Context, _ string, _ int, _ int, _ string, forceRefresh bool, excluded []string, _ string) (ResolvedVirtualMedia, error) {
		call := *calls
		*calls++
		*rotateFlags = append(*rotateFlags, VirtualCandidateRotationAllowed(ctx))
		return resolve(call, forceRefresh, VirtualCandidateRotationAllowed(ctx), excluded), nil
	})
	return handler, session, calls, rotateFlags
}

func streamRequestFor(session *playback.Session) (*httptest.ResponseRecorder, *http.Request) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID, nil).WithContext(newAuthorizedPlaybackContext())
	req = withPlaybackRouteParam(req, "session_id", session.ID)
	return httptest.NewRecorder(), req
}

// TestVirtualDirectPlayRepeated5xxDoesNotIndict proves a pinned release whose
// provider answers 503 on every attempt (relay open retry + the bounded
// rotation) is never stamped failed. The rotation is still allowed for this
// attempt, retries are bounded, and every adopted registration is released.
func TestVirtualDirectPlayRepeated5xxDoesNotIndict(t *testing.T) {
	const (
		pinnedURI = "virtual://movie/tt-repeated-5xx?result=cand-a"
		media     = "media-bytes"
	)
	server, upstreamCalls := relayTransientServer(http.StatusServiceUnavailable, 0, media)
	defer server.Close()

	file := &models.MediaFile{ID: 501, ContentID: "movie-repeated-5xx", FilePath: pinnedURI, VirtualOwnerInstallationID: 5}
	var stamps []string
	handler, session, resolverCalls, rotateFlags := relayTransientFixture(t, file,
		func(_ int, _ bool, _ bool, _ []string) ResolvedVirtualMedia {
			return ResolvedVirtualMedia{URL: server.URL + "/provider/video.mp4", URI: pinnedURI, CandidateID: "cand-a"}
		})
	handler.VirtualCandidateFailMarker = func(_ context.Context, _ int, expectedFilePath string, _ *time.Time) error {
		stamps = append(stamps, expectedFilePath)
		return nil
	}

	started := time.Now()
	recorder, req := streamRequestFor(session)
	handler.HandleStream(recorder, req)
	elapsed := time.Since(started)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want the preserved 502, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(stamps) != 0 {
		t.Fatalf("transport 5xx stamped candidate(s) %v; a transport error must not persist a dead-candidate verdict", stamps)
	}
	if *resolverCalls != 2 {
		t.Fatalf("resolver calls = %d, want exactly 2 (initial + bounded rotation)", *resolverCalls)
	}
	// The rotation retry is still declared: it lets a same-release sibling
	// serve this attempt even though the pin was not stamped.
	if len(*rotateFlags) != 2 || !(*rotateFlags)[1] {
		t.Fatalf("rotation flags = %v, want the retry to declare bounded rotation", *rotateFlags)
	}
	// Two relay opens each absorb one 5xx retry: 2 attempts per open.
	if n := upstreamCalls.Load(); n != 4 {
		t.Fatalf("upstream calls = %d, want 4 (2 open attempts x 2 serve attempts)", n)
	}
	if got := handler.RemoteStreamRelay.ActiveRegistrations(); got != 0 {
		t.Fatalf("live relay registrations after serve = %d, want 0 (no leak)", got)
	}
	// Retry stacking bound: the relay adds at most one backoff per open, and
	// there are two opens (one relay retry x the serve layer's one
	// same-release retry), so the total relay-added latency is about
	// 2 x relayOpenRetryBackoff. Assert it stays there so the stack cannot
	// silently grow past the tightest 15s virtual budget as more retries are
	// added. The 500ms here mirrors unexported relayUpstreamOpenRetryBackoff;
	// the relay-side boundary test pins the real value.
	const relayOpenRetryBackoff = 500 * time.Millisecond
	if elapsed > 2*relayOpenRetryBackoff+time.Second {
		t.Fatalf("repeated 5xx stacked %v of relay retries; want <= ~2 backoff (+1s slack) so it fits a 15s caller budget", elapsed)
	}
	t.Logf("repeated-5xx: elapsed=%v upstream_calls=%d resolver_calls=%d", elapsed.Round(time.Millisecond), upstreamCalls.Load(), *resolverCalls)
}

// TestVirtualDirectPlay5xxRotatesToSiblingWithoutIndicting proves bounded
// rotation still works when the pinned release is unavailable: the rotation
// retry serves the sibling's bytes and the pin is not stamped dead.
func TestVirtualDirectPlay5xxRotatesToSiblingWithoutIndicting(t *testing.T) {
	const (
		pinnedURI  = "virtual://movie/tt-5xx-sibling?result=pinned"
		siblingURI = "virtual://movie/tt-5xx-sibling?result=sibling"
		media      = "sibling-media-bytes"
	)
	dead, deadCalls := relayTransientServer(http.StatusServiceUnavailable, 0, "")
	defer dead.Close()
	sibling, siblingCalls := relayTransientServer(http.StatusOK, 1, media)
	defer sibling.Close()

	file := &models.MediaFile{ID: 502, ContentID: "movie-5xx-sibling", FilePath: pinnedURI, VirtualOwnerInstallationID: 5}
	var stamps []string
	handler, session, resolverCalls, rotateFlags := relayTransientFixture(t, file,
		func(call int, _ bool, _ bool, excluded []string) ResolvedVirtualMedia {
			if call == 0 {
				return ResolvedVirtualMedia{URL: dead.URL + "/provider/dead.mp4", URI: pinnedURI, CandidateID: "pinned"}
			}
			if len(excluded) != 1 || excluded[0] != "pinned" {
				t.Fatalf("rotation exclusions = %v, want the unavailable pin excluded", excluded)
			}
			return ResolvedVirtualMedia{URL: sibling.URL + "/provider/sibling.mp4", URI: siblingURI, CandidateID: "sibling"}
		})
	handler.VirtualCandidateFailMarker = func(_ context.Context, _ int, expectedFilePath string, _ *time.Time) error {
		stamps = append(stamps, expectedFilePath)
		return nil
	}

	recorder, req := streamRequestFor(session)
	handler.HandleStream(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want the sibling 200, body = %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Body.String() != media {
		t.Fatalf("body = %q, want %q", recorder.Body.String(), media)
	}
	if len(stamps) != 0 {
		t.Fatalf("transport 5xx stamped candidate(s) %v", stamps)
	}
	if *resolverCalls != 2 {
		t.Fatalf("resolver calls = %d, want 2 (initial + bounded rotation)", *resolverCalls)
	}
	if len(*rotateFlags) != 2 || !(*rotateFlags)[1] {
		t.Fatalf("rotation flags = %v, want the retry to declare rotation", *rotateFlags)
	}
	// The dead pin: 2 open attempts (one relay retry). The sibling: 1.
	if n := deadCalls.Load(); n != 2 {
		t.Fatalf("dead upstream calls = %d, want 2", n)
	}
	if n := siblingCalls.Load(); n != 1 {
		t.Fatalf("sibling upstream calls = %d, want 1", n)
	}
	if got := handler.RemoteStreamRelay.ActiveRegistrations(); got != 0 {
		t.Fatalf("live relay registrations after serve = %d, want 0", got)
	}
}

// TestVirtualDirectPlayDialFailureDoesNotIndict proves an unreachable provider
// (dial failure) is treated like the 5xx case: no indictment, bounded rotation
// preserved, registration cleanup complete.
func TestVirtualDirectPlayDialFailureDoesNotIndict(t *testing.T) {
	const pinnedURI = "virtual://movie/tt-dial-fail?result=cand-a"
	// Reserve an ephemeral port and close it so the dial is refused.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	file := &models.MediaFile{ID: 503, ContentID: "movie-dial-fail", FilePath: pinnedURI, VirtualOwnerInstallationID: 5}
	var stamps []string
	handler, session, resolverCalls, rotateFlags := relayTransientFixture(t, file,
		func(_ int, _ bool, _ bool, _ []string) ResolvedVirtualMedia {
			return ResolvedVirtualMedia{URL: deadURL + "/provider/video.mp4", URI: pinnedURI, CandidateID: "cand-a"}
		})
	handler.VirtualCandidateFailMarker = func(_ context.Context, _ int, expectedFilePath string, _ *time.Time) error {
		stamps = append(stamps, expectedFilePath)
		return nil
	}

	recorder, req := streamRequestFor(session)
	handler.HandleStream(recorder, req)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(stamps) != 0 {
		t.Fatalf("dial failure stamped candidate(s) %v", stamps)
	}
	if *resolverCalls != 2 {
		t.Fatalf("resolver calls = %d, want 2 (initial + bounded rotation)", *resolverCalls)
	}
	if len(*rotateFlags) != 2 || !(*rotateFlags)[1] {
		t.Fatalf("rotation flags = %v, want the retry to declare rotation", *rotateFlags)
	}
	if got := handler.RemoteStreamRelay.ActiveRegistrations(); got != 0 {
		t.Fatalf("live relay registrations after serve = %d, want 0", got)
	}
}

// TestVirtualDirectPlayVerdictStatusStillIndictsAndRotates guards the
// pinned-contract regression: an upstream 404 (a verdict-shaped failure the
// relay collapses to 502 with no temporary marker) must still stamp the
// candidate and rotate. The temporary-failure change must not swallow a real
// verdict.
func TestVirtualDirectPlayVerdictStatusStillIndictsAndRotates(t *testing.T) {
	const (
		pinnedURI  = "virtual://movie/tt-verdict?result=pinned"
		siblingURI = "virtual://movie/tt-verdict?result=sibling"
	)
	dead, deadCalls := relayTransientServer(http.StatusNotFound, 0, "")
	defer dead.Close()
	sibling, _ := relayTransientServer(http.StatusOK, 0, "sibling-media")
	defer sibling.Close()

	file := &models.MediaFile{ID: 504, ContentID: "movie-verdict", FilePath: pinnedURI, VirtualOwnerInstallationID: 5}

	handler, session, resolverCalls, rotateFlags := relayTransientFixture(t, file,
		func(call int, _ bool, _ bool, _ []string) ResolvedVirtualMedia {
			if call == 0 {
				return ResolvedVirtualMedia{URL: dead.URL + "/provider/dead.mp4", URI: pinnedURI, CandidateID: "pinned"}
			}
			return ResolvedVirtualMedia{URL: sibling.URL + "/provider/sibling.mp4", URI: siblingURI, CandidateID: "sibling"}
		})
	var stamps []string
	handler.VirtualCandidateFailMarker = func(_ context.Context, _ int, expectedFilePath string, _ *time.Time) error {
		stamps = append(stamps, expectedFilePath)
		return nil
	}

	recorder, req := streamRequestFor(session)
	handler.HandleStream(recorder, req)

	if len(stamps) != 1 || !strings.Contains(stamps[0], "result=pinned") {
		t.Fatalf("verdict indictment stamps = %v, want exactly the pinned candidate", stamps)
	}
	if len(*rotateFlags) != 2 || !(*rotateFlags)[1] {
		t.Fatalf("rotation flags = %v, want the retry to declare rotation", *rotateFlags)
	}
	if *resolverCalls != 2 {
		t.Fatalf("resolver calls = %d, want 2 (initial + rotation)", *resolverCalls)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want the sibling 200, body = %s", recorder.Code, recorder.Body.String())
	}
	if upstreamCalls := deadCalls.Load(); upstreamCalls != 1 {
		t.Fatalf("dead-upstream calls = %d, want 1 (a 404 verdict never retries)", upstreamCalls)
	}
}
