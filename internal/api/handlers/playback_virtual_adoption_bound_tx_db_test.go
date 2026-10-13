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

// adoptionSaverOutcome carries one async saver result back to the test.
type adoptionSaverOutcome struct {
	result VirtualFileMetadataUpdateResult
	err    error
}

// waitForAdoptionLockWaiter polls pg_stat_activity until some backend is blocked
// by blockerPID, i.e. a lock wait on the row (or its unique index) the blocker
// holds. Waiting on observable lock state keeps the interleave tests
// deterministic: the commit is only released once the other transaction is
// provably queued behind the row-first order.
func waitForAdoptionLockWaiter(t *testing.T, pool *pgxpool.Pool, blockerPID int) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var blocked bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM pg_stat_activity waiter
				WHERE $1 = ANY(pg_blocking_pids(waiter.pid)))`, blockerPID).Scan(&blocked); err != nil {
			t.Fatalf("poll row-lock waiter: %v", err)
		}
		if blocked {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no transaction ever waited behind the held row lock")
}

// adoptionSuccessArgs builds a same-path evidence write that lands on row id.
func adoptionSuccessArgs(id int, path string, ownerID, folderID int, updatedAt time.Time, probeUpdatedAt *time.Time) models.VirtualFilePersistArgs {
	return models.VirtualFilePersistArgs{
		FileID: id, ExpectedFilePath: path,
		VideoTracks: []byte(`[{"codec":"hevc"}]`), AudioTracks: []byte(`[]`), SubtitleTracks: []byte(`[]`),
		Resolution: "2160p", CodecVideo: "hevc", CodecAudio: "eac3", Container: "mkv",
		StampProbe: true, UpdatedAt: updatedAt, ProbeUpdatedAt: probeUpdatedAt,
		OwnerID: ownerID, LibraryID: folderID,
	}
}

// TestVirtualAdoptionRefusalNonRequiredZeroRowDoesNotCountDB is the blocker-1
// regression against the real schema: an ordinary metadata CAS miss on a
// non-required write leaves the exact-pin counter and the row's failed_at
// verdict untouched, so ordinary misses can never replenish the bound after a
// clear.
func TestVirtualAdoptionRefusalNonRequiredZeroRowDoesNotCountDB(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994441, 7441
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	path := "virtual://movie/tt-db-nonrequired-zero?result=pin"
	id, updatedAt, probeUpdatedAt := insertVirtualAdoptionRow(t, pool, "movie-db-nonrequired-zero", folderID, ownerID, path)
	repo := scanner.NewFileRepository(pool)
	for i := 0; i < virtualAdoptionRefusalBound; i++ {
		if _, err := repo.RecordVirtualAdoptionRefusal(ctx, id, path, virtualAdoptionRefusalTTL); err != nil {
			t.Fatalf("seed refusal: %v", err)
		}
	}

	args := adoptionSuccessArgs(id, path, ownerID, folderID, updatedAt.Add(-time.Hour), probeUpdatedAt)
	result, err := ExecVirtualFileMetadataUpdateResultWithRefusals(ctx, pool, args, repo)
	if err != nil {
		t.Fatalf("non-required stale write returned an error: %v", err)
	}
	if result.MetadataUpdated || result.RowsAffected != 0 {
		t.Fatalf("result = %+v, want a zero-row miss", result)
	}
	count, err := repo.CountVirtualAdoptionRefusal(ctx, id, path)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != virtualAdoptionRefusalBound {
		t.Fatalf("refusal count = %d, want the pre-existing %d (an ordinary miss must not increment)", count, virtualAdoptionRefusalBound)
	}
	var failedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT failed_at FROM media_files WHERE id = $1`, id).Scan(&failedAt); err != nil {
		t.Fatalf("read failed_at: %v", err)
	}
	if failedAt != nil {
		t.Fatalf("failed_at = %v, want NULL (an ordinary miss must not stamp a terminal)", failedAt)
	}
}

// TestVirtualAdoptionRefusalRowLockSerializesEvidenceAndClearDB proves the
// row-first serialization against Postgres: while one transaction holds the
// media_files row lock and has recorded a refusal, a required-adoption success
// on another connection waits behind that lock, then commits its evidence and
// its exact-pin clear together. The success observes the post-commit count
// rather than racing it, and leaves the count cleared.
func TestVirtualAdoptionRefusalRowLockSerializesEvidenceAndClearDB(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994442, 7442
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	path := "virtual://movie/tt-db-lock-wait?result=pin"
	adoptPath := "virtual://movie/tt-db-lock-wait?result=fresh"
	id, updatedAt, probeUpdatedAt := insertVirtualAdoptionRow(t, pool, "movie-db-lock-wait", folderID, ownerID, path)
	repo := scanner.NewFileRepository(pool)

	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	defer func() { _ = blocker.Rollback(context.Background()) }()
	var blockerPID int
	if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatalf("blocker pid: %v", err)
	}
	var lockedID int
	if err := blocker.QueryRow(ctx, `SELECT id FROM media_files WHERE id = $1 FOR UPDATE`, id).Scan(&lockedID); err != nil {
		t.Fatalf("blocker lock: %v", err)
	}
	if _, err := repo.RecordVirtualAdoptionRefusalTx(ctx, blocker, id, path, virtualAdoptionRefusalTTL); err != nil {
		t.Fatalf("blocker refusal: %v", err)
	}

	args := adoptionSuccessArgs(id, path, ownerID, folderID, updatedAt, probeUpdatedAt)
	args.AdoptPath = adoptPath
	args.RequireAdopt = true
	done := make(chan adoptionSaverOutcome, 1)
	go func() {
		result, err := ExecVirtualFileMetadataUpdateResultWithRefusals(ctx, pool, args, repo)
		done <- adoptionSaverOutcome{result: result, err: err}
	}()

	waitForAdoptionLockWaiter(t, pool, blockerPID)
	if err := blocker.Commit(ctx); err != nil {
		t.Fatalf("commit blocker: %v", err)
	}
	got := <-done
	if got.err != nil {
		t.Fatalf("waiting success write failed: %v", got.err)
	}
	if !got.result.MetadataUpdated || !got.result.IdentityAdopted {
		t.Fatalf("result = %+v, want the adopted evidence write", got.result)
	}
	count, err := repo.CountVirtualAdoptionRefusal(ctx, id, path)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("refusal count = %d, want the serialized success to have cleared it", count)
	}
}

// TestVirtualAdoptionRefusalWaitsBehindEvidenceAndRestartsDB is the reverse
// interleave: a committed evidence write advances the revision and clears the
// count inside one held transaction, and a required refusal queued behind the
// row lock then observes the cleared state — it records one fresh refusal, not
// the pre-success bound, and installs no terminal.
func TestVirtualAdoptionRefusalWaitsBehindEvidenceAndRestartsDB(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994446, 7446
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	path := "virtual://movie/tt-db-reverse?result=pin"
	adoptPath := "virtual://movie/tt-db-reverse?result=foreign"
	id, updatedAt, probeUpdatedAt := insertVirtualAdoptionRow(t, pool, "movie-db-reverse", folderID, ownerID, path)
	repo := scanner.NewFileRepository(pool)
	for i := 0; i < virtualAdoptionRefusalBound; i++ {
		if _, err := repo.RecordVirtualAdoptionRefusal(ctx, id, path, virtualAdoptionRefusalTTL); err != nil {
			t.Fatalf("seed refusal: %v", err)
		}
	}

	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	defer func() { _ = blocker.Rollback(context.Background()) }()
	var blockerPID int
	if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatalf("blocker pid: %v", err)
	}
	var lockedID int
	if err := blocker.QueryRow(ctx, `SELECT id FROM media_files WHERE id = $1 FOR UPDATE`, id).Scan(&lockedID); err != nil {
		t.Fatalf("blocker lock: %v", err)
	}
	// Emulate the committed success: advance the evidence revision and clear the
	// exact-pin mark, still uncommitted behind the held row lock.
	if _, err := blocker.Exec(ctx, `
		UPDATE media_files
		SET resolution = '2160p',
		    updated_at = GREATEST(clock_timestamp(), updated_at + interval '1 microsecond'),
		    probe_updated_at = GREATEST(clock_timestamp(), probe_updated_at + interval '1 microsecond')
		WHERE id = $1`, id); err != nil {
		t.Fatalf("blocker evidence write: %v", err)
	}
	if _, err := blocker.Exec(ctx, `DELETE FROM virtual_adoption_refusals WHERE file_id = $1 AND expected_path = $2`, id, path); err != nil {
		t.Fatalf("blocker clear: %v", err)
	}

	args := adoptionSuccessArgs(id, path, ownerID, folderID, updatedAt, probeUpdatedAt)
	args.AdoptPath = adoptPath
	args.RequireAdopt = true
	done := make(chan adoptionSaverOutcome, 1)
	go func() {
		result, err := ExecVirtualFileMetadataUpdateResultWithRefusals(ctx, pool, args, repo)
		done <- adoptionSaverOutcome{result: result, err: err}
	}()

	waitForAdoptionLockWaiter(t, pool, blockerPID)
	if err := blocker.Commit(ctx); err != nil {
		t.Fatalf("commit blocker: %v", err)
	}
	got := <-done
	if got.err == nil {
		t.Fatal("a refusal behind an advanced revision reported success")
	}
	count, err := repo.CountVirtualAdoptionRefusal(ctx, id, path)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("refusal count = %d, want one fresh refusal after the serialized success cleared the bound", count)
	}
	var failedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT failed_at FROM media_files WHERE id = $1`, id).Scan(&failedAt); err != nil {
		t.Fatalf("read failed_at: %v", err)
	}
	if failedAt != nil {
		t.Fatalf("failed_at = %v, want NULL below the bound", failedAt)
	}
}

// TestVirtualAdoptionRefusalLockedRecorderCancelsUnderContentionDB proves the
// row-first recorder is a real transaction: while another transaction holds the
// row lock, a recorder with an expiring context aborts on the lock wait, rolls
// its transaction back, and records nothing.
func TestVirtualAdoptionRefusalLockedRecorderCancelsUnderContentionDB(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994443, 7443
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	path := "virtual://movie/tt-db-cancel-locked?result=pin"
	id, _, _ := insertVirtualAdoptionRow(t, pool, "movie-db-cancel-locked", folderID, ownerID, path)
	repo := scanner.NewFileRepository(pool)

	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	defer func() { _ = blocker.Rollback(context.Background()) }()
	var blockerPID int
	if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatalf("blocker pid: %v", err)
	}
	var lockedID int
	if err := blocker.QueryRow(ctx, `SELECT id FROM media_files WHERE id = $1 FOR UPDATE`, id).Scan(&lockedID); err != nil {
		t.Fatalf("blocker lock: %v", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := repo.RecordVirtualAdoptionRefusalLocked(waitCtx, id, path, virtualAdoptionRefusalTTL)
		done <- err
	}()
	waitForAdoptionLockWaiter(t, pool, blockerPID)
	if err := <-done; err == nil {
		t.Fatal("a cancelled row-lock wait reported success")
	} else if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled recorder error = %v, want a context cancellation", err)
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatalf("release blocker: %v", err)
	}
	count, err := repo.CountVirtualAdoptionRefusal(context.Background(), id, path)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("refusal count = %d, want the cancelled increment rolled back", count)
	}
}

// cancelClearRefusalStore cancels the caller's context when the exact-pin clear
// runs, modelling a cancellation between the evidence statement and the clear.
type cancelClearRefusalStore struct {
	*scanner.FileRepository
	cancel context.CancelFunc
}

func (s cancelClearRefusalStore) ClearVirtualAdoptionRefusalTx(context.Context, pgx.Tx, int, string) error {
	s.cancel()
	return context.Canceled
}

// TestVirtualAdoptionRefusalClearCancellationRollsBackEvidenceDB proves a
// cancellation at the evidence/clear boundary rolls the whole transaction back:
// the row's evidence and the refusal mark are both untouched, so a cancelled
// write can never commit evidence behind a still-live threshold.
func TestVirtualAdoptionRefusalClearCancellationRollsBackEvidenceDB(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994444, 7444
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	path := "virtual://movie/tt-db-cancel-clear?result=pin"
	id, updatedAt, probeUpdatedAt := insertVirtualAdoptionRow(t, pool, "movie-db-cancel-clear", folderID, ownerID, path)
	repo := scanner.NewFileRepository(pool)
	if _, err := repo.RecordVirtualAdoptionRefusal(ctx, id, path, virtualAdoptionRefusalTTL); err != nil {
		t.Fatalf("seed refusal: %v", err)
	}

	writeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	args := adoptionSuccessArgs(id, path, ownerID, folderID, updatedAt, probeUpdatedAt)
	if _, err := ExecVirtualFileMetadataUpdateResultWithRefusals(writeCtx, pool, args, cancelClearRefusalStore{FileRepository: repo, cancel: cancel}); err == nil {
		t.Fatal("a cancelled clear reported a committed write")
	}
	var resolution string
	if err := pool.QueryRow(context.Background(), `SELECT COALESCE(resolution, '') FROM media_files WHERE id = $1`, id).Scan(&resolution); err != nil {
		t.Fatalf("read resolution: %v", err)
	}
	if resolution != "" {
		t.Fatalf("resolution = %q, want the evidence write rolled back", resolution)
	}
	count, err := repo.CountVirtualAdoptionRefusal(context.Background(), id, path)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("refusal count = %d, want the pre-write mark preserved", count)
	}
}

// TestVirtualAdoptionCollisionSavepointRecoveryDB proves the savepoint recovery
// against the real unique index. A peer transaction holds the candidate path's
// uncommitted media_files_virtual_file_owner_key entry, invisible to the
// sibling guard, so the saver's adoption UPDATE raises a real Postgres 23505.
// The saver rewinds to its savepoint, the metadata-only retry lands on the
// current path, and the exact-pin clear commits with it. Without the rewind the
// retry would run in an aborted transaction; the success proves the real
// aborted-txn state is cleared.
func TestVirtualAdoptionCollisionSavepointRecoveryDB(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994445, 7445
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	candidate := "virtual://movie/tt-db-collision?result=fresh"
	path := "virtual://movie/tt-db-collision?result=pinned"
	var peerID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source)
		VALUES('movie-db-collision', $1, $2, 'virtual', $3, 'virtual')
		RETURNING id`, folderID, "virtual://movie/tt-db-collision?result=peer", ownerID).Scan(&peerID); err != nil {
		t.Fatalf("insert peer row: %v", err)
	}
	id, updatedAt, probeUpdatedAt := insertVirtualAdoptionRow(t, pool, "movie-db-collision", folderID, ownerID, path)
	repo := scanner.NewFileRepository(pool)
	if _, err := repo.RecordVirtualAdoptionRefusal(ctx, id, path, virtualAdoptionRefusalTTL); err != nil {
		t.Fatalf("seed refusal: %v", err)
	}

	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	defer func() { _ = blocker.Rollback(context.Background()) }()
	var blockerPID int
	if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatalf("blocker pid: %v", err)
	}
	// The peer takes the candidate path's unique-index entry, still uncommitted
	// and therefore invisible to the saver's sibling guard.
	if _, err := blocker.Exec(ctx, `UPDATE media_files SET file_path = $1 WHERE id = $2`, candidate, peerID); err != nil {
		t.Fatalf("peer adoption: %v", err)
	}

	args := adoptionSuccessArgs(id, path, ownerID, folderID, updatedAt, probeUpdatedAt)
	args.AdoptPath = candidate
	done := make(chan adoptionSaverOutcome, 1)
	go func() {
		result, err := ExecVirtualFileMetadataUpdateResultWithRefusals(ctx, pool, args, repo)
		done <- adoptionSaverOutcome{result: result, err: err}
	}()

	waitForAdoptionLockWaiter(t, pool, blockerPID)
	if err := blocker.Commit(ctx); err != nil {
		t.Fatalf("commit blocker: %v", err)
	}
	got := <-done
	if got.err != nil {
		t.Fatalf("collision recovery failed: %v", got.err)
	}
	if !got.result.MetadataUpdated || got.result.IdentityAdopted {
		t.Fatalf("result = %+v, want a metadata-only retry without adoption", got.result)
	}
	var gotPath, resolution string
	if err := pool.QueryRow(ctx, `SELECT file_path, COALESCE(resolution, '') FROM media_files WHERE id = $1`, id).Scan(&gotPath, &resolution); err != nil {
		t.Fatalf("read target row: %v", err)
	}
	if gotPath != path {
		t.Fatalf("file_path = %q, want the un-adopted current path %q", gotPath, path)
	}
	if resolution != "2160p" {
		t.Fatalf("resolution = %q, want the retried evidence persisted", resolution)
	}
	var peerPath string
	if err := pool.QueryRow(ctx, `SELECT file_path FROM media_files WHERE id = $1`, peerID).Scan(&peerPath); err != nil {
		t.Fatalf("read peer row: %v", err)
	}
	if peerPath != candidate {
		t.Fatalf("peer path = %q, want %q (unchanged)", peerPath, candidate)
	}
	count, err := repo.CountVirtualAdoptionRefusal(ctx, id, path)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("refusal count = %d, want the retry to have cleared it atomically", count)
	}
}
