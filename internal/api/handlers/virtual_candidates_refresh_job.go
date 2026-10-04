package handlers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Silo-Server/silo-server/internal/adminjob"
	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/events"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/notifications"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/prowlarr"
)

const (
	// virtualIndexerSearchBudget bounds the on-demand indexer search. It is
	// deliberately smaller than one provider request: the search is best-effort
	// and degrades to an altmount-only result rather than holding the job.
	virtualIndexerSearchBudget = 10 * time.Second
	// virtualEnrichmentBudget bounds the whole probe pass. Every new candidate
	// is attempted before the job completes unless this deadline fires first.
	virtualEnrichmentBudget = 4 * time.Minute
	// contentIDKey is the shared log/serialization key for a content id. It is
	// also the JSON field name every catalog event carries.
	contentIDKey = "content_id"
	// episodeIDKey is the shared log/serialization key for an episode id.
	episodeIDKey = "episode_id"
)

// VirtualCandidatesRefreshJobService accepts the asynchronous "Refresh List":
// it validates access through the same watch-detail path the version list uses,
// resolves the item's indexer-search identity once, and queues one job per
// title. A second request for an in-flight title returns the existing job; a
// refresh the caller owns is canceled through CancelRefreshJob instead.
type VirtualCandidatesRefreshJobService struct {
	// Detail validates the target and the caller's access.
	Detail VirtualCandidatesDetailReader
	// Items / Episodes resolve the indexer-search identity.
	Items interface {
		GetByID(ctx context.Context, contentID string) (*models.MediaItem, error)
	}
	Episodes interface {
		GetByID(ctx context.Context, contentID string) (*models.Episode, error)
	}
	// Files resolves the item's media files so the request can name a media
	// folder. Nil disables the lookup; the job resolves the folder from the
	// provider sources instead.
	ContentFiles VirtualCandidateFiles
	EpisodeFiles VirtualCandidateFiles
	// Jobs creates the durable refresh job.
	Jobs *adminjob.Repository
	// CancelRegistry bridges a running refresh's cancellation to its in-process
	// context on this node. Nil (or a job owned by another node) still cancels
	// durably through the job row; the owning runner observes CancelRequested.
	CancelRegistry *adminjob.CancelRegistry
}

// CreateRefreshJob validates the request, builds the job payload, and queues it.
// An in-flight job for the same title is returned as-is so the caller answers
// 202 with the existing job rather than a conflict. Cancellation of an owned
// in-flight refresh is the separate CancelRefreshJob command, so a retried
// start never stops the work it started.
func (s *VirtualCandidatesRefreshJobService) CreateRefreshJob(ctx context.Context, userID int, profileID, contentID string, filter catalog.AccessFilter) (*models.AdminJob, error) {
	if s == nil || s.Jobs == nil || s.Detail == nil {
		return nil, apiError(503, "unavailable", "Virtual candidates refresh is unavailable")
	}
	if _, err := s.Detail.WatchDetail(ctx, userID, profileID, contentID, filter); err != nil {
		return nil, err
	}
	req := adminjob.VirtualCandidatesRefreshRequest{
		ContentID:     contentID,
		UserID:        userID,
		ProfileID:     profileID,
		MediaFolderID: s.firstFolderID(ctx, contentID),
	}
	if episode, err := s.episodeFor(ctx, contentID); err == nil && episode != nil {
		req.EpisodeID = episode.ContentID
		req.SeasonNumber = episode.SeasonNumber
		req.EpisodeNumber = episode.EpisodeNumber
		// An indexer search for an episode queries the series title; release
		// names carry the series name, not the episode name. Fall back to the
		// episode row's own external ids when the series cannot be resolved.
		req.IMDbID = episode.ImdbID
		req.TMDBID = episode.TmdbID
		req.TVDBID = episode.TvdbID
		if series, seriesErr := s.itemFor(ctx, episode.SeriesID); seriesErr == nil && series != nil {
			req.Title = series.Title
			req.Year = int32(series.Year)
			if series.ImdbID != "" {
				req.IMDbID = series.ImdbID
			}
			if series.TmdbID != "" {
				req.TMDBID = series.TmdbID
			}
			if series.TvdbID != "" {
				req.TVDBID = series.TvdbID
			}
		}
	} else if item, err := s.itemFor(ctx, contentID); err == nil && item != nil {
		req.Title = item.Title
		req.Year = int32(item.Year)
		req.IMDbID = item.ImdbID
		req.TMDBID = item.TmdbID
		req.TVDBID = item.TvdbID
	}
	job, err := s.Jobs.CreateVirtualCandidatesRefresh(ctx, userID, req, "Queued virtual candidates refresh")
	if err != nil {
		var conflict *adminjob.ActiveJobConflictError
		if errors.As(err, &conflict) && conflict.Job != nil {
			return conflict.Job, nil
		}
		return nil, apiError(500, "internal_error", "Failed to queue the refresh")
	}
	return job, nil
}

// CancelRefreshJob cancels the caller's in-flight refresh for a title. It is the
// second press of a locked Refresh control: the job is canceled (queued) or
// marked for cancellation (running) and returned so the control can unlock.
// Cancellation is non-destructive to candidates already persisted, and the
// automatic re-listing intervals are untouched. A refresh owned by another
// account is refused, preserving the refusal semantics of a second press that a
// caller has no authority to act on.
func (s *VirtualCandidatesRefreshJobService) CancelRefreshJob(ctx context.Context, userID int, profileID, contentID string, filter catalog.AccessFilter) (*models.AdminJob, error) {
	if s == nil || s.Jobs == nil || s.Detail == nil {
		return nil, apiError(503, "unavailable", "Virtual candidates refresh is unavailable")
	}
	if _, err := s.Detail.WatchDetail(ctx, userID, profileID, contentID, filter); err != nil {
		return nil, err
	}
	active, err := s.Jobs.GetActiveVirtualRefreshByContentID(ctx, contentID)
	if err != nil {
		if errors.Is(err, adminjob.ErrJobNotFound) {
			return nil, apiError(409, "not_cancellable", "There is no refresh to cancel")
		}
		return nil, apiError(500, "internal_error", "Failed to load the refresh")
	}
	if active.CreatedByUserID != userID {
		return nil, apiError(403, "forbidden", "This refresh belongs to another account")
	}
	expiresAt := time.Now().UTC().Add(7 * 24 * time.Hour)
	switch active.Status {
	case adminjob.StatusQueued:
		// A queued job releases its unique active-job lock the moment it turns
		// terminal, so cancel it here rather than waiting for the runner to
		// claim and acknowledge it.
		canceled, cancelErr := s.Jobs.CancelQueued(ctx, active.ID, "Virtual candidates refresh canceled", expiresAt)
		if cancelErr == nil {
			return canceled, nil
		}
		if !errors.Is(cancelErr, adminjob.ErrJobNotCancellable) {
			return nil, apiError(500, "internal_error", "Failed to cancel the refresh")
		}
		// The job advanced to running between the lookup and the cancel; fall
		// through to the running path.
	case adminjob.StatusRunning:
	default:
		return nil, apiError(409, "not_cancellable", "The refresh is no longer running")
	}
	updated, err := s.Jobs.RequestCancellation(ctx, active.ID)
	if err != nil {
		if errors.Is(err, adminjob.ErrJobNotCancellable) || errors.Is(err, adminjob.ErrJobNotFound) {
			return nil, apiError(409, "not_cancellable", "The refresh is no longer running")
		}
		return nil, apiError(500, "internal_error", "Failed to cancel the refresh")
	}
	// Cancel the in-process context promptly on this node; a job running on
	// another node observes the durable CancelRequested through its runner.
	if s.CancelRegistry != nil {
		s.CancelRegistry.Cancel(active.ID)
	}
	return updated, nil
}

func (s *VirtualCandidatesRefreshJobService) itemFor(ctx context.Context, contentID string) (*models.MediaItem, error) {
	if s.Items == nil {
		return nil, nil
	}
	return s.Items.GetByID(ctx, contentID)
}

func (s *VirtualCandidatesRefreshJobService) episodeFor(ctx context.Context, contentID string) (*models.Episode, error) {
	if s.Episodes == nil {
		return nil, nil
	}
	return s.Episodes.GetByID(ctx, contentID)
}

func (s *VirtualCandidatesRefreshJobService) firstFolderID(ctx context.Context, contentID string) int {
	for _, lookup := range []VirtualCandidateFiles{s.ContentFiles, s.EpisodeFiles} {
		if lookup == nil {
			continue
		}
		if files, err := lookup(ctx, contentID); err == nil {
			for _, file := range files {
				if file != nil && file.MediaFolderID > 0 {
					return file.MediaFolderID
				}
			}
		}
	}
	return 0
}

// IndexerReleaseSearcher is the on-demand indexer lookup the refresh job runs.
// It is a narrow slice so the executor can be unit-tested with a fake.
type IndexerReleaseSearcher interface {
	SearchMonitoredReleases(ctx context.Context, item virtuallibrary.MonitoredMedia, episode *virtuallibrary.VirtualEpisode) ([]virtuallibrary.SearchItem, error)
}

// VirtualCandidateEnricher probes a batch of freshly persisted candidates and
// returns how many were attempted. Implemented by *PlaybackHandler.
type VirtualCandidateEnricher interface {
	EnrichVirtualCandidates(ctx context.Context, contentID, episodeID string, userID int, profileID string, streams []VirtualPlaybackStream) int
}

// VirtualCandidatesRefreshExecutor runs the asynchronous refresh pipeline:
// provider re-list and persist first (fatal on failure), then a best-effort
// indexer search whose failure degrades to an altmount-only result, then a
// best-effort probe pass over the new candidates, then a cleanup pass that
// prunes dead candidates the fresh listing no longer offers, and finally a
// catalog invalidation so every client reloads the version list.
type VirtualCandidatesRefreshExecutor struct {
	// Refresh performs step (a): force-list and persist the provider candidates.
	Refresh *VirtualCandidatesRefreshService
	// ProviderRefresher forces a fresh provider classification snapshot before
	// the listing so the resolver classifies against current AltMount
	// completed/failed state, not a cached snapshot. Nil skips it; a failure
	// is warned and the job continues (classification then uses the snapshot
	// the resolver already has).
	ProviderRefresher VirtualProviderStateRefresher
	// Pruner deletes persisted candidate rows that are both absent from the
	// fresh listing and dead, while honoring the same retention keep-list the
	// re-list sweep uses. Nil skips it.
	Pruner VirtualCandidatePruner
	// ReleaseDownloading reports whether AltMount is actively fetching a
	// release, for the pending-candidate count. Nil skips the count. An
	// unconfigured provider (known=false) never marks a release pending.
	ReleaseDownloading func(releaseName string) (downloading bool, known bool)
	// Store persists the indexer-only releases (step c).
	Store *virtuallibrary.IndexerReleaseStore
	// Searcher is the on-demand Prowlarr lookup (step b). Nil skips the search.
	Searcher IndexerReleaseSearcher
	// Enricher probes the new candidates (step d). Nil skips enrichment.
	Enricher VirtualCandidateEnricher
	// Events publishes the catalog invalidation (step e). Nil skips it.
	Events *notifications.Hub
	Logger *slog.Logger

	searchBudget time.Duration
}

// VirtualProviderStateRefresher forces a fresh virtual provider classification
// snapshot (AltMount's completed/failed history) before the refresh lists. It is
// implemented by *virtuallibrary.Service.
type VirtualProviderStateRefresher interface {
	RefreshProviderState(ctx context.Context) error
}

// VirtualCandidatePruner removes persisted provider-candidate rows that the
// fresh listing no longer offers and that are dead, without touching any row
// the sweep's retention keeps. It is implemented by *CandidateDeadPruner.
type VirtualCandidatePruner interface {
	PruneDeadAbsentCandidates(ctx context.Context, contentID, episodeID string, sources []*models.MediaFile, fresh []VirtualPlaybackStream) (int, error)
}

// Execute runs the pipeline. It returns an error only for a non-degradable
// failure: a provider listing that could not be persisted. A provider-state
// refresh, indexer search, probe failure, or candidate prune is a warning,
// never a failed job.
func (e *VirtualCandidatesRefreshExecutor) Execute(ctx context.Context, req adminjob.VirtualCandidatesRefreshRequest, progress func(current, total int, message string)) (*adminjob.VirtualCandidatesRefreshResult, error) {
	if e == nil || e.Refresh == nil {
		return nil, errors.New("virtual candidates refresh executor is not configured")
	}
	logger := e.logger()
	report := func(current, total int, message string) {
		if progress != nil {
			progress(current, total, message)
		}
	}
	const totalSteps = 6

	// Force the provider's authoritative completed/failed snapshot current
	// before the listing. Classification runs on every serve, so a stale
	// history would let a release that just failed (or just completed) be
	// persisted under the old verdict for up to one refresh interval.
	report(0, totalSteps, "Refreshing provider state")
	if e.ProviderRefresher != nil {
		if err := e.ProviderRefresher.RefreshProviderState(ctx); err != nil {
			logger.WarnContext(ctx, "virtual candidates refresh: provider state refresh failed; classifying against cached state",
				"component", "api", contentIDKey, req.ContentID, "error", err)
		}
	}

	report(1, totalSteps, "Listing provider candidates")
	sources, streams, err := e.Refresh.RefreshSources(ctx, req.ContentID, req.UserID, req.ProfileID)
	if err != nil {
		return nil, fmt.Errorf("list provider candidates: %w", err)
	}
	result := &adminjob.VirtualCandidatesRefreshResult{
		ContentID:          req.ContentID,
		EpisodeID:          req.EpisodeID,
		ProviderCandidates: len(streams),
	}

	report(2, totalSteps, "Searching indexers")
	indexerReleases, searchOK := e.searchIndexerReleases(ctx, req)
	result.IndexerSearchOK = searchOK

	report(3, totalSteps, "Persisting indexer releases")
	persisted, err := e.persistIndexerReleases(ctx, req, sources, streams, indexerReleases)
	if err != nil {
		return nil, fmt.Errorf("persist indexer releases: %w", err)
	}
	result.IndexerReleases = len(persisted)
	result.Releases = persisted

	// Count freshly listed candidates AltMount is still fetching: these are
	// the releases playback will wait for rather than skip. Best-effort and
	// informational only. Counted per release, not per variant: one release
	// with several result= variants is one pending release.
	if e.ReleaseDownloading != nil {
		seen := make(map[string]struct{})
		for _, stream := range streams {
			name := strings.TrimSpace(stream.ProviderReleaseName)
			if name == "" {
				continue
			}
			key := strings.ToLower(name)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			if downloading, known := e.ReleaseDownloading(name); known && downloading {
				result.PendingCandidates++
			}
		}
	}

	report(4, totalSteps, "Enriching provider candidates")
	if e.Enricher != nil && len(streams) > 0 {
		result.Enriched = e.Enricher.EnrichVirtualCandidates(ctx, req.ContentID, req.EpisodeID, req.UserID, req.ProfileID, streams)
	}

	// Prune after persistence: a row the fresh listing dropped is now provably
	// absent, so the only question left is whether it is dead and unprotected.
	// A prune failure is cleanup-only and must not fail an otherwise good
	// refresh; the next run retries it.
	report(5, totalSteps, "Pruning dead candidates")
	if e.Pruner != nil {
		pruned, pruneErr := e.Pruner.PruneDeadAbsentCandidates(ctx, req.ContentID, req.EpisodeID, sources, streams)
		if pruneErr != nil {
			logger.WarnContext(ctx, "virtual candidates refresh: dead-candidate prune failed",
				"component", "api", contentIDKey, req.ContentID, episodeIDKey, req.EpisodeID, "error", pruneErr)
		} else {
			result.PrunedCandidates = pruned
		}
	}

	// Publish only after persistence: a client that reloads on the event must
	// read the new rows, not the pre-refresh list.
	e.publishVersionsUpdated(ctx, req.ContentID)

	report(totalSteps, totalSteps, "Refresh complete")
	logger.InfoContext(ctx, "virtual candidates refresh complete",
		"component", "api", contentIDKey, req.ContentID, episodeIDKey, req.EpisodeID,
		"provider_candidates", result.ProviderCandidates, "indexer_releases", result.IndexerReleases,
		"indexer_search_ok", result.IndexerSearchOK, "enriched", result.Enriched,
		"pruned_candidates", result.PrunedCandidates)
	return result, nil
}

// searchIndexerReleases runs the on-demand search under its own budget. A
// failure or timeout is warned and reported as not-ok; the caller continues
// with the altmount-only result.
func (e *VirtualCandidatesRefreshExecutor) searchIndexerReleases(ctx context.Context, req adminjob.VirtualCandidatesRefreshRequest) ([]virtuallibrary.SearchItem, bool) {
	if e.Searcher == nil || strings.TrimSpace(req.Title) == "" {
		return nil, false
	}
	budget := e.searchBudget
	if budget <= 0 {
		budget = virtualIndexerSearchBudget
	}
	searchCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	item := virtuallibrary.MonitoredMedia{
		Key:       req.ContentID,
		MediaType: trailerItemTypeMovie,
		Title:     req.Title,
		Year:      req.Year,
		IMDbID:    req.IMDbID,
		TMDBID:    req.TMDBID,
		TVDBID:    req.TVDBID,
	}
	var episode *virtuallibrary.VirtualEpisode
	if req.SeasonNumber > 0 && req.EpisodeNumber > 0 {
		item.MediaType = trailerItemTypeSeries
		episode = &virtuallibrary.VirtualEpisode{Season: req.SeasonNumber, Episode: req.EpisodeNumber}
	}
	releases, err := e.Searcher.SearchMonitoredReleases(searchCtx, item, episode)
	if err != nil {
		e.logger().WarnContext(ctx, "virtual candidates refresh: indexer search failed; continuing altmount-only",
			"component", "api", contentIDKey, req.ContentID, "error", err)
		return nil, false
	}
	return releases, true
}

// persistIndexerReleases filters the search to usenet releases the provider
// does not already have, caps the batch, and upserts them for the item scope.
func (e *VirtualCandidatesRefreshExecutor) persistIndexerReleases(ctx context.Context, req adminjob.VirtualCandidatesRefreshRequest, sources []*models.MediaFile, streams []VirtualPlaybackStream, releases []virtuallibrary.SearchItem) ([]adminjob.IndexerReleaseResult, error) {
	if e.Store == nil {
		return nil, nil
	}
	// The scope is derived from the provider source rows, not the media_id.
	// An episode's media_id is its episode id while its virtual rows carry
	// content_id = series id and episode_id = episode id, so the source rows
	// are the only correct scope. Fall back to the request fields when no
	// source is available (the provider re-list always yields at least one).
	scope := virtuallibrary.IndexerReleaseScope{ContentID: req.ContentID, EpisodeID: req.EpisodeID, MediaFolderID: req.MediaFolderID}
	for _, source := range sources {
		if source == nil || source.ContentID == "" {
			continue
		}
		scope = virtuallibrary.IndexerReleaseScope{
			ContentID:     source.ContentID,
			EpisodeID:     source.EpisodeID,
			MediaFolderID: source.MediaFolderID,
		}
		break
	}

	deduped := make([]virtuallibrary.IndexerRelease, 0, len(releases))
	for i := range releases {
		release := releases[i]
		if !strings.EqualFold(strings.TrimSpace(release.Protocol), "usenet") {
			continue
		}
		if indexerReleaseMatchesAnyProvider(release, streams) {
			continue
		}
		deduped = append(deduped, indexerReleaseRow(release))
		if len(deduped) >= virtuallibrary.MaxIndexerReleasesPerContent {
			break
		}
	}
	if len(deduped) == 0 {
		return nil, nil
	}
	if err := e.Store.UpsertIndexerReleases(ctx, scope, deduped); err != nil {
		return nil, err
	}
	// Re-read to obtain the server-assigned row ids. The upsert keys on guid,
	// so an existing row keeps its id and a new row gets one; the wire result
	// must carry the real ids the request endpoint resolves.
	persisted, err := e.Store.ListIndexerReleases(ctx, scope)
	if err != nil {
		return nil, err
	}
	return indexerReleaseResults(persisted), nil
}

// indexerReleaseMatchesAnyProvider reports whether the provider already lists
// the release. The provider set includes every fresh listing stream, including
// badge-confirmed/cached ones (they are part of the listing), so a cached
// release is never offered as an indexer-only request.
func indexerReleaseMatchesAnyProvider(release virtuallibrary.SearchItem, streams []VirtualPlaybackStream) bool {
	for _, stream := range streams {
		name := strings.TrimSpace(stream.ProviderReleaseName)
		if name == "" {
			name = strings.TrimSpace(stream.Label)
		}
		if name == "" {
			continue
		}
		if prowlarr.IndexerReleaseMatchesProvider(release, name, stream.FileSize) {
			return true
		}
	}
	return false
}

func indexerReleaseRow(release virtuallibrary.SearchItem) virtuallibrary.IndexerRelease {
	row := virtuallibrary.IndexerRelease{
		GUID:           release.GUID,
		Title:          release.Title,
		NormalizedName: release.NormalizedTitle(),
		Protocol:       release.Protocol,
		Indexer:        release.Indexer,
		IndexerID:      release.IndexerID,
		SizeBytes:      release.Size,
		DownloadURL:    release.DownloadURL,
		EnqueueState:   virtuallibrary.IndexerReleaseStateNotDownloaded,
		ExpireAt:       time.Now().Add(virtuallibrary.IndexerReleaseTTLDefault),
	}
	score := release.QualityScore()
	row.FormatScore = &score
	if published, err := time.Parse(time.RFC3339, strings.TrimSpace(release.PublishDate)); err == nil {
		row.PublishedAt = &published
	} else if published, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(release.PublishDate)); err == nil {
		row.PublishedAt = &published
	}
	return row
}

func indexerReleaseResults(rows []virtuallibrary.IndexerRelease) []adminjob.IndexerReleaseResult {
	out := make([]adminjob.IndexerReleaseResult, 0, len(rows))
	for _, row := range rows {
		meta := row.Meta()
		out = append(out, adminjob.IndexerReleaseResult{
			ReleaseID:     fmt.Sprintf("%d", row.ID),
			Title:         row.Title,
			Resolution:    meta.Resolution,
			CodecVideo:    meta.CodecVideo,
			CodecAudio:    meta.CodecAudio,
			HDR:           meta.HDR,
			SizeBytes:     row.SizeBytes,
			Indexer:       row.Indexer,
			PublishedAt:   row.PublishedAt,
			FormatScore:   row.FormatScore,
			Protocol:      row.Protocol,
			DownloadState: virtuallibrary.IndexerReleaseStateNotDownloaded,
		})
	}
	return out
}

func (e *VirtualCandidatesRefreshExecutor) publishVersionsUpdated(ctx context.Context, contentID string) {
	if e.Events == nil {
		return
	}
	hub := e.Events.EventsHub()
	if hub == nil {
		return
	}
	if err := hub.PublishJSON(ctx, events.ChannelCatalog, "catalog.item.changed", map[string]any{
		contentIDKey: contentID,
		"change":     "versions_updated",
	}, events.PublishOptions{}); err != nil {
		e.logger().WarnContext(ctx, "virtual candidates refresh: failed to publish versions_updated",
			"component", "api", contentIDKey, contentID, "error", err)
	}
}

func (e *VirtualCandidatesRefreshExecutor) logger() *slog.Logger {
	if e == nil || e.Logger == nil {
		return slog.Default()
	}
	return e.Logger
}
