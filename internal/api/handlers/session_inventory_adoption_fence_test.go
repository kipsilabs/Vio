package handlers

import (
	"context"
	"sync"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// lookupMutationResolver wraps the canonical resolver and fires a one-shot
// callback the first time the exact-path lookup runs. The late adoption resolves
// the concrete owner row through GetByPath, so the callback lands exactly in the
// window between the binding capture and the CAS - the window a rotation or a
// row-only move would otherwise escape.
type lookupMutationResolver struct {
	*canonicalCollectionResolver
	fire func()
	once sync.Once
}

func (r *lookupMutationResolver) GetByPath(ctx context.Context, path string) (*models.MediaFile, error) {
	r.once.Do(r.fire)
	return r.canonicalCollectionResolver.GetByPath(ctx, path)
}

// lateAdoptionSession builds a live session bound to a provider-neutral row with
// the given concrete candidate URI, mirroring the first-ever auto start shape:
// the effective row is the neutral sibling while the binding names the concrete
// ?result= candidate, and the evidence anchor points at the neutral row.
func lateAdoptionSession(t *testing.T, manager *playback.SessionManager, neutral models.MediaFile, candidateURI string) *playback.Session {
	t.Helper()
	session, err := manager.StartSession(1, "profile-1", neutral.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := manager.SetVirtualSource(session.ID, candidateURI, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	if err := manager.UpdateStreamState(session.ID, playback.SessionStreamState{
		VirtualSourceSet:              true,
		VirtualSourceOwnershipSet:     true,
		VirtualSourceURI:              candidateURI,
		VirtualSourceRevision:         "planrev",
		VirtualSubtitleEvidenceSet:    true,
		VirtualSubtitleEvidenceURI:    neutral.FilePath,
		VirtualSubtitleEvidenceFileID: neutral.ID,
		VirtualAudioTracks:            []models.AudioTrack{{Index: 0, Codec: "eac3", Channels: 6, Language: "jpn", Default: true}},
	}); err != nil {
		t.Fatalf("UpdateStreamState: %v", err)
	}
	return session
}

// TestLateAdoptionRejectedWhenBindingRotatesDuringLookup is the
// rotation-during-lookup race: the caller captures the full binding, the
// owner-row lookup then runs a catalog round trip during which the session
// rotates to a different candidate (a generation bump), and the adoption's
// full-binding CAS must reject rather than move the rotated binding onto the old
// owner row.
func TestLateAdoptionRejectedWhenBindingRotatesDuringLookup(t *testing.T) {
	collection, result := autoResultRows()
	resolver := newCanonicalCollectionResolver(collection, result)
	manager := playback.NewSessionManager(0, 0)
	session := lateAdoptionSession(t, manager, collection, autoResultURI)
	rotatedURI := autoCollectionURI + "&result=rotated1"

	wrapped := &lookupMutationResolver{
		canonicalCollectionResolver: resolver,
		fire: func() {
			if err := manager.SetVirtualSource(session.ID, rotatedURI, 5); err != nil {
				t.Errorf("SetVirtualSource: %v", err)
			}
		},
	}

	live, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	served, _ := reanchorLateVirtualOwnerRow(context.Background(), wrapped, manager, &collection, live, autoResultURI)
	if served.ID != collection.ID {
		t.Fatalf("served row = %d, want the neutral row %d when the binding rotated during the owner lookup", served.ID, collection.ID)
	}
	after, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession after: %v", err)
	}
	if after.VirtualSourceURI != rotatedURI {
		t.Fatalf("binding = %q, want the rotated candidate %q", after.VirtualSourceURI, rotatedURI)
	}
	if after.MediaFileID != collection.ID {
		t.Fatalf("effective row = %d, want the neutral row %d untouched after a rejected rotation", after.MediaFileID, collection.ID)
	}
}

// TestLateAdoptionRejectedOnRowOnlyMoveDuringLookup is the rejected-CAS case
// where the binding generation does NOT move: SetEffectiveMediaFileID points the
// session at the owner row during the lookup. A generation-only fence would miss
// it, so the adoption CAS must compare the effective row and reject; the caller
// must then keep serving the file it was handed rather than the rejected owner.
func TestLateAdoptionRejectedOnRowOnlyMoveDuringLookup(t *testing.T) {
	collection, result := autoResultRows()
	resolver := newCanonicalCollectionResolver(collection, result)
	manager := playback.NewSessionManager(0, 0)
	session := lateAdoptionSession(t, manager, collection, autoResultURI)
	generation, err := manager.VirtualSourceGeneration(session.ID)
	if err != nil {
		t.Fatalf("VirtualSourceGeneration: %v", err)
	}

	wrapped := &lookupMutationResolver{
		canonicalCollectionResolver: resolver,
		fire: func() {
			if err := manager.SetEffectiveMediaFileID(session.ID, result.ID); err != nil {
				t.Errorf("SetEffectiveMediaFileID: %v", err)
			}
		},
	}

	live, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	served, _ := reanchorLateVirtualOwnerRow(context.Background(), wrapped, manager, &collection, live, autoResultURI)
	if served.ID != collection.ID {
		t.Fatalf("served row = %d, want the handed neutral row %d after a rejected row-only CAS", served.ID, collection.ID)
	}
	after, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession after: %v", err)
	}
	if after.MediaFileID != result.ID {
		t.Fatalf("effective row = %d, want the mutated row %d", after.MediaFileID, result.ID)
	}
	nowGeneration, err := manager.VirtualSourceGeneration(session.ID)
	if err != nil {
		t.Fatalf("VirtualSourceGeneration after: %v", err)
	}
	if nowGeneration != generation {
		t.Fatalf("generation moved %d -> %d; this case must not advance it", generation, nowGeneration)
	}
}

// TestLateAdoptionRefusesConcreteToConcreteSwap pins the neutral-only gate: a
// session whose effective row already names a concrete ?result= release must
// never be adopted onto a different concrete owner row. The old same-release
// guard compared only the bound candidate, so a different concrete release
// passed and the session's row was silently swapped.
func TestLateAdoptionRefusesConcreteToConcreteSwap(t *testing.T) {
	_, result := autoResultRows()
	other := models.MediaFile{
		ID:                         5316899,
		ContentID:                  "movie-tt-auto",
		FilePath:                   autoCollectionURI + "&result=other999",
		Container:                  "virtual",
		VirtualOwnerInstallationID: 5,
		MediaFolderID:              3,
		AudioTracks:                []models.AudioTrack{{Index: 0, Codec: "eac3", Channels: 6, Language: "eng", Default: true}},
	}
	resolver := newCanonicalCollectionResolver(other, result)
	manager := playback.NewSessionManager(0, 0)
	// The session's effective row is the concrete "other" release while its
	// binding names the concrete result candidate.
	session := lateAdoptionSession(t, manager, other, autoResultURI)

	live, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	served, _ := reanchorLateVirtualOwnerRow(context.Background(), resolver, manager, &other, live, autoResultURI)
	if served.ID != other.ID {
		t.Fatalf("served row = %d, want the concrete row %d; a concrete effective row must not be swapped onto another owner", served.ID, other.ID)
	}
	after, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession after: %v", err)
	}
	if after.MediaFileID != other.ID || after.VirtualSubtitleEvidenceFileID != other.ID {
		t.Fatalf("session row = %d / evidence %d, want the concrete row %d for both", after.MediaFileID, after.VirtualSubtitleEvidenceFileID, other.ID)
	}
}

// TestLateAdoptionDeferredWhileProbePending is the
// adoption-during-pending-propagation lifecycle: while the deferred probe for
// the neutral binding is pending, the adoption is refused, so the probe's
// full-binding propagation still lands on the unchanged binding instead of being
// rejected and stranded to the convergence watchdog. Once the lifecycle is
// terminal the adoption applies.
func TestLateAdoptionDeferredWhileProbePending(t *testing.T) {
	collection, result := autoResultRows()
	resolver := newCanonicalCollectionResolver(collection, result)
	manager := playback.NewSessionManager(0, 0)
	session := lateAdoptionSession(t, manager, collection, autoResultURI)

	binding, err := manager.VirtualSourceBinding(session.ID)
	if err != nil {
		t.Fatalf("VirtualSourceBinding: %v", err)
	}
	if applied, err := manager.ArmVirtualProbePendingIfGeneration(session.ID, binding.Generation); err != nil || !applied {
		t.Fatalf("ArmVirtualProbePendingIfGeneration = %v, %v; want armed", applied, err)
	}

	live, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	served, _ := reanchorLateVirtualOwnerRow(context.Background(), resolver, manager, &collection, live, autoResultURI)
	if served.ID != collection.ID {
		t.Fatalf("served row = %d, want the neutral row %d while the probe is pending", served.ID, collection.ID)
	}
	pending, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession pending: %v", err)
	}
	if pending.MediaFileID != collection.ID {
		t.Fatalf("effective row = %d, want the neutral row %d untouched while pending", pending.MediaFileID, collection.ID)
	}

	// The deferred probe's own propagation carries the binding captured at
	// admission; because the adoption never moved the row, it still lands.
	applied, err := manager.SetVirtualProbeVerifiedInventoryIfBinding(
		session.ID, binding, "rev-verified", verifiedCanonicalAudio(), verifiedCanonicalSubtitles(), nil,
	)
	if err != nil || !applied {
		t.Fatalf("SetVirtualProbeVerifiedInventoryIfBinding = %v, %v; want the probe to land on the unchanged binding", applied, err)
	}

	// The lifecycle is terminal now, so the adoption applies and re-anchors.
	terminal, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession terminal: %v", err)
	}
	served, refreshed := reanchorLateVirtualOwnerRow(context.Background(), resolver, manager, &collection, terminal, autoResultURI)
	if served.ID != result.ID {
		t.Fatalf("served row = %d, want the owner row %d once the lifecycle is terminal", served.ID, result.ID)
	}
	if refreshed == nil || refreshed.MediaFileID != result.ID || refreshed.VirtualSubtitleEvidenceFileID != result.ID {
		t.Fatalf("refreshed session = %+v, want both anchors on the owner row %d", refreshed, result.ID)
	}
	final, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession final: %v", err)
	}
	if final.MediaFileID != result.ID || final.VirtualSubtitleEvidenceFileID != result.ID || final.VirtualSubtitleEvidenceURI != autoResultURI {
		t.Fatalf("final anchors = row %d / evidence %d / uri %q, want the owner row %d and the candidate URI",
			final.MediaFileID, final.VirtualSubtitleEvidenceFileID, final.VirtualSubtitleEvidenceURI, result.ID)
	}
}
