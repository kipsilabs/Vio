package handlers

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// A cold-start track change on a session whose catalog row has not been probed
// yet must reuse the verified inventory the running plan already validated on
// the same bytes. The provider churn replaces the effective row with a fresh
// listing row that carries no bit depth, frame rate, or bitrate; the session
// still carries the verified audio evidence and the running plan carries the
// probed video descriptor. Before the fix the replan planned against the
// unprobed row and terminaled source_metadata_incomplete, so the menu click
// appeared inert.
func TestReplanTrackChangeCarriesValidatedVideoEvidenceForUnprobedRow(t *testing.T) {
	fx := newPinnedTrackChangeFixture(t, "churned")
	// The session keeps its verified evidence; only the catalog row is a fresh,
	// never-probed listing row.
	fx.churnCatalogRowToStub("churned")

	response := fx.trackChange(t, 1, "pinned-coldstart-inventory-0001")
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
	if fx.listCalls != 0 || fx.detailCalls != 0 || fx.probeCalls != 0 {
		t.Fatalf("track change re-listed/resolved/probed the provider: list=%d detail=%d probe=%d", fx.listCalls, fx.detailCalls, fx.probeCalls)
	}
}

// flakyIntentPlanStoreV3 blanks the first attempt read's selection intent, then
// serves the real record. It models the probe-commit/attempt-visibility race: a
// reconcile evaluation that reads once sees no intent and skips a generation
// that is about to become reconcilable.
type flakyIntentPlanStoreV3 struct {
	playback.PlanStoreV3
	blankFirst      bool
	getAttemptCalls int32
}

func (s *flakyIntentPlanStoreV3) GetAttempt(ctx context.Context, sessionID string) (*playback.AttemptRecordV3, error) {
	call := atomic.AddInt32(&s.getAttemptCalls, 1)
	record, err := s.PlanStoreV3.GetAttempt(ctx, sessionID)
	if err != nil || record == nil {
		return record, err
	}
	if s.blankFirst && call == 1 {
		blanked := *record
		blanked.SelectionOrigin = ""
		return &blanked, nil
	}
	return record, nil
}

// A reconciliation evaluation that loses the intent read to a racing probe
// commit must retry the attempt once before falling back to the live session.
// A single read that sees no intent skips a reconcilable generation and leaves
// the declared or wrong language pinned.
func TestAudioSelectionIntentRetriesRacingAttemptRead(t *testing.T) {
	verified := &models.MediaFile{ID: 7, FilePath: "virtual://movie/tt-race", AudioTracks: []models.AudioTrack{
		{Index: 1, Language: "en", Codec: "aac", Channels: 2},
		{Index: 3, Language: "pt", Codec: "aac", Channels: 2},
	}}
	// The live session mirror carries no intent either: the durable attempt row
	// is the only place the race can be recovered from.
	session := &playback.Session{
		ID: "11111111-1111-1111-1111-111111111111", UserID: 1, ProfileID: "profile-1",
		MediaFileID: 7, RequestedMediaFileID: 7, PlayMethod: playback.PlayDirect,
		AudioTrackIndex: 0,
		VirtualAudioTracks: []models.AudioTrack{
			{Index: 1, Language: "en", Codec: "aac", Channels: 2},
			{Index: 3, Language: "pt", Codec: "aac", Channels: 2},
		},
		VirtualSourceURI:              "virtual://movie/tt-race",
		VirtualSubtitleEvidenceURI:    "virtual://movie/tt-race",
		VirtualSubtitleEvidenceFileID: 7,
		VirtualSubtitleEvidenceSet:    true,
	}
	record := reconcileIntent("pt", nil, playback.AudioTrackSignatureFromTrack(session.VirtualAudioTracks[0]))
	record.CurrentPlan.SelectedTracks.Audio = &playback.TrackIdentityV3{ID: playback.TrackIDV3(7, "audio", 0), Index: intPtrReconcile(0)}
	verified.FilePath = "virtual://movie/tt-race"

	handler := reconcileFixture(t, session, record, verified)
	flaky := &flakyIntentPlanStoreV3{PlanStoreV3: handler.PlanStoreV3, blankFirst: true}
	handler.PlanStoreV3 = flaky
	live := reconcileLiveSession(t, handler, session.ID)

	intent := handler.audioSelectionIntentForSession(context.Background(), live)
	if intent.origin != SelectionOriginAuto {
		t.Fatalf("intent origin = %q, want %q after retrying the racing attempt read", intent.origin, SelectionOriginAuto)
	}
	if calls := atomic.LoadInt32(&flaky.getAttemptCalls); calls < 2 {
		t.Fatalf("attempt read was not retried: calls = %d, want >= 2", calls)
	}
}

// The fail-closed placeholder an empty inventory is served as must be
// observable: when no catalog row names the bound candidate, the placeholder
// decision is logged with the held row id, the bound candidate URI, and the row
// id the placeholder was built from, so an operator can correlate an empty
// menu with the reason it is empty.
func TestInventoryEffectiveFileLogsFailClosedPlaceholder(t *testing.T) {
	logs := captureHandlerLogs(t)
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	file := &models.MediaFile{ID: 900, FilePath: "virtual://movie/tt-placeholder?result=other", Container: "virtual"}

	got := handler.inventoryEffectiveFile(context.Background(), file, &playback.Session{}, nil, "virtual://movie/tt-placeholder?result=bound")
	if got == nil {
		t.Fatal("inventoryEffectiveFile returned nil")
	}
	if len(got.AudioTracks) != 0 || len(got.SubtitleTracks) != 0 {
		t.Fatalf("placeholder served tracks: audio=%+v subtitles=%+v", got.AudioTracks, got.SubtitleTracks)
	}
	out := logs.String()
	if !strings.Contains(out, "virtual inventory serving fail-closed placeholder") {
		t.Fatalf("placeholder decision was not logged:\n%s", out)
	}
	for _, want := range []string{`"file_id":900`, `"base_id":900`, `"candidate_uri":"virtual://movie/tt-placeholder?result=bound"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("placeholder log missing %s:\n%s", want, out)
		}
	}
}
