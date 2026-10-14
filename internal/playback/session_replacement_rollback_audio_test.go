package playback

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

// TestSessionReplacementRollbackRestoresInvalidAudioSelection is the regression
// for the replacement-rollback marker loss: a virtual remux session whose
// verified overlay removed the committed selection (index -1 plus the invalid
// marker) must survive an Apply/Rollback round trip with both the -1 index and
// the marker intact. Rolling back without restoring the marker leaves a -1
// index with no marker, and the serve path would map that negative selection to
// 0:a:0 and mux the wrong track instead of failing closed with a retryable 409.
func TestSessionReplacementRollbackRestoresInvalidAudioSelection(t *testing.T) {
	manager := NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", 7, PlayRemux, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	uri := "virtual://movie/tt-rollback-invalid?result=cand-1"
	if err := manager.SetVirtualSource(session.ID, uri, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	plan := []models.AudioTrack{
		{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
		{Index: 3, Language: "pt", Codec: "eac3", Channels: 6, Layout: "5.1"},
	}
	if err := manager.UpdateStreamState(session.ID, SessionStreamState{
		PlayMethod:                    PlayRemux,
		BasePlayMethod:                PlayRemux,
		AudioTrackIndex:               1, // pt
		VirtualSourceSet:              true,
		VirtualSourceOwnershipSet:     true,
		VirtualSourceURI:              uri,
		VirtualSubtitleEvidenceSet:    true,
		VirtualAudioTracks:            plan,
		VirtualSubtitleEvidenceURI:    uri,
		VirtualSubtitleEvidenceFileID: 7,
	}); err != nil {
		t.Fatalf("UpdateStreamState: %v", err)
	}
	binding, err := manager.VirtualSourceBinding(session.ID)
	if err != nil {
		t.Fatalf("VirtualSourceBinding: %v", err)
	}
	// The verified inventory drops pt: the committed selection becomes invalid.
	verified := []models.AudioTrack{{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"}}
	applied, err := manager.SetVirtualProbeVerifiedInventoryIfBinding(session.ID, binding, "rev-removed", verified, nil, nil)
	if err != nil || !applied {
		t.Fatalf("SetVirtualProbeVerifiedInventoryIfBinding = %v, %v; want applied", applied, err)
	}
	invalid, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !invalid.VirtualAudioSelectionInvalid || invalid.AudioTrackIndex != -1 {
		t.Fatalf("fixture not invalid: index=%d invalid=%v", invalid.AudioTrackIndex, invalid.VirtualAudioSelectionInvalid)
	}

	// A replacement applies a fresh selection and clears the marker on the
	// live session (the forward-apply half).
	rollback, err := manager.ApplyReplacement(session.ID, SessionReplacement{
		EffectiveMediaFileID: 9,
		StreamState: SessionStreamState{
			PlayMethod:                    PlayRemux,
			BasePlayMethod:                PlayRemux,
			AudioTrackIndex:               0,
			TranscodeRouteSet:             true,
			VirtualSourceSet:              true,
			VirtualSourceOwnershipSet:     true,
			VirtualSourceURI:              uri,
			VirtualSubtitleEvidenceSet:    true,
			VirtualAudioTracks:            verified,
			VirtualSubtitleEvidenceURI:    uri,
			VirtualSubtitleEvidenceFileID: 7,
		},
	})
	if err != nil {
		t.Fatalf("ApplyReplacement: %v", err)
	}
	replaced, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession after replacement: %v", err)
	}
	if replaced.VirtualAudioSelectionInvalid {
		t.Fatal("a fresh replacement selection did not clear the invalid-selection marker")
	}
	if replaced.AudioTrackIndex != 0 {
		t.Fatalf("replaced audio index = %d, want 0", replaced.AudioTrackIndex)
	}

	// Rolling the failed replacement back must restore the invalid verdict,
	// not just the -1 index.
	if err := manager.RollbackReplacement(session.ID, rollback); err != nil {
		t.Fatalf("RollbackReplacement: %v", err)
	}
	restored, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession after rollback: %v", err)
	}
	if restored.AudioTrackIndex != -1 {
		t.Fatalf("rolled-back audio index = %d, want the invalid sentinel -1", restored.AudioTrackIndex)
	}
	if !restored.VirtualAudioSelectionInvalid {
		t.Fatal("rollback dropped the invalid-selection marker: index -1 without the marker mints 0:a:0 on the next remux")
	}
}
