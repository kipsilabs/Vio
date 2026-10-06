package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// minimalRecipeDeferredRow is an identity-less virtual row with NO probe
// evidence: the listing must supply the essential video metadata or the
// deferred commit must not happen. It mirrors the production shape whose
// minimal recipe produced a black screen.
func minimalRecipeDeferredRow(id int, contentID, neutralURI string) *models.MediaFile {
	return &models.MediaFile{
		ID:                         id,
		ContentID:                  contentID,
		FilePath:                   neutralURI + "?result=cand-1",
		Container:                  "mp4",
		Resolution:                 "1080p",
		CodecVideo:                 "h264",
		CodecAudio:                 "aac",
		VirtualOwnerInstallationID: 5,
		Duration:                   3600,
		ProbeUpdatedAt:             nil,
	}
}

// minimalRecipeSparseListing declares only resolution and codec: no frame rate,
// bitrate, bit depth, HDR range or container. This is exactly the shape that
// used to commit a metadata-blind recipe.
func minimalRecipeSparseListing(row *models.MediaFile) VirtualPlaybackStream {
	return VirtualPlaybackStream{
		ID: "cand-1", URI: row.FilePath,
		Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac",
	}
}

// minimalRecipeProbedTrack is the real ffprobe evidence the fill/full probe
// supplies: a complete, client-playable 1080p 8-bit track carrying every field
// the sparse listing omitted (bit depth, frame rate, bitrate, range).
func minimalRecipeProbedTrack() models.VideoTrack {
	return models.VideoTrack{
		Codec: "h264", Profile: "high", Level: 41,
		Width: 1920, Height: 1080, BitDepth: 8, FrameRate: "24000/1001",
		Bitrate: 8000, VideoRange: "SDR", VideoRangeType: "SDR",
	}
}

// minimalRecipeFillHandler wires a start-path resolve whose listing declares no
// essential video metadata. prober supplies the probe behavior; the first call
// is the bounded fill (the deferred path pays exactly one).
func minimalRecipeFillHandler(row *models.MediaFile, prober VirtualPlaybackSourceProber) *PlaybackHandler {
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0), testPlaybackFileResolver{file: row})
	handler.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
		return "http://127.0.0.1:8080/stream?path=" + path, nil
	})
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		return ResolvedVirtualMedia{URL: "http://127.0.0.1:8080/stream?uri=" + uri, URI: uri, CandidateID: virtualResultCandidateID(uri)}, nil
	})
	handler.VirtualPlaybackStreamLister = VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
		return []VirtualPlaybackStream{minimalRecipeSparseListing(row)}, nil
	})
	handler.VirtualPlaybackSourceProber = prober
	return handler
}

func minimalRecipeResolveRequest() *http.Request {
	return httptest.NewRequest(http.MethodPost, "/api/v2/playback/start", nil)
}

// TestMinimalRecipeListingWithoutVideoMetadataTriggersBoundedFill is case (a):
// a listing that declares no essential video metadata cannot be committed as
// the deferred minimal recipe. One bounded fill probe supplies the real
// evidence, and the resolved (committed) file carries complete metadata.
func TestMinimalRecipeListingWithoutVideoMetadataTriggersBoundedFill(t *testing.T) {
	row := minimalRecipeDeferredRow(900, "movie-min-fill", "virtual://movie/tt-min-fill")
	var probeCalls atomic.Int32
	handler := minimalRecipeFillHandler(row, func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		probeCalls.Add(1)
		f.VideoTracks = []models.VideoTrack{minimalRecipeProbedTrack()}
		f.CodecVideo = "h264"
		f.Container = "mp4"
		f.Resolution = "1080p"
		f.Bitrate = 8000
		return f, nil
	})

	resolved, err := handler.resolveVirtualPlaybackSource(minimalRecipeResolveRequest(), row, "profile-1", true, nil, "", "auto", 0, false, virtualResolveOptionsV3{deferProbePastCommit: true})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.File == nil {
		t.Fatal("resolve returned no file")
	}
	if resolved.DeferredProbe == nil {
		t.Fatal("the deferred enumeration must still be scheduled after the fill")
	}
	if !virtualTransientHasEssentialVideoMetadata(resolved.File, VirtualPlaybackStream{}) {
		t.Fatalf("committed file is metadata-blind: %#v", resolved.File.VideoTracks)
	}
	if !completeVirtualVideoEvidenceV3(resolved.File) {
		t.Fatalf("committed file does not satisfy the planner's video predicate: %s", playback.VirtualRouteVideoMetadataGapsV3(resolved.File))
	}
	if got := probeCalls.Load(); got != 1 {
		t.Fatalf("probe calls = %d, want exactly 1 (the bounded fill)", got)
	}
}

// TestMinimalRecipeFillFailureFallsBackToSynchronousProbe is case (b): when the
// bounded fill cannot supply real evidence, the start falls back to the
// synchronous full probe and still commits complete metadata rather than a
// metadata-blind recipe.
func TestMinimalRecipeFillFailureFallsBackToSynchronousProbe(t *testing.T) {
	row := minimalRecipeDeferredRow(901, "movie-min-fallback", "virtual://movie/tt-min-fallback")
	var probeCalls atomic.Int32
	handler := minimalRecipeFillHandler(row, func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		n := probeCalls.Add(1)
		if n == 1 {
			// The bounded fill probe fails terminally.
			return nil, errors.New("bounded fill probe refused the stream")
		}
		// The synchronous full probe for this start supplies the real evidence.
		f.VideoTracks = []models.VideoTrack{minimalRecipeProbedTrack()}
		f.CodecVideo = "h264"
		f.Container = "mp4"
		f.Resolution = "1080p"
		f.Bitrate = 8000
		return f, nil
	})

	resolved, err := handler.resolveVirtualPlaybackSource(minimalRecipeResolveRequest(), row, "profile-1", true, nil, "", "auto", 0, false, virtualResolveOptionsV3{deferProbePastCommit: true})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.File == nil {
		t.Fatal("resolve returned no file")
	}
	if resolved.Provenance == ProbeProvenancePending || resolved.DeferredProbe != nil {
		t.Fatalf("a failed fill must not defer: provenance=%q deferred=%v", resolved.Provenance, resolved.DeferredProbe != nil)
	}
	if !virtualTransientHasEssentialVideoMetadata(resolved.File, VirtualPlaybackStream{}) {
		t.Fatalf("fallback committed a metadata-blind file: %#v", resolved.File.VideoTracks)
	}
	if !completeVirtualVideoEvidenceV3(resolved.File) {
		t.Fatalf("fallback does not satisfy the planner's video predicate: %s", playback.VirtualRouteVideoMetadataGapsV3(resolved.File))
	}
	if got := probeCalls.Load(); got < 2 {
		t.Fatalf("probe calls = %d, want >= 2 (bounded fill failure + synchronous full probe)", got)
	}
}

// TestMinimalRecipeAudioOnlyRowSkipsFill proves the guard exempts audio-only
// rows: they have no video route to be metadata-blind about, so the essential
// video predicate reports true. A video row with no probed inventory does not
// pass the gate.
func TestMinimalRecipeAudioOnlyRowSkipsFill(t *testing.T) {
	audioOnly := &models.MediaFile{
		ID: 902, ContentID: "movie-min-audio", FilePath: "virtual://movie/tt-min-audio?result=cand-1",
		Container: "mp3", VirtualOwnerInstallationID: 5,
		BaseType: "audiobook", CodecAudio: "aac", AudioTracks: []models.AudioTrack{{Codec: "aac", Channels: 2}},
	}
	if !audioOnly.IsAudioOnly() {
		t.Fatal("test fixture is not recognized as audio-only")
	}
	if !virtualTransientHasEssentialVideoMetadata(audioOnly, VirtualPlaybackStream{}) {
		t.Fatal("an audio-only row must be exempt from the essential-video gate")
	}
	videoRow := minimalRecipeDeferredRow(903, "movie-min-video", "virtual://movie/tt-min-video")
	if virtualTransientHasEssentialVideoMetadata(videoRow, VirtualPlaybackStream{}) {
		t.Fatal("a video row with no probed tracks must not pass the essential-video gate")
	}
}

// TestReplanDefersPendingIncompleteMetadataVerdict is case (c): a replan that
// would terminal source_metadata_incomplete while the session's deferred probe
// outcome is still pending must defer the verdict, not terminal. The predicate
// is the single guard the replan path consults.
func TestReplanDefersPendingIncompleteMetadataVerdict(t *testing.T) {
	incomplete := &playback.TerminalV3{Reason: sourceMetadataIncompleteReasonV3, Message: "missing", Retryable: true}

	cases := []struct {
		name     string
		terminal *playback.TerminalV3
		session  *playback.Session
		want     bool
	}{
		{"pending defers", incomplete, &playback.Session{VirtualProbeOutcome: probeOutcomePending}, true},
		{"verified passes through", incomplete, &playback.Session{VirtualProbeOutcome: probeOutcomeVerified}, false},
		{"failed passes through", incomplete, &playback.Session{VirtualProbeOutcome: probeOutcomeFailed}, false},
		{"no deferred probe passes through", incomplete, &playback.Session{}, false},
		{"nil session passes through", incomplete, nil, false},
		{"nil terminal passes through", nil, &playback.Session{VirtualProbeOutcome: probeOutcomePending}, false},
		{"other terminal passes through", &playback.TerminalV3{Reason: "adaptation_exhausted"}, &playback.Session{VirtualProbeOutcome: probeOutcomePending}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := replanDefersIncompleteMetadataVerdict(tc.terminal, tc.session); got != tc.want {
				t.Fatalf("defer = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestReplanProbePendingResponseIsRetryableAndDistinct pins the wire shape the
// deferred verdict returns: a retryable reason distinct from
// source_metadata_incomplete, so a client shows "still working" rather than a
// scan failure and may retry once the probe lands.
func TestReplanProbePendingResponseIsRetryableAndDistinct(t *testing.T) {
	if replanProbePendingReasonV3 == sourceMetadataIncompleteReasonV3 {
		t.Fatal("the deferred verdict must not reuse source_metadata_incomplete")
	}
	response := playback.NewTerminalResponseV3(replanProbePendingReasonV3, "still enumerating", true)
	if response.Terminal == nil {
		t.Fatal("no terminal on the deferred response")
	}
	if response.Terminal.Reason != replanProbePendingReasonV3 || !response.Terminal.Retryable {
		t.Fatalf("terminal = %#v, want retryable %q", response.Terminal, replanProbePendingReasonV3)
	}
	encoded, err := json.Marshal(response.Terminal)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !json.Valid(encoded) {
		t.Fatalf("terminal did not marshal to valid JSON: %s", encoded)
	}
}
