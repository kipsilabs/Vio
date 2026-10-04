package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
)

// CatalogResourceHandler serves canonical catalog resource read routes.
type CatalogResourceHandler struct {
	items *ItemsHandler
	// FileResolver looks up a media file row by ID for the batched version
	// liveness check. Optional: without it the check reports every file as
	// unavailable.
	FileResolver FilePathResolver
	// VirtualResolver resolves a virtual candidate's pinned result through the
	// provider for the liveness check. Optional: without it virtual rows are
	// reported from their durable failed_at stamp only and never re-stamped.
	VirtualResolver VirtualMediaDetailedResolver
	// MarkVirtualFailed stamps a virtual candidate row as known-bad after a
	// confirmed dead pin. Optional: without it the check never stamps.
	MarkVirtualFailed func(ctx context.Context, fileID int, expectedFilePath string, observedFailedAt *time.Time) error
	// ClearVirtualFailed clears a virtual candidate's failed stamp after a
	// successful resolution. Optional: without it the check never clears.
	ClearVirtualFailed func(ctx context.Context, fileID int, expectedFilePath string, observedFailedAt *time.Time) error
	// VirtualFileMetadataSaver and VirtualFileSaver adopt a same-release
	// candidate the resolver re-identified under a new provider result id, via
	// the same CAS-fenced Phase-1 write the playback path uses. Optional:
	// without them the check still treats a rematch as live and leaves the
	// adoption to the next playback resolve.
	VirtualFileMetadataSaver VirtualFileMetadataSaver
	VirtualFileSaver         VirtualFileSaver
	// ItemAccess authorizes a file's parent item for the requesting profile
	// before the liveness check resolves or stamps anything. Optional: without
	// it every file is reported unavailable, indistinguishable from unknown.
	ItemAccess PlaybackItemAccessChecker
	// EpisodeLookup and ExtraLookup resolve the parent of episode and extra
	// files so they authorize through their series/item, mirroring the
	// playback handler's loadAuthorizedFile. Optional: without them episode
	// and extra files are denied.
	EpisodeLookup PlaybackEpisodeLookup
	ExtraLookup   PlaybackExtraLookup

	watchlistPromoter WatchlistItemPromoter
}

// WatchlistItemPromoter moves the profile's watchlist entry for a title the
// library did not have onto the library watchlist once the item carries the
// title's IDs. *watchlist.Titles implements it; a failure is its to log.
type WatchlistItemPromoter interface {
	PromoteWatchlistItem(ctx context.Context, access catalog.AccessFilter, contentID string)
}

// SetWatchlistPromoter makes item detail promote the viewer's matching
// watchlist entry before it reports user_state.in_watchlist.
func (h *CatalogResourceHandler) SetWatchlistPromoter(p WatchlistItemPromoter) {
	h.watchlistPromoter = p
}

// NewCatalogResourceHandler creates a new canonical catalog resource handler.
func NewCatalogResourceHandler(items *ItemsHandler) *CatalogResourceHandler {
	return &CatalogResourceHandler{items: items}
}

func (h *CatalogResourceHandler) HandleGetItemDetail(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Item ID is required")
		return
	}
	filter, ok := h.items.accessFilterOrError(w, r)
	if !ok {
		return
	}
	view, err := h.ItemDetail(r.Context(), viewerFromRequest(r, filter), id)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	h.prefetchItemDetailVirtual(r.Context(), view, requestProfileID(r))
	writeJSON(w, http.StatusOK, view)
}

// prefetchItemDetailVirtual warms virtual listings for a movie/episode detail
// view. Series details carry no versions and are skipped; per-episode warming
// happens on the season-episodes read instead.
func (h *CatalogResourceHandler) prefetchItemDetailVirtual(ctx context.Context, view *catalog.ItemDetail, profileID string) {
	if h == nil || h.items == nil || view == nil {
		return
	}
	fileIDs := make([]int, 0, 2)
	for _, v := range view.Versions {
		if v.FileID != 0 {
			fileIDs = append(fileIDs, v.FileID)
		}
		if len(fileIDs) >= 2 {
			break
		}
	}
	h.items.prefetchVirtualFileIDs(ctx, fileIDs, profileID)
}

func (h *CatalogResourceHandler) HandleGetItemVersions(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Item ID is required")
		return
	}
	filter, ok := h.items.accessFilterOrError(w, r)
	if !ok {
		return
	}
	view, err := h.ItemVersions(r.Context(), viewerFromRequest(r, filter), id)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// HandleGetMangaFiles returns the local file listing for a manga series (the
// series "View Details" dialog): folder paths plus per-chapter file rows.
func (h *CatalogResourceHandler) HandleGetMangaFiles(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Item ID is required")
		return
	}
	filter, ok := h.items.accessFilterOrError(w, r)
	if !ok {
		return
	}
	view, err := h.MangaFiles(r.Context(), viewerFromRequest(r, filter), id)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *CatalogResourceHandler) HandleGetItemEpisodes(w http.ResponseWriter, r *http.Request) {
	filter, ok := h.items.accessFilterOrError(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Item ID is required")
		return
	}
	view, err := h.ItemEpisodes(r.Context(), viewerFromRequest(r, filter), id)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	h.prefetchEpisodeViewsVirtual(r.Context(), view, requestProfileID(r))
	writeJSON(w, http.StatusOK, episodesListResponse{Episodes: view})
}

func (h *CatalogResourceHandler) HandleGetSeasons(w http.ResponseWriter, r *http.Request) {
	includeArtwork, valid := seasonListArtwork(r)
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid_include_artwork", "include_artwork must be true or false")
		return
	}
	filter, ok := h.items.accessFilterOrError(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Series ID is required")
		return
	}
	view, err := h.seriesSeasons(r.Context(), viewerFromRequest(r, filter), id, includeArtwork)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, seasonsResponse{Seasons: view})
}

func (h *CatalogResourceHandler) HandleGetSeason(w http.ResponseWriter, r *http.Request) {
	filter, ok := h.items.accessFilterOrError(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	numStr := chi.URLParam(r, "num")
	if id == "" || numStr == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Series ID and season number are required")
		return
	}

	num, err := strconv.Atoi(numStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid season number")
		return
	}
	view, err := h.SeriesSeason(r.Context(), viewerFromRequest(r, filter), id, num)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, seasonDetailResponse{Season: view})
}

func (h *CatalogResourceHandler) HandleGetEpisodes(w http.ResponseWriter, r *http.Request) {
	filter, ok := h.items.accessFilterOrError(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	numStr := chi.URLParam(r, "num")
	if id == "" || numStr == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Series ID and season number are required")
		return
	}

	num, err := strconv.Atoi(numStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid season number")
		return
	}
	view, err := h.SeasonEpisodes(r.Context(), viewerFromRequest(r, filter), id, num)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	h.prefetchEpisodeViewsVirtual(r.Context(), view, requestProfileID(r))
	writeJSON(w, http.StatusOK, episodesListResponse{Episodes: view})
}

// prefetchEpisodeViewsVirtual warms the first episode's files only: it is the
// likely next play (autoplay/next-up order), and warming a whole season would
// fan out provider listings per episode. Bounded to 2 file rows like the
// other triggers.
func (h *CatalogResourceHandler) prefetchEpisodeViewsVirtual(ctx context.Context, views []EpisodeView, profileID string) {
	if h == nil || h.items == nil || len(views) == 0 {
		return
	}
	fileIDs := make([]int, 0, 2)
	for _, f := range views[0].Files {
		if f.FileID != 0 {
			fileIDs = append(fileIDs, f.FileID)
		}
		if len(fileIDs) >= 2 {
			break
		}
	}
	h.items.prefetchVirtualFileIDs(ctx, fileIDs, profileID)
}

func (h *CatalogResourceHandler) syntheticSeasonDetail(ctx context.Context, v ItemViewer, seasonID string) (*catalog.ItemDetail, error) {
	seriesID, seasonNum, ok := parseSyntheticSeasonID(seasonID)
	if !ok {
		return nil, catalog.ErrItemNotFound
	}
	if h.items.detailSvc == nil || h.items.episodeRepo == nil {
		return nil, catalog.ErrItemNotFound
	}

	filter := v.Access

	seriesDetail, err := h.items.detailSvc.GetItemDetail(ctx, seriesID, filter)
	if err != nil {
		return nil, err
	}

	episodes, err := h.items.episodeRepo.ListBySeason(ctx, seriesID, seasonNum)
	if err != nil {
		return nil, fmt.Errorf("listing season episodes: %w", err)
	}
	if len(episodes) == 0 {
		return nil, catalog.ErrItemNotFound
	}

	season := &models.Season{
		ContentID:    seasonID,
		SeriesID:     seriesID,
		SeasonNumber: seasonNum,
	}
	if seasonNum == 0 {
		season.Title = "Specials"
	} else {
		season.Title = "Season " + strconv.Itoa(seasonNum)
	}
	seasonResp := h.items.toSeasonResponseFromEpisodes(
		ctx,
		v,
		seriesID,
		season,
		episodes,
		h.items.getAggregateUserData(ctx, v, episodes),
		filter.ImageSize,
	)
	return &catalog.ItemDetail{
		ContentID:         seasonID,
		Type:              "season",
		Title:             seasonResp.Title,
		Overview:          seasonResp.Overview,
		SeriesID:          seriesID,
		SeriesTitle:       seriesDetail.Title,
		SeasonNumber:      &seasonNum,
		EpisodeCount:      &seasonResp.EpisodeCount,
		IsSpecials:        seasonNum == 0,
		SeasonUserData:    seasonResp.UserData,
		Cast:              seriesDetail.Cast,
		Crew:              seriesDetail.Crew,
		BackdropURL:       seriesDetail.BackdropURL,
		BackdropThumbhash: seriesDetail.BackdropThumbhash,
		PosterThumbhash:   seasonResp.PosterThumbhash,
		Versions:          []catalog.FileVersion{},
		Subtitles:         []catalog.SubtitleInfo{},
	}, nil
}

func parseSyntheticSeasonID(contentID string) (string, int, bool) {
	return catalog.ParseSyntheticSeasonID(contentID)
}

func (h *CatalogResourceHandler) enrichItemDetail(ctx context.Context, v ItemViewer, detail *catalog.ItemDetail) {
	if detail == nil {
		return
	}
	{
		input := catalog.PlayableTargetInput{
			ContentID:    detail.ContentID,
			Type:         detail.Type,
			SeriesID:     detail.SeriesID,
			SeasonNumber: detail.SeasonNumber,
		}
		playTargets := h.items.resolvePlayableTargetInputs(ctx, v, []catalog.PlayableTargetInput{input}, nil, v.Access)
		detail.PlayContentID = playTargets[input.Key()]
	}

	switch detail.Type {
	case "season":
		if h.items.episodeRepo != nil {
			if userData, ok := h.items.parentRollupUserData(ctx, v, detail.Type, detail.ContentID); ok {
				detail.SeasonUserData = userData
			} else if episodes, err := h.items.episodeRepo.ListBySeasonID(ctx, detail.ContentID); err == nil {
				detail.SeasonUserData = h.items.getAggregateUserData(ctx, v, episodes)
			}
		}
	case "series":
		if h.items.episodeRepo != nil {
			if userData, ok := h.items.parentRollupUserData(ctx, v, detail.Type, detail.ContentID); ok {
				detail.SeasonUserData = userData
			} else if episodes, err := h.items.episodeRepo.ListBySeries(ctx, detail.ContentID); err == nil {
				detail.SeasonUserData = h.items.getAggregateUserData(ctx, v, episodes)
			}
		}
	case "movie", "episode", "audiobook", "ebook":
		detail.SeasonUserData = h.items.getLeafUserData(ctx, v, detail.ContentID, detail.Type)
		applyEffectiveEditionPreference(detail.SeasonUserData, &detail.EffectiveVersionEditionKey)
	}

	detail.ViewerCurates = h.items.canViewFilePaths(ctx)
	if !detail.ViewerCurates {
		for i := range detail.Versions {
			detail.Versions[i].FilePath = ""
		}
		detail.FolderPaths = nil
	}

	h.enrichViewerState(ctx, v, detail)
}

func (h *CatalogResourceHandler) enrichViewerState(ctx context.Context, v ItemViewer, detail *catalog.ItemDetail) {
	store, profileID, ok := h.items.viewerUserStore(ctx, v.ProfileID)
	if !ok || detail == nil {
		return
	}

	isFavorite, err := store.IsFavorite(ctx, profileID, detail.ContentID)
	if err != nil {
		return
	}
	if h.watchlistPromoter != nil && (detail.Type == "movie" || detail.Type == "series") {
		promoteAccess := v.Access
		promoteAccess.UserID, promoteAccess.ProfileID = apimw.GetUserID(ctx), profileID
		h.watchlistPromoter.PromoteWatchlistItem(ctx, promoteAccess, detail.ContentID)
	}
	inWatchlist, err := store.InWatchlist(ctx, profileID, detail.ContentID)
	if err != nil {
		return
	}

	detail.UserState = &catalog.ItemUserState{
		Played:      detail.SeasonUserData != nil && detail.SeasonUserData.Played,
		IsFavorite:  isFavorite,
		InWatchlist: inWatchlist,
	}

	if h.items.ratingsRepo == nil {
		return
	}

	userID := apimw.GetUserID(ctx)
	if userID == 0 {
		return
	}

	rating, err := h.items.ratingsRepo.Get(ctx, userID, profileID, detail.ContentID)
	if err != nil || rating == nil {
		return
	}

	detail.UserRating = &rating.Rating
}

// seasonListArtworkParam is the series-seasons query parameter advertised by
// the images capability; false omits all poster preparation.
const seasonListArtworkParam = "include_artwork"

// seasonListArtwork allows text-only selectors to omit all poster preparation.
func seasonListArtwork(r *http.Request) (bool, bool) {
	value := r.URL.Query().Get(seasonListArtworkParam)
	if value == "" {
		return true, true
	}
	include, err := strconv.ParseBool(value)
	return include, err == nil
}
