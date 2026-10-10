package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/auth"
	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
)

func materializeTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	var databaseName string
	if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&databaseName); err != nil {
		t.Fatalf("identify test database: %v", err)
	}
	if !strings.Contains(strings.ToLower(databaseName), "test") && !strings.Contains(strings.ToLower(databaseName), "purge") {
		t.Fatalf("refusing destructive handler fixture database %q", databaseName)
	}
	lockConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire test database lock: %v", err)
	}
	if _, err := lockConn.Exec(ctx, "SELECT pg_advisory_lock($1)", int64(0x53494c4f5f544553)); err != nil {
		lockConn.Release()
		t.Fatalf("lock test database fixtures: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	t.Cleanup(func() {
		_, _ = lockConn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", int64(0x53494c4f5f544553))
		lockConn.Release()
	})

	for _, statement := range []string{
		"DELETE FROM media_files",
		"TRUNCATE public.episodes CASCADE",
		"DELETE FROM seasons",
		"DELETE FROM media_item_libraries",
		"DELETE FROM library_collection_items",
		"DELETE FROM library_collection_libraries",
		"DELETE FROM library_collections",
		"DELETE FROM media_items",
		"DELETE FROM media_folders",
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("reset materialize test database: %v", err)
		}
	}

	return pool
}

func TestHandleMaterializeAdminCollectionItem_ValidationAndErrors(t *testing.T) {
	pool := materializeTestPool(t)
	ctx := context.Background()

	itemRepo := catalog.NewItemRepository(pool)
	collRepo := catalog.NewLibraryCollectionRepository(pool)
	service := catalog.NewLibraryCollectionService(collRepo, itemRepo, nil, nil)
	handler := NewLibraryCollectionHandler(collRepo, service, itemRepo, nil)

	// Seed test folder
	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(9901,'Mat Folder','movies',true) ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}

	const colID = "col-mat-val-1"
	const itemID = "movie-mat-val-1"
	const nonMemberID = "movie-mat-val-nonmember"
	const bookID = "book-mat-val-1"

	enabledCfg, _ := json.Marshal(map[string]any{"virtual_playback": true})
	disabledCfg, _ := json.Marshal(map[string]any{"virtual_playback": false})

	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
		VALUES($1,$1,$1,'tmdb',9901,$2) ON CONFLICT (id) DO UPDATE SET source_config=$2`, colID, enabledCfg); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_libraries(collection_id,library_id)
		VALUES($1,9901) ON CONFLICT DO NOTHING`, colID); err != nil {
		t.Fatalf("seed collection library: %v", err)
	}

	// Seed valid movie item
	movieItem := &models.MediaItem{ContentID: itemID, Type: "movie", Title: "Valid Movie", TmdbID: "9901", Status: "matched"}
	if err := itemRepo.Upsert(ctx, movieItem); err != nil {
		t.Fatalf("seed movie item: %v", err)
	}
	// Seed non-member item
	nonMemberItem := &models.MediaItem{ContentID: nonMemberID, Type: "movie", Title: "Non Member", TmdbID: "9902", Status: "matched"}
	if err := itemRepo.Upsert(ctx, nonMemberItem); err != nil {
		t.Fatalf("seed non-member item: %v", err)
	}
	// Seed ebook item
	ebookItem := &models.MediaItem{ContentID: bookID, Type: "ebook", Title: "EBook Item", Status: "matched"}
	if err := itemRepo.Upsert(ctx, ebookItem); err != nil {
		t.Fatalf("seed ebook item: %v", err)
	}

	// Add only movieItem and ebookItem to collection membership
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_items(collection_id,media_item_id,position)
		VALUES ($1,$2,1), ($1,$3,2) ON CONFLICT DO NOTHING`, colID, itemID, bookID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	runReq := func(cID, iID string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/admin/collections/"+cID+"/materialize/"+iID, nil)
		rCtx := chi.NewRouteContext()
		if cID != "" {
			rCtx.URLParams.Add("id", cID)
		}
		if iID != "" {
			rCtx.URLParams.Add("item_id", iID)
		}
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rCtx))
		handler.HandleMaterializeAdminCollectionItem(rec, req)
		return rec
	}

	// 1. Missing URL params -> 400
	if rec := runReq("", itemID); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty collection id code = %d, want 400", rec.Code)
	}
	if rec := runReq(colID, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty item id code = %d, want 400", rec.Code)
	}

	// 2. Collection not found -> 404
	if rec := runReq("non-existent-collection", itemID); rec.Code != http.StatusNotFound {
		t.Fatalf("non-existent collection code = %d, want 404", rec.Code)
	}

	// 3. Item not in collection -> 404
	if rec := runReq(colID, nonMemberID); rec.Code != http.StatusNotFound {
		t.Fatalf("non-member item code = %d, want 404", rec.Code)
	}

	// 4. Item not in catalog -> 404
	if rec := runReq(colID, "non-existent-item"); rec.Code != http.StatusNotFound {
		t.Fatalf("non-existent item code = %d, want 404", rec.Code)
	}

	// 5. Ineligible media type (ebook) -> 400
	if rec := runReq(colID, bookID); rec.Code != http.StatusBadRequest {
		t.Fatalf("ebook item code = %d, want 400", rec.Code)
	}

	// 6. Disabled virtual playback on collection -> 400
	if _, err := pool.Exec(ctx, `UPDATE library_collections SET source_config=$2 WHERE id=$1`, colID, disabledCfg); err != nil {
		t.Fatalf("disable virtual playback: %v", err)
	}
	if rec := runReq(colID, itemID); rec.Code != http.StatusBadRequest {
		t.Fatalf("disabled virtual playback code = %d, want 400", rec.Code)
	}
}

// releasedDigitalReleaseChecker reports every movie as home-released so
// handler tests exercise routing and storage behavior past release gating.
type releasedDigitalReleaseChecker struct{}

func (releasedDigitalReleaseChecker) HasDigitalRelease(context.Context, int) (bool, error) {
	return true, nil
}

func TestHandleMaterializeAdminCollectionItem_SuccessAndIdempotency(t *testing.T) {
	pool := materializeTestPool(t)
	ctx := context.Background()

	itemRepo := catalog.NewItemRepository(pool)
	collRepo := catalog.NewLibraryCollectionRepository(pool)
	service := catalog.NewLibraryCollectionService(collRepo, itemRepo, nil, nil)
	service.TMDBDigitalReleases = releasedDigitalReleaseChecker{}
	service.VirtualVariants = func(_ context.Context, _, _ string) ([]catalog.VirtualPlaybackVariant, error) {
		return []catalog.VirtualPlaybackVariant{{OwnerInstallationID: 11}}, nil
	}
	handler := NewLibraryCollectionHandler(collRepo, service, itemRepo, nil)

	// Seed test folder
	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(9902,'Mat Folder 2','movies',true) ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}

	const colID = "col-mat-succ-1"
	const itemID = "movie-mat-succ-1"
	cfg, _ := json.Marshal(map[string]any{"virtual_playback": true})

	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
		VALUES($1,$1,$1,'tmdb',9902,$2) ON CONFLICT (id) DO UPDATE SET source_config=$2`, colID, cfg); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_libraries(collection_id,library_id)
		VALUES($1,9902) ON CONFLICT DO NOTHING`, colID); err != nil {
		t.Fatalf("seed collection library: %v", err)
	}

	movieItem := &models.MediaItem{ContentID: itemID, Type: "movie", Title: "Success Movie", TmdbID: "9902", Status: "matched"}
	if err := itemRepo.Upsert(ctx, movieItem); err != nil {
		t.Fatalf("seed movie item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_items(collection_id,media_item_id,position)
		VALUES ($1,$2,1) ON CONFLICT DO NOTHING`, colID, itemID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	runReq := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/admin/collections/"+colID+"/materialize/"+itemID, nil)
		rCtx := chi.NewRouteContext()
		rCtx.URLParams.Add("id", colID)
		rCtx.URLParams.Add("item_id", itemID)
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rCtx))
		handler.HandleMaterializeAdminCollectionItem(rec, req)
		return rec
	}

	// Call 1: First materialization
	rec1 := runReq()
	if rec1.Code != http.StatusOK {
		t.Fatalf("call 1 code = %d, body = %s", rec1.Code, rec1.Body.String())
	}
	var resp1 struct {
		Success      bool   `json:"success"`
		ContentID    string `json:"content_id"`
		FilesCreated int    `json:"files_created"`
	}
	if err := json.Unmarshal(rec1.Body.Bytes(), &resp1); err != nil {
		t.Fatalf("decode resp1: %v", err)
	}
	if !resp1.Success || resp1.ContentID != itemID || resp1.FilesCreated != 1 {
		t.Fatalf("resp1 = %+v, want success=true, content_id=%s, files_created=1", resp1, itemID)
	}

	// Call 2: Repeat materialization (idempotent)
	rec2 := runReq()
	if rec2.Code != http.StatusOK {
		t.Fatalf("call 2 code = %d, body = %s", rec2.Code, rec2.Body.String())
	}
	var resp2 struct {
		Success       bool `json:"success"`
		FilesCreated  int  `json:"files_created"`
		FilesExisting int  `json:"files_existing"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("decode resp2: %v", err)
	}
	if !resp2.Success || resp2.FilesCreated != 0 || resp2.FilesExisting != 1 {
		t.Fatalf("resp2 = %+v, want success=true, files_created=0, files_existing=1", resp2)
	}
}

func TestHandleMaterializeAdminCollectionItem_ProviderFailureAndIncompatibleLibrary(t *testing.T) {
	pool := materializeTestPool(t)
	ctx := context.Background()

	itemRepo := catalog.NewItemRepository(pool)
	collRepo := catalog.NewLibraryCollectionRepository(pool)
	service := catalog.NewLibraryCollectionService(collRepo, itemRepo, nil, nil)
	service.TMDBDigitalReleases = releasedDigitalReleaseChecker{}
	// Provider outage returns error
	service.VirtualVariants = func(_ context.Context, _, _ string) ([]catalog.VirtualPlaybackVariant, error) {
		return nil, errors.New("upstream provider connection timed out")
	}
	handler := NewLibraryCollectionHandler(collRepo, service, itemRepo, nil)

	// Seed test folder (series folder only!)
	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(9903,'Series Only Folder','series',true) ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}

	const colID = "col-mat-prov-1"
	const movieID = "movie-mat-prov-1"
	const seriesID = "series-mat-prov-1"
	cfg, _ := json.Marshal(map[string]any{"virtual_playback": true})

	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
		VALUES($1,$1,$1,'tmdb',9903,$2) ON CONFLICT (id) DO UPDATE SET source_config=$2`, colID, cfg); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_libraries(collection_id,library_id)
		VALUES($1,9903) ON CONFLICT DO NOTHING`, colID); err != nil {
		t.Fatalf("seed collection library: %v", err)
	}

	movieItem := &models.MediaItem{ContentID: movieID, Type: "movie", Title: "Movie Item", TmdbID: "9903", Status: "matched"}
	if err := itemRepo.Upsert(ctx, movieItem); err != nil {
		t.Fatalf("seed movie item: %v", err)
	}
	seriesItem := &models.MediaItem{ContentID: seriesID, Type: "series", Title: "Series Item", TvdbID: "9903", Status: "matched"}
	if err := itemRepo.Upsert(ctx, seriesItem); err != nil {
		t.Fatalf("seed series item: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_items(collection_id,media_item_id,position)
		VALUES ($1,$2,1), ($1,$3,2) ON CONFLICT DO NOTHING`, colID, movieID, seriesID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	runReq := func(iID string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/admin/collections/"+colID+"/materialize/"+iID, nil)
		rCtx := chi.NewRouteContext()
		rCtx.URLParams.Add("id", colID)
		rCtx.URLParams.Add("item_id", iID)
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rCtx))
		handler.HandleMaterializeAdminCollectionItem(rec, req)
		return rec
	}

	// 1. Provider failure -> 503 StatusServiceUnavailable
	recProv := runReq(seriesID)
	if recProv.Code != http.StatusServiceUnavailable {
		t.Fatalf("provider failure code = %d, want 503. Body: %s", recProv.Code, recProv.Body.String())
	}
	var provBody map[string]any
	if err := json.Unmarshal(recProv.Body.Bytes(), &provBody); err != nil {
		t.Fatalf("decode 503 body: %v", err)
	}
	if provBody["error"] != "provider_unavailable" {
		t.Fatalf("expected error='provider_unavailable', got %v", provBody)
	}

	// 2. Incompatible library (movie in a series-only folder)
	// Now fix provider so it doesn't fail on provider check
	service.VirtualVariants = func(_ context.Context, _, _ string) ([]catalog.VirtualPlaybackVariant, error) {
		return []catalog.VirtualPlaybackVariant{{OwnerInstallationID: 11}}, nil
	}
	recIncompat := runReq(movieID)
	if recIncompat.Code != http.StatusBadRequest {
		t.Fatalf("incompatible library code = %d, want 400. Body: %s", recIncompat.Code, recIncompat.Body.String())
	}
}

func TestHandleMaterializeAdminCollectionItem_SeriesWithEpisodes(t *testing.T) {
	pool := materializeTestPool(t)
	ctx := context.Background()

	itemRepo := catalog.NewItemRepository(pool)
	collRepo := catalog.NewLibraryCollectionRepository(pool)
	service := catalog.NewLibraryCollectionService(collRepo, itemRepo, nil, nil)
	service.VirtualVariants = func(_ context.Context, _, _ string) ([]catalog.VirtualPlaybackVariant, error) {
		return []catalog.VirtualPlaybackVariant{{OwnerInstallationID: 11}}, nil
	}
	handler := NewLibraryCollectionHandler(collRepo, service, itemRepo, nil)

	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(9904,'Series Folder 4','series',true) ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}

	const colID = "col-mat-series-1"
	const seriesID = "series-mat-series-1"
	cfg, _ := json.Marshal(map[string]any{"virtual_playback": true})

	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
		VALUES($1,$1,$1,'tmdb',9904,$2) ON CONFLICT (id) DO UPDATE SET source_config=$2`, colID, cfg); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_libraries(collection_id,library_id)
		VALUES($1,9904) ON CONFLICT DO NOTHING`, colID); err != nil {
		t.Fatalf("seed collection library: %v", err)
	}

	seriesItem := &models.MediaItem{ContentID: seriesID, Type: "series", Title: "Series With Episodes", TvdbID: "9904", Status: "matched"}
	if err := itemRepo.Upsert(ctx, seriesItem); err != nil {
		t.Fatalf("seed series item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_items(collection_id,media_item_id,position)
		VALUES ($1,$2,1) ON CONFLICT DO NOTHING`, colID, seriesID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	// Seed season and episodes: 1 released, 1 future, 1 special
	if _, err := pool.Exec(ctx, `
		INSERT INTO seasons(content_id,series_id,season_number,title)
		VALUES ('season-mat-s1',$1,1,'Season 1') ON CONFLICT DO NOTHING`, seriesID); err != nil {
		t.Fatalf("seed season: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO episodes(content_id,series_id,season_id,season_number,episode_number,title,air_date)
		VALUES
			('ep-mat-1-1',$1,'season-mat-s1',1,1,'Released Ep',CURRENT_DATE),
			('ep-mat-1-2',$1,'season-mat-s1',1,2,'Future Ep',CURRENT_DATE+1),
			('ep-mat-0-1',$1,'season-mat-s1',0,1,'Special Ep',CURRENT_DATE)
		ON CONFLICT DO NOTHING`, seriesID); err != nil {
		t.Fatalf("seed episodes: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/collections/"+colID+"/materialize/"+seriesID, nil)
	rCtx := chi.NewRouteContext()
	rCtx.URLParams.Add("id", colID)
	rCtx.URLParams.Add("item_id", seriesID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rCtx))
	handler.HandleMaterializeAdminCollectionItem(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("series materialize code = %d, body: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Success              bool   `json:"success"`
		ContentID            string `json:"content_id"`
		MediaType            string `json:"media_type"`
		FilesCreated         int    `json:"files_created"`
		FilesExisting        int    `json:"files_existing"`
		EpisodesMaterialized int    `json:"episodes_materialized"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode series resp: %v", err)
	}
	// FilesCreated = 1 base + 1 released episode = 2
	// EpisodesMaterialized = 1
	if !resp.Success || resp.ContentID != seriesID || resp.MediaType != "series" || resp.FilesCreated != 2 || resp.EpisodesMaterialized != 1 {
		t.Fatalf("unexpected series resp: %+v, want FilesCreated=2 EpisodesMaterialized=1", resp)
	}
}

func TestHandleMaterializeAdminCollectionItem_RoutedAuthorization(t *testing.T) {
	pool := materializeTestPool(t)
	ctx := context.Background()

	itemRepo := catalog.NewItemRepository(pool)
	collRepo := catalog.NewLibraryCollectionRepository(pool)
	service := catalog.NewLibraryCollectionService(collRepo, itemRepo, nil, nil)
	service.TMDBDigitalReleases = releasedDigitalReleaseChecker{}
	service.VirtualVariants = func(_ context.Context, _, _ string) ([]catalog.VirtualPlaybackVariant, error) {
		return []catalog.VirtualPlaybackVariant{{OwnerInstallationID: 11}}, nil
	}
	handler := NewLibraryCollectionHandler(collRepo, service, itemRepo, nil)

	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(9905,'Auth Folder','movies',true) ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}

	const colID = "col-mat-auth-1"
	const itemID = "movie-mat-auth-1"
	cfg, _ := json.Marshal(map[string]any{"virtual_playback": true})

	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
		VALUES($1,$1,$1,'tmdb',9905,$2) ON CONFLICT (id) DO UPDATE SET source_config=$2`, colID, cfg); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_libraries(collection_id,library_id)
		VALUES($1,9905) ON CONFLICT DO NOTHING`, colID); err != nil {
		t.Fatalf("seed collection library: %v", err)
	}

	movieItem := &models.MediaItem{ContentID: itemID, Type: "movie", Title: "Auth Movie", TmdbID: "9905", Status: "matched"}
	if err := itemRepo.Upsert(ctx, movieItem); err != nil {
		t.Fatalf("seed movie item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_items(collection_id,media_item_id,position)
		VALUES ($1,$2,1) ON CONFLICT DO NOTHING`, colID, itemID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	primaryChecker := func(_ context.Context, _ int, profileID string) (bool, bool, error) {
		if profileID == "primary-profile" {
			return true, true, nil
		}
		if profileID == "secondary-profile" {
			return false, true, nil
		}
		return false, false, nil
	}

	r := chi.NewRouter()
	r.Route("/api/v1/admin/collections", func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				authHeader := req.Header.Get("Authorization")
				if authHeader == "" {
					http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
					return
				}
				role := req.Header.Get("X-Test-Role")
				if role == "" {
					role = "user"
				}
				reqCtx := apimw.SetClaims(req.Context(), &auth.Claims{UserID: 1, Role: role, TokenType: auth.TokenTypeAccess})
				next.ServeHTTP(w, req.WithContext(reqCtx))
			})
		})
		r.Use(apimw.RequireActingAdmin(primaryChecker, nil))
		r.Post("/{id}/materialize/{item_id}", handler.HandleMaterializeAdminCollectionItem)
	})

	runReq := func(token, role, profileID string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/collections/"+colID+"/materialize/"+itemID, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if role != "" {
			req.Header.Set("X-Test-Role", role)
		}
		if profileID != "" {
			req.Header.Set("X-Profile-Id", profileID)
		}
		r.ServeHTTP(rec, req)
		return rec
	}

	assertFilesCount := func(want int) {
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_files WHERE content_id=$1`, itemID).Scan(&count); err != nil {
			t.Fatalf("count files: %v", err)
		}
		if count != want {
			t.Fatalf("media_files count = %d, want %d", count, want)
		}
	}

	// 1. Unauthenticated -> 401
	recUnauth := runReq("", "", "")
	if recUnauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauth code = %d, want 401", recUnauth.Code)
	}
	assertFilesCount(0)

	// 2. Non-admin role -> 403
	recNonAdmin := runReq("valid-token", "user", "")
	if recNonAdmin.Code != http.StatusForbidden {
		t.Fatalf("non-admin code = %d, want 403", recNonAdmin.Code)
	}
	assertFilesCount(0)

	// 3. Admin role with secondary profile -> 403
	recSecondary := runReq("valid-token", "admin", "secondary-profile")
	if recSecondary.Code != http.StatusForbidden {
		t.Fatalf("secondary profile code = %d, want 403", recSecondary.Code)
	}
	assertFilesCount(0)

	// 4. Admin role with unknown profile -> 403
	recUnknown := runReq("valid-token", "admin", "unknown-profile")
	if recUnknown.Code != http.StatusForbidden {
		t.Fatalf("unknown profile code = %d, want 403", recUnknown.Code)
	}
	assertFilesCount(0)

	// 5. Admin role with primary profile -> 200 OK
	recAllowed := runReq("valid-token", "admin", "primary-profile")
	if recAllowed.Code != http.StatusOK {
		t.Fatalf("allowed admin code = %d, want 200, body = %s", recAllowed.Code, recAllowed.Body.String())
	}
	assertFilesCount(1)
}

func TestHandleRemoveAdminCollectionItem_Routed(t *testing.T) {
	pool := materializeTestPool(t)
	ctx := context.Background()

	itemRepo := catalog.NewItemRepository(pool)
	collRepo := catalog.NewLibraryCollectionRepository(pool)
	service := catalog.NewLibraryCollectionService(collRepo, itemRepo, nil, nil)
	service.TMDBDigitalReleases = releasedDigitalReleaseChecker{}
	service.VirtualVariants = func(_ context.Context, _, _ string) ([]catalog.VirtualPlaybackVariant, error) {
		return []catalog.VirtualPlaybackVariant{{OwnerInstallationID: 11}}, nil
	}
	handler := NewLibraryCollectionHandler(collRepo, service, itemRepo, nil)

	if _, err := pool.Exec(ctx, `INSERT INTO media_folders(id,name,type,enabled) VALUES(9906,'Remove Item Folder','movies',true) ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}

	const manualColID = "col-manual-remove-1"
	const syncedColID = "col-synced-remove-1"
	const itemID = "movie-manual-remove-1"
	cfg, _ := json.Marshal(map[string]any{"virtual_playback": true})

	// Seed manual collection
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
		VALUES($1,$1,'Manual Col','manual',9906,$2) ON CONFLICT (id) DO NOTHING`, manualColID, cfg); err != nil {
		t.Fatalf("seed manual collection: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_libraries(collection_id,library_id)
		VALUES($1,9906) ON CONFLICT DO NOTHING`, manualColID); err != nil {
		t.Fatalf("seed collection library: %v", err)
	}

	// Seed synced (tmdb) collection
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collections(id,slug,title,collection_type,library_id,source_config)
		VALUES($1,$1,'Synced Col','tmdb',9906,$2) ON CONFLICT (id) DO NOTHING`, syncedColID, cfg); err != nil {
		t.Fatalf("seed synced collection: %v", err)
	}

	movieItem := &models.MediaItem{ContentID: itemID, Type: "movie", Title: "Remove Movie", TmdbID: "9906", Status: "matched"}
	if err := itemRepo.Upsert(ctx, movieItem); err != nil {
		t.Fatalf("seed movie item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO library_collection_items(collection_id,media_item_id,position)
		VALUES ($1,$2,1) ON CONFLICT DO NOTHING`, manualColID, itemID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	// Materialize item in manual collection
	manualCol, _ := collRepo.GetByID(ctx, manualColID)
	if _, err := service.EnsureCollectionItemMaterialized(ctx, manualCol, movieItem); err != nil {
		t.Fatalf("materialize item: %v", err)
	}

	// Router setup with auth & acting admin middleware
	primaryChecker := func(_ context.Context, _ int, profileID string) (bool, bool, error) {
		if profileID == "primary-profile" {
			return true, true, nil
		}
		return false, false, nil
	}

	r := chi.NewRouter()
	r.Route("/api/v1/admin/collections", func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				authHeader := req.Header.Get("Authorization")
				if authHeader == "" {
					http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
					return
				}
				role := req.Header.Get("X-Test-Role")
				if role == "" {
					role = "user"
				}
				reqCtx := apimw.SetClaims(req.Context(), &auth.Claims{UserID: 1, Role: role, TokenType: auth.TokenTypeAccess})
				next.ServeHTTP(w, req.WithContext(reqCtx))
			})
		})
		r.Use(apimw.RequireActingAdmin(primaryChecker, nil))
		r.Delete("/{id}/items/{item_id}", handler.HandleRemoveAdminCollectionItem)
	})

	runReq := func(token, role, profileID, col, item string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/collections/"+col+"/items/"+item, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if role != "" {
			req.Header.Set("X-Test-Role", role)
		}
		if profileID != "" {
			req.Header.Set("X-Profile-Id", profileID)
		}
		r.ServeHTTP(rec, req)
		return rec
	}

	// 1. Unauthenticated -> 401
	if rec := runReq("", "", "", manualColID, itemID); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth code = %d, want 401", rec.Code)
	}

	// 2. Non-admin role -> 403
	if rec := runReq("valid-token", "user", "primary-profile", manualColID, itemID); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin code = %d, want 403", rec.Code)
	}

	// 3. Synced collection -> 409 Conflict (manual collections only)
	if rec := runReq("valid-token", "admin", "primary-profile", syncedColID, itemID); rec.Code != http.StatusConflict {
		t.Fatalf("synced collection code = %d, want 409", rec.Code)
	}

	// 4. Non-existent collection -> 404
	if rec := runReq("valid-token", "admin", "primary-profile", "non-existent-col", itemID); rec.Code != http.StatusNotFound {
		t.Fatalf("non-existent collection code = %d, want 404", rec.Code)
	}

	// 5. Successful removal -> 204 No Content
	if rec := runReq("valid-token", "admin", "primary-profile", manualColID, itemID); rec.Code != http.StatusNoContent {
		t.Fatalf("successful remove code = %d, want 204, body = %s", rec.Code, rec.Body.String())
	}

	// Verify membership removed
	var isMember bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM library_collection_items WHERE collection_id=$1 AND media_item_id=$2)`, manualColID, itemID).Scan(&isMember); err != nil || isMember {
		t.Fatalf("membership still exists: %v", isMember)
	}

	// Verify orphaned file removed
	var filesCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_files WHERE content_id=$1`, itemID).Scan(&filesCount); err != nil || filesCount != 0 {
		t.Fatalf("media files count = %d, want 0", filesCount)
	}
}
