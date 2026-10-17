package jellycompat

import (
	"context"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/playback"
)

func compatGateSettings(seconds string) stubSettingsReader {
	return stubSettingsReader{values: map[string]string{
		config.PlaybackScrobbleStartSecondsSettingKey: seconds,
	}}
}

// compatScrobbleReportFixture builds a handler whose upstream session's watched
// baseline the test controls, so the minimum-watched gate can be exercised
// without sleeping.
func compatScrobbleReportFixture(t *testing.T, baseline time.Time, seconds string) (*PlaybackHandler, *recordingCompatWatchScrobbler, *PlaybackSession, PlaybackMediaSource) {
	t.Helper()
	mgr := playback.NewSessionManager(0, 0)
	mgr.RegisterReconstructed(&playback.Session{
		ID:                    "upstream-1",
		UserID:                7,
		ProfileID:             "profile-1",
		MediaFileID:           42,
		StartedAt:             baseline,
		ScrobbleStartBaseline: baseline,
	})
	scrobbler := &recordingCompatWatchScrobbler{}
	handler := &PlaybackHandler{
		sessionMgr:     mgr,
		WatchScrobbler: scrobbler,
		SettingsRepo:   compatGateSettings(seconds),
	}
	source := PlaybackMediaSource{ID: "source-1", FileID: 42, Version: testCompatVersion()}
	playSession := &PlaybackSession{
		ID:                "play-1",
		ItemID:            "movie-1",
		UpstreamSessionID: "upstream-1",
		MediaSources:      []PlaybackMediaSource{source},
	}
	return handler, scrobbler, playSession, source
}

func TestScrobbleCompatReportMinWatchedGate(t *testing.T) {
	now := time.Now()

	t.Run("playing report before the window emits nothing", func(t *testing.T) {
		handler, scrobbler, playSession, source := compatScrobbleReportFixture(t, now, "30")
		previous := &playback.Session{ID: "upstream-1", Position: 5}
		handler.scrobbleCompatReport(context.Background(), playSession, previous, &source, 10, false)
		if len(scrobbler.calls) != 0 {
			t.Fatalf("calls = %+v, want none before the window", scrobbler.calls)
		}
	})

	t.Run("pause before the window emits nothing", func(t *testing.T) {
		handler, scrobbler, playSession, source := compatScrobbleReportFixture(t, now, "30")
		previous := &playback.Session{ID: "upstream-1", Position: 5}
		handler.scrobbleCompatReport(context.Background(), playSession, previous, &source, 10, true)
		if len(scrobbler.calls) != 0 {
			t.Fatalf("calls = %+v, want none for a pause before the window", scrobbler.calls)
		}
	})

	t.Run("deferred start at the first report after the window", func(t *testing.T) {
		handler, scrobbler, playSession, source := compatScrobbleReportFixture(t, now.Add(-31*time.Second), "30")
		previous := &playback.Session{ID: "upstream-1", Position: 5}
		handler.scrobbleCompatReport(context.Background(), playSession, previous, &source, 10, false)
		if len(scrobbler.calls) != 1 || scrobbler.calls[0].action != "start" {
			t.Fatalf("calls = %+v, want one start", scrobbler.calls)
		}
	})

	t.Run("zero keeps immediate resume behavior", func(t *testing.T) {
		handler, scrobbler, playSession, source := compatScrobbleReportFixture(t, now, "0")
		previous := &playback.Session{ID: "upstream-1", IsPaused: true, Position: 5}
		handler.scrobbleCompatReport(context.Background(), playSession, previous, &source, 10, false)
		if len(scrobbler.calls) != 1 || scrobbler.calls[0].action != "start" {
			t.Fatalf("calls = %+v, want one start", scrobbler.calls)
		}
	})
}
