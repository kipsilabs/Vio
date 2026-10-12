package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

type fallbackEvidenceSaver struct {
	mu   sync.Mutex
	args []models.VirtualFilePersistArgs
}

func (s *fallbackEvidenceSaver) save(_ context.Context, args models.VirtualFilePersistArgs) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.args = append(s.args, args)
	return 1, nil
}

func (s *fallbackEvidenceSaver) recorded() []models.VirtualFilePersistArgs {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]models.VirtualFilePersistArgs(nil), s.args...)
}

func fallbackProbedFile() *models.MediaFile {
	return &models.MediaFile{
		Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
		VideoTracks: []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080, FrameRate: "24", BitDepth: 8, Bitrate: 10_000}},
		AudioTracks: []models.AudioTrack{{Codec: "aac", Channels: 2}},
	}
}

// TestPersistVirtualProbeEvidenceForegroundFallsBackWhenBufferRejected proves a
// saturated evidence pool no longer drops the foreground request's own
// candidate: when admission is rejected, one bounded direct write persists the
// evidence so the row cannot stay permanently unprobed.
func TestPersistVirtualProbeEvidenceForegroundFallsBackWhenBufferRejected(t *testing.T) {
	logs := captureHandlerLogs(t)
	buf := newVirtualEvidenceBuffer(1)
	buf.close() // reject every admission, as a saturated/shut-down pool would
	saver := &fallbackEvidenceSaver{}
	h := &PlaybackHandler{VirtualFileSaver: saver.save}
	h.virtualEvidenceBuffer = buf
	t.Cleanup(h.stopVirtualEvidence)

	file := &models.MediaFile{ID: 501, ContentID: "movie-fallback", FilePath: "virtual://movie/tt-fallback", ProbeSource: "virtual"}
	h.persistVirtualProbeEvidence(context.Background(), file, "virtual://movie/tt-fallback?result=cand", fallbackProbedFile(), noCandidateIdentity, true, true)

	calls := saver.recorded()
	if len(calls) != 1 {
		t.Fatalf("direct fallback writes = %d, want exactly 1", len(calls))
	}
	if calls[0].FileID != 501 || !calls[0].StampProbe {
		t.Fatalf("fallback args = %#v, want file 501 with the probe stamp", calls[0])
	}
	if !strings.Contains(logs.String(), "direct fallback after buffer rejection") {
		t.Fatalf("buffer-rejection fallback was not logged:\n%s", logs.String())
	}
}

// TestPersistVirtualProbeEvidenceBackgroundNeverFallsBack proves the fallback
// is scoped to the foreground request's own candidate: a background or
// speculative write that the pool rejects is not written directly, so the
// fallback cannot amplify load under a burst.
func TestPersistVirtualProbeEvidenceBackgroundNeverFallsBack(t *testing.T) {
	buf := newVirtualEvidenceBuffer(1)
	buf.close()
	saver := &fallbackEvidenceSaver{}
	h := &PlaybackHandler{VirtualFileSaver: saver.save}
	h.virtualEvidenceBuffer = buf
	t.Cleanup(h.stopVirtualEvidence)

	file := &models.MediaFile{ID: 502, ContentID: "movie-bg-fallback", FilePath: "virtual://movie/tt-bg", ProbeSource: "virtual"}
	h.persistVirtualProbeEvidence(context.Background(), file, "virtual://movie/tt-bg?result=cand", fallbackProbedFile(), noCandidateIdentity, true, false)

	if calls := saver.recorded(); len(calls) != 0 {
		t.Fatalf("background candidate triggered %d direct fallback write(s), want 0", len(calls))
	}
}

// TestResolveVirtualPlaybackSourceForegroundFallbackOnDetachedGateExhaustion
// proves an exhausted detached-probe gate leaves the foreground response
// unaffected: the declared-evidence instant path returns the playable URL
// immediately and the gate contention never blocks the start. The probe is
// retried on a later play when a gate slot frees.
func TestResolveVirtualPlaybackSourceForegroundFallbackOnDetachedGateExhaustion(t *testing.T) {
	_ = captureHandlerLogs(t)
	saver := &fallbackEvidenceSaver{}
	const candidateURI = "virtual://movie/tt-gate-fallback?result=cand-1"

	h := &PlaybackHandler{
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
			return "http://provider.example/legacy", nil
		}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			return ResolvedVirtualMedia{URL: "http://provider.example/stream.mkv", URI: uri, CandidateID: "cand-1"}, nil
		}),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{{ID: "cand-1", URI: candidateURI, Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac", Container: "mkv"}}, nil
		}),
		VirtualPlaybackSourceProber: func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
			f.VideoTracks = fallbackProbedFile().VideoTracks
			f.AudioTracks = fallbackProbedFile().AudioTracks
			f.CodecVideo, f.CodecAudio, f.Resolution, f.Container = "h264", "aac", "1080p", "mkv"
			return f, nil
		},
		VirtualFileSaver: saver.save,
	}
	// Exhaust the detached gate so the start cannot background the probe.
	h.detachedWorkGate = newVirtualDetachedGate(1)
	if !h.detachedWorkGate.tryAcquire() {
		t.Fatal("precondition: the detached gate had no slot to exhaust")
	}

	file := &models.MediaFile{ID: 503, ContentID: "movie-gate-fallback", FilePath: candidateURI, Container: "virtual", VirtualOwnerInstallationID: 5}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if resolved.File == nil {
		t.Fatalf("resolved source = %#v, want a file", resolved)
	}
	if resolved.URL == "" {
		t.Fatal("instant declared resolve must still return a playable URL under gate exhaustion")
	}
	if resolved.Provenance != ProbeProvenanceDeclared && resolved.Provenance != ProbeProvenancePending {
		t.Fatalf("provenance = %q, want declared/pending instant response under gate exhaustion", resolved.Provenance)
	}
	if calls := saver.recorded(); len(calls) != 0 {
		t.Fatalf("gate-exhaustion triggered %d synchronous write(s), want 0 (probe is shed, never blocking)", len(calls))
	}
}
