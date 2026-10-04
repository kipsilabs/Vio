package scanner

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/models"
)

func TestReplaceVirtualCandidatesIsScopedToLibrary(t *testing.T) {
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

	suffix := time.Now().UnixNano()
	contentID := fmt.Sprintf("virtual-candidate-scope-%d", suffix)
	basePath := fmt.Sprintf("virtual://movie/tt%d?profile=1080p", suffix)
	firstPath := basePath + "&result=first"
	secondPath := basePath + "&result=second"
	var folderA, folderB int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Candidate A %d", suffix)).Scan(&folderA); err != nil {
		t.Fatalf("seed first folder: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Candidate B %d", suffix)).Scan(&folderB); err != nil {
		t.Fatalf("seed second folder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_files WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_item_libraries WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id=ANY($1::int[])`, []int{folderA, folderB})
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id,type,title,status,genres)
		VALUES($1,'movie','Candidate Scope','matched','{}'::text[])`, contentID); err != nil {
		t.Fatalf("seed candidate item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_item_libraries(content_id,media_folder_id)
		VALUES($1,$2),($1,$3)`, contentID, folderA, folderB); err != nil {
		t.Fatalf("seed candidate library links: %v", err)
	}

	repo := NewFileRepository(pool)
	source := func(folderID int) *models.MediaFile {
		return &models.MediaFile{
			ContentID:                  contentID,
			MediaFolderID:              folderID,
			FilePath:                   basePath,
			VirtualOwnerInstallationID: 91,
		}
	}
	first := []VirtualCandidate{{URI: firstPath, Label: "1080p"}}
	if err := repo.ReplaceVirtualCandidates(ctx, source(folderA), first); err != nil {
		t.Fatalf("replace first-library candidates: %v", err)
	}
	if err := repo.ReplaceVirtualCandidates(ctx, source(folderB), first); err != nil {
		t.Fatalf("replace second-library candidates: %v", err)
	}
	if err := repo.ReplaceVirtualCandidates(ctx, source(folderA), []VirtualCandidate{{URI: secondPath, Label: "1080p"}}); err != nil {
		t.Fatalf("refresh first-library candidates: %v", err)
	}

	var firstA, firstB, secondA int
	if err := pool.QueryRow(ctx, `
		SELECT
		  count(*) FILTER(WHERE media_folder_id=$2 AND file_path=$4),
		  count(*) FILTER(WHERE media_folder_id=$3 AND file_path=$4),
		  count(*) FILTER(WHERE media_folder_id=$2 AND file_path=$5)
		FROM media_files
		WHERE content_id=$1 AND virtual_owner_installation_id=91`,
		contentID, folderA, folderB, firstPath, secondPath,
	).Scan(&firstA, &firstB, &secondA); err != nil {
		t.Fatalf("inspect library-scoped candidates: %v", err)
	}
	if firstA != 0 || firstB != 1 || secondA != 1 {
		t.Fatalf("candidate rows firstA=%d firstB=%d secondA=%d, want 0/1/1", firstA, firstB, secondA)
	}
}

// TestReplaceVirtualCandidatesRetainsLastPlayed covers the retention rule in
// ReplaceVirtualCandidates: provider re-lists churn result ids, so a stale
// candidate that is some user's user_watch_progress.last_file_id must survive,
// an unplayed stale candidate must still be deleted, and a retained row that is
// re-listed with the same URI must be updated in place rather than duplicated.
func TestReplaceVirtualCandidatesRetainsLastPlayed(t *testing.T) {
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

	suffix := time.Now().UnixNano()
	contentID := fmt.Sprintf("virtual-retain-last-played-%d", suffix)
	basePath := fmt.Sprintf("virtual://movie/tt%d?profile=1080p", suffix)
	playedPath := basePath + "&result=played"
	unplayedPath := basePath + "&result=unplayed"
	relistedPath := basePath + "&result=relisted"

	var folderID, userID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Retain Last Played %d", suffix)).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO users(username, role) VALUES($1,'user') RETURNING id`,
		fmt.Sprintf("retain-last-played-%d", suffix)).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM user_watch_progress WHERE user_id=$1`, userID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_files WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_item_libraries WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, folderID)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, userID)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id,type,title,status,genres)
		VALUES($1,'movie','Retain Last Played','matched','{}'::text[])`, contentID); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_item_libraries(content_id,media_folder_id)
		VALUES($1,$2)`, contentID, folderID); err != nil {
		t.Fatalf("seed item library: %v", err)
	}

	seed := func(path string, failed bool) int {
		var failedAt *time.Time
		if failed {
			now := time.Now()
			failedAt = &now
		}
		var id int
		if err := pool.QueryRow(ctx, `
			INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id,failed_at)
			VALUES($1,$2,$3,0,'mkv',5,$4) RETURNING id`,
			contentID, folderID, path, failedAt).Scan(&id); err != nil {
			t.Fatalf("seed virtual file %q: %v", path, err)
		}
		return id
	}
	playedID := seed(playedPath, false)
	_ = seed(unplayedPath, false)
	// The re-listed row is seeded failed to prove a fresh listing preserves the
	// verdict rather than clearing it (only real delivery recovers a row).
	relistedID := seed(relistedPath, true)

	if _, err := pool.Exec(ctx, `
		INSERT INTO user_watch_progress(user_id, profile_id, media_item_id, position_seconds, duration_seconds, last_file_id)
		VALUES($1,'default',$2,100,7200,$3)`, userID, contentID, playedID); err != nil {
		t.Fatalf("seed watch progress: %v", err)
	}

	repo := NewFileRepository(pool)
	source := &models.MediaFile{
		ContentID:                  contentID,
		MediaFolderID:              folderID,
		FilePath:                   basePath,
		VirtualOwnerInstallationID: 5,
	}
	// The re-list drops played and unplayed; only relisted is offered again.
	if err := repo.ReplaceVirtualCandidates(ctx, source, []VirtualCandidate{{URI: relistedPath, Label: "1080p"}}); err != nil {
		t.Fatalf("replace virtual candidates: %v", err)
	}

	var playedCount, unplayedCount, relistedCount, relistedSameID int
	if err := pool.QueryRow(ctx, `
		SELECT
		  count(*) FILTER(WHERE file_path=$2),
		  count(*) FILTER(WHERE file_path=$3),
		  count(*) FILTER(WHERE file_path=$4),
		  count(*) FILTER(WHERE file_path=$4 AND id=$5)
		FROM media_files
		WHERE content_id=$1 AND virtual_owner_installation_id=5`,
		contentID, playedPath, unplayedPath, relistedPath, relistedID,
	).Scan(&playedCount, &unplayedCount, &relistedCount, &relistedSameID); err != nil {
		t.Fatalf("inspect retained candidates: %v", err)
	}
	if playedCount != 1 {
		t.Fatalf("last-played stale candidate was deleted: count=%d, want 1", playedCount)
	}
	if unplayedCount != 0 {
		t.Fatalf("unplayed stale candidate survived: count=%d, want 0", unplayedCount)
	}
	if relistedCount != 1 || relistedSameID != 1 {
		t.Fatalf("relisted candidate duplicated or replaced: count=%d sameID=%d, want 1/1", relistedCount, relistedSameID)
	}

	// The retained last-played row is untouched.
	var playedPathNow string
	var playedFailed *time.Time
	if err := pool.QueryRow(ctx, `SELECT file_path, failed_at FROM media_files WHERE id=$1`, playedID).
		Scan(&playedPathNow, &playedFailed); err != nil {
		t.Fatalf("fetch retained played row: %v", err)
	}
	if playedPathNow != playedPath || playedFailed != nil {
		t.Fatalf("retained played row changed: path=%q failed=%v, want %q/NULL", playedPathNow, playedFailed, playedPath)
	}

	// The re-listed row keeps its id and keeps its failure verdict: a provider
	// re-list is not a recovery, so the auto-pick keeps skipping it until a
	// real delivery clears the stamp.
	var relistedPathNow string
	var relistedFailed *time.Time
	if err := pool.QueryRow(ctx, `SELECT file_path, failed_at FROM media_files WHERE id=$1`, relistedID).
		Scan(&relistedPathNow, &relistedFailed); err != nil {
		t.Fatalf("fetch relisted row: %v", err)
	}
	if relistedPathNow != relistedPath || relistedFailed == nil {
		t.Fatalf("relisted row verdict lost: path=%q failed=%v, want %q/non-NULL", relistedPathNow, relistedFailed, relistedPath)
	}
}

func TestReplaceVirtualCandidatesPreservesBarePlaceholder(t *testing.T) {
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

	suffix := time.Now().UnixNano()
	contentID := fmt.Sprintf("virtual-bare-placeholder-%d", suffix)
	barePath := fmt.Sprintf("virtual://movie/tt%d", suffix)
	candidatePath := barePath + "?result=beststream123"

	var folderID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Placeholder Folder %d", suffix)).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_files WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_item_libraries WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, folderID)
	})

	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id,type,title,status,genres)
		VALUES($1,'movie','Placeholder Scope','matched','{}'::text[])`, contentID); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	// Seed initial bare placeholder file
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id)
		VALUES($1,$2,$3,0,'mkv',5)`, contentID, folderID, barePath); err != nil {
		t.Fatalf("seed bare placeholder file: %v", err)
	}

	repo := NewFileRepository(pool)
	source := &models.MediaFile{
		ContentID:                  contentID,
		MediaFolderID:              folderID,
		FilePath:                   barePath,
		VirtualOwnerInstallationID: 5,
	}

	if err := repo.ReplaceVirtualCandidates(ctx, source, []VirtualCandidate{{URI: candidatePath, Label: "2160p"}}); err != nil {
		t.Fatalf("replace virtual candidates: %v", err)
	}

	var bareCount, candidateCount int
	if err := pool.QueryRow(ctx, `
		SELECT
		  count(*) FILTER(WHERE file_path=$2),
		  count(*) FILTER(WHERE file_path=$3)
		FROM media_files
		WHERE content_id=$1 AND virtual_owner_installation_id=5`,
		contentID, barePath, candidatePath,
	).Scan(&bareCount, &candidateCount); err != nil {
		t.Fatalf("inspect files: %v", err)
	}

	if bareCount != 1 || candidateCount != 1 {
		t.Fatalf("expected bareCount=1 candidateCount=1, got bare=%d candidate=%d", bareCount, candidateCount)
	}
}

func TestReplaceVirtualResultPin_PostgresCAS(t *testing.T) {
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

	suffix := time.Now().UnixNano()
	contentID := fmt.Sprintf("virtual-pin-cas-%d", suffix)
	deadPath := fmt.Sprintf("virtual://movie/tt%d?result=dead-candidate", suffix)
	livePath := fmt.Sprintf("virtual://movie/tt%d?result=live-winner", suffix)
	stalePath := fmt.Sprintf("virtual://movie/tt%d?result=stale-loser", suffix)

	var folderID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Pin CAS %d", suffix)).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_files WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_item_libraries WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, folderID)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id,type,title,status,genres)
		VALUES($1,'movie','Pin CAS Item','matched','{}'::text[])`, contentID); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_item_libraries(content_id,media_folder_id)
		VALUES($1,$2)`, contentID, folderID); err != nil {
		t.Fatalf("seed item library: %v", err)
	}

	var fileID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id)
		VALUES($1,$2,$3,0,'mkv',5) RETURNING id`, contentID, folderID, deadPath).Scan(&fileID); err != nil {
		t.Fatalf("seed pinned virtual file: %v", err)
	}

	repo := NewFileRepository(pool)

	// 1. CAS with wrong expected path should return (false, nil) and not modify row
	replaced, err := repo.ReplaceVirtualResultPin(ctx, fileID, "virtual://movie/tt1234?result=wrong", livePath)
	if err != nil {
		t.Fatalf("ReplaceVirtualResultPin error on mismatch: %v", err)
	}
	if replaced {
		t.Fatal("ReplaceVirtualResultPin returned true on mismatched expectedPath")
	}

	var currentPath string
	if err := pool.QueryRow(ctx, `SELECT file_path FROM media_files WHERE id=$1`, fileID).Scan(&currentPath); err != nil {
		t.Fatalf("fetch current path: %v", err)
	}
	if currentPath != deadPath {
		t.Fatalf("file path was modified on failed CAS: got %q, want %q", currentPath, deadPath)
	}

	// 2. CAS with matching expected path should return (true, nil) and update row
	replaced, err = repo.ReplaceVirtualResultPin(ctx, fileID, deadPath, livePath)
	if err != nil {
		t.Fatalf("ReplaceVirtualResultPin error on match: %v", err)
	}
	if !replaced {
		t.Fatal("ReplaceVirtualResultPin returned false on matching expectedPath")
	}

	if err := pool.QueryRow(ctx, `SELECT file_path FROM media_files WHERE id=$1`, fileID).Scan(&currentPath); err != nil {
		t.Fatalf("fetch current path: %v", err)
	}
	if currentPath != livePath {
		t.Fatalf("file path was not updated on successful CAS: got %q, want %q", currentPath, livePath)
	}

	// 3. Subsequent CAS with original deadPath should fail because row is now livePath
	replaced, err = repo.ReplaceVirtualResultPin(ctx, fileID, deadPath, stalePath)
	if err != nil {
		t.Fatalf("ReplaceVirtualResultPin error on stale: %v", err)
	}
	if replaced {
		t.Fatal("ReplaceVirtualResultPin returned true on stale expectedPath")
	}
}

// TestGetVirtualCandidateByNeutralPath_ResultVariant exercises the
// provider-neutral lookup used when re-resolving a stale virtual candidate. The
// stored row carries a rotating result= pick; the lookup is called with the
// neutral key (result stripped). Rows that merely share the scheme/host/path
// prefix must not leak through the left-anchored LIKE.
func TestGetVirtualCandidateByNeutralPath_ResultVariant(t *testing.T) {
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

	suffix := time.Now().UnixNano()
	contentID := fmt.Sprintf("virtual-neutral-lookup-%d", suffix)
	neutral := fmt.Sprintf("virtual://series/tt%d/1/1?profile=1080p", suffix)
	stored := neutral + "&result=abc"

	var folderID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('series',$1,true) RETURNING id`, fmt.Sprintf("Neutral Lookup %d", suffix)).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_files WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, folderID)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id,type,title,status,genres)
		VALUES($1,'series','Neutral Lookup','matched','{}'::text[])`, contentID); err != nil {
		t.Fatalf("seed item: %v", err)
	}

	const ownerID = 5
	seed := func(path string) int {
		var id int
		if err := pool.QueryRow(ctx, `
			INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id,episode_id)
			VALUES($1,$2,$3,0,'mkv',$4,NULL) RETURNING id`,
			contentID, folderID, path, ownerID).Scan(&id); err != nil {
			t.Fatalf("seed virtual file %q: %v", path, err)
		}
		return id
	}

	wantID := seed(stored)
	// Same item and profile but a nested path that shares the neutral prefix:
	// "virtual://series/tt.../1/1" prefixes "virtual://series/tt.../1/10".
	seed(fmt.Sprintf("virtual://series/tt%d/1/10?profile=1080p&result=def", suffix))
	// Same path but a different quality profile.
	seed(fmt.Sprintf("virtual://series/tt%d/1/1?profile=720p&result=ghi", suffix))

	repo := NewFileRepository(pool)
	got, err := repo.GetVirtualCandidateByNeutralPath(ctx, neutral, contentID, "", ownerID)
	if err != nil {
		t.Fatalf("GetVirtualCandidateByNeutralPath: %v", err)
	}
	if got == nil || got.ID != wantID {
		t.Fatalf("GetVirtualCandidateByNeutralPath returned %+v, want id %d", got, wantID)
	}
	if got.FilePath != stored {
		t.Fatalf("returned path %q, want %q", got.FilePath, stored)
	}

	// A bare neutral key matches the result-only pin and nothing else.
	bareNeutral := fmt.Sprintf("virtual://movie/tt%d", suffix)
	bareID := seed(bareNeutral + "?result=xyz")
	gotBare, err := repo.GetVirtualCandidateByNeutralPath(ctx, bareNeutral, contentID, "", ownerID)
	if err != nil {
		t.Fatalf("GetVirtualCandidateByNeutralPath (bare): %v", err)
	}
	if gotBare == nil || gotBare.ID != bareID {
		t.Fatalf("bare lookup returned %+v, want id %d", gotBare, bareID)
	}
}

func TestReplaceVirtualResultPin_CollisionUnpinsInsteadOfErroring(t *testing.T) {
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

	suffix := time.Now().UnixNano()
	contentID := fmt.Sprintf("virtual-pin-collision-%d", suffix)
	deadPath := fmt.Sprintf("virtual://movie/tt%d?result=dead-candidate", suffix)
	livePath := fmt.Sprintf("virtual://movie/tt%d?result=live-winner", suffix)
	neutralPath := fmt.Sprintf("virtual://movie/tt%d", suffix)

	var folderID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Pin Collision %d", suffix)).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_files WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_item_libraries WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, folderID)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id,type,title,status,genres)
		VALUES($1,'movie','Pin Collision Item','matched','{}'::text[])`, contentID); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_item_libraries(content_id,media_folder_id)
		VALUES($1,$2)`, contentID, folderID); err != nil {
		t.Fatalf("seed item library: %v", err)
	}

	// Row A: the dead-pinned row that playback is trying to repoint.
	var deadFileID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id)
		VALUES($1,$2,$3,0,'mkv',5) RETURNING id`, contentID, folderID, deadPath).Scan(&deadFileID); err != nil {
		t.Fatalf("seed dead-pinned file: %v", err)
	}
	// Row B: the sibling live row that already owns the winning (file_path, owner, folder) tuple.
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id)
		VALUES($1,$2,$3,0,'mkv',5)`, contentID, folderID, livePath); err != nil {
		t.Fatalf("seed sibling live file: %v", err)
	}

	repo := NewFileRepository(pool)

	// Repointing the dead-pinned row at the live path collides with the sibling row:
	// the pin must be stripped from this row and (false, nil) returned, not an error.
	replaced, err := repo.ReplaceVirtualResultPin(ctx, deadFileID, deadPath, livePath)
	if err != nil {
		t.Fatalf("ReplaceVirtualResultPin error on collision: %v", err)
	}
	if replaced {
		t.Fatal("ReplaceVirtualResultPin returned true on collision; pin should be stripped instead")
	}

	var deadPathNow, livePathNow string
	if err := pool.QueryRow(ctx, `SELECT file_path FROM media_files WHERE id=$1`, deadFileID).Scan(&deadPathNow); err != nil {
		t.Fatalf("fetch dead-pinned row path: %v", err)
	}
	if deadPathNow != neutralPath {
		t.Fatalf("dead-pinned row was not unpinned on collision: got %q, want %q", deadPathNow, neutralPath)
	}
	if err := pool.QueryRow(ctx, `SELECT file_path FROM media_files WHERE file_path=$1 AND virtual_owner_installation_id=5 AND media_folder_id=$2`, livePath, folderID).Scan(&livePathNow); err != nil {
		t.Fatalf("fetch sibling live row path: %v", err)
	}
	if livePathNow != livePath {
		t.Fatalf("sibling live row was modified: got %q, want %q", livePathNow, livePath)
	}
}

func TestClearVirtualResultPin_StripsResultPreservingOtherParams(t *testing.T) {
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

	suffix := time.Now().UnixNano()
	contentID := fmt.Sprintf("virtual-pin-clear-%d", suffix)

	var folderID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Pin Clear %d", suffix)).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_files WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_item_libraries WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, folderID)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id,type,title,status,genres)
		VALUES($1,'movie','Pin Clear Item','matched','{}'::text[])`, contentID); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_item_libraries(content_id,media_folder_id)
		VALUES($1,$2)`, contentID, folderID); err != nil {
		t.Fatalf("seed item library: %v", err)
	}

	repo := NewFileRepository(pool)

	seed := func(path string) int {
		var id int
		if err := pool.QueryRow(ctx, `
			INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id)
			VALUES($1,$2,$3,0,'mkv',5) RETURNING id`, contentID, folderID, path).Scan(&id); err != nil {
			t.Fatalf("seed pinned virtual file %q: %v", path, err)
		}
		return id
	}
	fetch := func(id int) string {
		var path string
		if err := pool.QueryRow(ctx, `SELECT file_path FROM media_files WHERE id=$1`, id).Scan(&path); err != nil {
			t.Fatalf("fetch path for file %d: %v", id, err)
		}
		return path
	}

	// result-first order: ?result=abc&profile=1080p -> ?profile=1080p
	resultFirst := fmt.Sprintf("virtual://movie/tt%d?result=abc&profile=1080p", suffix)
	resultFirstID := seed(resultFirst)
	if err := repo.ClearVirtualResultPin(ctx, resultFirstID); err != nil {
		t.Fatalf("ClearVirtualResultPin error on result-first: %v", err)
	}
	if got := fetch(resultFirstID); got != fmt.Sprintf("virtual://movie/tt%d?profile=1080p", suffix) {
		t.Fatalf("result-first order: got %q, want %q", got, fmt.Sprintf("virtual://movie/tt%d?profile=1080p", suffix))
	}

	// profile-first order: ?profile=1080p&result=abc -> ?profile=1080p
	profileFirst := fmt.Sprintf("virtual://movie/tt%d?profile=1080p&result=abc", suffix+1)
	profileFirstID := seed(profileFirst)
	if err := repo.ClearVirtualResultPin(ctx, profileFirstID); err != nil {
		t.Fatalf("ClearVirtualResultPin error on profile-first: %v", err)
	}
	if got := fetch(profileFirstID); got != fmt.Sprintf("virtual://movie/tt%d?profile=1080p", suffix+1) {
		t.Fatalf("profile-first order: got %q, want %q", got, fmt.Sprintf("virtual://movie/tt%d?profile=1080p", suffix+1))
	}

	// trailing-only: ?result=abc -> bare path
	trailingOnly := fmt.Sprintf("virtual://movie/tt%d?result=abc", suffix+2)
	trailingOnlyID := seed(trailingOnly)
	if err := repo.ClearVirtualResultPin(ctx, trailingOnlyID); err != nil {
		t.Fatalf("ClearVirtualResultPin error on trailing-only: %v", err)
	}
	if got := fetch(trailingOnlyID); got != fmt.Sprintf("virtual://movie/tt%d", suffix+2) {
		t.Fatalf("trailing-only: got %q, want %q", got, fmt.Sprintf("virtual://movie/tt%d", suffix+2))
	}

	// no-pin -> no-op
	noPin := fmt.Sprintf("virtual://movie/tt%d?profile=1080p", suffix+3)
	noPinID := seed(noPin)
	if err := repo.ClearVirtualResultPin(ctx, noPinID); err != nil {
		t.Fatalf("ClearVirtualResultPin error on no-pin: %v", err)
	}
	if got := fetch(noPinID); got != noPin {
		t.Fatalf("no-pin: got %q, want %q", got, noPin)
	}

	// vanished row -> no-op (no error)
	if err := repo.ClearVirtualResultPin(ctx, 999999999); err != nil {
		t.Fatalf("ClearVirtualResultPin error on vanished row: %v", err)
	}
}

func TestReplaceVirtualResultPin_CollisionUnpinsProfileQualified(t *testing.T) {
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

	suffix := time.Now().UnixNano()
	contentID := fmt.Sprintf("virtual-pin-collision-profile-%d", suffix)
	deadPath := fmt.Sprintf("virtual://movie/tt%d?profile=1080p&result=dead-candidate", suffix)
	livePath := fmt.Sprintf("virtual://movie/tt%d?profile=1080p&result=live-winner", suffix)
	neutralPath := fmt.Sprintf("virtual://movie/tt%d?profile=1080p", suffix)

	var folderID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Pin Collision Profile %d", suffix)).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_files WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_item_libraries WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, folderID)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id,type,title,status,genres)
		VALUES($1,'movie','Pin Collision Profile Item','matched','{}'::text[])`, contentID); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_item_libraries(content_id,media_folder_id)
		VALUES($1,$2)`, contentID, folderID); err != nil {
		t.Fatalf("seed item library: %v", err)
	}

	// Row A: the dead-pinned row that playback is trying to repoint.
	var deadFileID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id)
		VALUES($1,$2,$3,0,'mkv',5) RETURNING id`, contentID, folderID, deadPath).Scan(&deadFileID); err != nil {
		t.Fatalf("seed dead-pinned file: %v", err)
	}
	// Row B: the sibling live row that already owns the winning (file_path, owner, folder) tuple.
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id)
		VALUES($1,$2,$3,0,'mkv',5)`, contentID, folderID, livePath); err != nil {
		t.Fatalf("seed sibling live file: %v", err)
	}

	repo := NewFileRepository(pool)

	replaced, err := repo.ReplaceVirtualResultPin(ctx, deadFileID, deadPath, livePath)
	if err != nil {
		t.Fatalf("ReplaceVirtualResultPin error on collision: %v", err)
	}
	if replaced {
		t.Fatal("ReplaceVirtualResultPin returned true on collision; pin should be stripped instead")
	}

	var deadPathNow, livePathNow string
	if err := pool.QueryRow(ctx, `SELECT file_path FROM media_files WHERE id=$1`, deadFileID).Scan(&deadPathNow); err != nil {
		t.Fatalf("fetch dead-pinned row path: %v", err)
	}
	if deadPathNow != neutralPath {
		t.Fatalf("dead-pinned row was not unpinned preserving profile: got %q, want %q", deadPathNow, neutralPath)
	}
	if err := pool.QueryRow(ctx, `SELECT file_path FROM media_files WHERE file_path=$1 AND virtual_owner_installation_id=5 AND media_folder_id=$2`, livePath, folderID).Scan(&livePathNow); err != nil {
		t.Fatalf("fetch sibling live row path: %v", err)
	}
	if livePathNow != livePath {
		t.Fatalf("sibling live row was modified: got %q, want %q", livePathNow, livePath)
	}
}

func TestReplaceVirtualResultPin_NoCollisionReplaces(t *testing.T) {
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

	suffix := time.Now().UnixNano()
	contentID := fmt.Sprintf("virtual-pin-nocollision-%d", suffix)
	deadPath := fmt.Sprintf("virtual://movie/tt%d?result=dead-candidate", suffix)
	livePath := fmt.Sprintf("virtual://movie/tt%d?result=live-winner", suffix)

	var folderID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Pin NoCollision %d", suffix)).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_files WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_item_libraries WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, folderID)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id,type,title,status,genres)
		VALUES($1,'movie','Pin NoCollision Item','matched','{}'::text[])`, contentID); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_item_libraries(content_id,media_folder_id)
		VALUES($1,$2)`, contentID, folderID); err != nil {
		t.Fatalf("seed item library: %v", err)
	}

	var fileID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id)
		VALUES($1,$2,$3,0,'mkv',5) RETURNING id`, contentID, folderID, deadPath).Scan(&fileID); err != nil {
		t.Fatalf("seed pinned virtual file: %v", err)
	}

	repo := NewFileRepository(pool)

	replaced, err := repo.ReplaceVirtualResultPin(ctx, fileID, deadPath, livePath)
	if err != nil {
		t.Fatalf("ReplaceVirtualResultPin error on no collision: %v", err)
	}
	if !replaced {
		t.Fatal("ReplaceVirtualResultPin returned false on no collision")
	}

	var currentPath string
	if err := pool.QueryRow(ctx, `SELECT file_path FROM media_files WHERE id=$1`, fileID).Scan(&currentPath); err != nil {
		t.Fatalf("fetch current path: %v", err)
	}
	if currentPath != livePath {
		t.Fatalf("file path was not replaced on no collision: got %q, want %q", currentPath, livePath)
	}
}

// TestReplaceVirtualCandidatesRetainsDelivered covers the delivery-evidence
// retention rule in ReplaceVirtualCandidates: a stale candidate that once
// delivered media bytes (last_delivered_at set) survives a provider re-list
// even when no user_watch_progress row points at it, an undelivered stale
// candidate is still deleted, and a re-listed known-good row keeps both its
// delivery evidence and its failure verdict (a re-list is not a recovery).
func TestReplaceVirtualCandidatesRetainsDelivered(t *testing.T) {
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

	suffix := time.Now().UnixNano()
	contentID := fmt.Sprintf("virtual-retain-delivered-%d", suffix)
	basePath := fmt.Sprintf("virtual://movie/tt%d?profile=1080p", suffix)
	deliveredPath := basePath + "&result=delivered"
	unplayedPath := basePath + "&result=unplayed"
	relistedPath := basePath + "&result=relisted"

	var folderID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Retain Delivered %d", suffix)).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_files WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_item_libraries WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, folderID)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id,type,title,status,genres)
		VALUES($1,'movie','Retain Delivered','matched','{}'::text[])`, contentID); err != nil {
		t.Fatalf("seed item: %v", err)
	}

	seed := func(path string, failed bool, delivered bool) int {
		var failedAt, deliveredAt *time.Time
		if failed {
			now := time.Now()
			failedAt = &now
		}
		if delivered {
			now := time.Now()
			deliveredAt = &now
		}
		var id int
		if err := pool.QueryRow(ctx, `
			INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id,failed_at,last_delivered_at)
			VALUES($1,$2,$3,0,'virtual',11,$4,$5) RETURNING id`,
			contentID, folderID, path, failedAt, deliveredAt).Scan(&id); err != nil {
			t.Fatalf("seed virtual file %q: %v", path, err)
		}
		return id
	}
	deliveredID := seed(deliveredPath, false, true)
	_ = seed(unplayedPath, false, false)
	// The re-listed row is seeded failed to prove a fresh listing preserves the
	// verdict (only real delivery recovers a row).
	relistedID := seed(relistedPath, true, true)

	repo := NewFileRepository(pool)
	source := &models.MediaFile{
		ContentID:                  contentID,
		MediaFolderID:              folderID,
		FilePath:                   basePath,
		VirtualOwnerInstallationID: 11,
	}
	// The re-list drops delivered and unplayed; only relisted is offered again.
	if err := repo.ReplaceVirtualCandidates(ctx, source, []VirtualCandidate{{URI: relistedPath, Label: "1080p"}}); err != nil {
		t.Fatalf("replace virtual candidates: %v", err)
	}

	read := func(id int) (path string, failedAt, lastDeliveredAt *time.Time) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT file_path, failed_at, last_delivered_at FROM media_files WHERE id=$1`, id).
			Scan(&path, &failedAt, &lastDeliveredAt); err != nil {
			t.Fatalf("read virtual file %d: %v", id, err)
		}
		return path, failedAt, lastDeliveredAt
	}

	// The delivered row survives the re-list with its evidence intact.
	deliveredPathNow, deliveredFailed, deliveredEvidence := read(deliveredID)
	if deliveredPathNow != deliveredPath || deliveredFailed != nil || deliveredEvidence == nil {
		t.Fatalf("delivered stale row not retained: path=%q failed=%v delivered=%v", deliveredPathNow, deliveredFailed, deliveredEvidence)
	}

	// The undelivered row is deleted.
	var unplayedCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_files WHERE content_id=$1 AND file_path=$2`, contentID, unplayedPath).Scan(&unplayedCount); err != nil {
		t.Fatalf("count undelivered stale row: %v", err)
	}
	if unplayedCount != 0 {
		t.Fatalf("undelivered stale candidate survived: count=%d, want 0", unplayedCount)
	}

	// The re-listed row keeps its id and delivery evidence, and the fresh
	// listing preserves its failure verdict: a re-list is not a recovery.
	relistedPathNow, relistedFailed, relistedEvidence := read(relistedID)
	if relistedPathNow != relistedPath || relistedFailed == nil || relistedEvidence == nil {
		t.Fatalf("relisted known-good row not updated: path=%q failed=%v delivered=%v", relistedPathNow, relistedFailed, relistedEvidence)
	}
}

// The playback optimistic-start gate reads delivery evidence from the
// MediaFile model, so the standard row scan must surface last_delivered_at.
func TestGetByIDExposesLastDeliveredAt(t *testing.T) {
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

	suffix := time.Now().UnixNano()
	deliveredAt := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	deliveredPath := fmt.Sprintf("virtual://movie/tt-scan-delivered-%d?result=delivered", suffix)
	neverDeliveredPath := fmt.Sprintf("virtual://movie/tt-scan-delivered-%d?result=never", suffix)

	var folderID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Scan Delivered %d", suffix)).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_files WHERE file_path = ANY($1)`, []string{deliveredPath, neverDeliveredPath})
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, folderID)
	})

	seed := func(path string, delivered *time.Time) int {
		var id int
		if err := pool.QueryRow(ctx, `
			INSERT INTO media_files(media_folder_id,file_path,file_size,container,last_delivered_at)
			VALUES($1,$2,0,'virtual',$3) RETURNING id`,
			folderID, path, delivered).Scan(&id); err != nil {
			t.Fatalf("seed media file %q: %v", path, err)
		}
		return id
	}
	deliveredID := seed(deliveredPath, &deliveredAt)
	neverID := seed(neverDeliveredPath, nil)

	repo := NewFileRepository(pool)
	delivered, err := repo.GetByID(ctx, deliveredID)
	if err != nil {
		t.Fatalf("GetByID delivered: %v", err)
	}
	if delivered.LastDeliveredAt == nil {
		t.Fatal("GetByID did not expose last_delivered_at for a delivered row")
	}
	if got := delivered.LastDeliveredAt.UTC(); !got.Equal(deliveredAt.UTC()) {
		t.Fatalf("last_delivered_at = %s, want %s", got, deliveredAt.UTC())
	}

	never, err := repo.GetByID(ctx, neverID)
	if err != nil {
		t.Fatalf("GetByID never-delivered: %v", err)
	}
	if never.LastDeliveredAt != nil {
		t.Fatalf("last_delivered_at = %v, want nil for a never-delivered row", never.LastDeliveredAt)
	}
}

// TestReplaceVirtualResultPin_ResetsCandidateEvidence requires a migrated
// database via SILO_TEST_DATABASE_URL. Moving a pin to a different provider
// candidate must discard the old candidate's probe and delivery evidence:
// otherwise the replacement inherits optimistic-start eligibility and a probe
// stamp for bytes it never delivered or had probed.
func TestReplaceVirtualResultPin_ResetsCandidateEvidence(t *testing.T) {
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

	suffix := time.Now().UnixNano()
	contentID := fmt.Sprintf("virtual-pin-evidence-%d", suffix)
	deadPath := fmt.Sprintf("virtual://movie/tt%d?result=dead-candidate", suffix)
	livePath := fmt.Sprintf("virtual://movie/tt%d?result=live-winner", suffix)

	var folderID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Pin Evidence %d", suffix)).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_files WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_item_libraries WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, folderID)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id,type,title,status,genres)
		VALUES($1,'movie','Pin Evidence Item','matched','{}'::text[])`, contentID); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_item_libraries(content_id,media_folder_id)
		VALUES($1,$2)`, contentID, folderID); err != nil {
		t.Fatalf("seed item library: %v", err)
	}

	probedAt := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	storedURLExpiry := time.Now().Add(2 * time.Hour).Truncate(time.Millisecond)
	var fileID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(
			content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id,
			probe_source,probe_updated_at,last_delivered_at,
			resolution,codec_video,codec_audio,hdr,bitrate,
			resolved_url,resolved_url_expires_at,
			video_tracks,audio_tracks,subtitle_tracks)
		VALUES($1,$2,$3,0,'mkv',5,'virtual',$4,$4,
			'2160p','hevc','eac3',true,5000,
			'https://93.184.216.34/stream/token=old-candidate',$5,
			'[{"codec":"hevc","width":3840,"height":2160,"frame_rate":"23.976"}]'::jsonb,
			'[{"codec":"eac3","channels":6,"language":"eng"}]'::jsonb,
			'[{"codec":"srt","language":"eng"}]'::jsonb)
		RETURNING id`, contentID, folderID, deadPath, probedAt, storedURLExpiry).Scan(&fileID); err != nil {
		t.Fatalf("seed probed pinned virtual file: %v", err)
	}

	repo := NewFileRepository(pool)
	replaced, err := repo.ReplaceVirtualResultPin(ctx, fileID, deadPath, livePath)
	if err != nil {
		t.Fatalf("ReplaceVirtualResultPin error: %v", err)
	}
	if !replaced {
		t.Fatal("ReplaceVirtualResultPin returned false on matching path")
	}

	file, err := repo.GetByPath(ctx, livePath)
	if err != nil {
		t.Fatalf("load replaced row: %v", err)
	}
	if file == nil {
		t.Fatal("replaced row not found")
	}
	if file.ProbeUpdatedAt != nil {
		t.Fatalf("probe_updated_at = %v, want nil after pin replacement", file.ProbeUpdatedAt)
	}
	if file.LastDeliveredAt != nil {
		t.Fatalf("last_delivered_at = %v, want nil after pin replacement", file.LastDeliveredAt)
	}
	if file.ProbeSource != "" {
		t.Fatalf("probe_source = %q, want empty after pin replacement", file.ProbeSource)
	}
	if file.Resolution != "" || file.CodecVideo != "" || file.CodecAudio != "" {
		t.Fatalf("candidate metadata survived replacement: resolution=%q codecVideo=%q codecAudio=%q",
			file.Resolution, file.CodecVideo, file.CodecAudio)
	}
	if file.Container == "mkv" {
		t.Fatalf("probed container survived replacement: %q", file.Container)
	}
	if len(file.VideoTracks) != 0 || len(file.AudioTracks) != 0 || len(file.SubtitleTracks) != 0 {
		t.Fatalf("track evidence survived replacement: video=%d audio=%d subtitle=%d",
			len(file.VideoTracks), len(file.AudioTracks), len(file.SubtitleTracks))
	}
	if file.ResolvedURL != "" || file.ResolvedURLExpiresAt != nil {
		t.Fatalf("stored URL for the old candidate survived replacement: url=%q expires=%v",
			file.ResolvedURL, file.ResolvedURLExpiresAt)
	}
}

// TestReplaceVirtualCandidatesRetainsActiveSessionCandidates proves the sweep
// never deletes the media_files row an active playback attempt or an open
// ABS-compatible session is streaming. Deleting it would cascade the attempt
// (playback_v3_attempts.effective_media_file_id is ON DELETE CASCADE) and strip
// a session's file identity (ON DELETE SET NULL). An expired attempt does not
// protect its row, and an unreferenced stale row is still deleted.
func TestReplaceVirtualCandidatesRetainsActiveSessionCandidates(t *testing.T) {
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

	suffix := time.Now().UnixNano()
	profileID := fmt.Sprintf("active-session-%d", suffix)
	contentID := fmt.Sprintf("virtual-active-session-%d", suffix)
	basePath := fmt.Sprintf("virtual://movie/tt%d?profile=1080p", suffix)
	liveAttemptPath := basePath + "&result=live-attempt"
	openSessionPath := basePath + "&result=open-session"
	expiredAttemptPath := basePath + "&result=expired-attempt"
	unreferencedPath := basePath + "&result=unreferenced"
	relistedPath := basePath + "&result=relisted"

	var folderID, userID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Active Session %d", suffix)).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO users(username, role) VALUES($1,'user') RETURNING id`,
		fmt.Sprintf("active-session-%d", suffix)).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM abs_playback_sessions WHERE id LIKE $1`, fmt.Sprintf("abs-%d-%%", suffix))
		_, _ = pool.Exec(ctx, `DELETE FROM playback_v3_attempts WHERE profile_id=$1`, profileID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_files WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_item_libraries WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, folderID)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, userID)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id,type,title,status,genres)
		VALUES($1,'movie','Active Session','matched','{}'::text[])`, contentID); err != nil {
		t.Fatalf("seed item: %v", err)
	}

	seed := func(path string) int {
		var id int
		if err := pool.QueryRow(ctx, `
			INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id)
			VALUES($1,$2,$3,0,'virtual',5) RETURNING id`,
			contentID, folderID, path).Scan(&id); err != nil {
			t.Fatalf("seed virtual candidate %q: %v", path, err)
		}
		return id
	}
	liveAttemptID := seed(liveAttemptPath)
	openSessionID := seed(openSessionPath)
	expiredAttemptID := seed(expiredAttemptPath)
	_ = seed(unreferencedPath)

	if _, err := pool.Exec(ctx, `
		INSERT INTO playback_v3_attempts(
			playback_attempt_id, session_id, user_id, profile_id,
			requested_media_file_id, effective_media_file_id,
			current_plan_id, current_plan, normalized_request, expires_at)
		VALUES($1, gen_random_uuid(), $2, $3, $4, $4, 'plan-live', '{}'::jsonb, '{}'::jsonb, NOW() + INTERVAL '1 hour')`,
		fmt.Sprintf("attempt-live-%d", suffix), userID, profileID, liveAttemptID); err != nil {
		t.Fatalf("seed live attempt: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO playback_v3_attempts(
			playback_attempt_id, session_id, user_id, profile_id,
			requested_media_file_id, effective_media_file_id,
			current_plan_id, current_plan, normalized_request, expires_at)
		VALUES($1, gen_random_uuid(), $2, $3, $4, $4, 'plan-expired', '{}'::jsonb, '{}'::jsonb, NOW() - INTERVAL '1 hour')`,
		fmt.Sprintf("attempt-expired-%d", suffix), userID, profileID, expiredAttemptID); err != nil {
		t.Fatalf("seed expired attempt: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO abs_playback_sessions(id, user_id, profile_id, content_id, media_file_id)
		VALUES($1, $2, 'default', $3, $4)`,
		fmt.Sprintf("abs-%d-open", suffix), userID, contentID, openSessionID); err != nil {
		t.Fatalf("seed open ABS session: %v", err)
	}

	repo := NewFileRepository(pool)
	source := &models.MediaFile{
		ContentID:                  contentID,
		MediaFolderID:              folderID,
		FilePath:                   basePath,
		VirtualOwnerInstallationID: 5,
	}
	if err := repo.ReplaceVirtualCandidates(ctx, source, []VirtualCandidate{{URI: relistedPath, Label: "1080p"}}); err != nil {
		t.Fatalf("replace virtual candidates: %v", err)
	}

	count := func(path string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_files WHERE content_id=$1 AND file_path=$2`, contentID, path).Scan(&n); err != nil {
			t.Fatalf("count %q: %v", path, err)
		}
		return n
	}
	if got := count(liveAttemptPath); got != 1 {
		t.Fatalf("live-attempt candidate deleted: count=%d, want 1", got)
	}
	if got := count(openSessionPath); got != 1 {
		t.Fatalf("open-session candidate deleted: count=%d, want 1", got)
	}
	if got := count(expiredAttemptPath); got != 0 {
		t.Fatalf("expired-attempt candidate survived: count=%d, want 0", got)
	}
	if got := count(unreferencedPath); got != 0 {
		t.Fatalf("unreferenced stale candidate survived: count=%d, want 0", got)
	}
	if got := count(relistedPath); got != 1 {
		t.Fatalf("relisted candidate missing: count=%d, want 1", got)
	}
}

// TestReplaceVirtualCandidatesKeepsFailureVerdictUntilDelivery proves a
// decode-rejected candidate stays failed across a provider re-list, so the
// auto-pick does not re-select the same undecodable release, and that a real
// delivery clears the verdict. A genuinely new candidate is inserted unfailed.
func TestReplaceVirtualCandidatesKeepsFailureVerdictUntilDelivery(t *testing.T) {
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

	suffix := time.Now().UnixNano()
	contentID := fmt.Sprintf("virtual-verdict-%d", suffix)
	basePath := fmt.Sprintf("virtual://movie/tt%d?profile=1080p", suffix)
	rejectedPath := basePath + "&result=rejected"
	freshPath := basePath + "&result=fresh"

	var folderID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Verdict %d", suffix)).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_files WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, folderID)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id,type,title,status,genres)
		VALUES($1,'movie','Verdict','matched','{}'::text[])`, contentID); err != nil {
		t.Fatalf("seed item: %v", err)
	}

	repo := NewFileRepository(pool)
	source := &models.MediaFile{
		ContentID: contentID, MediaFolderID: folderID, FilePath: basePath, VirtualOwnerInstallationID: 5,
	}

	if err := repo.ReplaceVirtualCandidates(ctx, source, []VirtualCandidate{{URI: rejectedPath, Label: "1080p"}}); err != nil {
		t.Fatalf("initial replace: %v", err)
	}
	rejected, err := repo.GetByPath(ctx, rejectedPath)
	if err != nil || rejected == nil {
		t.Fatalf("load rejected candidate: file=%v err=%v", rejected, err)
	}
	if err := repo.MarkVirtualCandidateDecodeRejected(ctx, rejected.ID, rejectedPath, nil); err != nil {
		t.Fatalf("mark decode rejected: %v", err)
	}

	// The provider re-lists the rejected candidate and adds a new one. The
	// re-list must not clear the verdict.
	if err := repo.ReplaceVirtualCandidates(ctx, source, []VirtualCandidate{
		{URI: rejectedPath, Label: "1080p"},
		{URI: freshPath, Label: "720p"},
	}); err != nil {
		t.Fatalf("re-list replace: %v", err)
	}
	var rejectedFailed *time.Time
	if err := pool.QueryRow(ctx, `SELECT failed_at FROM media_files WHERE id=$1`, rejected.ID).Scan(&rejectedFailed); err != nil {
		t.Fatalf("read rejected verdict: %v", err)
	}
	if rejectedFailed == nil {
		t.Fatal("re-list cleared the decode-rejection verdict; the auto-pick would re-select the undecodable release")
	}
	var freshFailed *time.Time
	if err := pool.QueryRow(ctx, `SELECT failed_at FROM media_files WHERE content_id=$1 AND file_path=$2`, contentID, freshPath).Scan(&freshFailed); err != nil {
		t.Fatalf("read fresh verdict: %v", err)
	}
	if freshFailed != nil {
		t.Fatalf("new candidate inserted failed: failed_at=%v", freshFailed)
	}

	// Real delivery is the recovery path: it clears the verdict and records
	// delivery evidence in the same update.
	if err := repo.MarkVirtualCandidateRecovered(ctx, rejected.ID, rejectedPath, rejectedFailed); err != nil {
		t.Fatalf("mark recovered: %v", err)
	}
	var clearedAt, deliveredAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT failed_at, last_delivered_at FROM media_files WHERE id=$1`, rejected.ID).Scan(&clearedAt, &deliveredAt); err != nil {
		t.Fatalf("read recovered row: %v", err)
	}
	if clearedAt != nil || deliveredAt == nil {
		t.Fatalf("recovery did not clear/record: failed_at=%v last_delivered_at=%v", clearedAt, deliveredAt)
	}
}

// TestReplaceVirtualCandidatesRetainsInsideStoreWindow covers the window-gated
// retention clause. A listed candidate omitted from a re-list survives while it
// is inside the configured candidate store window, and is swept once it falls
// outside. With no window configured the pre-window sweep runs unchanged.
func TestReplaceVirtualCandidatesRetainsInsideStoreWindow(t *testing.T) {
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

	suffix := time.Now().UnixNano()
	contentID := fmt.Sprintf("virtual-store-window-%d", suffix)
	basePath := fmt.Sprintf("virtual://movie/tt%d?profile=1080p", suffix)
	stalePath := basePath + "&result=stale"
	relistedPath := basePath + "&result=relisted"
	latePath := basePath + "&result=late"

	var folderID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Store Window %d", suffix)).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_files WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_item_libraries WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, folderID)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id,type,title,status,genres)
		VALUES($1,'movie','Store Window','matched','{}'::text[])`, contentID); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_item_libraries(content_id,media_folder_id)
		VALUES($1,$2)`, contentID, folderID); err != nil {
		t.Fatalf("seed item library: %v", err)
	}

	repo := NewFileRepository(pool)
	source := &models.MediaFile{
		ContentID:                  contentID,
		MediaFolderID:              folderID,
		FilePath:                   basePath,
		VirtualOwnerInstallationID: 5,
	}
	countPath := func(path string) int {
		t.Helper()
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_files WHERE content_id=$1 AND file_path=$2`, contentID, path).Scan(&count); err != nil {
			t.Fatalf("count %q: %v", path, err)
		}
		return count
	}
	backdate := func(path string, age time.Duration) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE media_files SET updated_at = NOW() - $3::interval WHERE content_id=$1 AND file_path=$2`, contentID, path, age.String()); err != nil {
			t.Fatalf("backdate %q: %v", path, err)
		}
	}

	// Seed the stale candidate, then re-list without it while it is inside a
	// 24h window: it is retained.
	if err := repo.ReplaceVirtualCandidates(ctx, source, []VirtualCandidate{{URI: stalePath, Label: "1080p"}}); err != nil {
		t.Fatalf("seed stale candidate: %v", err)
	}
	backdate(stalePath, time.Hour)
	repo.SetVirtualCandidateStoreWindow(func() time.Duration { return 24 * time.Hour })
	if err := repo.ReplaceVirtualCandidates(ctx, source, []VirtualCandidate{{URI: relistedPath, Label: "1080p"}}); err != nil {
		t.Fatalf("relist inside window: %v", err)
	}
	if got := countPath(stalePath); got != 1 {
		t.Fatalf("candidate inside the window was swept: count=%d, want 1", got)
	}

	// Backdate it beyond the window and re-list again: now it is swept.
	backdate(stalePath, 48*time.Hour)
	if err := repo.ReplaceVirtualCandidates(ctx, source, []VirtualCandidate{{URI: relistedPath, Label: "1080p"}}); err != nil {
		t.Fatalf("relist beyond window: %v", err)
	}
	if got := countPath(stalePath); got != 0 {
		t.Fatalf("candidate beyond the window survived: count=%d, want 0", got)
	}

	// With the window disabled, a fresh stale candidate is swept immediately.
	if err := repo.ReplaceVirtualCandidates(ctx, source, []VirtualCandidate{{URI: latePath, Label: "1080p"}}); err != nil {
		t.Fatalf("seed disabled-window candidate: %v", err)
	}
	repo.SetVirtualCandidateStoreWindow(nil)
	if err := repo.ReplaceVirtualCandidates(ctx, source, []VirtualCandidate{{URI: relistedPath, Label: "1080p"}}); err != nil {
		t.Fatalf("relist with window disabled: %v", err)
	}
	if got := countPath(latePath); got != 0 {
		t.Fatalf("candidate survived with the window disabled: count=%d, want 0", got)
	}
}

// TestListVirtualCandidatesNeedingRefreshSelectsOnlySignedExpiringRows covers
// the refresh pass's selection rule: only a row with a signed URL that expires
// inside the lead window and is itself inside the trust window is eligible. An
// unsigned URL (NULL expiry), an already-expired URL, a URL beyond the lead,
// and a row outside the window are all excluded.
func TestListVirtualCandidatesNeedingRefreshSelectsOnlySignedExpiringRows(t *testing.T) {
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

	suffix := time.Now().UnixNano()
	contentID := fmt.Sprintf("virtual-refresh-select-%d", suffix)
	var folderID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Refresh Select %d", suffix)).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_files WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, folderID)
	})

	now := time.Now()
	rows := []struct {
		name      string
		result    string
		url       *string
		expiresAt *time.Time
		updatedAt time.Time
		wantHit   bool
	}{
		{name: "unsigned", result: "unsigned", url: ptrString("https://93.184.216.34/u"), expiresAt: nil, updatedAt: now, wantHit: false},
		{name: "already expired", result: "expired", url: ptrString("https://93.184.216.34/e"), expiresAt: ptrTime(now.Add(-time.Hour)), updatedAt: now, wantHit: false},
		{name: "beyond lead", result: "far", url: ptrString("https://93.184.216.34/f"), expiresAt: ptrTime(now.Add(10 * time.Hour)), updatedAt: now, wantHit: false},
		{name: "outside window", result: "outside", url: ptrString("https://93.184.216.34/o"), expiresAt: ptrTime(now.Add(30 * time.Minute)), updatedAt: now.Add(-48 * time.Hour), wantHit: false},
		{name: "inside", result: "inside", url: ptrString("https://93.184.216.34/i"), expiresAt: ptrTime(now.Add(30 * time.Minute)), updatedAt: now.Add(-time.Hour), wantHit: true},
	}
	ids := map[string]int{}
	for _, row := range rows {
		path := fmt.Sprintf("virtual://movie/tt-%d?result=%s", suffix, row.result)
		var id int
		if err := pool.QueryRow(ctx, `
			INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id,resolved_url,resolved_url_expires_at,updated_at)
			VALUES($1,$2,$3,0,'virtual',5,$4,$5,$6) RETURNING id`,
			contentID, folderID, path, row.url, row.expiresAt, row.updatedAt).Scan(&id); err != nil {
			t.Fatalf("seed %s: %v", row.name, err)
		}
		ids[row.name] = id
	}

	repo := NewFileRepository(pool)
	got, err := repo.ListVirtualCandidatesNeedingRefresh(ctx, 24*time.Hour, 2*time.Hour, 50)
	if err != nil {
		t.Fatalf("ListVirtualCandidatesNeedingRefresh: %v", err)
	}
	found := map[int]bool{}
	for _, row := range got {
		if row.ContentID == contentID {
			found[row.ID] = true
		}
	}
	for _, row := range rows {
		if found[ids[row.name]] != row.wantHit {
			t.Fatalf("%s row selected=%v, want %v", row.name, found[ids[row.name]], row.wantHit)
		}
	}

	// A disabled window matches nothing.
	if none, err := repo.ListVirtualCandidatesNeedingRefresh(ctx, 0, 2*time.Hour, 50); err != nil || len(none) != 0 {
		t.Fatalf("window=0 returned %d rows (err=%v), want 0", len(none), err)
	}
}

func ptrString(v string) *string     { return &v }
func ptrTime(v time.Time) *time.Time { return &v }

// TestReplaceVirtualCandidatesEmptyListingPreservesState proves a zero-count
// replacement is inert: it neither rewrites nor sweeps the stored candidates.
// An empty provider answer is a hiccup and must never erase a healthy version
// list.
func TestReplaceVirtualCandidatesEmptyListingPreservesState(t *testing.T) {
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

	suffix := time.Now().UnixNano()
	contentID := fmt.Sprintf("virtual-empty-listing-%d", suffix)
	basePath := fmt.Sprintf("virtual://movie/tt%d?profile=1080p", suffix)
	pinnedPath := basePath + "&result=pinned"

	var folderID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Empty Listing %d", suffix)).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_files WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_item_libraries WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, folderID)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id,type,title,status,genres)
		VALUES($1,'movie','Empty Listing','matched','{}'::text[])`, contentID); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_item_libraries(content_id,media_folder_id)
		VALUES($1,$2)`, contentID, folderID); err != nil {
		t.Fatalf("seed library link: %v", err)
	}

	repo := NewFileRepository(pool)
	source := &models.MediaFile{
		ContentID:                  contentID,
		MediaFolderID:              folderID,
		FilePath:                   basePath,
		VirtualOwnerInstallationID: 93,
	}
	if err := repo.ReplaceVirtualCandidates(ctx, source, []VirtualCandidate{{
		URI: pinnedPath, Label: "1080p", ResolvedURL: "https://93.184.216.34/stream/token=stored",
	}}); err != nil {
		t.Fatalf("seed candidate: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE media_files SET failed_at=NOW() WHERE content_id=$1 AND file_path=$2`, contentID, pinnedPath); err != nil {
		t.Fatalf("stamp seed candidate: %v", err)
	}

	if err := repo.ReplaceVirtualCandidates(ctx, source, nil); err != nil {
		t.Fatalf("empty replacement must be inert, got: %v", err)
	}

	var count int
	var failedAt *time.Time
	var resolvedURL *string
	if err := pool.QueryRow(ctx, `
		SELECT count(*), max(failed_at), max(resolved_url)
		FROM media_files WHERE content_id=$1 AND file_path=$2`, contentID, pinnedPath).
		Scan(&count, &failedAt, &resolvedURL); err != nil {
		t.Fatalf("inspect candidate: %v", err)
	}
	if count != 1 {
		t.Fatalf("candidate rows = %d, want 1 after an empty listing", count)
	}
	if resolvedURL == nil || *resolvedURL != "https://93.184.216.34/stream/token=stored" {
		t.Fatalf("stored URL = %v, want it preserved", resolvedURL)
	}
	if failedAt == nil {
		t.Fatal("failed_at was cleared by an empty listing, want it preserved")
	}
}

// TestPruneDeadAbsentVirtualCandidatesHonorsRetention proves the repository
// prune applies the same keep-list the re-list sweep does: a row that delivered
// bytes, is inside the candidate store window, is some user's last-played file,
// or is pointed at by a live playback attempt survives even when the caller
// nominates it for deletion. Only the genuinely unprotected dead row is removed.
func TestPruneDeadAbsentVirtualCandidatesHonorsRetention(t *testing.T) {
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

	suffix := time.Now().UnixNano()
	contentID := fmt.Sprintf("virtual-prune-retention-%d", suffix)
	basePath := fmt.Sprintf("virtual://movie/tt-prune-%d", suffix)

	var folderID, userID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Prune Retention %d", suffix)).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO users(username, role) VALUES($1,'user') RETURNING id`,
		fmt.Sprintf("prune-retention-%d", suffix)).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id,type,title,status,genres)
		VALUES($1,'movie','Prune Retention','matched','{}'::text[])`, contentID); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	t.Cleanup(func() {
		b := context.Background()
		_, _ = pool.Exec(b, `DELETE FROM playback_v3_attempts WHERE user_id=$1`, userID)
		_, _ = pool.Exec(b, `DELETE FROM user_watch_progress WHERE user_id=$1`, userID)
		_, _ = pool.Exec(b, `DELETE FROM media_files WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(b, `DELETE FROM media_items WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(b, `DELETE FROM media_folders WHERE id=$1`, folderID)
		_, _ = pool.Exec(b, `DELETE FROM users WHERE id=$1`, userID)
	})

	seed := func(result string, delivered bool) int {
		var deliveredAt *time.Time
		if delivered {
			now := time.Now()
			deliveredAt = &now
		}
		var id int
		if err := pool.QueryRow(ctx, `
			INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id,failed_at,last_delivered_at)
			VALUES($1,$2,$3,0,'virtual',5,NOW(),$4) RETURNING id`,
			contentID, folderID, basePath+"?result="+result, deliveredAt).Scan(&id); err != nil {
			t.Fatalf("seed %q: %v", result, err)
		}
		return id
	}
	removable := seed("removable", false)
	delivered := seed("delivered", true)
	inWindow := seed("in-window", false)
	lastPlayed := seed("last-played", false)
	liveAttempt := seed("live-attempt", false)

	// Backdate every row except the in-window one beyond the 24h window, so the
	// window protects only that row and the others are decided by their specific
	// retention evidence.
	if _, err := pool.Exec(ctx, `
		UPDATE media_files SET updated_at = NOW() - interval '48 hours'
		WHERE id = ANY($1::bigint[])`,
		[]int{removable, delivered, lastPlayed, liveAttempt}); err != nil {
		t.Fatalf("backdate rows: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO user_watch_progress(user_id, profile_id, media_item_id, position_seconds, duration_seconds, last_file_id)
		VALUES($1,'default',$2,1,100,$3)`, userID, contentID, lastPlayed); err != nil {
		t.Fatalf("seed watch progress: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO playback_v3_attempts(
			playback_attempt_id, session_id, user_id, profile_id,
			requested_media_file_id, effective_media_file_id,
			current_plan_id, current_plan, normalized_request, expires_at)
		VALUES($1, gen_random_uuid(), $2, 'default', $3, $3, 'plan', '{}'::jsonb, '{}'::jsonb, NOW() + interval '1 hour')`,
		fmt.Sprintf("attempt-%d", suffix), userID, liveAttempt); err != nil {
		t.Fatalf("seed live attempt: %v", err)
	}

	repo := NewFileRepository(pool)
	repo.SetVirtualCandidateStoreWindow(func() time.Duration { return 24 * time.Hour })
	t.Cleanup(func() { repo.SetVirtualCandidateStoreWindow(nil) })

	all := []int{removable, delivered, inWindow, lastPlayed, liveAttempt}
	pruned, err := repo.PruneDeadAbsentVirtualCandidates(ctx, all)
	if err != nil {
		t.Fatalf("PruneDeadAbsentVirtualCandidates: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("pruned = %d, want 1 (only the unprotected row)", pruned)
	}

	exists := func(id int) bool {
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_files WHERE id=$1`, id).Scan(&count); err != nil {
			t.Fatalf("count %d: %v", id, err)
		}
		return count > 0
	}
	if exists(removable) {
		t.Fatal("unprotected dead row survived the prune")
	}
	for name, id := range map[string]int{"delivered": delivered, "in-window": inWindow, "last-played": lastPlayed, "live-attempt": liveAttempt} {
		if !exists(id) {
			t.Fatalf("%s row was pruned despite its retention rule", name)
		}
	}
}
