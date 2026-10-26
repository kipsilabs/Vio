package handlers

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// HandleGetPlaybackInventoryV3 serves GET /api/v2/playback/{session_id}/inventory.
// It returns the live audio and subtitle track inventory for an active playback
// session, allowing web, iOS, and Android clients to update their track selection
// pickers dynamically without reloading or interrupting playback.
//
// It supports HTTP conditional requests via ETag and If-None-Match: when the
// client's cached inventory revision matches the server's current revision,
// it responds with 304 Not Modified.
func (h *PlaybackHandler) HandleGetPlaybackInventoryV3(w http.ResponseWriter, r *http.Request) {
	userID := apimw.GetUserID(r.Context())
	if userID == 0 {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return
	}
	profileID := apimw.GetProfileID(r.Context())

	sessionID := chi.URLParam(r, "session_id")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Session ID is required")
		return
	}

	session, err := h.sessionMgr.GetSession(sessionID)
	if err != nil {
		if errors.Is(err, playback.ErrSessionNotFound) {
			writeError(w, http.StatusNotFound, "playback_session_not_found", "Playback session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to load playback session")
		return
	}
	if session.UserID != userID || (profileID != "" && session.ProfileID != profileID) {
		writeError(w, http.StatusForbidden, "forbidden", "Session belongs to another profile")
		return
	}

	var record *playback.AttemptRecordV3
	if h.PlanStoreV3 != nil {
		if loaded, loadErr := h.PlanStoreV3.GetAttempt(r.Context(), sessionID); loadErr == nil {
			record = loaded
		}
	}
	inventory, invErr := h.playbackInventoryForSession(r.Context(), session, record)
	if invErr != nil {
		writePlaybackOperationError(w, invErr)
		return
	}
	revision := inventory.InventoryRevision
	etag := fmt.Sprintf("%q", revision)

	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, no-cache")

	if match := r.Header.Get("If-None-Match"); match != "" {
		if match == etag || match == revision || match == "*" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	writeJSON(w, http.StatusOK, inventory)
}
