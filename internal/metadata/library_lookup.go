package metadata

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

// ErrMetadataNotConfigured reports that the destination library has no
// metadata provider chain. It is a configuration failure rather than a
// transient one: an operator has to enable a provider for that library.
var ErrMetadataNotConfigured = errors.New("no metadata provider is configured for this library")

// libraryMaxSeasonsPerLookup bounds how many seasons one lookup traverses.
// A long-running show carries more history than a single evaluation budget
// can fetch, so a lookup takes the seasons it can reach and reports the
// snapshot as incomplete. The next pass continues with the seasons that are
// still missing, and the episodes already registered stay in place.
const libraryMaxSeasonsPerLookup = 8

// LibraryMetadataResult is the outcome of one library-scoped metadata lookup.
type LibraryMetadataResult struct {
	// Metadata is the folded provider metadata for the item.
	Metadata *MetadataResult
	// Episodes are the episodes the providers reported. An incomplete lookup
	// returns the episodes it did fetch, not the whole series.
	Episodes []EpisodeResult
	// Complete reports that the episode snapshot is authoritative: every
	// season the lookup set out to fetch was answered successfully. An
	// incomplete snapshot must never replace the episodes already known.
	Complete bool
	// Images are the images the configured image providers reported. They
	// supply artwork for providers that do not carry it in their metadata.
	Images []RemoteImage
}

// FetchLibraryMetadata resolves the metadata for one library item: the
// providers configured for the destination library, the episodes of a series,
// and the artwork the image providers know about.
//
// knownSeasons lists the seasons the caller already holds episodes for. Those
// seasons are not fetched again, so a refresh stays inside its budget and
// grows the registered history a season at a time.
func (s *MetadataService) FetchLibraryMetadata(ctx context.Context, folderID int, item *models.MediaItem, knownSeasons []int) (*LibraryMetadataResult, error) {
	if s == nil || folderID <= 0 || item == nil || (item.Type != "movie" && item.Type != "series") {
		return nil, fmt.Errorf("library metadata requires a destination library and movie or series identity")
	}
	language := s.resolveFolderLanguage(ctx, folderID)
	providerIDs, err := s.resolveSeriesRefreshProviderIDs(ctx, item, folderID, language, true)
	if err != nil {
		return nil, err
	}
	chain, err := s.resolveChainCached(ctx, folderID, item.Type)
	if err != nil {
		return nil, err
	}
	if len(chain) == 0 {
		return nil, fmt.Errorf("%w: library %d", ErrMetadataNotConfigured, folderID)
	}
	result := &MetadataResult{ProviderIDs: maps.Clone(providerIDs)}
	for _, provider := range chain {
		metadataProvider, ok := provider.(MetadataProvider)
		if !ok {
			continue
		}
		value, err := metadataProvider.GetMetadata(ctx, MetadataRequest{
			ProviderIDs: result.ProviderIDs,
			ContentType: item.Type,
			Language:    language,
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			slog.WarnContext(ctx, "library metadata provider failed", "component", "metadata", "provider", provider.Slug(), "error", err)
			continue
		}
		if value == nil || !value.HasMetadata {
			continue
		}
		// Validate against the identity the lookup has accumulated, not the
		// IDs it started with: a provider that answers with a different
		// canonical ID than an earlier provider already contributed
		// contradicts the item's identity, and its metadata is not folded in.
		if searchResultConflictsWithTrustedIDs(result.ProviderIDs, value.ProviderIDs) {
			slog.WarnContext(ctx, "library metadata provider contradicted the trusted identity", "component", "metadata", "provider", provider.Slug())
			continue
		}
		_, isIdentityHinter := provider.(IdentityHintProvider)
		foldProviderResult(result, value, language, provider.Slug(), isIdentityHinter, nil)
		result.HasMetadata = true
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !result.HasMetadata {
		return nil, ErrMetadataNotFound
	}
	lookup := &LibraryMetadataResult{Metadata: result, Complete: true}
	images, _, err := s.FetchItemImages(ctx, result.ProviderIDs, item.Type, language, folderID)
	if err != nil {
		slog.WarnContext(ctx, "library artwork lookup failed", "component", "metadata", "error", err)
	} else {
		lookup.Images = images
	}
	if item.Type != "series" {
		return lookup, nil
	}
	episodes, complete, err := s.fetchLibrarySeriesEpisodes(ctx, result.ProviderIDs, folderID, language, knownSeasons)
	if err != nil {
		return nil, err
	}
	lookup.Episodes = episodes
	lookup.Complete = complete
	if len(episodes) == 0 {
		if len(knownSeasons) > 0 {
			// Every season the lookup was asked about is already known, so
			// there is nothing to add. The caller keeps the episodes it holds.
			return lookup, nil
		}
		// Nothing is playable yet. The caller keeps the episodes it already
		// has and retries; an empty answer is not a complete snapshot.
		return nil, ErrMetadataNotFound
	}
	return lookup, nil
}

// fetchLibrarySeriesEpisodes fetches the episodes of the seasons the lookup
// still needs.
//
// It reports whether the snapshot is complete. A provider failure, a season
// the lookup budget did not reach, and a season that answered empty without
// being a future season all leave the snapshot incomplete, so the caller
// keeps the episodes it already has instead of replacing them with a partial
// view.
func (s *MetadataService) fetchLibrarySeriesEpisodes(ctx context.Context, providerIDs map[string]string, folderID int, language string, knownSeasons []int) ([]EpisodeResult, bool, error) {
	seasons, _, seasonFailed, err := s.fetchTargetSeasonResults(ctx, providerIDs, folderID, language, -1, seriesChildLocalContext{}, true)
	if err != nil {
		return nil, false, err
	}
	complete := !seasonFailed
	known := make(map[int]struct{}, len(knownSeasons))
	for _, season := range knownSeasons {
		known[season] = struct{}{}
	}
	episodes := make([]EpisodeResult, 0, len(seasons))
	fetched := 0
	for _, season := range seasons {
		if season.SeasonNumber <= 0 {
			continue
		}
		if _, already := known[season.SeasonNumber]; already {
			// The caller already holds this season. Fetching it again would
			// spend the budget on history the catalog has.
			continue
		}
		if fetched >= libraryMaxSeasonsPerLookup {
			complete = false
			continue
		}
		if err := ctx.Err(); err != nil {
			return episodes, false, err
		}
		rows, _, episodeFailed, err := s.fetchTargetEpisodeResults(ctx, providerIDs, folderID, language, season.SeasonNumber, 0, seriesChildLocalContext{}, true)
		if err != nil {
			return episodes, false, err
		}
		fetched++
		if episodeFailed {
			// A provider failed for this season: what came back may be
			// partial, so the snapshot is not authoritative.
			complete = false
		}
		if len(rows) == 0 {
			// An empty answer is only acceptable for a season that has not
			// aired yet. A released season that answers empty is a provider
			// inconsistency, not a statement about the series.
			airDate, parseErr := time.Parse(time.DateOnly, season.AirDate)
			if parseErr != nil || !airDate.After(time.Now()) {
				complete = false
			}
			continue
		}
		episodes = append(episodes, rows...)
	}
	return episodes, complete, nil
}

// EnqueueVirtualMediaArtwork queues the artwork cache jobs for a registered
// virtual item. It satisfies the virtual library's artwork cache contract.
func (s *MetadataService) EnqueueVirtualMediaArtwork(ctx context.Context, contentID, mediaType string, providerIDs map[string]string, poster, backdrop string) error {
	return s.EnqueueLibraryMediaImages(ctx, contentID, mediaType, providerIDs, LibraryMediaArtwork{Poster: poster, Backdrop: backdrop})
}

// LibraryMediaArtwork carries the artwork a virtual item was registered with,
// so the image cache can be primed for exactly those sources.
type LibraryMediaArtwork struct {
	Poster   string
	Backdrop string
}

// EnqueueLibraryMediaImages queues the artwork cache jobs for a registered
// virtual item. It runs after registration, so the catalog row exists and the
// cached copy can replace the provider URL. With image caching off, nothing
// is queued and the client keeps loading the artwork from the provider.
func (s *MetadataService) EnqueueLibraryMediaImages(ctx context.Context, contentID, mediaType string, providerIDs map[string]string, artwork LibraryMediaArtwork) error {
	if s == nil || contentID == "" {
		return nil
	}
	inputs := make([]EnqueueImageCacheJobInput, 0, 2)
	for _, field := range []struct {
		imageType ImageType
		source    string
	}{
		{imageType: ImagePoster, source: artwork.Poster},
		{imageType: ImageBackdrop, source: artwork.Backdrop},
	} {
		sourcePath := strings.TrimSpace(field.source)
		if !isCacheableImageSourcePath(sourcePath) {
			continue
		}
		inputs = append(inputs, EnqueueImageCacheJobInput{
			TargetType:        ImageCacheTargetItem,
			TargetContentID:   contentID,
			SeriesID:          contentID,
			SourcePath:        sourcePath,
			ProviderID:        libraryImageProviderID(sourcePath, providerIDs),
			ProviderContentID: libraryImageProviderContentID(providerIDs),
			ContentType:       imageCacheContentType(mediaType),
			ImageType:         ImageTypeToString(field.imageType),
			// The item was registered with the provider URL, so it holds no
			// cached copy yet: the job has to run even when an earlier job
			// for the same source succeeded.
			RequeueSucceeded: true,
		})
	}
	return s.enqueueImageCacheJobs(ctx, "virtual media", contentID, inputs)
}

// libraryImageProviderID names the provider an artwork URL came from, so the
// cache attributes the copy the way the metadata service does.
func libraryImageProviderID(sourcePath string, providerIDs map[string]string) string {
	if provider := providerIDFromPluginURL(sourcePath); provider != "" {
		return provider
	}
	for _, key := range []string{"tmdb", "tvdb", "imdb"} {
		if providerIDs[key] != "" {
			return key
		}
	}
	return ""
}

// libraryImageProviderContentID is the provider's own id for the item, which
// the cache stores next to the copy.
func libraryImageProviderContentID(providerIDs map[string]string) string {
	for _, key := range []string{"tmdb", "tvdb", "imdb"} {
		if providerIDs[key] != "" {
			return providerIDs[key]
		}
	}
	return ""
}
