package handlers

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/scanner"
)

// These tests exercise the durable refusal store against the real migrated
// schema: the atomic ON CONFLICT upsert, the TTL restart, and the clear-on-
// success. They skip cleanly when SILO_TEST_DATABASE_URL is unset.

// TestVirtualAdoptionRefusalStoreDBAtomicIncrementAndClear proves the store is
// the durable, atomic counter the bound assumes: concurrent refusals all land on
// one row, the count is read back exactly, and a clear resets it so the bound
// does not survive a successful write.
func TestVirtualAdoptionRefusalStoreDBAtomicIncrementAndClear(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994421, 7421
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	path := "virtual://movie/tt-db-refusal?result=pin"
	var id int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source)
		VALUES('movie-db-refusal', $1, $2, 'virtual', $3, 'virtual')
		RETURNING id`, folderID, path, ownerID).Scan(&id); err != nil {
		t.Fatalf("insert row: %v", err)
	}
	repo := scanner.NewFileRepository(pool)
	ttl := 10 * time.Minute

	const workers, perWorker = 8, 5
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				if _, err := repo.RecordVirtualAdoptionRefusal(ctx, id, path, ttl); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent record: %v", err)
	}

	count, err := repo.CountVirtualAdoptionRefusal(ctx, id, path)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if want := workers * perWorker; count != want {
		t.Fatalf("durable count = %d, want %d", count, want)
	}

	if err := repo.ClearVirtualAdoptionRefusal(ctx, id, path); err != nil {
		t.Fatalf("clear: %v", err)
	}
	count, err = repo.CountVirtualAdoptionRefusal(ctx, id, path)
	if err != nil {
		t.Fatalf("count after clear: %v", err)
	}
	if count != 0 {
		t.Fatalf("count after clear = %d, want 0", count)
	}
}

// TestVirtualAdoptionRefusalStoreDBExpiryRestartsCount proves an expired mark is
// treated as absent: a refusal stream separated by more than the TTL restarts at
// one, so a provider that recovers is not permanently treated as stale.
func TestVirtualAdoptionRefusalStoreDBExpiryRestartsCount(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994422, 7422
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	path := "virtual://movie/tt-db-refusal-expiry?result=pin"
	var id int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, container, virtual_owner_installation_id, probe_source)
		VALUES('movie-db-refusal-expiry', $1, $2, 'virtual', $3, 'virtual')
		RETURNING id`, folderID, path, ownerID).Scan(&id); err != nil {
		t.Fatalf("insert row: %v", err)
	}
	repo := scanner.NewFileRepository(pool)

	if _, err := repo.RecordVirtualAdoptionRefusal(ctx, id, path, time.Minute); err != nil {
		t.Fatalf("record: %v", err)
	}
	if _, err := repo.RecordVirtualAdoptionRefusal(ctx, id, path, time.Minute); err != nil {
		t.Fatalf("record: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE virtual_adoption_refusals SET expires_at = now() - interval '1 second' WHERE file_id = $1 AND expected_path = $2`, id, path); err != nil {
		t.Fatalf("expire mark: %v", err)
	}
	count, err := repo.CountVirtualAdoptionRefusal(ctx, id, path)
	if err != nil {
		t.Fatalf("count expired: %v", err)
	}
	if count != 0 {
		t.Fatalf("expired count = %d, want 0", count)
	}
	restarted, err := repo.RecordVirtualAdoptionRefusal(ctx, id, path, time.Minute)
	if err != nil {
		t.Fatalf("record after expiry: %v", err)
	}
	if restarted != 1 {
		t.Fatalf("count after expiry = %d, want 1", restarted)
	}
}
