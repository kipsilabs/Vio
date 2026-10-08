package apiv2

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/playback"
)

// TestPlaybackDecisionV2ProjectsDeliveryChange locks the additive v2 wire
// contract for the mid-session delivery-swap marker (issue #244 item 3): the
// plan's delivery_change is projected from PlanV3 into the apiv2-owned
// DeliveryChange DTO, carrying the old and new delivery/play-method tokens, and
// is omitted entirely on a plan with no swap so a client never sees a spurious
// route change. The DTO projection is what keeps the native schema owned by
// apiv2 rather than the shared domain type.
func TestPlaybackDecisionV2ProjectsDeliveryChange(t *testing.T) {
	in := playback.DecisionResponseV3{PlaybackPlan: &playback.PlanV3{
		Delivery: playback.DeliveryRemuxProgressiveV3,
		DeliveryChange: &playback.DeliveryChangeV3{
			PreviousDelivery:   playback.DeliveryTranscodeHLSV3,
			Delivery:           playback.DeliveryRemuxProgressiveV3,
			PreviousPlayMethod: playback.PlayTranscode,
			PlayMethod:         playback.PlayRemux,
			DeliveryChanged:    true,
			PlayMethodChanged:  true,
		},
	}}
	out := playbackDecision(in)
	if out.PlaybackPlan == nil || out.PlaybackPlan.DeliveryChange == nil {
		t.Fatal("projection dropped the delivery change")
	}
	want := DeliveryChange{
		PreviousDelivery:   playback.DeliveryTranscodeHLSV3,
		Delivery:           playback.DeliveryRemuxProgressiveV3,
		PreviousPlayMethod: playback.PlayTranscode,
		PlayMethod:         playback.PlayRemux,
		DeliveryChanged:    true,
		PlayMethodChanged:  true,
	}
	if *out.PlaybackPlan.DeliveryChange != want {
		t.Fatalf("delivery change = %#v, want %#v", out.PlaybackPlan.DeliveryChange, want)
	}
	encoded, err := json.Marshal(out.PlaybackPlan)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{
		`"delivery_change":{`,
		`"previous_delivery":"server_transcode_hls"`,
		`"delivery":"server_remux_progressive"`,
		`"previous_play_method":"transcode"`,
		`"play_method":"remux"`,
		`"delivery_changed":true`,
		`"play_method_changed":true`,
	} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("delivery change did not serialize %s: %s", want, encoded)
		}
	}

	// A plan that did not swap routes omits the marker entirely.
	empty := playbackDecision(playback.DecisionResponseV3{PlaybackPlan: &playback.PlanV3{Delivery: playback.DeliveryRemuxProgressiveV3}})
	if empty.PlaybackPlan.DeliveryChange != nil {
		t.Fatalf("unchanged plan projected a delivery change: %#v", empty.PlaybackPlan.DeliveryChange)
	}
	encodedEmpty, err := json.Marshal(empty.PlaybackPlan)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encodedEmpty), "delivery_change") {
		t.Fatalf("unchanged plan serialized delivery_change: %s", encodedEmpty)
	}
}
