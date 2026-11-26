package handlers

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// A start must probe the release the viewer selected, not every sibling the
// planner might fall back to. The probe opens the provider's play endpoint,
// which mints a release and can fetch its NZB; walking six versions with one
// probe each turned one Play into a burst of provider side effects. The
// selected row is still probed (so a genuinely unplayable pick is detected and
// substituted), while the other candidates are planned from the provider
// listing's declared metadata.
func TestVirtualStartProbesOnlyTheSelectedCandidate(t *testing.T) {
	const content = "movie-probe-bound"
	neutral := "virtual://movie/" + content

	requested := v3HandlerFixtureFile(t)
	requested.ID = 700
	requested.ContentID = content
	requested.FilePath = neutral + "?result=selected"
	requested.VirtualOwnerInstallationID = 5
	requested.ProviderVideoHash = "hash-selected"
	requested.ProviderReleaseName = "Movie.Selected.1080p"
	requested.FileSize = 2_000_000_000
	// A stale probe stamp makes the row suspect, forcing the start's synchronous
	// validation instead of the deferred fast path.
	stale := time.Now().Add(-30 * 24 * time.Hour)
	requested.ProbeUpdatedAt = &stale

	alternates := make([]*models.MediaFile, 0, 6)
	streams := []VirtualPlaybackStream{{
		ID: "selected", URI: requested.FilePath, OwnerInstallationID: 5,
	}}
	for i := 1; i <= 6; i++ {
		alternateValue := *requested
		alternate := &alternateValue
		alternate.ID = requested.ID + i
		alternate.FilePath = neutral + "?result=alt-" + strconv.Itoa(i)
		alternate.ProviderVideoHash = "hash-alt-" + strconv.Itoa(i)
		alternate.ProviderReleaseName = "Movie.Alt" + strconv.Itoa(i) + ".1080p"
		// The alternates can be planned from what the listing declares.
		alternate.Container = "mp4"
		alternate.Resolution = "1080p"
		alternate.CodecVideo = "h264"
		alternate.CodecAudio = "aac"
		alternate.VideoTracks = []models.VideoTrack{{Codec: "h264", Profile: "high", Level: 41, Width: 1920, Height: 1080, FrameRate: "24000/1001", Bitrate: 8_000, BitDepth: 8, VideoRange: "SDR", VideoRangeType: "SDR"}}
		alternate.AudioTracks = []models.AudioTrack{{Codec: "aac", Channels: 2, Layout: "stereo", Default: true}}
		alternates = append(alternates, alternate)
		streams = append(streams, VirtualPlaybackStream{
			ID: "alt-" + strconv.Itoa(i), URI: alternate.FilePath, OwnerInstallationID: 5,
			Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac", Container: "mp4",
		})
	}

	byPath := map[string]*models.MediaFile{requested.FilePath: requested}
	for _, alternate := range alternates {
		byPath[alternate.FilePath] = alternate
	}
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0), byPathPlaybackFileResolver{
		testPlaybackFileResolver: testPlaybackFileResolver{file: requested},
		byPath:                   byPath,
	})
	handler.FileVersionFetcher = testPlaybackFileVersionFetcher{byContent: map[string][]*models.MediaFile{
		content: append([]*models.MediaFile{requested}, alternates...),
	}}
	handler.VirtualFileLookup = func(_ context.Context, path string) (*models.MediaFile, error) {
		return byPath[path], nil
	}
	handler.VirtualPlaybackStreamLister = VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
		return streams, nil
	})
	handler.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
		return "http://127.0.0.1:8080/stream?path=" + path, nil
	})
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		return ResolvedVirtualMedia{
			URL: "http://127.0.0.1:8080/stream?path=" + uri, URI: uri,
			CandidateID: virtualResultCandidateID(uri), OwnerID: 5,
		}, nil
	})
	var probeCalls atomic.Int32
	handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, file *models.MediaFile) (*models.MediaFile, error) {
		probeCalls.Add(1)
		probed := *file
		// The provider served different bytes than the selected release
		// declares: a track-layout contradiction (declared video, probed
		// audio-only container) the start must treat as unplayable and rotate
		// away from.
		probed.VideoTracks = nil
		probed.Resolution = ""
		probed.AudioTracks = []models.AudioTrack{{Codec: "aac", Channels: 2, Layout: "stereo", Default: true}}
		probed.Container = "mp4"
		probed.CodecVideo = ""
		probed.CodecAudio = "aac"
		probed.ProbedFormatSize = 1_500_000_000
		return &probed, nil
	}
	handler.VirtualFileSaver = func(context.Context, models.VirtualFilePersistArgs) (int64, error) { return 1, nil }
	// AltMount is wired: every substitute is reported uncached, so only the
	// selected release may be probed.
	handler.VirtualReleaseCacheStatus = func(context.Context, string, int) (bool, bool) { return false, true }
	handler.ItemLookup = staticRuntimeItemLookup{runtime: 10}
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"allow_4k_transcode": "true"}}
	handler.PlaybackConfig = playbackTestConfig("", "")
	handler.ItemAccess = allowAllPlaybackItemAccess{}

	start := v3HandlerStartRequest()
	start.FileID = requested.ID
	start.QualityPreference = "auto"
	start.ClientPlaybackContext.Deliveries[playback.DeliveryClassHLSV3] = playback.DeliveryCapabilityV3{
		Enabled: true, SupportedOnDevice: true, Containers: []string{"hls"},
		VideoCodecs: []string{"h264"}, AudioDecodeCodecs: []string{"aac"},
	}
	started := startV3PlaybackForHandlerTest(t, handler, start)

	if started.PlaybackPlan == nil {
		t.Fatalf("start returned no plan: terminal=%#v", started.Terminal)
	}
	if started.PlaybackPlan.EffectiveMediaFileID == requested.ID {
		t.Fatalf("start served the unplayable selected row %d instead of substituting", requested.ID)
	}
	if started.PlaybackPlan.SubstitutedFromFileID != requested.ID || started.PlaybackPlan.SubstitutionReason != substitutionReasonVersionNotPlayableV3 {
		t.Fatalf("substitution = from %d reason %q, want from %d reason %q",
			started.PlaybackPlan.SubstitutedFromFileID, started.PlaybackPlan.SubstitutionReason,
			requested.ID, substitutionReasonVersionNotPlayableV3)
	}
	if got := probeCalls.Load(); got != 1 {
		t.Fatalf("provider play calls = %d, want 1 (only the selected candidate); alternates must be planned from the listing", got)
	}
}
