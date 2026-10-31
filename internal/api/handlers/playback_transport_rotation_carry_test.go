package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/remotestream"
)

// transportRotationCarryResponse drives a virtual start whose primary transport
// fails transiently and whose alternate starts. sourceSubs and targetSubs
// describe the two releases' external subtitle inventories; the request selects
// the carried ordinal in sourceSubs.
func transportRotationCarryResponse(t *testing.T, sourceSubs, targetSubs []models.ExternalSubtitle, carried int) playback.DecisionResponseV3 {
	t.Helper()
	const (
		neutral   = "virtual://movie/tt-transport-carry"
		pinnedURI = neutral + "?result=pinned"
		altURI    = neutral + "?result=alt"
	)
	stamp := time.Now()
	source := v3HandlerFixtureFile(t)
	source.ID = 900
	source.ContentID = "movie-transport-carry"
	source.Container = "mkv"
	source.FilePath = pinnedURI
	source.VirtualOwnerInstallationID = 5
	source.ProbeUpdatedAt = &stamp
	source.ExternalSubtitles = sourceSubs
	source.SubtitleTracks = nil

	alternateValue := *source
	alternate := &alternateValue
	alternate.ID = 901
	alternate.FilePath = altURI
	alternate.ProbeUpdatedAt = &stamp
	alternate.ExternalSubtitles = targetSubs

	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0), mapPlaybackFileResolver{files: map[int]*models.MediaFile{source.ID: source, alternate.ID: alternate}})
	handler.FileVersionFetcher = testPlaybackFileVersionFetcher{byContent: map[string][]*models.MediaFile{
		source.ContentID: {source, alternate},
	}}
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"allow_4k_transcode": "true"}}
	handler.PlaybackConfig = playbackTestConfig(writePlaybackTestFFmpeg(t), t.TempDir())
	presetLocalRegistryV3(handler, playback.NewTransformationRegistryV3([]playback.TransformationSpecV3{
		{Name: "audio_to_aac", RecipeVersion: "2", Available: true},
		{Name: "video_to_h264", RecipeVersion: "2", Available: true},
	}))
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	relay := remotestream.NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()
	handler.RemoteStreamRelay = relay
	handler.AllowPrivateStreams = func(int) bool { return true }
	handler.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
		return "http://127.0.0.1:9/stream?path=" + path, nil
	})
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		switch {
		case strings.Contains(uri, "pinned"):
			// The bound pin is absent: a release verdict at the transport anchor,
			// which the start path handles by rotating to the alternate.
			return ResolvedVirtualMedia{}, absentSessionPinError("pinned")
		case strings.Contains(uri, "alt"):
			return ResolvedVirtualMedia{URL: "http://127.0.0.1:9/alt.mkv", URI: altURI, CandidateID: "alt"}, nil
		default:
			return ResolvedVirtualMedia{}, fmt.Errorf("unexpected resolve %q", uri)
		}
	})
	handler.copySeekAnchor = func(_ context.Context, _ string, _ string, requested float64, segmentDuration int) (float64, int, error) {
		return requested, computeStartSegment(requested, segmentDuration), nil
	}

	start := v3HandlerStartRequest()
	start.FileID = source.ID
	start.QualityPreference = "auto"
	start.ClientPlaybackContext.Deliveries = map[string]playback.DeliveryCapabilityV3{
		playback.DeliveryClassProgressiveV3: {Enabled: true, SupportedOnDevice: true, Subtitles: playback.DeliverySubtitleCapabilitiesV3{EmbeddedText: true}},
		playback.DeliveryClassHLSV3:         {Enabled: true, SupportedOnDevice: true, Subtitles: playback.DeliverySubtitleCapabilitiesV3{EmbeddedText: true, SidecarText: true}},
	}
	start.SubtitleTrackIndex = &carried
	start.SubtitleTrackID = playback.TrackIDV3(source.ID, "subtitle", carried)
	startPosition := 30.0
	start.StartPosition = &startPosition

	rr := httptest.NewRecorder()
	handler.HandleStartPlayback(rr, httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", strings.NewReader(marshalV3StartRequest(t, start))).WithContext(newAuthorizedPlaybackContext()))
	if rr.Code != http.StatusCreated {
		t.Fatalf("start status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var response playback.DecisionResponseV3
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("start response invalid: %v", err)
	}
	return response
}

// TestTransportRotationCarriesExactSubtitleSelection proves the transport-failure
// rotation carries an unchanged track identity onto the alternate even when its
// ordinal moves: the French sidecar is at 1 on the dead release and at 0 on the
// alternate, with the same format and label.
func TestTransportRotationCarriesExactSubtitleSelection(t *testing.T) {
	response := transportRotationCarryResponse(t,
		[]models.ExternalSubtitle{
			{Path: writePlaybackTestMediaFile(t, "eng.srt"), Language: "eng", Format: "srt"},
			{Path: writePlaybackTestMediaFile(t, "fra.srt"), Language: "fra", Format: "srt"},
		},
		[]models.ExternalSubtitle{
			{Path: writePlaybackTestMediaFile(t, "fra.srt"), Language: "fra", Format: "srt"},
			{Path: writePlaybackTestMediaFile(t, "jpn.srt"), Language: "jpn", Format: "srt"},
		},
		1,
	)
	if response.Terminal != nil {
		t.Fatalf("transport rotation terminaled instead of carrying: %+v", response.Terminal)
	}
	if response.PlaybackPlan == nil {
		t.Fatal("transport rotation produced no plan")
	}
	if response.PlaybackPlan.EffectiveMediaFileID != 901 {
		t.Fatalf("effective file = %d, want the alternate 901", response.PlaybackPlan.EffectiveMediaFileID)
	}
	selected := response.PlaybackPlan.SelectedTracks.Subtitle
	if selected == nil || selected.Index == nil || *selected.Index != 0 {
		t.Fatalf("selected subtitle = %#v, want the carried French track at the alternate ordinal 0", selected)
	}
	for _, warning := range response.PlaybackPlan.DegradationWarnings {
		if warning.Code == playback.DegradationWarningSubtitleCarryV3 {
			t.Fatalf("degradation warnings = %#v, an exact carry must not warn", response.PlaybackPlan.DegradationWarnings)
		}
	}
}

// TestTransportRotationCarriesFuzzySubtitleSelection proves the transport-failure
// rotation runs the #396 carry matcher instead of the generic edition-switch
// remap: the alternate publishes the French track in a different webvtt-family
// codec at a different ordinal, so only the strict matcher can carry it, and the
// plan reports the fuzzy-carry warning.
func TestTransportRotationCarriesFuzzySubtitleSelection(t *testing.T) {
	// Source: English then French, both srt. Target: French then Japanese, with
	// French re-encoded as vtt. Same muxer class and language, different codec
	// tiebreak and ordinal.
	response := transportRotationCarryResponse(t,
		[]models.ExternalSubtitle{
			{Path: writePlaybackTestMediaFile(t, "eng.srt"), Language: "eng", Format: "srt"},
			{Path: writePlaybackTestMediaFile(t, "fra.srt"), Language: "fra", Format: "srt"},
		},
		[]models.ExternalSubtitle{
			{Path: writePlaybackTestMediaFile(t, "fra.vtt"), Language: "fra", Format: "vtt"},
			{Path: writePlaybackTestMediaFile(t, "jpn.srt"), Language: "jpn", Format: "srt"},
		},
		1,
	)
	if response.Terminal != nil {
		t.Fatalf("transport rotation terminaled instead of degrading: %+v", response.Terminal)
	}
	if response.PlaybackPlan == nil {
		t.Fatal("transport rotation produced no plan")
	}
	if response.PlaybackPlan.EffectiveMediaFileID != 901 {
		t.Fatalf("effective file = %d, want the alternate 901", response.PlaybackPlan.EffectiveMediaFileID)
	}
	selected := response.PlaybackPlan.SelectedTracks.Subtitle
	if selected == nil || selected.Index == nil || *selected.Index != 0 {
		t.Fatalf("selected subtitle = %#v, want the carried French track at the alternate ordinal 0", selected)
	}
	carried := false
	for _, warning := range response.PlaybackPlan.DegradationWarnings {
		if warning.Code == playback.DegradationWarningSubtitleCarryV3 {
			carried = true
		}
	}
	if !carried {
		t.Fatalf("degradation warnings = %#v, want the fuzzy subtitle carry warning", response.PlaybackPlan.DegradationWarnings)
	}
}

// TestTransportRotationDegradesOnCarryMiss proves the same transport-failure
// rotation uses the strict matcher's fail-closed verdict: when the alternate has
// no equivalent of the carried track, the plan plays without subtitles instead of
// leaving the old ordinal to name whatever now occupies that position.
func TestTransportRotationDegradesOnCarryMiss(t *testing.T) {
	response := transportRotationCarryResponse(t,
		[]models.ExternalSubtitle{
			{Path: writePlaybackTestMediaFile(t, "eng.srt"), Language: "eng", Format: "srt"},
			{Path: writePlaybackTestMediaFile(t, "fra.srt"), Language: "fra", Format: "srt"},
		},
		[]models.ExternalSubtitle{
			{Path: writePlaybackTestMediaFile(t, "jpn.srt"), Language: "jpn", Format: "srt"},
		},
		1,
	)
	if response.Terminal != nil {
		t.Fatalf("transport rotation terminaled instead of degrading: %+v", response.Terminal)
	}
	if response.PlaybackPlan == nil {
		t.Fatal("transport rotation produced no plan")
	}
	if response.PlaybackPlan.EffectiveMediaFileID != 901 {
		t.Fatalf("effective file = %d, want the alternate 901", response.PlaybackPlan.EffectiveMediaFileID)
	}
	if selected := response.PlaybackPlan.SelectedTracks.Subtitle; selected != nil {
		t.Fatalf("selected subtitle = %#v, want subtitles off after a confirmed carry miss", selected)
	}
	for _, warning := range response.PlaybackPlan.DegradationWarnings {
		if warning.Code == playback.DegradationWarningSubtitleCarryV3 {
			t.Fatalf("degradation warnings = %#v, a miss must not claim a fuzzy carry", response.PlaybackPlan.DegradationWarnings)
		}
	}
}
