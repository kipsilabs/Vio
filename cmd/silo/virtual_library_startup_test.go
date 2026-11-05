package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Silo-Server/silo-server/internal/api"
	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/metadata/tmdb"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

type mapVirtualLibrarySettings struct {
	values map[string]string
}

func (m *mapVirtualLibrarySettings) GetAll(context.Context) (map[string]string, error) {
	out := make(map[string]string, len(m.values))
	for key, value := range m.values {
		out[key] = value
	}
	return out, nil
}

func (m *mapVirtualLibrarySettings) Get(_ context.Context, key string) (string, error) {
	return m.values[key], nil
}

func (m *mapVirtualLibrarySettings) SetMany(_ context.Context, values map[string]string) error {
	for key, value := range values {
		m.values[key] = value
	}
	return nil
}

func newVirtualLibraryProvider(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/manifest.json" {
			_, _ = w.Write([]byte(`{"id":"org.stremio.core","resources":["stream"],"types":["movie"]}`))
			return
		}
		_, _ = w.Write([]byte(`{"streams":[{"name":"HD","url":"http://127.0.0.1:8080/stream.mkv"}]}`))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestVirtualLibraryStartupWiresCoreServiceAndResolvers(t *testing.T) {
	provider := newVirtualLibraryProvider(t)
	settings := &mapVirtualLibrarySettings{values: map[string]string{
		"virtual_library.enabled":               "true",
		"virtual_library.manifest_url":          provider.URL + "/manifest.json",
		"virtual_library.allow_insecure_http":   "true",
		"virtual_library.allow_private_streams": "true",
	}}
	collections := catalog.NewLibraryCollectionService(nil, nil, nil, nil)
	deps := &api.Dependencies{}

	svc, cfg := virtualLibraryStartup{
		Settings: settings,
		Installations: virtuallibrary.InstallationListerFunc(func(context.Context, string) ([]*virtuallibrary.PluginInstallation, error) {
			return nil, nil
		}),
		Configs: virtuallibrary.PluginConfigReaderFunc(func(context.Context, int, string) (map[string]any, error) {
			return nil, nil
		}),
		Collections: collections,
		Deps:        deps,
	}.start(context.Background())

	if svc == nil {
		t.Fatal("start returned a nil service for a configured provider")
	}
	if cfg.ManifestURL == "" {
		t.Fatal("start returned an empty config")
	}
	if deps.VirtualLibraryService != svc {
		t.Fatal("startup did not install the core virtual library on the API dependencies")
	}
	if deps.VirtualLibraryVariants == nil {
		t.Fatal("startup did not install the core virtual variants closure")
	}
	if collections.VirtualVariants == nil {
		t.Fatal("startup did not install core variants on the collection service")
	}

	resolved, err := svc.ResolveDetailed(context.Background(), "virtual://movie/tt100", false, nil, "", false)
	if err != nil {
		t.Fatalf("core ResolveDetailed failed: %v", err)
	}
	if resolved.URL != "http://127.0.0.1:8080/stream.mkv" {
		t.Fatalf("resolved URL = %q, want stream.mkv", resolved.URL)
	}

	variants, err := deps.VirtualLibraryVariants(context.Background(), "virtual://movie/tt100", "movie")
	if err != nil {
		t.Fatalf("wired variants closure failed: %v", err)
	}
	if len(variants) == 0 {
		t.Fatal("wired variants closure returned no variants")
	}
}

func TestVirtualLibraryStartupDormantLeavesDependenciesUnset(t *testing.T) {
	settings := &mapVirtualLibrarySettings{values: map[string]string{
		"virtual_library.enabled": "true",
	}}
	collections := catalog.NewLibraryCollectionService(nil, nil, nil, nil)
	deps := &api.Dependencies{}

	svc, _ := virtualLibraryStartup{
		Settings: settings,
		Installations: virtuallibrary.InstallationListerFunc(func(context.Context, string) ([]*virtuallibrary.PluginInstallation, error) {
			return nil, nil
		}),
		Configs: virtuallibrary.PluginConfigReaderFunc(func(context.Context, int, string) (map[string]any, error) {
			return nil, nil
		}),
		Collections: collections,
		Deps:        deps,
	}.start(context.Background())

	if svc != nil {
		t.Fatal("start activated a service with no manifest URL")
	}
	if deps.VirtualLibraryService != nil || deps.VirtualLibraryVariants != nil {
		t.Fatal("dormant startup must leave the virtual library dependencies unset")
	}
	if collections.VirtualVariants != nil {
		t.Fatal("dormant startup must leave collection variants unset")
	}
}

func TestWireCollectionTMDBUsesTheSharedClientForDigitalReleases(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/movie/123/release_dates" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"iso_3166_1":"US","release_dates":[{"type":4,"release_date":"2020-01-01"}]}]}`))
	}))
	t.Cleanup(server.Close)

	client := tmdb.NewClient("test-key", 40)
	// Point the shared client at the test server: a second client built inside
	// the adapter would still target TMDB and this lookup would fail.
	client.SetBaseURL(server.URL)

	collections := catalog.NewLibraryCollectionService(nil, nil, nil, nil)
	adapter := wireCollectionTMDB(collections, client)
	if adapter == nil {
		t.Fatal("wireCollectionTMDB returned a nil adapter")
	}
	if collections.TMDBCollections == nil {
		t.Fatal("collection preset fetcher was not wired")
	}
	if collections.TMDBDiscovers != adapter {
		t.Fatal("discover source was not wired to the shared TMDB adapter")
	}
	if collections.TMDBDigitalReleases != adapter {
		t.Fatal("digital release checker was not wired to the shared TMDB adapter")
	}

	released, err := collections.TMDBDigitalReleases.HasDigitalRelease(context.Background(), 123)
	if err != nil {
		t.Fatalf("digital release lookup through the shared client failed: %v", err)
	}
	if !released {
		t.Fatal("a movie with a past digital release must be eligible")
	}
}
