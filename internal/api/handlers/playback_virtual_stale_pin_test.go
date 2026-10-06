package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// stalePinRow builds an identity-less virtual row. FileSize/CodecVideo/Duration
// are the fingerprint tiers; probe evidence is intentionally absent so the P0
// fast path cannot serve the row.
func stalePinRow(id int, contentID, neutralURI, resultID string) *models.MediaFile {
	return &models.MediaFile{
		ID:                         id,
		ContentID:                  contentID,
		FilePath:                   neutralURI + "?result=" + resultID,
		Container:                  "mkv",
		CodecVideo:                 "h264",
		CodecAudio:                 "aac",
		Resolution:                 "1080p",
		VirtualOwnerInstallationID: 5,
		FileSize:                   8_500_000_000,
		Duration:                   7200,
	}
}

// stalePinLiveStream is a live listing record for the same content under a new
// result id. FileSize and codec are overridable so a test can force a mismatch.
func stalePinLiveStream(neutralURI, resultID string, fileSize int64, videoCodec string) VirtualPlaybackStream {
	return VirtualPlaybackStream{
		ID: resultID, URI: neutralURI + "?result=" + resultID,
		Resolution: "1080p", CodecVideo: videoCodec, CodecAudio: "aac",
		Container: "mkv", FileSize: fileSize,
	}
}

// stalePinRecoveryHandler wires the recovery's two collaborators: the lister
// answers a fixed live set, and the resolver records every call and succeeds for
// any non-pinned candidate (or fails for all, when failAll is set).
func stalePinRecoveryHandler(live []VirtualPlaybackStream, pinnedURI string, failAll bool) (*PlaybackHandler, *atomic.Int32, *atomic.Int32) {
	var resolveCalls, listCalls atomic.Int32
	h := &PlaybackHandler{
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
			return "", errors.New("simple resolver must not be used when the detailed resolver is set")
		}),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
			listCalls.Add(1)
			return live, nil
		}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			resolveCalls.Add(1)
			if failAll || uri == pinnedURI {
				return ResolvedVirtualMedia{}, absentSessionPinError("STALE")
			}
			return ResolvedVirtualMedia{URL: "http://127.0.0.1:8080/stream?uri=" + uri, URI: uri, CandidateID: "NEW"}, nil
		}),
	}
	return h, &resolveCalls, &listCalls
}

func stalePinRecoveryRequest() *http.Request {
	return httptest.NewRequest(http.MethodPost, "/api/v2/playback/start", nil)
}

func stalePinRequest() playback.StartRequestV3 {
	return playback.StartRequestV3{QualityPreference: "auto"}
}

// (a) A dead pin absent from a NON-EMPTY listing, on an identity-less row, with a
// live fingerprint match recovers: the recovery resolves the matched candidate
// exactly once and returns it, in memory only.
func TestStalePinRecoveryRecoversFingerprintMatch(t *testing.T) {
	const neutral = "virtual://movie/tt-stale-recover"
	pinnedURI := neutral + "?result=STALE"
	row := stalePinRow(1, "movie-stale-recover", neutral, "STALE")
	live := []VirtualPlaybackStream{stalePinLiveStream(neutral, "NEW", row.FileSize, "h264")}
	h, resolveCalls, _ := stalePinRecoveryHandler(live, pinnedURI, false)

	recovered, ok := h.recoverStaleIdentityLessPinV3(stalePinRecoveryRequest(), row, "profile-1", stalePinRequest(), 0)
	if !ok {
		t.Fatal("a fingerprint-matched live candidate must recover the pin")
	}
	if recovered.File == nil || recovered.URI != neutral+"?result=NEW" {
		t.Fatalf("recovered = %#v, want the fingerprint-matched live candidate", recovered)
	}
	if got := virtualResultCandidateID(recovered.URI); got != "NEW" {
		t.Fatalf("recovered candidate = %q, want NEW", got)
	}
	if got := resolveCalls.Load(); got != 1 {
		t.Fatalf("resolver calls = %d, want exactly 1 (one recovery retry, no loop)", got)
	}
	// The row's durable pin is never rewritten: only the returned copy changed.
	if got := virtualResultCandidateID(row.FilePath); got != "STALE" {
		t.Fatalf("catalog row pin = %q, want the durable STALE unchanged", got)
	}
}

// (b) An EMPTY listing is a transient blackout, not a renumber: no recovery.
func TestStalePinRecoveryEmptyListingPreservesTerminal(t *testing.T) {
	const neutral = "virtual://movie/tt-stale-empty"
	row := stalePinRow(2, "movie-stale-empty", neutral, "STALE")
	h, resolveCalls, _ := stalePinRecoveryHandler(nil, neutral+"?result=STALE", false)

	_, ok := h.recoverStaleIdentityLessPinV3(stalePinRecoveryRequest(), row, "profile-1", stalePinRequest(), 0)
	if ok {
		t.Fatal("an empty listing must not recover")
	}
	if got := resolveCalls.Load(); got != 0 {
		t.Fatalf("resolver calls = %d, want 0 for an empty listing", got)
	}
}

// (c) A non-empty listing with NO fingerprint match (size outside tolerance)
// does not recover.
func TestStalePinRecoveryNoFingerprintMatchPreservesTerminal(t *testing.T) {
	const neutral = "virtual://movie/tt-stale-nomatch"
	row := stalePinRow(3, "movie-stale-nomatch", neutral, "STALE")
	// 30% larger: outside the 5% size tolerance.
	live := []VirtualPlaybackStream{stalePinLiveStream(neutral, "OTHER", row.FileSize*13/10, "h264")}
	h, resolveCalls, _ := stalePinRecoveryHandler(live, neutral+"?result=STALE", false)

	_, ok := h.recoverStaleIdentityLessPinV3(stalePinRecoveryRequest(), row, "profile-1", stalePinRequest(), 0)
	if ok {
		t.Fatal("a fingerprint miss must not recover")
	}
	if got := resolveCalls.Load(); got != 0 {
		t.Fatalf("resolver calls = %d, want 0 without a fingerprint match", got)
	}
}

// (c2) A non-empty listing with the right size but a mismatched video codec does
// not recover.
func TestStalePinRecoveryCodecMismatchPreservesTerminal(t *testing.T) {
	const neutral = "virtual://movie/tt-stale-codec"
	row := stalePinRow(4, "movie-stale-codec", neutral, "STALE")
	live := []VirtualPlaybackStream{stalePinLiveStream(neutral, "HEVC", row.FileSize, "hevc")}
	h, resolveCalls, _ := stalePinRecoveryHandler(live, neutral+"?result=STALE", false)

	_, ok := h.recoverStaleIdentityLessPinV3(stalePinRecoveryRequest(), row, "profile-1", stalePinRequest(), 0)
	if ok {
		t.Fatal("a codec mismatch must not recover")
	}
	if got := resolveCalls.Load(); got != 0 {
		t.Fatalf("resolver calls = %d, want 0", got)
	}
}

// (d) The recovery never adopts across a different content_id: a live candidate
// under another neutral path is dropped by the same-content filter before
// matching, so no resolve is attempted for it.
func TestStalePinRecoveryNoCrossContentAdoption(t *testing.T) {
	const (
		neutral = "virtual://movie/tt-stale-cross"
		foreign = "virtual://movie/tt-some-other-content"
	)
	row := stalePinRow(5, "movie-stale-cross", neutral, "STALE")
	// Same size and codec, but a different content path.
	live := []VirtualPlaybackStream{stalePinLiveStream(foreign, "FOREIGN", row.FileSize, "h264")}
	h, resolveCalls, _ := stalePinRecoveryHandler(live, neutral+"?result=STALE", false)

	_, ok := h.recoverStaleIdentityLessPinV3(stalePinRecoveryRequest(), row, "profile-1", stalePinRequest(), 0)
	if ok {
		t.Fatal("a foreign-content candidate must not recover the pin")
	}
	if got := resolveCalls.Load(); got != 0 {
		t.Fatalf("resolver calls = %d, want 0 (the foreign candidate is never resolved)", got)
	}
}

// (e) The recovery is a single retry, not a loop: even when the matched
// candidate also fails, exactly one recovery resolve is attempted.
func TestStalePinRecoverySingleRetryBound(t *testing.T) {
	const neutral = "virtual://movie/tt-stale-retry"
	row := stalePinRow(6, "movie-stale-retry", neutral, "STALE")
	live := []VirtualPlaybackStream{stalePinLiveStream(neutral, "NEW", row.FileSize, "h264")}
	h, resolveCalls, _ := stalePinRecoveryHandler(live, neutral+"?result=STALE", true)

	_, ok := h.recoverStaleIdentityLessPinV3(stalePinRecoveryRequest(), row, "profile-1", stalePinRequest(), 0)
	if ok {
		t.Fatal("a persistently failing recovery resolve must not report success")
	}
	if got := resolveCalls.Load(); got != 1 {
		t.Fatalf("resolver calls = %d, want exactly 1 (bounded single retry)", got)
	}
}

// (f) A row WITH durable identity is left to the resolver's same-release rematch:
// the recovery does not fire and no listing or resolve is attempted.
func TestStalePinRecoverySkippedForIdentityCarryingRow(t *testing.T) {
	const neutral = "virtual://movie/tt-stale-identity"
	row := stalePinRow(7, "movie-stale-identity", neutral, "STALE")
	row.ProviderVideoHash = "hash-a" // durable identity present
	h, resolveCalls, listCalls := stalePinRecoveryHandler(
		[]VirtualPlaybackStream{stalePinLiveStream(neutral, "NEW", row.FileSize, "h264")},
		neutral+"?result=STALE", false)

	_, ok := h.recoverStaleIdentityLessPinV3(stalePinRecoveryRequest(), row, "profile-1", stalePinRequest(), 0)
	if ok {
		t.Fatal("an identity-carrying row must not be recovered by the identity-less path")
	}
	if got := resolveCalls.Load(); got != 0 {
		t.Fatalf("resolver calls = %d, want 0 (identity-carrying rows use the resolver rematch)", got)
	}
	if got := listCalls.Load(); got != 0 {
		t.Fatalf("lister calls = %d, want 0 (the recovery must not even list)", got)
	}
}

// (g) The kill switch disables the recovery entirely.
func TestStalePinRecoveryKillSwitchDisables(t *testing.T) {
	prev := virtualStalePinRecoveryEnabled
	virtualStalePinRecoveryEnabled = false
	t.Cleanup(func() { virtualStalePinRecoveryEnabled = prev })

	const neutral = "virtual://movie/tt-stale-kill"
	row := stalePinRow(8, "movie-stale-kill", neutral, "STALE")
	h, resolveCalls, listCalls := stalePinRecoveryHandler(
		[]VirtualPlaybackStream{stalePinLiveStream(neutral, "NEW", row.FileSize, "h264")},
		neutral+"?result=STALE", false)

	_, ok := h.recoverStaleIdentityLessPinV3(stalePinRecoveryRequest(), row, "profile-1", stalePinRequest(), 0)
	if ok {
		t.Fatal("the kill switch must disable recovery")
	}
	if got := resolveCalls.Load(); got != 0 {
		t.Fatalf("resolver calls = %d, want 0 with the kill switch off", got)
	}
	if got := listCalls.Load(); got != 0 {
		t.Fatalf("lister calls = %d, want 0 with the kill switch off", got)
	}
}

// TestStalePinRecoveryIntegratedInVersionFallbackWalk proves the walk actually
// invokes the recovery: an identity-less pin absent from the live listing
// recovers to the fingerprint-matched candidate instead of terminaling. The
// control run below (recovery disabled) proves the recovery is load-bearing.
func TestStalePinRecoveryIntegratedInVersionFallbackWalk(t *testing.T) {
	const neutral = "virtual://movie/tt-stale-walk"
	pinnedURI := neutral + "?result=STALE"
	row := stalePinRow(9, "movie-stale-walk", neutral, "STALE")
	live := []VirtualPlaybackStream{stalePinLiveStream(neutral, "NEW", row.FileSize, "h264")}
	h, resolveCalls, _ := stalePinRecoveryHandler(live, pinnedURI, false)
	h.FileVersionFetcher = testPlaybackFileVersionFetcher{byContent: map[string][]*models.MediaFile{row.ContentID: {row}}}

	req := stalePinRecoveryRequest()
	resolved, err := h.resolveVirtualStartWithVersionFallback(req, row, "profile-1", stalePinRequest(), false, 0)
	if err != nil {
		t.Fatalf("walk recovery: %v", err)
	}
	if resolved.File == nil || virtualResultCandidateID(resolved.URI) != "NEW" {
		t.Fatalf("resolved = %#v, want the fingerprint-matched candidate NEW", resolved)
	}
	if got := resolveCalls.Load(); got == 0 {
		t.Fatal("the walk never consulted the resolver")
	}

	// Control: with the recovery disabled the same scenario terminals, so the
	// recovery is what recovers it.
	prev := virtualStalePinRecoveryEnabled
	virtualStalePinRecoveryEnabled = false
	t.Cleanup(func() { virtualStalePinRecoveryEnabled = prev })
	control := stalePinRow(10, "movie-stale-walk", neutral, "STALE")
	controlHandler, _, _ := stalePinRecoveryHandler(live, pinnedURI, false)
	controlHandler.FileVersionFetcher = testPlaybackFileVersionFetcher{byContent: map[string][]*models.MediaFile{control.ContentID: {control}}}
	if _, controlErr := controlHandler.resolveVirtualStartWithVersionFallback(stalePinRecoveryRequest(), control, "profile-1", stalePinRequest(), false, 0); controlErr == nil {
		t.Fatal("with the recovery disabled the dead pin must terminal; the recovery is masking a walk that already substitutes")
	}
}

// The fingerprint predicate is the boundary: inside tolerance accepts, outside
// refuses, and only the duration tier is neutral for an unknown side.
func TestStalePinFingerprintTolerance(t *testing.T) {
	row := &models.MediaFile{CodecVideo: "h264", CodecAudio: "aac", FileSize: 1_000_000, Duration: 7200}
	cases := []struct {
		name      string
		candidate VirtualPlaybackStream
		want      bool
	}{
		{"exact", VirtualPlaybackStream{CodecVideo: "h264", CodecAudio: "aac", FileSize: 1_000_000}, true},
		{"size +4%", VirtualPlaybackStream{CodecVideo: "h264", CodecAudio: "aac", FileSize: 1_040_000}, true},
		{"size -5%", VirtualPlaybackStream{CodecVideo: "h264", CodecAudio: "aac", FileSize: 950_000}, true},
		{"size +6% refused", VirtualPlaybackStream{CodecVideo: "h264", CodecAudio: "aac", FileSize: 1_060_000}, false},
		{"unknown candidate size refused", VirtualPlaybackStream{CodecVideo: "h264", CodecAudio: "aac", FileSize: 0}, false},
		{"codec mismatch refused", VirtualPlaybackStream{CodecVideo: "hevc", CodecAudio: "aac", FileSize: 1_000_000}, false},
		{"unknown candidate codec refused", VirtualPlaybackStream{CodecAudio: "aac", FileSize: 1_000_000}, false},
		{"audio codec mismatch refused", VirtualPlaybackStream{CodecVideo: "h264", CodecAudio: "eac3", FileSize: 1_000_000}, false},
		{"unknown candidate audio neutral", VirtualPlaybackStream{CodecVideo: "h264", FileSize: 1_000_000}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := virtualStalePinStreamFingerprintMatch(row, tc.candidate); got != tc.want {
				t.Fatalf("fingerprint = %v, want %v", got, tc.want)
			}
		})
	}

	durations := []struct {
		row, candidate int
		want           bool
	}{
		{7200, 7200, true},
		{7200, 7300, true},  // +1.4%
		{7200, 7400, false}, // +2.8%
		{0, 7300, true},     // unknown row duration is neutral
		{7200, 0, true},     // unknown candidate duration is neutral
	}
	for _, tc := range durations {
		if got := virtualStalePinDurationMatch(tc.row, tc.candidate); got != tc.want {
			t.Fatalf("duration(%d,%d) = %v, want %v", tc.row, tc.candidate, got, tc.want)
		}
	}
}
