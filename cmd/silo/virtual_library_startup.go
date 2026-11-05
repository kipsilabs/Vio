package main

import (
	"context"
	"log/slog"

	"github.com/Silo-Server/silo-server/internal/api"
	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/metadata/tmdb"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

// virtualLibrarySettings is the settings surface the core virtual-library
// boot sequence needs. Production supplies the encrypted server settings
// repo; tests supply an in-memory map.
type virtualLibrarySettings interface {
	GetAll(ctx context.Context) (map[string]string, error)
	Get(ctx context.Context, key string) (string, error)
	SetMany(ctx context.Context, values map[string]string) error
}

// virtualLibraryStartup carries the dependencies of the core virtual-library
// boot sequence. Boot order is migrate -> construct -> validate -> activate.
type virtualLibraryStartup struct {
	Settings      virtualLibrarySettings
	Installations virtuallibrary.InstallationLister
	Configs       virtuallibrary.PluginConfigReader
	Registrar     *catalog.VirtualMediaRegistrar
	Metadata      virtuallibrary.LibraryMetadataResolver
	Collections   *catalog.LibraryCollectionService
	Deps          *api.Dependencies
}

// start runs the boot sequence and installs the active service on the API
// dependencies and the collection service. It returns nil when the service is
// dormant, misconfigured, or migration failed; a nil result keeps the retired
// plugin path available.
func (s virtualLibraryStartup) start(ctx context.Context) (*virtuallibrary.Service, virtuallibrary.Config) {
	settings, err := s.Settings.GetAll(ctx)
	if err != nil {
		slog.WarnContext(ctx, "virtual library: settings unavailable; core service dormant", "error", err)
		return nil, virtuallibrary.Config{}
	}
	migrated, err := virtuallibrary.MigrateFromPlugin(ctx, s.Installations, s.Configs, s.Settings, slog.Default())
	if err != nil {
		slog.WarnContext(ctx, "virtual library migration failed; core service unavailable, plugin retained", "error", err)
		return nil, virtuallibrary.Config{}
	}
	if migrated.Ran {
		settings, err = s.Settings.GetAll(ctx)
		if err != nil {
			slog.WarnContext(ctx, "virtual library settings reread failed after migration; core service unavailable, plugin retained", "error", err)
			return nil, virtuallibrary.Config{}
		}
	}
	cfg := virtuallibrary.ConfigFromSettings(settings)
	svc := virtuallibrary.New(cfg, s.Registrar, slog.Default())
	if svc == nil {
		slog.InfoContext(ctx, "virtual library core service dormant (no manifest URL)")
		return nil, cfg
	}
	if err := svc.Validate(ctx); err != nil {
		slog.WarnContext(ctx, "virtual library core service misconfigured; unavailable", "error", err)
		return nil, cfg
	}
	slog.InfoContext(ctx, "virtual library core service active",
		"movie_library_id", cfg.MovieLibraryID,
		"series_library_id", cfg.SeriesLibraryID,
		"migrated", migrated.Migrated)
	svc.SetLibraryMetadataResolver(s.Metadata)
	if s.Collections != nil {
		// Hard cutover: core Variants replaces the plugin closure.
		// Plugin-owned catalog rows keep their owner IDs; new variants
		// carry core provenance (OwnerInstallationID 0 + Provenance core)
		// and the SSRF dispatch reads virtual_library.allow_private_streams.
		s.Collections.VirtualVariants = svc.VirtualVariants()
	}
	if s.Deps != nil {
		s.Deps.VirtualLibraryService = svc
		s.Deps.VirtualLibraryVariants = svc.VirtualVariants()
	}
	return svc, cfg
}

// wireCollectionTMDB installs the shared TMDB client's collection, discover,
// and digital-release adapters on the collection service. Both discover paths
// use one adapter so collection sync and theatrical release gating spend the
// same client (and its cache and rate budget) instead of constructing a second
// TMDB client.
func wireCollectionTMDB(collectionService *catalog.LibraryCollectionService, client *tmdb.Client) *api.TMDBDiscoverAdapter {
	discover := api.NewTMDBDiscoverAdapter(client)
	collectionService.TMDBCollections = api.NewTMDBCollectionFetcher(client)
	collectionService.TMDBDiscovers = discover
	collectionService.TMDBDigitalReleases = discover
	return discover
}
