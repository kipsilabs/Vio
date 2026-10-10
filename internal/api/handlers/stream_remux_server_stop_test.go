package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/remotestream"
)

type detailedResolveRecord struct {
	virtualURI   string
	forceRefresh bool
	excluded     []string
	rotation     bool
}

type remuxServerStopTestResolver struct {
	file *models.MediaFile
}

func (r remuxServerStopTestResolver) GetByID(_ context.Context, id int) (*models.MediaFile, error) {
	if r.file == nil || r.file.ID != id {
		return nil, errors.New("file not found")
	}
	return r.file, nil
}

func (r remuxServerStopTestResolver) GetByPath(_ context.Context, path string) (*models.MediaFile, error) {
	if r.file == nil || r.file.FilePath != path {
		return nil, errors.New("file not found")
	}
	return r.file, nil
}

// createHandshakeFIFO creates a named pipe and starts a background reader that
// closes the returned channel once the writer process writes to the FIFO.
func createHandshakeFIFO(t *testing.T, dir string) (string, <-chan struct{}) {
	t.Helper()
	fifoPath := filepath.Join(dir, "handshake.fifo")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Fatalf("Mkfifo: %v", err)
	}
	f, err := os.OpenFile(fifoPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("OpenFile fifo: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	handshake := make(chan struct{})
	go func() {
		var buf [16]byte
		_, _ = f.Read(buf[:])
		close(handshake)
	}()
	return fifoPath, handshake
}

// writeFakeFFmpegWithHandshake generates a fake FFmpeg executable that supports
// bitstream filter discovery, optionally fails the first remux attempt, and
// deterministically signals a startup handshake via FIFO before sleeping until signaled.
func writeFakeFFmpegWithHandshake(t *testing.T, dir string, failFirstPath string, handshakeFIFO string) string {
	t.Helper()
	ffmpeg := filepath.Join(dir, "ffmpeg")
	script := "#!/bin/sh\n" +
		"for arg in \"$@\"; do\n" +
		"  case \"$arg\" in\n" +
		"    -bsfs) printf 'dovi_rpu\\nfilter_units\\n'; exit 0 ;;\n" +
		"  esac\n" +
		"done\n" +
		"for arg in \"$@\"; do\n" +
		"  if [ \"$arg\" = \"pipe:1\" ]; then\n"
	if failFirstPath != "" {
		script += "    if [ -e \"" + failFirstPath + "\" ]; then\n" +
			"      rm -f \"" + failFirstPath + "\"\n" +
			"      echo 'intentional remux failure' >&2\n" +
			"      exit 1\n" +
			"    fi\n"
	}
	script += "    printf 'started\\n' > \"" + handshakeFIFO + "\"\n" +
		"    exec sleep 120\n" +
		"  fi\n" +
		"done\n" +
		"exit 0\n"
	if err := os.WriteFile(ffmpeg, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	return ffmpeg
}

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

	handshakePath, handshake := createHandshakeFIFO(t, dir)
	ffmpeg := writeFakeFFmpegWithHandshake(t, dir, "", handshakePath)

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
	handler := NewStreamHandler(sessionMgr, remuxServerStopTestResolver{file: file})
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

	var detailedMu sync.Mutex
	var detailedCalls []detailedResolveRecord
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(
		func(ctx context.Context, virtualURI string, _ int, _ int, _ string, forceRefresh bool, excluded []string, _ string) (ResolvedVirtualMedia, error) {
			detailedMu.Lock()
			detailedCalls = append(detailedCalls, detailedResolveRecord{
				virtualURI:   virtualURI,
				forceRefresh: forceRefresh,
				excluded:     append([]string(nil), excluded...),
				rotation:     VirtualCandidateRotationAllowed(ctx),
			})
			detailedMu.Unlock()
			return ResolvedVirtualMedia{
				URL:         relay.URL + "/stream?token=resolved",
				URI:         virtualURI,
				CandidateID: "cand-a",
			}, nil
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

	select {
	case <-handshake:
	case <-time.After(15 * time.Second):
		t.Fatal("the remux process did not signal startup")
	}

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

	detailedMu.Lock()
	calls := append([]detailedResolveRecord(nil), detailedCalls...)
	detailedMu.Unlock()

	for _, call := range calls {
		if call.rotation || len(call.excluded) > 0 {
			t.Fatalf("a server-initiated stop drove a virtual candidate rotation: %+v", call)
		}
	}
}

// TestHandleStream_VirtualRemuxClientCancelDoesNotBlameCandidate covers the client
// cancellation half: when the viewer navigates away or disconnects before the
// first media byte, the resulting remux error must not indict the candidate or rotate.
func TestHandleStream_VirtualRemuxClientCancelDoesNotBlameCandidate(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(source, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}

	handshakePath, handshake := createHandshakeFIFO(t, dir)
	ffmpeg := writeFakeFFmpegWithHandshake(t, dir, "", handshakePath)

	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("video"))
	}))
	defer relay.Close()

	pinned := "virtual://movie/tt-server-cancel?result=cand-a"
	delivered := time.Now().Add(-time.Minute)
	file := &models.MediaFile{
		ID:                         4245,
		ContentID:                  "movie-server-cancel",
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

	var detailedMu sync.Mutex
	var detailedCalls []detailedResolveRecord
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(
		func(ctx context.Context, virtualURI string, _ int, _ int, _ string, forceRefresh bool, excluded []string, _ string) (ResolvedVirtualMedia, error) {
			detailedMu.Lock()
			detailedCalls = append(detailedCalls, detailedResolveRecord{
				virtualURI:   virtualURI,
				forceRefresh: forceRefresh,
				excluded:     append([]string(nil), excluded...),
				rotation:     VirtualCandidateRotationAllowed(ctx),
			})
			detailedMu.Unlock()
			return ResolvedVirtualMedia{
				URL:         relay.URL + "/stream?token=resolved",
				URI:         virtualURI,
				CandidateID: "cand-a",
			}, nil
		})

	rec := httptest.NewRecorder()
	reqCtx, reqCancel := context.WithCancel(newAuthorizedPlaybackContext())
	defer reqCancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID, nil).WithContext(reqCtx)
	req = withPlaybackRouteParam(req, "session_id", session.ID)

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.HandleStream(rec, req)
	}()

	select {
	case <-handshake:
	case <-time.After(15 * time.Second):
		t.Fatal("the remux process did not signal startup")
	}

	reqCancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the remux response did not end when the client canceled")
	}

	select {
	case how := <-blamed:
		t.Fatalf("a client cancellation blamed the virtual candidate (%s)", how)
	default:
	}

	detailedMu.Lock()
	calls := append([]detailedResolveRecord(nil), detailedCalls...)
	detailedMu.Unlock()

	for _, call := range calls {
		if call.rotation || len(call.excluded) > 0 {
			t.Fatalf("a client cancellation drove a virtual candidate rotation: %+v", call)
		}
	}
}

// TestHandleStream_VirtualRemuxCachedRetryServerStopDoesNotBlameCandidate verifies that
// when an initial remux attempt fails and the candidate is retried via cache handoff,
// a server-initiated stop during that retry does not mark the candidate failed or
// rotate to a sibling candidate.
func TestHandleStream_VirtualRemuxCachedRetryServerStopDoesNotBlameCandidate(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(source, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}

	failFirst := filepath.Join(dir, "fail_first")
	if err := os.WriteFile(failFirst, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}

	handshakePath, handshake := createHandshakeFIFO(t, dir)
	ffmpeg := writeFakeFFmpegWithHandshake(t, dir, failFirst, handshakePath)

	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("video"))
	}))
	defer relay.Close()

	pinned := "virtual://movie/tt-cached-server-stop?result=cand-a"
	file := &models.MediaFile{
		ID:                         4243,
		ContentID:                  "movie-cached-server-stop",
		FilePath:                   pinned,
		CodecVideo:                 "h264",
		VideoTracks:                []models.VideoTrack{{Codec: "h264"}},
		VirtualOwnerInstallationID: 5,
		ProviderReleaseName:        "Movie.2024.1080p.WEB-DL",
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
	handler := NewStreamHandler(sessionMgr, remuxServerStopTestResolver{file: file})
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
	handler.VirtualReleaseCacheStatus = func(context.Context, string, int) (bool, bool) {
		return true, true
	}

	var detailedMu sync.Mutex
	var detailedCalls []detailedResolveRecord
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(
		func(ctx context.Context, virtualURI string, _ int, _ int, _ string, forceRefresh bool, excluded []string, _ string) (ResolvedVirtualMedia, error) {
			detailedMu.Lock()
			detailedCalls = append(detailedCalls, detailedResolveRecord{
				virtualURI:   virtualURI,
				forceRefresh: forceRefresh,
				excluded:     append([]string(nil), excluded...),
				rotation:     VirtualCandidateRotationAllowed(ctx),
			})
			detailedMu.Unlock()
			if len(excluded) > 0 {
				return ResolvedVirtualMedia{
					URL:         relay.URL + "/stream?token=sibling",
					URI:         "virtual://movie/tt-cached-server-stop?result=cand-b",
					CandidateID: "cand-b",
				}, nil
			}
			return ResolvedVirtualMedia{
				URL:                 relay.URL + "/stream?token=cached",
				URI:                 pinned,
				CandidateID:         "cand-a",
				OwnerID:             5,
				ProviderReleaseName: file.ProviderReleaseName,
			}, nil
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

	select {
	case <-handshake:
	case <-time.After(15 * time.Second):
		t.Fatal("the cached retry remux process did not signal startup")
	}

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
		t.Fatalf("a server-initiated stop on cached retry blamed the virtual candidate (%s)", how)
	default:
	}

	detailedMu.Lock()
	calls := append([]detailedResolveRecord(nil), detailedCalls...)
	detailedMu.Unlock()

	rotationCount := 0
	cachedHandoffCount := 0
	for _, call := range calls {
		if call.rotation || len(call.excluded) > 0 {
			rotationCount++
		}
		if call.forceRefresh && len(call.excluded) == 0 {
			cachedHandoffCount++
		}
	}
	if cachedHandoffCount != 1 {
		t.Fatalf("cached handoff resolve count = %d, want 1", cachedHandoffCount)
	}
	if rotationCount != 0 {
		t.Fatalf("server-initiated stop on cached retry drove %d rotation resolves: %+v", rotationCount, calls)
	}
}

// TestHandleStream_VirtualRemuxCachedRetryClientCancelDoesNotBlameCandidate verifies that
// when an initial remux attempt fails and the candidate is retried via cache handoff,
// a client cancellation (disconnect) during that retry does not mark the candidate failed
// or rotate to a sibling candidate.
func TestHandleStream_VirtualRemuxCachedRetryClientCancelDoesNotBlameCandidate(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(source, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}

	failFirst := filepath.Join(dir, "fail_first")
	if err := os.WriteFile(failFirst, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}

	handshakePath, handshake := createHandshakeFIFO(t, dir)
	ffmpeg := writeFakeFFmpegWithHandshake(t, dir, failFirst, handshakePath)

	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("video"))
	}))
	defer relay.Close()

	pinned := "virtual://movie/tt-cached-client-cancel?result=cand-a"
	file := &models.MediaFile{
		ID:                         4244,
		ContentID:                  "movie-cached-client-cancel",
		FilePath:                   pinned,
		CodecVideo:                 "h264",
		VideoTracks:                []models.VideoTrack{{Codec: "h264"}},
		VirtualOwnerInstallationID: 5,
		ProviderReleaseName:        "Movie.2024.1080p.WEB-DL",
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
	handler := NewStreamHandler(sessionMgr, remuxServerStopTestResolver{file: file})
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
	handler.VirtualReleaseCacheStatus = func(context.Context, string, int) (bool, bool) {
		return true, true
	}

	var detailedMu sync.Mutex
	var detailedCalls []detailedResolveRecord
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(
		func(ctx context.Context, virtualURI string, _ int, _ int, _ string, forceRefresh bool, excluded []string, _ string) (ResolvedVirtualMedia, error) {
			detailedMu.Lock()
			detailedCalls = append(detailedCalls, detailedResolveRecord{
				virtualURI:   virtualURI,
				forceRefresh: forceRefresh,
				excluded:     append([]string(nil), excluded...),
				rotation:     VirtualCandidateRotationAllowed(ctx),
			})
			detailedMu.Unlock()
			if len(excluded) > 0 {
				return ResolvedVirtualMedia{
					URL:         relay.URL + "/stream?token=sibling",
					URI:         "virtual://movie/tt-cached-client-cancel?result=cand-b",
					CandidateID: "cand-b",
				}, nil
			}
			return ResolvedVirtualMedia{
				URL:                 relay.URL + "/stream?token=cached",
				URI:                 pinned,
				CandidateID:         "cand-a",
				OwnerID:             5,
				ProviderReleaseName: file.ProviderReleaseName,
			}, nil
		})

	rec := httptest.NewRecorder()
	reqCtx, reqCancel := context.WithCancel(newAuthorizedPlaybackContext())
	defer reqCancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID, nil).WithContext(reqCtx)
	req = withPlaybackRouteParam(req, "session_id", session.ID)

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.HandleStream(rec, req)
	}()

	select {
	case <-handshake:
	case <-time.After(15 * time.Second):
		t.Fatal("the cached retry remux process did not signal startup")
	}

	reqCancel()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the remux response did not end when the client canceled")
	}

	select {
	case how := <-blamed:
		t.Fatalf("a client cancellation on cached retry blamed the virtual candidate (%s)", how)
	default:
	}

	detailedMu.Lock()
	calls := append([]detailedResolveRecord(nil), detailedCalls...)
	detailedMu.Unlock()

	rotationCount := 0
	cachedHandoffCount := 0
	for _, call := range calls {
		if call.rotation || len(call.excluded) > 0 {
			rotationCount++
		}
		if call.forceRefresh && len(call.excluded) == 0 {
			cachedHandoffCount++
		}
	}
	if cachedHandoffCount != 1 {
		t.Fatalf("cached handoff resolve count = %d, want 1", cachedHandoffCount)
	}
	if rotationCount != 0 {
		t.Fatalf("client cancellation on cached retry drove %d rotation resolves: %+v", rotationCount, calls)
	}
}
