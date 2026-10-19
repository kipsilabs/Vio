package apiv2

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/adminjob"
	"github.com/Silo-Server/silo-server/internal/api/handlers"
	catalogpkg "github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
)

// fakeVirtualCandidatesRefresh is the async refresh seam: it records the
// resolved identity and answers a queued job or a fixed error. cancelJob is the
// job the cancel command returns; cancelErr a fixed cancel error.
type fakeVirtualCandidatesRefresh struct {
	calls     int
	userID    int
	profileID string
	contentID string
	job       *models.AdminJob
	err       error
	cancelJob *models.AdminJob
	cancelErr error
}

func (f *fakeVirtualCandidatesRefresh) CreateRefreshJob(_ context.Context, userID int, profileID, contentID string, _ catalogpkg.AccessFilter) (*models.AdminJob, error) {
	f.calls++
	f.userID = userID
	f.profileID = profileID
	f.contentID = contentID
	if f.err != nil {
		return nil, f.err
	}
	if f.job != nil {
		return f.job, nil
	}
	return &models.AdminJob{
		ID: "job-1", JobType: adminjob.JobTypeVirtualCandidatesRefresh, Status: adminjob.StatusQueued,
		CreatedByUserID: userID, RequestedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}, nil
}

func (f *fakeVirtualCandidatesRefresh) CancelRefreshJob(_ context.Context, userID int, profileID, contentID string, _ catalogpkg.AccessFilter) (*models.AdminJob, error) {
	if f.cancelErr != nil {
		return nil, f.cancelErr
	}
	if f.cancelJob != nil {
		return f.cancelJob, nil
	}
	return &models.AdminJob{
		ID: "job-1", JobType: adminjob.JobTypeVirtualCandidatesRefresh, Status: adminjob.StatusCancelled,
		CreatedByUserID: userID, RequestedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}, nil
}

// fakeVirtualReleaseRequest is the request-action seam.
type fakeVirtualReleaseRequest struct {
	calls     int
	userID    int
	profileID string
	contentID string
	releaseID int64
	result    handlers.IndexerReleaseRequestResult
	err       error
}

func (f *fakeVirtualReleaseRequest) RequestIndexerRelease(_ context.Context, userID int, profileID, contentID string, releaseID int64, _ catalogpkg.AccessFilter) (handlers.IndexerReleaseRequestResult, error) {
	f.calls++
	f.userID = userID
	f.profileID = profileID
	f.contentID = contentID
	f.releaseID = releaseID
	if f.err != nil {
		return handlers.IndexerReleaseRequestResult{}, f.err
	}
	return f.result, nil
}

func mediaCandidatesDeps(refresh *fakeVirtualCandidatesRefresh) Dependencies {
	deps := pilotDeps(nil, nil)
	deps.Watch = &fakeWatch{}
	deps.VirtualCandidatesRefresh = refresh
	return deps
}

func (f *fakeVirtualCandidatesRefresh) DiscoverVirtualCandidates(_ context.Context, userID int, profileID, contentID string, _ catalogpkg.AccessFilter) ([]catalogpkg.FileVersion, error) {
	f.calls++
	f.userID, f.profileID, f.contentID = userID, profileID, contentID
	return nil, f.err
}

func (f *fakeVirtualCandidatesRefresh) HasVirtualSource(_ context.Context, _ string) (bool, error) {
	return true, nil
}

func TestDiscoverVirtualCandidatesSettingAndAccess(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		discovery := &fakeVirtualCandidatesRefresh{}
		deps := mediaCandidatesDeps(discovery)
		deps.VirtualCandidatesDiscovery = discovery
		deps.PreplayDiscoveryEnabled = func(context.Context) bool { return enabled }
		handler := newTestHandler(t, deps)
		endpoint := "/api/v2/media/movie:heat-1995/virtual-candidates:discover"
		requireProblem(t, do(t, handler, http.MethodPost, endpoint, "", nil), TypeAuthenticationRequired)
		if discovery.calls != 0 {
			t.Fatal("unauthenticated discovery reached the service")
		}
		owner := with(bearer(memberToken), "X-Profile-Id", "p-owner")
		response := do(t, handler, http.MethodPost, endpoint, "", owner)
		if !enabled {
			requireProblem(t, response, TypeDependencyUnavailable)
			if discovery.calls != 0 {
				t.Fatal("disabled discovery reached the service")
			}
			continue
		}
		if response.Code != http.StatusNoContent || discovery.calls != 1 || discovery.userID != 1 || discovery.profileID != "p-owner" || discovery.contentID != "movie:heat-1995" {
			t.Fatalf("status=%d discovery=%+v", response.Code, discovery)
		}
		// An empty candidate list is a successful discovery (the fake returns
		// nil versions); provider unavailability stays a retryable 503.
		discovery.err = &handlers.APIError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "The provider could not be reached; try again."}
		requireProblem(t, do(t, handler, http.MethodPost, endpoint, "", owner), TypeDependencyUnavailable)
		discovery.err = &handlers.APIError{Status: http.StatusNotFound, Code: "not_found", Message: "Watch target not found"}
		requireProblem(t, do(t, handler, http.MethodPost, endpoint, "", owner), TypeNotFound)
	}
}

func TestRefreshVirtualCandidatesAcceptsJob(t *testing.T) {
	refresh := &fakeVirtualCandidatesRefresh{}
	h := newTestHandler(t, mediaCandidatesDeps(refresh))
	owner := with(bearer(memberToken), "X-Profile-Id", "p-owner")

	rec := do(t, h, http.MethodPost, "/api/v2/media/movie:heat-1995/virtual-candidates:refresh", "", owner)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/api/v2/admin/jobs/job-1" {
		t.Fatalf("location = %q", loc)
	}
	if retry := rec.Header().Get("Retry-After"); retry != "5" {
		t.Fatalf("retry-after = %q", retry)
	}
	var body struct {
		ID    string `json:"id"`
		Kind  string `json:"kind"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ID != "job-1" || body.Kind != adminjob.JobTypeVirtualCandidatesRefresh || body.State != "queued" {
		t.Fatalf("body = %+v", body)
	}
	if refresh.calls != 1 || refresh.contentID != "movie:heat-1995" || refresh.userID != 1 || refresh.profileID != "p-owner" {
		t.Fatalf("refresh call = %+v", refresh)
	}
}

func TestRefreshVirtualCandidatesRejects(t *testing.T) {
	owner := with(bearer(memberToken), "X-Profile-Id", "p-owner")

	// Authentication is required before the service is reached.
	refresh := &fakeVirtualCandidatesRefresh{}
	h := newTestHandler(t, mediaCandidatesDeps(refresh))
	requireProblem(t, do(t, h, http.MethodPost, "/api/v2/media/movie:heat-1995/virtual-candidates:refresh", "", nil), TypeAuthenticationRequired)
	if refresh.calls != 0 {
		t.Fatalf("unauthenticated request reached the service %d times", refresh.calls)
	}

	// A missing service is a retryable wiring gap, not a 404.
	off := newTestHandler(t, pilotDeps(nil, nil))
	requireProblem(t, do(t, off, http.MethodPost, "/api/v2/media/movie:heat-1995/virtual-candidates:refresh", "", owner), TypeDependencyUnavailable)

	// Unknown media is a problem 404.
	refresh = &fakeVirtualCandidatesRefresh{err: &handlers.APIError{Status: http.StatusNotFound, Code: "not_found", Message: "Watch target not found"}}
	h = newTestHandler(t, mediaCandidatesDeps(refresh))
	requireProblem(t, do(t, h, http.MethodPost, "/api/v2/media/movie:missing/virtual-candidates:refresh", "", owner), TypeNotFound)

	// Non-virtual content is a problem 4xx.
	refresh = &fakeVirtualCandidatesRefresh{err: &handlers.APIError{Status: http.StatusUnprocessableEntity, Code: "validation_failed", Message: "The item has no virtual candidates to refresh."}}
	h = newTestHandler(t, mediaCandidatesDeps(refresh))
	requireProblem(t, do(t, h, http.MethodPost, "/api/v2/media/movie:local/virtual-candidates:refresh", "", owner), TypeValidationFailed)

	// A provider failure is a retryable dependency problem.
	refresh = &fakeVirtualCandidatesRefresh{err: &handlers.APIError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "The provider could not be reached; try again."}}
	h = newTestHandler(t, mediaCandidatesDeps(refresh))
	requireProblem(t, do(t, h, http.MethodPost, "/api/v2/media/movie:heat-1995/virtual-candidates:refresh", "", owner), TypeDependencyUnavailable)
}

// TestRefreshVirtualCandidatesIsProfileOptional mirrors GET /watch/{id}: an
// account-scoped caller may refresh without a profile header.
func TestRefreshVirtualCandidatesIsProfileOptional(t *testing.T) {
	refresh := &fakeVirtualCandidatesRefresh{}
	h := newTestHandler(t, mediaCandidatesDeps(refresh))

	rec := do(t, h, http.MethodPost, "/api/v2/media/movie:heat-1995/virtual-candidates:refresh", "", bearer(memberToken))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body %s", rec.Code, rec.Body.String())
	}
	if refresh.profileID != "" {
		t.Fatalf("profile id = %q, want empty", refresh.profileID)
	}
}

// TestCancelVirtualCandidatesRefresh covers the owner-authorized cancel command:
// the second press cancels the in-flight job and answers it, while a foreign
// job is refused with a forbidden problem.
func TestCancelVirtualCandidatesRefresh(t *testing.T) {
	owner := with(bearer(memberToken), "X-Profile-Id", "p-owner")

	refresh := &fakeVirtualCandidatesRefresh{cancelJob: &models.AdminJob{
		ID: "job-9", JobType: adminjob.JobTypeVirtualCandidatesRefresh, Status: adminjob.StatusCancelled,
		CreatedByUserID: 1, RequestedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}}
	h := newTestHandler(t, mediaCandidatesDeps(refresh))
	rec := do(t, h, http.MethodPost, "/api/v2/media/movie:heat-1995/virtual-candidates:refresh/cancel", "", owner)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ID != "job-9" || body.State != "canceled" {
		t.Fatalf("body = %+v", body)
	}

	// Authentication is required before the service runs.
	requireProblem(t, do(t, h, http.MethodPost, "/api/v2/media/movie:heat-1995/virtual-candidates:refresh/cancel", "", nil), TypeAuthenticationRequired)

	// A foreign job is a forbidden problem, not a silent cancellation.
	refresh = &fakeVirtualCandidatesRefresh{cancelErr: &handlers.APIError{Status: http.StatusForbidden, Code: "forbidden", Message: "This refresh belongs to another account"}}
	h = newTestHandler(t, mediaCandidatesDeps(refresh))
	requireProblem(t, do(t, h, http.MethodPost, "/api/v2/media/movie:heat-1995/virtual-candidates:refresh/cancel", "", owner), TypePermissionDenied)

	// Nothing to cancel is a conflict problem.
	refresh = &fakeVirtualCandidatesRefresh{cancelErr: &handlers.APIError{Status: http.StatusConflict, Code: "not_cancellable", Message: "There is no refresh to cancel"}}
	h = newTestHandler(t, mediaCandidatesDeps(refresh))
	requireProblem(t, do(t, h, http.MethodPost, "/api/v2/media/movie:heat-1995/virtual-candidates:refresh/cancel", "", owner), TypeJobNotCancelable)
}

func releaseRequestDeps(request *fakeVirtualReleaseRequest) Dependencies {
	deps := pilotDeps(nil, nil)
	deps.Watch = &fakeWatch{}
	deps.VirtualReleaseRequest = request
	return deps
}

func TestRequestVirtualRelease(t *testing.T) {
	request := &fakeVirtualReleaseRequest{result: handlers.IndexerReleaseRequestResult{ReleaseID: "42", State: "queued"}}
	h := newTestHandler(t, releaseRequestDeps(request))
	owner := with(bearer(memberToken), "X-Profile-Id", "p-owner")

	rec := do(t, h, http.MethodPost, "/api/v2/media/movie:heat-1995/virtual-releases/42:request", "", owner)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		ReleaseID string `json:"release_id"`
		State     string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ReleaseID != "42" || body.State != "queued" {
		t.Fatalf("body = %+v", body)
	}
	if request.calls != 1 || request.releaseID != 42 || request.contentID != "movie:heat-1995" {
		t.Fatalf("request call = %+v", request)
	}
	// The response and the whole handler never carry the stored download URL.
	if strings.Contains(rec.Body.String(), "http://") || strings.Contains(rec.Body.String(), "https://") {
		t.Fatalf("response leaks a URL: %s", rec.Body.String())
	}
}

func TestRequestVirtualReleaseAlreadyQueuedAndFailures(t *testing.T) {
	owner := with(bearer(memberToken), "X-Profile-Id", "p-owner")

	// A missing service is a dependency_unavailable problem.
	off := newTestHandler(t, pilotDeps(nil, nil))
	requireProblem(t, do(t, off, http.MethodPost, "/api/v2/media/movie:heat-1995/virtual-releases/42:request", "", owner), TypeDependencyUnavailable)

	// An unknown or foreign release id is a 404 problem.
	request := &fakeVirtualReleaseRequest{err: &handlers.APIError{Status: http.StatusNotFound, Code: "not_found", Message: "Release not found"}}
	h := newTestHandler(t, releaseRequestDeps(request))
	requireProblem(t, do(t, h, http.MethodPost, "/api/v2/media/movie:heat-1995/virtual-releases/99:request", "", owner), TypeNotFound)

	// Access denied surfaces as the service's problem (404 for an item the
	// caller cannot see).
	request = &fakeVirtualReleaseRequest{err: &handlers.APIError{Status: http.StatusNotFound, Code: "not_found", Message: "Watch target not found"}}
	h = newTestHandler(t, releaseRequestDeps(request))
	requireProblem(t, do(t, h, http.MethodPost, "/api/v2/media/movie:foreign/virtual-releases/42:request", "", owner), TypeNotFound)

	// A failed enqueue is answered 200 with state failed, so the row stays
	// retryable.
	request = &fakeVirtualReleaseRequest{result: handlers.IndexerReleaseRequestResult{ReleaseID: "42", State: "failed", Message: "The provider could not queue this release; try again."}}
	h = newTestHandler(t, releaseRequestDeps(request))
	rec := do(t, h, http.MethodPost, "/api/v2/media/movie:heat-1995/virtual-releases/42:request", "", owner)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		State   string `json:"state"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.State != "failed" || body.Message == "" {
		t.Fatalf("body = %+v", body)
	}
}

// TestRequestVirtualReleaseRejectsMalformedID pins the path-parameter
// validation: a non-numeric release id is a 422 before the service runs.
func TestRequestVirtualReleaseRejectsMalformedID(t *testing.T) {
	request := &fakeVirtualReleaseRequest{result: handlers.IndexerReleaseRequestResult{ReleaseID: "42", State: "queued"}}
	h := newTestHandler(t, releaseRequestDeps(request))
	owner := with(bearer(memberToken), "X-Profile-Id", "p-owner")
	requireProblem(t, do(t, h, http.MethodPost, "/api/v2/media/movie:heat-1995/virtual-releases/not-a-number:request", "", owner), TypeValidationFailed)
	if request.calls != 0 {
		t.Fatalf("malformed id reached the service %d times", request.calls)
	}
}
