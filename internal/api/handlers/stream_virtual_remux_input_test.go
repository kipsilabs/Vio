package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/remotestream"
)

// A virtual row's FilePath is the session's virtual:// identity, not something
// FFmpeg can open. A progressive remux must be handed the resolved relay URL,
// as direct play is; feeding FFmpeg the identity made it exit before its first
// media byte, which reached the viewer as "remux produced no output" and a 502
// (production session a88e6d0c-2edd-463b-a052-e19c41bf6ebc, tt11378946).
func TestHandleStream_VirtualRemuxServesResolvedInput(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(source, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}

	recordedInput := filepath.Join(dir, "input.txt")
	ffmpeg := filepath.Join(dir, "ffmpeg")
	script := "#!/bin/sh\n" +
		"for arg in \"$@\"; do\n" +
		"  if [ \"$arg\" = \"-bsfs\" ]; then printf 'dovi_rpu\\nfilter_units\\n'; exit 0; fi\n" +
		"done\n" +
		"prev=\"\"\n" +
		"for arg in \"$@\"; do\n" +
		"  if [ \"$prev\" = \"-i\" ]; then printf '%s' \"$arg\" > \"" + recordedInput + "\"; fi\n" +
		"  prev=\"$arg\"\n" +
		"done\n" +
		"printf 'media'\n" +
		"exit 0\n"
	if err := os.WriteFile(ffmpeg, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}

	const resolvedURL = "http://relay.test/source/token/stream"
	pinned := "virtual://movie/tt11378946?result=cand-a"
	file := &models.MediaFile{
		ID:                         4247,
		ContentID:                  "movie-tt11378946",
		FilePath:                   pinned,
		CodecVideo:                 "hevc",
		VideoTracks:                []models.VideoTrack{{Codec: "hevc"}},
		VirtualOwnerInstallationID: 5,
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

	handler := NewStreamHandler(sessionMgr, testPlaybackFileResolver{file: file})
	handler.RemoteStreamRelay = remotestream.NewRelay()
	t.Cleanup(func() { _ = handler.RemoteStreamRelay.Close(context.Background()) })
	handler.AllowPrivateStreams = func(int) bool { return true }
	handler.VirtualCandidateTrustWindow = func() time.Duration { return 720 * time.Hour }
	handler.PlaybackConfig = func() config.PlaybackConfig {
		return config.PlaybackConfig{FFmpegPath: ffmpeg}
	}
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(
		func(_ context.Context, virtualURI string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			return ResolvedVirtualMedia{URL: resolvedURL, URI: virtualURI, CandidateID: "cand-a"}, nil
		})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID, nil)
	req = req.WithContext(newAuthorizedPlaybackContext())
	req = withPlaybackRouteParam(req, "session_id", session.ID)

	handler.HandleStream(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("stream status = %d, body %q", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "media" {
		t.Fatalf("stream body = %q, want the remux bytes", rec.Body.String())
	}
	got, err := os.ReadFile(recordedInput)
	if err != nil {
		t.Fatalf("the remux never started FFmpeg with an input: %v", err)
	}
	// The resolver's URL is rewritten to the relay's loopback address, so pin
	// the shape rather than the exact string: FFmpeg must receive an HTTP URL
	// that the relay serves, never the virtual:// identity on the catalog row.
	parsed, err := url.Parse(string(got))
	if err != nil {
		t.Fatalf("remux input %q is not a URL: %v", got, err)
	}
	if parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.Path == "" {
		t.Fatalf("remux input = %q, want the resolved loopback relay URL", got)
	}
}
