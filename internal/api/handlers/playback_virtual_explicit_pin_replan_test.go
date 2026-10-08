package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// replanAlternateFileFetcher records how many times the alternate-file hunt
// consulted the version fetcher. It is the observable seam that proves whether
// the rehydration-failure hunt ran at all.
type replanAlternateFileFetcher struct {
	inner testPlaybackFileVersionFetcher
	calls atomic.Int32
}

func (f *replanAlternateFileFetcher) GetByContentID(ctx context.Context, id string) ([]*models.MediaFile, error) {
	f.calls.Add(1)
	return f.inner.GetByContentID(ctx, id)
}

func (f *replanAlternateFileFetcher) GetByEpisodeID(ctx context.Context, id string) ([]*models.MediaFile, error) {
	f.calls.Add(1)
	return f.inner.GetByEpisodeID(ctx, id)
}

// TestExplicitPinReplanDoesNotHuntAlternateOnRehydrationFailure proves issue
// #245 item 2 end to end. An explicit version pick starts, its provider pin then
// vanishes, and a failure_recovery replan cannot rehydrate it: the rehydration
// retry resolves a genuinely different release, so the same-release assertion
// refuses it. The terminal-driven alternate-file hunt is gated by the same
// negotiated fallback policy the non-rehydration path uses, so an explicit pick
// never consults the version fetcher and the session stays on the selected
// release. A control auto-selection case confirms the same fixture does hunt,
// so the assertion is load-bearing rather than trivially satisfied.
func TestExplicitPinReplanDoesNotHuntAlternateOnRehydrationFailure(t *testing.T) {
	const (
		neutralURI   = "virtual://movie/tt-explicit-alt"
		pinnedURI    = neutralURI + "?result=pinned"
		alternateURI = neutralURI + "?result=alt"
	)

	newFixture := func(t *testing.T) (*PlaybackHandler, *replanAlternateFileFetcher) {
		t.Helper()
		source := v3HandlerFixtureFile(t)
		source.ID = 810
		source.ContentID = "movie-explicit-alt"
		source.FilePath = pinnedURI
		source.VirtualOwnerInstallationID = 5
		source.Container = "virtual"
		source.CodecVideo = "hevc"
		source.Resolution = "1080p"
		source.Bitrate = 8_000
		source.ProviderVideoHash = "hash-pinned"
		source.ProviderReleaseName = "Movie.Pinned.1080p"
		source.VideoTracks = []models.VideoTrack{{
			Codec: "hevc", Profile: "Main", Level: 120, Width: 1920, Height: 1080,
			FrameRate: "24000/1001", Bitrate: 8_000, BitDepth: 8, VideoRange: "SDR", VideoRangeType: "SDR",
		}}

		alternate := *source
		alternate.ID = 811
		alternate.FilePath = alternateURI
		alternate.ProviderVideoHash = "hash-alt"
		alternate.ProviderReleaseName = "Movie.Alt.1080p"

		handler := NewPlaybackHandler(playback.NewSessionManager(0, 0), mapPlaybackFileResolver{files: map[int]*models.MediaFile{source.ID: source, alternate.ID: &alternate}})
		stubCopySeekAnchorV3(handler)
		handler.PlaybackConfig = playbackTestConfig(writePlaybackTestFFmpeg(t), t.TempDir())
		presetLocalRegistryV3(handler, playback.NewTransformationRegistryV3([]playback.TransformationSpecV3{
			{Name: playback.TransformationAudioToAACV3, RecipeVersion: playback.TransformationAudioToAACRecipeVersionV3, Available: true},
			{Name: playback.TransformationVideoToH264V3, RecipeVersion: playback.TransformationVideoToH264RecipeVersionV3, Available: true},
		}))
		handler.ItemAccess = allowAllPlaybackItemAccess{}
		handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{
			"allow_4k_transcode":                     "true",
			"playback.max_virtual_failover_attempts": "3",
		}}
		fetcher := &replanAlternateFileFetcher{inner: testPlaybackFileVersionFetcher{byContent: map[string][]*models.MediaFile{
			source.ContentID: {source, &alternate},
		}}}
		handler.FileVersionFetcher = fetcher

		handler.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
			return "http://127.0.0.1:9/stream?path=" + path, nil
		})
		handler.VirtualPlaybackStreamLister = VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{{
				ID: "pinned", URI: pinnedURI,
				Resolution: source.Resolution, CodecVideo: source.CodecVideo, CodecAudio: source.CodecAudio, Container: "mkv",
			}}, nil
		})
		// The pin vanishes after the session bound: a fresh, unbound start
		// serves it; a session-bound resolve without rotation reports it absent;
		// with rotation it serves a genuinely different release under the same
		// neutral key, which the same-release assertion must refuse.
		handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(ctx context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			if virtualResultCandidateID(uri) == "pinned" {
				switch {
				case !VirtualSessionBinding(ctx):
					return ResolvedVirtualMedia{
						URL: "http://127.0.0.1:9/pinned", URI: pinnedURI, CandidateID: "pinned",
						ProviderVideoHash: "hash-pinned", ProviderReleaseName: "Movie.Pinned.1080p",
					}, nil
				case !VirtualCandidateRotationAllowed(ctx):
					return ResolvedVirtualMedia{}, absentSessionPinError("pinned")
				default:
					return ResolvedVirtualMedia{
						URL: "http://127.0.0.1:9/other", URI: alternateURI, CandidateID: "alt",
						ProviderVideoHash: "hash-alt", ProviderReleaseName: "Movie.Alt.1080p",
					}, nil
				}
			}
			return ResolvedVirtualMedia{
				URL: "http://127.0.0.1:9/alt", URI: uri, CandidateID: virtualResultCandidateID(uri),
				ProviderVideoHash: "hash-alt", ProviderReleaseName: "Movie.Alt.1080p",
			}, nil
		})
		handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, file *models.MediaFile) (*models.MediaFile, error) {
			file.VideoTracks = source.VideoTracks
			file.AudioTracks = source.AudioTracks
			file.CodecVideo, file.CodecAudio, file.Resolution, file.Container = "hevc", "aac", "1080p", "mkv"
			stamp := time.Now()
			file.ProbeUpdatedAt = &stamp
			return file, nil
		}
		return handler, fetcher
	}

	startForSelection := func(fileSelection playback.FileSelectionV3) playback.StartRequestV3 {
		start := v3HandlerStartRequest()
		start.FileID = 810
		start.QualityPreference = "auto"
		start.FileSelection = fileSelection
		start.ClientPlaybackContext.Deliveries[playback.DeliveryClassHLSV3] = playback.DeliveryCapabilityV3{
			Enabled: true, SupportedOnDevice: true,
			Containers: []string{"hls"}, VideoCodecs: []string{"h264"}, AudioDecodeCodecs: []string{"aac"},
		}
		return start
	}

	run := func(t *testing.T, fileSelection playback.FileSelectionV3) (playback.DecisionResponseV3, *replanAlternateFileFetcher, *playback.AttemptRecordV3) {
		t.Helper()
		handler, fetcher := newFixture(t)
		start := startForSelection(fileSelection)
		rr := httptest.NewRecorder()
		handler.HandleStartPlayback(rr, httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", strings.NewReader(marshalV3StartRequest(t, start))).WithContext(newAuthorizedPlaybackContext()))
		var started playback.DecisionResponseV3
		if rr.Code != http.StatusCreated || json.Unmarshal(rr.Body.Bytes(), &started) != nil || started.PlaybackPlan == nil {
			t.Fatalf("start: %d %s", rr.Code, rr.Body.String())
		}
		session, err := handler.sessionMgr.GetSession(started.SessionID)
		if err != nil || session == nil {
			t.Fatalf("GetSession: %v", err)
		}
		if session.VirtualSourceURI != pinnedURI {
			t.Fatalf("start bound %q, want the pin %q", session.VirtualSourceURI, pinnedURI)
		}
		// Reset after the start so only the replan's hunts are counted.
		fetcher.calls.Store(0)

		recovery := playback.ReplanRequestV3{
			ProtocolVersion: playback.ProtocolV3, ClientFeatures: start.ClientFeatures,
			Operation: playback.ReplanOperationFailureRecoveryV3, PlaybackAttemptID: start.PlaybackAttemptID,
			ReplanRequestID: "explicit-alt-recovery-0001", FailedPlanID: started.PlaybackPlan.PlanID,
			PlanAttemptID: "explicit-alt-attempt-0001", PlanAttemptKey: started.PlaybackPlan.PlanAttemptKey,
			AttemptedPlanKeys: []string{started.PlaybackPlan.PlanAttemptKey}, AttemptCount: 1,
			PositionSeconds: 10, SelectedTracks: started.PlaybackPlan.SelectedTracks,
			Failure:               playback.FailureV3{Classification: "player_failure"},
			Capabilities:          start.Capabilities,
			ClientPlaybackContext: start.ClientPlaybackContext,
		}
		response := postPlaybackReplanV3(t, handler, started.SessionID, recovery)
		rec, err := handler.PlanStoreV3.GetAttempt(t.Context(), started.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		return response, fetcher, rec
	}

	t.Run("explicit pin never hunts", func(t *testing.T) {
		response, fetcher, rec := run(t, playback.FileSelectionExplicitV3)
		if got := fetcher.calls.Load(); got != 0 {
			t.Fatalf("explicit pin consulted the alternate-file hunt %d times, want 0", got)
		}
		if response.PlaybackPlan != nil {
			t.Fatalf("explicit pin was substituted with a plan: %#v", response.PlaybackPlan)
		}
		if response.Terminal == nil {
			t.Fatalf("explicit pin replan returned neither plan nor terminal: %#v", response)
		}
		if rec.EffectiveMediaFileID != 810 {
			t.Fatalf("effective file = %d, want the unchanged pinned row 810", rec.EffectiveMediaFileID)
		}
	})

	t.Run("auto selection still hunts", func(t *testing.T) {
		_, fetcher, _ := run(t, playback.FileSelectionAutoV3)
		if got := fetcher.calls.Load(); got == 0 {
			t.Fatal("auto selection did not consult the alternate-file hunt; the control is not exercising the path")
		}
	})
}
