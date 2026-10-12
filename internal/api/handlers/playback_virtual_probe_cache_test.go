package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

// TestResolveVirtualPlaybackSourceServesFreshProbeCacheEntry pins variant (c) of
// the audio-switch latency fix: an audio track change respawns the muxed HLS
// generation (the audio is interleaved into the same segments, so the transport
// cannot be reused), and the respawn's rehydration resolve would otherwise pay a
// full synchronous ffprobe for bytes that have not changed. When the probe cache
// already holds a completed probe for the candidate's canonical identity, the
// synchronous resolve must serve it and never invoke the prober.
func TestResolveVirtualPlaybackSourceServesFreshProbeCacheEntry(t *testing.T) {
	const candidateURI = "virtual://movie/tt-probe-cache?result=cand-a"

	file := &models.MediaFile{
		ID:                         501,
		ContentID:                  "movie-probe-cache",
		FilePath:                   candidateURI,
		VirtualOwnerInstallationID: 5,
		Container:                  "virtual",
	}

	detailedCalls := 0
	proberCalls := 0
	cacheLookups := 0
	h := &PlaybackHandler{
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			detailedCalls++
			return ResolvedVirtualMedia{
				URL: "https://93.184.216.34/stream/token=fresh", URI: candidateURI, CandidateID: "cand-a",
			}, nil
		}),
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			return "https://93.184.216.34/stream/token=legacy", nil
		}),
		VirtualPlaybackSourceProber: func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
			proberCalls++
			f.VideoTracks = []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080}}
			f.AudioTracks = []models.AudioTrack{{Codec: "aac", Channels: 2}}
			f.Container = "mkv"
			f.Resolution = "1080p"
			f.CodecVideo = "h264"
			f.CodecAudio = "aac"
			return f, nil
		},
		VirtualProbeCacheLookup: func(_ string, probeFile *models.MediaFile) *models.MediaFile {
			cacheLookups++
			return &models.MediaFile{
				FilePath:                   probeFile.FilePath,
				VirtualOwnerInstallationID: probeFile.VirtualOwnerInstallationID,
				VideoTracks:                []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080}},
				AudioTracks:                []models.AudioTrack{{Codec: "aac", Channels: 2}},
				Resolution:                 "1080p",
				CodecVideo:                 "h264",
				CodecAudio:                 "aac",
				Container:                  "mkv",
				Duration:                   5400,
			}
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/replan", nil)
	// A session-bound rehydration resolve (the audio-switch respawn's evidence
	// refresh) runs with deferProbe=false: it needs the probed inventory to
	// remap the new audio ordinal, so it is exactly the synchronous probe the
	// cache must short-circuit.
	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "", 0, false,
		virtualResolveOptionsV3{sessionBound: true, sessionAnchorURI: candidateURI})
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if resolved.Provenance != ProbeProvenanceVerified {
		t.Fatalf("provenance = %q, want verified (the cache hit is completed probe evidence)", resolved.Provenance)
	}
	if resolved.File == nil || len(resolved.File.AudioTracks) == 0 || resolved.File.AudioTracks[0].Codec != "aac" {
		t.Fatalf("resolved file = %#v, want the cached probe inventory", resolved.File)
	}
	if proberCalls != 0 {
		t.Fatalf("synchronous prober calls = %d, want 0: a fresh probe-cache entry must skip the ffprobe", proberCalls)
	}
	if cacheLookups == 0 {
		t.Fatal("probe cache was never consulted before the synchronous probe")
	}
	if detailedCalls == 0 {
		t.Fatal("the provider resolve did not run: the cache short-circuits the probe, not the resolve")
	}
}

// TestResolveVirtualPlaybackCacheLookupForwardsCandidateAudio pins the
// cache-only half of the completeness gate. The synchronous probe input is
// built from the catalog row, which can carry no audio metadata even when the
// served candidate declares audio through its language list. The probe cache is
// keyed on the expected-audio signal, so a lookup that ran before the
// candidate's declaration was forwarded would take the video-only key,
// retrieve a thin video-without-audio enumeration cached under that
// expectation, merge synthesized candidate audio onto it, and report verified
// while the gate never saw the item's audio. The shared cache helper must
// forward the candidate's declared audio before consulting the cache; a
// no-audio lookup here means the fix regressed.
func TestResolveVirtualPlaybackCacheLookupForwardsCandidateAudio(t *testing.T) {
	stored := provenanceResolveRow()
	probeKey := virtualProbeFailureKey(stored.FilePath, stored.VirtualOwnerInstallationID)
	virtualProbeFailures.clear(probeKey)
	virtualProbeFailures.clearPartial(probeKey)
	t.Cleanup(func() {
		virtualProbeFailures.clear(probeKey)
		virtualProbeFailures.clearPartial(probeKey)
	})

	sawAudioExpectation := false
	cacheHits := 0
	proberCalls := 0
	h := virtualProvenanceResolution(stored, func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		proberCalls++
		f.VideoTracks = []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080}}
		f.AudioTracks = []models.AudioTrack{{Codec: "aac", Channels: 2, Language: "fre"}}
		f.SubtitleTracks = []models.SubtitleTrack{{Index: 2, Codec: "ass", Language: "fre"}}
		f.CodecVideo, f.CodecAudio, f.Resolution, f.Container = "h264", "aac", "1080p", "mkv"
		return f, nil
	})
	// The candidate declares audio only through its language list; the stored
	// row carries none.
	h.VirtualPlaybackStreamLister = VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
		return []VirtualPlaybackStream{{
			ID: "cand-1", URI: stored.FilePath,
			Resolution: "1080p", CodecVideo: "h264", Container: "mkv",
			AudioLanguages: []string{"FRE"},
		}}, nil
	})
	h.VirtualProbeCacheLookup = func(_ string, probeFile *models.MediaFile) *models.MediaFile {
		sawAudioExpectation = strings.TrimSpace(probeFile.CodecAudio) != "" || len(probeFile.AudioTracks) > 0
		if sawAudioExpectation {
			// A thin video-only enumeration is only ever cached under the
			// video-only expectation, so an audio-expecting lookup misses.
			return nil
		}
		cacheHits++
		return &models.MediaFile{
			FilePath: probeFile.FilePath,
			VideoTracks: []models.VideoTrack{
				{Codec: "h264", Width: 3840, Height: 2160},
			},
			SubtitleTracks: []models.SubtitleTrack{{Index: 2, Codec: "ass", Language: "fre"}},
			Resolution:     "2160p",
			CodecVideo:     "h264",
			Container:      "mkv",
		}
	}

	resolved, err := h.resolveVirtualPlaybackSource(provenanceResolveRequest(), stored, "profile-1", false, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if !sawAudioExpectation {
		t.Fatal("the cache lookup ran before the candidate's declared audio was forwarded")
	}
	if cacheHits != 0 {
		t.Fatalf("the video-only cache entry was served %d time(s); the lookup must take the audio expectation's key", cacheHits)
	}
	if proberCalls != 1 {
		t.Fatalf("synchronous prober calls = %d, want 1: the audio-expecting lookup must miss and fall through", proberCalls)
	}
	if resolved.Provenance != ProbeProvenanceVerified {
		t.Fatalf("provenance = %q, want verified from the real audio-bearing probe", resolved.Provenance)
	}
	if resolved.File == nil || len(resolved.File.AudioTracks) == 0 {
		t.Fatalf("resolved file = %#v, want the real probe's audio inventory", resolved.File)
	}
}

// TestResolveVirtualPlaybackSourceProbesOnCacheMiss keeps the cache-first path
// honest: with no cached entry the synchronous prober still runs, so a stale or
// absent cache can never fabricate evidence.
func TestResolveVirtualPlaybackSourceProbesOnCacheMiss(t *testing.T) {
	const candidateURI = "virtual://movie/tt-probe-miss?result=cand-a"

	file := &models.MediaFile{
		ID:                         502,
		ContentID:                  "movie-probe-miss",
		FilePath:                   candidateURI,
		VirtualOwnerInstallationID: 5,
		Container:                  "virtual",
	}

	proberCalls := 0
	h := &PlaybackHandler{
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			return ResolvedVirtualMedia{URL: "https://93.184.216.34/stream/token=fresh", URI: candidateURI, CandidateID: "cand-a"}, nil
		}),
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			return "https://93.184.216.34/stream/token=legacy", nil
		}),
		VirtualPlaybackSourceProber: func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
			proberCalls++
			f.VideoTracks = []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080}}
			f.AudioTracks = []models.AudioTrack{{Codec: "aac", Channels: 2}}
			f.Container = "mkv"
			f.Resolution = "1080p"
			f.CodecVideo = "h264"
			f.CodecAudio = "aac"
			return f, nil
		},
		VirtualProbeCacheLookup: func(_ string, _ *models.MediaFile) *models.MediaFile { return nil },
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/replan", nil)
	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "", 0, false,
		virtualResolveOptionsV3{sessionBound: true, sessionAnchorURI: candidateURI})
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if resolved.Provenance != ProbeProvenanceVerified {
		t.Fatalf("provenance = %q, want verified", resolved.Provenance)
	}
	if proberCalls != 1 {
		t.Fatalf("synchronous prober calls = %d, want 1 on a cache miss", proberCalls)
	}
}
