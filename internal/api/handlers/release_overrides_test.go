package handlers

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/auth"
	"github.com/Silo-Server/silo-server/internal/catalog"
)

type overrideTestStore struct {
	actor int
	clear bool
}

func (s *overrideTestStore) Read(_ context.Context, actor int, id catalog.ReleaseIdentity, _ int64, _ int) ([]catalog.ReleaseOverride, error) {
	s.actor = actor
	return []catalog.ReleaseOverride{}, nil
}
func (s *overrideTestStore) Mutate(_ context.Context, actor int, m catalog.ReleaseOverrideMutation, clear bool) (catalog.ReleaseOverride, error) {
	s.actor = actor
	s.clear = clear
	return catalog.ReleaseOverride{ReleaseIdentity: m.ReleaseIdentity, ActorAccountID: actor}, nil
}
func (s *overrideTestStore) ListNeedsMetadata(context.Context, int, int, int) ([]catalog.ReleaseMetadataEntry, error) {
	return nil, nil
}
func (s *overrideTestStore) RetryNeedsMetadata(context.Context, int, catalog.ReleaseIdentity) (int64, error) {
	return 1, nil
}

func TestReleaseOverrideAuthorizationAndActor(t *testing.T) {
	for _, role := range []string{"", "user", "admin"} {
		for _, method := range []string{"GET", "PUT", "DELETE"} {
			t.Run(role+method, func(t *testing.T) {
				store := &overrideTestStore{}
				h := &RequestsHandler{ReleaseOverrides: store}
				req := httptest.NewRequest(method, "/api/v1/admin/release-overrides?media_type=movie&provider=tmdb&provider_id=42", strings.NewReader(`{"media_type":"movie","provider":"tmdb","provider_id":"42","evidence_note":"verified"}`))
				if role != "" {
					ctx := apimw.SetClaims(req.Context(), &auth.Claims{UserID: 27, Role: role, TokenType: auth.TokenTypeAccess})
					// Admin reach follows the acting-admin decision, as it does
					// on the router; only the admin role acts as an admin here.
					ctx = apimw.SetActingAdminDecider(ctx, func(context.Context, apimw.ActingAdminRequest) (bool, error) {
						return role == "admin", nil
					})
					req = req.WithContext(ctx)
				}
				rec := httptest.NewRecorder()
				h.HandleReleaseOverrides(rec, req)
				want := 200
				if role == "" {
					want = 401
				} else if role != "admin" {
					want = 403
				}
				if rec.Code != want {
					t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
				}
				if role == "admin" && (store.actor != 27 || store.clear != (method == "DELETE")) {
					t.Fatalf("actor/clear: %+v", store)
				}
				if role != "admin" && store.actor != 0 {
					t.Fatal("unauthorized storage access")
				}
			})
		}
	}
}

func TestReleaseOverrideRejectsForgedActor(t *testing.T) {
	h := &RequestsHandler{ReleaseOverrides: &overrideTestStore{}}
	req := httptest.NewRequest("PUT", "/", strings.NewReader(`{"actor_account_id":1}`))
	ctx := apimw.SetClaims(req.Context(), &auth.Claims{UserID: 27, Role: "admin", TokenType: auth.TokenTypeAccess})
	ctx = apimw.SetActingAdminDecider(ctx, func(context.Context, apimw.ActingAdminRequest) (bool, error) { return true, nil })
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.HandleReleaseOverrides(rec, req)
	if rec.Code != 400 {
		t.Fatalf("%d", rec.Code)
	}
}
