package handlers

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

// captureHandlerLogs routes the package logger into a buffer for the duration
// of one test so a fast-path skip line can be asserted.
func captureHandlerLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs
}

// TestCompleteVirtualVideoEvidenceRequiresPlannerFields pins the alignment with
// the planner's routeVideoMetadataCompleteV3: codec, bit depth, dimensions, a
// frame rate that parses above zero, and a positive bitrate are all required.
// A stored string like "0" is a real, present but unusable frame rate and must
// be rejected, not treated as missing-and-benign.
func TestCompleteVirtualVideoEvidenceRequiresPlannerFields(t *testing.T) {
	base := func() *models.MediaFile {
		return &models.MediaFile{
			CodecVideo: "h264",
			Resolution: "1080p",
			Bitrate:    10_000,
			VideoTracks: []models.VideoTrack{{
				Codec: "h264", Width: 1920, Height: 1080, FrameRate: "30000/1001", BitDepth: 8, Bitrate: 10_000,
			}},
		}
	}
	if !completeVirtualVideoEvidenceV3(base()) {
		t.Fatal("a planner-complete row was rejected")
	}

	cases := []struct {
		name   string
		mutate func(*models.MediaFile)
	}{
		{"missing bit depth", func(f *models.MediaFile) { f.VideoTracks[0].BitDepth = 0 }},
		{"zero bitrate", func(f *models.MediaFile) {
			f.VideoTracks[0].Bitrate = 0
			f.Bitrate = 0
		}},
		{"zero frame rate", func(f *models.MediaFile) { f.VideoTracks[0].FrameRate = "0" }},
		{"zero-rational frame rate", func(f *models.MediaFile) { f.VideoTracks[0].FrameRate = "0/1" }},
		{"missing video codec", func(f *models.MediaFile) {
			f.CodecVideo = ""
			f.VideoTracks[0].Codec = ""
		}},
		{"missing dimensions", func(f *models.MediaFile) {
			f.VideoTracks[0].Width = 0
			f.VideoTracks[0].Height = 0
			f.Resolution = ""
		}},
		{"empty tracks", func(f *models.MediaFile) { f.VideoTracks = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file := base()
			tc.mutate(file)
			if completeVirtualVideoEvidenceV3(file) {
				t.Fatalf("row reported planner-complete with %s: %#v", tc.name, file)
			}
		})
	}
}

// TestResolveVirtualResumeIncompleteMetadataFallsThroughToProbe is the incident
// regression: a listing-written row that owns a stored URL but no probed track
// inventory must not take the durable-resume fast path. The resolve+probe runs,
// the row is filled in, and the skip is logged with the planner's gap wording.
func TestResolveVirtualResumeIncompleteMetadataFallsThroughToProbe(t *testing.T) {
	expiresAt := time.Now().Add(3 * time.Hour)
	file := virtualResumeRow("https://93.184.216.34/stream/token=stored", &expiresAt)
	if completeVirtualVideoEvidenceV3(file) {
		t.Fatal("fixture precondition: the row must lack planner-grade video evidence")
	}
	logs := captureHandlerLogs(t)

	probed := make(chan struct{}, 1)
	listerCalls, detailedCalls := 0, 0
	h := virtualResumeHandler(&listerCalls, &detailedCalls, nil)
	h.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		f.CodecVideo, f.Resolution, f.Bitrate = "h264", "1080p", 10_000
		f.VideoTracks = []models.VideoTrack{{
			Codec: "h264", Width: 1920, Height: 1080, FrameRate: "24000/1001", BitDepth: 8, Bitrate: 10_000,
		}}
		select {
		case probed <- struct{}{}:
		default:
		}
		return f, nil
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if detailedCalls != 1 {
		t.Fatalf("detailed resolver called %d times, want 1: the unprobed row must resolve rather than resume blind", detailedCalls)
	}
	// The deferral guard routes unplannable merged metadata to the bounded
	// synchronous cache-first probe: the row is filled in before the planner
	// sees it, so this resolve returns the probe's verified inventory rather
	// than a pending deferred plan the planner would terminal. The probe ran
	// synchronously on the start path.
	if resolved.Provenance != ProbeProvenanceVerified || !resolved.ProbeSucceeded {
		t.Fatalf("provenance=%q succeeded=%v, want verified/true after the guard's sync probe", resolved.Provenance, resolved.ProbeSucceeded)
	}
	select {
	case <-probed:
	case <-time.After(5 * time.Second):
		t.Fatal("the resolve+probe chain never probed the candidate after the fast path was skipped")
	}

	logged := logs.String()
	if !strings.Contains(logged, "fast_path_skipped_incomplete_metadata") {
		t.Fatalf("fast-path skip was not logged: %s", logged)
	}
	for _, want := range []string{`"fast_path":"durable_resume"`, `"file_id":731`, "bit depth", "frame rate", "bitrate"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("skip log missing %q:\n%s", want, logged)
		}
	}
}

// TestResolveVirtualOptimisticIncompleteMetadataFallsThrough proves the
// delivery-grace fast path has the same precondition as durable resume: recent
// delivery alone is not planner-grade evidence, so an unprobed row resolves
// instead of starting blind.
func TestResolveVirtualOptimisticIncompleteMetadataFallsThrough(t *testing.T) {
	deliveredAt := time.Now().Add(-time.Hour)
	file := &models.MediaFile{
		ID:                         902,
		ContentID:                  "movie-optimistic-incomplete",
		FilePath:                   "virtual://movie/tt-optimistic-incomplete?result=cand-1",
		Container:                  "virtual",
		LastDeliveredAt:            &deliveredAt,
		VirtualOwnerInstallationID: 5,
	}
	if completeVirtualVideoEvidenceV3(file) {
		t.Fatal("fixture precondition: the row must lack planner-grade video evidence")
	}
	logs := captureHandlerLogs(t)

	var detailedCalls int32
	h := &PlaybackHandler{
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(
			func(_ context.Context, virtualURI string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
				atomic.AddInt32(&detailedCalls, 1)
				return ResolvedVirtualMedia{URL: "http://provider.example/stream.mkv", URI: virtualURI}, nil
			}),
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(
			func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
				return "http://provider.example/legacy?path=" + path, nil
			}),
		VirtualPlaybackSourceProber: func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
			f.VideoTracks = []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080}}
			f.AudioTracks = []models.AudioTrack{{Codec: "aac", Channels: 2, Language: "eng"}}
			f.CodecVideo, f.CodecAudio, f.Resolution, f.Container = "h264", "aac", "1080p", "mkv"
			return f, nil
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	if _, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false); err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if got := atomic.LoadInt32(&detailedCalls); got != 1 {
		t.Fatalf("detailed resolver called %d times, want 1: an unprobed row must not start optimistically", got)
	}
	logged := logs.String()
	if !strings.Contains(logged, "fast_path_skipped_incomplete_metadata") || !strings.Contains(logged, `"fast_path":"delivery_grace"`) {
		t.Fatalf("delivery-grace skip was not logged correctly:\n%s", logged)
	}
}

// TestResolveVirtualResumeRotationClearedRowHasNoStaleURL simulates the row
// ReplaceVirtualResultPin leaves behind: the pin moved to a new candidate and
// the stored URL was cleared with the rest of the old candidate's evidence.
// There is no stored URL to serve, so neither the durable-resume shortcut nor a
// stale-URL serve can apply; the resolve path runs.
func TestResolveVirtualResumeRotationClearedRowHasNoStaleURL(t *testing.T) {
	file := withVirtualResumeVideoEvidence(virtualResumeRow("", nil))

	if _, state := evaluateStoredVirtualURLCandidate(context.Background(), file.FilePath, file, true, time.Now(), 0); state != virtualStoredURLMissing {
		t.Fatalf("stored URL state = %v, want missing after a rotation cleared resolved_url", state)
	}

	listed := []VirtualPlaybackStream{{
		ID: "cand-resume", URI: virtualResumeCandidate,
		Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
	}}
	listerCalls, detailedCalls := 0, 0
	h := virtualResumeHandler(&listerCalls, &detailedCalls, listed)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	if _, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false); err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if detailedCalls != 1 {
		t.Fatalf("detailed resolver called %d times, want 1: a cleared stored URL must not resume from a stale candidate", detailedCalls)
	}
	if listerCalls != 1 {
		t.Fatalf("provider lister called %d times, want 1: the row has no stored URL to serve", listerCalls)
	}
}
