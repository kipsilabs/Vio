package handlers

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// TestReplacementRollbackKeepsInvalidSelectionFailingClosed is the handler half
// of the replacement-rollback marker regression: after ApplyReplacement and a
// RollbackReplacement with no intervening mutations, the session must still
// fail closed on the invalid selection — a stream request must answer 409
// audio_selection_invalid and never start a remux that would map the restored
// -1 index onto 0:a:0.
func TestReplacementRollbackKeepsInvalidSelectionFailingClosed(t *testing.T) {
	pinned := "virtual://movie/tt-rollback-gate?result=cand-1"
	file := &models.MediaFile{
		ID:                         4311,
		ContentID:                  "movie-rollback-gate",
		FilePath:                   pinned,
		CodecVideo:                 "h264",
		VideoTracks:                []models.VideoTrack{{Codec: "h264"}},
		VirtualOwnerInstallationID: 5,
	}
	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", file.ID, playback.PlayRemux, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := manager.SetVirtualSource(session.ID, pinned, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	plan := []models.AudioTrack{
		{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
		{Index: 3, Language: "pt", Codec: "eac3", Channels: 6, Layout: "5.1"},
	}
	if err := manager.UpdateStreamState(session.ID, playback.SessionStreamState{
		PlayMethod:                    playback.PlayRemux,
		BasePlayMethod:                playback.PlayRemux,
		AudioTrackIndex:               1, // pt
		VirtualSourceSet:              true,
		VirtualSourceOwnershipSet:     true,
		VirtualSourceURI:              pinned,
		VirtualSubtitleEvidenceSet:    true,
		VirtualAudioTracks:            plan,
		VirtualSubtitleEvidenceURI:    pinned,
		VirtualSubtitleEvidenceFileID: file.ID,
	}); err != nil {
		t.Fatalf("UpdateStreamState: %v", err)
	}
	binding, err := manager.VirtualSourceBinding(session.ID)
	if err != nil {
		t.Fatalf("VirtualSourceBinding: %v", err)
	}
	verified := []models.AudioTrack{{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"}}
	if applied, err := manager.SetVirtualProbeVerifiedInventoryIfBinding(session.ID, binding, "rev", verified, nil, nil); err != nil || !applied {
		t.Fatalf("SetVirtualProbeVerifiedInventoryIfBinding = %v, %v; want applied", applied, err)
	}
	if invalid, _ := manager.GetSession(session.ID); !invalid.VirtualAudioSelectionInvalid || invalid.AudioTrackIndex != -1 {
		t.Fatalf("fixture not invalid: index=%d invalid=%v", invalid.AudioTrackIndex, invalid.VirtualAudioSelectionInvalid)
	}

	// Apply a valid replacement and roll it back with no intervening
	// mutations: the rollback must succeed and restore the invalid verdict.
	rollbackToken, err := manager.ApplyReplacement(session.ID, playback.SessionReplacement{
		EffectiveMediaFileID: file.ID + 1,
		StreamState: playback.SessionStreamState{
			PlayMethod:                    playback.PlayRemux,
			BasePlayMethod:                playback.PlayRemux,
			AudioTrackIndex:               0,
			TranscodeRouteSet:             true,
			VirtualSourceSet:              true,
			VirtualSourceOwnershipSet:     true,
			VirtualSourceURI:              pinned,
			VirtualSubtitleEvidenceSet:    true,
			VirtualAudioTracks:            verified,
			VirtualSubtitleEvidenceURI:    pinned,
			VirtualSubtitleEvidenceFileID: file.ID,
		},
	})
	if err != nil {
		t.Fatalf("ApplyReplacement: %v", err)
	}
	if err := manager.RollbackReplacement(session.ID, rollbackToken); err != nil {
		t.Fatalf("RollbackReplacement: %v", err)
	}
	restored, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession after rollback: %v", err)
	}
	if restored.AudioTrackIndex != -1 || !restored.VirtualAudioSelectionInvalid {
		t.Fatalf("rollback did not restore the invalid verdict: index=%d invalid=%v", restored.AudioTrackIndex, restored.VirtualAudioSelectionInvalid)
	}

	// The real transport gate must answer 409; a lost marker would start a
	// remux against the restored -1 index.
	assertStreamAudioSelectionRejected(t, manager, testPlaybackFileResolver{file: file}, session.ID, pinned)
}
