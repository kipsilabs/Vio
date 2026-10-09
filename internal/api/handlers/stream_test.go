package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/httpstream"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/noderouting"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/remotestream"
	"github.com/Silo-Server/silo-server/internal/streamtoken"
	"github.com/Silo-Server/silo-server/internal/subtitles"
)

type hookedSessionManager struct {
	*playback.SessionManager
	beginTransportHook func()
}

type errStreamFileResolver struct {
	err error
}

type countingStreamFileResolver struct {
	calls int
}

func (r *countingStreamFileResolver) GetByID(context.Context, int) (*models.MediaFile, error) {
	r.calls++
	return nil, errors.New("file lookup must not run")
}

func (r errStreamFileResolver) GetByID(context.Context, int) (*models.MediaFile, error) {
	return nil, r.err
}

func (m *hookedSessionManager) BeginTransport(sessionID string) error {
	if m.beginTransportHook != nil {
		m.beginTransportHook()
	}
	return m.SessionManager.BeginTransport(sessionID)
}

type recoveryFailedWriter struct{ *httptest.ResponseRecorder }

func (w recoveryFailedWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestHandleStreamRecoveryResolvedIdentity(t *testing.T) {
	for _, method := range []playback.PlayMethod{playback.PlayDirect, playback.PlayRemux} {
		for _, tc := range []struct {
			name, result, identity string
		}{
			{"matching", "A", "A"},
			{"substituted", "B", "B"},
			{"unknown", "", ""},
			{"retained requested URI without identity", "A", ""},
			{"mismatched identity", "A", "B"},
		} {
			t.Run(string(method)+"/"+tc.name, func(t *testing.T) {
				identity := tc.identity
				known := identity != "" && identity == tc.result
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("media")) }))
				defer upstream.Close()
				stamp := time.Now().Add(-time.Hour)
				file := &models.MediaFile{ID: 42, FilePath: "virtual://movie/test?result=A", FailedAt: &stamp, VirtualOwnerInstallationID: 7}
				manager := playback.NewSessionManager(0, 0)
				session, err := manager.StartSession(1, "profile-1", file.ID, method, false)
				if err != nil {
					t.Fatal(err)
				}
				h := NewStreamHandler(manager, testPlaybackFileResolver{file: file})
				ffmpeg := filepath.Join(t.TempDir(), "ffmpeg")
				if err := os.WriteFile(ffmpeg, []byte("#!/bin/sh\nprintf media\n"), 0o700); err != nil {
					t.Fatal(err)
				}
				h.PlaybackConfig = func() config.PlaybackConfig { return config.PlaybackConfig{FFmpegPath: ffmpeg} }
				h.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
					uri := ""
					if tc.result != "" {
						uri = "virtual://movie/test?result=" + tc.result
					}
					return ResolvedVirtualMedia{URL: upstream.URL, URI: uri, CandidateID: identity}, nil
				})
				calls := 0
				h.VirtualCandidateRecoveredMarker = func(_ context.Context, id int, path string, observed *time.Time) error {
					calls++
					if path != "virtual://movie/test?result="+identity || observed == nil || !observed.Equal(stamp) {
						t.Fatalf("incorrect recovery: %s %v", path, observed)
					}
					if path == file.FilePath {
						file.FailedAt = nil
					}
					return nil
				}
				req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID, nil).WithContext(newAuthorizedPlaybackContext())
				req = withPlaybackRouteParam(req, "session_id", session.ID)
				rec := httptest.NewRecorder()
				h.HandleStream(rec, req)
				if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
					t.Fatalf("delivery: %d %s", rec.Code, rec.Body.String())
				}
				if (file.FailedAt == nil) != (known && identity == "A") {
					t.Fatalf("wrong recovery for %q", identity)
				}
				if (calls > 0) != known {
					t.Fatalf("marker calls: %d", calls)
				}
				file.FailedAt = &stamp
				calls = 0
				func() {
					defer func() {
						if caught := recover(); caught != nil {
							err, ok := caught.(error)
							if !ok || !errors.Is(err, http.ErrAbortHandler) {
								panic(caught)
							}
						}
					}()
					h.HandleStream(recoveryFailedWriter{httptest.NewRecorder()}, req)
				}()
				if calls != 0 || file.FailedAt == nil {
					t.Fatal("failed initial write cleared failure")
				}
			})
		}
	}
}

// TestHandleStreamRecoveryStampsHealthyFirstDelivery verifies the delivery
// evidence marker fires even when the candidate was never failed. The marker
// now also records durable last_delivered_at evidence, so a healthy first play
// must reach it. (The no-bytes case is covered by
// TestHandleStreamRecoveryResolvedIdentity.)
func TestHandleStreamRecoveryStampsHealthyFirstDelivery(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("media"))
	}))
	defer upstream.Close()
	file := &models.MediaFile{ID: 42, FilePath: "virtual://movie/test?result=A", VirtualOwnerInstallationID: 7}
	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatal(err)
	}
	h := NewStreamHandler(manager, testPlaybackFileResolver{file: file})
	ffmpeg := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(ffmpeg, []byte("#!/bin/sh\nprintf media\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	h.PlaybackConfig = func() config.PlaybackConfig { return config.PlaybackConfig{FFmpegPath: ffmpeg} }
	h.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
		return ResolvedVirtualMedia{URL: upstream.URL, URI: file.FilePath, CandidateID: "A"}, nil
	})
	calls := 0
	h.VirtualCandidateRecoveredMarker = func(_ context.Context, id int, path string, observed *time.Time) error {
		calls++
		if path != file.FilePath || observed != nil {
			t.Fatalf("recovery marker: path=%q observed=%v", path, observed)
		}
		return nil
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID, nil).WithContext(newAuthorizedPlaybackContext())
	req = withPlaybackRouteParam(req, "session_id", session.ID)
	rec := httptest.NewRecorder()
	h.HandleStream(rec, req)
	if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
		t.Fatalf("delivery: %d %s", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Fatalf("recovery marker calls = %d, want 1", calls)
	}
}

func TestHandleStreamVirtualResolveFailureLogsCause(t *testing.T) {
	file := &models.MediaFile{
		ID:                         42,
		ContentID:                  "movie-virtual-unresolvable",
		FilePath:                   "virtual://movie/movie-unresolvable?result=dead",
		VirtualOwnerInstallationID: 0,
	}
	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	cause := errors.New("virtual playback provider returned an unsafe stream URL: remote stream URL targets a non-public address")
	h := NewStreamHandler(manager, testPlaybackFileResolver{file: file})
	h.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
		return ResolvedVirtualMedia{}, cause
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID, nil).WithContext(newAuthorizedPlaybackContext())
	req = withPlaybackRouteParam(req, "session_id", session.ID)
	rec := httptest.NewRecorder()
	h.HandleStream(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", rec.Code, rec.Body.String())
	}
	logged := logs.String()
	for _, want := range []string{
		`"session":"` + session.ID + `"`,
		`"owner_installation_id":0`,
		`"virtual_uri":"` + file.FilePath + `"`,
		"non-public address",
	} {
		if !strings.Contains(logged, want) {
			t.Fatalf("502 log missing %q in:\n%s", want, logged)
		}
	}
}

func TestHandleStream_VirtualDirectPlayUsesPinnedRelayPathAndHeaders(t *testing.T) {
	var gotPath, gotRange, gotReferer string
	var gotQuery url.Values
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query()
		gotRange = r.Header.Get("Range")
		gotReferer = r.Header.Get("Referer")
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Range", "bytes 2-13/14")
		w.Header().Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("virtual-media"))
	}))
	defer upstream.Close()

	file := &models.MediaFile{
		ID:                         42,
		ContentID:                  "movie-virtual",
		FilePath:                   "virtual://movie/movie-virtual?result=selected",
		VirtualOwnerInstallationID: 7,
	}
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := sessionMgr.SetVirtualSource(session.ID, file.FilePath, file.VirtualOwnerInstallationID); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}

	handler := NewStreamHandler(sessionMgr, testPlaybackFileResolver{file: file})
	handler.RemoteStreamRelay = remotestream.NewRelay()
	defer func() { _ = handler.RemoteStreamRelay.Close(context.Background()) }()
	handler.AllowPrivateStreams = func(int) bool { return true }
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, ownerInstallationID, userID int, profileID string, forceRefresh bool, excludedCandidateIDs []string, preferredCandidateID string) (ResolvedVirtualMedia, error) {
		if uri != file.FilePath || ownerInstallationID != 7 || userID != 1 || profileID != "profile-1" || forceRefresh || len(excludedCandidateIDs) != 0 || preferredCandidateID != "" {
			t.Fatalf("unexpected resolver arguments: uri=%q owner=%d user=%d profile=%q refresh=%v excluded=%v preferred=%q", uri, ownerInstallationID, userID, profileID, forceRefresh, excludedCandidateIDs, preferredCandidateID)
		}
		return ResolvedVirtualMedia{
			URL:            upstream.URL + "/provider/video.mp4?provider=1",
			RequestHeaders: map[string]string{"Referer": "https://provider.example/player"},
		}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID+"?st=server-token", nil)
	req.Header.Set("Range", "bytes=2-")
	req = req.WithContext(newAuthorizedPlaybackContext())
	req = withPlaybackRouteParam(req, "session_id", session.ID)
	rr := httptest.NewRecorder()
	handler.HandleStream(rr, req)

	if rr.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Content-Range") != "bytes 2-13/14" || rr.Header().Get("Accept-Ranges") != "bytes" {
		t.Fatalf("range headers = Content-Range %q Accept-Ranges %q", rr.Header().Get("Content-Range"), rr.Header().Get("Accept-Ranges"))
	}
	if rr.Body.String() != "virtual-media" {
		t.Fatalf("body = %q, want virtual-media", rr.Body.String())
	}
	if gotPath != "/provider/video.mp4" {
		t.Fatalf("upstream path = %q, want /provider/video.mp4", gotPath)
	}
	if gotQuery.Get("provider") != "1" || gotQuery.Get("st") != "" {
		t.Fatalf("upstream query = %q, want provider=1 without stream token", gotQuery.Encode())
	}
	if gotRange != "bytes=2-" {
		t.Fatalf("upstream Range = %q, want bytes=2-", gotRange)
	}
	if gotReferer != "https://provider.example/player" {
		t.Fatalf("upstream Referer = %q, want provider header", gotReferer)
	}
}

func TestHandleStream_VirtualDirectPlayProxyErrorHandlerReturns502(t *testing.T) {
	file := &models.MediaFile{
		ID:                         42,
		ContentID:                  "movie-virtual-broken",
		FilePath:                   "virtual://movie/movie-broken?result=dead",
		VirtualOwnerInstallationID: 7,
	}
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := sessionMgr.SetVirtualSource(session.ID, file.FilePath, file.VirtualOwnerInstallationID); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}

	// Create an ephemeral closed port
	deadServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := deadServer.URL
	deadServer.Close()

	handler := NewStreamHandler(sessionMgr, testPlaybackFileResolver{file: file})
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		// Return an unreachable loopback URL without a relay wrapper to trigger ReverseProxy dial failure and ErrorHandler
		return ResolvedVirtualMedia{
			URL: deadURL + "/video.mp4",
		}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID, nil)
	req = req.WithContext(newAuthorizedPlaybackContext())
	req = withPlaybackRouteParam(req, "session_id", session.ID)
	rr := httptest.NewRecorder()
	handler.HandleStream(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body = %s", rr.Code, rr.Body.String())
	}
	var errResp struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decode json response: %v, body = %s", err, rr.Body.String())
	}
	if errResp.Error != "virtual_stream_unavailable" {
		t.Fatalf("error code = %q, want virtual_stream_unavailable", errResp.Error)
	}
}

func TestHandleStream_VirtualDirectPlayRelay502ReturnsStructuredJSON(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream provider failure"))
	}))
	defer upstream.Close()

	file := &models.MediaFile{
		ID:                         42,
		ContentID:                  "movie-virtual-relay-fail",
		FilePath:                   "virtual://movie/movie-fail?result=dead",
		VirtualOwnerInstallationID: 7,
	}
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := sessionMgr.SetVirtualSource(session.ID, file.FilePath, file.VirtualOwnerInstallationID); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}

	handler := NewStreamHandler(sessionMgr, testPlaybackFileResolver{file: file})
	handler.RemoteStreamRelay = remotestream.NewRelay()
	defer func() { _ = handler.RemoteStreamRelay.Close(context.Background()) }()
	handler.AllowPrivateStreams = func(int) bool { return true }
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		return ResolvedVirtualMedia{
			URL: upstream.URL + "/provider/stream.mp4",
		}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID, nil)
	req = req.WithContext(newAuthorizedPlaybackContext())
	req = withPlaybackRouteParam(req, "session_id", session.ID)
	rr := httptest.NewRecorder()
	handler.HandleStream(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body = %s", rr.Code, rr.Body.String())
	}
	var errResp struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decode json response: %v, body = %s", err, rr.Body.String())
	}
	if errResp.Error != "virtual_stream_unavailable" {
		t.Fatalf("error code = %q, want virtual_stream_unavailable", errResp.Error)
	}
}

func TestHandleStream_VirtualDirectPlayRetriesWithForcedRefresh(t *testing.T) {
	liveServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("refreshed-live-stream"))
	}))
	defer liveServer.Close()

	deadServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer deadServer.Close()

	file := &models.MediaFile{
		ID:                         42,
		ContentID:                  "movie-virtual-refresh",
		FilePath:                   "virtual://movie/movie-refresh?result=first",
		VirtualOwnerInstallationID: 7,
	}
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := sessionMgr.SetVirtualSource(session.ID, file.FilePath, file.VirtualOwnerInstallationID); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}

	handler := NewStreamHandler(sessionMgr, testPlaybackFileResolver{file: file})
	handler.RemoteStreamRelay = remotestream.NewRelay()
	defer func() { _ = handler.RemoteStreamRelay.Close(context.Background()) }()
	handler.AllowPrivateStreams = func(int) bool { return true }

	refreshCalls := 0
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, forceRefresh bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		if forceRefresh {
			refreshCalls++
			return ResolvedVirtualMedia{URL: liveServer.URL + "/video.mp4"}, nil
		}
		return ResolvedVirtualMedia{URL: deadServer.URL + "/video.mp4"}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID, nil)
	req = req.WithContext(newAuthorizedPlaybackContext())
	req = withPlaybackRouteParam(req, "session_id", session.ID)
	rr := httptest.NewRecorder()
	handler.HandleStream(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != "refreshed-live-stream" {
		t.Fatalf("body = %q, want refreshed-live-stream", rr.Body.String())
	}
	if refreshCalls != 1 {
		t.Fatalf("refreshCalls = %d, want 1", refreshCalls)
	}
}

func TestHandleStream_VirtualDirectPlayRefreshRejectsMismatchedCandidateID(t *testing.T) {
	candBServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("candidate-B-bytes"))
	}))
	defer candBServer.Close()

	deadServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer deadServer.Close()

	file := &models.MediaFile{
		ID:                         42,
		ContentID:                  "movie-virtual-mismatch",
		FilePath:                   "virtual://movie/movie-mismatch?result=cand-A",
		VirtualOwnerInstallationID: 7,
	}
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := sessionMgr.SetVirtualSource(session.ID, file.FilePath, file.VirtualOwnerInstallationID); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}

	handler := NewStreamHandler(sessionMgr, testPlaybackFileResolver{file: file})
	handler.RemoteStreamRelay = remotestream.NewRelay()
	defer func() { _ = handler.RemoteStreamRelay.Close(context.Background()) }()
	handler.AllowPrivateStreams = func(int) bool { return true }

	var rotationFlags []bool
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(ctx context.Context, _ string, _ int, _ int, _ string, forceRefresh bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		rotationFlags = append(rotationFlags, VirtualCandidateRotationAllowed(ctx))
		if forceRefresh {
			return ResolvedVirtualMedia{
				URL:         candBServer.URL + "/video.mp4",
				CandidateID: "cand-B",
			}, nil
		}
		return ResolvedVirtualMedia{
			URL:         deadServer.URL + "/video.mp4",
			CandidateID: "cand-A",
		}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID, nil)
	req = req.WithContext(newAuthorizedPlaybackContext())
	req = withPlaybackRouteParam(req, "session_id", session.ID)
	rr := httptest.NewRecorder()
	handler.HandleStream(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body = %s", rr.Code, rr.Body.String())
	}
	if rr.Body.String() == "candidate-B-bytes" {
		t.Fatal("handler served candidate B bytes instead of rejecting mismatched candidate")
	}
	// No candidate was indicted (the resolve carried no URI to name the
	// delivered release), so the retry must not declare substitution: the
	// mismatch stays a silent-swap refusal.
	if len(rotationFlags) < 2 {
		t.Fatalf("resolution calls = %d, want the initial resolve and the retry", len(rotationFlags))
	}
	if rotationFlags[0] || rotationFlags[1] {
		t.Fatalf("non-indicting retry declared rotation: %#v", rotationFlags)
	}
}

// multiWriteHeaderRecorder records every WriteHeader a handler emits. A remux
// first attempt commits its own 502 through remux.go, so the status a plain
// recorder keeps is the 502; the recorded sequence still shows the retry's 200.
type multiWriteHeaderRecorder struct {
	*httptest.ResponseRecorder
	statuses []int
}

func (w *multiWriteHeaderRecorder) WriteHeader(code int) {
	w.statuses = append(w.statuses, code)
	w.ResponseRecorder.WriteHeader(code)
}

// TestHandleStream_VirtualDirectPlayIndictingRetryServesSibling covers the
// direct-play serve-layer failover: a pinned candidate that produced no bytes is
// indicted (marked failed) and excluded, and the retry must declare substitution
// so the resolver may serve a sibling release. Without the declaration the
// resolver refuses the swap and the failover degenerates into a replan.
func TestHandleStream_VirtualDirectPlayIndictingRetryServesSibling(t *testing.T) {
	liveServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("candidate-B-bytes"))
	}))
	defer liveServer.Close()

	deadServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer deadServer.Close()

	file := &models.MediaFile{
		ID:                         42,
		ContentID:                  "movie-indict",
		FilePath:                   "virtual://movie/movie-indict?result=cand-A",
		VirtualOwnerInstallationID: 7,
	}
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := sessionMgr.SetVirtualSource(session.ID, file.FilePath, file.VirtualOwnerInstallationID); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}

	handler := NewStreamHandler(sessionMgr, testPlaybackFileResolver{file: file})
	handler.RemoteStreamRelay = remotestream.NewRelay()
	defer func() { _ = handler.RemoteStreamRelay.Close(context.Background()) }()
	handler.AllowPrivateStreams = func(int) bool { return true }

	var rotationFlags []bool
	var excludedSeen [][]string
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(ctx context.Context, _ string, _ int, _ int, _ string, forceRefresh bool, excluded []string, _ string) (ResolvedVirtualMedia, error) {
		rotationFlags = append(rotationFlags, VirtualCandidateRotationAllowed(ctx))
		excludedSeen = append(excludedSeen, append([]string(nil), excluded...))
		if forceRefresh && len(excluded) == 1 && excluded[0] == "cand-A" {
			return ResolvedVirtualMedia{
				URL:         liveServer.URL + "/video.mp4",
				URI:         "virtual://movie/movie-indict?result=cand-B",
				CandidateID: "cand-B",
			}, nil
		}
		return ResolvedVirtualMedia{
			URL:         deadServer.URL + "/video.mp4",
			URI:         "virtual://movie/movie-indict?result=cand-A",
			CandidateID: "cand-A",
		}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID, nil)
	req = req.WithContext(newAuthorizedPlaybackContext())
	req = withPlaybackRouteParam(req, "session_id", session.ID)
	rr := httptest.NewRecorder()
	handler.HandleStream(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != "candidate-B-bytes" {
		t.Fatalf("body = %q, want the sibling candidate's bytes", rr.Body.String())
	}
	if len(rotationFlags) < 2 {
		t.Fatalf("resolution calls = %d, want the initial resolve and the indicting retry", len(rotationFlags))
	}
	if rotationFlags[0] {
		t.Fatal("initial resolve declared rotation")
	}
	if !rotationFlags[1] {
		t.Fatalf("indicting retry did not declare rotation: flags=%#v excluded=%#v", rotationFlags, excludedSeen)
	}
	if len(excludedSeen[1]) != 1 || excludedSeen[1][0] != "cand-A" {
		t.Fatalf("retry exclusions = %#v, want [cand-A]", excludedSeen[1])
	}
}

// TestHandleStream_VirtualRemuxIndictingRetryServesSibling covers the remux
// serve layer. A remux that produced no output indicted the delivered candidate,
// and the retry must declare substitution and serve a sibling. The first attempt
// commits its own 502 through remux.go, so the recorded status sequence (502
// then 200) and the sibling bytes are what prove the retry served.
func TestHandleStream_VirtualRemuxIndictingRetryServesSibling(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	ffmpeg := filepath.Join(dir, "ffmpeg.sh")
	script := "#!/bin/sh\n" +
		"for arg in \"$@\"; do\n" +
		"  case \"$arg\" in\n" +
		"    -bsfs) printf 'dovi_rpu'; exit 0 ;;\n" +
		"  esac\n" +
		"done\n" +
		"if [ -e \"" + started + "\" ]; then printf 'sibling-bytes'; exit 0; fi\n" +
		": > \"" + started + "\"\n" +
		"echo 'intentional remux failure' >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(ffmpeg, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}

	file := &models.MediaFile{
		ID:                         42,
		ContentID:                  "movie-remux-indict",
		FilePath:                   "virtual://movie/movie-remux-indict?result=cand-A",
		VirtualOwnerInstallationID: 7,
		CodecVideo:                 "h264",
		VideoTracks:                []models.VideoTrack{{Codec: "h264"}},
	}
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", file.ID, playback.PlayRemux, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := sessionMgr.SetVirtualSource(session.ID, file.FilePath, file.VirtualOwnerInstallationID); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}

	handler := NewStreamHandler(sessionMgr, testPlaybackFileResolver{file: file})
	handler.RemoteStreamRelay = remotestream.NewRelay()
	defer func() { _ = handler.RemoteStreamRelay.Close(context.Background()) }()
	handler.AllowPrivateStreams = func(int) bool { return true }
	handler.PlaybackConfig = func() config.PlaybackConfig {
		return config.PlaybackConfig{FFmpegPath: ffmpeg}
	}

	var rotationFlags []bool
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(ctx context.Context, _ string, _ int, _ int, _ string, forceRefresh bool, excluded []string, _ string) (ResolvedVirtualMedia, error) {
		rotationFlags = append(rotationFlags, VirtualCandidateRotationAllowed(ctx))
		if forceRefresh && len(excluded) == 1 && excluded[0] == "cand-A" {
			return ResolvedVirtualMedia{
				URL:         "http://127.0.0.1:9/cand-b.mp4",
				URI:         "virtual://movie/movie-remux-indict?result=cand-B",
				CandidateID: "cand-B",
			}, nil
		}
		return ResolvedVirtualMedia{
			URL:         "http://127.0.0.1:9/cand-a.mp4",
			URI:         "virtual://movie/movie-remux-indict?result=cand-A",
			CandidateID: "cand-A",
		}, nil
	})

	rr := &multiWriteHeaderRecorder{ResponseRecorder: httptest.NewRecorder()}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID, nil)
	req = req.WithContext(newAuthorizedPlaybackContext())
	req = withPlaybackRouteParam(req, "session_id", session.ID)
	handler.HandleStream(rr, req)

	if !strings.Contains(rr.Body.String(), "sibling-bytes") {
		t.Fatalf("body = %q, want the sibling remux output", rr.Body.String())
	}
	servedSibling := false
	for _, status := range rr.statuses {
		if status == http.StatusOK {
			servedSibling = true
		}
	}
	if !servedSibling {
		t.Fatalf("write statuses = %#v, want a 200 for the sibling attempt after the first 502", rr.statuses)
	}
	if len(rotationFlags) < 2 {
		t.Fatalf("resolution calls = %d, want the initial resolve and the indicting retry", len(rotationFlags))
	}
	if rotationFlags[0] {
		t.Fatal("initial resolve declared rotation")
	}
	if !rotationFlags[1] {
		t.Fatalf("remux indicting retry did not declare rotation: %#v", rotationFlags)
	}
}

func TestHandleStream_VirtualDirectPlaySurvivesServerWriteTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", "3000")
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			_, _ = w.Write(make([]byte, 1000))
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(200 * time.Millisecond)
		}
	}))
	defer upstream.Close()

	file := &models.MediaFile{
		ID:                         42,
		ContentID:                  "movie-virtual-deadline",
		FilePath:                   "virtual://movie/movie-deadline?result=slow",
		VirtualOwnerInstallationID: 7,
	}
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := sessionMgr.SetVirtualSource(session.ID, file.FilePath, file.VirtualOwnerInstallationID); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}

	handler := NewStreamHandler(sessionMgr, testPlaybackFileResolver{file: file})
	handler.RemoteStreamRelay = remotestream.NewRelay()
	defer func() { _ = handler.RemoteStreamRelay.Close(context.Background()) }()
	handler.AllowPrivateStreams = func(int) bool { return true }
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		return ResolvedVirtualMedia{URL: upstream.URL + "/video.mp4"}, nil
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/stream/{session_id}", func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(newAuthorizedPlaybackContext())
		r = withPlaybackRouteParam(r, "session_id", session.ID)
		handler.HandleStream(w, r)
	})

	server := &http.Server{
		Handler:      mux,
		WriteTimeout: 400 * time.Millisecond,
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = listener.Close() }()
	go func() { _ = server.Serve(listener) }()
	defer func() { _ = server.Close() }()

	resp, err := http.Get("http://" + listener.Addr().String() + "/api/v1/stream/" + session.ID)
	if err != nil {
		t.Fatalf("Get stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll failed (deadline hit?): %v", err)
	}
	if len(body) != 3000 {
		t.Fatalf("body len = %d, want 3000", len(body))
	}
}

func TestHandleStream_VirtualDirectPlayInvalidTargetOrNonLoopbackReturnsStructured502(t *testing.T) {
	file := &models.MediaFile{
		ID:                         42,
		ContentID:                  "movie-virtual-ssrf",
		FilePath:                   "virtual://movie/movie-ssrf?result=ssrf",
		VirtualOwnerInstallationID: 7,
	}
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := sessionMgr.SetVirtualSource(session.ID, file.FilePath, file.VirtualOwnerInstallationID); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}

	handler := NewStreamHandler(sessionMgr, testPlaybackFileResolver{file: file})
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		return ResolvedVirtualMedia{
			URL: "http://non-loopback.example.com/video.mp4",
		}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID, nil)
	req = req.WithContext(newAuthorizedPlaybackContext())
	req = withPlaybackRouteParam(req, "session_id", session.ID)
	rr := httptest.NewRecorder()
	handler.HandleStream(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body = %s", rr.Code, rr.Body.String())
	}
	var errResp struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decode json response: %v, body = %s", err, rr.Body.String())
	}
	if errResp.Error != "virtual_stream_unavailable" {
		t.Fatalf("error code = %q, want virtual_stream_unavailable", errResp.Error)
	}
}

func TestBindSessionVirtualSourceUsesPinnedURIAndOwner(t *testing.T) {
	file := &models.MediaFile{ID: 42, FilePath: "virtual://movie/catalog?result=old", VirtualOwnerInstallationID: 7}
	session := &playback.Session{VirtualSourceURI: "virtual://movie/session?result=pinned", VirtualSourceOwnerInstallationID: 0}

	bound := bindSessionVirtualSource(file, session)
	if bound == file {
		t.Fatal("binding returned the catalog file instead of a copy")
	}
	if bound.FilePath != session.VirtualSourceURI || bound.VirtualOwnerInstallationID != 0 {
		t.Fatalf("bound source = (%q, owner=%d), want (%q, owner=0)", bound.FilePath, bound.VirtualOwnerInstallationID, session.VirtualSourceURI)
	}
	if file.FilePath != "virtual://movie/catalog?result=old" || file.VirtualOwnerInstallationID != 7 {
		t.Fatalf("catalog file mutated: %#v", file)
	}
}

func TestHandleStreamRejectsCommittedProxyEgressBeforeFileLookup(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			manager := playback.NewSessionManager(0, 0)
			manager.RegisterReconstructed(&playback.Session{
				ID: "proxy-stream", UserID: 1, MediaFileID: 42, PlayMethod: playback.PlayDirect,
				RoutingWorkload: string(noderouting.WorkloadDirectPlay), RoutingExecution: string(noderouting.ExecutionNone),
				RoutingEgress: string(noderouting.EgressProxy),
			})
			resolver := &countingStreamFileResolver{}
			handler := NewStreamHandler(manager, resolver)
			recorder := httptest.NewRecorder()
			handler.HandleStream(recorder, playbackTestRequest(method, "/api/v1/stream/proxy-stream", nil, map[string]string{"session_id": "proxy-stream"}))

			if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), `"error":"routing_policy_unsatisfied"`) {
				t.Fatalf("response = %d %s, want proxy-egress refusal", recorder.Code, recorder.Body.String())
			}
			if resolver.calls != 0 {
				t.Fatalf("file resolver calls = %d, want 0", resolver.calls)
			}
		})
	}
}

func TestHandleStreamRejectsProxyRecipeBeforeSessionReconstruction(t *testing.T) {
	const secret = "test-secret"
	card := playback.NewDirectRecipeCard("lost-proxy-stream", 1, "profile-1", 42)
	card.RoutingWorkload = string(noderouting.WorkloadDirectPlay)
	card.RoutingExecution = string(noderouting.ExecutionNone)
	card.RoutingEgress = string(noderouting.EgressProxy)
	token, err := streamtoken.Sign(card.ToClaims(), secret, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	manager := playback.NewSessionManager(0, 0)
	resolver := &countingStreamFileResolver{}
	handler := NewStreamHandler(manager, resolver)
	handler.JWTSecret = secret
	recorder := httptest.NewRecorder()
	handler.HandleStream(recorder, playbackTestRequest(
		http.MethodGet, "/api/v1/stream/lost-proxy-stream?st="+token, nil,
		map[string]string{"session_id": "lost-proxy-stream"},
	))

	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), `"error":"routing_policy_unsatisfied"`) {
		t.Fatalf("response = %d %s, want proxy-egress refusal", recorder.Code, recorder.Body.String())
	}
	if resolver.calls != 0 {
		t.Fatalf("file resolver calls = %d, want 0", resolver.calls)
	}
	if _, err := manager.GetSession(card.SessionID); !errors.Is(err, playback.ErrSessionNotFound) {
		t.Fatalf("proxy recipe reconstructed a native session: %v", err)
	}
}

func TestHandleStreamLiveAPIRouteOverridesStaleProxyRecipe(t *testing.T) {
	const secret = "test-secret"
	filePath := writePlaybackTestMediaFile(t, "movie.mp4")
	manager := playback.NewSessionManager(0, 0)
	manager.RegisterReconstructed(&playback.Session{
		ID: "replanned-stream", UserID: 1, MediaFileID: 42, PlayMethod: playback.PlayDirect,
		RoutingWorkload: string(noderouting.WorkloadDirectPlay), RoutingExecution: string(noderouting.ExecutionNone),
		RoutingEgress: string(noderouting.EgressAPI),
	})
	stale := playback.NewDirectRecipeCard("replanned-stream", 1, "profile-1", 42)
	stale.RoutingWorkload = string(noderouting.WorkloadDirectPlay)
	stale.RoutingExecution = string(noderouting.ExecutionNone)
	stale.RoutingEgress = string(noderouting.EgressProxy)
	token, err := streamtoken.Sign(stale.ToClaims(), secret, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	handler := NewStreamHandler(manager, testPlaybackFileResolver{file: &models.MediaFile{ID: 42, FilePath: filePath}})
	handler.JWTSecret = secret
	recorder := httptest.NewRecorder()
	handler.HandleStream(recorder, playbackTestRequest(
		http.MethodGet, "/api/v1/stream/replanned-stream?st="+token, nil,
		map[string]string{"session_id": "replanned-stream"},
	))

	if recorder.Code != http.StatusOK || recorder.Body.String() != "video" {
		t.Fatalf("response = %d %q, want live API route", recorder.Code, recorder.Body.String())
	}
}

func TestHandleStream_PausedSessionResumesWithDelayedRangeRequest(t *testing.T) {
	const (
		contentID       = "movie-1"
		sessionRouteKey = "session_id"
	)
	filePath := writePlaybackTestMediaFile(t, "movie.mp4")
	file := &models.MediaFile{
		ID:        42,
		ContentID: contentID,
		FilePath:  filePath,
		Duration:  3600,
	}
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", 42, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := sessionMgr.UpdateProgress(session.ID, 1, true); err != nil {
		t.Fatalf("UpdateProgress(paused): %v", err)
	}

	handler := NewStreamHandler(sessionMgr, testPlaybackFileResolver{file: file})
	request := func(rangeHeader, ifRange string) *httptest.ResponseRecorder {
		t.Helper()
		req := playbackTestRequest(
			http.MethodGet,
			"/api/v1/stream/"+session.ID,
			nil,
			map[string]string{sessionRouteKey: session.ID},
		)
		if rangeHeader != "" {
			req.Header.Set("Range", rangeHeader)
		}
		if ifRange != "" {
			req.Header.Set("If-Range", ifRange)
		}
		rr := httptest.NewRecorder()
		handler.HandleStream(rr, req)
		return rr
	}

	initial := request("", "")
	if initial.Code != http.StatusOK {
		t.Fatalf("initial status = %d, body = %s", initial.Code, initial.Body.String())
	}
	etag := initial.Header().Get("ETag")
	if etag == "" {
		t.Fatal("initial response omitted ETag")
	}

	const (
		activeGrace = 5 * time.Millisecond
		pausedGrace = 5 * time.Second
	)
	time.Sleep(20 * time.Millisecond)
	sessionMgr.CleanInactive(activeGrace, pausedGrace)
	if _, err := sessionMgr.GetSession(session.ID); err != nil {
		t.Fatalf("paused session expired before ranged resume: %v", err)
	}

	resumed := request("bytes=2-", etag)
	if resumed.Code != http.StatusPartialContent {
		t.Fatalf("resume status = %d, body = %s", resumed.Code, resumed.Body.String())
	}
	if got := resumed.Body.String(); got != "deo" {
		t.Fatalf("resume body = %q, want %q", got, "deo")
	}
	if live, err := sessionMgr.GetSession(session.ID); err != nil || live.ID != session.ID {
		t.Fatalf("ranged request did not preserve session %q: session=%#v err=%v", session.ID, live, err)
	}
}

func TestHandleStream_AbortsSessionWhenDirectPlayFileDisappearsAfterPreflight(t *testing.T) {
	filePath := writePlaybackTestMediaFile(t, "movie.mkv")
	file := &models.MediaFile{
		ID:        42,
		ContentID: "movie-1",
		FilePath:  filePath,
		Duration:  3600,
	}
	baseMgr := playback.NewSessionManager(0, 0)
	session, err := baseMgr.StartSession(1, "profile-1", 42, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	adminStore := &recordingPlaybackAdminStore{}
	syncer := &recordingSessionSyncer{}
	marker := &recordingMissingMarker{}
	sessionMgr := &hookedSessionManager{
		SessionManager: baseMgr,
		beginTransportHook: func() {
			_ = os.Remove(filePath)
		},
	}
	handler := NewStreamHandler(sessionMgr, testPlaybackFileResolver{file: file})
	handler.AdminStore = adminStore
	handler.SessionSyncer = syncer
	handler.MissingMarker = marker

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID, nil)
	req = req.WithContext(newAuthorizedPlaybackContext())
	req = withPlaybackRouteParam(req, "session_id", session.ID)

	rr := httptest.NewRecorder()
	handler.HandleStream(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if _, err := baseMgr.GetSession(session.ID); !errors.Is(err, playback.ErrSessionNotFound) {
		t.Fatalf("GetSession error = %v, want %v", err, playback.ErrSessionNotFound)
	}
	if len(marker.ids) != 1 || marker.ids[0] != 42 {
		t.Fatalf("marked ids = %v, want [42]", marker.ids)
	}
	if len(adminStore.deleted) != 1 || adminStore.deleted[0] != session.ID {
		t.Fatalf("deleted sessions = %v, want [%s]", adminStore.deleted, session.ID)
	}
	if len(adminStore.history) != 0 {
		t.Fatalf("history entries = %d, want 0", len(adminStore.history))
	}
	if syncer.calls == 0 {
		t.Fatal("expected session sync after abort")
	}
}

func TestHandleStream_KeepsSessionWhenLookupFailsForNonMissingReason(t *testing.T) {
	baseMgr := playback.NewSessionManager(0, 0)
	session, err := baseMgr.StartSession(1, "profile-1", 42, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	adminStore := &recordingPlaybackAdminStore{}
	syncer := &recordingSessionSyncer{}
	handler := NewStreamHandler(baseMgr, errStreamFileResolver{err: errors.New("db unavailable")})
	handler.AdminStore = adminStore
	handler.SessionSyncer = syncer

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID, nil)
	req = req.WithContext(newAuthorizedPlaybackContext())
	req = withPlaybackRouteParam(req, "session_id", session.ID)

	rr := httptest.NewRecorder()
	handler.HandleStream(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if _, err := baseMgr.GetSession(session.ID); err != nil {
		t.Fatalf("GetSession error = %v, want live session", err)
	}
	if len(adminStore.deleted) != 0 {
		t.Fatalf("deleted sessions = %v, want none", adminStore.deleted)
	}
	if syncer.calls != 0 {
		t.Fatalf("sync calls = %d, want 0", syncer.calls)
	}
}

// A v3 plan for an audio-only source promises audio/mp4, because a
// declared-tier client probes the advertised MIME with isTypeSupported before
// it will attach a source buffer and "video/mp4" for a stream carrying no video
// track is exactly the mismatch that makes that probe lie. The remux response
// has to keep the promise the plan made.
func TestHandleStream_AudioOnlyRemuxServesAudioContentType(t *testing.T) {
	for _, tc := range []struct {
		name        string
		file        *models.MediaFile
		wantContent string
	}{
		{
			name: "audio only source",
			file: &models.MediaFile{
				ID:         42,
				ContentID:  "audiobook-1",
				BaseType:   "audiobook",
				CodecAudio: "flac",
				Duration:   39600,
			},
			wantContent: playback.AudioOnlyRemuxMIMEV3,
		},
		{
			name: "video source",
			file: &models.MediaFile{
				ID:          42,
				ContentID:   "movie-1",
				CodecVideo:  "h264",
				CodecAudio:  "flac",
				VideoTracks: []models.VideoTrack{{Codec: "h264"}},
				Duration:    3600,
			},
			wantContent: "video/mp4",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := *tc.file
			file.FilePath = writePlaybackTestMediaFile(t, "source.mkv")

			sessionMgr := playback.NewSessionManager(0, 0)
			session, err := sessionMgr.StartSession(1, "profile-1", file.ID, playback.PlayRemux, true)
			if err != nil {
				t.Fatalf("StartSession: %v", err)
			}

			ffmpeg := filepath.Join(t.TempDir(), "ffmpeg")
			if err := os.WriteFile(ffmpeg, []byte("#!/bin/sh\nprintf muxed\n"), 0o755); err != nil {
				t.Fatalf("write fake ffmpeg: %v", err)
			}
			handler := NewStreamHandler(sessionMgr, testPlaybackFileResolver{file: &file})
			handler.PlaybackConfig = func() config.PlaybackConfig {
				return config.PlaybackConfig{FFmpegPath: ffmpeg}
			}

			req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID, nil)
			req = req.WithContext(newAuthorizedPlaybackContext())
			req = withPlaybackRouteParam(req, "session_id", session.ID)

			rr := httptest.NewRecorder()
			handler.HandleStream(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
			}
			if got := rr.Header().Get("Content-Type"); got != tc.wantContent {
				t.Fatalf("Content-Type = %q, want %q", got, tc.wantContent)
			}
		})
	}
}

// signalingStreamWriter reports the first response byte without buffering a
// stream that never ends on its own.
type signalingStreamWriter struct {
	header http.Header
	once   sync.Once
	wrote  chan struct{}
}

func newSignalingStreamWriter() *signalingStreamWriter {
	return &signalingStreamWriter{header: http.Header{}, wrote: make(chan struct{})}
}

func (w *signalingStreamWriter) Header() http.Header { return w.header }
func (w *signalingStreamWriter) WriteHeader(int)     {}

func (w *signalingStreamWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.wrote) })
	return len(p), nil
}

// TestHandleStream_RemuxAbortsWhenSessionStopped covers the API-host progressive
// remux: it is one long response whose FFmpeg belongs to the serving request, so
// StopSession (an admin kill, an idle reap, a copy-safety withdrawal) can only
// end it if the handler feeds the session's transport-stop watch into the remux
// abort. Before that wiring only the client's own disconnect could end it, and
// this test would hang to its timeout.
func TestHandleStream_RemuxAbortsWhenSessionStopped(t *testing.T) {
	source := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(source, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Capability probes answer at once; the remux invocation streams forever
	// until the abort kills FFmpeg.
	ffmpeg := filepath.Join(t.TempDir(), "ffmpeg")
	script := "#!/bin/sh\n" +
		"for arg in \"$@\"; do\n" +
		"  if [ \"$arg\" = \"pipe:1\" ]; then while :; do printf '0123456789'; sleep 0.01; done; fi\n" +
		"done\n" +
		"exit 0\n"
	if err := os.WriteFile(ffmpeg, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	file := &models.MediaFile{
		ID:          42,
		ContentID:   "movie-remux-stop",
		FilePath:    source,
		CodecVideo:  "h264",
		VideoTracks: []models.VideoTrack{{Codec: "h264"}},
	}
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", file.ID, playback.PlayRemux, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	handler := NewStreamHandler(sessionMgr, testPlaybackFileResolver{file: file})
	handler.PlaybackConfig = func() config.PlaybackConfig {
		return config.PlaybackConfig{FFmpegPath: ffmpeg}
	}

	writer := newSignalingStreamWriter()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID, nil)
	req = req.WithContext(newAuthorizedPlaybackContext())
	req = withPlaybackRouteParam(req, "session_id", session.ID)

	served := make(chan struct{})
	go func() {
		defer close(served)
		handler.HandleStream(writer, req)
	}()

	select {
	case <-writer.wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("the remux never delivered bytes")
	}

	if err := sessionMgr.StopSession(session.ID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}

	select {
	case <-served:
	case <-time.After(10 * time.Second):
		t.Fatal("StopSession did not end an in-flight API-host remux")
	}
}

// TestHandleStream_RemuxAbortsWhenRequestCanceled covers the other half of the
// API-host remux abort: the request context must end the response when the
// client goes away. A remux that outlived its request would keep encoding and
// downloading the source.
func TestHandleStream_RemuxAbortsWhenRequestCanceled(t *testing.T) {
	source := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(source, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}

	ffmpeg := filepath.Join(t.TempDir(), "ffmpeg")
	script := "#!/bin/sh\n" +
		"for arg in \"$@\"; do\n" +
		"  if [ \"$arg\" = \"pipe:1\" ]; then while :; do printf '0123456789'; sleep 0.01; done; fi\n" +
		"done\n" +
		"exit 0\n"
	if err := os.WriteFile(ffmpeg, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	file := &models.MediaFile{
		ID:          42,
		ContentID:   "movie-remux-cancel",
		FilePath:    source,
		CodecVideo:  "h264",
		VideoTracks: []models.VideoTrack{{Codec: "h264"}},
	}
	sessionMgr := playback.NewSessionManager(0, 0)
	session, err := sessionMgr.StartSession(1, "profile-1", file.ID, playback.PlayRemux, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	handler := NewStreamHandler(sessionMgr, testPlaybackFileResolver{file: file})
	handler.PlaybackConfig = func() config.PlaybackConfig {
		return config.PlaybackConfig{FFmpegPath: ffmpeg}
	}

	ctx, cancel := context.WithCancel(newAuthorizedPlaybackContext())
	writer := newSignalingStreamWriter()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID, nil).WithContext(ctx)
	req = withPlaybackRouteParam(req, "session_id", session.ID)

	served := make(chan struct{})
	go func() {
		defer close(served)
		handler.HandleStream(writer, req)
	}()

	select {
	case <-writer.wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("the remux never delivered bytes")
	}

	cancel()
	select {
	case <-served:
	case <-time.After(10 * time.Second):
		t.Fatal("request cancellation did not end an in-flight API-host remux")
	}
}

// TestHandleSubtitle_ListDownloadedSubtitlesErrorReturns500 pins the fix for
// issue #248: a failure listing downloaded subtitles must surface as a 500 with
// an "internal_error" code, not be swallowed and reported to the client as a
// generic "Subtitle track not found" 404 (which made a real backing-store
// failure look like an intermittent client-side subtitle bug).
func TestHandleSubtitle_ListDownloadedSubtitlesErrorReturns500(t *testing.T) {
	// No external or embedded tracks, so track index 0 falls through to the
	// downloaded-subtitle branch that queries the repository.
	file := &models.MediaFile{
		ID:        42,
		ContentID: "movie-1",
		FilePath:  "/tmp/movie.mkv",
		Duration:  3600,
	}
	baseMgr := playback.NewSessionManager(0, 0)
	session, err := baseMgr.StartSession(1, "profile-1", 42, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	handler := NewStreamHandler(baseMgr, testPlaybackFileResolver{file: file})
	handler.SubtitleRepo = &handlerMockSubtitleRepo{listErr: errors.New("db unavailable")}
	handler.SubtitleBlobs = newMockBlobStoreForHandler()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID+"/subtitles/0.vtt", nil)
	req = req.WithContext(newAuthorizedPlaybackContext())
	routeCtx := chi.NewRouteContext()
	routeCtx.URLParams.Add("session_id", session.ID)
	routeCtx.URLParams.Add("track", "0.vtt")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeCtx))

	rr := httptest.NewRecorder()
	handler.HandleSubtitle(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v (body = %s)", err, rr.Body.String())
	}
	if body.Error != "internal_error" {
		t.Fatalf("error code = %q, want %q (body = %s)", body.Error, "internal_error", rr.Body.String())
	}
}

func TestHandleSubtitleUsesBoundDownloadedIdentityAfterInventoryReorder(t *testing.T) {
	file := &models.MediaFile{ID: 42, ContentID: "movie-1", FilePath: "/tmp/movie.mkv", Duration: 3600}
	baseMgr := playback.NewSessionManager(0, 0)
	session, err := baseMgr.StartSession(1, "profile-1", 42, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	repo := newMockSubtitleRepoForHandler()
	repo.subtitles[71] = &subtitles.DownloadedSubtitle{ID: 71, MediaFileID: 42, Format: subtitles.FormatVTT, S3Key: "selected-71.vtt"}
	// The mutable ordinal now points at a different subtitle. An ID-bound URL
	// must still fetch 71 without consulting this reordered list.
	repo.list = []subtitles.DownloadedSubtitle{
		{ID: 72, MediaFileID: 42, Format: subtitles.FormatVTT, S3Key: "other-72.vtt"},
		{ID: 71, MediaFileID: 42, Format: subtitles.FormatVTT, S3Key: "selected-71.vtt"},
	}
	handler := NewStreamHandler(baseMgr, testPlaybackFileResolver{file: file})
	handler.SubtitleRepo = repo
	handler.SubtitleBlobs = subtitleContentBlobStore{objects: map[string][]byte{
		"selected-71.vtt": []byte("WEBVTT\n\n00:00.000 --> 00:01.000\nselected-71\n"),
		"other-72.vtt":    []byte("WEBVTT\n\n00:00.000 --> 00:01.000\nother-72\n"),
	}}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID+"/subtitles/0.vtt?file_id=42&downloaded_subtitle_id=71", nil)
	req = req.WithContext(newAuthorizedPlaybackContext())
	routeCtx := chi.NewRouteContext()
	routeCtx.URLParams.Add("session_id", session.ID)
	routeCtx.URLParams.Add("track", "0.vtt")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeCtx))

	rr := httptest.NewRecorder()
	handler.HandleSubtitle(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "selected-71") || strings.Contains(rr.Body.String(), "other-72") {
		t.Fatalf("status = %d, body = %q", rr.Code, rr.Body.String())
	}
}

// A downloaded SRT answers the published .srt?original=1 URL with its stored
// bytes on both GET and HEAD, whether the URL pins the row or uses the ordinal.
func TestHandleSubtitleServesDownloadedSRTOriginalOnRequest(t *testing.T) {
	const stored = "1\n00:00:01,000 --> 00:00:02,000\n{\\an8}Top\n"
	file := &models.MediaFile{ID: 42, ContentID: "movie-1", FilePath: "/tmp/movie.mkv", Duration: 3600}
	baseMgr := playback.NewSessionManager(0, 0)
	session, err := baseMgr.StartSession(1, "profile-1", 42, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	repo := newMockSubtitleRepoForHandler()
	row := subtitles.DownloadedSubtitle{ID: 71, MediaFileID: 42, Format: subtitles.FormatSRT, S3Key: "ai-71.srt"}
	repo.subtitles[71] = &row
	repo.list = []subtitles.DownloadedSubtitle{row}
	handler := NewStreamHandler(baseMgr, testPlaybackFileResolver{file: file})
	handler.SubtitleRepo = repo
	handler.SubtitleBlobs = subtitleContentBlobStore{objects: map[string][]byte{"ai-71.srt": []byte(stored)}}

	for _, query := range []string{"file_id=42&original=1&downloaded_subtitle_id=71", "file_id=42&original=1"} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			req := httptest.NewRequest(method, "/api/v2/stream/"+session.ID+"/subtitles/0.srt?"+query, nil)
			req = req.WithContext(WithNativeAPIV2(newAuthorizedPlaybackContext()))
			routeCtx := chi.NewRouteContext()
			routeCtx.URLParams.Add("session_id", session.ID)
			routeCtx.URLParams.Add("track", "0.srt")
			req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeCtx))
			rr := httptest.NewRecorder()
			handler.HandleSubtitle(rr, req)
			if rr.Code != http.StatusOK || !strings.HasPrefix(rr.Header().Get("Content-Type"), "application/x-subrip") ||
				(method == http.MethodGet && rr.Body.String() != stored) {
				t.Fatalf("%s ?%s = %d %q %q", method, query, rr.Code, rr.Header().Get("Content-Type"), rr.Body.String())
			}
		}
	}
}

// A downloaded SRT row with a stored timing correction is retimed before the
// WebVTT conversion, on the ID-bound and ordinal URLs, and the original SRT
// representation carries the same correction.
func TestHandleSubtitleAppliesDownloadedSubtitleTiming(t *testing.T) {
	const stored = "1\n00:00:01,000 --> 00:00:02,000\nHello\n"
	file := &models.MediaFile{ID: 42, ContentID: "movie-1", FilePath: "/tmp/movie.mkv", Duration: 3600}
	baseMgr := playback.NewSessionManager(0, 0)
	session, err := baseMgr.StartSession(1, "profile-1", 42, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	repo := newMockSubtitleRepoForHandler()
	row := subtitles.DownloadedSubtitle{ID: 71, MediaFileID: 42, Format: subtitles.FormatSRT, S3Key: "timed-71.srt",
		Timing: subtitles.Timing{OffsetMS: 2500, Scale: 1}}
	repo.subtitles[71] = &row
	repo.list = []subtitles.DownloadedSubtitle{row}
	handler := NewStreamHandler(baseMgr, testPlaybackFileResolver{file: file})
	handler.SubtitleRepo = repo
	handler.SubtitleBlobs = subtitleContentBlobStore{objects: map[string][]byte{"timed-71.srt": []byte(stored)}}

	serve := func(prefix, track, query string, native bool) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, prefix+session.ID+"/subtitles/"+track+"?"+query, nil)
		ctx := newAuthorizedPlaybackContext()
		if native {
			ctx = WithNativeAPIV2(ctx)
		}
		routeCtx := chi.NewRouteContext()
		routeCtx.URLParams.Add("session_id", session.ID)
		routeCtx.URLParams.Add("track", track)
		req = req.WithContext(context.WithValue(ctx, chi.RouteCtxKey, routeCtx))
		rr := httptest.NewRecorder()
		handler.HandleSubtitle(rr, req)
		return rr
	}
	for _, query := range []string{"file_id=42&downloaded_subtitle_id=71", "file_id=42"} {
		rr := serve("/api/v1/stream/", "0.vtt", query, false)
		body := rr.Body.String()
		if rr.Code != http.StatusOK || !strings.HasPrefix(body, "WEBVTT") || !strings.Contains(body, "00:00:03.500 --> 00:00:04.500") {
			t.Fatalf("?%s = %d %q", query, rr.Code, body)
		}
		if got := rr.Header().Get("Cache-Control"); got != "private, no-cache" {
			t.Fatalf("?%s Cache-Control = %q", query, got)
		}
	}
	rr := serve("/api/v2/stream/", "0.srt", "file_id=42&original=1&downloaded_subtitle_id=71", true)
	if want := "1\n00:00:03,500 --> 00:00:04,500\nHello\n"; rr.Code != http.StatusOK || rr.Body.String() != want {
		t.Fatalf("original SRT = %d %q, want %q", rr.Code, rr.Body.String(), want)
	}
}

type subtitleContentBlobStore struct {
	objects map[string][]byte
}

func (subtitleContentBlobStore) Put(context.Context, string, []byte) error { return nil }
func (c subtitleContentBlobStore) Get(_ context.Context, key string) ([]byte, error) {
	return append([]byte(nil), c.objects[key]...), nil
}
func (subtitleContentBlobStore) Delete(context.Context, string) error { return nil }

func TestHandleSubtitle_NilMediaFileReturns404(t *testing.T) {
	baseMgr := playback.NewSessionManager(0, 0)
	session, err := baseMgr.StartSession(1, "profile-1", 42, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	handler := NewStreamHandler(baseMgr, errStreamFileResolver{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID+"/subtitles/0.vtt", nil)
	req = req.WithContext(newAuthorizedPlaybackContext())
	routeCtx := chi.NewRouteContext()
	routeCtx.URLParams.Add("session_id", session.ID)
	routeCtx.URLParams.Add("track", "0.vtt")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeCtx))

	rr := httptest.NewRecorder()
	handler.HandleSubtitle(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s; want 404", rr.Code, rr.Body.String())
	}
}

// A .vtt request for a bitmap (PGS) embedded track must be rejected up front:
// PGS passes the burn-in guard because it is deliverable as .sup, but it has
// no text to convert, so forcing WebVTT would spawn an ffmpeg that always
// fails after the 200 and headers are committed.
func TestHandleSubtitle_BitmapTrackVTTRequestReturns415(t *testing.T) {
	file := &models.MediaFile{
		ID:        42,
		ContentID: "movie-1",
		FilePath:  "/tmp/movie.mkv",
		Duration:  3600,
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 0, Language: "eng", Codec: "hdmv_pgs_subtitle"},
		},
	}
	baseMgr := playback.NewSessionManager(0, 0)
	session, err := baseMgr.StartSession(1, "profile-1", 42, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	handler := NewStreamHandler(baseMgr, testPlaybackFileResolver{file: file})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID+"/subtitles/0.vtt", nil)
	req = req.WithContext(newAuthorizedPlaybackContext())
	routeCtx := chi.NewRouteContext()
	routeCtx.URLParams.Add("session_id", session.ID)
	routeCtx.URLParams.Add("track", "0.vtt")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeCtx))

	rr := httptest.NewRecorder()
	handler.HandleSubtitle(rr, req)

	if rr.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, body = %s; want 415", rr.Code, rr.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v (body = %s)", err, rr.Body.String())
	}
	if body.Error != "unsupported_media_type" {
		t.Fatalf("error code = %q, want %q", body.Error, "unsupported_media_type")
	}
}

func TestHandleSubtitle_ExternalTextTrackSRTRequestReturnsVTT(t *testing.T) {
	subtitlePath := filepath.Join(t.TempDir(), "movie.en.srt")
	if err := os.WriteFile(subtitlePath, []byte("1\n00:00:01,000 --> 00:00:02,000\nHello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	file := &models.MediaFile{
		ID:        42,
		ContentID: "movie-1",
		FilePath:  "/tmp/movie.mkv",
		Duration:  3600,
		ExternalSubtitles: []models.ExternalSubtitle{
			{Path: subtitlePath, Language: "eng", Format: "srt"},
		},
	}
	baseMgr := playback.NewSessionManager(0, 0)
	session, err := baseMgr.StartSession(1, "profile-1", 42, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	handler := NewStreamHandler(baseMgr, testPlaybackFileResolver{file: file})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID+"/subtitles/0.srt", nil)
	req = req.WithContext(newAuthorizedPlaybackContext())
	routeCtx := chi.NewRouteContext()
	routeCtx.URLParams.Add("session_id", session.ID)
	routeCtx.URLParams.Add("track", "0.srt")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeCtx))
	rr := httptest.NewRecorder()
	handler.HandleSubtitle(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, content-type = %q, body = %s; want 200", rr.Code, rr.Header().Get("Content-Type"), rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/vtt") {
		t.Fatalf("content type = %q, want WebVTT", got)
	}
	if body := rr.Body.String(); !strings.HasPrefix(body, "WEBVTT") || !strings.Contains(body, "Hello") {
		t.Fatalf("body = %q, want converted WebVTT", body)
	}
}

func TestHandleSubtitle_ExternalASSTrackASSRequestReturnsRawASS(t *testing.T) {
	const assBody = "[Script Info]\nTitle: Test\n"
	subtitlePath := filepath.Join(t.TempDir(), "movie.en.ass")
	if err := os.WriteFile(subtitlePath, []byte(assBody), 0o644); err != nil {
		t.Fatal(err)
	}
	file := &models.MediaFile{
		ID:                42,
		ContentID:         "movie-1",
		FilePath:          "/tmp/movie.mkv",
		Duration:          3600,
		ExternalSubtitles: []models.ExternalSubtitle{{Path: subtitlePath, Language: "eng", Format: "ass"}},
	}
	baseMgr := playback.NewSessionManager(0, 0)
	session, err := baseMgr.StartSession(1, "profile-1", 42, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	handler := NewStreamHandler(baseMgr, testPlaybackFileResolver{file: file})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID+"/subtitles/0.ass", nil)
	req = req.WithContext(newAuthorizedPlaybackContext())
	routeCtx := chi.NewRouteContext()
	routeCtx.URLParams.Add("session_id", session.ID)
	routeCtx.URLParams.Add("track", "0.ass")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeCtx))
	rr := httptest.NewRecorder()
	handler.HandleSubtitle(rr, req)
	if rr.Code != http.StatusOK || rr.Body.String() != assBody {
		t.Fatalf("status = %d, content-type = %q, body = %q; want raw ASS", rr.Code, rr.Header().Get("Content-Type"), rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/x-ssa") {
		t.Fatalf("content type = %q, want raw ASS", got)
	}
}

func TestHandleSubtitle_ExternalTextHEADDoesNotLoadTheArtifact(t *testing.T) {
	file := &models.MediaFile{
		ID:                42,
		ContentID:         "movie-1",
		FilePath:          "/missing/movie.mkv",
		Duration:          3600,
		ExternalSubtitles: []models.ExternalSubtitle{{Path: "/missing/movie.en.srt", Language: "eng", Format: "srt"}},
	}
	baseMgr := playback.NewSessionManager(0, 0)
	session, err := baseMgr.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	handler := NewStreamHandler(baseMgr, testPlaybackFileResolver{file: file})
	req := httptest.NewRequest(http.MethodHead, "/api/v1/stream/"+session.ID+"/subtitles/0.vtt", nil)
	req = req.WithContext(newAuthorizedPlaybackContext())
	routeCtx := chi.NewRouteContext()
	routeCtx.URLParams.Add("session_id", session.ID)
	routeCtx.URLParams.Add("track", "0.vtt")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeCtx))
	rr := httptest.NewRecorder()
	handler.HandleSubtitle(rr, req)

	if rr.Code != http.StatusOK || rr.Body.Len() != 0 || !strings.HasPrefix(rr.Header().Get("Content-Type"), "text/vtt") {
		t.Fatalf("HEAD status=%d type=%q body=%q", rr.Code, rr.Header().Get("Content-Type"), rr.Body.String())
	}
}

func TestHandleSubtitleAllowsAPIAuxiliaryResourceForProxyRoutedSession(t *testing.T) {
	file := &models.MediaFile{
		ID:                42,
		ContentID:         "movie-1",
		FilePath:          "/missing/movie.mkv",
		ExternalSubtitles: []models.ExternalSubtitle{{Path: "/missing/movie.en.srt", Language: "eng", Format: "srt"}},
	}
	manager := playback.NewSessionManager(0, 0)
	manager.RegisterReconstructed(&playback.Session{
		ID: "proxy-subtitle", UserID: 1, MediaFileID: file.ID, PlayMethod: playback.PlayDirect,
		RoutingWorkload: string(noderouting.WorkloadDirectPlay), RoutingExecution: string(noderouting.ExecutionNone),
		RoutingEgress: string(noderouting.EgressProxy),
	})
	handler := NewStreamHandler(manager, testPlaybackFileResolver{file: file})
	recorder := httptest.NewRecorder()
	handler.HandleSubtitle(recorder, playbackTestRequest(
		http.MethodHead,
		"/api/v1/stream/proxy-subtitle/subtitles/0.vtt",
		nil,
		map[string]string{"session_id": "proxy-subtitle", "track": "0.vtt"},
	))

	if recorder.Code != http.StatusOK || recorder.Body.Len() != 0 || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/vtt") {
		t.Fatalf("HEAD status=%d type=%q body=%q", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.String())
	}
}

func TestHandleSubtitle_EmbeddedPGSSupportsCachedHEADAndRange(t *testing.T) {
	file := &models.MediaFile{
		ID:        42,
		ContentID: "movie-1",
		FilePath:  writePlaybackTestMediaFile(t, "movie.mkv"),
		Duration:  3600,
		// The external track deliberately occupies combined ordinal 0. The PGS
		// container track is therefore addressed as ordinal 1, even though its
		// embedded subtitle-stream index is 0.
		ExternalSubtitles: []models.ExternalSubtitle{{Path: "/tmp/movie.en.srt", Format: "srt"}},
		SubtitleTracks:    []models.SubtitleTrack{{Index: 2, Language: "eng", Codec: "hdmv_pgs_subtitle"}},
	}
	baseMgr := playback.NewSessionManager(0, 0)
	session, err := baseMgr.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	cacheRoot := t.TempDir()
	cache := playback.NewSubtitleCache(func() string { return cacheRoot })
	warmReq := httptest.NewRequest(http.MethodGet, "/sub.sup", nil)
	warmRR := httptest.NewRecorder()
	if err := cache.ServeSUPExtract(warmRR, warmReq, playback.StreamExtractOpts{
		InputPath: file.FilePath, TrackIndex: 0, SourceCodec: "hdmv_pgs_subtitle",
	}, func(_ context.Context, opts playback.StreamExtractOpts) error {
		_, err := opts.Writer.Write([]byte("SUP PAYLOAD"))
		return err
	}); err != nil {
		t.Fatalf("warm PGS cache: %v", err)
	}

	handler := NewStreamHandler(baseMgr, testPlaybackFileResolver{file: file})
	handler.SubtitleCache = cache
	request := func(method, rangeHeader string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "/api/v1/stream/"+session.ID+"/subtitles/1.sup?file_id=42", nil)
		req = req.WithContext(newAuthorizedPlaybackContext())
		if rangeHeader != "" {
			req.Header.Set("Range", rangeHeader)
		}
		routeCtx := chi.NewRouteContext()
		routeCtx.URLParams.Add("session_id", session.ID)
		routeCtx.URLParams.Add("track", "1.sup")
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeCtx))
		rr := httptest.NewRecorder()
		handler.HandleSubtitle(rr, req)
		return rr
	}

	head := request(http.MethodHead, "")
	if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Type") != "application/octet-stream" || head.Header().Get("Content-Length") != "11" {
		t.Fatalf("HEAD status=%d type=%q length=%q body=%q", head.Code, head.Header().Get("Content-Type"), head.Header().Get("Content-Length"), head.Body.String())
	}
	ranged := request(http.MethodGet, "bytes=4-10")
	if ranged.Code != http.StatusPartialContent || ranged.Body.String() != "PAYLOAD" || ranged.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("Range status=%d type=%q body=%q", ranged.Code, ranged.Header().Get("Content-Type"), ranged.Body.String())
	}
}

func TestSubtitleSourceFileIDPinsURLAcrossEffectiveFileSwitch(t *testing.T) {
	session := &playback.Session{MediaFileID: 200, RequestedMediaFileID: 100}

	request := httptest.NewRequest(http.MethodGet, "/subtitles/4.vtt?file_id=100", nil)
	fileID, err := subtitleSourceFileID(request, session)
	if err != nil {
		t.Fatalf("subtitleSourceFileID: %v", err)
	}
	if fileID != 100 {
		t.Fatalf("fileID = %d, want original subtitle source 100", fileID)
	}

	request = httptest.NewRequest(http.MethodGet, "/subtitles/4.vtt?file_id=300", nil)
	if _, err := subtitleSourceFileID(request, session); err == nil {
		t.Fatal("expected unrelated subtitle source file to be rejected")
	}
}

func TestHandleTransportStartFailure_KeepsSessionForNonMissingError(t *testing.T) {
	filePath := writePlaybackTestMediaFile(t, "movie.mkv")
	file := &models.MediaFile{
		ID:        42,
		ContentID: "movie-1",
		FilePath:  filePath,
		Duration:  3600,
	}
	baseMgr := playback.NewSessionManager(0, 0)
	session, err := baseMgr.StartSession(1, "profile-1", 42, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	adminStore := &recordingPlaybackAdminStore{}
	syncer := &recordingSessionSyncer{}
	handler := NewStreamHandler(baseMgr, testPlaybackFileResolver{file: file})
	handler.AdminStore = adminStore
	handler.SessionSyncer = syncer

	handler.handleTransportStartFailure(context.Background(), session, file, errors.New("ffmpeg unavailable"))

	if _, err := baseMgr.GetSession(session.ID); err != nil {
		t.Fatalf("GetSession error = %v, want live session", err)
	}
	if len(adminStore.deleted) != 0 {
		t.Fatalf("deleted sessions = %v, want none", adminStore.deleted)
	}
	if syncer.calls != 0 {
		t.Fatalf("sync calls = %d, want 0", syncer.calls)
	}
}

type virtualInheritanceTestResolver struct {
	files  map[int]*models.MediaFile
	byPath map[string]*models.MediaFile
}

func (r virtualInheritanceTestResolver) GetByID(_ context.Context, id int) (*models.MediaFile, error) {
	if f, ok := r.files[id]; ok {
		return f, nil
	}
	return nil, errors.New("file not found")
}

func (r virtualInheritanceTestResolver) GetByPath(_ context.Context, p string) (*models.MediaFile, error) {
	if f, ok := r.byPath[p]; ok {
		return f, nil
	}
	return nil, errors.New("file not found")
}

func TestHandleSubtitle_VirtualPlaceholderInheritsCandidateTracks(t *testing.T) {
	placeholderFile := &models.MediaFile{
		ID:             100,
		ContentID:      "movie-virtual",
		FilePath:       "virtual://movie/movie-virtual",
		Duration:       3600,
		SubtitleTracks: nil,
	}
	candidateFile := &models.MediaFile{
		ID:        200,
		ContentID: "movie-virtual",
		FilePath:  "virtual://movie/movie-virtual?result=selected",
		Duration:  3600,
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 2, Language: "eng", Codec: "subrip"},
		},
	}

	resolver := virtualInheritanceTestResolver{
		files: map[int]*models.MediaFile{
			100: placeholderFile,
			200: candidateFile,
		},
		byPath: map[string]*models.MediaFile{
			candidateFile.FilePath: candidateFile,
		},
	}

	baseMgr := playback.NewSessionManager(0, 0)
	session, err := baseMgr.StartSession(1, "profile-1", 100, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := baseMgr.SetVirtualSource(session.ID, candidateFile.FilePath, 0); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	if err := baseMgr.SetEffectiveMediaFileID(session.ID, 200); err != nil {
		t.Fatalf("SetEffectiveMediaFileID: %v", err)
	}

	handler := NewStreamHandler(baseMgr, resolver)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/"+session.ID+"/subtitles/0.vtt?file_id=100", nil)
	req = req.WithContext(newAuthorizedPlaybackContext())
	routeCtx := chi.NewRouteContext()
	routeCtx.URLParams.Add("session_id", session.ID)
	routeCtx.URLParams.Add("track", "0.vtt")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeCtx))

	rr := httptest.NewRecorder()
	handler.HandleSubtitle(rr, req)

	// If tracks were not inherited, HandleSubtitle would 404 with "Embedded subtitle track not found".
	// Since candidate tracks are inherited, it progresses past track inventory lookup.
	if rr.Code == http.StatusNotFound && strings.Contains(rr.Body.String(), "Embedded subtitle track not found") {
		t.Fatalf("Candidate subtitle tracks were not inherited: %s", rr.Body.String())
	}
}

func TestSubtitleDefaultRequestReturnsWholeTrack(t *testing.T) {
	for _, query := range []string{"", "?file_id=42", "?duration=invalid", "?position=NaN", "?position=+Inf"} {
		req := httptest.NewRequest(http.MethodGet, "/subtitles/0.vtt"+query, nil)
		if got := subtitleSeekPosition(req); got != 0 {
			t.Errorf("query %q seek = %v, want full track from zero", query, got)
		}
		if got := subtitleWindowDuration(req); got != 0 {
			t.Errorf("query %q duration = %v, want complete track", query, got)
		}
	}
}

func TestSubtitleExplicitWindow(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/subtitles/0.vtt?position=1200&duration=600", nil)
	if got := subtitleSeekPosition(req); got != 1200 {
		t.Fatalf("seek = %v", got)
	}
	if got := subtitleWindowDuration(req); got != 600 {
		t.Fatalf("duration = %v", got)
	}
	req = httptest.NewRequest(http.MethodGet, "/subtitles/0.vtt?duration=600", nil)
	if got := subtitleSeekPosition(req); got != 0 {
		t.Fatalf("duration-only seek = %v, want zero", got)
	}
}

// ASS requests read the same explicit ?position/?duration window as WebVTT:
// the web client's JASSUB renderer asks for a slice as playback approaches
// the tail, and the server must window those requests instead of demuxing the
// complete track over the relay.
func TestSubtitleExtractWindowASSReadsExplicitWindow(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/subtitles/0.ass?position=120.5&duration=600", nil)
	allow, seek, duration := subtitleExtractWindow(req, subtitleFormatASS)
	if allow || seek != 120.5 || duration != 600 {
		t.Fatalf("ASS window = (%v, %v, %v), want (false, 120.5, 600)", allow, seek, duration)
	}

	// Absent params leave the whole-track defaults.
	req = httptest.NewRequest(http.MethodGet, "/subtitles/0.ass", nil)
	if allow, seek, duration := subtitleExtractWindow(req, subtitleFormatASS); allow || seek != 0 || duration != 0 {
		t.Fatalf("default ASS window = (%v, %v, %v), want zeros", allow, seek, duration)
	}
}

// PGS windowing still requires the explicit ?windowed=1 opt-in: the ASS path
// must not enable AllowWindow for PGS.
func TestSubtitleExtractWindowPGSStillRequiresOptIn(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/subtitles/0.sup?position=120&duration=600", nil)
	if allow, seek, duration := subtitleExtractWindow(req, subtitleFormatSUP); allow || seek != 0 || duration != 0 {
		t.Fatalf("PGS without windowed=1 = (%v, %v, %v), want zeros", allow, seek, duration)
	}
	req = httptest.NewRequest(http.MethodGet, "/subtitles/0.sup?windowed=1&position=120&duration=600", nil)
	allow, seek, duration := subtitleExtractWindow(req, subtitleFormatSUP)
	if !allow || seek != 120 || duration != 600 {
		t.Fatalf("PGS opt-in = (%v, %v, %v), want (true, 120, 600)", allow, seek, duration)
	}
}

// An explicit position parameter, even zero, requests a bounded window; an
// absent position (including a duration-only startup fetch) keeps the
// whole-track default. Without this distinction position=0&duration=600 was
// indistinguishable from a full fetch and re-demuxed the entire source.
func TestSubtitleWindowRequestedNeedsExplicitPosition(t *testing.T) {
	cases := []struct {
		query string
		want  bool
	}{
		{"", false},
		{"?duration=600", false},
		{"?position=0", true},
		{"?position=0&duration=600", true},
		{"?position=120&duration=600", true},
		{"?position=", false},
		{"?position=invalid", false},
		{"?position=-1", false},
		{"?position=NaN", false},
		{"?position=+Inf", false},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, "/subtitles/0.vtt"+tc.query, nil)
		if got := subtitleWindowRequested(req); got != tc.want {
			t.Errorf("query %q windowRequested = %v, want %v", tc.query, got, tc.want)
		}
	}
}

func TestEmbeddedSubtitleExtractionFailures(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		status       int
		interrupted  bool
	}{
		{"before_output", "exit 1", http.StatusInternalServerError, false},
		{"after_output", "printf 'WEBVTT\\n\\n00:00:01.000 --> 00:00:02.000\\nPartial\\n\\n'; exit 1", http.StatusOK, true},
		{"complete", "printf 'WEBVTT\\n\\n00:20:01.000 --> 00:20:02.000\\nComplete\\n\\n'", http.StatusOK, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ffmpeg := filepath.Join(t.TempDir(), "ffmpeg")
			if err := os.WriteFile(ffmpeg, []byte("#!/bin/sh\n"+tc.script+"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			handler := NewStreamHandler(nil, nil)
			handler.PlaybackConfig = func() config.PlaybackConfig { return config.PlaybackConfig{FFmpegPath: ffmpeg} }
			file := &models.MediaFile{ID: 42, FilePath: "/synthetic/media.mkv", SubtitleTracks: []models.SubtitleTrack{{Codec: "subrip"}}}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handler.streamEmbeddedSubtitle(w, r, file, 0, nil, false, "vtt")
			}))
			defer server.Close()
			response, err := server.Client().Get(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			_, err = io.ReadAll(response.Body)
			if response.StatusCode != tc.status {
				t.Fatalf("status=%d, want %d", response.StatusCode, tc.status)
			}
			if tc.interrupted && err == nil {
				t.Fatal("failed extraction ended with successful EOF")
			}
			if !tc.interrupted && err != nil {
				t.Fatal(err)
			}
		})
	}
}

// writeFakeFFprobe installs an executable named "ffprobe" that emits the given
// stdout and exit status, so ProbeSubtitleLayout uses it verbatim (the
// resolver treats a path whose basename contains "ffprobe" as the probe
// binary itself).
func writeFakeFFprobe(t *testing.T, script string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "ffprobe")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin
}

func virtualLayoutHandler(ffprobePath string) *StreamHandler {
	handler := NewStreamHandler(nil, nil)
	handler.PlaybackConfig = func() config.PlaybackConfig {
		return config.PlaybackConfig{FFmpegPath: ffprobePath}
	}
	return handler
}

// A drift probe that fails to run (context canceled, relay timeout) cannot
// establish the live layout, so verifyVirtualSubtitleLayout reports the failure
// alongside "proceed" and every caller fails closed: serving the plan ordinal
// unverified can emit the wrong release's track, and a same-ordinal
// different-language rotation never trips the post-spawn map net. A positive
// mismatch is the only signal that warrants a replan.
func TestVerifyVirtualSubtitleLayoutProbeFailureFailsClosed(t *testing.T) {
	for _, codec := range []string{"ass", "hdmv_pgs_subtitle"} {
		t.Run(codec, func(t *testing.T) {
			handler := virtualLayoutHandler(writeFakeFFprobe(t, "exit 1"))
			opts := &playback.StreamExtractOpts{
				InputPath:   "/relay/pinned/stream.mkv",
				TrackIndex:  3,
				SourceCodec: codec,
			}
			session := &playback.Session{
				ID:                    "sess",
				VirtualSubtitleTracks: []models.SubtitleTrack{{Index: 3, Codec: codec}},
			}
			requested := models.SubtitleTrack{Index: 3, Codec: codec}

			proceed, probeErr := handler.verifyVirtualSubtitleLayout(t.Context(), requested, session.VirtualSubtitleTracks, opts)
			if !proceed {
				t.Fatal("a probe failure is not a positive mismatch; proceed is reported so the caller can fail closed")
			}
			if probeErr == nil {
				t.Fatal("probe failure must be reported so the caller fails closed")
			}
			if opts.TrackIndex != 3 || opts.SourceCodec != codec {
				t.Fatalf("plan ordinal/codec must be preserved: %+v", opts)
			}
		})
	}
}

// A probe that succeeds and positively reports a different layout must keep
// the existing 409 behavior.
func TestVerifyVirtualSubtitleLayoutPositiveMismatchForcesReplan(t *testing.T) {
	probe := `printf '%s' '{"streams":[{"id":"1","index":0,"codec_name":"subrip","codec_type":"subtitle","tags":{"language":"eng"}}]}'`
	handler := virtualLayoutHandler(writeFakeFFprobe(t, probe))
	opts := &playback.StreamExtractOpts{
		InputPath:   "/relay/pinned/stream.mkv",
		TrackIndex:  0,
		SourceCodec: "ass",
	}
	session := &playback.Session{
		ID:                    "sess",
		VirtualSubtitleTracks: []models.SubtitleTrack{{Index: 0, Codec: "ass"}},
	}
	requested := models.SubtitleTrack{Index: 0, Codec: "ass", Language: "eng"}

	proceed, probeErr := handler.verifyVirtualSubtitleLayout(t.Context(), requested, session.VirtualSubtitleTracks, opts)
	if proceed || probeErr != nil {
		t.Fatal("a positively different live layout must force a 409 replan")
	}
}

// deliverySeamRecorder captures the optional v1 code/cause seam the v2 delivery
// adapter implements.
type deliverySeamRecorder struct {
	http.ResponseWriter
	code  string
	cause error
}

func (s *deliverySeamRecorder) SetPlaybackProblemCode(code string) { s.code = code }
func (s *deliverySeamRecorder) SetPlaybackProblemCause(err error)  { s.cause = err }

// TestWriteErrorCauseReachesDeliverySeamThroughStreamingWrapper pins that a v1
// stream handler writing through httpstream.RollingDeadlineWriter still reaches
// the v2 delivery adapter's code/cause seam: the wrapper exposes only Unwrap,
// so writeErrorCause must walk it. Without the walk the 502 reaches the request
// log as the bare status and the underlying stream failure is lost. The
// response body stays the generic message.
func TestWriteErrorCauseReachesDeliverySeamThroughStreamingWrapper(t *testing.T) {
	rec := httptest.NewRecorder()
	seam := &deliverySeamRecorder{ResponseWriter: rec}
	wrapped := httpstream.NewRollingDeadlineWriter(seam)
	cause := errors.New("provider stream dial failed")
	writeErrorCause(wrapped, http.StatusBadGateway, "virtual_stream_unavailable", "Failed to stream virtual media source", cause)

	if seam.code != "virtual_stream_unavailable" {
		t.Fatalf("adapter code = %q, want virtual_stream_unavailable", seam.code)
	}
	if !errors.Is(seam.cause, cause) {
		t.Fatalf("adapter cause = %v, want %v", seam.cause, cause)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Failed to stream virtual media source") || strings.Contains(rec.Body.String(), cause.Error()) {
		t.Fatalf("response body = %s", rec.Body.String())
	}
}
