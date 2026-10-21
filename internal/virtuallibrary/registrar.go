package virtuallibrary

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/monitor"
)

// catalogMonitorRegistrar adapts the catalog's transactional virtual-media
// registrar to the monitor's domain-level registration interface.
type catalogMonitorRegistrar struct {
	registrar       *catalog.VirtualMediaRegistrar
	movieLibraryID  int
	seriesLibraryID int
	artworkCache    monitor.VirtualMediaArtworkCache
}

var _ monitor.MediaRegistrar = (*catalogMonitorRegistrar)(nil)
var _ interface {
	Reconcile(context.Context, string, []string, []int, monitor.ReconcileEvidence) error
} = (*catalogMonitorRegistrar)(nil)

func virtualURIForMonitored(item monitor.MonitoredMedia) string {
	streamID := strings.ReplaceAll(item.StreamID, ":", "/")
	if item.MediaType == "movie" && streamID != "" {
		return "virtual://movie/" + streamID
	}
	return ""
}

func virtualEpisodeURI(item monitor.MonitoredMedia, ep monitor.VirtualEpisode) string {
	if ep.Season <= 0 || ep.Episode <= 0 {
		return ""
	}
	if item.IMDbID != "" {
		return fmt.Sprintf("virtual://series/%s/%d/%d", item.IMDbID, ep.Season, ep.Episode)
	}
	if item.TVDBID != "" {
		return fmt.Sprintf("virtual://series/tvdb/%s/%d/%d", item.TVDBID, ep.Season, ep.Episode)
	}
	if item.TMDBID != "" {
		return fmt.Sprintf("virtual://series/tmdb/%s/%d/%d", item.TMDBID, ep.Season, ep.Episode)
	}
	streamID := strings.ReplaceAll(item.StreamID, ":", "/")
	if streamID != "" {
		return fmt.Sprintf("virtual://series/%s/%d/%d", streamID, ep.Season, ep.Episode)
	}
	return ""
}

func (r *catalogMonitorRegistrar) Register(ctx context.Context, item monitor.MonitoredMedia) error {
	if r == nil || r.registrar == nil {
		return fmt.Errorf("virtual catalog registrar is unavailable")
	}
	folderID := item.MediaFolderID
	if folderID <= 0 {
		if item.MediaType == "movie" {
			folderID = r.movieLibraryID
		} else if item.MediaType == "series" {
			folderID = r.seriesLibraryID
		}
	}
	var libraryIDStr string
	if folderID > 0 {
		libraryIDStr = strconv.Itoa(folderID)
	}

	payload := virtualMediaRegistration(item, libraryIDStr)
	// Resolve the catalog identity through the item's provider aliases. An
	// enriched registration carries provider ids the item did not have when
	// it was first registered, and those ids are aliases of the existing
	// item rather than an instruction to create a second one.
	contentID, err := r.registrar.ResolveVirtualMediaContentID(ctx, item.MediaType, item.TMDBID, item.TVDBID, item.IMDbID)
	if err != nil {
		return fmt.Errorf("resolve virtual media identity: %w", err)
	}
	if contentID != "" {
		payload.ContentID = contentID
	}
	if _, err := r.registrar.Upsert(ctx, payload); err != nil {
		return err
	}
	r.primeArtworkCache(ctx, contentID, item)
	return nil
}

// primeArtworkCache queues the image cache jobs for a registered item, so the
// artwork a provider offered is stored locally when image caching is on. It
// runs after the registration, because the cached copy replaces the provider
// URL on the catalog row.
func (r *catalogMonitorRegistrar) primeArtworkCache(ctx context.Context, contentID string, item monitor.MonitoredMedia) {
	if r == nil || r.artworkCache == nil || contentID == "" {
		return
	}
	providerIDs := map[string]string{}
	if item.TMDBID != "" {
		providerIDs["tmdb"] = item.TMDBID
	}
	if item.TVDBID != "" {
		providerIDs["tvdb"] = item.TVDBID
	}
	if item.IMDbID != "" {
		providerIDs["imdb"] = item.IMDbID
	}
	if err := r.artworkCache.EnqueueVirtualMediaArtwork(ctx, contentID, item.MediaType, providerIDs, item.Poster, item.Backdrop); err != nil {
		// The artwork stays on the provider, so a cache failure is not a
		// registration failure.
		slog.WarnContext(ctx, "queue virtual media artwork cache", "component", "virtuallibrary", "content_id", contentID, "error", err)
	}
}

// virtualMediaRegistration maps a monitored item onto the catalog payload.
//
// Metadata providers can return surrounding whitespace in titles, overviews,
// artwork paths, and genres. The catalog validator rejects those values
// (ErrInvalidVirtualMedia, "must not have surrounding whitespace"), so clean
// them here — the boundary between provider data and the strict validator —
// instead of relaxing validation.
func virtualMediaRegistration(item monitor.MonitoredMedia, libraryID string) catalog.VirtualMedia {
	return catalog.VirtualMedia{
		MediaType:      item.MediaType,
		LibraryID:      libraryID,
		IMDbID:         item.IMDbID,
		TMDBID:         item.TMDBID,
		TVDBID:         item.TVDBID,
		Title:          strings.TrimSpace(item.Title),
		Year:           int(item.Year),
		Overview:       strings.TrimSpace(item.Overview),
		PosterPath:     strings.TrimSpace(item.Poster),
		BackdropPath:   strings.TrimSpace(item.Backdrop),
		Genres:         trimVirtualStrings(item.Genres),
		RuntimeMinutes: item.Runtime,
		Source:         strings.TrimSpace(item.SourceKey),
		VirtualURI:     virtualURIForMonitored(item),
		Episodes:       monitoredEpisodePayload(item),
		Variants:       make([]catalog.VirtualMediaVariant, 0),
	}
}

// trimVirtualStrings trims each element while preserving nil-vs-empty so the
// payload shape is unchanged for callers that distinguish the two.
func trimVirtualStrings(values []string) []string {
	if values == nil {
		return nil
	}
	trimmed := make([]string, len(values))
	for i, value := range values {
		trimmed[i] = strings.TrimSpace(value)
	}
	return trimmed
}

// monitoredEpisodePayload maps monitor episodes onto the catalog payload.
//
// Season 0 is the provider's specials bucket and the catalog rejects
// non-positive season/episode coordinates (see catalog.validateVirtualEpisode);
// a single special must not fail the whole series registration, so specials are
// omitted here. Genuinely malformed input (a negative season, a missing field)
// is intentionally preserved so catalog validation still rejects it.
//
// Provider text fields are trimmed here for the same reason as the series-level
// fields in virtualMediaRegistration: the catalog validator rejects surrounding
// whitespace and must stay strict.
func monitoredEpisodePayload(item monitor.MonitoredMedia) []catalog.VirtualEpisode {
	episodes := make([]catalog.VirtualEpisode, 0, len(item.Episodes))
	for _, episode := range item.Episodes {
		if episode.Season == 0 {
			continue
		}
		episodes = append(episodes, catalog.VirtualEpisode{
			SeasonNumber:   episode.Season,
			EpisodeNumber:  episode.Episode,
			Title:          strings.TrimSpace(episode.Title),
			Overview:       strings.TrimSpace(episode.Overview),
			StillPath:      strings.TrimSpace(episode.Thumbnail),
			AirDate:        episode.Released,
			RuntimeMinutes: episode.Runtime,
			VirtualURI:     virtualEpisodeURI(item, episode),
		})
	}
	return episodes
}

// MissingVirtualMedia reports which of the given content IDs no longer exist
// in the catalog, so the monitor can evict queue entries whose media was
// genuinely removed.
func (r *catalogMonitorRegistrar) MissingVirtualMedia(ctx context.Context, contentIDs []string) (map[string]struct{}, error) {
	if r == nil || r.registrar == nil {
		return nil, fmt.Errorf("virtual catalog registrar is unavailable")
	}
	return r.registrar.MissingVirtualMediaContentIDs(ctx, contentIDs)
}

func (r *catalogMonitorRegistrar) Reconcile(ctx context.Context, source string, keepIDs []string, libraryIDs []int, evidence monitor.ReconcileEvidence) error {
	if r == nil || r.registrar == nil {
		return fmt.Errorf("virtual catalog registrar is unavailable")
	}
	if len(libraryIDs) == 0 {
		if r.movieLibraryID > 0 {
			libraryIDs = append(libraryIDs, r.movieLibraryID)
		}
		if r.seriesLibraryID > 0 {
			libraryIDs = append(libraryIDs, r.seriesLibraryID)
		}
	}
	_, err := r.registrar.ReconcileVirtualMediaVerified(ctx, 0, source, keepIDs, libraryIDs, catalog.VirtualReconcileEvidence{
		FullCycle:   evidence.FullCycle,
		SourceCount: evidence.SourceCount,
		QueueCount:  evidence.QueueCount,
	})
	if errors.Is(err, catalog.ErrVirtualReconcileRefused) {
		// Keep the catalog's source and counts in the message while preserving
		// the refusal identity so the monitor can log it loudly instead of
		// treating it as an ordinary retryable failure.
		return fmt.Errorf("%w: %w", monitor.ErrReconcileRefused, err)
	}
	return err
}
