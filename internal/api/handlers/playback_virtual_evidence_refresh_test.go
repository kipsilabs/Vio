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
