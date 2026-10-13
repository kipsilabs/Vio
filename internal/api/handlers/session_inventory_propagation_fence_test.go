package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// faultingBoundRowResolver makes GetByID fail for one row, modeling a transient
// catalog read failure while proving the session's bound release. Every other
// row, including the committed one, reads normally.
type faultingBoundRowResolver struct {
	inner  FilePathResolver
	failID int
	err    error
}

func (r *faultingBoundRowResolver) GetByID(ctx context.Context, id int) (*models.MediaFile, error) {
	if id == r.failID {
		return nil, r.err
	}
	return r.inner.GetByID(ctx, id)
}

func (r *faultingBoundRowResolver) GetByPath(ctx context.Context, path string) (*models.MediaFile, error) {
	if pr, ok := r.inner.(interface {
		GetByPath(context.Context, string) (*models.MediaFile, error)
	}); ok {
		return pr.GetByPath(ctx, path)
	}
	return nil, errors.New("no path resolver")
}

// TestDeferredProbePropagationReadFailureDoesNotPublishVerified is the
// deadline/read-failure regression: when the bound-row read that proves release
// continuity fails, the committed inventory must not be published as verified.
// The durable write itself landed, so before the fix the session recorded
// verified while the carried overlay still held the provisional plan-time menu.
func TestDeferredProbePropagationReadFailureDoesNotPublishVerified(t *testing.T) {
	identity := canonicalCollectionIdentity()
	fx := setupCanonicalCollectionFixture(t, identity, identity)

	verifiedAudio, verifiedSubs := verifiedCanonicalAudio(), verifiedCanonicalSubtitles()
	probedAt := time.Now().UTC()
	fx.handler.VirtualFileMetadataSaver = func(_ context.Context, args models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
		var audio []models.AudioTrack
		if err := json.Unmarshal(args.AudioTracks, &audio); err != nil {
			return VirtualFileMetadataUpdateResult{}, err
		}
		var subs []models.SubtitleTrack
		if len(args.SubtitleTracks) > 0 {
			if err := json.Unmarshal(args.SubtitleTracks, &subs); err != nil {
				return VirtualFileMetadataUpdateResult{}, err
			}
		}
		fx.resolver.update(args.FileID, func(f *models.MediaFile) {
			f.AudioTracks = audio
			f.SubtitleTracks = subs
			f.ProbeUpdatedAt = &probedAt
		})
		return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
	}
	fx.handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		f.AudioTracks = verifiedAudio
		f.SubtitleTracks = verifiedSubs
		f.CodecAudio = "eac3"
		return f, nil
	}
	// The committed row (the collection sibling) reads fine; the session's bound
	// row cannot be read, so release continuity cannot be proven.
	fx.handler.fileResolver = &faultingBoundRowResolver{
		inner:  fx.resolver,
		failID: canonicalPinnedRowID,
		err:    errors.New("catalog unavailable"),
	}

	deferred := readmitDeferredProbe(t, fx.handler, canonicalCollectionProbe(fx.session, *fx.canonicalRow, identity))
	fx.handler.runDeferredVirtualProbe(deferred)

	// The durable write landed on the collection row: this is a propagation
	// failure, not a write failure.
	committedRow, err := fx.resolver.GetByID(context.Background(), canonicalCollectionRowID)
	if err != nil || committedRow == nil || len(committedRow.AudioTracks) != 2 {
		t.Fatalf("durable write did not land on the collection row: %+v (err %v)", committedRow, err)
	}

	live, err := fx.manager.GetSession(fx.session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome == probeOutcomeVerified {
		t.Fatalf("unproven propagation published a verified session outcome")
	}
	if len(live.VirtualAudioTracks) != 1 || live.VirtualAudioTracks[0].Language != "jpn" {
		t.Fatalf("carried audio = %+v, want the untouched plan-time jpn overlay", live.VirtualAudioTracks)
	}
}

// TestDeferredProbePropagationIgnoresInPlaceAdoptionReread is the
// adoption-during-reread regression: the committed row is mutated in place by
// another release after the durable write, so a reread would supply that
// release's tracks under an unchanged binding generation. The propagation must
// carry the inventory the probe actually committed, never the reread.
func TestDeferredProbePropagationIgnoresInPlaceAdoptionReread(t *testing.T) {
	const fileID = 5272909
	candidateURI := "virtual://movie/tt-adopt-reread?result=cand-1"
	planAudio := []models.AudioTrack{{Index: 1, Codec: "eac3", Channels: 6, Language: "jpn"}}
	committedAudio := []models.AudioTrack{
		{Index: 1, Codec: "eac3", Channels: 6, Language: "jpn", Default: true},
		{Index: 2, Codec: "aac", Channels: 2, Language: "eng"},
	}
	// Another release adopted the same row in place after the write: a reread
	// would return these tracks.
	otherReleaseAudio := []models.AudioTrack{
		{Index: 7, Codec: "opus", Channels: 2, Language: "deu"},
	}

	source := &models.MediaFile{
		ID:                         fileID,
		ContentID:                  "movie-adopt-reread",
		FilePath:                   candidateURI,
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
		AudioTracks:                planAudio,
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
		VirtualSourceURI:              candidateURI,
		VirtualSourceSet:              true,
		VirtualSubtitleEvidenceSet:    true,
		VirtualAudioTracks:            planAudio,
		VirtualSubtitleEvidenceURI:    candidateURI,
		VirtualSubtitleEvidenceFileID: fileID,
	}); err != nil {
		t.Fatalf("UpdateStreamState: %v", err)
	}
	resolver := &syncPlaybackFileResolver{file: *source}
	handler := NewPlaybackHandler(manager, resolver)
	handler.InstallationID = "test-install"
	serviceCtx, serviceCancel := context.WithCancel(context.Background())
	handler.ServiceContext = serviceCtx
	t.Cleanup(serviceCancel)
	handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		f.AudioTracks = committedAudio
		f.CodecAudio = "eac3"
		return f, nil
	}
	handler.VirtualFileMetadataSaver = func(_ context.Context, args models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
		var audio []models.AudioTrack
		if err := json.Unmarshal(args.AudioTracks, &audio); err != nil {
			return VirtualFileMetadataUpdateResult{}, err
		}
		probedAt := time.Now().UTC()
		resolver.update(func(f *models.MediaFile) {
			f.AudioTracks = audio
			f.CodecAudio = args.CodecAudio
			f.ProbeUpdatedAt = &probedAt
		})
		// A concurrent writer adopts the row in place to another release. A
		// reread of the committed row would return the wrong release's tracks.
		resolver.update(func(f *models.MediaFile) {
			f.AudioTracks = otherReleaseAudio
		})
		return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
	}

	deferred := readmitDeferredProbe(t, handler, deferredOverflowProbe(session, candidateURI))
	handler.runDeferredVirtualProbe(deferred)

	live, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if len(live.VirtualAudioTracks) != len(committedAudio) || live.VirtualAudioTracks[0].Language != "jpn" || live.VirtualAudioTracks[1].Language != "eng" {
		t.Fatalf("carried audio = %+v, want the probe's committed 2 tracks, not the adopted release's reread", live.VirtualAudioTracks)
	}
	row, err := resolver.GetByID(context.Background(), fileID)
	if err != nil || row == nil {
		t.Fatalf("GetByID: %v", err)
	}
	if len(row.AudioTracks) != 1 || row.AudioTracks[0].Language != "deu" {
		t.Fatalf("fixture row = %+v, want the in-place adopted release's tracks", row.AudioTracks)
	}
}

// rotatingBoundRowResolver rotates the session binding the first time the bound
// row is read, which is the last validation step before the generation-fenced
// session write. It models a rotation landing between validation and commit.
type rotatingBoundRowResolver struct {
	inner    FilePathResolver
	rotateID int
	rotate   func()
	rotated  bool
}

func (r *rotatingBoundRowResolver) GetByID(ctx context.Context, id int) (*models.MediaFile, error) {
	if id == r.rotateID && !r.rotated {
		r.rotated = true
		r.rotate()
	}
	return r.inner.GetByID(ctx, id)
}

func (r *rotatingBoundRowResolver) GetByPath(ctx context.Context, path string) (*models.MediaFile, error) {
	if pr, ok := r.inner.(interface {
		GetByPath(context.Context, string) (*models.MediaFile, error)
	}); ok {
		return pr.GetByPath(ctx, path)
	}
	return nil, errors.New("no path resolver")
}

// TestDeferredProbePropagationRotationBetweenFenceAndCommitFenced is the
// rotation-between-validation-and-commit regression: the fence validates the
// old generation, then a rotation lands before the session write. The write's
// generation CAS must reject the superseded inventory so the replacement binding
// keeps its own lifecycle.
func TestDeferredProbePropagationRotationBetweenFenceAndCommitFenced(t *testing.T) {
	identity := canonicalCollectionIdentity()
	fx := setupCanonicalCollectionFixture(t, identity, identity)

	committed := virtualCommittedProbeInventory{
		fileID:       canonicalCollectionRowID,
		candidateURI: canonicalPinnedURI,
		identity:     identity.tiers(),
		audio:        verifiedCanonicalAudio(),
		subtitles:    verifiedCanonicalSubtitles(),
	}
	deferred := readmitDeferredProbe(t, fx.handler, canonicalCollectionProbe(fx.session, *fx.canonicalRow, identity))

	newURI := canonicalCollectionURI + "&result=ffffffff"
	fx.handler.fileResolver = &rotatingBoundRowResolver{
		inner:    fx.resolver,
		rotateID: canonicalPinnedRowID,
		rotate: func() {
			if err := fx.manager.SetVirtualSource(fx.session.ID, newURI, 5); err != nil {
				t.Errorf("SetVirtualSource: %v", err)
			}
		},
	}

	if disp := fx.handler.propagateVerifiedProbeInventory(deferred, committed); disp != probeInventorySuperseded {
		t.Fatalf("propagation disposition = %d, want superseded after a rotation before commit", disp)
	}
	live, err := fx.manager.GetSession(fx.session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualSourceURI != newURI {
		t.Fatalf("binding = %q, want the rotated candidate %q", live.VirtualSourceURI, newURI)
	}
	if live.VirtualProbeOutcome != "" {
		t.Fatalf("superseded propagation wrote outcome %q onto the replacement binding; want empty", live.VirtualProbeOutcome)
	}
	if len(live.VirtualAudioTracks) != 1 || live.VirtualAudioTracks[0].Language != "jpn" {
		t.Fatalf("carried audio = %+v, want the untouched plan-time overlay", live.VirtualAudioTracks)
	}
}

// rowIDMutatingResolver mutates the session's effective row id the first time
// the bound row is read — the last validation step before the fenced session
// write — without moving the binding generation. It models a
// SetEffectiveMediaFileID row-id-only rotation landing between validate and
// commit, which a generation-only fence cannot see.
type rowIDMutatingResolver struct {
	inner   FilePathResolver
	readID  int
	mutate  func()
	mutated bool
}

func (r *rowIDMutatingResolver) GetByID(ctx context.Context, id int) (*models.MediaFile, error) {
	if id == r.readID && !r.mutated {
		r.mutated = true
		r.mutate()
	}
	return r.inner.GetByID(ctx, id)
}

func (r *rowIDMutatingResolver) GetByPath(ctx context.Context, path string) (*models.MediaFile, error) {
	if pr, ok := r.inner.(interface {
		GetByPath(context.Context, string) (*models.MediaFile, error)
	}); ok {
		return pr.GetByPath(ctx, path)
	}
	return nil, errors.New("no path resolver")
}

// TestDeferredProbePropagationRowIDMutationBetweenFenceAndCommitFenced is the
// row-id-only mutation regression: the fence validates the session's bound row,
// then SetEffectiveMediaFileID moves the effective row without advancing the
// binding generation, and only then does the propagation commit. The commit must
// reject the validated inventory rather than mark it verified onto the changed
// binding, so the full-binding CAS compares the effective row id and not just
// the generation.
func TestDeferredProbePropagationRowIDMutationBetweenFenceAndCommitFenced(t *testing.T) {
	identity := canonicalCollectionIdentity()
	fx := setupCanonicalCollectionFixture(t, identity, identity)

	committed := virtualCommittedProbeInventory{
		fileID:       canonicalCollectionRowID,
		candidateURI: canonicalPinnedURI,
		identity:     identity.tiers(),
		audio:        verifiedCanonicalAudio(),
		subtitles:    verifiedCanonicalSubtitles(),
	}
	deferred := readmitDeferredProbe(t, fx.handler, canonicalCollectionProbe(fx.session, *fx.canonicalRow, identity))

	fx.handler.fileResolver = &rowIDMutatingResolver{
		inner:  fx.resolver,
		readID: canonicalPinnedRowID,
		mutate: func() {
			if err := fx.manager.SetEffectiveMediaFileID(fx.session.ID, canonicalCollectionRowID); err != nil {
				t.Errorf("SetEffectiveMediaFileID: %v", err)
			}
		},
	}

	if disp := fx.handler.propagateVerifiedProbeInventory(deferred, committed); disp != probeInventorySuperseded {
		t.Fatalf("propagation disposition = %d, want superseded after a row-id-only mutation before commit", disp)
	}
	live, err := fx.manager.GetSession(fx.session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.MediaFileID != canonicalCollectionRowID {
		t.Fatalf("effective row = %d, want the mutated row %d", live.MediaFileID, canonicalCollectionRowID)
	}
	if live.VirtualProbeOutcome == probeOutcomeVerified {
		t.Fatalf("row-id mutation let the probe commit a verified outcome onto the changed binding")
	}
	if len(live.VirtualAudioTracks) != 1 || live.VirtualAudioTracks[0].Language != "jpn" {
		t.Fatalf("carried audio = %+v, want the untouched plan-time overlay", live.VirtualAudioTracks)
	}
}

// TestDeferredProbeExplicitSelectionPreservedAcrossOverlaySwap is the explicit
// viewer-selection case: a reorder must not move the viewer's chosen track, the
// remux selector must keep naming its stream, and the reconcile must record a
// no-op rather than override the explicit choice.
func TestDeferredProbeExplicitSelectionPreservedAcrossOverlaySwap(t *testing.T) {
	const fileID = 5272911
	candidateURI := "virtual://movie/tt-explicit-overlay?result=cand-1"
	planTime := []models.AudioTrack{
		{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
		{Index: 5, Language: "de", Codec: "eac3", Channels: 6, Layout: "5.1"},
		{Index: 3, Language: "pt", Codec: "eac3", Channels: 6, Layout: "5.1"},
	}
	// The verified list reorders de from position 1 to position 0.
	verified := []models.AudioTrack{
		{Index: 5, Language: "de", Codec: "eac3", Channels: 6, Layout: "5.1"},
		{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
		{Index: 3, Language: "pt", Codec: "eac3", Channels: 6, Layout: "5.1"},
	}
	const explicitIndex = 1 // de in planTime
	deSignature := playback.AudioTrackSignatureFromTrack(planTime[explicitIndex])

	source := &models.MediaFile{
		ID: fileID, ContentID: "movie-explicit-overlay", FilePath: candidateURI, Container: "virtual",
		CodecVideo: "h264", Resolution: "1080p", Duration: 3600, VirtualOwnerInstallationID: 5,
		VideoTracks: []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080, BitDepth: 8}},
		AudioTracks: planTime,
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
		AudioTrackIndex:               explicitIndex,
		VirtualSourceURI:              candidateURI,
		VirtualSourceSet:              true,
		VirtualSubtitleEvidenceSet:    true,
		VirtualAudioTracks:            planTime,
		VirtualSubtitleEvidenceURI:    candidateURI,
		VirtualSubtitleEvidenceFileID: fileID,
		SelectionOriginSet:            true,
		SelectionOrigin:               SelectionOriginExplicit,
		SelectedAudioSignature:        deSignature,
	}); err != nil {
		t.Fatalf("UpdateStreamState: %v", err)
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
		f.AudioTracks = verified
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
		PlaybackAttemptID:      "attempt-explicit-overlay",
		SessionID:              session.ID,
		UserID:                 1,
		ProfileID:              "profile-1",
		RequestedMediaFileID:   fileID,
		EffectiveMediaFileID:   fileID,
		ExpiresAt:              time.Now().Add(time.Hour),
		CurrentPlanID:          "plan-explicit-overlay",
		SelectionOrigin:        SelectionOriginExplicit,
		SelectedAudioSignature: deSignature,
		NormalizedRequest: playback.StartRequestV3{
			ClientFeatures: []string{playback.FeaturePlanInvalidatedV3, playback.FeatureDefaultAudioReconcileResponseV3},
			Capabilities:   reconcileCapabilities(),
		},
	}
	record.CurrentPlan.SelectedTracks.Audio = &playback.TrackIdentityV3{
		ID:    playback.TrackIDV3(fileID, "audio", explicitIndex),
		Index: intPtrReconcile(explicitIndex),
	}
	record.CurrentPlan.PlanAttemptKey = "v3:plan-explicit-overlay:a:b"
	if err := handler.PlanStoreV3.SaveAttempt(context.Background(), record); err != nil {
		t.Fatalf("SaveAttempt: %v", err)
	}

	deferred := readmitDeferredProbe(t, handler, deferredOverflowProbe(session, candidateURI))
	handler.runDeferredVirtualProbe(deferred)

	live, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.AudioTrackIndex < 0 || live.AudioTrackIndex >= len(live.VirtualAudioTracks) {
		t.Fatalf("session audio index %d out of range for verified %+v", live.AudioTrackIndex, live.VirtualAudioTracks)
	}
	if got := live.VirtualAudioTracks[live.AudioTrackIndex].Language; got != "de" {
		t.Fatalf("session audio index now names %q, want the explicit de track; verified = %+v", got, live.VirtualAudioTracks)
	}
	// The running mux's stream ordinal is unchanged: de keeps 0:a:2.
	planBound := &models.MediaFile{AudioTracks: planTime}
	_, committedOrdinal := transportOrdinalForTrack(t, planBound, "de")
	boundRow, err := resolver.GetByID(context.Background(), live.MediaFileID)
	if err != nil || boundRow == nil {
		t.Fatalf("GetByID(bound): %v", err)
	}
	bound := bindSessionVirtualSourceWithTracks(context.Background(), boundRow, live, resolver)
	if got := audioStreamOrdinalV3(bound, live.AudioTrackIndex); got != committedOrdinal {
		t.Fatalf("explicit selection moved the remux stream to 0:a:%d, want 0:a:%d", got, committedOrdinal)
	}
	after, err := handler.PlanStoreV3.GetAttempt(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("GetAttempt: %v", err)
	}
	generation := reconcileGenerationV3(&models.MediaFile{AudioTracks: verified})
	entry := playback.FindAudioReconcileEntry(after.AudioReconcileLedger, generation, session.ID)
	if entry != nil && entry.Decision == playback.AudioReconcileInvalidated {
		t.Fatalf("reconcile overrode an explicit selection that survived the reorder: %#v", entry)
	}
}

// flakyBoundRowResolver fails the bound row's read for the first `remaining`
// reads, then delegates. It models a transient catalog read failure that clears
// within the propagation retry bound.
type flakyBoundRowResolver struct {
	inner     FilePathResolver
	failID    int
	err       error
	remaining int
}

func (r *flakyBoundRowResolver) GetByID(ctx context.Context, id int) (*models.MediaFile, error) {
	if id == r.failID && r.remaining > 0 {
		r.remaining--
		return nil, r.err
	}
	return r.inner.GetByID(ctx, id)
}

func (r *flakyBoundRowResolver) GetByPath(ctx context.Context, path string) (*models.MediaFile, error) {
	if pr, ok := r.inner.(interface {
		GetByPath(context.Context, string) (*models.MediaFile, error)
	}); ok {
		return pr.GetByPath(ctx, path)
	}
	return nil, errors.New("no path resolver")
}

// canonicalProbeWithVerifiedWrite wires a successful prober and durable write on
// the canonical fixture, so a test can drive the full deferred worker path.
func canonicalProbeWithVerifiedWrite(t *testing.T, fx *canonicalCollectionFixture) {
	t.Helper()
	probedAt := time.Now().UTC()
	fx.handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		f.AudioTracks = verifiedCanonicalAudio()
		f.SubtitleTracks = verifiedCanonicalSubtitles()
		f.CodecAudio = "eac3"
		return f, nil
	}
	fx.handler.VirtualFileMetadataSaver = func(_ context.Context, args models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
		var audio []models.AudioTrack
		if err := json.Unmarshal(args.AudioTracks, &audio); err != nil {
			return VirtualFileMetadataUpdateResult{}, err
		}
		var subs []models.SubtitleTrack
		if len(args.SubtitleTracks) > 0 {
			if err := json.Unmarshal(args.SubtitleTracks, &subs); err != nil {
				return VirtualFileMetadataUpdateResult{}, err
			}
		}
		fx.resolver.update(args.FileID, func(f *models.MediaFile) {
			f.AudioTracks = audio
			f.SubtitleTracks = subs
			f.CodecAudio = args.CodecAudio
			f.ProbeUpdatedAt = &probedAt
		})
		return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
	}
}

// TestDeferredProbePropagationExhaustedBoundReadFailsTerminally is the
// persistent/deadline regression for the third P2 item: when the bound-row read
// that proves release continuity fails through the whole retry bound (here with
// a deadline), the session must not be left pending. The probe committed, but
// the binding terminally fails so a client watching the poll or the push leaves
// its loading state; the carried provisional overlay is untouched.
func TestDeferredProbePropagationExhaustedBoundReadFailsTerminally(t *testing.T) {
	identity := canonicalCollectionIdentity()
	fx := setupCanonicalCollectionFixture(t, identity, identity)
	canonicalProbeWithVerifiedWrite(t, fx)
	fx.handler.fileResolver = &faultingBoundRowResolver{
		inner:  fx.resolver,
		failID: canonicalPinnedRowID,
		err:    context.DeadlineExceeded,
	}

	deferred := readmitDeferredProbe(t, fx.handler, canonicalCollectionProbe(fx.session, *fx.canonicalRow, identity))
	fx.handler.runDeferredVirtualProbe(deferred)

	live, err := fx.manager.GetSession(fx.session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome != probeOutcomeFailed {
		t.Fatalf("exhausted bound reads left outcome %q, want the terminal failed so the client leaves loading", live.VirtualProbeOutcome)
	}
	if len(live.VirtualAudioTracks) != 1 || live.VirtualAudioTracks[0].Language != "jpn" {
		t.Fatalf("carried audio = %+v, want the untouched provisional overlay", live.VirtualAudioTracks)
	}
	// The catalog outage is over: the poll must now report a terminal failed
	// status so the client leaves loading instead of staying pending.
	fx.handler.fileResolver = fx.resolver
	inventory, err := fx.handler.GetPlaybackInventoryV2(newAuthorizedPlaybackContext(),
		PlaybackCaller{UserID: 1, ProfileID: "profile-1", InstallationID: "test-install"}, fx.session.ID)
	if err != nil {
		t.Fatalf("GetPlaybackInventoryV2: %v", err)
	}
	if inventory.InventoryStatus != string(ProbeProvenanceFailed) {
		t.Fatalf("polled inventory status = %q, want failed", inventory.InventoryStatus)
	}
}

// TestDeferredProbePropagationTransientBoundReadRecovers is the transient case:
// a bound-row read that fails once and then succeeds within the retry bound must
// still propagate the committed inventory and record verified, never a terminal
// failure.
func TestDeferredProbePropagationTransientBoundReadRecovers(t *testing.T) {
	identity := canonicalCollectionIdentity()
	fx := setupCanonicalCollectionFixture(t, identity, identity)
	canonicalProbeWithVerifiedWrite(t, fx)
	fx.handler.fileResolver = &flakyBoundRowResolver{
		inner:     fx.resolver,
		failID:    canonicalPinnedRowID,
		err:       errors.New("catalog unavailable"),
		remaining: 1,
	}

	deferred := readmitDeferredProbe(t, fx.handler, canonicalCollectionProbe(fx.session, *fx.canonicalRow, identity))
	fx.handler.runDeferredVirtualProbe(deferred)

	live, err := fx.manager.GetSession(fx.session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualProbeOutcome != probeOutcomeVerified {
		t.Fatalf("transient read failure left outcome %q, want verified after recovery", live.VirtualProbeOutcome)
	}
	if len(live.VirtualAudioTracks) != len(verifiedCanonicalAudio()) {
		t.Fatalf("carried audio = %+v, want the committed verified inventory", live.VirtualAudioTracks)
	}
}

// TestDeferredProbeFailureWriteFencedOnRowOnlyMutation is the failure-write
// analogue of the row-id-mutation case: when the propagation refuses the
// committed inventory and a row-only move lands while the release-continuity read
// runs, the terminal failed write must be fenced on the full binding. A
// generation-only fence would stamp the old probe's failure onto the changed
// binding.
func TestDeferredProbeFailureWriteFencedOnRowOnlyMutation(t *testing.T) {
	canonicalIdentity := virtualProbeIdentity{VideoHash: "hash-canonical", ReleaseName: "Movie.2024.1080p.WEB-DL.x264", ReleaseSize: 8_000_000_000}
	pinnedIdentity := virtualProbeIdentity{VideoHash: "hash-pinned", ReleaseName: "Movie.2024.1080p.WEB-DL.x264", ReleaseSize: 8_000_000_000}
	// Conflicting identities make the release-continuity check refuse.
	fx := setupCanonicalCollectionFixture(t, canonicalIdentity, pinnedIdentity)
	canonicalProbeWithVerifiedWrite(t, fx)

	deferred := readmitDeferredProbe(t, fx.handler, canonicalCollectionProbe(fx.session, *fx.canonicalRow, canonicalIdentity))

	fx.handler.fileResolver = &rowIDMutatingResolver{
		inner:  fx.resolver,
		readID: canonicalPinnedRowID,
		mutate: func() {
			if err := fx.manager.SetEffectiveMediaFileID(fx.session.ID, canonicalCollectionRowID); err != nil {
				t.Errorf("SetEffectiveMediaFileID: %v", err)
			}
		},
	}

	fx.handler.runDeferredVirtualProbe(deferred)

	live, err := fx.manager.GetSession(fx.session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.MediaFileID != canonicalCollectionRowID {
		t.Fatalf("effective row = %d, want the mutated row %d", live.MediaFileID, canonicalCollectionRowID)
	}
	if live.VirtualProbeOutcome == probeOutcomeFailed {
		t.Fatal("a row-only move during the release-continuity read let the old probe's failure land on the changed binding")
	}
}

// transportOrdinalForTrack is the audio-only stream ordinal a bound list assigns
// to a chosen language's first track, mirroring what the transport hands ffmpeg
// as 0:a:N.
func transportOrdinalForTrack(t *testing.T, file *models.MediaFile, language string) (int, int) {
	t.Helper()
	index := -1
	for i, track := range file.AudioTracks {
		if track.Language == language {
			index = i
			break
		}
	}
	if index < 0 {
		t.Fatalf("no %q track in %+v", language, file.AudioTracks)
	}
	return index, audioStreamOrdinalV3(file, index)
}

// TestDeferredProbeOverlayTransportOrdinalCoherentAfterReorderAndRemoval
// demonstrates transport coherence, not just list agreement: after the deferred
// probe replaces the carried overlay with a reordered or trimmed verified list,
// the transport's committed stream ordinal is re-derived against that same list
// by the default-audio reconcile, so 0:a:N still names the committed track's
// stream. The active transport was muxed against the plan-time list, so a shift
// must be corrected rather than silently left pointing at another stream.
func TestDeferredProbeOverlayTransportOrdinalCoherentAfterReorderAndRemoval(t *testing.T) {
	cases := []struct {
		name            string
		planTime        []models.AudioTrack
		verified        []models.AudioTrack
		committed       int
		wantInvalidated bool
	}{
		{
			name: "reorder preserves the committed stream ordinal",
			planTime: []models.AudioTrack{
				{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
				{Index: 3, Language: "pt", Codec: "eac3", Channels: 6, Layout: "5.1"},
			},
			// The array position of pt moved from 1 to 0, but its container
			// stream index (3) did not, so 0:a:1 still names pt and the running
			// transport needs no correction.
			verified: []models.AudioTrack{
				{Index: 3, Language: "pt", Codec: "eac3", Channels: 6, Layout: "5.1"},
				{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
			},
			committed:       1,
			wantInvalidated: false,
		},
		{
			name: "removal below the committed ordinal shifts it",
			planTime: []models.AudioTrack{
				{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
				{Index: 3, Language: "pt", Codec: "eac3", Channels: 6, Layout: "5.1"},
				{Index: 5, Language: "de", Codec: "ac3", Channels: 2, Layout: "stereo"},
			},
			// en is removed, so pt's 0:a:N ordinal shifts from 1 to 0: the
			// running transport must be corrected or it plays the wrong stream.
			verified: []models.AudioTrack{
				{Index: 3, Language: "pt", Codec: "eac3", Channels: 6, Layout: "5.1"},
				{Index: 5, Language: "de", Codec: "ac3", Channels: 2, Layout: "stereo"},
			},
			committed:       1,
			wantInvalidated: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const fileID = 5272910
			candidateURI := "virtual://movie/tt-transport-coherent?result=cand-1"

			source := &models.MediaFile{
				ID:                         fileID,
				ContentID:                  "movie-transport-coherent",
				FilePath:                   candidateURI,
				Container:                  "virtual",
				CodecVideo:                 "h264",
				Resolution:                 "1080p",
				Duration:                   3600,
				VirtualOwnerInstallationID: 5,
				VideoTracks:                []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080, BitDepth: 8}},
				AudioTracks:                tc.planTime,
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
				AudioTrackIndex:               tc.committed,
				VirtualSourceURI:              candidateURI,
				VirtualSourceSet:              true,
				VirtualSubtitleEvidenceSet:    true,
				VirtualAudioTracks:            tc.planTime,
				VirtualSubtitleEvidenceURI:    candidateURI,
				VirtualSubtitleEvidenceFileID: fileID,
				SelectionOriginSet:            true,
				SelectionOrigin:               SelectionOriginAuto,
				PreferredAudioLanguage:        "pt",
				SelectedAudioSignature:        playback.AudioTrackSignatureFromTrack(tc.planTime[tc.committed]),
			}); err != nil {
				t.Fatalf("UpdateStreamState: %v", err)
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
				f.AudioTracks = tc.verified
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
				PlaybackAttemptID:      "attempt-transport-coherent",
				SessionID:              session.ID,
				UserID:                 1,
				ProfileID:              "profile-1",
				RequestedMediaFileID:   fileID,
				EffectiveMediaFileID:   fileID,
				ExpiresAt:              time.Now().Add(time.Hour),
				CurrentPlanID:          "plan-transport-coherent",
				SelectionOrigin:        SelectionOriginAuto,
				PreferredAudioLanguage: "pt",
				SelectedAudioSignature: playback.AudioTrackSignatureFromTrack(tc.planTime[tc.committed]),
				NormalizedRequest: playback.StartRequestV3{
					ClientFeatures: []string{playback.FeaturePlanInvalidatedV3, playback.FeatureDefaultAudioReconcileResponseV3},
					Capabilities:   reconcileCapabilities(),
				},
			}
			record.CurrentPlan.SelectedTracks.Audio = &playback.TrackIdentityV3{
				ID:    playback.TrackIDV3(fileID, "audio", tc.committed),
				Index: intPtrReconcile(tc.committed),
			}
			record.CurrentPlan.PlanAttemptKey = "v3:plan-transport-coherent:a:b"
			if err := handler.PlanStoreV3.SaveAttempt(context.Background(), record); err != nil {
				t.Fatalf("SaveAttempt: %v", err)
			}

			deferred := readmitDeferredProbe(t, handler, deferredOverflowProbe(session, candidateURI))
			handler.runDeferredVirtualProbe(deferred)

			live, err := manager.GetSession(session.ID)
			if err != nil {
				t.Fatalf("GetSession: %v", err)
			}
			// The overlay write carries the verified list and preserves the
			// selected track's identity: the session's index must name the same
			// committed track (pt) in the NEW list, so the transport's derived
			// 0:a:N still names the committed stream.
			if len(live.VirtualAudioTracks) != len(tc.verified) {
				t.Fatalf("carried audio = %+v, want the verified list", live.VirtualAudioTracks)
			}
			if live.AudioTrackIndex < 0 || live.AudioTrackIndex >= len(live.VirtualAudioTracks) {
				t.Fatalf("session audio index %d out of range for verified %+v", live.AudioTrackIndex, live.VirtualAudioTracks)
			}
			if got := live.VirtualAudioTracks[live.AudioTrackIndex].Language; got != "pt" {
				t.Fatalf("session audio index now names %q, want the committed pt track; verified = %+v", got, live.VirtualAudioTracks)
			}
			boundRow, err := resolver.GetByID(context.Background(), live.MediaFileID)
			if err != nil || boundRow == nil {
				t.Fatalf("GetByID(bound): %v", err)
			}
			bound := bindSessionVirtualSourceWithTracks(context.Background(), boundRow, live, resolver)
			if len(bound.AudioTracks) != len(tc.verified) {
				t.Fatalf("serve bind audio = %+v, want the verified overlay", bound.AudioTracks)
			}

			// The real remux selector: production hands ffmpeg
			// audioStreamOrdinalV3(file, session.AudioTrackIndex). After the
			// swap it must still name the committed pt stream; a removal that
			// shifts its ordinal is the reconciliation's correction, handled
			// below.
			planBound := &models.MediaFile{AudioTracks: tc.planTime}
			_, committedOrdinal := transportOrdinalForTrack(t, planBound, "pt")
			_, verifiedOrdinal := transportOrdinalForTrack(t, bound, "pt")
			if got := audioStreamOrdinalV3(bound, live.AudioTrackIndex); got != verifiedOrdinal {
				t.Fatalf("remux selects 0:a:%d after the overlay swap, want the committed pt stream 0:a:%d", got, verifiedOrdinal)
			}

			after, err := handler.PlanStoreV3.GetAttempt(context.Background(), session.ID)
			if err != nil {
				t.Fatalf("GetAttempt: %v", err)
			}
			// The deferred reconcile now decides against the exact committed
			// carrier, so its generation is the committed inventory's fingerprint.
			generation := reconcileGenerationV3(&models.MediaFile{AudioTracks: tc.verified})
			entry := playback.FindAudioReconcileEntry(after.AudioReconcileLedger, generation, session.ID)

			if !tc.wantInvalidated {
				// The committed track keeps its audio-only stream ordinal, so
				// the transport's 0:a:N still names it and no correction is
				// owed. The array reorder alone must not move the running mux.
				if committedOrdinal != verifiedOrdinal {
					t.Fatalf("committed stream ordinal %d != verified %d in a case expected to preserve it", committedOrdinal, verifiedOrdinal)
				}
				if entry != nil && entry.Decision == playback.AudioReconcileInvalidated {
					t.Fatalf("reconcile invalidated an unchanged executable selection: %#v", entry)
				}
				return
			}
			// The committed track's stream ordinal shifted, so the reconcile
			// must point the transport at that same track's new ordinal rather
			// than let 0:a:N name another stream.
			if entry == nil || entry.Decision != playback.AudioReconcileInvalidated {
				t.Fatalf("reconcile did not correct the shifted transport ordinal (ledger = %#v)", after.AudioReconcileLedger)
			}
			if entry.AudioIndex == nil || *entry.AudioIndex < 0 || *entry.AudioIndex >= len(bound.AudioTracks) {
				t.Fatalf("reconcile correction index = %v, out of range for %+v", entry.AudioIndex, bound.AudioTracks)
			}
			if lang := bound.AudioTracks[*entry.AudioIndex].Language; lang != "pt" {
				t.Fatalf("corrected track = %q, want the committed pt track", lang)
			}
			if got := audioStreamOrdinalV3(bound, *entry.AudioIndex); got != verifiedOrdinal {
				t.Fatalf("corrected 0:a:%d = %d, want the committed track's verified ordinal %d", *entry.AudioIndex, got, verifiedOrdinal)
			}
		})
	}
}
