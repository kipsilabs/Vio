package handlers

import (
	"context"
	"errors"
	"fmt"
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
