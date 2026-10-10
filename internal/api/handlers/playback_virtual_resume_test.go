package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

// virtualResumeCandidate is the concrete ?result= candidate a durable-resume
// row points at.
const virtualResumeCandidate = "virtual://movie/tt-resume?result=cand-resume"

// virtualResumeRow is a virtual candidate row that has never been probed (no
// probe stamp, no track inventory) but carries a persisted provider URL and its
// expiry. That is the state Phase 1 leaves behind, and the state a fresh
// process must resume from without any in-memory cache or pin.
func virtualResumeRow(resolvedURL string, expiresAt *time.Time) *models.MediaFile {
	return &models.MediaFile{
		ID:                         731,
		ContentID:                  "movie-resume",
		FilePath:                   virtualResumeCandidate,
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
		ResolvedURL:                resolvedURL,
		ResolvedURLExpiresAt:       expiresAt,
	}
}

// withVirtualResumeVideoEvidence gives a resume row the planner-grade video
// evidence the stored-URL fast paths now require. A row carrying only a stored
// URL (no tracks) must fall through to resolve+probe instead.
func withVirtualResumeVideoEvidence(file *models.MediaFile) *models.MediaFile {
	file.CodecVideo = "h264"
	file.Resolution = "1080p"
	file.Bitrate = 10_000
	file.VideoTracks = []models.VideoTrack{{
		Codec: "h264", Width: 1920, Height: 1080, FrameRate: "24000/1001", BitDepth: 8, Bitrate: 10_000,
	}}
	return file
}

// virtualResumeHandler wires a fresh (cold-cache) handler. The lister counts
// every provider listing; the detailed resolver counts every provider resolve.
// listed==nil makes the lister fail the test if it is ever reached, so a test
// proves the listing was skipped and not merely answered from a cache.
func virtualResumeHandler(listerCalls, detailedCalls *int, listed []VirtualPlaybackStream) *PlaybackHandler {
	return &PlaybackHandler{
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(
			func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
				return "https://93.184.216.34/legacy?path=" + path, nil
			}),
		VirtualMediaDetailedResolver: countingDetailedResolver(detailedCalls, ResolvedVirtualMedia{
			URL: "https://93.184.216.34/stream/token=fresh", URI: virtualResumeCandidate, CandidateID: "cand-resume",
		}),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(
			func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
				*listerCalls++
				if listed == nil {
					return nil, errors.New("provider list must not run on a durable resume")
				}
				return listed, nil
			}),
	}
}

func virtualResumeStickyKey(file *models.MediaFile) string {
	return bestResultCacheKey(file.ContentID, virtualPlaybackNeutralKey(file.FilePath), file.VirtualOwnerInstallationID)
}

// A resume after a restart (fresh handler, cold caches) whose row still owns an
// unexpired persisted URL and planner-grade video evidence must take the
// deferred fast path: no provider listing and no provider resolve, the row's
// own candidate is served, and the sticky pin is re-derived from the durable
// row instead of being lost with the old process. A row without that evidence
// is covered by TestResolveVirtualResumeIncompleteMetadataFallsThroughToProbe.
func TestResolveVirtualResumeFromPersistedRowSkipsListingAndRepins(t *testing.T) {
	expiresAt := time.Now().Add(3 * time.Hour)
	file := withVirtualResumeVideoEvidence(virtualResumeRow("https://93.184.216.34/stream/token=stored", &expiresAt))

	listerCalls, detailedCalls := 0, 0
	h := virtualResumeHandler(&listerCalls, &detailedCalls, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if listerCalls != 0 {
		t.Fatalf("provider lister called %d times, want 0 on a durable resume", listerCalls)
	}
	if detailedCalls != 0 {
		t.Fatalf("detailed resolver called %d times, want 0 on a durable resume", detailedCalls)
	}
	if resolved.URI != virtualResumeCandidate {
		t.Fatalf("resolved URI = %q, want the persisted candidate %q", resolved.URI, virtualResumeCandidate)
	}
	if resolved.File == nil || resolved.File.FilePath != virtualResumeCandidate {
		t.Fatalf("resolved file = %#v, want the persisted candidate", resolved.File)
	}
	if resolved.Provenance != ProbeProvenancePending || resolved.ProbeSucceeded {
		t.Fatalf("provenance=%q succeeded=%v, want pending/false", resolved.Provenance, resolved.ProbeSucceeded)
	}
	if got := h.peekVirtualSticky(virtualResumeStickyKey(file)); got != virtualResumeCandidate {
		t.Fatalf("sticky pin = %q, want it re-derived as %q", got, virtualResumeCandidate)
	}
}

// An expired or absent stored URL must not take the durable-resume fast path:
// the provider is listed as before and a successful resolve re-pins the
// candidate. This is the negative half of restart equivalence, proving the
// fast path reads the persisted URL's expiry rather than assuming it is usable.
func TestResolveVirtualResumeExpiredOrAbsentStoredURLFallsBackAndRepins(t *testing.T) {
	expiredAt := time.Now().Add(-time.Hour)
	cases := []struct {
		name        string
		resolvedURL string
		expiresAt   *time.Time
	}{
		{name: "expired", resolvedURL: "https://93.184.216.34/stream/token=stale", expiresAt: &expiredAt},
		{name: "absent", resolvedURL: "", expiresAt: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file := virtualResumeRow(tc.resolvedURL, tc.expiresAt)
			listed := []VirtualPlaybackStream{{
				ID: "cand-resume", URI: virtualResumeCandidate,
				Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
			}}
			listerCalls, detailedCalls := 0, 0
			h := virtualResumeHandler(&listerCalls, &detailedCalls, listed)

			req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
			resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false)
			if err != nil {
				t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
			}
			if listerCalls != 1 {
				t.Fatalf("provider lister called %d times, want 1 for a %s stored URL", listerCalls, tc.name)
			}
			if detailedCalls != 1 {
				t.Fatalf("detailed resolver called %d times, want 1 for a %s stored URL", detailedCalls, tc.name)
			}
			if resolved.URI != virtualResumeCandidate {
				t.Fatalf("resolved URI = %q, want the requested candidate %q", resolved.URI, virtualResumeCandidate)
			}
			if got := h.peekVirtualSticky(virtualResumeStickyKey(file)); got != virtualResumeCandidate {
				t.Fatalf("sticky pin = %q, want the resolved candidate %q", got, virtualResumeCandidate)
			}
		})
	}
}

// A neutral requested row (no adopted ?result= identity) carries no candidate
// identity to derive from the row alone, so it must keep the existing
// list-and-rank behavior even when the row happens to carry a stored URL. This
// pins the scope boundary: the durable-resume fast path never invents a
// candidate for a neutral row.
func TestResolveVirtualResumeNeutralRowStillLists(t *testing.T) {
	expiresAt := time.Now().Add(3 * time.Hour)
	file := virtualResumeRow("https://93.184.216.34/stream/token=stored", &expiresAt)
	file.FilePath = "virtual://movie/tt-resume"

	listed := []VirtualPlaybackStream{{
		ID: "cand-resume", URI: virtualResumeCandidate,
		Resolution: "1080p", CodecVideo: "h264", CodecAudio: "aac", Container: "mkv",
	}}
	listerCalls, detailedCalls := 0, 0
	h := virtualResumeHandler(&listerCalls, &detailedCalls, listed)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	if _, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false); err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if listerCalls != 1 {
		t.Fatalf("provider lister called %d times, want 1 for a neutral row", listerCalls)
	}
}

// Cross-node continuation: a resume's zero-discovery decision must be derived
// from the durable catalog row, never from process-local sticky/best-result
// state. Two independent handler instances — separate process state, as two API
// nodes behind a load balancer — resuming the same durable row must both skip
// provider discovery, and the second node never saw the first node's pin or
// cache. This pins the no-process-local-state property for resume.
func TestResolveVirtualResumeCrossNodeSkipsDiscoveryOnBothNodes(t *testing.T) {
	expiresAt := time.Now().Add(3 * time.Hour)
	for node := 1; node <= 2; node++ {
		// A fresh row per node: nothing about the first node's in-memory state
		// travels, only the persisted columns do.
		file := withVirtualResumeVideoEvidence(virtualResumeRow("https://93.184.216.34/stream/token=stored", &expiresAt))
		listerCalls, detailedCalls := 0, 0
		h := virtualResumeHandler(&listerCalls, &detailedCalls, nil)

		req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
		resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false)
		if err != nil {
			t.Fatalf("node %d resolve: %v", node, err)
		}
		if listerCalls != 0 || detailedCalls != 0 {
			t.Fatalf("node %d discovery: lister=%d detailed=%d, want 0/0", node, listerCalls, detailedCalls)
		}
		if resolved.URI != virtualResumeCandidate {
			t.Fatalf("node %d resolved URI = %q, want the persisted %q", node, resolved.URI, virtualResumeCandidate)
		}
	}
}
