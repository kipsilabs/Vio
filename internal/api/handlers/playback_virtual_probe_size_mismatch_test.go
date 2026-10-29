package handlers

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// sizeMismatchFallbackHandler builds a synchronous resolve whose listed
// candidates are A, B, and C under one neutral row. A and B are two indexer
// offers of the same release; C is a different release. The prober's behavior
// is supplied per candidate file path.
func sizeMismatchFallbackHandler(t *testing.T, file *models.MediaFile, candidates []VirtualPlaybackStream, rows map[string]*models.MediaFile, probe func(*models.MediaFile) (int64, error)) (*PlaybackHandler, *[]int, *[]models.VirtualFilePersistArgs) {
	t.Helper()
	var mu sync.Mutex
	var probes []int
	var saved []models.VirtualFilePersistArgs
	h := &PlaybackHandler{
		ServiceContext: context.Background(),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
			return candidates, nil
		}),
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
			return "https://93.184.216.34/stream/" + virtualResultCandidateID(path), nil
		}),
		VirtualFileLookup: func(_ context.Context, path string) (*models.MediaFile, error) {
			return rows[path], nil
		},
		VirtualPlaybackSourceProber: func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
			mu.Lock()
			probes = append(probes, f.ID)
			mu.Unlock()
			served, err := probe(f)
			if err != nil {
				return nil, err
			}
			f.ProbedFormatSize = served
			f.Duration = 5400
			f.VideoTracks = []models.VideoTrack{{Codec: "hevc", Width: 3840, Height: 2160}}
			f.AudioTracks = []models.AudioTrack{{Codec: "eac3", Channels: 6, Language: "eng"}}
			f.CodecVideo, f.CodecAudio, f.Resolution, f.Container = "hevc", "eac3", "2160p", "mkv"
			return f, nil
		},
		VirtualFileSaver: func(_ context.Context, args models.VirtualFilePersistArgs) (int64, error) {
			mu.Lock()
			saved = append(saved, args)
			mu.Unlock()
			return 1, nil
		},
	}
	t.Cleanup(h.StopVirtualEvidence)
	_ = file
	return h, &probes, &saved
}

func sizeMismatchFallbackRow(id int, uri, release string, size int64) *models.MediaFile {
	return &models.MediaFile{
		ID: id, ContentID: "movie-tt-size-fallback", FilePath: uri, Container: "virtual",
		FileSize: size, ProviderReleaseName: release, ProviderReleaseSize: size,
		VirtualOwnerInstallationID: 5, MediaFolderID: 9, ProbeSource: "virtual",
	}
}

func sizeMismatchFallbackCandidate(id, uri, release string, size int64) VirtualPlaybackStream {
	// Deliberately no declared codecs/container: that keeps the synchronous
	// probe path (the one this test exercises) instead of the declared-metadata
	// fast path that hands the probe to a background worker.
	return VirtualPlaybackStream{
		ID: id, URI: uri, FileSize: size, ProviderReleaseName: release,
		OwnerInstallationID: 5,
	}
}

// TestResolveVirtualPlaybackMatchingFirstProbeProbesNoSiblings pins the
// no-op-when-the-provider-is-fixed property: when the selected row's first
// probe serves bytes matching its declared size, the sibling fallback must not
// run a single extra probe. The sibling path exists only after a mismatch.
func TestResolveVirtualPlaybackMatchingFirstProbeProbesNoSiblings(t *testing.T) {
	const (
		release = "Movie.2024.2160p.WEB-DL"
		size    = int64(8_000_000_000)
		uriA    = "virtual://movie/tt-size-fallback?result=treasure"
		uriB    = "virtual://movie/tt-size-fallback?result=althub"
	)
	file := &models.MediaFile{
		ID: 600, ContentID: "movie-tt-size-fallback", FilePath: "virtual://movie/tt-size-fallback",
		Container: "virtual", VirtualOwnerInstallationID: 5, MediaFolderID: 9,
	}
	rows := map[string]*models.MediaFile{
		uriA: sizeMismatchFallbackRow(601, uriA, release, size),
		uriB: sizeMismatchFallbackRow(602, uriB, release, size),
	}
	candidates := []VirtualPlaybackStream{
		sizeMismatchFallbackCandidate("treasure", uriA, release, size),
		sizeMismatchFallbackCandidate("althub", uriB, release, size),
	}
	h, probes, _ := sizeMismatchFallbackHandler(t, file, candidates, rows, func(*models.MediaFile) (int64, error) {
		return size, nil
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolve error: %v", err)
	}
	if resolved.URI != uriA {
		t.Fatalf("resolved URI = %q, want the first candidate %q", resolved.URI, uriA)
	}
	if len(*probes) != 1 {
		t.Fatalf("probe calls = %v, want exactly the first candidate; a matching first probe must not probe siblings", *probes)
	}
}

// TestResolveVirtualPlaybackServedSizeMismatchAdoptsSameReleaseSibling pins the
// fallback contract: the selected row's probe served a different release, so the
// same release's second indexer row is probed and adopted, and its verified
// inventory is persisted onto that sibling row only. The selected row keeps the
// short wrong-source damper, and the different release (C) is never probed.
func TestResolveVirtualPlaybackServedSizeMismatchAdoptsSameReleaseSibling(t *testing.T) {
	const (
		release = "Movie.2024.2160p.WEB-DL"
		size    = int64(8_000_000_000)
		uriA    = "virtual://movie/tt-size-fallback?result=treasure"
		uriB    = "virtual://movie/tt-size-fallback?result=althub"
		uriC    = "virtual://movie/tt-size-fallback?result=ninja"
		other   = "Movie.2024.720p.WEB-DL"
		otherSz = int64(2_000_000_000)
	)
	keyA := virtualProbeFailureKey(uriA, 5)
	keyB := virtualProbeFailureKey(uriB, 5)
	for _, key := range []string{keyA, keyB, virtualProbeFailureKey(uriC, 5)} {
		virtualProbeFailures.clear(key)
		virtualProbeFailures.clearSizeMismatch(key)
	}
	t.Cleanup(func() {
		for _, key := range []string{keyA, keyB, virtualProbeFailureKey(uriC, 5)} {
			virtualProbeFailures.clear(key)
			virtualProbeFailures.clearSizeMismatch(key)
		}
	})

	file := &models.MediaFile{
		ID: 610, ContentID: "movie-tt-size-fallback", FilePath: "virtual://movie/tt-size-fallback",
		Container: "virtual", VirtualOwnerInstallationID: 5, MediaFolderID: 9,
	}
	rows := map[string]*models.MediaFile{
		uriA: sizeMismatchFallbackRow(611, uriA, release, size),
		uriB: sizeMismatchFallbackRow(612, uriB, release, size),
		uriC: sizeMismatchFallbackRow(613, uriC, other, otherSz),
	}
	candidates := []VirtualPlaybackStream{
		sizeMismatchFallbackCandidate("treasure", uriA, release, size),
		sizeMismatchFallbackCandidate("althub", uriB, release, size),
		sizeMismatchFallbackCandidate("ninja", uriC, other, otherSz),
	}
	h, probes, saved := sizeMismatchFallbackHandler(t, file, candidates, rows, func(f *models.MediaFile) (int64, error) {
		if f.FilePath == uriA {
			// A stale cache fill of a shorter different release.
			return size * 88 / 100, nil
		}
		return size, nil
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolve error: %v", err)
	}
	if resolved.URI != uriB {
		t.Fatalf("resolved URI = %q, want the matching same-release sibling %q", resolved.URI, uriB)
	}
	if resolved.File == nil || resolved.File.ID != 612 {
		t.Fatalf("resolved file = %+v, want the sibling row 612", resolved.File)
	}
	if len(*probes) != 2 || (*probes)[0] != 611 || (*probes)[1] != 612 {
		t.Fatalf("probe order = %v, want [611 612]: only the selected row and its same-release sibling", *probes)
	}
	if !virtualProbeFailures.recent(virtualProbeSizeMismatchKey(keyA)) {
		t.Fatal("the selected row did not keep its short wrong-source damper")
	}
	if virtualProbeFailures.recent(virtualProbeSizeMismatchKey(keyB)) {
		t.Fatal("the adopted sibling kept a wrong-source damper after matching bytes")
	}
	if virtualProbeFailures.recent(keyA) {
		t.Fatal("a served-size mismatch must not feed the dead-candidate damper")
	}

	h.StopVirtualEvidence()
	if len(*saved) != 1 {
		t.Fatalf("persisted %d inventories, want exactly one on the sibling row: %+v", len(*saved), *saved)
	}
	write := (*saved)[0]
	if write.FileID != 612 {
		t.Fatalf("inventory persisted on file %d, want only the sibling 612", write.FileID)
	}
	if write.ExpectedFilePath != uriB {
		t.Fatalf("inventory expected path = %q, want the sibling's own path %q", write.ExpectedFilePath, uriB)
	}
}

// TestBackgroundProbeServedSizeMismatchAdoptsSameReleaseSibling pins the shared
// probe tail's fallback: the deferred/background probe of the selected row
// served the wrong release's bytes, so the release's sibling row (second in the
// version list's order) is probed and adopted. The durable write commits on the
// sibling's row only, so the session's committed inventory belongs to the bytes
// it verified.
func TestBackgroundProbeServedSizeMismatchAdoptsSameReleaseSibling(t *testing.T) {
	const (
		release = "Movie.2024.2160p.WEB-DL"
		size    = int64(8_000_000_000)
		uriA    = "virtual://movie/tt-background-sibling?result=treasure"
		uriB    = "virtual://movie/tt-background-sibling?result=althub"
		uriC    = "virtual://movie/tt-background-sibling?result=ninja"
		urlA    = "https://93.184.216.34/stream/treasure"
		urlB    = "https://93.184.216.34/stream/althub"
		urlC    = "https://93.184.216.34/stream/ninja"
	)
	rowA := sizeMismatchFallbackRow(811, uriA, release, size)
	rowA.ContentID = "movie-tt-background-sibling"
	rowA.ResolvedURL = urlA
	rowB := sizeMismatchFallbackRow(812, uriB, release, size)
	rowB.ContentID = rowA.ContentID
	rowB.ResolvedURL = urlB
	rowC := sizeMismatchFallbackRow(813, uriC, "Movie.2024.720p.WEB-DL", 2_000_000_000)
	rowC.ContentID = rowA.ContentID
	rowC.ResolvedURL = urlC

	keyA := virtualProbeFailureKey(uriA, 5)
	virtualProbeFailures.clear(keyA)
	virtualProbeFailures.clearSizeMismatch(keyA)
	t.Cleanup(func() {
		virtualProbeFailures.clear(keyA)
		virtualProbeFailures.clearSizeMismatch(keyA)
	})

	var mu sync.Mutex
	var probes []string
	var saved []models.VirtualFilePersistArgs
	h := &PlaybackHandler{
		ServiceContext: context.Background(),
		FileVersionFetcher: testPlaybackFileVersionFetcher{
			byContent: map[string][]*models.MediaFile{rowA.ContentID: {rowA, rowB, rowC}},
		},
		VirtualPlaybackSourceProber: func(_ context.Context, sourceURL string, f *models.MediaFile) (*models.MediaFile, error) {
			mu.Lock()
			probes = append(probes, sourceURL)
			mu.Unlock()
			f.ProbedFormatSize = size
			if sourceURL == urlA {
				f.ProbedFormatSize = size * 88 / 100
			}
			f.Duration = 5400
			f.VideoTracks = []models.VideoTrack{{Codec: "hevc", Width: 3840, Height: 2160}}
			f.AudioTracks = []models.AudioTrack{{Codec: "eac3", Channels: 6, Language: "eng"}}
			f.CodecVideo, f.CodecAudio, f.Resolution, f.Container = "hevc", "eac3", "2160p", "mkv"
			return f, nil
		},
		VirtualFileMetadataSaver: func(_ context.Context, args models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
			mu.Lock()
			saved = append(saved, args)
			mu.Unlock()
			return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
		},
	}

	candA := virtualPlaybackStreamFromFile(rowA)
	outcome, committed := h.probeVirtualSourceAndPersistWith(
		context.Background(), "sticky-background-sibling", rowA, urlA, *rowA, *candA,
		noCandidateIdentity, 0, 5, true, nil,
	)
	if outcome != probeOutcomeVerified {
		t.Fatalf("outcome = %q, want verified from the adopted sibling", outcome)
	}
	if !committed.committed() || committed.fileID != rowB.ID {
		t.Fatalf("committed inventory = %+v, want the sibling row %d", committed, rowB.ID)
	}
	if len(probes) != 2 || probes[0] != urlA || probes[1] != urlB {
		t.Fatalf("probes = %v, want [selected sibling-url]: only the selected row and its same-release sibling", probes)
	}
	if !virtualProbeFailures.recent(virtualProbeSizeMismatchKey(keyA)) {
		t.Fatal("the selected row did not keep its short wrong-source damper")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(saved) != 1 || saved[0].FileID != rowB.ID {
		t.Fatalf("saved = %+v, want exactly one inventory on the sibling row %d", saved, rowB.ID)
	}
}
