package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
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
