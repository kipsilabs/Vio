package handlers

import (
	"context"
	"testing"

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
	file.AudioTracks = file.AudioTracks[:2]
	file.SubtitleTracks = []models.SubtitleTrack{{Language: "en", Codec: "subrip"}}
	previous := selectionCarryFixtureSession(t, manager, file.ID)
	previous.SelectionOrigin = SelectionOriginExplicit
	previous.AudioTrackIndex = 5
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
