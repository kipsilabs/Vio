package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// newPinnedTrackChangeFixture drives a real virtual start that takes the P0
// fast path: the catalog row already owns the concrete ?result= candidate with
// complete probed evidence and a probe stamp, so the start neither lists nor
// probes and the session binds to that pinned candidate with its inventory
// captured as plan-time evidence.
type pinnedTrackChangeFixture struct {
	handler     *PlaybackHandler
	manager     *playback.SessionManager
	source      *models.MediaFile
	files       map[int]*models.MediaFile
	pinnedURI   string
	start       playback.StartRequestV3
	started     playback.DecisionResponseV3
	detailCalls int
	listCalls   int
	probeCalls  int
}

func newPinnedTrackChangeFixture(t *testing.T, churnedResultToken string) *pinnedTrackChangeFixture {
	t.Helper()
	source := v3HandlerFixtureFile(t)
	source.ID = 720
	source.ContentID = "movie-pinned-trackchange"
	source.FilePath = "virtual://movie/tt-pinned-trackchange"
	source.VirtualOwnerInstallationID = 5
	source.AudioTracks = []models.AudioTrack{
		{Index: 0, Codec: "aac", Channels: 2, Layout: "stereo", Language: "eng", Default: true},
		{Index: 1, Codec: "aac", Channels: 2, Layout: "stereo", Language: "spa"},
	}
	probedAt := time.Now().Add(-time.Hour)
	source.ProbeUpdatedAt = &probedAt
	pinnedURI := withVirtualResultKey(source.FilePath, "pinned")
	source.FilePath = pinnedURI

	fx := &pinnedTrackChangeFixture{
		manager:   playback.NewSessionManager(0, 0),
		source:    source,
		files:     map[int]*models.MediaFile{source.ID: source},
		pinnedURI: pinnedURI,
	}
	fx.handler = NewPlaybackHandler(fx.manager, mapPlaybackFileResolver{files: fx.files})
	stubCopySeekAnchorV3(fx.handler)
	fx.handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"allow_4k_transcode": "true"}}
	fx.handler.ItemAccess = allowAllPlaybackItemAccess{}
	fx.handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(_ context.Context, virtualURI string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		fx.detailCalls++
		return ResolvedVirtualMedia{URL: "http://127.0.0.1:9/stream?path=" + virtualURI, URI: virtualURI, CandidateID: virtualResultCandidateID(virtualURI)}, nil
	})
	fx.handler.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
		return "http://127.0.0.1:9/stream?path=" + path, nil
	})
	fx.handler.VirtualPlaybackStreamLister = VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
		fx.listCalls++
		return []VirtualPlaybackStream{{
			ID: "churned", URI: withVirtualResultKey(source.FilePath, churnedResultToken),
			Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac", Container: "mp4",
		}}, nil
	})
	fx.handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		fx.probeCalls++
		f.VideoTracks = source.VideoTracks
		f.AudioTracks = source.AudioTracks
		f.CodecVideo, f.CodecAudio, f.Resolution, f.Container = "h264", "aac", "1080p", "mp4"
		return f, nil
	}

	fx.start = v3HandlerStartRequest()
	fx.start.FileID = source.ID
	fx.start.QualityPreference = "auto"
	// Advertise in-place audio-track selection on the direct route so a
	// non-default audio pick stays on the source-preserving transport instead
	// of forcing a progressive remux. The pinned candidate, row and route must
	// not move; only the selected track changes.
	direct := fx.start.ClientPlaybackContext.Deliveries[playback.DeliveryClassOriginalHTTPV3]
	direct.ValidatedClaims = append(direct.ValidatedClaims, playback.ClaimClientSelectedAudioTrackV3)
	fx.start.ClientPlaybackContext.Deliveries[playback.DeliveryClassOriginalHTTPV3] = direct
	fx.start.ClientPlaybackContext.Deliveries[playback.DeliveryClassHLSV3] = playback.DeliveryCapabilityV3{
		Enabled: true, SupportedOnDevice: true, Containers: []string{"hls"}, VideoCodecs: []string{"h264"}, AudioDecodeCodecs: []string{"aac"},
	}
	fx.start.ClientPlaybackContext.Deliveries[playback.DeliveryClassProgressiveV3] = playback.DeliveryCapabilityV3{
		Enabled: true, SupportedOnDevice: true, Containers: []string{"mp4"}, VideoCodecs: []string{"h264"}, AudioDecodeCodecs: []string{"aac"},
	}
	fx.started = startV3PlaybackForHandlerTest(t, fx.handler, fx.start)

	session, err := fx.manager.GetSession(fx.started.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if session.VirtualSourceURI != pinnedURI {
		t.Fatalf("start bound %q, want the pinned candidate %q", session.VirtualSourceURI, pinnedURI)
	}
	if fx.listCalls != 0 || fx.detailCalls != 0 || fx.probeCalls != 0 {
		t.Fatalf("start did not take the pinned fast path: list=%d detail=%d probe=%d", fx.listCalls, fx.detailCalls, fx.probeCalls)
	}
	return fx
}

// churnCatalogRow replaces the resolved effective row with the same row id and
// the same release's probed inventory under a different provider result token —
// the per-listing id churn the menu sink writes, with the row's evidence
// intact. The session stays bound to the pinned candidate it is already
// playing.
func (fx *pinnedTrackChangeFixture) churnCatalogRow(resultToken string) {
	churned := *fx.source
	churned.FilePath = withVirtualResultKey(virtualPlaybackNeutralKey(fx.source.FilePath), resultToken)
	churned.ProbeUpdatedAt = nil
	fx.files[fx.source.ID] = &churned
}

// churnCatalogRowToStub replaces the effective row with a different result token
// and no probed evidence at all — a listing row that was never probed. Without
// the session's plan-time inventory the pin cannot be described without a probe.
func (fx *pinnedTrackChangeFixture) churnCatalogRowToStub(resultToken string) {
	churned := *fx.source
	churned.FilePath = withVirtualResultKey(virtualPlaybackNeutralKey(fx.source.FilePath), resultToken)
	churned.AudioTracks = nil
	churned.VideoTracks = nil
	churned.SubtitleTracks = nil
	churned.ExternalSubtitles = nil
	churned.Container = "virtual"
	churned.ProbeUpdatedAt = nil
	fx.files[fx.source.ID] = &churned
}

// discardPinnedEvidence clears the plan-time evidence captured on the session,
// so the pinned candidate cannot be described without a fresh resolve/probe.
func (fx *pinnedTrackChangeFixture) discardPinnedEvidence(t *testing.T) {
	t.Helper()
	if err := fx.manager.UpdateStreamState(fx.started.SessionID, playback.SessionStreamState{
		VirtualSourceURI:          fx.pinnedURI,
		VirtualSourceSet:          true,
		VirtualSourceOwnershipSet: true,
		SubtitleTrackIndex:        -1,
	}); err != nil {
		t.Fatalf("clear session evidence: %v", err)
	}
}

func (fx *pinnedTrackChangeFixture) trackChangeRequest(audioIndex int, requestID string) playback.ReplanRequestV3 {
	return playback.ReplanRequestV3{
		ProtocolVersion:       playback.ProtocolV3,
		Operation:             playback.ReplanOperationTrackChangeV3,
		PlaybackAttemptID:     fx.start.PlaybackAttemptID,
		ReplanRequestID:       requestID,
		FailedPlanID:          fx.started.PlaybackPlan.PlanID,
		PlanAttemptID:         requestID + "-plan",
		PlanAttemptKey:        fx.started.PlaybackPlan.PlanAttemptKey,
		AttemptCount:          1,
		PositionSeconds:       30,
		QualityPreference:     "auto",
		SelectedTracks:        playback.SelectedTracksV3{Audio: &playback.TrackIdentityV3{ID: playback.TrackIDV3(fx.source.ID, "audio", audioIndex), Index: &audioIndex}},
		Capabilities:          fx.start.Capabilities,
		ClientPlaybackContext: fx.start.ClientPlaybackContext,
	}
}

func (fx *pinnedTrackChangeFixture) trackChange(t *testing.T, audioIndex int, requestID string) playback.DecisionResponseV3 {
	t.Helper()
	return postPlaybackReplanV3(t, fx.handler, fx.started.SessionID, fx.trackChangeRequest(audioIndex, requestID))
}

// A track change against a churned provider listing must apply to the candidate
// the session is already serving. The replan may not re-list, re-resolve or
// re-probe: a fresh listing is keyed by per-listing result ids that churn for
// bytes that never changed, and adopting the churned token is exactly what the
// identity guard refuses — the same-row track pick that silently did nothing.
func TestReplanTrackChangeCarriesPinnedCandidateAgainstChurnedListing(t *testing.T) {
	fx := newPinnedTrackChangeFixture(t, "churned")
	fx.churnCatalogRow("churned")

	response := fx.trackChange(t, 1, "pinned-track-change-0001")
	if response.Terminal != nil || response.PlaybackPlan == nil {
		t.Fatalf("track change = plan %#v terminal %#v, want a plan", response.PlaybackPlan, response.Terminal)
	}
	if response.PlaybackPlan.EffectiveMediaFileID != fx.source.ID {
		t.Fatalf("effective file = %d, want the session's pinned row %d", response.PlaybackPlan.EffectiveMediaFileID, fx.source.ID)
	}
	if response.PlaybackPlan.EffectiveVirtualURI != fx.pinnedURI {
		t.Fatalf("effective virtual URI = %q, want the pinned candidate %q", response.PlaybackPlan.EffectiveVirtualURI, fx.pinnedURI)
	}
	if response.PlaybackPlan.SelectedTracks.Audio == nil || response.PlaybackPlan.SelectedTracks.Audio.Index == nil ||
		*response.PlaybackPlan.SelectedTracks.Audio.Index != 1 {
		t.Fatalf("audio selection = %#v, want index 1 applied", response.PlaybackPlan.SelectedTracks.Audio)
	}
	if response.PlaybackPlan.Delivery != fx.started.PlaybackPlan.Delivery {
		t.Fatalf("track change moved the delivery: %s -> %s", fx.started.PlaybackPlan.Delivery, response.PlaybackPlan.Delivery)
	}
	if response.PlaybackPlan.SubstitutedFromFileID != 0 {
		t.Fatalf("track change reported a version substitution from %d", response.PlaybackPlan.SubstitutedFromFileID)
	}
	if fx.listCalls != 0 {
		t.Fatalf("track change re-listed the provider: list calls = %d", fx.listCalls)
	}
	if fx.detailCalls != 0 {
		t.Fatalf("track change re-resolved the provider: detail calls = %d", fx.detailCalls)
	}
	if fx.probeCalls != 0 {
		t.Fatalf("track change re-probed the pinned candidate: probe calls = %d", fx.probeCalls)
	}
	session, err := fx.manager.GetSession(fx.started.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if session.VirtualSourceURI != fx.pinnedURI {
		t.Fatalf("session binding moved to %q, want the unchanged pin %q", session.VirtualSourceURI, fx.pinnedURI)
	}
}

// A seek reanchor is the same class of operation: it moves within the mounted
// source and must carry the pinned candidate rather than re-list the provider.
func TestReplanSeekReanchorCarriesPinnedCandidateAgainstChurnedListing(t *testing.T) {
	fx := newPinnedTrackChangeFixture(t, "churned")
	fx.churnCatalogRow("churned")

	response := postPlaybackReplanV3(t, fx.handler, fx.started.SessionID, playback.ReplanRequestV3{
		ProtocolVersion:       playback.ProtocolV3,
		Operation:             playback.ReplanOperationSeekReanchorV3,
		PlaybackAttemptID:     fx.start.PlaybackAttemptID,
		ReplanRequestID:       "pinned-seek-reanchor-0001",
		FailedPlanID:          fx.started.PlaybackPlan.PlanID,
		PlanAttemptID:         "pinned-seek-reanchor-0001-plan",
		PlanAttemptKey:        fx.started.PlaybackPlan.PlanAttemptKey,
		AttemptCount:          1,
		PositionSeconds:       120,
		QualityPreference:     "auto",
		SelectedTracks:        fx.started.PlaybackPlan.SelectedTracks,
		Capabilities:          fx.start.Capabilities,
		ClientPlaybackContext: fx.start.ClientPlaybackContext,
	})
	if response.Terminal != nil || response.PlaybackPlan == nil {
		t.Fatalf("seek reanchor = plan %#v terminal %#v, want a plan", response.PlaybackPlan, response.Terminal)
	}
	if response.PlaybackPlan.EffectiveMediaFileID != fx.source.ID {
		t.Fatalf("seek reanchor effective file = %d, want the pinned row %d", response.PlaybackPlan.EffectiveMediaFileID, fx.source.ID)
	}
	if fx.listCalls != 0 || fx.detailCalls != 0 || fx.probeCalls != 0 {
		t.Fatalf("seek reanchor re-listed/resolved/probed the provider: list=%d detail=%d probe=%d", fx.listCalls, fx.detailCalls, fx.probeCalls)
	}
}

// When the pinned candidate cannot be described without a probe (the carried
// inventory is gone and the churned row has no usable evidence), the replan
// falls back to the existing resolve path instead of planning against a stub.
func TestReplanTrackChangeWithoutUsablePinFallsBackToResolve(t *testing.T) {
	fx := newPinnedTrackChangeFixture(t, "churned")
	fx.discardPinnedEvidence(t)
	fx.churnCatalogRowToStub("churned")

	response := fx.trackChange(t, 1, "pinned-track-change-fallback-0001")
	if response.Terminal != nil || response.PlaybackPlan == nil {
		t.Fatalf("fallback track change = plan %#v terminal %#v, want a plan", response.PlaybackPlan, response.Terminal)
	}
	if fx.detailCalls == 0 {
		t.Fatal("track change skipped the resolve path even though the pin had no usable evidence")
	}
	if response.PlaybackPlan.EffectiveMediaFileID != fx.source.ID {
		t.Fatalf("fallback effective file = %d, want the pinned row %d", response.PlaybackPlan.EffectiveMediaFileID, fx.source.ID)
	}
}

// When the pin is genuinely gone the existing rotation semantics apply, and a
// rotation that resolves a different release is refused: the replan must never
// silently swap the release under a same-row track pick.
func TestReplanTrackChangeRefusesSilentReleaseSwapWhenPinGone(t *testing.T) {
	fx := newPinnedTrackChangeFixture(t, "churned")
	fx.discardPinnedEvidence(t)
	fx.churnCatalogRowToStub("churned")
	// The provider no longer lists the pin; the rotation retry resolves a
	// genuinely different release (no same-release identity).
	siblingURI := withVirtualResultKey(virtualPlaybackNeutralKey(fx.source.FilePath), "other-release")
	fx.handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(ctx context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		if !VirtualCandidateRotationAllowed(ctx) {
			return ResolvedVirtualMedia{}, absentSessionPinError("pinned")
		}
		return ResolvedVirtualMedia{URL: "http://127.0.0.1:9/stream", URI: siblingURI, CandidateID: "other-release", ProviderGUID: "other-release-guid"}, nil
	})

	status, response := postReplanRawV3(t, fx.handler, fx.started.SessionID, fx.trackChangeRequest(1, "pinned-track-change-swap-0001"))
	if status == 200 && response.PlaybackPlan != nil {
		t.Fatalf("track change silently swapped to a different release: plan = %#v", response.PlaybackPlan)
	}
	if response.PlaybackPlan != nil && response.PlaybackPlan.EffectiveVirtualURI == siblingURI {
		t.Fatalf("track change adopted the different release %q", siblingURI)
	}
}
