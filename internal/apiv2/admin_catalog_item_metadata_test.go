package apiv2

import (
	"context"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/access"
	"github.com/Silo-Server/silo-server/internal/adminjob"
	"github.com/Silo-Server/silo-server/internal/api/handlers"
	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	catalogsvc "github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/policy"
)

type fakeAdminItemMetadata struct {
	calls, userID int
	id            string
	mode          adminjob.ItemRefreshMode
	update        handlers.UpdateItemMetadataRequest
}

func (f *fakeAdminItemMetadata) CreateItemMetadataRefresh(_ context.Context, id string, mode adminjob.ItemRefreshMode, userID int) (*models.AdminJob, error) {
	f.calls++
	f.id, f.mode, f.userID = id, mode, userID
	return &models.AdminJob{ID: "item-refresh-1", JobType: adminjob.JobTypeItemRefresh, Status: adminjob.StatusQueued, CreatedByUserID: userID, RequestedAt: fixedTime()}, nil
}
func (f *fakeAdminItemMetadata) UpdateCatalogItemMetadata(_ context.Context, id string, req handlers.UpdateItemMetadataRequest) (*catalogsvc.ItemDetail, error) {
	f.calls++
	f.id, f.update = id, req
	return &catalogsvc.ItemDetail{ContentID: id, Title: "Updated", Type: "movie"}, nil
}
func TestAdminItemMetadataTransport(t *testing.T) {
	deps := pilotDeps(nil, nil)
	f := &fakeAdminItemMetadata{}
	deps.AdminItemMetadata = f
	deps.PermissionGates[policy.PermissionMetadataCuration] = adminTranslationGate
	h := newTestHandler(t, deps)
	path := Prefix + "/admin/items/item-1"
	rec := do(t, h, "POST", path+"/refresh-metadata", `{}`, bearer(memberToken))
	if rec.Code != 202 || f.calls != 1 || f.mode != adminjob.ItemRefreshModeQuick || f.userID != 1 || rec.Header().Get("Location") != Prefix+"/admin/jobs/item-refresh-1" {
		t.Fatalf("refresh: %d %s %#v", rec.Code, rec.Body, f)
	}
	rec = do(t, h, "PATCH", path+"/metadata", `{"overview":"","genres":[],"year":0,"title":null}`, bearer(memberToken))
	if rec.Code != 200 || f.update.Overview == nil || *f.update.Overview != "" || f.update.Genres == nil || len(*f.update.Genres) != 0 || f.update.Year == nil || *f.update.Year != 0 || f.update.Title != nil || !strings.Contains(rec.Body.String(), `"title":"Updated"`) {
		t.Fatalf("update: %d %s %#v", rec.Code, rec.Body, f.update)
	}
	before := f.calls
	rec = do(t, h, "POST", path+"/refresh-metadata", `{"mode":"wrong"}`, bearer(memberToken))
	if rec.Code != 422 || f.calls != before {
		t.Fatalf("invalid mode: %d", rec.Code)
	}
	for _, tc := range []struct{ method, path string }{{"POST", "/refresh-metadata"}, {"PATCH", "/metadata"}} {
		rec = do(t, h, tc.method, Prefix+"/admin/items/forbidden"+tc.path, `{}`, bearer(adminToken))
		if rec.Code != 403 || f.calls != before {
			t.Fatalf("permission: %d calls=%d", rec.Code, f.calls)
		}
	}
}

// TestUpdateAdminItemMetadataCarriesCollections pins that a successful metadata
// save answers the same collections row the read detail does, using the
// viewer's resolved access, so an item in a collection does not appear to lose
// its memberships until a refetch.
func TestUpdateAdminItemMetadataCarriesCollections(t *testing.T) {
	deps, _ := catalogDeps(t)
	deps.AdminItemMetadata = &fakeAdminItemMetadata{}
	deps.PermissionGates[policy.PermissionMetadataCuration] = adminTranslationGate
	// The metadata route is not profile-scoped, so the lookup reads the scope
	// the permission gate resolved. Restrict it so the test proves that scope,
	// not the catalog seam's default, reaches the reverse lookup.
	deps.ViewerAccess = apimw.NewViewerAccessMiddleware(policyResolver{scope: &access.Scope{AllowedLibraryIDs: []int{1, 2}}})
	fake := deps.LibraryCollections.(*fakeLibraryViews)
	fake.collections = []handlers.ItemCollectionView{{ID: "alpha", Title: "Dune Saga", ItemCount: 3}}
	h := newTestHandler(t, deps)

	rec := do(t, h, "PATCH", Prefix+"/admin/items/item-1/metadata", `{"title":"Updated"}`, bearer(memberToken))
	if rec.Code != 200 {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}
	var detail CatalogItemDetail
	decodeJSON(t, rec.Body, &detail)
	if fake.lastCollections != "item-1" {
		t.Fatalf("lookup = %q, want the edited item id", fake.lastCollections)
	}
	if got := fake.lastCollectionsAccess.AllowedLibraryIDs; len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("lookup access = %+v, want the gate-resolved allowed libraries [1 2]", fake.lastCollectionsAccess)
	}
	if len(detail.Collections) != 1 || detail.Collections[0].ID != "alpha" || detail.Collections[0].Title != "Dune Saga" || detail.Collections[0].ItemCount != 3 {
		t.Fatalf("collections = %+v, want the membership row on the save response", detail.Collections)
	}
}

func adminCatalogItemMetadataFixtureCases() []fixtureCase {
	return []fixtureCase{
		{name: "admin_item_refresh_queued", operationID: "refreshAdminItemMetadata", method: "POST", path: Prefix + "/admin/items/item-1/refresh-metadata", body: `{"mode":"complete"}`, headers: bearer(memberToken), status: 202, schema: "#/components/schemas/AdminTaskJob", assertHeaders: []string{"Content-Type", "Location", "Retry-After"}, scenario: "A delegated curator receives a persisted refresh job without administrator-only payloads."},
		{name: "admin_item_metadata_updated", operationID: "updateAdminItemMetadata", method: "PATCH", path: Prefix + "/admin/items/item-1/metadata", body: `{"title":"Updated"}`, headers: bearer(memberToken), status: 200, schema: "#/components/schemas/CatalogItemDetail", assertHeaders: []string{"Content-Type"}, scenario: "Successful curation returns canonical catalog detail after persistence."},
	}
}
