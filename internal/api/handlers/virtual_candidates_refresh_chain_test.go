package handlers

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/adminjob"
	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/notifications"
	"github.com/Silo-Server/silo-server/internal/scanner"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

// recordingEventCount counts recorded events whose payload contains every
// needle. Used to prove the completion event fired exactly once.
func recordingEventCount(bus *recordingEventBus, needles ...string) int {
	bus.mu.Lock()
	defer bus.mu.Unlock()
	count := 0
	for _, event := range bus.events {
		matched := true
		for _, needle := range needles {
			if !strings.Contains(event.Payload, needle) {
				matched = false
				break
			}
		}
		if matched {
			count++
		}
	}
	return count
}

func recordingEventPayloads(bus *recordingEventBus) []string {
	bus.mu.Lock()
	defer bus.mu.Unlock()
	out := make([]string, 0, len(bus.events))
	for _, event := range bus.events {
		out = append(out, event.Payload)
	}
	return out
}

// waitForJobTerminal polls the durable job row until it reaches a terminal
// state (observable state, never a fixed sleep).
func waitForJobTerminal(t *testing.T, repo *adminjob.Repository, id string, timeout time.Duration) *models.AdminJob {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		job, err := repo.GetByID(context.Background(), id)
		if err != nil {
			t.Fatalf("read job %s: %v", id, err)
		}
		switch job.Status {
		case adminjob.StatusCompleted, adminjob.StatusFailed, adminjob.StatusCancelled:
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s did not reach a terminal state within %s; last status %s (%s)", id, timeout, job.Status, job.ErrorMessage)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// TestVirtualCandidatesRefreshEndToEndCarriesListIndexerReleasesAndEvent is
// the whole-chain proof: a Refresh List job runs through the real adminjob
// runner and the real executor pipeline (provider re-list and DB persist →
// indexer search → dedup → indexer-release upsert → completion event) against
// the real catalog projections. It asserts the watch detail's version list
// carries the provider-declared candidate, the indexer-release projection
// carries the new release, the versions_updated event fired exactly once, and
// the one-active-job lock is held while running and released after completion.
// The refresh is list-only: no candidate media URL is opened (no prober wired).
func TestVirtualCandidatesRefreshEndToEndCarriesTracksIndexerReleasesAndEvent(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := t.Context()

	suffix := time.Now().UnixNano()
	contentID := fmt.Sprintf("movie-refresh-chain-%d", suffix)
	sourcePath := fmt.Sprintf("virtual://movie/refresh-chain-%d", suffix)
	candidateURI := sourcePath + "?result=c1"

	// --- Seed a virtual movie with its source row ---
	var userID int
	if err := pool.QueryRow(ctx, `INSERT INTO users(username, role, enabled) VALUES($1,'user',true) RETURNING id`, fmt.Sprintf("refresh-chain-%d", suffix)).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	var folderID int
	if err := pool.QueryRow(ctx, `INSERT INTO media_folders(type, name, enabled) VALUES('movies', $1, true) RETURNING id`, contentID).Scan(&folderID); err != nil {
		t.Fatalf("seed library: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO media_items(content_id, type, title, genres, runtime) VALUES($1,'movie','Refresh Chain','{}',90)`, contentID); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO media_item_libraries(content_id, media_folder_id) VALUES($1,$2)`, contentID, folderID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	// The neutral virtual source row the refresh service discovers. Owner 5
	// drives the candidate rows' owner identity.
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id, media_folder_id, file_path, file_size, container, probe_source, virtual_owner_installation_id, duration)
		VALUES($1,$2,$3,0,'virtual','virtual',5,5400)`, contentID, folderID, sourcePath); err != nil {
		t.Fatalf("seed virtual source: %v", err)
	}
	t.Cleanup(func() {
		b := context.Background()
		_, _ = pool.Exec(b, `DELETE FROM admin_jobs WHERE created_by_user_id=$1`, userID)
		_, _ = pool.Exec(b, `DELETE FROM virtual_indexer_releases WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(b, `DELETE FROM media_files WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(b, `DELETE FROM media_item_libraries WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(b, `DELETE FROM media_items WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(b, `DELETE FROM media_folders WHERE id=$1`, folderID)
		_, _ = pool.Exec(b, `DELETE FROM users WHERE id=$1`, userID)
	})

	fileRepo := scanner.NewFileRepository(pool)
	store := virtuallibrary.NewIndexerReleaseStore(pool)

	// --- Provider re-list + real DB persistence (mirrors router wiring) ---
	sink := func(sinkCtx context.Context, source *models.MediaFile, streams []VirtualPlaybackStream) error {
		candidates := make([]scanner.VirtualCandidate, 0, len(streams))
		for _, stream := range streams {
			if !strings.HasPrefix(stream.URI, "virtual://") || stream.URI == source.FilePath {
				continue
			}
			candidates = append(candidates, scanner.VirtualCandidate{
				OwnerInstallationID:    stream.OwnerInstallationID,
				URI:                    stream.URI,
				Label:                  stream.Label,
				Resolution:             stream.Resolution,
				CodecVideo:             stream.CodecVideo,
				CodecAudio:             stream.CodecAudio,
				HDR:                    stream.HDR,
				FileSize:               stream.FileSize,
				Bitrate:                stream.Bitrate,
				AudioLanguages:         stream.AudioLanguages,
				SubtitleLanguages:      stream.SubtitleLanguages,
				AudioChannels:          stream.AudioChannels,
				HasAtmos:               stream.HasAtmos,
				Is10Bit:                stream.Is10Bit,
				IsMultiAudio:           stream.IsMultiAudio,
				VisualTags:             stream.VisualTags,
				ResolvedURL:            stream.ProviderURL,
				ResolvedURLExpiresAt:   stream.ProviderExpiresAt,
				ProviderVideoHash:      stream.ProviderVideoHash,
				ProviderGUID:           stream.ProviderGUID,
				ProviderReleaseName:    stream.ProviderReleaseName,
				ProviderReleaseSize:    stream.FileSize,
				ProviderRequestHeaders: stream.RequestHeaders,
			})
		}
		return fileRepo.ReplaceVirtualCandidates(sinkCtx, source, candidates)
	}

	// The provider re-list is gated so the test can observe the job in its
	// running state and prove the one-active-job lock holds there too.
	listEntered := make(chan struct{})
	listRelease := make(chan struct{})
	var listEnteredOnce sync.Once
	refreshSvc := &VirtualCandidatesRefreshService{
		ListFresh: VirtualPlaybackStreamListerFunc(func(_ context.Context, path string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			listEnteredOnce.Do(func() {
				close(listEntered)
				<-listRelease
			})
			return []VirtualPlaybackStream{{
				ID: "c1", URI: path + "?result=c1", Label: "Heat 1995 1080p WEB-DL x264-GRP",
				Resolution: "2160p", CodecVideo: "hevc", CodecAudio: "eac3",
				HDR: "dv", VisualTags: []string{"dv", "hdr10"}, AudioChannels: "5.1", HasAtmos: true,
				AudioLanguages: []string{"eng"},
				FileSize:       8_000_000_000, ProviderURL: "https://provider.example/stream",
				OwnerInstallationID: 5,
			}}, nil
		}),
		Persist:      sink,
		ContentFiles: fileRepo.GetByContentID,
		EpisodeFiles: fileRepo.GetByEpisodeID,
	}

	// The refresh is list-only: the version list carries the provider-declared
	// candidate metadata, and verified track inventory arrives with an explicit
	// Play of one candidate.
	_ = fileRepo

	searcher := &fakeIndexerSearcher{releases: []virtuallibrary.SearchItem{
		// Deduped: same release name and size as the provider listing.
		fakeSearchRelease("dup", "Heat.1995.1080p.WEB-DL.x264-GRP", 8_000_000_000),
		// Kept: a different release the provider does not have.
		fakeSearchRelease("new", "Heat.1995.2160p.WEB-DL.x265-NEWGRP", 8_000_000_000),
	}}

	bus := &recordingEventBus{}
	hub := notifications.NewHub("refresh-chain", bus)
	executor := &VirtualCandidatesRefreshExecutor{
		Refresh:  refreshSvc,
		Store:    store,
		Searcher: searcher,
		Events:   hub,
	}

	// --- Acceptance path: real job service, real repositories ---
	jobs := adminjob.NewRepository(pool)
	jobSvc := &VirtualCandidatesRefreshJobService{
		Detail:       &fakeIndexerReleaseDetail{},
		Items:        catalog.NewItemRepository(pool),
		Episodes:     catalog.NewEpisodeRepository(pool),
		ContentFiles: fileRepo.GetByContentID,
		EpisodeFiles: fileRepo.GetByEpisodeID,
		Jobs:         jobs,
	}

	job1, err := jobSvc.CreateRefreshJob(ctx, userID, "", contentID, catalog.AccessFilter{})
	if err != nil {
		t.Fatalf("create refresh job: %v", err)
	}
	if job1.Status != adminjob.StatusQueued {
		t.Fatalf("job1 status = %q, want queued", job1.Status)
	}

	// One active job per title: a second refresh while the first is queued is
	// coalesced onto the same job rather than starting a second provider work.
	coalesced, err := jobSvc.CreateRefreshJob(ctx, userID, "", contentID, catalog.AccessFilter{})
	if err != nil {
		t.Fatalf("second create while queued: %v", err)
	}
	if coalesced.ID != job1.ID {
		t.Fatalf("second create returned job %s, want the active %s", coalesced.ID, job1.ID)
	}

	// --- Run the real production runner path ---
	runner := adminjob.NewRunner(jobs, nil, nil, nil, nil, nil, nil, nil, nil)
	runner.SetVirtualRefreshExecutor(executor)
	runner.Start()
	t.Cleanup(runner.Stop)

	// Wait until the job is actually running (the provider re-list is in
	// flight), then prove the lock also holds while running.
	select {
	case <-listEntered:
	case <-time.After(30 * time.Second):
		t.Fatal("the refresh job never entered the provider re-list")
	}
	running, err := jobs.GetByID(ctx, job1.ID)
	if err != nil {
		t.Fatalf("read running job: %v", err)
	}
	if running.Status != adminjob.StatusRunning {
		t.Fatalf("job status = %q, want running", running.Status)
	}
	coalescedWhileRunning, err := jobSvc.CreateRefreshJob(ctx, userID, "", contentID, catalog.AccessFilter{})
	if err != nil {
		t.Fatalf("second create while running: %v", err)
	}
	if coalescedWhileRunning.ID != job1.ID {
		t.Fatalf("second create while running returned job %s, want the active %s", coalescedWhileRunning.ID, job1.ID)
	}
	close(listRelease)

	completed := waitForJobTerminal(t, jobs, job1.ID, 30*time.Second)
	if completed.Status != adminjob.StatusCompleted {
		t.Fatalf("job failed: status=%s message=%s error=%s", completed.Status, completed.Message, completed.ErrorMessage)
	}

	// The completion event fires exactly once, after persistence, as
	// catalog.item.changed carrying change=versions_updated for the item.
	if n := recordingEventCount(bus, "catalog.item.changed", "versions_updated", contentID); n != 1 {
		all := recordingEventPayloads(bus)
		t.Fatalf("catalog.item.changed/versions_updated for %s fired %d times, want 1; payloads=%v", contentID, n, all)
	}

	// --- The watch detail's version list carries the refreshed version with
	// the provider-declared metadata (list-only: no probe ran) ---
	detailSvc := catalog.NewDetailService(
		catalog.NewItemRepository(pool),
		catalog.NewEpisodeRepository(pool),
		catalog.NewSeasonRepository(pool),
		catalog.NewPersonRepository(pool),
		fileRepo,
	)
	detail, err := detailSvc.GetWatchDetail(ctx, contentID, catalog.AccessFilter{})
	if err != nil {
		t.Fatalf("GetWatchDetail: %v", err)
	}
	var candidateVersion *catalog.FileVersion
	for i := range detail.Versions {
		if detail.Versions[i].FilePath == candidateURI {
			candidateVersion = &detail.Versions[i]
			break
		}
	}
	if candidateVersion == nil {
		paths := make([]string, 0, len(detail.Versions))
		for _, version := range detail.Versions {
			paths = append(paths, version.FilePath)
		}
		t.Fatalf("refreshed candidate %q missing from watch versions %v", candidateURI, paths)
	}
	if candidateVersion.Resolution != "2160p" || candidateVersion.CodecVideo != "hevc" || candidateVersion.CodecAudio != "eac3" {
		t.Fatalf("watch version = %+v, want the provider-declared 2160p/hevc/eac3", candidateVersion)
	}
	if len(candidateVersion.VideoTracks) != 1 || candidateVersion.VideoTracks[0].VideoRangeType != "DOVIWithHDR10" {
		t.Fatalf("watch version video tracks = %+v, want synthetic DOVIWithHDR10", candidateVersion.VideoTracks)
	}
	if len(candidateVersion.AudioTracks) != 1 || candidateVersion.AudioTracks[0].Codec != "eac3" || candidateVersion.AudioTracks[0].Channels != 6 {
		t.Fatalf("watch version audio tracks = %+v, want synthetic eac3 6ch", candidateVersion.AudioTracks)
	}

	// --- The indexer-release projection that feeds apiv2's indexer_releases ---
	releaseSvc := &VirtualIndexerReleaseService{
		Store:        store,
		Detail:       &fakeIndexerReleaseDetail{},
		ContentFiles: fileRepo.GetByContentID,
		EpisodeFiles: fileRepo.GetByEpisodeID,
	}
	views, err := releaseSvc.IndexerReleasesForWatch(ctx, contentID)
	if err != nil {
		t.Fatalf("IndexerReleasesForWatch: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("indexer releases = %+v, want the one new release", views)
	}
	if views[0].DownloadState != "not_downloaded" {
		t.Fatalf("indexer release state = %q", views[0].DownloadState)
	}
	if !strings.Contains(views[0].Title, "2160p") {
		t.Fatalf("indexer release title = %q, want the new 2160p release", views[0].Title)
	}
	if strings.Contains(views[0].ReleaseID, "http") {
		t.Fatalf("indexer release id leaked a URL: %q", views[0].ReleaseID)
	}

	// --- The active-job lock is released after completion ---
	runner.Stop()
	job3, err := jobSvc.CreateRefreshJob(ctx, userID, "", contentID, catalog.AccessFilter{})
	if err != nil {
		t.Fatalf("create after completion: %v", err)
	}
	if job3.ID == job1.ID {
		t.Fatalf("a refresh after completion reused the completed job %s; lock was not released", job1.ID)
	}
}

// TestVirtualCandidatesRefreshCancelOnSecondPress is the cancel-on-second-press
// proof against real repositories: a queued refresh is canceled by the owner,
// the unique active-job lock is released, and a non-owning account is refused
// with the existing coalescing answer.
func TestVirtualCandidatesRefreshCancelOnSecondPress(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := t.Context()

	suffix := time.Now().UnixNano()
	contentID := fmt.Sprintf("movie-refresh-cancel-%d", suffix)
	var ownerID int
	if err := pool.QueryRow(ctx, `INSERT INTO users(username, role, enabled) VALUES($1,'user',true) RETURNING id`, fmt.Sprintf("refresh-cancel-owner-%d", suffix)).Scan(&ownerID); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	var otherID int
	if err := pool.QueryRow(ctx, `INSERT INTO users(username, role, enabled) VALUES($1,'user',true) RETURNING id`, fmt.Sprintf("refresh-cancel-other-%d", suffix)).Scan(&otherID); err != nil {
		t.Fatalf("seed other: %v", err)
	}
	t.Cleanup(func() {
		b := context.Background()
		_, _ = pool.Exec(b, `DELETE FROM admin_jobs WHERE created_by_user_id = ANY($1)`, []int{ownerID, otherID})
		_, _ = pool.Exec(b, `DELETE FROM users WHERE id = ANY($1)`, []int{ownerID, otherID})
	})

	jobs := adminjob.NewRepository(pool)
	svc := &VirtualCandidatesRefreshJobService{
		Detail:         &fakeIndexerReleaseDetail{},
		ContentFiles:   func(context.Context, string) ([]*models.MediaFile, error) { return nil, nil },
		EpisodeFiles:   func(context.Context, string) ([]*models.MediaFile, error) { return nil, nil },
		Jobs:           jobs,
		CancelRegistry: adminjob.NewCancelRegistry(),
	}

	started, err := svc.CreateRefreshJob(ctx, ownerID, "", contentID, catalog.AccessFilter{})
	if err != nil {
		t.Fatalf("start refresh: %v", err)
	}
	if started.Status != adminjob.StatusQueued {
		t.Fatalf("started status = %q, want queued", started.Status)
	}

	// The owner's second press cancels the queued job instead of coalescing.
	canceled, err := svc.CancelRefreshJob(ctx, ownerID, "", contentID, catalog.AccessFilter{})
	if err != nil {
		t.Fatalf("cancel refresh: %v", err)
	}
	if canceled.ID != started.ID || canceled.Status != adminjob.StatusCancelled {
		t.Fatalf("canceled = %+v, want the started job canceled", canceled)
	}

	// The lock is released: a new refresh after cancellation starts.
	restarted, err := svc.CreateRefreshJob(ctx, ownerID, "", contentID, catalog.AccessFilter{})
	if err != nil {
		t.Fatalf("restart after cancel: %v", err)
	}
	if restarted.ID == started.ID {
		t.Fatalf("a refresh after cancellation reused the canceled job %s", started.ID)
	}

	// A non-owning account cannot cancel the owner's refresh; it keeps the
	// refusal semantics and leaves the job active for the owner.
	if _, err := svc.CancelRefreshJob(ctx, otherID, "", contentID, catalog.AccessFilter{}); err == nil {
		t.Fatal("a non-owning account canceled another account's refresh")
	}
	active, err := jobs.GetByID(ctx, restarted.ID)
	if err != nil || active.Status == adminjob.StatusCancelled {
		t.Fatalf("owner's refresh = %+v %v, want still active", active, err)
	}
}
