package handlers

import (
	"context"
	"net/http"

	"github.com/Silo-Server/silo-server/internal/catalog"
)

// prefetchVirtualFileIDs warms virtual candidate listings for raw file rows
// surfaced by the item-detail, versions, and episode-list reads. Dedupes, caps
// at 2, and runs fully off the response path.
//
// The watch detail is not a trigger: a v2 watch read runs its own candidate
// discovery (which lists, persists, and seeds the play path's listing cache —
// see PlaybackHandler.SeedVirtualListing) and warms the rows it lists, so a
// list-only prefetch from that read would pay provider traffic discovery
// already paid for.
func (h *ItemsHandler) prefetchVirtualFileIDs(ctx context.Context, fileIDs []int, profileID string) {
	if h == nil || h.virtualPrefetcher == nil || h.virtualFileByID == nil || profileID == "" {
		return
	}
	seen := make(map[int]bool)
	unique := make([]int, 0, 2)
	for _, fid := range fileIDs {
		if fid != 0 && !seen[fid] {
			seen[fid] = true
			unique = append(unique, fid)
		}
		if len(unique) >= 2 {
			break
		}
	}
	if len(unique) == 0 {
		return
	}
	fileIDs = unique
	// Row loads run off the response path: the detail response must not wait
	// on database reads for an optional warm. A short timeout bounds the
	// background load; PrefetchVirtualPlayback itself is already bounded
	// (2 files), deduped, and detached.
	//
	// File-level admission runs BEFORE the goroutine and the row loads: the
	// content-keyed admission inside PrefetchVirtualPlayback only fires after
	// rows load, so without this every catalog read would pay DB lookups even
	// for an already-warming file. Keys live only for the load window.
	admitKey := virtualPrefetchFileAdmitKey(fileIDs, profileID)
	if _, loaded := h.virtualPrefetchInflight.LoadOrStore(admitKey, struct{}{}); loaded {
		return
	}
	// Shed when saturated: speculative work must never queue without bound.
	// capVirtualPrefetchLoads mirrors the prefetch worker count so background
	// loads cannot outrun the workers that consume them.
	h.prefetchSemOnce.Do(func() {
		h.virtualPrefetchLoadSem = make(chan struct{}, capVirtualPrefetchLoads)
	})
	sem := h.virtualPrefetchLoadSem
	select {
	case sem <- struct{}{}:
	default:
		h.virtualPrefetchInflight.Delete(admitKey)
		return
	}
	prefetcher := h.virtualPrefetcher
	loader := h.virtualFileByID
	go func() {
		defer h.virtualPrefetchInflight.Delete(admitKey)
		defer func() { <-sem }()
		// WithoutCancel keeps request values (user ID for PrefetchVirtualPlayback)
		// while detaching from request cancellation; the timeout still bounds
		// the background load.
		loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		files := make([]*models.MediaFile, 0, len(fileIDs))
		for _, fid := range fileIDs {
			f, err := loader.GetByID(loadCtx, fid)
			if err != nil || f == nil || !isVirtualPlaybackFile(f) {
				continue
			}
			files = append(files, f)
		}
		if len(files) == 0 {
			return
		}
		prefetcher.PrefetchVirtualPlayback(loadCtx, files, profileID)
	}()
}

// requestProfileID resolves the active profile for a request, falling back to
// the X-Profile-Id header when the auth context does not carry one.
func requestProfileID(r *http.Request) string {
	profileID := apimw.GetProfileID(r.Context())
	if profileID == "" {
		profileID = r.Header.Get("X-Profile-Id")
	}
	return profileID
}

// viewerFromRequest pairs an already-resolved access filter with the
// request's declared profile.
func viewerFromRequest(r *http.Request, filter catalog.AccessFilter) ItemViewer {
	return ItemViewer{Access: filter, ProfileID: requestProfileID(r)}
}

// accessFilterOrError resolves the viewer's access filter for a handler that
// owns the response. An unresolvable policy is answered with 500 rather than
// an unrestricted listing.
//
// It is also where the request's image_size preference is read, because it is
// the single choke point every catalog read handler already goes through, and
// the size applies to the whole response. An unrecognized size is a 400: a
// client that sends one has a bug, and silently serving the default size would
// hide it.
func (h *ItemsHandler) accessFilterOrError(w http.ResponseWriter, r *http.Request) (catalog.AccessFilter, bool) {
	filter, err := h.accessFilter(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to resolve user access")
		return catalog.AccessFilter{}, false
	}
	size, err := imagesize.FromRequest(r)
	if err != nil {
		writeInvalidImageSize(w)
		return catalog.AccessFilter{}, false
	}
	filter.ImageSize = size
	return filter, true
}
