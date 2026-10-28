package playback

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

// subtitleReuseLogCapture installs a JSON logger over the default handler for
// one test, so the out-of-range warning emitted inside the subtitle policy is
// observable.
func subtitleReuseLogCapture(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs
}

// subtitleReuseFixtureFile is an SDR H.264 source whose subtitle tracks are
// distinct physical tracks (each carries its own stream index, so the inventory
// de-duplication keeps them all). withTracks controls how many the inventory
// publishes.
func subtitleReuseFixtureFile(t *testing.T, withTracks int) *models.MediaFile {
	t.Helper()
	file := detailedFixtureFileV3()
	file.VideoTracks[0].VideoRange = "SDR"
	file.VideoTracks[0].VideoRangeType = "SDR"
	file.VideoTracks[0].ColorTransfer = "bt709"
	file.ExternalSubtitles = nil
	file.SubtitleTracks = make([]models.SubtitleTrack, 0, withTracks)
	for index := 0; index < withTracks; index++ {
		file.SubtitleTracks = append(file.SubtitleTracks, models.SubtitleTrack{Codec: "subrip", Index: index + 1, Language: "eng"})
	}
	return file
}

func subtitleReusePlannerInput(file *models.MediaFile, subtitleIndex int) PlannerInputV3 {
	req := validStartRequestV3()
	req.Capabilities.VideoDecode = []VideoDecodeCapabilityV3{{
		Codec: "hevc", Profiles: []string{"main 10"}, Levels: []int{153},
		BitDepths: []int{10}, MaxWidth: 3840, MaxHeight: 2160, MaxFrameRate: 60,
		MaxBitrateKbps: 80_000, Hardware: true,
	}}
	req.SubtitleTrackIndex = &subtitleIndex
	req.SubtitleTrackID = TrackIDV3(file.ID, "subtitle", subtitleIndex)
	return PlannerInputV3{
		Request: req, RequestedFile: file, EffectiveFile: file, AudioTrackIndex: 0,
		Settings: PlannerSettingsV3{TranscodeEnabled: false}, Registry: testTransformationRegistryV3(),
	}
}

// A replan that re-sends the identical out-of-range subtitle pick against the
// identical effective inventory must republish the predecessor's subtitles-off
// outcome instead of running (and logging) the disable again. This is the prod
// replan storm: dozens of identical retries, each re-logging the same warning.
func TestPlanPlaybackV3ReusesUnchangedSubtitleDegradeWithoutRelogging(t *testing.T) {
	logs := subtitleReuseLogCapture(t)
	file := subtitleReuseFixtureFile(t, 3)
	input := subtitleReusePlannerInput(file, 20)

	first := PlanPlaybackV3(input)
	if first.Plan == nil {
		t.Fatalf("first plan failed: %s", ExplainPlannerResultV3(first))
	}
	if first.Plan.Subtitle.Mode != SubtitleOffV3 || first.Plan.SelectedTracks.Subtitle != nil {
		t.Fatalf("first plan = %#v, want the out-of-range selection degraded to subtitles off", first.Plan.Subtitle)
	}
	if !planCarriesSubtitleUnavailableWarningV3(first.Plan) {
		t.Fatal("first plan must report the dropped subtitle")
	}
	if got := strings.Count(logs.String(), "subtitle selection out of range"); got != 1 {
		t.Fatalf("first evaluation logged the disable %d times, want 1:\n%s", got, logs.String())
	}

	previous := PreviousSubtitleEvaluationV3(input.Request, *first.Plan)
	if previous == nil || !previous.Disabled {
		t.Fatalf("previous evaluation = %#v, want the degraded outcome recorded", previous)
	}
	input.PreviousSubtitleEvaluation = previous
	before := strings.Count(logs.String(), "subtitle selection out of range")

	second := PlanPlaybackV3(input)
	if second.Plan == nil {
		t.Fatalf("second plan failed: %s", ExplainPlannerResultV3(second))
	}
	if second.Plan.Subtitle.Mode != SubtitleOffV3 || second.Plan.SelectedTracks.Subtitle != nil {
		t.Fatalf("second plan = %#v, want the previous subtitles-off outcome kept", second.Plan.Subtitle)
	}
	if !planCarriesSubtitleUnavailableWarningV3(second.Plan) {
		t.Fatal("the reused outcome must keep the degradation warning on the new plan")
	}
	if after := strings.Count(logs.String(), "subtitle selection out of range"); after != before {
		t.Fatalf("an unchanged pick re-ran the out-of-range disable (%d -> %d):\n%s", before, after, logs.String())
	}
}

// A changed effective inventory is new evidence: the same pick must be
// re-evaluated rather than reused, both when it now resolves and when it is
// still out of range.
func TestPlanPlaybackV3ReevaluatesSubtitleDegradeWhenInventoryChanges(t *testing.T) {
	logs := subtitleReuseLogCapture(t)
	file := subtitleReuseFixtureFile(t, 3)
	input := subtitleReusePlannerInput(file, 20)

	first := PlanPlaybackV3(input)
	if first.Plan == nil {
		t.Fatalf("first plan failed: %s", ExplainPlannerResultV3(first))
	}
	previous := PreviousSubtitleEvaluationV3(input.Request, *first.Plan)
	if previous == nil {
		t.Fatal("expected the degraded outcome to be reusable evidence")
	}

	// The inventory grows past the selected ordinal: still a different
	// inventory, and now the pick resolves.
	file.SubtitleTracks = subtitleReuseFixtureFile(t, 21).SubtitleTracks
	input.PreviousSubtitleEvaluation = previous
	resolved := PlanPlaybackV3(input)
	if resolved.Plan == nil {
		t.Fatalf("plan with the grown inventory failed: %s", ExplainPlannerResultV3(resolved))
	}
	if resolved.Plan.Subtitle.Mode == SubtitleOffV3 || resolved.Plan.SelectedTracks.Subtitle == nil {
		t.Fatalf("plan = %#v, want the selection resolved by the grown inventory", resolved.Plan.Subtitle)
	}
	if resolved.Plan.SelectedTracks.Subtitle.Index == nil || *resolved.Plan.SelectedTracks.Subtitle.Index != 20 {
		t.Fatalf("selected subtitle = %#v, want ordinal 20", resolved.Plan.SelectedTracks.Subtitle)
	}

	// A smaller inventory that still lacks the ordinal must re-run the disable
	// and log it again: the previous digest no longer describes this inventory.
	file.SubtitleTracks = subtitleReuseFixtureFile(t, 4).SubtitleTracks
	input.PreviousSubtitleEvaluation = previous
	before := strings.Count(logs.String(), "subtitle selection out of range")
	stillOutOfRange := PlanPlaybackV3(input)
	if stillOutOfRange.Plan == nil {
		t.Fatalf("plan with the changed inventory failed: %s", ExplainPlannerResultV3(stillOutOfRange))
	}
	if stillOutOfRange.Plan.Subtitle.Mode != SubtitleOffV3 {
		t.Fatalf("plan = %#v, want subtitles off", stillOutOfRange.Plan.Subtitle)
	}
	if after := strings.Count(logs.String(), "subtitle selection out of range"); after != before+1 {
		t.Fatalf("changed inventory did not re-evaluate the pick (%d -> %d):\n%s", before, after, logs.String())
	}
}

func planCarriesSubtitleUnavailableWarningV3(plan *PlanV3) bool {
	for _, warning := range plan.DegradationWarnings {
		if warning.Code == DegradationWarningSubtitleTrackUnavailableV3 {
			return true
		}
	}
	return false
}
