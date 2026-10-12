package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// TestDurableResumeUnstampedStartArmsProbeOnEverySurface drives the full start
// through the shared v3 entrypoint for the three client surfaces: a negotiating
// v2 client, a non-negotiating v2 client, and the frozen v1 surface. On an
// unstamped row that owns a persisted candidate, all three must admit the
// post-commit probe so the row is stamped and the next play reads verified
// inventory. Only the negotiating client may see tracks_pending on the response,
// because the flag is part of the deferred_track_inventory_v1 lifecycle.
func TestDurableResumeUnstampedStartArmsProbeOnEverySurface(t *testing.T) {
	cases := []struct {
		name              string
		v2                bool
		negotiate         bool
		wantTracksPending bool
	}{
		{name: "negotiated v2", v2: true, negotiate: true, wantTracksPending: true},
		{name: "non-negotiating v2", v2: true, negotiate: false, wantTracksPending: false},
		{name: "frozen v1", v2: false, negotiate: false, wantTracksPending: false},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probedAt := time.Now().UTC()
			source := v3HandlerFixtureFile(t)
			source.ID = 760 + i
			source.ContentID = fmt.Sprintf("movie-arm-%d", i)
			source.FilePath = fmt.Sprintf("virtual://movie/tt-arm-%d?result=cand-1", i)
			source.VirtualOwnerInstallationID = 5
			// The row owns a persisted candidate URL but has never been probed.
			source.ProbeUpdatedAt = nil
			expiresAt := time.Now().Add(3 * time.Hour)
			source.ResolvedURL = "https://93.184.216.34/stream/token=stored"
			source.ResolvedURLExpiresAt = &expiresAt

			manager := playback.NewSessionManager(0, 0)
			resolver := &syncPlaybackFileResolver{file: *source}
			handler := NewPlaybackHandler(manager, resolver)
			handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{"allow_4k_transcode": "true"}}
			handler.ItemAccess = allowAllPlaybackItemAccess{}
			handler.PlaybackConfig = playbackTestConfig("", "")
			handler.RealtimeHub = playback.NewRealtimeHub()
			stubCopySeekAnchorV3(handler)
			handler.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
				return "http://127.0.0.1:8080/stream?path=" + path, nil
			})

			probeStarted := make(chan struct{})
			releaseProbe := make(chan struct{})
			var probeOnce sync.Once
			handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
				probeOnce.Do(func() { close(probeStarted) })
				<-releaseProbe
				f.AudioTracks = []models.AudioTrack{{Index: 1, Codec: "aac", Channels: 2, Language: "eng", Default: true}}
				f.CodecAudio = "aac"
				return f, nil
			}
			stamped := make(chan struct{})
			var stampOnce sync.Once
			// The saver records whether the deferred write asked for the probe
			// stamp and only stamps the row when it did. A fake saver that stamps
			// unconditionally would hide a write that dropped its explicit
			// successful-probe provenance; asserting the flag keeps this
			// all-surface acceptance test honest about what the worker sent.
			var stampRequested bool
			var stampMu sync.Mutex
			handler.VirtualFileMetadataSaver = func(_ context.Context, args models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
				var audio []models.AudioTrack
				if err := json.Unmarshal(args.AudioTracks, &audio); err != nil {
					return VirtualFileMetadataUpdateResult{}, err
				}
				stampMu.Lock()
				stampRequested = args.StampProbe
				stampMu.Unlock()
				resolver.update(func(f *models.MediaFile) {
					f.AudioTracks = audio
					if args.StampProbe {
						f.ProbeUpdatedAt = &probedAt
					}
				})
				stampOnce.Do(func() { close(stamped) })
				return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
			}

			request := v3HandlerStartRequest()
			request.FileID = source.ID
			request.QualityPreference = "original"
			if tc.negotiate {
				request.ClientFeatures = append(request.ClientFeatures, playback.FeatureDeferredTrackInventoryV3)
			}
			var start playback.DecisionResponseV3
			if tc.v2 {
				start = startV3PlaybackV2ForHandlerTest(t, handler, request)
			} else {
				start = startV3PlaybackForHandlerTest(t, handler, request)
			}
			if start.PlaybackPlan == nil {
				t.Fatal("start returned no playback plan")
			}
			if start.PlaybackPlan.TracksPending != tc.wantTracksPending {
				t.Fatalf("tracks_pending = %v, want %v", start.PlaybackPlan.TracksPending, tc.wantTracksPending)
			}
			select {
			case <-probeStarted:
			case <-time.After(2 * time.Second):
				t.Fatal("post-commit probe was not admitted for an unstamped row")
			}
			close(releaseProbe)
			select {
			case <-stamped:
			case <-time.After(2 * time.Second):
				t.Fatal("post-commit probe did not stamp the row")
			}
			stampMu.Lock()
			gotStamp := stampRequested
			stampMu.Unlock()
			if !gotStamp {
				t.Fatal("deferred probe write did not request the probe stamp")
			}
		})
	}
}
