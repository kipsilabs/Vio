package handlers

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// TestDeferredProbeRunsDefaultAudioReconciliation is the #233 hook regression:
// the deferred durable evidence write must run the shared
// reconcileVerifiedDefaultAudio correction against the committed owner row, the
// same hook the direct and queued evidence paths run. Without it a deferred
// session whose probe reorders the audio tracks keeps the declared selection
// (wrong language) while the menu already shows the corrected inventory.
//
// The declared inventory commits en at index 0 while the viewer's preference is
// pt; the verified enumeration reverses the order and differs in codec/channels,
// so the executable selection moved and the reconcile path must settle an
// invalidation on the attempt ledger.
func TestDeferredProbeRunsDefaultAudioReconciliation(t *testing.T) {
	const fileID = 8100
	candidateURI := "virtual://movie/tt-deferred-reconcile?result=cand-1"
	planTime := []models.AudioTrack{
		{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
		{Index: 3, Language: "pt", Codec: "eac3", Channels: 6, Layout: "5.1"},
	}
	verifiedAudio := []models.AudioTrack{
		{Index: 3, Language: "pt", Codec: "eac3", Channels: 6, Layout: "5.1"},
		{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
	}

	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", fileID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := manager.SetVirtualSource(session.ID, candidateURI, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	// Mirror the committed start: declared audio inventory, the auto-selection
	// intent, and the bound candidate's evidence provenance.
	if err := manager.UpdateStreamState(session.ID, playback.SessionStreamState{
		VirtualSourceURI:              candidateURI,
		VirtualSourceSet:              true,
		VirtualSubtitleEvidenceSet:    true,
		VirtualAudioTracks:            planTime,
		VirtualSubtitleEvidenceURI:    candidateURI,
		VirtualSubtitleEvidenceFileID: fileID,
		SelectionOriginSet:            true,
		SelectionOrigin:               SelectionOriginAuto,
		PreferredAudioLanguage:        "pt",
		SelectedAudioSignature:        playback.AudioTrackSignatureFromTrack(planTime[0]),
	}); err != nil {
		t.Fatalf("UpdateStreamState: %v", err)
	}

	source := &models.MediaFile{
		ID:                         fileID,
		ContentID:                  "movie-deferred-reconcile",
		FilePath:                   candidateURI,
		Container:                  "virtual",
		CodecVideo:                 "h264",
		Resolution:                 "1080p",
		Duration:                   3600,
		VirtualOwnerInstallationID: 5,
		VideoTracks:                []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080, BitDepth: 8}},
		AudioTracks:                planTime,
	}
	resolver := &syncPlaybackFileResolver{file: *source}
	handler := NewPlaybackHandler(manager, resolver)
	handler.RealtimeHub = playback.NewRealtimeHub()
	handler.InstallationID = "test-install"
	serviceCtx, serviceCancel := context.WithCancel(context.Background())
	handler.ServiceContext = serviceCtx
	t.Cleanup(serviceCancel)

	probedAt := time.Now().UTC()
	handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		f.AudioTracks = verifiedAudio
		f.CodecAudio = "eac3"
		return f, nil
	}
	// The durable evidence write commits synchronously; model the committed row
	// the reconcile sweep re-reads, including the fresh probe stamp that keys
	// the verified generation.
	handler.VirtualFileMetadataSaver = func(_ context.Context, args models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
		var audio []models.AudioTrack
		if err := json.Unmarshal(args.AudioTracks, &audio); err != nil {
			return VirtualFileMetadataUpdateResult{}, err
		}
		resolver.update(func(f *models.MediaFile) {
			f.AudioTracks = audio
			f.CodecAudio = args.CodecAudio
			f.ProbeUpdatedAt = &probedAt
		})
		return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
	}

	// The attempt row carries the auto-selection intent and the response-side
	// capability the withdrawal is gated on; the decision is recorded either
	// way, which is what this test asserts.
	record := playback.AttemptRecordV3{
		PlaybackAttemptID:      "attempt-deferred-reconcile",
		SessionID:              session.ID,
		UserID:                 1,
		ProfileID:              "profile-1",
		RequestedMediaFileID:   fileID,
		EffectiveMediaFileID:   fileID,
		ExpiresAt:              time.Now().Add(time.Hour),
		CurrentPlanID:          "plan-deferred-reconcile",
		SelectionOrigin:        SelectionOriginAuto,
		PreferredAudioLanguage: "pt",
		SelectedAudioSignature: playback.AudioTrackSignatureFromTrack(planTime[0]),
		NormalizedRequest: playback.StartRequestV3{
			ClientFeatures: []string{playback.FeaturePlanInvalidatedV3, playback.FeatureDefaultAudioReconcileResponseV3},
			Capabilities:   reconcileCapabilities(),
		},
	}
	record.CurrentPlan.SelectedTracks.Audio = &playback.TrackIdentityV3{ID: playback.TrackIDV3(fileID, "audio", 0), Index: intPtrReconcile(0)}
	record.CurrentPlan.PlanAttemptKey = "v3:plan-deferred-reconcile:a:b"
	if err := handler.PlanStoreV3.SaveAttempt(context.Background(), record); err != nil {
		t.Fatalf("SaveAttempt: %v", err)
	}

	deferred := readmitDeferredProbe(t, handler, deferredOverflowProbe(session, candidateURI))
	handler.runDeferredVirtualProbe(deferred)

	after, err := handler.PlanStoreV3.GetAttempt(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("GetAttempt: %v", err)
	}
	// The reconcile now decides against the exact committed carrier, so the
	// ledger generation is the committed inventory's fingerprint.
	generation := reconcileGenerationV3(&models.MediaFile{AudioTracks: verifiedAudio})
	entry := playback.FindAudioReconcileEntry(after.AudioReconcileLedger, generation, session.ID)
	if entry == nil {
		t.Fatalf("deferred durable write did not run default-audio reconciliation (ledger = %#v)", after.AudioReconcileLedger)
	}
	if entry.Decision != playback.AudioReconcileInvalidated {
		t.Fatalf("reconcile decision = %q, want %q (the reorder moved the executable selection)", entry.Decision, playback.AudioReconcileInvalidated)
	}
	if entry.AudioIndex == nil || *entry.AudioIndex != 0 {
		t.Fatalf("corrected audio index = %v, want 0 (pt at its verified position)", entry.AudioIndex)
	}
	// The overlay swap preserves the selected track's identity: the session's
	// index names pt in the verified list, so the remux selector still resolves
	// the pt stream rather than whatever moved into the old position.
	live, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	// The session's committed executable stream is the declared en (planTime[0],
	// container index 1); the overlay swap must keep the index naming that same
	// stream rather than the position that moved into the old slot.
	if live.AudioTrackIndex < 0 || live.AudioTrackIndex >= len(live.VirtualAudioTracks) ||
		live.VirtualAudioTracks[live.AudioTrackIndex].Index != planTime[0].Index {
		t.Fatalf("session audio selection = index %d of %+v, want the committed en stream (container index %d)",
			live.AudioTrackIndex, live.VirtualAudioTracks, planTime[0].Index)
	}
}

// TestDeferredProbeReconcilePreservesExplicitSelection is the #233 counterpart:
// a viewer who explicitly chose an audio track must keep it. The deferred
// durable write still runs the reconcile hook, but an explicit origin is only
// verified present, never overridden, so no invalidation decision is recorded
// even though the verified order differs.
func TestDeferredProbeReconcilePreservesExplicitSelection(t *testing.T) {
	const fileID = 8101
	candidateURI := "virtual://movie/tt-deferred-explicit?result=cand-1"
	planTime := []models.AudioTrack{
		{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
		{Index: 3, Language: "de", Codec: "eac3", Channels: 6, Layout: "5.1"},
	}
	verifiedAudio := []models.AudioTrack{
		{Index: 3, Language: "de", Codec: "eac3", Channels: 6, Layout: "5.1"},
		{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
	}

	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", fileID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := manager.SetVirtualSource(session.ID, candidateURI, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	if err := manager.UpdateStreamState(session.ID, playback.SessionStreamState{
		AudioTrackIndex:               1,
		VirtualSourceURI:              candidateURI,
		VirtualSourceSet:              true,
		VirtualSubtitleEvidenceSet:    true,
		VirtualAudioTracks:            planTime,
		VirtualSubtitleEvidenceURI:    candidateURI,
		VirtualSubtitleEvidenceFileID: fileID,
		SelectionOriginSet:            true,
		SelectionOrigin:               SelectionOriginExplicit,
		SelectedAudioSignature:        playback.AudioTrackSignatureFromTrack(planTime[1]),
	}); err != nil {
		t.Fatalf("UpdateStreamState: %v", err)
	}

	source := &models.MediaFile{
		ID:                         fileID,
		ContentID:                  "movie-deferred-explicit",
		FilePath:                   candidateURI,
		Container:                  "virtual",
		CodecVideo:                 "h264",
		Resolution:                 "1080p",
		Duration:                   3600,
		VirtualOwnerInstallationID: 5,
		VideoTracks:                []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080, BitDepth: 8}},
		AudioTracks:                planTime,
	}
	resolver := &syncPlaybackFileResolver{file: *source}
	handler := NewPlaybackHandler(manager, resolver)
	handler.RealtimeHub = playback.NewRealtimeHub()
	handler.InstallationID = "test-install"
	serviceCtx, serviceCancel := context.WithCancel(context.Background())
	handler.ServiceContext = serviceCtx
	t.Cleanup(serviceCancel)

	probedAt := time.Now().UTC()
	handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		f.AudioTracks = verifiedAudio
		f.CodecAudio = "eac3"
		return f, nil
	}
	handler.VirtualFileMetadataSaver = func(_ context.Context, args models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
		var audio []models.AudioTrack
		if err := json.Unmarshal(args.AudioTracks, &audio); err != nil {
			return VirtualFileMetadataUpdateResult{}, err
		}
		resolver.update(func(f *models.MediaFile) {
			f.AudioTracks = audio
			f.CodecAudio = args.CodecAudio
			f.ProbeUpdatedAt = &probedAt
		})
		return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
	}

	record := playback.AttemptRecordV3{
		PlaybackAttemptID:      "attempt-deferred-explicit",
		SessionID:              session.ID,
		UserID:                 1,
		ProfileID:              "profile-1",
		RequestedMediaFileID:   fileID,
		EffectiveMediaFileID:   fileID,
		ExpiresAt:              time.Now().Add(time.Hour),
		CurrentPlanID:          "plan-deferred-explicit",
		SelectionOrigin:        SelectionOriginExplicit,
		SelectedAudioSignature: playback.AudioTrackSignatureFromTrack(planTime[1]),
		NormalizedRequest: playback.StartRequestV3{
			ClientFeatures: []string{playback.FeaturePlanInvalidatedV3, playback.FeatureDefaultAudioReconcileResponseV3},
			Capabilities:   reconcileCapabilities(),
		},
	}
	record.CurrentPlan.SelectedTracks.Audio = &playback.TrackIdentityV3{ID: playback.TrackIDV3(fileID, "audio", 1), Index: intPtrReconcile(1)}
	record.CurrentPlan.PlanAttemptKey = "v3:plan-deferred-explicit:a:b"
	if err := handler.PlanStoreV3.SaveAttempt(context.Background(), record); err != nil {
		t.Fatalf("SaveAttempt: %v", err)
	}

	deferred := readmitDeferredProbe(t, handler, deferredOverflowProbe(session, candidateURI))
	handler.runDeferredVirtualProbe(deferred)

	after, err := handler.PlanStoreV3.GetAttempt(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("GetAttempt: %v", err)
	}
	generation := reconcileGenerationV3(&models.MediaFile{AudioTracks: verifiedAudio})
	if entry := playback.FindAudioReconcileEntry(after.AudioReconcileLedger, generation, session.ID); entry != nil && entry.Decision == playback.AudioReconcileInvalidated {
		t.Fatalf("explicit selection was overridden by the deferred reconcile hook: %#v", entry)
	}
	live, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	// Identity, not raw position: the viewer chose de, and after the reorder the
	// session's index must still name de so the remux selector resolves its
	// stream instead of the track that moved into the old slot.
	if live.AudioTrackIndex < 0 || live.AudioTrackIndex >= len(live.VirtualAudioTracks) ||
		live.VirtualAudioTracks[live.AudioTrackIndex].Language != "de" {
		t.Fatalf("explicit selection = index %d of %+v, want the viewer's de track", live.AudioTrackIndex, live.VirtualAudioTracks)
	}
	planOrdinal := playback.AudioStreamOrdinal(planTime, 1)
	if got := audioStreamOrdinalV3(&models.MediaFile{AudioTracks: live.VirtualAudioTracks}, live.AudioTrackIndex); got != planOrdinal {
		t.Fatalf("explicit selection moved the remux stream to 0:a:%d, want 0:a:%d", got, planOrdinal)
	}
}
