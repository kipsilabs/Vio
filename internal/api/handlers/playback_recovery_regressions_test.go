package handlers

import (
	"context"
	"reflect"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// applyReplanProgressV3 records one durable progress sample so the next replan
// sees the attempt as having shown frames. The store's sequencing state is
// what executeReplanV3 reads; the session's live position is deliberately not
// involved.
func applyReplanProgressV3(t *testing.T, handler *PlaybackHandler, sessionID string) {
	t.Helper()
	store, ok := handler.PlanStoreV3.(playback.ProgressStoreV3)
	if !ok {
		t.Fatal("plan store does not implement progress")
	}
	receipt, err := store.ApplyProgress(context.Background(), sessionID, playback.ProgressSampleV3{
		Sequence: 3,
		Position: 42,
	})
	if err != nil {
		t.Fatalf("apply progress: %v", err)
	}
	if receipt.Accepted == nil || receipt.Accepted.Sequence != 3 {
		t.Fatalf("progress receipt = %#v, want sequence 3 accepted", receipt)
	}
}

func newProgressReissueHandlerV3(t *testing.T) *PlaybackHandler {
	t.Helper()
	file := v3HandlerFixtureFile(t)
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0), testPlaybackFileResolver{file: file})
	stubCopySeekAnchorV3(handler)
	// Video adaptation is unavailable: after the copy route is retired the
	// planner can only fall through to transcoding_disabled / routes_exhausted.
	handler.PlaybackConfig = playbackTestConfigNoTranscode(t.TempDir())
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"transcode_enabled": "false", "allow_4k_transcode": "false"}}
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	return handler
}

func progressReissueStartV3(t *testing.T, handler *PlaybackHandler) (playback.DecisionResponseV3, playback.StartRequestV3) {
	t.Helper()
	request := v3HandlerStartRequest()
	request.QualityPreference = "original"
	// HLS stays declared so a recovery that exhausts the copy route reaches
	// the transcoding-disabled branch instead of client_hls_unsupported. Its
	// declared video codec is intentionally not the source's, so the HLS copy
	// route is unavailable and only video adaptation remains.
	request.ClientPlaybackContext.Deliveries[playback.DeliveryClassHLSV3] = playback.DeliveryCapabilityV3{
		Enabled: true, SupportedOnDevice: true, Containers: []string{"hls"},
		VideoCodecs: []string{"hevc"}, AudioDecodeCodecs: []string{"aac"},
	}
	started := startV3PlaybackForHandlerTest(t, handler, request)
	if started.PlaybackPlan == nil || started.PlaybackPlan.Delivery != playback.DeliveryOriginalHTTPV3 {
		t.Fatalf("start plan = %#v, want original HTTP", started.PlaybackPlan)
	}
	return started, request
}

func progressReissueRequestV3(started playback.DecisionResponseV3, startRequest playback.StartRequestV3, requestID string, attemptCount int, attemptedKeys []string) playback.ReplanRequestV3 {
	return playback.ReplanRequestV3{
		ProtocolVersion:       playback.ProtocolV3,
		Operation:             playback.ReplanOperationFailureRecoveryV3,
		PlaybackAttemptID:     startRequest.PlaybackAttemptID,
		ReplanRequestID:       requestID,
		FailedPlanID:          started.PlaybackPlan.PlanID,
		PlanAttemptID:         requestID,
		PlanAttemptKey:        started.PlaybackPlan.PlanAttemptKey,
		AttemptedPlanKeys:     attemptedKeys,
		AttemptCount:          attemptCount,
		QualityPreference:     "original",
		PositionSeconds:       42,
		SelectedTracks:        started.PlaybackPlan.SelectedTracks,
		Failure:               playback.FailureV3{Classification: "transport_stall"},
		Capabilities:          startRequest.Capabilities,
		ClientPlaybackContext: startRequest.ClientPlaybackContext,
	}
}

// A recovery after frames reissues the recipe that was playing, with a fresh
// plan identity so the client does not mistake it for a cached failed plan. The
// next failure of that reissue retires the route as before.
func TestHandleReplanPlaybackV3ReissuesRecipeAfterProgress(t *testing.T) {
	handler := newProgressReissueHandlerV3(t)
	started, startRequest := progressReissueStartV3(t, handler)
	applyReplanProgressV3(t, handler, started.SessionID)

	first := postPlaybackReplanV3(t, handler, started.SessionID, progressReissueRequestV3(started, startRequest, "replan-progress-reissue-0001", 1, []string{started.PlaybackPlan.PlanAttemptKey}))
	if first.Terminal != nil || first.PlaybackPlan == nil {
		t.Fatalf("reissue response = %#v terminal = %#v", first, first.Terminal)
	}
	if first.PlaybackPlan.Delivery != started.PlaybackPlan.Delivery ||
		!reflect.DeepEqual(first.PlaybackPlan.EffectiveRecipe, started.PlaybackPlan.EffectiveRecipe) {
		t.Fatalf("reissued route = %s / %#v, want the recipe that played", first.PlaybackPlan.Delivery, first.PlaybackPlan.EffectiveRecipe)
	}
	if first.PlaybackPlan.PlanID == started.PlaybackPlan.PlanID {
		t.Fatalf("reissued plan id = %q, want a fresh id", first.PlaybackPlan.PlanID)
	}
	if first.PlaybackPlan.PlanAttemptKey == started.PlaybackPlan.PlanAttemptKey {
		t.Fatalf("reissued attempt key = %q, want a fresh key", first.PlaybackPlan.PlanAttemptKey)
	}

	// The reissued transport is a second transport; its failure is the second
	// recovery and must retire the route rather than reissue it forever.
	second := postPlaybackReplanV3(t, handler, started.SessionID, progressReissueRequestV3(first, startRequest, "replan-progress-reissue-0002", 2, []string{
		started.PlaybackPlan.PlanAttemptKey,
		first.PlaybackPlan.PlanAttemptKey,
	}))
	if second.Terminal == nil || second.Terminal.Reason != playback.TerminalRoutesExhaustedV3 || !second.Terminal.Retryable {
		t.Fatalf("second recovery = %#v terminal = %#v, want retryable routes_exhausted", second, second.Terminal)
	}
}

// Without a durable progress sample the route never proved it could reach this
// client. The ordinary failure path retires it even when the client reports a
// transport stall.
func TestHandleReplanPlaybackV3RetiresRecipeBeforeFirstFrame(t *testing.T) {
	handler := newProgressReissueHandlerV3(t)
	started, startRequest := progressReissueStartV3(t, handler)

	response := postPlaybackReplanV3(t, handler, started.SessionID, progressReissueRequestV3(started, startRequest, "replan-no-progress-0001", 1, []string{started.PlaybackPlan.PlanAttemptKey}))
	if response.Terminal == nil || response.Terminal.Reason != playback.TerminalRoutesExhaustedV3 || !response.Terminal.Retryable {
		t.Fatalf("pre-first-frame recovery = %#v terminal = %#v, want routes_exhausted", response, response.Terminal)
	}
}

func newVirtualSameCandidateRecoveryHandlerV3(t *testing.T) (*PlaybackHandler, *models.MediaFile, map[int]*models.MediaFile) {
	t.Helper()
	complete := v3HandlerFixtureFile(t)
	complete.FilePath = "virtual://movie/tt-recovery-carry"
	complete.VirtualOwnerInstallationID = 5
	files := map[int]*models.MediaFile{complete.ID: complete}
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0), mapPlaybackFileResolver{files: files})
	stubCopySeekAnchorV3(handler)
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"allow_4k_transcode": "true"}}
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	handler.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
		return "http://127.0.0.1:8080/stream?path=" + path, nil
	})
	handler.VirtualCandidateFileLookup = func(_ context.Context, path, _ string, _ string, _ int) (*models.MediaFile, error) {
		candidate := *files[complete.ID]
		candidate.FilePath = path
		candidate.VideoTracks = append([]models.VideoTrack(nil), candidate.VideoTracks...)
		candidate.AudioTracks = append([]models.AudioTrack(nil), candidate.AudioTracks...)
		return &candidate, nil
	}
	return handler, complete, files
}

func startVirtualSameCandidateRecoveryV3(t *testing.T, handler *PlaybackHandler, complete *models.MediaFile) (playback.DecisionResponseV3, playback.StartRequestV3) {
	t.Helper()
	startRequest := v3HandlerStartRequest()
	startRequest.FileID = complete.ID
	startRequest.QualityPreference = "auto"
	startRequest.ClientPlaybackContext.Deliveries[playback.DeliveryClassHLSV3] = playback.DeliveryCapabilityV3{
		Enabled: true, SupportedOnDevice: true, Containers: []string{"hls"}, VideoCodecs: []string{"h264"}, AudioDecodeCodecs: []string{"aac"},
	}
	startRequest.ClientPlaybackContext.Deliveries[playback.DeliveryClassProgressiveV3] = playback.DeliveryCapabilityV3{
		Enabled: true, SupportedOnDevice: true, Containers: []string{"mp4"}, VideoCodecs: []string{"h264"}, AudioDecodeCodecs: []string{"aac"},
	}
	started := startV3PlaybackForHandlerTest(t, handler, startRequest)
	if started.PlaybackPlan == nil {
		t.Fatalf("virtual start response = %#v", started)
	}
	if started.PlaybackPlan.InventoryStatus != "declared" {
		t.Fatalf("start inventory status = %q, want declared", started.PlaybackPlan.InventoryStatus)
	}
	return started, startRequest
}

// A same-candidate failure recovery must keep the video descriptor the start
// plan validated when the rehydrated catalog row lost it. The top-up is
// in-memory: the catalog row stays unprobed and the plan stays declared.
func TestHandleReplanPlaybackV3CarriesDescriptorForSameCandidateRecovery(t *testing.T) {
	handler, complete, files := newVirtualSameCandidateRecoveryHandlerV3(t)
	started, startRequest := startVirtualSameCandidateRecoveryV3(t, handler, complete)

	// The row still names the pinned candidate but lost the probe's frame rate
	// and bitrate, exactly as a fresh listing row does.
	degraded := &models.MediaFile{
		ID:                         complete.ID,
		ContentID:                  complete.ContentID,
		FilePath:                   complete.FilePath,
		Container:                  "virtual",
		CodecVideo:                 complete.CodecVideo,
		CodecAudio:                 complete.CodecAudio,
		Resolution:                 complete.Resolution,
		Duration:                   complete.Duration,
		VirtualOwnerInstallationID: complete.VirtualOwnerInstallationID,
		VideoTracks: []models.VideoTrack{{
			Codec: "h264", Profile: "high", Level: 41, Width: 1920, Height: 1080, BitDepth: 8,
			VideoRange: "SDR", VideoRangeType: "SDR",
		}},
		AudioTracks: complete.AudioTracks,
	}
	files[complete.ID] = degraded

	failedKey := playback.PlanAttemptKeyV3(*started.PlaybackPlan, startRequest.ClientPlaybackContext.Output.OutputContextID, nil)
	response := postPlaybackReplanV3(t, handler, started.SessionID, playback.ReplanRequestV3{
		ProtocolVersion:       playback.ProtocolV3,
		Operation:             playback.ReplanOperationFailureRecoveryV3,
		PlaybackAttemptID:     "attempt-handler-0001",
		ReplanRequestID:       "replan-recovery-carry-0001",
		FailedPlanID:          started.PlaybackPlan.PlanID,
		PlanAttemptID:         "plan-attempt-recovery-carry-0001",
		PlanAttemptKey:        failedKey,
		AttemptedPlanKeys:     []string{failedKey},
		AttemptCount:          1,
		QualityPreference:     "auto",
		PositionSeconds:       12,
		Failure:               playback.FailureV3{Classification: "browser_engine_unsupported_source"},
		Capabilities:          startRequest.Capabilities,
		ClientPlaybackContext: startRequest.ClientPlaybackContext,
	})
	if response.Terminal != nil || response.PlaybackPlan == nil {
		t.Fatalf("same-candidate recovery = %#v terminal = %#v", response, response.Terminal)
	}
	if response.PlaybackPlan.Source.FrameRate <= 0 || response.PlaybackPlan.Source.BitrateKbps <= 0 {
		t.Fatalf("source descriptor = %#v, want the running plan's frame rate and bitrate", response.PlaybackPlan.Source)
	}
	if response.PlaybackPlan.InventoryStatus != "declared" {
		t.Fatalf("inventory status = %q, want declared for an unprobed carry", response.PlaybackPlan.InventoryStatus)
	}
	stored := files[complete.ID]
	if stored.ProbeUpdatedAt != nil {
		t.Fatal("descriptor carry stamped the catalog row as probed")
	}
	if stored.Bitrate != 0 || len(stored.VideoTracks) != 1 || stored.VideoTracks[0].FrameRate != "" || stored.VideoTracks[0].Bitrate != 0 {
		t.Fatalf("descriptor carry wrote to the catalog row: %#v", stored)
	}
}

func TestSameCandidateFailureDescriptorCarryV3Fences(t *testing.T) {
	newSession := func() *playback.Session {
		return &playback.Session{
			VirtualSourceURI:                 "virtual://movie/item?result=candidate-a",
			VirtualSourceOwnerInstallationID: 7,
		}
	}
	newRecord := func() *playback.AttemptRecordV3 {
		return &playback.AttemptRecordV3{CurrentPlan: playback.PlanV3{
			EffectiveVirtualURI: "virtual://movie/item?result=candidate-a",
		}}
	}
	newResolved := func() resolvedVirtualPlaybackSource {
		return resolvedVirtualPlaybackSource{
			URI:     "virtual://movie/item?result=candidate-a",
			OwnerID: 7,
			File:    &models.MediaFile{ID: 42, FilePath: "virtual://movie/item?result=candidate-a"},
		}
	}

	tests := []struct {
		name       string
		mutate     func(*playback.Session, *playback.AttemptRecordV3, *resolvedVirtualPlaybackSource)
		provenance bool
		rotation   bool
		wantCarry  bool
	}{
		{name: "same candidate and installation", provenance: true, wantCarry: true},
		{name: "no session provenance", provenance: false},
		{name: "decode rotation", provenance: true, rotation: true},
		{
			name:       "rotated candidate",
			provenance: true,
			mutate: func(_ *playback.Session, _ *playback.AttemptRecordV3, resolved *resolvedVirtualPlaybackSource) {
				resolved.URI = "virtual://movie/item?result=candidate-b"
			},
		},
		{
			name:       "changed installation",
			provenance: true,
			mutate: func(_ *playback.Session, _ *playback.AttemptRecordV3, resolved *resolvedVirtualPlaybackSource) {
				resolved.OwnerID = 8
			},
		},
		{
			name:       "plan no longer names the pin",
			provenance: true,
			mutate: func(_ *playback.Session, record *playback.AttemptRecordV3, _ *resolvedVirtualPlaybackSource) {
				record.CurrentPlan.EffectiveVirtualURI = "virtual://movie/item?result=candidate-b"
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			session, record, candidate := newSession(), newRecord(), newResolved()
			if tc.mutate != nil {
				tc.mutate(session, record, &candidate)
			}
			got := sameCandidateFailureDescriptorCarryV3(playback.ReplanOperationFailureRecoveryV3, session, record, tc.provenance, candidate, tc.rotation)
			if got != tc.wantCarry {
				t.Fatalf("carry = %v, want %v", got, tc.wantCarry)
			}
		})
	}
}
