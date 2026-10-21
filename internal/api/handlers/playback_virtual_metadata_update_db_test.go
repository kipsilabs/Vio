package handlers

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/jackc/pgx/v5/pgxpool"
)

// virtualMetadataUpdateTestPool connects to the disposable migrated test
// database provisioned by TestMain and serializes fixture access with the same
// advisory lock the other destructive handler fixtures use. It skips cleanly
// when SILO_TEST_DATABASE_URL is unset.
func virtualMetadataUpdateTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	var databaseName string
	if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&databaseName); err != nil {
		t.Fatalf("identify test database: %v", err)
	}
	if !strings.Contains(strings.ToLower(databaseName), "test") && !strings.Contains(strings.ToLower(databaseName), "purge") {
		t.Fatalf("refusing destructive handler fixture database %q", databaseName)
	}
	lockConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire test database lock: %v", err)
	}
	if _, err := lockConn.Exec(ctx, "SELECT pg_advisory_lock($1)", int64(0x53494c4f5f544553)); err != nil {
		lockConn.Release()
		t.Fatalf("lock test database fixtures: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	t.Cleanup(func() {
		_, _ = lockConn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", int64(0x53494c4f5f544553))
		lockConn.Release()
	})
	return pool
}

// seedVirtualMetadataUpdateFolder inserts a throwaway library and returns its
// id together with a cleanup that removes every row the test created.
func seedVirtualMetadataUpdateFolder(t *testing.T, pool *pgxpool.Pool, folderID, ownerID int) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM media_files WHERE virtual_owner_installation_id = $1`, ownerID); err != nil {
		t.Fatalf("reset virtual rows: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM media_folders WHERE id = $1`, folderID); err != nil {
		t.Fatalf("reset library: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id, name, type, enabled) VALUES($1, 'probe evidence guard', 'movies', true)`, folderID); err != nil {
		t.Fatalf("insert library: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_files WHERE virtual_owner_installation_id = $1`, ownerID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_folders WHERE id = $1`, folderID)
	})
}

// runVirtualMetadataUpdate executes the real VirtualFileMetadataUpdateSQL with
// the same argument shape the router saver uses.
func runVirtualMetadataUpdate(t *testing.T, pool *pgxpool.Pool, candidateID int, expectedPath, adoptPath string, ownerID, folderID int, updatedAt time.Time, probeUpdatedAt *time.Time) {
	t.Helper()
	ctx := context.Background()
	neutralPath := ""
	if adoptPath != "" {
		neutralPath = virtualPlaybackNeutralKey(adoptPath)
	}
	tag, err := pool.Exec(ctx, VirtualFileMetadataUpdateSQL,
		`[]`, `[]`, `[]`,
		"2160p", "hevc", "eac3", "mkv", true, 8000000, 5400,
		candidateID, expectedPath, true,
		updatedAt, probeUpdatedAt, ownerID, folderID, adoptPath,
		neutralPath, virtualFailedVerdictMaxAge.Seconds(), true, false,
		"", nil, "", "", "", 0,
		nil,
		false, // $30 replaceIdentity: this helper exercises best-effort adoption only
		false, // $31 clearProbe
		false, // $32 reconcileCollectionVariant: no collection-row reconcile
	)
	if err != nil {
		t.Fatalf("VirtualFileMetadataUpdateSQL: %v", err)
	}
	if got := tag.RowsAffected(); got != 1 {
		t.Fatalf("rows affected = %d, want 1", got)
	}
}

// Adopting a candidate path onto the requested row is the normal case: with no
// sibling owning the resolved path, file_path moves and the evidence lands.
func TestVirtualFileMetadataUpdateAdoptsPathWithoutSibling(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994311, 7001
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	adoptPath := "virtual://movie/tt-db-adopt-control"
	candidatePath := adoptPath + "?result=cand-a"
	var candidateID int
	var updatedAt time.Time
	var probeUpdatedAt *time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source)
		VALUES('movie-db-adopt-control', $1, $2, 'virtual', $3, 'virtual')
		RETURNING id, updated_at, probe_updated_at`, folderID, candidatePath, ownerID,
	).Scan(&candidateID, &updatedAt, &probeUpdatedAt); err != nil {
		t.Fatalf("insert candidate row: %v", err)
	}

	runVirtualMetadataUpdate(t, pool, candidateID, candidatePath, adoptPath, ownerID, folderID, updatedAt, probeUpdatedAt)

	var path, resolution string
	var stampedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT file_path, resolution, probe_updated_at FROM media_files WHERE id = $1`, candidateID).Scan(&path, &resolution, &stampedAt); err != nil {
		t.Fatalf("read candidate row: %v", err)
	}
	if path != adoptPath {
		t.Fatalf("file_path = %q, want adopted %q", path, adoptPath)
	}
	if resolution != "2160p" {
		t.Fatalf("resolution = %q, want 2160p evidence persisted", resolution)
	}
	if stampedAt == nil {
		t.Fatal("probe_updated_at was not stamped")
	}
}

// When a sibling row (same virtual owner and library) already owns the resolved
// path, the update must not adopt it: the unique index would reject the write and
// drop the probe evidence. The row keeps its path but the evidence and stamp
// still land.
func TestVirtualFileMetadataUpdateSkipsAdoptionWhenSiblingOwnsPath(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994312, 7002
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	adoptPath := "virtual://movie/tt-db-guard"
	candidatePath := adoptPath + "?result=cand-a"
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source)
		VALUES('movie-db-guard', $1, $2, 'virtual', $3, 'virtual')`, folderID, adoptPath, ownerID); err != nil {
		t.Fatalf("insert sibling row: %v", err)
	}

	var candidateID int
	var updatedAt time.Time
	var probeUpdatedAt *time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source)
		VALUES('movie-db-guard', $1, $2, 'virtual', $3, 'virtual')
		RETURNING id, updated_at, probe_updated_at`, folderID, candidatePath, ownerID,
	).Scan(&candidateID, &updatedAt, &probeUpdatedAt); err != nil {
		t.Fatalf("insert candidate row: %v", err)
	}

	runVirtualMetadataUpdate(t, pool, candidateID, candidatePath, adoptPath, ownerID, folderID, updatedAt, probeUpdatedAt)

	var path, resolution string
	var stampedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT file_path, resolution, probe_updated_at FROM media_files WHERE id = $1`, candidateID).Scan(&path, &resolution, &stampedAt); err != nil {
		t.Fatalf("read candidate row: %v", err)
	}
	if path != candidatePath {
		t.Fatalf("file_path = %q, want unchanged candidate %q (sibling owns %q)", path, candidatePath, adoptPath)
	}
	if resolution != "2160p" {
		t.Fatalf("resolution = %q, want 2160p evidence persisted despite skipped adoption", resolution)
	}
	if stampedAt == nil {
		t.Fatal("probe_updated_at was not stamped despite skipped adoption")
	}
}

// A row that has never been stamped (probe_source IS NULL) must adopt its
// resolved path: the adoption guard uses IS DISTINCT FROM so NULL behaves like
// any other non-collection source. With a plain `probe_source != ...` the guard
// evaluated to NULL and the row silently kept its old path.
func TestVirtualFileMetadataUpdateAdoptsPathForNullProbeSource(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994313, 7003
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	adoptPath := "virtual://movie/tt-db-null-source"
	candidatePath := adoptPath + "?result=cand-a"
	var candidateID int
	var updatedAt time.Time
	var probeUpdatedAt *time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id)
		VALUES('movie-db-null-source', $1, $2, 'virtual', $3)
		RETURNING id, updated_at, probe_updated_at`, folderID, candidatePath, ownerID,
	).Scan(&candidateID, &updatedAt, &probeUpdatedAt); err != nil {
		t.Fatalf("insert candidate row: %v", err)
	}

	runVirtualMetadataUpdate(t, pool, candidateID, candidatePath, adoptPath, ownerID, folderID, updatedAt, probeUpdatedAt)

	var path, resolution, probeSource string
	var stampedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT file_path, resolution, COALESCE(probe_source, ''), probe_updated_at FROM media_files WHERE id = $1`, candidateID).Scan(&path, &resolution, &probeSource, &stampedAt); err != nil {
		t.Fatalf("read candidate row: %v", err)
	}
	if path != adoptPath {
		t.Fatalf("file_path = %q, want adopted %q", path, adoptPath)
	}
	if resolution != "2160p" {
		t.Fatalf("resolution = %q, want 2160p evidence persisted", resolution)
	}
	if probeSource != "virtual" {
		t.Fatalf("probe_source = %q, want virtual after a real probe", probeSource)
	}
	if stampedAt == nil {
		t.Fatal("probe_updated_at was not stamped")
	}
}

// A transient outer timeout leaves the inner probe running under its own
// scanner timeout. Once that probe completes and lands in the cache, the
// damper's cache-only recovery must still persist the evidence and stamp
// probe_updated_at, so the row is not left unprobed until the backoff lapses.
func TestVirtualMetadataUpdateRecoversEvidenceAfterTransientTimeout(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994315, 7005
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	candidatePath := "virtual://movie/tt-db-recover?result=cand-a"
	failureKey := virtualProbeFailureKey(candidatePath, ownerID)
	virtualProbeFailures.clear(failureKey)
	t.Cleanup(func() { virtualProbeFailures.clear(failureKey) })

	var candidateID int
	var updatedAt time.Time
	var probeUpdatedAt *time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source)
		VALUES('movie-db-recover', $1, $2, 'virtual', $3, 'virtual')
		RETURNING id, updated_at, probe_updated_at`, folderID, candidatePath, ownerID,
	).Scan(&candidateID, &updatedAt, &probeUpdatedAt); err != nil {
		t.Fatalf("insert candidate row: %v", err)
	}

	var cacheLookups int
	h := &PlaybackHandler{
		VirtualPlaybackSourceProber: func(context.Context, string, *models.MediaFile) (*models.MediaFile, error) {
			return nil, context.DeadlineExceeded
		},
		VirtualFileSaver: func(ctx context.Context, args models.VirtualFilePersistArgs) (int64, error) {
			return ExecVirtualFileMetadataUpdate(ctx, pool, args)
		},
		VirtualProbeCacheLookup: func(_ string, file *models.MediaFile) *models.MediaFile {
			cacheLookups++
			return &models.MediaFile{
				FilePath:                   file.FilePath,
				VirtualOwnerInstallationID: file.VirtualOwnerInstallationID,
				VideoTracks:                []models.VideoTrack{{Codec: "hevc", Width: 3840, Height: 2160}},
				AudioTracks:                []models.AudioTrack{{Codec: "eac3", Channels: 6, Language: "eng"}},
				Resolution:                 "2160p",
				CodecVideo:                 "hevc",
				CodecAudio:                 "eac3",
				Container:                  "mkv",
				Duration:                   5400,
			}
		},
	}
	stored := &models.MediaFile{
		ID:                         candidateID,
		ContentID:                  "movie-db-recover",
		FilePath:                   candidatePath,
		MediaFolderID:              folderID,
		VirtualOwnerInstallationID: ownerID,
		UpdatedAt:                  updatedAt,
		ProbeUpdatedAt:             probeUpdatedAt,
	}
	cand := VirtualPlaybackStream{ID: "cand-a", URI: candidatePath}

	// The transient timeout marks the damper but must not stamp the row.
	h.probeVirtualSourceAndPersist(ctx, "sticky-recover", stored, "http://provider.example/stream", *stored, cand, noCandidateIdentity, 90, ownerID)
	if got := virtualProbeFailures.count(failureKey); got == 0 {
		t.Fatal("transient timeout did not mark the probe failure damper")
	}

	// The completed inner probe is now available; recovery persists it.
	if !h.recoverVirtualProbeFromCache(ctx, stored, "http://provider.example/stream", *stored, cand, noCandidateIdentity, ownerID) {
		t.Fatal("cache-only recovery did not use the completed probe")
	}
	if cacheLookups != 1 {
		t.Fatalf("cache lookups = %d, want 1", cacheLookups)
	}
	if virtualProbeFailures.count(failureKey) != 0 {
		t.Fatal("cache-only recovery did not clear the failure mark")
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		var stampedAt *time.Time
		if err := pool.QueryRow(ctx, `SELECT probe_updated_at FROM media_files WHERE id = $1`, candidateID).Scan(&stampedAt); err != nil {
			t.Fatalf("read candidate row: %v", err)
		}
		if stampedAt != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("probe_updated_at was not stamped after cache-only recovery")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// TestVirtualFileMetadataUpdateRetriesCatalogRevisionOnlyAdvance covers the
// cold-player race: the inventory poll's detail read re-lists candidates and
// advances updated_at while a probe is still running. The probe snapshot is
// then one catalog revision old, but its path, owner and probe stamp are
// unchanged, so the evidence write must re-base and land instead of reporting
// a superseded snapshot.
func TestVirtualFileMetadataUpdateRetriesCatalogRevisionOnlyAdvance(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994316, 7006
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	candidatePath := "virtual://movie/tt-db-revision?profile=4K+Dolby+Vision&result=cand-a"
	var candidateID int
	var updatedAt time.Time
	var probeUpdatedAt *time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source)
		VALUES('movie-db-revision', $1, $2, 'virtual', $3, 'virtual')
		RETURNING id, updated_at, probe_updated_at`, folderID, candidatePath, ownerID,
	).Scan(&candidateID, &updatedAt, &probeUpdatedAt); err != nil {
		t.Fatalf("insert candidate row: %v", err)
	}

	// A re-list rewrites the candidate row's catalog revision without changing
	// its path or probe evidence.
	if _, err := pool.Exec(ctx, `UPDATE media_files SET updated_at = updated_at + interval '1 minute' WHERE id = $1`, candidateID); err != nil {
		t.Fatalf("simulate re-list revision: %v", err)
	}

	result, err := ExecVirtualFileMetadataUpdateResult(ctx, pool, models.VirtualFilePersistArgs{
		FileID:           candidateID,
		ExpectedFilePath: candidatePath,
		VideoTracks:      []byte(`[]`),
		AudioTracks:      []byte(`[{"codec":"eac3","channels":6,"language":"eng"}]`),
		SubtitleTracks:   []byte(`[]`),
		Resolution:       "2160p",
		CodecVideo:       "hevc",
		CodecAudio:       "eac3",
		Container:        "mkv",
		HDR:              true,
		Bitrate:          8000000,
		Duration:         5400,
		StampProbe:       true,
		UpdatedAt:        updatedAt,
		ProbeUpdatedAt:   probeUpdatedAt,
		OwnerID:          ownerID,
		LibraryID:        folderID,
	})
	if err != nil {
		t.Fatalf("revision-only evidence write failed: %v", err)
	}
	if !result.MetadataUpdated {
		t.Fatal("revision-only evidence write reported a superseded snapshot")
	}

	var resolution string
	var stampedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT resolution, probe_updated_at FROM media_files WHERE id = $1`, candidateID).Scan(&resolution, &stampedAt); err != nil {
		t.Fatalf("read evidence row: %v", err)
	}
	if resolution != "2160p" || stampedAt == nil {
		t.Fatalf("resolution = %q, probe_updated_at = %v; evidence did not land", resolution, stampedAt)
	}
}

// TestVirtualFileMetadataUpdateRequestHeadersPreserveAndReplace proves the
// adopt-path write treats request headers as part of the resolution pair: a
// metadata-only write with no headers preserves the stored set, and a write
// that carries headers replaces it.
func TestVirtualFileMetadataUpdateRequestHeadersPreserveAndReplace(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994316, 7006
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	candidatePath := "virtual://movie/tt-db-headers?result=cand-a"
	var candidateID int
	var updatedAt time.Time
	var probeUpdatedAt *time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source, provider_request_headers)
		VALUES('movie-db-headers', $1, $2, 'virtual', $3, 'virtual', '{"Referer":"https://keep.example/"}'::jsonb)
		RETURNING id, updated_at, probe_updated_at`, folderID, candidatePath, ownerID,
	).Scan(&candidateID, &updatedAt, &probeUpdatedAt); err != nil {
		t.Fatalf("insert candidate row: %v", err)
	}

	write := func(headers map[string]string) time.Time {
		t.Helper()
		result, err := ExecVirtualFileMetadataUpdateResult(ctx, pool, models.VirtualFilePersistArgs{
			FileID:                 candidateID,
			ExpectedFilePath:       candidatePath,
			StampProbe:             true,
			UpdatedAt:              updatedAt,
			ProbeUpdatedAt:         probeUpdatedAt,
			OwnerID:                ownerID,
			LibraryID:              folderID,
			ProviderRequestHeaders: headers,
		})
		if err != nil {
			t.Fatalf("ExecVirtualFileMetadataUpdateResult: %v", err)
		}
		if !result.MetadataUpdated {
			t.Fatal("metadata write did not land")
		}
		var next time.Time
		if err := pool.QueryRow(ctx, `SELECT updated_at FROM media_files WHERE id = $1`, candidateID).Scan(&next); err != nil {
			t.Fatalf("read updated_at: %v", err)
		}
		return next
	}

	// A metadata-only write carries no headers and must preserve the stored set.
	updatedAt = write(nil)
	var preserved string
	if err := pool.QueryRow(ctx, `SELECT provider_request_headers->>'Referer' FROM media_files WHERE id = $1`, candidateID).Scan(&preserved); err != nil {
		t.Fatalf("read preserved headers: %v", err)
	}
	if preserved != "https://keep.example/" {
		t.Fatalf("Referer = %q, want the preserved stored header", preserved)
	}

	// A write that carries headers replaces the stored set. Refresh the CAS
	// snapshot first: the previous write advanced updated_at and stamped
	// probe_updated_at.
	if err := pool.QueryRow(ctx, `SELECT probe_updated_at FROM media_files WHERE id = $1`, candidateID).Scan(&probeUpdatedAt); err != nil {
		t.Fatalf("reread probe stamp: %v", err)
	}
	write(map[string]string{"Referer": "https://new.example/", "Origin": "https://new.example"})
	var replaced string
	if err := pool.QueryRow(ctx, `SELECT provider_request_headers->>'Referer' FROM media_files WHERE id = $1`, candidateID).Scan(&replaced); err != nil {
		t.Fatalf("read replaced headers: %v", err)
	}
	if replaced != "https://new.example/" {
		t.Fatalf("Referer = %q, want the replaced header", replaced)
	}
}

// A NULL-probe_source row still honors the sibling guard: when another row in
// the same library and virtual owner already owns the resolved path, the update
// must skip adoption rather than let the unique index reject the write and drop
// the evidence.
func TestVirtualFileMetadataUpdateSkipsAdoptionForNullProbeSourceSiblingOwner(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994314, 7004
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	adoptPath := "virtual://movie/tt-db-null-source-guard"
	candidatePath := adoptPath + "?result=cand-a"
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id)
		VALUES('movie-db-null-source-guard', $1, $2, 'virtual', $3)`, folderID, adoptPath, ownerID); err != nil {
		t.Fatalf("insert sibling row: %v", err)
	}

	var candidateID int
	var updatedAt time.Time
	var probeUpdatedAt *time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id)
		VALUES('movie-db-null-source-guard', $1, $2, 'virtual', $3)
		RETURNING id, updated_at, probe_updated_at`, folderID, candidatePath, ownerID,
	).Scan(&candidateID, &updatedAt, &probeUpdatedAt); err != nil {
		t.Fatalf("insert candidate row: %v", err)
	}

	runVirtualMetadataUpdate(t, pool, candidateID, candidatePath, adoptPath, ownerID, folderID, updatedAt, probeUpdatedAt)

	var path, resolution string
	var stampedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT file_path, resolution, probe_updated_at FROM media_files WHERE id = $1`, candidateID).Scan(&path, &resolution, &stampedAt); err != nil {
		t.Fatalf("read candidate row: %v", err)
	}
	if path != candidatePath {
		t.Fatalf("file_path = %q, want unchanged candidate %q (sibling owns %q)", path, candidatePath, adoptPath)
	}
	if resolution != "2160p" {
		t.Fatalf("resolution = %q, want 2160p evidence persisted despite skipped adoption", resolution)
	}
	if stampedAt == nil {
		t.Fatal("probe_updated_at was not stamped despite skipped adoption")
	}
}

// TestVirtualFileMetadataUpdateNeutralRowRefusesSiblingOwnedCandidate is the
// DB-gated proof of the neutral-row candidate-URI fence: when a sibling row
// already owns the concrete candidate a provider-neutral row is about to take
// evidence for, the whole write is refused. Without the fence the adoption
// would be skipped but the sibling's tracks and probe stamp would still land on
// the neutral row, describing bytes it does not own.
func TestVirtualFileMetadataUpdateNeutralRowRefusesSiblingOwnedCandidate(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994313, 7003
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	neutral := "virtual://movie/tt-db-neutral-sibling"
	candidatePath := neutral + "?result=cand-a"
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source)
		VALUES('movie-db-neutral-sibling', $1, $2, 'virtual', $3, 'virtual')`, folderID, candidatePath, ownerID); err != nil {
		t.Fatalf("insert sibling row: %v", err)
	}

	var rowID int
	var updatedAt time.Time
	var probeUpdatedAt *time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source)
		VALUES('movie-db-neutral-sibling', $1, $2, 'virtual', $3, 'virtual')
		RETURNING id, updated_at, probe_updated_at`, folderID, neutral, ownerID,
	).Scan(&rowID, &updatedAt, &probeUpdatedAt); err != nil {
		t.Fatalf("insert neutral row: %v", err)
	}
	before := readVirtualEvidenceRowJSON(t, pool, rowID)

	args := models.VirtualFilePersistArgs{
		FileID: rowID, ExpectedFilePath: neutral,
		VideoTracks: []byte(`[{"codec":"av1"}]`), AudioTracks: []byte(`[]`), SubtitleTracks: []byte(`[]`),
		Resolution: "2160p", CodecVideo: "av1", CodecAudio: "eac3", Container: "mkv",
		StampProbe: true, UpdatedAt: updatedAt, ProbeUpdatedAt: probeUpdatedAt,
		OwnerID: ownerID, LibraryID: folderID,
		AdoptPath: candidatePath,
	}
	result, err := ExecVirtualFileMetadataUpdateResult(ctx, pool, args)
	if err != nil {
		t.Fatalf("neutral-row sibling-owned write error = %v, want a silent CAS miss", err)
	}
	if result.MetadataUpdated || result.RowsAffected != 0 || result.IdentityAdopted {
		t.Fatalf("result = %+v, want a refused write", result)
	}
	if after := readVirtualEvidenceRowJSON(t, pool, rowID); after != before {
		t.Fatalf("neutral row changed on a sibling-owned candidate write:\nbefore=%s\nafter =%s", before, after)
	}
}

// TestVirtualFileMetadataUpdateNeutralRowRefusesSiblingOwnedCandidateWithoutExpectedPath
// proves the neutral-row candidate-URI fence does not depend on the optional
// expected path. A metadata-only write that omits it must still refuse when a
// sibling row owns the concrete candidate, so the neutral row cannot absorb
// evidence for bytes it does not own. Comparing the optional expected path
// against the neutral key instead of the row's actual file_path made an omitted
// expected path bypass the fence.
func TestVirtualFileMetadataUpdateNeutralRowRefusesSiblingOwnedCandidateWithoutExpectedPath(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994317, 7007
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	neutral := "virtual://movie/tt-db-neutral-no-expected"
	candidatePath := neutral + "?result=cand-a"
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source)
		VALUES('movie-db-neutral-no-expected', $1, $2, 'virtual', $3, 'virtual')`, folderID, candidatePath, ownerID); err != nil {
		t.Fatalf("insert sibling row: %v", err)
	}

	var rowID int
	var updatedAt time.Time
	var probeUpdatedAt *time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source)
		VALUES('movie-db-neutral-no-expected', $1, $2, 'virtual', $3, 'virtual')
		RETURNING id, updated_at, probe_updated_at`, folderID, neutral, ownerID,
	).Scan(&rowID, &updatedAt, &probeUpdatedAt); err != nil {
		t.Fatalf("insert neutral row: %v", err)
	}
	before := readVirtualEvidenceRowJSON(t, pool, rowID)

	args := models.VirtualFilePersistArgs{
		FileID:      rowID, // ExpectedFilePath is deliberately omitted.
		VideoTracks: []byte(`[{"codec":"av1"}]`), AudioTracks: []byte(`[]`), SubtitleTracks: []byte(`[]`),
		Resolution: "2160p", CodecVideo: "av1", CodecAudio: "eac3", Container: "mkv",
		StampProbe: true, UpdatedAt: updatedAt, ProbeUpdatedAt: probeUpdatedAt,
		OwnerID: ownerID, LibraryID: folderID,
		AdoptPath: candidatePath,
	}
	result, err := ExecVirtualFileMetadataUpdateResult(ctx, pool, args)
	if err != nil {
		t.Fatalf("neutral-row missing-expected write error = %v, want a silent CAS miss", err)
	}
	if result.MetadataUpdated || result.RowsAffected != 0 || result.IdentityAdopted {
		t.Fatalf("result = %+v, want a refused write", result)
	}
	if after := readVirtualEvidenceRowJSON(t, pool, rowID); after != before {
		t.Fatalf("neutral row changed on a sibling-owned candidate write with no expected path:\nbefore=%s\nafter =%s", before, after)
	}
}
