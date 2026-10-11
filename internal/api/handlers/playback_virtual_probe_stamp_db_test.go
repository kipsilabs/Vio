package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// virtualStampFixtureRow inserts one virtual media row with the given
// probe_source and returns its id, updated_at and current probe stamp. The row
// starts with no inventory and no stamp, the state a registration or a
// metadata-only adoption can leave behind.
func virtualStampFixtureRow(t *testing.T, pool *pgxpool.Pool, contentID, path, probeSource string, folderID, ownerID int) (int, time.Time, *time.Time) {
	t.Helper()
	ctx := context.Background()
	var rowID int
	var updatedAt time.Time
	var probeUpdatedAt *time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source)
		VALUES($1, $2, $3, 'virtual', $4, NULLIF($5, ''))
		RETURNING id, updated_at, probe_updated_at`, contentID, folderID, path, ownerID, probeSource,
	).Scan(&rowID, &updatedAt, &probeUpdatedAt); err != nil {
		t.Fatalf("insert virtual row: %v", err)
	}
	return rowID, updatedAt, probeUpdatedAt
}

// execVirtualStampUpdate runs the real statement with explicit inventory and
// stamp flags and returns the row's probe stamp afterwards. It mirrors the
// argument shape ExecVirtualFileMetadataUpdateResult uses.
func execVirtualStampUpdate(t *testing.T, pool *pgxpool.Pool, rowID int, expectedPath string, updatedAt time.Time, probeUpdatedAt *time.Time, ownerID, folderID int, audioJSON, subtitleJSON string, stampProbe, clearProbe bool) *time.Time {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, VirtualFileMetadataUpdateSQL,
		`[]`, audioJSON, subtitleJSON,
		"1080p", "h264", "aac", "mkv", false, 8000000, 5400,
		rowID, expectedPath, stampProbe,
		updatedAt, probeUpdatedAt, ownerID, folderID, "",
		"", virtualFailedVerdictMaxAge.Seconds(), false, false,
		"", nil, "", "", "", 0,
		nil,
		false, // $30 replaceIdentity
		clearProbe,
		false, // $32 reconcileCollectionVariant
	); err != nil {
		t.Fatalf("VirtualFileMetadataUpdateSQL: %v", err)
	}
	var stamped *time.Time
	if err := pool.QueryRow(ctx, `SELECT probe_updated_at FROM media_files WHERE id = $1`, rowID).Scan(&stamped); err != nil {
		t.Fatalf("read probe_updated_at: %v", err)
	}
	return stamped
}

// A metadata-only write that lands codec-bearing audio inventory must stamp the
// row: real tracks may never persist alongside a NULL probe_updated_at, or the
// probe gate treats the row as never-probed forever.
func TestVirtualFileMetadataUpdateStampsCodecInventoryWithoutStampFlag(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	const folderID, ownerID = 994341, 7101
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	path := "virtual://movie/tt-stamp-codec?result=cand"
	rowID, updatedAt, probeUpdatedAt := virtualStampFixtureRow(t, pool, "movie-tt-stamp-codec", path, "virtual", folderID, ownerID)

	stamped := execVirtualStampUpdate(t, pool, rowID, path, updatedAt, probeUpdatedAt, ownerID, folderID,
		`[{"codec":"aac","channels":2,"language":"eng"}]`, `[]`, false, false)
	if stamped == nil {
		t.Fatal("codec-bearing audio persisted with probe_updated_at NULL")
	}
}

// A metadata-only write carrying only provider hint entries (language, no
// codec) is not real inventory: it must preserve the row's unstamped state so a
// later probe still runs.
func TestVirtualFileMetadataUpdatePreservesNullForHintOnlyInventory(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	const folderID, ownerID = 994342, 7102
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	path := "virtual://movie/tt-stamp-hint?result=cand"
	rowID, updatedAt, probeUpdatedAt := virtualStampFixtureRow(t, pool, "movie-tt-stamp-hint", path, "virtual", folderID, ownerID)

	stamped := execVirtualStampUpdate(t, pool, rowID, path, updatedAt, probeUpdatedAt, ownerID, folderID,
		`[{"language":"eng"}]`, `[{"language":"eng"}]`, false, false)
	if stamped != nil {
		t.Fatalf("hint-only inventory stamped probe_updated_at = %v, want NULL preserved", stamped)
	}
}

// ClearProbe is independent of the stamp coupling: an explicit clear (a release
// swap invalidating the evidence) nulls the stamp even when the write carries
// codec-bearing inventory.
func TestVirtualFileMetadataUpdateClearProbeWinsOverCodecCoupling(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	const folderID, ownerID = 994343, 7103
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	path := "virtual://movie/tt-stamp-clear?result=cand"
	rowID, updatedAt, probeUpdatedAt := virtualStampFixtureRow(t, pool, "movie-tt-stamp-clear", path, "virtual", folderID, ownerID)

	stamped := execVirtualStampUpdate(t, pool, rowID, path, updatedAt, probeUpdatedAt, ownerID, folderID,
		`[{"codec":"aac","channels":2,"language":"eng"}]`, `[]`, false, true)
	if stamped != nil {
		t.Fatalf("ClearProbe left probe_updated_at = %v, want NULL", stamped)
	}
}

// A collection-owned row keeps its own stamp, so the codec coupling does not
// stamp it: the collection materializer owns that row's lifecycle.
func TestVirtualFileMetadataUpdateCollectionRowKeepsOwnStamp(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	const folderID, ownerID = 994344, 7104
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	path := "virtual://movie/tt-stamp-collection?result=cand"
	rowID, updatedAt, probeUpdatedAt := virtualStampFixtureRow(t, pool, "movie-tt-stamp-collection", path, "virtual_collection", folderID, ownerID)

	stamped := execVirtualStampUpdate(t, pool, rowID, path, updatedAt, probeUpdatedAt, ownerID, folderID,
		`[{"codec":"aac","channels":2,"language":"eng"}]`, `[]`, false, false)
	if stamped != nil {
		t.Fatalf("collection row stamped probe_updated_at = %v, want its own (NULL) stamp preserved", stamped)
	}
}
