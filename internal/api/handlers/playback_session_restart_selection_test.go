package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/subtitles"
)

func selectionCarryFixtureSession(t *testing.T, manager *playback.SessionManager, fileID int) *playback.Session {
	t.Helper()
	session := manager.RegisterReconstructed(&playback.Session{
		ID:          "previous-session",
		UserID:      1,
		ProfileID:   "profile-1",
		MediaFileID: fileID,
		PlayMethod:  playback.PlayRemux,
	})
	return session
}

func selectionCarryFixtureFile() *models.MediaFile {
	return &models.MediaFile{
		ID:       91,
		FilePath: "/media/movie.mkv",
		AudioTracks: []models.AudioTrack{
			{Language: "ja", Codec: "aac"},
			{Language: "en", Codec: "aac"},
			{Language: "de", Codec: "aac"},
			{Language: "es", Codec: "aac"},
			{Language: "pt", Codec: "aac"},
			{Language: "fr", Codec: "aac"},
		},
		SubtitleTracks: []models.SubtitleTrack{
			{Language: "en", Codec: "subrip"},
			{Language: "fr", Codec: "subrip"},
		},
	}
}

// A new playback attempt for the same profile and file must carry the viewer's
// explicit audio choice and subtitle selection from the session it replaces.
// Production saw the restarted session plan audio index 0 after the viewer had
// chosen fr/index 5, which was audible as a spontaneous audio switch.
func TestCarrySelectionsFromActiveSessionPreservesExplicitTracks(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	file := selectionCarryFixtureFile()
	previous := selectionCarryFixtureSession(t, manager, file.ID)
	previous.SelectionOrigin = SelectionOriginExplicit
	previous.AudioTrackIndex = 5
	previous.VirtualAudioTracks = file.AudioTracks
	previous.SubtitleTrackIndex = 0
	previous.VirtualSubtitleTracks = []models.SubtitleTrack{{Language: "fr", Codec: "subrip"}}

	handler := NewPlaybackHandler(manager)
	request := playback.StartRequestV3{FileID: file.ID, ProfileID: "profile-1"}
	audioCarried, subtitleUnavailable, audioUnavailable := handler.carrySelectionsFromActiveSessionV3(
		context.Background(), 1, "profile-1", file, &request, "", "")

	if !audioCarried || audioUnavailable {
		t.Fatalf("audio carry = %v unavailable=%v, want the explicit track carried", audioCarried, audioUnavailable)
	}
	if request.AudioTrackIndex == nil || *request.AudioTrackIndex != 5 {
		t.Fatalf("carried audio index = %v, want 5", request.AudioTrackIndex)
	}
	if subtitleUnavailable {
		t.Fatal("subtitle carry reported unavailable for a track present on the new candidate")
	}
	if request.SubtitleTrackIndex == nil || *request.SubtitleTrackIndex != 1 {
		t.Fatalf("carried subtitle index = %v, want the fr track at published index 1", request.SubtitleTrackIndex)
	}
}

// A server-resolved auto audio pick is not an explicit viewer choice: a new
// start must be allowed to re-resolve it rather than inheriting a stale ordinal.
func TestCarrySelectionsDoesNotCarryAutoAudioChoice(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	file := selectionCarryFixtureFile()
	previous := selectionCarryFixtureSession(t, manager, file.ID)
	previous.SelectionOrigin = SelectionOriginAuto
	previous.AudioTrackIndex = 5

	handler := NewPlaybackHandler(manager)
	request := playback.StartRequestV3{FileID: file.ID, ProfileID: "profile-1"}
	audioCarried, _, audioUnavailable := handler.carrySelectionsFromActiveSessionV3(
		context.Background(), 1, "profile-1", file, &request, "", "")

	if audioCarried || audioUnavailable || request.AudioTrackIndex != nil {
		t.Fatalf("auto audio choice was carried: index=%v carried=%v unavailable=%v",
			request.AudioTrackIndex, audioCarried, audioUnavailable)
	}
}

// A second device on the same profile must not inherit another device's
// explicit picks just because it starts the same file.
func TestCarrySelectionsSkipsAnotherClient(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	file := selectionCarryFixtureFile()
	previous := selectionCarryFixtureSession(t, manager, file.ID)
	previous.SelectionOrigin = SelectionOriginExplicit
	previous.AudioTrackIndex = 5
	previous.SubtitleTrackIndex = 0
	previous.VirtualSubtitleTracks = []models.SubtitleTrack{{Language: "fr", Codec: "subrip"}}
	previous.ClientIP = "client-a"
	previous.ClientUserAgent = "agent-a"

	handler := NewPlaybackHandler(manager)
	request := playback.StartRequestV3{FileID: file.ID, ProfileID: "profile-1"}
	audioCarried, subtitleUnavailable, audioUnavailable := handler.carrySelectionsFromActiveSessionV3(
		context.Background(), 1, "profile-1", file, &request, "client-b", "agent-b")

	if audioCarried || audioUnavailable || subtitleUnavailable || request.AudioTrackIndex != nil || request.SubtitleTrackIndex != nil {
		t.Fatalf("another client's selections were carried: audio=%v subtitleUnavailable=%v audioUnavailable=%v req=%#v",
			audioCarried, subtitleUnavailable, audioUnavailable, request)
	}
}

// A selection the new candidate cannot honor must be reported, not silently
// replaced: the audio carry reports unavailable and the subtitle carry does not
// invent an ordinal.
func TestCarrySelectionsReportsUnavailableTracks(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	file := selectionCarryFixtureFile()
	previousAudioTracks := file.AudioTracks
	file.AudioTracks = file.AudioTracks[:2]
	file.SubtitleTracks = []models.SubtitleTrack{{Language: "en", Codec: "subrip"}}
	previous := selectionCarryFixtureSession(t, manager, file.ID)
	previous.SelectionOrigin = SelectionOriginExplicit
	previous.AudioTrackIndex = 5
	previous.VirtualAudioTracks = previousAudioTracks
	previous.SubtitleTrackIndex = 1
	previous.VirtualSubtitleTracks = []models.SubtitleTrack{{Language: "fr", Codec: "subrip"}}

	handler := NewPlaybackHandler(manager)
	request := playback.StartRequestV3{FileID: file.ID, ProfileID: "profile-1"}
	audioCarried, subtitleUnavailable, audioUnavailable := handler.carrySelectionsFromActiveSessionV3(
		context.Background(), 1, "profile-1", file, &request, "", "")

	if audioCarried || !audioUnavailable {
		t.Fatalf("out-of-range audio carry = carried:%v unavailable:%v, want unavailable", audioCarried, audioUnavailable)
	}
	if request.AudioTrackIndex != nil {
		t.Fatalf("out-of-range audio index was carried: %v", *request.AudioTrackIndex)
	}
	if !subtitleUnavailable {
		t.Fatal("missing subtitle language was not reported unavailable")
	}
	if request.SubtitleTrackIndex != nil {
		t.Fatalf("unmatched subtitle index was carried: %v", *request.SubtitleTrackIndex)
	}
}

// A candidate substitution or re-probe can reorder the new file's audio
// inventory. The previous ordinal is transport-space for the old source, so
// the carry must resolve the selected track's identity and find it in the new
// inventory rather than copying the number and silently changing language.
func TestCarrySelectionsMapsAudioByIdentityAcrossReorderedInventory(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	previousTracks := []models.AudioTrack{
		{Language: "ja", Codec: "aac", Layout: "stereo"},
		{Language: "en", Codec: "ac3", Layout: "5.1"},
		{Language: "fr", Codec: "aac", Layout: "stereo"},
	}
	file := &models.MediaFile{
		ID: 91,
		AudioTracks: []models.AudioTrack{
			{Language: "de", Codec: "aac", Layout: "stereo"},
			{Language: "ja", Codec: "aac", Layout: "stereo"},
			{Language: "en", Codec: "ac3", Layout: "5.1"},
			{Language: "fr", Codec: "aac", Layout: "stereo"},
		},
	}
	previous := selectionCarryFixtureSession(t, manager, file.ID)
	previous.SelectionOrigin = SelectionOriginExplicit
	previous.AudioTrackIndex = 1 // en/ac3/5.1 in the previous inventory
	previous.VirtualAudioTracks = previousTracks

	handler := NewPlaybackHandler(manager)
	request := playback.StartRequestV3{FileID: file.ID, ProfileID: "profile-1"}
	audioCarried, _, audioUnavailable := handler.carrySelectionsFromActiveSessionV3(
		context.Background(), 1, "profile-1", file, &request, "", "")

	if !audioCarried || audioUnavailable {
		t.Fatalf("audio carry = %v unavailable=%v, want the identity match", audioCarried, audioUnavailable)
	}
	if request.AudioTrackIndex == nil || *request.AudioTrackIndex != 2 {
		t.Fatalf("carried audio index = %v, want 2 (en/ac3/5.1 in the new inventory)", request.AudioTrackIndex)
	}
}

// Two tabs or two devices behind one NAT share IP and user agent. The legacy
// session fallback must treat that as ambiguous and carry nothing; otherwise
// one tab's explicit track silently replaces the other's.
func TestCarrySelectionsSkipsAmbiguousClientSessions(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	file := selectionCarryFixtureFile()
	for i, id := range []string{"previous-a", "previous-b"} {
		previous := manager.RegisterReconstructed(&playback.Session{
			ID:                 id,
			UserID:             1,
			ProfileID:          "profile-1",
			MediaFileID:        file.ID,
			SelectionOrigin:    SelectionOriginExplicit,
			AudioTrackIndex:    i,
			ClientIP:           "192.0.2.10",
			ClientUserAgent:    "same-agent",
			VirtualAudioTracks: file.AudioTracks,
		})
		if previous == nil {
			t.Fatal("failed to register fixture session")
		}
	}

	handler := NewPlaybackHandler(manager)
	request := playback.StartRequestV3{FileID: file.ID, ProfileID: "profile-1"}
	audioCarried, subtitleUnavailable, audioUnavailable := handler.carrySelectionsFromActiveSessionV3(
		context.Background(), 1, "profile-1", file, &request, "192.0.2.10", "same-agent")

	if audioCarried || audioUnavailable || subtitleUnavailable || request.AudioTrackIndex != nil {
		t.Fatalf("ambiguous client sessions were used for carry: audio=%v audioUnavailable=%v subtitleUnavailable=%v index=%v",
			audioCarried, audioUnavailable, subtitleUnavailable, request.AudioTrackIndex)
	}
}

// The explicit predecessor attempt id is resolved from the durable store, so a
// restart carries the viewer's selection even when the predecessor session ran
// on another replica or is already gone. Client identity is deliberately
// different here to prove it is not consulted.
func TestCarrySelectionsUsesExplicitPredecessorAttempt(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	priorFile := &models.MediaFile{
		ID: 91,
		AudioTracks: []models.AudioTrack{
			{Language: "ja", Codec: "aac", Layout: "stereo"},
			{Language: "en", Codec: "ac3", Layout: "5.1"},
			{Language: "fr", Codec: "aac", Layout: "stereo"},
		},
		SubtitleTracks: []models.SubtitleTrack{{Language: "fr", Codec: "subrip"}},
	}
	file := &models.MediaFile{
		ID: 91,
		AudioTracks: []models.AudioTrack{
			{Language: "de", Codec: "aac", Layout: "stereo"},
			{Language: "ja", Codec: "aac", Layout: "stereo"},
			{Language: "en", Codec: "ac3", Layout: "5.1"},
			{Language: "fr", Codec: "aac", Layout: "stereo"},
		},
		SubtitleTracks: []models.SubtitleTrack{
			{Language: "en", Codec: "subrip"},
			{Language: "fr", Codec: "subrip"},
		},
	}
	audioIndex := 2
	subtitleID := playback.TrackIDV3(priorFile.ID, "subtitle", 0)
	plan := playback.PlanV3{
		AudioTracks: playback.AudioInventoryV3(priorFile),
		Subtitle: playback.SubtitleDecisionV3{
			TrackID:   subtitleID,
			Inventory: playback.BuildSubtitleInventoryV3(priorFile, nil),
		},
	}
	plan.SelectedTracks.Audio = &playback.TrackIdentityV3{ID: playback.TrackIDV3(priorFile.ID, "audio", audioIndex), Index: &audioIndex}
	plan.SelectedTracks.Subtitle = &playback.TrackIdentityV3{ID: subtitleID}

	handler := NewPlaybackHandler(manager)
	record := playback.AttemptRecordV3{
		PlaybackAttemptID:    "attempt-predecessor-0001",
		SessionID:            "predecessor-session",
		UserID:               1,
		ProfileID:            "profile-1",
		RequestedMediaFileID: priorFile.ID,
		EffectiveMediaFileID: priorFile.ID,
		CurrentPlan:          plan,
		SelectionOrigin:      SelectionOriginExplicit,
		ExpiresAt:            time.Now().Add(time.Hour),
	}
	if err := handler.PlanStoreV3.SaveAttempt(context.Background(), record); err != nil {
		t.Fatalf("save predecessor attempt: %v", err)
	}

	request := playback.StartRequestV3{
		FileID:                    file.ID,
		ProfileID:                 "profile-1",
		PreviousPlaybackAttemptID: record.PlaybackAttemptID,
	}
	audioCarried, subtitleUnavailable, audioUnavailable := handler.carrySelectionsFromActiveSessionV3(
		context.Background(), 1, "profile-1", file, &request, "203.0.113.9", "another-agent")

	if !audioCarried || audioUnavailable {
		t.Fatalf("audio carry = %v unavailable=%v, want the predecessor's explicit track", audioCarried, audioUnavailable)
	}
	if request.AudioTrackIndex == nil || *request.AudioTrackIndex != 3 {
		t.Fatalf("carried audio index = %v, want 3 (fr in the new inventory)", request.AudioTrackIndex)
	}
	if subtitleUnavailable {
		t.Fatal("subtitle carry reported unavailable for the predecessor's fr track")
	}
	if request.SubtitleTrackIndex == nil || *request.SubtitleTrackIndex != 1 {
		t.Fatalf("carried subtitle index = %v, want 1", request.SubtitleTrackIndex)
	}
}

// Duplicate audio identities are ambiguous, not a first-match choice. Carrying
// either stream would silently move the transport onto a different one.
func TestCarrySelectionsFailsClosedOnDuplicateAudioIdentity(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	file := &models.MediaFile{
		ID: 91,
		AudioTracks: []models.AudioTrack{
			{Language: "en", Codec: "aac", Layout: "stereo"},
			{Language: "en", Codec: "aac", Layout: "stereo"},
		},
	}
	previous := selectionCarryFixtureSession(t, manager, file.ID)
	previous.SelectionOrigin = SelectionOriginExplicit
	previous.AudioTrackIndex = 0
	previous.VirtualAudioTracks = []models.AudioTrack{{Language: "en", Codec: "aac", Layout: "stereo"}}

	handler := NewPlaybackHandler(manager)
	request := playback.StartRequestV3{FileID: file.ID, ProfileID: "profile-1"}
	audioCarried, _, audioUnavailable := handler.carrySelectionsFromActiveSessionV3(
		context.Background(), 1, "profile-1", file, &request, "", "")

	if audioCarried || !audioUnavailable || request.AudioTrackIndex != nil {
		t.Fatalf("duplicate audio identity = carried:%v unavailable:%v index:%v, want unavailable",
			audioCarried, audioUnavailable, request.AudioTrackIndex)
	}
}

// The subtitle carry ignores authored titles, so two same-language/format/flags
// tracks are ambiguous. It must report unavailable rather than take the first.
func TestCarrySelectionsFailsClosedOnDuplicateSubtitleIdentity(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	file := &models.MediaFile{
		ID: 91,
		SubtitleTracks: []models.SubtitleTrack{
			{Language: "en", Codec: "subrip", Title: "Full"},
			{Language: "en", Codec: "subrip", Title: "Signs"},
		},
	}
	previous := selectionCarryFixtureSession(t, manager, file.ID)
	previous.SubtitleTrackIndex = 0
	previous.VirtualSubtitleTracks = []models.SubtitleTrack{{Language: "en", Codec: "subrip"}}

	handler := NewPlaybackHandler(manager)
	request := playback.StartRequestV3{FileID: file.ID, ProfileID: "profile-1"}
	_, subtitleUnavailable, _ := handler.carrySelectionsFromActiveSessionV3(
		context.Background(), 1, "profile-1", file, &request, "", "")

	if !subtitleUnavailable || request.SubtitleTrackIndex != nil {
		t.Fatalf("duplicate subtitle identity = unavailable:%v index:%v, want unavailable",
			subtitleUnavailable, request.SubtitleTrackIndex)
	}
}

// An external subtitle selected on the predecessor plan is carried by identity,
// not dropped because the session's transport index is -1 for non-embedded
// tracks.
func TestCarrySubtitleFromPlanCarriesExternalTrack(t *testing.T) {
	prior := &models.MediaFile{
		ID: 91,
		ExternalSubtitles: []models.ExternalSubtitle{
			{Language: "fr", Format: "srt", Path: "/subs/fr.srt"},
		},
	}
	file := &models.MediaFile{
		ID: 91,
		ExternalSubtitles: []models.ExternalSubtitle{
			{Language: "en", Format: "srt", Path: "/subs/en.srt"},
			{Language: "fr", Format: "srt", Path: "/subs/fr-commentary.srt"},
		},
	}
	index := 0
	plan := playback.PlanV3{
		SelectedTracks: playback.SelectedTracksV3{
			Subtitle: &playback.TrackIdentityV3{ID: playback.TrackIDV3(prior.ID, "subtitle", index), Index: &index},
		},
		Subtitle: playback.SubtitleDecisionV3{
			TrackID:   playback.TrackIDV3(prior.ID, "subtitle", index),
			Inventory: playback.BuildSubtitleInventoryV3(prior, nil),
		},
	}

	handler := &PlaybackHandler{}
	request := playback.StartRequestV3{FileID: file.ID}
	if unavailable := handler.carrySubtitleFromPlanV3(context.Background(), &request, plan, file); unavailable {
		t.Fatal("external subtitle carry reported unavailable for a track present on the target")
	}
	if request.SubtitleTrackIndex == nil || *request.SubtitleTrackIndex != 1 {
		t.Fatalf("carried subtitle index = %v, want the target's fr external track at 1", request.SubtitleTrackIndex)
	}
}

// A downloaded selection carries through its durable row id, never by the
// combined ordinal or a same-language sibling.
func TestCarrySubtitleFromPlanCarriesDownloadedTrack(t *testing.T) {
	prior := &models.MediaFile{ID: 91}
	file := &models.MediaFile{ID: 92}
	repo := downloadedSubtitleRepoByFile{byFile: map[int][]subtitles.DownloadedSubtitle{
		prior.ID: {{ID: 71, MediaFileID: prior.ID, Language: "eng", Format: subtitles.FormatSRT}},
		file.ID: {
			{ID: 72, MediaFileID: file.ID, Language: "eng", Format: subtitles.FormatSRT},
			{ID: 71, MediaFileID: file.ID, Language: "eng", Format: subtitles.FormatSRT},
		},
	}}
	index := 0
	plan := playback.PlanV3{
		SelectedTracks: playback.SelectedTracksV3{
			Subtitle: &playback.TrackIdentityV3{ID: playback.TrackIDV3(prior.ID, "subtitle", index), Index: &index},
		},
		Subtitle: playback.SubtitleDecisionV3{
			TrackID: playback.TrackIDV3(prior.ID, "subtitle", index),
			Inventory: playback.BuildSubtitleInventoryV3(prior, []playback.SubtitleInventoryEntryV3{{
				DownloadedSubtitleID: 71,
				Language:             "eng",
				Codec:                "srt",
				Source:               playback.SubtitleSourceDownloadedV3,
			}}),
		},
	}
	handler := &PlaybackHandler{SubtitleRepo: repo}
	request := playback.StartRequestV3{FileID: file.ID}
	if unavailable := handler.carrySubtitleFromPlanV3(context.Background(), &request, plan, file); unavailable {
		t.Fatal("downloaded subtitle carry reported unavailable for a row present on the target")
	}
	if request.SubtitleTrackIndex == nil || *request.SubtitleTrackIndex != 1 {
		t.Fatalf("carried subtitle index = %v, want the id-matched downloaded row at 1", request.SubtitleTrackIndex)
	}
}

// The legacy live-session fallback has no combined ordinal in the session
// (transport -1 covers every non-embedded selection), but the durable attempt
// does. It must carry an external selection instead of silently dropping it.
func TestCarrySelectionsUsesAttemptPlanForExternalSubtitle(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	prior := &models.MediaFile{
		ID: 91,
		ExternalSubtitles: []models.ExternalSubtitle{
			{Language: "fr", Format: "srt", Path: "/subs/fr.srt"},
		},
	}
	file := &models.MediaFile{
		ID: 91,
		ExternalSubtitles: []models.ExternalSubtitle{
			{Language: "en", Format: "srt", Path: "/subs/en.srt"},
			{Language: "fr", Format: "srt", Path: "/subs/fr.srt"},
		},
	}
	previous := selectionCarryFixtureSession(t, manager, file.ID)
	previous.SubtitleTrackIndex = -1
	index := 0
	plan := playback.PlanV3{
		SelectedTracks: playback.SelectedTracksV3{
			Subtitle: &playback.TrackIdentityV3{ID: playback.TrackIDV3(prior.ID, "subtitle", index), Index: &index},
		},
		Subtitle: playback.SubtitleDecisionV3{
			TrackID:   playback.TrackIDV3(prior.ID, "subtitle", index),
			Inventory: playback.BuildSubtitleInventoryV3(prior, nil),
		},
	}
	handler := NewPlaybackHandler(manager)
	handler.PlanStoreV3 = playback.NewMemoryPlanStoreV3()
	if err := handler.PlanStoreV3.SaveAttempt(context.Background(), playback.AttemptRecordV3{
		PlaybackAttemptID:    "attempt-external-subtitle",
		SessionID:            previous.ID,
		UserID:               previous.UserID,
		ProfileID:            previous.ProfileID,
		RequestedMediaFileID: prior.ID,
		EffectiveMediaFileID: prior.ID,
		CurrentPlan:          plan,
		SelectionOrigin:      SelectionOriginExplicit,
		ExpiresAt:            time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("save predecessor attempt: %v", err)
	}

	request := playback.StartRequestV3{FileID: file.ID, ProfileID: previous.ProfileID}
	_, subtitleUnavailable, _ := handler.carrySelectionsFromActiveSessionV3(
		context.Background(), previous.UserID, previous.ProfileID, file, &request, "", "")

	if subtitleUnavailable {
		t.Fatal("external subtitle carry reported unavailable for a track present on the target")
	}
	if request.SubtitleTrackIndex == nil || *request.SubtitleTrackIndex != 1 {
		t.Fatalf("carried subtitle index = %v, want the target's fr external track at 1", request.SubtitleTrackIndex)
	}
}

// A probed container index names a track only inside its own file. When the
// target inventory differs, carrying another version's index verbatim can land
// on a different language at the same position; the carry must remap by
// signature instead.
func TestCarrySelectionsIgnoresContainerIndexAcrossDifferentInventory(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	previousTracks := []models.AudioTrack{
		{Index: 0, Language: "ja", Codec: "aac", Layout: "stereo"},
		{Index: 1, Language: "en", Codec: "ac3", Layout: "5.1"},
	}
	file := &models.MediaFile{
		ID: 91,
		AudioTracks: []models.AudioTrack{
			{Index: 0, Language: "ja", Codec: "aac", Layout: "stereo"},
			{Index: 1, Language: "fr", Codec: "ac3", Layout: "5.1"},
			{Index: 2, Language: "en", Codec: "ac3", Layout: "5.1"},
		},
	}
	previous := selectionCarryFixtureSession(t, manager, file.ID)
	previous.SelectionOrigin = SelectionOriginExplicit
	previous.AudioTrackIndex = 1 // en/ac3/5.1 at container index 1 in the old file
	previous.VirtualAudioTracks = previousTracks

	handler := NewPlaybackHandler(manager)
	request := playback.StartRequestV3{FileID: file.ID, ProfileID: "profile-1"}
	audioCarried, _, audioUnavailable := handler.carrySelectionsFromActiveSessionV3(
		context.Background(), 1, "profile-1", file, &request, "", "")

	if !audioCarried || audioUnavailable {
		t.Fatalf("audio carry = %v unavailable=%v, want the signature match", audioCarried, audioUnavailable)
	}
	if request.AudioTrackIndex == nil || *request.AudioTrackIndex != 2 {
		t.Fatalf("carried audio index = %v, want 2 (en at container index 2, not fr at 1)", request.AudioTrackIndex)
	}
}
