package virtuallibrary

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Silo-Server/silo-server/internal/metadata"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/monitor"
)

// LibraryMetadataResolver resolves the metadata of a virtual library item
// from the providers configured for the destination library.
//
// The seasons the caller already holds are passed in, so a refresh spends its
// budget on the seasons that are still missing instead of re-reading the
// whole history of a long-running show.
type LibraryMetadataResolver interface {
	FetchLibraryMetadata(context.Context, int, *models.MediaItem, []int) (*metadata.LibraryMetadataResult, error)
}

// SetVirtualMediaArtworkCache installs the artwork cache the registrar primes
// after a registration. It is optional: without it the artwork stays on the
// provider.
func (s *Service) SetVirtualMediaArtworkCache(cache monitor.VirtualMediaArtworkCache) {
	if s == nil || s.registrar == nil {
		return
	}
	s.registrar.artworkCache = cache
}

// SetLibraryMetadataResolver installs the library metadata path for the
// monitor. The resolver is consulted first; when it cannot answer, the
// monitor falls back to the public providers, so a request never depends on
// the stronger provider requirement alone.
func (s *Service) SetLibraryMetadataResolver(resolver LibraryMetadataResolver) {
	var imageCache monitor.VirtualMediaArtworkCache
	if cache, ok := resolver.(monitor.VirtualMediaArtworkCache); ok {
		imageCache = cache
	}
	s.SetVirtualMediaArtworkCache(imageCache)
	s.Monitor.SetMetadataResolver(s.libraryMetadataResolver(resolver))
}

// libraryMetadataResolver builds the monitor's metadata step. It resolves the
// item against the providers configured for the destination library, keeps the
// episodes the item already holds, and reports the artwork the image
// providers know about.
func (s *Service) libraryMetadataResolver(resolver LibraryMetadataResolver) func(context.Context, monitor.MonitoredMedia) (monitor.MonitoredMedia, error) {
	return func(ctx context.Context, item monitor.MonitoredMedia) (monitor.MonitoredMedia, error) {
		if resolver == nil {
			return item, fmt.Errorf("library metadata service is unavailable")
		}
		folderID := item.MediaFolderID
		if folderID <= 0 {
			if item.MediaType == "series" {
				folderID = s.cfg.SeriesLibraryID
			} else {
				folderID = s.cfg.MovieLibraryID
			}
		}
		if folderID <= 0 {
			return item, fmt.Errorf("%w: no destination library for %s", monitor.ErrMetadataNotConfigured, item.MediaType)
		}
		lookup, err := resolver.FetchLibraryMetadata(ctx, folderID, &models.MediaItem{
			Type: item.MediaType, Title: item.Title, Year: int(item.Year),
			TmdbID: item.TMDBID, TvdbID: item.TVDBID, ImdbID: item.IMDbID,
		}, knownSeasons(item.Episodes))
		if err != nil {
			if errors.Is(err, metadata.ErrMetadataNotConfigured) {
				return item, fmt.Errorf("%w: %w", monitor.ErrMetadataNotConfigured, err)
			}
			return item, err
		}
		if lookup == nil || lookup.Metadata == nil {
			return item, metadata.ErrMetadataNotFound
		}
		result := lookup.Metadata
		if title := monitor.CleanMetadataText(result.Title); title != "" {
			item.Title = title
		}
		if overview := monitor.CleanMetadataSummary(result.Overview); overview != "" {
			item.Overview = overview
		}
		if poster := strings.TrimSpace(result.PosterPath); poster != "" {
			item.Poster = poster
		} else if poster := bestLibraryImage(lookup.Images, metadata.ImagePoster); poster != "" {
			// The metadata provider carried no artwork, but a provider that
			// only answers image requests did.
			item.Poster = poster
		}
		if backdrop := strings.TrimSpace(result.BackdropPath); backdrop != "" {
			item.Backdrop = backdrop
		} else if backdrop := bestLibraryImage(lookup.Images, metadata.ImageBackdrop); backdrop != "" {
			item.Backdrop = backdrop
		}
		if result.Runtime > 0 {
			item.Runtime = result.Runtime
		}
		if len(result.Genres) > 0 {
			item.Genres = make([]string, 0, len(result.Genres))
			for _, genre := range result.Genres {
				if genre = monitor.CleanMetadataText(genre); genre != "" {
					item.Genres = append(item.Genres, genre)
				}
			}
		}
		if item.TMDBID == "" {
			item.TMDBID = result.ProviderIDs["tmdb"]
		}
		if item.TVDBID == "" {
			item.TVDBID = result.ProviderIDs["tvdb"]
		}
		if item.IMDbID == "" {
			item.IMDbID = result.ProviderIDs["imdb"]
		}
		if item.MediaType == "series" {
			fresh := make([]monitor.VirtualEpisode, 0, len(lookup.Episodes))
			for _, episode := range lookup.Episodes {
				if episode.SeasonNumber <= 0 || episode.EpisodeNumber <= 0 {
					continue
				}
				title := monitor.CleanMetadataText(episode.Title)
				if title == "" {
					// One unusable episode does not condemn the rest of the
					// series: the episode is skipped, the others stand.
					continue
				}
				released, err := time.Parse(time.DateOnly, episode.AirDate)
				if err != nil && episode.AirDate != "" {
					released, err = time.Parse(time.RFC3339, episode.AirDate)
					if err != nil {
						continue
					}
				}
				fresh = append(fresh, monitor.VirtualEpisode{
					Season: episode.SeasonNumber, Episode: episode.EpisodeNumber,
					Title: title, Overview: monitor.CleanMetadataSummary(episode.Overview),
					Runtime: episode.Runtime, Thumbnail: episode.StillPath, Released: released,
				})
			}
			if len(fresh) == 0 {
				return item, metadata.ErrMetadataNotFound
			}
			// The lookup answered only the seasons it could reach, so the
			// episodes the item already holds stay in place.
			item.Episodes = monitor.MergeSeriesEpisodeMetadata(item.Episodes, fresh)
		}
		item.MediaFolderID = folderID
		return item, nil
	}
}

// knownSeasons lists the seasons the item already holds episodes for, so the
// lookup can spend its budget on the seasons that are still missing.
func knownSeasons(episodes []monitor.VirtualEpisode) []int {
	seen := make(map[int]struct{}, len(episodes))
	seasons := make([]int, 0, len(episodes))
	for _, episode := range episodes {
		if episode.Season <= 0 {
			continue
		}
		if _, duplicate := seen[episode.Season]; duplicate {
			continue
		}
		seen[episode.Season] = struct{}{}
		seasons = append(seasons, episode.Season)
	}
	return seasons
}

// bestLibraryImage returns the highest rated image of the requested type.
func bestLibraryImage(images []metadata.RemoteImage, imageType metadata.ImageType) string {
	best := ""
	bestRating := 0.0
	for _, image := range images {
		if image.Type != imageType {
			continue
		}
		url := strings.TrimSpace(image.URL)
		if url == "" {
			continue
		}
		if best == "" || image.Rating > bestRating {
			best = url
			bestRating = image.Rating
		}
	}
	return best
}
