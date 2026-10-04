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

// TestVirtualSiblingOwnerSharesRelease pins the durable-identity predicate that
// separates duplicates of one release from genuinely distinct releases: only
// matching strongest tiers compare equal, and an identity-less row is not proof.
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
