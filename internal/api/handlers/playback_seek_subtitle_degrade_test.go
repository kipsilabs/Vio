package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/subtitles"
)

// seekSubtitleSubstitutionV3 is the production shape behind the random-seek
// refusal: a session requested one edition, the provider substituted another,
// and the seek replays a frozen recipe whose downloaded subtitle still names
// the file the viewer originally asked for. The running plan's selected track
// is re-keyed onto the substituted row, so only the frozen identity is stale.
type seekSubtitleSubstitutionV3 struct {
	handler    *PlaybackHandler
	repo       *handlerMockSubtitleRepo
	source     *models.MediaFile
	substitute *models.MediaFile
	request    playback.StartRequestV3
	started    playback.DecisionResponseV3
	record     *playback.AttemptRecordV3
}

func newSeekSubtitleSubstitutionV3(t *testing.T) *seekSubtitleSubstitutionV3 {
	t.Helper()
	source := v3HandlerFixtureFile(t)
	substituteCopy := *source
	substitute := &substituteCopy
	substitute.ID = 84
	substitute.FilePath = writePlaybackTestMediaFile(t, "movie-1080p.mp4")

	repo := newMockSubtitleRepoForHandler()
	downloaded := subtitles.DownloadedSubtitle{ID: 71, MediaFileID: source.ID, Format: subtitles.FormatSRT, Language: "eng"}
	repo.list = []subtitles.DownloadedSubtitle{downloaded}
	repo.subtitles[downloaded.ID] = &downloaded

	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0), mapPlaybackFileResolver{files: map[int]*models.MediaFile{
		source.ID: source, substitute.ID: substitute,
	}})
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{}}
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	handler.SubtitleRepo = repo
	handler.PlaybackConfig = playbackTestConfig(writePlaybackTestFFmpeg(t), t.TempDir())

	request := v3HandlerStartRequest()
	downloadedIndex := 0
	request.SubtitleTrackIndex = &downloadedIndex
	request.SubtitleTrackID = playback.TrackIDV3(source.ID, "subtitle", downloadedIndex)

	rec := httptest.NewRecorder()
	handler.HandleStartPlayback(rec, httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", strings.NewReader(marshalV3StartRequest(t, request))).WithContext(newAuthorizedPlaybackContext()))
	var started playback.DecisionResponseV3
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &started) != nil || started.PlaybackPlan == nil ||
		started.PlaybackPlan.SelectedTracks.Subtitle == nil {
		t.Fatalf("start with the downloaded subtitle: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if started.PlaybackPlan.EffectiveMediaFileID != source.ID {
		t.Fatalf("start effective file = %d, want %d", started.PlaybackPlan.EffectiveMediaFileID, source.ID)
	}

	record, err := handler.PlanStoreV3.GetAttempt(context.Background(), started.SessionID)
	if err != nil {
		t.Fatalf("GetAttempt: %v", err)
	}
	record.EffectiveMediaFileID = substitute.ID
	record.CurrentPlan.EffectiveMediaFileID = substitute.ID
	rekeyed := *record.CurrentPlan.SelectedTracks.Subtitle
	rekeyed.ID = playback.TrackIDV3(substitute.ID, "subtitle", downloadedIndex)
	record.CurrentPlan.SelectedTracks.Subtitle = &rekeyed
	handler.PlanStoreV3.(*playback.MemoryPlanStoreV3).ReplaceAttempt(context.Background(), *record)

	return &seekSubtitleSubstitutionV3{
		handler: handler, repo: repo, source: source, substitute: substitute,
		request: request, started: started, record: record,
	}
}

func (fx *seekSubtitleSubstitutionV3) seek(t *testing.T, position float64, requestID string) playback.DecisionResponseV3 {
	t.Helper()
	current, err := fx.handler.PlanStoreV3.GetAttempt(context.Background(), fx.started.SessionID)
	if err != nil {
		t.Fatalf("GetAttempt: %v", err)
	}
	return postPlaybackReplanV3(t, fx.handler, fx.started.SessionID, playback.ReplanRequestV3{
		ProtocolVersion:       playback.ProtocolV3,
		Operation:             playback.ReplanOperationSeekReanchorV3,
		PlaybackAttemptID:     fx.request.PlaybackAttemptID,
		ReplanRequestID:       requestID,
		FailedPlanID:          current.CurrentPlan.PlanID,
		PlanAttemptID:         "seek-subtitle-" + requestID,
		PlanAttemptKey:        current.CurrentPlan.PlanAttemptKey,
		AttemptCount:          1,
		QualityPreference:     fx.request.QualityPreference,
		PositionSeconds:       position,
		SelectedTracks:        current.CurrentPlan.SelectedTracks,
		Capabilities:          fx.request.Capabilities,
		ClientPlaybackContext: fx.request.ClientPlaybackContext,
	})
}

// requireSeekMovedWithoutSubtitleV3 asserts the seek reached its target with
// video and audio intact and one recorded subtitle-unavailable warning.
func requireSeekMovedWithoutSubtitleV3(t *testing.T, response playback.DecisionResponseV3, effectiveFileID int, wantAudio *playback.TrackIdentityV3, position float64) {
	t.Helper()
	if response.Terminal != nil {
		t.Fatalf("seek terminaled: %+v", response.Terminal)
	}
	if response.PlaybackPlan == nil {
		t.Fatalf("seek returned no plan: %#v", response)
	}
	if response.Outcome != playback.OutcomePlayableV3 {
		t.Fatalf("seek outcome = %q, want %q", response.Outcome, playback.OutcomePlayableV3)
	}
	plan := response.PlaybackPlan
	if plan.EffectiveMediaFileID != effectiveFileID {
		t.Fatalf("seek effective file = %d, want %d", plan.EffectiveMediaFileID, effectiveFileID)
	}
	if plan.Timeline.SourceStartSeconds != position {
		t.Fatalf("seek timeline = %#v, want source start %v", plan.Timeline, position)
	}
	if plan.Subtitle.Mode != playback.SubtitleOffV3 || plan.SelectedTracks.Subtitle != nil {
		t.Fatalf("plan subtitle = %#v selected = %#v, want subtitles off", plan.Subtitle, plan.SelectedTracks.Subtitle)
	}
	if !sameTrackIdentityV3(plan.SelectedTracks.Audio, wantAudio) {
		t.Fatalf("seek audio = %#v, want the running selection %#v", plan.SelectedTracks.Audio, wantAudio)
	}
	warned := 0
	for _, warning := range plan.DegradationWarnings {
		if warning.Code == playback.DegradationWarningSubtitleTrackUnavailableV3 {
			warned++
		}
	}
	if warned != 1 {
		t.Fatalf("subtitle_track_unavailable reported %d times: %#v", warned, plan.DegradationWarnings)
	}
}

// A seek reanchor whose carried subtitle no longer binds to the substituted
// effective file must move, not terminal. The requested position outranks
// subtitle fidelity: the subtitle is dropped for the replan with the existing
// one-time warning, and video plus audio keep playing.
func TestHandleReplanPlaybackV3SeekReanchorDegradesUnbindableSubtitle(t *testing.T) {
	logs := captureHandlerLogs(t)
	fx := newSeekSubtitleSubstitutionV3(t)

	response := fx.seek(t, 300, "seek-subtitle-degrade-0001")
	requireSeekMovedWithoutSubtitleV3(t, response, fx.substitute.ID, fx.started.PlaybackPlan.SelectedTracks.Audio, 300)

	live, err := fx.handler.sessionMgr.GetSession(fx.started.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.Position != 300 {
		t.Fatalf("live position = %v, want the requested 300", live.Position)
	}

	// The durable recipe lost the subtitle identity with the plan, so the next
	// seek neither rediscovers the stale selection nor repeats the warning.
	second := fx.seek(t, 600, "seek-subtitle-degrade-0002")
	requireSeekMovedWithoutSubtitleV3(t, second, fx.substitute.ID, fx.started.PlaybackPlan.SelectedTracks.Audio, 600)
	if got := strings.Count(logs.String(), "seek reanchor subtitle no longer binds"); got != 1 {
		t.Fatalf("subtitle degrade logged %d times, want once:\n%s", got, logs.String())
	}
	record, err := fx.handler.PlanStoreV3.GetAttempt(context.Background(), fx.started.SessionID)
	if err != nil {
		t.Fatalf("GetAttempt: %v", err)
	}
	if record.FrozenRecipe.SubtitleSource != "" || record.FrozenRecipe.DownloadedSubtitleID != 0 || record.FrozenRecipe.SubtitleTrackIndex != -1 {
		t.Fatalf("durable recipe kept the dropped subtitle identity: %#v", record.FrozenRecipe)
	}
	if record.NormalizedRequest.SubtitleTrackID != "" || record.NormalizedRequest.SubtitleTrackIndex != nil {
		t.Fatalf("durable request kept the dropped subtitle selection: %#v", record.NormalizedRequest.SubtitleTrackID)
	}
}

// A subtitle store that fails mid-seek is a lost subtitle, not an unplayable
// position: the seek still reaches the target and records the warning once
// instead of terminal-failing the whole replan.
func TestHandleReplanPlaybackV3SeekReanchorSurvivesSubtitleStoreFailure(t *testing.T) {
	logs := captureHandlerLogs(t)
	fx := newSeekSubtitleSubstitutionV3(t)
	fx.repo.listErr = errors.New("subtitle store unavailable")

	response := fx.seek(t, 420, "seek-subtitle-outage-0001")
	requireSeekMovedWithoutSubtitleV3(t, response, fx.substitute.ID, fx.started.PlaybackPlan.SelectedTracks.Audio, 420)
	if got := strings.Count(logs.String(), "seek reanchor subtitle inventory is temporarily unavailable"); got != 1 {
		t.Fatalf("subtitle outage logged %d times, want once:\n%s", got, logs.String())
	}

	// Once the store recovers, the same position still moves and the dropped
	// selection is not resurrected from the durable request.
	fx.repo.listErr = nil
	second := fx.seek(t, 780, "seek-subtitle-outage-0002")
	requireSeekMovedWithoutSubtitleV3(t, second, fx.substitute.ID, fx.started.PlaybackPlan.SelectedTracks.Audio, 780)
	if got := strings.Count(logs.String(), "seek reanchor subtitle inventory is temporarily unavailable"); got != 1 {
		t.Fatalf("recovered seek re-logged the outage (%d lines):\n%s", got, logs.String())
	}
}

// Only the seek outcome degrades. A quality change and a track change still
// refuse a subtitle they cannot verify as retryable, so the viewer keeps the
// playing subtitle and can repeat the change once the store recovers.
func TestHandleReplanPlaybackV3NonSeekSubtitleOutageStillRefuses(t *testing.T) {
	fx := newSeekSubtitleSubstitutionV3(t)
	fx.repo.listErr = errors.New("subtitle store unavailable")

	replan := func(operation playback.ReplanOperationV3, requestID string) playback.DecisionResponseV3 {
		t.Helper()
		current, err := fx.handler.PlanStoreV3.GetAttempt(context.Background(), fx.started.SessionID)
		if err != nil {
			t.Fatalf("GetAttempt: %v", err)
		}
		return postPlaybackReplanV3(t, fx.handler, fx.started.SessionID, playback.ReplanRequestV3{
			ProtocolVersion:       playback.ProtocolV3,
			Operation:             operation,
			PlaybackAttemptID:     fx.request.PlaybackAttemptID,
			ReplanRequestID:       requestID,
			FailedPlanID:          current.CurrentPlan.PlanID,
			PlanAttemptID:         "non-seek-outage-" + requestID,
			PlanAttemptKey:        current.CurrentPlan.PlanAttemptKey,
			AttemptCount:          1,
			QualityPreference:     playback.QualityAutoV3,
			PositionSeconds:       180,
			SelectedTracks:        current.CurrentPlan.SelectedTracks,
			Capabilities:          fx.request.Capabilities,
			ClientPlaybackContext: fx.request.ClientPlaybackContext,
		})
	}

	for _, operation := range []playback.ReplanOperationV3{
		playback.ReplanOperationQualityChangeV3,
		playback.ReplanOperationTrackChangeV3,
	} {
		response := replan(operation, "non-seek-outage-"+string(operation))
		if response.PlaybackPlan != nil || response.Terminal == nil {
			t.Fatalf("%s = plan %#v terminal %#v, want a retryable subtitle refusal", operation, response.PlaybackPlan, response.Terminal)
		}
		if response.Terminal.Reason != subtitleUnavailableReasonV3 || !response.Terminal.Retryable {
			t.Fatalf("%s terminal = %#v, want retryable %s", operation, response.Terminal, subtitleUnavailableReasonV3)
		}
	}
}
