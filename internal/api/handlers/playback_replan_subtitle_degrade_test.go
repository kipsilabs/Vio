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

// A track_change naming a subtitle the effective file's inventory cannot honor
// must answer with a stable, documented rejection instead of a bare
// subtitles-off success. The plan stays playable, but the claims reason is the
// terminal signal a client stops retrying on.
func TestReplanOutOfRangeSubtitlePickReportsSelectionUnavailable(t *testing.T) {
	file := subtitleReplanFixtureFile(t, 3)
	handler := subtitleReplanHandler(t, file)
	sessionID, started, start := startSubtitleReplanSessionV3(t, handler)

	request := subtitleReplanTrackChangeV3(started, start, file.ID, 20, "selection-rejected-0001")
	response := postPlaybackReplanV3(t, handler, sessionID, request)
	if response.Terminal != nil {
		t.Fatalf("an out-of-range subtitle pick must not refuse playback: %#v", response.Terminal)
	}
	if response.PlaybackPlan == nil {
		t.Fatal("replan returned no plan")
	}
	if response.PlaybackPlan.Subtitle.Mode != playback.SubtitleOffV3 {
		t.Fatalf("subtitle mode = %q, want off for an unhonorable pick", response.PlaybackPlan.Subtitle.Mode)
	}
	if got := response.PlaybackPlan.Claims.Subtitles.Reason; got != playback.SubtitleReasonSelectionUnavailableV3 {
		t.Fatalf("claims.subtitles.reason = %q, want %q", got, playback.SubtitleReasonSelectionUnavailableV3)
	}
	if !planWarnsSubtitleUnavailableV3(response.PlaybackPlan) {
		t.Fatal("the rejected pick must still carry the degradation warning")
	}
}

// A client that ignores selection_unavailable can replay the same rejected
// subtitle pick forever. The first rejection is still explicit so a capable
// client learns it; every identical repeat must return the durable plan
// unchanged, with the same plan id and playhead, instead of minting a fresh
// plan and rebuilding the transport.
func TestReplanRepeatedUnhonorableSubtitlePickReturnsCurrentPlan(t *testing.T) {
	file := subtitleReplanFixtureFile(t, 3)
	handler := subtitleReplanHandler(t, file)
	sessionID, started, start := startSubtitleReplanSessionV3(t, handler)

	const livePosition = 187.5
	if err := handler.sessionMgr.UpdateProgress(sessionID, livePosition, false); err != nil {
		t.Fatalf("update progress: %v", err)
	}

	first := postPlaybackReplanV3(t, handler, sessionID,
		subtitleReplanTrackChangeV3(started, start, file.ID, 20, "subtitle-loop-0001"))
	if first.PlaybackPlan == nil {
		t.Fatalf("first replan returned no plan: %#v", first.Terminal)
	}
	if got := first.PlaybackPlan.Claims.Subtitles.Reason; got != playback.SubtitleReasonSelectionUnavailableV3 {
		t.Fatalf("first claims.subtitles.reason = %q, want %q", got, playback.SubtitleReasonSelectionUnavailableV3)
	}
	firstPlanID := first.PlaybackPlan.PlanID
	firstStart := first.PlaybackPlan.Timeline.PlayerStartSeconds
	firstSourceStart := first.PlaybackPlan.Timeline.SourceStartSeconds

	// The identical retry: same file, same raw subtitle identity. The stale
	// position echo must not move the plan either.
	repeat := subtitleReplanTrackChangeV3(first, start, file.ID, 20, "subtitle-loop-0002")
	repeat.PositionSeconds = 12
	second := postPlaybackReplanV3(t, handler, sessionID, repeat)
	if second.PlaybackPlan == nil {
		t.Fatalf("the repeated pick returned no plan: %#v", second.Terminal)
	}
	if got := second.PlaybackPlan.PlanID; got != firstPlanID {
		t.Fatalf("repeated pick minted a new plan %q, want the current plan %q", got, firstPlanID)
	}
	if got := second.PlaybackPlan.Timeline.PlayerStartSeconds; got != firstStart {
		t.Fatalf("repeated pick moved player_start_seconds to %v, want %v", got, firstStart)
	}
	if got := second.PlaybackPlan.Timeline.SourceStartSeconds; got != firstSourceStart {
		t.Fatalf("repeated pick moved source start to %v, want %v", got, firstSourceStart)
	}
	if second.PlaybackPlan.Timeline.SourceStartSeconds != livePosition {
		t.Fatalf("repeated pick reset the playhead to %v, want the live position %v",
			second.PlaybackPlan.Timeline.SourceStartSeconds, livePosition)
	}
	if got := second.PlaybackPlan.Claims.Subtitles.Reason; got != playback.SubtitleReasonSelectionUnavailableV3 {
		t.Fatalf("no-op claims.subtitles.reason = %q, want the original rejection %q", got, playback.SubtitleReasonSelectionUnavailableV3)
	}

	// A different unhonorable pick is a new intent and must still be answered
	// explicitly before it, too, becomes a no-op on repetition.
	third := postPlaybackReplanV3(t, handler, sessionID,
		subtitleReplanTrackChangeV3(second, start, file.ID, 21, "subtitle-loop-0003"))
	if third.PlaybackPlan == nil {
		t.Fatalf("a different unhonorable pick returned no plan: %#v", third.Terminal)
	}
	if got := third.PlaybackPlan.Claims.Subtitles.Reason; got != playback.SubtitleReasonSelectionUnavailableV3 {
		t.Fatalf("third claims.subtitles.reason = %q, want %q", got, playback.SubtitleReasonSelectionUnavailableV3)
	}
	// Two rejected picks with subtitles off can resolve to the same plan body,
	// so plan identity alone cannot tell a new rejection from a no-op. The
	// tracker must have moved to the new pick: the old one is no longer what a
	// repeat would fold, and the new one now is.
	key20 := subtitleSelectionTestKeyV3(file.ID, 20)
	key21 := subtitleSelectionTestKeyV3(file.ID, 21)
	if handler.subtitleSelectionBreaker.repeated(sessionID, key20, time.Now()) {
		t.Fatal("the previous pick must not remain the tracked rejection after a different pick")
	}
	if !handler.subtitleSelectionBreaker.repeated(sessionID, key21, time.Now()) {
		t.Fatal("a different unhonorable pick must be recorded as the new rejection, not treated as a no-op")
	}

	// Stopping the session drops the per-session entry.
	session, err := handler.sessionMgr.GetSession(sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if _, err := handler.endPlaybackSessionAuthority(context.Background(), session, true); err != nil {
		t.Fatalf("end session: %v", err)
	}
	if got := handler.subtitleSelectionBreaker.size(); got != 0 {
		t.Fatalf("subtitle loop breaker kept %d entries after the session ended, want 0", got)
	}
}

func TestSubtitleSelectionBreakerIsBoundedAndForgets(t *testing.T) {
	var breaker subtitleSelectionBreakerV3
	key := subtitleSelectionKeyV3{fileID: 7, trackID: playback.TrackIDV3(7, "subtitle", 3)}
	now := time.Now()

	breaker.record("session-a", key, now)
	if !breaker.repeated("session-a", key, now) {
		t.Fatal("a recorded pick must be recognized as a repeat")
	}
	breaker.forget("session-a")
	if breaker.repeated("session-a", key, now) {
		t.Fatal("a forgotten session must not match")
	}

	breaker.record("session-b", key, now.Add(-subtitleSelectionBreakerTTL-time.Minute))
	if breaker.repeated("session-b", key, now) {
		t.Fatal("an entry older than the TTL must not match")
	}
	if got := breaker.size(); got != 0 {
		t.Fatalf("expired entry was not dropped: size = %d", got)
	}

	for i := 0; i < subtitleSelectionBreakerMaxSessions+10; i++ {
		breaker.record(fmt.Sprintf("bounded-%d", i), key, now.Add(time.Duration(i)*time.Millisecond))
	}
	if got := breaker.size(); got > subtitleSelectionBreakerMaxSessions {
		t.Fatalf("breaker size = %d, want at most %d", got, subtitleSelectionBreakerMaxSessions)
	}
}

func subtitleSelectionTestKeyV3(fileID, ordinal int) subtitleSelectionKeyV3 {
	index := ordinal
	key, _ := subtitleSelectionKeyForRequestV3(fileID, &playback.TrackIdentityV3{
		ID:    playback.TrackIDV3(fileID, "subtitle", ordinal),
		Index: &index,
	})
	return key
}
