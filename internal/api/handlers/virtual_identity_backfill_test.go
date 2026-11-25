package handlers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/adminjob"
	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/scanner"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

// fakeBackfillStore is an in-memory catalog: a fill removes the row from the
// legacy set, exactly as the real FillVirtualProviderIdentity removes it from
// the selection predicate, so the executor's page loop terminates the way the
// production pass does.
type fakeBackfillStore struct {
	rows    []*models.MediaFile
	filled  map[int]scanner.VirtualProviderIdentity
	noWrite map[int]bool
	listErr error
	fillErr error
}

func (f *fakeBackfillStore) ListVirtualIdentityBackfillRows(_ context.Context, afterID, limit int) ([]*models.MediaFile, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []*models.MediaFile
	for _, row := range f.rows {
		if row.ID <= afterID {
			continue
		}
		if f.filled != nil {
			if _, ok := f.filled[row.ID]; ok {
				continue
			}
		}
		out = append(out, row)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *fakeBackfillStore) CountVirtualIdentityBackfillRows(_ context.Context) (int, error) {
	if f.listErr != nil {
		return 0, f.listErr
	}
	n := 0
	for _, row := range f.rows {
		if f.filled != nil {
			if _, ok := f.filled[row.ID]; ok {
				continue
			}
		}
		n++
	}
	return n, nil
}

func (f *fakeBackfillStore) FillVirtualProviderIdentity(_ context.Context, fileID int, _ string, identity scanner.VirtualProviderIdentity) (bool, error) {
	if f.fillErr != nil {
		return false, f.fillErr
	}
	if f.noWrite[fileID] {
		return false, nil
	}
	if f.filled == nil {
		f.filled = map[int]scanner.VirtualProviderIdentity{}
	}
	if _, ok := f.filled[fileID]; ok {
		return false, nil
	}
	f.filled[fileID] = identity
	return true, nil
}

// fakeBackfillLister answers a fixed listing per group path.
type fakeBackfillLister struct {
	byPath map[string][]virtuallibrary.PlaybackStream
	errs   map[string]error
	calls  []string
}

func (f *fakeBackfillLister) list(_ context.Context, path string) ([]virtuallibrary.PlaybackStream, error) {
	f.calls = append(f.calls, path)
	if err := f.errs[path]; err != nil {
		return nil, err
	}
	return f.byPath[path], nil
}

// fakeBackfillJobs records the bulk gate's decision without a database.
type fakeBackfillJobs struct {
	completed    bool
	completedErr error
	requeue      bool
	requeueErr   error
	requeued     int
	latest       time.Time
	latestOK     bool
	latestErr    error
	createErr    error
	created      []adminjob.CreateJobInput
}

func (f *fakeBackfillJobs) HasFinishedJobOfType(context.Context, string) (bool, error) {
	return f.completed, f.completedErr
}

func (f *fakeBackfillJobs) RequeueLatestFailedOfType(context.Context, string) (bool, error) {
	f.requeued++
	return f.requeue, f.requeueErr
}

func (f *fakeBackfillJobs) LatestJobRequestedAt(context.Context, string) (time.Time, bool, error) {
	return f.latest, f.latestOK, f.latestErr
}

func (f *fakeBackfillJobs) Create(_ context.Context, in adminjob.CreateJobInput) (*models.AdminJob, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.created = append(f.created, in)
	return &models.AdminJob{ID: "job-1", JobType: in.JobType}, nil
}

func legacyBackfillRow(id int, path, releaseName string, releaseSize int64) *models.MediaFile {
	return &models.MediaFile{
		ID:                         id,
		ContentID:                  "movie-1",
		MediaFolderID:              2,
		VirtualOwnerInstallationID: 5,
		FilePath:                   path,
		ProviderReleaseName:        releaseName,
		ProviderReleaseSize:        releaseSize,
	}
}

func runBackfill(t *testing.T, executor *VirtualIdentityBackfillExecutor, resume adminjob.VirtualIdentityBackfillResume) *adminjob.VirtualIdentityBackfillResult {
	t.Helper()
	result, err := executor.Execute(context.Background(), adminjob.VirtualIdentityBackfillRequest{}, resume, func(int, int, int, string) {})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return result
}

// TestVirtualIdentityBackfillWritesNameSizeMatch proves the backfill adopts a
// fresh candidate's stronger identity for a legacy row whose only durable tier
// is a corroborated release name + size.
func TestVirtualIdentityBackfillWritesNameSizeMatch(t *testing.T) {
	path := "virtual://movie/tt1"
	stream := virtuallibrary.PlaybackStream{
		ID:                  "fresh",
		ProviderVideoHash:   "hash-1",
		ProviderGUID:        "guid-1",
		ProviderReleaseName: "Movie.2024.1080p",
		FileSize:            8_000_000_000,
	}
	store := &fakeBackfillStore{rows: []*models.MediaFile{
		legacyBackfillRow(1, path+"?result=stale", "Movie.2024.1080p", 8_100_000_000),
	}}
	lister := &fakeBackfillLister{byPath: map[string][]virtuallibrary.PlaybackStream{path: {stream}}}
	executor := &VirtualIdentityBackfillExecutor{Lister: lister.list, Store: store}

	result := runBackfill(t, executor, adminjob.VirtualIdentityBackfillResume{})
	if result.RowsMatched != 1 || result.RowsUnmatched != 0 {
		t.Fatalf("result = %+v, want one name-tier match", result)
	}
	if result.Tiers["release_name"] != 1 {
		t.Fatalf("tiers = %v, want release_name", result.Tiers)
	}
	if result.Cursor != 1 || result.RowsScanned != 1 || result.RowsTotal != 1 {
		t.Fatalf("progress = %+v, want cursor 1 of 1 scanned", result)
	}
	got := store.filled[1]
	if got.VideoHash != "hash-1" || got.GUID != "guid-1" || got.ReleaseName != "Movie.2024.1080p" || got.ReleaseSize != 8_000_000_000 {
		t.Fatalf("filled identity = %+v, want the candidate's stronger tiers", got)
	}
}

// TestVirtualIdentityBackfillMatchesStableListingID proves a row whose provider
// still lists its exact result id is bound to that candidate even when the
// release name does not corroborate, because the listing id is direct evidence
// of the same entry rather than an invented name match.
func TestVirtualIdentityBackfillMatchesStableListingID(t *testing.T) {
	path := "virtual://movie/tt1"
	stream := virtuallibrary.PlaybackStream{
		ID:                  "keep",
		ProviderVideoHash:   "hash-keep",
		ProviderReleaseName: "Some.Other.Release.2025",
		FileSize:            4_000_000_000,
	}
	store := &fakeBackfillStore{rows: []*models.MediaFile{
		legacyBackfillRow(1, path+"?result=keep", "Movie.2024.1080p", 8_000_000_000),
	}}
	lister := &fakeBackfillLister{byPath: map[string][]virtuallibrary.PlaybackStream{path: {stream}}}
	executor := &VirtualIdentityBackfillExecutor{Lister: lister.list, Store: store}

	result := runBackfill(t, executor, adminjob.VirtualIdentityBackfillResume{})
	if result.Tiers["listing"] != 1 {
		t.Fatalf("tiers = %v, want a listing-id match; result %+v", result.Tiers, result)
	}
	if store.filled[1].VideoHash != "hash-keep" {
		t.Fatalf("filled identity = %+v, want the stable listing candidate", store.filled[1])
	}
}

// TestVirtualIdentityBackfillRefusesBareName proves the central prohibition: a
// row that carries a release name but no size cannot invent identity from a
// name coincidence.
func TestVirtualIdentityBackfillRefusesBareName(t *testing.T) {
	path := "virtual://movie/tt1"
	stream := virtuallibrary.PlaybackStream{
		ID:                  "fresh",
		ProviderVideoHash:   "hash-1",
		ProviderReleaseName: "Movie.2024.1080p",
		FileSize:            8_000_000_000,
	}
	store := &fakeBackfillStore{rows: []*models.MediaFile{
		legacyBackfillRow(1, path+"?result=stale", "Movie.2024.1080p", 0),
	}}
	lister := &fakeBackfillLister{byPath: map[string][]virtuallibrary.PlaybackStream{path: {stream}}}
	executor := &VirtualIdentityBackfillExecutor{Lister: lister.list, Store: store}

	result := runBackfill(t, executor, adminjob.VirtualIdentityBackfillResume{})
	if result.RowsMatched != 0 || result.RowsUnmatched != 1 {
		t.Fatalf("result = %+v, want the name coincidence refused", result)
	}
	if len(store.filled) != 0 {
		t.Fatalf("identity was invented from a bare name: %+v", store.filled)
	}
}

// TestVirtualIdentityBackfillProviderFailureStaysRetriable proves one
// unreachable provider does not abort the pass or advance the watermark past
// its page, leaves another group's match intact, and is retried on a resumed
// run once the provider recovers.
func TestVirtualIdentityBackfillProviderFailureStaysRetriable(t *testing.T) {
	badPath := "virtual://movie/tt-bad"
	goodPath := "virtual://movie/tt-good"
	stream := virtuallibrary.PlaybackStream{
		ID:                  "fresh",
		ProviderVideoHash:   "hash-good",
		ProviderReleaseName: "Good.Movie.2024.1080p",
		FileSize:            6_000_000_000,
	}
	store := &fakeBackfillStore{rows: []*models.MediaFile{
		legacyBackfillRow(3, badPath+"?result=bad", "Bad.Movie.2024.1080p", 5_000_000_000),
		legacyBackfillRow(4, goodPath+"?result=old", "Good.Movie.2024.1080p", 6_100_000_000),
	}}
	lister := &fakeBackfillLister{
		byPath: map[string][]virtuallibrary.PlaybackStream{goodPath: {stream}},
		errs:   map[string]error{badPath: errors.New("provider offline")},
	}
	executor := &VirtualIdentityBackfillExecutor{Lister: lister.list, Store: store}

	result := runBackfill(t, executor, adminjob.VirtualIdentityBackfillResume{})
	if result.GroupsFailed != 1 || result.RowsMatched != 1 || result.RowsUnmatched != 1 {
		t.Fatalf("result = %+v, want one failed group and one match", result)
	}
	if len(store.filled) != 1 || store.filled[4].VideoHash != "hash-good" {
		t.Fatalf("filled = %+v, want only the reachable group's row", store.filled)
	}
	if result.Cursor != 0 || result.RowsScanned != 2 || result.RowsTotal != 2 {
		t.Fatalf("progress = %+v, want the watermark held before the failed page", result)
	}

	// The provider recovers: the resumed run retries the failed group from the
	// watermark instead of skipping it.
	recovered := &fakeBackfillLister{byPath: map[string][]virtuallibrary.PlaybackStream{
		badPath:  {{ID: "fresh", ProviderVideoHash: "hash-bad", ProviderReleaseName: "Bad.Movie.2024.1080p", FileSize: 5_000_000_000}},
		goodPath: {stream},
	}}
	executor = &VirtualIdentityBackfillExecutor{Lister: recovered.list, Store: store}
	second := runBackfill(t, executor, adminjob.VirtualIdentityBackfillResume{Cursor: result.Cursor})
	if second.GroupsFailed != 0 || second.RowsMatched != 1 {
		t.Fatalf("resumed result = %+v, want the previously failed row matched", second)
	}
	if second.Cursor != 3 || store.filled[3].VideoHash != "hash-bad" {
		t.Fatalf("resumed cursor = %d, filled = %+v, want row 3 matched and cursor 3", second.Cursor, store.filled)
	}
}

// TestVirtualIdentityBackfillStopsAtLastFullyProcessedPage proves a failure in
// a later page holds the watermark at the page boundary before it, so a resumed
// run re-lists only the failed page rather than skipping its rows.
func TestVirtualIdentityBackfillStopsAtLastFullyProcessedPage(t *testing.T) {
	goodPath := "virtual://movie/tt-good"
	badPath := "virtual://movie/tt-bad"
	good := virtuallibrary.PlaybackStream{
		ID:                  "fresh",
		ProviderVideoHash:   "hash-good",
		ProviderReleaseName: "Good.Movie.2024.1080p",
		FileSize:            6_000_000_000,
	}
	store := &fakeBackfillStore{rows: []*models.MediaFile{
		legacyBackfillRow(5, goodPath+"?result=old", "Good.Movie.2024.1080p", 6_100_000_000),
		legacyBackfillRow(11, badPath+"?result=bad", "Bad.Movie.2024.1080p", 5_000_000_000),
	}}
	lister := &fakeBackfillLister{
		byPath: map[string][]virtuallibrary.PlaybackStream{goodPath: {good}},
		errs:   map[string]error{badPath: errors.New("provider offline")},
	}
	executor := &VirtualIdentityBackfillExecutor{Lister: lister.list, Store: store, batchSize: 1}

	result := runBackfill(t, executor, adminjob.VirtualIdentityBackfillResume{})
	if result.Cursor != 5 || result.GroupsFailed != 1 || result.RowsMatched != 1 {
		t.Fatalf("result = %+v, want the cursor held at the first fully-processed page boundary", result)
	}
	if len(lister.calls) != 2 || lister.calls[0] != goodPath || lister.calls[1] != badPath {
		t.Fatalf("listed groups = %v, want the good page then the failed page", lister.calls)
	}
}

// TestVirtualIdentityBackfillResumesFromCursor proves a requeued claim starts
// after the persisted cursor instead of re-listing earlier groups.
func TestVirtualIdentityBackfillResumesFromCursor(t *testing.T) {
	earlyPath := "virtual://movie/tt-early"
	latePath := "virtual://movie/tt-late"
	stream := virtuallibrary.PlaybackStream{
		ID:                  "fresh",
		ProviderVideoHash:   "hash-late",
		ProviderReleaseName: "Late.Movie.2024.1080p",
		FileSize:            7_000_000_000,
	}
	store := &fakeBackfillStore{rows: []*models.MediaFile{
		legacyBackfillRow(5, earlyPath+"?result=old", "Early.Movie.2024.1080p", 3_000_000_000),
		legacyBackfillRow(11, latePath+"?result=old", "Late.Movie.2024.1080p", 7_100_000_000),
	}}
	lister := &fakeBackfillLister{byPath: map[string][]virtuallibrary.PlaybackStream{
		earlyPath: {
			{ID: "fresh", ProviderVideoHash: "hash-early", ProviderReleaseName: "Early.Movie.2024.1080p", FileSize: 3_000_000_000},
		},
		latePath: {stream},
	}}
	executor := &VirtualIdentityBackfillExecutor{Lister: lister.list, Store: store}

	result := runBackfill(t, executor, adminjob.VirtualIdentityBackfillResume{Cursor: 10})
	if len(lister.calls) != 1 || lister.calls[0] != latePath {
		t.Fatalf("listed groups = %v, want only the group after the cursor", lister.calls)
	}
	if len(store.filled) != 1 || store.filled[11].VideoHash != "hash-late" {
		t.Fatalf("filled = %+v, want only the late row", store.filled)
	}
	if _, ok := store.filled[5]; ok {
		t.Fatal("a row before the resume cursor was reworked")
	}
	if result.Cursor != 11 || result.RowsScanned != 1 || result.RowsTotal != 2 {
		t.Fatalf("progress = %+v, want resume past id 10", result)
	}
}

// TestEnsureBulkBackfillJobGates proves the bulk gate: it queues only when
// legacy rows exist, no backfill has completed, a job owner is available, and
// the previous attempt is past its cooldown.
func TestEnsureBulkBackfillJobGates(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	newExecutor := func(store *fakeBackfillStore, jobs *fakeBackfillJobs) *VirtualIdentityBackfillExecutor {
		return &VirtualIdentityBackfillExecutor{Store: store, Jobs: jobs, nowFunc: func() time.Time { return now }}
	}

	baseStore := func() *fakeBackfillStore {
		return &fakeBackfillStore{rows: []*models.MediaFile{
			legacyBackfillRow(1, "virtual://movie/tt1?result=stale", "Movie.2024.1080p", 8_000_000_000),
		}}
	}

	// No admin owner: the FK would reject the job, so the gate is a no-op.
	jobs := &fakeBackfillJobs{}
	if err := newExecutor(baseStore(), jobs).EnsureBulkBackfillJob(context.Background(), 0); err != nil {
		t.Fatalf("owner 0: %v", err)
	}
	if len(jobs.created) != 0 {
		t.Fatal("queued a job without an owner")
	}

	// Already completed: the one-shot marker suppresses a re-run.
	jobs = &fakeBackfillJobs{completed: true}
	if err := newExecutor(baseStore(), jobs).EnsureBulkBackfillJob(context.Background(), 7); err != nil {
		t.Fatalf("completed: %v", err)
	}
	if len(jobs.created) != 0 {
		t.Fatal("queued a second one-shot backfill")
	}

	// A failed partial run is retriable past the cooldown: the failed job is
	// requeued with its watermark instead of a fresh job starting from zero.
	jobs = &fakeBackfillJobs{requeue: true, latest: now.Add(-2 * virtualIdentityBulkBackfillCooldown), latestOK: true}
	if err := newExecutor(baseStore(), jobs).EnsureBulkBackfillJob(context.Background(), 7); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if jobs.requeued != 1 || len(jobs.created) != 0 {
		t.Fatalf("requeued=%d created=%d, want the failed job revived, not a duplicate", jobs.requeued, len(jobs.created))
	}

	// A recent attempt is still cooling down, so a restart queues nothing even
	// though no job has finished. This is the restart storm's second gate.
	jobs = &fakeBackfillJobs{requeue: true, latest: now.Add(-time.Hour), latestOK: true}
	if err := newExecutor(baseStore(), jobs).EnsureBulkBackfillJob(context.Background(), 7); err != nil {
		t.Fatalf("cooldown: %v", err)
	}
	if jobs.requeued != 0 || len(jobs.created) != 0 {
		t.Fatalf("requeued=%d created=%d, want the cooldown to suppress a run", jobs.requeued, len(jobs.created))
	}

	// No legacy rows: nothing to do.
	jobs = &fakeBackfillJobs{}
	if err := newExecutor(&fakeBackfillStore{}, jobs).EnsureBulkBackfillJob(context.Background(), 7); err != nil {
		t.Fatalf("no rows: %v", err)
	}
	if len(jobs.created) != 0 {
		t.Fatal("queued a job with no legacy rows")
	}

	// Rows exist, never completed, owner present, no previous attempt: queue
	// exactly one.
	jobs = &fakeBackfillJobs{}
	if err := newExecutor(baseStore(), jobs).EnsureBulkBackfillJob(context.Background(), 7); err != nil {
		t.Fatalf("queue: %v", err)
	}
	if len(jobs.created) != 1 || jobs.created[0].JobType != adminjob.JobTypeVirtualIdentityBackfill {
		t.Fatalf("created = %+v, want one virtual identity backfill job", jobs.created)
	}
	if jobs.created[0].CreatedByUserID != 7 {
		t.Fatalf("owner = %d, want 7", jobs.created[0].CreatedByUserID)
	}
}

// TestQueueBulkIdentityBackfillAtBootIsOffByDefault proves the boot path never
// reaches the bulk gate unless the operator selected bulk: off and on_demand
// create no job, so no restart can start a sweep.
func TestQueueBulkIdentityBackfillAtBootIsOffByDefault(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	newExecutor := func(jobs *fakeBackfillJobs) *VirtualIdentityBackfillExecutor {
		return &VirtualIdentityBackfillExecutor{
			Store: &fakeBackfillStore{rows: []*models.MediaFile{
				legacyBackfillRow(1, "virtual://movie/tt1?result=stale", "Movie.2024.1080p", 8_000_000_000),
			}},
			Jobs:    jobs,
			nowFunc: func() time.Time { return now },
		}
	}

	for _, mode := range []string{"", config.VirtualIdentityRepairOff, config.VirtualIdentityRepairOnDemand, "nonsense"} {
		jobs := &fakeBackfillJobs{}
		QueueBulkIdentityBackfillAtBoot(context.Background(), mode, newExecutor(jobs), 7, nil)
		if len(jobs.created) != 0 {
			t.Fatalf("mode %q created a job at boot", mode)
		}
	}

	jobs := &fakeBackfillJobs{}
	QueueBulkIdentityBackfillAtBoot(context.Background(), config.VirtualIdentityRepairBulk, newExecutor(jobs), 7, nil)
	if len(jobs.created) != 1 {
		t.Fatalf("created = %d, want exactly one bulk job when the operator selected bulk", len(jobs.created))
	}

	// A nil executor is the virtual library being inactive, not an error.
	QueueBulkIdentityBackfillAtBoot(context.Background(), config.VirtualIdentityRepairBulk, nil, 7, nil)
}

var (
	_ VirtualIdentityBackfillStore = (*fakeBackfillStore)(nil)
	_ VirtualIdentityBackfillJobs  = (*fakeBackfillJobs)(nil)
)
