package virtuallibrary

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Silo-Server/silo-server/internal/metadata"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/monitor"
)

const (
	mediaTypeMovie  = "movie"
	mediaTypeSeries = "series"
)

const (
	// libraryMetadataTimeout bounds enrichment inside the monitor's 45-second
	// per-item budget. Provider metadata, artwork discovery, and one resumable
	// season batch must fit here; registration keeps the remaining time.
	libraryMetadataTimeout = 30 * time.Second
	// libraryMetadataSeasonsPerCall keeps a long series resumable instead of
	// requiring a complete season traversal before anything can register.
	libraryMetadataSeasonsPerCall = 2
)

// LibraryMetadataResolver is the metadata service surface the virtual monitor
// uses. A lookup returns partial observations with an error when it could not
// finish; the caller keeps the partial result and retries from NextSeason.
type LibraryMetadataResolver interface {
	FetchLibraryMetadata(context.Context, int, *models.MediaItem, metadata.LibraryMetadataLookupOptions) (*metadata.LibraryMetadataLookupResult, error)
}

var _ LibraryMetadataResolver = (*metadata.MetadataService)(nil)
var _ libraryArtworkEnqueuer = (*metadata.MetadataService)(nil)

// SetLibraryMetadataResolver installs the destination-library enrichment path.
// A nil resolver disables it, leaving the monitor's legacy metadata path.
func (s *Service) SetLibraryMetadataResolver(resolver LibraryMetadataResolver) {
	if resolver == nil {
		s.Monitor.SetMetadataResolver(nil)
		if s.registrarAdapter != nil {
			s.registrarAdapter.artwork = nil
		}
		return
	}
	if s.registrarAdapter != nil {
		if enqueuer, ok := resolver.(libraryArtworkEnqueuer); ok {
			s.registrarAdapter.artwork = enqueuer
		}
	}
	logger := s.logger
	if logger == nil {
		logger = slog.Default()
	}
	s.Monitor.SetMetadataResolver(func(ctx context.Context, item monitor.MonitoredMedia) (monitor.MonitoredMedia, error) {
		return s.enrichFromLibraryMetadata(ctx, logger, resolver, item)
	})
}

// enrichFromLibraryMetadata applies whatever the destination library could
// resolve. Enrichment is optional: provider failures and partial traversals
// preserve existing metadata, episodes, and the resumable season cursor.
// Identity and release eligibility are evaluated by the monitor afterwards.
func (s *Service) enrichFromLibraryMetadata(
	ctx context.Context,
	logger *slog.Logger,
	resolver LibraryMetadataResolver,
	item monitor.MonitoredMedia,
) (monitor.MonitoredMedia, error) {
	folderID := item.MediaFolderID
	if folderID <= 0 {
		// The monitor resolves the destination before enrichment. Retain the
		// service defaults as a fallback for callers that predate that step.
		if item.MediaType == mediaTypeSeries {
			folderID = s.cfg.SeriesLibraryID
		} else {
			folderID = s.cfg.MovieLibraryID
		}
	}
	if folderID <= 0 {
		return item, errors.New("library metadata requires a destination library")
	}
	metadataCtx, cancel := context.WithTimeout(ctx, libraryMetadataTimeout)
	defer cancel()

	lookupItem := &models.MediaItem{
		ContentID:    item.CatalogContentID,
		Type:         item.MediaType,
		Title:        item.Title,
		Year:         int(item.Year),
		TmdbID:       item.TMDBID,
		TvdbID:       item.TVDBID,
		ImdbID:       item.IMDbID,
		PosterPath:   item.Poster,
		BackdropPath: item.Backdrop,
	}
	// Each call stays bounded so a long provider traversal remains
	// resumable, but registration needs the whole snapshot. Walk the
	// remaining season cursor here and merge every page into the item.
	afterSeason := item.MetadataNextSeason
	for {
		result, lookupErr := resolver.FetchLibraryMetadata(metadataCtx, folderID, lookupItem, metadata.LibraryMetadataLookupOptions{
			AfterSeason:       afterSeason,
			MaxSeasons:        libraryMetadataSeasonsPerCall,
			PreferredProvider: item.MetadataSource,
		})
		if result != nil {
			applyLibraryMetadataResult(&item, result, folderID)
		}
		if lookupErr != nil {
			if metadataCtx.Err() != nil {
				return item, metadataCtx.Err()
			}
			// Provider failure is degraded, not fatal: known metadata and
			// episodes, including those merged by earlier calls, remain
			// usable and the caller falls back to public providers.
			logger.WarnContext(ctx, "virtual library metadata incomplete; falling back to public providers",
				"key", item.Key, "folder_id", folderID, "error", lookupErr)
			return item, lookupErr
		}
		if result == nil || result.Metadata == nil {
			return item, metadata.ErrMetadataNotFound
		}
		if result.Complete || result.NextSeason <= afterSeason {
			// Complete ends the traversal. A cursor that did not advance
			// also ends it so a provider repeating a season cannot loop.
			return item, nil
		}
		afterSeason = result.NextSeason
		if err := metadataCtx.Err(); err != nil {
			return item, err
		}
	}
}

func applyLibraryMetadataResult(item *monitor.MonitoredMedia, result *metadata.LibraryMetadataLookupResult, folderID int) {
	if item == nil || result == nil {
		return
	}
	applyMetadataResultFields(item, result.Metadata)
	poster := strings.TrimSpace(result.PosterPath)
	if poster == "" && result.Metadata != nil {
		poster = strings.TrimSpace(result.Metadata.PosterPath)
	}
	if poster != "" {
		item.Poster = poster
	}
	posterSource := strings.TrimSpace(result.PosterSourcePath)
	if posterSource == "" {
		posterSource = poster
	}
	if posterSource != "" {
		item.PosterSourcePath = posterSource
	}

	backdrop := strings.TrimSpace(result.BackdropPath)
	if backdrop == "" && result.Metadata != nil {
		backdrop = strings.TrimSpace(result.Metadata.BackdropPath)
	}
	if backdrop != "" {
		item.Backdrop = backdrop
	}
	backdropSource := strings.TrimSpace(result.BackdropSourcePath)
	if backdropSource == "" {
		backdropSource = backdrop
	}
	if backdropSource != "" {
		item.BackdropSourcePath = backdropSource
	}
	if item.MediaType == mediaTypeSeries {
		// Episodes are merged by coordinate. A partial lookup must never
		// delete an episode that an earlier lookup or the catalog already
		// knows about, and Complete does not authorize deletion either.
		item.Episodes = monitor.MergeEpisodes(item.Episodes, libraryEpisodes(result.Episodes))
	}
	item.MediaFolderID = folderID
	item.MetadataNextSeason = result.NextSeason
	item.MetadataComplete = result.Complete
}

func applyMetadataResultFields(item *monitor.MonitoredMedia, md *metadata.MetadataResult) {
	if md == nil {
		return
	}
	if title := monitor.CleanMetadataText(md.Title); title != "" {
		item.Title = title
	}
	if overview := monitor.CleanMetadataSummary(md.Overview); overview != "" {
		item.Overview = overview
	}
	if md.Runtime > 0 {
		item.Runtime = md.Runtime
	}
	if len(md.Genres) > 0 {
		var genres []string
		for _, genre := range md.Genres {
			if cleaned := monitor.CleanMetadataText(genre); cleaned != "" {
				genres = append(genres, cleaned)
			}
		}
		if len(genres) > 0 {
			item.Genres = genres
		}
	}
	if item.TMDBID == "" {
		item.TMDBID = strings.TrimSpace(md.ProviderIDs["tmdb"])
	}
	if item.TVDBID == "" {
		item.TVDBID = strings.TrimSpace(md.ProviderIDs["tvdb"])
	}
	if item.IMDbID == "" {
		item.IMDbID = strings.TrimSpace(md.ProviderIDs["imdb"])
	}
}

func libraryEpisodes(episodes []metadata.EpisodeResult) []monitor.VirtualEpisode {
	fresh := make([]monitor.VirtualEpisode, 0, len(episodes))
	for _, episode := range episodes {
		if episode.SeasonNumber <= 0 || episode.EpisodeNumber <= 0 {
			continue
		}
		title := monitor.CleanMetadataText(episode.Title)
		if title == "" {
			title = fmt.Sprintf("Episode %d", episode.EpisodeNumber)
		}
		released, ok := parseLibraryAirDate(episode.AirDate)
		if !ok {
			// The lookup already rejects malformed dates. Skip a row that
			// still slips through rather than failing usable siblings.
			continue
		}
		fresh = append(fresh, monitor.VirtualEpisode{
			Season:    episode.SeasonNumber,
			Episode:   episode.EpisodeNumber,
			Title:     title,
			Overview:  monitor.CleanMetadataSummary(episode.Overview),
			Runtime:   episode.Runtime,
			Thumbnail: strings.TrimSpace(episode.StillPath),
			Released:  released,
		})
	}
	return fresh
}

func parseLibraryAirDate(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, true
	}
	if released, err := time.Parse(time.DateOnly, value); err == nil {
		return released, true
	}
	if released, err := time.Parse(time.RFC3339, value); err == nil {
		return released, true
	}
	return time.Time{}, false
}
