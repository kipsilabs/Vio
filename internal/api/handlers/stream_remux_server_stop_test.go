package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/remotestream"
)

// A server-initiated stop on a virtual progressive remux is not a client
// cancellation, and before the first media byte the abort lands in the
// no-output path. The virtual failure branch must not read that as a bad
// provider release: StopSession, an admin kill, and an idle reap all end the
// response this way, and blaming the candidate would retire a working release
// because of a stop the operator asked for.
func TestHandleStream_VirtualRemuxServerStopDoesNotBlameCandidate(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(source, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	// ffmpeg that never produces output and only exits on a signal, so the
	// abort is the sole way out and the handler returns before any byte.
	ffmpeg := filepath.Join(dir, "ffmpeg")
	script := "#!/bin/sh\n" +
		"for arg in \"$@\"; do\n" +
		"  if [ \"$arg\" = \"pipe:1\" ]; then while :; do sleep 0.05; done; fi\n" +
		"done\n" +
		"exit 0\n"
	if err := os.WriteFile(ffmpeg, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	// The provider served fine. A plain local relay hands ffmpeg real bytes so
	// the remux actually starts, and the trust window keeps the handler on the
	// stored URL instead of re-resolving.
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("video"))
	}))
	defer relay.Close()

	pinned := "virtual://movie/tt-server-stop?result=cand-a"
	delivered := time.Now().Add(-time.Minute)
	file := &models.MediaFile{
		ID:                         4242,
		ContentID:                  "movie-server-stop",
		FilePath:                   pinned,
		CodecVideo:                 "h264",
		VideoTracks:                []models.VideoTrack{{Codec: "h264"}},
		VirtualOwnerInstallationID: 5,
		ResolvedURL:                relay.URL + "/stream?token=stored",
		ResolvedURLExpiresAt:       new(time.Now().Add(2 * time.Hour)),
		LastDeliveredAt:            &delivered,
		UpdatedAt:                  time.Now(),
	}

	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", file.ID, playback.PlayRemux, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := sessionMgr.SetVirtualSource(session.ID, pinned, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	t.Cleanup(func() { _ = sessionMgr.StopSession(session.ID) })

	blamed := make(chan string, 4)
	handler := NewStreamHandler(sessionMgr, testPlaybackFileResolver{file: file})
	handler.RemoteStreamRelay = remotestream.NewRelay()
	t.Cleanup(func() { _ = handler.RemoteStreamRelay.Close(context.Background()) })
	handler.AllowPrivateStreams = func(int) bool { return true }
	handler.VirtualCandidateTrustWindow = func() time.Duration { return 720 * time.Hour }
	handler.PlaybackConfig = func() config.PlaybackConfig {
		return config.PlaybackConfig{FFmpegPath: ffmpeg}
	}
	handler.VirtualCandidateFailMarker = func(_ context.Context, _ int, _ string, _ *time.Time) error {
		blamed <- "marked"
		return nil
	}
	// A detailed resolver that confirms the pinned candidate, so the stored-URL
	// shortcut is not the only thing keeping the remux alive.
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(
		func(_ context.Context, virtualURI string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			return ResolvedVirtualMedia{
				URL:         relay.URL + "/stream?token=resolved",
				URI:         virtualURI,
				CandidateID: "cand-a",
			}, nil
		})
	// The rotation path must also stay closed: a stop is not provider
	// unavailability, so a re-resolve must not be attempted for it.
	refreshed := make(chan struct{}, 1)
	handler.VirtualMediaRefreshResolver = VirtualMediaRefreshResolverFunc(
		func(context.Context, string, int, int, string) (string, error) {
			refreshed <- struct{}{}
			return "", errors.New("provider unavailable")
		})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID, nil)
	req = req.WithContext(newAuthorizedPlaybackContext())
	req = withPlaybackRouteParam(req, "session_id", session.ID)

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.HandleStream(rec, req)
	}()

	// Stop the session server-side while the remux is still producing nothing.
	time.Sleep(300 * time.Millisecond)
	if err := sessionMgr.StopSession(session.ID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the remux response did not end when the session was stopped")
	}

	select {
	case how := <-blamed:
		t.Fatalf("a server-initiated stop blamed the virtual candidate (%s)", how)
	default:
	}
	select {
	case <-refreshed:
		t.Fatal("a server-initiated stop drove a virtual re-resolve as if the provider failed")
	default:
	}
}
