package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/remotestream"
)

// coldCatalogResolver serves a fresh copy of one catalog row on every load, so a
// probe or resolve that mutates its in-memory file cannot change what the next
// replan reads. That keeps the row in its cold, never-persisted state: the
// neutral virtual path with no adopted ?result= and no probe evidence.
type coldCatalogResolver struct {
	file models.MediaFile
}

func (r *coldCatalogResolver) GetByID(_ context.Context, id int) (*models.MediaFile, error) {
	if id != r.file.ID {
		return nil, errors.New("media file not found")
	}
	copied := r.file
	return &copied, nil
}

// TestVirtualColdSessionSeekReanchorStaysOnPinnedCandidate is the issue #275
// repro step 4 regression. A never-played virtual item starts cold: the catalog
// row still names the provider-neutral path (no persisted ?result=), so the
// first plan is provisional and the session binds a concrete candidate at
// resolve time. A random seek in that first session must stay on the session's
// pinned candidate — the plan keeps the same EffectiveVirtualURI and
// EffectiveMediaFileID — and must not replace the running transport. A
// mid-session re-resolve that churned the listing token, changed the plan
// identity, or rebuilt the stream is exactly the reload #275 reports.
func TestVirtualColdSessionSeekReanchorStaysOnPinnedCandidate(t *testing.T) {
	const (
		neutralURI = "virtual://movie/tt-275-cold"
		pinnedURI  = neutralURI + "?result=pinned"
	)

	// Cold row: the row has never been played, so it still names the neutral
	// path and carries no persisted candidate.
	source := v3HandlerFixtureFile(t)
	source.ID = 8275
	source.ContentID = "movie-275-cold"
	source.FilePath = neutralURI
	source.VirtualOwnerInstallationID = 5
	source.Container = "mkv"
	source.CodecVideo = "h264"
	source.CodecAudio = "aac"
	source.Resolution = "1080p"

	manager := playback.NewSessionManager(0, 0)
	handler := NewPlaybackHandler(manager, &coldCatalogResolver{file: *source})
	stubCopySeekAnchorV3(handler)
	handler.PlaybackConfig = playbackTestConfig(writePlaybackTestFFmpeg(t), t.TempDir())
	presetLocalRegistryV3(handler, playback.NewTransformationRegistryV3(nil))
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{
		"allow_4k_transcode":                     "true",
		"playback.max_virtual_failover_attempts": "3",
	}}
	relay := remotestream.NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()
	handler.RemoteStreamRelay = relay
	handler.AllowPrivateStreams = func(int) bool { return true }
	var listCalls, resolveCalls atomic.Int32
	handler.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
		return "http://127.0.0.1:9/stream?path=" + path, nil
	})
	handler.VirtualPlaybackStreamLister = VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
		listCalls.Add(1)
		return []VirtualPlaybackStream{{
			ID: "pinned", URI: pinnedURI,
			Resolution: source.Resolution, CodecVideo: source.CodecVideo, CodecAudio: source.CodecAudio, Container: "mkv",
		}}, nil
	})
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		resolveCalls.Add(1)
		// The only candidate this provider ever lists is the session pin.
		if virtualResultCandidateID(uri) != "pinned" {
			return ResolvedVirtualMedia{}, absentSessionPinError(virtualResultCandidateID(uri))
		}
		return ResolvedVirtualMedia{
			URL: "http://127.0.0.1:9/pinned.mkv", URI: pinnedURI, CandidateID: "pinned",
			ProviderVideoHash: "hash-pinned", ProviderReleaseName: "Movie.Pinned.1080p",
		}, nil
	})
	handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, file *models.MediaFile) (*models.MediaFile, error) {
		file.VideoTracks = source.VideoTracks
		file.AudioTracks = source.AudioTracks
		stamp := time.Now().UTC()
		file.ProbeUpdatedAt = &stamp
		return file, nil
	}

	start := v3HandlerStartRequest()
	start.FileID = source.ID
	start.QualityPreference = "auto"
	start.Capabilities.Containers = []string{"m3u8"}
	start.ClientPlaybackContext.Deliveries = map[string]playback.DeliveryCapabilityV3{
		playback.DeliveryClassHLSV3: {
			Enabled: true, SupportedOnDevice: true,
			Containers: []string{"hls"}, VideoCodecs: []string{"h264"}, AudioDecodeCodecs: []string{"aac"},
		},
	}

	rr := httptest.NewRecorder()
	handler.HandleStartPlayback(rr, httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", strings.NewReader(marshalV3StartRequest(t, start))).WithContext(newAuthorizedPlaybackContext()))
	if rr.Code != http.StatusCreated {
		t.Fatalf("start status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var started playback.DecisionResponseV3
	if err := json.Unmarshal(rr.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if started.PlaybackPlan == nil {
		t.Fatalf("cold start returned no plan: outcome=%s terminal=%+v", started.Outcome, started.Terminal)
	}
	t.Logf("cold start: delivery=%s effective_file=%d virtual_uri=%q", started.PlaybackPlan.Delivery, started.PlaybackPlan.EffectiveMediaFileID, started.PlaybackPlan.EffectiveVirtualURI)
	session, err := manager.GetSession(started.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if session.VirtualSourceURI != pinnedURI {
		t.Fatalf("cold start bound %q, want the pinned candidate %q", session.VirtualSourceURI, pinnedURI)
	}
	if started.PlaybackPlan.EffectiveVirtualURI != pinnedURI {
		t.Fatalf("cold start plan bound %q, want the pinned candidate %q", started.PlaybackPlan.EffectiveVirtualURI, pinnedURI)
	}

	transport := handler.tm.GetTranscodeSession(started.SessionID)
	if transport == nil {
		t.Fatalf("cold start created no active transport for delivery %s", started.PlaybackPlan.Delivery)
	}
	t.Cleanup(func() { handler.tm.CloseTranscodeSession(started.SessionID, "") })
	beforeGeneration := transport.SegmentGeneration()
	beforeURL := started.PlaybackPlan.Stream.URL
	// Reset so the seek's provider contact is measured separately. The invariant
	// under test is that the seek never replaces the pinned candidate or the
	// running transport; a cold, unverified pin may still re-resolve once.
	listCalls.Store(0)
	resolveCalls.Store(0)

	replanned := postPlaybackReplanV3(t, handler, started.SessionID, playback.ReplanRequestV3{
		ProtocolVersion:       playback.ProtocolV3,
		Operation:             playback.ReplanOperationSeekReanchorV3,
		PlaybackAttemptID:     start.PlaybackAttemptID,
		ReplanRequestID:       "seek-reanchor-cold-0001",
		FailedPlanID:          started.PlaybackPlan.PlanID,
		PlanAttemptID:         "seek-reanchor-cold-plan-0001",
		PlanAttemptKey:        started.PlaybackPlan.PlanAttemptKey,
		AttemptCount:          1,
		QualityPreference:     start.QualityPreference,
		PositionSeconds:       120,
		SelectedTracks:        started.PlaybackPlan.SelectedTracks,
		Capabilities:          start.Capabilities,
		ClientPlaybackContext: start.ClientPlaybackContext,
	})
	if replanned.PlaybackPlan == nil {
		t.Fatalf("seek reanchor returned no plan: outcome=%s terminal=%+v", replanned.Outcome, replanned.Terminal)
	}
	// The seek may re-resolve an unverified cold pin, but it must never land on
	// a different candidate: resolver/list counters are informational here, and
	// the stub refuses any URI that is not the pin.
	t.Logf("cold seek provider contact: list=%d resolve=%d", listCalls.Load(), resolveCalls.Load())
	if replanned.PlaybackPlan.EffectiveVirtualURI != started.PlaybackPlan.EffectiveVirtualURI {
		t.Fatalf("seek reanchor changed the pinned candidate: %q -> %q", started.PlaybackPlan.EffectiveVirtualURI, replanned.PlaybackPlan.EffectiveVirtualURI)
	}
	if replanned.PlaybackPlan.EffectiveMediaFileID != started.PlaybackPlan.EffectiveMediaFileID {
		t.Fatalf("seek reanchor changed the effective file: %d -> %d", started.PlaybackPlan.EffectiveMediaFileID, replanned.PlaybackPlan.EffectiveMediaFileID)
	}
	if replanned.PlaybackPlan.PlanID != started.PlaybackPlan.PlanID {
		t.Fatalf("seek reanchor changed plan identity: %q -> %q", started.PlaybackPlan.PlanID, replanned.PlaybackPlan.PlanID)
	}
	if !sameSelectedTracksV3(replanned.PlaybackPlan.SelectedTracks, started.PlaybackPlan.SelectedTracks) {
		t.Fatalf("seek reanchor changed selected tracks: %#v -> %#v", started.PlaybackPlan.SelectedTracks, replanned.PlaybackPlan.SelectedTracks)
	}
	after := handler.tm.GetTranscodeSession(started.SessionID)
	if after != transport {
		t.Fatalf("seek reanchor replaced the active transport: before=%p after=%p", transport, after)
	}
	if after == nil || !after.IsRunning() {
		t.Fatal("seek reanchor stopped the active transport")
	}
	if after.SegmentGeneration() != beforeGeneration {
		t.Fatalf("seek reanchor restarted FFmpeg: generation %d -> %d", beforeGeneration, after.SegmentGeneration())
	}
	if replanned.PlaybackPlan.Stream.URL != beforeURL {
		t.Fatalf("seek reanchor changed the stream URL: %q -> %q", beforeURL, replanned.PlaybackPlan.Stream.URL)
	}
	if replanned.PlaybackPlan.Timeline.SourceStartSeconds != 120 {
		t.Fatalf("seek reanchor lost the requested position: timeline %#v", replanned.PlaybackPlan.Timeline)
	}
	// A seek reanchor replays the durable route verbatim, so it must not publish
	// the additive delivery-swap marker the client reloads on.
	if replanned.PlaybackPlan.DeliveryChange != nil {
		t.Fatalf("seek reanchor published a delivery swap: %#v", replanned.PlaybackPlan.DeliveryChange)
	}
	bound, err := manager.GetSession(started.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if bound.VirtualSourceURI != pinnedURI || bound.MediaFileID != started.PlaybackPlan.EffectiveMediaFileID {
		t.Fatalf("seek reanchor moved the session binding: uri=%q file=%d", bound.VirtualSourceURI, bound.MediaFileID)
	}
}
