package apiv2

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/playback"
)

// TestPlaybackDecisionV2ProjectsDeferredTrackInventory locks the additive v2
// wire contract for the deferred track-inventory lifecycle: the plan carries
// tracks_pending and inventory_url, both mapped from PlanV3, and both are
// omitted on a fully probed plan so a client never sees a provisional marker it
// did not negotiate. A v1-prefixed inventory URL is normalized into the v2
// namespace so the field can never leak a frozen-surface path.
func TestPlaybackDecisionV2ProjectsDeferredTrackInventory(t *testing.T) {
	in := playback.DecisionResponseV3{PlaybackPlan: &playback.PlanV3{
		TracksPending: true,
		InventoryURL:  "/api/v1/playback/session-1/inventory",
	}}
	out := playbackDecision(in)
	if out.PlaybackPlan == nil {
		t.Fatal("projection dropped the plan")
	}
	if !out.PlaybackPlan.TracksPending {
		t.Fatal("projection dropped tracks_pending")
	}
	if out.PlaybackPlan.InventoryURL != Prefix+"/playback/session-1/inventory" {
		t.Fatalf("inventory_url = %q, want the v2 namespace", out.PlaybackPlan.InventoryURL)
	}
	encoded, err := json.Marshal(out.PlaybackPlan)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"tracks_pending":true`) || !strings.Contains(string(encoded), `"inventory_url":"`+Prefix+`/playback/session-1/inventory"`) {
		t.Fatalf("deferred plan did not serialize both fields: %s", encoded)
	}

	// A fully probed plan omits both fields.
	empty := playbackDecision(playback.DecisionResponseV3{PlaybackPlan: &playback.PlanV3{}})
	if empty.PlaybackPlan.TracksPending || empty.PlaybackPlan.InventoryURL != "" {
		t.Fatalf("a fully probed plan projected deferred fields: %+v", empty.PlaybackPlan)
	}
	encodedEmpty, err := json.Marshal(empty.PlaybackPlan)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encodedEmpty), "tracks_pending") || strings.Contains(string(encodedEmpty), "inventory_url") {
		t.Fatalf("a fully probed plan serialized deferred fields: %s", encodedEmpty)
	}
}

// TestPlaybackV2DeferredStartAndInventoryPoll is the end-to-end v2 proof: a
// negotiated deferred start returns tracks_pending and inventory_url, and the
// inventory endpoint reports pending while the enumeration is outstanding and
// the terminal status (failed) once it lands. A client watching either the push
// or the poll leaves its loading state.
func TestPlaybackV2DeferredStartAndInventoryPoll(t *testing.T) {
	deps, _ := catalogDeps(t)
	fake := &fakePlaybackService{}
	deps.Playback = fake
	h := newTestHandler(t, deps)

	// The deferred start response carries the provisional plan. The response is
	// built by the service seam, so the fake returns a plan as the shared
	// application would.
	fake.response = playback.DecisionResponseV3{
		ProtocolVersion: playback.ProtocolV3,
		ServerFeatures:  playback.NativeServerFeaturesV3(),
		Outcome:         playback.OutcomePlayableV3,
		SessionID:       "11111111-1111-4111-8111-111111111111",
		PlaybackPlan: &playback.PlanV3{
			ProtocolVersion: playback.ProtocolV3,
			PlanID:          "plan:deferred",
			SessionID:       "11111111-1111-4111-8111-111111111111",
			TracksPending:   true,
			InventoryURL:    "/api/v2/playback/11111111-1111-4111-8111-111111111111/inventory",
		},
	}
	body := playbackStartFixture(t)
	body["client_features"] = []string{playback.FeatureDeferredTrackInventoryV3}
	started := do(t, h, http.MethodPost, Prefix+"/playback/start", playbackJSON(t, body), viewerHeaders())
	if started.Code != http.StatusCreated {
		t.Fatalf("deferred start: %d %s", started.Code, started.Body.String())
	}
	var startBody struct {
		Plan struct {
			TracksPending bool   `json:"tracks_pending"`
			InventoryURL  string `json:"inventory_url"`
		} `json:"playback_plan"`
	}
	if err := json.Unmarshal(started.Body.Bytes(), &startBody); err != nil {
		t.Fatalf("decode start: %v %s", err, started.Body.String())
	}
	if !startBody.Plan.TracksPending || startBody.Plan.InventoryURL != Prefix+"/playback/11111111-1111-4111-8111-111111111111/inventory" {
		t.Fatalf("deferred plan on the wire = %+v", startBody.Plan)
	}

	// The poll reports pending while the enumeration is outstanding.
	fake.inventoryStatus = "pending"
	pending := do(t, h, http.MethodGet, Prefix+"/playback/11111111-1111-4111-8111-111111111111/inventory", "", viewerHeaders())
	if pending.Code != http.StatusOK || !strings.Contains(pending.Body.String(), `"inventory_status":"pending"`) {
		t.Fatalf("pending poll: %d %s", pending.Code, pending.Body.String())
	}

	// The terminal failure is observable on the same endpoint; the revision
	// changed, so a client gating on ETag re-reads instead of serving its stale
	// pending copy.
	fake.inventoryStatus = "failed"
	terminal := do(t, h, http.MethodGet, Prefix+"/playback/11111111-1111-4111-8111-111111111111/inventory", "", viewerHeaders())
	if terminal.Code != http.StatusOK || !strings.Contains(terminal.Body.String(), `"inventory_status":"failed"`) {
		t.Fatalf("terminal poll: %d %s", terminal.Code, terminal.Body.String())
	}
}
