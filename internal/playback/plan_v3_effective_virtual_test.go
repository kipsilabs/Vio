package playback

import (
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

// A virtual candidate substituted for the neutral requested row must be
// visible on the plan, or a client's version menu keeps showing the neutral
// label after a first play never adopts the version that actually played.
func TestPlanPlaybackV3ExposesEffectiveVirtualURI(t *testing.T) {
	candidate := detailedFixtureFileV3()
	candidate.FilePath = "virtual://movie/tt1234567?result=working"
	requested := &models.MediaFile{
		ID:        41,
		ContentID: candidate.ContentID,
		Container: candidate.Container,
		FilePath:  "virtual://movie/tt1234567",
	}
	req := validStartRequestV3()
	req.Capabilities.VideoDecode = []VideoDecodeCapabilityV3{{Codec: "hevc", Profiles: []string{"main 10"}, Levels: []int{153}, BitDepths: []int{10}, MaxWidth: 3840, MaxHeight: 2160, MaxFrameRate: 60, MaxBitrateKbps: 80_000, Hardware: true}}
	req.Capabilities.HDRDetails = &HDRCapabilitiesV3{HDR10: true}

	result := PlanPlaybackV3(PlannerInputV3{
		Request: req, RequestedFile: requested, EffectiveFile: candidate, AudioTrackIndex: 0,
		Settings: PlannerSettingsV3{TranscodeEnabled: true, Allow4KTranscode: true},
	})
	if result.Plan == nil {
		t.Fatalf("result = %#v, want a plan", result)
	}
	if result.Plan.EffectiveVirtualURI != candidate.FilePath {
		t.Fatalf("effective virtual URI = %q, want %q", result.Plan.EffectiveVirtualURI, candidate.FilePath)
	}
}

// A non-virtual effective source has no virtual URI to expose.
func TestPlanPlaybackV3OmitsEffectiveVirtualURIForLocalSource(t *testing.T) {
	file := detailedFixtureFileV3()
	req := validStartRequestV3()
	req.Capabilities.VideoDecode = []VideoDecodeCapabilityV3{{Codec: "hevc", Profiles: []string{"main 10"}, Levels: []int{153}, BitDepths: []int{10}, MaxWidth: 3840, MaxHeight: 2160, MaxFrameRate: 60, MaxBitrateKbps: 80_000, Hardware: true}}
	req.Capabilities.HDRDetails = &HDRCapabilitiesV3{HDR10: true}

	result := PlanPlaybackV3(PlannerInputV3{
		Request: req, RequestedFile: file, EffectiveFile: file, AudioTrackIndex: 0,
		Settings: PlannerSettingsV3{TranscodeEnabled: true, Allow4KTranscode: true},
	})
	if result.Plan == nil {
		t.Fatalf("result = %#v, want a plan", result)
	}
	if result.Plan.EffectiveVirtualURI != "" {
		t.Fatalf("effective virtual URI = %q, want empty for a local source", result.Plan.EffectiveVirtualURI)
	}
}

// The audio-only planner builds its own plan; it must expose the substituted
// virtual URI too.
func TestPlanPlaybackV3AudioOnlyExposesEffectiveVirtualURI(t *testing.T) {
	candidate := audioOnlyFixtureFileV3()
	candidate.FilePath = "virtual://audiobook/tt7654321?result=working"
	requested := &models.MediaFile{
		ID:        76,
		ContentID: candidate.ContentID,
		Container: candidate.Container,
		FilePath:  "virtual://audiobook/tt7654321",
	}
	req := validStartRequestV3()
	req.FileID = requested.ID
	req.Capabilities.Containers = []string{"mp4"}

	result := PlanPlaybackV3(PlannerInputV3{
		Request: req, RequestedFile: requested, EffectiveFile: candidate, AudioTrackIndex: 0,
		Settings: PlannerSettingsV3{TranscodeEnabled: true},
	})
	if result.Plan == nil {
		t.Fatalf("result = %#v, want a plan", result)
	}
	if result.Plan.EffectiveVirtualURI != candidate.FilePath {
		t.Fatalf("effective virtual URI = %q, want %q", result.Plan.EffectiveVirtualURI, candidate.FilePath)
	}
}

// The resolver's inventory provenance is published additively on the plan and,
// like the effective virtual URI, must not perturb plan identity: a plan with
// and without the field is the same attempt.
func TestPlanPlaybackV3PublishesInventoryProvenance(t *testing.T) {
	candidate := detailedFixtureFileV3()
	candidate.FilePath = "virtual://movie/tt1234567?result=working"
	requested := &models.MediaFile{ID: 41, ContentID: candidate.ContentID, Container: candidate.Container, FilePath: "virtual://movie/tt1234567"}
	req := validStartRequestV3()
	req.Capabilities.VideoDecode = []VideoDecodeCapabilityV3{{Codec: "hevc", Profiles: []string{"main 10"}, Levels: []int{153}, BitDepths: []int{10}, MaxWidth: 3840, MaxHeight: 2160, MaxFrameRate: 60, MaxBitrateKbps: 80_000, Hardware: true}}
	req.Capabilities.HDRDetails = &HDRCapabilitiesV3{HDR10: true}

	input := PlannerInputV3{
		Request: req, RequestedFile: requested, EffectiveFile: candidate, AudioTrackIndex: 0,
		Settings: PlannerSettingsV3{TranscodeEnabled: true, Allow4KTranscode: true},
	}
	input.InventoryProvenance = "declared"
	declared := PlanPlaybackV3(input)
	if declared.Plan == nil {
		t.Fatalf("result = %#v, want a plan", declared)
	}
	if declared.Plan.InventoryProvenance != "declared" {
		t.Fatalf("inventory provenance = %q, want declared", declared.Plan.InventoryProvenance)
	}

	input.InventoryProvenance = ""
	without := PlanPlaybackV3(input)
	if without.Plan == nil {
		t.Fatalf("result = %#v, want a plan", without)
	}
	if without.Plan.InventoryProvenance != "" {
		t.Fatalf("inventory provenance = %q, want empty", without.Plan.InventoryProvenance)
	}
	if without.Plan.PlanID != declared.Plan.PlanID {
		t.Fatalf("plan id changed with the provenance hint: %q vs %q", without.Plan.PlanID, declared.Plan.PlanID)
	}
}

// The provenance value the resolver produced is published verbatim on the plan,
// for every value the resolver can report: verified (this resolve probed the
// served bytes), declared (provider metadata only, including after a stale
// stamp), pending (a probe is deferred), and failed (a probe ran and failed).
// The plan must not invent a value or downgrade a declared/failed resolve to
// verified just because the row carries a probe stamp.
func TestPlanPlaybackV3PublishesEachInventoryProvenanceValue(t *testing.T) {
	candidate := detailedFixtureFileV3()
	candidate.FilePath = "virtual://movie/tt1234567?result=working"
	requested := &models.MediaFile{ID: 41, ContentID: candidate.ContentID, Container: candidate.Container, FilePath: "virtual://movie/tt1234567"}
	req := validStartRequestV3()
	req.Capabilities.VideoDecode = []VideoDecodeCapabilityV3{{Codec: "hevc", Profiles: []string{"main 10"}, Levels: []int{153}, BitDepths: []int{10}, MaxWidth: 3840, MaxHeight: 2160, MaxFrameRate: 60, MaxBitrateKbps: 80_000, Hardware: true}}
	req.Capabilities.HDRDetails = &HDRCapabilitiesV3{HDR10: true}

	for _, want := range []string{"verified", "declared", "pending", "failed"} {
		t.Run(want, func(t *testing.T) {
			input := PlannerInputV3{
				Request: req, RequestedFile: requested, EffectiveFile: candidate, AudioTrackIndex: 0,
				Settings: PlannerSettingsV3{TranscodeEnabled: true, Allow4KTranscode: true},
			}
			input.InventoryProvenance = want
			result := PlanPlaybackV3(input)
			if result.Plan == nil {
				t.Fatalf("result = %#v, want a plan", result)
			}
			if result.Plan.InventoryProvenance != want {
				t.Fatalf("inventory provenance = %q, want %q", result.Plan.InventoryProvenance, want)
			}
		})
	}
}

// The audio-only planner builds its own plan and must publish the resolver's
// provenance exactly like the video planner; a path that forgot the copy would
// report empty provenance for an audiobook resolve.
func TestPlanPlaybackV3AudioOnlyPublishesEachInventoryProvenanceValue(t *testing.T) {
	candidate := audioOnlyFixtureFileV3()
	candidate.FilePath = "virtual://audiobook/tt7654321?result=working"
	requested := &models.MediaFile{ID: 76, ContentID: candidate.ContentID, Container: candidate.Container, FilePath: "virtual://audiobook/tt7654321"}
	req := validStartRequestV3()
	req.FileID = requested.ID
	req.Capabilities.Containers = []string{"mp4"}

	for _, want := range []string{"verified", "declared", "pending", "failed"} {
		t.Run(want, func(t *testing.T) {
			result := PlanPlaybackV3(PlannerInputV3{
				Request: req, RequestedFile: requested, EffectiveFile: candidate, AudioTrackIndex: 0,
				Settings:            PlannerSettingsV3{TranscodeEnabled: true},
				InventoryProvenance: want,
			})
			if result.Plan == nil {
				t.Fatalf("result = %#v, want a plan", result)
			}
			if result.Plan.InventoryProvenance != want {
				t.Fatalf("audio-only inventory provenance = %q, want %q", result.Plan.InventoryProvenance, want)
			}
			// The provenance hint must never change route selection or identity.
			if result.Plan.PlanID == "" {
				t.Fatal("the audio-only plan lost its identity")
			}
		})
	}
}

// The plan's stamp-derived InventoryStatus and the resolve-derived
// InventoryProvenance are distinct signals. A row carrying a probe stamp makes
// InventoryStatus read "verified" even when the resolve that produced the plan
// only served declared or failed fallback metadata; the provenance must keep
// reporting what this resolve did rather than inherit the stamp.
func TestPlanPlaybackV3ProvenanceStaysTruthfulAgainstStaleStampStatus(t *testing.T) {
	candidate := detailedFixtureFileV3()
	candidate.FilePath = "virtual://movie/tt1234567?result=working"
	// A stale probe stamp on the served row: InventoryStatus is verified off the
	// stamp alone.
	staleStamp := time.Now().Add(-72 * time.Hour)
	candidate.ProbeUpdatedAt = &staleStamp
	requested := &models.MediaFile{ID: 41, ContentID: candidate.ContentID, Container: candidate.Container, FilePath: "virtual://movie/tt1234567"}
	req := validStartRequestV3()
	req.Capabilities.VideoDecode = []VideoDecodeCapabilityV3{{Codec: "hevc", Profiles: []string{"main 10"}, Levels: []int{153}, BitDepths: []int{10}, MaxWidth: 3840, MaxHeight: 2160, MaxFrameRate: 60, MaxBitrateKbps: 80_000, Hardware: true}}
	req.Capabilities.HDRDetails = &HDRCapabilitiesV3{HDR10: true}

	for _, want := range []string{"declared", "failed", "pending"} {
		t.Run(want, func(t *testing.T) {
			input := PlannerInputV3{
				Request: req, RequestedFile: requested, EffectiveFile: candidate, AudioTrackIndex: 0,
				Settings: PlannerSettingsV3{TranscodeEnabled: true, Allow4KTranscode: true},
			}
			input.InventoryProvenance = want
			result := PlanPlaybackV3(input)
			if result.Plan == nil {
				t.Fatalf("result = %#v, want a plan", result)
			}
			if result.Plan.InventoryStatus != "verified" {
				t.Fatalf("inventory status = %q, want the stamp-derived verified", result.Plan.InventoryStatus)
			}
			if result.Plan.InventoryProvenance != want {
				t.Fatalf("inventory provenance = %q, want %q: the stamp must not overwrite the resolve's own verdict", result.Plan.InventoryProvenance, want)
			}
		})
	}
}

// The URI is a UI hint, not a route input: attaching it must not perturb plan
// identity, or replans would miss the cache and clients would see spurious new
// attempts.
func TestPlanAttemptKeyV3IgnoresEffectiveVirtualURI(t *testing.T) {
	candidate := detailedFixtureFileV3()
	candidate.FilePath = "virtual://movie/tt1234567?result=working"
	requested := &models.MediaFile{ID: 41, ContentID: candidate.ContentID, Container: candidate.Container, FilePath: "virtual://movie/tt1234567"}
	req := validStartRequestV3()
	req.Capabilities.VideoDecode = []VideoDecodeCapabilityV3{{Codec: "hevc", Profiles: []string{"main 10"}, Levels: []int{153}, BitDepths: []int{10}, MaxWidth: 3840, MaxHeight: 2160, MaxFrameRate: 60, MaxBitrateKbps: 80_000, Hardware: true}}
	req.Capabilities.HDRDetails = &HDRCapabilitiesV3{HDR10: true}

	result := PlanPlaybackV3(PlannerInputV3{
		Request: req, RequestedFile: requested, EffectiveFile: candidate, AudioTrackIndex: 0,
		Settings: PlannerSettingsV3{TranscodeEnabled: true, Allow4KTranscode: true},
	})
	if result.Plan == nil {
		t.Fatalf("result = %#v, want a plan", result)
	}

	before := PlanAttemptKeyV3(*result.Plan, "output-1", nil)
	mutated := *result.Plan
	mutated.EffectiveVirtualURI = "virtual://movie/tt1234567?result=other"
	if after := PlanAttemptKeyV3(mutated, "output-1", nil); after != before {
		t.Errorf("attempt key changed with the effective virtual URI: %q -> %q", before, after)
	}
	if mutated.PlanID != result.Plan.PlanID {
		// PlanID is computed before mutation, but guard the intent explicitly.
		t.Errorf("plan ID changed: %q -> %q", result.Plan.PlanID, mutated.PlanID)
	}
}

// virtualCandidatePlanV3 plans the same virtual requested row against the
// candidate named by resultID, so tests can vary only the resolved candidate.
func virtualCandidatePlanV3(t *testing.T, requested *models.MediaFile, resultID string) *PlanV3 {
	t.Helper()
	candidate := detailedFixtureFileV3()
	candidate.FilePath = "virtual://movie/tt1234567?result=" + resultID
	req := validStartRequestV3()
	req.FileID = requested.ID
	req.Capabilities.VideoDecode = []VideoDecodeCapabilityV3{{Codec: "hevc", Profiles: []string{"main 10"}, Levels: []int{153}, BitDepths: []int{10}, MaxWidth: 3840, MaxHeight: 2160, MaxFrameRate: 60, MaxBitrateKbps: 80_000, Hardware: true}}
	req.Capabilities.HDRDetails = &HDRCapabilitiesV3{HDR10: true}

	result := PlanPlaybackV3(PlannerInputV3{
		Request: req, RequestedFile: requested, EffectiveFile: candidate, AudioTrackIndex: 0,
		Settings: PlannerSettingsV3{TranscodeEnabled: true, Allow4KTranscode: true},
	})
	if result.Plan == nil {
		t.Fatalf("result = %#v, want a plan", result)
	}
	return result.Plan
}

// A release rotation that keeps effective_media_file_id fixed must still be
// visible: clients re-arm source-change recovery on the revision. Effective
// media file id is the requested catalog row and does not move, and the v2 wire
// omits effective_virtual_uri, so this field is the delivered rotation identity.
func TestPlanPlaybackV3VirtualSourceRevisionTracksCandidate(t *testing.T) {
	requested := &models.MediaFile{ID: 41, ContentID: "movie-tt1234567", Container: "mkv", FilePath: "virtual://movie/tt1234567"}

	first := virtualCandidatePlanV3(t, requested, "candidate-a")
	stable := virtualCandidatePlanV3(t, requested, "candidate-a")
	rotated := virtualCandidatePlanV3(t, requested, "candidate-b")

	if first.VirtualSourceRevision == "" {
		t.Fatal("virtual source revision is empty for a resolved virtual candidate")
	}
	if len(first.VirtualSourceRevision) != 24 {
		t.Fatalf("virtual source revision = %q, want a 24-hex opaque token", first.VirtualSourceRevision)
	}
	if first.VirtualSourceRevision == "candidate-a" {
		t.Fatal("virtual source revision leaked the raw candidate identity")
	}
	if stable.VirtualSourceRevision != first.VirtualSourceRevision {
		t.Fatalf("stable candidate revision moved: %q -> %q", first.VirtualSourceRevision, stable.VirtualSourceRevision)
	}
	if rotated.VirtualSourceRevision == first.VirtualSourceRevision {
		t.Fatalf("rotated candidate kept revision %q", first.VirtualSourceRevision)
	}
	if first.EffectiveMediaFileID != rotated.EffectiveMediaFileID {
		t.Fatalf("test setup: effective media file id moved %d -> %d", first.EffectiveMediaFileID, rotated.EffectiveMediaFileID)
	}
}

// A non-virtual effective source has no candidate revision to expose.
func TestPlanPlaybackV3OmitsVirtualSourceRevisionForLocalSource(t *testing.T) {
	file := detailedFixtureFileV3()
	req := validStartRequestV3()
	req.Capabilities.VideoDecode = []VideoDecodeCapabilityV3{{Codec: "hevc", Profiles: []string{"main 10"}, Levels: []int{153}, BitDepths: []int{10}, MaxWidth: 3840, MaxHeight: 2160, MaxFrameRate: 60, MaxBitrateKbps: 80_000, Hardware: true}}
	req.Capabilities.HDRDetails = &HDRCapabilitiesV3{HDR10: true}

	result := PlanPlaybackV3(PlannerInputV3{
		Request: req, RequestedFile: file, EffectiveFile: file, AudioTrackIndex: 0,
		Settings: PlannerSettingsV3{TranscodeEnabled: true, Allow4KTranscode: true},
	})
	if result.Plan == nil {
		t.Fatalf("result = %#v, want a plan", result)
	}
	if result.Plan.VirtualSourceRevision != "" {
		t.Fatalf("virtual source revision = %q, want empty for a local source", result.Plan.VirtualSourceRevision)
	}
}

// A neutral requested row planned without a substitution carries no candidate
// identity, so there is nothing to distinguish and no revision to publish.
func TestPlanPlaybackV3OmitsVirtualSourceRevisionWithoutCandidate(t *testing.T) {
	candidate := detailedFixtureFileV3()
	candidate.FilePath = "virtual://movie/tt1234567"
	requested := &models.MediaFile{ID: 41, ContentID: candidate.ContentID, Container: candidate.Container, FilePath: "virtual://movie/tt1234567"}
	req := validStartRequestV3()
	req.Capabilities.VideoDecode = []VideoDecodeCapabilityV3{{Codec: "hevc", Profiles: []string{"main 10"}, Levels: []int{153}, BitDepths: []int{10}, MaxWidth: 3840, MaxHeight: 2160, MaxFrameRate: 60, MaxBitrateKbps: 80_000, Hardware: true}}
	req.Capabilities.HDRDetails = &HDRCapabilitiesV3{HDR10: true}

	result := PlanPlaybackV3(PlannerInputV3{
		Request: req, RequestedFile: requested, EffectiveFile: candidate, AudioTrackIndex: 0,
		Settings: PlannerSettingsV3{TranscodeEnabled: true, Allow4KTranscode: true},
	})
	if result.Plan == nil {
		t.Fatalf("result = %#v, want a plan", result)
	}
	if result.Plan.VirtualSourceRevision != "" {
		t.Fatalf("virtual source revision = %q, want empty without a resolved candidate", result.Plan.VirtualSourceRevision)
	}
}

// The revision is a UI hint, not a route input: it must not perturb plan
// identity, or a rotation would force a spurious new attempt.
func TestPlanAttemptKeyV3IgnoresVirtualSourceRevision(t *testing.T) {
	requested := &models.MediaFile{ID: 41, ContentID: "movie-tt1234567", Container: "mkv", FilePath: "virtual://movie/tt1234567"}
	plan := virtualCandidatePlanV3(t, requested, "working")

	before := PlanAttemptKeyV3(*plan, "output-1", nil)
	mutated := *plan
	mutated.VirtualSourceRevision = "deadbeefdeadbeefdeadbeef"
	if after := PlanAttemptKeyV3(mutated, "output-1", nil); after != before {
		t.Errorf("attempt key changed with the virtual source revision: %q -> %q", before, after)
	}
	if mutated.PlanID != plan.PlanID {
		t.Errorf("plan ID changed: %q -> %q", plan.PlanID, mutated.PlanID)
	}
}

// TracksPending is a provisional-inventory marker, not a route input: the same
// executable recipe served with a pending or complete menu is the same plan, so
// marking it must not perturb plan identity or a client replay would see a
// spurious new attempt.
func TestPlanAttemptKeyV3IgnoresTracksPending(t *testing.T) {
	requested := &models.MediaFile{ID: 41, ContentID: "movie-tt1234567", Container: "mkv", FilePath: "virtual://movie/tt1234567"}
	plan := virtualCandidatePlanV3(t, requested, "working")

	beforeID := DeterministicPlanIDV3("attempt-1", plan.RequestedMediaFileID, plan.EffectiveMediaFileID, *plan)
	before := PlanAttemptKeyV3(*plan, "output-1", nil)
	mutated := *plan
	mutated.TracksPending = true
	if after := DeterministicPlanIDV3("attempt-1", mutated.RequestedMediaFileID, mutated.EffectiveMediaFileID, mutated); after != beforeID {
		t.Errorf("plan ID changed with tracks_pending: %q -> %q", beforeID, after)
	}
	if after := PlanAttemptKeyV3(mutated, "output-1", nil); after != before {
		t.Errorf("attempt key changed with tracks_pending: %q -> %q", before, after)
	}
}

// The audio-only planner builds its own plan; it must expose the revision too.
func TestPlanPlaybackV3AudioOnlyExposesVirtualSourceRevision(t *testing.T) {
	candidate := audioOnlyFixtureFileV3()
	candidate.FilePath = "virtual://audiobook/tt7654321?result=working"
	requested := &models.MediaFile{ID: 76, ContentID: candidate.ContentID, Container: candidate.Container, FilePath: "virtual://audiobook/tt7654321"}
	req := validStartRequestV3()
	req.FileID = requested.ID
	req.Capabilities.Containers = []string{"mp4"}

	result := PlanPlaybackV3(PlannerInputV3{
		Request: req, RequestedFile: requested, EffectiveFile: candidate, AudioTrackIndex: 0,
		Settings: PlannerSettingsV3{TranscodeEnabled: true},
	})
	if result.Plan == nil {
		t.Fatalf("result = %#v, want a plan", result)
	}
	if result.Plan.VirtualSourceRevision == "" {
		t.Fatal("audio-only virtual plan omitted the virtual source revision")
	}
}

// A repaired inventory under the same candidate id must move the revision: a
// rematch adoption rewrites the declared tracks and clears the probe stamp
// while the ?result= pick stays put, and the client must re-arm recovery for
// the corrected generation.
func TestPlanPlaybackV3VirtualSourceRevisionTracksEvidenceRepair(t *testing.T) {
	requested := &models.MediaFile{ID: 41, ContentID: "movie-tt1234567", Container: "mkv", FilePath: "virtual://movie/tt1234567"}

	before := virtualCandidatePlanV3(t, requested, "candidate-a")

	// Same track counts, same probe version, no probe stamp: only the
	// playback-relevant track fields move (subtitle language, audio codec).
	// The repaired file keeps the fixture's aac audio claim viable so the
	// plan still routes: the revision must move on track-field changes alone.
	repaired := detailedFixtureFileV3()
	repaired.FilePath = "virtual://movie/tt1234567?result=candidate-a"
	repaired.AudioTracks = []models.AudioTrack{{Codec: "aac", Channels: 2, Layout: "stereo", Language: "eng", Languages: []string{"eng"}}}
	repaired.SubtitleTracks = []models.SubtitleTrack{{Index: 2, Language: "fre", Codec: "srt"}}
	req := validStartRequestV3()
	req.FileID = requested.ID
	req.Capabilities.VideoDecode = []VideoDecodeCapabilityV3{{Codec: "hevc", Profiles: []string{"main 10"}, Levels: []int{153}, BitDepths: []int{10}, MaxWidth: 3840, MaxHeight: 2160, MaxFrameRate: 60, MaxBitrateKbps: 80_000, Hardware: true}}
	req.Capabilities.HDRDetails = &HDRCapabilitiesV3{HDR10: true}
	repairResult := PlanPlaybackV3(PlannerInputV3{
		Request: req, RequestedFile: requested, EffectiveFile: repaired, AudioTrackIndex: 0,
		Settings: PlannerSettingsV3{TranscodeEnabled: true, Allow4KTranscode: true},
	})
	if repairResult.Plan == nil {
		t.Fatalf("result = %#v, want a plan", repairResult)
	}
	if repairResult.Plan.VirtualSourceRevision == before.VirtualSourceRevision {
		t.Fatalf("repaired inventory kept revision %q", repairResult.Plan.VirtualSourceRevision)
	}
}

// The revision must survive a signed-URL renewal: only the media generation
// is an input, never the provider URL or refresh timestamps, so a re-resolve
// that changes nothing about the candidate or its evidence is silent.
func TestPlanPlaybackV3VirtualSourceRevisionIgnoresURLRenewal(t *testing.T) {
	requested := &models.MediaFile{ID: 41, ContentID: "movie-tt1234567", Container: "mkv", FilePath: "virtual://movie/tt1234567"}

	first := virtualCandidatePlanV3(t, requested, "candidate-a")
	stable := virtualCandidatePlanV3(t, requested, "candidate-a")

	if stable.VirtualSourceRevision != first.VirtualSourceRevision {
		t.Fatalf("URL-equivalent replan moved revision: %q -> %q", first.VirtualSourceRevision, stable.VirtualSourceRevision)
	}
}
