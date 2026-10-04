package userdb

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/userstore"
	"github.com/Silo-Server/silo-server/internal/userstore/storetest"
)

func newConformanceStore(t *testing.T) userstore.UserStore {
	t.Helper()
	db, err := NewUserDB(filepath.Join(t.TempDir(), "user.db"), 1)
	if err != nil {
		t.Fatalf("open user database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewSQLiteUserStore(db.DB)
}

// SeedHiddenHistoryItem implements storetest's Next Up hidden-history test
// seam against the raw per-user SQLite table. It lives in the test build only:
// the public store API timestamp-adjusts or suppresses any write at or before
// a hidden watermark, so the conformance suite cannot construct a hidden
// progress row any other way.
func (s *SQLiteUserStore) SeedHiddenHistoryItem(ctx context.Context, profileID, mediaItemID string, hiddenBefore time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO hidden_history_items (profile_id, media_item_id, hidden_before, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(profile_id, media_item_id) DO UPDATE SET
			hidden_before = excluded.hidden_before,
			updated_at = excluded.updated_at`,
		profileID, mediaItemID, hiddenBefore.UTC().Format(time.RFC3339), nowUTC())
	return err
}

// TestSQLiteNextUpState runs the Next Up state provider conformance suite
// against the per-user SQLite backend. The Postgres backend runs the same suite
// in internal/userstore/pgstore.
func TestSQLiteNextUpState(t *testing.T) {
	storetest.RunNextUpState(t, newConformanceStore)
}

// TestSQLiteProgressSince runs the offline-sync progress-reconciliation
// conformance test (invariant 1) against the real SQLite backend, exercising the
// synced_seq stamping triggers and event_at LWW comparison.
func TestSQLiteProgressSince(t *testing.T) {
	storetest.RunProgressSince(t, newConformanceStore)
}

// TestSQLiteDeviceProfiles runs the per-device capability registry
// conformance tests against the per-user SQLite backend; the Postgres backend
// runs the same suite in internal/userstore/pgstore.
func TestSQLiteDeviceProfiles(t *testing.T) {
	storetest.RunDeviceProfiles(t, newConformanceStore)
}

// TestSQLiteMarkWatchedBatch runs the batch mark-watched conformance test
// (series/season mark-watched) against the real SQLite backend. The Postgres
// backend runs the same suite in internal/userdb/pgstore, which is what
// keeps the two transactional implementations from drifting.
func TestSQLiteMarkWatchedBatch(t *testing.T) {
	storetest.RunMarkWatchedBatch(t, newConformanceStore)
}

// TestSQLiteSettingValues runs the canonical settings-contract storage
// conformance tests against the per-user SQLite backend. The Postgres backend
// runs the same suite in internal/userstore/pgstore, which is what keeps the two
// from drifting on scope identity, partial uniqueness and delete behavior.
func TestSQLiteSettingValues(t *testing.T) {
	storetest.RunSettingValues(t, newConformanceStore)
}

// TestSQLiteJellycompatDisplayPrefs runs the Jellyfin DisplayPreferences
// storage conformance tests against the per-user SQLite backend; the Postgres
// backend runs the same suite in internal/userstore/pgstore.
func TestSQLiteJellycompatDisplayPrefs(t *testing.T) {
	storetest.RunJellycompatDisplayPrefs(t, newConformanceStore)
}

func TestSQLiteCollectionSortPreferences(t *testing.T) {
	storetest.RunCollectionSortPreferences(t, newConformanceStore)
}

func TestSQLiteAddFavoriteAtReportsInsertion(t *testing.T) {
	ctx := context.Background()
	store := newConformanceStore(t)
	if err := store.CreateProfile(ctx, userstore.Profile{ID: "p1", Name: "Test"}); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}

	addedAt := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	inserted, err := store.AddFavoriteAt(ctx, "p1", "movie-1", addedAt)
	if err != nil {
		t.Fatalf("first AddFavoriteAt: %v", err)
	}
	if !inserted {
		t.Fatal("first AddFavoriteAt reported no insertion")
	}
	inserted, err = store.AddFavoriteAt(ctx, "p1", "movie-1", addedAt)
	if err != nil {
		t.Fatalf("duplicate AddFavoriteAt: %v", err)
	}
	if inserted {
		t.Fatal("duplicate AddFavoriteAt reported an insertion")
	}
}

// TestSQLiteProgressPage runs the keyset progress paging conformance test
// against the real SQLite backend; the Postgres backend runs the same suite in
// internal/userstore/pgstore.
func TestSQLiteProgressPage(t *testing.T) {
	storetest.RunProgressPage(t, newConformanceStore)
}

// TestSQLitePersonalListPage runs the keyset favorites/watchlist paging
// conformance test against the real SQLite backend, which is what pins the
// text comparison of added_at to the RFC 3339 form AddFavoriteAt writes.
func TestSQLitePersonalListPage(t *testing.T) {
	storetest.RunPersonalListPage(t, newConformanceStore)
}

func TestSQLiteHistoryEntryOnce(t *testing.T) {
	storetest.RunHistoryEntryOnce(t, newConformanceStore(t))
}

func TestSQLiteDatedMarkWatchedBatchAtomic(t *testing.T) {
	storetest.RunDatedMarkWatchedBatch(t, newConformanceStore(t))
}

func TestSQLiteAtomicJellycompatProgressHistoryRollback(t *testing.T) {
	store, ok := newConformanceStore(t).(*SQLiteUserStore)
	if !ok {
		t.Fatal("SQLite fixture unavailable")
	}
	for _, operation := range []string{"INSERT", "UPDATE"} {
		if _, err := store.db.Exec("CREATE TRIGGER fail_progress_" + operation + " BEFORE " + operation + " ON watch_progress WHEN NEW.position_seconds = 321 BEGIN SELECT RAISE(ABORT, 'forced progress failure'); END"); err != nil {
			t.Fatal(err)
		}
	}
	storetest.RunAtomicJellycompatProgress(t, store)
}
