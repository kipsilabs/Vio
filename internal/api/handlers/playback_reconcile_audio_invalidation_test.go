package handlers

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// These tests cover the delivery side of cold-start default-audio
// reconciliation: the server records the corrected decision and withdraws the
// plan the client is playing, and only the client's own replan commits the
// replacement recipe. The assertions are about what the running player
// receives and what the committed attempt looks like immediately after
// reconciliation — not about the replan the client issues afterwards.

// reconcileInvalidationConn captures the realtime commands pushed to a session
// so a test can count them and read their payloads.
type reconcileInvalidationConn struct {
	mu       sync.Mutex
	messages []any
}

func (c *reconcileInvalidationConn) WriteJSON(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages = append(c.messages, v)
	return nil
}

func (c *reconcileInvalidationConn) all() []any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]any(nil), c.messages...)
}

func (c *reconcileInvalidationConn) commands() []playback.CommandEnvelope {
	var out []playback.CommandEnvelope
	for _, message := range c.all() {
		command, ok := message.(playback.CommandEnvelope)
		if !ok {
			continue
		}
		if command.Name == playback.CommandPlanInvalidated {
			out = append(out, command)
		}
	}
	return out
}

// invalidationFixture wires a reconcile handler whose live session has a
// realtime lane, so a plan withdrawal can actually be delivered.
//
// The reorder is the cold-start story: the declared inventory committed index 0
// (en) while the preference is pt, and the verified probe reverses the order
// so pt now sits at index 0. The two streams differ in codec and channel
// count, so the correction cannot reuse the committed transport — exactly the
// case where the player has to adopt a different route.
type invalidationFixture struct {
	handler  *PlaybackHandler
	session  *playback.Session
	record   *playback.AttemptRecordV3
	verified *models.MediaFile
	conn     *reconcileInvalidationConn
}

func newInvalidationFixture(t *testing.T, features []string, position float64) *invalidationFixture {
	t.Helper()
	planTime := []models.AudioTrack{
		{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
		{Index: 3, Language: "pt", Codec: "eac3", Channels: 6, Layout: "5.1"},
	}
	verified := &models.MediaFile{
		ID:       7,
		FilePath: "virtual://movie/tt-invalidated",
		AudioTracks: []models.AudioTrack{
			{Index: 3, Language: "pt", Codec: "eac3", Channels: 6, Layout: "5.1"},
			{Index: 1, Language: "en", Codec: "aac", Channels: 2, Layout: "stereo"},
		},
		ProbeUpdatedAt: &invalidationProbeStamp,
	}
	session := &playback.Session{
		ID: "11111111-1111-1111-1111-111111111111", UserID: 1, ProfileID: "profile-1",
		MediaFileID: 7, RequestedMediaFileID: 7, PlayMethod: playback.PlayDirect,
		AudioTrackIndex:               0,
		Position:                      position,
		SelectionOrigin:               SelectionOriginAuto,
		PreferredAudioLanguage:        "pt",
		SelectedAudioSignature:        playback.AudioTrackSignatureFromTrack(planTime[0]),
		VirtualAudioTracks:            planTime,
		VirtualSourceURI:              "virtual://movie/tt-invalidated",
		VirtualSubtitleEvidenceURI:    "virtual://movie/tt-invalidated",
		VirtualSubtitleEvidenceFileID: 7,
		VirtualSubtitleEvidenceSet:    true,
	}
	record := reconcileIntent("pt", nil, playback.AudioTrackSignatureFromTrack(planTime[0]))
	record.NormalizedRequest.ClientFeatures = features
	// The canonical request is a real replan body, so it must satisfy the
	// replan contract's own validation: a fixture that skipped this would
	// prove nothing about a request that can never be replayed.
	record.NormalizedRequest.Capabilities = reconcileCapabilities()
	record.CurrentPlan.PlanAttemptKey = "v3:plan-reconcile-1:a:b"
	record.CurrentPlan.SelectedTracks.Audio = &playback.TrackIdentityV3{ID: playback.TrackIDV3(7, "audio", 0), Index: intPtrReconcile(0)}
	record.FrozenRecipe.TargetVideoCodec = "h264"

	handler := reconcileFixture(t, session, record, verified)
	handler.RealtimeHub = playback.NewRealtimeHub()
	conn := &reconcileInvalidationConn{}
	registration := handler.RealtimeHub.Register(session.ID, conn)
	if registration == nil {
		t.Fatal("expected a realtime registration for the reconcile session")
	}
	t.Cleanup(func() { handler.RealtimeHub.Unregister(registration) })
	return &invalidationFixture{handler: handler, session: session, record: record, verified: verified, conn: conn}
}

var invalidationProbeStamp = time.Now().UTC()

// reconcile runs the sweep and returns the attempt row afterwards.
func (f *invalidationFixture) reconcile(t *testing.T) *playback.AttemptRecordV3 {
	t.Helper()
	f.handler.reconcileSessionDefaultAudio(context.Background(), f.session, f.verified.ID)
	record, err := f.handler.PlanStoreV3.GetAttempt(context.Background(), f.session.ID)
	if err != nil {
		t.Fatalf("get attempt after reconcile: %v", err)
	}
	return record
}

// TestReconcileAudioReplanAdoptsThroughPlanInvalidated is the running-stream
// adoption case: reconciliation persists the corrected decision, emits exactly
// one withdrawal naming the previously-active plan, and commits no replacement
// recipe of its own. The client's own replan then consumes the same decision
// and lands on the corrected audio index with its position preserved.
func TestReconcileAudioReplanAdoptsThroughPlanInvalidated(t *testing.T) {
	f := newInvalidationFixture(t, []string{playback.FeaturePlanInvalidatedV3}, 321.5)
	activePlanID := f.record.CurrentPlanID
	activeStreamURL := f.record.CurrentPlan.Stream.URL

	after := f.reconcile(t)

	// (a) No self-committed replacement: the active plan and its committed
	// recipe are untouched, so a server-side commit cannot have happened.
	if after.CurrentPlanID != activePlanID {
		t.Fatalf("active plan = %q, want the pre-reconciliation plan %q: reconciliation must not commit a replacement",
			after.CurrentPlanID, activePlanID)
	}
	if after.CurrentPlan.Stream.URL != activeStreamURL {
		t.Fatal("reconciliation must not rewrite the committed stream URL")
	}
	if after.FrozenRecipe.TargetVideoCodec != f.record.FrozenRecipe.TargetVideoCodec {
		t.Fatal("reconciliation must not commit a replacement recipe")
	}

	// The corrected selection and its canonical request are durable BEFORE
	// anything was emitted, which is what makes the client's replan replayable.
	entry := playback.FindAudioReconcileEntry(after.AudioReconcileLedger, reconcileGenerationV3(f.verified), f.session.ID)
	if entry == nil {
		t.Fatal("the settled decision must be recorded on the attempt ledger")
	}
	if entry.Decision != playback.AudioReconcileInvalidated {
		t.Fatalf("decision = %q, want %q", entry.Decision, playback.AudioReconcileInvalidated)
	}
	if entry.AudioIndex == nil || *entry.AudioIndex != 0 {
		t.Fatalf("corrected audio index = %v, want 0 (pt at its verified position)", entry.AudioIndex)
	}
	if entry.PlanID != activePlanID {
		t.Fatalf("decision plan id = %q, want the active plan %q", entry.PlanID, activePlanID)
	}
	if entry.RequestDigest == "" || entry.Request == nil {
		t.Fatal("the canonical request and its digest must be persisted with the decision")
	}

	// (b) Exactly one withdrawal, naming the previously-active plan and the
	// automatic-reconciliation reason.
	commands := f.conn.commands()
	if len(commands) != 1 {
		t.Fatalf("plan_invalidated pushes = %d, want exactly 1", len(commands))
	}
	command := commands[0]
	if command.SessionID != f.session.ID {
		t.Fatalf("command session = %q, want %q", command.SessionID, f.session.ID)
	}
	if command.Reason != playback.PlanInvalidatedDefaultAudioReconciliation {
		t.Fatalf("command reason = %q, want %q", command.Reason, playback.PlanInvalidatedDefaultAudioReconciliation)
	}
	if command.DeadlineMS == 0 {
		t.Fatal("the withdrawal must carry the documented replan deadline")
	}
	var payload struct {
		PlanID     string `json:"plan_id"`
		Reason     string `json:"reason"`
		Generation string `json:"generation"`
	}
	if err := json.Unmarshal(command.Payload, &payload); err != nil {
		t.Fatalf("withdrawal payload: %v", err)
	}
	if payload.PlanID != activePlanID {
		t.Fatalf("withdrawal plan_id = %q, want the active plan %q", payload.PlanID, activePlanID)
	}
	if payload.Reason != playback.PlanInvalidatedDefaultAudioReconciliation {
		t.Fatalf("withdrawal payload reason = %q, want %q", payload.Reason, playback.PlanInvalidatedDefaultAudioReconciliation)
	}
	if payload.Generation != entry.Generation {
		t.Fatalf("withdrawal generation = %q, want the settled generation %q", payload.Generation, entry.Generation)
	}

	// (c) The client-driven replan consumes the SAME decision and lands on the
	// corrected audio index.
	req := clientReplanForInvalidation(f, activePlanID)
	applied := replayRequestForTest(req)
	f.handler.pendingAudioReconciliationReplan(after, &applied)
	if applied.SelectedTracks.Audio == nil {
		t.Fatal("the replacement replan must carry the corrected audio selection")
	}
	if applied.SelectedTracks.Audio.ID != playback.TrackIDV3(f.verified.ID, "audio", 0) {
		t.Fatalf("replan audio track id = %q, want the corrected track %q",
			applied.SelectedTracks.Audio.ID, playback.TrackIDV3(f.verified.ID, "audio", 0))
	}
	if applied.SelectedTracks.Audio.Index == nil || *applied.SelectedTracks.Audio.Index != 0 {
		t.Fatalf("replan audio index = %v, want 0", applied.SelectedTracks.Audio.Index)
	}

	// (d) The viewer's position survives adoption: the correction replaces the
	// audio identity only and leaves everything the client sent intact.
	if applied.PositionSeconds != 321.5 {
		t.Fatalf("replan position = %v, want the client's live position 321.5", applied.PositionSeconds)
	}
	if applied.ReplanRequestID != "client-replan-1" || applied.FailedPlanID != activePlanID {
		t.Fatal("the correction must not rewrite the client's own request identity")
	}
	if applied.Automatic != "" {
		t.Fatal("a client-issued replan must stay client-issued")
	}
}

// TestReconcileAudioNoDoubleCorrection covers the duplicate-evidence case: a
// second probe write and a second heartbeat on a settled generation emit
// nothing, and the stored canonical request replays verbatim.
func TestReconcileAudioNoDoubleCorrection(t *testing.T) {
	f := newInvalidationFixture(t, []string{playback.FeaturePlanInvalidatedV3}, 12)

	after := f.reconcile(t)
	entry := playback.FindAudioReconcileEntry(after.AudioReconcileLedger, reconcileGenerationV3(f.verified), f.session.ID)
	if entry == nil {
		t.Fatal("the first evaluation must settle the generation")
	}

	// A duplicate probe write: the same verified row persisted again.
	f.reconcile(t)
	// A second heartbeat after the generation settled.
	f.handler.reconcilePendingAudioStartup(context.Background(), f.session.ID)

	if got := len(f.conn.commands()); got != 1 {
		t.Fatalf("plan_invalidated pushes = %d after duplicate write and heartbeat, want exactly 1", got)
	}
	replayed, err := f.handler.PlanStoreV3.GetAttempt(context.Background(), f.session.ID)
	if err != nil {
		t.Fatalf("get attempt: %v", err)
	}
	replayEntry := playback.FindAudioReconcileEntry(replayed.AudioReconcileLedger, entry.Generation, f.session.ID)
	if replayEntry == nil {
		t.Fatal("the settled decision must survive the duplicate evaluation")
	}
	if replayEntry.RequestDigest != entry.RequestDigest {
		t.Fatalf("replayed digest = %q, want the stored %q", replayEntry.RequestDigest, entry.RequestDigest)
	}
	if replayEntry.Request == nil || replayEntry.Request.ReplanRequestID != entry.Request.ReplanRequestID {
		t.Fatal("a replayed decision must reuse the stored canonical request verbatim")
	}
	if len(replayed.AudioReconcileLedger.Entries) != 1 {
		t.Fatalf("ledger entries = %d, want 1: a settled generation is never re-decided",
			len(replayed.AudioReconcileLedger.Entries))
	}
}

// TestReconcileAudioImmutableIdentity is the divergence case: two evaluations
// of one generation whose canonical bodies differ (the viewer moved between
// them) must not both proceed. The second is refused and recorded, and only one
// replan is ever issued.
func TestReconcileAudioImmutableIdentity(t *testing.T) {
	f := newInvalidationFixture(t, []string{playback.FeaturePlanInvalidatedV3}, 10)

	after := f.reconcile(t)
	entry := playback.FindAudioReconcileEntry(after.AudioReconcileLedger, reconcileGenerationV3(f.verified), f.session.ID)
	if entry == nil || entry.RequestDigest == "" {
		t.Fatal("the first evaluation must settle the generation with a canonical digest")
	}

	// The viewer keeps watching: the live position moves, then a second
	// evaluator of the SAME generation settles. That is the concurrent case —
	// two evaluators that both read the ledger before either wrote. The
	// canonical request embeds the live position, so the second body digests
	// differently while its id (plan + target index) is unchanged.
	if err := f.handler.sessionMgr.UpdateProgress(f.session.ID, 999.5, false); err != nil {
		t.Fatalf("advance progress: %v", err)
	}
	moved := reconcileLiveSession(t, f.handler, f.session.ID)
	settled, err := f.handler.PlanStoreV3.GetAttempt(context.Background(), f.session.ID)
	if err != nil {
		t.Fatalf("get attempt for second evaluator: %v", err)
	}
	f.handler.settleAudioReconcileInvalidation(context.Background(), moved, settled, f.verified, 0)

	if got := len(f.conn.commands()); got != 1 {
		t.Fatalf("plan_invalidated pushes = %d after a diverged re-evaluation, want exactly 1", got)
	}

	settled, err = f.handler.PlanStoreV3.GetAttempt(context.Background(), f.session.ID)
	if err != nil {
		t.Fatalf("get attempt: %v", err)
	}
	// The stored decision is unchanged: the first writer wins.
	stored := playback.FindAudioReconcileEntry(settled.AudioReconcileLedger, entry.Generation, f.session.ID)
	if stored == nil {
		t.Fatal("the settled decision must survive the diverged evaluation")
	}
	if stored.RequestDigest != entry.RequestDigest {
		t.Fatalf("stored digest = %q, want the first writer's %q", stored.RequestDigest, entry.RequestDigest)
	}
	// And the divergence is recorded as an explicit refusal, so it is
	// auditable rather than silently dropped.
	refusals := 0
	for _, e := range settled.AudioReconcileLedger.Entries {
		if e.Decision == playback.AudioReconcileRefused {
			refusals++
			if e.RequestDigest == entry.RequestDigest {
				t.Fatal("the refusal must name the rejected digest, not the stored one")
			}
		}
	}
	if refusals != 1 {
		t.Fatalf("refusal entries = %d, want exactly 1", refusals)
	}

	// A retry of the same canonical decision replays the identical stored
	// digest and issues no second replan.
	retry := *stored.Request
	if ReplanDigestV3(mustMarshalReconcile(t, retry)) != stored.RequestDigest {
		t.Fatal("replaying the stored canonical request must reproduce the stored digest")
	}
}

// TestReconcileAudioWithdrawalSkippedWithoutCapability is the capability gate:
// a client that never advertised plan_invalidated_v1 gets no event, and its
// decision is still recorded so the correction lands on its next start.
func TestReconcileAudioWithdrawalSkippedWithoutCapability(t *testing.T) {
	f := newInvalidationFixture(t, []string{playback.FeaturePlaybackPlanV3}, 5)

	after := f.reconcile(t)

	if got := len(f.conn.commands()); got != 0 {
		t.Fatalf("plan_invalidated pushes = %d for a client without plan_invalidated_v1, want 0", got)
	}
	entry := playback.FindAudioReconcileEntry(after.AudioReconcileLedger, reconcileGenerationV3(f.verified), f.session.ID)
	if entry == nil {
		t.Fatal("the decision must still be recorded without the capability")
	}
	if entry.Decision != playback.AudioReconcileInvalidated || entry.Request == nil {
		t.Fatalf("decision = %+v, want a recorded invalidation with its canonical request", entry)
	}
	if after.CurrentPlanID != f.record.CurrentPlanID {
		t.Fatal("an unnegotiated client must not get its plan replaced server-side either")
	}
}

// TestReconcileAudioWithdrawalReachesOnlyTheOwnerLane pins the multi-node
// contract: the withdrawal travels the session's own hub lane, so it reaches
// the replica that owns that session's control socket and nothing else. The
// ledger, not a cross-replica RPC, is what keeps another replica from
// emitting a duplicate.
func TestReconcileAudioWithdrawalReachesOnlyTheOwnerLane(t *testing.T) {
	f := newInvalidationFixture(t, []string{playback.FeaturePlanInvalidatedV3}, 7)
	other := &reconcileInvalidationConn{}
	otherRegistration := f.handler.RealtimeHub.Register("22222222-2222-2222-2222-222222222222", other)
	if otherRegistration == nil {
		t.Fatal("expected a second realtime registration")
	}
	t.Cleanup(func() { f.handler.RealtimeHub.Unregister(otherRegistration) })

	f.reconcile(t)

	if got := len(f.conn.commands()); got != 1 {
		t.Fatalf("owner lane pushes = %d, want 1", got)
	}
	if got := len(other.commands()); got != 0 {
		t.Fatalf("other session pushes = %d, want 0: the event is per session", got)
	}
	if command := f.conn.commands()[0]; command.SessionID != f.session.ID {
		t.Fatalf("command session = %q, want %q", command.SessionID, f.session.ID)
	}
}

// TestPendingAudioReconciliationConsumesOnce pins the one-shot nature of the
// consumption: the correction applies to the replan that replans off the
// withdrawn plan, and to nothing else.
func TestPendingAudioReconciliationConsumesOnce(t *testing.T) {
	audioIndex := 0
	record := &playback.AttemptRecordV3{
		SessionID:   "11111111-1111-1111-1111-111111111111",
		CurrentPlan: playback.PlanV3{PlanID: "plan:current"},
		AudioReconcileLedger: playback.AudioReconcileLedgerV3{Entries: []playback.AudioReconcileEntryV3{{
			Generation:    "gen-1",
			SessionID:     "11111111-1111-1111-1111-111111111111",
			Decision:      playback.AudioReconcileInvalidated,
			AudioIndex:    &audioIndex,
			PlanID:        "plan:withdrawn",
			RequestDigest: "digest-1",
			Request: &playback.ReplanRequestV3{SelectedTracks: playback.SelectedTracksV3{
				Audio: &playback.TrackIdentityV3{ID: "file:7:audio:0", Index: intPtrReconcile(0)},
			}},
		}}},
	}
	h := &PlaybackHandler{}
	f := &invalidationFixture{session: &playback.Session{ID: record.SessionID}, record: record}

	answering := clientReplanForInvalidation(f, "plan:withdrawn")
	h.pendingAudioReconciliationReplan(record, &answering)
	if answering.SelectedTracks.Audio == nil || answering.SelectedTracks.Audio.ID != "file:7:audio:0" {
		t.Fatal("the replan off the withdrawn plan must carry the correction")
	}

	// The next replan runs off the replacement plan, so the correction is not
	// re-applied over whatever the viewer chose afterwards.
	later := clientReplanForInvalidation(f, "plan:current")
	h.pendingAudioReconciliationReplan(record, &later)
	if later.SelectedTracks.Audio != nil {
		t.Fatal("a replan off the replacement plan must keep the client's own selection")
	}

	// The server's own automatic replan already names the corrected selection.
	automatic := clientReplanForInvalidation(f, "plan:withdrawn")
	automatic.Automatic = playback.ReplanAutomaticV3
	automatic.SelectedTracks.Audio = &playback.TrackIdentityV3{ID: "file:7:audio:2", Index: intPtrReconcile(2)}
	h.pendingAudioReconciliationReplan(record, &automatic)
	if automatic.SelectedTracks.Audio.ID != "file:7:audio:2" {
		t.Fatal("the automatic path must not be overridden by the stored correction")
	}
}

// clientReplanForInvalidation builds the failure_recovery body a client issues
// when it answers a plan withdrawal: it names the plan it was playing, carries
// its live position, and selects no audio track of its own.
func clientReplanForInvalidation(f *invalidationFixture, failedPlanID string) playback.ReplanRequestV3 {
	return playback.ReplanRequestV3{
		ProtocolVersion:   playback.ProtocolV3,
		Operation:         playback.ReplanOperationFailureRecoveryV3,
		PlaybackAttemptID: f.record.PlaybackAttemptID,
		ReplanRequestID:   "client-replan-1",
		FailedPlanID:      failedPlanID,
		PlanAttemptID:     "client-plan-attempt-1",
		PlanAttemptKey:    f.record.CurrentPlan.PlanAttemptKey,
		AttemptCount:      2,
		PositionSeconds:   321.5,
		Failure:           playback.FailureV3{Classification: playback.PlanInvalidatedDefaultAudioReconciliation},
	}
}

// replayRequestForTest deep-copies a request the way the replan handler does
// before overlaying the correction, so a test asserts on the same value the
// replan path sees rather than on a shared pointer.
func replayRequestForTest(req playback.ReplanRequestV3) playback.ReplanRequestV3 {
	copied := req
	if req.SelectedTracks.Audio != nil {
		audio := *req.SelectedTracks.Audio
		copied.SelectedTracks.Audio = &audio
	}
	return copied
}

func mustMarshalReconcile(t *testing.T, req playback.ReplanRequestV3) []byte {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal reconcile request: %v", err)
	}
	return body
}
