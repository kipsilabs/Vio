package handlers

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// churnCatalogRowToProfile replaces the resolved effective row with its
// provider-neutral counterpart: same catalog id, the concrete "result=" pick
// removed, and no probe stamp. This is the prod shape where the provider churns
// the per-listing result id for bytes that never changed, so a reloaded catalog
// row is the profile while the session stays bound to the concrete candidate it
// is already serving.
func (fx *pinnedTrackChangeFixture) churnCatalogRowToProfile() {
	neutral := *fx.source
	neutral.FilePath = virtualPlaybackNeutralKey(fx.source.FilePath)
	neutral.ProbeUpdatedAt = nil
	fx.files[fx.source.ID] = &neutral
}

// dropSessionSubtitleEvidence models the state a full v3 stream-state write
// leaves after a plan whose effective row was unprobed: the probe-evidence flag
// that would take the existing same-row carry shortcut is cleared, while the
// provenance anchor (the evidence URI and row id) and the plan-time audio
// inventory survive. That is the prod shape — the session once proved a
// concrete candidate, then a state write dropped the flag but kept the anchor.
func (fx *pinnedTrackChangeFixture) dropSessionSubtitleEvidence(t *testing.T) {
	t.Helper()
	session, err := fx.manager.GetSession(fx.started.SessionID)
	if err != nil || session == nil {
		t.Fatalf("GetSession: %v", err)
	}
	audio := append([]models.AudioTrack(nil), session.VirtualAudioTracks...)
	evidenceURI := session.VirtualSubtitleEvidenceURI
	evidenceFileID := session.VirtualSubtitleEvidenceFileID
	if evidenceURI == "" || evidenceFileID == 0 {
		t.Fatalf("fixture precondition: session carried no evidence anchor (%q, %d)", evidenceURI, evidenceFileID)
	}
	if err := fx.manager.UpdateStreamState(fx.started.SessionID, playback.SessionStreamState{
		VirtualSourceURI:              fx.pinnedURI,
		VirtualSourceSet:              true,
		VirtualSourceOwnershipSet:     true,
		VirtualAudioTracks:            audio,
		VirtualSubtitleEvidenceURI:    evidenceURI,
		VirtualSubtitleEvidenceFileID: evidenceFileID,
		SubtitleTrackIndex:            -1,
	}); err != nil {
		t.Fatalf("clear session subtitle evidence: %v", err)
	}
	session, err = fx.manager.GetSession(fx.started.SessionID)
	if err != nil || session == nil {
		t.Fatalf("GetSession: %v", err)
	}
	if session.VirtualSubtitleEvidenceSet {
		t.Fatal("subtitle evidence flag still set; the regression shape needs it clear")
	}
	if session.VirtualSubtitleEvidenceURI == "" || session.VirtualSubtitleEvidenceFileID == 0 {
		t.Fatal("the cleared state did not keep the evidence provenance anchor")
	}
}

// A track change on a session bound to a concrete pin whose reloaded catalog row
// is the provider-neutral profile must carry the pin, not re-list. Before the
// fix the carry predicate required result-id equality or the probe-evidence
// flag, so a provider-declared plan re-listed and handed back a churned token
// the identity guard then refused to adopt.
func TestReplanTrackChangeCarriesPinnedCandidateAgainstProfileRow(t *testing.T) {
	fx := newPinnedTrackChangeFixture(t, "churned")
	fx.churnCatalogRowToProfile()
	fx.dropSessionSubtitleEvidence(t)

	response := fx.trackChange(t, 1, "pinned-profile-track-change-0001")
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

// A subtitle switch is the same user-intent class: it re-selects within the
// source the session is serving and must carry the pinned candidate rather than
// re-list the provider under the profile→concrete binding.
func TestReplanSubtitleSwitchCarriesPinnedCandidateAgainstProfileRow(t *testing.T) {
	fx := newPinnedTrackChangeFixture(t, "churned")
	fx.churnCatalogRowToProfile()
	fx.dropSessionSubtitleEvidence(t)

	subtitleIndex := 1
	response := postPlaybackReplanV3(t, fx.handler, fx.started.SessionID, playback.ReplanRequestV3{
		ProtocolVersion:   playback.ProtocolV3,
		Operation:         playback.ReplanOperationTrackChangeV3,
		PlaybackAttemptID: fx.start.PlaybackAttemptID,
		ReplanRequestID:   "pinned-profile-subtitle-switch-0001",
		FailedPlanID:      fx.started.PlaybackPlan.PlanID,
		PlanAttemptID:     "pinned-profile-subtitle-switch-0001-plan",
		PlanAttemptKey:    fx.started.PlaybackPlan.PlanAttemptKey,
		AttemptCount:      1,
		PositionSeconds:   30,
		QualityPreference: "auto",
		SelectedTracks: playback.SelectedTracksV3{
			Subtitle: &playback.TrackIdentityV3{
				ID:    playback.TrackIDV3(fx.source.ID, "subtitle", subtitleIndex),
				Index: &subtitleIndex,
			},
		},
		Capabilities:          fx.start.Capabilities,
		ClientPlaybackContext: fx.start.ClientPlaybackContext,
	})
	if response.Terminal != nil || response.PlaybackPlan == nil {
		t.Fatalf("subtitle switch = plan %#v terminal %#v, want a plan", response.PlaybackPlan, response.Terminal)
	}
	if response.PlaybackPlan.EffectiveVirtualURI != fx.pinnedURI {
		t.Fatalf("subtitle switch effective virtual URI = %q, want the pinned candidate %q", response.PlaybackPlan.EffectiveVirtualURI, fx.pinnedURI)
	}
	if response.PlaybackPlan.SelectedTracks.Subtitle == nil || response.PlaybackPlan.SelectedTracks.Subtitle.Index == nil ||
		*response.PlaybackPlan.SelectedTracks.Subtitle.Index != subtitleIndex {
		t.Fatalf("subtitle selection = %#v, want index %d applied", response.PlaybackPlan.SelectedTracks.Subtitle, subtitleIndex)
	}
	if fx.listCalls != 0 || fx.detailCalls != 0 || fx.probeCalls != 0 {
		t.Fatalf("subtitle switch re-listed/resolved/probed the provider: list=%d detail=%d probe=%d", fx.listCalls, fx.detailCalls, fx.probeCalls)
	}
}

// A seek reanchor moves within the mounted source and must carry the pinned
// candidate under the profile→concrete binding rather than re-list the provider.
func TestReplanSeekReanchorCarriesPinnedCandidateAgainstProfileRow(t *testing.T) {
	fx := newPinnedTrackChangeFixture(t, "churned")
	fx.churnCatalogRowToProfile()
	fx.dropSessionSubtitleEvidence(t)

	response := postPlaybackReplanV3(t, fx.handler, fx.started.SessionID, playback.ReplanRequestV3{
		ProtocolVersion:       playback.ProtocolV3,
		Operation:             playback.ReplanOperationSeekReanchorV3,
		PlaybackAttemptID:     fx.start.PlaybackAttemptID,
		ReplanRequestID:       "pinned-profile-seek-reanchor-0001",
		FailedPlanID:          fx.started.PlaybackPlan.PlanID,
		PlanAttemptID:         "pinned-profile-seek-reanchor-0001-plan",
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
	if response.PlaybackPlan.EffectiveVirtualURI != fx.pinnedURI {
		t.Fatalf("seek reanchor effective virtual URI = %q, want the pinned candidate %q", response.PlaybackPlan.EffectiveVirtualURI, fx.pinnedURI)
	}
	if fx.listCalls != 0 || fx.detailCalls != 0 || fx.probeCalls != 0 {
		t.Fatalf("seek reanchor re-listed/resolved/probed the provider: list=%d detail=%d probe=%d", fx.listCalls, fx.detailCalls, fx.probeCalls)
	}
}

// The profile→concrete carry only applies when the session carries evidence to
// describe the pinned bytes. A profile row with no carried inventory still
// re-resolves, so an offline pin surfaces through the existing rotation
// semantics instead of planning against an undescribed stub.
func TestReplanTrackChangeProfileRowWithoutEvidenceReResolves(t *testing.T) {
	fx := newPinnedTrackChangeFixture(t, "churned")
	fx.discardPinnedEvidence(t)
	fx.churnCatalogRowToProfile()

	response := fx.trackChange(t, 1, "pinned-profile-no-evidence-0001")
	if response.Terminal != nil || response.PlaybackPlan == nil {
		t.Fatalf("no-evidence track change = plan %#v terminal %#v, want a plan", response.PlaybackPlan, response.Terminal)
	}
	if fx.detailCalls == 0 {
		t.Fatal("a profile row with no carried evidence skipped the resolve path")
	}
}

// virtualPinnedProfileRowV3 must recognize only the provider-neutral profile of
// the session's concrete pin. A concrete sibling result is a release
// substitution candidate and must keep re-resolving; a concrete pin with a
// neutral row id mismatch must not be matched by construction.
func TestVirtualPinnedProfileRowV3RecognizesProfileNotSiblingRelease(t *testing.T) {
	neutral := "virtual://movie/tt-row"
	pinned := neutral + "?result=pinned"
	session := &playback.Session{VirtualSourceURI: pinned}

	if !virtualPinnedProfileRowV3(session, &models.MediaFile{ID: 1, FilePath: neutral}) {
		t.Fatal("the provider-neutral profile row of the pinned candidate was not recognized")
	}
	if virtualPinnedProfileRowV3(session, &models.MediaFile{ID: 1, FilePath: neutral + "?result=sibling"}) {
		t.Fatal("a concrete sibling result was treated as the pinned profile")
	}
	if virtualPinnedProfileRowV3(session, &models.MediaFile{ID: 1, FilePath: "virtual://movie/other"}) {
		t.Fatal("a different release was treated as the pinned profile")
	}
	if virtualPinnedProfileRowV3(&playback.Session{VirtualSourceURI: neutral}, &models.MediaFile{ID: 1, FilePath: neutral}) {
		t.Fatal("a neutral-session pin was matched against a neutral row")
	}
	if sessionCarriesVirtualEvidenceV3(nil) || sessionCarriesVirtualEvidenceV3(&playback.Session{}) {
		t.Fatal("a session with no carried evidence reported evidence")
	}
	if sessionCarriesVirtualEvidenceV3(&playback.Session{VirtualAudioTracks: []models.AudioTrack{{Codec: "aac"}}}) {
		t.Fatal("provider-declared audio alone was treated as evidence")
	}
	if !sessionCarriesVirtualEvidenceV3(&playback.Session{VirtualSubtitleEvidenceSet: true}) {
		t.Fatal("the probe-evidence flag was not recognized as evidence")
	}
	if !sessionCarriesVirtualEvidenceV3(&playback.Session{VirtualSubtitleEvidenceURI: "virtual://movie/tt?result=a"}) {
		t.Fatal("the evidence provenance anchor was not recognized as evidence")
	}
	if !sessionCarriesVirtualEvidenceV3(&playback.Session{VirtualSubtitleEvidenceFileID: 4912232}) {
		t.Fatal("the evidence provenance row id was not recognized as evidence")
	}
}

// A client stop that lands while a virtual re-resolve is stalled must not
// collapse the replan's own retryable provider-stall terminal into
// playback_session_not_found. The stall — not the stop — is what the replan
// observed, so the client keeps the retryable virtual_source_unavailable signal
// instead of being told the session is gone and forced to tear down and
// re-resume.
func TestReplanVirtualSourceStallSurvivesConcurrentStop(t *testing.T) {
	fx := newPinnedTrackChangeFixture(t, "churned")
	// No carried evidence, so the pin cannot be carried and the rehydration
	// re-resolves — the prod shape whose resolve stalls on the provider.
	fx.discardPinnedEvidence(t)
	fx.churnCatalogRowToStub("churned")

	var stopOnce sync.Once
	fx.handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(ctx context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		// The client's stop arrives while the provider resolve is in flight.
		stopOnce.Do(func() {
			_ = fx.manager.StopSession(fx.started.SessionID)
			fx.handler.markAttemptStoppedServerSide(context.Background(), fx.started.SessionID)
		})
		if !VirtualCandidateRotationAllowed(ctx) {
			return ResolvedVirtualMedia{}, absentSessionPinError("pinned")
		}
		siblingURI := withVirtualResultKey(virtualPlaybackNeutralKey(fx.source.FilePath), "other-release")
		return ResolvedVirtualMedia{URL: "http://127.0.0.1:9/stream", URI: siblingURI, CandidateID: "other-release", ProviderGUID: "other-release-guid"}, nil
	})

	status, response := postReplanRawV3(t, fx.handler, fx.started.SessionID, fx.trackChangeRequest(1, "pinned-stall-concurrent-stop-0001"))
	if status != http.StatusOK {
		t.Fatalf("replan status = %d, want the retryable virtual-source 200 terminal; body = %+v", status, response)
	}
	if response.Terminal == nil || response.Terminal.Reason != "virtual_source_unavailable" {
		t.Fatalf("terminal = %#v, want the retryable virtual_source_unavailable terminal", response.Terminal)
	}
	if !response.Terminal.Retryable {
		t.Fatalf("virtual-source stall terminal is not retryable: %#v", response.Terminal)
	}
}
