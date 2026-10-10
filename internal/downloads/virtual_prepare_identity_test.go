package downloads

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// virtualPrepareFixture is a managed fixture plus a provider-backed virtual file
// and a second account, so a shared preparation can be exercised across two
// profiles of one account and across accounts.
type virtualPrepareFixture struct {
	f                         managedFixture
	virtualFileID             int
	otherUserID               int
	otherProfile, otherDevice string
}

func seedVirtualPrepareFixture(t *testing.T) virtualPrepareFixture {
	t.Helper()
	f := seedManagedFixture(t)
	ctx := context.Background()

	// The virtual-file identity columns must exist for a provider-backed row.
	var present *string
	if err := f.pool.QueryRow(ctx, `SELECT to_regclass('public.download_artifacts')::text`).Scan(&present); err != nil {
		t.Fatalf("check download_artifacts: %v", err)
	}
	if present == nil {
		t.Skip("download_artifacts migration has not been applied")
	}
	var hasOwnershipColumn bool
	if err := f.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM information_schema.columns
		 WHERE table_schema='public' AND table_name='media_files' AND column_name='virtual_owner_installation_id')`,
	).Scan(&hasOwnershipColumn); err != nil {
		t.Fatalf("check virtual ownership column: %v", err)
	}
	if !hasOwnershipColumn {
		t.Skip("virtual media file ownership migration has not been applied")
	}

	// Turn the fixture file into a provider-backed virtual row carrying the
	// durable identity that authorizes a preparation.
	virtualPath := fmt.Sprintf("virtual://movie/tt%d?result=shared", time.Now().UnixNano())
	if _, err := f.pool.Exec(ctx,
		`UPDATE media_files SET container='virtual', file_path=$2,
		     virtual_owner_installation_id=7, provider_video_hash='vh-1', provider_guid='guid-1',
		     provider_release_name='Release.2026.1080p', provider_release_size=2048000
		 WHERE id=$1`, f.fileID, virtualPath,
	); err != nil {
		t.Fatalf("make media file virtual: %v", err)
	}

	suffix := time.Now().UnixNano()
	var otherUserID int
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO users (username, role, download_allowed) VALUES ($1,'user',true) RETURNING id`,
		fmt.Sprintf("dluser2-%d", suffix),
	).Scan(&otherUserID); err != nil {
		t.Fatalf("seed second user: %v", err)
	}
	otherProfile := fmt.Sprintf("dlp-c-%d", suffix)
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO user_profiles (id, user_id, name) VALUES ($1,$2,'C')`, otherProfile, otherUserID,
	); err != nil {
		t.Fatalf("seed second profile: %v", err)
	}
	otherDevice := fmt.Sprintf("dev-c-%d", suffix)
	if err := f.repo.EnsureDevice(ctx, otherUserID, otherProfile, otherDevice, "Phone C", "android"); err != nil {
		t.Fatalf("ensure second device: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(ctx, `DELETE FROM downloads WHERE user_id = $1`, otherUserID)
		_, _ = f.pool.Exec(ctx, `DELETE FROM user_devices WHERE user_id = $1`, otherUserID)
		_, _ = f.pool.Exec(ctx, `DELETE FROM user_profiles WHERE user_id = $1`, otherUserID)
		_, _ = f.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, otherUserID)
	})

	return virtualPrepareFixture{f: f, virtualFileID: f.fileID, otherUserID: otherUserID, otherProfile: otherProfile, otherDevice: otherDevice}
}

// TestVirtualPrepareIsRequesterScoped proves a shared virtual preparation is
// safe for every requester: two profiles of one account and a second account
// all link the same artifact, each can read only its own row, and deleting one
// requester's row never aborts the job the others still wait on.
func TestVirtualPrepareIsRequesterScoped(t *testing.T) {
	vf := seedVirtualPrepareFixture(t)
	f := vf.f
	ctx := context.Background()

	arepo := NewArtifactRepository(f.pool)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(ctx, `DELETE FROM download_artifacts WHERE media_file_id = $1`, vf.virtualFileID)
	})
	artifact := newArtifact(t, vf.virtualFileID, fmt.Sprintf("hash-virtual-%d", time.Now().UnixNano()))
	if _, _, err := arepo.EnsureQueued(ctx, artifact); err != nil {
		t.Fatalf("ensure virtual artifact: %v", err)
	}

	now := time.Now()
	mkRow := func(userID int, profile, device string) string {
		id := fmt.Sprintf("dl-virt-%d-%s", now.UnixNano(), profile)
		if err := f.repo.Create(ctx, &Download{
			ID: id, UserID: userID, ProfileID: profile, DeviceID: device,
			MediaFileID: vf.virtualFileID, ContentID: f.contentID, Kind: KindQueued,
			Status: StatusPreparing, Format: FormatOriginal, ArtifactID: artifact.ID,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create %s row: %v", profile, err)
		}
		return id
	}
	rowA := mkRow(f.userID, f.profileA, f.deviceA)
	rowB := mkRow(f.userID, f.profileB, f.deviceB)
	rowC := mkRow(vf.otherUserID, vf.otherProfile, vf.otherDevice)

	svc := &Service{repo: f.repo}

	// Each requester reads exactly its own row; another scope's id is not found.
	if got, err := svc.Get(ctx, f.userID, f.profileA, f.deviceA, rowA); err != nil || got.ID != rowA {
		t.Fatalf("profile A Get = %+v (%v), want its own row", got, err)
	}
	if got, err := svc.Get(ctx, f.userID, f.profileB, f.deviceB, rowB); err != nil || got.ID != rowB {
		t.Fatalf("profile B Get = %+v (%v), want its own row", got, err)
	}
	if _, err := svc.Get(ctx, f.userID, f.profileB, f.deviceB, rowA); !errors.Is(err, ErrNotFound) {
		t.Fatalf("profile B reading profile A's row err = %v, want ErrNotFound", err)
	}
	if _, err := svc.Get(ctx, vf.otherUserID, vf.otherProfile, vf.otherDevice, rowA); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second account reading first account's row err = %v, want ErrNotFound", err)
	}

	// Deleting one requester's row leaves the artifact: two live links remain.
	if err := f.repo.DeleteManaged(ctx, rowA, f.userID, f.profileA, f.deviceA); err != nil {
		t.Fatalf("delete row A: %v", err)
	}
	mgr := &ArtifactManager{repo: arepo}
	mgr.CancelAbandonedPrepare(ctx, artifact.ID)
	if _, err := arepo.GetByID(ctx, artifact.ID); err != nil {
		t.Fatalf("shared virtual artifact was aborted for one requester's delete: %v", err)
	}

	// Deleting the last two requesters' rows abandons the job; it is released,
	// not left to lease expiry.
	if err := f.repo.DeleteManaged(ctx, rowB, f.userID, f.profileB, f.deviceB); err != nil {
		t.Fatalf("delete row B: %v", err)
	}
	if err := f.repo.DeleteManaged(ctx, rowC, vf.otherUserID, vf.otherProfile, vf.otherDevice); err != nil {
		t.Fatalf("delete row C: %v", err)
	}
	mgr.CancelAbandonedPrepare(ctx, artifact.ID)
	if _, err := arepo.GetByID(ctx, artifact.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("abandoned virtual artifact still present: %v", err)
	}
	// Cancel-before-start: a worker that arrives after the last requester left
	// finds no job to claim, so it never encodes bytes no one waits on.
	if _, err := arepo.ClaimNext(ctx, "late-worker", time.Minute); !errors.Is(err, ErrNoArtifactJob) {
		t.Fatalf("late worker claimed a canceled job: %v", err)
	}
}

// TestCancelAbandonedPrepareStopsLocalAttempt covers the cancel-from-this-worker
// half of the cancel-vs-attach race: an abandoned running job is deleted and the
// attempt this replica runs is cancelled at once. A worker on another replica has
// no local attempt here and is stopped by the lease fence instead, which the
// deleted row already guarantees.
func TestCancelAbandonedPrepareStopsLocalAttempt(t *testing.T) {
	f := seedManagedFixture(t)
	ctx := context.Background()
	var present *string
	if err := f.pool.QueryRow(ctx, `SELECT to_regclass('public.download_artifacts')::text`).Scan(&present); err != nil || present == nil {
		t.Skip("download_artifacts migration has not been applied")
	}
	arepo := NewArtifactRepository(f.pool)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(ctx, `DELETE FROM download_artifacts WHERE media_file_id = $1`, f.fileID)
	})

	artifact := newArtifact(t, f.fileID, fmt.Sprintf("hash-cancel-%d", time.Now().UnixNano()))
	if _, _, err := arepo.EnsureQueued(ctx, artifact); err != nil {
		t.Fatalf("ensure artifact: %v", err)
	}
	if _, err := arepo.ClaimNext(ctx, "worker-a", time.Minute); err != nil {
		t.Fatalf("claim artifact: %v", err)
	}

	// This replica runs the attempt; the map is keyed by artifact id.
	cancelled := false
	cancel := func() { cancelled = true }
	mgr := &ArtifactManager{repo: arepo, localAttempts: map[string]*localAttempt{artifact.ID: {cancel: cancel}}}

	mgr.CancelAbandonedPrepare(ctx, artifact.ID)

	if !cancelled {
		t.Fatal("abandoned running job did not cancel this replica's local attempt")
	}
	if _, err := arepo.GetByID(ctx, artifact.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("abandoned running artifact still present: %v", err)
	}
}

// TestSetVirtualInputResolverDrainsImmediately covers readiness ordering: when
// the router wires the resolver after the startup sweep, a virtual job deferred
// before wiring is drained at once instead of waiting out its retry backoff.
func TestSetVirtualInputResolverDrainsImmediately(t *testing.T) {
	m := &ArtifactManager{}
	drained := make(chan struct{}, 1)
	m.kick = func() { drained <- struct{}{} }

	m.SetVirtualInputResolver(&stubVirtualResolver{inputPath: "http://relay/x"})
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("wiring the virtual resolver did not trigger a drain")
	}

	// Wiring nil (a server without virtual support) must not drain.
	select {
	case <-drained:
	default:
	}
	m.SetVirtualInputResolver(nil)
	select {
	case <-drained:
		t.Fatal("clearing the virtual resolver triggered a drain")
	case <-time.After(50 * time.Millisecond):
	}
}

// TestVirtualResolverPendingRecoveryIsBounded is the failure policy for a
// resolver that never becomes usable: the pending outcome runs under the same
// bounded attempt budget as any failed encode, so the job goes terminal and its
// download fails with an actionable message instead of sitting in "preparing"
// forever or churning through lease recovery.
func TestVirtualResolverPendingRecoveryIsBounded(t *testing.T) {
	f := seedManagedFixture(t)
	ctx := context.Background()
	var present *string
	if err := f.pool.QueryRow(ctx, `SELECT to_regclass('public.download_artifacts')::text`).Scan(&present); err != nil || present == nil {
		t.Skip("download_artifacts migration has not been applied")
	}
	arepo := NewArtifactRepository(f.pool)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(ctx, `DELETE FROM download_artifacts WHERE media_file_id = $1`, f.fileID)
	})

	artifact := newArtifact(t, f.fileID, fmt.Sprintf("hash-pending-%d", time.Now().UnixNano()))
	if _, _, err := arepo.EnsureQueued(ctx, artifact); err != nil {
		t.Fatalf("ensure artifact: %v", err)
	}
	now := time.Now()
	downloadID := fmt.Sprintf("dl-pending-%d", now.UnixNano())
	if err := f.repo.Create(ctx, &Download{
		ID: downloadID, UserID: f.userID, ProfileID: f.profileA, DeviceID: f.deviceA,
		MediaFileID: f.fileID, ContentID: f.contentID, Kind: KindQueued,
		Status: StatusPreparing, Format: FormatTranscode, ArtifactID: artifact.ID,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create preparing download: %v", err)
	}

	mgr := &ArtifactManager{repo: arepo, downloads: f.repo, owner: "worker-pending"}
	terminal := false
	for attempt := 0; attempt <= artifactMaxAttempts && !terminal; attempt++ {
		claimed, err := arepo.ClaimNext(ctx, mgr.owner, time.Minute)
		if errors.Is(err, ErrNoArtifactJob) {
			break
		}
		if err != nil {
			t.Fatalf("claim pending artifact: %v", err)
		}
		mgr.failJob(ctx, claimed, virtualResolverPendingMessage)
		row, err := arepo.GetByID(ctx, artifact.ID)
		if err != nil {
			t.Fatalf("read pending artifact: %v", err)
		}
		terminal = row.Status == ArtifactFailed
		if !terminal {
			// Clear the retry gate so the next attempt runs now, as a drained
			// retry would.
			if _, err := f.pool.Exec(ctx, `UPDATE download_artifacts SET next_retry_at = now() - interval '1 second' WHERE id = $1`, artifact.ID); err != nil {
				t.Fatalf("clear retry gate: %v", err)
			}
		}
	}
	if !terminal {
		t.Fatal("a resolver that never becomes usable left the job non-terminal after max attempts")
	}
	got, err := f.repo.GetByID(ctx, downloadID)
	if err != nil {
		t.Fatalf("read failed download: %v", err)
	}
	if got.Status != StatusFailed || got.ErrorMessage != virtualResolverPendingMessage {
		t.Fatalf("download = (%s, %q), want failed with the actionable pending message", got.Status, got.ErrorMessage)
	}
	if virtualResolverPendingMessage == "" {
		t.Fatal("pending message must be actionable, not empty")
	}
}
