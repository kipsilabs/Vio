package handlers

import (
	"context"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

// TestRotateTargetRefusesDuplicateSiblingRow pins item 3: two catalog rows for
// one release (same durable identity) that share a candidate URI must not have
// their inventories silently swapped. When the probed candidate path is owned by
// a duplicate sibling row, the rotation is refused and the evidence stays on the
// requested row; the SQL adoption fence then refuses the write, so no sibling is
// repainted.
func TestRotateTargetRefusesDuplicateSiblingRow(t *testing.T) {
	const (
		neutral      = "virtual://movie/tt-dup"
		requestedURI = neutral + "?result=a"
		duplicateURI = neutral + "?result=b"
		content      = "movie-dup"
		folderID     = 9
		ownerID      = 5
	)
	// Same durable identity: the two rows are the same release.
	requested := &models.MediaFile{
		ID: 101, ContentID: content, FilePath: requestedURI,
		MediaFolderID: folderID, VirtualOwnerInstallationID: ownerID, ProbeSource: "virtual",
		ProviderVideoHash: "hash-shared", ProviderReleaseName: "Movie.2024.2160p",
	}
	duplicate := &models.MediaFile{
		ID: 102, ContentID: content, FilePath: duplicateURI,
		MediaFolderID: folderID, VirtualOwnerInstallationID: ownerID, ProbeSource: "virtual",
		ProviderVideoHash: "hash-shared", ProviderReleaseName: "Movie.2024.2160p",
	}
	probed := &models.MediaFile{
		FilePath:    duplicateURI,
		AudioTracks: []models.AudioTrack{{Codec: "eac3", Channels: 6, Language: "eng"}},
	}
	h := &PlaybackHandler{
		fileResolver: byPathPlaybackFileResolver{byPath: map[string]*models.MediaFile{duplicateURI: duplicate}},
	}

	identityRow, evidence, ok := h.virtualProbeEvidenceRotateTarget(context.Background(), requested, duplicateURI, probed)
	if ok {
		t.Fatal("duplicate-sibling rotation was accepted; the duplicate row would be repainted")
	}
	if identityRow != nil || evidence != nil {
		t.Fatalf("refused rotation returned (%v, %v), want (nil, nil)", identityRow, evidence)
	}
}

// TestRotateTargetStillRotatesDistinctRelease proves the guard is scoped to
// duplicate rows of one release: a genuinely different release whose row owns
// the candidate path still rotates, preserving the alternate-version behavior.
func TestRotateTargetStillRotatesDistinctRelease(t *testing.T) {
	const (
		neutral      = "virtual://movie/tt-alt"
		requestedURI = neutral + "?result=a"
		ownerURI     = neutral + "?result=b"
		content      = "movie-alt"
		folderID     = 9
		ownerID      = 5
	)
	requested := &models.MediaFile{
		ID: 201, ContentID: content, FilePath: requestedURI,
		MediaFolderID: folderID, VirtualOwnerInstallationID: ownerID, ProbeSource: "virtual",
		ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.A.2160p",
	}
	owner := &models.MediaFile{
		ID: 202, ContentID: content, FilePath: ownerURI,
		MediaFolderID: folderID, VirtualOwnerInstallationID: ownerID, ProbeSource: "virtual",
		ProviderVideoHash: "hash-b", ProviderReleaseName: "Movie.B.2160p",
	}
	probed := &models.MediaFile{
		FilePath:    ownerURI,
		AudioTracks: []models.AudioTrack{{Codec: "eac3", Channels: 6, Language: "eng"}},
	}
	h := &PlaybackHandler{
		fileResolver: byPathPlaybackFileResolver{byPath: map[string]*models.MediaFile{ownerURI: owner}},
	}

	identityRow, evidence, ok := h.virtualProbeEvidenceRotateTarget(context.Background(), requested, ownerURI, probed)
	if !ok {
		t.Fatal("distinct-release rotation was refused")
	}
	if identityRow == nil || identityRow.ID != owner.ID {
		t.Fatalf("rotation identity row = %v, want the owner row %d", identityRow, owner.ID)
	}
	if evidence == nil || len(evidence.AudioTracks) != 1 {
		t.Fatalf("rotated evidence = %+v, want the probed tracks retained", evidence)
	}
}

// TestRotateTargetRefusesMixedTierDuplicateRow pins the compatible-tier
// comparison: a strongest-tier key would read a name-only row and a GUID row of
// the same release as distinct (different keys) and rotate onto the duplicate.
// The tier-by-tier predicate recognizes the same release and refuses, so the
// sibling is not repainted. A genuinely different release in the name tier stays
// a rotation.
func TestRotateTargetRefusesMixedTierDuplicateRow(t *testing.T) {
	const (
		neutral      = "virtual://movie/tt-mixed"
		requestedURI = neutral + "?result=a"
		ownerURI     = neutral + "?result=b"
		content      = "movie-mixed"
		folderID     = 9
		ownerID      = 5
	)
	// The requested row carries only the durable name+size tier; the owner row
	// carries the same name+size plus a source GUID for the same release. A
	// strongest-tier key reads them as different (name+size vs guid), so it
	// misses the duplicate and rotates; the tier-by-tier comparison sees the
	// shared name tier and refuses.
	requested := &models.MediaFile{
		ID: 301, ContentID: content, FilePath: requestedURI,
		MediaFolderID: folderID, VirtualOwnerInstallationID: ownerID, ProbeSource: "virtual",
		ProviderReleaseName: "Movie.2024.1080p", ProviderReleaseSize: 8_000_000_000,
	}
	owner := &models.MediaFile{
		ID: 302, ContentID: content, FilePath: ownerURI,
		MediaFolderID: folderID, VirtualOwnerInstallationID: ownerID, ProbeSource: "virtual",
		ProviderGUID: "guid-shared", ProviderReleaseName: "Movie.2024.1080p", ProviderReleaseSize: 8_000_000_000,
	}
	probed := &models.MediaFile{
		FilePath:    ownerURI,
		AudioTracks: []models.AudioTrack{{Codec: "eac3", Channels: 6, Language: "eng"}},
	}
	h := &PlaybackHandler{
		fileResolver: byPathPlaybackFileResolver{byPath: map[string]*models.MediaFile{ownerURI: owner}},
	}

	identityRow, evidence, ok := h.virtualProbeEvidenceRotateTarget(context.Background(), requested, ownerURI, probed)
	if ok {
		t.Fatal("mixed-tier duplicate rotation was accepted; a strongest-tier key missed the duplicate")
	}
	if identityRow != nil || evidence != nil {
		t.Fatalf("refused mixed-tier rotation returned (%v, %v), want (nil, nil)", identityRow, evidence)
	}
}

// TestVirtualSiblingOwnerSharesRelease pins the durable-identity predicate that
// separates duplicates of one release from genuinely distinct releases: matching
// tiers across a compatible pair compare equal, and an identity-less row is not
// proof.
func TestVirtualSiblingOwnerSharesRelease(t *testing.T) {
	cases := []struct {
		name string
		a, b *models.MediaFile
		want bool
	}{
		{"same video hash", &models.MediaFile{ProviderVideoHash: "h1"}, &models.MediaFile{ProviderVideoHash: "h1"}, true},
		{"different video hash", &models.MediaFile{ProviderVideoHash: "h1"}, &models.MediaFile{ProviderVideoHash: "h2"}, false},
		{"same guid", &models.MediaFile{ProviderGUID: "g1"}, &models.MediaFile{ProviderGUID: "g1"}, true},
		{"same name and size", &models.MediaFile{ProviderReleaseName: "Movie.2024", ProviderReleaseSize: 100}, &models.MediaFile{ProviderReleaseName: "Movie.2024", ProviderReleaseSize: 100}, true},
		{"same name different size", &models.MediaFile{ProviderReleaseName: "Movie.2024", ProviderReleaseSize: 100}, &models.MediaFile{ProviderReleaseName: "Movie.2024", ProviderReleaseSize: 200}, false},
		{"name-only vs name+guid is the same release", &models.MediaFile{ProviderReleaseName: "Movie.2024", ProviderReleaseSize: 100}, &models.MediaFile{ProviderGUID: "g1", ProviderReleaseName: "Movie.2024", ProviderReleaseSize: 100}, true},
		{"name-only vs name+hash is the same release", &models.MediaFile{ProviderReleaseName: "Movie.2024", ProviderReleaseSize: 100}, &models.MediaFile{ProviderVideoHash: "h1", ProviderReleaseName: "Movie.2024", ProviderReleaseSize: 100}, true},
		{"disjoint tiers are not proof", &models.MediaFile{ProviderReleaseName: "Movie.2024", ProviderReleaseSize: 100}, &models.MediaFile{ProviderGUID: "g1"}, false},
		{"hash vs different hash is not the same release", &models.MediaFile{ProviderVideoHash: "h1", ProviderReleaseName: "Movie.2024"}, &models.MediaFile{ProviderVideoHash: "h2", ProviderReleaseName: "Movie.2024"}, false},
		{"identity-less is not proof", &models.MediaFile{}, &models.MediaFile{}, false},
		{"one identity-less is not proof", &models.MediaFile{ProviderVideoHash: "h1"}, &models.MediaFile{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := virtualSiblingOwnerSharesRelease(tc.a, tc.b); got != tc.want {
				t.Fatalf("shares release = %v, want %v", got, tc.want)
			}
		})
	}
}
