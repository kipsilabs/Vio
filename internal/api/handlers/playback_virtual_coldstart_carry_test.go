package handlers

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// A cold-start track change on a session whose catalog row has not been probed
// yet must reuse the verified inventory the running plan already validated on
// the same bytes. The provider churn replaces the effective row with a fresh
// listing row that carries no bit depth, frame rate, or bitrate; the session
// still carries the verified audio evidence and the running plan carries the
// probed video descriptor. Before the fix the replan planned against the
// unprobed row and terminaled source_metadata_incomplete, so the menu click
// appeared inert.
func TestReplanTrackChangeCarriesValidatedVideoEvidenceForUnprobedRow(t *testing.T) {
	fx := newPinnedTrackChangeFixture(t, "churned")
	// The session keeps its verified evidence; only the catalog row is a fresh,
	// never-probed listing row.
	fx.churnCatalogRowToStub("churned")

	response := fx.trackChange(t, 1, "pinned-coldstart-inventory-0001")
	if response.Terminal != nil || response.PlaybackPlan == nil {
		t.Fatalf("track change = plan %#v terminal %#v, want a plan", response.PlaybackPlan, response.Terminal)
	}
	if response.PlaybackPlan.EffectiveMediaFileID != fx.source.ID {
		t.Fatalf("effective file = %d, want the session's pinned row %d", response.PlaybackPlan.EffectiveMediaFileID, fx.source.ID)
	}
	if response.PlaybackPlan.EffectiveVirtualURI != fx.pinnedURI {
		t.Fatalf("effective virtual URI = %q, want the pinned candidate %q", response.PlaybackPlan.EffectiveVirtualURI, fx.pinnedURI)
	}
	if response.PlaybackPlan.SelectedTracks.Audio == nil || response.PlaybackPlan.SelectedTracks.Audio.Index == nil ||
		*response.PlaybackPlan.SelectedTracks.Audio.Index != 1 {
		t.Fatalf("audio selection = %#v, want index 1 applied", response.PlaybackPlan.SelectedTracks.Audio)
	}
	// The replan must land on the running plan's validated descriptor, not a
	// stub: the reconstructed track has to round-trip through the planner's
	// source descriptor with the same bit depth, frame rate, and bitrate.
	descriptor := fx.started.PlaybackPlan.Source
	planned := response.PlaybackPlan.Source
	if planned.BitDepth != descriptor.BitDepth || planned.BitrateKbps != descriptor.BitrateKbps ||
		math.Abs(planned.FrameRate-descriptor.FrameRate) > 1e-6 {
		t.Fatalf("carried plan source = {bit_depth:%d frame_rate:%v bitrate_kbps:%d}, want the running plan's descriptor {bit_depth:%d frame_rate:%v bitrate_kbps:%d}",
			planned.BitDepth, planned.FrameRate, planned.BitrateKbps, descriptor.BitDepth, descriptor.FrameRate, descriptor.BitrateKbps)
	}
	if fx.listCalls != 0 || fx.detailCalls != 0 || fx.probeCalls != 0 {
		t.Fatalf("track change re-listed/resolved/probed the provider: list=%d detail=%d probe=%d", fx.listCalls, fx.detailCalls, fx.probeCalls)
	}
}

// A first-ever auto start can plan a concrete candidate while the effective
// catalog row is still the provider-neutral sibling. That sibling carries no
// installation owner, so binding its path and owner leaves a later seek unable
// to reopen the frozen candidate. The start binding must name the planned
// candidate and carry the resolver's owner.
func TestBindPlannedVirtualFilePinsConcreteCandidateOwner(t *testing.T) {
	neutral := &models.MediaFile{
		ID: 3971178, FilePath: "virtual://series/tt26743760/2/23",
		Container: "virtual", VirtualOwnerInstallationID: 0,
	}
	candidate := "virtual://series/tt26743760/2/23?result=0f52cdc36ee53a6b1bbc3853"
	plan := &playback.PlanV3{EffectiveVirtualURI: candidate}

	bound := bindPlannedVirtualFileV3(neutral, plan, 7)
	if bound == nil {
		t.Fatal("bindPlannedVirtualFileV3 returned nil")
	}
	if bound.ID != neutral.ID {
		t.Fatalf("bound row id = %d, want the effective catalog row %d", bound.ID, neutral.ID)
	}
	if bound.FilePath != candidate {
		t.Fatalf("bound path = %q, want the planned candidate %q", bound.FilePath, candidate)
	}
	if bound.VirtualOwnerInstallationID != 7 {
		t.Fatalf("bound owner = %d, want the resolver's owner 7", bound.VirtualOwnerInstallationID)
	}
	if neutral.FilePath != "virtual://series/tt26743760/2/23" || neutral.VirtualOwnerInstallationID != 0 {
		t.Fatal("bindPlannedVirtualFileV3 mutated the catalog row")
	}
}

// The fail-closed placeholder an empty inventory is served as must be
// observable: when no catalog row names the bound candidate, the placeholder
// decision is logged with the held row id, the bound candidate URI, and the row
// id the placeholder was built from, so an operator can correlate an empty
// menu with the reason it is empty.
func TestInventoryEffectiveFileLogsFailClosedPlaceholder(t *testing.T) {
	logs := captureHandlerLogs(t)
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	file := &models.MediaFile{ID: 900, FilePath: "virtual://movie/tt-placeholder?result=other", Container: "virtual"}

	got := handler.inventoryEffectiveFile(context.Background(), file, &playback.Session{}, nil, "virtual://movie/tt-placeholder?result=bound")
	if got == nil {
		t.Fatal("inventoryEffectiveFile returned nil")
	}
	if len(got.AudioTracks) != 0 || len(got.SubtitleTracks) != 0 {
		t.Fatalf("placeholder served tracks: audio=%+v subtitles=%+v", got.AudioTracks, got.SubtitleTracks)
	}
	out := logs.String()
	if !strings.Contains(out, "virtual inventory serving session evidence fallback") {
		t.Fatalf("placeholder decision was not logged:\n%s", out)
	}
	for _, want := range []string{`"file_id":900`, `"base_id":900`, `"candidate_path":"virtual://movie/tt-placeholder"`, `"candidate_id":"bound"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("placeholder log missing %s:\n%s", want, out)
		}
	}
}
