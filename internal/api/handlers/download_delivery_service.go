package handlers

import (
	"errors"
	"log/slog"
	"net/http"

	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/downloads"
	"github.com/Silo-Server/silo-server/internal/httpstream"
)

// ServeDownloadFile is the shared frozen/v2 file lifecycle. Both transports
// arrive with the same verified account/profile context; authorization and
// proxy reservation remain in the existing resolver/serving service.
func (h *DownloadHandler) ServeDownloadFile(w http.ResponseWriter, r *http.Request, id string, delegate bool) error {
	if h == nil || h.svc == nil {
		return &APIError{Status: 503, Message: "Downloads not configured"}
	}
	userID := apimw.GetUserID(r.Context())
	if userID == 0 {
		return &APIError{Status: 401, Message: "Authentication required"}
	}
	profileID, deviceID, _, _ := managedIdentity(r)
	filter := requestAccessFilter(r)
	serveCtx := downloads.WithServeAuthorized(r.Context(), func(target downloads.FileTarget) { attachTransfer(r.Context(), userID, profileID, target.MediaFileID) })
	if delegate && deviceID != "" {
		handled, err := h.redirectManagedDownload(r.Context(), w, r, userID, profileID, deviceID, id, filter)
		if err != nil {
			return err
		}
		if handled {
			return nil
		}
	}
	err := h.svc.ServeFile(serveCtx, httpstream.NewRollingDeadlineWriter(w), r, userID, profileID, deviceID, id, filter)
	if errors.Is(err, downloads.ErrResponseCommitted) {
		// Headers and part of the file already went out, so an error body would
		// corrupt the media response. Abort it instead: both transports (the v1
		// handler and the v2 raw download routes) leave the sentinel to
		// net/http, which closes the connection on http.ErrAbortHandler rather
		// than ending a known-length or chunked body cleanly. A client that left
		// has canceled the request; any other failure, such as a short read from
		// storage, is the server's.
		if r.Context().Err() == nil {
			slog.WarnContext(r.Context(), "download failed after the response started",
				"component", "downloads", "download_id", id, "error", err)
		}
		panic(http.ErrAbortHandler)
	}
	return err
}
func (h *DownloadHandler) ServeDownloadArtwork(w http.ResponseWriter, r *http.Request, id, kind string) error {
	if h == nil || h.svc == nil {
		return &APIError{Status: 503, Message: "Downloads not configured"}
	}
	profile, device, _, _ := managedIdentity(r)
	if profile == "" || device == "" {
		return downloads.ErrProfileRequired
	}
	return h.svc.ServeArtwork(r.Context(), w, r, apimw.GetUserID(r.Context()), profile, device, id, kind, requestAccessFilter(r))
}
func (h *DownloadHandler) ServeDownloadSubtitle(w http.ResponseWriter, r *http.Request, id, ref string) error {
	if h == nil || h.svc == nil {
		return &APIError{Status: 503, Message: "Downloads not configured"}
	}
	user := apimw.GetUserID(r.Context())
	profile, device, _, _ := managedIdentity(r)
	if profile == "" || device == "" {
		return downloads.ErrProfileRequired
	}
	ctx := downloads.WithServeAuthorized(r.Context(), func(target downloads.FileTarget) { attachTransfer(r.Context(), user, profile, target.MediaFileID) })
	return h.svc.ServeSubtitle(ctx, w, r, user, profile, device, id, ref, requestAccessFilter(r))
}
