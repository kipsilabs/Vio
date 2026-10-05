package handlers

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/remotestream"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

// TestResolveVirtualAnchorURIRotatesEmptyProviderListing proves the transport
// anchor resolve retries an empty provider listing, not only an absent pin: the
// remux-seek anchor is the first live provider resolve for a plan bound by the
// probe-evidence P0 fast path (no liveness check by design), so a provider that
// briefly answers empty must get the same forced relist the plan-time walk
// gets instead of terminaling the start at the first resolve.
func TestResolveVirtualAnchorURIRotatesEmptyProviderListing(t *testing.T) {
	const (
		neutralURI = "virtual://movie/tt-anchor-empty"
		pinnedURI  = neutralURI + "?result=pinned"
		siblingURI = neutralURI + "?result=sibling"
	)
	file := &models.MediaFile{
		ID: 12, ContentID: "movie-anchor-empty", FilePath: pinnedURI,
		VirtualOwnerInstallationID: 5, ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024",
	}
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	var rotates, refreshes []bool
	h.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(ctx context.Context, _ string, _ int, _ int, _ string, forceRefresh bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		rotates = append(rotates, VirtualCandidateRotationAllowed(ctx))
		refreshes = append(refreshes, forceRefresh)
		if !VirtualCandidateRotationAllowed(ctx) {
			return ResolvedVirtualMedia{}, providerEmptyListing()
		}
		return ResolvedVirtualMedia{
			URL: "http://127.0.0.1:9/sibling", URI: siblingURI, CandidateID: "sibling",
			IdentityRematched: true, ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024",
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
	if got := virtualResultCandidateID(resolved.URI); got != "sibling" {
		t.Fatalf("resolved anchor = %q, want the rotated same-release sibling", got)
	}
	if len(rotates) != 2 || rotates[0] || !rotates[1] {
		t.Fatalf("rotation intents = %v, want exactly [false true]", rotates)
	}
	if len(refreshes) != 2 || !refreshes[1] {
		t.Fatalf("relist intents = %v, want the retry to force a fresh listing", refreshes)
	}
}

// TestPrepareTransportTimelineRotatesEmptyProviderListing proves the remux seek
// anchor recovers end-to-end: an empty listing on the session-bound resolve
// triggers the rotation retry and the same-release sibling anchors the
// already-built plan instead of returning the "Failed to resolve remux seek
// position." terminal.
func TestPrepareTransportTimelineRotatesEmptyProviderListing(t *testing.T) {
	const (
		neutralURI = "virtual://movie/tt-timeline-empty"
		pinnedURI  = neutralURI + "?result=pinned"
		siblingURI = neutralURI + "?result=sibling"
	)
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	handler.AllowPrivateStreams = func(int) bool { return true }
	handler.RemoteStreamRelay = remotestream.NewRelay()
	defer func() { _ = handler.RemoteStreamRelay.Close(context.Background()) }()
	var calls int
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(ctx context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		calls++
		if !VirtualCandidateRotationAllowed(ctx) {
			return ResolvedVirtualMedia{}, providerEmptyListing()
		}
		return ResolvedVirtualMedia{
			URL: "http://127.0.0.1:9/sibling.mp4", URI: siblingURI, CandidateID: "sibling",
			IdentityRematched: true, ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024",
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
		t.Fatalf("resolver calls = %d, want exactly 2 (session-bound empty then rotation)", calls)
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
	// The anchor still gets the bounded rotation retry before it classifies.
	if calls != 2 {
		t.Fatalf("resolver calls = %d, want exactly 2 (initial empty + one rotation retry)", calls)
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
