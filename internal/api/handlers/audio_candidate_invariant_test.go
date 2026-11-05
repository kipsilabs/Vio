package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/nodepool"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/transcodenode"
)

// An audio-only refusal must not trigger the alternate-version hunt. The
// genuine video/policy reasons keep theirs; a nil terminal is never audio-only.
func TestTerminalAllowsAlternateFileV3AudioReasonDoesNotHunt(t *testing.T) {
	if terminalAllowsAlternateFileV3(&playback.TerminalV3{Reason: playback.TerminalAudioConversionUnsupportedV3}) {
		t.Fatal("an audio-only refusal must not trigger an alternate-version retry; it resolves in place")
	}
	if !audioOnlyTerminalV3(&playback.TerminalV3{Reason: playback.TerminalAudioConversionUnsupportedV3}) {
		t.Fatal("audio_conversion_unsupported was not recognized as audio-only")
	}
	for _, reason := range []string{terminalNoAlternateVersionV3, terminalHDRTranscodeUnsupportedV3, sourceDecodeFailedReasonV3} {
		if audioOnlyTerminalV3(&playback.TerminalV3{Reason: reason}) {
			t.Fatalf("video/policy terminal %q must keep its alternate-file failover", reason)
		}
		if !terminalAllowsAlternateFileV3(&playback.TerminalV3{Reason: reason}) {
			t.Fatalf("video/policy terminal %q must still allow an alternate-version retry", reason)
		}
	}
	if audioOnlyTerminalV3(nil) {
		t.Fatal("a nil terminal must not be treated as audio-only")
	}
}

// A transport failure that names an audio condition is audio-local; a video or
// route transport reason is not.
func TestAudioOnlyTransportReasonsAreAudioLocal(t *testing.T) {
	for _, reason := range []string{audioAdaptationFailedReasonV3, "audio_transcoding_disabled"} {
		if !audioOnlyTransportReasonV3(reason) {
			t.Fatalf("transport reason %q was not recognized as audio-local", reason)
		}
	}
	for _, reason := range []string{transcodeStartFailedReasonV3, candidateSourceDecodeRejectedReasonV3, "route_preparation_failed"} {
		if audioOnlyTransportReasonV3(reason) {
			t.Fatalf("video/route transport reason %q was misclassified as audio-local", reason)
		}
	}
}

// An audio remap miss on every candidate is an audio-local outcome. It must not
// be classified as a failed media source, which is what would feed the
// candidate-failure machinery and indict the release.
func TestClassifyVirtualReplanExhaustionAudioRemapIsNotReleaseFailure(t *testing.T) {
	errs := []*candidateV3Error{{
		Stage:   candidateStageAudioRemap,
		Message: "candidate 84 audio remap failed",
		Err:     errors.New("selected audio track identity is invalid"),
	}}
	got := classifyVirtualReplanExhaustionV3(nil, errs)
	if got == nil {
		t.Fatal("audio remap exhaustion returned no error")
	}
	if got.reason != "track_unavailable" {
		t.Fatalf("audio remap exhaustion reason = %q, want track_unavailable", got.reason)
	}
	if got.reason == virtualSourceUnavailableReasonV3 {
		t.Fatal("an audio remap miss was classified as a virtual source failure")
	}
}

// An explicit audio pick must never be silently replaced by the in-place audio
// resolution: a viewer's track choice surfaces its failure instead.
func TestDegradeStartAudioInPlaceLeavesExplicitPickAlone(t *testing.T) {
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	index := 0
	req := playback.StartRequestV3{AudioTrackIndex: &index}
	_, _, _, _, ok := handler.degradeStartAudioInPlaceV3(
		httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil),
		req, &models.MediaFile{ID: 1}, &models.MediaFile{ID: 1, AudioTracks: []models.AudioTrack{{Codec: "dts"}, {Codec: "aac"}}}, 0, playback.PlannerSettingsV3{}, ProbeProvenance(""),
	)
	if ok {
		t.Fatal("an explicit audio pick was silently substituted")
	}
}

// An audio codec/track failure keeps the release: the selected track cannot be
// adapted, but another track in the same file can, so the in-place resolution
// switches to it instead of moving to the sibling version.
func TestHandleStartPlaybackV3AudioConversionUnsupportedKeepsRelease(t *testing.T) {
	source := v3HandlerFixtureFile(t)
	// The server's preferred-language track cannot be adapted to this client
	// and the file's other track can. The omitted audio selection resolves to
	// track 0, so the initial plan terminalls audio_conversion_unsupported.
	source.AudioTracks = []models.AudioTrack{
		{Index: 0, Codec: "dts", Channels: 6, Layout: "5.1", Language: "eng", Default: true},
		{Index: 1, Codec: "aac", Channels: 2, Layout: "stereo", Language: "spa"},
	}
	source.CodecAudio = "dts"

	alternateValue := *source
	alternate := &alternateValue
	alternate.ID = 84
	alternate.FilePath = writePlaybackTestMediaFile(t, "movie-other-version.mp4")

	files := map[int]*models.MediaFile{source.ID: source, alternate.ID: alternate}
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0), mapPlaybackFileResolver{files: files})
	handler.FileVersionFetcher = testPlaybackFileVersionFetcher{byContent: map[string][]*models.MediaFile{
		source.ContentID: {source, alternate},
	}}
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{}}
	handler.PlaybackConfig = playbackTestConfig("", t.TempDir())
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	// A registry with video conversion but no audio_to_aac: the dts-only plan
	// terminalls audio_conversion_unsupported while the aac track still plays.
	presetLocalRegistryV3(handler, playback.NewTransformationRegistryV3([]playback.TransformationSpecV3{
		{Name: "video_to_h264", RecipeVersion: "2", Available: true},
	}))

	start := v3HandlerStartRequest()
	start.QualityPreference = "auto"
	start.ClientPlaybackContext.Deliveries[playback.DeliveryClassProgressiveV3] = playback.DeliveryCapabilityV3{Enabled: true, SupportedOnDevice: true}
	start.ClientPlaybackContext.Deliveries[playback.DeliveryClassHLSV3] = playback.DeliveryCapabilityV3{Enabled: true, SupportedOnDevice: true}

	rr := httptest.NewRecorder()
	handler.HandleStartPlayback(rr, httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", strings.NewReader(marshalV3StartRequest(t, start))).WithContext(newAuthorizedPlaybackContext()))

	var response playback.DecisionResponseV3
	if rr.Code != http.StatusCreated || json.Unmarshal(rr.Body.Bytes(), &response) != nil || response.PlaybackPlan == nil {
		t.Fatalf("start status=%d body=%s", rr.Code, rr.Body.String())
	}
	if response.PlaybackPlan.EffectiveMediaFileID != source.ID {
		t.Fatalf("effective file = %d, want the mounted source %d (an audio reason must not move the release)",
			response.PlaybackPlan.EffectiveMediaFileID, source.ID)
	}
	if response.PlaybackPlan.SelectedTracks.Audio == nil || response.PlaybackPlan.SelectedTracks.Audio.Index == nil ||
		*response.PlaybackPlan.SelectedTracks.Audio.Index != 1 {
		t.Fatalf("in-place audio selection = %#v, want track 1 in the same file", response.PlaybackPlan.SelectedTracks.Audio)
	}
	substituted := false
	for _, warning := range response.PlaybackPlan.DegradationWarnings {
		if warning.Code == "audio_track_substituted" {
			substituted = true
		}
	}
	if !substituted {
		t.Fatalf("in-place audio resolution did not state the substitution: %#v", response.PlaybackPlan.DegradationWarnings)
	}
}

// A node that cannot confirm the audio adaptation recipe fails transport
// preparation with the audio-local reason, not the generic video transport
// reason, so the start/replan hunts cannot rotate the release over it.
func TestPrepareRemoteTransportV3AudioRecipeFailureIsAudioLocal(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/transcode/start" {
			var request transcodenode.TranscodeStartRequest
			_ = json.NewDecoder(r.Body).Decode(&request)
			// Attest a different audio recipe version than the request asked for.
			writeJSON(w, http.StatusAccepted, transcodenode.TranscodeStartResponse{
				SessionID: request.SessionID, Status: "started", AudioRecipeVersion: "unexpected-version",
			})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer node.Close()

	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	handler.JWTSecret = "test-secret"
	result := remoteHLSResultV3()
	transport, transportErr := handler.prepareRemoteTransportV3(
		httptest.NewRequest(http.MethodPost, "/", nil),
		&playback.Session{ID: "session-audio-attest", UserID: 7, ProfileID: "profile-1"},
		v3HandlerFixtureFile(t), result,
		nodepool.Plan{TranscodeNode: &nodepool.Node{URL: node.URL}},
		preparedTimelineV3{}, mediaAuthModeV3{},
	)
	if transportErr == nil {
		transport.rollback()
		t.Fatal("a mismatched audio recipe attestation must fail transport preparation")
	}
	if transportErr.reason != audioAdaptationFailedReasonV3 {
		t.Fatalf("audio recipe failure reason = %q, want the audio-local %q", transportErr.reason, audioAdaptationFailedReasonV3)
	}
	if !audioOnlyTransportReasonV3(transportErr.reason) {
		t.Fatal("the audio recipe failure was not recognized as audio-local")
	}
}

// An audio remap failure on a replan candidate is surfaced as audio-local, not
// as a virtual source failure, so the candidate machinery cannot indict the
// release for a track the file lacks.
func TestEvaluateAudioDegradeInPlaceRequiresAFileAndNoExplicitPick(t *testing.T) {
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	req := playback.ReplanRequestV3{SelectedTracks: playback.SelectedTracksV3{Audio: &playback.TrackIdentityV3{ID: "file:1:audio:0"}}}
	_, err, ok := handler.evaluateAudioDegradeInPlaceV3(
		httptest.NewRequest(http.MethodPost, "/api/v1/playback/replan", nil),
		nil, &playback.AttemptRecordV3{}, req, playback.StartRequestV3{},
		&models.MediaFile{ID: 1, AudioTracks: []models.AudioTrack{{Codec: "dts"}, {Codec: "aac"}}},
		ProbeProvenance(""),
		&models.MediaFile{ID: 1}, playback.PlannerSettingsV3{}, nil, nil,
	)
	if ok {
		t.Fatal("an explicit replan audio pick was silently substituted")
	}
	if err == nil || err.Stage != candidateStageAudioRemap {
		t.Fatalf("explicit-pick refusal = %#v, want an audio-local candidate error", err)
	}
}
