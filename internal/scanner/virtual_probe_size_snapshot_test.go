package scanner

import (
	"context"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

const (
	snapshotDeclaredBytes = int64(2_040_000_000) // provider's rounded declaration
	snapshotObservedBytes = int64(1_780_000_000) // what ffprobe actually read (~13% off)
	snapshotRuntimeMin    = 102
	snapshotDurationSec   = 6_120
)

func snapshotSubtitleTracks(codec string) []models.SubtitleTrack {
	return []models.SubtitleTrack{{Codec: codec, Language: "eng"}}
}

// A rounded provider declaration is the production shape: the served bytes are
// ~13% short of the declared size, so the 5% gate can only excuse them when the
// measured duration proves the content. That acceptance must record the
// observed size for the pin, because the declared size will never clear the
// gate on its own.
func TestVirtualServedSizeSnapshotRecordedOnDurationProvenAcceptance(t *testing.T) {
	ResetVirtualServedSizeSnapshots()
	t.Cleanup(ResetVirtualServedSizeSnapshots)

	pin := VirtualServedSizePin("virtual://movie/tt-snap?result=rounded", 7)
	if pin == "" {
		t.Fatal("a concrete candidate must have a pin")
	}
	token := VirtualServedSizeSubtitleToken(snapshotSubtitleTracks("subrip"))

	// Without a snapshot the evidence is playable but unverified: the size
	// contradicts the declaration and only the duration argues for identity.
	verdict := ClassifyVirtualServedEvidenceForPin(pin, snapshotDeclaredBytes, snapshotObservedBytes, snapshotDurationSec, snapshotRuntimeMin, token)
	if verdict != VirtualServedEvidenceUnverified {
		t.Fatalf("verdict before acceptance = %v, want unverified", verdict)
	}

	verdict = ClassifyVirtualServedEvidenceForPinWithSnapshot(pin, snapshotDeclaredBytes, snapshotObservedBytes, snapshotDurationSec, snapshotRuntimeMin, token)
	if verdict != VirtualServedEvidenceAdmitted {
		t.Fatalf("verdict after duration-proven acceptance = %v, want admitted", verdict)
	}
	snapshot, ok := VirtualServedSizeSnapshotFor(pin, snapshotDeclaredBytes, token)
	if !ok {
		t.Fatal("duration-proven acceptance recorded no snapshot")
	}
	if snapshot.ObservedBytes != snapshotObservedBytes {
		t.Fatalf("snapshot observed = %d, want %d", snapshot.ObservedBytes, snapshotObservedBytes)
	}
	if snapshot.DeclaredBytes != snapshotDeclaredBytes {
		t.Fatalf("snapshot declared = %d, want the provider declaration %d", snapshot.DeclaredBytes, snapshotDeclaredBytes)
	}
}

// The snapshot is the baseline future comparisons for the same pin use, so a
// re-probe of the same bytes is admitted instead of being re-probed forever,
// while a genuinely different size under the pin still has to prove identity by
// duration.
func TestVirtualServedSizeSnapshotDrivesLaterEvidenceComparisons(t *testing.T) {
	ResetVirtualServedSizeSnapshots()
	t.Cleanup(ResetVirtualServedSizeSnapshots)

	pin := VirtualServedSizePin("virtual://movie/tt-snap-later?result=rounded", 7)
	token := VirtualServedSizeSubtitleToken(snapshotSubtitleTracks("subrip"))
	if verdict := ClassifyVirtualServedEvidenceForPinWithSnapshot(pin, snapshotDeclaredBytes, snapshotObservedBytes, snapshotDurationSec, snapshotRuntimeMin, token); verdict != VirtualServedEvidenceAdmitted {
		t.Fatalf("first acceptance verdict = %v, want admitted", verdict)
	}

	// A later probe reads the same release: the baseline is now the observed
	// size, so a few megabytes of drift stays inside the 5% window.
	later := snapshotObservedBytes + 20_000_000
	if verdict := ClassifyVirtualServedEvidenceForPin(pin, snapshotDeclaredBytes, later, 0, 0, token); verdict != VirtualServedEvidenceAdmitted {
		t.Fatalf("later probe verdict = %v, want admitted against the observed baseline", verdict)
	}
	// A size the declared baseline would have accepted is a different posting
	// against the observed baseline; with no duration proof it is a rejection,
	// not merely unverified evidence.
	declaredBaselineOnly := snapshotDeclaredBytes
	if verdict := ClassifyVirtualServedEvidenceForPin(pin, snapshotDeclaredBytes, declaredBaselineOnly, 0, 0, token); verdict != VirtualServedEvidenceMismatch {
		t.Fatalf("different posting verdict = %v, want mismatch (observed baseline disagrees)", verdict)
	}
}

// A subtitle-layout change means the bytes the pin's ordinals address changed,
// so the observed size recorded for the old layout must stop standing in for the
// declaration.
func TestVirtualServedSizeSnapshotInvalidatedOnLayoutChange(t *testing.T) {
	ResetVirtualServedSizeSnapshots()
	t.Cleanup(ResetVirtualServedSizeSnapshots)

	pin := VirtualServedSizePin("virtual://movie/tt-snap-layout?result=rounded", 7)
	before := VirtualServedSizeSubtitleToken(snapshotSubtitleTracks("subrip"))
	if verdict := ClassifyVirtualServedEvidenceForPinWithSnapshot(pin, snapshotDeclaredBytes, snapshotObservedBytes, snapshotDurationSec, snapshotRuntimeMin, before); verdict != VirtualServedEvidenceAdmitted {
		t.Fatalf("acceptance verdict = %v, want admitted", verdict)
	}

	after := VirtualServedSizeSubtitleToken([]models.SubtitleTrack{{Codec: "ass", Language: "eng"}})
	if _, ok := VirtualServedSizeSnapshotFor(pin, snapshotDeclaredBytes, after); ok {
		t.Fatal("snapshot survived a subtitle-layout change")
	}
	if verdict := ClassifyVirtualServedEvidenceForPin(pin, snapshotDeclaredBytes, snapshotObservedBytes, snapshotDurationSec, snapshotRuntimeMin, after); verdict != VirtualServedEvidenceUnverified {
		t.Fatalf("verdict after layout change = %v, want unverified (declared baseline again)", verdict)
	}
}

// The snapshot records what the pin's bytes measured; it must never replace the
// release's declared size, which backs the AltMount-completion and release
// identity comparisons.
func TestVirtualServedSizeSnapshotPreservesDeclaredSize(t *testing.T) {
	ResetVirtualServedSizeSnapshots()
	t.Cleanup(ResetVirtualServedSizeSnapshots)

	pin := VirtualServedSizePin("virtual://movie/tt-snap-declared?result=rounded", 7)
	token := VirtualServedSizeSubtitleToken(snapshotSubtitleTracks("subrip"))
	row := &models.MediaFile{FileSize: snapshotDeclaredBytes}

	if ok := RecordVirtualServedSizeSnapshotIfProven(pin, row.FileSize, snapshotObservedBytes, snapshotDurationSec, snapshotRuntimeMin, token); !ok {
		t.Fatal("duration-proven acceptance recorded no snapshot")
	}
	if row.FileSize != snapshotDeclaredBytes {
		t.Fatalf("row declared size = %d, want the untouched declaration %d", row.FileSize, snapshotDeclaredBytes)
	}
	if !VirtualServedSizeMismatch(row.FileSize, snapshotObservedBytes) {
		t.Fatal("declared-vs-served mismatch disappeared: the snapshot leaked into the declared size")
	}
	// A re-declaration invalidates the snapshot rather than silently comparing
	// the new listing against the old observation.
	if _, ok := VirtualServedSizeSnapshotFor(pin, snapshotDeclaredBytes+1, token); ok {
		t.Fatal("snapshot survived a declared-size change")
	}
}

// Unproven evidence must never teach the gate: a size gap with an implausible
// duration is a different release, and a size inside the tolerance needs no
// snapshot at all.
func TestVirtualServedSizeSnapshotNeverRecordedWithoutDurationProof(t *testing.T) {
	ResetVirtualServedSizeSnapshots()
	t.Cleanup(ResetVirtualServedSizeSnapshots)

	pin := VirtualServedSizePin("virtual://movie/tt-snap-unproven?result=wrong", 7)
	token := VirtualServedSizeSubtitleToken(snapshotSubtitleTracks("subrip"))

	tests := []struct {
		name          string
		declared      int64
		served        int64
		probedSeconds int
		expectedMins  int
	}{
		{name: "duration does not prove identity", declared: snapshotDeclaredBytes, served: snapshotObservedBytes, probedSeconds: 600, expectedMins: 102},
		{name: "no measured duration", declared: snapshotDeclaredBytes, served: snapshotObservedBytes, probedSeconds: 0, expectedMins: 102},
		{name: "no expected runtime", declared: snapshotDeclaredBytes, served: snapshotObservedBytes, probedSeconds: 6_120, expectedMins: 0},
		{name: "size inside tolerance", declared: snapshotDeclaredBytes, served: snapshotDeclaredBytes - 20_000_000, probedSeconds: 6_120, expectedMins: 102},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if ok := RecordVirtualServedSizeSnapshotIfProven(pin, tc.declared, tc.served, tc.probedSeconds, tc.expectedMins, token); ok {
				t.Fatal("unproven evidence recorded a snapshot")
			}
			if _, ok := VirtualServedSizeSnapshotFor(pin, tc.declared, token); ok {
				t.Fatal("unproven evidence left a snapshot behind")
			}
		})
	}

	// A rejected probe is returned as a mismatch and records nothing even
	// through the acceptance entry point.
	if verdict := ClassifyVirtualServedEvidenceForPinWithSnapshot(pin, snapshotDeclaredBytes, snapshotObservedBytes, 600, snapshotRuntimeMin, token); verdict != VirtualServedEvidenceMismatch {
		t.Fatalf("verdict = %v, want mismatch", verdict)
	}
	if _, ok := VirtualServedSizeSnapshotFor(pin, snapshotDeclaredBytes, token); ok {
		t.Fatal("a rejected probe recorded a snapshot")
	}
}

// A release-level path carries no concrete ?result= candidate, so no observed
// size can be attributed to one pin's bytes.
func TestVirtualServedSizePinRequiresConcreteCandidate(t *testing.T) {
	ResetVirtualServedSizeSnapshots()
	t.Cleanup(ResetVirtualServedSizeSnapshots)

	if pin := VirtualServedSizePin("virtual://movie/tt-neutral", 7); pin != "" {
		t.Fatalf("neutral path pin = %q, want empty", pin)
	}
	pin := VirtualServedSizePin("virtual://movie/tt-neutral?profile=4k&result=abc", 7)
	want := VirtualServedSizePin("virtual://movie/tt-neutral?profile=4k&result=abc", 7)
	if pin != want || pin == "" {
		t.Fatalf("concrete pin = %q, want stable %q", pin, want)
	}
	if other := VirtualServedSizePin("virtual://movie/tt-neutral?profile=4k&result=abc", 8); other == pin {
		t.Fatal("pins from different owners must not collide")
	}
}

// The probe cache is the recurring 5% comparison: a rounded declaration
// accepted by duration must cache (so the row is not re-probed on every start),
// and the cached entry must remain the evidence the row receives.
func TestVirtualProbeCacheCachesDurationProvenRoundedDeclaration(t *testing.T) {
	ResetVirtualServedSizeSnapshots()
	t.Cleanup(ResetVirtualServedSizeSnapshots)

	cache := NewVirtualProbeCache(time.Minute, 4)
	row := &models.MediaFile{
		ID: 7, FilePath: "virtual://movie/tt-cache-rounded?result=rounded",
		FileSize:                      snapshotDeclaredBytes,
		VirtualExpectedRuntimeMinutes: snapshotRuntimeMin,
	}
	var calls int
	probe := func(_ context.Context, _ string, input *models.MediaFile) (*models.MediaFile, error) {
		calls++
		result := *input
		result.ProbedFormatSize = snapshotObservedBytes
		result.Duration = snapshotDurationSec
		result.SubtitleTracks = snapshotSubtitleTracks("subrip")
		return &result, nil
	}
	first, err := cache.Probe(context.Background(), "https://provider.example/video.mkv?token=one", row, probe)
	if err != nil {
		t.Fatal(err)
	}
	if first == nil || first.ProbedFormatSize != snapshotObservedBytes {
		t.Fatalf("first probe = %+v, want the observed bytes", first)
	}
	second, err := cache.Probe(context.Background(), "https://provider.example/video.mkv?token=two", row, probe)
	if err != nil {
		t.Fatal(err)
	}
	if second == nil || second.ProbedFormatSize != snapshotObservedBytes {
		t.Fatalf("second probe = %+v, want the cached observed bytes", second)
	}
	if calls != 1 {
		t.Fatalf("probe calls = %d, want 1: a duration-proven rounded declaration must be cached", calls)
	}

	// The cached inventory keeps the subtitle layout the snapshot was keyed on,
	// so warming consumers see the same token.
	if got, want := playback.SubtitleInventoryToken(second.SubtitleTracks), playback.SubtitleInventoryToken(snapshotSubtitleTracks("subrip")); got != want {
		t.Fatalf("cached subtitle token = %q, want %q", got, want)
	}
}
