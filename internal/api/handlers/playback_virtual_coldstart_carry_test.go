package handlers

import (
	"context"
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
	if fx.listCalls != 0 || fx.detailCalls != 0 || fx.probeCalls != 0 {
		t.Fatalf("track change re-listed/resolved/probed the provider: list=%d detail=%d probe=%d", fx.listCalls, fx.detailCalls, fx.probeCalls)
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
	if !strings.Contains(out, "virtual inventory serving fail-closed placeholder") {
		t.Fatalf("placeholder decision was not logged:\n%s", out)
	}
	for _, want := range []string{`"file_id":900`, `"base_id":900`, `"candidate_uri":"virtual://movie/tt-placeholder?result=bound"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("placeholder log missing %s:\n%s", want, out)
		}
	}
}
