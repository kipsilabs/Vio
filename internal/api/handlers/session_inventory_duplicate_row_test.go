package handlers

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// The duplicate-row inventory case: a virtual release can have more than one
// catalog row stored at the exact same candidate URI (a renumbered listing, an
// owner row materialized beside its neutral sibling, a re-probe that inserted a
// fresh row). The plan commits and the probe writes to one specific row; an
// exact-path lookup can return the other. The inventory endpoint must resolve
// the row the session is actually bound to — the plan's committed effective row
// the probe delivered for — instead of an arbitrary duplicate, or the session's
// own verified evidence is rejected by row id and the menu freezes on declared.
const (
	dupProbedRowID   = 4912237
	dupResolvedRowID = 5382723
	dupOtherRowID    = 5382724
	dupCandidateURI  = "virtual://series/tt26743760/2/20?profile=4K+Dolby+Vision&result=b5950ed2eb640feafff4e925"
	dupOtherURI      = "virtual://series/tt26743760/2/20?profile=4K+Dolby+Vision&result=ffffffffffffffffffffffff"
)

// duplicateRowResolver serves two catalog rows that share one candidate path,
// exactly as the production catalog can. GetByPath resolves the duplicate that
// is NOT the row the session carries evidence for, so the test fails before the
// resolution keeps the bound row.
type duplicateRowResolver struct {
	mu     sync.Mutex
	byID   map[int]models.MediaFile
	byPath map[string]int
}

func (r *duplicateRowResolver) GetByID(_ context.Context, id int) (*models.MediaFile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	row, ok := r.byID[id]
	if !ok {
		return nil, nil
	}
	cp := row
	return &cp, nil
}

func (r *duplicateRowResolver) GetByPath(_ context.Context, path string) (*models.MediaFile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.byPath[strings.TrimSpace(path)]
	if !ok {
		return nil, nil
	}
	cp := r.byID[id]
	return &cp, nil
}

func (r *duplicateRowResolver) update(id int, fn func(*models.MediaFile)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	row := r.byID[id]
	fn(&row)
	r.byID[id] = row
}

func duplicateProbedAudio() []models.AudioTrack {
	return []models.AudioTrack{
		{Index: 0, Codec: "aac", Channels: 2, Language: "eng", Default: true},
		{Index: 1, Codec: "eac3", Channels: 6, Language: "deu"},
	}
}

// duplicateInventoryRows builds the probed row the plan commits to and the
// unprobed duplicate the path lookup returns for the same candidate URI. The
// probed probeAt flag lets a test model the row before and after the probe.
func duplicateInventoryRows(probed bool) (models.MediaFile, models.MediaFile) {
	planAudio := []models.AudioTrack{{Index: 0, Codec: "aac", Channels: 2, Language: "eng", Default: true}}
	probedRow := models.MediaFile{
		ID: dupProbedRowID, ContentID: "episode-dup", FilePath: dupCandidateURI,
		Container: "mkv", VirtualOwnerInstallationID: 5,
		AudioTracks: planAudio,
	}
	if probed {
		stamp := time.Now().Add(-time.Minute).UTC()
		probedRow.ProbeUpdatedAt = &stamp
		probedRow.AudioTracks = duplicateProbedAudio()
		probedRow.SubtitleTracks = []models.SubtitleTrack{{Index: 2, Codec: "subrip", Language: "eng"}}
	}
	resolvedRow := models.MediaFile{
		ID: dupResolvedRowID, ContentID: "episode-dup", FilePath: dupCandidateURI,
		Container: "virtual", VirtualOwnerInstallationID: 5,
		AudioTracks: planAudio,
	}
	return probedRow, resolvedRow
}

type duplicateInventoryFixture struct {
	handler   *PlaybackHandler
	manager   *playback.SessionManager
	session   *playback.Session
	resolver  *duplicateRowResolver
	probedRow models.MediaFile
}

// setupDuplicateInventoryFixture binds a session to the probed row while the
// catalog's exact-path lookup resolves the duplicate, mirroring the production
// shape where the plan committed the probed row but the inventory resolved the
// unprobed sibling. The session carries the plan row's evidence anchor and its
// declared overlay.
func setupDuplicateInventoryFixture(t *testing.T, probed bool) *duplicateInventoryFixture {
	t.Helper()
	probedRow, resolvedRow := duplicateInventoryRows(probed)
	resolver := &duplicateRowResolver{
		byID:   map[int]models.MediaFile{probedRow.ID: probedRow, resolvedRow.ID: resolvedRow},
		byPath: map[string]int{dupCandidateURI: resolvedRow.ID},
	}

	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", probedRow.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := manager.SetVirtualSource(session.ID, dupCandidateURI, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	if err := manager.UpdateStreamState(session.ID, playback.SessionStreamState{
		VirtualSourceSet:              true,
		VirtualSourceOwnershipSet:     true,
		VirtualSourceURI:              dupCandidateURI,
		VirtualSourceRevision:         "planrev",
		VirtualSubtitleEvidenceSet:    true,
		VirtualSubtitleEvidenceURI:    dupCandidateURI,
		VirtualSubtitleEvidenceFileID: probedRow.ID,
		VirtualAudioTracks:            probedRow.AudioTracks,
		VirtualSubtitleTracks:         probedRow.SubtitleTracks,
	}); err != nil {
		t.Fatalf("UpdateStreamState: %v", err)
	}

	handler := NewPlaybackHandler(manager, resolver)
	handler.InstallationID = "test-install"
	handler.VirtualFileLookup = resolver.GetByPath
	// The path lookup resolves the duplicate, which is the shape the fix must
	// not let override the session's bound row.
	probedAt := time.Now().UTC()
	handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
		f.AudioTracks = duplicateProbedAudio()
		f.ProbeUpdatedAt = &probedAt
		return f, nil
	}
	if err := handler.PlanStoreV3.SaveAttempt(context.Background(), playback.AttemptRecordV3{
		PlaybackAttemptID: "attempt-dup", SessionID: session.ID, UserID: 1, ProfileID: "profile-1",
		RequestedMediaFileID: probedRow.ID, EffectiveMediaFileID: probedRow.ID, ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("SaveAttempt: %v", err)
	}

	return &duplicateInventoryFixture{
		handler:   handler,
		manager:   manager,
		session:   session,
		resolver:  resolver,
		probedRow: probedRow,
	}
}

func (fx *duplicateInventoryFixture) inventory(t *testing.T) playback.PlaybackInventoryV3 {
	t.Helper()
	inventory, err := fx.handler.GetPlaybackInventoryV2(newAuthorizedPlaybackContext(),
		PlaybackCaller{UserID: 1, ProfileID: "profile-1", InstallationID: "test-install"}, fx.session.ID)
	if err != nil {
		t.Fatalf("GetPlaybackInventoryV2: %v", err)
	}
	return inventory
}

// TestSessionInventoryResolvesPlanRowOverUnprobedDuplicate is the production
// shape: the plan committed (and the probe wrote to) row 4912237, but an
// exact-path lookup resolves the unprobed duplicate 5382723 for the same
// candidate URI. The inventory must serve the committed probed row: effective
// 4912237 with its verified inventory, never the duplicate's declared list.
func TestSessionInventoryResolvesPlanRowOverUnprobedDuplicate(t *testing.T) {
	fx := setupDuplicateInventoryFixture(t, true)

	inventory := fx.inventory(t)
	if inventory.EffectiveMediaFileID != dupProbedRowID {
		t.Fatalf("effective media file id = %d, want the plan's committed probed row %d (not the duplicate %d)",
			inventory.EffectiveMediaFileID, dupProbedRowID, dupResolvedRowID)
	}
	if inventory.InventoryStatus != string(ProbeProvenanceVerified) {
		t.Fatalf("inventory status = %q, want verified for the probe-committed row", inventory.InventoryStatus)
	}
	if len(inventory.AudioTracks) != len(duplicateProbedAudio()) {
		t.Fatalf("served audio = %+v, want the probed row's %d tracks, not the duplicate's declared list",
			inventory.AudioTracks, len(duplicateProbedAudio()))
	}
	if len(inventory.SubtitleInventory) == 0 {
		t.Fatal("served no subtitle inventory for the probed row")
	}
}

// TestSessionInventoryTransitionsToVerifiedWhenProbeLandsOnPlanRow pins the
// terminal transition the client waits for: with the committed row still
// unprobed and its deferred probe outstanding, the poll is pending; once the
// probe lands on that row, the same poll is verified. Throughout, the effective
// row stays the committed row and never the duplicate the path lookup resolves.
func TestSessionInventoryTransitionsToVerifiedWhenProbeLandsOnPlanRow(t *testing.T) {
	fx := setupDuplicateInventoryFixture(t, false)
	if err := fx.manager.SetVirtualProbeOutcome(fx.session.ID, probeOutcomePending); err != nil {
		t.Fatalf("SetVirtualProbeOutcome: %v", err)
	}

	pending := fx.inventory(t)
	if pending.EffectiveMediaFileID != dupProbedRowID {
		t.Fatalf("pending effective media file id = %d, want the committed row %d (not the duplicate %d)",
			pending.EffectiveMediaFileID, dupProbedRowID, dupResolvedRowID)
	}
	if pending.InventoryStatus != string(ProbeProvenancePending) {
		t.Fatalf("pre-probe inventory status = %q, want pending", pending.InventoryStatus)
	}

	// The deferred probe lands: the durable evidence write stamps the committed
	// row and advances the session's carried inventory to the verified tracks,
	// exactly as the propagation does after a real enumeration.
	probedAt := time.Now().UTC()
	verified := duplicateProbedAudio()
	fx.resolver.update(dupProbedRowID, func(f *models.MediaFile) {
		f.AudioTracks = verified
		f.SubtitleTracks = []models.SubtitleTrack{{Index: 2, Codec: "subrip", Language: "eng"}}
		f.ProbeUpdatedAt = &probedAt
	})
	if err := fx.manager.UpdateStreamState(fx.session.ID, playback.SessionStreamState{
		VirtualSourceSet:              true,
		VirtualSourceURI:              dupCandidateURI,
		VirtualSubtitleEvidenceSet:    true,
		VirtualSubtitleEvidenceURI:    dupCandidateURI,
		VirtualSubtitleEvidenceFileID: dupProbedRowID,
		VirtualAudioTracks:            verified,
	}); err != nil {
		t.Fatalf("UpdateStreamState after probe: %v", err)
	}

	verifiedInventory := fx.inventory(t)
	if verifiedInventory.EffectiveMediaFileID != dupProbedRowID {
		t.Fatalf("verified effective media file id = %d, want the committed row %d", verifiedInventory.EffectiveMediaFileID, dupProbedRowID)
	}
	if verifiedInventory.InventoryStatus != string(ProbeProvenanceVerified) {
		t.Fatalf("post-probe inventory status = %q, want verified", verifiedInventory.InventoryStatus)
	}
	if len(verifiedInventory.AudioTracks) != len(verified) {
		t.Fatalf("post-probe served audio = %+v, want the verified %d tracks", verifiedInventory.AudioTracks, len(verified))
	}
	if verifiedInventory.InventoryRevision == pending.InventoryRevision {
		t.Fatalf("inventory revision did not move when the probe landed: %q", verifiedInventory.InventoryRevision)
	}
}

// TestSessionInventoryKeepsRefusingDifferentReleaseEvidence keeps the
// fail-closed half: when the session is bound to a genuinely different candidate
// whose row does not name the probed row's release, the probed row's verified
// inventory must not be served under the bound identity. The resolution must not
// adopt another release's row just because a plan/evidence row exists.
func TestSessionInventoryKeepsRefusingDifferentReleaseEvidence(t *testing.T) {
	fx := setupDuplicateInventoryFixture(t, true)

	// Rebind the session to a different concrete candidate whose row exists and
	// is unprobed, carrying no evidence. The plan's committed row still names the
	// original candidate, so it must not speak for this release.
	other := models.MediaFile{
		ID: dupOtherRowID, ContentID: "episode-dup", FilePath: dupOtherURI,
		Container: "virtual", VirtualOwnerInstallationID: 5,
		AudioTracks: []models.AudioTrack{{Index: 0, Codec: "aac", Channels: 2, Language: "eng", Default: true}},
	}
	fx.resolver.mu.Lock()
	fx.resolver.byID[dupOtherRowID] = other
	fx.resolver.byPath[dupOtherURI] = dupOtherRowID
	fx.resolver.mu.Unlock()
	if err := fx.manager.UpdateStreamState(fx.session.ID, playback.SessionStreamState{
		VirtualSourceSet:              true,
		VirtualSourceOwnershipSet:     true,
		VirtualSourceURI:              dupOtherURI,
		VirtualSubtitleEvidenceSet:    true,
		VirtualSubtitleEvidenceURI:    dupOtherURI,
		VirtualSubtitleEvidenceFileID: dupOtherRowID,
		VirtualAudioTracks:            other.AudioTracks,
	}); err != nil {
		t.Fatalf("UpdateStreamState: %v", err)
	}
	if err := fx.manager.SetEffectiveMediaFileID(fx.session.ID, dupOtherRowID); err != nil {
		t.Fatalf("SetEffectiveMediaFileID: %v", err)
	}

	inventory := fx.inventory(t)
	if inventory.EffectiveMediaFileID == dupProbedRowID {
		t.Fatalf("inventory adopted the plan's row %d for a different bound release", dupProbedRowID)
	}
	if inventory.InventoryStatus == string(ProbeProvenanceVerified) {
		t.Fatalf("inventory reported verified from a different release's probed row: %q", inventory.InventoryStatus)
	}
	if len(inventory.AudioTracks) == len(duplicateProbedAudio()) {
		t.Fatalf("served the probed release's %d tracks for a different bound release", len(duplicateProbedAudio()))
	}
}
