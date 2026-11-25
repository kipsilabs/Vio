package handlers

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/scanner"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

// fakeRepairStore is the catalog slice one repair touches: one item's rows and
// the fenced identity write.
type fakeRepairStore struct {
	mu      sync.Mutex
	rows    []*models.MediaFile
	filled  map[int]scanner.VirtualProviderIdentity
	readKey string
	getErr  error
	fillErr error
	// filledOnce closes on the first successful write, so a concurrent test can
	// wait on the repair's observable effect instead of sleeping.
	filledOnce sync.Once
	filledCh   chan struct{}
}

func newFakeRepairStore(rows ...*models.MediaFile) *fakeRepairStore {
	return &fakeRepairStore{rows: rows, filled: map[int]scanner.VirtualProviderIdentity{}, filledCh: make(chan struct{})}
}

func (f *fakeRepairStore) GetByContentID(_ context.Context, contentID string) ([]*models.MediaFile, error) {
	return f.itemRows(contentID)
}

func (f *fakeRepairStore) GetByEpisodeID(_ context.Context, episodeID string) ([]*models.MediaFile, error) {
	return f.itemRows(episodeID)
}

func (f *fakeRepairStore) itemRows(id string) ([]*models.MediaFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readKey = id
	if f.getErr != nil {
		return nil, f.getErr
	}
	out := make([]*models.MediaFile, 0, len(f.rows))
	for _, row := range f.rows {
		if row.ContentID == id || (row.EpisodeID != "" && row.EpisodeID == id) {
			out = append(out, row)
		}
	}
	return out, nil
}

func (f *fakeRepairStore) FillVirtualProviderIdentity(_ context.Context, fileID int, _ string, identity scanner.VirtualProviderIdentity) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fillErr != nil {
		return false, f.fillErr
	}
	if _, ok := f.filled[fileID]; ok {
		return false, nil
	}
	f.filled[fileID] = identity
	f.filledOnce.Do(func() { close(f.filledCh) })
	return true, nil
}

// fakeRepairLister counts provider listings and answers a fixed listing per
// candidate group. before runs ahead of the answer, so a test can hold a
// listing open and prove a second view coalesces into it.
type fakeRepairLister struct {
	mu     sync.Mutex
	byPath map[string][]virtuallibrary.PlaybackStream
	err    error
	calls  []string
	before func(path string)
}

func (f *fakeRepairLister) list(_ context.Context, path string) ([]virtuallibrary.PlaybackStream, error) {
	f.mu.Lock()
	f.calls = append(f.calls, path)
	before := f.before
	f.mu.Unlock()
	if before != nil {
		before(path)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.byPath[path], nil
}

func (f *fakeRepairLister) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func legacyRepairRow(id int, path string) *models.MediaFile {
	return &models.MediaFile{
		ID:                          id,
		ContentID:                   "movie-1",
		MediaFolderID:               2,
		VirtualOwnerInstallationID:  5,
		VirtualOwnerInstallationSet: true,
		Container:                   "virtual",
		FilePath:                    path,
	}
}

func matchedRepairStream(id string) virtuallibrary.PlaybackStream {
	return virtuallibrary.PlaybackStream{
		ID:                  id,
		ProviderVideoHash:   "hash-" + id,
		ProviderGUID:        "guid-" + id,
		ProviderReleaseName: "Movie.2024.1080p",
		FileSize:            8_000_000_000,
	}
}

// newTestRepairer builds a repairer whose detached work runs inline, so every
// test observes a completed repair without sleeping.
func newTestRepairer(lister *fakeRepairLister, store *fakeRepairStore) *VirtualIdentityRepairer {
	r := NewVirtualIdentityRepairer(lister.list, store, true, slog.New(slog.DiscardHandler))
	r.run = func(work func()) { work() }
	return r
}

// TestVirtualIdentityRepairOnItemViewWritesIdentity proves one movie detail view
// costs one provider listing for that item's candidate group, matches through
// the same matcher the bulk pass uses, and writes the identity.
func TestVirtualIdentityRepairOnItemViewWritesIdentity(t *testing.T) {
	path := "virtual://movie/tt1"
	store := newFakeRepairStore(legacyRepairRow(1, path+"?result=keep"))
	lister := &fakeRepairLister{byPath: map[string][]virtuallibrary.PlaybackStream{path: {matchedRepairStream("keep")}}}
	r := newTestRepairer(lister, store)

	r.ScheduleVirtualIdentityRepair(context.Background(), "movie-1", "movie")

	if got := lister.paths(); len(got) != 1 || got[0] != path {
		t.Fatalf("listings = %v, want one listing of %s", got, path)
	}
	if store.readKey != "movie-1" {
		t.Fatalf("row lookup = %q, want the movie content id", store.readKey)
	}
	got := store.filled[1]
	if got.VideoHash != "hash-keep" || got.GUID != "guid-keep" || got.ReleaseName != "Movie.2024.1080p" || got.ReleaseSize != 8_000_000_000 {
		t.Fatalf("filled identity = %+v, want the matched candidate's tiers", got)
	}
}

// TestVirtualIdentityRepairEpisodeUsesEpisodeRows proves the episode page reads
// the episode's own rows, not the series'.
func TestVirtualIdentityRepairEpisodeUsesEpisodeRows(t *testing.T) {
	path := "virtual://series/tt1/1/2"
	row := legacyRepairRow(4, path+"?result=keep")
	row.ContentID = ""
	row.EpisodeID = "ep-4"
	store := newFakeRepairStore(row)
	lister := &fakeRepairLister{byPath: map[string][]virtuallibrary.PlaybackStream{path: {matchedRepairStream("keep")}}}
	r := newTestRepairer(lister, store)

	r.ScheduleVirtualIdentityRepair(context.Background(), "ep-4", "episode")

	if store.readKey != "ep-4" {
		t.Fatalf("row lookup = %q, want the episode id", store.readKey)
	}
	if len(lister.paths()) != 1 || len(store.filled) != 1 {
		t.Fatalf("listings = %v filled = %v, want one listing and one write", lister.paths(), store.filled)
	}
}

// TestVirtualIdentityRepairNegativeCacheHoldsTheSecondView proves an item that
// listed and did not match is not re-listed within the cache window, and is
// retried after it.
func TestVirtualIdentityRepairNegativeCacheHoldsTheSecondView(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	path := "virtual://movie/tt1"
	store := newFakeRepairStore(legacyRepairRow(1, path+"?result=stale"))
	lister := &fakeRepairLister{byPath: map[string][]virtuallibrary.PlaybackStream{path: {matchedRepairStream("other")}}}
	r := newTestRepairer(lister, store)
	r.now = func() time.Time { return now }

	r.ScheduleVirtualIdentityRepair(context.Background(), "movie-1", "movie")
	if len(lister.paths()) != 1 {
		t.Fatalf("listings = %v, want one listing", lister.paths())
	}
	if len(store.filled) != 0 {
		t.Fatalf("identity was written from a non-matching listing: %v", store.filled)
	}

	r.ScheduleVirtualIdentityRepair(context.Background(), "movie-1", "movie")
	if len(lister.paths()) != 1 {
		t.Fatalf("listings = %v, want the second view inside the cache window to list nothing", lister.paths())
	}

	now = now.Add(virtualIdentityRepairNegativeTTL + time.Minute)
	r.ScheduleVirtualIdentityRepair(context.Background(), "movie-1", "movie")
	if len(lister.paths()) != 2 {
		t.Fatalf("listings = %v, want a retry once the cache window expired", lister.paths())
	}
}

// TestVirtualIdentityRepairSkipsItemWithIdentity proves the earliest guard: an
// item whose rows already carry a hash or GUID is never listed.
func TestVirtualIdentityRepairSkipsItemWithIdentity(t *testing.T) {
	path := "virtual://movie/tt1"
	row := legacyRepairRow(1, path+"?result=keep")
	row.ProviderVideoHash = "hash-already"
	store := newFakeRepairStore(row)
	lister := &fakeRepairLister{byPath: map[string][]virtuallibrary.PlaybackStream{path: {matchedRepairStream("keep")}}}
	r := newTestRepairer(lister, store)

	r.ScheduleVirtualIdentityRepair(context.Background(), "movie-1", "movie")

	if got := lister.paths(); len(got) != 0 {
		t.Fatalf("listings = %v, want none for an item that already has identity", got)
	}
	if len(store.filled) != 0 {
		t.Fatalf("wrote identity onto an item that already had it: %v", store.filled)
	}
}

// TestVirtualIdentityRepairIgnoresNonLeafDetails proves the trigger's type gate:
// a series or season detail has no rows of its own and must not fan out.
func TestVirtualIdentityRepairIgnoresNonLeafDetails(t *testing.T) {
	store := newFakeRepairStore(legacyRepairRow(1, "virtual://movie/tt1?result=keep"))
	lister := &fakeRepairLister{byPath: map[string][]virtuallibrary.PlaybackStream{"virtual://movie/tt1": {matchedRepairStream("keep")}}}
	r := newTestRepairer(lister, store)

	for _, mediaType := range []string{"series", "season", "", "collection"} {
		r.ScheduleVirtualIdentityRepair(context.Background(), "series-1", mediaType)
	}
	if got := lister.paths(); len(got) != 0 {
		t.Fatalf("listings = %v, want none for a non-leaf detail", got)
	}
	if len(store.filled) != 0 {
		t.Fatalf("wrote identity from a non-leaf detail: %v", store.filled)
	}
}

// TestVirtualIdentityRepairDisabledListsNothing proves off is inert: the
// default mode never reaches a provider.
func TestVirtualIdentityRepairDisabledListsNothing(t *testing.T) {
	store := newFakeRepairStore(legacyRepairRow(1, "virtual://movie/tt1?result=keep"))
	lister := &fakeRepairLister{byPath: map[string][]virtuallibrary.PlaybackStream{"virtual://movie/tt1": {matchedRepairStream("keep")}}}
	r := newTestRepairer(lister, store)
	r.Enabled = false

	r.ScheduleVirtualIdentityRepair(context.Background(), "movie-1", "movie")
	if got := lister.paths(); len(got) != 0 {
		t.Fatalf("listings = %v, want none when the mode is off", got)
	}
}

// TestVirtualIdentityRepairCoalescesConcurrentOpens proves a second view while
// the listing is in flight performs no second listing, and that the first view
// returned without waiting for the provider.
func TestVirtualIdentityRepairCoalescesConcurrentOpens(t *testing.T) {
	path := "virtual://movie/tt1"
	store := newFakeRepairStore(legacyRepairRow(1, path+"?result=keep"))
	lister := &fakeRepairLister{byPath: map[string][]virtuallibrary.PlaybackStream{path: {matchedRepairStream("keep")}}}

	listed := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	lister.before = func(string) {
		once.Do(func() { close(listed) })
		<-release
	}

	// The default seam starts the work on its own goroutine.
	r := NewVirtualIdentityRepairer(lister.list, store, true, nil)
	r.ScheduleVirtualIdentityRepair(context.Background(), "movie-1", "movie")
	<-listed
	// The first view's provider listing is still open; this view must coalesce.
	r.ScheduleVirtualIdentityRepair(context.Background(), "movie-1", "movie")
	if got := lister.paths(); len(got) != 1 {
		t.Fatalf("listings = %v, want the concurrent view to coalesce", got)
	}

	close(release)
	select {
	case <-store.filledCh:
	case <-time.After(5 * time.Second):
		t.Fatal("the repair never wrote identity")
	}
	if got := lister.paths(); len(got) != 1 {
		t.Fatalf("listings = %v, want exactly one for two concurrent opens", got)
	}
}

// TestVirtualIdentityRepairProviderFailureIsRetriable proves a provider outage
// is not negative-cached: the next view tries again.
func TestVirtualIdentityRepairProviderFailureIsRetriable(t *testing.T) {
	path := "virtual://movie/tt1"
	store := newFakeRepairStore(legacyRepairRow(1, path+"?result=keep"))
	lister := &fakeRepairLister{err: errors.New("provider down")}
	r := newTestRepairer(lister, store)

	r.ScheduleVirtualIdentityRepair(context.Background(), "movie-1", "movie")
	r.ScheduleVirtualIdentityRepair(context.Background(), "movie-1", "movie")

	if len(lister.paths()) != 2 {
		t.Fatalf("listings = %v, want a retry after a provider failure", lister.paths())
	}
}

// TestVirtualIdentityRepairGlobalRateLimitHolds proves the process-wide listing
// budget bounds browsing: once it is spent, another item's page lists nothing
// even though it shares no cache with the first.
func TestVirtualIdentityRepairGlobalRateLimitHolds(t *testing.T) {
	first := "virtual://movie/tt1"
	second := "virtual://movie/tt2"
	firstRow := legacyRepairRow(1, first+"?result=keep")
	secondRow := legacyRepairRow(2, second+"?result=keep")
	secondRow.ContentID = "movie-2"
	store := newFakeRepairStore(firstRow, secondRow)
	lister := &fakeRepairLister{byPath: map[string][]virtuallibrary.PlaybackStream{
		first:  {matchedRepairStream("keep")},
		second: {matchedRepairStream("keep")},
	}}
	r := newTestRepairer(lister, store)
	// One listing per minute, no burst: the first item spends the budget.
	r.limiter = rate.NewLimiter(rate.Every(time.Minute), 1)

	r.ScheduleVirtualIdentityRepair(context.Background(), "movie-1", "movie")
	r.ScheduleVirtualIdentityRepair(context.Background(), "movie-2", "movie")

	if got := lister.paths(); len(got) != 1 || got[0] != first {
		t.Fatalf("listings = %v, want only the first item inside one rate window", got)
	}
	if _, ok := store.filled[2]; ok {
		t.Fatal("wrote identity for an item the rate limit refused to list")
	}
}

// TestVirtualIdentityRepairBoundsGroupsPerItem proves one item view lists at
// most the bounded set of candidate groups.
func TestVirtualIdentityRepairBoundsGroupsPerItem(t *testing.T) {
	rows := []*models.MediaFile{
		legacyRepairRow(1, "virtual://movie/tt1?profile=a&result=keep"),
		legacyRepairRow(2, "virtual://movie/tt1?profile=b&result=keep"),
		legacyRepairRow(3, "virtual://movie/tt1?profile=c&result=keep"),
	}
	store := newFakeRepairStore(rows...)
	lister := &fakeRepairLister{}
	r := newTestRepairer(lister, store)
	// The default burst two admits exactly the cap.
	r.limiter = rate.NewLimiter(rate.Every(time.Minute), virtualIdentityRepairListBurst)

	r.ScheduleVirtualIdentityRepair(context.Background(), "movie-1", "movie")

	if got := lister.paths(); len(got) != virtualIdentityRepairMaxGroups {
		t.Fatalf("listings = %v, want at most %d groups", got, virtualIdentityRepairMaxGroups)
	}
}
