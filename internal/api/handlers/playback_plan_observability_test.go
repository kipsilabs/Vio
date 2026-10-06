package handlers

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/playback"
)

// TestSelectedAudioTrackLogFieldsV3 pins the audio fields the plan-decision
// line shows: the selection ordinal and the track's language and codec from the
// plan's authoritative inventory. A missing or out-of-range inventory reads as
// empty strings rather than a bogus language.
func TestSelectedAudioTrackLogFieldsV3(t *testing.T) {
	ordinal := 1
	plan := &playback.PlanV3{
		SelectedTracks: playback.SelectedTracksV3{Audio: &playback.TrackIdentityV3{Index: &ordinal}},
		AudioTracks: []playback.AudioInventoryItemV3{
			{Language: "eng", Codec: "aac"},
			{Language: "spa", Codec: "eac3"},
		},
	}
	language, codec, got := selectedAudioTrackLogFieldsV3(playback.PlannerResultV3{Plan: plan}, 0)
	if language != "spa" || codec != "eac3" || got != 1 {
		t.Fatalf("fields = %q/%q/%d, want spa/eac3/1", language, codec, got)
	}

	// A multi-language inventory entry falls back to its language list when the
	// single language is empty.
	multi := &playback.PlanV3{
		SelectedTracks: playback.SelectedTracksV3{Audio: &playback.TrackIdentityV3{Index: &ordinal}},
		AudioTracks: []playback.AudioInventoryItemV3{
			{Languages: []string{"eng", "fra"}, Codec: "aac"},
			{Language: "spa", Codec: "eac3"},
		},
	}
	language, codec, got = selectedAudioTrackLogFieldsV3(playback.PlannerResultV3{Plan: multi}, 1)
	if language != "spa" || codec != "eac3" || got != 1 {
		t.Fatalf("fields = %q/%q/%d, want spa/eac3/1", language, codec, got)
	}

	// No inventory: the requested ordinal is echoed and nothing is invented.
	empty := &playback.PlanV3{}
	language, codec, got = selectedAudioTrackLogFieldsV3(playback.PlannerResultV3{Plan: empty}, 4)
	if language != "" || codec != "" || got != 4 {
		t.Fatalf("empty inventory fields = %q/%q/%d, want empty/empty/4", language, codec, got)
	}

	language, codec, got = selectedAudioTrackLogFieldsV3(playback.PlannerResultV3{}, 3)
	if language != "" || codec != "" || got != 3 {
		t.Fatalf("nil plan fields = %q/%q/%d, want empty/empty/3", language, codec, got)
	}
}

func attrsToMap(attrs []any) map[string]any {
	out := make(map[string]any, len(attrs)/2)
	for i := 0; i+1 < len(attrs); i += 2 {
		key, ok := attrs[i].(string)
		if !ok {
			continue
		}
		out[key] = attrs[i+1]
	}
	return out
}

// TestVirtualPlanDecisionAttrsV3 pins the virtual part of the plan-decision
// line: the selected candidate, its rank and the considered count, the audio
// track the plan will play, and the resolved playback.audio_language. A
// non-virtual plan emits nothing, an unranked resolve omits count and rank,
// and a measured zero prints the count without a rank — each distinct from
// the others rather than a misleading zero.
func TestVirtualPlanDecisionAttrsV3(t *testing.T) {
	ordinal := 0
	virtualPlan := &playback.PlanV3{
		EffectiveVirtualURI: "virtual://movie/tt1?result=cand-a",
		SelectedTracks:      playback.SelectedTracksV3{Audio: &playback.TrackIdentityV3{Index: &ordinal}},
		AudioTracks:         []playback.AudioInventoryItemV3{{Language: "eng", Codec: "aac"}},
	}

	attrs := attrsToMap(virtualPlanDecisionAttrsV3(playback.PlannerResultV3{Plan: virtualPlan}, 0, virtualPlanDecisionV3{
		preferredAudioLanguage: "en",
		candidateRank:          2,
		candidateCount:         34,
	}))
	for key, want := range map[string]any{
		"candidate_uri":            "virtual://movie/tt1?result=cand-a",
		"candidate_count":          34,
		"candidate_rank":           2,
		"preferred_audio_language": "en",
		"audio_track_language":     "eng",
		"audio_track_codec":        "aac",
		"audio_track_index":        0,
	} {
		if attrs[key] != want {
			t.Errorf("%s = %v, want %v", key, attrs[key], want)
		}
	}

	// A non-virtual plan carries no virtual attributes at all.
	localPlan := &playback.PlanV3{}
	if attrs := virtualPlanDecisionAttrsV3(playback.PlannerResultV3{Plan: localPlan}, 0, virtualPlanDecisionV3{candidateCount: 34}); len(attrs) != 0 {
		t.Fatalf("local plan emitted virtual attrs: %#v", attrs)
	}

	// An unranked resolve (no counted list) omits candidate_rank and
	// candidate_count, and an absent preference renders as an empty string
	// rather than noise.
	unranked := attrsToMap(virtualPlanDecisionAttrsV3(playback.PlannerResultV3{Plan: virtualPlan}, 0, virtualPlanDecisionV3{candidateCount: -1}))
	if _, present := unranked["candidate_rank"]; present {
		t.Fatalf("unranked resolve printed candidate_rank: %#v", unranked)
	}
	if _, present := unranked["candidate_count"]; present {
		t.Fatalf("unranked resolve printed candidate_count: %#v", unranked)
	}
	if unranked["preferred_audio_language"] != "" {
		t.Fatalf("preferred_audio_language = %v, want empty", unranked["preferred_audio_language"])
	}

	// A measured zero from an empty ranked list prints the count without a
	// rank, so it stays distinguishable from an unranked path that never
	// counted (no count, no rank).
	zero := attrsToMap(virtualPlanDecisionAttrsV3(playback.PlannerResultV3{Plan: virtualPlan}, 0, virtualPlanDecisionV3{}))
	if zero["candidate_count"] != 0 {
		t.Fatalf("zero-list candidate_count = %v, want 0", zero["candidate_count"])
	}
	if _, present := zero["candidate_rank"]; present {
		t.Fatalf("zero-list printed candidate_rank: %#v", zero)
	}
}
