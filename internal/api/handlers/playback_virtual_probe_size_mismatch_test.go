package handlers

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

// captureVirtualProbeLogs installs a JSON slog handler for the duration of the
// test and returns the buffer it writes to.
func captureVirtualProbeLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	logs := &bytes.Buffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return logs
}

// A probe can read a perfectly valid container whose bytes are a different
// release than the row declares: the provider's cache served a stale fill.
// The persistence path must reject it by size before the duration guard runs,
// because the observed failure shares the item's real runtime across releases.
// The release is not dead, so the row keeps its failure state, the short
// wrong-source damper is used instead of the dead-candidate damper, and the pin
// is released so the next start re-ranks.
func TestBackgroundProbeRejectsServedSizeMismatch(t *testing.T) {
	const (
		declaredBytes = int64(8_449_999_999)
		servedBytes   = int64(7_427_636_945) // 12% short
	)
	uri := "virtual://movie/tt-size-mismatch?result=cand-1"
	key := virtualProbeFailureKey(uri, 5)
	virtualProbeFailures.clear(key)
	virtualProbeFailures.clearSizeMismatch(key)
	t.Cleanup(func() {
		virtualProbeFailures.clear(key)
		virtualProbeFailures.clearSizeMismatch(key)
	})
	logs := captureVirtualProbeLogs(t)

	persisted := false
	failStamped := false
	h := &PlaybackHandler{
		VirtualPlaybackSourceProber: func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
			f.VideoTracks = []models.VideoTrack{{Codec: "hevc", Width: 3840, Height: 2160}}
			f.AudioTracks = []models.AudioTrack{{Codec: "eac3", Channels: 6, Language: "eng"}}
			f.CodecVideo, f.CodecAudio, f.Resolution, f.Container = "hevc", "eac3", "2160p", "mkv"
			f.ProbedFormatSize = servedBytes
			// The same runtime as the real release: the duration guard would
			// accept this probe, so only the size gate can catch it.
			f.Duration = 3072
			return f, nil
		},
		VirtualFileSaver: func(context.Context, models.VirtualFilePersistArgs) (int64, error) {
			persisted = true
			return 1, nil
		},
		VirtualCandidateFailMarker: func(context.Context, int, string, *time.Time) error {
			failStamped = true
			return nil
		},
	}
	stickyKey := "sticky-size-mismatch"
	h.pinVirtualSticky(stickyKey, uri)
	file := &models.MediaFile{
		ID:                         601,
		ContentID:                  "movie-tt",
		FilePath:                   uri,
		FileSize:                   declaredBytes,
		VirtualOwnerInstallationID: 5,
	}
	cand := VirtualPlaybackStream{ID: "cand-1", URI: uri}

	outcome, committed := h.probeVirtualSourceAndPersistWith(
		context.Background(), stickyKey, file, "http://provider.example/stream", *file, cand,
		noCandidateIdentity, 51, 5, true, nil,
	)
	if outcome != probeOutcomeFailed {
		t.Fatalf("outcome = %q, want failed", outcome)
	}
	if len(committed.audio) != 0 || len(committed.subtitles) != 0 {
		t.Fatal("a rejected served-size mismatch must not carry a committed inventory")
	}
	if persisted {
		t.Fatal("a served-size mismatch must not persist probe evidence")
	}
	if failStamped {
		t.Fatal("a served-size mismatch must not stamp failed_at: the release is not dead")
	}
	if file.FailedAt != nil {
		t.Fatal("a served-size mismatch wrote a failed_at verdict onto the row")
	}
	if got := h.peekVirtualSticky(stickyKey); got != "" {
		t.Fatalf("served-size mismatch kept the sticky pin: %q", got)
	}
	if virtualProbeFailures.recent(key) {
		t.Fatal("served-size mismatch fed the dead-candidate failure damper")
	}
	if !virtualProbeFailures.recent(virtualProbeSizeMismatchKey(key)) {
		t.Fatal("served-size mismatch did not arm the short wrong-source damper")
	}

	logged := logs.String()
	if !strings.Contains(logged, "background virtual probe rejected: served size mismatch") {
		t.Fatalf("missing distinct size-mismatch warning:\n%s", logged)
	}
	if strings.Contains(logged, "probed duration implausible") {
		t.Fatalf("size mismatch reached the duration guard instead of being rejected first:\n%s", logged)
	}
	for _, attr := range []string{`"file_id":601`, `"candidate_uri":"` + uri + `"`, `"served_bytes":7427636945`, `"declared_bytes":8449999999`} {
		if !strings.Contains(logged, attr) {
			t.Fatalf("size-mismatch warning is missing %s:\n%s", attr, logged)
		}
	}
}

// The control: when the served size matches the declared release, the probe
// proceeds to the existing duration guard. The duration rejection must keep its
// own reason and its own dead-candidate damper, proving the size gate did not
// absorb it.
func TestBackgroundProbeMatchingServedSizeStillChecksDuration(t *testing.T) {
	const declaredBytes = int64(8_449_999_999)
	uri := "virtual://movie/tt-size-match?result=cand-1"
	key := virtualProbeFailureKey(uri, 5)
	virtualProbeFailures.clear(key)
	virtualProbeFailures.clearSizeMismatch(key)
	t.Cleanup(func() {
		virtualProbeFailures.clear(key)
		virtualProbeFailures.clearSizeMismatch(key)
	})
	logs := captureVirtualProbeLogs(t)

	persisted := false
	h := &PlaybackHandler{
		VirtualPlaybackSourceProber: func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
			f.VideoTracks = []models.VideoTrack{{Codec: "hevc", Width: 3840, Height: 2160}}
			f.AudioTracks = []models.AudioTrack{{Codec: "eac3", Channels: 6, Language: "eng"}}
			f.CodecVideo, f.CodecAudio, f.Resolution, f.Container = "hevc", "eac3", "2160p", "mkv"
			f.ProbedFormatSize = declaredBytes
			f.Duration = 300 // 5 minutes against a 51-minute expectation
			return f, nil
		},
		VirtualFileSaver: func(context.Context, models.VirtualFilePersistArgs) (int64, error) {
			persisted = true
			return 1, nil
		},
	}
	stickyKey := "sticky-size-match"
	h.pinVirtualSticky(stickyKey, uri)
	file := &models.MediaFile{
		ID:                         602,
		ContentID:                  "movie-tt",
		FilePath:                   uri,
		FileSize:                   declaredBytes,
		VirtualOwnerInstallationID: 5,
	}
	cand := VirtualPlaybackStream{ID: "cand-1", URI: uri}

	outcome, _ := h.probeVirtualSourceAndPersistWith(
		context.Background(), stickyKey, file, "http://provider.example/stream", *file, cand,
		noCandidateIdentity, 51, 5, true, nil,
	)
	if outcome != probeOutcomeFailed {
		t.Fatalf("outcome = %q, want failed from the duration guard", outcome)
	}
	if persisted {
		t.Fatal("an implausible duration must not persist probe evidence")
	}
	if got := h.peekVirtualSticky(stickyKey); got != "" {
		t.Fatalf("duration rejection kept the sticky pin: %q", got)
	}
	if !virtualProbeFailures.recent(key) {
		t.Fatal("duration rejection did not arm the dead-candidate damper")
	}
	if virtualProbeFailures.recent(virtualProbeSizeMismatchKey(key)) {
		t.Fatal("a matching served size armed the wrong-source damper")
	}

	logged := logs.String()
	if !strings.Contains(logged, "probed duration implausible") {
		t.Fatalf("matching size did not reach the duration guard:\n%s", logged)
	}
	if strings.Contains(logged, "served size mismatch") {
		t.Fatalf("matching size was rejected by the size gate:\n%s", logged)
	}
}
