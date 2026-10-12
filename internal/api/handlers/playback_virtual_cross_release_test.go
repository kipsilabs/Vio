package handlers

import (
	"bytes"
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/jackc/pgx/v5/pgxpool"
)

// readVirtualRowJSON returns the entire row serialized by Postgres. Comparing
// the full row text is the strongest byte-identical assertion available: it
// covers every column (track jsonb, probe stamp, updated_at, file_path, …),
// not only the fields the test happens to remember, so a rejected write that
// touched anything at all fails.
func readVirtualRowJSON(t *testing.T, pool *pgxpool.Pool, id int) string {
	t.Helper()
	var row string
	if err := pool.QueryRow(context.Background(),
		`SELECT row_to_json(mf)::text FROM media_files mf WHERE id = $1`, id).Scan(&row); err != nil {
		t.Fatalf("read row %d as json: %v", id, err)
	}
	return row
}

// TestVirtualProbeEvidenceRequiresAdoptionClassification pins exactly which
// evidence writes are cross-release. Same-release evidence (the row's own
// concrete pick, or a concrete pick under a neutral row's own provider-neutral
// identity) stays metadata-only; a different concrete pick under the same
// neutral key, or a different neutral key, is cross-release and must be gated
// on a confirmed identity adoption.
func TestVirtualProbeEvidenceRequiresAdoptionClassification(t *testing.T) {
	const neutral = "virtual://movie/tt-cross"
	cases := []struct {
		name      string
		rowPath   string
		candidate string
		want      bool
	}{
		{"same concrete release", neutral + "?result=anchor", neutral + "?result=anchor", false},
		{"neutral row owns its concrete candidate", neutral, neutral + "?result=anchor", false},
		{"different concrete pick under the same neutral", neutral + "?result=anchor", neutral + "?result=sibling", true},
		{"different neutral release from a concrete row", neutral + "?result=anchor", "virtual://movie/tt-other?result=sibling", true},
		{"different neutral release from a neutral row", neutral, "virtual://movie/tt-other", true},
		{"empty candidate identity is not evidence", neutral + "?result=anchor", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := &models.MediaFile{ID: 7, FilePath: tc.rowPath}
			if got := virtualProbeEvidenceRequiresAdoption(row, tc.candidate); got != tc.want {
				t.Fatalf("requires adoption = %v, want %v (row %q candidate %q)",
					got, tc.want, tc.rowPath, tc.candidate)
			}
		})
	}
}

// TestVirtualProbeEvidenceSameReleaseStaysMetadataOnly is the legitimate path
// the fix must not disturb: a provider-neutral row receiving its own release's
// concrete candidate keeps the previous metadata-only contract
// (RequireAdopt=false), adopts the concrete path best-effort, and writes the
// probed inventory and stamp.
func TestVirtualProbeEvidenceSameReleaseStaysMetadataOnly(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994341, 7031
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	const neutral = "virtual://movie/tt-same-release"
	candidateURI := neutral + "?result=same"

	var rowID int
	var updatedAt time.Time
	var probeUpdatedAt *time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source, video_tracks)
		VALUES('movie-same-release', $1, $2, 'virtual', $3, 'virtual', '[{"codec":"hevc","width":3840,"height":2160}]'::jsonb)
		RETURNING id, updated_at, probe_updated_at`, folderID, neutral, ownerID,
	).Scan(&rowID, &updatedAt, &probeUpdatedAt); err != nil {
		t.Fatalf("insert row: %v", err)
	}

	var argsMu sync.Mutex
	var savedArgs []models.VirtualFilePersistArgs
	h := &PlaybackHandler{
		VirtualPlaybackSourceProber: func(context.Context, string, *models.MediaFile) (*models.MediaFile, error) {
			return &models.MediaFile{
				FilePath:    candidateURI,
				VideoTracks: []models.VideoTrack{{Codec: "av1", Width: 3840, Height: 2160}},
				AudioTracks: []models.AudioTrack{{Codec: "eac3", Channels: 6}},
				Resolution:  "2160p", CodecVideo: "av1", CodecAudio: "eac3", Container: "mkv",
				Duration: 5400, Bitrate: 8000000,
			}, nil
		},
		VirtualFileSaver: func(ctx context.Context, args models.VirtualFilePersistArgs) (int64, error) {
			return ExecVirtualFileMetadataUpdate(ctx, pool, args)
		},
		VirtualFileMetadataSaver: func(ctx context.Context, args models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
			argsMu.Lock()
			savedArgs = append(savedArgs, args)
			argsMu.Unlock()
			return ExecVirtualFileMetadataUpdateResult(ctx, pool, args)
		},
	}
	t.Cleanup(func() { h.StopVirtualEvidence() })
	failureKey := virtualProbeFailureKey(candidateURI, ownerID)
	virtualProbeFailures.clear(failureKey)
	t.Cleanup(func() { virtualProbeFailures.clear(failureKey) })

	row := &models.MediaFile{
		ID: rowID, ContentID: "movie-same-release", FilePath: neutral,
		MediaFolderID: folderID, VirtualOwnerInstallationID: ownerID,
		UpdatedAt: updatedAt, ProbeUpdatedAt: probeUpdatedAt,
	}
	probeTransient := *row
	probeTransient.FilePath = candidateURI
	cand := VirtualPlaybackStream{ID: "same", URI: candidateURI, Resolution: "2160p"}

	h.probeVirtualSourceAndPersist(ctx, "sticky-same-release", row, "http://provider.example/same", probeTransient, cand, noCandidateIdentity, 90, ownerID)
	h.StopVirtualEvidence()

	argsMu.Lock()
	defer argsMu.Unlock()
	if len(savedArgs) == 0 {
		t.Fatal("same-release evidence never reached the saver")
	}
	for _, a := range savedArgs {
		if a.RequireAdopt {
			t.Fatalf("same-release write required adoption: %+v", a)
		}
		if a.AdoptPath != candidateURI {
			t.Fatalf("AdoptPath = %q, want the row's own candidate %q", a.AdoptPath, candidateURI)
		}
	}

	var path, tracks string
	var stampedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT file_path, video_tracks::text, probe_updated_at FROM media_files WHERE id = $1`, rowID).Scan(&path, &tracks, &stampedAt); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if path != candidateURI {
		t.Fatalf("file_path = %q, want the row's own candidate %q", path, candidateURI)
	}
	if !strings.Contains(tracks, "av1") {
		t.Fatalf("tracks = %s, want the probed inventory", tracks)
	}
	if stampedAt == nil {
		t.Fatal("probe_updated_at was not stamped on the same-release path")
	}
}

// TestVirtualProbeEvidenceCrossReleaseRefusedKeepsOriginalRowByteIdentical is
// the review's two-row conflict repro at the handler layer. Candidate X belongs
// to a sibling row that owns X's path; the original row is a different concrete
// release. The probe-evidence write must be admitted as a required adoption,
// the sibling guard must refuse it, and the original row must come out of the
// refused write byte-identical — tracks, probe stamp, timestamps and all.
func TestVirtualProbeEvidenceCrossReleaseRefusedKeepsOriginalRowByteIdentical(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994340, 7030
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	const base = "virtual://movie/tt-cross-release"
	anchorURI := base + "?result=anchor"
	siblingURI := base + "?result=sibling"

	// The sibling owns the candidate path the probe resolves.
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source, video_tracks, resolution)
		VALUES('movie-cross-release', $1, $2, 'virtual', $3, 'virtual', '[{"codec":"h264","width":1920,"height":1080}]'::jsonb, '1080p')`,
		folderID, siblingURI, ownerID); err != nil {
		t.Fatalf("insert sibling row: %v", err)
	}

	// The original row is a different concrete release under the same content.
	var originalID int
	var updatedAt time.Time
	var probeUpdatedAt *time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source, video_tracks)
		VALUES('movie-cross-release', $1, $2, 'virtual', $3, 'virtual', '[{"codec":"hevc","width":3840,"height":2160}]'::jsonb)
		RETURNING id, updated_at, probe_updated_at`, folderID, anchorURI, ownerID,
	).Scan(&originalID, &updatedAt, &probeUpdatedAt); err != nil {
		t.Fatalf("insert original row: %v", err)
	}
	before := readVirtualRowJSON(t, pool, originalID)

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	var argsMu sync.Mutex
	var savedArgs []models.VirtualFilePersistArgs
	h := &PlaybackHandler{
		VirtualPlaybackSourceProber: func(context.Context, string, *models.MediaFile) (*models.MediaFile, error) {
			return &models.MediaFile{
				FilePath:    siblingURI,
				VideoTracks: []models.VideoTrack{{Codec: "av1", Width: 3840, Height: 2160}},
				AudioTracks: []models.AudioTrack{{Codec: "eac3", Channels: 6, Language: "eng"}},
				Resolution:  "2160p", CodecVideo: "av1", CodecAudio: "eac3", Container: "mkv",
				Duration: 5400, Bitrate: 8000000,
			}, nil
		},
		VirtualFileSaver: func(ctx context.Context, args models.VirtualFilePersistArgs) (int64, error) {
			return ExecVirtualFileMetadataUpdate(ctx, pool, args)
		},
		VirtualFileMetadataSaver: func(ctx context.Context, args models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
			argsMu.Lock()
			savedArgs = append(savedArgs, args)
			argsMu.Unlock()
			return ExecVirtualFileMetadataUpdateResult(ctx, pool, args)
		},
	}
	t.Cleanup(func() { h.StopVirtualEvidence() })
	failureKey := virtualProbeFailureKey(siblingURI, ownerID)
	virtualProbeFailures.clear(failureKey)
	t.Cleanup(func() { virtualProbeFailures.clear(failureKey) })

	original := &models.MediaFile{
		ID: originalID, ContentID: "movie-cross-release", FilePath: anchorURI,
		MediaFolderID: folderID, VirtualOwnerInstallationID: ownerID,
		UpdatedAt: updatedAt, ProbeUpdatedAt: probeUpdatedAt,
	}
	probeTransient := *original
	probeTransient.FilePath = siblingURI
	cand := VirtualPlaybackStream{ID: "sibling", URI: siblingURI, Resolution: "2160p"}

	h.probeVirtualSourceAndPersist(ctx, "sticky-cross-release", original, "http://provider.example/cross", probeTransient, cand, noCandidateIdentity, 90, ownerID)
	h.StopVirtualEvidence()

	after := readVirtualRowJSON(t, pool, originalID)
	if after != before {
		t.Fatalf("original row changed on a refused cross-release write:\nbefore=%s\nafter =%s", before, after)
	}

	argsMu.Lock()
	if len(savedArgs) == 0 {
		argsMu.Unlock()
		t.Fatal("cross-release evidence never reached the saver")
	}
	for _, a := range savedArgs {
		if !a.RequireAdopt || a.AdoptPath != siblingURI || a.FileID != originalID {
			argsMu.Unlock()
			t.Fatalf("saver args = %+v, want RequireAdopt=true adopt=%q file=%d", a, siblingURI, originalID)
		}
	}
	argsMu.Unlock()

	// The sibling owns the candidate path and must not have been overwritten
	// by the refused write either.
	var siblingTracks string
	if err := pool.QueryRow(ctx, `SELECT video_tracks::text FROM media_files WHERE file_path = $1 AND virtual_owner_installation_id = $2`, siblingURI, ownerID).Scan(&siblingTracks); err != nil {
		t.Fatalf("read sibling row: %v", err)
	}
	if !strings.Contains(siblingTracks, "h264") || strings.Contains(siblingTracks, "av1") {
		t.Fatalf("sibling tracks = %s, want its own h264 and not the candidate av1", siblingTracks)
	}

	// The refusal must be observable: the file id, the candidate identity and
	// a reason are all in the log.
	logText := logs.String()
	if !strings.Contains(logText, "required identity adoption matched no row") {
		t.Fatalf("refusal reason was not logged:\n%s", logText)
	}
	if !strings.Contains(logText, siblingURI) {
		t.Fatalf("candidate identity %q was not logged:\n%s", siblingURI, logText)
	}
	if !strings.Contains(logText, `"file_id":`+strconv.Itoa(originalID)) {
		t.Fatalf("file id %d was not logged:\n%s", originalID, logText)
	}
}
