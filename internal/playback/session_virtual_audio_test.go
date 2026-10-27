package playback

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

// TestVirtualProbeOverlayInvalidatesRemovedCommittedAudio pins the fail-closed
// half of the overlay swap: a committed selection that the verified inventory no
// longer contains must become a server-enforced invalid selection (index -1 plus
// the marker), never an unchanged array position that now names a different
// track. A reorder that still re-identifies the track clears the marker.
func TestVirtualProbeOverlayInvalidatesRemovedCommittedAudio(t *testing.T) {
	manager := NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", 7, PlayRemux, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	uri := "virtual://movie/tt-invalid-audio?result=cand-1"
	if err := manager.SetVirtualSource(session.ID, uri, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	plan := []models.AudioTrack{
		{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
		{Index: 3, Language: "pt", Codec: "eac3", Channels: 6, Layout: "5.1"},
	}
	if err := manager.UpdateStreamState(session.ID, SessionStreamState{
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

	// The verified inventory drops pt entirely.
	verified := []models.AudioTrack{{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"}}
	applied, err := manager.SetVirtualProbeVerifiedInventoryIfBinding(session.ID, binding, "rev-removed", verified, nil, nil)
	if err != nil || !applied {
		t.Fatalf("SetVirtualProbeVerifiedInventoryIfBinding = %v, %v; want applied", applied, err)
	}
	live, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !live.VirtualAudioSelectionInvalid {
		t.Fatalf("removed committed audio was not marked invalid; index = %d", live.AudioTrackIndex)
	}
	if live.AudioTrackIndex != -1 {
		t.Fatalf("session audio index = %d, want the invalid sentinel -1", live.AudioTrackIndex)
	}
	if len(live.VirtualAudioTracks) != 1 {
		t.Fatalf("carried audio = %+v, want the verified 1-track list", live.VirtualAudioTracks)
	}
}

// TestVirtualProbeOverlayClearsInvalidMarkerOnSuccessfulRemap pins that the
// invalid marker is not sticky: a later verified inventory that still contains
// the committed track (reordered) clears it and re-points the selection.
func TestVirtualProbeOverlayClearsInvalidMarkerOnSuccessfulRemap(t *testing.T) {
	manager := NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", 8, PlayRemux, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	uri := "virtual://movie/tt-invalid-clear?result=cand-1"
	if err := manager.SetVirtualSource(session.ID, uri, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	plan := []models.AudioTrack{
		{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
		{Index: 3, Language: "pt", Codec: "eac3", Channels: 6, Layout: "5.1"},
	}
	if err := manager.UpdateStreamState(session.ID, SessionStreamState{
		AudioTrackIndex:               1, // pt
		VirtualSourceSet:              true,
		VirtualSourceOwnershipSet:     true,
		VirtualSourceURI:              uri,
		VirtualSubtitleEvidenceSet:    true,
		VirtualAudioTracks:            plan,
		VirtualSubtitleEvidenceURI:    uri,
		VirtualSubtitleEvidenceFileID: 8,
	}); err != nil {
		t.Fatalf("UpdateStreamState: %v", err)
	}
	binding, err := manager.VirtualSourceBinding(session.ID)
	if err != nil {
		t.Fatalf("VirtualSourceBinding: %v", err)
	}
	// The verified list reorders pt to position 0; it is still the same stream.
	verified := []models.AudioTrack{
		{Index: 3, Language: "pt", Codec: "eac3", Channels: 6, Layout: "5.1"},
		{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
	}
	applied, err := manager.SetVirtualProbeVerifiedInventoryIfBinding(session.ID, binding, "rev-reorder", verified, nil, nil)
	if err != nil || !applied {
		t.Fatalf("SetVirtualProbeVerifiedInventoryIfBinding = %v, %v; want applied", applied, err)
	}
	live, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualAudioSelectionInvalid {
		t.Fatal("a successful remap left the invalid-selection marker set")
	}
	if live.AudioTrackIndex != 0 || live.VirtualAudioTracks[live.AudioTrackIndex].Language != "pt" {
		t.Fatalf("remapped index = %d names %q, want the reordered pt track", live.AudioTrackIndex, live.VirtualAudioTracks[live.AudioTrackIndex].Language)
	}
}

// TestVirtualProbeOverlayResolvesSyntheticPlaceholderToVerifiedDefault pins the
// other half: a plan-time inventory that is only a synthesized placeholder
// ({Index: -1, Default: true}) carries no identity, so the verified swap must
// re-resolve the committed "default" intent against the real list instead of
// tying every track and invalidating the selection the client still shows.
func TestVirtualProbeOverlayResolvesSyntheticPlaceholderToVerifiedDefault(t *testing.T) {
	manager := NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", 9, PlayRemux, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	uri := "virtual://movie/tt-synthetic?result=cand-1"
	if err := manager.SetVirtualSource(session.ID, uri, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	if err := manager.UpdateStreamState(session.ID, SessionStreamState{
		AudioTrackIndex:               0,
		VirtualSourceSet:              true,
		VirtualSourceOwnershipSet:     true,
		VirtualSourceURI:              uri,
		VirtualSubtitleEvidenceSet:    true,
		VirtualAudioTracks:            []models.AudioTrack{{Index: -1, Default: true}},
		VirtualSubtitleEvidenceURI:    uri,
		VirtualSubtitleEvidenceFileID: 9,
	}); err != nil {
		t.Fatalf("UpdateStreamState: %v", err)
	}
	binding, err := manager.VirtualSourceBinding(session.ID)
	if err != nil {
		t.Fatalf("VirtualSourceBinding: %v", err)
	}
	verified := []models.AudioTrack{
		{Index: 0, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo", Default: true},
		{Index: 1, Language: "ja", Codec: "aac", Channels: 2, Layout: "stereo"},
	}
	applied, err := manager.SetVirtualProbeVerifiedInventoryIfBinding(session.ID, binding, "rev-synthetic", verified, nil, nil)
	if err != nil || !applied {
		t.Fatalf("SetVirtualProbeVerifiedInventoryIfBinding = %v, %v; want applied", applied, err)
	}
	live, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualAudioSelectionInvalid {
		t.Fatal("a synthesized default placeholder was invalidated instead of resolved to the verified default")
	}
	if live.AudioTrackIndex != 0 || !live.VirtualAudioTracks[live.AudioTrackIndex].Default {
		t.Fatalf("resolved index = %d, want the verified default track", live.AudioTrackIndex)
	}
}

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
