package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/resolver"
)

// consumerStatuses are the transport statuses the all-path safety contract
// treats identically. 502 is the pre-existing case; 500 and 503 must behave
// exactly the same, so a temporary provider failure never becomes a verdict on
// one status while staying retryable on another. The errors carry the resolver's
// typed provider-unavailable sentinel exactly as fetchProviderCandidates wraps
// every non-200 provider status.
var consumerStatuses = []struct {
	name string
	err  error
}{
	{"500", fmt.Errorf("%w: streaming provider returned status 500", resolver.ErrProviderUnavailable)},
	{"502", provider502()},
	{"503", fmt.Errorf("%w: streaming provider returned status 503", resolver.ErrProviderUnavailable)},
}

// TestConsumerStartupTemporaryStatusIsRetryable503 proves the transport-startup
// consumer classifies 500 and 503 exactly as 502: a trusted session-bound row
// whose provider stayed unavailable surfaces the retryable provider_unavailable
// terminal, so the client keeps retrying the release it picked on every 5xx,
// not just 502.
func TestConsumerStartupTemporaryStatusIsRetryable503(t *testing.T) {
	for _, tc := range consumerStatuses {
		t.Run(tc.name, func(t *testing.T) {
			pinned := "virtual://movie/tt-consumer-startup?result=pinned"
			row := providerOutageRow(401, pinned)
			row.ResolvedURL = ""

			trusted := virtualCandidateTrustedForOutageRetry(row, 720*time.Hour)
			if !trusted {
				t.Fatal("precondition: row must be inside the outage trust window")
			}
			classified := classifyVirtualProviderOutage(tc.err, trusted)
			if !errors.Is(classified, errVirtualProviderUnavailable) {
				t.Fatalf("status %s classified as %v, want errVirtualProviderUnavailable", tc.name, classified)
			}
			terminal := transportStartFailureV3(classified, nil)
			if terminal == nil || terminal.reason != providerUnavailableReasonV3 || !terminal.retryable {
				t.Fatalf("status %s terminal = %#v, want retryable %s", tc.name, terminal, providerUnavailableReasonV3)
			}
		})
	}
}

// TestConsumerStartupEndToEndTemporaryStatusNeverStamps drives the real
// transport-startup path for a session-bound virtual row whose provider stays
// unavailable with 500/502/503. Every status must behave identically: exactly
// one initial resolve plus the two bounded outage retries, no durable
// dead-candidate stamp, and a retryable provider-unavailable classification on
// the final error. This is the startup consumer's all-path guarantee, not the
// classification helper's.
func TestConsumerStartupEndToEndTemporaryStatusNeverStamps(t *testing.T) {
	restore := virtualProviderOutageBackoff
	virtualProviderOutageBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	defer func() { virtualProviderOutageBackoff = restore }()

	for _, tc := range consumerStatuses {
		t.Run(tc.name, func(t *testing.T) {
			pinned := "virtual://movie/tt-consumer-startup-e2e?result=pinned"
			row := providerOutageRow(402, pinned)
			row.ResolvedURL = ""

			provider := &outageStartupProvider{}
			handler := outageStartupHandler(t, row, provider)
			handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(context.Context, string, int, int, string, bool, []string, string) (ResolvedVirtualMedia, error) {
				provider.calls.Add(1)
				return ResolvedVirtualMedia{}, tc.err
			})
			var stamps []string
			handler.VirtualCandidateFailMarker = func(_ context.Context, _ int, expectedFilePath string, _ *time.Time) error {
				stamps = append(stamps, expectedFilePath)
				return nil
			}
			sessionID := outageStartupSession(t, handler, row.ID, pinned)

			_, err := handler.startLocalPlaybackTransportOnce(context.Background(), playback.TranscodeOpts{
				MediaFileID:                      row.ID,
				InputPath:                        pinned,
				VirtualSourceOwnerInstallationID: 5,
				SessionID:                        sessionID,
			})
			if err == nil {
				t.Fatalf("status %s: startup succeeded while the provider stayed unavailable", tc.name)
			}
			if !errors.Is(err, errVirtualProviderUnavailable) {
				t.Fatalf("status %s: final error = %v, want the retryable provider_unavailable classification", tc.name, err)
			}
			if len(stamps) != 0 {
				t.Fatalf("status %s: temporary startup failure stamped candidate(s) %v, want none", tc.name, stamps)
			}
			// The row's ResolvedURL is empty, so every startup resolve hits the
			// provider. The bounded outage retry is initial + 2; a same-release
			// failover attempt may add one more, but never an unbounded storm.
			if got := provider.calls.Load(); got < 3 || got > 4 {
				t.Fatalf("status %s: provider resolves = %d, want 3 (initial + 2 outage retries) or 4 (one failover), not more", tc.name, got)
			}
		})
	}
}

// TestConsumerStartPathStampTemporaryStatusNeverStamps proves the fresh-start
// stamp consumer (stampStartVirtualCandidateFailed) treats 500 and 503 exactly
// as 502: none is a confirmed-dead verdict, so none stamps the row, while a
// genuine dead message still does. This is what keeps a provider 5xx from
// indicting a release before playback has even begun.
func TestConsumerStartPathStampTemporaryStatusNeverStamps(t *testing.T) {
	row := &models.MediaFile{
		ID:                         901,
		ContentID:                  "movie-start-stamp",
		FilePath:                   "virtual://movie/tt-start-stamp?result=cand-a",
		ProviderVideoHash:          "hash-a",
		ProviderReleaseName:        "Movie.2024.1080p",
		VirtualOwnerInstallationID: 5,
	}
	for _, tc := range consumerStatuses {
		t.Run(tc.name, func(t *testing.T) {
			var stamps []string
			h := &PlaybackHandler{
				VirtualCandidateFailMarker: func(_ context.Context, _ int, expectedFilePath string, _ *time.Time) error {
					stamps = append(stamps, expectedFilePath)
					return nil
				},
			}
			h.stampStartVirtualCandidateFailed(context.Background(), row, tc.err)
			if len(stamps) != 0 {
				t.Fatalf("status %s stamped candidate(s) %v, want none", tc.name, stamps)
			}
		})
	}

	t.Run("confirmed dead still stamps", func(t *testing.T) {
		var stamps []string
		h := &PlaybackHandler{
			VirtualCandidateFailMarker: func(_ context.Context, _ int, expectedFilePath string, _ *time.Time) error {
				stamps = append(stamps, expectedFilePath)
				return nil
			},
		}
		h.stampStartVirtualCandidateFailed(context.Background(), row, errors.New("virtual stream provider returned no matching candidate"))
		if len(stamps) != 1 {
			t.Fatalf("confirmed-dead start stamped %d candidates, want 1", len(stamps))
		}
	})
}

// TestConsumerRemuxSeekAnchorTemporaryStatusRetriesIdentically proves the remux
// seek-anchor consumer treats 500 and 503 exactly as 502: the same-release
// probe is retried once after a bounded backoff and, when it still fails, the
// terminal is the retryable transcode_start_failed carrying the transient cause
// rather than a dead-release verdict.
func TestConsumerRemuxSeekAnchorTemporaryStatusRetriesIdentically(t *testing.T) {
	for _, tc := range consumerStatuses {
		t.Run(tc.name, func(t *testing.T) {
			handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
			probeCalls := 0
			handler.copySeekAnchor = func(context.Context, string, string, float64, int) (float64, int, error) {
				probeCalls++
				// ffmpeg's stderr for an upstream 5xx carries the concrete status;
				// transientProviderCause classifies 500, 502, and 503 identically.
				return 0, 0, fmt.Errorf("%w: exit status 8 (stderr: Server returned %s Server Error)", playback.ErrTransientProvider, tc.name)
			}
			var backoffs []time.Duration
			handler.copySeekAnchorBackoff = func(_ context.Context, d time.Duration) bool {
				backoffs = append(backoffs, d)
				return true
			}
			plan := &playback.PlanV3{PlanID: "plan:consumer-remux", Delivery: playback.DeliveryRemuxHLSV3, Timeline: playback.TimelineV3{SourceStartSeconds: 120}}
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			ctx, cancel := context.WithTimeout(req.Context(), time.Minute)
			defer cancel()
			_, transportErr := handler.prepareTransportV3(
				req.WithContext(ctx),
				&playback.Session{ID: "session-consumer-remux"},
				&models.MediaFile{ID: 42, FilePath: "/media/movie.mkv"},
				playback.PlannerResultV3{Plan: plan, PlayMethod: playback.PlayRemux}, mediaAuthModeV3{})

			if probeCalls != 2 {
				t.Fatalf("status %s: copy anchor probes = %d, want 2 (one retry)", tc.name, probeCalls)
			}
			if len(backoffs) != 1 {
				t.Fatalf("status %s: backoffs = %d, want exactly 1 before the retry", tc.name, len(backoffs))
			}
			if transportErr == nil || transportErr.reason != "transcode_start_failed" || !transportErr.retryable || !playback.IsTransientProviderError(transportErr.cause) {
				t.Fatalf("status %s transport error = %#v, want retryable transcode_start_failed with the transient cause", tc.name, transportErr)
			}
		})
	}
}

// TestConsumerProbeTemporaryFailureNeverStampsVideoCandidate proves the
// background probe consumer treats a temporary probe failure as unknown, not a
// verdict: across 500/502/503 it never calls the candidate fail marker, and a
// single occurrence keeps the active sticky pin. The candidate is never
// indicted by a probe that only shows the provider was briefly unavailable.
func TestConsumerProbeTemporaryFailureNeverStampsVideoCandidate(t *testing.T) {
	for _, tc := range consumerStatuses {
		t.Run(tc.name, func(t *testing.T) {
			uri := "virtual://series/tt-consumer-probe/3/5?result=cand-1"
			key := virtualProbeFailureKey(uri, 5)
			virtualProbeFailures.clear(key)
			t.Cleanup(func() { virtualProbeFailures.clear(key) })

			var stamped int
			h := &PlaybackHandler{
				VirtualPlaybackSourceProber: func(context.Context, string, *models.MediaFile) (*models.MediaFile, error) {
					return nil, tc.err
				},
				VirtualCandidateFailMarker: func(context.Context, int, string, *time.Time) error {
					stamped++
					return nil
				},
			}
			stickyKey := "sticky-consumer-probe"
			h.pinVirtualSticky(stickyKey, uri)
			file := &models.MediaFile{ID: 801, ContentID: "series-tt", EpisodeID: "ep-1", FilePath: uri, VirtualOwnerInstallationID: 5}
			cand := VirtualPlaybackStream{ID: "cand-1", URI: uri}

			h.probeVirtualSourceAndPersist(context.Background(), stickyKey, file, "http://provider.example/stream", *file, cand, 45, 5)

			if stamped != 0 {
				t.Fatalf("status %s: probe failure stamped the video candidate %d times, want 0", tc.name, stamped)
			}
			if got := h.peekVirtualSticky(stickyKey); got != uri {
				t.Fatalf("status %s: one transient probe failure released the active pin (got %q, want %q)", tc.name, got, uri)
			}
		})
	}
}

// TestConsumerSubtitleTemporaryResolveNeverStampsOrRotates proves the subtitle
// sidecar and font consumers treat a provider failure identically across
// 500/502/503: the response is a subtitle-local 502, the video candidate is
// never stamped, and no resolve declares rotation or excludes a candidate. A
// subtitle asset failure must never become a video-release verdict.
func TestConsumerSubtitleTemporaryResolveNeverStampsOrRotates(t *testing.T) {
	routes := []struct {
		name    string
		request func(sessionID string) *http.Request
	}{
		{"sidecar", func(sessionID string) *http.Request {
			return playbackTestRequest(http.MethodGet, "/api/v1/stream/"+sessionID+"/subtitles/0.vtt", nil,
				map[string]string{"session_id": sessionID, "track": "0.vtt"})
		}},
		{"font", func(sessionID string) *http.Request {
			return playbackFontRequest(sessionID, "0")
		}},
	}
	for _, route := range routes {
		for _, tc := range consumerStatuses {
			t.Run(route.name+"/"+tc.name, func(t *testing.T) {
				tracks := []models.SubtitleTrack{{Index: 0, Codec: "subrip"}}
				if route.name == "font" {
					tracks = []models.SubtitleTrack{{Index: 4, Codec: "ass"}}
				}
				handler, session, _, spy, _ := newVirtualSubtitleCandidateFixture(t, tracks)
				// Replace the fixture resolver with the status-shaped failure.
				handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(ctx context.Context, _ string, _ int, _ int, _ string, _ bool, excluded []string, _ string) (ResolvedVirtualMedia, error) {
					spy.rotation(ctx, excluded)
					return ResolvedVirtualMedia{}, tc.err
				})

				recorder := httptest.NewRecorder()
				handler.subtitleRouteForTest(route.name, recorder, route.request(session.ID))

				if recorder.Code != http.StatusBadGateway {
					t.Fatalf("status %s: %s status = %d, body = %s, want 502 subtitle-local", tc.name, route.name, recorder.Code, recorder.Body.String())
				}
				var body struct {
					Error string `json:"error"`
				}
				if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
					t.Fatalf("status %s: decode body: %v (body = %s)", tc.name, err, recorder.Body.String())
				}
				if body.Error != subtitleSourceUnavailableErrorCode {
					t.Fatalf("status %s: %s error code = %q, want %q", tc.name, route.name, body.Error, subtitleSourceUnavailableErrorCode)
				}
				if spy.candidateStamped {
					t.Fatalf("status %s: %s resolve failure stamped the video candidate", tc.name, route.name)
				}
				for _, allowed := range spy.rotationSeen {
					if allowed {
						t.Fatalf("status %s: %s resolve declared rotation", tc.name, route.name)
					}
				}
				for _, excluded := range spy.excludedSeen {
					if len(excluded) != 0 {
						t.Fatalf("status %s: %s resolve excluded candidates %v", tc.name, route.name, excluded)
					}
				}
			})
		}
	}
}

// subtitleRouteForTest dispatches the two subtitle-asset routes by name so the
// table above reads uniformly.
func (h *StreamHandler) subtitleRouteForTest(route string, w http.ResponseWriter, r *http.Request) {
	if route == "font" {
		h.HandleSubtitleFonts(w, r)
		return
	}
	h.HandleSubtitle(w, r)
}
