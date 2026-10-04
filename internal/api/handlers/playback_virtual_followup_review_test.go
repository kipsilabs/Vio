package handlers

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

// --- Blocker 1: the in-flight subtitle search key includes the candidate. ---

// TestSubtitleSearchDedupeDistinguishesCandidates pins that two concurrent
// replans of the same row with different release candidates are two distinct
// searches: both reach the provider. Keying on the file ID alone silently
// dropped the second legitimate search.
func TestSubtitleSearchDedupeDistinguishesCandidates(t *testing.T) {
	h := &PlaybackHandler{SubtitleSearchInFlight: &sync.Map{}}
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	h.VirtualSubtitleSearcher = func(context.Context, string, string, string, int, int, int, int, []string) {
		started <- struct{}{}
		<-release
	}

	file := &models.MediaFile{ID: 42, ContentID: "movie-candidate-dedupe"}
	candA := VirtualPlaybackStream{URI: "virtual://movie/x?result=a"}
	candB := VirtualPlaybackStream{URI: "virtual://movie/x?result=b"}

	h.maybeTriggerSubtitleSearch(context.Background(), file, candA)
	h.maybeTriggerSubtitleSearch(context.Background(), file, candB)

	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of 2 distinct candidates reached subtitle search", i)
		}
	}
	close(release)
}

// TestSubtitleSearchDedupeCollapsesIdenticalRequest pins the other half: the
// same row and candidate is a single search even when boosted concurrently.
func TestSubtitleSearchDedupeCollapsesIdenticalRequest(t *testing.T) {
	h := &PlaybackHandler{SubtitleSearchInFlight: &sync.Map{}}
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	h.VirtualSubtitleSearcher = func(context.Context, string, string, string, int, int, int, int, []string) {
		started <- struct{}{}
		<-release
	}

	file := &models.MediaFile{ID: 43, ContentID: "movie-identical-dedupe"}
	cand := VirtualPlaybackStream{URI: "virtual://movie/y?result=a"}

	h.maybeTriggerSubtitleSearch(context.Background(), file, cand)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first subtitle search did not start")
	}
	h.maybeTriggerSubtitleSearch(context.Background(), file, cand)
	select {
	case <-started:
		t.Fatal("identical (row, candidate) request was not deduped")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
}

// TestSubtitleSearchDedupeDistinguishesCandidatesWithoutID pins the
// non-positive-ID path: the candidate URI is part of the key there too, so two
// candidates for a not-yet-persisted row both reach the provider.
func TestSubtitleSearchDedupeDistinguishesCandidatesWithoutID(t *testing.T) {
	h := &PlaybackHandler{SubtitleSearchInFlight: &sync.Map{}}
	var started int64
	release := make(chan struct{})
	h.VirtualSubtitleSearcher = func(context.Context, string, string, string, int, int, int, int, []string) {
		atomic.AddInt64(&started, 1)
		<-release
	}

	file := &models.MediaFile{ContentID: "movie-unsaved"}
	candA := VirtualPlaybackStream{URI: "virtual://movie/z?result=a"}
	candB := VirtualPlaybackStream{URI: "virtual://movie/z?result=b"}
	h.maybeTriggerSubtitleSearch(context.Background(), file, candA)
	h.maybeTriggerSubtitleSearch(context.Background(), file, candB)

	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt64(&started) < 2 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&started); got != 2 {
		t.Fatalf("distinct candidates for an ID-less row started %d searches, want 2", got)
	}
	close(release)
}

// --- Blocker 2: a positive ID alone is not candidate ownership. ---

// TestLookupRejectsUnverifiedRowAsCandidateOwner pins that the detailed lookup
// fails closed on a partial or stale row: a positive ID with no verified path
// is treated as incomplete, never as the trusted owner. A verified row (exact
// or provider-neutral identity) is still returned.
func TestLookupRejectsUnverifiedRowAsCandidateOwner(t *testing.T) {
	const uri = "virtual://movie/tt-partial-row?result=cand"
	cases := []struct {
		name      string
		row       *models.MediaFile
		wantFound bool
	}{
		{"positive ID with null path", &models.MediaFile{ID: 5}, false},
		{"positive ID with stale path", &models.MediaFile{ID: 5, FilePath: "virtual://movie/other?result=x"}, false},
		{"different candidate under same neutral", &models.MediaFile{ID: 5, FilePath: "virtual://movie/tt-partial-row?result=other"}, false},
		{"exact candidate path", &models.MediaFile{ID: 5, FilePath: uri}, true},
		{"provider-neutral path", &models.MediaFile{ID: 5, FilePath: "virtual://movie/tt-partial-row"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &PlaybackHandler{
				VirtualFileLookup: func(context.Context, string) (*models.MediaFile, error) {
					return tc.row, nil
				},
			}
			row, found, err := h.lookupVirtualCandidateRowDetailed(context.Background(), uri, "movie", "", 5)
			if found != tc.wantFound {
				t.Fatalf("found = %v, want %v (row %#v)", found, tc.wantFound, row)
			}
			if tc.wantFound {
				if err != nil {
					t.Fatalf("verified row returned error: %v", err)
				}
				if row == nil || row.ID != 5 {
					t.Fatalf("verified row = %#v, want the looked-up row", row)
				}
				return
			}
			if !errors.Is(err, errVirtualCandidateVerdictIncomplete) {
				t.Fatalf("unverified row error = %v, want errVirtualCandidateVerdictIncomplete", err)
			}
			if row != nil {
				t.Fatalf("unverified row = %#v, want nil so it is not used as the owner", row)
			}
		})
	}
}

// TestBackgroundRevalidationIgnoresUnverifiedLookupRow pins the background
// clone path: a partial lookup row (positive ID, no verified path) is not the
// probe target for the candidate it does not own; the requested row stays the
// target. A verified lookup row is used instead.
func TestBackgroundRevalidationIgnoresUnverifiedLookupRow(t *testing.T) {
	const candURI = "virtual://movie/tt-bg-partial?result=cand"

	run := func(t *testing.T, lookupRow *models.MediaFile) int {
		t.Helper()
		const ownerID = 5
		virtualProbeFailures.clear(virtualProbeFailureKey(candURI, ownerID))
		t.Cleanup(func() { virtualProbeFailures.clear(virtualProbeFailureKey(candURI, ownerID)) })

		targets := make(chan int, 1)
		h := &PlaybackHandler{
			VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
				return "http://localhost:8080/stream.mp4", nil
			}),
			VirtualFileLookup: func(context.Context, string) (*models.MediaFile, error) {
				return lookupRow, nil
			},
			VirtualPlaybackSourceProber: func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
				targets <- f.ID
				return nil, errors.New("stop after capturing the probe target")
			},
		}
		file := &models.MediaFile{
			ID: 77, ContentID: "movie-bg-partial", FilePath: candURI,
			VirtualOwnerInstallationID: ownerID, MediaFolderID: 9,
		}
		h.revalidateVirtualCandidateBackground(context.Background(), "sticky", file,
			VirtualPlaybackStream{ID: "cand", URI: candURI}, ownerID, 1, "profile-1", file.ID)
		select {
		case got := <-targets:
			return got
		case <-time.After(2 * time.Second):
			t.Fatal("background revalidation did not reach the prober")
			return 0
		}
	}

	if got := run(t, &models.MediaFile{ID: 999, VirtualOwnerInstallationID: 5}); got != 77 {
		t.Fatalf("partial lookup row was used as the probe target: got ID %d, want the requested row 77", got)
	}
	if got := run(t, &models.MediaFile{ID: 999, FilePath: candURI, VirtualOwnerInstallationID: 5}); got != 999 {
		t.Fatalf("verified lookup row was not used as the probe target: got ID %d, want 999", got)
	}
}

// --- Blocker 3: a refused adoption writes no tracks or probe stamp. ---

// TestVirtualFileMetadataUpdateConflictKeepsLoserTracks is the two-row
// folder/installation conflict repro: the winner owns the resolved path, the
// loser keeps its own path and its own track inventory, and gains neither the
// winner's tracks nor a probe stamp.
func TestVirtualFileMetadataUpdateConflictKeepsLoserTracks(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994330, 7020
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	adoptPath := "virtual://movie/tt-followup-conflict"
	candidatePath := adoptPath + "?result=cand-a"

	winnerTracks := `[{"codec":"h264","width":1920,"height":1080}]`
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source, video_tracks, resolution)
		VALUES('movie-followup-conflict', $1, $2, 'virtual', $3, 'virtual', $4::jsonb, '1080p')`,
		folderID, adoptPath, ownerID, winnerTracks); err != nil {
		t.Fatalf("insert winner row: %v", err)
	}

	loserTracks := `[{"codec":"hevc","width":3840,"height":2160}]`
	var loserID int
	var updatedAt time.Time
	var probeUpdatedAt *time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source, video_tracks)
		VALUES('movie-followup-conflict', $1, $2, 'virtual', $3, 'virtual', $4::jsonb)
		RETURNING id, updated_at, probe_updated_at`, folderID, candidatePath, ownerID, loserTracks,
	).Scan(&loserID, &updatedAt, &probeUpdatedAt); err != nil {
		t.Fatalf("insert loser row: %v", err)
	}

	substituteTracks := []byte(`[{"codec":"av1","width":3840,"height":2160}]`)
	args := models.VirtualFilePersistArgs{
		FileID: loserID, ExpectedFilePath: candidatePath,
		VideoTracks: substituteTracks, AudioTracks: []byte(`[]`), SubtitleTracks: []byte(`[]`),
		Resolution: "2160p", CodecVideo: "av1", CodecAudio: "eac3", Container: "mkv",
		HDR: true, Bitrate: 8000000, Duration: 5400,
		StampProbe: true, UpdatedAt: updatedAt, ProbeUpdatedAt: probeUpdatedAt,
		OwnerID: ownerID, LibraryID: folderID,
		AdoptPath: adoptPath, RequireAdopt: true,
	}
	rows, err := ExecVirtualFileMetadataUpdate(ctx, pool, args)
	if !errors.Is(err, errVirtualAdoptIdentityNotPersisted) {
		t.Fatalf("error = %v, want errVirtualAdoptIdentityNotPersisted", err)
	}
	if rows != 0 {
		t.Fatalf("rows = %d, want 0 for a refused adoption", rows)
	}

	var path, tracks string
	var resolution, stampedAt *string
	if err := pool.QueryRow(ctx, `SELECT file_path, video_tracks::text, resolution, probe_updated_at::text FROM media_files WHERE id = $1`, loserID).Scan(&path, &tracks, &resolution, &stampedAt); err != nil {
		t.Fatalf("read loser row: %v", err)
	}
	if path != candidatePath {
		t.Fatalf("loser file_path = %q, want its own %q", path, candidatePath)
	}
	if !strings.Contains(tracks, "hevc") || strings.Contains(tracks, "av1") {
		t.Fatalf("loser tracks = %s, want its own hevc inventory and not the substitute av1", tracks)
	}
	if resolution != nil {
		t.Fatalf("loser resolution = %q, want no substitute resolution", *resolution)
	}
	if stampedAt != nil {
		t.Fatalf("loser probe_updated_at = %q, want no probe stamp", *stampedAt)
	}

	var winnerTracksAfter string
	if err := pool.QueryRow(ctx, `SELECT video_tracks::text FROM media_files WHERE file_path = $1 AND virtual_owner_installation_id = $2`, adoptPath, ownerID).Scan(&winnerTracksAfter); err != nil {
		t.Fatalf("read winner row: %v", err)
	}
	if !strings.Contains(winnerTracksAfter, "h264") {
		t.Fatalf("winner tracks changed to %s, want h264", winnerTracksAfter)
	}
}

// TestVirtualFileMetadataUpdateRequireAdoptSuccessWritesEverything pins that a
// confirmed adoption still writes the whole row, tracks and probe stamp
// included.
func TestVirtualFileMetadataUpdateRequireAdoptSuccessWritesEverything(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994331, 7021
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	adoptPath := "virtual://movie/tt-followup-success"
	candidatePath := adoptPath + "?result=cand-a"
	var candidateID int
	var updatedAt time.Time
	var probeUpdatedAt *time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source)
		VALUES('movie-followup-success', $1, $2, 'virtual', $3, 'virtual')
		RETURNING id, updated_at, probe_updated_at`, folderID, candidatePath, ownerID,
	).Scan(&candidateID, &updatedAt, &probeUpdatedAt); err != nil {
		t.Fatalf("insert candidate row: %v", err)
	}

	args := models.VirtualFilePersistArgs{
		FileID: candidateID, ExpectedFilePath: candidatePath,
		VideoTracks: []byte(`[{"codec":"av1"}]`), AudioTracks: []byte(`[]`), SubtitleTracks: []byte(`[]`),
		Resolution: "2160p", CodecVideo: "av1", CodecAudio: "eac3", Container: "mkv",
		HDR: true, Bitrate: 8000000, Duration: 5400,
		StampProbe: true, UpdatedAt: updatedAt, ProbeUpdatedAt: probeUpdatedAt,
		OwnerID: ownerID, LibraryID: folderID,
		AdoptPath: adoptPath, RequireAdopt: true,
	}
	rows, err := ExecVirtualFileMetadataUpdate(ctx, pool, args)
	if err != nil {
		t.Fatalf("confirmed adoption failed: %v", err)
	}
	if rows != 1 {
		t.Fatalf("rows = %d, want 1", rows)
	}

	var path, tracks, resolution string
	var stampedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT file_path, video_tracks::text, resolution, probe_updated_at FROM media_files WHERE id = $1`, candidateID).Scan(&path, &tracks, &resolution, &stampedAt); err != nil {
		t.Fatalf("read adopted row: %v", err)
	}
	if path != adoptPath {
		t.Fatalf("file_path = %q, want adopted %q", path, adoptPath)
	}
	if !strings.Contains(tracks, "av1") {
		t.Fatalf("video_tracks = %s, want the substitute inventory", tracks)
	}
	if resolution != "2160p" {
		t.Fatalf("resolution = %q, want 2160p", resolution)
	}
	if stampedAt == nil {
		t.Fatal("probe_updated_at was not stamped on a confirmed adoption")
	}
}

// TestVerdictLookupAcceptsRotatedConcreteRow pins the live rehydration
// regression: the provider rotates a release's ?result= id, so the exact lookup
// misses by design and the provider-neutral lookup returns the same release's
// row still carrying the old concrete pick. That row owns the release, so its
// verdict must be readable; treating the rotated concrete pick as an incomplete
// row hard-failed rehydration (4 warning-loop retries) for a row that exists.
func TestVerdictLookupAcceptsRotatedConcreteRow(t *testing.T) {
	const (
		candidate = "virtual://movie/tt-rotated?result=newpick"
		stale     = "virtual://movie/tt-rotated?result=oldpick"
	)
	file := &models.MediaFile{ID: 7, ContentID: "movie-tt-rotated", FilePath: candidate}

	h := &PlaybackHandler{
		VirtualFileLookup: func(context.Context, string) (*models.MediaFile, error) {
			return nil, ErrVirtualCandidateNotFound
		},
		VirtualCandidateFileLookup: func(context.Context, string, string, string, int) (*models.MediaFile, error) {
			return &models.MediaFile{ID: 77, FilePath: stale}, nil
		},
	}
	row, found, err := h.lookupVirtualCandidateRowDetailed(context.Background(), candidate, file.ContentID, file.EpisodeID, 5)
	if err != nil {
		t.Fatalf("rotated concrete row treated as incomplete: %v", err)
	}
	if !found || row == nil || row.ID != 77 {
		t.Fatalf("row = %#v found=%v, want the rotated row id 77", row, found)
	}
	if err := h.virtualCandidateVerdictError(context.Background(), candidate, file, 5, false); err != nil {
		t.Fatalf("rotated concrete row refused the verdict gate: %v", err)
	}

	// The rotation relaxation is not a blanket acceptance: a live failed_at on
	// the same rotated row still refuses.
	failedAt := time.Now()
	failed := &PlaybackHandler{
		VirtualFileLookup: func(context.Context, string) (*models.MediaFile, error) {
			return nil, ErrVirtualCandidateNotFound
		},
		VirtualCandidateFileLookup: func(context.Context, string, string, string, int) (*models.MediaFile, error) {
			return &models.MediaFile{ID: 77, FilePath: stale, FailedAt: &failedAt}, nil
		},
	}
	if err := failed.virtualCandidateVerdictError(context.Background(), candidate, file, 5, false); err == nil {
		t.Fatal("a genuinely failed rotated row was accepted")
	}
}

// TestVerdictLookupStillRejectsDifferentProfileVariant pins that the rotation
// relaxation keeps the profile: the provider-neutral key retains ?profile=, so
// a row for another profile variant is a different release and stays
// incomplete rather than owning the candidate's verdict.
func TestVerdictLookupStillRejectsDifferentProfileVariant(t *testing.T) {
	const candidate = "virtual://movie/tt-profile?profile=4k&result=newpick"
	file := &models.MediaFile{ID: 7, ContentID: "movie-tt-profile", FilePath: candidate}

	h := &PlaybackHandler{
		VirtualFileLookup: func(context.Context, string) (*models.MediaFile, error) {
			return nil, ErrVirtualCandidateNotFound
		},
		VirtualCandidateFileLookup: func(context.Context, string, string, string, int) (*models.MediaFile, error) {
			return &models.MediaFile{ID: 88, FilePath: "virtual://movie/tt-profile?profile=1080p&result=oldpick"}, nil
		},
	}
	if _, _, err := h.lookupVirtualCandidateRowDetailed(context.Background(), candidate, file.ContentID, file.EpisodeID, 5); !errors.Is(err, errVirtualCandidateVerdictIncomplete) {
		t.Fatalf("different profile variant error = %v, want errVirtualCandidateVerdictIncomplete", err)
	}
}

// TestVerdictLookupDoesNotVetoSiblingWithIdentity proves a failed row that
// carries durable identity never vetoes a different pick under the same
// neutral key: the German STARS row's verdict must not refuse the ToonsHub
// candidate. Only same-identity rows, or identity-less legacy rows, bind.
func TestVerdictLookupDoesNotVetoSiblingWithIdentity(t *testing.T) {
	const candidate = "virtual://series/tt-toonshub?result=toonshub1"
	file := &models.MediaFile{ID: 7, ContentID: "series-toonshub", FilePath: candidate}
	failedAt := time.Now()
	h := &PlaybackHandler{
		VirtualFileLookup: func(context.Context, string) (*models.MediaFile, error) {
			return nil, ErrVirtualCandidateNotFound
		},
		VirtualCandidateFileLookup: func(context.Context, string, string, string, int) (*models.MediaFile, error) {
			return &models.MediaFile{
				ID: 77, FilePath: "virtual://series/tt-toonshub?result=stars1",
				ProviderReleaseName: "germanstarsrelease1080p", FailedAt: &failedAt,
			}, nil
		},
	}
	row, found, err := h.lookupVirtualCandidateRowDetailed(context.Background(), candidate, file.ContentID, file.EpisodeID, 5)
	if err != nil {
		t.Fatalf("sibling lookup treated as incomplete: %v", err)
	}
	if !found || row == nil {
		t.Fatal("sibling row not found for metadata adoption")
	}
	if err := h.virtualCandidateVerdictError(context.Background(), candidate, file, 5, false); err != nil {
		t.Fatalf("failed sibling with identity vetoed a different pick: %v", err)
	}
}
