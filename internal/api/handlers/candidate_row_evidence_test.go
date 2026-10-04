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

// TestVirtualEvidenceMatchesBoundFileEnforcesRowIDWithoutEvidenceURI pins the
// missing-URI branch: a session that predates the evidence URI field still
// carries a known evidence row id, and that row id must be checked before the
// session-URI fallback. A mismatched row must not match even though the
// session's virtual URI names the bound candidate.
func TestVirtualEvidenceMatchesBoundFileEnforcesRowIDWithoutEvidenceURI(t *testing.T) {
	uri := "virtual://movie/dup?result=a"
	bound := &models.MediaFile{ID: 12, FilePath: uri}

	mismatchedRow := &playback.Session{
		VirtualSourceURI:              uri,
		VirtualSubtitleEvidenceSet:    true,
		VirtualSubtitleEvidenceFileID: 11,
	}
	if virtualEvidenceMatchesBoundFile(bound, mismatchedRow) {
		t.Fatal("mismatched evidence row 11 matched bound row 12 through the URI-only fallback")
	}

	sameRow := *mismatchedRow
	sameRow.VirtualSubtitleEvidenceFileID = 12
	if !virtualEvidenceMatchesBoundFile(bound, &sameRow) {
		t.Fatal("same-row evidence without an evidence URI lost its URI-only match")
	}
}

// TestVirtualEvidenceMatchesBoundFileRejectsUnknownBoundRow pins the final
// branch: a known evidence row cannot be confirmed against an unknown bound row
// (id 0), so it must not match — a bound row with no identity cannot be the row
// the evidence was captured from.
func TestVirtualEvidenceMatchesBoundFileRejectsUnknownBoundRow(t *testing.T) {
	uri := "virtual://movie/dup?result=a"
	unknownBound := &models.MediaFile{FilePath: uri}

	known := &playback.Session{
		VirtualSourceURI:              uri,
		VirtualSubtitleEvidenceSet:    true,
		VirtualSubtitleEvidenceURI:    uri,
		VirtualSubtitleEvidenceFileID: 12,
	}
	if virtualEvidenceMatchesBoundFile(unknownBound, known) {
		t.Fatal("known evidence row 12 matched an unknown bound row")
	}

	legacy := *known
	legacy.VirtualSubtitleEvidenceFileID = 0
	if !virtualEvidenceMatchesBoundFile(unknownBound, &legacy) {
		t.Fatal("legacy evidence without a row id lost its URI-only match")
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
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0), mapPlaybackFileResolver{files: map[int]*models.MediaFile{11: requestedRow, 12: {ID: 12, ContentID: "movie-dup", FilePath: candidateURI}}})

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

// TestRefusedProbeInventoryFileRejectsUnknownBoundRow pins the zero-ID guard:
// a session whose effective catalog row is unknown (id 0) cannot be confirmed as
// the row the probe came from, so it must not receive the in-memory override,
// even though its requested row and virtual URI match. This mirrors the
// serving-path rule that rejects a known evidence row against an unknown bound
// row.
func TestRefusedProbeInventoryFileRejectsUnknownBoundRow(t *testing.T) {
	candidateURI := "virtual://movie/dup-zero?result=a"
	requestedRow := &models.MediaFile{ID: 11, ContentID: "movie-dup-zero", FilePath: candidateURI}
	probed := &models.MediaFile{
		AudioTracks:    []models.AudioTrack{{Codec: "eac3", Channels: 6, Language: "eng"}},
		SubtitleTracks: []models.SubtitleTrack{{Index: 1, Codec: "subrip", Language: "eng"}},
	}
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0), mapPlaybackFileResolver{files: map[int]*models.MediaFile{11: requestedRow}})

	unknown := &playback.Session{ID: "s-zero", MediaFileID: 0, VirtualSourceURI: candidateURI}
	if override := h.refusedProbeInventoryFile(context.Background(), unknown, 0, candidateURI, probed); override != nil {
		t.Fatalf("unknown bound row received probed inventory: %+v", override)
	}

	// A known effective row with a row-id-less caller keeps the historical
	// URI-only path.
	known := &playback.Session{ID: "s-known", MediaFileID: 11, VirtualSourceURI: candidateURI}
	if override := h.refusedProbeInventoryFile(context.Background(), known, 0, candidateURI, probed); override == nil {
		t.Fatal("known effective row lost the historical URI-only override")
	}
}

// TestRefusedProbePublishSkipsUnknownEffectiveRow proves the push fan-out does
// not deliver a probe's tracks to a session whose effective catalog row is
// unknown: it is enumerated by its requested row, but it carries no identity to
// confirm against the probed row.
func TestRefusedProbePublishSkipsUnknownEffectiveRow(t *testing.T) {
	candidateURI := "virtual://movie/dup-push-zero?result=a"
	requestedRow := &models.MediaFile{ID: 11, ContentID: "movie-dup-push-zero", FilePath: candidateURI}
	probed := &models.MediaFile{
		AudioTracks: []models.AudioTrack{{Codec: "eac3", Channels: 6, Language: "eng"}},
	}

	mgr := playback.NewSessionManager(0, 0)
	// Effective row unknown (0); the requested row makes it enumerable.
	session, err := mgr.StartSessionWithFiles(1, "profile-1", 0, requestedRow.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSessionWithFiles: %v", err)
	}
	if err := mgr.SetVirtualSource(session.ID, candidateURI, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}

	h := NewPlaybackHandler(mgr, mapPlaybackFileResolver{files: map[int]*models.MediaFile{11: requestedRow}})
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
		t.Fatalf("notified %d sessions, want 0: an unknown effective row must not receive probed tracks", notified)
	}
	if len(conn.messages) != 0 {
		t.Fatalf("delivered %d events to the unknown-row session, want 0", len(conn.messages))
	}
}

// TestProbeRotatedVirtualCandidateRefreshesReplacement proves the post-rotation
// refresh: once a serve-layer rotation commits a replacement binding, the
// replacement is re-resolved and probed so its verified evidence lands without a
// replan. A rotation that moves the binding again before the probe runs is
// fenced out, so a probe can never be applied to a superseded candidate.
func TestProbeRotatedVirtualCandidateRefreshesReplacement(t *testing.T) {
	const (
		neutral = "virtual://movie/tt-post-rot"
		oldURI  = neutral + "?result=cand-a"
		newURI  = neutral + "?result=cand-b"
	)

	mgr := playback.NewSessionManager(0, 0)
	session, err := mgr.StartSession(1, "profile-1", 42, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := mgr.UpdateStreamState(session.ID, playback.SessionStreamState{
		VirtualSourceSet:              true,
		VirtualSourceOwnershipSet:     true,
		VirtualSourceURI:              oldURI,
		VirtualSourceRevision:         "rev-a",
		VirtualSubtitleEvidenceSet:    true,
		VirtualSubtitleEvidenceURI:    oldURI,
		VirtualSubtitleEvidenceFileID: 42,
		VirtualSubtitleTracks:         []models.SubtitleTrack{{Index: 1, Codec: "subrip", Language: "eng"}},
	}); err != nil {
		t.Fatalf("UpdateStreamState: %v", err)
	}
	// The rotation moves the binding to the replacement and clears the revision,
	// leaving the evidence anchored at the old candidate: the recorded-rotation
	// signal the refresh keys on.
	if err := mgr.SetVirtualSource(session.ID, newURI, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	rotated, err := mgr.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}

	file := &models.MediaFile{
		ID: 42, ContentID: "movie-post-rot", FilePath: newURI,
		Container: "virtual", VirtualOwnerInstallationID: 5,
	}
	if !virtualCandidateRotationRecordedV3(rotated, file) {
		t.Fatal("rotation was not recorded")
	}

	var resolvedPath string
	probed := false
	h := NewPlaybackHandler(mgr, testPlaybackFileResolver{file: file})
	h.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		resolvedPath = uri
		return ResolvedVirtualMedia{
			URL: "http://127.0.0.1:9/replacement", URI: newURI, CandidateID: "cand-b", IdentityRematched: true,
		}, nil
	})
	h.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		probed = true
		f.AudioTracks = []models.AudioTrack{{Codec: "eac3", Channels: 6, Language: "eng"}}
		return f, nil
	}

	generation, ok := h.inventorySourceGeneration(session.ID)
	if !ok {
		t.Fatal("session manager did not expose a binding generation")
	}
	if !h.probeRotatedVirtualCandidate(context.Background(), rotated, file, generation) {
		t.Fatal("rotation refresh did not probe the replacement")
	}
	if resolvedPath != newURI {
		t.Fatalf("re-resolved %q, want the rotated replacement %q", resolvedPath, newURI)
	}
	if !probed {
		t.Fatal("replacement was not probed after rotation")
	}

	// A newer rotation moves the binding again before this probe runs: the
	// stale generation is fenced out and no probe is spent on a superseded
	// candidate, keeping the gate fail-closed for the new binding.
	if err := mgr.SetVirtualSource(session.ID, neutral+"?result=cand-c", 5); err != nil {
		t.Fatalf("second SetVirtualSource: %v", err)
	}
	probed = false
	if h.probeRotatedVirtualCandidate(context.Background(), rotated, file, generation) {
		t.Fatal("a superseded generation still probed")
	}
	if probed {
		t.Fatal("a superseded generation spent a probe")
	}
}
