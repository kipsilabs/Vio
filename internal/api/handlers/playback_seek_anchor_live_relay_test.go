package handlers

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/remotestream"
)

// seekAnchorLiveRelayFixture builds a copy-remux seek whose session is bound to
// the candidate being probed and whose running transport already reads it
// through a live relay registration.
func seekAnchorLiveRelayFixture(t *testing.T, sessionURI string) (*PlaybackHandler, *playback.Session, *models.MediaFile, *playback.PlanV3, string) {
	t.Helper()
	const candidate = "virtual://movie/tt-seek-live-relay?result=pinned"
	if sessionURI == "" {
		sessionURI = candidate
	}
	relay := remotestream.NewRelay()
	t.Cleanup(func() { _ = relay.Close(context.Background()) })
	relayURL, cleanup, err := relay.RegisterInsecure(context.Background(), "http://127.0.0.1:9/pinned.mkv")
	if err != nil {
		t.Fatalf("register relay: %v", err)
	}
	t.Cleanup(cleanup)

	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	handler.RemoteStreamRelay = relay
	handler.AllowPrivateStreams = func(int) bool { return true }

	session := &playback.Session{ID: "session-seek-live-relay", UserID: 1, ProfileID: "profile-1", VirtualSourceURI: sessionURI}
	live, err := playback.NewReadyTranscodeSessionForTesting(t.TempDir(), playback.TranscodeOpts{
		InputPath: relayURL, SessionID: session.ID, TargetCodecVideo: "copy", TargetCodecAudio: "copy",
	})
	if err != nil {
		t.Fatalf("ready transcode session: %v", err)
	}
	handler.TranscodeManager().RegisterTranscodeSession(session.ID, live)
	t.Cleanup(func() { handler.TranscodeManager().CloseTranscodeSession(session.ID, "") })

	file := &models.MediaFile{ID: 42, FilePath: candidate, VirtualOwnerInstallationID: 5}
	plan := &playback.PlanV3{
		PlanID:   "plan:seek-live-relay",
		Delivery: playback.DeliveryRemuxHLSV3,
		Timeline: playback.TimelineV3{SourceStartSeconds: 120},
	}
	return handler, session, file, plan, relayURL
}

// TestLivePinnedAnchorInputReusesMatchingLiveRegistration pins the admission
// rules of the live-registration reuse: only a session bound to the probed
// candidate, whose transport still runs and whose relay registration is live,
// hands back an input. Every other case falls back to the provider resolve.
func TestLivePinnedAnchorInputReusesMatchingLiveRegistration(t *testing.T) {
	t.Run("matching live binding", func(t *testing.T) {
		handler, session, file, _, relayURL := seekAnchorLiveRelayFixture(t, "")
		input, ok := handler.livePinnedAnchorInputV3(session, file)
		if !ok || input != relayURL {
			t.Fatalf("live input = %q, %v; want the running relay URL %q", input, ok, relayURL)
		}
	})
	t.Run("moved session binding is not reused", func(t *testing.T) {
		handler, session, file, _, _ := seekAnchorLiveRelayFixture(t, "virtual://movie/tt-seek-live-relay?result=moved")
		if input, ok := handler.livePinnedAnchorInputV3(session, file); ok {
			t.Fatalf("moved binding reused input %q", input)
		}
	})
	t.Run("non-virtual file is never reused", func(t *testing.T) {
		handler, session, _, _, _ := seekAnchorLiveRelayFixture(t, "")
		if input, ok := handler.livePinnedAnchorInputV3(session, &models.MediaFile{ID: 9, FilePath: "/media/movie.mkv"}); ok {
			t.Fatalf("non-virtual file reused input %q", input)
		}
	})
	t.Run("absent registration is not reused", func(t *testing.T) {
		handler, session, file, _, _ := seekAnchorLiveRelayFixture(t, "")
		if err := handler.RemoteStreamRelay.Close(context.Background()); err != nil {
			t.Fatalf("close relay: %v", err)
		}
		if input, ok := handler.livePinnedAnchorInputV3(session, file); ok {
			t.Fatalf("closed relay reused input %q", input)
		}
	})
}

// TestSeekAnchorReusesLivePinnedRelayWithoutProviderResolve proves a seek
// reanchor that rebuilds a copy transport no longer pays a provider resolve
// for the same candidate its running generation already reads. The provider
// resolver is counted, so the assertion is deterministic; production measures
// one listing at 380-902 ms, which is the per-seek work this removes.
func TestSeekAnchorReusesLivePinnedRelayWithoutProviderResolve(t *testing.T) {
	handler, session, file, plan, relayURL := seekAnchorLiveRelayFixture(t, "")

	var resolves atomic.Int32
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
		resolves.Add(1)
		return ResolvedVirtualMedia{URL: "http://127.0.0.1:9/re-resolved.mkv", URI: file.FilePath, CandidateID: "pinned"}, nil
	})
	var probes atomic.Int32
	probedInput := ""
	handler.copySeekAnchor = func(_ context.Context, _ string, inputPath string, requested float64, segmentDuration int) (float64, int, error) {
		probes.Add(1)
		probedInput = inputPath
		return requested, computeStartSegment(requested, segmentDuration), nil
	}

	timeline, transportErr := handler.prepareTransportTimelineV3(
		context.Background(), session, file, playback.PlannerResultV3{Plan: plan, PlayMethod: playback.PlayRemux},
	)
	if transportErr != nil {
		t.Fatalf("prepare timeline: %#v", transportErr)
	}
	if got := resolves.Load(); got != 0 {
		t.Fatalf("provider resolves = %d, want 0: the live registration already holds the candidate", got)
	}
	if got := probes.Load(); got != 1 {
		t.Fatalf("anchor probes = %d, want 1", got)
	}
	if probedInput != relayURL {
		t.Fatalf("probed input = %q, want the live relay URL %q", probedInput, relayURL)
	}
	if timeline.copySeekAnchorResolved != true {
		t.Fatalf("timeline = %#v, want a resolved copy anchor", timeline)
	}
}

// TestSeekAnchorMovedBindingStillResolvesProvider keeps the fallback honest: a
// session whose pin no longer names the probed candidate must resolve the
// provider instead of probing a registration that belongs to different bytes.
func TestSeekAnchorMovedBindingStillResolvesProvider(t *testing.T) {
	handler, session, file, plan, relayURL := seekAnchorLiveRelayFixture(t, "virtual://movie/tt-seek-live-relay?result=moved")

	var resolves atomic.Int32
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
		resolves.Add(1)
		return ResolvedVirtualMedia{URL: "http://127.0.0.1:9/re-resolved.mkv", URI: file.FilePath, CandidateID: "pinned"}, nil
	})
	var probes atomic.Int32
	probedInput := ""
	handler.copySeekAnchor = func(_ context.Context, _ string, inputPath string, requested float64, segmentDuration int) (float64, int, error) {
		probes.Add(1)
		probedInput = inputPath
		return requested, computeStartSegment(requested, segmentDuration), nil
	}

	if _, transportErr := handler.prepareTransportTimelineV3(
		context.Background(), session, file, playback.PlannerResultV3{Plan: plan, PlayMethod: playback.PlayRemux},
	); transportErr != nil {
		t.Fatalf("prepare timeline: %#v", transportErr)
	}
	if got := resolves.Load(); got != 1 {
		t.Fatalf("provider resolves = %d, want 1 for a moved binding", got)
	}
	if got := probes.Load(); got != 1 {
		t.Fatalf("anchor probes = %d, want 1", got)
	}
	if probedInput == relayURL {
		t.Fatalf("probed the stale live registration %q; a moved binding must resolve fresh bytes", probedInput)
	}
}
