package handlers

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/adminjob"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/notifications"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

// fakeIndexerSearcher is the on-demand indexer seam.
type fakeIndexerSearcher struct {
	calls    int
	releases []virtuallibrary.SearchItem
	err      error
}

func (f *fakeIndexerSearcher) SearchMonitoredReleases(context.Context, virtuallibrary.MonitoredMedia, *virtuallibrary.VirtualEpisode) ([]virtuallibrary.SearchItem, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.releases, nil
}

// fakeCandidateEnricher records the batch it was handed.
type fakeCandidateEnricher struct {
	calls   int
	streams int
	result  int
}

func (f *fakeCandidateEnricher) EnrichVirtualCandidates(_ context.Context, _, _ string, _ int, _ string, streams []VirtualPlaybackStream) int {
	f.calls++
	f.streams = len(streams)
	return f.result
}

// fakeSearchRelease builds one usenet SearchItem for the executor tests.
func fakeSearchRelease(guid, title string, size int64) virtuallibrary.SearchItem {
	return virtuallibrary.SearchItem{
		GUID: guid, Title: title, Size: size, Indexer: "idx", IndexerID: 1,
		Protocol: "usenet", DownloadURL: "https://indexer.example/" + guid,
	}
}

func refreshJobTestStore(t *testing.T) *virtuallibrary.IndexerReleaseStore {
	t.Helper()
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return virtuallibrary.NewIndexerReleaseStore(pool)
}

// refreshJobSource returns a virtual source row for the provider re-list.
func refreshJobSource(contentID string) *models.MediaFile {
	return &models.MediaFile{
		ID: 1, ContentID: contentID, FilePath: "virtual://movie/x",
		Container: "virtual", MediaFolderID: 2, VirtualOwnerInstallationID: 5,
	}
}

func refreshJobExecutor(store *virtuallibrary.IndexerReleaseStore, listErr error, searcher IndexerReleaseSearcher, enricher VirtualCandidateEnricher) *VirtualCandidatesRefreshExecutor {
	return refreshJobExecutorFor(store, listErr, searcher, enricher, "movie:x")
}

func refreshJobExecutorFor(store *virtuallibrary.IndexerReleaseStore, listErr error, searcher IndexerReleaseSearcher, enricher VirtualCandidateEnricher, sourceContentID string) *VirtualCandidatesRefreshExecutor {
	source := refreshJobSource(sourceContentID)
	return &VirtualCandidatesRefreshExecutor{
		Refresh: &VirtualCandidatesRefreshService{
			ListFresh: VirtualPlaybackStreamListerFunc(func(_ context.Context, path string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
				if listErr != nil {
					return nil, listErr
				}
				return []VirtualPlaybackStream{{ID: "c1", URI: path + "?result=c1", Label: "Heat 1995 1080p WEB-DL x264-GRP", FileSize: 4_000_000_000}}, nil
			}),
			Persist: func(context.Context, *models.MediaFile, []VirtualPlaybackStream) error { return nil },
			ContentFiles: func(context.Context, string) ([]*models.MediaFile, error) {
				return []*models.MediaFile{source}, nil
			},
		},
		Store:    store,
		Searcher: searcher,
		Enricher: enricher,
	}
}

// TestRefreshJobPipelinePersistsIndexerReleasesAndEnriches covers the happy
// path: the provider is re-listed, the indexer search is filtered to usenet and
// deduped against the provider listing, the new rows are persisted, and the new
// candidates are enriched.
func TestRefreshJobPipelinePersistsIndexerReleasesAndEnriches(t *testing.T) {
	store := refreshJobTestStore(t)
	contentID := "movie:job-happy"
	searcher := &fakeIndexerSearcher{releases: []virtuallibrary.SearchItem{
		// Deduped: same release name and size as the provider listing.
		fakeSearchRelease("dup", "Heat.1995.1080p.WEB-DL.x264-GRP", 4_000_000_000),
		// Kept: a different release not on the provider.
		fakeSearchRelease("new", "Heat.1995.2160p.WEB-DL.x265-NEWGRP", 8_000_000_000),
		// Dropped: torrent protocol is not enqueueable via altmount.
		{GUID: "tor", Title: "Heat.1995.1080p.BluRay.x264-TOR", Size: 9_000_000_000, Protocol: "torrent", DownloadURL: "magnet:x"},
	}}
	enricher := &fakeCandidateEnricher{result: 1}
	executor := refreshJobExecutorFor(store, nil, searcher, enricher, contentID)
	t.Cleanup(func() { _, _ = store.PruneExpiredIndexerReleases(context.Background()) })

	result, err := executor.Execute(t.Context(), adminjob.VirtualCandidatesRefreshRequest{
		ContentID: contentID, MediaFolderID: 2, Title: "Heat", Year: 1995, IMDbID: "tt0113277",
	}, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !result.IndexerSearchOK || result.IndexerReleases != 1 || result.Enriched != 1 {
		t.Fatalf("result = %+v", result)
	}
	if len(result.Releases) != 1 || !strings.Contains(result.Releases[0].Title, "2160p") {
		t.Fatalf("releases = %+v", result.Releases)
	}
	// The stored URL is never part of the result.
	for _, release := range result.Releases {
		if strings.Contains(release.Title, "indexer.example") {
			t.Fatalf("result leaks a URL: %+v", release)
		}
	}
	scope := virtuallibrary.IndexerReleaseScope{ContentID: contentID, MediaFolderID: 2}
	list, err := store.ListIndexerReleases(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].GUID != "new" {
		t.Fatalf("persisted rows = %+v", list)
	}
	if enricher.calls != 1 {
		t.Fatalf("enricher calls = %d", enricher.calls)
	}
}

// TestRefreshJobIndexerFailureCompletesAltmountOnly proves an indexer failure
// warns and continues: the job succeeds with the provider result and no
// indexer releases.
func TestRefreshJobIndexerFailureCompletesAltmountOnly(t *testing.T) {
	store := refreshJobTestStore(t)
	searcher := &fakeIndexerSearcher{err: errors.New("indexer offline")}
	contentID := "movie:job-indexer-fail"
	executor := refreshJobExecutorFor(store, nil, searcher, nil, contentID)

	result, err := executor.Execute(t.Context(), adminjob.VirtualCandidatesRefreshRequest{
		ContentID: "movie:job-indexer-fail", MediaFolderID: 2, Title: "Heat",
	}, nil)
	if err != nil {
		t.Fatalf("indexer failure must not fail the job: %v", err)
	}
	if result.IndexerSearchOK || result.IndexerReleases != 0 {
		t.Fatalf("result = %+v", result)
	}
	if result.ProviderCandidates != 1 {
		t.Fatalf("provider candidates = %d, want the altmount-only result", result.ProviderCandidates)
	}
}

// TestRefreshJobProviderFailureFails proves an altmount re-list failure is the
// one fatal stage.
func TestRefreshJobProviderFailureFails(t *testing.T) {
	store := refreshJobTestStore(t)
	executor := refreshJobExecutorFor(store, errors.New("provider offline"), nil, nil, "movie:job-provider-fail")
	_, err := executor.Execute(t.Context(), adminjob.VirtualCandidatesRefreshRequest{
		ContentID: "movie:job-provider-fail", MediaFolderID: 2, Title: "Heat",
	}, nil)
	if err == nil {
		t.Fatal("provider failure should fail the job")
	}
	if !strings.Contains(err.Error(), "list provider candidates") {
		t.Fatalf("err = %v", err)
	}
}

// TestRefreshJobPublishesVersionsUpdated drives the completion event through a
// real hub over a recording bus and asserts the change is versions_updated,
// which is what tells every client to invalidate the version list.
func TestRefreshJobPublishesVersionsUpdated(t *testing.T) {
	store := refreshJobTestStore(t)
	bus := &recordingEventBus{}
	hub := notifications.NewHub("test", bus)
	executor := refreshJobExecutor(store, nil, nil, nil)
	executor.Events = hub
	contentID := "movie:job-event"
	if _, err := executor.Execute(t.Context(), adminjob.VirtualCandidatesRefreshRequest{
		ContentID: contentID, MediaFolderID: 2, Title: "Heat",
	}, nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	payload := recordingEventPayload(bus, "versions_updated")
	if payload == "" {
		t.Fatalf("no versions_updated event published")
	}
	if !strings.Contains(payload, contentID) {
		t.Fatalf("event payload %q does not name the content", payload)
	}
}

// recordingEventPayload returns the first recorded event payload containing
// needle.
func recordingEventPayload(bus *recordingEventBus, needle string) string {
	bus.mu.Lock()
	defer bus.mu.Unlock()
	for _, event := range bus.events {
		if strings.Contains(event.Payload, needle) {
			return event.Payload
		}
	}
	return ""
}

// TestEnrichVirtualCandidatesProbesEveryCandidate proves the enrichment pass
// attempts every persisted candidate and persists the probe evidence, and that
// a probe failure on one candidate does not stop the others.
func TestEnrichVirtualCandidatesProbesEveryCandidate(t *testing.T) {
	rows := map[string]*models.MediaFile{
		"virtual://movie/x?result=a": {ID: 11, ContentID: "movie:x", FilePath: "virtual://movie/x?result=a", MediaFolderID: 2, Container: virtualURIScheme, VirtualOwnerInstallationID: 5},
		"virtual://movie/x?result=b": {ID: 12, ContentID: "movie:x", FilePath: "virtual://movie/x?result=b", MediaFolderID: 2, Container: virtualURIScheme, VirtualOwnerInstallationID: 5},
	}
	probes := 0
	saved := make([]models.VirtualFilePersistArgs, 0)
	h := &PlaybackHandler{
		VirtualFileLookup: func(ctx context.Context, path string) (*models.MediaFile, error) {
			if row, ok := rows[path]; ok {
				return row, nil
			}
			return nil, errors.New("not found")
		},
		VirtualPlaybackSourceProberWithHeaders: func(_ context.Context, _ string, file *models.MediaFile, _ map[string]string) (*models.MediaFile, error) {
			probes++
			if file.FilePath == "virtual://movie/x?result=b" {
				return nil, errors.New("probe failed")
			}
			probed := *file
			probed.Resolution = "2160p"
			probed.CodecVideo = "hevc"
			probed.VideoTracks = []models.VideoTrack{{Codec: "hevc"}}
			return &probed, nil
		},
		VirtualFileSaver: func(_ context.Context, args models.VirtualFilePersistArgs) (int64, error) {
			saved = append(saved, args)
			return 1, nil
		},
	}
	streams := []VirtualPlaybackStream{
		{URI: "virtual://movie/x?result=a", ProviderURL: "https://provider.example/a", Resolution: "2160p", OwnerInstallationID: 5},
		{URI: "virtual://movie/x?result=b", ProviderURL: "https://provider.example/b", OwnerInstallationID: 5},
	}
	enriched := h.EnrichVirtualCandidates(context.Background(), "movie:x", "", 1, "p", streams)
	if probes != 2 {
		t.Fatalf("probes = %d, want every candidate attempted", probes)
	}
	if enriched != 1 {
		t.Fatalf("enriched = %d, want the one successful probe", enriched)
	}
	if len(saved) != 1 || saved[0].FileID != 11 || !saved[0].StampProbe {
		t.Fatalf("saved = %+v", saved)
	}
}

func TestRefreshJobWithoutEventHubCompletes(t *testing.T) {
	store := refreshJobTestStore(t)
	executor := refreshJobExecutor(store, nil, nil, nil)
	if _, err := executor.Execute(t.Context(), adminjob.VirtualCandidatesRefreshRequest{
		ContentID: "movie:job-no-hub", MediaFolderID: 2, Title: "Heat",
	}, nil); err != nil {
		t.Fatalf("Execute without a hub: %v", err)
	}
}

// fakeProviderStateRefresher records the forced provider-state refresh.
type fakeProviderStateRefresher struct {
	calls int
	err   error
}

func (f *fakeProviderStateRefresher) RefreshProviderState(context.Context) error {
	f.calls++
	return f.err
}

// fakeCandidatePruner records the prune call and returns a fixed count.
type fakeCandidatePruner struct {
	calls   int
	pruned  int
	err     error
	sources int
	fresh   int
	content string
	episode string
}

func (f *fakeCandidatePruner) PruneDeadAbsentCandidates(_ context.Context, contentID, episodeID string, sources []*models.MediaFile, fresh []VirtualPlaybackStream) (int, error) {
	f.calls++
	f.content = contentID
	f.episode = episodeID
	f.sources = len(sources)
	f.fresh = len(fresh)
	if f.err != nil {
		return 0, f.err
	}
	return f.pruned, nil
}

// TestRefreshJobForcesProviderStateAndPrunes proves the executor forces a fresh
// provider classification snapshot before listing and runs the dead-candidate
// prune after persistence, reporting the prune count through the progress
// reporter.
func TestRefreshJobForcesProviderStateAndPrunes(t *testing.T) {
	store := refreshJobTestStore(t)
	refresher := &fakeProviderStateRefresher{}
	pruner := &fakeCandidatePruner{pruned: 3}
	executor := refreshJobExecutorFor(store, nil, nil, nil, "movie:job-prune")
	executor.ProviderRefresher = refresher
	executor.Pruner = pruner

	var messages []string
	var lastCurrent, lastTotal int
	result, err := executor.Execute(t.Context(), adminjob.VirtualCandidatesRefreshRequest{
		ContentID: "movie:job-prune", MediaFolderID: 2, Title: "Heat",
	}, func(current, total int, message string) {
		messages = append(messages, message)
		lastCurrent, lastTotal = current, total
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if refresher.calls != 1 {
		t.Fatalf("provider-state refresher calls = %d, want 1", refresher.calls)
	}
	if pruner.calls != 1 {
		t.Fatalf("pruner calls = %d, want 1", pruner.calls)
	}
	if pruner.content != "movie:job-prune" || pruner.sources == 0 || pruner.fresh != 1 {
		t.Fatalf("pruner args: content=%q sources=%d fresh=%d", pruner.content, pruner.sources, pruner.fresh)
	}
	if result.PrunedCandidates != 3 {
		t.Fatalf("PrunedCandidates = %d, want 3", result.PrunedCandidates)
	}
	if lastCurrent != lastTotal || lastTotal == 0 {
		t.Fatalf("progress ended at %d/%d, want complete", lastCurrent, lastTotal)
	}
	sawPrune := false
	for _, message := range messages {
		if strings.Contains(message, "Prun") {
			sawPrune = true
		}
	}
	if !sawPrune {
		t.Fatalf("progress messages did not report the prune step: %v", messages)
	}
}

// TestRefreshJobProviderStateFailureIsNonFatal proves a provider-state refresh
// failure warns and continues: the job still lists, persists, and completes.
func TestRefreshJobProviderStateFailureIsNonFatal(t *testing.T) {
	store := refreshJobTestStore(t)
	refresher := &fakeProviderStateRefresher{err: errors.New("altmount offline")}
	executor := refreshJobExecutorFor(store, nil, nil, nil, "movie:job-refresh-state-fail")
	executor.ProviderRefresher = refresher
	executor.Pruner = &fakeCandidatePruner{pruned: 1}

	result, err := executor.Execute(t.Context(), adminjob.VirtualCandidatesRefreshRequest{
		ContentID: "movie:job-refresh-state-fail", MediaFolderID: 2, Title: "Heat",
	}, nil)
	if err != nil {
		t.Fatalf("provider-state failure must not fail the job: %v", err)
	}
	if refresher.calls != 1 || result.ProviderCandidates != 1 {
		t.Fatalf("result = %+v, refresher calls = %d", result, refresher.calls)
	}
}

// TestRefreshJobPruneFailureIsNonFatal proves a prune failure warns and
// continues: an otherwise good refresh is not failed by cleanup.
func TestRefreshJobPruneFailureIsNonFatal(t *testing.T) {
	store := refreshJobTestStore(t)
	pruner := &fakeCandidatePruner{err: errors.New("delete failed")}
	executor := refreshJobExecutorFor(store, nil, nil, nil, "movie:job-prune-fail")
	executor.Pruner = pruner

	result, err := executor.Execute(t.Context(), adminjob.VirtualCandidatesRefreshRequest{
		ContentID: "movie:job-prune-fail", MediaFolderID: 2, Title: "Heat",
	}, nil)
	if err != nil {
		t.Fatalf("prune failure must not fail the job: %v", err)
	}
	if result.PrunedCandidates != 0 {
		t.Fatalf("PrunedCandidates = %d, want 0 on a prune failure", result.PrunedCandidates)
	}
}

// TestRefreshJobCapsPersistedBatch proves the batch is capped before the
// upsert.
func TestRefreshJobCapsPersistedBatch(t *testing.T) {
	store := refreshJobTestStore(t)
	releases := make([]virtuallibrary.SearchItem, 0, virtuallibrary.MaxIndexerReleasesPerContent+10)
	for i := 0; i < virtuallibrary.MaxIndexerReleasesPerContent+10; i++ {
		releases = append(releases, fakeSearchRelease(
			"g"+time.Duration(i).String(),
			"Some.Movie.1995.2160p.WEB-DL.x265-GRP"+time.Duration(i).String(),
			int64(1_000_000+i),
		))
	}
	executor := refreshJobExecutor(store, nil, &fakeIndexerSearcher{releases: releases}, nil)
	t.Cleanup(func() { _, _ = store.PruneExpiredIndexerReleases(context.Background()) })
	result, err := executor.Execute(t.Context(), adminjob.VirtualCandidatesRefreshRequest{
		ContentID: "movie:job-cap", MediaFolderID: 2, Title: "Some Movie",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.IndexerReleases > virtuallibrary.MaxIndexerReleasesPerContent {
		t.Fatalf("persisted %d releases, want <= %d", result.IndexerReleases, virtuallibrary.MaxIndexerReleasesPerContent)
	}
}
