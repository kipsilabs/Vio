package handlers

import (
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// moveSessionToCandidateV3 moves the fixture's session binding to candidateURI
// on ownerInstallationID and, when evidenceSet is true, advances the session's
// retained evidence anchor to that candidate. It models the serve-layer rotation
// (the pin and owner move) plus a probe/plan commit that moved the evidence past
// the running plan.
func moveSessionToCandidateV3(t *testing.T, fx *pinnedTrackChangeFixture, candidateURI string, ownerInstallationID int, evidenceSet bool, audio []models.AudioTrack, subtitles []models.SubtitleTrack) {
	t.Helper()
	if err := fx.manager.SetVirtualSource(fx.started.SessionID, candidateURI, ownerInstallationID); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	if !evidenceSet {
		return
	}
	if err := fx.manager.UpdateStreamState(fx.started.SessionID, playback.SessionStreamState{
		VirtualSourceURI:                           candidateURI,
		VirtualSourceSet:                           true,
		VirtualSourceOwnershipSet:                  true,
		VirtualSubtitleEvidenceSet:                 true,
		VirtualSubtitleEvidenceURI:                 candidateURI,
		VirtualSubtitleEvidenceFileID:              fx.source.ID,
		VirtualSubtitleEvidenceOwnerInstallationID: ownerInstallationID,
		VirtualAudioTracks:                         audio,
		VirtualSubtitleTracks:                      subtitles,
		SubtitleTrackIndex:                         -1,
	}); err != nil {
		t.Fatalf("UpdateStreamState: %v", err)
	}
}

// A cross-installation binding move must not let a seek reanchor resolve the
// frozen candidate through the moved binding's provider installation. The
// frozen candidate's owner is the one captured with its evidence, not the live
// session owner.
func TestReplanSeekReanchorRestoresFrozenCandidateOwner(t *testing.T) {
	fx := newPinnedTrackChangeFixture(t, "b")
	candidateB := withVirtualResultKey(virtualPlaybackNeutralKey(fx.source.FilePath), "b")
	movedOwner := fx.source.VirtualOwnerInstallationID + 94
	moveSessionToCandidateV3(t, fx, candidateB, movedOwner, false, nil, nil)
	// The reloaded row names B, so the frozen candidate's inventory must come
	// from the session's retained evidence rather than the row.
	fx.churnCatalogRowToStub("b")

	response := postPlaybackReplanV3(t, fx.handler, fx.started.SessionID, seekReanchorRequestV3(fx, "seek-restore-owner-0001"))
	if response.Terminal != nil || response.PlaybackPlan == nil {
		t.Fatalf("seek = plan %#v terminal %#v, want a plan", response.PlaybackPlan, response.Terminal)
	}
	if response.PlaybackPlan.EffectiveVirtualURI != fx.pinnedURI {
		t.Fatalf("seek effective virtual URI = %q, want the frozen candidate %q", response.PlaybackPlan.EffectiveVirtualURI, fx.pinnedURI)
	}
	session, err := fx.manager.GetSession(fx.started.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if session.VirtualSourceURI != fx.pinnedURI {
		t.Fatalf("seek session bound %q, want the frozen candidate %q", session.VirtualSourceURI, fx.pinnedURI)
	}
	// The transport resolves the source through this owner, so borrowing the
	// moved owner would resolve the pinned bytes through another installation.
	if session.VirtualSourceOwnerInstallationID != fx.source.VirtualOwnerInstallationID {
		t.Fatalf("seek resolved through owner %d, want the frozen candidate's owner %d", session.VirtualSourceOwnerInstallationID, fx.source.VirtualOwnerInstallationID)
	}
	// No provider resolve ran: the frozen candidate's own evidence was carried.
	if fx.detailCalls != 0 || len(fx.resolverInputs) != 0 {
		t.Fatalf("seek re-resolved the provider: detail=%d inputs=%v", fx.detailCalls, fx.resolverInputs)
	}
}

// When the frozen candidate's own owner cannot be established the seek must
// fail closed (retryably) rather than borrow the moved binding's owner.
func TestReplanSeekReanchorFailsClosedWithoutFrozenOwner(t *testing.T) {
	fx := newPinnedTrackChangeFixture(t, "b")
	candidateB := withVirtualResultKey(virtualPlaybackNeutralKey(fx.source.FilePath), "b")
	fx.churnCatalogRowToStub("b")
	if err := fx.manager.SetVirtualSource(fx.started.SessionID, candidateB, fx.source.VirtualOwnerInstallationID+94); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	// A legacy/reconstructed session with an evidence anchor but no captured
	// installation owner: the anchor survives, the owner is unknown.
	session, err := fx.manager.GetSession(fx.started.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if err := fx.manager.UpdateStreamState(fx.started.SessionID, playback.SessionStreamState{
		VirtualSourceURI:              candidateB,
		VirtualSourceSet:              true,
		VirtualSourceOwnershipSet:     true,
		VirtualAudioTracks:            session.VirtualAudioTracks,
		VirtualSubtitleEvidenceURI:    fx.pinnedURI,
		VirtualSubtitleEvidenceFileID: fx.source.ID,
		SubtitleTrackIndex:            -1,
	}); err != nil {
		t.Fatalf("clear evidence owner: %v", err)
	}

	_, response := postReplanRawV3(t, fx.handler, fx.started.SessionID, seekReanchorRequestV3(fx, "seek-fail-closed-owner-0001"))
	if response.PlaybackPlan != nil {
		t.Fatalf("seek reopened the frozen recipe without an owner: %#v", response.PlaybackPlan)
	}
	if response.Terminal == nil || response.Terminal.Reason != seekReanchorEvidenceUnavailableReasonV3 || !response.Terminal.Retryable {
		t.Fatalf("seek terminal = %#v, want retryable %s", response.Terminal, seekReanchorEvidenceUnavailableReasonV3)
	}
}

// A seek reanchor whose session evidence has advanced to another release must
// not relabel that release's probed row as the frozen candidate: the frozen
// candidate's own inventory is gone, so the seek fails closed instead of
// committing the foreign inventory under the frozen candidate's URI.
func TestReplanSeekReanchorFailsClosedOnForeignEvidence(t *testing.T) {
	fx := newPinnedTrackChangeFixture(t, "b")
	candidateB := withVirtualResultKey(virtualPlaybackNeutralKey(fx.source.FilePath), "b")
	movedOwner := fx.source.VirtualOwnerInstallationID + 94
	bAudio := []models.AudioTrack{{Index: 0, Codec: "eac3", Channels: 6, Layout: "5.1", Language: "fra"}}
	bSubtitles := []models.SubtitleTrack{{Index: 0, Codec: "ass", Language: "fra"}}

	// A fully probed B row with inventories distinct from A's.
	probed := *fx.source
	probed.FilePath = candidateB
	probed.AudioTracks = bAudio
	probed.SubtitleTracks = bSubtitles
	probed.ExternalSubtitles = nil
	stamp := time.Now().UTC()
	probed.ProbeUpdatedAt = &stamp
	fx.files[fx.source.ID] = &probed

	// The session's pin and evidence have both advanced to B.
	moveSessionToCandidateV3(t, fx, candidateB, movedOwner, true, bAudio, bSubtitles)

	_, response := postReplanRawV3(t, fx.handler, fx.started.SessionID, seekReanchorRequestV3(fx, "seek-foreign-evidence-0001"))
	if response.PlaybackPlan != nil {
		t.Fatalf("seek planned against foreign evidence: %#v", response.PlaybackPlan)
	}
	if response.Terminal == nil || response.Terminal.Reason != seekReanchorEvidenceUnavailableReasonV3 || !response.Terminal.Retryable {
		t.Fatalf("seek terminal = %#v, want retryable %s", response.Terminal, seekReanchorEvidenceUnavailableReasonV3)
	}
	// No commit stamped B's inventory under A's URI.
	session, err := fx.manager.GetSession(fx.started.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if session.VirtualSubtitleEvidenceURI != candidateB {
		t.Fatalf("session evidence anchor = %q, want it left at the moved candidate %q", session.VirtualSubtitleEvidenceURI, candidateB)
	}
	if session.VirtualSourceURI != candidateB {
		t.Fatalf("session binding = %q, want it left at the moved candidate %q", session.VirtualSourceURI, candidateB)
	}
	if len(session.VirtualAudioTracks) != 1 || session.VirtualAudioTracks[0].Codec != "eac3" {
		t.Fatalf("session audio inventory = %+v, want the moved candidate's eac3 track left untouched", session.VirtualAudioTracks)
	}
	// No executor input was produced: the seek neither resolved nor handed the
	// frozen candidate to a provider.
	if fx.detailCalls != 0 || len(fx.resolverInputs) != 0 {
		t.Fatalf("seek re-resolved the provider: detail=%d inputs=%v", fx.detailCalls, fx.resolverInputs)
	}
}

// Carry admission must prove the running plan's descriptor describes the
// candidate being carried, not merely that the session's evidence matches the
// live pin. When the pin and evidence have moved to B while the durable plan
// still describes A, a user-intent replan must resolve B rather than top B's
// row up with A's descriptor.
func TestReplanTrackChangeResolvesWhenPlanDescriptorLagsMovedPin(t *testing.T) {
	fx := newPinnedTrackChangeFixture(t, "b")
	candidateB := withVirtualResultKey(virtualPlaybackNeutralKey(fx.source.FilePath), "b")
	movedOwner := fx.source.VirtualOwnerInstallationID + 94
	bAudio := []models.AudioTrack{{Index: 0, Codec: "eac3", Channels: 6, Layout: "5.1", Language: "fra"}}
	bSubtitles := []models.SubtitleTrack{{Index: 0, Codec: "ass", Language: "fra"}}

	// An incomplete B row: without the provenance gate the carry would top its
	// bytes up with A's validated descriptor.
	incomplete := *fx.source
	incomplete.FilePath = candidateB
	incomplete.AudioTracks = bAudio
	incomplete.SubtitleTracks = bSubtitles
	incomplete.ExternalSubtitles = nil
	incomplete.VideoTracks = nil
	incomplete.CodecVideo = ""
	incomplete.Container = "virtual"
	incomplete.ProbeUpdatedAt = nil
	fx.files[fx.source.ID] = &incomplete

	// The session's pin and evidence have advanced to B; the durable plan still
	// describes A.
	moveSessionToCandidateV3(t, fx, candidateB, movedOwner, true, bAudio, bSubtitles)

	response := fx.trackChange(t, 1, "plan-lags-pin-0001")
	if response.Terminal != nil || response.PlaybackPlan == nil {
		t.Fatalf("track change = plan %#v terminal %#v, want a plan", response.PlaybackPlan, response.Terminal)
	}
	if fx.detailCalls == 0 {
		t.Fatal("track change carried the lagging plan's descriptor for the moved pin without re-resolving")
	}
	if response.PlaybackPlan.EffectiveVirtualURI != candidateB {
		t.Fatalf("track change effective virtual URI = %q, want the moved pin %q", response.PlaybackPlan.EffectiveVirtualURI, candidateB)
	}
}
