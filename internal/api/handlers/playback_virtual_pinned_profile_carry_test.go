package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	if sessionHasVirtualProvenanceV3(nil) || sessionHasVirtualProvenanceV3(&playback.Session{}) {
		t.Fatal("a session with no carried provenance reported provenance")
	}
	if sessionHasVirtualProvenanceV3(&playback.Session{VirtualAudioTracks: []models.AudioTrack{{Codec: "aac"}}}) {
		t.Fatal("provider-declared audio alone was treated as provenance")
	}
	if !sessionHasVirtualProvenanceV3(&playback.Session{VirtualSubtitleEvidenceSet: true}) {
		t.Fatal("the probe-evidence flag was not recognized as provenance")
	}
	if !sessionHasVirtualProvenanceV3(&playback.Session{VirtualSubtitleEvidenceURI: "virtual://movie/tt?result=a"}) {
		t.Fatal("the evidence provenance anchor was not recognized as provenance")
	}
	if !sessionHasVirtualProvenanceV3(&playback.Session{VirtualSubtitleEvidenceFileID: 4912232}) {
		t.Fatal("the evidence provenance row id was not recognized as provenance")
	}
}

// sessionProvenanceMatchesPinnedRowV3 must require the retained provenance to
// describe the pin and the row being bound, not merely be present. A stale
// anchor left by a binding move, or an evidence row id captured from another
// row, is not applicable evidence and must not admit the carry.
func TestSessionProvenanceMatchesPinnedRowV3RequiresApplicableProvenance(t *testing.T) {
	neutral := "virtual://movie/tt"
	pinned := neutral + "?result=pinned"
	row := &models.MediaFile{ID: 720, FilePath: neutral}

	ctx := context.Background()
	applicable := &playback.Session{
		VirtualSourceURI:              pinned,
		VirtualSubtitleEvidenceURI:    pinned,
		VirtualSubtitleEvidenceFileID: 720,
	}
	if !sessionProvenanceMatchesPinnedRowV3(ctx, applicable, row, nil) {
		t.Fatal("provenance that names the pin and the bound row was rejected")
	}

	// A binding move left the anchor naming the previous candidate; it no longer
	// describes the pin, so the carry must not be admitted on it.
	stale := &playback.Session{
		VirtualSourceURI:              pinned,
		VirtualSubtitleEvidenceSet:    true,
		VirtualSubtitleEvidenceURI:    neutral + "?result=previous",
		VirtualSubtitleEvidenceFileID: 720,
	}
	if sessionProvenanceMatchesPinnedRowV3(ctx, stale, row, nil) {
		t.Fatal("provenance for a previous candidate was admitted for the pin")
	}

	// Legacy: a probe-evidence flag with no anchor still matches on the pin, and
	// the unknown row id falls back to URI-only matching.
	legacy := &playback.Session{VirtualSourceURI: pinned, VirtualSubtitleEvidenceSet: true}
	if !sessionProvenanceMatchesPinnedRowV3(ctx, legacy, row, nil) {
		t.Fatal("legacy flag-only provenance did not fall back to the pin anchor")
	}

	// The evidence row id is authoritative when known: another row's inventory
	// must not describe this row.
	otherRow := &playback.Session{
		VirtualSourceURI:              pinned,
		VirtualSubtitleEvidenceURI:    pinned,
		VirtualSubtitleEvidenceFileID: 999,
	}
	if sessionProvenanceMatchesPinnedRowV3(ctx, otherRow, row, nil) {
		t.Fatal("provenance captured from another row was admitted for this row")
	}

	if sessionProvenanceMatchesPinnedRowV3(ctx, &playback.Session{VirtualSourceURI: pinned}, row, nil) {
		t.Fatal("a session with no provenance reported applicable evidence")
	}
	if sessionProvenanceMatchesPinnedRowV3(ctx, applicable, nil, nil) {
		t.Fatal("a nil row reported applicable evidence")
	}
}

// Scope equality is not release identity: two concrete sibling picks under one
// profile share a scope, so the carry's safety must rest on the pinned evidence,
// not on the scope comparison. virtualPinnedProfileRowV3 therefore recognizes
// the pin's neutral profile row but never a concrete sibling.
func TestVirtualProfileScopeTwoSiblingsShareOneProfile(t *testing.T) {
	neutral := "virtual://movie/tt?profile=4K"
	first := neutral + "&result=aaaaaaaa"
	second := neutral + "&result=bbbbbbbb"

	if !sameVirtualProfileScope(first, second) {
		t.Fatal("two concrete siblings under one profile did not share a profile scope")
	}
	if sameVirtualCandidate(first, second) {
		t.Fatal("two distinct concrete results were treated as the same candidate")
	}
	if virtualPinnedProfileRowV3(&playback.Session{VirtualSourceURI: first}, &models.MediaFile{ID: 1, FilePath: second}) {
		t.Fatal("a concrete sibling was treated as the pinned profile")
	}
	// Scope cannot tell the siblings apart. Only the pin's own evidence can, so a
	// sibling's evidence is not applicable to the pin's profile row.
	if sessionProvenanceMatchesPinnedRowV3(context.Background(), &playback.Session{
		VirtualSourceURI:              first,
		VirtualSubtitleEvidenceURI:    second,
		VirtualSubtitleEvidenceFileID: 1,
	}, &models.MediaFile{ID: 1, FilePath: neutral}, nil) {
		t.Fatal("a sibling's evidence was admitted for the pin")
	}
}

// A stale evidence URI — the anchor names a previous candidate, not the pin — is
// not applicable evidence. The profile-row carry must fall back to the resolve
// path instead of planning against a row the binder cannot describe.
func TestReplanTrackChangeProfileRowWithStaleEvidenceURIReResolves(t *testing.T) {
	fx := newPinnedTrackChangeFixture(t, "churned")
	fx.churnCatalogRowToProfile()
	fx.setSessionEvidenceAnchor(t, withVirtualResultKey(virtualPlaybackNeutralKey(fx.source.FilePath), "previous"), fx.source.ID)

	response := fx.trackChange(t, 1, "pinned-profile-stale-uri-0001")
	if response.Terminal != nil || response.PlaybackPlan == nil {
		t.Fatalf("stale-uri track change = plan %#v terminal %#v, want a plan", response.PlaybackPlan, response.Terminal)
	}
	if fx.detailCalls == 0 {
		t.Fatal("a profile row with a stale evidence URI skipped the resolve path")
	}
}

// A mismatched evidence row id — the anchor was captured from a different row
// than the one being bound — is not applicable evidence either, and the carry
// must fall back to the resolve path.
func TestReplanTrackChangeProfileRowWithMismatchedEvidenceRowReResolves(t *testing.T) {
	fx := newPinnedTrackChangeFixture(t, "churned")
	fx.churnCatalogRowToProfile()
	fx.setSessionEvidenceAnchor(t, fx.pinnedURI, fx.source.ID+1)

	response := fx.trackChange(t, 1, "pinned-profile-mismatched-row-0001")
	if response.Terminal != nil || response.PlaybackPlan == nil {
		t.Fatalf("mismatched-row track change = plan %#v terminal %#v, want a plan", response.PlaybackPlan, response.Terminal)
	}
	if fx.detailCalls == 0 {
		t.Fatal("a profile row with a mismatched evidence row id skipped the resolve path")
	}
}

// A client stop that lands while a virtual re-resolve is stalled must not
// collapse the replan's own retryable provider-stall terminal into
// playback_session_not_found. The stall — not the stop — is what the replan
// observed, so the client keeps the retryable signal instead of being told the
// session is gone and forced to tear down and re-resume.
//
// The fence must also be durable, not merely a response: the attempt row is
// stopped, no terminal decision is persisted, no live session survives, and a
// repeat reads as the fast not-found. Both allowlisted stall reasons are covered.
func TestReplanVirtualSourceStallSurvivesConcurrentStop(t *testing.T) {
	type stallCase struct {
		name    string
		reason  string
		request func(fx *pinnedTrackChangeFixture) playback.ReplanRequestV3
		resolve func() (ResolvedVirtualMedia, error)
	}
	cases := []stallCase{
		{
			name:   "virtual_source_unavailable",
			reason: virtualSourceUnavailableReasonV3,
			request: func(fx *pinnedTrackChangeFixture) playback.ReplanRequestV3 {
				return fx.trackChangeRequest(1, "pinned-stall-concurrent-stop-0001")
			},
			resolve: func() (ResolvedVirtualMedia, error) {
				return ResolvedVirtualMedia{}, absentSessionPinError("pinned")
			},
		},
		{
			name:   "replan_virtual_input_timeout",
			reason: replanVirtualInputTimeoutReasonV3,
			request: func(fx *pinnedTrackChangeFixture) playback.ReplanRequestV3 {
				return fx.failureRecoveryRequest("pinned-stall-input-timeout-0001")
			},
			resolve: func() (ResolvedVirtualMedia, error) {
				return ResolvedVirtualMedia{}, fmt.Errorf("virtual resolve: %w", context.DeadlineExceeded)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newPinnedTrackChangeFixture(t, "churned")
			// No carried evidence, so the pin cannot be carried and the
			// rehydration re-resolves — the prod shape whose resolve stalls on
			// the provider.
			fx.discardPinnedEvidence(t)
			fx.churnCatalogRowToStub("churned")

			var stopOnce sync.Once
			fx.handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
				// The client's stop arrives while the provider resolve is in flight.
				stopOnce.Do(func() {
					_ = fx.manager.StopSession(fx.started.SessionID)
					fx.handler.markAttemptStoppedServerSide(context.Background(), fx.started.SessionID)
				})
				return tc.resolve()
			})

			before, err := fx.handler.PlanStoreV3.GetAttempt(context.Background(), fx.started.SessionID)
			if err != nil || before == nil {
				t.Fatalf("GetAttempt before: %v", err)
			}

			status, response := postReplanRawV3(t, fx.handler, fx.started.SessionID, tc.request(fx))
			if status != http.StatusOK {
				t.Fatalf("replan status = %d, want the retryable virtual-source 200 terminal; body = %+v", status, response)
			}
			if response.Terminal == nil || response.Terminal.Reason != tc.reason {
				t.Fatalf("terminal = %#v, want the retryable %s terminal", response.Terminal, tc.reason)
			}
			if !response.Terminal.Retryable {
				t.Fatalf("virtual-source stall terminal is not retryable: %#v", response.Terminal)
			}

			// Durable stop: the attempt is stopped and no terminal decision was
			// persisted for it.
			after, err := fx.handler.PlanStoreV3.GetAttempt(context.Background(), fx.started.SessionID)
			if err != nil || after == nil {
				t.Fatalf("GetAttempt after: %v", err)
			}
			if after.StoppedAt == nil {
				t.Fatal("the concurrent stop was not applied to the durable attempt")
			}
			if after.CurrentReplanRequestID != before.CurrentReplanRequestID {
				t.Fatalf("a terminal replan decision was persisted on a stopped attempt: %q -> %q", before.CurrentReplanRequestID, after.CurrentReplanRequestID)
			}
			// No surviving resources: the live session is gone.
			if _, err := fx.manager.GetSession(fx.started.SessionID); err == nil {
				t.Fatal("the live session survived the concurrent stop")
			}

			// Repeat: the stall terminal is not re-served for a durably stopped
			// session; the client reads the fast not-found instead.
			status2, _ := postReplanRawV3(t, fx.handler, fx.started.SessionID, tc.request(fx))
			if status2 != http.StatusNotFound {
				t.Fatalf("repeat replan status = %d, want 404 for the durably stopped session", status2)
			}
		})
	}
}

// The stall survival is deliberately narrow. A stop that lands on a replan whose
// own outcome is not a virtual-source stall must still read as the fast 404: the
// allowlist fences the retryable terminal, it is not a general "ignore the stop"
// door. A source_unavailable verdict on a durably stopped attempt is the
// deliberate-stop case the web client must rebuild from.
func TestReplanStoppedAttemptNonStallTerminalReadsAsNotFound(t *testing.T) {
	fx := newPinnedTrackChangeFixture(t, "churned")
	fx.discardPinnedEvidence(t)
	fx.churnCatalogRowToStub("churned")

	store := &progressCountingPlanStoreV3{
		PlanStoreV3:     fx.handler.PlanStoreV3,
		ProgressStoreV3: fx.handler.PlanStoreV3.(playback.ProgressStoreV3),
	}
	fx.handler.PlanStoreV3 = store
	fx.handler.fileResolver = &stopThenMissingResolverV3{
		FilePathResolver: fx.handler.fileResolver,
		stop: func() {
			_ = fx.manager.StopSession(fx.started.SessionID)
			fx.handler.markAttemptStoppedServerSide(context.Background(), fx.started.SessionID)
		},
	}

	status, response := postReplanRawV3(t, fx.handler, fx.started.SessionID, fx.trackChangeRequest(1, "pinned-nonstall-stop-0001"))
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a non-stall terminal on a stopped attempt; body = %+v", status, response)
	}
	// The request reached execution (a lease was reserved) and completed through
	// the terminal path; the fence, not an early-out, produced the 404.
	if store.beginReplanCalls != 1 {
		t.Fatalf("BeginReplan called %d times, want 1", store.beginReplanCalls)
	}
	if store.completeReplanCalls != 1 {
		t.Fatalf("CompleteReplan called %d times, want 1", store.completeReplanCalls)
	}
}

// progressCountingPlanStoreV3 counts lease mutations while still exposing the
// ProgressStoreV3 capability markAttemptStoppedServerSide type-asserts for. A
// wrapper that embeds only PlanStoreV3 would make that assertion fail and the
// stop would be silently dropped, defeating the fixture.
type progressCountingPlanStoreV3 struct {
	playback.PlanStoreV3
	playback.ProgressStoreV3
	beginReplanCalls    int
	completeReplanCalls int
}

func (s *progressCountingPlanStoreV3) BeginReplan(ctx context.Context, sessionID, replanRequestID, digest, baseReplanRequestID string, expiresAt time.Time) (playback.ReplanLeaseV3, error) {
	s.beginReplanCalls++
	return s.PlanStoreV3.BeginReplan(ctx, sessionID, replanRequestID, digest, baseReplanRequestID, expiresAt)
}

func (s *progressCountingPlanStoreV3) CompleteReplan(ctx context.Context, sessionID, requestID, leaseToken, baseReplanRequestID string, response json.RawMessage, record playback.AttemptRecordV3) error {
	s.completeReplanCalls++
	return s.PlanStoreV3.CompleteReplan(ctx, sessionID, requestID, leaseToken, baseReplanRequestID, response, record)
}

// stopThenMissingResolverV3 stops the session on the replan's first catalog read
// inside execution, then reports the requested row missing so the replan
// computes a non-stall source_unavailable terminal on an already-stopped
// attempt.
type stopThenMissingResolverV3 struct {
	FilePathResolver
	stop func()
	once atomic.Bool
}

func (r *stopThenMissingResolverV3) GetByID(ctx context.Context, id int) (*models.MediaFile, error) {
	if r.once.CompareAndSwap(false, true) {
		r.stop()
		return nil, errors.New("requested source row is gone")
	}
	return r.FilePathResolver.GetByID(ctx, id)
}
