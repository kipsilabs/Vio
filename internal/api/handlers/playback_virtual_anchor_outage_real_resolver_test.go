package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

// anchorRealResolverProvider serves a Stremio manifest and one release, and
// answers the stream endpoint with the supplied per-request response sequence
// (the last entry repeats). It is the REAL provider transport the resolver
// talks to, so the candidate cache, the fresh-serve floor and the provider
// failure backoff are all exercised rather than stubbed out.
func anchorRealResolverProvider(t *testing.T, responses ...string) *httptest.Server {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/manifest.json" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"org.stremio.anchor","resources":["stream"],"types":["movie","series"]}`))
			return
		}
		index := int(calls.Add(1)) - 1
		if index >= len(responses) {
			index = len(responses) - 1
		}
		body := responses[index]
		if body == "" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

const anchorResolverCandidateJSON = `{"streams":[{"name":"1080p","title":"Movie.2024.1080p.WEB-DL.x264","url":"http://192.168.1.100:8080/pin.mkv","behaviorHints":{"videoHash":"anchor-hash"}}]}`

const anchorResolverEmptyJSON = `{"streams":[]}`

// realAnchorResolver builds the production detailed-resolver adapter around a
// real virtuallibrary.Service. The handlers test package cannot import the
// internal/api adapter (that would be an import cycle), so the three-line
// forwarding is mirrored here. Every resolve runs the real resolver: the
// candidate cache, the fresh-serve floor, the provider failure backoff and the
// outage-relist bypass all apply.
func realAnchorResolver(svc *virtuallibrary.Service) VirtualMediaDetailedResolver {
	return VirtualMediaDetailedResolverFunc(func(ctx context.Context, uri string, ownerInstallationID, userID int, profileID string, forceRefresh bool, excluded []string, preferred string) (ResolvedVirtualMedia, error) {
		res, err := svc.ResolveDetailed(ctx, uri, forceRefresh, excluded, preferred, VirtualSessionBinding(ctx), VirtualCandidateRotationAllowed(ctx))
		if err != nil {
			return ResolvedVirtualMedia{}, err
		}
		return ResolvedVirtualMedia{
			URL: res.URL, URI: res.URI, CandidateID: res.CandidateID,
			RequestHeaders: res.RequestHeaders, ExpiresAt: res.ExpiresAt,
			ProviderVideoHash: res.ProviderVideoHash, ProviderGUID: res.ProviderGUID,
			ProviderReleaseName: res.ProviderReleaseName, ProviderReleaseSize: res.ProviderReleaseSize,
			IdentityRematched: res.IdentityRematched,
			CodecAudio:        res.CodecAudio, AudioLanguages: res.AudioLanguages, SubtitleLanguages: res.SubtitleLanguages,
		}, nil
	})
}

// anchorRealResolverCandidateID learns the release's provider result id once,
// against a healthy provider of its own, so the anchored pin names the exact
// candidate the recovering provider later lists.
func anchorRealResolverCandidateID(t *testing.T, neutralURI string) string {
	t.Helper()
	healthy := anchorRealResolverProvider(t, anchorResolverCandidateJSON)
	svc := virtuallibrary.New(virtuallibrary.Config{
		Enabled: true, ManifestURL: healthy.URL + "/manifest.json",
		AllowInsecureHTTP: true, AllowPrivateStreams: true,
	}, nil, nil)
	if svc == nil {
		t.Fatal("expected a non-nil virtual library service")
	}
	streams, err := svc.ListStreams(context.Background(), neutralURI)
	if err != nil || len(streams) == 0 {
		t.Fatalf("ListStreams: count=%d err=%v, want the release", len(streams), err)
	}
	return streams[0].ID
}

// TestResolveVirtualAnchorURIRealResolverRecoversEmptyThenPin is the cache-aware
// recovery test the mock resolver cannot express. The provider answers an EMPTY
// listing on the first resolve, which the real resolver negative-caches; the
// retry then answers only the original bound pin under the same result id. The
// anchor retry must re-list past the cached empty answer and recover the pin
// instead of replaying the empty. An identity-less row is used deliberately: it
// is the case the inner evidence-gated outage retry cannot cover, so only the
// anchor's own outage-relist marker can make the retry fresh.
func TestResolveVirtualAnchorURIRealResolverRecoversEmptyThenPin(t *testing.T) {
	const neutralURI = "virtual://movie/tt-real-anchor-empty"
	id := anchorRealResolverCandidateID(t, neutralURI)
	pinnedURI := neutralURI + "?result=" + id

	provider := anchorRealResolverProvider(t, anchorResolverEmptyJSON, anchorResolverCandidateJSON)
	svc := virtuallibrary.New(virtuallibrary.Config{
		Enabled: true, ManifestURL: provider.URL + "/manifest.json",
		AllowInsecureHTTP: true, AllowPrivateStreams: true,
	}, nil, nil)
	if svc == nil {
		t.Fatal("expected a non-nil virtual library service")
	}

	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	h.VirtualMediaDetailedResolver = realAnchorResolver(svc)
	// No durable identity, no stored URL, no delivery stamp: an identity-less row.
	file := &models.MediaFile{ID: 41, ContentID: "movie-real-anchor-empty", FilePath: pinnedURI, VirtualOwnerInstallationID: 5}
	session := &playback.Session{ID: "real-anchor-empty", UserID: 1, ProfileID: "profile-1"}

	resolved, cleanup, err := h.resolveVirtualAnchorURIWithRotationV3(context.Background(), session, file)
	if err != nil {
		t.Fatalf("resolveVirtualAnchorURIWithRotationV3: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if resolved.CandidateID != id {
		t.Fatalf("recovered candidate = %q, want the bound pin %q recovered after an empty listing", resolved.CandidateID, id)
	}
	if got := virtualResultCandidateID(resolved.URI); got != id {
		t.Fatalf("recovered URI = %q, want the bound pin %q", resolved.URI, id)
	}
}

// TestResolveVirtualAnchorURIRealResolverRecovers502ThenPin is the failure-backoff
// counterpart: the provider answers 502 on the first resolve, which records the
// provider-failure backoff; the retry then answers the bound pin. The anchor
// retry must bypass that fail-fast negative and recover, which only the outage
// marker makes it do.
func TestResolveVirtualAnchorURIRealResolverRecovers502ThenPin(t *testing.T) {
	const neutralURI = "virtual://movie/tt-real-anchor-502"
	id := anchorRealResolverCandidateID(t, neutralURI)
	pinnedURI := neutralURI + "?result=" + id

	provider := anchorRealResolverProvider(t, "", anchorResolverCandidateJSON)
	svc := virtuallibrary.New(virtuallibrary.Config{
		Enabled: true, ManifestURL: provider.URL + "/manifest.json",
		AllowInsecureHTTP: true, AllowPrivateStreams: true,
	}, nil, nil)
	if svc == nil {
		t.Fatal("expected a non-nil virtual library service")
	}

	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	h.VirtualMediaDetailedResolver = realAnchorResolver(svc)
	file := &models.MediaFile{ID: 42, ContentID: "movie-real-anchor-502", FilePath: pinnedURI, VirtualOwnerInstallationID: 5}
	session := &playback.Session{ID: "real-anchor-502", UserID: 1, ProfileID: "profile-1"}

	resolved, cleanup, err := h.resolveVirtualAnchorURIWithRotationV3(context.Background(), session, file)
	if err != nil {
		t.Fatalf("resolveVirtualAnchorURIWithRotationV3: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if resolved.CandidateID != id {
		t.Fatalf("recovered candidate = %q, want the bound pin %q recovered after a 502", resolved.CandidateID, id)
	}
}

// TestResolveVirtualAnchorURIRealResolverAbsentPinStillRefuses proves the fix
// does not turn a genuine release verdict into a silent recovery: with the real
// resolver, a pin the provider never lists and no durable identity to rematch
// still refuses, so the caller's transport-failure alternate walk runs.
func TestResolveVirtualAnchorURIRealResolverAbsentPinStillRefuses(t *testing.T) {
	const neutralURI = "virtual://movie/tt-real-anchor-absent"
	pinnedURI := neutralURI + "?result=ffffffffffffffffffffffff"

	provider := anchorRealResolverProvider(t, anchorResolverCandidateJSON)
	svc := virtuallibrary.New(virtuallibrary.Config{
		Enabled: true, ManifestURL: provider.URL + "/manifest.json",
		AllowInsecureHTTP: true, AllowPrivateStreams: true,
	}, nil, nil)
	if svc == nil {
		t.Fatal("expected a non-nil virtual library service")
	}

	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	h.VirtualMediaDetailedResolver = realAnchorResolver(svc)
	// No durable identity on the row: the absent pin can never rematch, so the
	// refusal stands. With identity the resolver would rematch the listed
	// release instead, which is a separate recovery path.
	file := &models.MediaFile{
		ID: 43, ContentID: "movie-real-anchor-absent", FilePath: pinnedURI,
		VirtualOwnerInstallationID: 5,
	}
	session := &playback.Session{ID: "real-anchor-absent", UserID: 1, ProfileID: "profile-1"}

	_, cleanup, err := h.resolveVirtualAnchorURIWithRotationV3(context.Background(), session, file)
	if cleanup != nil {
		cleanup()
	}
	if err == nil {
		t.Fatal("an absent pin with no same-release candidate must refuse, not silently recover")
	}
	if !errors.Is(err, virtuallibrary.ErrSessionBoundCandidateAbsent) {
		t.Fatalf("err = %v, want the absent-pin verdict so the alternate walk runs", err)
	}
}

// TestResolveVirtualAnchorRealResolverDoesNotRecoverWithMockAssumptions guards
// the seam the two recovery tests rely on: the outage-relist marker actually
// changes the real resolver's behavior. A forced resolve WITHOUT the marker
// replays the negative-cached empty answer, so the retry would be pointless —
// which is exactly the bug the marker fixes.
func TestResolveVirtualAnchorURIRealResolverMarkerBypassesCachedEmpty(t *testing.T) {
	const neutralURI = "virtual://movie/tt-real-anchor-marker"
	id := anchorRealResolverCandidateID(t, neutralURI)

	provider := anchorRealResolverProvider(t, anchorResolverEmptyJSON, anchorResolverCandidateJSON)
	svc := virtuallibrary.New(virtuallibrary.Config{
		Enabled: true, ManifestURL: provider.URL + "/manifest.json",
		AllowInsecureHTTP: true, AllowPrivateStreams: true,
	}, nil, nil)
	if svc == nil {
		t.Fatal("expected a non-nil virtual library service")
	}
	pinnedURI := neutralURI + "?result=" + id

	// First: a plain forced relist (no marker) is served the cached empty answer.
	// The provider's first stream request was the empty answer; without the
	// marker the floor replays it, so the resolve fails and the pin is never
	// reached.
	if _, err := svc.ResolveDetailed(context.Background(), pinnedURI, true, nil, "", true, false); err == nil {
		t.Fatal("forced relist without the outage marker unexpectedly recovered; the cache-bypass seam is not exercised")
	}
	if _, err := svc.ResolveDetailed(context.Background(), pinnedURI, true, nil, "", true, false); err == nil {
		t.Fatal("forced relist without the outage marker recovered the pin from a cached empty answer")
	}
	// With the marker the resolver bypasses the floor and reaches the pin.
	res, err := svc.ResolveDetailed(virtuallibrary.WithProviderOutageRelist(context.Background()), pinnedURI, true, nil, "", true, false)
	if err != nil {
		t.Fatalf("outage relist: %v", err)
	}
	if res.CandidateID != id {
		t.Fatalf("outage relist candidate = %q, want %q", res.CandidateID, id)
	}
}
