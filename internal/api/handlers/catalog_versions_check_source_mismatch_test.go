package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Silo-Server/silo-server/internal/models"
)

// sourceMismatchProbedRow is the warm, virtual row the liveness check answers
// from its durable state without resolving the provider again. That is the row
// whose recent playback probe can have left a wrong-source verdict behind.
func sourceMismatchProbedRow(filePath string) *models.MediaFile {
	probedAt := time.Now()
	return &models.MediaFile{
		ID:                         7,
		ContentID:                  "movie-source-mismatch",
		MediaFolderID:              1,
		FilePath:                   filePath,
		Container:                  "virtual",
		FileSize:                   6_330_000_000,
		VirtualOwnerInstallationID: 5,
		ProbeUpdatedAt:             &probedAt,
	}
}

func postVersionCheck(t *testing.T, h *CatalogResourceHandler, ids ...int) versionCheckResponse {
	t.Helper()
	router := chi.NewRouter()
	router.Post("/catalog/versions/check", h.HandleCheckVersions)
	body, err := json.Marshal(map[string][]int{"file_ids": ids})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/catalog/versions/check", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp versionCheckResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

// A probe that read a different release's bytes leaves a short wrong-source
// verdict. The liveness read must surface it as its own additive signal while
// still reporting the release as available and not failed: the release is not
// dead, only the bytes the provider served were wrong.
func TestVersionCheckReportsSourceMismatchWithoutFailed(t *testing.T) {
	const (
		uri           = "virtual://movie/tt-source-mismatch?result=cand-1"
		servedBytes   = int64(7_427_636_945)
		declaredBytes = int64(8_449_999_999)
	)
	file := sourceMismatchProbedRow(uri)
	key := virtualProbeFailureKey(uri, file.VirtualOwnerInstallationID)
	virtualProbeFailures.clearSizeMismatch(key)
	t.Cleanup(func() { virtualProbeFailures.clearSizeMismatch(key) })
	virtualProbeFailures.markSizeMismatch(key, servedBytes, declaredBytes)

	marks := 0
	h := &CatalogResourceHandler{
		FileResolver: versionsCheckTestResolver{file: file},
		ItemAccess:   permissiveItemAccess{},
		MarkVirtualFailed: func(context.Context, int, string, *time.Time) error {
			marks++
			return nil
		},
	}

	resp := postVersionCheck(t, h, file.ID)
	if len(resp.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(resp.Results))
	}
	result := resp.Results[0]
	if result.FileID != file.ID {
		t.Fatalf("file_id = %d, want %d", result.FileID, file.ID)
	}
	if !result.Available {
		t.Fatal("a source mismatch must not make the release unavailable")
	}
	if result.Failed {
		t.Fatal("a source mismatch must never report failed")
	}
	if marks != 0 {
		t.Fatalf("a source mismatch performed %d failed stamps, want 0", marks)
	}
	if result.SourceMismatch == nil {
		t.Fatal("missing source_mismatch verdict")
	}
	if result.SourceMismatch.ServedBytes != servedBytes || result.SourceMismatch.DeclaredBytes != declaredBytes {
		t.Fatalf("source_mismatch = %+v, want served %d / declared %d",
			*result.SourceMismatch, servedBytes, declaredBytes)
	}
	if virtualProbeFailures.recent(key) {
		t.Fatal("the wrong-source verdict fed the dead-candidate failure damper")
	}
}

// The catalog detail projection reports the same verdict the check endpoint
// does. Both read the one playback-probe damper, so the detail page cannot
// drift into a second source-mismatch implementation.
func TestVersionSourceMismatchProjectsTheCheckVerdict(t *testing.T) {
	const (
		uri           = "virtual://movie/tt-detail-mismatch?result=cand-1"
		servedBytes   = int64(7_427_636_945)
		declaredBytes = int64(8_449_999_999)
	)
	file := sourceMismatchProbedRow(uri)
	key := virtualProbeFailureKey(uri, file.VirtualOwnerInstallationID)
	virtualProbeFailures.clearSizeMismatch(key)
	t.Cleanup(func() { virtualProbeFailures.clearSizeMismatch(key) })
	virtualProbeFailures.markSizeMismatch(key, servedBytes, declaredBytes)

	verdict := VersionSourceMismatch(file)
	if verdict == nil {
		t.Fatal("projected verdict is nil for a row with a recent mismatch")
	}
	if verdict.ServedBytes != servedBytes || verdict.DeclaredBytes != declaredBytes {
		t.Fatalf("verdict = %+v, want served %d / declared %d", *verdict, servedBytes, declaredBytes)
	}
	if check := versionCheckSourceMismatchFor(file); check == nil || check.ServedBytes != verdict.ServedBytes || check.DeclaredBytes != verdict.DeclaredBytes {
		t.Fatalf("detail verdict %+v and check verdict %+v disagree", *verdict, check)
	}
	if got := VersionSourceMismatch(&models.MediaFile{ID: 8, FilePath: "/media/local.mkv"}); got != nil {
		t.Fatalf("local row projected source_mismatch %+v", *got)
	}
}

// A row with no probe verdict keeps the generic unprobed presentation: the
// liveness answer carries no source_mismatch and no invented sizes.
func TestVersionCheckLeavesUnprobedRowsGeneric(t *testing.T) {
	uri := "virtual://movie/tt-unprobed?result=cand-2"
	file := sourceMismatchProbedRow(uri)
	key := virtualProbeFailureKey(uri, file.VirtualOwnerInstallationID)
	virtualProbeFailures.clearSizeMismatch(key)
	t.Cleanup(func() { virtualProbeFailures.clearSizeMismatch(key) })

	h := &CatalogResourceHandler{
		FileResolver: versionsCheckTestResolver{file: file},
		ItemAccess:   permissiveItemAccess{},
	}

	resp := postVersionCheck(t, h, file.ID)
	if len(resp.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(resp.Results))
	}
	result := resp.Results[0]
	if result.SourceMismatch != nil {
		t.Fatalf("unprobed row reported source_mismatch %+v", *result.SourceMismatch)
	}
	if !result.Available || result.Failed {
		t.Fatalf("unprobed row = %+v, want available and not failed", result)
	}
}

// The verdict lives only as long as the wrong-source damper: accepting the
// bytes (or letting the short window lapse) returns the row to the generic
// presentation instead of leaving a stale mismatch badge behind.
func TestSizeMismatchVerdictFollowsTheShortDamper(t *testing.T) {
	now := time.Now()
	cache := &virtualProbeFailureCache{
		marks: make(map[string]virtualProbeFailureMark),
		now:   func() time.Time { return now },
	}
	key := "neutral\x005\x00cand"
	cache.markSizeMismatch(key, 7_000, 8_000)

	served, declared, ok := cache.sizeMismatchVerdict(key)
	if !ok || served != 7_000 || declared != 8_000 {
		t.Fatalf("verdict = (%d, %d, %v), want (7000, 8000, true)", served, declared, ok)
	}
	if cache.recent(key) {
		t.Fatal("the wrong-source verdict fed the dead-candidate failure damper")
	}

	cache.clearSizeMismatch(key)
	if _, _, ok := cache.sizeMismatchVerdict(key); ok {
		t.Fatal("an accepted probe did not clear the mismatch verdict")
	}

	cache.markSizeMismatch(key, 7_000, 8_000)
	now = now.Add(virtualProbeSizeMismatchTTL + time.Second)
	if _, _, ok := cache.sizeMismatchVerdict(key); ok {
		t.Fatal("an expired wrong-source damper still reported a mismatch")
	}
}

// Only the wrong-source marker carries sizes. A dead-candidate mark must not
// let the version list read a mismatch out of an unrelated failure.
func TestOnlyWrongSourceMarkersCarryByteVerdicts(t *testing.T) {
	now := time.Now()
	cache := &virtualProbeFailureCache{
		marks: make(map[string]virtualProbeFailureMark),
		now:   func() time.Time { return now },
	}
	key := "neutral\x005\x00cand"
	cache.mark(key)
	if _, _, ok := cache.sizeMismatchVerdict(key); ok {
		t.Fatal("a dead-candidate mark reported a source mismatch")
	}
}
