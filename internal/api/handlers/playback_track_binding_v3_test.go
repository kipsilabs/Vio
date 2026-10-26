package handlers

import (
	"context"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

func TestRekeyStartTrackIdentitiesAcceptsPlanEffectiveRowOnRestart(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	// The predecessor plan bound the concrete effective row 200 while the
	// client keeps re-sending the requested provider-neutral row 100.
	session, err := manager.StartSessionWithFilesContext(context.Background(), 1, "profile-1", 200, 100, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSessionWithFilesContext: %v", err)
	}
	if session.MediaFileID != 200 || session.RequestedMediaFileID != 100 {
		t.Fatalf("fixture session = effective %d requested %d, want 200/100", session.MediaFileID, session.RequestedMediaFileID)
	}
	requested := &models.MediaFile{ID: 100, ContentID: "movie-1", Container: "virtual", FilePath: "virtual://movie/tt?profile=hd"}
	effective := &models.MediaFile{ID: 200, ContentID: "movie-1", Container: "virtual", FilePath: "virtual://movie/tt?profile=hd&result=abc"}
	foreign := &models.MediaFile{ID: 300, ContentID: "movie-2", Container: "virtual", FilePath: "virtual://movie/other?profile=hd&result=zzz"}
	handler := &PlaybackHandler{
		sessionMgr:   manager,
		fileResolver: mapPlaybackFileResolver{files: map[int]*models.MediaFile{100: requested, 200: effective, 300: foreign}},
	}

	req := playback.StartRequestV3{
		FileID:          100,
		AudioTrackID:    playback.TrackIDV3(200, "audio", 1),
		SubtitleTrackID: playback.TrackIDV3(200, "subtitle", 2),
	}
	handler.rekeyStartTrackIdentitiesV3(context.Background(), &req, 1, "profile-1", "", "")

	if want := playback.TrackIDV3(100, "audio", 1); req.AudioTrackID != want {
		t.Fatalf("audio id = %q, want %q (requested row, plan ordinal)", req.AudioTrackID, want)
	}
	if want := playback.TrackIDV3(100, "subtitle", 2); req.SubtitleTrackID != want {
		t.Fatalf("subtitle id = %q, want %q (requested row, plan ordinal)", req.SubtitleTrackID, want)
	}
	if playback.StaleTrackIdentityV3("audio", req.FileID, req.AudioTrackID) {
		t.Fatal("the re-keyed audio identity is still stale against the requested row")
	}

	// A row naming a different release is not a restart echo and must stay
	// foreign so the stale-identity guard drops it.
	foreignReq := playback.StartRequestV3{FileID: 100, AudioTrackID: playback.TrackIDV3(300, "audio", 0)}
	handler.rekeyStartTrackIdentitiesV3(context.Background(), &foreignReq, 1, "profile-1", "", "")
	if foreignReq.AudioTrackID != playback.TrackIDV3(300, "audio", 0) {
		t.Fatalf("foreign audio id = %q, want it left untouched", foreignReq.AudioTrackID)
	}
	if !playback.StaleTrackIdentityV3("audio", foreignReq.FileID, foreignReq.AudioTrackID) {
		t.Fatal("a foreign-release identity was accepted as a restart echo")
	}
}

func TestRekeyReplanTrackIdentitiesAcceptsSessionEffectiveRow(t *testing.T) {
	effective := &models.MediaFile{ID: 200, ContentID: "movie-1", Container: "virtual", FilePath: "virtual://movie/tt?result=abc"}
	duplicate := &models.MediaFile{ID: 201, ContentID: "movie-1", Container: "virtual", FilePath: "virtual://movie/tt?result=abc"}
	foreign := &models.MediaFile{ID: 300, ContentID: "movie-1", Container: "virtual", FilePath: "virtual://movie/tt?result=zzz"}
	handler := &PlaybackHandler{
		fileResolver: mapPlaybackFileResolver{files: map[int]*models.MediaFile{200: effective, 201: duplicate, 300: foreign}},
	}
	start := playback.StartRequestV3{
		FileID:       200,
		AudioTrackID: playback.TrackIDV3(201, "audio", 1),
	}
	handler.rekeyReplanTrackIdentitiesV3(context.Background(), &start, effective, &playback.Session{MediaFileID: 201, RequestedMediaFileID: 100})

	if want := playback.TrackIDV3(200, "audio", 1); start.AudioTrackID != want {
		t.Fatalf("replan audio id = %q, want %q", start.AudioTrackID, want)
	}

	start.AudioTrackID = playback.TrackIDV3(300, "audio", 0)
	handler.rekeyReplanTrackIdentitiesV3(context.Background(), &start, effective, &playback.Session{MediaFileID: 201})
	if start.AudioTrackID != playback.TrackIDV3(300, "audio", 0) {
		t.Fatalf("foreign replan audio id = %q, want it left untouched", start.AudioTrackID)
	}
}

// Enrichment can re-probe a catalog row in place (same id, different release).
// A carried audio id that only names that row must be remapped from the
// evidence generation the selection was minted against, not reused by row-id
// equality against the rewritten layout.
func TestResolveCarriedAudioTrackRemapsSameRowAfterInPlaceReprobe(t *testing.T) {
	target := &models.MediaFile{ID: 77, ContentID: "movie-1", Container: "virtual", FilePath: "virtual://movie/tt?result=abc", AudioTracks: []models.AudioTrack{
		{Index: 0, Language: "en", Codec: "aac", Channels: 2, Default: true},
		{Index: 1, Language: "ja", Codec: "aac", Channels: 2},
	}}
	source := &models.MediaFile{ID: 77, AudioTracks: []models.AudioTrack{
		{Index: 0, Language: "ja", Codec: "aac", Channels: 2},
		{Index: 1, Language: "en", Codec: "aac", Channels: 2, Default: true},
	}}
	handler := &PlaybackHandler{}

	got, ok := handler.resolveCarriedAudioTrackV3(context.Background(), playback.TrackIDV3(77, "audio", 1), target, source)
	if !ok {
		t.Fatal("carried audio selection was dropped")
	}
	if got != 0 {
		t.Fatalf("carried audio index = %d, want 0 (the English track on the re-probed layout)", got)
	}

	same := &models.MediaFile{ID: 77, AudioTracks: append([]models.AudioTrack(nil), source.AudioTracks...)}
	got, ok = handler.resolveCarriedAudioTrackV3(context.Background(), playback.TrackIDV3(77, "audio", 1), same, source)
	if !ok || got != 1 {
		t.Fatalf("unchanged evidence: index=%d ok=%v, want the original ordinal 1", got, ok)
	}
}
