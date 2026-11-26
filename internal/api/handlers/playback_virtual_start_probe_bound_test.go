package handlers

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

type virtualProbeBoundScenario struct {
	// selectedPlayable makes the requested release's probe return healthy bytes.
	// The default is the unplayable selection that forces a substitution.
	selectedPlayable bool
	// deadAlternates names 1-based alternate indexes whose probe fails as an
	// absent provider release.
	deadAlternates []int
	// cachedAlternates names 1-based alternate indexes the provider reports as
	// already completed locally, so their declared metadata is enough.
	cachedAlternates []int
}

type virtualProbeBoundFixture struct {
	requested  *models.MediaFile
	alternates []*models.MediaFile
	handler    *PlaybackHandler
	probeCalls *atomic.Int32
}

// A start must probe the release the viewer selected, not every sibling the
// planner might fall back to. The probe opens the provider's play endpoint,
// which mints a release and can fetch its NZB; walking six versions with one
// probe each turned one Play into a burst of provider side effects. The
// selected row is still probed, while sibling planning stays on the provider
// listing's declared metadata.
func TestVirtualStartProbesOnlyTheSelectedCandidate(t *testing.T) {
	fixture := newVirtualProbeBoundFixture(t, virtualProbeBoundScenario{selectedPlayable: true})
	started := startV3PlaybackForHandlerTest(t, fixture.handler, virtualProbeBoundStartRequest(t, fixture.requested))

	if started.PlaybackPlan == nil {
		t.Fatalf("start returned no plan: terminal=%#v", started.Terminal)
	}
	if started.PlaybackPlan.EffectiveMediaFileID != fixture.requested.ID {
		t.Fatalf("start served file %d, want the playable selected row %d",
			started.PlaybackPlan.EffectiveMediaFileID, fixture.requested.ID)
	}
	if started.PlaybackPlan.SubstitutedFromFileID != 0 {
		t.Fatalf("canonical start reported a substitution from %d", started.PlaybackPlan.SubstitutedFromFileID)
	}
	if got := fixture.probeCalls.Load(); got != 1 {
		t.Fatalf("provider play calls = %d, want 1 (only the selected candidate); alternates must be planned from the listing", got)
	}
}

// A substitute is one probe away from being trusted. The selected release is
// unplayable, so the start moves to a live alternate and must verify those bytes
// before the plan commits to them.
func TestVirtualStartVerifiesLiveSubstituteBeforeCommit(t *testing.T) {
	fixture := newVirtualProbeBoundFixture(t, virtualProbeBoundScenario{})
	started := startV3PlaybackForHandlerTest(t, fixture.handler, virtualProbeBoundStartRequest(t, fixture.requested))

	if started.PlaybackPlan == nil {
		t.Fatalf("start returned no plan: terminal=%#v", started.Terminal)
	}
	if started.PlaybackPlan.EffectiveMediaFileID == fixture.requested.ID {
		t.Fatalf("start served the unplayable selected row %d instead of substituting", fixture.requested.ID)
	}
	if started.PlaybackPlan.SubstitutedFromFileID != fixture.requested.ID || started.PlaybackPlan.SubstitutionReason != substitutionReasonVersionNotPlayableV3 {
		t.Fatalf("substitution = from %d reason %q, want from %d reason %q",
			started.PlaybackPlan.SubstitutedFromFileID, started.PlaybackPlan.SubstitutionReason,
			fixture.requested.ID, substitutionReasonVersionNotPlayableV3)
	}
	if got := fixture.probeCalls.Load(); got != 2 {
		t.Fatalf("provider play calls = %d, want 2 (the selected row and the chosen substitute)", got)
	}
}

// A substitute the provider reports completed is already held locally. Its
// declared metadata is enough, so the start must not spend a provider play call
// on it.
func TestVirtualStartTrustsCachedSubstituteWithoutProbe(t *testing.T) {
	fixture := newVirtualProbeBoundFixture(t, virtualProbeBoundScenario{cachedAlternates: []int{1}})
	started := startV3PlaybackForHandlerTest(t, fixture.handler, virtualProbeBoundStartRequest(t, fixture.requested))

	if started.PlaybackPlan == nil {
		t.Fatalf("start returned no plan: terminal=%#v", started.Terminal)
	}
	if started.PlaybackPlan.EffectiveMediaFileID != fixture.alternates[0].ID {
		t.Fatalf("start served file %d, want the cached substitute %d",
			started.PlaybackPlan.EffectiveMediaFileID, fixture.alternates[0].ID)
	}
	if started.PlaybackPlan.SubstitutedFromFileID != fixture.requested.ID || started.PlaybackPlan.SubstitutionReason != substitutionReasonVersionNotPlayableV3 {
		t.Fatalf("substitution = from %d reason %q, want from %d reason %q",
			started.PlaybackPlan.SubstitutedFromFileID, started.PlaybackPlan.SubstitutionReason,
			fixture.requested.ID, substitutionReasonVersionNotPlayableV3)
	}
	if got := fixture.probeCalls.Load(); got != 1 {
		t.Fatalf("provider play calls = %d, want 1 (only the selected row; the cached substitute needs none)", got)
	}
}

// A substitute the provider no longer has must never be committed on its
// listing declaration alone. The dead first alternate is rejected by its probe
// and the walk moves to the next live candidate, with a bounded number of
// provider calls instead of waiting out the transport budget on dead bytes.
func TestVirtualStartRejectsDeadSubstituteBeforeCommit(t *testing.T) {
	fixture := newVirtualProbeBoundFixture(t, virtualProbeBoundScenario{deadAlternates: []int{1}})
	started := startV3PlaybackForHandlerTest(t, fixture.handler, virtualProbeBoundStartRequest(t, fixture.requested))

	if started.PlaybackPlan == nil {
		t.Fatalf("start returned no plan: terminal=%#v", started.Terminal)
	}
	effective := started.PlaybackPlan.EffectiveMediaFileID
	if effective == fixture.requested.ID {
		t.Fatalf("start served the unplayable selected row %d instead of substituting", fixture.requested.ID)
	}
	if effective == fixture.alternates[0].ID {
		t.Fatalf("start committed to the dead substitute %d", effective)
	}
	if started.PlaybackPlan.SubstitutedFromFileID != fixture.requested.ID || started.PlaybackPlan.SubstitutionReason != substitutionReasonVersionNotPlayableV3 {
		t.Fatalf("substitution = from %d reason %q, want from %d reason %q",
			started.PlaybackPlan.SubstitutedFromFileID, started.PlaybackPlan.SubstitutionReason,
			fixture.requested.ID, substitutionReasonVersionNotPlayableV3)
	}
	// Selected probe + dead substitute probe + one live substitute probe. Any
	// higher count means a dead candidate was planned and re-walked instead of
	// being rejected once.
	if got := fixture.probeCalls.Load(); got != 3 {
		t.Fatalf("provider play calls = %d, want 3 (selected, dead substitute, live substitute)", got)
	}
}

func newVirtualProbeBoundFixture(t *testing.T, scenarios ...virtualProbeBoundScenario) virtualProbeBoundFixture {
	t.Helper()
	scenario := virtualProbeBoundScenario{}
	if len(scenarios) > 0 {
		scenario = scenarios[0]
	}
	// Each test needs its own content key: probe failures and sticky pins live in
	// package-level tables, and a damper armed by one case would make the next
	// answer from declared metadata instead of exercising its probe.
	content := "movie-probe-bound-" + strings.ToLower(t.Name())
	neutral := "virtual://movie/" + content

	requested := virtualProbeBoundSelectedRow(t, content, neutral)
	alternates, streams := virtualProbeBoundAlternates(requested, neutral)
	deadURIs := make(map[string]bool, len(scenario.deadAlternates))
	for _, index := range scenario.deadAlternates {
		if index >= 1 && index <= len(alternates) {
			deadURIs[alternates[index-1].FilePath] = true
		}
	}
	cachedURIs := make(map[string]bool, len(scenario.cachedAlternates))
	for _, index := range scenario.cachedAlternates {
		if index >= 1 && index <= len(alternates) {
			cachedURIs[alternates[index-1].FilePath] = true
		}
	}
	probeCalls := &atomic.Int32{}
	return virtualProbeBoundFixture{
		requested:  requested,
		alternates: alternates,
		handler:    virtualProbeBoundHandler(t, requested, alternates, streams, probeCalls, scenario.selectedPlayable, deadURIs, cachedURIs),
		probeCalls: probeCalls,
	}
}

func virtualProbeBoundSelectedRow(t *testing.T, content, neutral string) *models.MediaFile {
	t.Helper()
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
	return requested
}

func virtualProbeBoundAlternates(requested *models.MediaFile, neutral string) ([]*models.MediaFile, []VirtualPlaybackStream) {
	alternates := make([]*models.MediaFile, 0, 6)
	streams := []VirtualPlaybackStream{{
		ID: "selected", URI: requested.FilePath, OwnerInstallationID: 5,
		PreserveProviderOrder: true,
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
			FileSize: alternate.FileSize, PreserveProviderOrder: true,
		})
	}
	return alternates, streams
}

func virtualProbeBoundHandler(t *testing.T, requested *models.MediaFile, alternates []*models.MediaFile, streams []VirtualPlaybackStream, probeCalls *atomic.Int32, selectedPlayable bool, deadURIs, cachedURIs map[string]bool) *PlaybackHandler {
	t.Helper()
	byPath := map[string]*models.MediaFile{requested.FilePath: requested}
	for _, alternate := range alternates {
		byPath[alternate.FilePath] = alternate
	}
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0), byPathPlaybackFileResolver{
		testPlaybackFileResolver: testPlaybackFileResolver{file: requested},
		byPath:                   byPath,
	})
	handler.FileVersionFetcher = testPlaybackFileVersionFetcher{byContent: map[string][]*models.MediaFile{
		requested.ContentID: append([]*models.MediaFile{requested}, alternates...),
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
	handler.VirtualPlaybackSourceProber = virtualProbeBoundProber(probeCalls, requested, selectedPlayable, deadURIs)
	handler.VirtualFileSaver = func(context.Context, models.VirtualFilePersistArgs) (int64, error) { return 1, nil }
	// AltMount is wired: report the scenario's cached releases, otherwise every
	// substitute is uncached and must be verified before commit.
	handler.VirtualReleaseCacheStatus = func(_ context.Context, uri string, _ int) (bool, bool) {
		return cachedURIs[uri], true
	}
	handler.ItemLookup = staticRuntimeItemLookup{runtime: 10}
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"allow_4k_transcode": "true"}}
	handler.PlaybackConfig = playbackTestConfig("", "")
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	return handler
}

// virtualProbeBoundProber returns the requested release's bytes as
// contradicting its declared runtime unless selectedPlayable is set. A listed
// alternate is healthy unless its URI is dead, and an absent dead release
// returns the provider error a vanished release produces.
func virtualProbeBoundProber(probeCalls *atomic.Int32, requested *models.MediaFile, selectedPlayable bool, deadURIs map[string]bool) func(context.Context, string, *models.MediaFile) (*models.MediaFile, error) {
	return func(_ context.Context, _ string, file *models.MediaFile) (*models.MediaFile, error) {
		probeCalls.Add(1)
		if deadURIs[file.FilePath] {
			return nil, errors.New("provider release is absent")
		}
		probed := *file
		probed.ProbedFormatSize = file.FileSize
		if file.FilePath != requested.FilePath || selectedPlayable {
			probed.Duration = 600 // matches the fixture item's 10-minute runtime
			return &probed, nil
		}
		// The requested release's bytes contradict its declared runtime, so the
		// start must treat it as unplayable and substitute.
		probed.VideoTracks = nil
		probed.Resolution = ""
		probed.AudioTracks = []models.AudioTrack{{Codec: "aac", Channels: 2, Layout: "stereo", Default: true}}
		probed.Container = "mp4"
		probed.CodecVideo = ""
		probed.CodecAudio = "aac"
		probed.ProbedFormatSize = 1_500_000_000
		return &probed, nil
	}
}

func virtualProbeBoundStartRequest(t *testing.T, requested *models.MediaFile) playback.StartRequestV3 {
	t.Helper()
	start := v3HandlerStartRequest()
	start.FileID = requested.ID
	start.QualityPreference = "auto"
	start.ClientPlaybackContext.Deliveries[playback.DeliveryClassHLSV3] = playback.DeliveryCapabilityV3{
		Enabled: true, SupportedOnDevice: true, Containers: []string{"hls"},
		VideoCodecs: []string{"h264"}, AudioDecodeCodecs: []string{"aac"},
	}
	return start
}
