package catalog

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/models"
)

// TestFolderRepositoryProgressThresholdOverrides covers the persisted
// watched / min-resume overrides end to end: create, read back, set, clear,
// and the override lookup the progress paths use.
func TestFolderRepositoryProgressThresholdOverrides(t *testing.T) {
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)
	repo := NewFolderRepository(pool)

	suffix := time.Now().UnixNano()
	watched, resume := 80, 10
	folder, err := repo.Create(ctx, CreateFolderInput{
		Paths:              []string{fmt.Sprintf("/media/thresholds-%d", suffix)},
		Type:               "movies",
		Name:               fmt.Sprintf("thresholds-%d", suffix),
		WatchedThreshold:   &watched,
		MinResumeThreshold: &resume,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = repo.Delete(context.Background(), folder.ID) })

	if folder.WatchedThreshold == nil || *folder.WatchedThreshold != 80 ||
		folder.MinResumeThreshold == nil || *folder.MinResumeThreshold != 10 {
		t.Fatalf("created overrides = %v/%v, want 80/10", folder.WatchedThreshold, folder.MinResumeThreshold)
	}

	loaded, err := repo.GetByID(ctx, folder.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if loaded.WatchedThreshold == nil || *loaded.WatchedThreshold != 80 ||
		loaded.MinResumeThreshold == nil || *loaded.MinResumeThreshold != 10 {
		t.Fatalf("reloaded overrides = %v/%v, want 80/10", loaded.WatchedThreshold, loaded.MinResumeThreshold)
	}

	overrides, err := repo.ProgressThresholdOverrides(ctx, folder.ID)
	if err != nil {
		t.Fatalf("ProgressThresholdOverrides: %v", err)
	}
	if overrides.WatchedPct == nil || *overrides.WatchedPct != 80 || overrides.MinResumePct == nil || *overrides.MinResumePct != 10 {
		t.Fatalf("override lookup = %v/%v, want 80/10", overrides.WatchedPct, overrides.MinResumePct)
	}

	// A name-only update (the frozen v1 shape) leaves both overrides alone.
	newName := fmt.Sprintf("thresholds-renamed-%d", suffix)
	if err := repo.Update(ctx, folder.ID, UpdateFolderInput{Name: &newName}); err != nil {
		t.Fatalf("update name: %v", err)
	}
	loaded, err = repo.GetByID(ctx, folder.ID)
	if err != nil {
		t.Fatalf("GetByID after rename: %v", err)
	}
	if loaded.Name != newName || loaded.WatchedThreshold == nil || *loaded.WatchedThreshold != 80 {
		t.Fatalf("after rename: name=%q overrides=%v/%v", loaded.Name, loaded.WatchedThreshold, loaded.MinResumeThreshold)
	}

	// Setting a new value replaces it; clearing writes NULL (inherit).
	newResume := 20
	if err := repo.Update(ctx, folder.ID, UpdateFolderInput{
		MinResumeThreshold: models.Optional[int]{Set: true, Value: &newResume},
	}); err != nil {
		t.Fatalf("update resume: %v", err)
	}
	loaded, err = repo.GetByID(ctx, folder.ID)
	if err != nil {
		t.Fatalf("GetByID after resume update: %v", err)
	}
	if loaded.MinResumeThreshold == nil || *loaded.MinResumeThreshold != 20 {
		t.Fatalf("resume after update = %v, want 20", loaded.MinResumeThreshold)
	}
	if loaded.WatchedThreshold == nil || *loaded.WatchedThreshold != 80 {
		t.Fatalf("watched changed unexpectedly: %v", loaded.WatchedThreshold)
	}

	if err := repo.Update(ctx, folder.ID, UpdateFolderInput{
		WatchedThreshold:   models.Optional[int]{Set: true},
		MinResumeThreshold: models.Optional[int]{Set: true},
	}); err != nil {
		t.Fatalf("clear overrides: %v", err)
	}
	loaded, err = repo.GetByID(ctx, folder.ID)
	if err != nil {
		t.Fatalf("GetByID after clear: %v", err)
	}
	if loaded.WatchedThreshold != nil || loaded.MinResumeThreshold != nil {
		t.Fatalf("after clear = %v/%v, want nil/nil", loaded.WatchedThreshold, loaded.MinResumeThreshold)
	}

	// A missing folder yields zero overrides rather than an error.
	overrides, err = repo.ProgressThresholdOverrides(ctx, -1)
	if err != nil {
		t.Fatalf("missing folder lookup: %v", err)
	}
	if overrides.WatchedPct != nil || overrides.MinResumePct != nil {
		t.Fatalf("missing folder overrides = %+v, want zero", overrides)
	}
}
