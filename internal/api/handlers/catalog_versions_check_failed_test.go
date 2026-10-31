package handlers

import (
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

func failedRow(id int) *models.MediaFile {
	stamp := time.Now().Add(-time.Hour)
	return &models.MediaFile{ID: id, FailedAt: &stamp}
}

func TestVersionCheckFailedMarksDurableVerdicts(t *testing.T) {
	if !versionCheckFailed(versionCheckOutcome{fileID: 7, file: failedRow(7)}, false) {
		t.Fatal("durable failed_at was not reported")
	}
	if !versionCheckFailed(versionCheckOutcome{fileID: 8, file: &models.MediaFile{ID: 8}}, true) {
		t.Fatal("freshly stamped row was not reported")
	}
}

func TestVersionCheckFailedLeavesAmbiguousRowsAlone(t *testing.T) {
	if versionCheckFailed(versionCheckOutcome{fileID: 9, file: &models.MediaFile{ID: 9}}, false) {
		t.Fatal("row without a verdict was marked failed")
	}
	if versionCheckFailed(versionCheckOutcome{fileID: 9}, false) {
		t.Fatal("missing row was marked failed")
	}
}
