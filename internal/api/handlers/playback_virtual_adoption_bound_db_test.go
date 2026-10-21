package handlers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/scanner"
)

// These tests exercise the bounded evidence-adoption decisions against the real
// migrated schema: the CAS fence, the failed-verdict fence, and the durable
// terminal refusal. The unit tests elsewhere assert the pure decision functions;
// these prove the SQL and the fenced failed_at marker actually behave as the
// decisions assume, which a fake QueryRow cannot.

// TestVirtualAdoptionBoundDBCASStaleSnapshotIsZeroRow proves the CAS fence
// still rejects a genuinely stale snapshot against the migrated schema: a write
// whose captured probe stamp no longer matches leaves the whole row
// byte-identical. A catalog re-list that advances only updated_at is handled by
// the bounded revision retry and is covered separately.
func TestVirtualAdoptionBoundDBCASStaleSnapshotIsZeroRow(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994401, 7401
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	path := "virtual://movie/tt-db-cas?result=pin"
	var id int
	var updatedAt time.Time
	var probeUpdatedAt *time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source)
		VALUES('movie-db-cas', $1, $2, 'virtual', $3, 'virtual')
		RETURNING id, updated_at, probe_updated_at`, folderID, path, ownerID,
	).Scan(&id, &updatedAt, &probeUpdatedAt); err != nil {
		t.Fatalf("insert row: %v", err)
	}
	before := readVirtualEvidenceRowJSON(t, pool, id)
	// Another probe committed after the caller's snapshot. The path is
	// unchanged, but the probe stamp is not, so this is not catalog re-list
	// churn and the revision retry must not re-base it.
	if _, err := pool.Exec(ctx, `UPDATE media_files SET probe_updated_at = now(), updated_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("advance probe stamp: %v", err)
	}
	before = readVirtualEvidenceRowJSON(t, pool, id)

	args := models.VirtualFilePersistArgs{
		FileID:           id,
		ExpectedFilePath: path,
		Resolution:       "2160p",
		CodecVideo:       "hevc",
		CodecAudio:       "eac3",
		Container:        "mkv",
		StampProbe:       true,
		UpdatedAt:        updatedAt.Add(-time.Hour), // stale generation
		ProbeUpdatedAt:   probeUpdatedAt,
		OwnerID:          ownerID,
		LibraryID:        folderID,
	}
	result, err := ExecVirtualFileMetadataUpdateResult(ctx, pool, args)
	if err != nil {
		t.Fatalf("stale CAS write returned a hard error: %v", err)
	}
	if result.MetadataUpdated || result.RowsAffected != 0 {
		t.Fatalf("stale CAS write reported commit: %+v", result)
	}
	if after := readVirtualEvidenceRowJSON(t, pool, id); after != before {
		t.Fatalf("stale CAS write mutated the row:\n before: %s\n after:  %s", before, after)
	}
}

// TestVirtualAdoptionBoundDBFailedVerdictFencesAdoption proves a live failed_at
// verdict on the candidate's neutral identity refuses the whole required
// adoption: the candidate row keeps its own path and the probe evidence and
// stamp never land. This is the fence blocker 2 must not bypass.
func TestVirtualAdoptionBoundDBFailedVerdictFencesAdoption(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994402, 7402
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	const neutral = "virtual://movie/tt-db-verdict"
	pinPath := neutral + "?result=pin"
	adoptPath := neutral + "?result=fresh"

	var id int
	var updatedAt time.Time
	var probeUpdatedAt *time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source)
		VALUES('movie-db-verdict', $1, $2, 'virtual', $3, 'virtual')
		RETURNING id, updated_at, probe_updated_at`, folderID, pinPath, ownerID,
	).Scan(&id, &updatedAt, &probeUpdatedAt); err != nil {
		t.Fatalf("insert pinned row: %v", err)
	}
	// The failed row carries the candidate's provider-neutral identity, so the
	// verdict binds the adopted candidate without a sibling owning its exact path.
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source, failed_at)
		VALUES('movie-db-verdict', $1, $2, 'virtual', $3, 'virtual', NOW())`, folderID, neutral, ownerID); err != nil {
		t.Fatalf("insert failed row: %v", err)
	}
	before := readVirtualEvidenceRowJSON(t, pool, id)

	args := models.VirtualFilePersistArgs{
		FileID:           id,
		ExpectedFilePath: pinPath,
		Resolution:       "2160p",
		CodecVideo:       "hevc",
		CodecAudio:       "eac3",
		Container:        "mkv",
		StampProbe:       true,
		UpdatedAt:        updatedAt,
		ProbeUpdatedAt:   probeUpdatedAt,
		OwnerID:          ownerID,
		LibraryID:        folderID,
		AdoptPath:        adoptPath,
		RequireAdopt:     true,
	}
	if _, err := ExecVirtualFileMetadataUpdateResult(ctx, pool, args); !errors.Is(err, errVirtualAdoptIdentityNotPersisted) {
		t.Fatalf("required adoption past a live verdict returned %v, want %v", err, errVirtualAdoptIdentityNotPersisted)
	}
	if after := readVirtualEvidenceRowJSON(t, pool, id); after != before {
		t.Fatalf("verdict-fenced adoption mutated the row:\n before: %s\n after:  %s", before, after)
	}
}

// TestVirtualAdoptionTerminalRefusalStopsRetriesDB proves the durable terminal
// state actually stops the loop across a process boundary: the fenced marker
// persists failed_at on the row, and a later resolve gate reading the reloaded
// row refuses the candidate instead of retrying the adoption. The process-local
// refusal counter is irrelevant once this stamp exists.
func TestVirtualAdoptionTerminalRefusalStopsRetriesDB(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994403, 7403
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	path := "virtual://movie/tt-db-terminal?result=pin"
	var id int
	var updatedAt time.Time
	var probeUpdatedAt *time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source)
		VALUES('movie-db-terminal', $1, $2, 'virtual', $3, 'virtual')
		RETURNING id, updated_at, probe_updated_at`, folderID, path, ownerID,
	).Scan(&id, &updatedAt, &probeUpdatedAt); err != nil {
		t.Fatalf("insert row: %v", err)
	}
	row := &models.MediaFile{
		ID: id, ContentID: "movie-db-terminal", FilePath: path,
		MediaFolderID: folderID, VirtualOwnerInstallationID: ownerID,
		UpdatedAt: updatedAt, ProbeUpdatedAt: probeUpdatedAt,
	}
	h := &PlaybackHandler{}
	// A stale in-memory counter is irrelevant: the bound already fired, so the
	// terminal install is what must durably stop retries. The stamp is installed
	// by the store's revision-fenced method, the same one the transactional
	// refusal reconciliation calls with its own transaction.
	repo := scanner.NewFileRepository(pool)
	if stamped, err := repo.MarkVirtualAdoptionTerminalRefusal(ctx, row.ID, row.FilePath, row.FailedAt, row.UpdatedAt, row.ProbeUpdatedAt); err != nil {
		t.Fatalf("terminal refusal stamp: %v", err)
	} else if !stamped {
		t.Fatal("terminal refusal did not install the durable marker")
	}

	// Reload through a fresh read (a different replica's view of the catalog).
	reloaded := &models.MediaFile{ID: id, ContentID: "movie-db-terminal", FilePath: path,
		MediaFolderID: folderID, VirtualOwnerInstallationID: ownerID}
	if err := pool.QueryRow(ctx, `SELECT failed_at FROM media_files WHERE id = $1`, id).Scan(&reloaded.FailedAt); err != nil {
		t.Fatalf("reload failed_at: %v", err)
	}
	if reloaded.FailedAt == nil {
		t.Fatal("terminal refusal did not persist failed_at")
	}
	if !virtualCandidateVerdictActive(reloaded.FailedAt, time.Now()) {
		t.Fatal("persisted terminal verdict is not active")
	}

	// The next attempt's verdict gate must refuse the row instead of retrying.
	h.VirtualFileLookup = func(context.Context, string) (*models.MediaFile, error) {
		return reloaded, nil
	}
	err := h.virtualCandidateVerdictError(ctx, path, reloaded, ownerID, false)
	if !errors.Is(err, ErrVirtualCandidateMarkedFailed) {
		t.Fatalf("verdict gate after terminal refusal returned %v, want %v", err, ErrVirtualCandidateMarkedFailed)
	}
}
