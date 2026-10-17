package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

func scrobbleGateSettings(seconds string) testPlaybackSettingsRepo {
	return testPlaybackSettingsRepo{values: map[string]string{
		config.PlaybackScrobbleStartSecondsSettingKey: seconds,
	}}
}

// registerGateSession inserts a session whose watched baseline the test
// controls, so the minimum-watched gate can be exercised without sleeping.
func registerGateSession(mgr *playback.SessionManager, baseline time.Time, paused bool) *playback.Session {
	sess := &playback.Session{
		ID:                    "gate-session",
		UserID:                1,
		ProfileID:             "profile-1",
		MediaFileID:           42,
		StartedAt:             baseline,
		ScrobbleStartBaseline: baseline,
		IsPaused:              paused,
	}
	mgr.RegisterReconstructed(sess)
	return sess
}

func TestScrobbleProgressTransitionMinWatchedGate(t *testing.T) {
	file := &models.MediaFile{ID: 42, ContentID: "movie-1", Duration: 3600}
	now := time.Now()

	newHandler := func(t *testing.T, baseline time.Time, paused bool, seconds string) (*PlaybackHandler, *playback.SessionManager, *recordingPlaybackWatchScrobbler) {
		t.Helper()
		mgr := playback.NewSessionManager(0, 0)
		registerGateSession(mgr, baseline, paused)
		scrobbler := &recordingPlaybackWatchScrobbler{}
		handler := NewPlaybackHandler(mgr, testPlaybackFileResolver{file: file})
		handler.WatchScrobbler = scrobbler
		handler.SettingsRepo = scrobbleGateSettings(seconds)
		return handler, mgr, scrobbler
	}

	t.Run("nothing before the window", func(t *testing.T) {
		handler, mgr, scrobbler := newHandler(t, now, false, "30")
		sess, _ := mgr.GetSession("gate-session")
		handler.scrobbleProgressTransition(context.Background(), sess, false)
		if len(scrobbler.starts) != 0 || len(scrobbler.pauses) != 0 {
			t.Fatalf("before the window: starts=%d pauses=%d, want none", len(scrobbler.starts), len(scrobbler.pauses))
		}
	})

	t.Run("deferred start emitted at first tick after the window", func(t *testing.T) {
		handler, mgr, scrobbler := newHandler(t, now.Add(-31*time.Second), false, "30")
		sess, _ := mgr.GetSession("gate-session")
		handler.scrobbleProgressTransition(context.Background(), sess, false)
		if len(scrobbler.starts) != 1 {
			t.Fatalf("starts = %d, want 1", len(scrobbler.starts))
		}
		if len(scrobbler.pauses) != 0 {
			t.Fatalf("pauses = %d, want 0", len(scrobbler.pauses))
		}
		if got := scrobbler.starts[0].MediaItemID; got != file.ContentID {
			t.Fatalf("start media item = %q, want %q", got, file.ContentID)
		}
	})

	t.Run("deferred start emitted only once", func(t *testing.T) {
		handler, mgr, scrobbler := newHandler(t, now.Add(-31*time.Second), false, "30")
		sess, _ := mgr.GetSession("gate-session")
		handler.scrobbleProgressTransition(context.Background(), sess, false)
		handler.scrobbleProgressTransition(context.Background(), sess, false)
		if len(scrobbler.starts) != 1 {
			t.Fatalf("starts = %d, want exactly 1", len(scrobbler.starts))
		}
	})

	t.Run("pause before the gate emits nothing", func(t *testing.T) {
		handler, mgr, scrobbler := newHandler(t, now, true, "30")
		sess, _ := mgr.GetSession("gate-session")
		handler.scrobbleProgressTransition(context.Background(), sess, false)
		if len(scrobbler.starts) != 0 || len(scrobbler.pauses) != 0 {
			t.Fatalf("pause before the gate: starts=%d pauses=%d, want none", len(scrobbler.starts), len(scrobbler.pauses))
		}
	})

	t.Run("resume after the gate emits a start", func(t *testing.T) {
		handler, mgr, scrobbler := newHandler(t, now.Add(-31*time.Second), false, "30")
		sess, _ := mgr.GetSession("gate-session")
		handler.scrobbleProgressTransition(context.Background(), sess, false)
		scrobbler.starts = nil
		handler.scrobbleProgressTransition(context.Background(), sess, true) // paused -> playing
		if len(scrobbler.starts) != 1 {
			t.Fatalf("resume starts = %d, want 1", len(scrobbler.starts))
		}
	})

	t.Run("zero keeps immediate behavior", func(t *testing.T) {
		handler, mgr, scrobbler := newHandler(t, now, false, "0")
		sess, _ := mgr.GetSession("gate-session")
		handler.scrobbleProgressTransition(context.Background(), sess, true) // paused -> playing, as on resume
		if len(scrobbler.starts) != 1 {
			t.Fatalf("starts = %d, want 1", len(scrobbler.starts))
		}
	})
}

func TestRunPlaybackStartSideEffectsV3MinWatchedGate(t *testing.T) {
	file := &models.MediaFile{ID: 42, ContentID: "movie-1", Duration: 3600}
	now := time.Now()

	run := func(t *testing.T, baseline time.Time, seconds string) *recordingPlaybackWatchScrobbler {
		t.Helper()
		mgr := playback.NewSessionManager(0, 0)
		sess := registerGateSession(mgr, baseline, false)
		scrobbler := &recordingPlaybackWatchScrobbler{}
		handler := NewPlaybackHandler(mgr, testPlaybackFileResolver{file: file})
		handler.WatchScrobbler = scrobbler
		handler.SettingsRepo = scrobbleGateSettings(seconds)
		handler.v3StartEffectsPending = map[string]*playbackStartSideEffectsStateV3{}

		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		state := &playbackStartSideEffectsStateV3{ctx: ctx, cancel: cancel, done: make(chan struct{})}
		handler.runPlaybackStartSideEffectsV3(playbackStartSideEffectsV3{
			session:   *sess,
			file:      *file,
			userID:    1,
			profileID: "profile-1",
			state:     state,
		})
		return scrobbler
	}

	t.Run("deferred before the window", func(t *testing.T) {
		scrobbler := run(t, now, "30")
		if len(scrobbler.starts) != 0 {
			t.Fatalf("starts = %d, want 0 before the window", len(scrobbler.starts))
		}
	})

	t.Run("immediate when the gate is disabled", func(t *testing.T) {
		scrobbler := run(t, now, "0")
		if len(scrobbler.starts) != 1 {
			t.Fatalf("starts = %d, want 1", len(scrobbler.starts))
		}
	})

	t.Run("emitted when the window has already elapsed", func(t *testing.T) {
		scrobbler := run(t, now.Add(-31*time.Second), "30")
		if len(scrobbler.starts) != 1 {
			t.Fatalf("starts = %d, want 1", len(scrobbler.starts))
		}
	})
}

func TestFinalizeSessionStopSuppressedBeforeMinWatchedGate(t *testing.T) {
	file := &models.MediaFile{ID: 42, ContentID: "movie-1", Duration: 3600}

	tests := []struct {
		name          string
		baseline      time.Time
		userInitiated bool
		wantPause     int
		wantStop      int
	}{
		{name: "stop before the gate is suppressed", baseline: time.Now(), userInitiated: true},
		{name: "teardown before the gate is suppressed", baseline: time.Now()},
		{name: "stop after the window is sent", baseline: time.Now().Add(-31 * time.Second), userInitiated: true, wantStop: 1},
		{name: "teardown after the window pauses", baseline: time.Now().Add(-31 * time.Second), wantPause: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newPlaybackTestStore(t)
			mgr := playback.NewSessionManager(0, 0)
			registerGateSession(mgr, tt.baseline, false)
			scrobbler := &recordingPlaybackWatchScrobbler{}
			handler := NewPlaybackHandler(mgr, testPlaybackFileResolver{file: file})
			handler.StoreProvider = testUserStoreProvider{store: store}
			handler.WatchScrobbler = scrobbler
			handler.SettingsRepo = scrobbleGateSettings("30")
			sess, _ := mgr.GetSession("gate-session")
			sess.Position = 120

			handler.finalizeSessionStop(context.Background(), sess, false, "", tt.userInitiated)

			if got := len(scrobbler.pauses); got != tt.wantPause {
				t.Fatalf("pause calls = %d, want %d", got, tt.wantPause)
			}
			if got := len(scrobbler.stops); got != tt.wantStop {
				t.Fatalf("stop calls = %d, want %d", got, tt.wantStop)
			}
		})
	}
}
