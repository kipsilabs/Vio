package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/remotestream"
)

// TestPrepareTransportTimelineV3LazyAnchorHitSkipsRelayReopen proves a proven
// anchor-cache hit returns the remux timeline without resolving the virtual
// candidate to a pinned relay URL and without probing. Both the resolver and the
// probe fail the test if reached, so a hit that still reopened the relay or
// probed cannot pass.
func TestPrepareTransportTimelineV3LazyAnchorHitSkipsRelayReopen(t *testing.T) {
	const pinnedURI = "virtual://movie/lazy-hit?result=pinned"
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	handler.AllowPrivateStreams = func(int) bool { return true }
	handler.RemoteStreamRelay = remotestream.NewRelay()
	defer func() { _ = handler.RemoteStreamRelay.Close(context.Background()) }()

	var resolverCalls, probeCalls int
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
		resolverCalls++
		return ResolvedVirtualMedia{}, errors.New("a proven cache hit must not reopen the relay")
	})
	handler.copySeekAnchorLookup = func(_ string, sourceIdentity string, requested float64, segmentDuration int) (float64, int, bool) {
		if sourceIdentity != pinnedURI || requested != 321.25 || segmentDuration != playback.DefaultSegmentDuration {
			t.Fatalf("lookup key = %q/%v/%d, want %q/321.25/%d", sourceIdentity, requested, segmentDuration, pinnedURI, playback.DefaultSegmentDuration)
		}
		return 320, 160, true
	}
	handler.copySeekAnchor = func(context.Context, string, string, float64, int) (float64, int, error) {
		probeCalls++
		return 0, 0, errors.New("a proven cache hit must not probe")
	}

	file := &models.MediaFile{
		ID: 7, ContentID: "movie-lazy-hit", FilePath: pinnedURI,
		VirtualOwnerInstallationID: 5, ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024",
	}
	session := &playback.Session{ID: "lazy-hit", UserID: 1, ProfileID: "profile-1"}
	plan := &playback.PlanV3{
		PlanID:   "plan:lazy-hit",
		Delivery: playback.DeliveryRemuxProgressiveV3,
		Timeline: playback.TimelineV3{SourceStartSeconds: 321.25, PlayerStartSeconds: 321.25},
	}

	timeline, timelineErr := handler.prepareTransportTimelineV3(context.Background(), session, file, playback.PlannerResultV3{Plan: plan, PlayMethod: playback.PlayRemux})
	if timelineErr != nil {
		t.Fatalf("prepareTransportTimelineV3: %v", timelineErr)
	}
	if !timeline.copySeekAnchorResolved || timeline.streamOriginSeconds != 320 || timeline.startSegmentNumber != 160 || timeline.seekSeconds != 321.25 {
		t.Fatalf("timeline = %#v, want the cached anchor 320/160 resolved at seek 321.25", timeline)
	}
	if resolverCalls != 0 {
		t.Fatalf("resolver calls = %d, want 0 (the eager relay reopen must be skipped)", resolverCalls)
	}
	if probeCalls != 0 {
		t.Fatalf("probe calls = %d, want 0 (a proven cache hit must not probe)", probeCalls)
	}
}

// TestPrepareTransportTimelineV3LazyAnchorHitStartsNoProviderWork proves a
// proven cache hit is answered entirely from the local cache: a start whose
// request is already canceled still resolves its timeline instead of beginning a
// relay reopen that cancellation would immediately supersede.
func TestPrepareTransportTimelineV3LazyAnchorHitStartsNoProviderWork(t *testing.T) {
	const pinnedURI = "virtual://movie/lazy-canceled?result=pinned"
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	handler.AllowPrivateStreams = func(int) bool { return true }
	handler.RemoteStreamRelay = remotestream.NewRelay()
	defer func() { _ = handler.RemoteStreamRelay.Close(context.Background()) }()

	var resolverCalls int
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
		resolverCalls++
		return ResolvedVirtualMedia{}, errors.New("a cache hit must not reopen the relay")
	})
	handler.copySeekAnchorLookup = func(string, string, float64, int) (float64, int, bool) {
		return 200, 100, true
	}
	handler.copySeekAnchor = func(context.Context, string, string, float64, int) (float64, int, error) {
		t.Fatal("a cache hit must not probe")
		return 0, 0, nil
	}

	file := &models.MediaFile{ID: 9, ContentID: "movie-lazy-canceled", FilePath: pinnedURI}
	session := &playback.Session{ID: "lazy-canceled", UserID: 1, ProfileID: "profile-1"}
	plan := &playback.PlanV3{
		PlanID:   "plan:lazy-canceled",
		Delivery: playback.DeliveryRemuxProgressiveV3,
		Timeline: playback.TimelineV3{SourceStartSeconds: 204, PlayerStartSeconds: 204},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	timeline, timelineErr := handler.prepareTransportTimelineV3(ctx, session, file, playback.PlannerResultV3{Plan: plan, PlayMethod: playback.PlayRemux})
	if timelineErr != nil {
		t.Fatalf("prepareTransportTimelineV3: %v", timelineErr)
	}
	if !timeline.copySeekAnchorResolved || timeline.streamOriginSeconds != 200 || timeline.startSegmentNumber != 100 {
		t.Fatalf("timeline = %#v, want the cached anchor 200/100 resolved without provider work", timeline)
	}
	if resolverCalls != 0 {
		t.Fatalf("resolver calls = %d, want 0 (a cache hit starts no provider work)", resolverCalls)
	}
}

// TestPrepareTransportTimelineV3LazyAnchorMissReopensAndProbes proves a cache
// miss still resolves the virtual candidate to a pinned relay URL and probes, so
// the lazy lookup removes work only on a proven hit.
func TestPrepareTransportTimelineV3LazyAnchorMissReopensAndProbes(t *testing.T) {
	const (
		neutralURI = "virtual://movie/lazy-miss"
		pinnedURI  = neutralURI + "?result=pinned"
	)
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	handler.AllowPrivateStreams = func(int) bool { return true }
	handler.RemoteStreamRelay = remotestream.NewRelay()
	defer func() { _ = handler.RemoteStreamRelay.Close(context.Background()) }()

	var resolverCalls, probeCalls int
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
		resolverCalls++
		return ResolvedVirtualMedia{
			URL: "http://127.0.0.1:9/pinned.mp4", URI: pinnedURI, CandidateID: "pinned",
			ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024",
		}, nil
	})
	handler.copySeekAnchorLookup = func(string, string, float64, int) (float64, int, bool) {
		return 0, 0, false
	}
	var probedInput string
	handler.copySeekAnchor = func(_ context.Context, _ string, inputPath string, requested float64, _ int) (float64, int, error) {
		probeCalls++
		probedInput = inputPath
		if requested != 300 {
			t.Fatalf("probe requested = %v, want 300", requested)
		}
		return 299, 149, nil
	}

	file := &models.MediaFile{
		ID: 8, ContentID: "movie-lazy-miss", FilePath: pinnedURI,
		VirtualOwnerInstallationID: 5, ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024",
	}
	session := &playback.Session{ID: "lazy-miss", UserID: 1, ProfileID: "profile-1"}
	plan := &playback.PlanV3{
		PlanID:   "plan:lazy-miss",
		Delivery: playback.DeliveryRemuxProgressiveV3,
		Timeline: playback.TimelineV3{SourceStartSeconds: 300, PlayerStartSeconds: 300},
	}

	timeline, timelineErr := handler.prepareTransportTimelineV3(context.Background(), session, file, playback.PlannerResultV3{Plan: plan, PlayMethod: playback.PlayRemux})
	if timelineErr != nil {
		t.Fatalf("prepareTransportTimelineV3: %v", timelineErr)
	}
	if !timeline.copySeekAnchorResolved || timeline.streamOriginSeconds != 299 || timeline.startSegmentNumber != 149 {
		t.Fatalf("timeline = %#v, want the probe anchor 299/149", timeline)
	}
	if resolverCalls != 1 {
		t.Fatalf("resolver calls = %d, want 1 (a miss must reopen the relay)", resolverCalls)
	}
	if probeCalls != 1 {
		t.Fatalf("probe calls = %d, want 1 (a miss must probe)", probeCalls)
	}
	if probedInput == "" || probedInput == file.FilePath {
		t.Fatalf("probe input = %q, want the reopened relay URL, not the virtual URI", probedInput)
	}
}
