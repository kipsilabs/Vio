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

	key := fontBundleCacheKey(file, handler.playbackConfig().FFmpegPath)
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
	key := fontBundleCacheKey(file, handler.playbackConfig().FFmpegPath)
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

// An unpinned virtual source is cacheable now that the identity follows the
// probed subtitle layout instead of the rotating result candidate: there is
// nothing left for the pin to anchor.
func TestWarmVirtualFontBundleRunsWithoutPinnedResult(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	handler, session, file, probeLog := newVirtualFontWarmFixture(t, "virtual://movie/tt1", assTestTracks())

	select {
	case <-handler.warmVirtualFontBundleV3(context.Background(), session, file):
	case <-time.After(10 * time.Second):
		t.Fatal("warm did not settle for an unpinned virtual source")
	}
	key := fontBundleCacheKey(file, handler.playbackConfig().FFmpegPath)
	if _, ok := handler.SubtitleCache.LookupFontBundle(key); !ok {
		t.Fatal("warm did not commit a font bundle for an unpinned virtual source")
	}
	if got := countLogLines(t, probeLog); got != 1 {
		t.Fatalf("ffprobe ran %d times for an unpinned virtual source, want 1", got)
	}
}

// A candidate rotation that leaves the probed layout intact is the same
// release: the warm committed under the previous pinned URI must be readable
// under the new one. A changed layout re-keys, so the previous bytes are not
// served for the new inventory.
func TestFontBundleCacheKeyFollowsInventoryAcrossCandidateRotation(t *testing.T) {
	tracks := assTestTracks()
	file := &models.MediaFile{ID: 42, FilePath: "virtual://movie/tt1?result=cand-a", SubtitleTracks: tracks}
	ffmpegPath := "/usr/bin/ffmpeg"
	base := fontBundleCacheKey(file, ffmpegPath)

	rotated := *file
	rotated.FilePath = "virtual://movie/tt1?result=cand-b"
	if got := fontBundleCacheKey(&rotated, ffmpegPath); got != base {
		t.Fatalf("identical layout re-keyed on candidate rotation: %+v vs %+v", got, base)
	}

	changed := *file
	changed.SubtitleTracks = []models.SubtitleTrack{{Index: 4, Codec: "ass"}, {Index: 7, Codec: "subrip"}}
	if got := fontBundleCacheKey(&changed, ffmpegPath); got == base {
		t.Fatal("changed probed inventory kept the font-bundle identity")
	}

	// Captured-empty evidence is still a stable inventory, keyed by file id and
	// the generation bucket rather than the relay URL.
	empty := *file
	empty.SubtitleTracks = nil
	emptyKey := fontBundleCacheKey(&empty, ffmpegPath)
	if emptyKey == base {
		t.Fatal("empty inventory kept the populated-layout identity")
	}
	if got := fontBundleCacheKey(&empty, "/opt/ffmpeg"); got == emptyKey {
		t.Fatal("ffmpeg relocation kept the font-bundle identity")
	}
}

// A candidate rotation that preserves the subtitle layout but moves the
// release's durable provider identity must re-key the font bundle: the attached
// fonts belong to the release, not to the track list, so the previous release's
// warmed bytes must never be served for the new one. A rotation that only moves
// the provider result id is the same release and keeps its warm. Local rows are
// unaffected by provider identity.
func TestFontBundleCacheKeyReKeysOnReleaseRotationWithIdenticalLayout(t *testing.T) {
	tracks := assTestTracks()
	file := &models.MediaFile{
		ID: 42, FilePath: "virtual://movie/tt1?result=cand-a", SubtitleTracks: tracks,
		ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024.1080p", ProviderReleaseSize: 8_000_000_000,
	}
	ffmpegPath := "/usr/bin/ffmpeg"
	base := fontBundleCacheKey(file, ffmpegPath)

	// Same release, new result id: identical inventory and identity still hit.
	rotatedID := *file
	rotatedID.FilePath = "virtual://movie/tt1?result=cand-b"
	if got := fontBundleCacheKey(&rotatedID, ffmpegPath); got != base {
		t.Fatalf("identical release and layout re-keyed on candidate rotation: %+v vs %+v", got, base)
	}

	// Same layout, different release: the provider identity moved, so the
	// previous bundle must not be served.
	rotatedRelease := *file
	rotatedRelease.ProviderVideoHash = "hash-b"
	if got := fontBundleCacheKey(&rotatedRelease, ffmpegPath); got == base {
		t.Fatal("changed provider identity with an identical layout kept the font-bundle identity")
	}

	// A row with no durable identity keeps the historical file+inventory key:
	// stable across a result rotation, never equal to an identified release.
	identityless := *file
	identityless.ProviderVideoHash = ""
	identityless.ProviderReleaseName = ""
	identityless.ProviderReleaseSize = 0
	identitylessKey := fontBundleCacheKey(&identityless, ffmpegPath)
	if identitylessKey == base {
		t.Fatal("identity-less virtual row reused the identified release's key")
	}
	identitylessRotated := identityless
	identitylessRotated.FilePath = "virtual://movie/tt1?result=cand-b"
	if got := fontBundleCacheKey(&identitylessRotated, ffmpegPath); got != identitylessKey {
		t.Fatal("identity-less virtual row re-keyed on a result rotation")
	}

	// Local rows key on the file row only; provider fields are not part of
	// their identity.
	mtime := time.Unix(1_700_000_000, 0)
	local := &models.MediaFile{ID: 7, FilePath: "/media/movie.mkv", FileSize: 1024, FileModifiedAt: &mtime}
	localKey := fontBundleCacheKey(local, ffmpegPath)
	withIdentity := *local
	withIdentity.ProviderVideoHash = "hash-a"
	withIdentity.ProviderReleaseName = "Movie.2024.1080p"
	withIdentity.ProviderReleaseSize = 8_000_000_000
	if got := fontBundleCacheKey(&withIdentity, ffmpegPath); got != localKey {
		t.Fatalf("local font-bundle key changed with provider identity: %+v vs %+v", got, localKey)
	}
}
