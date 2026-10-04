package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
)

// fakeRefreshDetail is the watch-detail seam: it answers a fixed detail or a
// fixed error, and can answer a different detail after the refresh so a test
// can prove the response is the post-refresh list.
type fakeRefreshDetail struct {
	detail *catalog.WatchDetail
	post   *catalog.WatchDetail
	err    error
	calls  int
}

func (f *fakeRefreshDetail) WatchDetail(_ context.Context, _ int, _ string, _ string, _ catalog.AccessFilter) (*catalog.WatchDetail, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.calls > 1 && f.post != nil {
		return f.post, nil
	}
	return f.detail, nil
}

func refreshTestSource(id int, contentID, path string) *models.MediaFile {
	return &models.MediaFile{
		ID: id, ContentID: contentID, FilePath: path,
		Container: "virtual", MediaFolderID: 3, VirtualOwnerInstallationID: 5,
	}
}

func refreshTestDetail() *catalog.WatchDetail {
	return &catalog.WatchDetail{Versions: []catalog.FileVersion{{
		FileID: 7, Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
		AddedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}}}
}

// TestVirtualCandidatesRefreshServiceRefreshesAndReturnsList covers the happy
// path: the item's virtual source group is force-listed, persisted through the
// candidate sink with the provider-neutral source row, and the post-refresh
// watch-detail versions are returned.
func TestVirtualCandidatesRefreshServiceRefreshesAndReturnsList(t *testing.T) {
	source := refreshTestSource(7, "movie:x", "virtual://movie/x")
	candidate := refreshTestSource(8, "movie:x", "virtual://movie/x?result=old")

	var listPaths []string
	var persistedSource *models.MediaFile
	var persistedStreams []VirtualPlaybackStream
	detail := &fakeRefreshDetail{detail: refreshTestDetail()}
	svc := &VirtualCandidatesRefreshService{
		ListFresh: VirtualPlaybackStreamListerFunc(func(_ context.Context, path string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			listPaths = append(listPaths, path)
			return []VirtualPlaybackStream{{
				ID: "new", URI: path + "?result=new", Resolution: "2160p",
			}}, nil
		}),
		Persist: func(_ context.Context, s *models.MediaFile, streams []VirtualPlaybackStream) error {
			persistedSource = s
			persistedStreams = streams
			return nil
		},
		ContentFiles: func(_ context.Context, contentID string) ([]*models.MediaFile, error) {
			if contentID == "movie:x" {
				return []*models.MediaFile{candidate, source}, nil
			}
			return nil, nil
		},
		Detail: detail,
	}

	_, streams, err := refreshSources(svc, "movie:x", 1, "p-owner")
	if err != nil {
		t.Fatalf("RefreshSources: %v", err)
	}
	if len(listPaths) != 1 || listPaths[0] != "virtual://movie/x" {
		t.Fatalf("listed paths = %v, want the neutral source", listPaths)
	}
	if persistedSource == nil || persistedSource.FilePath != "virtual://movie/x" || persistedSource.VirtualOwnerInstallationID != 5 {
		t.Fatalf("persisted source = %+v", persistedSource)
	}
	if len(persistedStreams) != 1 || persistedStreams[0].URI != "virtual://movie/x?result=new" {
		t.Fatalf("persisted streams = %+v", persistedStreams)
	}
	if len(streams) != 1 || streams[0].URI != "virtual://movie/x?result=new" {
		t.Fatalf("returned streams = %+v, want the freshly listed set", streams)
	}
	// RefreshSources does not read the detail: access was enforced when the job
	// was accepted.
	if detail.calls != 0 {
		t.Fatalf("detail reads = %d, want 0", detail.calls)
	}
}

// refreshSources drives the job's provider step for a test.
func refreshSources(svc *VirtualCandidatesRefreshService, contentID string, userID int, profileID string) ([]*models.MediaFile, []VirtualPlaybackStream, error) {
	return svc.RefreshSources(context.Background(), contentID, userID, profileID)
}

func refreshSourcesCtx(svc *VirtualCandidatesRefreshService, ctx context.Context, contentID string, userID int, profileID string) ([]*models.MediaFile, []VirtualPlaybackStream, error) {
	return svc.RefreshSources(ctx, contentID, userID, profileID)
}

// TestVirtualCandidatesRefreshServiceEpisodeSource proves an episode content id
// resolves its source through the episode lookup and still lists the neutral
// group.
func TestVirtualCandidatesRefreshServiceEpisodeSource(t *testing.T) {
	episode := refreshTestSource(21, "series:y", "virtual://series/y/1/2")
	var listed string
	persisted := 0
	svc := &VirtualCandidatesRefreshService{
		ListFresh: VirtualPlaybackStreamListerFunc(func(_ context.Context, path string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			listed = path
			return []VirtualPlaybackStream{{ID: "a", URI: path + "?result=a"}}, nil
		}),
		Persist: func(_ context.Context, _ *models.MediaFile, _ []VirtualPlaybackStream) error {
			persisted++
			return nil
		},
		EpisodeFiles: func(_ context.Context, episodeID string) ([]*models.MediaFile, error) {
			if episodeID == "episode:y:1:2" {
				return []*models.MediaFile{episode}, nil
			}
			return nil, nil
		},
		Detail: &fakeRefreshDetail{detail: refreshTestDetail()},
	}
	if _, _, err := refreshSources(svc, "episode:y:1:2", 1, "p"); err != nil {
		t.Fatalf("RefreshVirtualCandidates: %v", err)
	}
	if listed != "virtual://series/y/1/2" || persisted != 1 {
		t.Fatalf("listed=%q persisted=%d", listed, persisted)
	}
}

// TestVirtualCandidatesRefreshServiceNonVirtualIsClientError proves an item
// with no virtual rows is a 4xx problem, not a provider failure.
func TestVirtualCandidatesRefreshServiceNonVirtualIsClientError(t *testing.T) {
	svc := &VirtualCandidatesRefreshService{
		ListFresh: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
			t.Fatal("provider listed for a non-virtual item")
			return nil, nil
		}),
		Persist:      func(context.Context, *models.MediaFile, []VirtualPlaybackStream) error { return nil },
		ContentFiles: func(context.Context, string) ([]*models.MediaFile, error) { return nil, nil },
		Detail:       &fakeRefreshDetail{detail: refreshTestDetail()},
	}
	_, _, err := refreshSources(svc, "movie:local", 1, "p")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnprocessableEntity {
		t.Fatalf("err = %v, want a 422 APIError", err)
	}
}

// TestVirtualCandidatesRefreshServiceProviderFailureIsRetryable proves a
// provider failure is a retryable 503 and never persists.
func TestVirtualCandidatesRefreshServiceProviderFailureIsRetryable(t *testing.T) {
	source := refreshTestSource(7, "movie:x", "virtual://movie/x")
	persisted := 0
	svc := &VirtualCandidatesRefreshService{
		ListFresh: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
			return nil, errors.New("provider offline")
		}),
		Persist: func(context.Context, *models.MediaFile, []VirtualPlaybackStream) error {
			persisted++
			return nil
		},
		ContentFiles: func(context.Context, string) ([]*models.MediaFile, error) {
			return []*models.MediaFile{source}, nil
		},
		Detail: &fakeRefreshDetail{detail: refreshTestDetail()},
	}
	_, _, err := refreshSources(svc, "movie:x", 1, "p")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable {
		t.Fatalf("err = %v, want a 503 APIError", err)
	}
	if persisted != 0 {
		t.Fatalf("persist calls = %d, want 0 on provider failure", persisted)
	}
}

// TestVirtualCandidatesRefreshServiceEmptyListingIsRetryable proves a
// zero-count provider answer is treated as a retryable provider failure and
// never reaches the persistence sink: an empty listing must not overwrite or
// sweep the stored candidate state.
func TestVirtualCandidatesRefreshServiceEmptyListingIsRetryable(t *testing.T) {
	source := refreshTestSource(7, "movie:x", "virtual://movie/x")
	persisted := 0
	svc := &VirtualCandidatesRefreshService{
		ListFresh: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{}, nil
		}),
		Persist: func(context.Context, *models.MediaFile, []VirtualPlaybackStream) error {
			persisted++
			return nil
		},
		ContentFiles: func(context.Context, string) ([]*models.MediaFile, error) {
			return []*models.MediaFile{source}, nil
		},
		Detail: &fakeRefreshDetail{detail: refreshTestDetail()},
	}
	_, _, err := refreshSources(svc, "movie:x", 1, "p")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable {
		t.Fatalf("err = %v, want a retryable 503 APIError for an empty listing", err)
	}
	if persisted != 0 {
		t.Fatalf("persist calls = %d, want 0 on an empty listing", persisted)
	}
}

// TestVirtualCandidatesRefreshServiceCoalescesConcurrentCalls proves two
// concurrent refreshes of one title share a single provider re-list instead of
// storming the provider.
func TestVirtualCandidatesRefreshServiceCoalescesConcurrentCalls(t *testing.T) {
	source := refreshTestSource(7, "movie:x", "virtual://movie/x")
	var listCalls int32
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	svc := &VirtualCandidatesRefreshService{
		ListFresh: VirtualPlaybackStreamListerFunc(func(_ context.Context, path string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			if atomic.AddInt32(&listCalls, 1) == 1 {
				enteredOnce.Do(func() { close(entered) })
				<-release
			}
			return []VirtualPlaybackStream{{ID: "a", URI: path + "?result=a"}}, nil
		}),
		Persist: func(context.Context, *models.MediaFile, []VirtualPlaybackStream) error { return nil },
		ContentFiles: func(context.Context, string) ([]*models.MediaFile, error) {
			return []*models.MediaFile{source}, nil
		},
		Detail: &fakeRefreshDetail{detail: refreshTestDetail()},
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, errs[i] = refreshSources(svc, "movie:x", 1, "p")
		}(i)
	}
	// Wait until the first caller is inside the provider list, then give the
	// second caller a moment to join the in-flight refresh before releasing.
	<-entered
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("refresh %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&listCalls); got != 1 {
		t.Fatalf("provider list calls = %d, want 1 (coalesced)", got)
	}
}

// TestVirtualCandidatesRefreshServiceWaiterCancelDoesNotAbortSharedWork proves
// a waiter that disconnects does not cancel the shared provider re-list. The
// proof is channel-synced, not timing: the provider mock blocks until
// released, the cancel lands while it is blocked, and the test then waits for
// the background persist to land. If the shared work were bound to the waiter
// (the old bug), the persist would never run and the wait times out.
func TestVirtualCandidatesRefreshServiceWaiterCancelDoesNotAbortSharedWork(t *testing.T) {
	source := refreshTestSource(8, "movie:y", "virtual://movie/y")
	var listCalls int32
	entered := make(chan struct{})
	release := make(chan struct{})
	persisted := make(chan struct{})
	var enteredOnce, persistedOnce sync.Once
	svc := &VirtualCandidatesRefreshService{
		ListFresh: VirtualPlaybackStreamListerFunc(func(ctx context.Context, path string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			atomic.AddInt32(&listCalls, 1)
			enteredOnce.Do(func() { close(entered) })
			<-release
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}
			return []VirtualPlaybackStream{{ID: "a", URI: path + "?result=a"}}, nil
		}),
		Persist: func(context.Context, *models.MediaFile, []VirtualPlaybackStream) error {
			persistedOnce.Do(func() { close(persisted) })
			return nil
		},
		ContentFiles: func(context.Context, string) ([]*models.MediaFile, error) {
			return []*models.MediaFile{source}, nil
		},
		Detail: &fakeRefreshDetail{detail: refreshTestDetail()},
	}

	// One waiter joins the shared refresh; entered proves the provider call
	// is running inside the singleflight registration.
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancelErr := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		_, _, err := refreshSourcesCtx(svc, cancelCtx, "movie:y", 1, "p")
		cancelErr <- err
	}()
	<-started
	<-entered
	// The waiter disconnects while the provider is blocked. It must observe
	// its own interruption, while the shared work continues detached. Bound
	// the wait: with the old blocking behavior this receive would hang
	// forever instead of reaching any later timeout.
	cancel()
	select {
	case err := <-cancelErr:
		if err == nil {
			t.Fatal("canceled waiter got nil error, want an interruption error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("canceled waiter never returned: shared work is bound to the waiter context")
	}
	close(release)
	select {
	case <-persisted:
	case <-time.After(10 * time.Second):
		t.Fatal("shared provider work was aborted by the waiter cancel: persist never ran")
	}

	// A fresh waiter afterwards gets a good list — the cancel neither aborted
	// the first wave nor poisoned the shared state. No provider-call assertion
	// here: singleflight forgets completed calls, so a fresh waiter may join
	// the first wave (1 call) or start its own (2 calls) depending on timing,
	// and either is correct.
	if _, _, err := refreshSources(svc, "movie:y", 1, "p"); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if got := atomic.LoadInt32(&listCalls); got < 1 || got > 2 {
		t.Fatalf("provider list calls = %d, want 1 or 2 (join or fresh wave)", got)
	}
}

// TestVirtualCandidatesRefreshServiceProviderCauseSurfaces proves a failed
// provider listing keeps its underlying cause in the retryable 503: the async
// job persists exactly this message, so the version-list banner can only show
// what is kept here. Transport errors embed the request URL, which may carry
// credentials, so only the host plus the root cause may surface.
func TestVirtualCandidatesRefreshServiceProviderCauseSurfaces(t *testing.T) {
	cases := []struct {
		name    string
		listErr error
		wantSub string
		notSub  []string
	}{
		{
			name:    "id translation failure passes through",
			listErr: fmt.Errorf("%w: %w", ErrVirtualRefreshProvider, errors.New("TVMaze returned status 429 for TVDB series ID 424536")),
			wantSub: "TVMaze returned status 429 for TVDB series ID 424536",
		},
		{
			name:    "fail-fast backoff passes through",
			listErr: fmt.Errorf("%w: %w", ErrVirtualRefreshProvider, errors.New("provider listing failed recently")),
			wantSub: "provider listing failed recently",
		},
		{
			name: "transport error is reduced to host plus root cause",
			listErr: fmt.Errorf("%w: %w", ErrVirtualRefreshProvider,
				fmt.Errorf("lookup TVDB series ID 424536: %w", &url.Error{
					Op:  "Get",
					URL: "https://provider.example/streams?token=secret-token-123",
					Err: errors.New("connection refused"),
				})),
			wantSub: "provider.example: connection refused",
			notSub:  []string{"secret-token-123", "https://provider.example"},
		},
		{
			name:    "bare sentinel keeps the generic message",
			listErr: ErrVirtualRefreshProvider,
			wantSub: "The provider could not be reached; try again.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := refreshTestSource(7, "series:x", "virtual://series/x")
			svc := &VirtualCandidatesRefreshService{
				ListFresh: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
					return nil, tc.listErr
				}),
				Persist: func(context.Context, *models.MediaFile, []VirtualPlaybackStream) error {
					t.Fatal("persist must not run on provider failure")
					return nil
				},
				ContentFiles: func(context.Context, string) ([]*models.MediaFile, error) {
					return []*models.MediaFile{source}, nil
				},
				Detail: &fakeRefreshDetail{detail: refreshTestDetail()},
			}
			_, _, err := refreshSources(svc, "series:x", 1, "p")
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable {
				t.Fatalf("err = %v, want a 503 APIError", err)
			}
			if !strings.Contains(apiErr.Message, tc.wantSub) {
				t.Fatalf("message = %q, want substring %q", apiErr.Message, tc.wantSub)
			}
			for _, banned := range tc.notSub {
				if strings.Contains(apiErr.Message, banned) {
					t.Fatalf("message = %q, must not contain %q", apiErr.Message, banned)
				}
			}
		})
	}
}
