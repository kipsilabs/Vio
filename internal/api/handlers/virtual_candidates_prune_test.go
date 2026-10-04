package handlers

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/scanner"
)

// candidatePruneFixture seeds a throwaway library, item, user, source row and
// candidate rows for the dead-candidate prune tests. The handlers test pool is
// a disposable migrated database serialized by an advisory lock, so fixtures
// can be created and torn down freely.
type candidatePruneFixture struct {
	pool      *pgxpool.Pool
	repo      *scanner.FileRepository
	ctx       context.Context
	contentID string
	basePath  string
	folderID  int
	ownerID   int
	userID    int
}

func newCandidatePruneFixture(t *testing.T) *candidatePruneFixture {
	t.Helper()
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	contentID := fmt.Sprintf("movie-prune-%d", suffix)
	basePath := fmt.Sprintf("virtual://movie/prune-%d", suffix)
	ownerID := 8800 + int(suffix%100)

	var folderID, userID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Prune %d", suffix)).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO users(username, role) VALUES($1,'user') RETURNING id`,
		fmt.Sprintf("prune-%d", suffix)).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id,type,title,status,genres)
		VALUES($1,'movie','Prune Test','matched','{}'::text[])`, contentID); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	repo := scanner.NewFileRepository(pool)
	t.Cleanup(func() {
		b := context.Background()
		_, _ = pool.Exec(b, `DELETE FROM playback_v3_attempts WHERE user_id=$1`, userID)
		_, _ = pool.Exec(b, `DELETE FROM abs_playback_sessions WHERE user_id=$1`, userID)
		_, _ = pool.Exec(b, `DELETE FROM user_watch_progress WHERE user_id=$1`, userID)
		_, _ = pool.Exec(b, `DELETE FROM media_files WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(b, `DELETE FROM media_items WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(b, `DELETE FROM media_folders WHERE id=$1`, folderID)
		_, _ = pool.Exec(b, `DELETE FROM users WHERE id=$1`, userID)
	})
	// The provider-neutral source row every candidate shares a group with.
	if _, err := repo.Upsert(ctx, models.MediaFile{
		ContentID: contentID, MediaFolderID: folderID, FilePath: basePath,
		Container: "virtual", ProbeSource: "virtual", VirtualOwnerInstallationID: ownerID,
	}); err != nil {
		t.Fatalf("seed source row: %v", err)
	}
	return &candidatePruneFixture{
		pool: pool, repo: repo, ctx: ctx, contentID: contentID, basePath: basePath,
		folderID: folderID, ownerID: ownerID, userID: userID,
	}
}

func (f *candidatePruneFixture) source() *models.MediaFile {
	return &models.MediaFile{
		ContentID: f.contentID, MediaFolderID: f.folderID,
		FilePath: f.basePath, VirtualOwnerInstallationID: f.ownerID,
	}
}

func (f *candidatePruneFixture) seedCandidate(t *testing.T, result string, failed bool, delivered bool, releaseName, videoHash string) int {
	t.Helper()
	path := f.basePath + "?result=" + result
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
	if err := f.pool.QueryRow(f.ctx, `
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id,failed_at,last_delivered_at,provider_release_name,provider_video_hash)
		VALUES($1,$2,$3,0,'virtual',$4,$5,$6,$7,$8) RETURNING id`,
		f.contentID, f.folderID, path, f.ownerID, failedAt, deliveredAt,
		nullIfEmpty(releaseName), nullIfEmpty(videoHash)).Scan(&id); err != nil {
		t.Fatalf("seed candidate %q: %v", result, err)
	}
	return id
}

func (f *candidatePruneFixture) rowExists(t *testing.T, id int) bool {
	t.Helper()
	var count int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM media_files WHERE id=$1`, id).Scan(&count); err != nil {
		t.Fatalf("count row %d: %v", id, err)
	}
	return count > 0
}

func (f *candidatePruneFixture) pruner() *CandidateDeadPruner {
	return &CandidateDeadPruner{
		ContentFiles: f.repo.GetByContentID,
		EpisodeFiles: f.repo.GetByEpisodeID,
		Delete:       f.repo,
	}
}

// freshStream builds one fresh listing entry.
func freshStream(basePath, result, releaseName, videoHash string) VirtualPlaybackStream {
	return VirtualPlaybackStream{
		ID: result, URI: basePath + "?result=" + result,
		ProviderReleaseName: releaseName, ProviderVideoHash: videoHash,
	}
}

// TestPruneDeadAbsentCandidatesDeletesDeadUnprotectedRow proves the core rule:
// a candidate that is absent from the fresh listing, carries a failed_at
// verdict, and is protected by nothing is pruned.
func TestPruneDeadAbsentCandidatesDeletesDeadUnprotectedRow(t *testing.T) {
	f := newCandidatePruneFixture(t)
	deadAbsent := f.seedCandidate(t, "dead-absent", true, false, "", "")

	pruned, err := f.pruner().PruneDeadAbsentCandidates(f.ctx, f.contentID, "",
		[]*models.MediaFile{f.source()}, []VirtualPlaybackStream{freshStream(f.basePath, "live", "", "")})
	if err != nil {
		t.Fatalf("PruneDeadAbsentCandidates: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("pruned = %d, want 1", pruned)
	}
	if f.rowExists(t, deadAbsent) {
		t.Fatal("dead, absent, unprotected row survived the prune")
	}
}

// TestPruneDeadAbsentCandidatesRetainsLastPlayed proves a failed row that is a
// user's last-played file is never pruned even when dead and absent.
func TestPruneDeadAbsentCandidatesRetainsLastPlayed(t *testing.T) {
	f := newCandidatePruneFixture(t)
	played := f.seedCandidate(t, "failed-played", true, false, "", "")
	live := f.seedCandidate(t, "live-played", false, false, "", "")
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO user_watch_progress(user_id, profile_id, media_item_id, position_seconds, duration_seconds, last_file_id)
		VALUES($1,'default',$2,10,100,$3)`, f.userID, f.contentID, played); err != nil {
		t.Fatalf("seed watch progress: %v", err)
	}

	pruned, err := f.pruner().PruneDeadAbsentCandidates(f.ctx, f.contentID, "",
		[]*models.MediaFile{f.source()}, []VirtualPlaybackStream{freshStream(f.basePath, "other", "", "")})
	if err != nil {
		t.Fatalf("PruneDeadAbsentCandidates: %v", err)
	}
	if pruned != 0 {
		t.Fatalf("pruned = %d, want 0 (last-played row retained)", pruned)
	}
	if !f.rowExists(t, played) {
		t.Fatal("failed, absent, last-played row was pruned")
	}
	if !f.rowExists(t, live) {
		t.Fatal("live, absent, last-played row was pruned")
	}
}

// TestPruneDeadAbsentCandidatesRetainsDeliveredInWindowAndLive proves the three
// remaining retention rules independently keep a dead, absent row: delivered
// bytes, the candidate store window, and a live playback attempt.
func TestPruneDeadAbsentCandidatesRetainsDeliveredInWindowAndLive(t *testing.T) {
	f := newCandidatePruneFixture(t)

	delivered := f.seedCandidate(t, "dead-delivered", true, true, "", "")

	// The in-window row is protected only while the window is positive.
	inWindow := f.seedCandidate(t, "dead-in-window", true, false, "", "")
	f.repo.SetVirtualCandidateStoreWindow(func() time.Duration { return 24 * time.Hour })
	t.Cleanup(func() { f.repo.SetVirtualCandidateStoreWindow(nil) })

	live := f.seedCandidate(t, "dead-live", true, false, "", "")
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO playback_v3_attempts(
			playback_attempt_id, session_id, user_id, profile_id,
			requested_media_file_id, effective_media_file_id,
			current_plan_id, current_plan, normalized_request, expires_at)
		VALUES($1, gen_random_uuid(), $2, 'default', $3, $3, 'plan', '{}'::jsonb, '{}'::jsonb, NOW() + interval '1 hour')`,
		fmt.Sprintf("attempt-%d", time.Now().UnixNano()), f.userID, live); err != nil {
		t.Fatalf("seed live attempt: %v", err)
	}

	pruned, err := f.pruner().PruneDeadAbsentCandidates(f.ctx, f.contentID, "",
		[]*models.MediaFile{f.source()}, []VirtualPlaybackStream{freshStream(f.basePath, "other", "", "")})
	if err != nil {
		t.Fatalf("PruneDeadAbsentCandidates: %v", err)
	}
	if pruned != 0 {
		t.Fatalf("pruned = %d, want 0 (all three rows protected)", pruned)
	}
	for name, id := range map[string]int{"delivered": delivered, "in-window": inWindow, "live": live} {
		if !f.rowExists(t, id) {
			t.Fatalf("%s row was pruned despite its retention rule", name)
		}
	}

	// Disabling the window drops the in-window protection: that row alone is
	// now prunable.
	f.repo.SetVirtualCandidateStoreWindow(nil)
	pruned, err = f.pruner().PruneDeadAbsentCandidates(f.ctx, f.contentID, "",
		[]*models.MediaFile{f.source()}, []VirtualPlaybackStream{freshStream(f.basePath, "other", "", "")})
	if err != nil {
		t.Fatalf("PruneDeadAbsentCandidates (window off): %v", err)
	}
	if pruned != 1 || f.rowExists(t, inWindow) {
		t.Fatalf("window-off prune = %d, in-window row exists=%v; want 1/false", pruned, f.rowExists(t, inWindow))
	}
	if !f.rowExists(t, delivered) || !f.rowExists(t, live) {
		t.Fatal("delivered or live row was pruned when the window was disabled")
	}
}

// TestPruneDeadAbsentCandidatesLeavesFreshRows proves a dead row still present
// in the fresh listing is untouched, whether it matches by exact result id or
// by durable release identity after a renumber.
func TestPruneDeadAbsentCandidatesLeavesFreshRows(t *testing.T) {
	f := newCandidatePruneFixture(t)
	exact := f.seedCandidate(t, "fresh-exact", true, false, "", "")
	renumbered := f.seedCandidate(t, "fresh-old-id", true, false, "Some.Movie.2024.1080p-GRP", "hash-abc")

	fresh := []VirtualPlaybackStream{
		freshStream(f.basePath, "fresh-exact", "", ""),
		// A renumbered result id for the same release, matched by hash.
		freshStream(f.basePath, "fresh-new-id", "Some.Movie.2024.1080p-GRP", "hash-abc"),
	}
	pruned, err := f.pruner().PruneDeadAbsentCandidates(f.ctx, f.contentID, "",
		[]*models.MediaFile{f.source()}, fresh)
	if err != nil {
		t.Fatalf("PruneDeadAbsentCandidates: %v", err)
	}
	if pruned != 0 {
		t.Fatalf("pruned = %d, want 0 (both dead rows are still listed)", pruned)
	}
	if !f.rowExists(t, exact) || !f.rowExists(t, renumbered) {
		t.Fatal("a dead row still present in the fresh listing was pruned")
	}
}

// TestPruneDeadAbsentCandidatesAltmountFailedVerdict proves an AltMount failed
// verdict alone brands an absent row dead, while an unconfigured/unknown
// verdict (or a row with no release name) never does.
func TestPruneDeadAbsentCandidatesAltmountFailedVerdict(t *testing.T) {
	f := newCandidatePruneFixture(t)
	altmountDead := f.seedCandidate(t, "altmount-dead", false, false, "Dead.Release.2024-GRP", "")
	altmountUnknown := f.seedCandidate(t, "altmount-unknown", false, false, "Unknown.Release.2024-GRP", "")
	nameless := f.seedCandidate(t, "nameless", false, false, "", "")

	pruner := f.pruner()
	pruner.ProviderFailed = func(releaseName string) (bool, bool) {
		if strings.Contains(releaseName, "Dead.Release") {
			return true, true
		}
		return false, false
	}
	pruned, err := pruner.PruneDeadAbsentCandidates(f.ctx, f.contentID, "",
		[]*models.MediaFile{f.source()}, []VirtualPlaybackStream{freshStream(f.basePath, "other", "", "")})
	if err != nil {
		t.Fatalf("PruneDeadAbsentCandidates: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("pruned = %d, want 1", pruned)
	}
	if f.rowExists(t, altmountDead) {
		t.Fatal("AltMount-failed absent row survived")
	}
	if !f.rowExists(t, altmountUnknown) {
		t.Fatal("row with an unknown AltMount verdict was pruned")
	}
	if !f.rowExists(t, nameless) {
		t.Fatal("row without a release name was pruned")
	}
}

// TestPruneDeadAbsentCandidatesRequiresScope proves no prune runs when the
// listing resolves no source scope, so absence cannot be established.
func TestPruneDeadAbsentCandidatesRequiresScope(t *testing.T) {
	f := newCandidatePruneFixture(t)
	deadAbsent := f.seedCandidate(t, "dead-out-of-scope", true, false, "", "")

	pruned, err := f.pruner().PruneDeadAbsentCandidates(f.ctx, f.contentID, "", nil, nil)
	if err != nil {
		t.Fatalf("PruneDeadAbsentCandidates: %v", err)
	}
	if pruned != 0 || !f.rowExists(t, deadAbsent) {
		t.Fatalf("pruned = %d, row exists=%v; want 0/true on an empty scope", pruned, f.rowExists(t, deadAbsent))
	}
}
