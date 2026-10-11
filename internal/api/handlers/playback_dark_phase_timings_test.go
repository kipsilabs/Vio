package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/nodepool"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// darkPhaseRecordedV3 reports whether a phase key was touched on the recorder.
// The recorded value can legitimately round to zero milliseconds (a cache hit),
// so presence in the map, not the millisecond value, is the assertion that the
// phase was recorded at all.
func darkPhaseRecordedV3(t *testing.T, recorder *transportPhaseTimingsV3, name string) bool {
	t.Helper()
	if recorder == nil {
		t.Fatal("no recorder installed")
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	_, ok := recorder.phases[name]
	return ok
}

// TestTransportDarkPhasesAreEmitted pins that each phase added after the
// original split lands on the start timing line under its own name with the
// value it accumulated.
func TestTransportDarkPhasesAreEmitted(t *testing.T) {
	ctx, recorder := withTransportPhaseTimingsV3(context.Background())
	addTransportPhaseDurationV3(ctx, transportPhasePersistV3, 11*time.Millisecond)
	addTransportPhaseDurationV3(ctx, transportPhaseAuthorityV3, 22*time.Millisecond)
	addTransportPhaseDurationV3(ctx, transportPhaseCapabilityV3, 33*time.Millisecond)
	addTransportPhaseDurationV3(ctx, transportPhaseManifestV3, 44*time.Millisecond)

	timings := newPlaybackStartTimingsV3()
	timings.observeTransportPhasesV3(recorder)
	attrs := phaseAttrs(t, timings)

	want := map[string]int64{
		"transport_persist_ms":    11,
		"transport_authority_ms":  22,
		"transport_capability_ms": 33,
		"transport_manifest_ms":   44,
	}
	for name, value := range want {
		got, ok := attrs[name]
		if !ok {
			t.Fatalf("%s was not emitted; attrs=%v", name, attrs)
		}
		if got.(int64) != value {
			t.Fatalf("%s = %v, want %d", name, got, value)
		}
	}
}

// TestTransportDarkPhasesRequireARecorder pins that the new phases are nil-safe
// like the original set: recording on a bare context cannot panic and reads
// back as zero.
func TestTransportDarkPhasesRequireARecorder(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{transportPhasePersistV3, transportPhaseAuthorityV3, transportPhaseCapabilityV3, transportPhaseManifestV3} {
		recordTransportPhaseV3(ctx, name, time.Now().Add(-time.Second))
		addTransportPhaseDurationV3(ctx, name, time.Second)
		if got := transportPhaseDurationV3(ctx, name); got != 0 {
			t.Fatalf("%s without a recorder = %v, want 0", name, got)
		}
	}
}

// TestTransportDarkPhasesDoNotDisturbExistingPhases pins that recording any new
// phase leaves every original phase at exactly its recorded value, so the new
// spans cannot silently fold into the existing ones (no double-count).
func TestTransportDarkPhasesDoNotDisturbExistingPhases(t *testing.T) {
	ctx, recorder := withTransportPhaseTimingsV3(context.Background())

	existing := map[string]time.Duration{
		"transport_anchor":       5 * time.Millisecond,
		"transport_upstream":     6 * time.Millisecond,
		"transport_queue":        7 * time.Millisecond,
		"transport_route":        8 * time.Millisecond,
		"transport_spawn":        9 * time.Millisecond,
		"transport_readiness":    10 * time.Millisecond,
		"transport_remote_ready": 11 * time.Millisecond,
	}
	for name, d := range existing {
		addTransportPhaseDurationV3(ctx, name, d)
	}
	for _, name := range []string{transportPhasePersistV3, transportPhaseAuthorityV3, transportPhaseCapabilityV3, transportPhaseManifestV3} {
		addTransportPhaseDurationV3(ctx, name, 100*time.Millisecond)
	}

	timings := newPlaybackStartTimingsV3()
	timings.observeTransportPhasesV3(recorder)
	attrs := phaseAttrs(t, timings)

	for name, want := range existing {
		if got := attrs[name+"_ms"].(int64); got != want.Milliseconds() {
			t.Fatalf("%s = %d, want %d after recording the new phases", name, got, want.Milliseconds())
		}
	}
}

// TestTransportPhaseNettingKeepsRouteDisjointFromCapability pins the arithmetic
// that keeps the two phases disjoint when a capability probe runs inside the
// route resolver: route is recorded net of the capability delta it produced, so
// route+capability equals the enclosing wall time rather than double-counting
// the probe.
func TestTransportPhaseNettingKeepsRouteDisjointFromCapability(t *testing.T) {
	const capabilityWork = 50 * time.Millisecond
	ctx, _ := withTransportPhaseTimingsV3(context.Background())

	routeStart := time.Now()
	capabilityBefore := transportPhaseDurationV3(ctx, transportPhaseCapabilityV3)
	// Simulate the per-node capability probe the resolver performs inline: real
	// wall time charged to capability while the route span is open.
	time.Sleep(capabilityWork)
	recordTransportPhaseV3(ctx, transportPhaseCapabilityV3, routeStart)
	recordTransportPhaseNetOfV3(ctx, "transport_route", routeStart, transportPhaseCapabilityV3, capabilityBefore)
	wall := time.Since(routeStart)

	capability := transportPhaseDurationV3(ctx, transportPhaseCapabilityV3)
	route := transportPhaseDurationV3(ctx, "transport_route")
	if diff := (route + capability) - wall; diff < -20*time.Millisecond || diff > 20*time.Millisecond {
		t.Fatalf("route+capability = %v, want the enclosing wall %v (route=%v capability=%v)", route+capability, wall, route, capability)
	}
	if route >= capability {
		t.Fatalf("route = %v, want less than the capability probe %v it nets out", route, capability)
	}
}

// TestTransportPhaseNettingFloorsAtZero pins that a netting surprise (an inner
// delta larger than the outer span) floors the outer phase at zero instead of
// emitting a negative millisecond.
func TestTransportPhaseNettingFloorsAtZero(t *testing.T) {
	ctx, _ := withTransportPhaseTimingsV3(context.Background())
	start := time.Now()
	// Claim far more inner time than the outer span could have covered.
	addTransportPhaseDurationV3(ctx, transportPhaseCapabilityV3, 10*time.Second)
	recordTransportPhaseNetOfV3(ctx, "transport_route", start, transportPhaseCapabilityV3, 0)
	if got := transportPhaseDurationV3(ctx, "transport_route"); got != 0 {
		t.Fatalf("transport_route = %v, want 0 when the inner delta exceeds the span", got)
	}
}

// TestTransportPhaseTotalStaysSelfConsistent pins that the phase fields are
// bounded by the request's total_ms: the phases are disjoint spans of the same
// start, so their sum can never exceed the request wall time.
func TestTransportPhaseTotalStaysSelfConsistent(t *testing.T) {
	// Record each phase as a real, disjoint span of its own so the phase fields
	// model the production invariant: they partition work inside the request, so
	// their sum can never exceed the request wall time that total_ms reports.
	start := time.Now()
	ctx, recorder := withTransportPhaseTimingsV3(context.Background())
	for _, name := range transportPhaseNamesV3 {
		phaseStart := time.Now()
		time.Sleep(2 * time.Millisecond)
		recordTransportPhaseV3(ctx, name, phaseStart)
	}
	wall := time.Since(start)

	timings := newPlaybackStartTimingsV3()
	timings.observeTransportPhasesV3(recorder)
	attrs := phaseAttrs(t, timings)

	var sum int64
	for _, name := range transportPhaseNamesV3 {
		sum += attrs[name+"_ms"].(int64)
	}
	if total := wall.Milliseconds(); sum > total+2 {
		t.Fatalf("sum of phases = %d ms exceeds the request wall = %d ms", sum, total)
	}
	if sum == 0 {
		t.Fatal("no phase time was recorded")
	}
}

// TestLookupRemoteCapabilitiesRecordsCapabilityPhase pins that the remote
// capability read records the capability phase at its own definition, so every
// start-path call site is covered rather than a hand-picked subset. A fresh
// cached entry keeps the lookup off the network.
func TestLookupRemoteCapabilitiesRecordsCapabilityPhase(t *testing.T) {
	ctx, recorder := withTransportPhaseTimingsV3(context.Background())
	nodeURL := "http://cap.node.invalid:9999"
	h := &PlaybackHandler{}
	h.v3NodeCapabilities = map[string]v3NodeCapabilityCache{
		nodepool.NormalizeNodeURL(nodeURL): {expiresAt: time.Now().Add(time.Minute)},
	}

	if _, err := h.lookupRemoteCapabilitiesV3(ctx, nodeURL, false); err != nil {
		t.Fatalf("cached capability lookup failed: %v", err)
	}
	if !darkPhaseRecordedV3(t, recorder, transportPhaseCapabilityV3) {
		t.Fatal("transport_capability was not recorded by the capability lookup")
	}
}

// TestPutProxyGrantRecordsAuthorityPhase pins that the proxy-grant write, the
// identity and remote paths' authority write, records the authority phase. A
// disabled store returns before the write and must not be charged.
func TestPutProxyGrantRecordsAuthorityPhase(t *testing.T) {
	ctx, recorder := withTransportPhaseTimingsV3(context.Background())
	h := &PlaybackHandler{ProxyGrantStore: &recordingRecipeCardStoreV3{}}
	if _, stored := h.putProxyGrantV3(ctx, "session-1", playback.RecipeCard{SessionID: "session-1"}); !stored {
		t.Fatal("putProxyGrantV3 did not store")
	}
	if !darkPhaseRecordedV3(t, recorder, transportPhaseAuthorityV3) {
		t.Fatal("transport_authority was not recorded by a stored proxy grant")
	}

	disabledCtx, disabledRecorder := withTransportPhaseTimingsV3(context.Background())
	h.ProxyGrantStore = &recordingRecipeCardStoreV3{disabled: true}
	if _, stored := h.putProxyGrantV3(disabledCtx, "session-2", playback.RecipeCard{SessionID: "session-2"}); stored {
		t.Fatal("a disabled store reported a stored grant")
	}
	if darkPhaseRecordedV3(t, disabledRecorder, transportPhaseAuthorityV3) {
		t.Fatal("a disabled store was charged to the authority phase")
	}
}

// TestPutRequiredNodeRecipeRecordsAuthorityPhase pins the route-admission
// authority write on the same phase.
func TestPutRequiredNodeRecipeRecordsAuthorityPhase(t *testing.T) {
	ctx, recorder := withTransportPhaseTimingsV3(context.Background())
	h := &PlaybackHandler{NodeRecipeStore: &recordingRecipeCardStoreV3{}}
	if err := h.putRequiredNodeRecipeV3(ctx, "transport-1", playback.RecipeCard{SessionID: "s"}); err != nil {
		t.Fatalf("putRequiredNodeRecipeV3 failed: %v", err)
	}
	if !darkPhaseRecordedV3(t, recorder, transportPhaseAuthorityV3) {
		t.Fatal("transport_authority was not recorded by a stored node recipe")
	}
}
