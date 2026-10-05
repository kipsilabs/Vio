package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/remotestream"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

// TestResolveVirtualAnchorURIReResolvesEmptyProviderListingWithoutIndicting
// proves the transport anchor resolve retries an empty provider listing, not
// only an absent pin, and that it does so WITHOUT indicting the bound release:
// an empty listing is a transient blackout, so the retry re-lists fresh, asks
// for the bound pin again, and excludes nothing. Excluding the pin on an outage
// would skip the healthy candidate the moment the provider recovers with the
// same result id.
func TestResolveVirtualAnchorURIReResolvesEmptyProviderListingWithoutIndicting(t *testing.T) {
	const (
		neutralURI = "virtual://movie/tt-anchor-empty"
		pinnedURI  = neutralURI + "?result=pinned"
	)
	file := &models.MediaFile{
		ID: 12, ContentID: "movie-anchor-empty", FilePath: pinnedURI,
		VirtualOwnerInstallationID: 5, ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024",
	}
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	var rotates, refreshes, outageRelists []bool
	var excluded [][]string
	calls := 0
	h.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(ctx context.Context, _ string, _ int, _ int, _ string, forceRefresh bool, excludedCandidateIDs []string, _ string) (ResolvedVirtualMedia, error) {
		calls++
		rotates = append(rotates, VirtualCandidateRotationAllowed(ctx))
		refreshes = append(refreshes, forceRefresh)
		outageRelists = append(outageRelists, virtuallibrary.ProviderOutageRelistFromContext(ctx))
		excluded = append(excluded, append([]string(nil), excludedCandidateIDs...))
		if calls == 1 {
			// The provider briefly answers empty: a transient listing outage,
			// not a verdict about the release.
			return ResolvedVirtualMedia{}, providerEmptyListing()
		}
		// The provider recovered and lists the bound pin again under the same id.
		return ResolvedVirtualMedia{
			URL: "http://127.0.0.1:9/pinned", URI: pinnedURI, CandidateID: "pinned",
			ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024",
		}, nil
	})
	session := &playback.Session{ID: "anchor-empty", UserID: 1, ProfileID: "profile-1"}

	resolved, cleanup, err := h.resolveVirtualAnchorURIWithRotationV3(context.Background(), session, file)
	if err != nil {
		t.Fatalf("resolveVirtualAnchorURIWithRotationV3: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if got := virtualResultCandidateID(resolved.URI); got != "pinned" {
		t.Fatalf("resolved anchor = %q, want the recovered bound pin", got)
	}
	if calls != 2 {
		t.Fatalf("resolver calls = %d, want exactly 2 (session-bound empty then outage retry)", calls)
	}
	if len(rotates) != 2 || rotates[0] || rotates[1] {
		t.Fatalf("rotation intents = %v, want [false false]: an outage retry must not declare rotation", rotates)
	}
	if len(refreshes) != 2 || !refreshes[1] {
		t.Fatalf("relist intents = %v, want the retry to force a fresh listing", refreshes)
	}
	if len(outageRelists) != 2 || outageRelists[0] || !outageRelists[1] {
		t.Fatalf("outage-relist markers = %v, want the retry marked so the cached empty answer is bypassed", outageRelists)
	}
	if len(excluded) != 2 || len(excluded[0]) != 0 || len(excluded[1]) != 0 {
		t.Fatalf("retry exclusions = %v, want none: an outage must not exclude the bound pin", excluded)
	}
}

// TestResolveVirtualAnchorURIExcludesPinOnlyOnReleaseVerdict proves the split
// between a transcript verdict and a transient outage: an absent session pin
// still excludes the dead pin and declares rotation (so a renumbered
// same-release candidate can be re-identified), while an empty listing never
// excludes it.
func TestResolveVirtualAnchorURIExcludesPinOnlyOnReleaseVerdict(t *testing.T) {
	const (
		neutralURI = "virtual://movie/tt-anchor-verdict"
		pinnedURI  = neutralURI + "?result=pinned"
		siblingURI = neutralURI + "?result=sibling"
	)
	file := &models.MediaFile{
		ID: 17, ContentID: "movie-anchor-verdict", FilePath: pinnedURI,
		VirtualOwnerInstallationID: 5, ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024",
	}
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	var rotates []bool
	var excluded [][]string
	h.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(ctx context.Context, _ string, _ int, _ int, _ string, _ bool, excludedCandidateIDs []string, _ string) (ResolvedVirtualMedia, error) {
		rotates = append(rotates, VirtualCandidateRotationAllowed(ctx))
		excluded = append(excluded, append([]string(nil), excludedCandidateIDs...))
		if !VirtualCandidateRotationAllowed(ctx) {
			return ResolvedVirtualMedia{}, absentSessionPinError("pinned")
		}
		return ResolvedVirtualMedia{
			URL: "http://127.0.0.1:9/sibling", URI: siblingURI, CandidateID: "sibling",
			IdentityRematched: true, ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024",
		}, nil
	})
	session := &playback.Session{ID: "anchor-verdict", UserID: 1, ProfileID: "profile-1"}

	resolved, cleanup, err := h.resolveVirtualAnchorURIWithRotationV3(context.Background(), session, file)
	if err != nil {
		t.Fatalf("resolveVirtualAnchorURIWithRotationV3: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if got := virtualResultCandidateID(resolved.URI); got != "sibling" {
		t.Fatalf("resolved anchor = %q, want the rotated same-release sibling", got)
	}
	if len(rotates) != 2 || rotates[0] || !rotates[1] {
		t.Fatalf("rotation intents = %v, want exactly [false true] for a release verdict", rotates)
	}
	if !containsStringExactV3(excluded[1], "pinned") {
		t.Fatalf("retry exclusions = %v, want the absent pin excluded", excluded[1])
	}
}

// TestPrepareTransportTimelineReResolvesEmptyProviderListing proves the remux
// seek anchor recovers end-to-end from a transient empty listing: the retry
// re-lists fresh and re-serves the bound pin once the provider lists it again,
// instead of returning the "Failed to resolve remux seek position." terminal.
func TestPrepareTransportTimelineReResolvesEmptyProviderListing(t *testing.T) {
	const (
		neutralURI = "virtual://movie/tt-timeline-empty"
		pinnedURI  = neutralURI + "?result=pinned"
	)
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	handler.AllowPrivateStreams = func(int) bool { return true }
	handler.RemoteStreamRelay = remotestream.NewRelay()
	defer func() { _ = handler.RemoteStreamRelay.Close(context.Background()) }()
	var calls int
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(ctx context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		calls++
		if calls == 1 {
			return ResolvedVirtualMedia{}, providerEmptyListing()
		}
		return ResolvedVirtualMedia{
			URL: "http://127.0.0.1:9/pinned.mp4", URI: pinnedURI, CandidateID: "pinned",
			ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024",
		}, nil
	})
	probed := false
	handler.copySeekAnchor = func(_ context.Context, _ string, inputPath string, requested float64, _ int) (float64, int, error) {
		probed = true
		return requested - 0.5, 0, nil
	}
	file := &models.MediaFile{
		ID: 13, ContentID: "movie-timeline-empty", FilePath: pinnedURI,
		VirtualOwnerInstallationID: 5, ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024",
	}
	session := &playback.Session{ID: "timeline-empty", UserID: 1, ProfileID: "profile-1"}
	plan := &playback.PlanV3{
		PlanID:   "plan:timeline-empty",
		Delivery: playback.DeliveryRemuxProgressiveV3,
		Timeline: playback.TimelineV3{SourceStartSeconds: 30, PlayerStartSeconds: 30},
	}

	timeline, timelineErr := handler.prepareTransportTimelineV3(context.Background(), session, file, playback.PlannerResultV3{Plan: plan, PlayMethod: playback.PlayRemux})
	if timelineErr != nil {
		t.Fatalf("prepareTransportTimelineV3: %v", timelineErr)
	}
	if !probed || !timeline.copySeekAnchorResolved {
		t.Fatalf("timeline = %#v probed=%v, want a resolved copy anchor", timeline, probed)
	}
	if calls != 2 {
		t.Fatalf("resolver calls = %d, want exactly 2 (session-bound empty then outage retry)", calls)
	}
}

// TestPrepareTransportTimelineEmptyListingIsProviderUnavailable proves an empty
// provider listing at the remux seek anchor classifies as the retryable
// provider_unavailable rather than transcode_start_failed, and does so for a
// row with NO durable provider identity — the production case, where the
// evidence-gated outage retry cannot cover the row, so the anchor must
// classify unconditionally.
func TestPrepareTransportTimelineEmptyListingIsProviderUnavailable(t *testing.T) {
	const pinnedURI = "virtual://series/tt-anchor-identityless?result=pinned"
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	handler.AllowPrivateStreams = func(int) bool { return true }
	handler.RemoteStreamRelay = remotestream.NewRelay()
	defer func() { _ = handler.RemoteStreamRelay.Close(context.Background()) }()
	var calls int
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
		calls++
		return ResolvedVirtualMedia{}, providerEmptyListing()
	})
	// No ProviderVideoHash/GUID/ReleaseName: the row carries no durable identity.
	file := &models.MediaFile{ID: 14, ContentID: "series-tt-anchor-identityless", FilePath: pinnedURI, VirtualOwnerInstallationID: 5}
	session := &playback.Session{ID: "timeline-empty-identityless", UserID: 1, ProfileID: "profile-1"}
	plan := &playback.PlanV3{
		PlanID:   "plan:timeline-empty-identityless",
		Delivery: playback.DeliveryRemuxProgressiveV3,
		Timeline: playback.TimelineV3{SourceStartSeconds: 30, PlayerStartSeconds: 30},
	}

	_, transportErr := handler.prepareTransportTimelineV3(context.Background(), session, file, playback.PlannerResultV3{Plan: plan, PlayMethod: playback.PlayRemux})
	if transportErr == nil {
		t.Fatal("empty provider listing produced a timeline, want the retryable provider_unavailable")
	}
	if transportErr.reason != providerUnavailableReasonV3 || !transportErr.retryable {
		t.Fatalf("transport error = %#v, want retryable %s", transportErr, providerUnavailableReasonV3)
	}
	if !errors.Is(transportErr.cause, errVirtualProviderUnavailable) {
		t.Fatalf("transport cause = %v, want the provider_unavailable classification", transportErr.cause)
	}
	// The anchor still gets one bounded outage retry before it classifies.
	if calls != 2 {
		t.Fatalf("resolver calls = %d, want exactly 2 (initial empty + one outage retry)", calls)
	}
}

// TestClassifyVirtualAnchorProviderOutageScopesToTransientOutages pins the
// anchor classifier: a transient provider outage (empty listing, 5xx, trusted
// pin absent from an empty answer) classifies as provider_unavailable for any
// row, while a genuine release verdict (an absent session pin or a marked-failed
// candidate) is left unchanged so the start's alternate walk still runs.
func TestClassifyVirtualAnchorProviderOutageScopesToTransientOutages(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"empty listing", providerEmptyListing(), true},
		{"provider 5xx", provider502(), true},
		{"trusted persisted absent", fmt.Errorf("no longer listed: %w", virtuallibrary.ErrPersistedCandidateTrusted), true},
		{"absent session pin", absentSessionPinError("pinned"), false},
		{"marked failed", fmt.Errorf("candidate %s is marked failed: %w", "pinned", ErrVirtualCandidateMarkedFailed), false},
		{"other", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := errors.Is(classifyVirtualAnchorProviderOutage(tc.err), errVirtualProviderUnavailable)
			if got != tc.want {
				t.Fatalf("classifyVirtualAnchorProviderOutage(%v) provider_unavailable = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestHandleStartPlaybackV3AbsentPinReachesAlternateWalkAfterAnchorOutage maps
// the whole start path for a release verdict at the remux anchor. The primary
// start resolve succeeds on the P0 fast path (complete evidence + a probe
// stamp), builds a remux-progressive plan, and then the anchor resolve — the
// first live provider listing for that plan — refuses with an absent session
// pin. That is a verdict about the release, not a transient outage: it must not
// be reclassified as retryable provider_unavailable, or the transport-failure
// alternate walk would be suppressed and the viewer left stuck on the dead
// release. The test proves the start reaches the walk and lands on the
// alternate version. The transient-outage counterpart is covered by
// TestPrepareTransportTimelineEmptyListingIsProviderUnavailable: that one is
// classified and does NOT reach this walk.
func TestHandleStartPlaybackV3AbsentPinReachesAlternateWalkAfterAnchorOutage(t *testing.T) {
	const (
		neutral   = "virtual://movie/tt-anchor-to-alternate"
		pinnedURI = neutral + "?result=pinned"
		altURI    = neutral + "?result=alt"
	)
	stamp := time.Now()
	source := v3HandlerFixtureFile(t)
	source.ID = 800
	source.ContentID = "movie-anchor-to-alternate"
	source.Container = "mkv"
	source.FilePath = pinnedURI
	source.VirtualOwnerInstallationID = 5
	// A recent probe stamp plus the fixture's complete video/audio/container
	// evidence take the P0 fast path at start, so the start resolve never
	// consults the provider; the first live provider call is the anchor.
	source.ProbeUpdatedAt = &stamp

	alternateValue := *source
	alternate := &alternateValue
	alternate.ID = 801
	alternate.FilePath = altURI
	alternate.ProbeUpdatedAt = &stamp

	manager := playback.NewSessionManager(0, 0)
	handler := NewPlaybackHandler(manager, mapPlaybackFileResolver{files: map[int]*models.MediaFile{source.ID: source, alternate.ID: alternate}})
	handler.FileVersionFetcher = testPlaybackFileVersionFetcher{byContent: map[string][]*models.MediaFile{
		source.ContentID: {source, alternate},
	}}
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"allow_4k_transcode": "true"}}
	handler.PlaybackConfig = playbackTestConfig("", "")
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	relay := remotestream.NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()
	handler.RemoteStreamRelay = relay
	handler.AllowPrivateStreams = func(int) bool { return true }
	handler.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
		return "http://127.0.0.1:9/stream?path=" + path, nil
	})
	var pinnedResolves atomic.Int32
	var resolvedURIs []string
	var resolvedMu sync.Mutex
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		resolvedMu.Lock()
		resolvedURIs = append(resolvedURIs, uri)
		resolvedMu.Unlock()
		if strings.Contains(uri, "pinned") {
			// Every resolve of the bound pin — the session-bound anchor and its
			// rotation retry — refuses with the absent-pin verdict. It must stay
			// a verdict so the start's transport-failure alternate walk runs.
			pinnedResolves.Add(1)
			return ResolvedVirtualMedia{}, absentSessionPinError("pinned")
		}
		return ResolvedVirtualMedia{URL: "http://127.0.0.1:9/alt.mkv", URI: altURI, CandidateID: "alt"}, nil
	})
	handler.copySeekAnchor = func(_ context.Context, _ string, _ string, requested float64, _ int) (float64, int, error) {
		return requested, 0, nil
	}

	start := v3HandlerStartRequest()
	start.FileID = source.ID
	start.QualityPreference = "auto"
	startPosition := 30.0
	start.StartPosition = &startPosition
	start.ClientPlaybackContext.Deliveries = map[string]playback.DeliveryCapabilityV3{
		playback.DeliveryClassProgressiveV3: {Enabled: true, SupportedOnDevice: true},
	}

	rr := httptest.NewRecorder()
	handler.HandleStartPlayback(rr, httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", strings.NewReader(marshalV3StartRequest(t, start))).WithContext(newAuthorizedPlaybackContext()))
	if rr.Code != http.StatusCreated {
		t.Fatalf("start status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var response playback.DecisionResponseV3
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("start response invalid: %v", err)
	}
	// The bound pin must actually have been resolved at the anchor, or the test
	// is not exercising the transport-error mapping.
	if pinnedResolves.Load() == 0 {
		t.Fatal("the anchor never resolved the bound pin; the start never reached transport preparation")
	}
	if response.Terminal != nil {
		t.Fatalf("absent-pin anchor terminaled the start (%+v); it must reach the alternate walk", response.Terminal)
	}
	if response.PlaybackPlan == nil {
		t.Fatalf("absent-pin anchor produced no plan: %#v", response)
	}
	if response.PlaybackPlan.EffectiveMediaFileID != alternate.ID {
		t.Fatalf("effective file = %d, want the alternate version %d after the absent-pin anchor verdict",
			response.PlaybackPlan.EffectiveMediaFileID, alternate.ID)
	}
	resolvedMu.Lock()
	seenAlt := containsStringExactV3(resolvedURIs, altURI)
	uris := append([]string(nil), resolvedURIs...)
	resolvedMu.Unlock()
	if !seenAlt {
		t.Fatalf("resolved URIs = %v, want the alternate %q resolved by the transport-failure walk", uris, altURI)
	}
}
