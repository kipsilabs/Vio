package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// deferredInventoryPush records the first inventory_updated event delivered to a
// session and ignores everything else.
type deferredInventoryPush struct {
	ch chan deferredInventoryEvent
}

type deferredInventoryEvent struct {
	payload playback.InventoryUpdatedPayload
}

func newDeferredInventoryPush() *deferredInventoryPush {
	return &deferredInventoryPush{ch: make(chan deferredInventoryEvent, 1)}
}

func (c *deferredInventoryPush) WriteJSON(v any) error {
	event, ok := v.(playback.EventEnvelope)
	if !ok || event.Name != playback.RealtimeEventInventoryUpdated {
		return nil
	}
	var payload playback.InventoryUpdatedPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return err
	}
	select {
	case c.ch <- deferredInventoryEvent{payload: payload}:
	default:
	}
	return nil
}

// TestStartPlaybackDefersTrackInventoryUntilPostCommitPush is the atomic
// defer/live-update proof: the start returns a provisional plan (tracks_pending,
// pending provenance, declared inventory) while the full ffprobe enumeration is
// still blocked, and the verified inventory arrives later as the existing
// inventory_updated push. The probe and the push are gated on channels, not
// sleeps, so the ordering is deterministic: the commit must not wait for the
// probe, and the push must carry the probed menu rather than the declared one.
func TestStartPlaybackDefersTrackInventoryUntilPostCommitPush(t *testing.T) {
	probedAt := time.Now().UTC()
	source := v3HandlerFixtureFile(t)
	source.ID = 700
	source.ContentID = "movie-deferred-700"
	source.FilePath = "virtual://movie/tt-deferred-700?result=cand-1"
	source.VirtualOwnerInstallationID = 5
	// The row carries candidate-complete declared evidence but no probe stamp,
	// so the resolve takes the deferred-probe path (the probe is owed) without
	// needing a provider listing.
	source.ProbeUpdatedAt = nil

	manager := playback.NewSessionManager(0, 0)
	// The resolver serves the same row so the post-commit publish re-reads the
	// exact row it probed. The row is mutex-guarded because the evidence write
	// mutates it while the publish reads it.
	resolver := &syncPlaybackFileResolver{file: *source}
	handler := NewPlaybackHandler(manager, resolver)
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"allow_4k_transcode": "true"}}
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	handler.PlaybackConfig = playbackTestConfig("", "")
	handler.RealtimeHub = playback.NewRealtimeHub()
	stubCopySeekAnchorV3(handler)

	handler.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
		return "http://127.0.0.1:8080/stream?path=" + path, nil
	})

	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	var probeOnce sync.Once
	handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		probeOnce.Do(func() { close(probeStarted) })
		<-releaseProbe
		// The real ffprobe inventory: a second audio track and a subtitle the
		// candidate declaration could not express.
		f.AudioTracks = []models.AudioTrack{
			{Codec: "aac", Channels: 2, Language: "eng", Default: true},
			{Codec: "eac3", Channels: 6, Language: "deu"},
		}
		f.SubtitleTracks = []models.SubtitleTrack{{Codec: "subrip", Language: "eng"}}
		f.CodecAudio = "eac3"
		return f, nil
	}
	// The saver models the committed catalog write: it stamps the row the
	// publish re-reads, so the delivered inventory is the probed one.
	savedCh := make(chan struct{})
	var saveOnce sync.Once
	handler.VirtualFileSaver = func(context.Context, models.VirtualFilePersistArgs) (int64, error) { return 1, nil }
	handler.VirtualFileMetadataSaver = func(_ context.Context, args models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
		var audio []models.AudioTrack
		if err := json.Unmarshal(args.AudioTracks, &audio); err != nil {
			return VirtualFileMetadataUpdateResult{}, err
		}
		var subs []models.SubtitleTrack
		if err := json.Unmarshal(args.SubtitleTracks, &subs); err != nil {
			return VirtualFileMetadataUpdateResult{}, err
		}
		resolver.update(func(f *models.MediaFile) {
			f.AudioTracks = audio
			f.SubtitleTracks = subs
			f.CodecAudio = "eac3"
			f.ProbeUpdatedAt = &probedAt
		})
		saveOnce.Do(func() { close(savedCh) })
		return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
	}

	start := startV3PlaybackForHandlerTest(t, handler, func() playback.StartRequestV3 {
		request := v3HandlerStartRequest()
		request.FileID = source.ID
		request.QualityPreference = "original"
		return request
	}())

	if start.PlaybackPlan == nil {
		t.Fatal("start returned no playback plan")
	}
	if !start.PlaybackPlan.TracksPending {
		t.Fatal("plan did not mark tracks_pending on a deferred inventory")
	}
	if start.PlaybackPlan.InventoryProvenance != string(ProbeProvenancePending) {
		t.Fatalf("plan inventory provenance = %q, want pending", start.PlaybackPlan.InventoryProvenance)
	}
	select {
	case <-probeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("deferred post-commit probe was not scheduled")
	}
	select {
	case <-savedCh:
		t.Fatal("probe inventory persisted before the test released the probe")
	default:
	}

	// Register the live connection against the created session, then release the
	// probe. The push is the expected follow-up.
	if err := manager.SetRealtimeConnection(start.SessionID, true); err != nil {
		t.Fatalf("SetRealtimeConnection: %v", err)
	}
	conn := newDeferredInventoryPush()
	registration := handler.RealtimeHub.Register(start.SessionID, conn)
	if registration == nil {
		t.Fatal("expected a realtime registration")
	}
	defer handler.RealtimeHub.Unregister(registration)

	close(releaseProbe)
	select {
	case <-savedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("probe inventory was not persisted")
	}
	select {
	case record := <-conn.ch:
		if len(record.payload.AudioTracks) != 2 {
			t.Fatalf("pushed audio tracks = %#v, want the two probed tracks", record.payload.AudioTracks)
		}
		if len(record.payload.SubtitleInventory) != 1 {
			t.Fatalf("pushed subtitle inventory = %#v, want the probed subtitle", record.payload.SubtitleInventory)
		}
		if record.payload.InventoryStatus != string(ProbeProvenanceVerified) {
			t.Fatalf("pushed inventory status = %q, want verified", record.payload.InventoryStatus)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no inventory_updated push delivered after the deferred probe")
	}

	if !start.PlaybackPlan.TracksPending {
		t.Fatal("tracks_pending cleared on the start response (it is set at resolve/commit, not after)")
	}
}

// TestDeferredInventorySpawnedAfterSameReleaseTransientRetry is the retry-path
// half of the defer contract. The first transport attempt fails with a transient
// provider error (an upstream 5xx on the remux seek anchor), so the start spends
// its one same-release retry; the retry commits the same deferred plan. The plan
// must still carry tracks_pending and the deferred probe must be spawned on the
// retry path too, or the client would keep the provisional menu forever while the
// promised inventory_updated follow-up never arrives.
func TestDeferredInventorySpawnedAfterSameReleaseTransientRetry(t *testing.T) {
	source := v3HandlerFixtureFile(t)
	source.ID = 720
	source.ContentID = "movie-deferred-retry-720"
	source.Container = "mkv"
	source.FilePath = "virtual://movie/tt-deferred-retry-720?result=cand-1"
	source.VirtualOwnerInstallationID = 5
	source.ProbeUpdatedAt = nil

	manager := playback.NewSessionManager(0, 0)
	handler := NewPlaybackHandler(manager, testPlaybackFileResolver{file: source})
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"allow_4k_transcode": "true"}}
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	handler.PlaybackConfig = playbackTestConfig(writePlaybackTestFFmpeg(t), t.TempDir())
	presetLocalRegistryV3(handler, playback.NewTransformationRegistryV3(nil))
	handler.RealtimeHub = playback.NewRealtimeHub()

	// m3u8-only output over an mkv source forces a copy HLS remux; the seek
	// position makes the transport prepare the seek anchor the transient
	// failure is injected at.
	handler.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
		return "http://127.0.0.1:8080/stream?path=" + path, nil
	})
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(_ context.Context, virtualURI string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		return ResolvedVirtualMedia{
			URL: "http://127.0.0.1:8080/stream?path=" + virtualURI,
			URI: virtualURI, CandidateID: "cand-1",
		}, nil
	})
	// The remux seek anchor fails transiently on the first timeline attempt's two
	// probes, then succeeds on the same-release retry's probe. The backoff is
	// stubbed so the retry is immediate.
	var anchorCalls atomic.Int32
	handler.copySeekAnchor = func(_ context.Context, _ string, _ string, requested float64, segmentDuration int) (float64, int, error) {
		if anchorCalls.Add(1) <= 2 {
			return 0, 0, fmt.Errorf("%w: exit status 8 (stderr: Server returned 5XX Server Error reply)", playback.ErrTransientProvider)
		}
		return requested, computeStartSegment(requested, segmentDuration), nil
	}
	handler.copySeekAnchorBackoff = func(context.Context, time.Duration) bool { return true }

	probeStarted := make(chan struct{})
	var probeOnce sync.Once
	handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		probeOnce.Do(func() { close(probeStarted) })
		f.AudioTracks = []models.AudioTrack{
			{Codec: "aac", Channels: 2, Language: "eng", Default: true},
			{Codec: "eac3", Channels: 6, Language: "deu"},
		}
		f.SubtitleTracks = []models.SubtitleTrack{{Codec: "subrip", Language: "eng"}}
		f.CodecAudio = "eac3"
		return f, nil
	}

	start := startV3PlaybackForHandlerTest(t, handler, func() playback.StartRequestV3 {
		request := v3HandlerStartRequest()
		request.FileID = source.ID
		position := 120.0
		request.StartPosition = &position
		// m3u8-only output over an mkv source forces a copy HLS remux; the seek
		// position makes the transport prepare the seek anchor the transient
		// failure is injected at.
		request.Capabilities.Containers = []string{"m3u8"}
		request.ClientPlaybackContext.Deliveries = map[string]playback.DeliveryCapabilityV3{
			playback.DeliveryClassHLSV3: {
				Enabled: true, SupportedOnDevice: true, Containers: []string{"hls"},
				VideoCodecs: []string{"h264"}, AudioDecodeCodecs: []string{"aac"},
			},
		}
		return request
	}())

	if start.PlaybackPlan == nil {
		t.Fatal("start returned no playback plan")
	}
	t.Cleanup(func() { handler.tm.CloseTranscodeSession(start.SessionID, "") })
	// Three anchor probes (two on the failed first attempt, one on the retry)
	// prove the same-release retry actually ran; anything less would mean the
	// start never reached the retry branch this test covers.
	if got := anchorCalls.Load(); got != 3 {
		t.Fatalf("copy seek anchor probes = %d, want 3 (two failed + one retry)", got)
	}
	if !start.PlaybackPlan.TracksPending {
		t.Fatal("retried plan did not mark tracks_pending")
	}
	select {
	case <-probeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("deferred probe was not spawned after the same-release transient retry")
	}
}

// TestMissingCandidateMetadataForcesProviderListing is the contradictory-codec
// regression: a row that names a concrete ?result= candidate but carries no
// audio evidence must still pay the provider listing, because the listing is the
// only declaration of the candidate's codec. Without it the merge would
// synthesize an "aac" fallback and commit it into the recipe; the provider's
// declaration ("eac3") must win instead.
func TestMissingCandidateMetadataForcesProviderListing(t *testing.T) {
	newRow := func() *models.MediaFile {
		return &models.MediaFile{
			ID:                         710,
			ContentID:                  "movie-preprobe-710",
			FilePath:                   "virtual://movie/tt-preprobe-710?result=cand-1",
			Container:                  "virtual",
			CodecVideo:                 "h264",
			CodecAudio:                 "",
			Resolution:                 "1080p",
			Duration:                   3600,
			VirtualOwnerInstallationID: 5,
			VideoTracks:                []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080, BitDepth: 8}},
			// Audio evidence is incomplete, so the listing is the only source of
			// the candidate's declared codec.
		}
	}
	var listerCalls atomic.Int32
	h := &PlaybackHandler{
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
			return "http://127.0.0.1:8080/stream?path=" + path, nil
		}),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			listerCalls.Add(1)
			return []VirtualPlaybackStream{{
				ID: "cand-1", URI: "virtual://movie/tt-preprobe-710?result=cand-1",
				Resolution: "1080p", CodecVideo: "h264", CodecAudio: "eac3", Container: "mkv",
			}}, nil
		}),
		VirtualFileLookup: func(_ context.Context, _ string) (*models.MediaFile, error) {
			return newRow(), nil
		},
	}
	file := newRow()
	req := httptest.NewRequest(http.MethodPost, "/api/v2/playback/start", nil)
	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if listerCalls.Load() != 1 {
		t.Fatalf("provider lister called %d times while candidate metadata was missing, want 1 (the listing is the only codec declaration)", listerCalls.Load())
	}
	if resolved.File == nil {
		t.Fatal("resolve returned no file")
	}
	if resolved.File.CodecAudio != "eac3" {
		t.Fatalf("resolved audio codec = %q, want the provider's declared eac3, not a synthesized fallback", resolved.File.CodecAudio)
	}
}

// TestDeferredProbeFailureEndsLoadingViaPushAndPoll is the point-1 regression:
// a deferred probe that terminally fails must end the client's loading state
// through both surfaces. The plan promised tracks_pending, so a failure that
// returned silently would leave the menu provisional forever. The failure is
// observed as inventory_status "failed" on the inventory_updated push AND on a
// subsequent inventory poll, so a client watching either surface leaves loading.
func TestDeferredProbeFailureEndsLoadingViaPushAndPoll(t *testing.T) {
	source := v3HandlerFixtureFile(t)
	source.ID = 701
	source.ContentID = "movie-deferred-fail-701"
	source.FilePath = "virtual://movie/tt-deferred-fail-701?result=cand-1"
	source.VirtualOwnerInstallationID = 5
	source.ProbeUpdatedAt = nil

	manager := playback.NewSessionManager(0, 0)
	handler := NewPlaybackHandler(manager, testPlaybackFileResolver{file: source})
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"allow_4k_transcode": "true"}}
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	handler.PlaybackConfig = playbackTestConfig("", "")
	handler.RealtimeHub = playback.NewRealtimeHub()
	handler.InstallationID = "test-install"
	stubCopySeekAnchorV3(handler)

	handler.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
		return "http://127.0.0.1:8080/stream?path=" + path, nil
	})
	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	var probeOnce sync.Once
	handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, _ *models.MediaFile) (*models.MediaFile, error) {
		probeOnce.Do(func() { close(probeStarted) })
		<-releaseProbe
		return nil, errors.New("provider probe refused the stream")
	}
	// The failed probe marks the process-wide failure damper for this
	// candidate; a re-run of the test in the same process would read the
	// recent mark and skip the deferred probe, so the plan would not be
	// provisional. Clear the key before and after so the test is repeatable.
	failureKey := virtualProbeFailureKey(source.FilePath, source.VirtualOwnerInstallationID)
	virtualProbeFailures.clear(failureKey)
	t.Cleanup(func() { virtualProbeFailures.clear(failureKey) })

	start := startV3PlaybackForHandlerTest(t, handler, func() playback.StartRequestV3 {
		request := v3HandlerStartRequest()
		request.FileID = source.ID
		request.QualityPreference = "original"
		return request
	}())
	if start.PlaybackPlan == nil || !start.PlaybackPlan.TracksPending {
		t.Fatalf("start plan = %#v, want a provisional plan", start.PlaybackPlan)
	}
	t.Cleanup(func() { handler.tm.CloseTranscodeSession(start.SessionID, "") })

	select {
	case <-probeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("deferred post-commit probe was not scheduled")
	}
	if err := manager.SetRealtimeConnection(start.SessionID, true); err != nil {
		t.Fatalf("SetRealtimeConnection: %v", err)
	}
	conn := newDeferredInventoryPush()
	registration := handler.RealtimeHub.Register(start.SessionID, conn)
	if registration == nil {
		t.Fatal("expected a realtime registration")
	}
	defer handler.RealtimeHub.Unregister(registration)

	close(releaseProbe)

	// Push surface: the failure must be delivered so a push-driven client exits
	// the loading state.
	select {
	case event := <-conn.ch:
		if event.payload.InventoryStatus != string(ProbeProvenanceFailed) {
			t.Fatalf("pushed inventory status = %q, want failed", event.payload.InventoryStatus)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no inventory_updated failure push delivered after the deferred probe failed")
	}

	// Poll surface: a client that never holds the push connection must observe
	// the same terminal status from the inventory endpoint.
	deadline := time.Now().Add(2 * time.Second)
	for {
		inventory, err := handler.GetPlaybackInventoryV2(newAuthorizedPlaybackContext(), PlaybackCaller{UserID: 1, ProfileID: "profile-1", InstallationID: "test-install"}, start.SessionID)
		if err != nil {
			t.Fatalf("GetPlaybackInventoryV2: %v", err)
		}
		if inventory.InventoryStatus == string(ProbeProvenanceFailed) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("inventory poll status = %q, want failed", inventory.InventoryStatus)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestDeferredProbePendingThenVerifiedPollTransitions proves the poll surface
// distinguishes an unfinished probe from a failed one: while the probe is
// outstanding the inventory reports pending, and once it verifies the poll
// reports verified. A poll that collapsed both into "declared" would leave a
// client unable to tell "still loading" from "done".
func TestDeferredProbePendingThenVerifiedPollTransitions(t *testing.T) {
	source := v3HandlerFixtureFile(t)
	source.ID = 702
	source.ContentID = "movie-deferred-poll-702"
	source.FilePath = "virtual://movie/tt-deferred-poll-702?result=cand-1"
	source.VirtualOwnerInstallationID = 5
	source.ProbeUpdatedAt = nil

	manager := playback.NewSessionManager(0, 0)
	resolver := &syncPlaybackFileResolver{file: *source}
	handler := NewPlaybackHandler(manager, resolver)
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"allow_4k_transcode": "true"}}
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	handler.PlaybackConfig = playbackTestConfig("", "")
	handler.RealtimeHub = playback.NewRealtimeHub()
	handler.InstallationID = "test-install"
	stubCopySeekAnchorV3(handler)

	handler.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
		return "http://127.0.0.1:8080/stream?path=" + path, nil
	})
	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	var probeOnce sync.Once
	handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		probeOnce.Do(func() { close(probeStarted) })
		<-releaseProbe
		f.AudioTracks = []models.AudioTrack{{Codec: "eac3", Channels: 6, Language: "deu"}}
		f.CodecAudio = "eac3"
		return f, nil
	}
	// The deferred probe's evidence write is durable — committed synchronously
	// before the outcome and publish — so the saver models the committed catalog
	// row the poll reads.
	probedAt := time.Now().UTC()
	handler.VirtualFileMetadataSaver = func(_ context.Context, args models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
		var audio []models.AudioTrack
		if err := json.Unmarshal(args.AudioTracks, &audio); err != nil {
			return VirtualFileMetadataUpdateResult{}, err
		}
		resolver.update(func(f *models.MediaFile) {
			f.AudioTracks = audio
			f.CodecAudio = args.CodecAudio
			f.ProbeUpdatedAt = &probedAt
		})
		return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
	}

	start := startV3PlaybackForHandlerTest(t, handler, func() playback.StartRequestV3 {
		request := v3HandlerStartRequest()
		request.FileID = source.ID
		request.QualityPreference = "original"
		return request
	}())
	if start.PlaybackPlan == nil {
		t.Fatal("start returned no playback plan")
	}
	t.Cleanup(func() { handler.tm.CloseTranscodeSession(start.SessionID, "") })
	select {
	case <-probeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("deferred post-commit probe was not scheduled")
	}

	pending, err := handler.GetPlaybackInventoryV2(newAuthorizedPlaybackContext(), PlaybackCaller{UserID: 1, ProfileID: "profile-1", InstallationID: "test-install"}, start.SessionID)
	if err != nil {
		t.Fatalf("GetPlaybackInventoryV2: %v", err)
	}
	if pending.InventoryStatus != string(ProbeProvenancePending) {
		t.Fatalf("in-flight poll status = %q, want pending", pending.InventoryStatus)
	}

	close(releaseProbe)
	deadline := time.Now().Add(2 * time.Second)
	for {
		verified, err := handler.GetPlaybackInventoryV2(newAuthorizedPlaybackContext(), PlaybackCaller{UserID: 1, ProfileID: "profile-1", InstallationID: "test-install"}, start.SessionID)
		if err != nil {
			t.Fatalf("GetPlaybackInventoryV2: %v", err)
		}
		if verified.InventoryStatus == string(ProbeProvenanceVerified) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("post-probe poll status = %q, want verified", verified.InventoryStatus)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestSaturatedGateDefersProbeInsteadOfProbingOnRequestPath is the point-2
// regression: when the aggregate detached gate is saturated, the deferred probe
// must not fall back to a synchronous probe before the response returns. The
// start request has to come back promptly, and the probe must run only once a
// gate slot frees — proving saturation became backpressure, not a first-byte
// probe.
func TestSaturatedGateDefersProbeInsteadOfProbingOnRequestPath(t *testing.T) {
	source := v3HandlerFixtureFile(t)
	source.ID = 703
	source.ContentID = "movie-deferred-gate-703"
	source.FilePath = "virtual://movie/tt-deferred-gate-703?result=cand-1"
	source.VirtualOwnerInstallationID = 5
	source.ProbeUpdatedAt = nil

	manager := playback.NewSessionManager(0, 0)
	resolver := &syncPlaybackFileResolver{file: *source}
	handler := NewPlaybackHandler(manager, resolver)
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"allow_4k_transcode": "true"}}
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	handler.PlaybackConfig = playbackTestConfig("", "")
	stubCopySeekAnchorV3(handler)
	handler.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
		return "http://127.0.0.1:8080/stream?path=" + path, nil
	})

	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	var probeOnce sync.Once
	handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		probeOnce.Do(func() { close(probeStarted) })
		<-releaseProbe
		return f, nil
	}
	// The durable evidence write commits synchronously; model the committed row.
	probedAt := time.Now().UTC()
	handler.VirtualFileMetadataSaver = func(_ context.Context, args models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
		resolver.update(func(f *models.MediaFile) {
			f.AudioTracks = []models.AudioTrack{{Codec: args.CodecAudio, Channels: 2}}
			f.ProbeUpdatedAt = &probedAt
		})
		return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
	}

	// Saturate the aggregate detached gate so the deferred worker has nowhere
	// to run. The request path uses only non-blocking acquisitions, so the start
	// must still complete.
	gate := handler.detachedGate()
	held := gate.capacity()
	for i := 0; i < held; i++ {
		if !gate.tryAcquire() {
			t.Fatalf("could not hold slot %d of %d", i, held)
		}
	}
	defer func() {
		for i := 0; i < held; i++ {
			gate.release()
		}
	}()

	startDone := make(chan playback.DecisionResponseV3, 1)
	go func() {
		startDone <- startV3PlaybackForHandlerTest(t, handler, func() playback.StartRequestV3 {
			request := v3HandlerStartRequest()
			request.FileID = source.ID
			request.QualityPreference = "original"
			return request
		}())
	}()

	var start playback.DecisionResponseV3
	select {
	case start = <-startDone:
	case <-time.After(3 * time.Second):
		t.Fatal("start blocked while the detached gate was saturated")
	}
	if start.PlaybackPlan == nil || !start.PlaybackPlan.TracksPending {
		t.Fatalf("start plan = %#v, want a provisional plan", start.PlaybackPlan)
	}
	t.Cleanup(func() { handler.tm.CloseTranscodeSession(start.SessionID, "") })

	// The response is out and the probe must not have run on the request path:
	// the saturated gate parked it.
	select {
	case <-probeStarted:
		t.Fatal("probe ran synchronously on the request path while the detached gate was saturated")
	case <-time.After(100 * time.Millisecond):
	}

	// Freeing a slot lets the parked worker run the probe.
	gate.release()
	held--
	select {
	case <-probeStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("deferred probe never ran after a gate slot freed")
	}
	close(releaseProbe)
}

// TestDeferredProbeOutcomeFencedByCandidateBinding proves a deferred probe that
// completes after the session rotated to a different candidate does not write
// its outcome onto the new binding. The old candidate's disposition says
// nothing about the new one; the new binding's own probe owns its lifecycle.
func TestDeferredProbeOutcomeFencedByCandidateBinding(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", 100, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := manager.SetVirtualSource(session.ID, "virtual://movie/tt-fence?result=old", 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	h := NewPlaybackHandler(manager)

	// Capture the live binding generation the way admission does, so the fence
	// checks the candidate and generation together.
	deferred := readmitDeferredProbe(t, h, deferredOverflowProbe(session, "virtual://movie/tt-fence?result=old"))
	// Intact while the binding still names the probed candidate and generation.
	if !h.deferredProbeBindingIntact(deferred)() {
		t.Fatal("binding fence reported false for the still-bound candidate")
	}
	// Rotate the session to a different candidate: the old probe is now stale.
	if err := manager.SetVirtualSource(session.ID, "virtual://movie/tt-fence?result=new", 5); err != nil {
		t.Fatalf("SetVirtualSource rotate: %v", err)
	}
	if h.deferredProbeBindingIntact(deferred)() {
		t.Fatal("binding fence accepted a probe for the superseded candidate")
	}
}
