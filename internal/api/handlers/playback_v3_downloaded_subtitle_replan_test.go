package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/subtitles"
)

// downloadedSubtitleReplanFixtureV3 starts a session on a file with one
// embedded English subtitle plus the given downloaded rows, with startOrdinal
// selected. The downloaded rows follow the file's own inventory, so the first
// one is at ordinal 1.
func downloadedSubtitleReplanFixtureV3(t *testing.T, startOrdinal int, rows ...subtitles.DownloadedSubtitle) (*PlaybackHandler, *handlerMockSubtitleRepo, *models.MediaFile, playback.StartRequestV3, playback.DecisionResponseV3) {
	t.Helper()
	file := v3HandlerFixtureFile(t)
	file.SubtitleTracks = []models.SubtitleTrack{{Index: 0, Language: "eng", Codec: "subrip"}}
	repo := newMockSubtitleRepoForHandler()
	setDownloadedSubtitleRowsV3(repo, rows...)
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0), testPlaybackFileResolver{file: file})
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{}}
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	handler.SubtitleRepo = repo
	request := v3HandlerStartRequest()
	request.SubtitleTrackIndex = &startOrdinal
	request.SubtitleTrackID = playback.TrackIDV3(file.ID, "subtitle", startOrdinal)
	rec := httptest.NewRecorder()
	handler.HandleStartPlayback(rec, httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", strings.NewReader(marshalV3StartRequest(t, request))).WithContext(newAuthorizedPlaybackContext()))
	var started playback.DecisionResponseV3
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &started) != nil || started.PlaybackPlan == nil || started.PlaybackPlan.SelectedTracks.Subtitle == nil {
		t.Fatalf("start with the downloaded subtitle: status=%d body=%s", rec.Code, rec.Body.String())
	}
	return handler, repo, file, request, started
}

// setDownloadedSubtitleRowsV3 publishes rows as the repository's current list
// and makes each one readable by id, the way a downloaded row that the viewer
// can select is stored.
func setDownloadedSubtitleRowsV3(repo *handlerMockSubtitleRepo, rows ...subtitles.DownloadedSubtitle) {
	repo.list = append([]subtitles.DownloadedSubtitle(nil), rows...)
	repo.subtitles = make(map[int]*subtitles.DownloadedSubtitle, len(rows))
	repo.byKey = make(map[string]*subtitles.DownloadedSubtitle, len(rows))
	for i := range rows {
		copied := rows[i]
		repo.subtitles[copied.ID] = &copied
	}
}

// selectedDownloadedSubtitleRowV3 returns the durable downloaded-subtitle row
// the plan's selected subtitle ordinal names: 0 for subtitles off and -1 for an
// embedded or external pick. The published inventory, not the ordinal alone,
// decides which row the viewer is actually served.
func selectedDownloadedSubtitleRowV3(t *testing.T, plan *playback.PlanV3) int {
	t.Helper()
	if plan == nil || plan.SelectedTracks.Subtitle == nil {
		return 0
	}
	_, kind, ordinal, ok := playback.ParseTrackIDV3(plan.SelectedTracks.Subtitle.ID)
	if !ok || kind != subtitleTrackKindV3 {
		t.Fatalf("selected subtitle identity = %q, want a subtitle track id", plan.SelectedTracks.Subtitle.ID)
	}
	for _, item := range plan.Subtitle.Inventory {
		if item.CombinedIndex != ordinal {
			continue
		}
		if item.Source != playback.SubtitleSourceDownloadedV3 {
			return -1
		}
		return downloadedSubtitleCarryIDV3(item)
	}
	t.Fatalf("selected subtitle ordinal %d is absent from the published inventory %+v", ordinal, plan.Subtitle.Inventory)
	return 0
}

// A carried downloaded-subtitle selection is reconciled once. The drift remap
// already moves the ordinal into the current inventory when the base track count
// changes; re-reading the result against the plan-time downloaded tail points
// it at whichever row used to occupy that ordinal instead.
func TestHandleReplanPlaybackV3KeepsDownloadedSubtitleAcrossBaseTrackDrift(t *testing.T) {
	handler, _, file, startRequest, started := downloadedSubtitleReplanFixtureV3(t, 1,
		subtitles.DownloadedSubtitle{ID: 71, MediaFileID: 42, Format: subtitles.FormatSRT, Language: "eng"},
		subtitles.DownloadedSubtitle{ID: 72, MediaFileID: 42, Format: subtitles.FormatSRT, Language: "jpn"},
	)
	// The row was re-probed in place: one more embedded subtitle pushes the
	// downloaded tail from ordinals 1..2 to 2..3.
	file.SubtitleTracks = append(file.SubtitleTracks, models.SubtitleTrack{Index: 1, Language: "fra", Codec: "subrip"})
	current := started.PlaybackPlan
	response := postPlaybackReplanV3(t, handler, started.SessionID, playback.ReplanRequestV3{
		ProtocolVersion: playback.ProtocolV3, Operation: playback.ReplanOperationQualityChangeV3,
		PlaybackAttemptID: startRequest.PlaybackAttemptID, ReplanRequestID: "downloaded-base-drift-0001",
		FailedPlanID: current.PlanID, PlanAttemptID: "downloaded-base-drift-attempt-0001",
		PlanAttemptKey: current.PlanAttemptKey, AttemptCount: 1, PositionSeconds: 30,
		QualityPreference: "auto",
		// A quality change may omit the unchanged subtitle; the server carries
		// it over from the current plan.
		SelectedTracks: playback.SelectedTracksV3{Audio: current.SelectedTracks.Audio},
		Capabilities:   startRequest.Capabilities, ClientPlaybackContext: startRequest.ClientPlaybackContext,
	})
	if response.PlaybackPlan == nil || response.Terminal != nil {
		t.Fatalf("quality change = plan %#v terminal %#v", response.PlaybackPlan, response.Terminal)
	}
	if row := selectedDownloadedSubtitleRowV3(t, response.PlaybackPlan); row != 71 {
		t.Fatalf("replan selected downloaded row %d, want 71 (English); subtitle=%+v inventory=%+v",
			row, response.PlaybackPlan.SelectedTracks.Subtitle, response.PlaybackPlan.Subtitle.Inventory)
	}
}

// A track_change selection is minted against the current menu, not the plan
// that was replanned. When a downloaded row is inserted ahead of the carried
// pick, the requested ordinal names the inserted row and must not be
// reinterpreted through the predecessor's tail as the old occupant.
func TestHandleReplanPlaybackV3TrackChangeSelectsDownloadedRowAfterListInsertion(t *testing.T) {
	handler, repo, file, startRequest, started := downloadedSubtitleReplanFixtureV3(t, 1,
		subtitles.DownloadedSubtitle{ID: 71, MediaFileID: 42, Format: subtitles.FormatSRT, Language: "eng"},
	)
	_ = file
	// The user downloaded a Japanese subtitle; the refreshed menu lists it
	// first, so ordinal 1 now names Japanese and English moved to 2.
	setDownloadedSubtitleRowsV3(repo,
		subtitles.DownloadedSubtitle{ID: 72, MediaFileID: 42, Format: subtitles.FormatSRT, Language: "jpn"},
		subtitles.DownloadedSubtitle{ID: 71, MediaFileID: 42, Format: subtitles.FormatSRT, Language: "eng"},
	)
	current := started.PlaybackPlan
	requested := 1
	response := postPlaybackReplanV3(t, handler, started.SessionID, playback.ReplanRequestV3{
		ProtocolVersion: playback.ProtocolV3, Operation: playback.ReplanOperationTrackChangeV3,
		PlaybackAttemptID: startRequest.PlaybackAttemptID, ReplanRequestID: "downloaded-list-insert-0001",
		FailedPlanID: current.PlanID, PlanAttemptID: "downloaded-list-insert-attempt-0001",
		PlanAttemptKey: current.PlanAttemptKey, AttemptCount: 1, PositionSeconds: 30,
		QualityPreference: "original",
		SelectedTracks: playback.SelectedTracksV3{
			Audio:    current.SelectedTracks.Audio,
			Subtitle: &playback.TrackIdentityV3{ID: playback.TrackIDV3(42, "subtitle", requested), Index: &requested},
		},
		Capabilities: startRequest.Capabilities, ClientPlaybackContext: startRequest.ClientPlaybackContext,
	})
	if response.PlaybackPlan == nil || response.Terminal != nil {
		t.Fatalf("track change = plan %#v terminal %#v", response.PlaybackPlan, response.Terminal)
	}
	if row := selectedDownloadedSubtitleRowV3(t, response.PlaybackPlan); row != 72 {
		t.Fatalf("track change selected downloaded row %d, want 72 (Japanese); subtitle=%+v inventory=%+v",
			row, response.PlaybackPlan.SelectedTracks.Subtitle, response.PlaybackPlan.Subtitle.Inventory)
	}
}

// The same holds for a row appended past the plan-time tail: the predecessor
// evidence never published that ordinal, so applying it clears a selection the
// viewer just made from the current menu.
func TestHandleReplanPlaybackV3TrackChangeSelectsAppendedDownloadedRow(t *testing.T) {
	handler, repo, _, startRequest, started := downloadedSubtitleReplanFixtureV3(t, 1,
		subtitles.DownloadedSubtitle{ID: 71, MediaFileID: 42, Format: subtitles.FormatSRT, Language: "eng"},
	)
	setDownloadedSubtitleRowsV3(repo,
		subtitles.DownloadedSubtitle{ID: 71, MediaFileID: 42, Format: subtitles.FormatSRT, Language: "eng"},
		subtitles.DownloadedSubtitle{ID: 72, MediaFileID: 42, Format: subtitles.FormatSRT, Language: "jpn"},
	)
	current := started.PlaybackPlan
	requested := 2
	response := postPlaybackReplanV3(t, handler, started.SessionID, playback.ReplanRequestV3{
		ProtocolVersion: playback.ProtocolV3, Operation: playback.ReplanOperationTrackChangeV3,
		PlaybackAttemptID: startRequest.PlaybackAttemptID, ReplanRequestID: "downloaded-list-append-0001",
		FailedPlanID: current.PlanID, PlanAttemptID: "downloaded-list-append-attempt-0001",
		PlanAttemptKey: current.PlanAttemptKey, AttemptCount: 1, PositionSeconds: 30,
		QualityPreference: "original",
		SelectedTracks: playback.SelectedTracksV3{
			Audio:    current.SelectedTracks.Audio,
			Subtitle: &playback.TrackIdentityV3{ID: playback.TrackIDV3(42, "subtitle", requested), Index: &requested},
		},
		Capabilities: startRequest.Capabilities, ClientPlaybackContext: startRequest.ClientPlaybackContext,
	})
	if response.PlaybackPlan == nil || response.Terminal != nil {
		t.Fatalf("track change = plan %#v terminal %#v", response.PlaybackPlan, response.Terminal)
	}
	if row := selectedDownloadedSubtitleRowV3(t, response.PlaybackPlan); row != 72 {
		t.Fatalf("track change selected downloaded row %d, want 72 (Japanese); subtitle=%+v inventory=%+v",
			row, response.PlaybackPlan.SelectedTracks.Subtitle, response.PlaybackPlan.Subtitle.Inventory)
	}
}

// A track_change can name a row other than the one the replan validates
// against: the live menu publishes the requested row while the session is
// served by its effective row. The fresh ordinal belongs to the row that
// published it, so a downloaded row inserted ahead of the captured pick must be
// mapped by identity onto the effective row instead of being read through the
// predecessor tail.
func TestHandleReplanPlaybackV3TrackChangeCrossRowDownloadedInsertion(t *testing.T) {
	const publishedRowID, effectiveRowID = 201, 200
	// Downloaded rows are file-bound, so each catalog row owns its own ids.
	const publishedEnglishRowID, publishedJapaneseRowID = 171, 172
	const effectiveEnglishRowID, effectiveJapaneseRowID = 71, 72

	published := v3HandlerFixtureFile(t)
	published.ID = publishedRowID
	published.ContentID = "movie-cross-row-downloaded"
	published.FilePath = "virtual://movie/tt-cross-row-downloaded?result=cand"
	published.VirtualOwnerInstallationID = 5
	published.SubtitleTracks = []models.SubtitleTrack{{Index: 0, Language: "eng", Codec: "subrip"}}
	probedAt := time.Now().Add(-time.Hour)
	published.ProbeUpdatedAt = &probedAt

	repo := downloadedSubtitleRepoByFile{byFile: map[int][]subtitles.DownloadedSubtitle{
		publishedRowID: {{ID: publishedEnglishRowID, MediaFileID: publishedRowID, Language: "eng", Format: subtitles.FormatSRT}},
	}}
	files := map[int]*models.MediaFile{publishedRowID: published}
	manager := playback.NewSessionManager(0, 0)
	handler := NewPlaybackHandler(manager, mapPlaybackFileResolver{files: files})
	stubCopySeekAnchorV3(handler)
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"allow_4k_transcode": "true"}}
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	handler.SubtitleRepo = repo
	handler.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
		return "http://127.0.0.1:9/stream?path=" + path, nil
	})

	start := v3HandlerStartRequest()
	start.FileID = publishedRowID
	start.QualityPreference = "auto"
	direct := start.ClientPlaybackContext.Deliveries[playback.DeliveryClassOriginalHTTPV3]
	direct.ValidatedClaims = append(direct.ValidatedClaims, playback.ClaimClientSelectedAudioTrackV3)
	start.ClientPlaybackContext.Deliveries[playback.DeliveryClassOriginalHTTPV3] = direct
	start.ClientPlaybackContext.Deliveries[playback.DeliveryClassHLSV3] = playback.DeliveryCapabilityV3{
		Enabled: true, SupportedOnDevice: true, Containers: []string{"hls"}, VideoCodecs: []string{"h264"}, AudioDecodeCodecs: []string{"aac"},
	}
	start.ClientPlaybackContext.Deliveries[playback.DeliveryClassProgressiveV3] = playback.DeliveryCapabilityV3{
		Enabled: true, SupportedOnDevice: true, Containers: []string{"mp4"}, VideoCodecs: []string{"h264"}, AudioDecodeCodecs: []string{"aac"},
	}
	startedOrdinal := 1
	start.SubtitleTrackIndex = &startedOrdinal
	start.SubtitleTrackID = playback.TrackIDV3(publishedRowID, "subtitle", 1)
	started := startV3PlaybackForHandlerTest(t, handler, start)

	// The replan validates the session's effective row, which is a distinct
	// catalog row with the same release bytes. The session and menu still name
	// the requested row.
	effective := *published
	effective.ID = effectiveRowID
	files[effectiveRowID] = &effective
	store, ok := handler.PlanStoreV3.(*playback.MemoryPlanStoreV3)
	if !ok {
		t.Fatalf("plan store = %T, want *playback.MemoryPlanStoreV3", handler.PlanStoreV3)
	}
	record, err := store.GetAttempt(context.Background(), started.SessionID)
	if err != nil {
		t.Fatalf("GetAttempt: %v", err)
	}
	record.EffectiveMediaFileID = effectiveRowID
	store.ReplaceAttempt(context.Background(), *record)

	// A Japanese subtitle is downloaded and sorted ahead of the captured
	// English row in the published menu. The effective row keeps English first.
	repo.byFile[publishedRowID] = []subtitles.DownloadedSubtitle{
		{ID: publishedJapaneseRowID, MediaFileID: publishedRowID, Language: "jpn", Format: subtitles.FormatSRT},
		{ID: publishedEnglishRowID, MediaFileID: publishedRowID, Language: "eng", Format: subtitles.FormatSRT},
	}
	repo.byFile[effectiveRowID] = []subtitles.DownloadedSubtitle{
		{ID: effectiveEnglishRowID, MediaFileID: effectiveRowID, Language: "eng", Format: subtitles.FormatSRT},
		{ID: effectiveJapaneseRowID, MediaFileID: effectiveRowID, Language: "jpn", Format: subtitles.FormatSRT},
	}

	current := started.PlaybackPlan
	requested := 1
	response := postPlaybackReplanV3(t, handler, started.SessionID, playback.ReplanRequestV3{
		ProtocolVersion: playback.ProtocolV3, Operation: playback.ReplanOperationTrackChangeV3,
		PlaybackAttemptID: start.PlaybackAttemptID, ReplanRequestID: "downloaded-cross-row-insert-0001",
		FailedPlanID: current.PlanID, PlanAttemptID: "downloaded-cross-row-insert-attempt-0001",
		PlanAttemptKey: current.PlanAttemptKey, AttemptCount: 1, PositionSeconds: 30,
		QualityPreference: "original",
		SelectedTracks: playback.SelectedTracksV3{
			Audio:    current.SelectedTracks.Audio,
			Subtitle: &playback.TrackIdentityV3{ID: playback.TrackIDV3(publishedRowID, "subtitle", requested), Index: &requested},
		},
		Capabilities: start.Capabilities, ClientPlaybackContext: start.ClientPlaybackContext,
	})
	if response.PlaybackPlan == nil || response.Terminal != nil {
		t.Fatalf("cross-row track change = plan %#v terminal %#v", response.PlaybackPlan, response.Terminal)
	}
	if row := selectedDownloadedSubtitleRowV3(t, response.PlaybackPlan); row != effectiveJapaneseRowID {
		t.Fatalf("cross-row track change selected downloaded row %d, want %d (Japanese); subtitle=%+v inventory=%+v",
			row, effectiveJapaneseRowID, response.PlaybackPlan.SelectedTracks.Subtitle, response.PlaybackPlan.Subtitle.Inventory)
	}
}
