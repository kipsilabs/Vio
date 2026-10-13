package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// A session that planned against a probe-stamped row captures that row's
// inventory as its carried evidence. When a later enumeration replaces the row's
// tracks in place, the bound row's committed inventory is the truth for the
// release and must be served; the plan-time overlay must not mask it for the rest
// of the session.
func TestBindVirtualSourceServesVerifiedRowTracksOverFrozenOverlay(t *testing.T) {
	uri := "virtual://movie/tt-freeze?result=cand-1"
	probedAt := time.Now().UTC()
	row := &models.MediaFile{
		ID:       5272900,
		FilePath: uri,
		AudioTracks: []models.AudioTrack{
			{Index: 1, Codec: "eac3", Channels: 6, Language: "eng", Default: true},
			{Index: 2, Codec: "aac", Channels: 2, Language: "eng"},
		},
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 4, Codec: "subrip", Language: "eng"},
			{Index: 7, Codec: "subrip", Language: "fra"},
		},
		ProbeUpdatedAt: &probedAt,
	}
	session := &playback.Session{
		ID:                            "sess-freeze",
		MediaFileID:                   row.ID,
		VirtualSourceURI:              uri,
		VirtualSubtitleEvidenceSet:    true,
		VirtualSubtitleEvidenceURI:    uri,
		VirtualSubtitleEvidenceFileID: row.ID,
		// The plan-time overlay: one provisional audio entry, no subtitles.
		VirtualAudioTracks: []models.AudioTrack{{Index: 1, Codec: "eac3", Channels: 6, Language: "eng"}},
	}

	bound := bindSessionVirtualSourceWithTracks(context.Background(), row, session, nil)
	if len(bound.AudioTracks) != 2 {
		t.Fatalf("bound audio = %+v, want the verified row's 2 tracks", bound.AudioTracks)
	}
	if len(bound.SubtitleTracks) != 2 {
		t.Fatalf("bound subtitles = %+v, want the verified row's 2 tracks", bound.SubtitleTracks)
	}
}

// While the bound row is still unverified, the session's plan-time overlay stays
// authoritative: the declared menu the plan promised must still be served.
func TestBindVirtualSourceKeepsOverlayWhileRowUnverified(t *testing.T) {
	uri := "virtual://movie/tt-freeze-unverified?result=cand-1"
	row := &models.MediaFile{ID: 5272901, FilePath: uri}
	session := &playback.Session{
		ID:                            "sess-freeze-unverified",
		MediaFileID:                   row.ID,
		VirtualSourceURI:              uri,
		VirtualSubtitleEvidenceSet:    true,
		VirtualSubtitleEvidenceURI:    uri,
		VirtualSubtitleEvidenceFileID: row.ID,
		VirtualAudioTracks:            []models.AudioTrack{{Index: 1, Codec: "eac3", Channels: 6, Language: "jpn"}},
		VirtualSubtitleTracks:         []models.SubtitleTrack{{Index: 1, Codec: "subrip", Language: "fra"}},
	}

	bound := bindSessionVirtualSourceWithTracks(context.Background(), row, session, nil)
	if len(bound.AudioTracks) != 1 || bound.AudioTracks[0].Language != "jpn" {
		t.Fatalf("unverified overlay audio = %+v, want the carried jpn track", bound.AudioTracks)
	}
	if len(bound.SubtitleTracks) != 1 || bound.SubtitleTracks[0].Language != "fra" {
		t.Fatalf("unverified overlay subtitles = %+v, want the carried fra track", bound.SubtitleTracks)
	}
}

// The end-to-end acceptance: a cold session whose plan captured a probe-stamped
// row's provisional 1-audio/0-subtitle overlay must, once a later probe commits
// the true 2-audio/N-subtitle inventory to the same row, return that inventory
// from the same session's GET /inventory — verified status and a new revision —
// with no restart or resume.
func TestSessionInventoryServesVerifiedRowTracksInSameSession(t *testing.T) {
	const fileID = 5272902
	uri := "virtual://movie/tt-freeze-inv?result=cand-1"
	provisionalAt := time.Now().UTC().Add(-time.Minute)
	probedAt := time.Now().UTC()

	// The row as the plan saw it: probe-stamped with a partial enumeration (one
	// audio track, no subtitles).
	row := models.MediaFile{
		ID:                         fileID,
		ContentID:                  "movie-freeze-inv",
		FilePath:                   uri,
		VirtualOwnerInstallationID: 5,
		AudioTracks:                []models.AudioTrack{{Index: 1, Codec: "eac3", Channels: 6, Language: "eng", Default: true}},
		ProbeUpdatedAt:             &provisionalAt,
	}
	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", fileID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := manager.SetVirtualSource(session.ID, uri, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	// Capture the plan-time overlay the way the plan commit does for a stamped
	// row: the audio inventory travels with the evidence-set flag.
	if err := manager.UpdateStreamState(session.ID, playback.SessionStreamState{
		VirtualSourceSet:              true,
		VirtualSourceOwnershipSet:     true,
		VirtualSourceURI:              uri,
		VirtualSubtitleEvidenceSet:    true,
		VirtualSubtitleEvidenceURI:    uri,
		VirtualSubtitleEvidenceFileID: fileID,
		VirtualAudioTracks:            []models.AudioTrack{{Index: 1, Codec: "eac3", Channels: 6, Language: "eng"}},
	}); err != nil {
		t.Fatalf("UpdateStreamState: %v", err)
	}

	resolver := &syncPlaybackFileResolver{file: row}
	handler := NewPlaybackHandler(manager, resolver)
	handler.InstallationID = "test-install"
	serviceCtx, serviceCancel := context.WithCancel(context.Background())
	handler.ServiceContext = serviceCtx
	t.Cleanup(serviceCancel)
	if err := handler.PlanStoreV3.SaveAttempt(context.Background(), playback.AttemptRecordV3{
		PlaybackAttemptID: "attempt-freeze-inv", SessionID: session.ID, UserID: 1, ProfileID: "profile-1",
		RequestedMediaFileID: fileID, EffectiveMediaFileID: fileID, ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("SaveAttempt: %v", err)
	}

	caller := PlaybackCaller{UserID: 1, ProfileID: "profile-1", InstallationID: "test-install"}
	before, err := handler.GetPlaybackInventoryV2(newAuthorizedPlaybackContext(), caller, session.ID)
	if err != nil {
		t.Fatalf("GetPlaybackInventoryV2 before: %v", err)
	}
	if len(before.AudioTracks) != 1 || len(before.SubtitleInventory) != 0 {
		t.Fatalf("provisional inventory = %d audio / %d subtitles, want the captured 1/0 overlay", len(before.AudioTracks), len(before.SubtitleInventory))
	}

	// The deferred probe commits the full enumeration to the same row in place.
	resolver.update(func(f *models.MediaFile) {
		f.AudioTracks = []models.AudioTrack{
			{Index: 1, Codec: "eac3", Channels: 6, Language: "eng", Default: true},
			{Index: 2, Codec: "eac3", Channels: 6, Language: "fra"},
		}
		f.SubtitleTracks = []models.SubtitleTrack{
			{Index: 4, Codec: "subrip", Language: "eng"},
			{Index: 7, Codec: "subrip", Language: "fra"},
			{Index: 9, Codec: "subrip", Language: "deu"},
		}
		f.ProbeUpdatedAt = &probedAt
	})

	after, err := handler.GetPlaybackInventoryV2(newAuthorizedPlaybackContext(), caller, session.ID)
	if err != nil {
		t.Fatalf("GetPlaybackInventoryV2 after: %v", err)
	}
	if after.InventoryStatus != string(ProbeProvenanceVerified) {
		t.Fatalf("inventory status = %q, want verified", after.InventoryStatus)
	}
	if len(after.AudioTracks) != 2 {
		t.Fatalf("verified poll audio = %+v, want the row's 2 tracks", after.AudioTracks)
	}
	if len(after.SubtitleInventory) != 3 {
		t.Fatalf("verified poll subtitles = %+v, want the row's 3 tracks", after.SubtitleInventory)
	}
	if after.InventoryRevision == before.InventoryRevision {
		t.Fatalf("inventory revision did not change when the verified tracks landed: %q", after.InventoryRevision)
	}
}
