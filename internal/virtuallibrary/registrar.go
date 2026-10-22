package virtuallibrary

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/metadata"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/monitor"
)

// catalogMonitorRegistrar adapts the catalog's transactional virtual-media
// registrar to the monitor's domain-level registration interface.
type catalogMonitorRegistrar struct {
	registrar       *catalog.VirtualMediaRegistrar
	movieLibraryID  int
	seriesLibraryID int
	artwork         libraryArtworkEnqueuer
}

// libraryArtworkEnqueuer is the metadata service's artwork-cache surface. The
// adapter calls it after a successful registration with the stored content ID,
// so jobs are keyed to the item the catalog actually committed.
type libraryArtworkEnqueuer interface {
	EnqueueLibraryItemImages(context.Context, *models.MediaItem, map[string]string, []metadata.RemoteImage) error
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
	_, err := r.RegisterWithResult(ctx, item)
	return err
}

// RegisterWithResult registers the item and reports the catalog content ID the
// registrar actually stored. The monitor persists that ID so later enrichment
// cannot move the item to a new identity.
func (r *catalogMonitorRegistrar) RegisterWithResult(ctx context.Context, item monitor.MonitoredMedia) (string, error) {
	if r == nil || r.registrar == nil {
		return "", errors.New("virtual catalog registrar is unavailable")
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
	var (
		result *catalog.VirtualMediaResult
		err    error
	)
	if contentID := strings.TrimSpace(item.CatalogContentID); contentID != "" {
		result, err = r.registrar.UpsertVirtualMediaResolved(ctx, 0, contentID, payload)
	} else {
		result, err = r.registrar.Upsert(ctx, payload)
	}
	if err != nil {
		return "", err
	}
	if result == nil {
		return "", nil
	}
	return result.MediaID, nil
}

// ResolveContentID maps provider aliases onto a single existing catalog item.
func (r *catalogMonitorRegistrar) ResolveContentID(ctx context.Context, mediaType string, providerIDs map[string]string) (string, error) {
	if r == nil || r.registrar == nil {
		return "", errors.New("virtual catalog registrar is unavailable")
	}
	return r.registrar.ResolveVirtualMediaContentID(ctx, mediaType, providerIDs)
}

// ResolveLibrary validates the requested destination, or the configured
// default for the media type when the request did not name one.
func (r *catalogMonitorRegistrar) ResolveLibrary(ctx context.Context, mediaType string, requested int) (int, error) {
	if r == nil || r.registrar == nil {
		return 0, errors.New("virtual catalog registrar is unavailable")
	}
	folderID := requested
	if folderID <= 0 {
		switch mediaType {
		case "movie":
			folderID = r.movieLibraryID
		case "series":
			folderID = r.seriesLibraryID
		default:
			return 0, errors.New("destination library requires movie or series media type")
		}
	}
	if folderID <= 0 {
		return 0, fmt.Errorf("no destination library is configured for %s", mediaType)
	}
	if err := r.registrar.ValidateVirtualMediaLibrary(ctx, mediaType, folderID); err != nil {
		return 0, err
	}
	return folderID, nil
}

// EnqueueArtwork queues cache jobs for the artwork the registration stored.
// Failures are non-fatal and are logged by the monitor.
func (r *catalogMonitorRegistrar) EnqueueArtwork(ctx context.Context, contentID string, item monitor.MonitoredMedia) error {
	if r == nil || r.artwork == nil || contentID == "" {
		return nil
	}
	stored := &models.MediaItem{
		ContentID:    contentID,
		Type:         item.MediaType,
		PosterPath:   strings.TrimSpace(item.Poster),
		BackdropPath: strings.TrimSpace(item.Backdrop),
	}
	stored.PosterSourcePath = catalogArtworkSourcePath(stored.PosterPath)
	stored.BackdropSourcePath = catalogArtworkSourcePath(stored.BackdropPath)
	return r.artwork.EnqueueLibraryItemImages(ctx, stored, catalogProviderIDs(item), nil)
}

func catalogArtworkSourcePath(path string) string {
	path = strings.TrimSpace(path)
	if strings.Contains(path, "://") {
		return path
	}
	return ""
}

func catalogProviderIDs(item monitor.MonitoredMedia) map[string]string {
	ids := make(map[string]string, 3)
	if id := strings.TrimSpace(item.TMDBID); id != "" {
		ids["tmdb"] = id
	}
	if id := strings.TrimSpace(item.TVDBID); id != "" {
		ids["tvdb"] = id
	}
	if id := strings.TrimSpace(item.IMDbID); id != "" {
		ids["imdb"] = id
	}
	return ids
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
