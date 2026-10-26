package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// When no catalog row names the bound candidate, the inventory must still
// describe the tracks the plan committed to rather than an empty placeholder.
func TestInventoryEffectiveFileFallsBackToSessionEvidence(t *testing.T) {
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	file := &models.MediaFile{ID: 900, ContentID: "movie-1", Container: "virtual", FilePath: "virtual://movie/tt?result=other"}
	audio := []models.AudioTrack{{Index: 0, Language: "en", Codec: "aac", Channels: 2}}
	subtitles := []models.SubtitleTrack{{Index: 0, Language: "en", Codec: "subrip"}}
	session := &playback.Session{
		ID:                         "session-1",
		VirtualSubtitleEvidenceSet: true,
		VirtualAudioTracks:         audio,
		VirtualSubtitleTracks:      subtitles,
	}

	got := handler.inventoryEffectiveFile(context.Background(), file, session, nil, "virtual://movie/tt?result=bound")
	if got == nil {
		t.Fatal("inventoryEffectiveFile returned nil")
	}
	if len(got.AudioTracks) != 1 || got.AudioTracks[0].Language != "en" {
		t.Fatalf("placeholder audio = %+v, want the session's carried evidence", got.AudioTracks)
	}
	if len(got.SubtitleTracks) != 1 || got.SubtitleTracks[0].Language != "en" {
		t.Fatalf("placeholder subtitles = %+v, want the session's carried evidence", got.SubtitleTracks)
	}
}

// A failed downloaded-subtitle extension read is a partial inventory, not a
// failed menu: the endpoint keeps the embedded/external rows and marks the read
// retryable instead of answering 503.
func TestPlaybackInventoryDegradesDownloadedSubtitleOutage(t *testing.T) {
	file := v3HandlerFixtureFile(t)
	file.SubtitleTracks = []models.SubtitleTrack{{Index: 0, Language: "en", Codec: "subrip"}}
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0), testPlaybackFileResolver{file: file})
	repo := newMockSubtitleRepoForHandler()
	repo.listErr = errors.New("downloaded subtitle store unavailable")
	handler.SubtitleRepo = repo
	session := &playback.Session{ID: "session-1", MediaFileID: file.ID}

	inventory, err := handler.playbackInventoryForFileV3(context.Background(), session, nil, file, "")
	if err != nil {
		t.Fatalf("playbackInventoryForFileV3 returned %v, want a degraded 200 inventory", err)
	}
	if inventory.InventoryStatus != probeOutcomePending {
		t.Fatalf("inventory_status = %q, want pending so the client retries the missing extension", inventory.InventoryStatus)
	}
	if len(inventory.SubtitleInventory) == 0 {
		t.Fatal("base embedded/external subtitle inventory was dropped with the failed extension")
	}
}

// A subtitle URL minted from the row the session captured evidence from stays
// admissible even when the row's path has moved, while an unrelated row still
// fails closed.
func TestAcceptedSubtitleSourceFileIDAdmitsEvidenceRow(t *testing.T) {
	const evidenceURI = "virtual://movie/tt?result=abc"
	handler := &StreamHandler{fileResolver: mapPlaybackFileResolver{files: map[int]*models.MediaFile{
		5382473: {ID: 5382473, Container: "virtual", FilePath: "virtual://movie/tt?result=moved"},
		7001:    {ID: 7001, Container: "virtual", FilePath: "virtual://movie/tt?result=other"},
	}}}
	session := &playback.Session{
		MediaFileID:                   4912233,
		RequestedMediaFileID:          4912233,
		VirtualSourceURI:              evidenceURI,
		VirtualSubtitleEvidenceURI:    evidenceURI,
		VirtualSubtitleEvidenceFileID: 5382473,
	}

	if id, err := handler.acceptedSubtitleSourceFileID(context.Background(), "5382473", session); err != nil || id != 5382473 {
		t.Fatalf("evidence row: id=%d err=%v, want the evidence row admitted", id, err)
	}
	if _, err := handler.acceptedSubtitleSourceFileID(context.Background(), "7001", session); err == nil {
		t.Fatal("an unrelated row was admitted into the virtual subtitle window")
	}
}
