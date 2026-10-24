package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
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

func refreshJobExecutor(store *virtuallibrary.IndexerReleaseStore, listErr error, searcher IndexerReleaseSearcher) *VirtualCandidatesRefreshExecutor {
	return refreshJobExecutorFor(store, listErr, searcher, "movie:x")
}

func refreshJobExecutorFor(store *virtuallibrary.IndexerReleaseStore, listErr error, searcher IndexerReleaseSearcher, sourceContentID string) *VirtualCandidatesRefreshExecutor {
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
	}
}

// TestRefreshJobPipelinePersistsIndexerReleases covers the happy path: the
// provider is re-listed, the indexer search is filtered to usenet and deduped
// against the provider listing, and the new rows are persisted. The refresh is
// list-only: no candidate media URL is opened and result.Enriched stays 0.
func TestRefreshJobPipelinePersistsIndexerReleases(t *testing.T) {
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
	executor := refreshJobExecutorFor(store, nil, searcher, contentID)
	t.Cleanup(func() { _, _ = store.PruneExpiredIndexerReleases(context.Background()) })

	result, err := executor.Execute(t.Context(), adminjob.VirtualCandidatesRefreshRequest{
		ContentID: contentID, MediaFolderID: 2, Title: "Heat", Year: 1995, IMDbID: "tt0113277",
	}, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !result.IndexerSearchOK || result.IndexerReleases != 1 || result.Enriched != 0 {
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
}

// TestRefreshJobIsListOnly proves the refresh never opens a candidate media
// URL. The listing serves ProviderURLs that hit a counting HTTP server; any
// ffprobe, anchor probe, or range fetch would register a request. Execute must
// complete with zero media requests and Enriched 0. (Enriched is kept at 0 for
// wire compatibility; see VirtualCandidatesRefreshResult.)
func TestRefreshJobIsListOnly(t *testing.T) {
	store := refreshJobTestStore(t)
	contentID := "movie:job-no-probe"
	var mediaRequests atomic.Int64
	mediaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mediaRequests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(mediaServer.Close)
	source := refreshJobSource(contentID)
	providerURL := mediaServer.URL + "/stream/candidate.mkv"
	executor := &VirtualCandidatesRefreshExecutor{
		Refresh: &VirtualCandidatesRefreshService{
			ListFresh: VirtualPlaybackStreamListerFunc(func(_ context.Context, path string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
				return []VirtualPlaybackStream{{
					ID: "c1", URI: path + "?result=c1",
					Label: "Heat 1995 1080p WEB-DL x264-GRP", FileSize: 4_000_000_000,
					ProviderURL: providerURL,
				}}, nil
			}),
			Persist: func(context.Context, *models.MediaFile, []VirtualPlaybackStream) error { return nil },
			ContentFiles: func(context.Context, string) ([]*models.MediaFile, error) {
				return []*models.MediaFile{source}, nil
			},
		},
		Store:    store,
		Searcher: &fakeIndexerSearcher{},
	}
	result, err := executor.Execute(t.Context(), adminjob.VirtualCandidatesRefreshRequest{
		ContentID: contentID, MediaFolderID: 2, Title: "Heat",
	}, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Enriched != 0 {
		t.Fatalf("Enriched = %d, want 0: refresh must not probe candidates", result.Enriched)
	}
	if got := mediaRequests.Load(); got != 0 {
		t.Fatalf("media requests = %d, want 0: refresh must not open a candidate media URL", got)
	}
}

// TestRefreshJobIndexerFailureCompletesAltmountOnly proves an indexer failure
// warns and continues: the job succeeds with the provider result and no
// indexer releases.
func TestRefreshJobIndexerFailureCompletesAltmountOnly(t *testing.T) {
	store := refreshJobTestStore(t)
	searcher := &fakeIndexerSearcher{err: errors.New("indexer offline")}
	contentID := "movie:job-indexer-fail"
	executor := refreshJobExecutorFor(store, nil, searcher, contentID)

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

// fakeJobEpisodes resolves one episode for the job's library-folder fallback.
type fakeJobEpisodes struct {
	episode *models.Episode
}

func (f fakeJobEpisodes) GetByID(context.Context, string) (*models.Episode, error) {
	return f.episode, nil
}

// TestRefreshJobFirstFolderIDFallsBackToSeriesSource proves a fileless episode
// inherits the library from its series' virtual source row, so the indexer
// scope has a real folder even before the episode owns a file.
func TestRefreshJobFirstFolderIDFallsBackToSeriesSource(t *testing.T) {
	service := &VirtualCandidatesRefreshJobService{
		Episodes: fakeJobEpisodes{episode: &models.Episode{ContentID: "episode:fileless", SeriesID: "series:source"}},
		ContentFiles: func(_ context.Context, contentID string) ([]*models.MediaFile, error) {
			if contentID == "series:source" {
				return []*models.MediaFile{{MediaFolderID: 14}}, nil
			}
			return nil, nil
		},
	}
	if got := service.firstFolderID(t.Context(), "episode:fileless"); got != 14 {
		t.Fatalf("firstFolderID = %d, want 14 from the series source", got)
	}
}

// TestRefreshJobSearchesIndexersWithoutProviderSource proves a fileless
// episode still reaches the indexer search instead of failing at the missing
// provider source. The provider stage is empty; the job persists the releases
// under the episode scope and reports a successful search.
func TestRefreshJobSearchesIndexersWithoutProviderSource(t *testing.T) {
	store := refreshJobTestStore(t)
	contentID := "episode:job-no-source"
	searcher := &fakeIndexerSearcher{releases: []virtuallibrary.SearchItem{
		fakeSearchRelease("new", "Bleach.Thousand.Year.Blood.War.S17E01.1080p.WEB-DL-GRP", 1_400_000_000),
	}}
	executor := &VirtualCandidatesRefreshExecutor{
		Refresh: &VirtualCandidatesRefreshService{
			ListFresh: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
				t.Fatal("provider listing must not run without a virtual source")
				return nil, nil
			}),
			Persist: func(context.Context, *models.MediaFile, []VirtualPlaybackStream) error {
				t.Fatal("persistence must not run without a virtual source")
				return nil
			},
			ContentFiles: func(context.Context, string) ([]*models.MediaFile, error) { return nil, nil },
			EpisodeFiles: func(context.Context, string) ([]*models.MediaFile, error) { return nil, nil },
		},
		Store:    store,
		Searcher: searcher,
	}

	result, err := executor.Execute(t.Context(), adminjob.VirtualCandidatesRefreshRequest{
		ContentID: contentID, EpisodeID: contentID, MediaFolderID: 14,
		Title: "Bleach", Year: 2004, TVDBID: "74796", SeasonNumber: 17, EpisodeNumber: 1,
	}, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.ProviderCandidates != 0 || !result.IndexerSearchOK || result.IndexerReleases != 1 {
		t.Fatalf("result = %+v", result)
	}
	if len(result.Releases) != 1 || !strings.Contains(result.Releases[0].Title, "S17E01") {
		t.Fatalf("releases = %+v", result.Releases)
	}
	scope := virtuallibrary.IndexerReleaseScope{ContentID: contentID, EpisodeID: contentID, MediaFolderID: 14}
	persisted, err := store.ListIndexerReleases(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted) != 1 || persisted[0].GUID != "new" {
		t.Fatalf("persisted rows = %+v", persisted)
	}
}

// TestRefreshJobProviderFailureFails proves an altmount re-list failure is the
// one fatal stage.
func TestRefreshJobProviderFailureFails(t *testing.T) {
	store := refreshJobTestStore(t)
	executor := refreshJobExecutorFor(store, errors.New("provider offline"), nil, "movie:job-provider-fail")
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
	executor := refreshJobExecutor(store, nil, nil)
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

func TestRefreshJobWithoutEventHubCompletes(t *testing.T) {
	store := refreshJobTestStore(t)
	executor := refreshJobExecutor(store, nil, nil)
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
	executor := refreshJobExecutorFor(store, nil, nil, "movie:job-prune")
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
	executor := refreshJobExecutorFor(store, nil, nil, "movie:job-refresh-state-fail")
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
	executor := refreshJobExecutorFor(store, nil, nil, "movie:job-prune-fail")
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
	executor := refreshJobExecutor(store, nil, &fakeIndexerSearcher{releases: releases})
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
