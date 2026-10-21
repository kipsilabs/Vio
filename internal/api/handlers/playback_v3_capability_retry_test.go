package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/nodepool"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/tonemap"
	"github.com/Silo-Server/silo-server/internal/transcodenode"
)

// A capability lookup overtaken by an invalidation was handing the caller the
// pre-invalidation report, and that caller goes on to pick transformations and
// a tone-map executor from it — possibly hardware the newer report says is
// gone. It re-probes instead.
func TestLookupRemoteCapabilitiesRefetchesWhenOvertakenMidFlight(t *testing.T) {
	handler := NewPlaybackHandler(nil)
	var fetches atomic.Int32

	// Captured after the server exists; the cache is keyed by the URL the lookup
	// was given, which is not the Host header the node sees.
	var nodeURL string
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempt := fetches.Add(1)
		if attempt == 1 {
			// The health sweep notices the node changed while this first read is
			// still in flight. The counter is bumped directly rather than through
			// RefreshNodeCapabilitiesV3, which would also start a background
			// re-probe and make the fetch count say nothing about this lookup.
			handler.v3NodeCapabilitiesMu.Lock()
			if handler.v3NodeCapabilityInvalidations == nil {
				handler.v3NodeCapabilityInvalidations = make(map[string]uint64)
			}
			handler.v3NodeCapabilityInvalidations[nodeURL]++
			handler.v3NodeCapabilitiesMu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resolved":       "qsv",
			"render_devices": []string{"/dev/dri/renderD128"},
			"transformations": []map[string]any{
				{"name": "hdr_to_sdr_tone_map", "executor": "qsv", "recipe_version": "v3"},
			},
		})
	}))
	t.Cleanup(node.Close)
	nodeURL = node.URL

	if _, err := handler.lookupRemoteCapabilitiesV3(context.Background(), nodeURL, false); err != nil {
		t.Fatalf("lookupRemoteCapabilitiesV3: %v", err)
	}
	if got := fetches.Load(); got != 2 {
		t.Fatalf("fetches = %d, want the overtaken read repeated exactly once", got)
	}
}

// An acceleration change makes a node's inventory wrong and its next capability
// matrix cold — which is exactly when the read is slowest. Dropping the learned
// budget along with the inventory sent the refresh that invalidation triggers
// back to the 120s fallback, short of what a two-device node legitimately asks
// for, so planning lost its inventory precisely after the invalidation.
func TestRefreshNodeCapabilitiesKeepsTheLearnedProbeBudget(t *testing.T) {
	handler := NewPlaybackHandler(nil)

	// The invalidation under test also starts a background re-probe, and a
	// local node answers it in well under a millisecond. Left free, that probe
	// refills the cache between the invalidation and the check that it was
	// emptied whenever the test goroutine is descheduled in between — which a
	// loaded package run does routinely. The node holds the second read until
	// the assertions have run, so the cache state they inspect is the
	// invalidation's, not the scheduler's.
	var fetches atomic.Int32
	reprobeStarted := make(chan struct{})
	releaseReprobe := make(chan struct{})
	advertised := 160_000
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fetches.Add(1) == 2 {
			close(reprobeStarted)
			<-releaseReprobe
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resolved":                 "qsv",
			"probe_request_timeout_ms": advertised,
		})
	}))
	t.Cleanup(node.Close)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseReprobe) }) }
	// Registered after node.Close so it runs first: Close waits for the held
	// request, and a failed assertion must not turn into a hang.
	t.Cleanup(release)

	if _, err := handler.lookupRemoteCapabilitiesV3(context.Background(), node.URL, false); err != nil {
		t.Fatalf("lookupRemoteCapabilitiesV3: %v", err)
	}
	learned := handler.remoteToneMapProbeTimeoutV3(node.URL)
	if want := 160 * time.Second; learned != want {
		t.Fatalf("learned budget = %v, want the advertised %v", learned, want)
	}

	handler.RefreshNodeCapabilitiesV3(node.URL)
	select {
	case <-reprobeStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("invalidation did not start the background re-probe it owes")
	}

	if got := handler.remoteToneMapProbeTimeoutV3(node.URL); got != learned {
		t.Fatalf("budget after invalidation = %v, want the learned %v — the refresh it triggers is sized from this",
			got, learned)
	}
	// The inventory itself is still discarded; only the budget survives. With
	// the re-probe held at the node, an entry here can only be the one the
	// invalidation should have removed.
	handler.v3NodeCapabilitiesMu.Lock()
	_, cached := handler.v3NodeCapabilities[node.URL]
	handler.v3NodeCapabilitiesMu.Unlock()
	if cached {
		t.Fatal("invalidation left the inventory cached")
	}

	// Let the re-probe finish and refill the cache before the node goes away,
	// so the background goroutine does not outlive the test that started it.
	release()
	waitForCachedNodeCapabilitiesV3(t, handler, node.URL)
}

// TestPrepareTransportV3SkipsNodeMissingToneMapSupports verifies that a plan
// whose frozen tone map requires hardware PQ only routes to a node that
// advertises that exact Supports mode and kind, never to a sibling that
// advertises the transformation but only supports software PQ.
func TestPrepareTransportV3SkipsNodeMissingToneMapSupports(t *testing.T) {
	requiredTransformations := []playback.TransformationV3{
		{Name: playback.TransformationVideoToH264V3, Executor: playback.ExecutorServerV3, RecipeVersion: playback.TransformationVideoToH264RecipeVersionV3},
		{Name: playback.TransformationAudioToAACV3, Executor: playback.ExecutorServerV3, RecipeVersion: playback.TransformationAudioToAACRecipeVersionV3},
		{Name: playback.TransformationHDRToSDRToneMapV3, Executor: playback.ExecutorServerV3, RecipeVersion: playback.TransformationHDRToSDRToneMapRecipeVersionV3},
	}
	// hwNode advertises hardware PQ; swNode advertises only software PQ.
	hwNode := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/hw-capabilities":
			writeJSON(w, http.StatusOK, playback.HWAccelInfo{
				Transformations: requiredTransformations,
				ToneMapCapabilities: tonemap.Capabilities{{
					Mode: tonemap.ModeHardware, Backend: tonemap.BackendQSV, Filter: tonemap.HardwareFilterOpenCL,
					SourceKinds: []tonemap.SourceKind{tonemap.SourcePQ},
				}},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/transcode/start":
			var request transcodenode.TranscodeStartRequest
			_ = json.NewDecoder(r.Body).Decode(&request)
			writeJSON(w, http.StatusAccepted, transcodenode.TranscodeStartResponse{
				SessionID: request.SessionID, Status: "started", ToneMapMode: request.ToneMapMode,
			})
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer hwNode.Close()
	swNode := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/hw-capabilities":
			writeJSON(w, http.StatusOK, playback.HWAccelInfo{
				Transformations: requiredTransformations,
				ToneMapCapabilities: tonemap.Capabilities{{
					Mode: tonemap.ModeSoftware, Backend: tonemap.BackendSoftware, Filter: tonemap.SoftwareFilterBT2390,
					SourceKinds: []tonemap.SourceKind{tonemap.SourcePQ},
				}},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/transcode/start":
			var request transcodenode.TranscodeStartRequest
			_ = json.NewDecoder(r.Body).Decode(&request)
			writeJSON(w, http.StatusAccepted, transcodenode.TranscodeStartResponse{
				SessionID: request.SessionID, Status: "started", ToneMapMode: request.ToneMapMode,
			})
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer swNode.Close()

	transcodes := nodepool.NewTranscodePool()
	transcodes.SetNodes([]*nodepool.Node{
		{URL: swNode.URL, Enabled: true, Healthy: true},
		{URL: hwNode.URL, Enabled: true, Healthy: true},
	})
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	handler.JWTSecret = "test-secret"
	handler.NodePlanner = nodepool.NewPlanner(nodepool.NewProxyPool(), transcodes)
	requireWorkerRoutingV3(handler)
	file := stableToneMapTransportFileV3(t)
	result := playback.PlannerResultV3{
		Plan: &playback.PlanV3{
			PlanID: "plan:hw-only", Delivery: playback.DeliveryTranscodeHLSV3,
			Transformations: requiredTransformations,
		},
		PlayMethod: playback.PlayTranscode, TargetVideoCodec: "h264", TargetAudioCodec: "aac",
		ToneMapPolicy: tonemap.PolicyHardwareOnly, ToneMapMode: tonemap.ModeHardware, ToneMapSourceKind: tonemap.SourcePQ,
	}
	transport, transportErr := handler.prepareTransportV3(
		httptest.NewRequest(http.MethodPost, "/", nil),
		&playback.Session{ID: "session-hw-only", UserID: 7, ProfileID: "profile-1"},
		file, result, mediaAuthModeV3{},
	)
	if transportErr != nil || transport.nodeURL != hwNode.URL {
		t.Fatalf("transport = %+v error = %v, want hwNode", transport, transportErr)
	}
	transport.rollback()
}

// TestPrepareTransportV3FallsBackToSoftwareWhenHWNodeLacksCapability verifies
// that a hardware-only plan whose selected node lacks hardware PQ falls back to
// a software-capable node, and that the fallback is attempted exactly once.
func TestPrepareTransportV3FallsBackToSoftwareWhenHWNodeLacksCapability(t *testing.T) {
	requiredTransformations := []playback.TransformationV3{
		{Name: playback.TransformationVideoToH264V3, Executor: playback.ExecutorServerV3, RecipeVersion: playback.TransformationVideoToH264RecipeVersionV3},
		{Name: playback.TransformationAudioToAACV3, Executor: playback.ExecutorServerV3, RecipeVersion: playback.TransformationAudioToAACRecipeVersionV3},
		{Name: playback.TransformationHDRToSDRToneMapV3, Executor: playback.ExecutorServerV3, RecipeVersion: playback.TransformationHDRToSDRToneMapRecipeVersionV3},
	}
	newNode := func(capability tonemap.Capability) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/hw-capabilities":
				writeJSON(w, http.StatusOK, playback.HWAccelInfo{
					Transformations:     requiredTransformations,
					ToneMapCapabilities: tonemap.Capabilities{capability},
				})
			case r.Method == http.MethodPost && r.URL.Path == "/transcode/start":
				var request transcodenode.TranscodeStartRequest
				_ = json.NewDecoder(r.Body).Decode(&request)
				writeJSON(w, http.StatusAccepted, transcodenode.TranscodeStartResponse{
					SessionID: request.SessionID, Status: "started", ToneMapMode: request.ToneMapMode,
				})
			case r.Method == http.MethodDelete:
				w.WriteHeader(http.StatusNoContent)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
	}
	// hardware node without PQ support; software node with PQ support.
	hwNode := newNode(tonemap.Capability{
		Mode: tonemap.ModeHardware, Backend: tonemap.BackendQSV, Filter: tonemap.HardwareFilterOpenCL,
		SourceKinds: []tonemap.SourceKind{tonemap.SourceSDRBT709},
	})
	defer hwNode.Close()
	swNode := newNode(tonemap.Capability{
		Mode: tonemap.ModeSoftware, Backend: tonemap.BackendSoftware, Filter: tonemap.SoftwareFilterBT2390,
		SourceKinds: []tonemap.SourceKind{tonemap.SourcePQ},
	})
	defer swNode.Close()

	transcodes := nodepool.NewTranscodePool()
	transcodes.SetNodes([]*nodepool.Node{
		{URL: hwNode.URL, Enabled: true, Healthy: true},
		{URL: swNode.URL, Enabled: true, Healthy: true},
	})
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	handler.JWTSecret = "test-secret"
	handler.NodePlanner = nodepool.NewPlanner(nodepool.NewProxyPool(), transcodes)
	requireWorkerRoutingV3(handler)
	file := stableToneMapTransportFileV3(t)
	result := playback.PlannerResultV3{
		Plan: &playback.PlanV3{
			PlanID: "plan:hw-fallback", Delivery: playback.DeliveryTranscodeHLSV3,
			Transformations: requiredTransformations,
		},
		PlayMethod: playback.PlayTranscode, TargetVideoCodec: "h264", TargetAudioCodec: "aac",
		ToneMapPolicy: tonemap.PolicyHardwareThenSoftware, ToneMapMode: tonemap.ModeHardware, ToneMapSourceKind: tonemap.SourcePQ,
	}
	transport, transportErr := handler.prepareTransportV3(
		httptest.NewRequest(http.MethodPost, "/", nil),
		&playback.Session{ID: "session-hw-fallback", UserID: 7, ProfileID: "profile-1"},
		file, result, mediaAuthModeV3{},
	)
	if transportErr != nil || transport.nodeURL != swNode.URL || transport.toneMapMode != tonemap.ModeSoftware {
		t.Fatalf("transport = %+v error = %v, want software node fallback", transport, transportErr)
	}
	transport.rollback()
}
