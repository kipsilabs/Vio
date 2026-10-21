package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/api/handlers"
	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/auth"
	"github.com/Silo-Server/silo-server/internal/clientip"
	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/plugins"
	"github.com/Silo-Server/silo-server/internal/scanner"
	"github.com/Silo-Server/silo-server/internal/secret"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

func newCoreTestProvider(t *testing.T) (*httptest.Server, *virtuallibrary.Service) {
	t.Helper()
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/manifest.json" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"org.stremio.core","resources":["stream"],"types":["movie"]}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"streams":[{"name":"HD","url":"http://127.0.0.1:8080/stream.mkv"}]}`))
	}))
	t.Cleanup(mockServer.Close)

	vlSvc := virtuallibrary.New(virtuallibrary.Config{
		Enabled:             true,
		ManifestURL:         mockServer.URL + "/manifest.json",
		AllowInsecureHTTP:   true,
		AllowPrivateStreams: true,
	}, nil, nil)
	if vlSvc == nil {
		t.Fatal("expected non-nil virtuallibrary.Service")
	}
	return mockServer, vlSvc
}

func newAuthorizedContext() context.Context {
	ctx := context.Background()
	ctx = apimw.SetClaims(ctx, &auth.Claims{UserID: 1, Role: "user", TokenType: auth.TokenTypeAccess})
	return apimw.SetProfileID(ctx, "profile-1")
}

func TestRouterWiresVirtualLibraryService(t *testing.T) {
	_, vlSvc := newCoreTestProvider(t)

	deps := Dependencies{
		Config:                &config.Config{},
		VirtualLibraryService: vlSvc,
	}

	// router initialization must succeed without crashing or panicking
	router := NewRouter(deps)
	if router == nil {
		t.Fatal("NewRouter returned nil")
	}

	// Verify the core service can resolve a virtual path directly
	resolved, err := vlSvc.ResolveDetailed(context.Background(), "virtual://movie/tt100", false, nil, "", false)
	if err != nil {
		t.Fatalf("core ResolveDetailed failed: %v", err)
	}
	if resolved.URL != "http://127.0.0.1:8080/stream.mkv" {
		t.Fatalf("resolved URL = %q, want stream.mkv", resolved.URL)
	}
}

// TestRouterWiresCorePlaybackWithoutPlugin exercises the real playback
// wiring path (SessionMgr + FileRepo present, PluginService absent). Virtual
// resolution is core-only, so the plugin service is simply not consulted:
// core owns every virtual URI with no plugin process running.
func TestRouterWiresCorePlaybackWithoutPlugin(t *testing.T) {
	_, vlSvc := newCoreTestProvider(t)

	deps := Dependencies{
		Config:                &config.Config{},
		SessionMgr:            playback.NewSessionManager(1, 1),
		FileRepo:              scanner.NewFileRepository(nil),
		VirtualLibraryService: vlSvc,
		// PluginService deliberately nil: core-only operation.
	}

	router := NewRouter(deps)
	if router == nil {
		t.Fatal("NewRouter returned nil for core-only deps")
	}

	// The service captured by the wired closures must serve owner<=0
	// (core-owned) media end to end: variants for listing, streams for
	// playback selection, detailed resolution for transport.
	variants, err := vlSvc.Variants(context.Background(), "virtual://movie/tt100", "movie")
	if err != nil {
		t.Fatalf("core Variants failed: %v", err)
	}
	if len(variants) == 0 {
		t.Fatal("expected at least one variant")
	}
	for _, v := range variants {
		if v.OwnerInstallationID != 0 {
			t.Fatalf("variant owner = %d, want 0 (core-owned)", v.OwnerInstallationID)
		}
	}

	streams, err := vlSvc.ListStreams(context.Background(), "virtual://movie/tt100")
	if err != nil {
		t.Fatalf("core ListStreams failed: %v", err)
	}
	if len(streams) == 0 {
		t.Fatal("expected at least one stream")
	}

	resolved, err := vlSvc.ResolveDetailed(context.Background(), "virtual://movie/tt100", false, nil, "", false)
	if err != nil {
		t.Fatalf("core ResolveDetailed failed: %v", err)
	}
	if resolved.URL == "" {
		t.Fatal("expected non-empty resolved URL")
	}
}

// TestRouterStreamRouteMounted tests that playback routes are mounted on
// the router and protected by authentication middleware.
func TestRouterStreamRouteMounted(t *testing.T) {
	_, vlSvc := newCoreTestProvider(t)

	cfg, err := config.LoadFromDB(map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), "postgres://nobody:nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	cipher, err := secret.New(bytes.Repeat([]byte{0}, secret.MinMasterKeyLen))
	if err != nil {
		t.Fatal(err)
	}

	sessionMgr := playback.NewSessionManager(1, 1)
	deps := Dependencies{
		Config:                cfg,
		AppContext:            t.Context(),
		DB:                    pool,
		SecretCipher:          cipher,
		ClientIPResolver:      clientip.NewResolver(nil),
		SessionMgr:            sessionMgr,
		FileRepo:              scanner.NewFileRepository(nil),
		VirtualLibraryService: vlSvc,
	}

	router := NewRouter(deps)
	if router == nil {
		t.Fatal("NewRouter returned nil")
	}

	// Request to /api/v1/playback/start without auth returns 401 Unauthorized,
	// proving the router mounts playback routes through authentication middleware.
	reqStart := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	rrStart := httptest.NewRecorder()
	router.ServeHTTP(rrStart, reqStart)
	if rrStart.Code != http.StatusUnauthorized {
		t.Fatalf("playback start status = %d, want 401; body = %s", rrStart.Code, rrStart.Body.String())
	}
}

// TestRouterCoreOwnershipPolicy documents the core-only dispatch contract the
// router closures implement: every virtual URI resolves by path through the
// core virtual library, including rows left over from plugin ownership. A nil
// core service surfaces unavailability explicitly.
func TestRouterCoreOwnershipPolicy(t *testing.T) {
	_, vlSvc := newCoreTestProvider(t)

	// Core serves unowned rows.
	if _, err := vlSvc.Resolve(context.Background(), "virtual://movie/tt100"); err != nil {
		t.Fatalf("core Resolve for unowned path failed: %v", err)
	}

	// A nil core service surfaces unavailability explicitly.
	var nilSvc *virtuallibrary.Service
	if _, err := nilSvc.Resolve(context.Background(), "virtual://movie/tt100"); err == nil {
		t.Fatal("expected unavailable error from nil service")
	}
}

func TestVirtualMediaResolverDoesNotFallBackToPlugin(t *testing.T) {
	proxy := &plugins.HTTPProxy{}
	handler := &handlers.PlaybackHandler{}
	wireVirtualMediaResolver(handler, Dependencies{PluginHTTPProxy: proxy})
	if handler.VirtualMediaResolver != nil {
		t.Fatal("virtual media resolver fell back to a plugin when core is unavailable")
	}

	_, core := newCoreTestProvider(t)
	handler = &handlers.PlaybackHandler{}
	wireVirtualMediaResolver(handler, Dependencies{VirtualLibraryService: core, PluginHTTPProxy: proxy})
	if handler.VirtualMediaResolver == nil {
		t.Fatal("core virtual-library resolver was not wired")
	}
}
