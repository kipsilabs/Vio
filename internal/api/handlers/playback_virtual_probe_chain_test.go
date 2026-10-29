package handlers

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/access"
	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/notifications"
)

// The pre-play probe chain must never block its caller, never probe bytes it
// cannot name, and must terminate on its own budget and free its slot. These
// tests pin the admission contract and the skip rules with stub targets and
// probers; the probe tail itself (damper, evidence write) is covered by the
// machinery tests it already shares.

const probeChainTestContent = "movie-probe-chain"

func probeChainProviderURL(row *models.MediaFile) string {
	return fmt.Sprintf("https://probe.example/stream/%d", row.ID)
}

func probeChainRow(fileID int, filePath string, probeUpdatedAt *time.Time) *models.MediaFile {
	row := &models.MediaFile{
		ID: fileID, ContentID: probeChainTestContent, FilePath: filePath,
		MediaFolderID: 77, VirtualOwnerInstallationID: 5, ProbeSource: "virtual",
		ProbeUpdatedAt: probeUpdatedAt,
	}
	row.ResolvedURL = probeChainProviderURL(row)
	return row
}

func probeChainStreams(row *models.MediaFile) []VirtualPlaybackStream {
	return []VirtualPlaybackStream{
		{URI: "virtual://movie/probe-chain?result=c5", OwnerInstallationID: 5, Resolution: "1080p"},
		{URI: row.FilePath, ProviderURL: probeChainProviderURL(row), OwnerInstallationID: row.VirtualOwnerInstallationID, Resolution: "2160p",
			AudioLanguages: []string{"eng"}, SubtitleLanguages: []string{"eng"}},
	}
}

func probeChainSourceRow() *models.MediaFile {
	return &models.MediaFile{
		ID: 300, ContentID: probeChainTestContent, FilePath: "virtual://movie/probe-chain",
		MediaFolderID: 77, VirtualOwnerInstallationID: 5, ProbeSource: "virtual",
	}
}

// blockedDefaultTarget gates the default-version re-read: the chain stays
// inside admitted work until released, so tests can hold the chain at that
// exact point and inspect admission behavior while it is live.
type blockedDefaultTarget struct {
	mu     sync.Mutex
	open   chan struct{}
	callN  int
	row    *models.MediaFile
	filter catalog.AccessFilter
}

func (b *blockedDefaultTarget) DefaultVirtualProbeTarget(ctx context.Context, contentID string, filter catalog.AccessFilter) (*models.MediaFile, error) {
	b.mu.Lock()
	b.callN++
	b.filter = filter
	b.mu.Unlock()
	select {
	case <-b.open:
		return b.row, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (b *blockedDefaultTarget) lastFilter() catalog.AccessFilter {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.filter
}

func (b *blockedDefaultTarget) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.callN
}

func (b *blockedDefaultTarget) releaseRow() {
	select {
	case <-b.open:
	default:
		close(b.open)
	}
}

// recordingProber records each probe's URL. The chain's probe tail persists
// through the queued evidence path; without a saver the write is refused
// harmlessly, so the stub only answers the enumeration itself.
type recordingProber struct {
	mu   sync.Mutex
	urls []string
	hits chan struct{}
}

func (r *recordingProber) probe(_ context.Context, sourceURL string, file *models.MediaFile) (*models.MediaFile, error) {
	r.mu.Lock()
	r.urls = append(r.urls, sourceURL)
	r.mu.Unlock()
	if r.hits != nil {
		select {
		case r.hits <- struct{}{}:
		default:
		}
	}
	return &models.MediaFile{FilePath: sourceURL, Duration: 5400, AudioTracks: []models.AudioTrack{{Codec: "eac3", Channels: 6}}}, nil
}

func (r *recordingProber) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.urls)
}

func (r *recordingProber) last() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.urls) == 0 {
		return ""
	}
	return r.urls[len(r.urls)-1]
}

func probeChainHandler(row *models.MediaFile) (*PlaybackHandler, *blockedDefaultTarget, *recordingProber) {
	target := &blockedDefaultTarget{open: make(chan struct{}), row: row}
	prober := &recordingProber{}
	h := &PlaybackHandler{
		ServiceContext:              context.Background(),
		DefaultProbeTarget:          target,
		VirtualPlaybackSourceProber: prober.probe,
	}
	return h, target, prober
}

// probeChainWaitFor polls until the observable reaches the expected value
// (bounded); use for positive expectations like "the chain began its read".
func probeChainWaitFor(t *testing.T, want int, read func() int, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := read(); got == want {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("%s = %d, want %d", what, got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// probeChainStableWait gives a negative assertion a bounded burn-in: the
// expectation is "no new target read within this window", the only observable
// formulation of a spawn that must not have happened.
func probeChainStableWait(t *testing.T, want int, read func() int, what string) {
	t.Helper()
	deadline := time.Now().Add(250 * time.Millisecond)
	for {
		if got := read(); got != want {
			t.Fatalf("%s = %d, want stable %d", what, got, want)
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestScheduleDefaultFileProbeProbesTheDefaultRowsOwnBytes(t *testing.T) {
	row := probeChainRow(301, "virtual://movie/probe-chain?result=c1", nil)
	h, target, prober := probeChainHandler(row)
	target.releaseRow()

	h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), probeChainStreams(row), catalog.AccessFilter{})
	h.probeChainWG.Wait()
	if prober.count() != 1 {
		t.Fatalf("probe calls = %d, want exactly 1", prober.count())
	}
	if got := prober.last(); got != probeChainProviderURL(row) {
		t.Fatalf("probed %q, want the default row's own resolved bytes %q", got, probeChainProviderURL(row))
	}
	if target.callCount() != 1 {
		t.Fatalf("target reads = %d, want 1", target.callCount())
	}
}

func TestNewVirtualDefaultProbeTargetUsesSelectedFile(t *testing.T) {
	defaultRow := probeChainRow(700, "virtual://movie/probe-chain?result=default", nil)
	selectedRow := probeChainRow(701, "virtual://movie/probe-chain?result=selected", nil)
	reader := &probeChainVersionReader{
		files: []*models.MediaFile{defaultRow, selectedRow},
		rows:  map[int]*models.MediaFile{defaultRow.ID: defaultRow, selectedRow.ID: selectedRow},
	}
	target := NewVirtualDefaultProbeTarget(reader, reader)

	got, err := target.DefaultVirtualProbeTarget(t.Context(), probeChainTestContent, catalog.AccessFilter{SelectedFileID: selectedRow.ID, AllowedLibraryIDs: []int{77}})
	if err != nil || got == nil || got.ID != selectedRow.ID || got.FilePath != selectedRow.FilePath {
		t.Fatalf("selected target = %+v, err = %v; want file %d", got, err, selectedRow.ID)
	}

	if got, err := target.DefaultVirtualProbeTarget(t.Context(), probeChainTestContent, catalog.AccessFilter{SelectedFileID: 999, AllowedLibraryIDs: []int{77}}); err != nil || got != nil {
		t.Fatalf("unknown selected target = %+v, err = %v; want no target", got, err)
	}

	selectedRow.MediaFolderID = 78
	reader.rows[selectedRow.ID] = selectedRow
	if got, err := target.DefaultVirtualProbeTarget(t.Context(), probeChainTestContent, catalog.AccessFilter{SelectedFileID: selectedRow.ID, AllowedLibraryIDs: []int{77}}); err != nil || got != nil {
		t.Fatalf("hidden selected target = %+v, err = %v; want no target", got, err)
	}
}

func TestScheduleDefaultFileProbeProbesPersistedSelectedWhenListingChanged(t *testing.T) {
	defaultRow := probeChainRow(730, "virtual://movie/probe-chain?result=default", nil)
	selectedRow := probeChainRow(731, "virtual://movie/probe-chain?profile=4K+Dolby+Vision&result=selected", nil)
	reader := &probeChainVersionReader{
		files: []*models.MediaFile{defaultRow, selectedRow},
		rows:  map[int]*models.MediaFile{defaultRow.ID: defaultRow, selectedRow.ID: selectedRow},
	}
	prober := &recordingProber{}
	h := &PlaybackHandler{
		ServiceContext:              context.Background(),
		DefaultProbeTarget:          NewVirtualDefaultProbeTarget(reader, reader),
		VirtualPlaybackSourceProber: prober.probe,
	}
	// The fresh listing no longer names the selected row's URI (the provider
	// renumbered it), but the selected row itself is persisted and authorized.
	h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), probeChainStreams(defaultRow), catalog.AccessFilter{SelectedFileID: selectedRow.ID, AllowedLibraryIDs: []int{77}})
	h.probeChainWG.Wait()
	if prober.count() != 1 || prober.last() != probeChainProviderURL(selectedRow) {
		t.Fatalf("probes = %d, last = %q; want the persisted selected row %q", prober.count(), prober.last(), probeChainProviderURL(selectedRow))
	}
}

func TestScheduleDefaultFileProbePublishesAfterDurableEvidence(t *testing.T) {
	defaultRow := probeChainRow(720, "virtual://movie/probe-chain?result=default", nil)
	selectedRow := probeChainRow(721, "virtual://movie/probe-chain?result=selected", nil)
	reader := &probeChainVersionReader{
		files: []*models.MediaFile{defaultRow, selectedRow},
		rows:  map[int]*models.MediaFile{defaultRow.ID: defaultRow, selectedRow.ID: selectedRow},
	}
	bus := &recordingEventBus{}
	hub := notifications.NewHub("probe-chain-event", bus)
	prober := &recordingProber{}
	h := &PlaybackHandler{
		ServiceContext:              context.Background(),
		EventsHub:                   hub.EventsHub(),
		DefaultProbeTarget:          NewVirtualDefaultProbeTarget(reader, reader),
		VirtualPlaybackSourceProber: prober.probe,
		VirtualFileMetadataSaver: func(context.Context, models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
			return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
		},
	}
	h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), probeChainStreams(selectedRow), catalog.AccessFilter{SelectedFileID: selectedRow.ID, AllowedLibraryIDs: []int{77}})
	h.probeChainWG.Wait()
	payload := waitForRecordingEvent(t, bus, 2*time.Second, "catalog.item.changed", "versions_updated", probeChainTestContent)
	if payload == "" {
		t.Fatal("verified evidence did not publish versions_updated")
	}
}

func TestScheduleDefaultFileProbeProbesSelectedBytes(t *testing.T) {
	defaultRow := probeChainRow(710, "virtual://movie/probe-chain?result=default", nil)
	selectedRow := probeChainRow(711, "virtual://movie/probe-chain?result=selected", nil)
	reader := &probeChainVersionReader{
		files: []*models.MediaFile{defaultRow, selectedRow},
		rows:  map[int]*models.MediaFile{defaultRow.ID: defaultRow, selectedRow.ID: selectedRow},
	}
	prober := &recordingProber{}
	h := &PlaybackHandler{
		ServiceContext:              context.Background(),
		DefaultProbeTarget:          NewVirtualDefaultProbeTarget(reader, reader),
		VirtualPlaybackSourceProber: prober.probe,
	}
	h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), probeChainStreams(selectedRow), catalog.AccessFilter{SelectedFileID: selectedRow.ID, AllowedLibraryIDs: []int{77}})
	h.probeChainWG.Wait()
	if prober.count() != 1 || prober.last() != probeChainProviderURL(selectedRow) {
		t.Fatalf("probes = %d, last = %q; want selected file %q", prober.count(), prober.last(), probeChainProviderURL(selectedRow))
	}
}

func TestScheduleDefaultFileProbeSecondCallCoalescesIntoLiveChain(t *testing.T) {
	row := probeChainRow(311, "virtual://movie/probe-chain?result=c11", nil)
	h, target, prober := probeChainHandler(row)

	// While the first chain sits inside admitted work, a second schedule of
	// the same item must return immediately and spawn no second read.
	h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), probeChainStreams(row), catalog.AccessFilter{})
	probeChainWaitFor(t, 1, target.callCount, "live chain target reads")

	h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), probeChainStreams(row), catalog.AccessFilter{})
	probeChainStableWait(t, 1, target.callCount, "coalesced chain target reads")

	target.releaseRow()
	h.probeChainWG.Wait()
	if prober.count() != 1 {
		t.Fatalf("probe calls after two schedules = %d, want one chain", prober.count())
	}
}

func TestScheduleDefaultFileProbeCoalescesSelectedVersionAcrossSources(t *testing.T) {
	row := probeChainRow(312, "virtual://movie/probe-chain?result=c12", nil)
	h, target, _ := probeChainHandler(row)

	// Discovery persists one source group at a time. Each group must not spawn
	// its own chain for the same selected version: that exhausted the process
	// slots with duplicates and left the actual selected chain unscheduled.
	secondSource := probeChainSourceRow()
	secondSource.FilePath = "virtual://movie/probe-chain?profile=1080p"
	filter := catalog.AccessFilter{SelectedFileID: row.ID, AllowedLibraryIDs: []int{77}}

	h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), probeChainStreams(row), filter)
	probeChainWaitFor(t, 1, target.callCount, "first source target reads")

	h.ScheduleDefaultFileProbe(probeChainTestContent, secondSource, probeChainStreams(row), filter)
	probeChainStableWait(t, 1, target.callCount, "coalesced second-source target reads")

	target.releaseRow()
	h.probeChainWG.Wait()
}

func TestScheduleDefaultFileProbeSkipsStampedRow(t *testing.T) {
	stampedAt := time.Now().Add(-time.Hour)
	row := probeChainRow(302, "virtual://movie/probe-chain?result=c2", &stampedAt)
	h, target, prober := probeChainHandler(row)
	target.releaseRow()

	h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), probeChainStreams(row), catalog.AccessFilter{})
	h.probeChainWG.Wait()

	if prober.count() != 0 {
		t.Fatalf("stamped row was probed %d times; the evidence stamp must skip re-enumeration", prober.count())
	}
	if target.callCount() != 1 {
		t.Fatalf("target reads = %d, want 1", target.callCount())
	}
}

func TestScheduleDefaultFileProbeSkipsNeutralGroupRow(t *testing.T) {
	neutral := probeChainRow(303, "virtual://movie/probe-chain", nil)
	h, target, prober := probeChainHandler(neutral)
	target.releaseRow()

	h.ScheduleDefaultFileProbe(probeChainTestContent, neutral, probeChainStreams(neutral), catalog.AccessFilter{})
	h.probeChainWG.Wait()

	if prober.count() != 0 {
		t.Fatalf("neutral group row was probed %d times; it may resolve to a sibling candidate", prober.count())
	}
}

func TestScheduleDefaultFileProbeSkipsDefaultBytesMissingFromListing(t *testing.T) {
	row := probeChainRow(304, "virtual://movie/probe-chain?result=c9", nil)
	h, target, prober := probeChainHandler(row)
	target.releaseRow()

	// The listing names other candidates; the default's own bytes are absent,
	// so a probe would enumerate unverified bytes.
	streams := []VirtualPlaybackStream{
		{URI: "virtual://movie/probe-chain?result=c5", OwnerInstallationID: 5},
	}
	h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), streams, catalog.AccessFilter{})
	h.probeChainWG.Wait()

	if prober.count() != 0 {
		t.Fatalf("unmatched default row was probed %d times; the chain must never guess bytes", prober.count())
	}
}

func TestScheduleDefaultFileProbeSkipsUnwiredParts(t *testing.T) {
	row := probeChainRow(305, "virtual://movie/probe-chain?result=c5", nil)

	// No target: the chain is inert.
	h := &PlaybackHandler{ServiceContext: context.Background()}
	h.ScheduleDefaultFileProbe(probeChainTestContent, row, probeChainStreams(row), catalog.AccessFilter{})
	h.probeChainWG.Wait()

	// No prober: similarly inert.
	target := &blockedDefaultTarget{open: make(chan struct{}), row: row}
	target.releaseRow()
	h2 := &PlaybackHandler{
		ServiceContext:     context.Background(),
		DefaultProbeTarget: target,
	}
	h2.ScheduleDefaultFileProbe(probeChainTestContent, row, probeChainStreams(row), catalog.AccessFilter{})
	h2.probeChainWG.Wait()
}

func TestScheduleDefaultFileProbeAdmissionBoundsSlotsAndReturnsImmediately(t *testing.T) {
	rowA := probeChainRow(306, "virtual://movie/probe-chain?result=cA", nil)
	rowB := probeChainRow(307, "virtual://movie/probe-chain2?result=cB", nil)
	rowD := probeChainRow(318, "virtual://movie/probe-chain4?result=cD", nil)
	contentB := "movie-probe-chain-b"
	contentD := "movie-probe-chain-d"

	// Targets exist before the chains start, so a release loop can never race
	// a lazily created target into a second blocked state.
	targetA := &blockedDefaultTarget{open: make(chan struct{}), row: rowA}
	targetB := &blockedDefaultTarget{open: make(chan struct{}), row: rowB}
	targetD := &blockedDefaultTarget{open: make(chan struct{}), row: rowD}
	targetD.releaseRow()
	targetsByContent := map[string]*blockedDefaultTarget{
		probeChainTestContent: targetA,
		contentB:              targetB,
		contentD:              targetD,
	}
	prober := &recordingProber{}
	h := &PlaybackHandler{
		ServiceContext:              context.Background(),
		VirtualPlaybackSourceProber: prober.probe,
	}
	h.DefaultProbeTarget = VirtualDefaultProbeTargetFunc(func(ctx context.Context, contentID string, _ catalog.AccessFilter) (*models.MediaFile, error) {
		tgt := targetsByContent[contentID]
		if tgt == nil {
			return nil, fmt.Errorf("unexpected chain for %q", contentID)
		}
		return tgt.DefaultVirtualProbeTarget(ctx, contentID, catalog.AccessFilter{})
	})

	// Two items fill the slots; their chains sit inside admitted work.
	h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), probeChainStreams(rowA), catalog.AccessFilter{})
	h.ScheduleDefaultFileProbe(contentB, &models.MediaFile{ID: 307, ContentID: contentB, FilePath: "virtual://movie/probe-chain2"}, probeChainStreams(rowB), catalog.AccessFilter{})
	probeChainWaitFor(t, 1, targetA.callCount, "chain A entered its default-version read")
	probeChainWaitFor(t, 1, targetB.callCount, "chain B entered its default-version read")

	// The third item is refused: no goroutine spawns (the target would
	// answer "unexpected chain"), so its schedule returns immediately.
	dropped := make(chan struct{})
	go func() {
		h.ScheduleDefaultFileProbe("movie-probe-chain-c", &models.MediaFile{ID: 308, ContentID: "movie-probe-chain-c", FilePath: "virtual://movie/probe-chain3"}, probeChainStreams(rowA), catalog.AccessFilter{})
		close(dropped)
	}()
	select {
	case <-dropped:
	case <-time.After(2 * time.Second):
		t.Fatal("saturated admission blocked the discovery caller")
	}

	// Releasing both slots frees capacity: a later chain is admitted and
	// completes end to end.
	targetA.releaseRow()
	targetB.releaseRow()
	h.probeChainWG.Wait()
	prober4 := &recordingProber{hits: make(chan struct{}, 1)}
	h.VirtualPlaybackSourceProber = prober4.probe
	h.ScheduleDefaultFileProbe(contentD, &models.MediaFile{ID: 318, ContentID: contentD, FilePath: "virtual://movie/probe-chain4"}, probeChainStreams(rowD), catalog.AccessFilter{})
	select {
	case <-prober4.hits:
	case <-time.After(2 * time.Second):
		t.Fatal("a chain admitted after slots freed did not probe")
	}
	h.probeChainWG.Wait()
}

func TestScheduleDefaultFileProbeStallTeriminatesAndRestoresSlot(t *testing.T) {
	prevBudget := virtualDefaultProbeChainBudget
	virtualDefaultProbeChainBudget = 150 * time.Millisecond
	t.Cleanup(func() { virtualDefaultProbeChainBudget = prevBudget })

	row := probeChainRow(309, "virtual://movie/probe-chain?result=cS", nil)
	h, target, prober := probeChainHandler(row)

	// The target never opens its gate; it returns only when the chain context
	// deadline fires. The goroutine must actually terminate and free the slot,
	// not merely let a deadline lapse while the read continues.
	h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), probeChainStreams(row), catalog.AccessFilter{})
	h.probeChainWG.Wait()

	if target.callCount() != 1 {
		t.Fatalf("stalled chain target reads = %d, want 1", target.callCount())
	}
	if prober.count() != 0 {
		t.Fatalf("stalled chain probed %d times", prober.count())
	}

	// The slot is free again: a well-behaved retry is admitted immediately.
	row2 := probeChainRow(319, "virtual://movie/probe-chain?result=cR", nil)
	prober2 := &recordingProber{hits: make(chan struct{}, 1)}
	target.row = row2
	target.releaseRow()
	h.VirtualPlaybackSourceProber = prober2.probe
	h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), probeChainStreams(row2), catalog.AccessFilter{})
	select {
	case <-prober2.hits:
	case <-time.After(2 * time.Second):
		t.Fatal("a well-behaved chain after a stall did not probe")
	}
	h.probeChainWG.Wait()
	if got := prober2.last(); got != probeChainProviderURL(row2) {
		t.Fatalf("retry probed %q, want the default row's resolved bytes %q", got, probeChainProviderURL(row2))
	}
}

// TestListSourceSchedulesChainAfterPersistOnlyForDiscover pins both the
// scheduling point (successful persistence inside the detached shared work,
// never the request side) and the chaining eligibility (discovery only, never
// the operator-driven :refresh path).
func TestListSourceSchedulesChainAfterPersistOnlyForDiscover(t *testing.T) {
	row := probeChainRow(310, "virtual://movie/probe-chain?result=cD", nil)
	streams := probeChainStreams(row)

	fired := make(chan []VirtualPlaybackStream, 4)
	svc := &VirtualCandidatesRefreshService{
		ListCached: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
			return streams, nil
		}),
		ListFresh: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
			return streams, nil
		}),
		Persist: func(context.Context, *models.MediaFile, []VirtualPlaybackStream) error { return nil },
	}
	svc.ScheduleDefaultFileProbe = func(contentID string, source *models.MediaFile, got []VirtualPlaybackStream, _ catalog.AccessFilter) {
		passed := make([]VirtualPlaybackStream, len(got))
		copy(passed, got)
		fired <- passed
	}

	if _, err := svc.listSource(context.Background(), row, 2, "p-1", svc.ListCached, probeChainTestContent, catalog.AccessFilter{}); err != nil {
		t.Fatalf("discover listSource: %v", err)
	}
	select {
	case got := <-fired:
		if len(got) != 2 || got[1].URI != row.FilePath {
			t.Fatalf("probe chain received %d streams; want the persisted listing snapshot", len(got))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("discover persist did not schedule the probe chain")
	}

	if _, err := svc.listSource(context.Background(), row, 2, "p-1", svc.ListFresh, "", catalog.AccessFilter{}); err != nil {
		t.Fatalf("refresh listSource: %v", err)
	}
	select {
	case stray := <-fired:
		t.Fatalf("refresh persist scheduled a probe chain (%d streams); only discovery schedules", len(stray))
	default:
	}
}

// TestDiscoverScheduleSurvivesWaiterDisconnect proves the winner's success is
// the schedule trigger: when the originating request's context is already
// dead, the detached persistence still lands and still schedules a chain.
func TestDiscoverScheduleSurvivesWaiterDisconnect(t *testing.T) {
	row := probeChainRow(312, "virtual://movie/probe-chain?result=cW", nil)
	streams := probeChainStreams(row)

	h, target, prober := probeChainHandler(row)
	target.releaseRow()

	svc := &VirtualCandidatesRefreshService{
		ListCached: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
			return streams, nil
		}),
		Persist: func(context.Context, *models.MediaFile, []VirtualPlaybackStream) error { return nil },
	}
	svc.ScheduleDefaultFileProbe = h.ScheduleDefaultFileProbe

	deadCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := svc.listSource(deadCtx, probeChainSourceRow(), 2, "p-1", svc.ListCached, probeChainTestContent, catalog.AccessFilter{}); err != nil {
		t.Fatalf("listSource with a dead caller context: %v", err)
	}
	h.probeChainWG.Wait()
	if prober.count() != 1 {
		t.Fatalf("probe calls after waiter disconnect = %d, want 1 (chain runs detached from the request)", prober.count())
	}
}

func TestDefaultProbeChainAdmissionIsAtomicWithoutGoroutines(t *testing.T) {
	var admission defaultProbeChainAdmission
	releaseA, refusal := admission.acquire("movie-a")
	if refusal != defaultProbeChainRefusalNone {
		t.Fatalf("empty admission refused a fresh chain: %q", refusal)
	}
	if _, refusal := admission.acquire("movie-a"); refusal != defaultProbeChainRefusalLive {
		t.Fatalf("a live chain did not deduplicate its item: %q", refusal)
	}
	releaseB, refusal := admission.acquire("movie-b")
	if refusal != defaultProbeChainRefusalNone || releaseB == nil {
		t.Fatalf("second distinct item did not admit within the bound: %q", refusal)
	}
	releaseA()
	releaseC, refusal := admission.acquire("movie-c")
	if refusal != defaultProbeChainRefusalNone {
		t.Fatalf("a released chain kept its slot reserved: %q", refusal)
	}
	// A double release must not hand out the same slot twice: with both slots
	// legitimately live (b and c), nothing else is admissible.
	releaseA()
	if _, refusal := admission.acquire("movie-d"); refusal != defaultProbeChainRefusalCapacity {
		t.Fatalf("a double release freed an extra slot: %q", refusal)
	}
	releaseC()
	releaseD, refusal := admission.acquire("movie-d")
	if refusal != defaultProbeChainRefusalNone {
		t.Fatalf("a released slot was not reusable: %q", refusal)
	}
	releaseB()
	releaseD()
}

// TestScheduleDefaultFileProbeSurvivesPanickingWork pins the panic guard at
// the detached goroutine boundary: this is optional best-effort work, so an
// unexpected panic — in the default-version read or in the probe itself —
// unwinds into a logged observation and releases every shared slot, and a
// later chain still runs.
func TestScheduleDefaultFileProbeSurvivesPanickingWork(t *testing.T) {
	prevBudget := virtualDefaultProbeChainBudget
	virtualDefaultProbeChainBudget = time.Second
	t.Cleanup(func() { virtualDefaultProbeChainBudget = prevBudget })

	// Panic inside the default-version read.
	row := probeChainRow(320, "virtual://movie/probe-chain?result=cP1", nil)
	panickingTarget := VirtualDefaultProbeTargetFunc(func(context.Context, string, catalog.AccessFilter) (*models.MediaFile, error) {
		panic("target blew up")
	})
	prober := &recordingProber{}
	h := &PlaybackHandler{
		ServiceContext:              context.Background(),
		DefaultProbeTarget:          panickingTarget,
		VirtualPlaybackSourceProber: prober.probe,
	}
	h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), probeChainStreams(row), catalog.AccessFilter{})
	h.probeChainWG.Wait()

	// The slot is reusable regardless of the panic.
	reworked := &blockedDefaultTarget{open: make(chan struct{}), row: row}
	reworked.releaseRow()
	h.DefaultProbeTarget = reworked
	h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), probeChainStreams(row), catalog.AccessFilter{})
	probeChainWaitFor(t, 1, prober.count, "chain after a panicking target")

	// Panic inside the probe: the chain must contain it, and both the
	// admission slot and the shared probe gate must come back. The target
	// answers the same row the listing names, so the chain actually reaches
	// the prober this time.
	probingRow := probeChainRow(321, "virtual://movie/probe-chain?result=cP2", nil)
	probedTarget := &blockedDefaultTarget{open: make(chan struct{}), row: probingRow}
	probedTarget.releaseRow()
	h.probeChainWG.Wait()
	proberEntered := &atomic.Int32{}
	explodingProber := func(context.Context, string, *models.MediaFile) (*models.MediaFile, error) {
		proberEntered.Add(1)
		panic("prober blew up")
	}
	h.DefaultProbeTarget = probedTarget
	h.VirtualPlaybackSourceProber = explodingProber
	h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), probeChainStreams(probingRow), catalog.AccessFilter{})
	h.probeChainWG.Wait()
	if proberEntered.Load() != 1 {
		t.Fatalf("panicking prober entered %d times, want exactly 1", proberEntered.Load())
	}
	// The crashing chain must not have kept a gate slot: after it settles,
	// every slot of the shared gate is free again.
	gate := h.detachedGate()
	for i := 0; i < gate.capacity(); i++ {
		if !gate.tryAcquire() {
			t.Fatalf("probe gate kept %d of %d slots free after a panicking chain", i, gate.capacity())
		}
	}
	for i := 0; i < gate.capacity(); i++ {
		gate.release()
	}
	// A retry on the same item then runs normally.
	h.VirtualPlaybackSourceProber = prober.probe
	h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), probeChainStreams(probingRow), catalog.AccessFilter{})
	probeChainWaitFor(t, 2, prober.count, "chain after a panicking prober")
	h.probeChainWG.Wait()
}

// TestScheduleDefaultFileProbeSnapshotsMutableStreams pins the input
// snapshot: mutating the caller's request-header map after scheduling must
// not change what the chain's probe reads, because the chain deep-copies the
// mutable members of every listing entry it hands off.
func TestScheduleDefaultFileProbeSnapshotsMutableStreams(t *testing.T) {
	row := probeChainRow(322, "virtual://movie/probe-chain?result=cH", nil)
	target := &blockedDefaultTarget{open: make(chan struct{}), row: row}
	target.releaseRow()

	var seenHeaders map[string]string
	headerProber := func(_ context.Context, sourceURL string, file *models.MediaFile, headers map[string]string) (*models.MediaFile, error) {
		seenHeaders = map[string]string{}
		maps.Copy(seenHeaders, headers)
		return &models.MediaFile{FilePath: sourceURL, Duration: 5400}, nil
	}
	h := &PlaybackHandler{
		ServiceContext:                         context.Background(),
		DefaultProbeTarget:                     target,
		VirtualPlaybackSourceProberWithHeaders: headerProber,
	}

	streams := probeChainStreams(row)
	streams[1].RequestHeaders = map[string]string{"User-Agent": "discovery-snapshotted"}
	h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), streams, catalog.AccessFilter{})
	// The caller mutates its map after the hand-off; the chain must still
	// probe with the value scheduled.
	streams[1].RequestHeaders["User-Agent"] = "mutated-after-hand-off"
	h.probeChainWG.Wait()

	if got := seenHeaders["User-Agent"]; got != "discovery-snapshotted" {
		t.Fatalf("prober headers = %q, want the value captured at scheduling", got)
	}
}

// probeChainVersionReader is the production target's catalog seam for these
// tests. It applies the real access filter to its file rows — the way
// DetailService.GetItemVersions does before returning versions — and records
// every filter it was handed and every file row it was asked to read.
type probeChainVersionReader struct {
	mu      sync.Mutex
	files   []*models.MediaFile
	rows    map[int]*models.MediaFile
	filters []catalog.AccessFilter
	reads   []int
}

func (r *probeChainVersionReader) GetItemVersions(_ context.Context, _ string, filter catalog.AccessFilter) ([]catalog.FileVersion, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.filters = append(r.filters, filter)
	visible := catalog.FilterMediaFilesByAccess(r.files, filter)
	versions := make([]catalog.FileVersion, 0, len(visible))
	for _, file := range visible {
		versions = append(versions, catalog.FileVersion{FileID: file.ID})
	}
	return versions, nil
}

func (r *probeChainVersionReader) GetByID(_ context.Context, id int) (*models.MediaFile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads = append(r.reads, id)
	return r.rows[id], nil
}

func (r *probeChainVersionReader) snapshot() ([]catalog.AccessFilter, []int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.filters), slices.Clone(r.reads)
}

// TestScheduleDefaultFileProbeHonorsTheDiscoveryAccessFilter pins the target
// lookup to the caller's access filter. The catalog read lists the hidden
// library's version first; with the filter threaded through, the target
// resolves and probes the visible default and never even reads the hidden
// row's file. An unfiltered lookup — the bug this guards — would probe the
// hidden version instead.
func TestScheduleDefaultFileProbeHonorsTheDiscoveryAccessFilter(t *testing.T) {
	const visibleLibrary, hiddenLibrary = 77, 78
	visible := &models.MediaFile{
		ID: 330, ContentID: probeChainTestContent, FilePath: "virtual://movie/probe-chain?result=visible",
		MediaFolderID: visibleLibrary, VirtualOwnerInstallationID: 5, ProbeSource: "virtual",
	}
	hidden := &models.MediaFile{
		ID: 331, ContentID: probeChainTestContent, FilePath: "virtual://movie/probe-chain?result=hidden",
		MediaFolderID: hiddenLibrary, VirtualOwnerInstallationID: 5, ProbeSource: "virtual",
	}
	visible.ResolvedURL = probeChainProviderURL(visible)
	hidden.ResolvedURL = probeChainProviderURL(hidden)
	reader := &probeChainVersionReader{
		// The hidden version sorts first, so an unfiltered read would pick it.
		files: []*models.MediaFile{hidden, visible},
		rows:  map[int]*models.MediaFile{visible.ID: visible, hidden.ID: hidden},
	}
	prober := &recordingProber{}
	h := &PlaybackHandler{
		ServiceContext:              context.Background(),
		DefaultProbeTarget:          NewVirtualDefaultProbeTarget(reader, reader),
		VirtualPlaybackSourceProber: prober.probe,
	}
	filter := catalog.AccessFilter{
		AllowedLibraryIDs:         []int{visibleLibrary},
		DisabledLibraryIDs:        []int{99},
		PresentationLibraryID:     new(visibleLibrary),
		MetadataLanguageOverrides: map[string]string{"en": "fr"},
		MaturityLimits:            access.MaturityLimits{MaxContentRating: "PG-13"},
	}
	streams := []VirtualPlaybackStream{
		{URI: hidden.FilePath, OwnerInstallationID: 5},
		{URI: visible.FilePath, OwnerInstallationID: 5},
	}

	h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), streams, filter)
	h.probeChainWG.Wait()

	filters, reads := reader.snapshot()
	if len(reads) != 1 || reads[0] != visible.ID {
		t.Fatalf("file rows read = %v, want only the visible version %d", reads, visible.ID)
	}
	if prober.count() != 1 || prober.last() != probeChainProviderURL(visible) {
		t.Fatalf("probed %q (%d probes), want the visible default %q", prober.last(), prober.count(), probeChainProviderURL(visible))
	}
	if len(filters) != 1 {
		t.Fatalf("catalog version reads = %d, want 1", len(filters))
	}
	got := filters[0]
	if !slices.Equal(got.AllowedLibraryIDs, filter.AllowedLibraryIDs) ||
		!slices.Equal(got.DisabledLibraryIDs, filter.DisabledLibraryIDs) ||
		got.PresentationLibraryID == nil || *got.PresentationLibraryID != visibleLibrary ||
		got.MetadataLanguageOverrides["en"] != "fr" ||
		got.MaxContentRating != "PG-13" {
		t.Fatalf("catalog read filter = %+v, want the discovery request's filter %+v", got, filter)
	}
}

// A selected profile variant that vanished from the fresh listing is probed
// from its own persisted candidate. That candidate can carry an expired signed
// URL copied from the selected row; it must be resolved afresh rather than
// probing the expired transport and marking the candidate failed.
func TestVirtualChainProbeURLRefreshesExpiredSelectedCandidate(t *testing.T) {
	const freshURL = "https://probe.example/fresh-selected"
	past := time.Now().Add(-time.Hour)
	row := probeChainRow(7001, "virtual://movie/probe-chain?result=selected", nil)
	row.ResolvedURL = "https://probe.example/expired-selected"
	row.ResolvedURLExpiresAt = &past
	cand := virtualPlaybackStreamFromFile(row)
	if cand == nil {
		t.Fatal("selected candidate was not built")
	}
	calls := 0
	h := &PlaybackHandler{VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		calls++
		return ResolvedVirtualMedia{URL: freshURL}, nil
	})}

	url, _, ok := h.virtualChainProbeURL(context.Background(), row, *cand, catalog.AccessFilter{UserID: 1, ProfileID: "profile-1"})
	if !ok {
		t.Fatal("expired selected candidate produced no probe URL")
	}
	if url != freshURL {
		t.Fatalf("probe URL = %q, want the fresh resolve %q", url, freshURL)
	}
	if calls != 1 {
		t.Fatalf("fresh resolver calls = %d, want 1", calls)
	}
}

// TestScheduleDefaultFileProbeSnapshotsAccessFilter pins the filter snapshot:
// the request that scheduled the chain may reuse or mutate its filter after
// the hand-off, and the detached read must still see the constraints that
// authorized the discovery run.
func TestScheduleDefaultFileProbeSnapshotsAccessFilter(t *testing.T) {
	row := probeChainRow(332, "virtual://movie/probe-chain?result=cAF", nil)
	h, target, _ := probeChainHandler(row)

	filter := catalog.AccessFilter{
		AllowedLibraryIDs:         []int{76},
		MetadataLanguageOverrides: map[string]string{"en": "fr"},
	}
	h.ScheduleDefaultFileProbe(probeChainTestContent, probeChainSourceRow(), probeChainStreams(row), filter)
	probeChainWaitFor(t, 1, target.callCount, "chain entered its default-version read")

	filter.AllowedLibraryIDs[0] = 99
	filter.MetadataLanguageOverrides["en"] = "de"
	target.releaseRow()
	h.probeChainWG.Wait()

	got := target.lastFilter()
	if len(got.AllowedLibraryIDs) != 1 || got.AllowedLibraryIDs[0] != 76 {
		t.Fatalf("chain read library scope = %v, want the value captured at scheduling", got.AllowedLibraryIDs)
	}
	if got.MetadataLanguageOverrides["en"] != "fr" {
		t.Fatalf("chain read language overrides = %v, want the value captured at scheduling", got.MetadataLanguageOverrides)
	}
}

func TestDefaultProbeChainAdmissionCancelsLiveChain(t *testing.T) {
	var admission defaultProbeChainAdmission
	ctx, cancel := context.WithCancel(context.Background())
	release, refusal := admission.acquireWithCancel("content-1\x000", cancel)
	if refusal != defaultProbeChainRefusalNone || release == nil {
		t.Fatalf("acquire refusal=%q, release_nil=%v", refusal, release == nil)
	}
	defer release()
	if !admission.cancelKey("content-1\x000") {
		t.Fatal("cancelKey reported no live chain")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("chain context was not canceled")
	}
}

// TestScheduleSelectedFileProbeProbesTheSelectedRow pins probe-on-select: an
// explicit selection schedules the shared chain for the row it names, never the
// resolved default, and a row that already carries probe evidence schedules no
// probe at all.
func TestScheduleSelectedFileProbeProbesTheSelectedRow(t *testing.T) {
	defaultRow := probeChainRow(740, "virtual://movie/probe-chain?result=default", nil)
	selectedRow := probeChainRow(741, "virtual://movie/probe-chain?result=selected", nil)
	reader := &probeChainVersionReader{
		files: []*models.MediaFile{defaultRow, selectedRow},
		rows:  map[int]*models.MediaFile{defaultRow.ID: defaultRow, selectedRow.ID: selectedRow},
	}
	prober := &recordingProber{}
	h := &PlaybackHandler{
		ServiceContext:              context.Background(),
		DefaultProbeTarget:          NewVirtualDefaultProbeTarget(reader, reader),
		VirtualPlaybackSourceProber: prober.probe,
	}

	h.ScheduleSelectedFileProbe(probeChainTestContent, selectedRow.ID, catalog.AccessFilter{AllowedLibraryIDs: []int{77}})
	h.probeChainWG.Wait()

	if prober.count() != 1 || prober.last() != probeChainProviderURL(selectedRow) {
		t.Fatalf("probes = %d, last = %q; want exactly the selected row %q", prober.count(), prober.last(), probeChainProviderURL(selectedRow))
	}
	if _, reads := reader.snapshot(); len(reads) != 1 || reads[0] != selectedRow.ID {
		t.Fatalf("file rows read = %v, want only the selected row %d", reads, selectedRow.ID)
	}

	// Fresh inventory: the same selection must schedule nothing to probe.
	probedAt := time.Now()
	selectedRow.ProbeUpdatedAt = &probedAt
	h.ScheduleSelectedFileProbe(probeChainTestContent, selectedRow.ID, catalog.AccessFilter{AllowedLibraryIDs: []int{77}})
	h.probeChainWG.Wait()
	if prober.count() != 1 {
		t.Fatalf("probes after a selected row with fresh inventory = %d, want no new probe", prober.count())
	}
}

type selectionProbeCall struct {
	contentID string
	fileID    int
	filter    catalog.AccessFilter
}

// recordingSelectionProber records what the watch-detail trigger hands to the
// probe scheduler.
type recordingSelectionProber struct {
	mu    sync.Mutex
	calls []selectionProbeCall
}

func (r *recordingSelectionProber) ScheduleSelectedFileProbe(contentID string, fileID int, filter catalog.AccessFilter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, selectionProbeCall{contentID: contentID, fileID: fileID, filter: filter})
}

func (r *recordingSelectionProber) snapshot() []selectionProbeCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

// TestWatchDetailSelectionSchedulesProbeOnlyForUnprobedVirtualRow pins the
// trigger rule: only a read that names an explicit, virtual, still-unprobed
// version schedules a probe. A bare read, a local selection, and a selection
// with fresh inventory all schedule nothing.
func TestWatchDetailSelectionSchedulesProbeOnlyForUnprobedVirtualRow(t *testing.T) {
	prober := &recordingSelectionProber{}
	h := &ItemsHandler{virtualSelectionProber: prober}
	detail := &catalog.WatchDetail{
		ContentID: "episode-select",
		Type:      itemTypeEpisode,
		Versions: []catalog.FileVersion{
			{FileID: 21, FilePath: "/media/local/episode.mkv"},
			{FileID: 22, FilePath: "virtual://series/show/s01e01?result=lupins03e01hdr2160pwebh265edith"},
		},
	}

	// A bare read carries no selection: nothing is scheduled.
	h.scheduleSelectedVirtualProbe(detail, catalog.AccessFilter{})
	if got := prober.snapshot(); len(got) != 0 {
		t.Fatalf("bare read scheduled %d probes, want none", len(got))
	}

	// An explicit selection of the virtual row schedules exactly that row.
	h.scheduleSelectedVirtualProbe(detail, catalog.AccessFilter{SelectedFileID: 22})
	got := prober.snapshot()
	if len(got) != 1 {
		t.Fatalf("explicit selection scheduled %d probes, want 1", len(got))
	}
	if got[0].contentID != detail.ContentID || got[0].fileID != 22 || got[0].filter.SelectedFileID != 22 {
		t.Fatalf("scheduled probe = %+v, want content %q file 22 with its selection", got[0], detail.ContentID)
	}

	// Selecting a local row has no provider bytes to enumerate.
	h.scheduleSelectedVirtualProbe(detail, catalog.AccessFilter{SelectedFileID: 21})
	if got := prober.snapshot(); len(got) != 1 {
		t.Fatalf("local selection scheduled %d probes, want none", len(got)-1)
	}

	// A selected row with fresh inventory needs no probe.
	probedAt := time.Now()
	detail.Versions[1].ProbedAt = &probedAt
	h.scheduleSelectedVirtualProbe(detail, catalog.AccessFilter{SelectedFileID: 22})
	if got := prober.snapshot(); len(got) != 1 {
		t.Fatalf("fresh selection scheduled %d probes, want none", len(got)-1)
	}
}
