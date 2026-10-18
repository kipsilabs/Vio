package apiv2

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
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
	DiscoverVirtualCandidates(ctx context.Context, userID int, profileID, contentID string, filter catalogpkg.AccessFilter) ([]catalogpkg.FileVersion, error)
	HasVirtualSource(ctx context.Context, contentID string) (bool, error)
}

const (
	// preplayDiscoveryWatchBudget bounds version discovery started from
	// GET /watch/{id}: the caller is preparing to play, so a cold provider
	// scrape may be waited out before the answer.
	preplayDiscoveryWatchBudget = 15 * time.Second
	// preplayDiscoveryCatalogBudget bounds the same discovery started from a
	// catalog leaf detail read: browsing is the hot path (Android opens item
	// detail before any watch request), so the answer keeps a short budget
	// while the discovery service keeps working detached from it.
	preplayDiscoveryCatalogBudget = 2 * time.Second
)

func (reg *Registry) discoverDetailVersions(ctx context.Context, contentID, mediaType string, filter catalogpkg.AccessFilter, trigger string, budget time.Duration) bool {
	claims := claimsFrom(ctx)
	if claims == nil || (mediaType != themeOwnerMovie && mediaType != themeOwnerEpisode) ||
		reg.deps.VirtualCandidatesDiscovery == nil || reg.deps.PreplayDiscoveryEnabled == nil ||
		!reg.deps.PreplayDiscoveryEnabled(ctx) {
		return false
	}
	// Eligibility reads the stored rows server-side instead of the caller's
	// (possibly path-redacted) version list, so ordinary viewers reach the
	// same discovery the watch path performs.
	virtual, err := reg.deps.VirtualCandidatesDiscovery.HasVirtualSource(ctx, contentID)
	if err != nil {
		slog.WarnContext(ctx, "pre-play version discovery skipped", "component", "apiv2", "item_id", contentID,
			"trigger", trigger, "reason", "eligibility_lookup_failed", "error", err)
		return false
	}
	if !virtual {
		return false
	}
	started := time.Now()
	discoveryCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	slog.InfoContext(ctx, "pre-play version discovery started", "component", "apiv2", "item_id", contentID, "trigger", trigger)
	discovered, err := reg.deps.VirtualCandidatesDiscovery.DiscoverVirtualCandidates(
		discoveryCtx, claims.UserID, profileFrom(ctx), contentID, filter,
	)
	if err != nil {
		slog.WarnContext(ctx, "pre-play version discovery unavailable", "component", "apiv2", "item_id", contentID,
			"trigger", trigger, "duration_ms", time.Since(started).Milliseconds())
		return false
	}
	slog.InfoContext(ctx, "pre-play version discovery completed", "component", "apiv2", "item_id", contentID,
		"trigger", trigger, "count", len(discovered), "duration_ms", time.Since(started).Milliseconds())
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
	if _, err := reg.deps.VirtualCandidatesDiscovery.DiscoverVirtualCandidates(ctx, claims.UserID, profileFrom(ctx), string(in.MediaID), filter); err != nil {
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
