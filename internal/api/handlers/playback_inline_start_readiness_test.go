package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// captureStartupTimingLog installs a JSON slog handler for one test and returns
// the buffer plus the startup-timing record it collected. The start path logs
// one line under that message; the fields on it are the observability contract
// this change adds to.
func captureStartupTimingLog(t *testing.T) (*bytes.Buffer, func() map[string]any) {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	read := func() map[string]any {
		t.Helper()
		for _, line := range bytes.Split(logs.Bytes(), []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			var record map[string]any
			if err := json.Unmarshal(line, &record); err != nil {
				t.Fatalf("decode startup log %q: %v", line, err)
			}
			if record["msg"] == "playback transport startup timing" && record["transport"] == "local" {
				return record
			}
		}
		t.Fatalf("playback transport startup timing line missing; logs:\n%s", logs.String())
		return nil
	}
	return &logs, read
}

func inlineStartFixture(t *testing.T, handler *PlaybackHandler) (*models.MediaFile, playback.PlannerResultV3, *playback.Session) {
	t.Helper()
	canonical := "virtual://movie/tt-inline-start?result=pinned"
	file := v3HandlerFixtureFile(t)
	file.ID = 42
	file.ContentID = "movie-inline-start"
	file.FilePath = canonical
	file.VirtualOwnerInstallationID = 5
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
		return ResolvedVirtualMedia{URL: "http://127.0.0.1:9/source.mp4", URI: canonical, CandidateID: "pinned"}, nil
	})
	result := playback.PlannerResultV3{
		Plan: &playback.PlanV3{
			PlanID:   "plan:inline-start",
			Delivery: playback.DeliveryTranscodeHLSV3,
		},
		PlayMethod:       playback.PlayTranscode,
		TargetVideoCodec: "h264", TargetAudioCodec: "aac", TargetResolution: "1080p",
		SubtitleTrackIndex: -1, SubtitleTransportTrackIndex: -1,
	}
	session := &playback.Session{ID: "session-inline-start", UserID: 7, ProfileID: "profile-1", MediaFileID: file.ID}
	return file, result, session
}

func newInlineStartRequest() *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	ctx, _ := withTransportPhaseTimingsV3(req.Context())
	return req.WithContext(ctx)
}

func inlineStartHandler(t *testing.T) *PlaybackHandler {
	t.Helper()
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	handler.PlaybackConfig = func() config.PlaybackConfig {
		return config.PlaybackConfig{
			TranscodeDir:     t.TempDir(),
			TranscodeEnabled: true,
			HWAccel:          playback.HWAccelNone,
		}
	}
	return handler
}

// TestInlineFirstManifestTimeoutIsRetryable pins the measured production shape:
// a virtual transport's inline wait for its first manifest runs out of the
// start budget before the provider answers a byte. The failure must be reported
// as a readiness timeout with the wait under manifest_wait_ms (not spawn_ms),
// and the start must carry a retryable terminal instead of a non-retryable
// transcode_start_failed.
func TestInlineFirstManifestTimeoutIsRetryable(t *testing.T) {
	previousBudget := virtualStartReadinessBudget
	virtualStartReadinessBudget = 120 * time.Millisecond
	t.Cleanup(func() { virtualStartReadinessBudget = previousBudget })

	_, readLog := captureStartupTimingLog(t)
	handler := inlineStartHandler(t)
	file, result, session := inlineStartFixture(t, handler)
	handler.StartTranscodeFunc = func(_ context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
		if err := os.MkdirAll(opts.OutputDir, 0o755); err != nil {
			return nil, err
		}
		ts, err := playback.NewReadyTranscodeSessionForTesting(opts.OutputDir, opts)
		if err != nil {
			return nil, err
		}
		return ts, os.Remove(filepath.Join(opts.OutputDir, "stream.m3u8"))
	}

	_, transportErr := handler.prepareLocalTransportV3(
		newInlineStartRequest(), session, file, result, preparedTimelineV3{}, mediaAuthModeV3{})
	if transportErr == nil || !transportErr.retryable || transportErr.reason != transcodeStartFailedReasonV3 {
		t.Fatalf("transport error = %#v, want retryable %s readiness timeout", transportErr, transcodeStartFailedReasonV3)
	}

	record := readLog()
	if got := record["outcome"]; got != "readiness_failed" {
		t.Fatalf("startup outcome = %v, want readiness_failed", got)
	}
	manifestWait, ok := record["manifest_wait_ms"].(float64)
	if !ok || manifestWait < 80 {
		t.Fatalf("manifest_wait_ms = %v, want the inline wait (~120ms)", record["manifest_wait_ms"])
	}
	spawn, ok := record["spawn_ms"].(float64)
	if !ok || spawn >= manifestWait {
		t.Fatalf("spawn_ms = %v, manifest_wait_ms = %v; the inline wait must not be charged to spawn", record["spawn_ms"], manifestWait)
	}
	if got, ok := record["readiness_budget_ms"].(float64); !ok || got <= 0 {
		t.Fatalf("readiness_budget_ms = %v, want the start budget", record["readiness_budget_ms"])
	}
}

// TestInlineEncoderFailureStaysNonRetryable keeps the terminal half of the
// classification: an FFmpeg process that exits with an encoder/device error
// before its first manifest is a real launch failure, not a provider read that
// merely took too long.
func TestInlineEncoderFailureStaysNonRetryable(t *testing.T) {
	_, readLog := captureStartupTimingLog(t)
	handler := inlineStartHandler(t)
	file, result, session := inlineStartFixture(t, handler)
	handler.StartTranscodeFunc = func(context.Context, playback.TranscodeOpts) (*playback.TranscodeSession, error) {
		return nil, fmt.Errorf("%w: exit status 1 (stderr: encoder device lost)", playback.ErrTranscodeFailed)
	}

	_, transportErr := handler.prepareLocalTransportV3(
		newInlineStartRequest(), session, file, result, preparedTimelineV3{}, mediaAuthModeV3{})
	if transportErr == nil || transportErr.retryable || transportErr.reason != transcodeStartFailedReasonV3 {
		t.Fatalf("transport error = %#v, want terminal %s", transportErr, transcodeStartFailedReasonV3)
	}
	if got := readLog()["outcome"]; got != "spawn_failed" {
		t.Fatalf("startup outcome = %v, want spawn_failed for an encoder exit", got)
	}
}

// TestSuccessfulStartKeepsStartupTimingFields pins that the successful start
// still reports spawn, first-manifest wait, and readiness budget on the same
// line the failure cases use.
func TestSuccessfulStartKeepsStartupTimingFields(t *testing.T) {
	_, readLog := captureStartupTimingLog(t)
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	tempDir := t.TempDir()
	handler.StartTranscodeFunc = func(_ context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
		return playback.NewReadyTranscodeSessionForTesting(tempDir, opts)
	}
	ts, failure := handler.startReadyLocalPlaybackTransportV3(context.Background(), playback.TranscodeOpts{
		InputPath:          "/media/movie.mkv",
		OutputDir:          tempDir,
		SessionID:          "successful-start-timing",
		TargetCodecVideo:   "h264",
		TargetCodecAudio:   "aac",
		SegmentDuration:    2,
		HWAccel:            playback.HWAccelNone,
		AudioTrackIndex:    -1,
		SubtitleTrackIndex: -1,
	})
	if failure != nil {
		t.Fatalf("startup failed: %+v", failure)
	}
	defer func() { _ = ts.Close() }()

	record := readLog()
	if got := record["outcome"]; got != transportStartupReadyV3 {
		t.Fatalf("startup outcome = %v, want %s", got, transportStartupReadyV3)
	}
	for _, field := range []string{"spawn_ms", "manifest_wait_ms", "readiness_budget_ms"} {
		if _, ok := record[field]; !ok {
			t.Fatalf("successful startup line missing %s: %v", field, record)
		}
	}
}
