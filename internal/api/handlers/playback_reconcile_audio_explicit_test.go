package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/userstore"
)

// The production shapes below are the ones that killed first-play virtual
// playback: the client (or the start-time snapshot) persisted a PARTIAL audio
// signature while the probe reported the same stream with more fields.
//
// partialExplicitSignature is the stored shape observed in
// playback_v3_attempts.audio_selection.selected_signature:
// {"codec":"aac","channels":2,"default":false} — every other member absent.
func partialExplicitSignature() *userstore.AudioTrackSignature {
	return &userstore.AudioTrackSignature{Codec: "aac", Channels: 2}
}

// TestExplicitAudioSelectionPresentPartialSignature: a partial client signature
// must match the richer probed track at the committed ordinal. Today this is
// declared missing (refused + hard terminal); it must be present.
func TestExplicitAudioSelectionPresentPartialSignature(t *testing.T) {
	tracks := []models.AudioTrack{{
		Index:         0,
		Codec:         "aac",
		Channels:      2,
		Layout:        "stereo",
		Language:      "ja",
		Title:         "[Erai-raws]_AAC_CR",
		EmbeddedTitle: "[Erai-raws]_AAC_CR",
		Default:       true,
	}}
	if !explicitAudioSelectionPresent(tracks, 0, partialExplicitSignature()) {
		t.Fatal("a partial client signature (codec+channels) must still match the richer probed track at the committed ordinal")
	}
	// The wildcard matcher must not become unconditional: a partial signature
	// that contradicts a member it DID name is still absent.
	if explicitAudioSelectionPresent(tracks, 0, &userstore.AudioTrackSignature{Codec: "eac3", Channels: 2}) {
		t.Fatal("a partial signature naming a different codec must not match the aac track")
	}
	if explicitAudioSelectionPresent(tracks, 0, &userstore.AudioTrackSignature{Codec: "aac", Channels: 6}) {
		t.Fatal("a partial signature naming a different channel count must not match the stereo track")
	}
}

// TestExplicitAudioSelectionPresentPlaceholderInventory: a provider placeholder
// row ({"index":-1,"default":true}) carries no identifying fields, so the
// inventory has nothing comparable and must never declare the selection
// missing.
func TestExplicitAudioSelectionPresentPlaceholderInventory(t *testing.T) {
	tracks := []models.AudioTrack{{Index: -1, Default: true}}
	if !explicitAudioSelectionPresent(tracks, 0, partialExplicitSignature()) {
		t.Fatal("a placeholder inventory with no identifying fields must not declare the selection missing")
	}
}

// TestExplicitAudioSelectionAbsentAcrossWholeInventory: the signature scan the
// reorder path uses is also wildcard-aware, so a present match at a different
// ordinal is still found even when the client named only some members.
func TestExplicitAudioSelectionAbsentAcrossWholeInventory(t *testing.T) {
	tracks := []models.AudioTrack{
		{Index: 0, Language: "en", Codec: "eac3", Channels: 6, Layout: "5.1"},
		{Index: 1, Language: "ja", Codec: "aac", Channels: 2, Layout: "stereo", Title: "[Erai-raws]_AAC_CR"},
	}
	// Committed ordinal 0 is the eac3 track, but the client's partial aac
	// signature survives at ordinal 1 after a reorder: still present.
	if !explicitAudioSelectionPresent(tracks, 0, partialExplicitSignature()) {
		t.Fatal("a reordered but present track must be found by the whole-inventory scan")
	}
	// Genuine absence: nothing in the inventory carries aac + 2 channels.
	eac3Only := []models.AudioTrack{{Index: 0, Language: "en", Codec: "eac3", Channels: 2, Layout: "stereo"}}
	if explicitAudioSelectionPresent(eac3Only, 0, partialExplicitSignature()) {
		t.Fatal("an eac3-only inventory must not match an aac selection")
	}
}

// explicitReconcileFixture wires a handler for the explicit-selection verify
// path: a live session bound to a virtual row whose probe reported the richer
// inventory, and an attempt record whose committed explicit signature is
// partial. Route events are captured synchronously so the test can prove the
// path emits nothing.
type explicitReconcileFixture struct {
	handler  *PlaybackHandler
	session  *playback.Session
	record   *playback.AttemptRecordV3
	verified *models.MediaFile
	intent   audioSelectionIntent
	events   chan playback.RouteEventRecordV3
}

func newExplicitReconcileFixture(t *testing.T, verified *models.MediaFile) *explicitReconcileFixture {
	t.Helper()
	const fileID = 7
	if verified == nil {
		verified = &models.MediaFile{ID: fileID}
	}
	if verified.ID == 0 {
		verified.ID = fileID
	}
	if verified.FilePath == "" {
		verified.FilePath = "virtual://episode/partial-signature"
	}
	selected := partialExplicitSignature()
	session := &playback.Session{
		ID:                            "22222222-2222-2222-2222-222222222222",
		UserID:                        1,
		ProfileID:                     "profile-1",
		MediaFileID:                   verified.ID,
		RequestedMediaFileID:          verified.ID,
		PlayMethod:                    playback.PlayDirect,
		AudioTrackIndex:               0,
		SelectionOrigin:               SelectionOriginExplicit,
		SelectedAudioSignature:        selected,
		VirtualAudioTracks:            []models.AudioTrack{{Index: 0, Codec: "aac", Channels: 2}},
		VirtualSourceURI:              verified.FilePath,
		VirtualSubtitleEvidenceURI:    verified.FilePath,
		VirtualSubtitleEvidenceFileID: verified.ID,
	}
	record := reconcileIntent("", nil, selected)
	record.SessionID = session.ID
	record.SelectionOrigin = SelectionOriginExplicit
	record.RequestedMediaFileID = verified.ID
	record.EffectiveMediaFileID = verified.ID
	record.CurrentPlan.SelectedTracks.Audio = &playback.TrackIdentityV3{
		ID:    playback.TrackIDV3(verified.ID, "audio", 0),
		Index: intPtrReconcile(0),
	}

	handler := reconcileFixture(t, session, record, verified)
	// Capture route events synchronously: marking the queue's Once done keeps
	// enqueueRouteEventV3 from starting its worker goroutine, and a buffered
	// channel lets the test observe exactly what (if anything) it enqueued.
	handler.v3EventOnce.Do(func() {})
	events := make(chan playback.RouteEventRecordV3, 16)
	handler.v3EventQueue = events

	return &explicitReconcileFixture{
		handler:  handler,
		session:  session,
		record:   record,
		verified: verified,
		intent:   audioSelectionIntent{origin: SelectionOriginExplicit, selectedOverride: selected},
		events:   events,
	}
}

// drainRouteEvents reports every route event the path enqueued.
func (f *explicitReconcileFixture) drainRouteEvents() []playback.RouteEventRecordV3 {
	var out []playback.RouteEventRecordV3
	for {
		select {
		case event := <-f.events:
			out = append(out, event)
		default:
			return out
		}
	}
}

func (f *explicitReconcileFixture) verify(t *testing.T) *playback.AttemptRecordV3 {
	t.Helper()
	f.handler.verifyExplicitAudioSelectionForInventory(
		context.Background(), f.session, f.intent, f.verified.ID, f.verified)
	record, err := f.handler.PlanStoreV3.GetAttempt(context.Background(), f.session.ID)
	if err != nil {
		t.Fatalf("get attempt after verify: %v", err)
	}
	return record
}

// TestVerifyExplicitAudioSelectionPartialSignatureSurvivesProbe is the
// end-to-end repro of the production failure: a partial explicit signature
// against the richer verified inventory must settle as a no-op, with no refusal
// and no terminal route event.
func TestVerifyExplicitAudioSelectionPartialSignatureSurvivesProbe(t *testing.T) {
	verified := &models.MediaFile{
		ID:       7,
		FilePath: "virtual://episode/partial-signature",
		AudioTracks: []models.AudioTrack{{
			Index:         0,
			Language:      "ja",
			Codec:         "aac",
			Channels:      2,
			Layout:        "stereo",
			Title:         "[Erai-raws]_AAC_CR",
			EmbeddedTitle: "[Erai-raws]_AAC_CR",
			Default:       true,
		}},
		ProbeUpdatedAt: probeStampPtr(t),
	}
	f := newExplicitReconcileFixture(t, verified)
	after := f.verify(t)

	entry := playback.FindAudioReconcileEntry(after.AudioReconcileLedger, reconcileGenerationV3(verified), f.session.ID)
	if entry == nil {
		t.Fatal("the verify path must settle the generation")
	}
	if entry.Decision != playback.AudioReconcileNoop {
		t.Fatalf("decision = %q, want %q: a partial signature matching the probed track is present",
			entry.Decision, playback.AudioReconcileNoop)
	}
	for _, event := range f.drainRouteEvents() {
		if event.Event == playback.RouteEventTerminalV3 {
			t.Fatalf("a present explicit selection must not emit a terminal route event: %+v", event)
		}
	}
}

// TestVerifyExplicitAudioSelectionPlaceholderInventorySurvivesProbe: the probe
// produced only a placeholder row, so the check has nothing comparable and must
// not refuse or terminate.
func TestVerifyExplicitAudioSelectionPlaceholderInventorySurvivesProbe(t *testing.T) {
	verified := &models.MediaFile{
		ID:             7,
		FilePath:       "virtual://episode/partial-signature",
		AudioTracks:    []models.AudioTrack{{Index: -1, Default: true}},
		ProbeUpdatedAt: probeStampPtr(t),
	}
	f := newExplicitReconcileFixture(t, verified)
	after := f.verify(t)

	entry := playback.FindAudioReconcileEntry(after.AudioReconcileLedger, reconcileGenerationV3(verified), f.session.ID)
	if entry == nil {
		t.Fatal("the verify path must settle the generation")
	}
	if entry.Decision == playback.AudioReconcileRefused {
		t.Fatal("a placeholder inventory has nothing comparable and must not be recorded as refused")
	}
	for _, event := range f.drainRouteEvents() {
		if event.Event == playback.RouteEventTerminalV3 {
			t.Fatalf("a placeholder inventory must not emit a terminal route event: %+v", event)
		}
	}
}

// TestVerifyExplicitAudioSelectionGenuineAbsenceDoesNotTerminate: the selection
// really is gone (aac vs an eac3-only inventory). The refusal stays recorded for
// observability, but playback must survive — no terminal route event may be
// emitted over this check.
func TestVerifyExplicitAudioSelectionGenuineAbsenceDoesNotTerminate(t *testing.T) {
	verified := &models.MediaFile{
		ID:       7,
		FilePath: "virtual://episode/partial-signature",
		AudioTracks: []models.AudioTrack{{
			Index: 0, Language: "en", Codec: "eac3", Channels: 2, Layout: "stereo",
		}},
		ProbeUpdatedAt: probeStampPtr(t),
	}
	f := newExplicitReconcileFixture(t, verified)
	after := f.verify(t)

	entry := playback.FindAudioReconcileEntry(after.AudioReconcileLedger, reconcileGenerationV3(verified), f.session.ID)
	if entry == nil {
		t.Fatal("the verify path must settle the generation")
	}
	if entry.Decision != playback.AudioReconcileRefused {
		t.Fatalf("decision = %q, want %q: a genuinely absent explicit selection is still refused for observability",
			entry.Decision, playback.AudioReconcileRefused)
	}
	for _, event := range f.drainRouteEvents() {
		if event.Event == playback.RouteEventTerminalV3 {
			t.Fatalf("a genuine absence must not hard-terminate a live stream: %+v", event)
		}
	}
}

// TestVerifyExplicitAudioSelectionGenuineAbsenceStaysRecoverable carries the
// genuinely-absent case one step further than the termination check: the
// refusal is durable for observability, the session survives with its committed
// selection intact, and when the verified overlay replaces the carried
// inventory it cannot re-identify the vanished stream and fails closed. That
// fail-closed marker is not a dead end: the client's own recovery replan commits
// a fresh selection and clears it, so playback is re-resolved rather than left
// stalled or unhandled.
func TestVerifyExplicitAudioSelectionGenuineAbsenceStaysRecoverable(t *testing.T) {
	verified := &models.MediaFile{
		ID:       7,
		FilePath: "virtual://episode/partial-signature",
		AudioTracks: []models.AudioTrack{{
			Index: 0, Language: "en", Codec: "eac3", Channels: 2, Layout: "stereo",
		}},
		ProbeUpdatedAt: probeStampPtr(t),
	}
	f := newExplicitReconcileFixture(t, verified)
	after := f.verify(t)

	entry := playback.FindAudioReconcileEntry(after.AudioReconcileLedger, reconcileGenerationV3(verified), f.session.ID)
	if entry == nil || entry.Decision != playback.AudioReconcileRefused {
		t.Fatalf("a genuinely absent explicit selection must record a durable refusal, got %+v", entry)
	}
	for _, event := range f.drainRouteEvents() {
		if event.Event == playback.RouteEventTerminalV3 {
			t.Fatalf("a genuine absence must not hard-terminate a live stream: %+v", event)
		}
	}

	// The refusal path is observability-only: the session still exists and its
	// committed selection keeps serving, not stranded at a dead index.
	manager, ok := f.handler.sessionMgr.(*playback.SessionManager)
	if !ok {
		t.Fatal("handler session manager is not a *playback.SessionManager")
	}
	live := reconcileLiveSession(t, f.handler, f.session.ID)
	if live.AudioTrackIndex != 0 || live.VirtualAudioSelectionInvalid {
		t.Fatalf("the refusal path must leave the committed selection serving: index=%d invalid=%v",
			live.AudioTrackIndex, live.VirtualAudioSelectionInvalid)
	}

	// Applying the verified overlay cannot re-identify the vanished selection,
	// so it fails closed (index -1, invalid marker set).
	binding, err := manager.VirtualSourceBinding(live.ID)
	if err != nil {
		t.Fatalf("virtual source binding: %v", err)
	}
	applied, err := manager.SetVirtualProbeVerifiedInventoryIfBinding(
		live.ID, binding, "rev-verified-absence", verified.AudioTracks, nil, nil)
	if err != nil || !applied {
		t.Fatalf("verified overlay write: applied=%v err=%v", applied, err)
	}
	live = reconcileLiveSession(t, f.handler, f.session.ID)
	if live.AudioTrackIndex != -1 || !live.VirtualAudioSelectionInvalid {
		t.Fatalf("an unreidentifiable verified overlay must fail closed: index=%d invalid=%v",
			live.AudioTrackIndex, live.VirtualAudioSelectionInvalid)
	}

	// The client's own recovery replan commits a fresh selection and clears the
	// marker: that is what proves the session is recoverable, not stuck.
	if err := manager.UpdateAudioTrack(live.ID, 0, playback.PlayDirect); err != nil {
		t.Fatalf("recovery replan update: %v", err)
	}
	live = reconcileLiveSession(t, f.handler, f.session.ID)
	if live.AudioTrackIndex != 0 || live.VirtualAudioSelectionInvalid {
		t.Fatalf("client recovery must clear the fail-closed marker: index=%d invalid=%v",
			live.AudioTrackIndex, live.VirtualAudioSelectionInvalid)
	}
}

// TestReconcileVerifiedDefaultAudioFromCommittedEmptyInventoryRefusesExplicit
// pins the verified-empty-inventory case: a durable probe write that committed
// a VERIFIED inventory with zero audio tracks is ground truth that the file
// carries no audio, so the committed explicit selection cannot be present. The
// reconcile sweep must still evaluate the session and record the refusal (plus
// a Warn) for observability, without emitting a terminal route event, and the
// verified-empty overlay application must remain the fail-closed half of the
// chain that re-resolves the stream.
func TestReconcileVerifiedDefaultAudioFromCommittedEmptyInventoryRefusesExplicit(t *testing.T) {
	verified := &models.MediaFile{
		ID:             7,
		FilePath:       "virtual://episode/partial-signature",
		AudioTracks:    []models.AudioTrack{{Index: 0, Language: "ja", Codec: "aac", Channels: 2, Layout: "stereo"}},
		ProbeUpdatedAt: probeStampPtr(t),
	}
	f := newExplicitReconcileFixture(t, verified)
	// The committed carrier carries no audio: the probe enumerated zero tracks.
	committed := virtualCommittedProbeInventory{
		fileID:       verified.ID,
		candidateURI: verified.FilePath,
	}
	f.handler.reconcileVerifiedDefaultAudioFromCommitted(context.Background(), committed)

	after, err := f.handler.PlanStoreV3.GetAttempt(context.Background(), f.session.ID)
	if err != nil {
		t.Fatalf("get attempt after reconcile: %v", err)
	}
	// The synthetic verified row carries no probe stamp, so the generation is the
	// empty-inventory track fingerprint — the same key this helper derives for a
	// row with no probe stamp and no audio tracks.
	generation := reconcileGenerationV3(&models.MediaFile{})
	entry := playback.FindAudioReconcileEntry(after.AudioReconcileLedger, generation, f.session.ID)
	if entry == nil || entry.Decision != playback.AudioReconcileRefused {
		t.Fatalf("a verified-empty inventory must record the explicit refusal, got %+v", entry)
	}
	for _, event := range f.drainRouteEvents() {
		if event.Event == playback.RouteEventTerminalV3 {
			t.Fatalf("a verified-empty inventory must not hard-terminate the stream: %+v", event)
		}
	}

	// The refusal is observability-only; the overlay fail-closed chain still
	// applies. Applying the verified-empty overlay cannot re-identify the
	// committed selection, so the session is marked fail-closed and the client's
	// own recovery replan re-resolves rather than the transport drifting onto
	// another track.
	manager, ok := f.handler.sessionMgr.(*playback.SessionManager)
	if !ok {
		t.Fatal("handler session manager is not a *playback.SessionManager")
	}
	live := reconcileLiveSession(t, f.handler, f.session.ID)
	binding, err := manager.VirtualSourceBinding(live.ID)
	if err != nil {
		t.Fatalf("virtual source binding: %v", err)
	}
	applied, err := manager.SetVirtualProbeVerifiedInventoryIfBinding(live.ID, binding, "rev-verified-empty", nil, nil, nil)
	if err != nil || !applied {
		t.Fatalf("verified-empty overlay write: applied=%v err=%v", applied, err)
	}
	live = reconcileLiveSession(t, f.handler, f.session.ID)
	if live.AudioTrackIndex != -1 || !live.VirtualAudioSelectionInvalid {
		t.Fatalf("a verified-empty overlay must fail closed: index=%d invalid=%v",
			live.AudioTrackIndex, live.VirtualAudioSelectionInvalid)
	}
}

func probeStampPtr(t *testing.T) *time.Time {
	t.Helper()
	return &invalidationProbeStamp
}

// TestReconcileFromCommittedIgnoresUncommittedInventory pins the boundary the
// verified-empty refusal depends on: only a durable evidence write that actually
// committed a verified inventory may reconcile. A probe that failed or returned
// an incomplete enumeration commits nothing (its carrier has fileID 0), so it can
// never land as a verified-empty inventory and must record no reconcile decision.
func TestReconcileFromCommittedIgnoresUncommittedInventory(t *testing.T) {
	verified := &models.MediaFile{
		ID:             7,
		FilePath:       "virtual://episode/partial-signature",
		AudioTracks:    []models.AudioTrack{{Index: 0, Codec: "aac", Channels: 2}},
		ProbeUpdatedAt: probeStampPtr(t),
	}
	f := newExplicitReconcileFixture(t, verified)
	// A failed/partial probe produces no committed carrier (fileID 0).
	f.handler.reconcileVerifiedDefaultAudioFromCommitted(context.Background(), virtualCommittedProbeInventory{})

	after, err := f.handler.PlanStoreV3.GetAttempt(context.Background(), f.session.ID)
	if err != nil {
		t.Fatalf("get attempt: %v", err)
	}
	if len(after.AudioReconcileLedger.Entries) != 0 {
		t.Fatalf("an uncommitted (failed/partial) probe must record no reconcile decision, got %+v", after.AudioReconcileLedger.Entries)
	}
}

// TestReconcileEmptyInventoryRefusalDoesNotSuppressPopulatedGeneration carries
// the verified-empty case through recovery: an empty verified inventory records a
// durable refusal under its own generation, and a later populated probe is a new
// generation that is re-evaluated (the selection is now present, so a no-op). The
// earlier refusal stays in the ledger and suppresses nothing.
func TestReconcileEmptyInventoryRefusalDoesNotSuppressPopulatedGeneration(t *testing.T) {
	verified := &models.MediaFile{
		ID:       7,
		FilePath: "virtual://episode/partial-signature",
		AudioTracks: []models.AudioTrack{{
			Index: 0, Language: "ja", Codec: "aac", Channels: 2, Layout: "stereo",
			Title: "[Erai-raws]_AAC_CR", EmbeddedTitle: "[Erai-raws]_AAC_CR", Default: true,
		}},
		ProbeUpdatedAt: probeStampPtr(t),
	}
	f := newExplicitReconcileFixture(t, verified)

	// A durable write commits a verified inventory with zero audio tracks: the
	// committed selection cannot be present, so the refusal is recorded.
	committed := virtualCommittedProbeInventory{fileID: verified.ID, candidateURI: verified.FilePath}
	f.handler.reconcileVerifiedDefaultAudioFromCommitted(context.Background(), committed)

	emptyGeneration := reconcileGenerationV3(&models.MediaFile{})
	afterEmpty, err := f.handler.PlanStoreV3.GetAttempt(context.Background(), f.session.ID)
	if err != nil {
		t.Fatalf("get attempt after empty reconcile: %v", err)
	}
	if entry := playback.FindAudioReconcileEntry(afterEmpty.AudioReconcileLedger, emptyGeneration, f.session.ID); entry == nil || entry.Decision != playback.AudioReconcileRefused {
		t.Fatalf("a verified-empty inventory must record the explicit refusal, got %+v", entry)
	}

	// A later probe populates the same row. Its probe stamp is a new generation,
	// so the earlier refusal must not suppress it: the selection is present and
	// the generation settles as a no-op.
	populatedGeneration := reconcileGenerationV3(verified)
	if populatedGeneration == emptyGeneration {
		t.Fatal("a populated probe generation must differ from the empty-inventory generation")
	}
	afterPopulated := f.verify(t)
	if entry := playback.FindAudioReconcileEntry(afterPopulated.AudioReconcileLedger, populatedGeneration, f.session.ID); entry == nil || entry.Decision != playback.AudioReconcileNoop {
		t.Fatalf("the populated generation must be re-evaluated to a present no-op, got %+v", entry)
	}
	if entry := playback.FindAudioReconcileEntry(afterPopulated.AudioReconcileLedger, emptyGeneration, f.session.ID); entry == nil {
		t.Fatal("the earlier empty-inventory refusal must remain in the ledger, not be overwritten")
	}
}
