package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// deferredGateFixture starts a handler whose virtual prober blocks until the
// test releases it, so a test can observe the deferred lifecycle deterministically
// (the probe stays outstanding) and then choose whether it succeeds or fails.
// The two post-commit paths are distinguishable without timing: only the
// deferred lifecycle records session.VirtualProbeOutcome = "pending" before the
// start returns and sets plan.TracksPending.
type deferredGateFixture struct {
	handler *PlaybackHandler
	manager *playback.SessionManager
	source  *models.MediaFile

	releaseOnce sync.Once
	release     chan struct{}

	probeMu   sync.Mutex
	probeFail bool
}

func newDeferredGateFixture(t *testing.T, fileID int) *deferredGateFixture {
	t.Helper()
	source := v3HandlerFixtureFile(t)
	id := fmt.Sprintf("%d", fileID)
	source.ID = fileID
	source.ContentID = "movie-deferred-gate-" + id
	source.FilePath = "virtual://movie/tt-deferred-gate-" + id + "?result=cand-1"
	source.VirtualOwnerInstallationID = 5
	// Candidate-complete evidence without a probe stamp: the resolve owes the
	// full enumeration, so it can defer it.
	source.ProbeUpdatedAt = nil

	manager := playback.NewSessionManager(0, 0)
	handler := NewPlaybackHandler(manager, testPlaybackFileResolver{file: source})
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"allow_4k_transcode": "true"}}
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	handler.PlaybackConfig = playbackTestConfig("", "")
	handler.RealtimeHub = playback.NewRealtimeHub()
	handler.InstallationID = serviceInstallation
	stubCopySeekAnchorV3(handler)
	handler.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
		return "http://127.0.0.1:8080/stream?path=" + path, nil
	})

	fx := &deferredGateFixture{handler: handler, manager: manager, source: source, release: make(chan struct{})}
	handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		<-fx.release
		fx.probeMu.Lock()
		fail := fx.probeFail
		fx.probeMu.Unlock()
		if fail {
			return nil, errors.New("provider probe refused the stream")
		}
		f.AudioTracks = []models.AudioTrack{{Codec: "eac3", Channels: 6, Language: "deu"}}
		f.CodecAudio = "eac3"
		return f, nil
	}
	handler.VirtualFileMetadataSaver = func(_ context.Context, _ models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
		return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
	}
	t.Cleanup(fx.releaseProbe)
	return fx
}

// releaseProbe unblocks the prober exactly once, so a test that already
// released it can still be cleaned up safely.
func (fx *deferredGateFixture) releaseProbe() {
	fx.releaseOnce.Do(func() { close(fx.release) })
}

// failProbe makes the outstanding probe return a terminal failure.
func (fx *deferredGateFixture) failProbe() {
	fx.probeMu.Lock()
	fx.probeFail = true
	fx.probeMu.Unlock()
}

func (fx *deferredGateFixture) start(t *testing.T, v2, feature bool, mutate func(*playback.StartRequestV3)) playback.DecisionResponseV3 {
	t.Helper()
	request := v3HandlerStartRequest()
	request.FileID = fx.source.ID
	request.QualityPreference = "original"
	if feature {
		request.ClientFeatures = append(request.ClientFeatures, playback.FeatureDeferredTrackInventoryV3)
	}
	if mutate != nil {
		mutate(&request)
	}
	if v2 {
		return startV3PlaybackV2ForHandlerTest(t, fx.handler, request)
	}
	return startV3PlaybackForHandlerTest(t, fx.handler, request)
}

// sessionProbeOutcome reads the live session's deferred-probe disposition.
func (fx *deferredGateFixture) sessionProbeOutcome(t *testing.T, sessionID string) string {
	t.Helper()
	live, err := fx.manager.GetSession(sessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	return live.VirtualProbeOutcome
}

// TestDeferredTrackInventoryGateV1Unchanged is the point-2 regression on the
// frozen surface: an /api/v1 start that sends deferred_track_inventory_v1 must
// not enter the deferred lifecycle. The plan keeps the pre-#228 shape (no
// tracks_pending), no session probe outcome is recorded, and the frozen surface
// does not advertise the capability — the v1 contract is byte-for-byte
// unchanged, so a v1 client never has to learn a provisional menu.
func TestDeferredTrackInventoryGateV1Unchanged(t *testing.T) {
	fx := newDeferredGateFixture(t, 730)
	start := fx.start(t, false, true, nil)
	if start.PlaybackPlan == nil {
		t.Fatal("start returned no playback plan")
	}
	if start.PlaybackPlan.TracksPending {
		t.Fatal("a frozen /api/v1 start entered the deferred track-inventory lifecycle")
	}
	if got := fx.sessionProbeOutcome(t, start.SessionID); got != "" {
		t.Fatalf("v1 start recorded deferred probe outcome %q, want empty", got)
	}
	if playback.HasFeatureV3(start.ServerFeatures, playback.FeatureDeferredTrackInventoryV3) {
		t.Fatal("the frozen /api/v1 surface advertised deferred_track_inventory_v1")
	}
}

// TestDeferredTrackInventoryGateV2WithoutFeature is the point-2 regression on
// v2: a client that did not advertise deferred_track_inventory_v1 keeps the
// pre-#228 behavior even on /api/v2. No tracks_pending is published and the
// session never enters the deferred lifecycle.
func TestDeferredTrackInventoryGateV2WithoutFeature(t *testing.T) {
	fx := newDeferredGateFixture(t, 731)
	start := fx.start(t, true, false, nil)
	if start.PlaybackPlan == nil {
		t.Fatal("start returned no playback plan")
	}
	if start.PlaybackPlan.TracksPending {
		t.Fatal("a v2 start without deferred_track_inventory_v1 marked tracks_pending")
	}
	if got := fx.sessionProbeOutcome(t, start.SessionID); got != "" {
		t.Fatalf("v2 start without the feature recorded deferred outcome %q, want empty", got)
	}
	// The server feature list still advertises the capability on v2: it is the
	// client that has not negotiated it.
	if !playback.HasFeatureV3(start.ServerFeatures, playback.FeatureDeferredTrackInventoryV3) {
		t.Fatal("v2 start did not advertise deferred_track_inventory_v1 to the client")
	}
	// inventory_url is unconditional on v2: it names the same live-inventory
	// endpoint getPlaybackInventory serves and predates the deferral. Only the
	// provisional marker is gated, so a non-deferred v2 plan still carries the
	// poll URL. This is the real response, not a projection unit check.
	if start.PlaybackPlan.InventoryURL != "/api/v2/playback/"+start.SessionID+"/inventory" {
		t.Fatalf("non-deferred v2 plan inventory_url = %q, want the live-inventory endpoint", start.PlaybackPlan.InventoryURL)
	}
}

// TestDeferredTrackInventoryGateV2WithFeature is the negotiated path: a v2
// client that advertised deferred_track_inventory_v1 gets a provisional plan
// (tracks_pending, pending provenance, inventory_url) and the session enters the
// deferred lifecycle while the full enumeration is still outstanding.
func TestDeferredTrackInventoryGateV2WithFeature(t *testing.T) {
	fx := newDeferredGateFixture(t, 732)
	start := fx.start(t, true, true, nil)
	if start.PlaybackPlan == nil {
		t.Fatal("start returned no playback plan")
	}
	if !start.PlaybackPlan.TracksPending {
		t.Fatal("a negotiated v2 deferred start did not mark tracks_pending")
	}
	if start.PlaybackPlan.InventoryURL == "" {
		t.Fatal("a deferred plan did not carry inventory_url for the pending → terminal poll")
	}
	if start.PlaybackPlan.InventoryProvenance != string(ProbeProvenancePending) {
		t.Fatalf("deferred plan provenance = %q, want pending", start.PlaybackPlan.InventoryProvenance)
	}
	if got := fx.sessionProbeOutcome(t, start.SessionID); got != probeOutcomePending {
		t.Fatalf("deferred start session outcome = %q, want pending", got)
	}
}

// TestDeferredTrackInventoryAttemptReplayKeepsPending is the point-2 replay
// regression: an idempotent retry of the same playback_attempt_id must replay
// the durable provisional decision, tracks_pending and all, instead of
// appearing fully probed. Plan identity ignores TracksPending, so the replay
// matches; the marker must survive it.
func TestDeferredTrackInventoryAttemptReplayKeepsPending(t *testing.T) {
	fx := newDeferredGateFixture(t, 733)
	first := fx.start(t, true, true, nil)
	if first.PlaybackPlan == nil || !first.PlaybackPlan.TracksPending {
		t.Fatalf("first deferred start = %#v, want tracks_pending", first.PlaybackPlan)
	}
	second := fx.start(t, true, true, nil)
	if second.PlaybackPlan == nil {
		t.Fatal("replay returned no playback plan")
	}
	if second.SessionID != first.SessionID || second.PlaybackPlan.PlanID != first.PlaybackPlan.PlanID {
		t.Fatalf("replay returned a different plan: session %q/%q plan %q/%q",
			second.SessionID, first.SessionID, second.PlaybackPlan.PlanID, first.PlaybackPlan.PlanID)
	}
	if !second.PlaybackPlan.TracksPending {
		t.Fatal("attempt replay lost tracks_pending and presented a fully probed plan")
	}
	if second.PlaybackPlan.InventoryURL == "" {
		t.Fatal("attempt replay lost inventory_url")
	}
}

// TestDeferredProbeFailureEndsLoadingWithoutSwitchingAudioTrack is the point-3
// client-handoff regression and the #233 overlap guard. A client that
// negotiated the deferred lifecycle and explicitly chose an audio track must
// keep that choice when the deferred enumeration terminally fails: the failure
// ends the loading state (inventory_status failed) through the poll, but it
// must never rewrite the executable audio selection. The deferred recipe commits
// video plus the viewer's audio; only the menu is provisional, so a failed menu
// upgrade is a UI outcome, not a track switch.
func TestDeferredProbeFailureEndsLoadingWithoutSwitchingAudioTrack(t *testing.T) {
	fx := newDeferredGateFixture(t, 734)
	// The terminal failure marks the process-wide probe damper; a re-run in the
	// same process would read the recent mark and skip the deferred probe, so
	// the plan would not be provisional. Clear the key before and after so the
	// test is repeatable.
	failureKey := virtualProbeFailureKey(fx.source.FilePath, fx.source.VirtualOwnerInstallationID)
	virtualProbeFailures.clear(failureKey)
	t.Cleanup(func() { virtualProbeFailures.clear(failureKey) })

	// Two audio tracks: the viewer picks the second (German) one explicitly.
	// Append rather than replace so the fixture's original default track (with
	// its layout) is preserved exactly as the known-good start fixtures use it.
	fx.source.AudioTracks = append(fx.source.AudioTracks, models.AudioTrack{Codec: "aac", Channels: 2, Layout: "stereo", Language: "deu"})

	// The viewer explicitly picked the second track at the raw stream index 1.
	// A non-default track cannot ride the direct original_http route, so enable
	// the progressive remux delivery the existing track-change tests use; the
	// committed recipe must then keep the viewer's track through the failure.
	start := fx.start(t, true, true, func(request *playback.StartRequestV3) {
		index := 1
		request.AudioTrackIndex = &index
		request.AudioTrackID = playback.TrackIDV3(fx.source.ID, "audio", index)
		request.ClientPlaybackContext.Deliveries[playback.DeliveryClassProgressiveV3] = playback.DeliveryCapabilityV3{
			Enabled: true, SupportedOnDevice: true,
			Subtitles: playback.DeliverySubtitleCapabilitiesV3{EmbeddedText: true, SidecarText: true},
		}
	})
	if start.PlaybackPlan == nil {
		t.Fatalf("start returned no playback plan: terminal=%+v", start.Terminal)
	}
	if !start.PlaybackPlan.TracksPending {
		t.Fatal("negotiated deferred start did not mark tracks_pending")
	}
	if start.PlaybackPlan.SelectedTracks.Audio == nil {
		t.Fatal("start returned no committed audio selection")
	}
	committedAudio := *start.PlaybackPlan.SelectedTracks.Audio
	live, err := fx.manager.GetSession(start.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.AudioTrackIndex != 1 {
		t.Fatalf("session audio index = %d, want the viewer's 1", live.AudioTrackIndex)
	}

	// The deferred enumeration fails terminally. The failure must end loading
	// without touching the audio selection.
	fx.failProbe()
	fx.releaseProbe()

	requireDeferredInventoryStatus(t, fx.handler, start.SessionID, string(ProbeProvenanceFailed))

	liveAfter, err := fx.manager.GetSession(start.SessionID)
	if err != nil {
		t.Fatalf("GetSession after failure: %v", err)
	}
	if liveAfter.AudioTrackIndex != 1 {
		t.Fatalf("failed deferred probe changed the session audio index to %d, want the viewer's 1", liveAfter.AudioTrackIndex)
	}
	record, err := fx.handler.PlanStoreV3.GetAttempt(context.Background(), start.SessionID)
	if err != nil {
		t.Fatalf("GetAttempt: %v", err)
	}
	audio := record.CurrentPlan.SelectedTracks.Audio
	if audio == nil {
		t.Fatal("failed deferred probe cleared the committed plan audio selection")
	}
	if audio.ID != committedAudio.ID || audio.Index == nil || committedAudio.Index == nil || *audio.Index != *committedAudio.Index {
		t.Fatalf("failed deferred probe changed the committed plan audio selection from %#v to %#v", committedAudio, *audio)
	}
}

// requireDeferredInventoryStatus polls the v2 inventory application seam until
// the session reports want, so a test waits on observable state rather than a
// fixed sleep.
func requireDeferredInventoryStatus(t *testing.T, handler *PlaybackHandler, sessionID, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		inventory, err := handler.GetPlaybackInventoryV2(newAuthorizedPlaybackContext(), PlaybackCaller{UserID: 1, ProfileID: "profile-1", InstallationID: serviceInstallation}, sessionID)
		if err != nil {
			t.Fatalf("GetPlaybackInventoryV2: %v", err)
		}
		if inventory.InventoryStatus == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("inventory status = %q, want %q", inventory.InventoryStatus, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// replayV3StartForHandlerTest issues one start through the requested surface
// and returns the raw recorder without asserting the status, so a test can
// observe an idempotency/surface refusal as well as a replay.
func replayV3StartForHandlerTest(t *testing.T, handler *PlaybackHandler, v2 bool, request playback.StartRequestV3) *httptest.ResponseRecorder {
	t.Helper()
	path := "/api/v1/playback/start"
	ctx := newAuthorizedPlaybackContext()
	if v2 {
		path = "/api/v2/playback/start"
		ctx = WithNativeAPIV2(ctx)
	}
	rr := httptest.NewRecorder()
	handler.HandleStartPlayback(rr, httptest.NewRequest(http.MethodPost, path, strings.NewReader(marshalV3StartRequest(t, request))).WithContext(ctx))
	return rr
}

// TestDeferredTrackInventoryCrossSurfaceReplayClosed is the cross-surface
// replay regression: the request digest does not bind the API surface, and the
// shared PlanV3 serializes tracks_pending, so without a surface check the same
// body could replay a deferred-v2 attempt through the frozen v1 bridge (and a
// v2 client could adopt an unnegotiated attempt). The attempt-surface guard
// refuses both directions with the existing playback_attempt_reused code.
func TestDeferredTrackInventoryCrossSurfaceReplayClosed(t *testing.T) {
	fx := newDeferredGateFixture(t, 735)
	request := func(attemptID string) playback.StartRequestV3 {
		r := v3HandlerStartRequest()
		r.PlaybackAttemptID = attemptID
		r.FileID = fx.source.ID
		r.QualityPreference = "original"
		r.ClientFeatures = append(r.ClientFeatures, playback.FeatureDeferredTrackInventoryV3)
		return r
	}

	// Direction A: a deferred-v2 attempt replayed through /api/v1. The retried
	// body is byte-identical, so the digest matches and only the surface guard
	// can stop the replay of a plan whose tracks_pending v1 cannot consume.
	deferredReq := request("attempt-cross-surface-deferred-v2")
	first := replayV3StartForHandlerTest(t, fx.handler, true, deferredReq)
	if first.Code != http.StatusCreated {
		t.Fatalf("v2 deferred start: %d %s", first.Code, first.Body.String())
	}
	var started playback.DecisionResponseV3
	if err := json.Unmarshal(first.Body.Bytes(), &started); err != nil || started.PlaybackPlan == nil || !started.PlaybackPlan.TracksPending {
		t.Fatalf("v2 deferred start body: err=%v response=%#v", err, started)
	}
	v1Replay := replayV3StartForHandlerTest(t, fx.handler, false, deferredReq)
	if v1Replay.Code != http.StatusConflict || !strings.Contains(v1Replay.Body.String(), "playback_attempt_reused") {
		t.Fatalf("v1 replay of a deferred-v2 attempt = %d %s, want 409 playback_attempt_reused", v1Replay.Code, v1Replay.Body.String())
	}

	// Direction B: the frozen v1 surface never advertises the feature, so a v1
	// start whose client sent deferred_track_inventory_v1 persists an
	// unnegotiated attempt (client token present, server feature absent; only
	// subrip_sidecar_v1 is stripped from the normalized request). The
	// byte-identical v2 retry has the feature in its requested list and must be
	// refused rather than adopt an attempt that will never defer.
	plainBody := request("attempt-cross-surface-plain-v1")
	plainFirst := replayV3StartForHandlerTest(t, fx.handler, false, plainBody)
	if plainFirst.Code != http.StatusCreated {
		t.Fatalf("v1 start: %d %s", plainFirst.Code, plainFirst.Body.String())
	}
	var plainStarted playback.DecisionResponseV3
	if err := json.Unmarshal(plainFirst.Body.Bytes(), &plainStarted); err != nil || plainStarted.PlaybackPlan == nil {
		t.Fatalf("v1 start body: err=%v response=%#v", err, plainStarted)
	}
	if plainStarted.PlaybackPlan.TracksPending {
		t.Fatal("a v1 start entered the deferred lifecycle")
	}
	v2Retry := replayV3StartForHandlerTest(t, fx.handler, true, plainBody)
	if v2Retry.Code != http.StatusConflict || !strings.Contains(v2Retry.Body.String(), "playback_attempt_reused") {
		t.Fatalf("v2 deferred retry of an unnegotiated attempt = %d %s, want 409 playback_attempt_reused", v2Retry.Code, v2Retry.Body.String())
	}
}

// TestDeferredTrackInventoryCrossSurfaceReplanClosed proves the same guard
// protects a replan: a deferred-v2 attempt must not continue on the frozen v1
// replan route, whose response would carry tracks_pending and whose push/poll
// lifecycle v1 cannot consume.
func TestDeferredTrackInventoryCrossSurfaceReplanClosed(t *testing.T) {
	fx := newDeferredGateFixture(t, 736)
	req := v3HandlerStartRequest()
	req.FileID = fx.source.ID
	req.QualityPreference = "original"
	req.ClientFeatures = append(req.ClientFeatures, playback.FeatureDeferredTrackInventoryV3)
	first := replayV3StartForHandlerTest(t, fx.handler, true, req)
	if first.Code != http.StatusCreated {
		t.Fatalf("v2 deferred start: %d %s", first.Code, first.Body.String())
	}
	var started playback.DecisionResponseV3
	if err := json.Unmarshal(first.Body.Bytes(), &started); err != nil || started.PlaybackPlan == nil {
		t.Fatalf("decode start: %v", err)
	}

	body, err := json.Marshal(playback.ReplanRequestV3{
		ProtocolVersion: playback.ProtocolV3, Operation: playback.ReplanOperationSeekReanchorV3,
		PlaybackAttemptID: req.PlaybackAttemptID,
		ReplanRequestID:   "seek-reanchor-cross-surface", FailedPlanID: started.PlaybackPlan.PlanID,
		PlanAttemptID: "plan-attempt-cross-surface", PlanAttemptKey: playback.PlanAttemptKeyV3(*started.PlaybackPlan, req.ClientPlaybackContext.Output.OutputContextID, nil), AttemptCount: 1,
		QualityPreference: "original", PositionSeconds: 321,
		SelectedTracks: started.PlaybackPlan.SelectedTracks,
		Capabilities:   req.Capabilities, ClientPlaybackContext: req.ClientPlaybackContext,
	})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	handler := fx.handler
	handler.HandleReplanPlaybackV3(rr, withPlaybackRouteParam(httptest.NewRequest(http.MethodPost, "/api/v1/playback/"+started.SessionID+"/replan", strings.NewReader(string(body))).WithContext(newAuthorizedPlaybackContext()), "session_id", started.SessionID))
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "playback_attempt_reused") {
		t.Fatalf("v1 replan of a deferred-v2 attempt = %d %s, want 409 playback_attempt_reused", rr.Code, rr.Body.String())
	}
}

// TestDeferredTrackInventorySurfaceGuardMatrix pins the guard directly for both
// negotiation axes so a future edit cannot weaken one while the integration
// tests still pass through the other.
func TestDeferredTrackInventorySurfaceGuardMatrix(t *testing.T) {
	record := func(client, server []string) *playback.AttemptRecordV3 {
		r := &playback.AttemptRecordV3{}
		r.NormalizedRequest.ClientFeatures = client
		r.StartResponse.ServerFeatures = server
		return r
	}
	deferred := []string{playback.FeatureDeferredTrackInventoryV3}
	none := []string(nil)
	v1, v2 := t.Context(), WithNativeAPIV2(t.Context())

	for name, tc := range map[string]struct {
		ctx       context.Context
		record    *playback.AttemptRecordV3
		requested []string
		refused   bool
	}{
		"deferred v2 attempt is refused on v1":         {v1, record(deferred, deferred), deferred, true},
		"deferred v2 attempt replays on v2":            {v2, record(deferred, deferred), deferred, false},
		"unnegotiated attempt continues on v1":         {v1, record(none, none), none, false},
		"v2 deferred retry of an unnegotiated attempt": {v2, record(none, none), deferred, true},
		"v2 deferred retry of a declared-only attempt": {v2, record(deferred, none), deferred, true},
		"v2 deferred retry of a client-only attempt":   {v2, record(none, deferred), deferred, true},
		"v2 ordinary retry of an unnegotiated attempt": {v2, record(none, none), none, false},
	} {
		t.Run(name, func(t *testing.T) {
			if err := requireAttemptAPISurfaceV3(tc.ctx, tc.record, tc.requested); (err != nil) != tc.refused {
				t.Fatalf("refused = %v, want %v", err != nil, tc.refused)
			}
		})
	}
}

// forceConflictOnSavePlanStoreV3 drives the start path's concurrent-duplicate
// recovery branch deterministically: every idempotency lookup before the save
// reports the attempt absent, SaveAttempt then collides as if another request
// won the CAS, and the post-collision re-read returns the durable record so the
// recovery branch (and its surface guard) is actually exercised.
type forceConflictOnSavePlanStoreV3 struct {
	playback.PlanStoreV3
	collided bool
}

func (s *forceConflictOnSavePlanStoreV3) GetAttemptByPlaybackAttemptID(ctx context.Context, attemptID string) (*playback.AttemptRecordV3, error) {
	if s.collided {
		return s.PlanStoreV3.GetAttemptByPlaybackAttemptID(ctx, attemptID)
	}
	// Before the save collision, every idempotency lookup misses so the start
	// cannot take the ordinary replay path.
	return nil, playback.ErrSessionNotFound
}

func (s *forceConflictOnSavePlanStoreV3) SaveAttempt(context.Context, playback.AttemptRecordV3) error {
	s.collided = true
	return playback.ErrPlaybackAttemptExistsV3
}

// TestDeferredTrackInventoryConcurrentDuplicateReplayClosed drives the
// concurrent-duplicate branch of the start path: two identical starts race, the
// first wins the SaveAttempt CAS, and the loser re-reads the durable record and
// replays it through requireAttemptAPISurfaceV3. On one surface the duplicate
// must replay; a surface-crossing retry must be refused. The store wrapper makes
// the loser deterministic by missing the pre-save lookup and then colliding.
func TestDeferredTrackInventoryConcurrentDuplicateReplayClosed(t *testing.T) {
	request := func(fx *deferredGateFixture) playback.StartRequestV3 {
		req := v3HandlerStartRequest()
		req.FileID = fx.source.ID
		req.QualityPreference = "original"
		req.ClientFeatures = append(req.ClientFeatures, playback.FeatureDeferredTrackInventoryV3)
		return req
	}

	t.Run("same-surface duplicate replays", func(t *testing.T) {
		fx := newDeferredGateFixture(t, 737)
		req := request(fx)
		winner := replayV3StartForHandlerTest(t, fx.handler, true, req)
		if winner.Code != http.StatusCreated {
			t.Fatalf("winner start: %d %s", winner.Code, winner.Body.String())
		}
		var won playback.DecisionResponseV3
		if err := json.Unmarshal(winner.Body.Bytes(), &won); err != nil {
			t.Fatal(err)
		}
		conflicting := &forceConflictOnSavePlanStoreV3{PlanStoreV3: fx.handler.PlanStoreV3}
		fx.handler.PlanStoreV3 = conflicting
		loser := replayV3StartForHandlerTest(t, fx.handler, true, req)
		if loser.Code != http.StatusCreated {
			t.Fatalf("duplicate replay through the collision branch = %d %s, want a replay", loser.Code, loser.Body.String())
		}
		if !conflicting.collided {
			t.Fatal("the loser never hit the SaveAttempt collision; the recovery branch was not exercised")
		}
		var replayed playback.DecisionResponseV3
		if err := json.Unmarshal(loser.Body.Bytes(), &replayed); err != nil || replayed.PlaybackPlan == nil || !replayed.PlaybackPlan.TracksPending {
			t.Fatalf("duplicate replay body: err=%v response=%#v", err, replayed)
		}
		// A genuine replay of the durable record names the winner's session; a
		// freshly planned start would mint a new one and pass a weaker status
		// check.
		if replayed.SessionID != won.SessionID {
			t.Fatalf("duplicate replay session = %q, want the winner's %q", replayed.SessionID, won.SessionID)
		}
	})

	t.Run("cross-surface retry is refused", func(t *testing.T) {
		fx := newDeferredGateFixture(t, 738)
		req := request(fx)
		winner := replayV3StartForHandlerTest(t, fx.handler, true, req)
		if winner.Code != http.StatusCreated {
			t.Fatalf("winner start: %d %s", winner.Code, winner.Body.String())
		}
		fx.handler.PlanStoreV3 = &forceConflictOnSavePlanStoreV3{PlanStoreV3: fx.handler.PlanStoreV3}
		loser := replayV3StartForHandlerTest(t, fx.handler, false, req)
		if loser.Code != http.StatusConflict || !strings.Contains(loser.Body.String(), "playback_attempt_reused") {
			t.Fatalf("cross-surface duplicate = %d %s, want 409 playback_attempt_reused", loser.Code, loser.Body.String())
		}
	})
}

// seedTerminalSaveCollisionStore pre-loads one playable durable attempt (the
// concurrent winner) and returns a handler whose post-collision read finds it.
// The terminal-save collision branch re-reads by playback attempt id after a
// losing SaveAttempt, so the store must return the winner record there while the
// initial lookup (the ordinary replay path) is bypassed by the caller driving
// persistTerminalStartDecisionV3 directly.
func seedTerminalSaveCollisionStore(t *testing.T, record playback.AttemptRecordV3) *PlaybackHandler {
	t.Helper()
	manager := playback.NewSessionManager(0, 0)
	manager.RegisterReconstructed(&playback.Session{
		ID: record.SessionID, UserID: record.UserID, ProfileID: record.ProfileID,
		MediaFileID: record.EffectiveMediaFileID, RequestedMediaFileID: record.RequestedMediaFileID,
		PlayMethod: playback.PlayDirect,
	})
	handler := NewPlaybackHandler(manager)
	if err := handler.PlanStoreV3.SaveAttempt(context.Background(), record); err != nil {
		t.Fatalf("seed attempt: %v", err)
	}
	return handler
}

// TestTerminalSaveCollisionKeepsSurfaceGuard is the #1 regression: a losing
// terminal save must not replay the concurrent winner's playable plan through
// the wrong surface. The winner negotiated the deferred track-inventory
// lifecycle on /api/v2 (a plan carrying tracks_pending); the loser's terminal
// save collides, re-reads that record, and must refuse to hand the playable plan
// back through /api/v1. Terminal decisions (no playable plan) still replay on
// either surface, and the same-surface retry still replays.
func TestTerminalSaveCollisionKeepsSurfaceGuard(t *testing.T) {
	deferred := []string{playback.FeatureDeferredTrackInventoryV3}
	subrip := []string{playback.FeatureSubripSidecarV3}
	terminalReq := v3HandlerStartRequest()

	record := func(attemptID string, clientFeatures, serverFeatures []string) playback.AttemptRecordV3 {
		plan := playback.PlanV3{PlanID: "plan-terminal-collision", TracksPending: true}
		return playback.AttemptRecordV3{
			PlaybackAttemptID:    attemptID,
			SessionID:            "11111111-1111-1111-1111-111111111111",
			UserID:               1,
			ProfileID:            "profile-1",
			RequestedMediaFileID: 42,
			EffectiveMediaFileID: 42,
			ExpiresAt:            time.Now().Add(time.Hour),
			RequestDigest:        "digest-terminal-collision",
			NormalizedRequest:    playback.StartRequestV3{ClientFeatures: clientFeatures},
			StartResponse: playback.DecisionResponseV3{
				ProtocolVersion: playback.ProtocolV3,
				Outcome:         playback.OutcomePlayableV3,
				SessionID:       "11111111-1111-1111-1111-111111111111",
				ServerFeatures:  serverFeatures,
				PlaybackPlan:    &playback.PlanV3{PlanID: "plan-terminal-collision", SessionID: "11111111-1111-1111-1111-111111111111", TracksPending: true},
			},
			CurrentPlan: plan,
		}
	}

	call := func(t *testing.T, h *PlaybackHandler, v2 bool, attemptID string, clientFeatures []string) (playback.DecisionResponseV3, error) {
		t.Helper()
		req := terminalReq
		req.PlaybackAttemptID = attemptID
		req.ClientFeatures = append(append([]string(nil), req.ClientFeatures...), clientFeatures...)
		digests := playbackStartRequestDigestsV3{current: "digest-terminal-collision"}
		ctx := newAuthorizedPlaybackContext()
		if v2 {
			ctx = WithNativeAPIV2(ctx)
		}
		terminal := playback.NewTerminalResponseV3("source_unavailable", "The provider listed no streams for this title.", true)
		return h.persistTerminalStartDecisionV3(ctx, 1, "profile-1", req, digests, 42, 42, terminal)
	}

	t.Run("deferred v2 winner refused through v1 terminal save", func(t *testing.T) {
		h := seedTerminalSaveCollisionStore(t, record("attempt-tsc-deferred", deferred, deferred))
		if _, err := call(t, h, false, "attempt-tsc-deferred", deferred); err == nil {
			t.Fatal("v1 terminal save replayed a deferred-v2 playable plan; the surface guard must refuse it")
		} else if e, ok := errors.AsType[*PlaybackOperationError](err); !ok || e.Code != "playback_attempt_reused" {
			t.Fatalf("refusal = %v, want a playback_attempt_reused operation error", err)
		}
	})

	t.Run("subrip v2 winner refused through v1 terminal save", func(t *testing.T) {
		h := seedTerminalSaveCollisionStore(t, record("attempt-tsc-subrip", subrip, subrip))
		if _, err := call(t, h, false, "attempt-tsc-subrip", nil); err == nil {
			t.Fatal("v1 terminal save replayed an original-SRT v2 plan; the surface guard must refuse it")
		}
	})

	t.Run("same-surface terminal collision replays the winner", func(t *testing.T) {
		h := seedTerminalSaveCollisionStore(t, record("attempt-tsc-same", deferred, deferred))
		response, err := call(t, h, true, "attempt-tsc-same", deferred)
		if err != nil {
			t.Fatalf("same-surface terminal collision = %v, want the winner replay", err)
		}
		if response.PlaybackPlan == nil || !response.PlaybackPlan.TracksPending {
			t.Fatalf("same-surface replay = %#v, want the winner's provisional plan", response)
		}
	})

	t.Run("unnegotiated winner replays through v1", func(t *testing.T) {
		h := seedTerminalSaveCollisionStore(t, record("attempt-tsc-plain", nil, nil))
		if _, err := call(t, h, false, "attempt-tsc-plain", nil); err != nil {
			t.Fatalf("unnegotiated winner must replay through v1, got %v", err)
		}
	})

	t.Run("v2 deferred loser refused through an unnegotiated winner", func(t *testing.T) {
		// The other direction: the loser is the v2 deferred start and the
		// winner's stored plan never negotiated deferred_track_inventory_v1. The
		// guard must refuse it too, or the v2 client adopts a plan that promised a
		// follow-up it will never receive.
		h := seedTerminalSaveCollisionStore(t, record("attempt-tsc-v2loser", nil, nil))
		if _, err := call(t, h, true, "attempt-tsc-v2loser", deferred); err == nil {
			t.Fatal("v2 deferred terminal save replayed an unnegotiated v1 plan; the surface guard must refuse it")
		}
	})

	t.Run("terminal winner replays cross-surface", func(t *testing.T) {
		// A terminal publishes no plan, so the surface guard is deliberately
		// skipped for it: a terminal collision must still replay through the
		// other surface. This is the exception the playable-only guard protects.
		rec := record("attempt-tsc-terminal", deferred, deferred)
		rec.StartResponse = playback.NewTerminalResponseV3("source_unavailable", "The provider listed no streams for this title.", true)
		h := seedTerminalSaveCollisionStore(t, rec)
		response, err := call(t, h, false, "attempt-tsc-terminal", deferred)
		if err != nil {
			t.Fatalf("cross-surface terminal collision = %v, want the terminal replay", err)
		}
		if response.Terminal == nil || response.Terminal.Reason != "source_unavailable" {
			t.Fatalf("cross-surface terminal collision = %#v, want the terminal replay", response)
		}
	})
}
