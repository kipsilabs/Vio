package handlers

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/jackc/pgx/v5/pgxpool"
)

// readVirtualEvidenceRowJSON serializes a whole media_files row from Postgres.
// Comparing the full text is the strongest byte-identical assertion available:
// it covers every column, not just the fields a test remembers to check, so a
// refused write that touched anything at all fails the comparison.
func readVirtualEvidenceRowJSON(t *testing.T, pool *pgxpool.Pool, id int) string {
	t.Helper()
	var row string
	if err := pool.QueryRow(context.Background(),
		`SELECT row_to_json(mf)::text FROM media_files mf WHERE id = $1`, id).Scan(&row); err != nil {
		t.Fatalf("read row %d as json: %v", id, err)
	}
	return row
}

// candidateOwnership is the pure rule the cross-release classification is built
// on: a row owns a candidate exactly when it carries the candidate's concrete
// release identity, or the provider-neutral identity with no concrete pick. A
// different concrete pick under the same neutral key, or a different neutral
// key, is not owned and forces the write through the adoption fence.
func TestCandidateOwnershipNeutralRowCrossRelease(t *testing.T) {
	const neutral = "virtual://movie/tt-ownership"
	neutralRow := &models.MediaFile{ID: 7, FilePath: neutral}

	cases := []struct {
		name      string
		row       *models.MediaFile
		candidate string
		wantOwned bool
	}{
		{"neutral row owns its concrete candidate", neutralRow, neutral + "?result=anchor", true},
		{"neutral row owns another pick of its own release", neutralRow, neutral + "?result=sibling", true},
		{"different neutral release is not owned", neutralRow, "virtual://movie/tt-other?result=sibling", false},
		{"different provider-neutral release is not owned", neutralRow, "virtual://movie/tt-other", false},
		{"empty candidate is not owned", neutralRow, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := virtualCandidateRowVerified(tc.row, tc.candidate); got != tc.wantOwned {
				t.Fatalf("virtualCandidateRowVerified(%q, %q) = %v, want %v",
					tc.row.FilePath, tc.candidate, got, tc.wantOwned)
			}
		})
	}
}

// TestPersistProbeEvidenceCollectionRowRefusesCrossReleaseBeforeEnqueue is the
// collection-refusal case against the real cross-release gate. A collection-owned
// row is never rewritten (the collection sync reconciles its file_path against
// its desired set), so a cross-release candidate has no adoption target and the
// write must be refused before it reaches the evidence buffer and logged with
// the file id, the candidate identity and the reason. The same row's own
// release still gets its metadata-only enrichment, which is not cross-release
// and still carries no adoption target.
func TestPersistProbeEvidenceCollectionRowRefusesCrossReleaseBeforeEnqueue(t *testing.T) {
	const (
		collectionPath = "virtual://movie/tt-collection"
		crossRelease   = "virtual://movie/tt-other?result=cand"
		ownRelease     = collectionPath + "?result=cand"
	)

	newHandler := func(captured *[]models.VirtualFilePersistArgs) *PlaybackHandler {
		return &PlaybackHandler{
			VirtualFileSaver: func(context.Context, models.VirtualFilePersistArgs) (int64, error) {
				return 0, nil
			},
			VirtualFileMetadataSaver: func(_ context.Context, args models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
				*captured = append(*captured, args)
				return VirtualFileMetadataUpdateResult{MetadataUpdated: true}, nil
			},
		}
	}
	collectionRow := func() *models.MediaFile {
		return &models.MediaFile{
			ID:                         200,
			FilePath:                   collectionPath,
			ProbeSource:                "virtual_collection",
			VirtualOwnerInstallationID: 5,
			MediaFolderID:              9,
			UpdatedAt:                  time.Now(),
		}
	}

	// Cross-release: refused before enqueue, with the refusal in the log.
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	var crossCaptured []models.VirtualFilePersistArgs
	crossHandler := newHandler(&crossCaptured)
	t.Cleanup(crossHandler.StopVirtualEvidence)
	probedCross := &models.MediaFile{FilePath: crossRelease, Resolution: "1080p"}
	crossHandler.persistVirtualProbeEvidence(context.Background(), collectionRow(), crossRelease, probedCross, noCandidateIdentity, true, false)
	crossHandler.StopVirtualEvidence()

	if len(crossCaptured) != 0 {
		t.Fatalf("cross-release collection-row evidence reached the saver: %+v", crossCaptured)
	}
	logText := logs.String()
	if !strings.Contains(logText, `"reason":"cross_release_without_adopt_target"`) {
		t.Fatalf("refusal reason was not logged:\n%s", logText)
	}
	if !strings.Contains(logText, crossRelease) {
		t.Fatalf("candidate identity %q was not logged:\n%s", crossRelease, logText)
	}
	if !strings.Contains(logText, `"file_id":200`) {
		t.Fatalf("file id was not logged:\n%s", logText)
	}

	// The row's own release is same-release evidence: it is admitted as
	// metadata-only and carries no adoption target.
	var ownCaptured []models.VirtualFilePersistArgs
	ownHandler := newHandler(&ownCaptured)
	t.Cleanup(ownHandler.StopVirtualEvidence)
	probedOwn := &models.MediaFile{FilePath: ownRelease, Resolution: "1080p"}
	ownHandler.persistVirtualProbeEvidence(context.Background(), collectionRow(), ownRelease, probedOwn, noCandidateIdentity, true, false)
	ownHandler.StopVirtualEvidence()

	if len(ownCaptured) != 1 {
		t.Fatalf("same-release collection-row evidence saved %d times, want 1", len(ownCaptured))
	}
	if ownCaptured[0].AdoptPath != "" || ownCaptured[0].RequireAdopt {
		t.Fatalf("collection-row evidence nominated an adoption target: %+v", ownCaptured[0])
	}
}

// TestVirtualFileMetadataUpdateCollectionRowRefusesRequiredAdoption is the
// collection-refusal case: a required adoption against a collection-owned row
// can never happen (its file_path is owned by the collection sync), so the
// whole atomic write is refused and the row is left byte-identical.
func TestVirtualFileMetadataUpdateCollectionRowRefusesRequiredAdoption(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994350, 7040
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	neutralPath := "virtual://movie/tt-db-collection"
	var rowID int
	var updatedAt time.Time
	var probeUpdatedAt *time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source,
			video_tracks, resolution, provider_video_hash, resolved_url, provider_request_headers)
		VALUES('movie-db-collection', $1, $2, 'virtual', $3, 'virtual_collection',
			'[{"codec":"h264","width":1920,"height":1080}]'::jsonb, '1080p', 'aaaaaaaa',
			'http://old.example/release-a', '{"Referer":"http://old.example/"}'::jsonb)
		RETURNING id, updated_at, probe_updated_at`, folderID, neutralPath, ownerID,
	).Scan(&rowID, &updatedAt, &probeUpdatedAt); err != nil {
		t.Fatalf("insert collection row: %v", err)
	}
	before := readVirtualEvidenceRowJSON(t, pool, rowID)

	// Two cross-release variants: a different concrete pick under the same
	// neutral key, and a different neutral release. Neither may be adopted.
	for _, adoptPath := range []string{
		neutralPath + "?result=cand-b",
		"virtual://movie/tt-db-other?result=cand-a",
	} {
		args := models.VirtualFilePersistArgs{
			FileID: rowID, ExpectedFilePath: neutralPath,
			VideoTracks: []byte(`[{"codec":"av1"}]`), AudioTracks: []byte(`[]`), SubtitleTracks: []byte(`[]`),
			Resolution: "2160p", CodecVideo: "av1", CodecAudio: "eac3", Container: "mkv",
			HDR: true, Bitrate: 8000000, Duration: 5400,
			StampProbe: true, UpdatedAt: updatedAt, ProbeUpdatedAt: probeUpdatedAt,
			OwnerID: ownerID, LibraryID: folderID,
			AdoptPath: adoptPath, RequireAdopt: true,
		}
		result, err := ExecVirtualFileMetadataUpdateResult(ctx, pool, args)
		if !errors.Is(err, errVirtualAdoptIdentityNotPersisted) {
			t.Fatalf("adopt %q error = %v, want errVirtualAdoptIdentityNotPersisted", adoptPath, err)
		}
		if result.RowsAffected != 0 || result.MetadataUpdated || result.IdentityAdopted {
			t.Fatalf("adopt %q result = %+v, want a fully refused write", adoptPath, result)
		}
		if after := readVirtualEvidenceRowJSON(t, pool, rowID); after != before {
			t.Fatalf("collection row changed on a refused required adoption %q:\nbefore=%s\nafter =%s", adoptPath, before, after)
		}
	}
}

// TestVirtualFileMetadataUpdateNeutralRowAdoptsOwnReleaseCandidate is the
// allowed half of the neutral-row rule: a provider-neutral row receiving its
// own release's concrete candidate adopts it atomically, tracks and stamp
// included.
func TestVirtualFileMetadataUpdateNeutralRowAdoptsOwnReleaseCandidate(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994351, 7041
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	neutralPath := "virtual://movie/tt-db-neutral-own"
	adoptPath := neutralPath + "?result=cand-a"
	var rowID int
	var updatedAt time.Time
	var probeUpdatedAt *time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source,
			video_tracks)
		VALUES('movie-db-neutral-own', $1, $2, 'virtual', $3, 'virtual',
			'[{"codec":"h264","width":1920,"height":1080}]'::jsonb)
		RETURNING id, updated_at, probe_updated_at`, folderID, neutralPath, ownerID,
	).Scan(&rowID, &updatedAt, &probeUpdatedAt); err != nil {
		t.Fatalf("insert neutral row: %v", err)
	}

	args := models.VirtualFilePersistArgs{
		FileID: rowID, ExpectedFilePath: neutralPath,
		VideoTracks: []byte(`[{"codec":"av1","width":3840,"height":2160}]`), AudioTracks: []byte(`[]`), SubtitleTracks: []byte(`[]`),
		Resolution: "2160p", CodecVideo: "av1", CodecAudio: "eac3", Container: "mkv",
		HDR: true, Bitrate: 8000000, Duration: 5400,
		StampProbe: true, UpdatedAt: updatedAt, ProbeUpdatedAt: probeUpdatedAt,
		OwnerID: ownerID, LibraryID: folderID,
		AdoptPath: adoptPath, RequireAdopt: true,
	}
	result, err := ExecVirtualFileMetadataUpdateResult(ctx, pool, args)
	if err != nil {
		t.Fatalf("neutral row failed to adopt its own release candidate: %v", err)
	}
	if !result.IdentityAdopted || !result.MetadataUpdated || result.RowsAffected != 1 {
		t.Fatalf("result = %+v, want a confirmed adoption", result)
	}

	var path, resolution string
	var stampedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT file_path, resolution, probe_updated_at FROM media_files WHERE id = $1`, rowID).
		Scan(&path, &resolution, &stampedAt); err != nil {
		t.Fatalf("read neutral row: %v", err)
	}
	if path != adoptPath {
		t.Fatalf("file_path = %q, want the adopted own-release candidate %q", path, adoptPath)
	}
	if resolution != "2160p" {
		t.Fatalf("resolution = %q, want the probed 2160p", resolution)
	}
	if stampedAt == nil {
		t.Fatal("probe_updated_at was not stamped on the own-release adoption")
	}
}

// TestVirtualFileMetadataUpdateNeutralRowCrossReleaseSiblingRefused is the
// refused half: a sibling owns the different release's concrete path, so the
// required adoption matches no row. The neutral row must come out byte-identical
// and the sibling's own inventory must remain untouched.
func TestVirtualFileMetadataUpdateNeutralRowCrossReleaseSiblingRefused(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994352, 7042
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	neutralPath := "virtual://movie/tt-db-neutral-cross"
	siblingPath := "virtual://movie/tt-db-neutral-cross-other?result=cand-a"

	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source, video_tracks, resolution)
		VALUES('movie-db-neutral-cross', $1, $2, 'virtual', $3, 'virtual',
			'[{"codec":"h264","width":1920,"height":1080}]'::jsonb, '1080p')`,
		folderID, siblingPath, ownerID); err != nil {
		t.Fatalf("insert sibling row: %v", err)
	}

	var rowID int
	var updatedAt time.Time
	var probeUpdatedAt *time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source, video_tracks)
		VALUES('movie-db-neutral-cross', $1, $2, 'virtual', $3, 'virtual',
			'[{"codec":"hevc","width":3840,"height":2160}]'::jsonb)
		RETURNING id, updated_at, probe_updated_at`, folderID, neutralPath, ownerID,
	).Scan(&rowID, &updatedAt, &probeUpdatedAt); err != nil {
		t.Fatalf("insert neutral row: %v", err)
	}
	before := readVirtualEvidenceRowJSON(t, pool, rowID)

	args := models.VirtualFilePersistArgs{
		FileID: rowID, ExpectedFilePath: neutralPath,
		VideoTracks: []byte(`[{"codec":"av1","width":3840,"height":2160}]`), AudioTracks: []byte(`[]`), SubtitleTracks: []byte(`[]`),
		Resolution: "2160p", CodecVideo: "av1", CodecAudio: "eac3", Container: "mkv",
		HDR: true, Bitrate: 8000000, Duration: 5400,
		StampProbe: true, UpdatedAt: updatedAt, ProbeUpdatedAt: probeUpdatedAt,
		OwnerID: ownerID, LibraryID: folderID,
		AdoptPath: siblingPath, RequireAdopt: true,
	}
	result, err := ExecVirtualFileMetadataUpdateResult(ctx, pool, args)
	if !errors.Is(err, errVirtualAdoptIdentityNotPersisted) {
		t.Fatalf("error = %v, want errVirtualAdoptIdentityNotPersisted", err)
	}
	if result.RowsAffected != 0 || result.MetadataUpdated || result.IdentityAdopted {
		t.Fatalf("result = %+v, want a fully refused write", result)
	}

	after := readVirtualEvidenceRowJSON(t, pool, rowID)
	if after != before {
		t.Fatalf("neutral row changed on a refused cross-release adoption:\nbefore=%s\nafter =%s", before, after)
	}

	var siblingTracks, siblingResolution string
	if err := pool.QueryRow(ctx, `SELECT video_tracks::text, resolution FROM media_files WHERE file_path = $1 AND virtual_owner_installation_id = $2`, siblingPath, ownerID).
		Scan(&siblingTracks, &siblingResolution); err != nil {
		t.Fatalf("read sibling row: %v", err)
	}
	if siblingResolution != "1080p" || !strings.Contains(siblingTracks, "h264") {
		t.Fatalf("sibling row = (%s, %s), want its own 1080p h264 inventory", siblingResolution, siblingTracks)
	}
}
