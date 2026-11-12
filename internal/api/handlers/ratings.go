package handlers

import (
	"context"
<<<<<<< HEAD
	"encoding/json"
	"log/slog"
=======
>>>>>>> upstream/main
	"net/http"
	"time"

	"github.com/Silo-Server/silo-server/internal/auth"
	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/watchsync"
)

// ratingsRepository defines the data access interface for user ratings.
type ratingsRepository interface {
	Set(ctx context.Context, userID int, profileID, mediaItemID string, rating int) error
	Get(ctx context.Context, userID int, profileID, mediaItemID string) (*catalog.UserRating, error)
	Delete(ctx context.Context, userID int, profileID, mediaItemID string) error
	List(ctx context.Context, userID int, profileID string, limit, offset int) ([]catalog.UserRating, error)
	ListPage(ctx context.Context, userID int, profileID string, filter catalog.AccessFilter, after *catalog.RatingKey, limit int) ([]catalog.UserRating, error)
}

// ratingNotifier is the optional outbound hook fired after a rating is set.
// Implemented by notifications.RatingNotifier; nil when notifications are
// unavailable.
type ratingNotifier interface {
	NotifyRating(ctx context.Context, userID int, profileID, contentID string, rating int) error
}

// LocalRatingEventDispatcher sends a profile's rating changes to its watch
// providers.
type LocalRatingEventDispatcher interface {
	HandleLocalRatingEvent(ctx context.Context, event watchsync.LocalRatingEvent) error
}

// RatingsHandler handles user rating operations.
type RatingsHandler struct {
	ratingsRepo      ratingsRepository
	itemRepo         personalDataItemRepository
	notifier         ratingNotifier
	signalsNotifier  SignalsChangedNotifier
	ratingDispatcher LocalRatingEventDispatcher
}

// NewRatingsHandler creates a new RatingsHandler.
func NewRatingsHandler(ratingsRepo ratingsRepository, itemRepo personalDataItemRepository) *RatingsHandler {
	return &RatingsHandler{ratingsRepo: ratingsRepo, itemRepo: itemRepo}
}

// SetRatingNotifier configures the optional outbound notification hook.
func (h *RatingsHandler) SetRatingNotifier(n ratingNotifier) {
	h.notifier = n
}

// SetSignalsChangedNotifier configures where changes to a profile's
// recommendation signals are reported. Without it they are not reported.
func (h *RatingsHandler) SetSignalsChangedNotifier(notifier SignalsChangedNotifier) {
	h.signalsNotifier = notifier
}

// SetLocalRatingEventDispatcher configures where rating changes are sent for
// watch-provider sync.
func (h *RatingsHandler) SetLocalRatingEventDispatcher(dispatcher LocalRatingEventDispatcher) {
	h.ratingDispatcher = dispatcher
}

func (h *RatingsHandler) dispatchRatingChange(ctx context.Context, userID int, profileID, itemID string) {
	if h.ratingDispatcher == nil {
		return
	}
	_ = h.ratingDispatcher.HandleLocalRatingEvent(ctx, watchsync.LocalRatingEvent{
		UserID:       userID,
		ProfileID:    profileID,
		MediaItemIDs: []string{itemID},
	})
}

// --- Response types ---

// --- Request types ---

<<<<<<< HEAD
type setRatingRequest struct {
	Rating int `json:"rating"`
}

// HandleSetRating handles PUT /ratings/{item_id}.
// Accepts {"rating": N} where N is 1-5. Returns 204 on success.
func (h *RatingsHandler) HandleSetRating(w http.ResponseWriter, r *http.Request) {
	userID := apimw.GetUserID(r.Context())
	profileID := apimw.GetProfileID(r.Context())
	itemID := chi.URLParam(r, "item_id")

	if itemID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Item ID is required")
		return
	}

	var req setRatingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}

	if req.Rating < 1 || req.Rating > 5 {
		writeError(w, http.StatusBadRequest, "bad_request", "Rating must be between 1 and 5")
		return
	}

	if err := h.SetRating(r.Context(), userID, profileID, itemID, requestAccessFilter(r), req.Rating); err != nil {
		writeAPIError(w, err)
		return
	}
	if h.notifier != nil {
		// Best-effort: a delivery failure must never fail the rating write.
		// Detached from the request context so a client disconnect after the
		// rating write doesn't abort the webhook delivery.
		go func() {
			dispatchCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
			defer cancel()
			if err := h.notifier.NotifyRating(dispatchCtx, userID, profileID, itemID, req.Rating); err != nil {
				slog.WarnContext(context.Background(), "failed to dispatch rating notification",
					"component", "ratings", "item_id", itemID, "error", err)
			}
		}()
	}
	w.WriteHeader(http.StatusNoContent)
}

=======
>>>>>>> upstream/main
// SetRating sets the profile's rating of an item the viewer may see. The
// caller validates the range; an item outside the viewer's access is 404.
// Setting the same rating twice converges, so a retried set is safe.
func (h *RatingsHandler) SetRating(ctx context.Context, userID int, profileID, itemID string, access catalog.AccessFilter, rating int) error {
	if err := h.itemRepo.EnsureAccessible(ctx, itemID, access); err != nil {
		return apiError(http.StatusNotFound, "not_found", "Item not found")
	}
	if err := h.ratingsRepo.Set(ctx, userID, profileID, itemID, rating); err != nil {
		return apiError(http.StatusInternalServerError, "internal_error", "Failed to set rating")
	}
	notifySignalsChanged(ctx, h.signalsNotifier, userID, profileID)
	h.dispatchRatingChange(ctx, userID, profileID, itemID)
	return nil
}

// DeleteRating removes the profile's rating of an item. Deleting a rating
// that does not exist succeeds, so a retried delete converges. No access
// check runs: the item may have left the viewer's scope since it was rated,
// and the profile must still be able to drop its own rating. The answer is
// the same 204 whether or not a rating existed, so it reveals nothing about
// a hidden item.
func (h *RatingsHandler) DeleteRating(ctx context.Context, userID int, profileID, itemID string) error {
	if err := h.ratingsRepo.Delete(ctx, userID, profileID, itemID); err != nil {
		return apiError(http.StatusInternalServerError, "internal_error", "Failed to delete rating")
	}
	notifySignalsChanged(ctx, h.signalsNotifier, userID, profileID)
	h.dispatchRatingChange(ctx, userID, profileID, itemID)
	return nil
}

// GetRating answers the profile's rating of an item the viewer may see;
// found is false when the profile has not rated it. An item outside the
// viewer's access is a 404 error whether or not it was rated, as the catalog
// answers it, so a restricted profile cannot learn which hidden titles it
// rated. The stored rating is kept and reads again once the item is visible.
func (h *RatingsHandler) GetRating(ctx context.Context, userID int, profileID, itemID string, access catalog.AccessFilter) (rating catalog.UserRating, found bool, err error) {
	if err := h.ensureAccessibleItem(ctx, itemID, access); err != nil {
		return catalog.UserRating{}, false, itemLookupError(ctx, itemID, err)
	}
	r, err := h.ratingsRepo.Get(ctx, userID, profileID, itemID)
	if err != nil {
		return catalog.UserRating{}, false, apiError(http.StatusInternalServerError, "internal_error", "Failed to get rating")
	}
	if r == nil {
		return catalog.UserRating{}, false, nil
	}
	return *r, true, nil
}

// ensureAccessibleItem checks that the viewer may see the item. A rating is
// only ever set on a catalog item (SetRating checks the same way), so there
// is no episode fallback.
func (h *RatingsHandler) ensureAccessibleItem(ctx context.Context, itemID string, access catalog.AccessFilter) error {
	// An ID Postgres refuses as text names no item; querying with it would
	// fail with an encoding error that reads as a server fault.
	if !auth.ValidTextKey(itemID) {
		return catalog.ErrItemNotFound
	}
	return h.itemRepo.EnsureAccessible(ctx, itemID, access)
}

// ListRatingsPage is the keyset form of ListRatings the v2 listing uses: at
// most limit of the ratings the viewer may see, ordered by (rated_at DESC,
// media_item_id DESC) strictly after the key (nil = from the most recently
// rated row). The store applies the access filter in the page query, so
// every row it answers, including the last one a caller builds its cursor
// from, is an item the viewer may see. A cursor payload is signed but not
// encrypted, so a cursor taken from a hidden row would name that hidden
// title.
func (h *RatingsHandler) ListRatingsPage(ctx context.Context, userID int, profileID string, access catalog.AccessFilter, after *catalog.RatingKey, limit int) ([]catalog.UserRating, error) {
	ratings, err := h.ratingsRepo.ListPage(ctx, userID, profileID, access, after, limit)
	if err != nil {
		return nil, apiError(http.StatusInternalServerError, "internal_error", "Failed to list ratings")
	}
	return ratings, nil
}
