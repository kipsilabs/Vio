package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/nodepool"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// The inventory fetch is bounded per node by that node's own cold probe
// budget. A node whose caches were just invalidated — a widened device
// override is the common case — legitimately walks past the flat floor, and
// cutting it off reported the node as failed on the Playback settings page
// while it was still inside its own advertised budget.
func TestRemoteInventoryTimeoutScalesWithTheNodeBudget(t *testing.T) {
	t.Parallel()

	advertisedMillis := (90 * time.Second).Milliseconds()
	report := json.RawMessage(fmt.Sprintf(`{"probe_request_timeout_ms":%d}`, advertisedMillis))
	node := &nodepool.Node{URL: "http://node:8082", Capabilities: report}

	handler := &SystemHandler{}
	// Derivation-guard: the expected budget is what the shared pricing rule
	// answers, asserted to exceed the flat floor so the test cannot pass
	// vacuously if the fixture stops out-pricing it.
	want := playback.ColdCapabilityRequestTimeout(report, "", "", remoteNodeInventoryProbeTimeout)
	if want <= remoteNodeInventoryProbeTimeout {
		t.Fatalf("fixture no longer out-prices the floor: got %v, floor %v", want, remoteNodeInventoryProbeTimeout)
	}
	if got := handler.remoteInventoryTimeout(node); got != want {
		t.Fatalf("remoteInventoryTimeout() = %v, want the node's cold budget %v", got, want)
	}

	// A node with no report and no override prices from the cluster policy
	// over the floor — never below it.
	if got := handler.remoteInventoryTimeout(&nodepool.Node{URL: "http://bare:8082"}); got < remoteNodeInventoryProbeTimeout {
		t.Fatalf("remoteInventoryTimeout(bare node) = %v, below the %v floor", got, remoteNodeInventoryProbeTimeout)
	}
}

func hwAccelNode(t *testing.T, info *playback.HWAccelInfo) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if info == nil || r.URL.Path != "/hw-capabilities" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
<<<<<<< HEAD
		_ = json.NewEncoder(w).Encode(playback.HWAccelInfo{
			Resolved:      "qsv",
			RenderDevices: []string{"/dev/dri/renderD128", "/dev/dri/renderD129"},
			RenderDeviceDetails: []playback.RenderDeviceInfo{
				{Path: "/dev/dri/renderD128", Description: "Intel GPU"},
				{Path: "/dev/dri/renderD129", Description: "Intel GPU"},
			},
		})
	}))
	defer nodeA.Close()
	nodeB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(playback.HWAccelInfo{
			Resolved:      "vaapi",
			RenderDevices: []string{"/dev/dri/renderD128"},
			RenderDeviceDetails: []playback.RenderDeviceInfo{
				{Path: "/dev/dri/renderD128", Description: "AMD GPU"},
			},
		})
	}))
	defer nodeB.Close()
	nodeDown := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer nodeDown.Close()

	pool := nodepool.NewTranscodePool()
	pool.SetNodes([]*nodepool.Node{
		{ID: 1, Name: "node-a", URL: nodeA.URL, Enabled: true, Healthy: true},
		{ID: 2, Name: "node-b", URL: nodeB.URL, Enabled: true, Healthy: true},
		{ID: 3, Name: "node-down", URL: nodeDown.URL, Enabled: true, Healthy: true},
		{ID: 4, Name: "node-unhealthy", URL: "http://unreachable.invalid", Enabled: true, Healthy: false},
	})
	handler := &SystemHandler{transcodePool: pool, jwtSecret: "secret"}

	req := httptest.NewRequest(http.MethodGet, "/admin/system/hw-accel", nil)
	rec := httptest.NewRecorder()
	handler.HandleHWAccel(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got HWAccelInventory
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	// Flat primary fields keep the historical single-probe shape, sourced
	// from the first healthy node that answered.
	if got.Resolved != "qsv" || got.Source != "transcode_node" || got.NodeURL != nodeA.URL {
		t.Fatalf("primary = resolved %q source %q node %q, want first node's probe", got.Resolved, got.Source, got.NodeURL)
	}

	// Every healthy node appears, in pool order; the unhealthy one does not.
	if len(got.Nodes) != 3 {
		t.Fatalf("nodes len = %d, want 3: %+v", len(got.Nodes), got.Nodes)
	}
	if got.Nodes[0].NodeName != "node-a" || got.Nodes[0].Resolved != "qsv" || len(got.Nodes[0].RenderDeviceDetails) != 2 {
		t.Fatalf("node-a entry = %+v", got.Nodes[0])
	}
	if got.Nodes[1].NodeName != "node-b" || got.Nodes[1].Resolved != "vaapi" || len(got.Nodes[1].RenderDevices) != 1 {
		t.Fatalf("node-b entry = %+v", got.Nodes[1])
	}
	if got.Nodes[2].NodeName != "node-down" || got.Nodes[2].Error == "" {
		t.Fatalf("failed node entry = %+v, want populated error", got.Nodes[2])
	}
=======
		_ = json.NewEncoder(w).Encode(info)
	}))
	t.Cleanup(server.Close)
	return server.URL
>>>>>>> upstream/main
}

// ReadHardwareAcceleration fans out to every enabled, healthy node in pool
// order. The flat primary fields come from the first node that answered, and
// only when no node answered does the local host's probe stand in.
func TestReadHardwareAccelerationAggregatesNodes(t *testing.T) {
	t.Parallel()

	qsvURL := hwAccelNode(t, &playback.HWAccelInfo{
		Resolved:            "qsv",
		RenderDevices:       []string{"/dev/dri/renderD128", "/dev/dri/renderD129"},
		RenderDeviceDetails: []playback.RenderDeviceInfo{{Path: "/dev/dri/renderD128"}, {Path: "/dev/dri/renderD129"}},
	})
	vaapiURL := hwAccelNode(t, &playback.HWAccelInfo{Resolved: "vaapi", RenderDevices: []string{"/dev/dri/renderD128"}})
	downURL := hwAccelNode(t, nil)

	cases := []struct {
		name        string
		nodes       []*nodepool.Node
		wantSource  string
		wantPrimary string
		wantNodes   []string // per node: resolved backend, or "error"
	}{
		{
			name: "first answering node is primary; failed node listed with its error",
			nodes: []*nodepool.Node{
				{ID: 1, Name: "down", URL: downURL, Enabled: true, Healthy: true},
				{ID: 2, Name: "a", URL: qsvURL, Enabled: true, Healthy: true},
				{ID: 3, Name: "b", URL: vaapiURL, Enabled: true, Healthy: true},
				{ID: 4, Name: "unhealthy", URL: "http://unreachable.invalid", Enabled: true, Healthy: false},
				{ID: 5, Name: "disabled", URL: "http://unreachable.invalid", Enabled: false, Healthy: true},
			},
			wantSource:  "transcode_node",
			wantPrimary: qsvURL,
			wantNodes:   []string{"error", "qsv", "vaapi"},
		},
		{
			name:       "every node fails, local probe stands in",
			nodes:      []*nodepool.Node{{ID: 1, Name: "down", URL: downURL, Enabled: true, Healthy: true}},
			wantSource: "local",
			wantNodes:  []string{"error"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool := nodepool.NewTranscodePool()
			pool.SetNodes(tc.nodes)
			handler := &SystemHandler{transcodePool: pool, jwtSecret: "secret"}

			got := handler.ReadHardwareAcceleration(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

			if got.Source != tc.wantSource || got.NodeURL != tc.wantPrimary {
				t.Fatalf("primary = source %q node %q, want %q %q", got.Source, got.NodeURL, tc.wantSource, tc.wantPrimary)
			}
			if len(got.Nodes) != len(tc.wantNodes) {
				t.Fatalf("nodes = %+v, want %d entries", got.Nodes, len(tc.wantNodes))
			}
			for i, want := range tc.wantNodes {
				entry := got.Nodes[i]
				if want == "error" {
					if entry.Error == "" || entry.Resolved != "" {
						t.Fatalf("node %d = %+v, want an error entry", i, entry)
					}
					continue
				}
				if entry.Error != "" || entry.Resolved != want {
					t.Fatalf("node %d = %+v, want resolved %q", i, entry, want)
				}
			}
			if tc.wantPrimary == qsvURL && len(got.Nodes[1].RenderDeviceDetails) != 2 {
				t.Fatalf("qsv node lost its device details: %+v", got.Nodes[1])
			}
		})
	}
}

// A failed node probe is logged by origin only: credentials, query and
// fragment in a stored node URL never reach the logs.
func TestReadHardwareAccelerationProbeLogRedactsNodeURLSecrets(t *testing.T) {
	const (
		username       = "probe-operator"
		password       = "node-password"
		querySecret    = "query-secret"
		fragmentSecret = "fragment-secret"
	)
	healthyURL := hwAccelNode(t, &playback.HWAccelInfo{Resolved: "qsv"})
	failed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	failed.Close()
	failedNodeURL := strings.Replace(failed.URL, "http://", "http://"+username+":"+password+"@", 1) +
		"?access_token=" + querySecret + "#" + fragmentSecret

	pool := nodepool.NewTranscodePool()
	pool.SetNodes([]*nodepool.Node{
		{ID: 1, Name: "healthy-node", URL: healthyURL, Enabled: true, Healthy: true},
		{ID: 2, Name: "failed-node", URL: failedNodeURL, Enabled: true, Healthy: true},
	})
	handler := &SystemHandler{transcodePool: pool, jwtSecret: "jwt-secret"}

	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	handler.ReadHardwareAcceleration(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	diagnostics := logs.String()
	for _, secret := range []string{username, password, querySecret, fragmentSecret} {
		if strings.Contains(diagnostics, secret) {
			t.Fatalf("capability probe log contains %q: %q", secret, diagnostics)
		}
	}
	if !strings.Contains(diagnostics, failed.URL) {
		t.Fatalf("capability probe log lost sanitized node origin: %q", diagnostics)
	}
}

// The local inventory runs a full hardware walk on the request goroutine, and
// that walk grows with the configured device set — eight Intel render devices
// draw five ffmpeg commands each, already past the API listener's 120-second
// write timeout. Without lifting the deadline the settings page loses its
// response while every probe is still inside its own bound.
func TestReadHardwareAccelerationExtendsTheWriteDeadlineForItsWalk(t *testing.T) {
	devices := make([]string, 0, 8)
	for i := range 8 {
		devices = append(devices, "/dev/dri/renderD"+strconv.Itoa(128+i))
	}
	hwDevice := strings.Join(devices, ",")
	handler := &SystemHandler{
		playback: func() (string, string, string) { return "/nonexistent/ffmpeg", "auto", hwDevice },
	}

	recorder := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	before := time.Now()
	handler.ReadHardwareAcceleration(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	// Sized from the walk's own budget, which depends on how the host's sysfs
	// classifies the devices, so this asserts the derivation, not a number.
	want := playback.HWAccelWalkTimeout(hwDevice) + hwAccelWriteSlack
	if reserved := recorder.deadline.Sub(before); reserved < want {
		t.Fatalf("reserved %s, want at least the walk's own budget plus slack (%s)", reserved, want)
	}
	if single := playback.HWAccelWalkTimeout(devices[0]); single >= playback.HWAccelWalkTimeout(hwDevice) {
		t.Fatalf("one device budgets %s and eight budget %s; the walk budget does not follow the device set",
			single, playback.HWAccelWalkTimeout(hwDevice))
	}
}
