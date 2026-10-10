package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

// trackDiscoveryHandler wires the minimum seams to drive the v2 negotiated
// deferred start (deferProbePastCommit) with a fake fast track-discovery. The
// full prober is non-nil so the resolve still builds its deferred verifier, but
// it must never run during the resolve when discovery is enabled.
func trackDiscoveryHandler(stored *models.MediaFile, lister VirtualPlaybackStreamLister, enumerator VirtualTrackEnumerator) *PlaybackHandler {
	return &PlaybackHandler{
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
			return "http://127.0.0.1:8080/stream?path=" + path, nil
		}),
		VirtualPlaybackStreamLister: lister,
		VirtualFileLookup: func(_ context.Context, _ string) (*models.MediaFile, error) {
			return stored, nil
		},
		VirtualPlaybackSourceProber: func(context.Context, string, *models.MediaFile) (*models.MediaFile, error) {
			return nil, errors.New("full probe must not run during the resolve")
		},
		VirtualTrackEnumerator: enumerator,
	}
}

// trackDiscoveryLister returns one candidate whose declared metadata collapses
// to a single audio language, so a resolve that only keeps the declaration
// reports one audio track and no subtitle tracks.
func trackDiscoveryLister(uri string) VirtualPlaybackStreamListerFunc {
	return func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
		return []VirtualPlaybackStream{{
			ID:             "cand-1",
			URI:            uri,
			Resolution:     "1080p",
			CodecVideo:     "h264",
			CodecAudio:     "aac",
			Container:      "mkv",
			AudioLanguages: []string{"eng"},
		}}, nil
	}
}

func trackDiscoveryFile(uri string) *models.MediaFile {
	return &models.MediaFile{
		ID:                         300,
		ContentID:                  "movie-multi",
		FilePath:                   uri,
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
	}
}

// The v2 deferred start must enumerate the real audio and subtitle streams
// before the plan is built, not just carry the provider's declared single-track
// label. A multi-track release therefore reaches the plan with every stream,
// while the deferred full probe is still scheduled as the verifier.
func TestDeferredStartDiscoversAllAudioAndSubtitleTracks(t *testing.T) {
	uri := "virtual://movie/tt-multi?result=cand-1"
	file := trackDiscoveryFile(uri)
	stored := trackDiscoveryFile(uri)

	calls := 0
	var gotURL string
	h := trackDiscoveryHandler(stored, trackDiscoveryLister(uri),
		func(_ context.Context, sourceURL string, _ map[string]string) ([]models.AudioTrack, []models.SubtitleTrack, error) {
			calls++
			gotURL = sourceURL
			return []models.AudioTrack{
					{Codec: "eac3", Channels: 6, Language: "eng", Default: true},
					{Codec: "aac", Channels: 2, Language: "jpn"},
					{Codec: "ac3", Channels: 2, Language: "deu"},
				}, []models.SubtitleTrack{
					{Codec: "subrip", Language: "eng"},
					{Codec: "hdmv_pgs_subtitle", Language: "fra"},
				}, nil
		})

	req := httptest.NewRequest(http.MethodPost, "/api/v2/playback/start", nil)
	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false,
		virtualResolveOptionsV3{deferProbePastCommit: true})
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("track enumerator calls = %d, want 1", calls)
	}
	if !strings.Contains(gotURL, uri) {
		t.Fatalf("enumerator source URL = %q, want the resolved candidate %q", gotURL, uri)
	}
	if resolved.Provenance != ProbeProvenancePending || resolved.ProbeSucceeded {
		t.Fatalf("provenance=%q succeeded=%v, want pending/false", resolved.Provenance, resolved.ProbeSucceeded)
	}
	if len(resolved.File.AudioTracks) != 3 {
		t.Fatalf("plan audio tracks = %#v, want all 3 discovered streams", resolved.File.AudioTracks)
	}
	if resolved.File.AudioTracks[1].Language != "jpn" || resolved.File.AudioTracks[2].Language != "deu" {
		t.Fatalf("plan audio tracks = %#v, want the jpn and deu streams from discovery", resolved.File.AudioTracks)
	}
	if len(resolved.File.SubtitleTracks) != 2 {
		t.Fatalf("plan subtitle tracks = %#v, want both discovered subtitle streams", resolved.File.SubtitleTracks)
	}
	if resolved.DeferredProbe == nil {
		t.Fatal("the deferred full probe must still be scheduled as the verifier")
	}
}

// A video file whose real stream table carries no audio and no subtitle streams
// must advertise an empty inventory, not the declared label.
func TestDeferredStartAdvertisesEmptyInventoryWhenProbeFindsNone(t *testing.T) {
	uri := "virtual://movie/tt-silent?result=cand-1"
	file := trackDiscoveryFile(uri)
	stored := trackDiscoveryFile(uri)

	h := trackDiscoveryHandler(stored, trackDiscoveryLister(uri),
		func(_ context.Context, _ string, _ map[string]string) ([]models.AudioTrack, []models.SubtitleTrack, error) {
			return nil, nil, nil
		})

	req := httptest.NewRequest(http.MethodPost, "/api/v2/playback/start", nil)
	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false,
		virtualResolveOptionsV3{deferProbePastCommit: true})
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if len(resolved.File.AudioTracks) != 0 {
		t.Fatalf("plan audio tracks = %#v, want empty", resolved.File.AudioTracks)
	}
	if len(resolved.File.SubtitleTracks) != 0 {
		t.Fatalf("plan subtitle tracks = %#v, want empty", resolved.File.SubtitleTracks)
	}
}

// When discovery exceeds its budget the start must not fail: the plan keeps the
// declared inventory exactly as before and the deferred probe still runs.
func TestDeferredStartFallsBackToDeclaredInventoryOnDiscoveryTimeout(t *testing.T) {
	uri := "virtual://movie/tt-slow?result=cand-1"
	file := trackDiscoveryFile(uri)
	stored := trackDiscoveryFile(uri)

	calls := 0
	h := trackDiscoveryHandler(stored, trackDiscoveryLister(uri),
		func(ctx context.Context, _ string, _ map[string]string) ([]models.AudioTrack, []models.SubtitleTrack, error) {
			calls++
			<-ctx.Done()
			return nil, nil, ctx.Err()
		})
	h.trackDiscoveryBudget = 40 * time.Millisecond

	req := httptest.NewRequest(http.MethodPost, "/api/v2/playback/start", nil)
	start := time.Now()
	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false,
		virtualResolveOptionsV3{deferProbePastCommit: true})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("track enumerator calls = %d, want 1", calls)
	}
	if elapsed < 30*time.Millisecond {
		t.Fatalf("discovery returned after %v; the budget boundary was not exercised", elapsed)
	}
	if len(resolved.File.AudioTracks) != 1 || resolved.File.AudioTracks[0].Codec != "aac" {
		t.Fatalf("plan audio tracks = %#v, want the declared aac fallback", resolved.File.AudioTracks)
	}
	if resolved.DeferredProbe == nil {
		t.Fatal("the deferred full probe must still be scheduled after a discovery timeout")
	}
}

// A single-track file is unchanged by discovery: one enumerated audio stream
// yields exactly one plan track and no fabricated extra streams.
func TestDeferredStartSingleTrackUnchanged(t *testing.T) {
	uri := "virtual://movie/tt-single?result=cand-1"
	file := trackDiscoveryFile(uri)
	stored := trackDiscoveryFile(uri)

	h := trackDiscoveryHandler(stored, trackDiscoveryLister(uri),
		func(_ context.Context, _ string, _ map[string]string) ([]models.AudioTrack, []models.SubtitleTrack, error) {
			return []models.AudioTrack{{Codec: "aac", Channels: 2, Language: "eng", Default: true}}, nil, nil
		})

	req := httptest.NewRequest(http.MethodPost, "/api/v2/playback/start", nil)
	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false,
		virtualResolveOptionsV3{deferProbePastCommit: true})
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if len(resolved.File.AudioTracks) != 1 || resolved.File.AudioTracks[0].Language != "eng" {
		t.Fatalf("plan audio tracks = %#v, want the single enumerated track", resolved.File.AudioTracks)
	}
	if len(resolved.File.SubtitleTracks) != 0 {
		t.Fatalf("plan subtitle tracks = %#v, want none", resolved.File.SubtitleTracks)
	}
}

// The v1 surface (or a v2 client that did not negotiate deferred_track_inventory)
// must not pay the discovery wait or change its plan: the deferred path is only
// reached with deferProbePastCommit, and discovery is scoped to it.
func TestDeferredStartSkipsDiscoveryWithoutNegotiation(t *testing.T) {
	uri := "virtual://movie/tt-v1?result=cand-1"
	file := trackDiscoveryFile(uri)
	stored := trackDiscoveryFile(uri)

	calls := 0
	h := trackDiscoveryHandler(stored, trackDiscoveryLister(uri),
		func(_ context.Context, _ string, _ map[string]string) ([]models.AudioTrack, []models.SubtitleTrack, error) {
			calls++
			return nil, nil, nil
		})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	if _, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false); err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if calls != 0 {
		t.Fatalf("track enumerator calls = %d, want 0 without negotiation", calls)
	}
}
