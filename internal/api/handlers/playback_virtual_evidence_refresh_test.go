package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/resolver"
)

type evidenceRefreshResolver struct {
	file *models.MediaFile
}

func (r evidenceRefreshResolver) GetByID(context.Context, int) (*models.MediaFile, error) {
	return r.file, nil
}

func TestRefreshVirtualCatalogFileForEvidenceUsesCurrentRowRevision(t *testing.T) {
	old := &models.MediaFile{
		ID: 7, ContentID: "series-1", MediaFolderID: 3, VirtualOwnerInstallationID: 5,
		FilePath: "virtual://series/1/2?result=old",
	}
	fresh := *old
	fresh.FilePath = "virtual://series/1/2?result=new"
	fresh.UpdatedAt = time.Now()
	h := &PlaybackHandler{fileResolver: evidenceRefreshResolver{file: &fresh}}

	got := h.refreshVirtualCatalogFileForEvidence(context.Background(), old)
	if got != &fresh {
		t.Fatalf("refresh returned %#v, want the current row", got)
	}
}

func TestRefreshVirtualCatalogFileForEvidenceRefusesNewerProbeEvidence(t *testing.T) {
	old := &models.MediaFile{
		ID: 7, ContentID: "series-1", MediaFolderID: 3, VirtualOwnerInstallationID: 5,
		FilePath: "virtual://series/1/2?result=old",
	}
	stamp := time.Now()
	fresh := *old
	fresh.FilePath = "virtual://series/1/2?result=new"
	fresh.ProbeUpdatedAt = &stamp
	h := &PlaybackHandler{fileResolver: evidenceRefreshResolver{file: &fresh}}

	if got := h.refreshVirtualCatalogFileForEvidence(context.Background(), old); got != old {
		t.Fatalf("refresh returned %#v, want the stale snapshot to keep the superseded path", got)
	}
}

func TestRefreshVirtualCatalogFileForEvidenceRefusesMovedIdentity(t *testing.T) {
	old := &models.MediaFile{
		ID: 7, ContentID: "series-1", MediaFolderID: 3, VirtualOwnerInstallationID: 5,
		FilePath: "virtual://series/1/2?result=old",
	}
	fresh := *old
	fresh.ContentID = "series-2"
	h := &PlaybackHandler{fileResolver: evidenceRefreshResolver{file: &fresh}}

	if got := h.refreshVirtualCatalogFileForEvidence(context.Background(), old); got != old {
		t.Fatalf("refresh returned %#v, want the old snapshot for a moved identity", got)
	}
}

func TestRefreshVirtualCatalogFileForEvidenceRefusesDifferentEpisodeID(t *testing.T) {
	old := &models.MediaFile{
		ID: 7, ContentID: "series-1", EpisodeID: "episode-1", MediaFolderID: 3, VirtualOwnerInstallationID: 5,
		FilePath: "virtual://series/1/2?result=old",
	}
	fresh := *old
	fresh.EpisodeID = "episode-2"
	h := &PlaybackHandler{fileResolver: evidenceRefreshResolver{file: &fresh}}

	if got := h.refreshVirtualCatalogFileForEvidence(context.Background(), old); got != old {
		t.Fatalf("refresh returned %#v, want the old snapshot for a moved episode", got)
	}
}

func TestRefreshVirtualCatalogFileForEvidenceRefusesDifferentNeutralKey(t *testing.T) {
	old := &models.MediaFile{
		ID: 7, ContentID: "series-1", MediaFolderID: 3, VirtualOwnerInstallationID: 5,
		FilePath: "virtual://series/1/2?result=old",
	}
	fresh := *old
	fresh.FilePath = "virtual://series/1/3?result=new"
	h := &PlaybackHandler{fileResolver: evidenceRefreshResolver{file: &fresh}}

	if got := h.refreshVirtualCatalogFileForEvidence(context.Background(), old); got != old {
		t.Fatalf("refresh returned %#v, want the old snapshot for a moved neutral key", got)
	}
}

func TestVirtualPersistSnapshotMovedRejectsAdoptionWhenProbeStampMoves(t *testing.T) {
	oldStamp := time.Now().Add(-time.Minute)
	newStamp := time.Now()
	original := models.VirtualFilePersistArgs{
		RequireAdopt:     true,
		AdoptPath:        "virtual://movie/tt-a?result=new-adoption",
		ExpectedFilePath: "virtual://movie/tt-a?result=old",
		ProbeUpdatedAt:   &oldStamp,
	}
	current := virtualRowCASState{
		FilePath:       "virtual://movie/tt-a?result=newer",
		ProbeUpdatedAt: &newStamp,
		UpdatedAt:      time.Now(),
	}
	refreshed := models.VirtualFilePersistArgs{ExpectedFilePath: current.FilePath}

	if virtualPersistSnapshotMoved(original, current, refreshed) {
		t.Fatal("stale adoption was allowed to re-base after a newer probe stamp advanced")
	}
}

func TestPersistVirtualProbeEvidenceDurableUsesCurrentRowSnapshot(t *testing.T) {
	old := &models.MediaFile{
		ID: 7, ContentID: "series-1", MediaFolderID: 3, VirtualOwnerInstallationID: 5,
		FilePath: "virtual://series/1/2?result=old", ProbeSource: "virtual",
	}
	fresh := *old
	fresh.FilePath = "virtual://series/1/2?result=new"
	fresh.UpdatedAt = time.Now()
	probed := &models.MediaFile{
		ID:          7,
		FilePath:    fresh.FilePath,
		AudioTracks: []models.AudioTrack{{Codec: "eac3", Channels: 6, Language: "en"}},
	}
	var captured models.VirtualFilePersistArgs
	h := &PlaybackHandler{
		fileResolver: evidenceRefreshResolver{file: &fresh},
		VirtualFileMetadataSaver: func(_ context.Context, args models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
			captured = args
			return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
		},
	}

	if _, committed := h.persistVirtualProbeEvidenceDurable(context.Background(), old, fresh.FilePath, probed, resolver.PersistedIdentityTiers{}, true); !committed {
		t.Fatal("durable evidence write did not commit against the refreshed row")
	}
	if !captured.UpdatedAt.Equal(fresh.UpdatedAt) {
		t.Fatalf("write snapshot = %v, want refreshed %v", captured.UpdatedAt, fresh.UpdatedAt)
	}
	if captured.ExpectedFilePath != fresh.FilePath {
		t.Fatalf("expected path = %q, want refreshed %q", captured.ExpectedFilePath, fresh.FilePath)
	}
}

func TestPersistVirtualProbeEvidenceDurableAdoptsCandidateFromStalePath(t *testing.T) {
	row := &models.MediaFile{
		ID: 7, ContentID: "series-1", MediaFolderID: 3, VirtualOwnerInstallationID: 5,
		FilePath: "virtual://series/1/2?result=old", ProbeSource: "virtual",
	}
	fresh := *row
	fresh.UpdatedAt = time.Now()
	candidate := "virtual://series/1/2?result=new"
	probed := &models.MediaFile{
		ID:          7,
		FilePath:    candidate,
		AudioTracks: []models.AudioTrack{{Codec: "eac3", Channels: 6, Language: "en"}},
	}
	var captured models.VirtualFilePersistArgs
	h := &PlaybackHandler{
		fileResolver: evidenceRefreshResolver{file: &fresh},
		VirtualFileMetadataSaver: func(_ context.Context, args models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
			captured = args
			return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
		},
	}

	if _, committed := h.persistVirtualProbeEvidenceDurable(context.Background(), row, candidate, probed, resolver.PersistedIdentityTiers{}, true); !committed {
		t.Fatal("durable evidence write did not commit")
	}
	if !captured.RequireAdopt || captured.AdoptPath != candidate {
		t.Fatalf("adoption args = RequireAdopt %v adopt %q, want true/%q", captured.RequireAdopt, captured.AdoptPath, candidate)
	}
}

func TestPersistVirtualProbeEvidenceRefusesUnverifiedSizeMismatch(t *testing.T) {
	row := &models.MediaFile{
		ID: 7, ContentID: "movie-1", MediaFolderID: 3,
		FilePath: "virtual://movie/tt7?result=exact", ProbeSource: "virtual",
		FileSize:                      6_330_000_000,
		VirtualExpectedRuntimeMinutes: 51,
	}
	fresh := *row
	fresh.UpdatedAt = time.Now()
	// Probed size contradicts declared size by 12% and duration does not match (30m vs 51m)
	probed := &models.MediaFile{
		ID:               7,
		FilePath:         row.FilePath,
		ProbedFormatSize: 5_570_000_000,
		Duration:         1800,
		AudioTracks:      []models.AudioTrack{{Codec: "eac3", Channels: 6, Language: "en"}},
	}
	h := &PlaybackHandler{
		fileResolver: evidenceRefreshResolver{file: &fresh},
		VirtualFileMetadataSaver: func(_ context.Context, args models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
			return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
		},
	}

	if _, committed := h.persistVirtualProbeEvidenceDurable(context.Background(), row, row.FilePath, probed, resolver.PersistedIdentityTiers{}, true); committed {
		t.Fatal("unverified size-contradicting evidence must be refused from durable catalog persistence")
	}
}

func TestPersistVirtualProbeEvidenceAdoptsCrossReleaseCandidateWithMatchingCandidateSize(t *testing.T) {
	row := &models.MediaFile{
		ID: 7, ContentID: "movie-1", MediaFolderID: 3,
		FilePath: "virtual://movie/tt7?result=old_release", ProbeSource: "virtual",
		FileSize:                      6_330_000_000, // Old release was 6.33GB
		VirtualExpectedRuntimeMinutes: 51,
	}
	fresh := *row
	fresh.UpdatedAt = time.Now()
	newCandidateURI := "virtual://movie/tt7?result=new_release"
	newCandidateTiers := resolver.NewPersistedIdentityTiers("", "", "Movie.New.Release", 5_570_000_000)
	// Probed bytes match the new candidate's declared size (5.57GB)
	probed := &models.MediaFile{
		ID:               7,
		FilePath:         newCandidateURI,
		FileSize:         5_570_000_000,
		ProbedFormatSize: 5_570_000_000,
		Duration:         3072,
		AudioTracks:      []models.AudioTrack{{Codec: "eac3", Channels: 6, Language: "en"}},
	}
	var captured models.VirtualFilePersistArgs
	h := &PlaybackHandler{
		fileResolver: evidenceRefreshResolver{file: &fresh},
		VirtualFileMetadataSaver: func(_ context.Context, args models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
			captured = args
			return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
		},
	}

	if _, committed := h.persistVirtualProbeEvidenceDurable(context.Background(), row, newCandidateURI, probed, newCandidateTiers, true); !committed {
		t.Fatal("legitimate cross-release adoption with matching candidate size must commit")
	}
	if !captured.RequireAdopt || captured.AdoptPath != newCandidateURI {
		t.Fatalf("adoption args = RequireAdopt %v adopt %q, want true/%q", captured.RequireAdopt, captured.AdoptPath, newCandidateURI)
	}
}

func TestPersistVirtualProbeEvidenceAdoptsCrossReleaseCandidateWithUnknownCandidateSize(t *testing.T) {
	row := &models.MediaFile{
		ID: 7, ContentID: "movie-1", MediaFolderID: 3,
		FilePath: "virtual://movie/tt7?result=old_release", ProbeSource: "virtual",
		FileSize:                      6_330_000_000, // Old release was 6.33GB
		VirtualExpectedRuntimeMinutes: 51,
	}
	fresh := *row
	fresh.UpdatedAt = time.Now()
	newCandidateURI := "virtual://movie/tt7?result=new_release_no_size"
	// Candidate carries no declared release size (ReleaseSize = 0)
	newCandidateTiers := resolver.NewPersistedIdentityTiers("", "", "Movie.New.Release", 0)
	probed := &models.MediaFile{
		ID:               7,
		FilePath:         newCandidateURI,
		FileSize:         0,
		ProbedFormatSize: 4_200_000_000, // Differs from old row's 6.33GB
		Duration:         3072,
		AudioTracks:      []models.AudioTrack{{Codec: "eac3", Channels: 6, Language: "en"}},
	}
	var captured models.VirtualFilePersistArgs
	h := &PlaybackHandler{
		fileResolver: evidenceRefreshResolver{file: &fresh},
		VirtualFileMetadataSaver: func(_ context.Context, args models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
			captured = args
			return VirtualFileMetadataUpdateResult{MetadataUpdated: true, RowsAffected: 1}, nil
		},
	}

	if _, committed := h.persistVirtualProbeEvidenceDurable(context.Background(), row, newCandidateURI, probed, newCandidateTiers, true); !committed {
		t.Fatal("cross-release adoption with unknown candidate size must commit without comparing against the old release size")
	}
	if !captured.RequireAdopt || captured.AdoptPath != newCandidateURI {
		t.Fatalf("adoption args = RequireAdopt %v adopt %q, want true/%q", captured.RequireAdopt, captured.AdoptPath, newCandidateURI)
	}
}
