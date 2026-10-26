package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/subtitles"
)

// The fallback overlay describes the bound candidate only while the
// session's evidence URI still names it. The matching setup serves the carried
// tracks; the mismatched setup (below) stays empty.
func TestInventoryEffectiveFileFallsBackToSessionEvidence(t *testing.T) {
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	const candidateURI = "virtual://movie/tt?result=bound"
	file := &models.MediaFile{ID: 900, ContentID: "movie-1", Container: "virtual", FilePath: "virtual://movie/tt?result=other"}
	audio := []models.AudioTrack{{Index: 0, Language: "en", Codec: "aac", Channels: 2}}
	subtitles := []models.SubtitleTrack{{Index: 0, Language: "en", Codec: "subrip"}}
	session := &playback.Session{
		ID:                         "session-1",
		VirtualSubtitleEvidenceSet: true,
		VirtualSubtitleEvidenceURI: candidateURI,
		VirtualAudioTracks:         audio,
		VirtualSubtitleTracks:      subtitles,
	}

	got := handler.inventoryEffectiveFile(context.Background(), file, session, nil, candidateURI)
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

// Evidence captured before a binding rotation belongs to the previous release.
// When no catalog row names the new bound candidate, the menu must stay the
// empty fail-closed placeholder rather than present the old release's tracks
// under the new identity.
func TestInventoryEffectiveFileRejectsMismatchedSessionEvidence(t *testing.T) {
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	const candidateURI = "virtual://movie/tt?result=new"
	file := &models.MediaFile{ID: 900, ContentID: "movie-1", Container: "virtual", FilePath: "virtual://movie/tt?result=other"}
	session := &playback.Session{
		ID:                         "session-1",
		VirtualSubtitleEvidenceSet: true,
		VirtualSubtitleEvidenceURI: "virtual://movie/tt?result=old",
		VirtualAudioTracks:         []models.AudioTrack{{Index: 0, Language: "en", Codec: "aac", Channels: 2}},
		VirtualSubtitleTracks:      []models.SubtitleTrack{{Index: 0, Language: "en", Codec: "subrip"}},
	}

	got := handler.inventoryEffectiveFile(context.Background(), file, session, nil, candidateURI)
	if got == nil {
		t.Fatal("inventoryEffectiveFile returned nil")
	}
	if len(got.AudioTracks) != 0 || len(got.SubtitleTracks) != 0 || len(got.ExternalSubtitles) != 0 {
		t.Fatalf("mismatched evidence was overlaid on the bound candidate: audio=%+v subtitles=%+v external=%+v",
			got.AudioTracks, got.SubtitleTracks, got.ExternalSubtitles)
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

// A recovered downloaded-subtitle extension must be observable through the
// ETag contract: the revision moves so a client holding the degraded menu can
// replace it instead of staying on a 304 forever.
func TestPlaybackInventoryRevisionChangesWhenDownloadedExtensionRecovers(t *testing.T) {
	file := v3HandlerFixtureFile(t)
	file.SubtitleTracks = []models.SubtitleTrack{{Index: 0, Language: "en", Codec: "subrip"}}
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0), testPlaybackFileResolver{file: file})
	repo := newMockSubtitleRepoForHandler()
	handler.SubtitleRepo = repo
	session := &playback.Session{ID: "session-1", MediaFileID: file.ID}

	repo.listErr = errors.New("downloaded subtitle store unavailable")
	degraded, err := handler.playbackInventoryForFileV3(context.Background(), session, nil, file, "")
	if err != nil {
		t.Fatalf("degraded inventory returned %v", err)
	}
	if degraded.InventoryStatus != probeOutcomePending {
		t.Fatalf("degraded inventory_status = %q, want pending", degraded.InventoryStatus)
	}

	downloaded := subtitles.DownloadedSubtitle{ID: 71, MediaFileID: file.ID, Format: subtitles.FormatSRT, Language: "eng"}
	repo.listErr = nil
	repo.list = []subtitles.DownloadedSubtitle{downloaded}
	recovered, err := handler.playbackInventoryForFileV3(context.Background(), session, nil, file, "")
	if err != nil {
		t.Fatalf("recovered inventory returned %v", err)
	}
	if recovered.InventoryStatus == probeOutcomePending {
		t.Fatalf("recovered inventory_status = %q, want a terminal status once the extension is read", recovered.InventoryStatus)
	}
	if recovered.InventoryRevision == degraded.InventoryRevision {
		t.Fatalf("inventory revision stayed %q after the downloaded extension recovered", recovered.InventoryRevision)
	}
	if len(recovered.SubtitleInventory) <= len(degraded.SubtitleInventory) {
		t.Fatalf("recovered subtitle inventory = %+v, want the downloaded row appended to the base inventory", recovered.SubtitleInventory)
	}
}

// The reverse transition must also move the revision: a healthy inventory whose
// downloaded extension becomes unreadable reports pending and a different
// revision, so a client holding the healthy ETag cannot keep the stale menu.
func TestPlaybackInventoryRevisionChangesWhenDownloadedExtensionDegrades(t *testing.T) {
	file := v3HandlerFixtureFile(t)
	file.SubtitleTracks = []models.SubtitleTrack{{Index: 0, Language: "en", Codec: "subrip"}}
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0), testPlaybackFileResolver{file: file})
	repo := newMockSubtitleRepoForHandler()
	handler.SubtitleRepo = repo
	session := &playback.Session{ID: "session-degrade", MediaFileID: file.ID}

	repo.list = []subtitles.DownloadedSubtitle{{ID: 71, MediaFileID: file.ID, Format: subtitles.FormatSRT, Language: "eng"}}
	healthy, err := handler.playbackInventoryForFileV3(context.Background(), session, nil, file, "")
	if err != nil {
		t.Fatalf("healthy inventory returned %v", err)
	}
	if healthy.InventoryStatus == probeOutcomePending {
		t.Fatalf("healthy inventory_status = %q, want a terminal status", healthy.InventoryStatus)
	}

	repo.listErr = errors.New("downloaded subtitle store unavailable")
	degraded, err := handler.playbackInventoryForFileV3(context.Background(), session, nil, file, "")
	if err != nil {
		t.Fatalf("degraded inventory returned %v", err)
	}
	if degraded.InventoryStatus != probeOutcomePending {
		t.Fatalf("degraded inventory_status = %q, want pending", degraded.InventoryStatus)
	}
	if degraded.InventoryRevision == healthy.InventoryRevision {
		t.Fatalf("inventory revision stayed %q after the downloaded extension became unreadable", degraded.InventoryRevision)
	}
	if len(degraded.SubtitleInventory) >= len(healthy.SubtitleInventory) {
		t.Fatalf("degraded subtitle inventory = %+v, want the downloaded row removed from the base", degraded.SubtitleInventory)
	}
}

// A subtitle URL minted from the row the session captured evidence from stays
// admissible only while that row still names the live bound candidate. Row ids
// are not content fingerprints: an in-place enrichment re-probe keeps the id
// while adopting a different result= path, and the old URL must then fail
// closed rather than serve or extract from the rewritten release.
func TestAcceptedSubtitleSourceFileIDRejectsEvidenceRowAdoptedAfterMint(t *testing.T) {
	const evidenceURI = "virtual://movie/tt?result=abc"
	row := &models.MediaFile{ID: 5382473, Container: "virtual", FilePath: evidenceURI}
	handler := &StreamHandler{fileResolver: mapPlaybackFileResolver{files: map[int]*models.MediaFile{
		5382473: row,
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
		t.Fatalf("evidence row before adoption: id=%d err=%v, want the evidence row admitted", id, err)
	}

	// The provider adopts the same row onto a different result= after mint.
	row.FilePath = "virtual://movie/tt?result=moved"
	if _, err := handler.acceptedSubtitleSourceFileID(context.Background(), "5382473", session); err == nil {
		t.Fatal("a row adopted to another result= after mint was admitted into the virtual subtitle window")
	}
	if _, err := handler.acceptedSubtitleSourceFileID(context.Background(), "7001", session); err == nil {
		t.Fatal("an unrelated row was admitted into the virtual subtitle window")
	}
}

// A stale evidence URI from a previous release must not widen the window after
// the session rotates: the fallback still requires the evidence URI to agree
// with the live binding.
func TestAcceptedSubtitleSourceFileIDRejectsStaleEvidenceURIFallback(t *testing.T) {
	const oldURI = "virtual://movie/tt?result=old"
	const newURI = "virtual://movie/tt?result=new"
	handler := &StreamHandler{fileResolver: mapPlaybackFileResolver{files: map[int]*models.MediaFile{
		5382473: {ID: 5382473, Container: "virtual", FilePath: oldURI},
	}}}
	session := &playback.Session{
		MediaFileID:                   4912233,
		RequestedMediaFileID:          4912233,
		VirtualSourceURI:              newURI,
		VirtualSubtitleEvidenceURI:    oldURI,
		VirtualSubtitleEvidenceFileID: 5382473,
	}

	if _, err := handler.acceptedSubtitleSourceFileID(context.Background(), "5382473", session); err == nil {
		t.Fatal("a row named only by the stale evidence URI was admitted after the binding rotated")
	}
}
