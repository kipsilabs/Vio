package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// TestVirtualEvidenceMatchesBoundFileRequiresRowID pins the row-id half of the
// provenance anchor: duplicate catalog rows for one release share candidate
// URIs, so URI equality alone would apply row A's evidence to sibling row B.
// The evidence row id must match the bound row, and a zero id (legacy) keeps the
// historical URI-only behavior.
func TestVirtualEvidenceMatchesBoundFileRequiresRowID(t *testing.T) {
	uri := "virtual://movie/dup?result=a"
	bound := &models.MediaFile{ID: 12, FilePath: uri}

	sameRow := &playback.Session{
		VirtualSourceURI:              uri,
		VirtualSubtitleEvidenceSet:    true,
		VirtualSubtitleEvidenceURI:    uri,
		VirtualSubtitleEvidenceFileID: 12,
	}
	if !virtualEvidenceMatchesBoundFile(bound, sameRow) {
		t.Fatal("evidence captured from the bound row did not match")
	}

	siblingRow := *sameRow
	siblingRow.VirtualSubtitleEvidenceFileID = 11
	if virtualEvidenceMatchesBoundFile(bound, &siblingRow) {
		t.Fatal("evidence from sibling row 11 matched bound row 12 sharing the same candidate URI")
	}

	legacy := *sameRow
	legacy.VirtualSubtitleEvidenceFileID = 0
	if !virtualEvidenceMatchesBoundFile(bound, &legacy) {
		t.Fatal("legacy evidence with no row id lost its URI-only match")
	}

	foreignURI := *sameRow
	foreignURI.VirtualSubtitleEvidenceURI = "virtual://movie/dup?result=b"
	if virtualEvidenceMatchesBoundFile(bound, &foreignURI) {
		t.Fatal("evidence anchored at a different candidate matched")
	}
}

// TestV3SessionStreamStateCapturesEvidenceRowID proves the plan-time capture
// writes the effective file's row id alongside the URI, so a later serve can
// disambiguate duplicate rows.
func TestV3SessionStreamStateCapturesEvidenceRowID(t *testing.T) {
	probedAt := time.Now()
	file := &models.MediaFile{
		ID:             345,
		ContentID:      "movie-dup",
		FilePath:       "virtual://movie/dup?result=a",
		Container:      "virtual",
		ProbeUpdatedAt: &probedAt,
		SubtitleTracks: []models.SubtitleTrack{{Index: 1, Codec: "subrip", Language: "eng"}},
	}
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0), testPlaybackFileResolver{file: file})
	state := h.v3SessionStreamState(context.Background(), &playback.Session{ID: "sess"}, file, playback.PlannerResultV3{}, preparedTransportV3{}, mediaAuthModeV3{})
	if state.VirtualSubtitleEvidenceFileID != file.ID {
		t.Fatalf("evidence row id = %d, want the effective file %d", state.VirtualSubtitleEvidenceFileID, file.ID)
	}
	if state.VirtualSubtitleEvidenceURI != file.FilePath {
		t.Fatalf("evidence URI = %q, want %q", state.VirtualSubtitleEvidenceURI, file.FilePath)
	}
	if !state.VirtualSubtitleEvidenceSet {
		t.Fatal("probed effective file did not freeze evidence")
	}

	// The row id travels with the evidence through the session manager.
	mgr := playback.NewSessionManager(0, 0)
	session, err := mgr.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := mgr.UpdateStreamState(session.ID, state); err != nil {
		t.Fatalf("UpdateStreamState: %v", err)
	}
	live, err := mgr.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if live.VirtualSubtitleEvidenceFileID != file.ID {
		t.Fatalf("applied evidence row id = %d, want %d", live.VirtualSubtitleEvidenceFileID, file.ID)
	}
}

// TestRefusedProbeInventoryFileRequiresSameRow pins the in-memory refused-probe
// fallback: it may only paint the session whose effective row is the probed row,
// even when a sibling row shares the candidate URI.
func TestRefusedProbeInventoryFileRequiresSameRow(t *testing.T) {
	candidateURI := "virtual://movie/dup?result=a"
	requestedRow := &models.MediaFile{ID: 11, ContentID: "movie-dup", FilePath: candidateURI}
	probed := &models.MediaFile{
		AudioTracks:    []models.AudioTrack{{Codec: "eac3", Channels: 6, Language: "eng"}},
		SubtitleTracks: []models.SubtitleTrack{{Index: 1, Codec: "subrip", Language: "eng"}},
	}
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0), mapPlaybackFileResolver{files: map[int]*models.MediaFile{11: requestedRow, 12: &models.MediaFile{ID: 12, ContentID: "movie-dup", FilePath: candidateURI}}})

	sameRow := &playback.Session{ID: "s1", MediaFileID: 11, VirtualSourceURI: candidateURI}
	if override := h.refusedProbeInventoryFile(context.Background(), sameRow, 11, candidateURI, probed); override == nil {
		t.Fatal("probed row's own session did not receive the in-memory inventory")
	}

	// A sibling session plays the same candidate URI under a different row id.
	sibling := &playback.Session{ID: "s2", MediaFileID: 12, VirtualSourceURI: candidateURI}
	if override := h.refusedProbeInventoryFile(context.Background(), sibling, 11, candidateURI, probed); override != nil {
		t.Fatalf("sibling row 12 received row 11's probed inventory: %+v", override)
	}

	// A caller without an evidence row id keeps the historical URI-only path.
	if override := h.refusedProbeInventoryFile(context.Background(), sibling, 0, candidateURI, probed); override == nil {
		t.Fatal("row-id-less caller lost the historical URI-only override")
	}
}

// TestRefusedProbePublishSkipsSiblingRowSharingURI proves the publish fan-out
// does not deliver row A's probed tracks to a session that requested row A but
// is effectively playing sibling row B.
func TestRefusedProbePublishSkipsSiblingRowSharingURI(t *testing.T) {
	candidateURI := "virtual://movie/dup?result=a"
	requestedRow := &models.MediaFile{ID: 11, ContentID: "movie-dup", FilePath: candidateURI}
	siblingRow := &models.MediaFile{ID: 12, ContentID: "movie-dup", FilePath: candidateURI}
	probed := &models.MediaFile{
		AudioTracks: []models.AudioTrack{{Codec: "eac3", Channels: 6, Language: "eng"}},
	}

	mgr := playback.NewSessionManager(0, 0)
	// The session requested row 11 but rotated to (effective) sibling row 12.
	session, err := mgr.StartSessionWithFiles(1, "profile-1", siblingRow.ID, requestedRow.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := mgr.SetVirtualSource(session.ID, candidateURI, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}

	h := NewPlaybackHandler(mgr, mapPlaybackFileResolver{files: map[int]*models.MediaFile{11: requestedRow, 12: siblingRow}})
	h.RealtimeHub = playback.NewRealtimeHub()
	if err := mgr.SetRealtimeConnection(session.ID, true); err != nil {
		t.Fatalf("SetRealtimeConnection: %v", err)
	}
	conn := &sourceCommittedTestConn{}
	registration := h.RealtimeHub.Register(session.ID, conn)
	if registration == nil {
		t.Fatal("expected a realtime registration")
	}
	defer h.RealtimeHub.Unregister(registration)

	if notified := h.publishRefusedProbeInventory(context.Background(), requestedRow.ID, candidateURI, probed); notified != 0 {
		t.Fatalf("notified %d sessions, want 0: the sibling row must not receive the requested row's probed tracks", notified)
	}
	if len(conn.messages) != 0 {
		t.Fatalf("delivered %d events to the sibling row, want 0", len(conn.messages))
	}
}
