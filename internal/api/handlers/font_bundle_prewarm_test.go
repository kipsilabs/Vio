package handlers

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

func newVirtualFontWarmFixture(t *testing.T, virtualURI string, tracks []models.SubtitleTrack) (*PlaybackHandler, *playback.Session, *models.MediaFile, string) {
	t.Helper()
	dir := t.TempDir()
	probeLog := filepath.Join(dir, "probe.log")
	dumpLog := filepath.Join(dir, "dump.log")
	mediaPath := filepath.Join(dir, "media.mkv")
	writeExecutableScript(t, filepath.Join(dir, "ffprobe"), fontProbeScript(probeLog, "", false, false))
	writeExecutableScript(t, filepath.Join(dir, "ffmpeg"), fontFFmpegDumpScript(dumpLog))

	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	handler.PlaybackConfig = playbackTestConfig(filepath.Join(dir, "ffmpeg"), dir)
	handler.SubtitleCache = playback.NewSubtitleCache(func() string { return dir })
	handler.VirtualMediaResolver = VirtualMediaResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string) (string, error) {
		return mediaPath, nil
	})
	file := &models.MediaFile{ID: 42, ContentID: "movie-1", FilePath: virtualURI, SubtitleTracks: tracks, VirtualOwnerInstallationID: 5}
	session := &playback.Session{
		ID: "font-warm-session", UserID: 1, ProfileID: "profile-1", MediaFileID: file.ID,
		VirtualSourceURI: virtualURI, VirtualSourceOwnerInstallationID: 5, VirtualSubtitleTracks: tracks,
	}
	return handler, session, file, probeLog
}

func TestWarmVirtualFontBundleRunsOncePerFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	handler, session, file, probeLog := newVirtualFontWarmFixture(t, "virtual://movie/tt1?result=cand-1", assTestTracks())

	done := handler.warmVirtualFontBundleV3(context.Background(), session, file)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("virtual font warm did not settle")
	}

	key := fontBundleCacheKey(file, session.VirtualSourceURI, handler.playbackConfig().FFmpegPath)
	if _, ok := handler.SubtitleCache.LookupFontBundle(key); !ok {
		t.Fatal("warm did not commit a font bundle for the file")
	}
	if got := countLogLines(t, probeLog); got != 1 {
		t.Fatalf("ffprobe ran %d times, want exactly 1 for a file with two ASS tracks", got)
	}
}

// The start path holds a session copy captured before UpdateStreamState wrote
// VirtualSourceURI, so the warm must key off the effective file's URI or it
// silently no-ops exactly where it matters most.
func TestWarmVirtualFontBundleUsesEffectiveFileURIWhenSessionCopyIsStale(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	handler, session, file, probeLog := newVirtualFontWarmFixture(t, "virtual://movie/tt1?result=cand-1", assTestTracks())
	session.VirtualSourceURI = "" // the stale copy startPlannedPlaybackV3 holds

	done := handler.warmVirtualFontBundleV3(context.Background(), session, file)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("virtual font warm did not settle")
	}
	key := fontBundleCacheKey(file, file.FilePath, handler.playbackConfig().FFmpegPath)
	if _, ok := handler.SubtitleCache.LookupFontBundle(key); !ok {
		t.Fatal("warm did not commit a font bundle when the session copy lacked VirtualSourceURI")
	}
	if got := countLogLines(t, probeLog); got != 1 {
		t.Fatalf("ffprobe ran %d times, want exactly 1", got)
	}
}

func TestWarmVirtualFontBundleSkipsWithoutASSTrack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	handler, session, file, probeLog := newVirtualFontWarmFixture(t, "virtual://movie/tt1?result=cand-1",
		[]models.SubtitleTrack{{Index: 4, Codec: "subrip"}})

	select {
	case <-handler.warmVirtualFontBundleV3(context.Background(), session, file):
	case <-time.After(2 * time.Second):
		t.Fatal("warm did not settle for a file without ASS tracks")
	}
	if got := countLogLines(t, probeLog); got != 0 {
		t.Fatalf("ffprobe ran %d times for a file without ASS tracks, want 0", got)
	}
}

func TestWarmVirtualFontBundleSkipsNonVirtualFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	handler, session, file, probeLog := newVirtualFontWarmFixture(t, "/local/movie.mkv", assTestTracks())
	session.VirtualSourceURI = ""

	select {
	case <-handler.warmVirtualFontBundleV3(context.Background(), session, file):
	case <-time.After(2 * time.Second):
		t.Fatal("warm did not settle for a non-virtual file")
	}
	if got := countLogLines(t, probeLog); got != 0 {
		t.Fatalf("ffprobe ran %d times for a non-virtual file, want 0", got)
	}
}

func TestWarmVirtualFontBundleSkipsWithoutPinnedResult(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	handler, session, file, probeLog := newVirtualFontWarmFixture(t, "virtual://movie/tt1", assTestTracks())

	select {
	case <-handler.warmVirtualFontBundleV3(context.Background(), session, file):
	case <-time.After(2 * time.Second):
		t.Fatal("warm did not settle for an unpinned virtual source")
	}
	if got := countLogLines(t, probeLog); got != 0 {
		t.Fatalf("ffprobe ran %d times for an unpinned virtual source, want 0", got)
	}
}
