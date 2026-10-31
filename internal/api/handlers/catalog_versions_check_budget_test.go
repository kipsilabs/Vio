package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

// Post-verdict writes run after the request's own deadline and deliberately
// drop its cancellation, because the row is already committed when the client
// gives up. They must still be bounded: a blocked repository call has to fail
// within versionCheckWriteBudget instead of keeping the handler and its worker
// alive, and an unconfirmed write must never be reported as a durable verdict.
func TestStampVirtualCandidateFailedIsBounded(t *testing.T) {
	var calls int
	h := &CatalogResourceHandler{
		MarkVirtualFailed: func(ctx context.Context, _ int, _ string, _ *time.Time) error {
			calls++
			<-ctx.Done()
			return ctx.Err()
		},
	}
	file := &models.MediaFile{ID: 1, FilePath: "virtual://movie/tt1?result=dead"}

	start := time.Now()
	stamped := h.stampVirtualCandidateFailed(context.Background(), file.ID, file)
	elapsed := time.Since(start)

	if stamped {
		t.Fatal("an unconfirmed write must not report a durable stamp")
	}
	if calls != 1 {
		t.Fatalf("stamp attempts = %d, want 1", calls)
	}
	if limit := versionCheckWriteBudget + 500*time.Millisecond; elapsed > limit {
		t.Fatalf("stamp waited %s, limit %s: the detached write is not bounded", elapsed, limit)
	}
}

func TestClearVirtualCandidateIfFailedIsBounded(t *testing.T) {
	observed := time.Now().Add(-time.Minute)
	var calls int
	h := &CatalogResourceHandler{
		ClearVirtualFailed: func(ctx context.Context, _ int, _ string, _ *time.Time) error {
			calls++
			<-ctx.Done()
			return ctx.Err()
		},
	}
	file := &models.MediaFile{ID: 2, FilePath: "virtual://movie/tt2?result=live", FailedAt: &observed}

	start := time.Now()
	cleared := h.clearVirtualCandidateIfFailed(context.Background(), file.ID, file)
	elapsed := time.Since(start)

	if cleared != file {
		t.Fatal("an unconfirmed clear must keep the caller's failed snapshot")
	}
	if calls != 1 {
		t.Fatalf("clear attempts = %d, want 1", calls)
	}
	if limit := versionCheckWriteBudget + 500*time.Millisecond; elapsed > limit {
		t.Fatalf("clear waited %s, limit %s: the detached write is not bounded", elapsed, limit)
	}
}
