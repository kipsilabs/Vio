package handlers

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/scanner"
)

// errStalePinSimpleResolverUnexpected guards the DB tests against accidentally
// wiring the simple resolver: the recovery always uses the detailed one.
var errStalePinSimpleResolverUnexpected = errors.New("simple resolver must not be used when the detailed resolver is set")

// TestStalePinRecoveryLeavesDurablePinUnchangedInDatabase is the DB-backed proof
// the reviewer asked for: the recovery runs end to end against the real
// VirtualFileMetadataUpdate, and the requested row's durable pin comes out
// byte-identical. A struct-only assertion cannot show this — the dangerous path
// is the adoption-capable writer landing an UPDATE on the row — so the test reads
// the whole row back from Postgres and compares it. It also counts saver writes:
// with no existing owner the recovery must admit none.
func TestStalePinRecoveryLeavesDurablePinUnchangedInDatabase(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994461, 7161
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	const neutral = "virtual://movie/tt-stale-db-transient"
	pinnedPath := neutral + "?result=STALE"
	var rowID int
	var updatedAt time.Time
	var probeUpdatedAt *time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source,
			codec_video, codec_audio, resolution, file_size, duration)
		VALUES('movie-stale-db-transient', $1, $2, 'virtual', $3, 'virtual',
			'h264', 'aac', '1080p', 8500000000, 7200)
		RETURNING id, updated_at, probe_updated_at`, folderID, pinnedPath, ownerID,
	).Scan(&rowID, &updatedAt, &probeUpdatedAt); err != nil {
		t.Fatalf("insert identity-less pinned row: %v", err)
	}
	before := readVirtualEvidenceRowJSON(t, pool, rowID)

	repo := scanner.NewFileRepository(pool)
	row, err := repo.GetByID(ctx, rowID)
	if err != nil || row == nil {
		t.Fatalf("load pinned row: %v", err)
	}

	var saveCalls atomic.Int32
	h := &PlaybackHandler{
		fileResolver: repo,
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
			return "", errStalePinSimpleResolverUnexpected
		}),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{stalePinLiveStream(neutral, "NEW", row.FileSize, "h264")}, nil
		}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			return ResolvedVirtualMedia{URL: "http://127.0.0.1:8080/stream?uri=" + uri, URI: uri, CandidateID: virtualResultCandidateID(uri)}, nil
		}),
		VirtualPlaybackSourceProber: func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
			probed := stalePinProbedTrack(row.Duration)
			probed.ID = f.ID
			probed.FilePath = f.FilePath
			return probed, nil
		},
		VirtualFileSaver: func(writeCtx context.Context, args models.VirtualFilePersistArgs) (int64, error) {
			saveCalls.Add(1)
			return ExecVirtualFileMetadataUpdate(writeCtx, pool, args)
		},
	}

	if _, ok := h.recoverStaleIdentityLessPinV3(stalePinRecoveryRequest(), row, "profile-1"); !ok {
		t.Fatal("the unique fingerprint match should recover the pin")
	}
	h.StopVirtualEvidence()

	if got := saveCalls.Load(); got != 0 {
		t.Fatalf("evidence writes = %d, want 0: the recovery must not persist against the requested row", got)
	}
	if after := readVirtualEvidenceRowJSON(t, pool, rowID); after != before {
		t.Fatalf("identity-less pinned row changed during a transient recovery:\nbefore=%s\nafter =%s", before, after)
	}
	if got := virtualResultCandidateID(row.FilePath); got != "STALE" {
		t.Fatalf("in-memory row pin = %q, want the durable STALE unchanged", got)
	}
}

// TestStalePinRecoveryStampsExistingOwnerWithoutMovingPath is the allowed half
// against a real database: when a distinct row already owns the matched
// candidate's concrete path, the recovery persists the probed evidence against
// that row as a metadata-only write. The owner keeps its path; the requested
// row stays byte-identical.
func TestStalePinRecoveryStampsExistingOwnerWithoutMovingPath(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994462, 7162
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	const (
		neutral    = "virtual://movie/tt-stale-db-owner"
		pinnedPath = neutral + "?result=STALE"
		ownerPath  = neutral + "?result=NEW"
	)
	var pinnedID int
	var updatedAt time.Time
	var probeUpdatedAt *time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source,
			codec_video, codec_audio, resolution, file_size, duration)
		VALUES('movie-stale-db-owner', $1, $2, 'virtual', $3, 'virtual',
			'h264', 'aac', '1080p', 8500000000, 7200)
		RETURNING id, updated_at, probe_updated_at`, folderID, pinnedPath, ownerID,
	).Scan(&pinnedID, &updatedAt, &probeUpdatedAt); err != nil {
		t.Fatalf("insert pinned row: %v", err)
	}
	pinnedBefore := readVirtualEvidenceRowJSON(t, pool, pinnedID)

	var ownerID2 int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source,
			codec_video, codec_audio, resolution, file_size, duration)
		VALUES('movie-stale-db-owner', $1, $2, 'virtual', $3, 'virtual',
			'h264', 'aac', '1080p', 8500000000, 7200)
		RETURNING id`, folderID, ownerPath, ownerID,
	).Scan(&ownerID2); err != nil {
		t.Fatalf("insert owner row: %v", err)
	}

	repo := scanner.NewFileRepository(pool)
	row, err := repo.GetByID(ctx, pinnedID)
	if err != nil || row == nil {
		t.Fatalf("load pinned row: %v", err)
	}

	h := &PlaybackHandler{
		fileResolver: repo,
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
			return "", errStalePinSimpleResolverUnexpected
		}),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{stalePinLiveStream(neutral, "NEW", row.FileSize, "h264")}, nil
		}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			return ResolvedVirtualMedia{URL: "http://127.0.0.1:8080/stream?uri=" + uri, URI: uri, CandidateID: virtualResultCandidateID(uri)}, nil
		}),
		VirtualPlaybackSourceProber: func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
			probed := stalePinProbedTrack(row.Duration)
			probed.ID = f.ID
			probed.FilePath = f.FilePath
			return probed, nil
		},
		VirtualFileSaver: func(writeCtx context.Context, args models.VirtualFilePersistArgs) (int64, error) {
			return ExecVirtualFileMetadataUpdate(writeCtx, pool, args)
		},
	}

	if _, ok := h.recoverStaleIdentityLessPinV3(stalePinRecoveryRequest(), row, "profile-1"); !ok {
		t.Fatal("the unique fingerprint match should recover the pin")
	}
	h.StopVirtualEvidence()

	// The requested row must not have moved or gained evidence.
	if after := readVirtualEvidenceRowJSON(t, pool, pinnedID); after != pinnedBefore {
		t.Fatalf("requested row changed during recovery:\nbefore=%s\nafter =%s", pinnedBefore, after)
	}

	// The existing owner keeps its path and gains the probed inventory/stamp.
	var ownerPathAfter, ownerResolution string
	var ownerStampedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT file_path, resolution, probe_updated_at FROM media_files WHERE id = $1`, ownerID2).
		Scan(&ownerPathAfter, &ownerResolution, &ownerStampedAt); err != nil {
		t.Fatalf("read owner row: %v", err)
	}
	if ownerPathAfter != ownerPath {
		t.Fatalf("owner file_path = %q, want it unchanged at %q", ownerPathAfter, ownerPath)
	}
	if ownerStampedAt == nil {
		t.Fatal("owner row was not stamped with the probed evidence")
	}
	if len(ownerResolution) == 0 {
		t.Fatal("owner row lost its resolution evidence")
	}
}
