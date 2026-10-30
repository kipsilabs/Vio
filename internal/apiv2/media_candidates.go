package apiv2

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Silo-Server/silo-server/internal/api/handlers"
	catalogpkg "github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
)

const (
	opDiscoverVirtualCandidates      = "discoverVirtualCandidates"
	opRefreshVirtualCandidates       = "refreshVirtualCandidates"
	opCancelVirtualCandidatesRefresh = "cancelVirtualCandidatesRefresh"
	opRequestVirtualRelease          = "requestVirtualRelease"
)

// VirtualCandidatesRefreshService accepts the asynchronous re-list and answers
// the durable job to wait on, and cancels that re-list when the owner presses
// the locked control a second time.
type VirtualCandidatesRefreshService interface {
	CreateRefreshJob(ctx context.Context, userID int, profileID, contentID string, filter catalogpkg.AccessFilter) (*models.AdminJob, error)
	CancelRefreshJob(ctx context.Context, userID int, profileID, contentID string, filter catalogpkg.AccessFilter) (*models.AdminJob, error)
}

type VirtualCandidatesDiscoveryService interface {
	DiscoverVirtualCandidates(ctx context.Context, userID int, profileID, contentID string, filter catalogpkg.AccessFilter, mode handlers.VirtualDiscoveryMode) ([]catalogpkg.FileVersion, error)
	HasVirtualSource(ctx context.Context, contentID string) (bool, error)
}

// VirtualSelectionWarmService schedules the pre-play work for one virtual
// version a detail read surfaced: the inventory probe when the row is
// unprobed, and the provider-side fetch warm-up. *handlers.PlaybackHandler
// implements it. Nil leaves detail reads free of side effects.
type VirtualSelectionWarmService interface {
	ScheduleSelectedFileProbe(contentID string, fileID int, filter catalogpkg.AccessFilter)
	ScheduleSelectedFileWarm(contentID string, fileID int, filter catalogpkg.AccessFilter)
}

const (
	// preplayDiscoveryWatchBudget bounds version discovery started from
	// GET /watch/{id}. The caller is about to play, but the discovery work is
	// detached and the start path resolves its own candidate; waiting longer
	// only delays the player's first request.
	preplayDiscoveryWatchBudget = 2 * time.Second
	// preplayDiscoveryCatalogBudget bounds the same discovery started from a
	// catalog leaf detail read: browsing is the hot path (Android opens item
	// detail before any watch request), so the answer keeps a short budget
	// while the discovery service keeps working detached from it.
	//
	// One second keeps navigation responsive. The discovery work continues
	// detached after this waiter returns, so a cold provider scrape is not
	// lost; the detail page reads the already-persisted rows and the chain
	// publishes the refreshed inventory when it lands. Deployments tune it
	// with the admin Streaming setting virtual_library.catalog_discovery_budget_ms.
	preplayDiscoveryCatalogBudget = 1 * time.Second

	// preplayDiscoveryDetailRepeatWindow is how long a successful detail
	// discovery suppresses another list for the same viewer, item, selected
	// version and library. The player's inventory poll re-reads watch detail
	// at 2s/4s/8s/20s while a virtual probe is pending; a listing on every read
	// would rewrite all candidates and invalidate the probe's CAS snapshot.
	// Manual refresh and the explicit :discover operation do not use this memo.
	preplayDiscoveryDetailRepeatWindow = 30 * time.Second
	// Bound the per-process memo so a busy catalog cannot grow it without
	// limit. Expired entries are swept first; if the map is still full, it is
	// cleared, which only costs one extra discovery per item.
	preplayDiscoveryDetailMemoMaxEntries = 512
)

const (
	// maxDetailVirtualWarmVersions caps how many virtual versions one catalog
	// detail read warms. Two matches the watch-detail prefetch cap and keeps a
	// many-version title from fanning out provider work on a browse read.
	maxDetailVirtualWarmVersions = 2
	// detailVirtualWarmRepeatWindow collapses repeated detail reads for the
	// same version. The handler's own chain admission already drops a duplicate
	// live chain; this window also covers the gap after one finishes while the
	// client is still opening or polling the page.
	detailVirtualWarmRepeatWindow = 30 * time.Second
	// detailVirtualWarmMemoMaxEntries bounds the memo so a busy catalog cannot
	// grow it without limit. Expired entries are swept first; a still-full map
	// is cleared, costing one extra warm per item.
	detailVirtualWarmMemoMaxEntries = 512
)

// preplayDiscoveryMemoKey identifies one viewer's detail read for one item and
// selected version. The selected file is part of the key: switching versions
// must run discovery for the newly selected row even inside the repeat window.
func preplayDiscoveryMemoKey(ctx context.Context, contentID string, filter catalogpkg.AccessFilter) string {
	userID := 0
	if claims := claimsFrom(ctx); claims != nil {
		userID = claims.UserID
	}
	libraryID := 0
	if filter.PresentationLibraryID != nil {
		libraryID = *filter.PresentationLibraryID
	}
	return strconv.Itoa(userID) + "\x00" + profileFrom(ctx) + "\x00" + contentID + "\x00" +
		strconv.Itoa(filter.SelectedFileID) + "\x00" + strconv.Itoa(libraryID)
}

func (reg *Registry) preplayDiscoveryRecentlyRan(key string) bool {
	if reg == nil || key == "" {
		return false
	}
	reg.preplayDiscoveryMu.Lock()
	defer reg.preplayDiscoveryMu.Unlock()
	seen, ok := reg.preplayDiscoverySeen[key]
	return ok && time.Since(seen) < preplayDiscoveryDetailRepeatWindow
}

func (reg *Registry) rememberPreplayDiscovery(key string) {
	if reg == nil || key == "" {
		return
	}
	now := time.Now()
	reg.preplayDiscoveryMu.Lock()
	defer reg.preplayDiscoveryMu.Unlock()
	if reg.preplayDiscoverySeen == nil {
		reg.preplayDiscoverySeen = make(map[string]time.Time)
	}
	if len(reg.preplayDiscoverySeen) >= preplayDiscoveryDetailMemoMaxEntries {
		for existing, seen := range reg.preplayDiscoverySeen {
			if now.Sub(seen) >= preplayDiscoveryDetailRepeatWindow {
				delete(reg.preplayDiscoverySeen, existing)
			}
		}
		if len(reg.preplayDiscoverySeen) >= preplayDiscoveryDetailMemoMaxEntries {
			reg.preplayDiscoverySeen = make(map[string]time.Time)
		}
	}
	reg.preplayDiscoverySeen[key] = now
}

// preplayDiscoveryCatalogBudgetFor resolves the catalog leaf detail budget:
// the configured setting when wired, otherwise the built-in default. A
// non-positive answer (unset reader, unreadable setting) falls back to the
// default rather than stretching the wait.
func (reg *Registry) preplayDiscoveryCatalogBudgetFor(ctx context.Context) time.Duration {
	if reg.deps.PreplayDiscoveryBudgetMs == nil {
		return preplayDiscoveryCatalogBudget
	}
	if ms := reg.deps.PreplayDiscoveryBudgetMs(ctx); ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return preplayDiscoveryCatalogBudget
}

// discoverDetailVersions runs the candidate discovery a detail-page read needs
// and reports whether it found any. A detail-page read may start the pre-play
// preparation; a bare listing request may not.
func (reg *Registry) discoverDetailVersions(ctx context.Context, contentID, mediaType string, filter catalogpkg.AccessFilter, trigger string, budget time.Duration) (discovered, hasVirtualSource bool) {
	claims := claimsFrom(ctx)
	if claims == nil || (mediaType != themeOwnerMovie && mediaType != themeOwnerEpisode) ||
		reg.deps.VirtualCandidatesDiscovery == nil {
		return false, false
	}
	// Eligibility reads the stored rows server-side instead of the caller's
	// (possibly path-redacted) version list, so ordinary viewers reach the
	// same discovery the watch path performs. It also feeds the detail
	// response's has_virtual_source hint, so it runs regardless of whether
	// discovery is enabled.
	virtual, err := reg.deps.VirtualCandidatesDiscovery.HasVirtualSource(ctx, contentID)
	if err != nil {
		slog.WarnContext(ctx, "pre-play version discovery skipped", "component", "apiv2", "item_id", contentID,
			"trigger", trigger, "reason", "eligibility_lookup_failed", "error", err)
		return false, false
	}
	hasVirtualSource = virtual
	if !virtual {
		return false, false
	}
	if reg.deps.PreplayDiscoveryEnabled == nil || !reg.deps.PreplayDiscoveryEnabled(ctx) {
		return false, hasVirtualSource
	}
	memoKey := preplayDiscoveryMemoKey(ctx, contentID, filter)
	if reg.preplayDiscoveryRecentlyRan(memoKey) {
		// The player polls watch detail for inventory while the pre-play probe
		// is still running. Re-listing on that poll rewrites every candidate and
		// invalidates the probe write's catalog snapshot; the first read in this
		// window already did the work.
		slog.DebugContext(ctx, "pre-play version discovery coalesced", "component", "apiv2", "item_id", contentID,
			"trigger", trigger, "selected_file_id", filter.SelectedFileID)
		return false, true
	}
	started := time.Now()
	discoveryCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	slog.InfoContext(ctx, "pre-play version discovery started", "component", "apiv2", "item_id", contentID, "trigger", trigger)
	listed, listErr := reg.deps.VirtualCandidatesDiscovery.DiscoverVirtualCandidates(
		discoveryCtx, claims.UserID, profileFrom(ctx), contentID, filter,
		handlers.VirtualDiscoveryDetail,
	)
	if listErr != nil {
		slog.WarnContext(ctx, "pre-play version discovery unavailable", "component", "apiv2", "item_id", contentID,
			"trigger", trigger, "duration_ms", time.Since(started).Milliseconds())
		return false, true
	}
	slog.InfoContext(ctx, "pre-play version discovery completed", "component", "apiv2", "item_id", contentID,
		"trigger", trigger, "count", len(listed), "duration_ms", time.Since(started).Milliseconds())
	reg.rememberPreplayDiscovery(memoKey)
	return true, true
}

// warmDetailVirtualVersions starts the pre-play probe/warm chain for the
// virtual versions a catalog leaf detail read surfaced. Android and other thin
// clients open the item page through getCatalogItem and pick a version
// client-side; they never call the versions check or a select endpoint, so
// this read is the only server-visible signal that a version is about to play.
//
// The work is bounded to movies and individual episodes (a series or season
// page carries no playable row), to maxDetailVirtualWarmVersions per read, and
// to one schedule per version inside detailVirtualWarmRepeatWindow. Scheduling
// is non-blocking: ScheduleSelectedFileProbe/Warm only take an admission slot
// and spawn detached work under their own budgets, and both refuse a duplicate
// live chain. A failed or durably unavailable row is skipped. This path only
// warms: it never writes a failure verdict, never stamps failed_at, and never
// fails the detail request.
func (reg *Registry) warmDetailVirtualVersions(ctx context.Context, detail *catalogpkg.ItemDetail, filter catalogpkg.AccessFilter) {
	if reg == nil || detail == nil || reg.deps.VirtualSelectionWarm == nil {
		return
	}
	if detail.Type != themeOwnerMovie && detail.Type != themeOwnerEpisode {
		return
	}
	scheduler := reg.deps.VirtualSelectionWarm
	warmed := 0
	for _, version := range detail.Versions {
		if warmed >= maxDetailVirtualWarmVersions {
			break
		}
		if version.FileID <= 0 || version.Failed || (version.Available != nil && !*version.Available) {
			continue
		}
		if !isVirtualVersion(version) {
			continue
		}
		// The cap counts candidate positions, not admissions, so a repeated
		// read resolves to the same two rows instead of drifting to the next
		// pair once the first pair is memoized.
		warmed++
		if !reg.rememberDetailVirtualWarm(detail.ContentID, version.FileID) {
			continue
		}
		// An unprobed row needs the inventory probe for its track menus; a
		// probed row still needs the warm because probe evidence says nothing
		// about whether the provider has fetched the bytes.
		if version.ProbedAt == nil {
			scheduler.ScheduleSelectedFileProbe(detail.ContentID, version.FileID, filter)
		}
		scheduler.ScheduleSelectedFileWarm(detail.ContentID, version.FileID, filter)
	}
}

// isVirtualVersion reports whether a projected version is provider-backed.
// The path prefix and the container sentinel are the two markers the catalog
// row uses; either is enough.
func isVirtualVersion(version catalogpkg.FileVersion) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(version.FilePath)), "virtual://") ||
		strings.EqualFold(strings.TrimSpace(version.Container), "virtual")
}

// rememberDetailVirtualWarm records a scheduled version and reports whether it
// was new. A repeat inside detailVirtualWarmRepeatWindow returns false so the
// caller skips scheduling.
func (reg *Registry) rememberDetailVirtualWarm(contentID string, fileID int) bool {
	if reg == nil || contentID == "" || fileID <= 0 {
		return false
	}
	key := contentID + "\x00" + strconv.Itoa(fileID)
	now := time.Now()
	reg.virtualWarmMu.Lock()
	defer reg.virtualWarmMu.Unlock()
	if reg.virtualWarmSeen == nil {
		reg.virtualWarmSeen = make(map[string]time.Time)
	}
	if seen, ok := reg.virtualWarmSeen[key]; ok && now.Sub(seen) < detailVirtualWarmRepeatWindow {
		return false
	}
	if len(reg.virtualWarmSeen) >= detailVirtualWarmMemoMaxEntries {
		for existing, seen := range reg.virtualWarmSeen {
			if now.Sub(seen) >= detailVirtualWarmRepeatWindow {
				delete(reg.virtualWarmSeen, existing)
			}
		}
		if len(reg.virtualWarmSeen) >= detailVirtualWarmMemoMaxEntries {
			reg.virtualWarmSeen = make(map[string]time.Time)
		}
	}
	reg.virtualWarmSeen[key] = now
	return true
}

// VirtualReleaseRequestService requests one stored indexer release on the
// provider.
type VirtualReleaseRequestService interface {
	RequestIndexerRelease(ctx context.Context, userID int, profileID, contentID string, releaseID int64, filter catalogpkg.AccessFilter) (handlers.IndexerReleaseRequestResult, error)
}

// VirtualCandidatesRefreshInput names the item whose version candidates to
// re-list.
type VirtualCandidatesRefreshInput struct {
	MediaID ID `path:"media_id" doc:"A movie or episode whose virtual candidates to re-list" example:"movie:heat-1995"`
}

// VirtualReleaseRequestInput names the item and the stored release to request.
type VirtualReleaseRequestInput struct {
	MediaID   ID `path:"media_id" doc:"A movie or episode whose indexer release to request" example:"movie:heat-1995"`
	ReleaseID ID `path:"release_id" doc:"The opaque release id read from the watch detail's indexer_releases" example:"42"`
}

// VirtualReleaseRequest is the request action's answer. It carries the opaque
// release id and the resulting state; a URL is never accepted or echoed.
type VirtualReleaseRequest struct {
	ReleaseID ID     `json:"release_id"`
	State     string `json:"state" enum:"queued,failed"`
	Message   string `json:"message,omitempty"`
}

type VirtualReleaseRequestOutput struct {
	Body VirtualReleaseRequest
}

func registerMediaCandidates(reg *Registry) {
	discover := humaOp(http.MethodPost, Prefix+"/media/{media_id}/virtual-candidates:discover", opDiscoverVirtualCandidates, "watch",
		"Discover and persist cached provider versions before playback when enabled, without probing or requesting releases.")
	discover.DefaultStatus = http.StatusNoContent
	Register(reg, Operation{
		Operation:       discover,
		Class:           ClassProfileScoped,
		ProfileOptional: true,
		ServiceBacked:   true,
		RetrySafety:     RetrySafetyCoalescing,
	}, reg.discoverVirtualCandidates)

	op := humaOp(http.MethodPost, Prefix+"/media/{media_id}/virtual-candidates:refresh", opRefreshVirtualCandidates, "watch",
		"Queue an asynchronous provider re-list of a virtual item's version candidates and answer the job to wait on.")
	op.DefaultStatus = http.StatusAccepted
	Register(reg, Operation{
		Operation:       op,
		Class:           ClassProfileScoped,
		ProfileOptional: true,
		ServiceBacked:   true,
		// One active job per title: a concurrent refresh returns the existing
		// job rather than starting a second provider re-list.
		RetrySafety: RetrySafetyCoalescing,
	}, reg.refreshVirtualCandidates)

	cancel := humaOp(http.MethodPost, Prefix+"/media/{media_id}/virtual-candidates:refresh/cancel", opCancelVirtualCandidatesRefresh, "watch",
		"Cancel the caller's in-flight refresh of a virtual item's version candidates. Cancellation is non-destructive to already-persisted candidates and leaves the automatic re-listing intervals untouched.")
	cancel.DefaultStatus = http.StatusOK
	cancel.Errors = []int{http.StatusConflict, http.StatusForbidden}
	Register(reg, Operation{
		Operation:       cancel,
		Class:           ClassProfileScoped,
		ProfileOptional: true,
		ServiceBacked:   true,
		// The job state machine makes repeated cancellation requests converge
		// on the same terminal canceled state.
		RetrySafety: RetrySafetyCoalescing,
	}, reg.cancelVirtualCandidatesRefresh)

	request := humaOp(http.MethodPost, Prefix+"/media/{media_id}/virtual-releases/{release_id}:request", opRequestVirtualRelease, "watch",
		"Request a release that exists on the indexers on the provider. The stored download URL is used server-side and is never returned.")
	request.DefaultStatus = http.StatusOK
	request.Errors = []int{http.StatusNotFound}
	Register(reg, Operation{
		Operation:       request,
		Class:           ClassProfileScoped,
		ProfileOptional: true,
		ServiceBacked:   true,
		// The stored release row is the domain identity: a second request for
		// an already-queued release returns the same state without a second
		// enqueue.
		RetrySafety: RetrySafetyDomainIdentity,
	}, reg.requestVirtualRelease)
}

func (reg *Registry) discoverVirtualCandidates(ctx context.Context, in *VirtualCandidatesRefreshInput) (*struct{}, error) {
	claims := claimsFrom(ctx)
	if claims == nil {
		return nil, NewProblem(TypeAuthenticationRequired, "Authentication is required.")
	}
	if reg.deps.VirtualCandidatesDiscovery == nil || reg.deps.Watch == nil ||
		reg.deps.PreplayDiscoveryEnabled == nil || !reg.deps.PreplayDiscoveryEnabled(ctx) {
		return nil, unavailable("pre-play version discovery")
	}
	filter, err := reg.deps.Watch.ContextAccessFilter(ctx, handlers.AccessFilterOptions{})
	if err != nil {
		return nil, NewProblem(TypeInternalError, "An unexpected error occurred.")
	}
	// This operation is a listing surface: it is callable for any item by any
	// client, so it lists and persists candidates and prepares nothing. A
	// detail-page read is what may start a preparation.
	if _, err := reg.deps.VirtualCandidatesDiscovery.DiscoverVirtualCandidates(ctx, claims.UserID, profileFrom(ctx), string(in.MediaID), filter, handlers.VirtualDiscoveryListingOnly); err != nil {
		return nil, serviceProblem(err)
	}
	return &struct{}{}, nil
}

func (reg *Registry) refreshVirtualCandidates(ctx context.Context, in *VirtualCandidatesRefreshInput) (*AdminCatalogJobAcceptedOutput, error) {
	if reg.deps.VirtualCandidatesRefresh == nil || reg.deps.Watch == nil {
		return nil, unavailable("virtual candidates refresh")
	}
	claims := claimsFrom(ctx)
	if claims == nil {
		return nil, NewProblem(TypeAuthenticationRequired, "Authentication is required.")
	}
	filter, err := reg.deps.Watch.ContextAccessFilter(ctx, handlers.AccessFilterOptions{})
	if err != nil {
		return nil, NewProblem(TypeInternalError, "An unexpected error occurred.")
	}
	job, err := reg.deps.VirtualCandidatesRefresh.CreateRefreshJob(ctx, claims.UserID, profileFrom(ctx), string(in.MediaID), filter)
	if err != nil {
		return nil, serviceProblem(err)
	}
	out := &AdminCatalogJobAcceptedOutput{
		Location:   Prefix + "/admin/jobs/" + job.ID,
		RetryAfter: "5",
		Body:       reg.adminTaskJobOf(ctx, job, claims.Role == models.RoleAdmin),
	}
	return out, nil
}

func (reg *Registry) cancelVirtualCandidatesRefresh(ctx context.Context, in *VirtualCandidatesRefreshInput) (*AdminCatalogJobAcceptedOutput, error) {
	if reg.deps.VirtualCandidatesRefresh == nil || reg.deps.Watch == nil {
		return nil, unavailable("virtual candidates refresh")
	}
	claims := claimsFrom(ctx)
	if claims == nil {
		return nil, NewProblem(TypeAuthenticationRequired, "Authentication is required.")
	}
	filter, err := reg.deps.Watch.ContextAccessFilter(ctx, handlers.AccessFilterOptions{})
	if err != nil {
		return nil, NewProblem(TypeInternalError, "An unexpected error occurred.")
	}
	job, err := reg.deps.VirtualCandidatesRefresh.CancelRefreshJob(ctx, claims.UserID, profileFrom(ctx), string(in.MediaID), filter)
	if err != nil {
		var apiErr *handlers.APIError
		if errors.As(err, &apiErr) && apiErr.Code == "not_cancellable" {
			return nil, NewProblem(TypeJobNotCancelable, apiErr.Message)
		}
		return nil, serviceProblem(err)
	}
	return &AdminCatalogJobAcceptedOutput{
		Location:   Prefix + "/admin/jobs/" + job.ID,
		RetryAfter: "5",
		Body:       reg.adminTaskJobOf(ctx, job, claims.Role == models.RoleAdmin),
	}, nil
}

func (reg *Registry) requestVirtualRelease(ctx context.Context, in *VirtualReleaseRequestInput) (*VirtualReleaseRequestOutput, error) {
	if reg.deps.VirtualReleaseRequest == nil || reg.deps.Watch == nil {
		return nil, unavailable("indexer release requests")
	}
	claims := claimsFrom(ctx)
	if claims == nil {
		return nil, NewProblem(TypeAuthenticationRequired, "Authentication is required.")
	}
	releaseID, err := strconv.ParseInt(string(in.ReleaseID), 10, 64)
	if err != nil || releaseID <= 0 || strconv.FormatInt(releaseID, 10) != string(in.ReleaseID) {
		return nil, NewProblem(TypeValidationFailed, "The request did not pass validation; see errors.").
			WithErrors(ProblemError{Location: "path.release_id", Code: codeInvalid, Detail: "release_id must name a release."})
	}
	filter, err := reg.deps.Watch.ContextAccessFilter(ctx, handlers.AccessFilterOptions{})
	if err != nil {
		return nil, NewProblem(TypeInternalError, "An unexpected error occurred.")
	}
	result, err := reg.deps.VirtualReleaseRequest.RequestIndexerRelease(ctx, claims.UserID, profileFrom(ctx), string(in.MediaID), releaseID, filter)
	if err != nil {
		return nil, serviceProblem(err)
	}
	return &VirtualReleaseRequestOutput{Body: VirtualReleaseRequest{ReleaseID: ID(result.ReleaseID), State: result.State, Message: result.Message}}, nil
}
