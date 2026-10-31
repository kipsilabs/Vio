package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
)

func failedRow(id int) *models.MediaFile {
	stamp := time.Now().Add(-time.Hour)
	return &models.MediaFile{ID: id, FailedAt: &stamp}
}

func TestVersionCheckFailedMarksDurableVerdicts(t *testing.T) {
	if !versionCheckFailed(versionCheckOutcome{fileID: 7, file: failedRow(7)}, false) {
		t.Fatal("durable failed_at was not reported")
	}
	if !versionCheckFailed(versionCheckOutcome{fileID: 8, file: &models.MediaFile{ID: 8}}, true) {
		t.Fatal("freshly stamped row was not reported")
	}
}

func TestVersionCheckFailedLeavesAmbiguousRowsAlone(t *testing.T) {
	if versionCheckFailed(versionCheckOutcome{fileID: 9, file: &models.MediaFile{ID: 9}}, false) {
		t.Fatal("row without a verdict was marked failed")
	}
	if versionCheckFailed(versionCheckOutcome{fileID: 9}, false) {
		t.Fatal("missing row was marked failed")
	}
}

// versionCheckRowStub serves one media file row and re-reads the same state
// after a health write, so a test can model a write that returns nil without
// landing (a delivery-grace skip or a fenced-out rotation).
type versionCheckRowStub struct {
	row *models.MediaFile
}

func (s versionCheckRowStub) GetByID(context.Context, int) (*models.MediaFile, error) {
	clone := *s.row
	return &clone, nil
}

// versionCheckAccessStub grants access when err is nil and denies it otherwise,
// mirroring the item authorization the handler applies per file.
type versionCheckAccessStub struct {
	err error
}

func (s versionCheckAccessStub) EnsureAccessible(context.Context, string, catalog.AccessFilter) error {
	return s.err
}

func versionCheckDeadPinResolver() VirtualMediaDetailedResolver {
	return VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
		return ResolvedVirtualMedia{}, errors.New("virtual stream provider returned no matching candidate")
	})
}

func postVersionCheck(t *testing.T, h *CatalogResourceHandler, fileIDs ...int) (*httptest.ResponseRecorder, versionCheckResponse, string) {
	t.Helper()
	body, err := json.Marshal(map[string][]int{"file_ids": fileIDs})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/catalog/versions/check", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.HandleCheckVersions(rec, r)
	var resp versionCheckResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return rec, resp, rec.Body.String()
}

// TestCatalogVersionsCheckFencedStampDoesNotReportFailure covers a dead-pin
// verdict whose fenced stamp returns nil without landing: the repository no-ops
// on a delivery-grace skip and on a row that rotated while resolution was in
// flight. A write that did not land must not be reported as failed, or the
// client hides a release the server never marked.
func TestCatalogVersionsCheckFencedStampDoesNotReportFailure(t *testing.T) {
	file := &models.MediaFile{
		ID:                         7,
		ContentID:                  "movie:fenced-stamp",
		FilePath:                   "virtual://movie/fenced-stamp?result=dead",
		ProviderVideoHash:          "hash-fenced-stamp",
		VirtualOwnerInstallationID: 7,
	}
	h := &CatalogResourceHandler{
		FileResolver:    versionCheckRowStub{row: file},
		VirtualResolver: versionCheckDeadPinResolver(),
		// A fenced-out write and a delivery-grace skip both return nil while
		// leaving the row unstamped.
		MarkVirtualFailed: func(context.Context, int, string, *time.Time) error { return nil },
		ItemAccess:        versionCheckAccessStub{},
	}

	rec, resp, rawBody := postVersionCheck(t, h, file.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if len(resp.Results) != 1 || resp.Results[0].FileID != file.ID || resp.Results[0].Available {
		t.Fatalf("unexpected results: %#v", resp.Results)
	}
	if resp.Results[0].Failed {
		t.Fatal("a stamp that did not land was reported as a durable failure")
	}
	if strings.Contains(rawBody, `"failed"`) {
		t.Fatalf("unconfirmed stamp leaked a failed field: %s", rawBody)
	}
}

// TestCatalogVersionsCheckDeniedRowHidesFailedVerdict proves an unauthorized
// row's durable verdict never reaches the response: a restricted profile must
// not learn a hidden version's failed_at (or that it exists) by probing it. The
// denied answer must match an unknown ID's shape and must not resolve or stamp.
func TestCatalogVersionsCheckDeniedRowHidesFailedVerdict(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	file := &models.MediaFile{
		ID:                         11,
		ContentID:                  "movie:denied-verdict",
		FilePath:                   "virtual://movie/denied-verdict?result=dead",
		ProviderVideoHash:          "hash-denied-verdict",
		VirtualOwnerInstallationID: 7,
		FailedAt:                   &past,
	}
	resolveCalls, markCalls, clearCalls := 0, 0, 0
	h := &CatalogResourceHandler{
		FileResolver: versionCheckRowStub{row: file},
		VirtualResolver: VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
			resolveCalls++
			return ResolvedVirtualMedia{}, errors.New("virtual stream provider returned no matching candidate")
		}),
		MarkVirtualFailed: func(context.Context, int, string, *time.Time) error {
			markCalls++
			return nil
		},
		ClearVirtualFailed: func(context.Context, int, string, *time.Time) error {
			clearCalls++
			return nil
		},
		ItemAccess: versionCheckAccessStub{err: errors.New("access denied")},
	}

	rec, resp, rawBody := postVersionCheck(t, h, file.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if len(resp.Results) != 1 {
		t.Fatalf("results = %#v, want 1", resp.Results)
	}
	want := versionCheckResult{FileID: file.ID}
	if resp.Results[0] != want {
		t.Fatalf("denied failed row leaked row state: %#v, want %#v", resp.Results[0], want)
	}
	if strings.Contains(rawBody, `"failed"`) {
		t.Fatalf("denied row leaked a failed field: %s", rawBody)
	}
	if resolveCalls != 0 || markCalls != 0 || clearCalls != 0 {
		t.Fatalf("denied row reached provider/stamp callbacks: resolve=%d mark=%d clear=%d", resolveCalls, markCalls, clearCalls)
	}
}
