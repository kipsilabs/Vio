package apiv2

import (
	"context"
	"net/http"
	"testing"

	"github.com/Silo-Server/silo-server/internal/api/handlers"
	"github.com/Silo-Server/silo-server/internal/usercollections"
)

// TestGetItemCollectionsCapabilityState: the capability reports available when
// the collection index is wired behind item detail, and unsupported when it is
// not. Route presence never changes with configuration.
func TestGetItemCollectionsCapabilityState(t *testing.T) {
	deps, _ := catalogDeps(t)
	rec := do(t, newTestHandler(t, deps), http.MethodGet, "/api/v2/capabilities/item-collections", "", viewerHeaders())
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Code, rec.Body.String())
	}
	var body ItemCollectionsCapability
	decodeJSON(t, rec.Body, &body)
	if body.State != StateAvailable || body.Allowed == nil || !*body.Allowed || body.Revision == "" {
		t.Fatalf("capability = %+v", body)
	}

	// A service without the collection index answers unsupported rather than
	// dropping the route.
	deps.LibraryCollections = collectionServiceWithoutIndex{}
	rec = do(t, newTestHandler(t, deps), http.MethodGet, "/api/v2/capabilities/item-collections", "", viewerHeaders())
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Code, rec.Body.String())
	}
	body = ItemCollectionsCapability{}
	decodeJSON(t, rec.Body, &body)
	if body.State != StateUnsupported {
		t.Fatalf("capability = %+v", body)
	}
}

// collectionServiceWithoutIndex satisfies LibraryCollectionService but not
// ItemCollectionIndex, standing in for a server whose item detail carries no
// collections row.
type collectionServiceWithoutIndex struct{}

func (collectionServiceWithoutIndex) LibraryCollectionsTab(context.Context, int, int, string) (handlers.LibraryCollectionTabView, error) {
	return handlers.LibraryCollectionTabView{}, nil
}

func (collectionServiceWithoutIndex) LibraryUserCollections(context.Context, int, int, string) ([]usercollections.ServerVisibleCollection, error) {
	return nil, nil
}
