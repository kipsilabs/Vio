package handlers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/scanner"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests exercise the transactional reconciliation against the real
// migrated schema: the evidence write and the exact-pin clear commit together,
// a clear failure rolls the evidence back, the terminal stamp is fenced on the
// observed evidence revision, and a concurrent success wins over a stale
// terminal. They skip cleanly when SILO_TEST_DATABASE_URL is unset.

// insertVirtualAdoptionRow seeds one pinned virtual row and returns its id and
// the evidence revision the caller must snapshot.
func insertVirtualAdoptionRow(t *testing.T, pool *pgxpool.Pool, contentID string, folderID, ownerID int, path string) (int, time.Time, *time.Time) {
	t.Helper()
	var id int
	var updatedAt time.Time
	var probeUpdatedAt *time.Time
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source)
		VALUES($1, $2, $3, 'virtual', $4, 'virtual')
		RETURNING id, updated_at, probe_updated_at`, contentID, folderID, path, ownerID,
	).Scan(&id, &updatedAt, &probeUpdatedAt); err != nil {
		t.Fatalf("insert virtual row %s: %v", contentID, err)
	}
	return id, updatedAt, probeUpdatedAt
}

// TestVirtualAdoptionRefusalClearCommitsWithEvidenceDB proves the core fix
// against Postgres: a successful evidence write empties the exact-pin refusal
// mark in the same transaction, so a committed row can never leave a stale
// threshold behind.
func TestVirtualAdoptionRefusalClearCommitsWithEvidenceDB(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994431, 7431
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	path := "virtual://movie/tt-db-tx-clear?result=pin"
	id, updatedAt, probeUpdatedAt := insertVirtualAdoptionRow(t, pool, "movie-db-tx-clear", folderID, ownerID, path)
	repo := scanner.NewFileRepository(pool)
	for i := 0; i < virtualAdoptionRefusalBound; i++ {
		if _, err := repo.RecordVirtualAdoptionRefusal(ctx, id, path, virtualAdoptionRefusalTTL); err != nil {
			t.Fatalf("seed refusal: %v", err)
		}
	}

	args := models.VirtualFilePersistArgs{
		FileID: id, ExpectedFilePath: path,
		VideoTracks: []byte(`[{"codec":"hevc"}]`), AudioTracks: []byte(`[]`), SubtitleTracks: []byte(`[]`),
		Resolution: "2160p", CodecVideo: "hevc", CodecAudio: "eac3", Container: "mkv",
		StampProbe: true, UpdatedAt: updatedAt, ProbeUpdatedAt: probeUpdatedAt,
		OwnerID: ownerID, LibraryID: folderID,
	}
	result, err := ExecVirtualFileMetadataUpdateResultWithRefusals(ctx, pool, args, repo)
	if err != nil {
		t.Fatalf("transactional evidence write failed: %v", err)
	}
	if !result.MetadataUpdated {
		t.Fatalf("result = %+v, want a metadata update", result)
	}
	count, err := repo.CountVirtualAdoptionRefusal(ctx, id, path)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("refusal count after the transactional write = %d, want 0", count)
	}
	var resolution string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(resolution, '') FROM media_files WHERE id = $1`, id).Scan(&resolution); err != nil {
		t.Fatalf("read resolution: %v", err)
	}
	if resolution != "2160p" {
		t.Fatalf("resolution = %q, want the evidence persisted", resolution)
	}
}

// failingClearRefusalStore is a durable refusal store whose transactional clear
// always fails, so the saver must roll the evidence write back rather than
// commit a row behind a still-live threshold.
type failingClearRefusalStore struct {
	*scanner.FileRepository
}

func (failingClearRefusalStore) ClearVirtualAdoptionRefusalTx(context.Context, pgx.Tx, int, string) error {
	return errors.New("clear unavailable")
}

// TestVirtualAdoptionRefusalClearFailureRollsBackEvidenceDB proves the atomicity
// against Postgres: a failing exact-pin clear rolls the whole transaction back,
// leaving both the row's evidence and the refusal mark untouched.
func TestVirtualAdoptionRefusalClearFailureRollsBackEvidenceDB(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994432, 7432
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	path := "virtual://movie/tt-db-tx-rollback?result=pin"
	id, updatedAt, probeUpdatedAt := insertVirtualAdoptionRow(t, pool, "movie-db-tx-rollback", folderID, ownerID, path)
	repo := scanner.NewFileRepository(pool)
	if _, err := repo.RecordVirtualAdoptionRefusal(ctx, id, path, virtualAdoptionRefusalTTL); err != nil {
		t.Fatalf("seed refusal: %v", err)
	}

	args := models.VirtualFilePersistArgs{
		FileID: id, ExpectedFilePath: path,
		VideoTracks: []byte(`[{"codec":"hevc"}]`), AudioTracks: []byte(`[]`), SubtitleTracks: []byte(`[]`),
		Resolution: "2160p", CodecVideo: "hevc", CodecAudio: "eac3", Container: "mkv",
		StampProbe: true, UpdatedAt: updatedAt, ProbeUpdatedAt: probeUpdatedAt,
		OwnerID: ownerID, LibraryID: folderID,
	}
	_, err := ExecVirtualFileMetadataUpdateResultWithRefusals(ctx, pool, args, failingClearRefusalStore{FileRepository: repo})
	if err == nil {
		t.Fatal("a failing refusal clear reported a committed write")
	}
	var resolution string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(resolution, '') FROM media_files WHERE id = $1`, id).Scan(&resolution); err != nil {
		t.Fatalf("read resolution: %v", err)
	}
	if resolution != "" {
		t.Fatalf("resolution = %q, want the evidence write rolled back", resolution)
	}
	count, err := repo.CountVirtualAdoptionRefusal(ctx, id, path)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("refusal count = %d, want the pre-write mark preserved", count)
	}
}

// TestVirtualAdoptionTerminalRefusalRevisionFenceDB proves the terminal stamp is
// fenced on the observed evidence revision: a stale revision is a no-op and the
// current revision lands the verdict.
func TestVirtualAdoptionTerminalRefusalRevisionFenceDB(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994433, 7433
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	path := "virtual://movie/tt-db-terminal-fence?result=pin"
	id, staleUpdated, staleProbe := insertVirtualAdoptionRow(t, pool, "movie-db-terminal-fence", folderID, ownerID, path)
	repo := scanner.NewFileRepository(pool)

	// Another replica commits a newer evidence write: the revision advances.
	if _, err := pool.Exec(ctx, `
		UPDATE media_files SET updated_at = updated_at + interval '1 hour', probe_updated_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("advance revision: %v", err)
	}

	stamped, err := repo.MarkVirtualAdoptionTerminalRefusal(ctx, id, path, nil, staleUpdated, staleProbe)
	if err != nil {
		t.Fatalf("stale terminal stamp: %v", err)
	}
	if stamped {
		t.Fatal("a stale revision installed a terminal verdict")
	}
	var failedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT failed_at FROM media_files WHERE id = $1`, id).Scan(&failedAt); err != nil {
		t.Fatalf("read failed_at: %v", err)
	}
	if failedAt != nil {
		t.Fatalf("failed_at = %v, want NULL after a stale terminal attempt", failedAt)
	}

	var freshUpdated time.Time
	var freshProbe *time.Time
	if err := pool.QueryRow(ctx, `SELECT updated_at, probe_updated_at FROM media_files WHERE id = $1`, id).Scan(&freshUpdated, &freshProbe); err != nil {
		t.Fatalf("read fresh revision: %v", err)
	}
	stamped, err = repo.MarkVirtualAdoptionTerminalRefusal(ctx, id, path, nil, freshUpdated, freshProbe)
	if err != nil {
		t.Fatalf("fresh terminal stamp: %v", err)
	}
	if !stamped {
		t.Fatal("the current revision did not install the terminal verdict")
	}
	if err := pool.QueryRow(ctx, `SELECT failed_at FROM media_files WHERE id = $1`, id).Scan(&failedAt); err != nil {
		t.Fatalf("read failed_at: %v", err)
	}
	if failedAt == nil {
		t.Fatal("failed_at is NULL after a landed terminal verdict")
	}
}

// TestVirtualAdoptionTerminalSupersededByConcurrentEvidenceDB proves the
// cross-replica interleave: replica B commits evidence (advancing the revision
// and clearing the count) through the transactional saver, and replica A's
// stale terminal decision is a no-op rather than a condemnation.
func TestVirtualAdoptionTerminalSupersededByConcurrentEvidenceDB(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994434, 7434
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	path := "virtual://movie/tt-db-two-replica?result=pin"
	id, staleUpdated, staleProbe := insertVirtualAdoptionRow(t, pool, "movie-db-two-replica", folderID, ownerID, path)
	repo := scanner.NewFileRepository(pool)
	for i := 0; i < virtualAdoptionRefusalBound; i++ {
		if _, err := repo.RecordVirtualAdoptionRefusal(ctx, id, path, virtualAdoptionRefusalTTL); err != nil {
			t.Fatalf("seed refusal: %v", err)
		}
	}

	// Replica B's successful evidence write advances the revision and clears the
	// count in one transaction.
	args := models.VirtualFilePersistArgs{
		FileID: id, ExpectedFilePath: path,
		VideoTracks: []byte(`[{"codec":"hevc"}]`), AudioTracks: []byte(`[]`), SubtitleTracks: []byte(`[]`),
		Resolution: "2160p", CodecVideo: "hevc", CodecAudio: "eac3", Container: "mkv",
		StampProbe: true, UpdatedAt: staleUpdated, ProbeUpdatedAt: staleProbe,
		OwnerID: ownerID, LibraryID: folderID,
	}
	if result, err := ExecVirtualFileMetadataUpdateResultWithRefusals(ctx, pool, args, repo); err != nil {
		t.Fatalf("replica B write failed: %v", err)
	} else if !result.MetadataUpdated {
		t.Fatalf("replica B result = %+v, want a metadata update", result)
	}

	// Replica A's terminal decision was taken from the pre-write revision.
	stamped, err := repo.MarkVirtualAdoptionTerminalRefusal(ctx, id, path, nil, staleUpdated, staleProbe)
	if err != nil {
		t.Fatalf("stale terminal stamp: %v", err)
	}
	if stamped {
		t.Fatal("a stale terminal condemned a row a newer evidence write had advanced")
	}
	var failedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT failed_at FROM media_files WHERE id = $1`, id).Scan(&failedAt); err != nil {
		t.Fatalf("read failed_at: %v", err)
	}
	if failedAt != nil {
		t.Fatalf("failed_at = %v, want NULL after the interleave", failedAt)
	}
	count, err := repo.CountVirtualAdoptionRefusal(ctx, id, path)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("refusal count = %d, want the concurrent success to have cleared it", count)
	}
}

// TestVirtualAdoptionTerminalStampedInEvidenceTransactionDB proves the refusal
// transaction installs the terminal verdict from its own in-transaction count:
// a required adoption a sibling owns is refused, the increment reaches the
// bound, and the verdict lands with the same commit.
func TestVirtualAdoptionTerminalStampedInEvidenceTransactionDB(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994435, 7435
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	path := "virtual://movie/tt-db-tx-terminal?result=pinned"
	adoptPath := "virtual://movie/tt-db-tx-terminal?result=sibling"
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source)
		VALUES('movie-db-tx-terminal', $1, $2, 'virtual', $3, 'virtual')`, folderID, adoptPath, ownerID); err != nil {
		t.Fatalf("insert sibling row: %v", err)
	}
	id, updatedAt, probeUpdatedAt := insertVirtualAdoptionRow(t, pool, "movie-db-tx-terminal", folderID, ownerID, path)
	repo := scanner.NewFileRepository(pool)
	for i := 0; i < virtualAdoptionRefusalBound-1; i++ {
		if _, err := repo.RecordVirtualAdoptionRefusal(ctx, id, path, virtualAdoptionRefusalTTL); err != nil {
			t.Fatalf("seed refusal: %v", err)
		}
	}

	args := models.VirtualFilePersistArgs{
		FileID: id, ExpectedFilePath: path, AdoptPath: adoptPath, RequireAdopt: true,
		VideoTracks: []byte(`[{"codec":"hevc"}]`), AudioTracks: []byte(`[]`), SubtitleTracks: []byte(`[]`),
		Resolution: "2160p", CodecVideo: "hevc", CodecAudio: "eac3", Container: "mkv",
		StampProbe: true, UpdatedAt: updatedAt, ProbeUpdatedAt: probeUpdatedAt,
		OwnerID: ownerID, LibraryID: folderID,
		RefusalReason: virtualProbeRefusalSiblingOwner,
	}
	if _, err := ExecVirtualFileMetadataUpdateResultWithRefusals(ctx, pool, args, repo); err == nil {
		t.Fatal("a sibling-owned required adoption reported success")
	}
	count, err := repo.CountVirtualAdoptionRefusal(ctx, id, path)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != virtualAdoptionRefusalBound {
		t.Fatalf("refusal count = %d, want the bound %d", count, virtualAdoptionRefusalBound)
	}
	var failedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT failed_at FROM media_files WHERE id = $1`, id).Scan(&failedAt); err != nil {
		t.Fatalf("read failed_at: %v", err)
	}
	if failedAt == nil {
		t.Fatal("the in-transaction count reached the bound but no terminal verdict landed")
	}
}
