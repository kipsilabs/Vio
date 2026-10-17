package handlers

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// flakyIntentPlanStoreV3 reports the durable attempt as absent on the first
// read, then serves the real record. It models the probe-commit/attempt-
// visibility race: a reconcile evaluation that reads once sees no row and skips
// a generation that is about to become reconcilable.
type flakyIntentPlanStoreV3 struct {
	playback.PlanStoreV3
	absentFirst     bool
	getAttemptCalls int32
}

func (s *flakyIntentPlanStoreV3) GetAttempt(ctx context.Context, sessionID string) (*playback.AttemptRecordV3, error) {
	call := atomic.AddInt32(&s.getAttemptCalls, 1)
	if s.absentFirst && call == 1 {
		// The probe commit has not yet made the row visible: the attempt reads
		// as absent, not as present with an empty intent.
		return nil, playback.ErrSessionNotFound
	}
	return s.PlanStoreV3.GetAttempt(ctx, sessionID)
}

// A reconciliation evaluation that loses the intent read to a racing probe
// commit must retry the attempt once before falling back to the live session.
// A single read that sees no row skips a reconcilable generation and leaves the
// declared or wrong language pinned.
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
	flaky := &flakyIntentPlanStoreV3{PlanStoreV3: handler.PlanStoreV3, absentFirst: true}
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

// intentCountingPlanStoreV3 counts durable-intent reads and can serve a fixed
// record or a persistent error.
type intentCountingPlanStoreV3 struct {
	playback.PlanStoreV3
	record          *playback.AttemptRecordV3
	err             error
	getAttemptCalls int32
}

func (s *intentCountingPlanStoreV3) GetAttempt(_ context.Context, _ string) (*playback.AttemptRecordV3, error) {
	atomic.AddInt32(&s.getAttemptCalls, 1)
	return s.record, s.err
}

// A durable record that is readable but declares no intent is definitive: a
// legacy no-intent row must not be taxed with a retry and its delay on every
// reconcile. Before the fix the retry loop keyed on an empty origin, so this
// shape slept and read twice.
func TestAudioSelectionIntentPresentWithoutIntentReadsOnce(t *testing.T) {
	session := &playback.Session{ID: "11111111-1111-1111-1111-111111111111", UserID: 1, ProfileID: "profile-1", MediaFileID: 7}
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	store := &intentCountingPlanStoreV3{
		PlanStoreV3: handler.PlanStoreV3,
		record:      &playback.AttemptRecordV3{PlaybackAttemptID: "attempt-empty-intent", SessionID: session.ID},
	}
	handler.PlanStoreV3 = store

	intent := handler.audioSelectionIntentForSession(context.Background(), session)
	if calls := atomic.LoadInt32(&store.getAttemptCalls); calls != 1 {
		t.Fatalf("present-without-intent read attempted %d times, want 1", calls)
	}
	if intent.origin != "" {
		t.Fatalf("intent origin = %q, want empty for a no-intent record", intent.origin)
	}
}

// A persistent durable read error is bounded to the retry budget and then falls
// back to the live session mirror, so a stored intent still reconciles when the
// attempt store is unavailable.
func TestAudioSelectionIntentPersistentErrorBoundedRetryThenMirrorFallback(t *testing.T) {
	session := &playback.Session{
		ID: "11111111-1111-1111-1111-111111111111", UserID: 1, ProfileID: "profile-1", MediaFileID: 7,
		SelectionOrigin:        SelectionOriginAuto,
		PreferredAudioLanguage: "pt",
	}
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	store := &intentCountingPlanStoreV3{PlanStoreV3: handler.PlanStoreV3, err: errors.New("attempt store unavailable")}
	handler.PlanStoreV3 = store

	intent := handler.audioSelectionIntentForSession(context.Background(), session)
	if calls := atomic.LoadInt32(&store.getAttemptCalls); calls != audioSelectionIntentReadAttempts {
		t.Fatalf("persistent error attempted %d reads, want the bounded %d", calls, audioSelectionIntentReadAttempts)
	}
	if intent.origin != SelectionOriginAuto || intent.preferredLang != "pt" {
		t.Fatalf("intent = %+v, want the live session mirror fallback", intent)
	}
}

// A nil context carries no reconcile deadline to bound the retry wait, so the
// retry must not run at all.
func TestWaitReconcileIntentRetryNilContextRefuses(t *testing.T) {
	// Route nil through a variable: the subject under test permits a nil
	// context, but a literal nil at the call site reads as a mistake.
	var noContext context.Context
	if waitReconcileIntentRetry(noContext) {
		t.Fatal("waitReconcileIntentRetry(nil) allowed a retry without a bound")
	}
}
