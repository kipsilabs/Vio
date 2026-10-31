package playback

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

// A candidate rotation must not leave evidence that describes the previous
// release bound to the new candidate: the inventories stay for the planner's
// remap provenance, but their anchor moves so the serve path can prove the
// mismatch and discard them. The captured evidence owner survives the move with
// the anchor, so a later seek reanchor can restore the frozen candidate's own
// installation instead of the moved binding's.
func TestSetVirtualSourceRotationMovesEvidenceAnchor(t *testing.T) {
	manager := NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", 42, PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	oldURI := "virtual://movie/tt1?result=candidate-a"
	if err := manager.UpdateStreamState(session.ID, SessionStreamState{
		VirtualSourceSet:           true,
		VirtualSourceOwnershipSet:  true,
		VirtualSourceURI:           oldURI,
		VirtualSourceRevision:      "rev-a",
		VirtualSubtitleEvidenceSet: true,
		VirtualSubtitleEvidenceURI: oldURI,
		VirtualSubtitleTracks:      []models.SubtitleTrack{{Index: 0, Codec: "subrip", Language: "fra"}},
		VirtualExternalSubtitles:   nil,
		VirtualAudioTracks:         []models.AudioTrack{{Codec: "aac", Language: "fra"}},
	}); err != nil {
		t.Fatalf("UpdateStreamState: %v", err)
	}

	newURI := "virtual://movie/tt1?result=candidate-b"
	if err := manager.SetVirtualSource(session.ID, newURI, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	current, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if current.VirtualSourceURI != newURI {
		t.Fatalf("bound URI = %q, want %q", current.VirtualSourceURI, newURI)
	}
	if current.VirtualSubtitleEvidenceURI != oldURI {
		t.Fatalf("evidence anchor = %q, want it left at the previous candidate %q", current.VirtualSubtitleEvidenceURI, oldURI)
	}
	if current.VirtualSourceRevision != "" {
		t.Fatalf("rotation kept revision %q; want empty until the planner re-probes", current.VirtualSourceRevision)
	}
	if !current.VirtualSubtitleEvidenceSet {
		t.Fatal("rotation dropped the evidence outright; the mismatch must stay provable")
	}
}

// The evidence owner is captured with the evidence anchor and, like the URI and
// row id, deliberately left untouched by a binding move: the two diverge after a
// cross-installation move, so a seek reanchor must restore the frozen
// candidate's own owner rather than the moved binding's.
func TestSetVirtualSourceRotationKeepsEvidenceOwner(t *testing.T) {
	manager := NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", 42, PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	uri := "virtual://movie/tt1?result=candidate-a"
	if err := manager.UpdateStreamState(session.ID, SessionStreamState{
		VirtualSourceSet:                           true,
		VirtualSourceOwnershipSet:                  true,
		VirtualSourceURI:                           uri,
		VirtualSourceOwnerInstallationID:           5,
		VirtualSubtitleEvidenceSet:                 true,
		VirtualSubtitleEvidenceURI:                 uri,
		VirtualSubtitleEvidenceOwnerInstallationID: 5,
		VirtualSubtitleTracks:                      []models.SubtitleTrack{{Index: 0, Codec: "subrip"}},
	}); err != nil {
		t.Fatalf("UpdateStreamState: %v", err)
	}
	if err := manager.SetVirtualSource(session.ID, "virtual://movie/tt1?result=candidate-b", 105); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	current, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if current.VirtualSourceOwnerInstallationID != 105 {
		t.Fatalf("binding owner = %d, want the moved owner 105", current.VirtualSourceOwnerInstallationID)
	}
	if current.VirtualSubtitleEvidenceOwnerInstallationID != 5 {
		t.Fatalf("evidence owner = %d, want the previous candidate's 5 kept with the anchor", current.VirtualSubtitleEvidenceOwnerInstallationID)
	}
}

// A rebind to the same candidate is not a rotation and must not disturb the
// evidence or its revision.
func TestSetVirtualSourceSameCandidateKeepsEvidence(t *testing.T) {
	manager := NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", 42, PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	uri := "virtual://movie/tt1?result=candidate-a"
	if err := manager.UpdateStreamState(session.ID, SessionStreamState{
		VirtualSourceSet:           true,
		VirtualSourceOwnershipSet:  true,
		VirtualSourceURI:           uri,
		VirtualSourceRevision:      "rev-a",
		VirtualSubtitleEvidenceSet: true,
		VirtualSubtitleEvidenceURI: uri,
		VirtualSubtitleTracks:      []models.SubtitleTrack{{Index: 0, Codec: "subrip"}},
	}); err != nil {
		t.Fatalf("UpdateStreamState: %v", err)
	}
	if err := manager.SetVirtualSource(session.ID, uri, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	current, _ := manager.GetSession(session.ID)
	if current.VirtualSourceRevision != "rev-a" || !current.VirtualSubtitleEvidenceSet {
		t.Fatalf("same-candidate rebind disturbed evidence: revision=%q set=%v", current.VirtualSourceRevision, current.VirtualSubtitleEvidenceSet)
	}
}

// A v3 stream snapshot whose effective file is not virtual owns the binding
// outright, so stale evidence from the previous candidate is cleared instead of
// lingering into the next serve.
func TestApplyStreamStateNonVirtualSnapshotClearsEvidence(t *testing.T) {
	manager := NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", 42, PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	uri := "virtual://movie/tt1?result=candidate-a"
	if err := manager.UpdateStreamState(session.ID, SessionStreamState{
		VirtualSourceSet:                           true,
		VirtualSourceOwnershipSet:                  true,
		VirtualSourceURI:                           uri,
		VirtualSourceRevision:                      "rev-a",
		VirtualSubtitleEvidenceSet:                 true,
		VirtualSubtitleEvidenceURI:                 uri,
		VirtualSubtitleEvidenceOwnerInstallationID: 5,
		VirtualSubtitleTracks:                      []models.SubtitleTrack{{Index: 0, Codec: "subrip"}},
	}); err != nil {
		t.Fatalf("UpdateStreamState: %v", err)
	}

	// The replan lands on a non-virtual effective file: the snapshot carries no
	// virtual URI and no evidence.
	if err := manager.UpdateStreamState(session.ID, SessionStreamState{
		VirtualSourceSet:          true,
		VirtualSourceOwnershipSet: true,
	}); err != nil {
		t.Fatalf("UpdateStreamState: %v", err)
	}
	current, _ := manager.GetSession(session.ID)
	if current.VirtualSubtitleEvidenceSet {
		t.Fatalf("non-virtual snapshot kept evidence set")
	}
	if len(current.VirtualSubtitleTracks) != 0 || current.VirtualSubtitleEvidenceURI != "" {
		t.Fatalf("non-virtual snapshot kept stale inventory: %+v uri=%q", current.VirtualSubtitleTracks, current.VirtualSubtitleEvidenceURI)
	}
	if current.VirtualSubtitleEvidenceOwnerInstallationID != 0 {
		t.Fatalf("non-virtual snapshot kept the evidence owner %d", current.VirtualSubtitleEvidenceOwnerInstallationID)
	}
}

// A legacy partial update must not clear an established binding's evidence:
// those updates historically only ever overwrote, and their VirtualSourceSet
// carries no evidence to replace what is there.
func TestApplyStreamStateLegacyPartialUpdateKeepsEvidence(t *testing.T) {
	manager := NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", 42, PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	uri := "virtual://movie/tt1?result=candidate-a"
	if err := manager.UpdateStreamState(session.ID, SessionStreamState{
		VirtualSourceSet:           true,
		VirtualSourceOwnershipSet:  true,
		VirtualSourceURI:           uri,
		VirtualSubtitleEvidenceSet: true,
		VirtualSubtitleEvidenceURI: uri,
		VirtualSubtitleTracks:      []models.SubtitleTrack{{Index: 0, Codec: "subrip"}},
	}); err != nil {
		t.Fatalf("UpdateStreamState: %v", err)
	}
	// Legacy partial shape: sets the binding but carries no evidence and does
	// not claim ownership of the whole snapshot.
	if err := manager.UpdateStreamState(session.ID, SessionStreamState{VirtualSourceSet: true, VirtualSourceURI: uri}); err != nil {
		t.Fatalf("UpdateStreamState: %v", err)
	}
	current, _ := manager.GetSession(session.ID)
	if !current.VirtualSubtitleEvidenceSet || len(current.VirtualSubtitleTracks) != 1 {
		t.Fatal("legacy partial update cleared the established evidence")
	}
}

func TestSubtitleLayoutsEqualIncludingExternalDetectsSidecarRotation(t *testing.T) {
	embedded := []models.SubtitleTrack{{Index: 2, Codec: "subrip", Language: "eng"}}
	aExternal := []models.ExternalSubtitle{{Path: "/subs/en.srt", Language: "eng", Format: "srt"}}
	bExternal := []models.ExternalSubtitle{{Path: "/subs/fr.srt", Language: "fra", Format: "srt"}}

	if SubtitleLayoutsEqualIncludingExternal(embedded, aExternal, embedded, aExternal) {
		// identical layouts are equal
	} else {
		t.Fatal("identical full layouts compared unequal")
	}
	if SubtitleLayoutsEqualIncludingExternal(embedded, aExternal, embedded, bExternal) {
		t.Fatal("a rotated sidecar layout compared equal; drift would go undetected")
	}
	// Embedded equality alone must not mask an external change.
	if SubtitleLayoutsEqual(embedded, embedded) != true {
		t.Fatal("embedded layout sanity check failed")
	}
}

func TestAudioLayoutsEqualDetectsSameRowRotation(t *testing.T) {
	french := []models.AudioTrack{{Index: 1, Codec: "aac", Language: "fra", Layout: "stereo", Channels: 2}}
	english := []models.AudioTrack{
		{Index: 1, Codec: "aac", Language: "eng", Layout: "stereo", Channels: 2},
		{Index: 2, Codec: "ac3", Language: "eng", Layout: "5.1", Channels: 6},
	}
	if !AudioLayoutsEqual(french, french) {
		t.Fatal("identical audio inventories compared unequal")
	}
	if AudioLayoutsEqual(french, english) {
		t.Fatal("a different-language same-row release compared equal; the carried ordinal would not be remapped")
	}
	// A MULTI track's language list is order-independent evidence.
	multiA := []models.AudioTrack{{Language: "mul", Languages: []string{"eng", "fra"}, Codec: "eac3", Channels: 6}}
	multiB := []models.AudioTrack{{Language: "mul", Languages: []string{"fra", "eng"}, Codec: "eac3", Channels: 6}}
	if !AudioLayoutsEqual(multiA, multiB) {
		t.Fatal("reordered MULTI languages compared unequal")
	}
}

// Two releases can agree on language, codec, channels, layout, and container
// index while ordering a main track and a commentary track differently. Titles
// are the only identity evidence that exposes the swap, so a carried ordinal
// must be remapped instead of reusing the position.
func TestAudioLayoutsEqualDetectsSameLanguageTitleOnlyReorder(t *testing.T) {
	mainTrack := func(index int) models.AudioTrack {
		return models.AudioTrack{Index: index, Codec: "aac", Language: "eng", Layout: "stereo", Channels: 2, Title: "Main"}
	}
	commentary := func(index int) models.AudioTrack {
		return models.AudioTrack{Index: index, Codec: "aac", Language: "eng", Layout: "stereo", Channels: 2, Title: "Commentary"}
	}
	mainFirst := []models.AudioTrack{mainTrack(0), commentary(1)}
	commentaryFirst := []models.AudioTrack{commentary(0), mainTrack(1)}
	if AudioLayoutsEqual(mainFirst, commentaryFirst) {
		t.Fatal("main/commentary reorder compared equal; the carried ordinal would silently name the other track")
	}

	// EmbeddedTitle is identity evidence too: providers often leave Title empty
	// and name the stream only in the container's embedded title.
	embeddedMain := models.AudioTrack{Index: 0, Codec: "eac3", Language: "eng", Channels: 6, EmbeddedTitle: "English 5.1"}
	embeddedCommentary := models.AudioTrack{Index: 0, Codec: "eac3", Language: "eng", Channels: 6, EmbeddedTitle: "English Commentary"}
	if AudioLayoutsEqual([]models.AudioTrack{embeddedMain}, []models.AudioTrack{embeddedCommentary}) {
		t.Fatal("embedded-title-only difference compared equal")
	}

	// Case and surrounding whitespace are not identity changes.
	reformatted := models.AudioTrack{Index: 0, Codec: "AAC", Language: "ENG", Layout: " Stereo ", Channels: 2, Title: " main "}
	if !AudioLayoutsEqual([]models.AudioTrack{mainTrack(0)}, []models.AudioTrack{reformatted}) {
		t.Fatal("case/whitespace-only title difference compared unequal")
	}
}
