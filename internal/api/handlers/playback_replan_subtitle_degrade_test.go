package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// subtitleReplanFixtureFile is the handler's standard fixture with a handful of
// distinct embedded subtitle tracks, so an ordinal past them is genuinely
// out of range for the effective file.
func subtitleReplanFixtureFile(t *testing.T, withTracks int) *models.MediaFile {
	t.Helper()
	file := v3HandlerFixtureFile(t)
	file.SubtitleTracks = make([]models.SubtitleTrack, 0, withTracks)
	for index := 0; index < withTracks; index++ {
		file.SubtitleTracks = append(file.SubtitleTracks, models.SubtitleTrack{Codec: "subrip", Index: index + 1, Language: "eng"})
	}
	return file
}

func subtitleReplanHandler(t *testing.T, file *models.MediaFile) *PlaybackHandler {
	t.Helper()
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0), testPlaybackFileResolver{file: file})
	stubCopySeekAnchorV3(handler)
	handler.PlaybackConfig = playbackTestConfig(writePlaybackTestFFmpeg(t), t.TempDir())
	presetLocalRegistryV3(handler, playback.NewTransformationRegistryV3([]playback.TransformationSpecV3{
		{Name: "audio_to_aac", RecipeVersion: "2", Available: true},
		{Name: "video_to_h264", RecipeVersion: "2", Available: true},
	}))
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"allow_4k_transcode": "true"}}
	return handler
}

func startSubtitleReplanSessionV3(t *testing.T, handler *PlaybackHandler) (string, playback.DecisionResponseV3, playback.StartRequestV3) {
	t.Helper()
	start := v3HandlerStartRequest()
	rr := httptest.NewRecorder()
	handler.HandleStartPlayback(rr, httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", strings.NewReader(marshalV3StartRequest(t, start))).WithContext(newAuthorizedPlaybackContext()))
	var started playback.DecisionResponseV3
	if rr.Code != http.StatusCreated || json.Unmarshal(rr.Body.Bytes(), &started) != nil || started.PlaybackPlan == nil {
		t.Fatalf("start: %d %s", rr.Code, rr.Body.String())
	}
	return started.SessionID, started, start
}

func subtitleReplanTrackChangeV3(started playback.DecisionResponseV3, start playback.StartRequestV3, fileID int, ordinal int, requestID string) playback.ReplanRequestV3 {
	index := ordinal
	return playback.ReplanRequestV3{
		ProtocolVersion: playback.ProtocolV3, ClientFeatures: start.ClientFeatures,
		Operation:         playback.ReplanOperationTrackChangeV3,
		PlaybackAttemptID: start.PlaybackAttemptID,
		ReplanRequestID:   requestID,
		FailedPlanID:      started.PlaybackPlan.PlanID,
		PlanAttemptID:     "subtitle-degrade-" + requestID,
		PlanAttemptKey:    started.PlaybackPlan.PlanAttemptKey,
		AttemptCount:      1,
		QualityPreference: start.QualityPreference,
		PositionSeconds:   10,
		SelectedTracks: playback.SelectedTracksV3{Subtitle: &playback.TrackIdentityV3{
			ID: playback.TrackIDV3(fileID, "subtitle", ordinal), Index: &index,
		}},
		Capabilities:          start.Capabilities,
		ClientPlaybackContext: start.ClientPlaybackContext,
	}
}

// A retry storm of identical track_change picks against the same effective
// inventory must record the out-of-range disable once. The second replan keeps
// the subtitles-off outcome silently instead of re-logging the same warning for
// a decision nothing changed. Once the inventory actually changes, the pick is
// re-evaluated and can resolve.
func TestReplanRepeatedSubtitlePickDisablesOnce(t *testing.T) {
	logs := captureHandlerLogs(t)
	file := subtitleReplanFixtureFile(t, 3)
	handler := subtitleReplanHandler(t, file)
	sessionID, started, start := startSubtitleReplanSessionV3(t, handler)

	degrade := func(requestID string, previous playback.DecisionResponseV3) playback.DecisionResponseV3 {
		t.Helper()
		request := subtitleReplanTrackChangeV3(previous, start, file.ID, 20, requestID)
		response := postPlaybackReplanV3(t, handler, sessionID, request)
		if response.Terminal != nil {
			t.Fatalf("replan %s returned a terminal: %#v", requestID, response.Terminal)
		}
		if response.PlaybackPlan == nil {
			t.Fatalf("replan %s returned no plan", requestID)
		}
		return response
	}

	first := degrade("subtitle-degrade-0001", started)
	if first.PlaybackPlan.Subtitle.Mode != playback.SubtitleOffV3 || first.PlaybackPlan.SelectedTracks.Subtitle != nil {
		t.Fatalf("first replan = %#v, want the out-of-range pick degraded to subtitles off", first.PlaybackPlan.Subtitle)
	}
	if !planWarnsSubtitleUnavailableV3(first.PlaybackPlan) {
		t.Fatal("first replan must report the dropped subtitle")
	}
	if got := strings.Count(logs.String(), "subtitle selection out of range"); got != 1 {
		t.Fatalf("first pick logged the disable %d times, want 1:\n%s", got, logs.String())
	}

	// The identical retry: same pick, same inventory, no rotation. It must keep
	// the outcome and stay silent.
	second := degrade("subtitle-degrade-0002", first)
	if second.PlaybackPlan.Subtitle.Mode != playback.SubtitleOffV3 || second.PlaybackPlan.SelectedTracks.Subtitle != nil {
		t.Fatalf("second replan = %#v, want the previous subtitles-off outcome", second.PlaybackPlan.Subtitle)
	}
	if !planWarnsSubtitleUnavailableV3(second.PlaybackPlan) {
		t.Fatal("the repeated replan must keep the degradation warning on its plan")
	}
	if got := strings.Count(logs.String(), "subtitle selection out of range"); got != 1 {
		t.Fatalf("the identical retry re-logged the disable (%d lines):\n%s", got, logs.String())
	}

	// The probe finally enumerates the real release: the inventory now carries
	// the ordinal, so the same pick resolves instead of being reused.
	file.SubtitleTracks = subtitleReplanFixtureFile(t, 21).SubtitleTracks
	third := degrade("subtitle-degrade-0003", second)
	if third.PlaybackPlan.Subtitle.Mode == playback.SubtitleOffV3 || third.PlaybackPlan.SelectedTracks.Subtitle == nil {
		t.Fatalf("third replan = %#v, want the pick resolved by the changed inventory", third.PlaybackPlan.Subtitle)
	}
	if third.PlaybackPlan.SelectedTracks.Subtitle.Index == nil || *third.PlaybackPlan.SelectedTracks.Subtitle.Index != 20 {
		t.Fatalf("selected subtitle = %#v, want ordinal 20", third.PlaybackPlan.SelectedTracks.Subtitle)
	}
	if got := strings.Count(logs.String(), "subtitle selection out of range"); got != 1 {
		t.Fatalf("the resolved replan re-ran the disable (%d lines):\n%s", got, logs.String())
	}
}

func planWarnsSubtitleUnavailableV3(plan *playback.PlanV3) bool {
	for _, warning := range plan.DegradationWarnings {
		if warning.Code == playback.DegradationWarningSubtitleTrackUnavailableV3 {
			return true
		}
	}
	return false
}
