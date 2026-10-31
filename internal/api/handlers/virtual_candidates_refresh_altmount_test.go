package handlers

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

func reconcileTestRow(id int, path, release string, failedAt ...*time.Time) *models.MediaFile {
	var fa *time.Time
	if len(failedAt) > 0 {
		fa = failedAt[0]
	}
	return &models.MediaFile{
		ID:                  id,
		ContentID:           "episode-test",
		FilePath:            path,
		Container:           "virtual",
		ProviderReleaseName: release,
		FailedAt:            fa,
	}
}

// AltMount explicitly branding a release failed is stamped so version
// pickers stop offering it. Everything else is left alone.
func TestReconcileAltMountVerdictsStampsFailedReleases(t *testing.T) {
	var stamped []int
	executor := &VirtualCandidatesRefreshExecutor{
		ReleaseFailed: func(releaseName string) (bool, bool) {
			return strings.EqualFold(releaseName, "bad release"), true
		},
		MarkFailed: func(_ context.Context, fileID int, _ string, _ *time.Time) error {
			stamped = append(stamped, fileID)
			return nil
		},
	}
	sources := []*models.MediaFile{
		reconcileTestRow(1, "virtual://x?result=a", "Bad Release"),
		reconcileTestRow(2, "virtual://x?result=b", "Good Release"),
	}
	if n := executor.reconcileAltMountVerdicts(context.Background(), "episode-test", "", sources); n != 1 {
		t.Fatalf("stamped = %d, want 1", n)
	}
	if len(stamped) != 1 || stamped[0] != 1 {
		t.Fatalf("stamped rows = %v, want [1]", stamped)
	}
}

// Unknown, downloading, already-stamped, and nameless rows are never
// indicted: only an explicit failed verdict from AltMount stamps.
func TestReconcileAltMountVerdictsLeavesOthersAlone(t *testing.T) {
	var stamped []int
	past := time.Now().Add(-time.Hour)
	executor := &VirtualCandidatesRefreshExecutor{
		ReleaseFailed: func(releaseName string) (bool, bool) {
			return false, false
		},
		ReleaseDownloading: func(releaseName string) (bool, bool) {
			return releaseName == "fetching release", true
		},
		MarkFailed: func(_ context.Context, fileID int, _ string, _ *time.Time) error {
			stamped = append(stamped, fileID)
			return nil
		},
	}
	sources := []*models.MediaFile{
		reconcileTestRow(1, "virtual://x?result=a", "Unknown Release"),
		reconcileTestRow(2, "virtual://x?result=b", "Fetching Release"),
		reconcileTestRow(3, "virtual://x?result=c", "Already Stamped", &past),
		// No release name: nothing to check against AltMount.
		{ID: 4, ContentID: "episode-test", FilePath: "virtual://x?result=d", Container: "virtual"},
		nil,
	}
	if n := executor.reconcileAltMountVerdicts(context.Background(), "episode-test", "", sources); n != 0 {
		t.Fatalf("stamped = %d, want 0", n)
	}
	if len(stamped) != 0 {
		t.Fatalf("stamped rows = %v, want none", stamped)
	}
}

// Nil funcs and nil receiver are no-ops, so unconfigured deployments keep
// the old behavior exactly.
func TestReconcileAltMountVerdictsNilSafe(t *testing.T) {
	var executor *VirtualCandidatesRefreshExecutor
	if n := executor.reconcileAltMountVerdicts(context.Background(), "e", "", nil); n != 0 {
		t.Fatalf("nil executor stamped %d", n)
	}
	bare := &VirtualCandidatesRefreshExecutor{}
	sources := []*models.MediaFile{reconcileTestRow(1, "virtual://x?result=a", "Any Release")}
	if n := bare.reconcileAltMountVerdicts(context.Background(), "episode-test", "", sources); n != 0 {
		t.Fatalf("unconfigured executor stamped %d", n)
	}
}
