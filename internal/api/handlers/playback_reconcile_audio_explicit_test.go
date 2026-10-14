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

func probeStampPtr(t *testing.T) *time.Time {
	t.Helper()
	return &invalidationProbeStamp
}
