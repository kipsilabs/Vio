package playback

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

// TestVirtualStreamStateCarriesDeclaredAudioWithoutEvidence pins the deferred
// audio retention: a virtual snapshot with no probe evidence must still leave
// the plan-time audio inventory and its provenance anchor on the session, so
// the serve overlay can describe the release instead of an empty menu. A
// non-virtual effective file clears both.
func TestVirtualStreamStateCarriesDeclaredAudioWithoutEvidence(t *testing.T) {
	manager := NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", 7, PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	uri := "virtual://movie/tt-audio?result=cand-1"
	audio := []models.AudioTrack{{Codec: "eac3", Channels: 6, Language: "jpn"}}
	if err := manager.UpdateStreamState(session.ID, SessionStreamState{
		VirtualSourceSet:              true,
		VirtualSourceOwnershipSet:     true,
		VirtualSourceURI:              uri,
		VirtualSubtitleEvidenceSet:    false,
		VirtualAudioTracks:            audio,
		VirtualSubtitleEvidenceURI:    uri,
		VirtualSubtitleEvidenceFileID: 7,
	}); err != nil {
		t.Fatalf("UpdateStreamState: %v", err)
	}

	got, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if len(got.VirtualAudioTracks) != 1 || got.VirtualAudioTracks[0].Language != "jpn" {
		t.Fatalf("deferred audio not retained: %+v", got.VirtualAudioTracks)
	}
	if got.VirtualSubtitleEvidenceSet {
		t.Fatal("evidence flag was set without a probe stamp")
	}
	if got.VirtualSubtitleEvidenceURI != uri || got.VirtualSubtitleEvidenceFileID != 7 {
		t.Fatalf("provenance anchor not retained: uri=%q id=%d", got.VirtualSubtitleEvidenceURI, got.VirtualSubtitleEvidenceFileID)
	}

	// A rotation moves the binding but leaves the anchor, which is what lets
	// the serve overlay reject the stale audio.
	if err := manager.SetVirtualSource(session.ID, "virtual://movie/tt-audio?result=cand-2", 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	rotated, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession after rotation: %v", err)
	}
	if rotated.VirtualSubtitleEvidenceURI != uri {
		t.Fatalf("rotation moved the provenance anchor to %q, want the previous candidate %q", rotated.VirtualSubtitleEvidenceURI, uri)
	}

	// A non-virtual effective file clears the carried inventory and anchor.
	if err := manager.UpdateStreamState(session.ID, SessionStreamState{
		VirtualSourceSet:          true,
		VirtualSourceOwnershipSet: true,
	}); err != nil {
		t.Fatalf("UpdateStreamState non-virtual: %v", err)
	}
	cleared, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession non-virtual: %v", err)
	}
	if cleared.VirtualAudioTracks != nil || cleared.VirtualSubtitleEvidenceURI != "" || cleared.VirtualSubtitleEvidenceFileID != 0 {
		t.Fatalf("non-virtual snapshot kept virtual inventory: audio=%+v uri=%q id=%d",
			cleared.VirtualAudioTracks, cleared.VirtualSubtitleEvidenceURI, cleared.VirtualSubtitleEvidenceFileID)
	}
}
