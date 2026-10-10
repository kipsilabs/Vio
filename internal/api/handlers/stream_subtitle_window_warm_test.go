package handlers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/remotestream"
)

// newVirtualSubtitleWindowFixture builds a StreamHandler rooted at dir that
// resolves a virtual source through a live relay and extracts text subtitles
// through a fake ffmpeg at dir/ffmpeg. The caller writes the ffmpeg script and
// gets back the handler, the bound session, the virtual file, and the pinned
// source URI.
func newVirtualSubtitleWindowFixture(t *testing.T, dir, ffmpegScript string) (*StreamHandler, *playback.Session, *models.MediaFile, string) {
	t.Helper()
	writeExecutableScript(t, filepath.Join(dir, "ffmpeg"), ffmpegScript)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("provider-media"))
	}))
	t.Cleanup(upstream.Close)

	handler := NewStreamHandler(nil, nil)
	handler.PlaybackConfig = playbackTestConfig(filepath.Join(dir, "ffmpeg"), dir)
	handler.SubtitleCache = playback.NewSubtitleCache(func() string { return dir })
	handler.RemoteStreamRelay = remotestream.NewRelay()
	t.Cleanup(func() { _ = handler.RemoteStreamRelay.Close(context.Background()) })
	handler.AllowPrivateStreams = func(int) bool { return true }
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		return ResolvedVirtualMedia{URL: upstream.URL + "/video.mkv"}, nil
	})

	virtualURI := "virtual://movie/tt-window?result=cand-window"
	file := &models.MediaFile{
		ID: 91, ContentID: "movie-window", FilePath: virtualURI,
		SubtitleTracks:             []models.SubtitleTrack{{Index: 0, Codec: "subrip"}},
		VirtualOwnerInstallationID: 5,
	}
	session := &playback.Session{
		ID: "sess-window", UserID: 1, ProfileID: "profile-1", MediaFileID: file.ID,
		VirtualSourceURI: virtualURI, VirtualSubtitleTracks: file.SubtitleTracks,
	}
	// The window-miss warm is detached; wait for it to settle before the
	// fixture's temp dir is removed, or the warm keeps writing into a directory
	// t.TempDir is tearing down. Registered after the relay-close cleanup so
	// LIFO runs this wait first — the warm needs the relay it resolved through.
	t.Cleanup(handler.waitForBackgroundSubtitleWarms)
	return handler, session, file, virtualURI
}

// warmArgsLogScript logs every invocation and distinguishes the full-track
// warm (no -ss) from the windowed extract. The warm blocks on gate so the
// coalescing test can hold it in flight.
func warmArgsLogScript(argsLog, gate string) string {
	return "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + argsLog + "'\n" +
		"case \"$*\" in\n" +
		"  *-ss*)\n    cat <<'VTT'\n" + warmSubtitleVTT + "VTT\n    ;;\n" +
		"  *)\n    while [ ! -f '" + gate + "' ]; do sleep 0.01; done\n    cat <<'VTT'\n" + warmSubtitleVTT + "VTT\n    ;;\n" +
		"esac\n"
}

func ffmpegLogLines(t *testing.T, argsLog string) (window, warm int) {
	t.Helper()
	data, err := os.ReadFile(argsLog)
	if os.IsNotExist(err) {
		return 0, 0
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.Contains(line, "-ss") {
			window++
		} else {
			warm++
		}
	}
	return window, warm
}

func waitForWarmInvocation(t *testing.T, argsLog string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, warm := ffmpegLogLines(t, argsLog); warm > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("detached full-track warm never started")
}

func waitForCommittedTextEntry(t *testing.T, c *playback.SubtitleCache, identity string, trackIndex int, codec string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c.HasCommittedTextEntry("unused", identity, trackIndex, codec, "") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("warm never committed a %s entry under identity %q", codec, identity)
}

// A windowed virtual text miss must stream its own window and then start
// exactly one detached full-track warm under the serve identity. A second
// window miss while that warm is in flight must not start a second fill.
func TestVirtualTextWindowMissWarmsOnceWithServeIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "ffmpeg.args")
	gate := filepath.Join(dir, "warm.gate")
	handler, session, file, _ := newVirtualSubtitleWindowFixture(t, dir, warmArgsLogScript(argsLog, gate))
	// Always release the warm, even if an assertion fails, so the blocked fake
	// ffmpeg cannot outlive the test. Registered after the fixture's wait
	// cleanup so LIFO releases the gate before the wait blocks on it.
	t.Cleanup(func() { _ = os.WriteFile(gate, []byte("go"), 0o644) })

	serve := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/subtitle?position=600&duration=600", nil)
		rec := httptest.NewRecorder()
		handler.streamEmbeddedSubtitle(rec, req, file, 0, session, false, "vtt")
		return rec
	}

	if rec := serve(); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "WEBVTT") {
		t.Fatalf("first window: status=%d body=%q", rec.Code, rec.Body.String())
	}
	// The first window request started its detached warm; wait until it holds
	// the in-flight fill before issuing the second miss.
	waitForWarmInvocation(t, argsLog)

	if rec := serve(); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "WEBVTT") {
		t.Fatalf("second window: status=%d body=%q", rec.Code, rec.Body.String())
	}

	// Release the warm and wait for the entry it commits.
	if err := os.WriteFile(gate, []byte("go"), 0o644); err != nil {
		t.Fatal(err)
	}
	identity := playback.VirtualSubtitleCacheIdentity(file.ID, session.VirtualSourceURI, 0)
	waitForCommittedTextEntry(t, handler.SubtitleCache, identity, 0, "subrip")

	window, warm := ffmpegLogLines(t, argsLog)
	if window != 2 {
		t.Fatalf("windowed extracts = %d, want 2", window)
	}
	if warm != 1 {
		t.Fatalf("full-track warms = %d, want exactly 1 (coalesced)", warm)
	}
}

// A committed full-track artifact makes the row-vs-evidence drift probe
// unnecessary: the windowed request must skip the probe and serve its window
// from the cached artifact.
func TestVirtualTextWindowCommittedEntrySkipsDriftProbe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "ffmpeg.args")
	probeLog := filepath.Join(dir, "ffprobe.log")
	handler, session, file, _ := newVirtualSubtitleWindowFixture(t, dir,
		"#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+argsLog+"'\ncat <<'VTT'\n"+warmSubtitleVTT+"VTT\n")
	// The drift probe derives ffprobe from the ffmpeg path; install one that
	// records any probe invocation. It reports an empty live layout, so if the
	// probe ran at all the test would still see its log line.
	writeExecutableScript(t, filepath.Join(dir, "ffprobe"),
		"#!/bin/sh\necho probe >> '"+probeLog+"'\nprintf '{\"streams\":[]}'\n")

	identity := playback.VirtualSubtitleCacheIdentity(file.ID, session.VirtualSourceURI, 0)
	done := handler.SubtitleCache.WarmTrackInBackground(playback.StreamExtractOpts{
		InputPath:     "unused",
		CacheIdentity: identity,
		TrackIndex:    0,
		SourceCodec:   "subrip",
		FFmpegPath:    filepath.Join(dir, "ffmpeg"),
	}, playback.StreamExtractSubtitle, "")
	<-done
	if !handler.SubtitleCache.HasCommittedTextEntry("unused", identity, 0, "subrip", "") {
		t.Fatal("pre-warm did not commit the serve identity")
	}
	if err := os.Remove(argsLog); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/subtitle?position=600&duration=600", nil)
	rec := httptest.NewRecorder()
	handler.streamEmbeddedSubtitle(rec, req, file, 0, session, true, "vtt")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "WEBVTT") {
		t.Fatalf("window with committed entry: status=%d body=%q", rec.Code, rec.Body.String())
	}
	if got := countLogLines(t, probeLog); got != 0 {
		t.Fatalf("drift probe ran %d times despite a committed artifact, want 0", got)
	}
	data, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.TrimSpace(string(data))
	if !strings.Contains(args, "subtitle-cache") {
		t.Fatalf("windowed extract did not read the cached artifact: %s", args)
	}
	if !strings.Contains(args, "-ss") {
		t.Fatalf("windowed extract lost its window: %s", args)
	}
}

// The window-miss warm keys on the exact ordinal the serve path hands it, so
// after a drift remap the post-remap identity is what gets populated and the
// remapped serve path finds it. The start-path warm still keys on the plan
// ordinal; the serve-path warm is authoritative for the remap case, so an
// orphaned plan-ordinal entry self-heals on the next window.
func TestVirtualTextWindowWarmKeysOnServeOrdinalAfterRemap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "ffmpeg.args")
	handler, session, file, virtualURI := newVirtualSubtitleWindowFixture(t, dir,
		"#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+argsLog+"'\ncat <<'VTT'\n"+warmSubtitleVTT+"VTT\n")

	const remappedOrdinal = 1
	// This is the identity the remapped serve path computes; the warm must
	// commit under it.
	serveIdentity := playback.VirtualSubtitleCacheIdentity(file.ID, virtualURI, remappedOrdinal)
	handler.warmVirtualSubtitleAfterWindowMiss(file, session, playback.StreamExtractOpts{
		InputPath:       "unused",
		CacheIdentity:   serveIdentity,
		TrackIndex:      remappedOrdinal,
		SourceCodec:     "subrip",
		SeekSeconds:     600,
		DurationSeconds: 600,
		FFmpegPath:      filepath.Join(dir, "ffmpeg"),
	}, true, "")
	waitForCommittedTextEntry(t, handler.SubtitleCache, serveIdentity, remappedOrdinal, "subrip")

	planIdentity := playback.VirtualSubtitleCacheIdentity(file.ID, virtualURI, 0)
	if handler.SubtitleCache.HasCommittedTextEntry("unused", planIdentity, 0, "subrip", "") {
		t.Fatal("warm must not commit under the orphaned plan ordinal")
	}
}

// A non-virtual handler (virtualActive false) must not start any warm: local
// sources keep the cache's own serve-path warming.
func TestVirtualTextWindowWarmSkipsNonVirtual(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "ffmpeg.args")
	handler, session, file, virtualURI := newVirtualSubtitleWindowFixture(t, dir,
		"#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+argsLog+"'\n")
	identity := playback.VirtualSubtitleCacheIdentity(file.ID, virtualURI, 0)

	handler.warmVirtualSubtitleAfterWindowMiss(file, session, playback.StreamExtractOpts{
		InputPath:       "unused",
		CacheIdentity:   identity,
		TrackIndex:      0,
		SourceCodec:     "subrip",
		SeekSeconds:     100,
		DurationSeconds: 600,
		FFmpegPath:      filepath.Join(dir, "ffmpeg"),
	}, false, "")

	if _, err := os.Stat(argsLog); !os.IsNotExist(err) {
		t.Fatalf("no warm should run for a non-virtual request: %v", err)
	}
}

// A virtual PGS whole-track fetch must be bounded to an implicit window and
// advertise its range, then warm the full .sup so a repeat can scan the cached
// artifact. This is the live regression: the whole-track .sup exceeded the
// client deadline, the partial fill was discarded on disconnect, and every
// repeat paid a fresh full remote demux while an uncommitted warm contended.
func TestVirtualPGSWholeTrackFetchServesImplicitWindowThenWarms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "ffmpeg.args")
	gate := filepath.Join(dir, "warm.gate")
	handler, session, file, virtualURI := newVirtualSubtitleWindowFixture(t, dir, warmArgsLogScript(argsLog, gate))
	// Release the warm before the fixture's wait cleanup blocks on it, even if
	// an assertion fails and the gate is never opened inline.
	t.Cleanup(func() { _ = os.WriteFile(gate, []byte("go"), 0o644) })
	file.SubtitleTracks = []models.SubtitleTrack{{Index: 0, Codec: "hdmv_pgs_subtitle"}}
	session.VirtualSubtitleTracks = file.SubtitleTracks

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/subtitle", nil)
	handler.streamEmbeddedSubtitle(rec, req, file, 0, session, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("PGS whole-track fetch = %d %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(playback.SubtitleCoverageHeader); got != "0.000-600.000" {
		t.Fatalf("PGS windowed coverage header = %q, want 0.000-600.000", got)
	}

	waitForWarmInvocation(t, argsLog)
	if err := os.WriteFile(gate, []byte("go"), 0o644); err != nil {
		t.Fatal(err)
	}
	identity := playback.VirtualSubtitleCacheIdentity(file.ID, virtualURI, 0)
	waitForCommittedEntry(t, handler.SubtitleCache, identity, 0, "hdmv_pgs_subtitle")

	window, warm := ffmpegLogLines(t, argsLog)
	if window != 1 {
		t.Fatalf("windowed PGS extracts = %d, want exactly 1", window)
	}
	if warm != 1 {
		t.Fatalf("full-track PGS warms = %d, want exactly 1", warm)
	}
}

// A cold whole-track ASS fetch against a large virtual source must be bounded
// to an implicit self-contained ASS window instead of demuxing the whole
// container, and advertise the range.
func TestVirtualImplicitWindowAppliesToASS(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "ffmpeg.args")
	handler, session, file, _ := newVirtualSubtitleWindowFixture(t, dir,
		"#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+argsLog+"'\ncat <<'VTT'\n"+warmSubtitleVTT+"VTT\n")
	file.SubtitleTracks = []models.SubtitleTrack{{Index: 0, Codec: "ass"}}
	session.VirtualSubtitleTracks = file.SubtitleTracks

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/subtitle", nil)
	handler.streamEmbeddedSubtitle(rec, req, file, 0, session, false, "ass")
	if rec.Code != http.StatusOK {
		t.Fatalf("ASS whole-track fetch = %d %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(playback.SubtitleCoverageHeader); got != "0.000-600.000" {
		t.Fatalf("ASS windowed coverage header = %q, want 0.000-600.000", got)
	}
	windowLine := windowedFFmpegArgs(t, argsLog)
	for _, want := range []string{"-ss 0.000", "-to 600.000", "-copyts", "-c:s copy", "-f ass pipe:1"} {
		if !strings.Contains(windowLine, want) {
			t.Fatalf("implicit ASS window args %q missing %q", windowLine, want)
		}
	}
}

// Once a full-track .sup is committed, a repeat whole-track PGS fetch must
// serve it whole from the cache with no ffmpeg and no coverage header — the
// warm paying off instead of re-demuxing the source.
func TestVirtualPGSWholeTrackFetchServesCommittedWholeTrack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "ffmpeg.args")
	handler, session, file, virtualURI := newVirtualSubtitleWindowFixture(t, dir,
		"#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+argsLog+"'\nprintf 'PG'\n")
	file.SubtitleTracks = []models.SubtitleTrack{{Index: 0, Codec: "hdmv_pgs_subtitle"}}
	session.VirtualSubtitleTracks = file.SubtitleTracks

	identity := playback.VirtualSubtitleCacheIdentity(file.ID, virtualURI, 0)
	done := handler.SubtitleCache.WarmTrackInBackground(playback.StreamExtractOpts{
		InputPath:     "unused",
		CacheIdentity: identity,
		TrackIndex:    0,
		SourceCodec:   "hdmv_pgs_subtitle",
		FFmpegPath:    filepath.Join(dir, "ffmpeg"),
	}, playback.StreamExtractSubtitle, "")
	<-done
	if !handler.SubtitleCache.HasCommittedEntry("unused", identity, 0, "hdmv_pgs_subtitle", "") {
		t.Fatal("pre-warm did not commit the .sup artifact")
	}
	if err := os.Remove(argsLog); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/subtitle", nil)
	handler.streamEmbeddedSubtitle(rec, req, file, 0, session, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("PGS cache serve = %d %q", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(argsLog); !os.IsNotExist(err) {
		t.Fatalf("ffmpeg ran for a committed PGS cache hit: %v", err)
	}
	if got := rec.Header().Get(playback.SubtitleCoverageHeader); got != "" {
		t.Fatalf("whole-track PGS response carried a coverage header: %q", got)
	}
}

// windowedFFmpegArgs returns the single logged invocation that carries -ss.
func windowedFFmpegArgs(t *testing.T, argsLog string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(argsLog)
		if err == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				if strings.Contains(line, "-ss") {
					return line
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no windowed ffmpeg invocation logged")
	return ""
}

// waitForCommittedEntry is waitForCommittedTextEntry generalized to every
// sidecar class, so a PGS warm can be asserted.
func waitForCommittedEntry(t *testing.T, c *playback.SubtitleCache, identity string, trackIndex int, codec string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c.HasCommittedEntry("unused", identity, trackIndex, codec, "") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("warm never committed a %s entry under identity %q", codec, identity)
}

// A detached virtual text warm that cannot resolve its relay input must not pin
// the cache's warm slot and in-flight fill forever: resolution runs on a child
// of the extraction context, so a stuck resolver is canceled when the resolve
// budget expires and a later warm can take the slot.
func TestVirtualTextWindowWarmResolveTimeoutReleasesSlot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	oldTimeout := virtualSubtitleWarmResolveTimeout
	virtualSubtitleWarmResolveTimeout = 50 * time.Millisecond
	t.Cleanup(func() { virtualSubtitleWarmResolveTimeout = oldTimeout })

	dir := t.TempDir()
	argsLog := filepath.Join(dir, "ffmpeg.args")
	handler, session, file, virtualURI := newVirtualSubtitleWindowFixture(t, dir,
		"#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+argsLog+"'\ncat <<'VTT'\n"+warmSubtitleVTT+"VTT\n")

	var calls atomic.Int32
	entered := make(chan struct{}, 2)
	released := make(chan struct{}, 2)
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(ctx context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		if calls.Add(1) <= 2 {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-ctx.Done()
			released <- struct{}{}
			return ResolvedVirtualMedia{}, ctx.Err()
		}
		return ResolvedVirtualMedia{URL: "http://127.0.0.1:1/video.mkv"}, nil
	})

	optsFor := func(track int) playback.StreamExtractOpts {
		return playback.StreamExtractOpts{
			InputPath:       "unused",
			CacheIdentity:   playback.VirtualSubtitleCacheIdentity(file.ID, virtualURI, track),
			TrackIndex:      track,
			SourceCodec:     "subrip",
			SeekSeconds:     600,
			DurationSeconds: 600,
			FFmpegPath:      filepath.Join(dir, "ffmpeg"),
		}
	}

	// Occupy both warm slots with resolvers that block until canceled.
	handler.warmVirtualSubtitleAfterWindowMiss(file, session, optsFor(0), true, "")
	handler.warmVirtualSubtitleAfterWindowMiss(file, session, optsFor(1), true, "")
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("blocked warm resolver never started")
		}
	}

	// With both slots held, a third warm is dropped rather than queued.
	handler.warmVirtualSubtitleAfterWindowMiss(file, session, optsFor(2), true, "")
	if _, err := os.Stat(argsLog); !os.IsNotExist(err) {
		t.Fatalf("a warm extracted while both slots were held: %v", err)
	}

	// The resolve deadline must cancel both resolvers and release the slots.
	for i := 0; i < 2; i++ {
		select {
		case <-released:
		case <-time.After(2 * time.Second):
			t.Fatal("blocked warm resolver was not canceled by the resolve deadline")
		}
	}

	// A retried warm can now take a slot and commit.
	identity := playback.VirtualSubtitleCacheIdentity(file.ID, virtualURI, 2)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		handler.warmVirtualSubtitleAfterWindowMiss(file, session, optsFor(2), true, "")
		if handler.SubtitleCache.HasCommittedTextEntry("unused", identity, 2, "subrip", "") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("retried warm never committed after the resolve deadline released the slots")
}

// A virtual PGS (.sup) response commits 200 before ffmpeg spawns, so a drift
// probe that cannot establish the live layout must fail closed with a
// retryable error rather than let an unvalidated extraction commit a possibly
// truncated track. Text/ASS fails closed the same way: the URL names a
// plan-time ordinal whose validity depended on the release the probe was
// supposed to confirm, and a same-ordinal different-language rotation would
// never trip the post-spawn map net.
func TestVirtualSubtitleProbeFailureFailsClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}

	t.Run("pgs_fails_closed", func(t *testing.T) {
		dir := t.TempDir()
		argsLog := filepath.Join(dir, "ffmpeg.args")
		handler, session, file, _ := newVirtualSubtitleWindowFixture(t, dir,
			"#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+argsLog+"'\nprintf 'SUP'\n")
		writeExecutableScript(t, filepath.Join(dir, "ffprobe"), "#!/bin/sh\nexit 1\n")
		file.SubtitleTracks = []models.SubtitleTrack{{Index: 0, Codec: "hdmv_pgs_subtitle"}}
		session.VirtualSubtitleTracks = file.SubtitleTracks

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/subtitle", nil)
		handler.streamEmbeddedSubtitle(rec, req, file, 0, session, true)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("PGS probe failure = %d %q, want retryable 503 before any 200", rec.Code, rec.Body.String())
		}
		if _, err := os.Stat(argsLog); !os.IsNotExist(err) {
			t.Fatalf("ffmpeg spawned after a failed PGS probe: %v", err)
		}
	})

	t.Run("text_fails_closed", func(t *testing.T) {
		dir := t.TempDir()
		argsLog := filepath.Join(dir, "ffmpeg.args")
		handler, session, file, _ := newVirtualSubtitleWindowFixture(t, dir,
			"#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+argsLog+"'\ncat <<'VTT'\n"+warmSubtitleVTT+"VTT\n")
		writeExecutableScript(t, filepath.Join(dir, "ffprobe"), "#!/bin/sh\nexit 1\n")

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/subtitle", nil)
		handler.streamEmbeddedSubtitle(rec, req, file, 0, session, true)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("text probe failure = %d %q, want retryable 503 (never serve a drifted ordinal unverified)", rec.Code, rec.Body.String())
		}
		if _, err := os.Stat(argsLog); !os.IsNotExist(err) {
			t.Fatalf("ffmpeg spawned after a failed text drift probe: %v", err)
		}
	})
}

// A cold virtual font fetch returns a pending response within its short client
// budget while the extraction keeps running. The detached flight, not the HTTP
// waiter, owns the relay registration: releasing it at handler return would
// leave ffprobe/ffmpeg opening a relay entry that no longer exists.
func TestHandleSubtitleFontsKeepsRelayForDetachedFlight(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	restore := setFontBundleClientWait(50 * time.Millisecond)
	defer restore()

	dir := t.TempDir()
	urlLog := filepath.Join(dir, "font.url")
	gate := filepath.Join(dir, "font.gate")
	probeScript := "#!/bin/sh\n" +
		"for a in \"$@\"; do url=\"$a\"; done\n" +
		"printf '%s' \"$url\" > '" + urlLog + "'\n" +
		"while [ ! -f '" + gate + "' ]; do sleep 0.01; done\n" +
		fontProbeJSON
	writeExecutableScript(t, filepath.Join(dir, "ffprobe"), probeScript)
	writeExecutableScript(t, filepath.Join(dir, "ffmpeg"), fontFFmpegDumpScript(filepath.Join(dir, "dump.log")))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("provider-media"))
	}))
	t.Cleanup(upstream.Close)

	virtualURI := "virtual://movie/tt-fonts?result=cand-fonts"
	file := &models.MediaFile{ID: 42, ContentID: "movie-fonts", FilePath: virtualURI,
		SubtitleTracks:             []models.SubtitleTrack{{Index: 4, Codec: "ass"}},
		VirtualOwnerInstallationID: 5,
	}
	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.UpdateStreamState(session.ID, playback.SessionStreamState{
		VirtualSourceSet:                 true,
		VirtualSourceURI:                 virtualURI,
		VirtualSourceOwnerInstallationID: 5,
		VirtualSubtitleEvidenceSet:       true,
		VirtualSubtitleTracks:            file.SubtitleTracks,
	}); err != nil {
		t.Fatal(err)
	}

	handler := NewStreamHandler(manager, testPlaybackFileResolver{file: file})
	handler.PlaybackConfig = playbackTestConfig(filepath.Join(dir, "ffmpeg"), dir)
	handler.SubtitleCache = playback.NewSubtitleCache(func() string { return dir })
	handler.RemoteStreamRelay = remotestream.NewRelay()
	t.Cleanup(func() { _ = handler.RemoteStreamRelay.Close(context.Background()) })
	handler.AllowPrivateStreams = func(int) bool { return true }
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		return ResolvedVirtualMedia{URL: upstream.URL + "/video.mkv"}, nil
	})

	recorder := httptest.NewRecorder()
	handler.HandleSubtitleFonts(recorder, fontBundleHTTPRequest(session.ID, "0"))
	if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != "[]" {
		t.Fatalf("cold font fetch = %d %q, want 200 pending bundle", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get(fontBundlePendingHeader); got != "true" {
		t.Fatalf("pending marker = %q, want true", got)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("pending Cache-Control = %q, want no-store", got)
	}

	relayURL := waitForLoggedURL(t, urlLog)
	resp, err := http.Get(relayURL)
	if err != nil {
		t.Fatalf("relay GET after handler return: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("relay URL after handler return = %d, want 200; the HTTP waiter released the flight's source", resp.StatusCode)
	}

	if err := os.WriteFile(gate, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	key := fontBundleCacheKey(file, virtualURI, handler.ffmpegPath())
	if !pollHandlerFontBundle(t, handler.SubtitleCache, key) {
		t.Fatal("detached extraction never committed after the HTTP waiter returned")
	}
}

func waitForLoggedURL(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			if u := strings.TrimSpace(string(data)); u != "" {
				return u
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("font extraction never recorded its relay input URL")
	return ""
}

// A cold whole-track text fetch against a virtual source must be bounded to an
// implicit first window instead of demuxing the entire container, then start
// exactly one detached full-track warm so later fetches read a small artifact.
// Without this the response needs a full multi-GB read and is aborted by the
// client long before it completes.
func TestVirtualWholeTrackTextFetchServesImplicitWindowThenWarms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "ffmpeg.args")
	gate := filepath.Join(dir, "warm.gate")
	handler, session, file, virtualURI := newVirtualSubtitleWindowFixture(t, dir, warmArgsLogScript(argsLog, gate))
	// Always release the warm, even if an assertion fails, so the blocked fake
	// ffmpeg cannot outlive the test. Registered after the fixture's wait
	// cleanup so LIFO releases the gate before the wait blocks on it.
	t.Cleanup(func() { _ = os.WriteFile(gate, []byte("go"), 0o644) })

	rec := httptest.NewRecorder()
	// No position/duration: the native whole-track fetch the live measurement
	// showed. The server must synthesize the window itself.
	req := httptest.NewRequest(http.MethodGet, "/subtitle", nil)
	handler.streamEmbeddedSubtitle(rec, req, file, 0, session, false, "vtt")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "WEBVTT") {
		t.Fatalf("whole-track fetch = %d %q, want a bounded window body", rec.Code, rec.Body.String())
	}
	// A whole-track request served only a window must say so, or the client
	// cannot know it needs to request subsequent windows.
	if got := rec.Header().Get(playback.SubtitleCoverageHeader); got != "0.000-600.000" {
		t.Fatalf("windowed coverage header = %q, want 0.000-600.000", got)
	}
	if got := rec.Header().Get(playback.SubtitleWindowedHeader); got != "true" {
		t.Fatalf("windowed marker = %q, want true", got)
	}

	waitForWarmInvocation(t, argsLog)
	if err := os.WriteFile(gate, []byte("go"), 0o644); err != nil {
		t.Fatal(err)
	}
	identity := playback.VirtualSubtitleCacheIdentity(file.ID, virtualURI, 0)
	waitForCommittedTextEntry(t, handler.SubtitleCache, identity, 0, "subrip")

	window, warm := ffmpegLogLines(t, argsLog)
	if window != 1 {
		t.Fatalf("windowed extracts = %d, want exactly 1", window)
	}
	if warm != 1 {
		t.Fatalf("full-track warms = %d, want exactly 1", warm)
	}

	data, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
	}
	var windowLine string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.Contains(line, "-ss") {
			windowLine = line
		}
	}
	if !strings.Contains(windowLine, "-ss 0.000") || !strings.Contains(windowLine, "-to 600.000") {
		t.Fatalf("implicit window args = %q, want -ss 0.000 -to 600.000", windowLine)
	}

	// Warm regression: once the detached warm has committed, a second
	// whole-track fetch is served from the small artifact with no source
	// demux — the cache path must pay off rather than re-read the multi-GB
	// source (which is what made the measured repeat fetch slower than the
	// first, because no artifact ever completed).
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/subtitle", nil)
	handler.streamEmbeddedSubtitle(rec2, req2, file, 0, session, false, "vtt")
	if rec2.Code != http.StatusOK || rec2.Body.String() != warmSubtitleVTT {
		t.Fatalf("post-warm fetch = %d %q, want the committed artifact", rec2.Code, rec2.Body.String())
	}
	if got := rec2.Header().Get(playback.SubtitleCoverageHeader); got != "" {
		t.Fatalf("post-warm whole-track fetch carried a coverage header: %q", got)
	}
	window2, warm2 := ffmpegLogLines(t, argsLog)
	if window2 != window || warm2 != warm {
		t.Fatalf("post-warm fetch ran ffmpeg: window %d->%d warm %d->%d", window, window2, warm, warm2)
	}
}

// When a committed full-track artifact already exists, a whole-track request
// must serve it whole from the cache — no window, no ffmpeg — so an established
// virtual track still returns every cue in one response.
func TestVirtualWholeTrackTextFetchServesCommittedWholeTrack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "ffmpeg.args")
	handler, session, file, virtualURI := newVirtualSubtitleWindowFixture(t, dir,
		"#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+argsLog+"'\ncat <<'VTT'\n"+warmSubtitleVTT+"VTT\n")

	identity := playback.VirtualSubtitleCacheIdentity(file.ID, virtualURI, 0)
	done := handler.SubtitleCache.WarmTrackInBackground(playback.StreamExtractOpts{
		InputPath:     "unused",
		CacheIdentity: identity,
		TrackIndex:    0,
		SourceCodec:   "subrip",
		FFmpegPath:    filepath.Join(dir, "ffmpeg"),
	}, playback.StreamExtractSubtitle, "")
	<-done
	if !handler.SubtitleCache.HasCommittedTextEntry("unused", identity, 0, "subrip", "") {
		t.Fatal("pre-warm did not commit the whole-track artifact")
	}
	if err := os.Remove(argsLog); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/subtitle", nil)
	handler.streamEmbeddedSubtitle(rec, req, file, 0, session, false, "vtt")
	if rec.Code != http.StatusOK || rec.Body.String() != warmSubtitleVTT {
		t.Fatalf("whole-track cache serve = %d %q, want the committed artifact", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(argsLog); !os.IsNotExist(err) {
		t.Fatalf("ffmpeg ran for a whole-track cache hit: %v", err)
	}
	// A whole-track response must not advertise a window; clients treat the
	// header's presence as "you got a slice".
	if got := rec.Header().Get(playback.SubtitleCoverageHeader); got != "" {
		t.Fatalf("whole-track response carried a coverage header: %q", got)
	}
	if got := rec.Header().Get(playback.SubtitleWindowedHeader); got != "" {
		t.Fatalf("whole-track response carried a windowed marker: %q", got)
	}
}

// Local files keep the existing whole-track behavior: a local read is cheap and
// bounding it would silently drop cues for a client that wants the complete
// artifact.
func TestLocalWholeTrackTextFetchStaysUnwindowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "ffmpeg.args")
	writeExecutableScript(t, filepath.Join(dir, "ffmpeg"),
		"#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+argsLog+"'\ncat <<'VTT'\n"+warmSubtitleVTT+"VTT\n")
	mediaPath := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(mediaPath, []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}

	handler := NewStreamHandler(nil, nil)
	handler.PlaybackConfig = playbackTestConfig(filepath.Join(dir, "ffmpeg"), dir)
	handler.SubtitleCache = playback.NewSubtitleCache(func() string { return dir })
	file := &models.MediaFile{
		ID: 12, ContentID: "local-movie", FilePath: mediaPath,
		SubtitleTracks: []models.SubtitleTrack{{Index: 0, Codec: "subrip"}},
	}
	session := &playback.Session{ID: "sess-local", UserID: 1, ProfileID: "profile-1", MediaFileID: file.ID}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/subtitle", nil)
	handler.streamEmbeddedSubtitle(rec, req, file, 0, session, false, "vtt")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "WEBVTT") {
		t.Fatalf("local whole-track fetch = %d %q", rec.Code, rec.Body.String())
	}

	window, warm := ffmpegLogLines(t, argsLog)
	if window != 0 || warm != 1 {
		t.Fatalf("local whole-track fetch ran window=%d warm=%d, want window=0 warm=1", window, warm)
	}
}

// A small known virtual source keeps the complete artifact: reading it whole is
// cheap, so bounding it would only drop cues.
func TestVirtualSmallKnownSourceWholeTrackFetchStaysUnwindowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "ffmpeg.args")
	handler, session, file, _ := newVirtualSubtitleWindowFixture(t, dir,
		"#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+argsLog+"'\ncat <<'VTT'\n"+warmSubtitleVTT+"VTT\n")
	file.FileSize = 10 << 20 // 10 MiB, below the implicit-window threshold

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/subtitle", nil)
	handler.streamEmbeddedSubtitle(rec, req, file, 0, session, false, "vtt")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "WEBVTT") {
		t.Fatalf("small virtual whole-track fetch = %d %q", rec.Code, rec.Body.String())
	}

	window, whole := ffmpegLogLines(t, argsLog)
	if window != 0 || whole != 1 {
		t.Fatalf("small virtual fetch ran window=%d whole=%d, want window=0 whole=1", window, whole)
	}
}

// A virtual position without a duration is window intent but open-ended. On a
// large/unknown source the server must supply the implicit window duration so
// the extract cannot run to EOF, and must advertise the resulting bounded range
// rather than `0.000-*`.
func TestVirtualPositionWithoutDurationClampsToImplicitWindow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "ffmpeg.args")
	handler, session, file, _ := newVirtualSubtitleWindowFixture(t, dir,
		"#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+argsLog+"'\ncat <<'VTT'\n"+warmSubtitleVTT+"VTT\n")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/subtitle?position=0", nil)
	handler.streamEmbeddedSubtitle(rec, req, file, 0, session, false, "vtt")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "WEBVTT") {
		t.Fatalf("position-only fetch = %d %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(playback.SubtitleCoverageHeader); got != "0.000-600.000" {
		t.Fatalf("position-only coverage header = %q, want 0.000-600.000", got)
	}
	if got := rec.Header().Get(playback.SubtitleWindowedHeader); got != "true" {
		t.Fatalf("position-only windowed marker = %q, want true", got)
	}
	// The advertised range must match the ffmpeg command: the open `-ss 0`
	// without a `-to` was the unbounded extract this finding is about.
	windowLine := windowedFFmpegArgs(t, argsLog)
	for _, want := range []string{"-ss 0.000", "-to 600.000"} {
		if !strings.Contains(windowLine, want) {
			t.Fatalf("position-only window args %q missing %q", windowLine, want)
		}
	}
}

// A small known virtual source keeps its existing open-ended behavior: reading
// it whole is cheap, so the implicit-window clamp must not touch it.
func TestVirtualSmallKnownSourcePositionOnlyStaysOpenEnded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "ffmpeg.args")
	handler, session, file, _ := newVirtualSubtitleWindowFixture(t, dir,
		"#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+argsLog+"'\ncat <<'VTT'\n"+warmSubtitleVTT+"VTT\n")
	file.FileSize = 10 << 20 // below the implicit-window threshold

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/subtitle?position=0", nil)
	handler.streamEmbeddedSubtitle(rec, req, file, 0, session, false, "vtt")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "WEBVTT") {
		t.Fatalf("small-source position-only fetch = %d %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(playback.SubtitleCoverageHeader); got != "0.000-*" {
		t.Fatalf("small-source coverage header = %q, want open-ended 0.000-*", got)
	}
}

// A subtitle error response that never committed a 200 must not carry the
// bounded-window markers: a header-only classifier would otherwise read a
// failure as a valid window.
func TestSubtitleErrorResponseOmitsCoverageHeaders(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test helper is unix-only")
	}
	dir := t.TempDir()
	handler, session, file, _ := newVirtualSubtitleWindowFixture(t, dir,
		"#!/bin/sh\necho 'intentional generic extract failure' >&2\nexit 1\n")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/subtitle?position=0", nil)
	handler.streamEmbeddedSubtitle(rec, req, file, 0, session, false, "vtt")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("failing extract = %d %q, want 500", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(playback.SubtitleCoverageHeader); got != "" {
		t.Fatalf("error response carried coverage header %q", got)
	}
	if got := rec.Header().Get(playback.SubtitleWindowedHeader); got != "" {
		t.Fatalf("error response carried windowed marker %q", got)
	}
}

// The implicit window start follows the session position (pulled back a little)
// so a resumed fetch covers playback, and falls back to zero for a fresh start.
func TestImplicitVirtualWindowStart(t *testing.T) {
	if got := implicitVirtualWindowStart(nil); got != 0 {
		t.Fatalf("nil session start = %v, want 0", got)
	}
	if got := implicitVirtualWindowStart(&playback.Session{Position: 0}); got != 0 {
		t.Fatalf("fresh session start = %v, want 0", got)
	}
	if got := implicitVirtualWindowStart(&playback.Session{Position: 1}); got != 0 {
		t.Fatalf("position within backoff start = %v, want 0", got)
	}
	if got := implicitVirtualWindowStart(&playback.Session{Position: 3600}); got != 3598 {
		t.Fatalf("resumed session start = %v, want 3598", got)
	}
}
