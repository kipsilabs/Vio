package handlers

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// TestPrepareTransportTimelineV3RejectsAnchorFarBeforeTarget pins the measured
// production failure: a seek to 118.757 s whose probe resolved the file's first
// keyframe at 3.667 s. The timeline must not publish that as the stream origin;
// it answers the retryable seek verdict so the start/replan fallback can plan a
// decoded route instead of resetting the playhead to the start.
func TestPrepareTransportTimelineV3RejectsAnchorFarBeforeTarget(t *testing.T) {
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	handler.copySeekAnchor = func(_ context.Context, _ string, _ string, requested float64, _ int) (float64, int, error) {
		if requested != 118.75698324022346 {
			t.Fatalf("probe requested = %v, want 118.75698324022346", requested)
		}
		return 3.667, 1, nil
	}

	plan := copyTimelinePlanV3("plan:far-anchor", playback.DeliveryRemuxProgressiveV3, 118.75698324022346)
	timeline, timelineErr := handler.prepareTransportTimelineV3(
		context.Background(),
		&playback.Session{ID: "far-anchor"},
		&models.MediaFile{ID: 1, FilePath: writePlaybackTestMediaFile(t, "movie.mkv"), Container: "mkv"},
		playback.PlannerResultV3{Plan: plan, PlayMethod: playback.PlayRemux},
	)
	if timelineErr == nil {
		t.Fatalf("timeline = %#v, want the unusable anchor rejected", timeline)
	}
	if !timelineErr.retryable || !timelineErr.seekFailure || timelineErr.reason != transcodeStartFailedReasonV3 {
		t.Fatalf("transport error = %+v, want a retryable transcode_start_failed seek failure", timelineErr)
	}
	if !errors.Is(timelineErr.cause, playback.ErrCopySeekAnchorUnusable) {
		t.Fatalf("cause = %v, want ErrCopySeekAnchorUnusable", timelineErr.cause)
	}
}

// TestPrepareTransportTimelineV3CopyMarkerEndKeepsCopyTimeline is the control:
// the measured Bleach marker-end seek (0-99 intro, skip sends position 99,
// probe resolves 97.89) is accepted and still plans the copy route with a
// ~1.11 s player-start offset. The rejected-anchor bar must not narrow the
// normal case.
func TestPrepareTransportTimelineV3CopyMarkerEndKeepsCopyTimeline(t *testing.T) {
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	handler.copySeekAnchor = func(_ context.Context, _ string, _ string, requested float64, _ int) (float64, int, error) {
		return 97.89, 48, nil
	}

	plan := copyTimelinePlanV3("plan:marker-end", playback.DeliveryRemuxProgressiveV3, 99)
	timeline, timelineErr := handler.prepareTransportTimelineV3(
		context.Background(),
		&playback.Session{ID: "marker-end"},
		&models.MediaFile{ID: 2, FilePath: writePlaybackTestMediaFile(t, "bleach.mkv"), Container: "mkv"},
		playback.PlannerResultV3{Plan: plan, PlayMethod: playback.PlayRemux},
	)
	if timelineErr != nil {
		t.Fatalf("prepareTransportTimelineV3: %v", timelineErr)
	}
	if !timeline.copySeekAnchorResolved || timeline.streamOriginSeconds != 97.89 || timeline.startSegmentNumber != 48 || timeline.seekSeconds != 99 {
		t.Fatalf("timeline = %#v, want the copy anchor 97.89/48 at seek 99", timeline)
	}
	if plan.Delivery != playback.DeliveryRemuxProgressiveV3 {
		t.Fatalf("delivery = %q, want the copy route unchanged", plan.Delivery)
	}
	if plan.Timeline.CanSeekAnywhere {
		t.Fatal("copy timeline claims can_seek_anywhere")
	}
	if got := plan.Timeline.PlayerStartSeconds; got < 1.10 || got > 1.12 {
		t.Fatalf("player_start_seconds = %v, want ~1.11", got)
	}
	if plan.Timeline.StreamOriginSeconds != 97.89 {
		t.Fatalf("stream_origin_seconds = %v, want 97.89", plan.Timeline.StreamOriginSeconds)
	}
}

// TestPrepareTransportTimelineV3RejectedAnchorIsProbedAgain proves the handler
// does not serve a rejected anchor on a second seek: each attempt runs its own
// probe and is rejected, instead of reusing the first answer as a proven
// interval.
func TestPrepareTransportTimelineV3RejectedAnchorIsProbedAgain(t *testing.T) {
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	var calls int
	handler.copySeekAnchor = func(_ context.Context, _ string, _ string, _ float64, _ int) (float64, int, error) {
		calls++
		return 3.667, 1, nil
	}
	file := &models.MediaFile{ID: 3, FilePath: writePlaybackTestMediaFile(t, "reprobe.mkv"), Container: "mkv"}
	session := &playback.Session{ID: "reprobe"}
	for attempt := 0; attempt < 2; attempt++ {
		_, timelineErr := handler.prepareTransportTimelineV3(
			context.Background(), session, file,
			playback.PlannerResultV3{Plan: copyTimelinePlanV3("plan:reprobe", playback.DeliveryRemuxProgressiveV3, 118.75698324022346), PlayMethod: playback.PlayRemux},
		)
		if !errors.Is(timelineErr.cause, playback.ErrCopySeekAnchorUnusable) {
			t.Fatalf("attempt %d cause = %v, want ErrCopySeekAnchorUnusable", attempt, timelineErr.cause)
		}
	}
	if calls != 2 {
		t.Fatalf("probe calls = %d, want 2 (a rejected anchor is never reused)", calls)
	}
}

// TestSeekAnchorUnusableRefusalRetryAvoidsCopy proves the retry context for an
// unusable anchor plans the decoded route instead of granting a longer probe,
// and that the mark survives the retry (a longer probe returns the same wrong
// keyframe every time).
func TestSeekAnchorUnusableRefusalRetryAvoidsCopy(t *testing.T) {
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	const (
		profile = "profile-retry"
		fileID  = 913
		target  = 118.75698324022346
	)
	handler.recordCopySeekAnchorRefusalV3(profile, fileID, target, &transportErrorV3{
		reason:      transcodeStartFailedReasonV3,
		retryable:   true,
		seekFailure: true,
		cause:       fmt.Errorf("probe: %w", playback.ErrCopySeekAnchorUnusable),
	})

	for attempt := 0; attempt < 2; attempt++ {
		ctx, ok := handler.copySeekAnchorRetryContextV3(context.Background(), profile, fileID, target)
		if !ok || !seekAnchorAvoidsVideoCopyV3(ctx) {
			t.Fatalf("attempt %d: retry context ok=%v avoidCopy=%v, want the decoded fallback", attempt, ok, seekAnchorAvoidsVideoCopyV3(ctx))
		}
		if grant := seekAnchorRetryGrantFromContextV3(ctx); grant != nil {
			t.Fatalf("attempt %d granted an extended probe window for an unusable anchor", attempt)
		}
	}

	// A different target is not this refusal's retry.
	if _, ok := handler.copySeekAnchorRetryContextV3(context.Background(), profile, fileID, target+5); ok {
		t.Fatal("a different seek target inherited the unusable-anchor retry")
	}

	// A probe timeout refusal keeps the extended-window behavior.
	handler.recordCopySeekAnchorRefusalV3(profile, fileID, 200, &transportErrorV3{
		reason:      transcodeStartFailedReasonV3,
		retryable:   true,
		seekFailure: true,
		cause:       context.DeadlineExceeded,
	})
	ctx, ok := handler.copySeekAnchorRetryContextV3(context.Background(), profile, fileID, 200)
	if !ok || seekAnchorAvoidsVideoCopyV3(ctx) || seekAnchorRetryGrantFromContextV3(ctx) == nil {
		t.Fatalf("timeout retry ok=%v avoidCopy=%v grant=%v, want an extended probe window", ok, seekAnchorAvoidsVideoCopyV3(ctx), seekAnchorRetryGrantFromContextV3(ctx))
	}
}

// TestCopySeekAnchorUnusableForSeekV3SkipsSourcesWithoutALiveInput pins the cost
// guard: the early probe only runs for a virtual source whose pinned transport
// is already open. A local file never gets a second provider-less probe from
// this path; the transport timeline keeps owning that verdict and its budgets.
func TestCopySeekAnchorUnusableForSeekV3SkipsSourcesWithoutALiveInput(t *testing.T) {
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	handler.copySeekAnchor = func(context.Context, string, string, float64, int) (float64, int, error) {
		t.Fatal("the early seek check probed a source with no live pinned transport")
		return 0, 0, nil
	}
	file := &models.MediaFile{ID: 4, FilePath: writePlaybackTestMediaFile(t, "local.mkv"), Container: "mkv"}
	if handler.copySeekAnchorUnusableForSeekV3(context.Background(), &playback.Session{ID: "local"}, file, 118.757) {
		t.Fatal("a local file was treated as an unusable copy seek")
	}
	virtual := &models.MediaFile{ID: 5, ContentID: "movie", FilePath: "virtual://movie/no-live"}
	if handler.copySeekAnchorUnusableForSeekV3(context.Background(), &playback.Session{ID: "no-live"}, virtual, 118.757) {
		t.Fatal("a virtual file without a live pinned transport was probed early")
	}
}

// TestStartCopySeekAnchorFallbackPlansDecodedRoute drives the measured path end
// to end: a start at a resume position plans the copy route, the anchor probe
// resolves the file's first keyframe, and the start re-plans the same target as
// server_transcode_hls in the same request. The copy probe runs once: an
// unusable anchor is a verdict, not a slow link, so it is not retried.
func TestStartCopySeekAnchorFallbackPlansDecodedRoute(t *testing.T) {
	file := v3HandlerFixtureFile(t)
	file.Container = "mkv"
	file.FilePath = writePlaybackTestMediaFile(t, "movie.mkv")
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0), testPlaybackFileResolver{file: file})
	handler.PlaybackConfig = playbackTestConfig(writePlaybackTestFFmpeg(t), t.TempDir())
	presetLocalRegistryV3(handler, playback.NewTransformationRegistryV3([]playback.TransformationSpecV3{
		{Name: playback.TransformationAudioToAACV3, RecipeVersion: playback.TransformationAudioToAACRecipeVersionV3, Available: true},
		{Name: playback.TransformationVideoToH264V3, RecipeVersion: playback.TransformationVideoToH264RecipeVersionV3, Available: true},
	}))
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"allow_4k_transcode": "true"}}
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	var probes int
	handler.copySeekAnchor = func(_ context.Context, _ string, _ string, requested float64, _ int) (float64, int, error) {
		probes++
		if requested != 118.75698324022346 {
			t.Fatalf("probe requested = %v, want 118.75698324022346", requested)
		}
		return 3.667, 1, nil
	}

	start := v3HandlerStartRequest()
	start.ClientPlaybackContext.Deliveries[playback.DeliveryClassProgressiveV3] = playback.DeliveryCapabilityV3{Enabled: true, SupportedOnDevice: true}
	start.ClientPlaybackContext.Deliveries[playback.DeliveryClassHLSV3] = playback.DeliveryCapabilityV3{Enabled: true, SupportedOnDevice: true}
	target := 118.75698324022346
	start.StartPosition = &target

	response := startV3PlaybackForHandlerTest(t, handler, start)
	if response.Terminal != nil {
		t.Fatalf("terminal = %#v, want the seek to land", response.Terminal)
	}
	if response.PlaybackPlan == nil {
		t.Fatal("no playback plan returned")
	}
	if response.PlaybackPlan.Delivery != playback.DeliveryTranscodeHLSV3 {
		t.Fatalf("delivery = %q, want server_transcode_hls for an unusable copy anchor", response.PlaybackPlan.Delivery)
	}
	if response.PlaybackPlan.Timeline.SourceStartSeconds != target {
		t.Fatalf("source_start_seconds = %v, want the requested %v", response.PlaybackPlan.Timeline.SourceStartSeconds, target)
	}
	if probes != 1 {
		t.Fatalf("copy anchor probes = %d, want 1 (an unusable anchor is not retried)", probes)
	}
}

// TestCopySeekAnchorUnusableForSeekV3UsesTheLivePinnedTransport proves the
// replan trigger probes the running generation's own registration (no provider
// resolve) and distinguishes an unusable anchor from an accepted one.
func TestCopySeekAnchorUnusableForSeekV3UsesTheLivePinnedTransport(t *testing.T) {
	handler, session, file, _, relayURL := seekAnchorLiveRelayFixture(t, "")
	handler.copySeekAnchorLookup = func(string, string, float64, int) (float64, int, bool) { return 0, 0, false }
	var probed []string
	handler.copySeekAnchor = func(_ context.Context, _ string, inputPath string, requested float64, _ int) (float64, int, error) {
		probed = append(probed, inputPath)
		if requested > 100 {
			return 3.667, 1, nil
		}
		return requested - 0.5, 0, nil
	}

	if !handler.copySeekAnchorUnusableForSeekV3(context.Background(), session, file, 118.75698324022346) {
		t.Fatal("a far-before anchor was not reported unusable")
	}
	if len(probed) != 1 || probed[0] != relayURL {
		t.Fatalf("probed inputs = %v, want the live relay URL %q", probed, relayURL)
	}
	if handler.copySeekAnchorUnusableForSeekV3(context.Background(), session, file, 40) {
		t.Fatal("an in-pre-roll anchor was reported unusable")
	}
}

// copyTimelinePlanV3 is a remux plan at position with the delivery under test.
func copyTimelinePlanV3(planID string, delivery playback.DeliveryV3, position float64) *playback.PlanV3 {
	return &playback.PlanV3{
		PlanID:   planID,
		Delivery: delivery,
		Timeline: playback.TimelineV3{SourceStartSeconds: position, PlayerStartSeconds: position},
	}
}
