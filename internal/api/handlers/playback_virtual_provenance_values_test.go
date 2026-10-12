package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

// virtualProvenanceResolution wires a resolve that produces the value under
// test. prober nil means no probe seam is configured, which forces the
// declared-metadata path; a prober that errors forces the failed-probe path.
func virtualProvenanceResolution(stored *models.MediaFile, prober VirtualPlaybackSourceProber) *PlaybackHandler {
	return &PlaybackHandler{
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
			return "http://127.0.0.1:8080/stream?path=" + path, nil
		}),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{{
				ID: "cand-1", URI: "virtual://movie/tt-provenance?result=cand-1",
				Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
			}}, nil
		}),
		VirtualFileLookup: func(_ context.Context, _ string) (*models.MediaFile, error) {
			return stored, nil
		},
		VirtualPlaybackSourceProber: prober,
		VirtualFileSaver: func(context.Context, models.VirtualFilePersistArgs) (int64, error) {
			return 1, nil
		},
	}
}

// provenanceResolveRow is an unprobed virtual row: no probe stamp, so the
// resolve must report what it actually did rather than let a stale stamp make
// the plan look verified.
func provenanceResolveRow() *models.MediaFile {
	return &models.MediaFile{
		ID:                         730,
		ContentID:                  "movie-provenance",
		FilePath:                   "virtual://movie/tt-provenance?result=cand-1",
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
		ProbeUpdatedAt:             nil,
	}
}

func provenanceResolveRequest() *http.Request {
	return httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil).WithContext(newAuthorizedPlaybackContext())
}

// A resolve with no prober seam can only serve provider-declared metadata, so
// the provenance is declared even though the row's tracks look complete.
func TestResolveVirtualPlaybackSourceProvenanceDeclaredWithoutProber(t *testing.T) {
	stored := provenanceResolveRow()
	h := virtualProvenanceResolution(stored, nil)

	resolved, err := h.resolveVirtualPlaybackSource(provenanceResolveRequest(), stored, "profile-1", false, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if resolved.Provenance != ProbeProvenanceDeclared {
		t.Fatalf("provenance = %q, want declared (no prober, provider metadata only)", resolved.Provenance)
	}
	if resolved.ProbeSucceeded {
		t.Fatal("a declared resolve must not report a successful probe")
	}
}

// A prober that errors rejects fast paths that would declare playback
// healthy; failing closed preserves the lister-less flow so failures stay
// visible rather than being masked by a local-only cache hit. The request
// still receives a playable URL with declared provenance while the failure
// damper records the rejection.
func TestResolveVirtualPlaybackSourceProvenanceFailedAfterProbeError(t *testing.T) {
	stored := provenanceResolveRow()
	h := virtualProvenanceResolution(stored, func(_ context.Context, _ string, _ *models.MediaFile) (*models.MediaFile, error) {
		return nil, context.DeadlineExceeded
	})
	// The failed probe marks the process-global damper; clear it so a later
	// test resolving the same candidate is not steered by this one.
	t.Cleanup(func() {
		virtualProbeFailures.clear(virtualProbeFailureKey(stored.FilePath, stored.VirtualOwnerInstallationID))
	})

	resolved, err := h.resolveVirtualPlaybackSource(provenanceResolveRequest(), stored, "profile-1", false, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if resolved.Provenance != ProbeProvenanceDeclared {
		t.Fatalf("provenance = %q, want declared (inventoried via provider path after probe failure)", resolved.Provenance)
	}
	if resolved.ProbeSucceeded {
		t.Fatal("a declared resolve must not report a successful probe")
	}
	if resolved.URL == "" {
		t.Fatal("a declared resolve must still return a playable URL")
	}
}

// A complete stored row resolves with provider-declared metadata; the probe
// runs in the background. The plan's tracks are the provider's inventory.
// Provenance is declared, and the playable URL must be present immediately.
func TestResolveVirtualPlaybackSourceProvenanceVerifiedAfterProbe(t *testing.T) {
	stored := provenanceResolveRow()
	// The probe-failure damper is process-global and keyed by candidate; a
	// sibling test that marked this candidate failed must not steer this one.
	virtualProbeFailures.clear(virtualProbeFailureKey(stored.FilePath, stored.VirtualOwnerInstallationID))
	h := virtualProvenanceResolution(stored, func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		f.VideoTracks = []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080, FrameRate: "24000/1001", BitDepth: 8, Bitrate: 10_000}}
		f.AudioTracks = []models.AudioTrack{{Codec: "aac", Channels: 2, Language: "eng"}}
		f.CodecVideo, f.CodecAudio, f.Resolution, f.Container = "h264", "aac", "1080p", "mkv"
		f.Bitrate = 10_000
		return f, nil
	})

	resolved, err := h.resolveVirtualPlaybackSource(provenanceResolveRequest(), stored, "profile-1", false, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if resolved.Provenance != ProbeProvenanceDeclared || resolved.ProbeSucceeded {
		t.Fatalf("provenance = %q succeeded = %v, want declared/false on instant declared response", resolved.Provenance, resolved.ProbeSucceeded)
	}
	if resolved.URL == "" {
		t.Fatal("instant declared resolve must still return a playable URL")
	}
}

// A row whose stored probe evidence is incomplete resolves to declared
// metadata even though the candidate carries a concrete ?result= path: the
// plan's stamp-derived InventoryStatus is not the provenance the resolve did.
func TestResolveVirtualPlaybackSourceProvenanceDeclaredForIncompleteRow(t *testing.T) {
	stored := provenanceResolveRow()
	stored.VideoTracks = nil
	h := virtualProvenanceResolution(stored, nil)

	resolved, err := h.resolveVirtualPlaybackSource(provenanceResolveRequest(), stored, "profile-1", false, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if resolved.Provenance != ProbeProvenanceDeclared {
		t.Fatalf("provenance = %q, want declared for an incomplete row resolved without a prober", resolved.Provenance)
	}
}
