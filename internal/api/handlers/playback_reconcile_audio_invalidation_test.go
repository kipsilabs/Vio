package handlers

import (
	"context"
	"encoding/json"
	"errors"
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
	f := newInvalidationFixture(t, []string{playback.FeaturePlanInvalidatedV3, playback.FeatureDefaultAudioReconcileResponseV3}, 321.5)
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
	// The correction replays start-time intent the viewer never re-picked, so
	// the server restores its own Automatic provenance on the request it
	// amends. Without it the correction would be persisted as a viewer
	// preference and steer every later start. The client cannot set this: the
	// inbound boundary strips whatever arrives on the wire.
	if applied.Automatic != playback.ReplanAutomaticV3 {
		t.Fatalf("Automatic = %q, want the restored %q so the correction is not stored as a viewer preference",
			applied.Automatic, playback.ReplanAutomaticV3)
	}
}

// failingSendConn is a RealtimeConnection whose WriteJSON fails, so the hub
// send reports an error and announceAudioReconcileInvalidation leaves
// AnnouncedAt nil. It models a client that connected but whose lane the hub
// has not accepted yet, or a replica without the session owner.
type failingSendConn struct{}

func (failingSendConn) WriteJSON(_ any) error { return errors.New("client not attached yet") }

// TestReconcileAudioWithdrawalRetriedUntilCapableClientAcks covers the
// durable-delivery fix: a withdrawal whose first announce failed is retried
// on a later reconcile pass once a healthy hub lane exists, the entry then
// carries AnnouncedAt, and a third pass emits nothing.
func TestReconcileAudioWithdrawalRetriedUntilCapableClientAcks(t *testing.T) {
	f := newInvalidationFixture(t, []string{playback.FeaturePlanInvalidatedV3, playback.FeatureDefaultAudioReconcileResponseV3}, 12)

	// First pass with a failing/nil hub send: the decision settles but
	// AnnouncedAt stays nil.
	f.handler.RealtimeHub = playback.NewRealtimeHub()
	failingRegistration := f.handler.RealtimeHub.Register(f.session.ID, failingSendConn{})
	t.Cleanup(func() { f.handler.RealtimeHub.Unregister(failingRegistration) })

	after := f.reconcile(t)
	entry := playback.FindAudioReconcileEntry(after.AudioReconcileLedger, reconcileGenerationV3(f.verified), f.session.ID)
	if entry == nil {
		t.Fatal("the first evaluation must settle the generation")
	}
	if entry.AnnouncedAt != nil {
		t.Fatalf("AnnouncedAt must stay nil when the hub send failed, got %v", entry.AnnouncedAt)
	}
	if got := len(f.conn.commands()); got != 0 {
		t.Fatalf("failing hub must deliver no plan_invalidated, got %d", got)
	}

	// Swap in a healthy lane; a later reconcile pass must retry the
	// withdrawal and stamp AnnouncedAt.
	f.handler.RealtimeHub.Unregister(failingRegistration)
	healthyRegistration := f.handler.RealtimeHub.Register(f.session.ID, f.conn)
	t.Cleanup(func() { f.handler.RealtimeHub.Unregister(healthyRegistration) })

	afterRetry := f.reconcile(t)
	retryEntry := playback.FindAudioReconcileEntry(afterRetry.AudioReconcileLedger, entry.Generation, f.session.ID)
	if retryEntry == nil {
		t.Fatal("the settled decision must survive the retry")
	}
	if retryEntry.AnnouncedAt == nil {
		t.Fatal("AnnouncedAt must be set after a successful announce")
	}
	if got := len(f.conn.commands()); got != 1 {
		t.Fatalf("a healthy hub must receive exactly one plan_invalidated on the retry, got %d", got)
	}

	// A third pass on the announced entry must emit nothing.
	f.reconcile(t)
	if got := len(f.conn.commands()); got != 1 {
		t.Fatalf("an announced entry must never re-emit, got %d pushes", got)
	}
}

// TestReconcileAudioAnnouncedOnceStaysSilent covers the no-double-correction
// invariant from the other side: a settled+announced entry never re-emits on
// a subsequent heartbeat.
func TestReconcileAudioAnnouncedOnceStaysSilent(t *testing.T) {
	f := newInvalidationFixture(t, []string{playback.FeaturePlanInvalidatedV3, playback.FeatureDefaultAudioReconcileResponseV3}, 12)

	after := f.reconcile(t)
	entry := playback.FindAudioReconcileEntry(after.AudioReconcileLedger, reconcileGenerationV3(f.verified), f.session.ID)
	if entry == nil || entry.AnnouncedAt == nil {
		t.Fatal("the first evaluation must settle and announce the generation")
	}
	if got := len(f.conn.commands()); got != 1 {
		t.Fatalf("the first evaluation must emit exactly one push, got %d", got)
	}

	// Second heartbeat on a settled+announced generation: silent.
	f.handler.reconcilePendingAudioStartup(context.Background(), f.session.ID)
	if got := len(f.conn.commands()); got != 1 {
		t.Fatalf("an announced entry must never re-emit on a heartbeat, got %d pushes", got)
	}
}

// TestReconcileAudioNoAnnounceWithoutResponseCapability pins the gate: a
// settled decision for a client that negotiated plan_invalidated_v1 but NOT
// default_audio_reconcile_response_v1 is never announced.
func TestReconcileAudioNoAnnounceWithoutResponseCapability(t *testing.T) {
	f := newInvalidationFixture(t, []string{playback.FeaturePlanInvalidatedV3}, 12)

	after := f.reconcile(t)
	entry := playback.FindAudioReconcileEntry(after.AudioReconcileLedger, reconcileGenerationV3(f.verified), f.session.ID)
	if entry == nil {
		t.Fatal("the generation must settle even without the capability")
	}
	if entry.AnnouncedAt != nil {
		t.Fatalf("AnnouncedAt must stay nil when the response capability was not negotiated, got %v", entry.AnnouncedAt)
	}
	if got := len(f.conn.commands()); got != 0 {
		t.Fatalf("a client without the response capability must receive no plan_invalidated, got %d", got)
	}
}

// TestReconcileAudioNoDoubleCorrection covers the duplicate-evidence case: a
// second probe write and a second heartbeat on a settled generation emit
// nothing, and the stored canonical request replays verbatim.
func TestReconcileAudioNoDoubleCorrection(t *testing.T) {
	f := newInvalidationFixture(t, []string{playback.FeaturePlanInvalidatedV3, playback.FeatureDefaultAudioReconcileResponseV3}, 12)

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
	f := newInvalidationFixture(t, []string{playback.FeaturePlanInvalidatedV3, playback.FeatureDefaultAudioReconcileResponseV3}, 10)

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
// The pending correction must be layered onto the executable selection, not
// onto the request after the selection was already applied: audio resolution
// reads start.AudioTrackID/AudioTrackIndex, so a correction applied afterwards
// never reaches the plan or the executor's audio map.
func TestPendingAudioCorrectionReachesTheStartSelection(t *testing.T) {
	f := newInvalidationFixture(t, []string{playback.FeaturePlanInvalidatedV3}, 10)

	after := f.reconcile(t)
	entry := playback.FindAudioReconcileEntry(after.AudioReconcileLedger, reconcileGenerationV3(f.verified), f.session.ID)
	if entry == nil || entry.Request == nil || entry.Request.SelectedTracks.Audio == nil {
		t.Fatal("expected a settled correction carrying an audio identity")
	}

	// A client replan off the withdrawn plan, with no audio identity of its own.
	start := after.NormalizedRequest
	req := &playback.ReplanRequestV3{
		ProtocolVersion: playback.ProtocolV3,
		Operation:       playback.ReplanOperationTrackChangeV3,
		FailedPlanID:    entry.PlanID,
	}
	f.handler.pendingAudioReconciliationReplan(after, req)
	if req.SelectedTracks.Audio == nil {
		t.Fatal("the replan did not consume the pending correction")
	}

	// Now the ordering that actually matters: applying the selection must
	// happen AFTER the correction, exactly as the replan overlay does.
	applySelectedTracksToStartV3(&start, req.SelectedTracks)
	if start.AudioTrackID == "" && start.AudioTrackIndex == nil {
		t.Fatal("the corrected audio identity never reached the start selection")
	}
	if start.AudioTrackIndex == nil || entry.Request.SelectedTracks.Audio.Index == nil {
		t.Fatal("the corrected audio identity did not carry an index onto the start selection")
	}
	if *start.AudioTrackIndex != *entry.Request.SelectedTracks.Audio.Index {
		t.Fatalf("start audio index = %d, want the corrected %d",
			*start.AudioTrackIndex, *entry.Request.SelectedTracks.Audio.Index)
	}
}

// A refusal recorded by a later evaluator must not erase the correction the
// winning evaluator already settled and announced: two concurrent evaluations
// that captured different playheads would otherwise strand the correction.
func TestRefusalDoesNotStrandTheSettledCorrection(t *testing.T) {
	f := newInvalidationFixture(t, []string{playback.FeaturePlanInvalidatedV3, playback.FeatureDefaultAudioReconcileResponseV3}, 10)

	after := f.reconcile(t)
	entry := playback.FindAudioReconcileEntry(after.AudioReconcileLedger, reconcileGenerationV3(f.verified), f.session.ID)
	if entry == nil {
		t.Fatal("expected a settled correction")
	}

	// A diverged re-evaluation records an explicit refusal for the same
	// (generation, session).
	record, err := f.handler.PlanStoreV3.GetAttempt(context.Background(), f.session.ID)
	if err != nil {
		t.Fatalf("get attempt: %v", err)
	}
	record.AudioReconcileLedger.Entries = append(record.AudioReconcileLedger.Entries,
		playback.AudioReconcileEntryV3{
			Generation:    entry.Generation,
			SessionID:     f.session.ID,
			PlanID:        entry.PlanID,
			Decision:      playback.AudioReconcileRefused,
			AudioIndex:    entry.AudioIndex,
			RequestDigest: "divergent-digest",
		})

	found := FindPendingAudioReconciliation(record, entry.PlanID)
	if found == nil {
		t.Fatal("a later refusal stranded the already-announced correction")
	}
	if found.RequestDigest != entry.RequestDigest {
		t.Fatalf("pending correction digest = %q, want the winning %q", found.RequestDigest, entry.RequestDigest)
	}
}

// The web request builder echoes the current plan's audio identity on every
// replan (buildReplanRequestV3), so the answering request normally carries the
// selection the withdrawn plan already had, even though the viewer chose
// nothing. Treating presence as intent skipped reconciliation on exactly that
// path and left the pre-reorder stream playing.
//
// Note the reorder case makes ID comparison a weak discriminator on its own:
// the correction targets the same ordinal the withdrawn plan named, and the
// identity string is derived from that ordinal, so the corrected and echoed
// identities are byte-equal while naming different underlying streams. The
// policy is therefore deliberately asymmetric — an identity the withdrawn plan
// did not select is a viewer choice and wins; anything else (echo, or no
// identity at all) is the reconciliation response, so the correction is
// applied and marked automatic. The cost of being wrong in that direction is a
// redundant viewer pick not being persisted, which is far smaller than writing
// a server decision into a stored preference.
func TestInheritedAudioEchoIsTreatedAsReconciliationResponse(t *testing.T) {
	f := newInvalidationFixture(t, []string{playback.FeaturePlanInvalidatedV3}, 10)
	after := f.reconcile(t)
	entry := playback.FindAudioReconcileEntry(after.AudioReconcileLedger, reconcileGenerationV3(f.verified), f.session.ID)
	if entry == nil || entry.Request == nil || entry.Request.SelectedTracks.Audio == nil {
		t.Fatal("expected a settled correction carrying an audio identity")
	}

	// The real client shape: the builder echoes the plan's own audio.
	echoed := *after.CurrentPlan.SelectedTracks.Audio
	req := &playback.ReplanRequestV3{
		ProtocolVersion: playback.ProtocolV3,
		Operation:       playback.ReplanOperationTrackChangeV3,
		FailedPlanID:    entry.PlanID,
		SelectedTracks:  playback.SelectedTracksV3{Audio: &echoed},
	}
	f.handler.pendingAudioReconciliationReplan(after, req)

	if req.SelectedTracks.Audio == nil {
		t.Fatal("the correction was skipped on an inherited echo")
	}
	if !sameTrackIdentityV3(req.SelectedTracks.Audio, entry.Request.SelectedTracks.Audio) {
		t.Fatalf("echoed identity was not replaced by the correction: got %+v, want %+v",
			req.SelectedTracks.Audio, entry.Request.SelectedTracks.Audio)
	}
	if req.Automatic != playback.ReplanAutomaticV3 {
		t.Fatalf("Automatic = %q, want %q: an echoed response is still a server correction and must not persist as a viewer preference",
			req.Automatic, playback.ReplanAutomaticV3)
	}

	// No identity at all is the same case: the correction fills it in.
	bare := &playback.ReplanRequestV3{
		ProtocolVersion: playback.ProtocolV3,
		Operation:       playback.ReplanOperationTrackChangeV3,
		FailedPlanID:    entry.PlanID,
	}
	f.handler.pendingAudioReconciliationReplan(after, bare)
	if bare.SelectedTracks.Audio == nil || bare.Automatic != playback.ReplanAutomaticV3 {
		t.Fatal("a request with no audio identity must still receive the correction, marked automatic")
	}
}

// A viewer who picks a different track while the withdrawal is in flight is
// making a choice, and it supersedes the automatic correction.
func TestExplicitAudioChoiceSupersedesPendingCorrection(t *testing.T) {
	f := newInvalidationFixture(t, []string{playback.FeaturePlanInvalidatedV3, playback.FeatureDefaultAudioReconcileResponseV3}, 10)
	after := f.reconcile(t)
	entry := playback.FindAudioReconcileEntry(after.AudioReconcileLedger, reconcileGenerationV3(f.verified), f.session.ID)
	if entry == nil || entry.Request == nil || entry.Request.SelectedTracks.Audio == nil {
		t.Fatal("expected a settled correction carrying an audio identity")
	}

	// Something the withdrawn plan did not select.
	chosen := &playback.TrackIdentityV3{ID: playback.TrackIDV3(7, "audio", 99)}
	req := &playback.ReplanRequestV3{
		ProtocolVersion: playback.ProtocolV3,
		Operation:       playback.ReplanOperationTrackChangeV3,
		FailedPlanID:    entry.PlanID,
		SelectedTracks:  playback.SelectedTracksV3{Audio: chosen},
	}
	f.handler.pendingAudioReconciliationReplan(after, req)
	if req.SelectedTracks.Audio == nil || req.SelectedTracks.Audio.ID != chosen.ID {
		t.Fatalf("the viewer's choice was overwritten: got %+v", req.SelectedTracks.Audio)
	}
	if req.Automatic == playback.ReplanAutomaticV3 {
		t.Fatal("a viewer's explicit change must not be marked as an automatic correction")
	}
}

func TestPendingCorrectionRestoresAutomaticProvenance(t *testing.T) {
	f := newInvalidationFixture(t, []string{playback.FeaturePlanInvalidatedV3, playback.FeatureDefaultAudioReconcileResponseV3}, 10)
	after := f.reconcile(t)
	entry := playback.FindAudioReconcileEntry(after.AudioReconcileLedger, reconcileGenerationV3(f.verified), f.session.ID)
	if entry == nil {
		t.Fatal("expected a settled correction")
	}

	// The client cannot forge this: the inbound boundary strips it.
	req := &playback.ReplanRequestV3{
		ProtocolVersion: playback.ProtocolV3,
		Operation:       playback.ReplanOperationTrackChangeV3,
		FailedPlanID:    entry.PlanID,
	}
	req.AnswersPlanInvalidation = playback.PlanInvalidatedDefaultAudioReconciliation
	stripClientSuppliedAutomatic(req)
	if req.Automatic != "" {
		t.Fatalf("inbound marker survived: %q", req.Automatic)
	}

	f.handler.pendingAudioReconciliationReplan(after, req)
	if req.Automatic != playback.ReplanAutomaticV3 {
		t.Fatalf("Automatic = %q, want the restored %q so the correction is not stored as a viewer preference",
			req.Automatic, playback.ReplanAutomaticV3)
	}
	if req.SelectedTracks.Audio == nil {
		t.Fatal("the correction was not applied")
	}
}

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

// TestReconcileAudioWithdrawalSkippedWithoutReconcileResponseCapability is the
// compatibility half of the gate: a client that advertised
// plan_invalidated_v1 but NOT default_audio_reconcile_response_v1 replays any
// withdrawal it receives as a failure_recovery — folding the withdrawn attempt
// key into attempted_plan_keys and excluding the healthy route that is
// currently playing from its own replacement. The withdrawal is therefore
// withheld from it specifically, while the decision stays recorded so the
// correction lands on its next start or reconnect.
func TestReconcileAudioWithdrawalSkippedWithoutReconcileResponseCapability(t *testing.T) {
	f := newInvalidationFixture(t, []string{
		playback.FeaturePlaybackPlanV3,
		playback.FeaturePlanInvalidatedV3,
	}, 5)

	after := f.reconcile(t)

	if got := len(f.conn.commands()); got != 0 {
		t.Fatalf("plan_invalidated pushes = %d without default_audio_reconcile_response_v1, want 0", got)
	}
	entry := playback.FindAudioReconcileEntry(after.AudioReconcileLedger, reconcileGenerationV3(f.verified), f.session.ID)
	if entry == nil {
		t.Fatal("the decision must still be recorded without the reconcile-response capability")
	}
	if entry.Decision != playback.AudioReconcileInvalidated || entry.Request == nil {
		t.Fatalf("decision = %+v, want a recorded invalidation with its canonical request", entry)
	}
	if entry.Reason != playback.PlanInvalidatedDefaultAudioReconciliation {
		t.Fatalf("entry reason = %q, want %q", entry.Reason, playback.PlanInvalidatedDefaultAudioReconciliation)
	}
	if after.CurrentPlanID != f.record.CurrentPlanID {
		t.Fatal("an unnegotiated client must not get its plan replaced server-side either")
	}
}

// TestSettledReconcileEntryRecordsTheWithdrawalReason pins the correlation
// half: the settled entry carries the exact reason string the withdrawal was
// announced with, because that is what an answering replan echoes back in
// answers_plan_invalidation and what pendingAudioReconciliationReplan matches
// against.
func TestSettledReconcileEntryRecordsTheWithdrawalReason(t *testing.T) {
	f := newInvalidationFixture(t, []string{
		playback.FeaturePlaybackPlanV3,
		playback.FeaturePlanInvalidatedV3,
		playback.FeatureDefaultAudioReconcileResponseV3,
	}, 9)

	after := f.reconcile(t)
	entry := playback.FindAudioReconcileEntry(after.AudioReconcileLedger, reconcileGenerationV3(f.verified), f.session.ID)
	if entry == nil {
		t.Fatal("expected a settled entry")
	}
	if entry.Reason != playback.PlanInvalidatedDefaultAudioReconciliation {
		t.Fatalf("entry reason = %q, want %q", entry.Reason, playback.PlanInvalidatedDefaultAudioReconciliation)
	}
	// The stored canonical request must NOT echo the reason back: it is a
	// server-built automatic replan, not the client's answer to a withdrawal,
	// and its bytes are the idempotency identity.
	if entry.Request.AnswersPlanInvalidation != "" {
		t.Fatalf("stored automatic replan answers_plan_invalidation = %q, want empty",
			entry.Request.AnswersPlanInvalidation)
	}
}

// TestReconcileAudioWithdrawalReachesOnlyTheOwnerLane pins the multi-node
// contract: the withdrawal travels the session's own hub lane, so it reaches
// the replica that owns that session's control socket and nothing else. The
// ledger, not a cross-replica RPC, is what keeps another replica from
// emitting a duplicate.
func TestReconcileAudioWithdrawalReachesOnlyTheOwnerLane(t *testing.T) {
	f := newInvalidationFixture(t, []string{playback.FeaturePlanInvalidatedV3, playback.FeatureDefaultAudioReconcileResponseV3}, 7)
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
// clientReplanForInvalidation builds the request a capability-aware web client
// actually sends in answer to the withdrawal: operation track_change, no failure
// payload, the plan's own audio identity echoed by the request builder, and the
// answers_plan_invalidation marker naming the reason it is answering.
func clientReplanForInvalidation(f *invalidationFixture, failedPlanID string) playback.ReplanRequestV3 {
	return playback.ReplanRequestV3{
		ProtocolVersion:         playback.ProtocolV3,
		Operation:               playback.ReplanOperationTrackChangeV3,
		PlaybackAttemptID:       f.record.PlaybackAttemptID,
		ReplanRequestID:         "client-replan-1",
		FailedPlanID:            failedPlanID,
		PlanAttemptID:           "client-plan-attempt-1",
		PlanAttemptKey:          f.record.CurrentPlan.PlanAttemptKey,
		AttemptCount:            2,
		PositionSeconds:         321.5,
		AnswersPlanInvalidation: playback.PlanInvalidatedDefaultAudioReconciliation,
		SelectedTracks: playback.SelectedTracksV3{
			Audio: copiedTrackIdentityForTest(f.record.CurrentPlan.SelectedTracks.Audio),
		},
	}
}

// copiedTrackIdentityForTest clones a track identity so a test asserts on the
// value the request builder produced, not on the plan's own pointer.
func copiedTrackIdentityForTest(identity *playback.TrackIdentityV3) *playback.TrackIdentityV3 {
	if identity == nil {
		return nil
	}
	copied := *identity
	if identity.Index != nil {
		index := *identity.Index
		copied.Index = &index
	}
	return &copied
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
