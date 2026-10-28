package handlers

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// The wait-start line is written once per session, names the first need, and
// classifies the gate that held the inventory: the relay deferral when no probe
// is armed, an unprobed row while a probe is enumerating, and stale evidence
// when the row's stamp does not describe the generation being served.
func TestSubtitleInventoryWaitStartLogsOnceAndClassifiesBlocker(t *testing.T) {
	logs := captureHandlerLogs(t)
	sessionMgr := playback.NewSessionManager(0, 0)
	unprobed := &models.MediaFile{ID: 71, ContentID: "movie-wait", FilePath: "virtual://movie/tt-wait"}
	session, err := sessionMgr.StartSession(1, "profile-1", unprobed.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	handler := NewPlaybackHandler(sessionMgr, testPlaybackFileResolver{file: unprobed})

	if blocker := subtitleInventoryWaitBlockerV3(session, unprobed); blocker != subtitleWaitBlockerRelay {
		t.Fatalf("unarmed probe blocker = %q, want %q", blocker, subtitleWaitBlockerRelay)
	}
	handler.noteSubtitleInventoryWaitStart(context.Background(), session.ID, unprobed, string(ProbeProvenanceDeclared), subtitleWaitBlockerRelay)
	// A poll loop or a replan re-reads the same provisional menu; it must not
	// multiply the start line.
	handler.noteSubtitleInventoryWaitStart(context.Background(), session.ID, unprobed, string(ProbeProvenanceDeclared), subtitleWaitBlockerRelay)

	logText := logs.String()
	if got := strings.Count(logText, "subtitle inventory probe wait started"); got != 1 {
		t.Fatalf("wait-start line count = %d, want 1:\n%s", got, logText)
	}
	for _, want := range []string{
		`"session":"` + session.ID + `"`,
		`"file_id":71`,
		`"blocker":"relay"`,
		`"inventory_status":"declared"`,
		`"wait_started_at":"`,
	} {
		if !strings.Contains(logText, want) {
			t.Fatalf("wait-start line omitted %s:\n%s", want, logText)
		}
	}

	if blocker := subtitleInventoryWaitBlockerV3(session, &models.MediaFile{ID: 72, ProbeUpdatedAt: timePtr(probeWaitStamp())}); blocker != subtitleWaitBlockerStale {
		t.Fatalf("stamped-row blocker = %q, want %q", blocker, subtitleWaitBlockerStale)
	}
	if err := sessionMgr.SetVirtualProbeOutcome(session.ID, probeOutcomePending); err != nil {
		t.Fatalf("SetVirtualProbeOutcome: %v", err)
	}
	if blocker := subtitleInventoryWaitBlockerV3(session, unprobed); blocker != subtitleWaitBlockerUnprobed {
		t.Fatalf("armed-probe blocker = %q, want %q", blocker, subtitleWaitBlockerUnprobed)
	}
}

// The existing evidence-delivery line is the wait's end mark: it must carry the
// start time and blocker recorded at first need, so the provisional-menu
// duration is measurable from logs alone.
func TestInventoryDeliveryLogCarriesSubtitleWaitFields(t *testing.T) {
	logs := captureHandlerLogs(t)
	stamp := probeWaitStamp()
	file := &models.MediaFile{
		ID: 81, ContentID: "movie-wait-delivery", FilePath: "virtual://movie/tt-wait-delivery",
		Container: "mkv", CodecVideo: "h264", CodecAudio: "aac", Resolution: "1080p",
		VideoTracks:    []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080}},
		AudioTracks:    []models.AudioTrack{{Codec: "aac", Channels: 2, Language: "eng"}},
		SubtitleTracks: []models.SubtitleTrack{{Codec: "subrip", Index: 1, Language: "eng"}},
		ProbeUpdatedAt: &stamp,
	}
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	handler := NewPlaybackHandler(sessionMgr, testPlaybackFileResolver{file: file})
	handler.RealtimeHub = playback.NewRealtimeHub()
	if err := sessionMgr.SetRealtimeConnection(session.ID, true); err != nil {
		t.Fatalf("SetRealtimeConnection: %v", err)
	}
	registration := handler.RealtimeHub.Register(session.ID, &sourceCommittedTestConn{})
	if registration == nil {
		t.Fatal("expected a realtime registration")
	}
	defer handler.RealtimeHub.Unregister(registration)

	handler.noteSubtitleInventoryWaitStart(context.Background(), session.ID, file, string(ProbeProvenancePending), subtitleWaitBlockerRelay)
	summary := handler.PublishInventoryUpdated(context.Background(), file.ID)
	if summary.SessionsNotified != 1 {
		t.Fatalf("sessions notified = %d, want 1", summary.SessionsNotified)
	}
	if len(summary.Waits) != 1 || summary.Waits[0].SessionID != session.ID {
		t.Fatalf("delivery summary waits = %#v, want the session's wait", summary.Waits)
	}
	handler.logInventoryDelivery(summary, 0)

	logText := logs.String()
	if !strings.Contains(logText, "virtual probe evidence inventory delivered") {
		t.Fatalf("delivery was not logged:\n%s", logText)
	}
	for _, want := range []string{
		`"waits_resolved":1`,
		`"wait_blocker":"relay"`,
		`"wait_started_at":"`,
		`"wait_ms":`,
		`"wait_session_id":"` + session.ID + `"`,
	} {
		if !strings.Contains(logText, want) {
			t.Fatalf("delivery line omitted %s:\n%s", want, logText)
		}
	}

	// The wait is consumed by its delivery; a second fan-out for the same
	// session must not report it again.
	if len(handler.PublishInventoryUpdated(context.Background(), file.ID).Waits) != 0 {
		t.Fatal("a resolved wait was reported by a later delivery")
	}
}

func probeWaitStamp() time.Time {
	return time.Now().Add(-time.Minute).UTC()
}
