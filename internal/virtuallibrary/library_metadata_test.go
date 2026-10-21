package virtuallibrary

import (
	"context"
	"errors"
	"testing"

	"github.com/Silo-Server/silo-server/internal/metadata"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/monitor"
)

type stubLibraryResolver struct {
	lookup       *metadata.LibraryMetadataResult
	err          error
	folderID     int
	knownSeasons []int
	artwork      []string
}

func (r *stubLibraryResolver) FetchLibraryMetadata(_ context.Context, folderID int, _ *models.MediaItem, knownSeasons []int) (*metadata.LibraryMetadataResult, error) {
	r.folderID = folderID
	r.knownSeasons = knownSeasons
	return r.lookup, r.err
}

func (r *stubLibraryResolver) EnqueueVirtualMediaArtwork(_ context.Context, _, _ string, _ map[string]string, poster, backdrop string) error {
	r.artwork = append(r.artwork, poster, backdrop)
	return nil
}

func libraryMetadataService(cfg Config) *Service {
	return &Service{cfg: cfg, Monitor: monitor.New(nil, nil, nil)}
}

// TestLibraryMetadataResolverUsesTheItemLibrary covers the destination: the
// lookup runs against the library the item is registered in.
func TestLibraryMetadataResolverUsesTheItemLibrary(t *testing.T) {
	service := libraryMetadataService(Config{MovieLibraryID: 7, SeriesLibraryID: 8})
	resolver := &stubLibraryResolver{lookup: &metadata.LibraryMetadataResult{
		Metadata: &metadata.MetadataResult{HasMetadata: true, Title: "Movie"},
	}}

	updated, err := service.libraryMetadataResolver(resolver)(context.Background(), monitor.MonitoredMedia{
		Key: "movie:imdb:tt1", MediaType: "movie", MediaFolderID: 42,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolver.folderID != 42 {
		t.Fatalf("lookup library = %d, want 42", resolver.folderID)
	}
	if updated.MediaFolderID != 42 {
		t.Fatalf("item library = %d, want 42", updated.MediaFolderID)
	}
}

// TestLibraryMetadataResolverFallsBackToTheServiceLibrary covers an item
// without a destination: the service-wide library for that media type is
// used.
func TestLibraryMetadataResolverFallsBackToTheServiceLibrary(t *testing.T) {
	service := libraryMetadataService(Config{MovieLibraryID: 7, SeriesLibraryID: 8})
	resolver := &stubLibraryResolver{lookup: &metadata.LibraryMetadataResult{
		Metadata: &metadata.MetadataResult{HasMetadata: true, Title: "Series"},
		Episodes: []metadata.EpisodeResult{{SeasonNumber: 1, EpisodeNumber: 1, Title: "Pilot"}},
	}}

	if _, err := service.libraryMetadataResolver(resolver)(context.Background(), monitor.MonitoredMedia{
		Key: "series:imdb:tt1", MediaType: "series",
	}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolver.folderID != 8 {
		t.Fatalf("lookup library = %d, want the series library 8", resolver.folderID)
	}
}

// TestLibraryMetadataResolverKeepsKnownEpisodes covers the merge: the
// episodes the item already holds survive a lookup that answered only part
// of the series.
func TestLibraryMetadataResolverKeepsKnownEpisodes(t *testing.T) {
	service := libraryMetadataService(Config{SeriesLibraryID: 8})
	resolver := &stubLibraryResolver{lookup: &metadata.LibraryMetadataResult{
		Metadata: &metadata.MetadataResult{HasMetadata: true, Title: "Series"},
		Episodes: []metadata.EpisodeResult{
			{SeasonNumber: 2, EpisodeNumber: 1, Title: "Second season"},
		},
		Complete: false,
	}}

	updated, err := service.libraryMetadataResolver(resolver)(context.Background(), monitor.MonitoredMedia{
		Key: "series:imdb:tt1", MediaType: "series", MediaFolderID: 42,
		Episodes: []monitor.VirtualEpisode{{Season: 1, Episode: 1, Title: "Pilot"}},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(updated.Episodes) != 2 {
		t.Fatalf("episodes = %d, want the known and the fresh one", len(updated.Episodes))
	}
	titles := map[string]bool{}
	for _, episode := range updated.Episodes {
		titles[episode.Title] = true
	}
	if !titles["Pilot"] || !titles["Second season"] {
		t.Fatalf("episodes = %+v, want both the known and the fresh episode", updated.Episodes)
	}
	if len(resolver.knownSeasons) != 1 || resolver.knownSeasons[0] != 1 {
		t.Fatalf("known seasons = %v, want [1]", resolver.knownSeasons)
	}
}

// TestLibraryMetadataResolverUsesImageProviderArtwork covers the artwork: a
// provider that only answers image requests still fills the poster.
func TestLibraryMetadataResolverUsesImageProviderArtwork(t *testing.T) {
	service := libraryMetadataService(Config{MovieLibraryID: 7})
	resolver := &stubLibraryResolver{lookup: &metadata.LibraryMetadataResult{
		Metadata: &metadata.MetadataResult{HasMetadata: true, Title: "Movie"},
		Images: []metadata.RemoteImage{
			{Type: metadata.ImagePoster, URL: "https://images.example/poster.jpg", Rating: 8},
			{Type: metadata.ImagePoster, URL: "https://images.example/other.jpg", Rating: 3},
			{Type: metadata.ImageBackdrop, URL: "https://images.example/backdrop.jpg"},
		},
	}}

	updated, err := service.libraryMetadataResolver(resolver)(context.Background(), monitor.MonitoredMedia{
		Key: "movie:imdb:tt1", MediaType: "movie", MediaFolderID: 42,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if updated.Poster != "https://images.example/poster.jpg" {
		t.Fatalf("poster = %q, want the highest rated poster", updated.Poster)
	}
	if updated.Backdrop != "https://images.example/backdrop.jpg" {
		t.Fatalf("backdrop = %q, want the image provider's backdrop", updated.Backdrop)
	}
}

// TestLibraryMetadataResolverReportsUnconfiguredLibrary covers the
// configuration failure: the monitor has to be able to tell it apart from a
// transient miss.
func TestLibraryMetadataResolverReportsUnconfiguredLibrary(t *testing.T) {
	service := libraryMetadataService(Config{})
	resolver := &stubLibraryResolver{err: metadata.ErrMetadataNotConfigured}

	_, err := service.libraryMetadataResolver(resolver)(context.Background(), monitor.MonitoredMedia{
		Key: "movie:imdb:tt1", MediaType: "movie",
	})
	if !errors.Is(err, monitor.ErrMetadataNotConfigured) {
		t.Fatalf("error = %v, want the configuration failure", err)
	}
}
