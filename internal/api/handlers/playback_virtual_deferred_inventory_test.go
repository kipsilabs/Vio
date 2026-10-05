package handlers

import (
	"context"
	"encoding/json"
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
		return nil
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
	// exact row it probed.
	handler := NewPlaybackHandler(manager, testPlaybackFileResolver{file: source})
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
		source.AudioTracks = audio
		source.SubtitleTracks = subs
		source.CodecAudio = "eac3"
		source.ProbeUpdatedAt = &probedAt
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

// TestPreprobeLivenessHitSkipsProviderListing proves the preprobe half: a fresh
// listing observation for the content lets a row that already names a concrete
// candidate skip the provider list, and without the observation the same resolve
// still lists (so the memo can only remove a listing a live call justified).
func TestPreprobeLivenessHitSkipsProviderListing(t *testing.T) {
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
			// Audio evidence is incomplete, so a resolve without the preprobe
			// observation must list the provider to fill it in.
		}
	}
	newHandler := func(listerCalls *atomic.Int32) *PlaybackHandler {
		return &PlaybackHandler{
			VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
				return "http://127.0.0.1:8080/stream?path=" + path, nil
			}),
			VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
				listerCalls.Add(1)
				return []VirtualPlaybackStream{{
					ID: "cand-1", URI: "virtual://movie/tt-preprobe-710?result=cand-1",
					Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
				}}, nil
			}),
			VirtualFileLookup: func(_ context.Context, _ string) (*models.MediaFile, error) {
				return newRow(), nil
			},
		}
	}

	t.Run("hit skips the list", func(t *testing.T) {
		var listerCalls atomic.Int32
		h := newHandler(&listerCalls)
		h.recordVirtualPreprobeLiveness(&fileIdentityV3{contentID: "movie-preprobe-710", ownerID: 5}, time.Now())
		file := newRow()
		req := httptest.NewRequest(http.MethodPost, "/api/v2/playback/start", nil)
		resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false)
		if err != nil {
			t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
		}
		if listerCalls.Load() != 0 {
			t.Fatalf("provider lister called %d times on a preprobe hit, want 0", listerCalls.Load())
		}
		if resolved.URI != file.FilePath {
			t.Fatalf("resolved URI = %q, want the row's own candidate %q", resolved.URI, file.FilePath)
		}
	})

	t.Run("miss lists as before", func(t *testing.T) {
		var listerCalls atomic.Int32
		h := newHandler(&listerCalls)
		file := newRow()
		req := httptest.NewRequest(http.MethodPost, "/api/v2/playback/start", nil)
		if _, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false); err != nil {
			t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
		}
		if listerCalls.Load() != 1 {
			t.Fatalf("provider lister called %d times without a preprobe observation, want 1", listerCalls.Load())
		}
	})
}

// TestVirtualPreprobeCacheExpiresAndIsBounded pins the TTL and admission bound
// of the liveness memo so a stale observation cannot suppress a listing forever
// and the map cannot grow without limit.
func TestVirtualPreprobeCacheExpiresAndIsBounded(t *testing.T) {
	previousTTL := virtualPreprobeTTL
	virtualPreprobeTTL = 50 * time.Millisecond
	defer func() { virtualPreprobeTTL = previousTTL }()

	cache := newVirtualPreprobeCache(2)
	now := time.Now()
	cache.record("a", now)
	if !cache.hit("a", now) {
		t.Fatal("a fresh observation did not hit")
	}
	if cache.hit("a", now.Add(virtualPreprobeWindow())) {
		t.Fatal("an observation did not expire at the window boundary")
	}

	// Admission past the ceiling stays bounded.
	cache.record("b", now)
	cache.record("c", now)
	if len(cache.entries) > 2 {
		t.Fatalf("cache size = %d, want <= 2", len(cache.entries))
	}
}
