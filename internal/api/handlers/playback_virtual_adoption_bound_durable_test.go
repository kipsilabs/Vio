package handlers

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/resolver"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeAdoptionRefusalStore is a shared, deterministic stand-in for the durable
// Postgres store. One instance handed to two handlers models two replicas
// reading and writing the same rows; the error fields model a store outage.
type fakeAdoptionRefusalStore struct {
	mu        sync.Mutex
	counts    map[string]int
	recordErr error
	countErr  error
	clearErr  error
	records   int
}

func newFakeAdoptionRefusalStore() *fakeAdoptionRefusalStore {
	return &fakeAdoptionRefusalStore{counts: make(map[string]int)}
}

func fakeAdoptionRefusalKey(fileID int, expectedPath string) string {
	return strconv.Itoa(fileID) + "|" + expectedPath
}

func (s *fakeAdoptionRefusalStore) RecordVirtualAdoptionRefusal(_ context.Context, fileID int, expectedPath string, ttl time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records++
	if s.recordErr != nil {
		return 0, s.recordErr
	}
	key := fakeAdoptionRefusalKey(fileID, expectedPath)
	s.counts[key]++
	return s.counts[key], nil
}

func (s *fakeAdoptionRefusalStore) ClearVirtualAdoptionRefusal(_ context.Context, fileID int, expectedPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clearErr != nil {
		return s.clearErr
	}
	delete(s.counts, fakeAdoptionRefusalKey(fileID, expectedPath))
	return nil
}

func (s *fakeAdoptionRefusalStore) CountVirtualAdoptionRefusal(_ context.Context, fileID int, expectedPath string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.countErr != nil {
		return 0, s.countErr
	}
	return s.counts[fakeAdoptionRefusalKey(fileID, expectedPath)], nil
}

// TestVirtualAdoptionRefusalDurableStoreSharedAcrossReplicas proves the bound is
// cluster-wide: refusals recorded by one replica advance the count a different
// replica reads, so alternating replicas converge on the same terminal decision
// instead of each restarting at zero.
func TestVirtualAdoptionRefusalDurableStoreSharedAcrossReplicas(t *testing.T) {
	ctx := context.Background()
	store := newFakeAdoptionRefusalStore()
	row := adoptionBoundTestRow(5031920, "replica-hash")
	adopt := "virtual://series/tt39087958/1/1?profile=4K+Dolby+Vision&result=replica"
	args := models.VirtualFilePersistArgs{
		FileID:           row.ID,
		ExpectedFilePath: row.FilePath,
		AdoptPath:        adopt,
		RequireAdopt:     true,
	}
	replicaA := &PlaybackHandler{VirtualAdoptionRefusalStore: store}
	replicaB := &PlaybackHandler{VirtualAdoptionRefusalStore: store}

	// Replica A refuses twice, replica B once; neither reaches the bound alone.
	for i := 0; i < 2; i++ {
		if _, err := ExecVirtualFileMetadataUpdateResultWithRefusals(ctx, adoptionBoundRefusingDB{}, args, store); err == nil {
			t.Fatal("refused adoption reported success")
		}
		if replicaA.adoptionRefusalBoundReached(ctx, args) {
			t.Fatalf("replica A reached the bound after %d refusals, want it to need the cluster count", i+1)
		}
	}
	if _, err := ExecVirtualFileMetadataUpdateResultWithRefusals(ctx, adoptionBoundRefusingDB{}, args, store); err == nil {
		t.Fatal("refused adoption reported success")
	}
	if !replicaB.adoptionRefusalBoundReached(ctx, args) {
		t.Fatal("replica B did not see the refusals replica A recorded")
	}

	// A successful write on replica A clears the mark for replica B too.
	if _, err := ExecVirtualFileMetadataUpdateResultWithRefusals(ctx, adoptionBoundAdoptingDB{path: adopt}, args, store); err != nil {
		t.Fatalf("adopting write failed: %v", err)
	}
	if replicaB.adoptionRefusalBoundReached(ctx, args) {
		t.Fatal("the bound survived a successful write on another replica")
	}
}

// TestVirtualAdoptionRefusalDurableStoreRestartPreservesCount proves a restarted
// process continues the same count: a fresh handler reading the durable store
// carries the refusals the previous process recorded and reaches the bound
// without having to re-accumulate them.
func TestVirtualAdoptionRefusalDurableStoreRestartPreservesCount(t *testing.T) {
	ctx := context.Background()
	store := newFakeAdoptionRefusalStore()
	row := adoptionBoundTestRow(5031921, "restart-hash")
	adopt := "virtual://series/tt39087958/1/1?profile=4K+Dolby+Vision&result=restart"
	args := models.VirtualFilePersistArgs{
		FileID:           row.ID,
		ExpectedFilePath: row.FilePath,
		AdoptPath:        adopt,
		RequireAdopt:     true,
	}
	before := &PlaybackHandler{VirtualAdoptionRefusalStore: store}
	for i := 0; i < virtualAdoptionRefusalBound-1; i++ {
		if _, err := ExecVirtualFileMetadataUpdateResultWithRefusals(ctx, adoptionBoundRefusingDB{}, args, store); err == nil {
			t.Fatal("refused adoption reported success")
		}
	}
	if before.adoptionRefusalBoundReached(ctx, args) {
		t.Fatal("the bound was reached before the restart")
	}

	// A new process with no in-memory history reads the durable count.
	after := &PlaybackHandler{VirtualAdoptionRefusalStore: store}
	if got := countAdoptionRefusalStore(ctx, store, row.ID, row.FilePath); got != virtualAdoptionRefusalBound-1 {
		t.Fatalf("durable count after restart = %d, want %d", got, virtualAdoptionRefusalBound-1)
	}
	if _, err := ExecVirtualFileMetadataUpdateResultWithRefusals(ctx, adoptionBoundRefusingDB{}, args, store); err == nil {
		t.Fatal("refused adoption reported success")
	}
	if !after.adoptionRefusalBoundReached(ctx, args) {
		t.Fatal("the restarted process did not reach the bound from the preserved count")
	}
}

// TestVirtualAdoptionRefusalStoreOutageDoesNotCondemn proves a refusal-store
// outage is a recoverable infrastructure failure, not an identity verdict: the
// write stays refused, but the count is not advanced and the bound is not
// reached. Once the store answers again, refusals accumulate normally toward the
// bound the transactional reconciliation decides from.
func TestVirtualAdoptionRefusalStoreOutageDoesNotCondemn(t *testing.T) {
	ctx := context.Background()
	store := newFakeAdoptionRefusalStore()
	store.recordErr = errors.New("database unavailable")
	store.countErr = errors.New("database unavailable")

	row := adoptionBoundTestRow(5031922, "outage-hash")
	candidate := "virtual://series/tt39087958/1/1?profile=4K+Dolby+Vision&result=outage"
	probed := &models.MediaFile{ProviderVideoHash: "foreign-hash", AudioTracks: []models.AudioTrack{{Codec: "aac", Channels: 2}}}
	foreignIdentity := resolver.NewPersistedIdentityTiers("foreign-hash", "", "", 0)
	args := models.VirtualFilePersistArgs{
		FileID:           row.ID,
		ExpectedFilePath: row.FilePath,
		AdoptPath:        candidate,
		RequireAdopt:     true,
	}
	h := &PlaybackHandler{VirtualAdoptionRefusalStore: store}

	// The saver keeps refusing the write; the outage must not be counted.
	for i := 0; i < virtualAdoptionRefusalBound*2; i++ {
		if _, err := ExecVirtualFileMetadataUpdateResultWithRefusals(ctx, adoptionBoundRefusingDB{}, args, store); err == nil {
			t.Fatal("refused adoption reported success")
		}
	}
	if got, err := store.CountVirtualAdoptionRefusal(ctx, row.ID, row.FilePath); err == nil && got != 0 {
		t.Fatalf("store outage advanced the refusal count to %d, want 0", got)
	}
	if h.adoptionRefusalBoundReached(ctx, args) {
		t.Fatal("a store outage read as a reached bound")
	}

	// The args builder must fence the write regardless of the unreadable count;
	// the terminal decision belongs to the transactional reconciliation, not to
	// the args builder.
	if _, ok := h.virtualProbeEvidenceArgsForRow(ctx, row, candidate, probed, foreignIdentity, true); !ok {
		t.Fatal("the args builder refused before the SQL fence could")
	}

	// Recovery: the store answers and the refusals accumulate to the bound.
	store.recordErr = nil
	store.countErr = nil
	for i := 0; i < virtualAdoptionRefusalBound; i++ {
		if _, err := ExecVirtualFileMetadataUpdateResultWithRefusals(ctx, adoptionBoundRefusingDB{}, args, store); err == nil {
			t.Fatal("refused adoption reported success")
		}
	}
	if got := countAdoptionRefusalStore(ctx, store, row.ID, row.FilePath); got != virtualAdoptionRefusalBound {
		t.Fatalf("recovered refusal count = %d, want %d", got, virtualAdoptionRefusalBound)
	}
	recovered, ok := h.virtualProbeEvidenceArgsForRow(ctx, row, candidate, probed, foreignIdentity, true)
	if !ok {
		t.Fatal("the args builder refused after the store recovered")
	}
	if !recovered.RequireAdopt {
		t.Fatalf("recovered write lost its adoption fence: %+v", recovered)
	}
}

// TestVirtualProbeEvidenceOwnerLookupOutageDoesNotCondemn proves the second
// separation: a path-ownership lookup error is an infrastructure failure, not an
// identity refusal. Repeated lookup failures must neither advance the refusal
// count nor install the terminal failed_at verdict, so a database outage cannot
// suppress healthy content.
func TestVirtualProbeEvidenceOwnerLookupOutageDoesNotCondemn(t *testing.T) {
	ctx := context.Background()
	row := adoptionBoundTestRow(5031923, "row-hash")
	uri := "virtual://series/tt39087958/1/1?result=owner"
	backoffKey := virtualOwnerLookupBackoffKey(row.ID, uri)
	virtualOwnerLookups.clear(backoffKey)
	t.Cleanup(func() { virtualOwnerLookups.clear(backoffKey) })
	refusalKey := virtualAdoptionRefusalKey(row.ID, row.FilePath)
	virtualAdoptionRefusals.clear(refusalKey)
	t.Cleanup(func() { virtualAdoptionRefusals.clear(refusalKey) })

	terminalInstalls := 0
	h := &PlaybackHandler{
		fileResolver: erroringPathFileResolver{err: errors.New("catalog unavailable")},
		VirtualCandidateFailMarker: func(context.Context, int, string, *time.Time) error {
			terminalInstalls++
			return nil
		},
	}

	for i := 0; i < virtualAdoptionRefusalBound*2; i++ {
		if _, _, ok := h.virtualProbeEvidenceRotateTarget(ctx, row, uri, probedDualAudioFile(), noCandidateIdentity); ok {
			t.Fatal("an owner-lookup outage rotated or adopted the candidate")
		}
	}
	if terminalInstalls != 0 {
		t.Fatalf("an owner-lookup outage installed %d terminal verdicts, want 0", terminalInstalls)
	}
	if got := virtualAdoptionRefusals.count(refusalKey); got != 0 {
		t.Fatalf("an owner-lookup outage counted as %d identity refusals, want 0", got)
	}
}

// TestVirtualProbeEvidenceOwnerLookupBackoffRecovers proves the ownership
// lookup backs off while the store is failing and resumes as soon as the window
// lapses: no durable state is written, and a recovered lookup rotates to the
// owner row exactly as before.
func TestVirtualProbeEvidenceOwnerLookupBackoffRecovers(t *testing.T) {
	ctx := context.Background()
	const (
		content = "episode-tvdb-471090-1-1"
		uri     = "virtual://series/tt39087958/1/1?result=owner"
	)
	row := &models.MediaFile{
		ID: 5031924, ContentID: content, EpisodeID: content,
		FilePath:      "virtual://series/tt39087958/1/1?result=pinned",
		MediaFolderID: 42, VirtualOwnerInstallationID: 7, ProbeSource: "virtual",
		ProviderVideoHash: "row-hash",
	}
	ownerRow := &models.MediaFile{
		ID: 5031925, ContentID: content, EpisodeID: content, FilePath: uri,
		MediaFolderID: 42, VirtualOwnerInstallationID: 7, ProbeSource: "virtual",
		ProviderVideoHash: "different-release-hash",
	}
	backoffKey := virtualOwnerLookupBackoffKey(row.ID, uri)
	virtualOwnerLookups.clear(backoffKey)
	base := time.Now()
	previousNow := virtualOwnerLookups.now
	virtualOwnerLookups.now = func() time.Time { return base }
	t.Cleanup(func() {
		virtualOwnerLookups.now = previousNow
		virtualOwnerLookups.clear(backoffKey)
	})

	h := &PlaybackHandler{fileResolver: erroringPathFileResolver{err: errors.New("catalog unavailable")}}
	if _, _, ok := h.virtualProbeEvidenceRotateTarget(ctx, row, uri, probedDualAudioFile(), noCandidateIdentity); ok {
		t.Fatal("an owner-lookup outage rotated or adopted the candidate")
	}
	if !virtualOwnerLookups.recent(backoffKey) {
		t.Fatal("the owner-lookup outage did not start a backoff window")
	}

	// The store recovers, but the backoff is still active: the question is not
	// re-asked yet, and the write stays fenced.
	h.fileResolver = byPathPlaybackFileResolver{byPath: map[string]*models.MediaFile{uri: ownerRow}}
	if _, _, ok := h.virtualProbeEvidenceRotateTarget(ctx, row, uri, probedDualAudioFile(), noCandidateIdentity); ok {
		t.Fatal("the backoff window did not suppress the re-ask")
	}

	// After the window lapses the lookup is re-asked and the rotation resumes.
	base = base.Add(virtualOwnerLookups.window() + time.Second)
	rotated, evidence, ok := h.virtualProbeEvidenceRotateTarget(ctx, row, uri, probedDualAudioFile(), noCandidateIdentity)
	if !ok {
		t.Fatal("the recovered lookup did not rotate")
	}
	if rotated == nil || rotated.ID != ownerRow.ID || evidence == nil || evidence.ID != ownerRow.ID {
		t.Fatalf("recovered rotation targeted %+v, want owner row %d", rotated, ownerRow.ID)
	}
	if virtualOwnerLookups.recent(backoffKey) {
		t.Fatal("a successful lookup did not clear the backoff window")
	}
}

// adoptionBoundCollidingRetryDB models the unique-path collision that forces a
// metadata-only retry: the first adoption write fails with Postgres
// unique-violation 23505, and the retry (with no adopt path) writes evidence on
// the row's current path and reports it as persisted.
type adoptionBoundCollidingRetryDB struct {
	calls int
}

func (d *adoptionBoundCollidingRetryDB) QueryRow(context.Context, string, ...any) pgx.Row {
	d.calls++
	if d.calls == 1 {
		return adoptionBoundScanErrorRow{err: &pgconn.PgError{Code: "23505"}}
	}
	return adoptionBoundScanStringRow{path: "virtual://retained-pin"}
}

type adoptionBoundScanErrorRow struct{ err error }

func (r adoptionBoundScanErrorRow) Scan(...any) error { return r.err }

type adoptionBoundScanStringRow struct{ path string }

func (r adoptionBoundScanStringRow) Scan(dest ...any) error {
	if len(dest) > 0 {
		if out, ok := dest[0].(*string); ok {
			*out = r.path
		}
	}
	return nil
}

// TestVirtualAdoptionMetadataOnlyRetryClearsRefusalCount proves the clear-on-
// success path also covers the metadata-only retry after an adoption uniqueness
// collision. The retry writes evidence without adopting the requested identity,
// so the row now carries evidence and its pre-success refusals are stale: the
// next unrelated refusal must start at one instead of inheriting the old count
// and reaching the terminal threshold prematurely.
func TestVirtualAdoptionMetadataOnlyRetryClearsRefusalCount(t *testing.T) {
	ctx := context.Background()
	store := newFakeAdoptionRefusalStore()
	row := adoptionBoundTestRow(5031935, "retry-hash")
	args := models.VirtualFilePersistArgs{
		FileID:           row.ID,
		ExpectedFilePath: row.FilePath,
		AdoptPath:        "virtual://series/tt39087958/1/1?profile=4K+Dolby+Vision&result=collides",
		// The caller wants the metadata write but does not require the adoption,
		// so a unique-path collision falls into the retry path rather than being
		// refused outright.
		RequireAdopt: false,
	}
	// A populated durable counter left by an earlier refusal stream.
	for i := 0; i < virtualAdoptionRefusalBound; i++ {
		if _, err := store.RecordVirtualAdoptionRefusal(ctx, row.ID, row.FilePath, virtualAdoptionRefusalTTL); err != nil {
			t.Fatalf("seed refusal: %v", err)
		}
	}

	db := &adoptionBoundCollidingRetryDB{}
	result, err := ExecVirtualFileMetadataUpdateResultWithRefusals(ctx, db, args, store)
	if err != nil {
		t.Fatalf("metadata-only retry failed: %v", err)
	}
	if !result.MetadataUpdated || result.IdentityAdopted {
		t.Fatalf("retry result = %+v, want a metadata update without adoption", result)
	}
	if got := countAdoptionRefusalStore(ctx, store, row.ID, row.FilePath); got != 0 {
		t.Fatalf("refusal count after the metadata-only retry = %d, want 0", got)
	}

	// The next refusal is a fresh stream: one, not the terminal bound.
	refusing := models.VirtualFilePersistArgs{
		FileID:           row.ID,
		ExpectedFilePath: row.FilePath,
		AdoptPath:        "virtual://series/tt39087958/1/1?profile=4K+Dolby+Vision&result=next",
		RequireAdopt:     true,
	}
	if _, err := ExecVirtualFileMetadataUpdateResultWithRefusals(ctx, adoptionBoundRefusingDB{}, refusing, store); err == nil {
		t.Fatal("refused adoption reported success")
	}
	if got := countAdoptionRefusalStore(ctx, store, row.ID, row.FilePath); got != 1 {
		t.Fatalf("next refusal count = %d, want 1", got)
	}
	h := &PlaybackHandler{VirtualAdoptionRefusalStore: store}
	if h.adoptionRefusalBoundReached(ctx, refusing) {
		t.Fatal("the next refusal reached the terminal bound from a pre-success count")
	}
}
