package handlers

import (
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

func TestVirtualPersistSnapshotMovedRebasesOnlyCatalogRevisionChurn(t *testing.T) {
	now := time.Now()
	stamp := now.Add(-time.Minute)
	path := "virtual://movie/tt-cas?profile=4K+Dolby+Vision&result=abc"
	base := models.VirtualFilePersistArgs{
		FileID:           71,
		ExpectedFilePath: path,
		OwnerID:          7,
		LibraryID:        9,
		UpdatedAt:        now.Add(-2 * time.Minute),
	}
	current := virtualRowCASState{
		FilePath:  path,
		UpdatedAt: now,
		OwnerID:   7,
		LibraryID: 9,
	}
	refreshed := refreshVirtualPersistArgsForRow(base, current)

	if !virtualPersistSnapshotMoved(base, current, refreshed) {
		t.Fatal("a metadata-only write was not re-based after a provider re-list advanced updated_at")
	}

	probedCurrent := current
	probedCurrent.ProbeUpdatedAt = &stamp
	if virtualPersistSnapshotMoved(base, probedCurrent, refreshVirtualPersistArgsForRow(base, probedCurrent)) {
		t.Fatal("a metadata-only write was re-based over a newer probe stamp")
	}

	movedCurrent := current
	movedCurrent.FilePath = "virtual://movie/tt-cas?profile=4K+Dolby+Vision&result=other"
	if virtualPersistSnapshotMoved(base, movedCurrent, refreshVirtualPersistArgsForRow(base, movedCurrent)) {
		t.Fatal("a metadata-only write followed a moved candidate path")
	}

	otherOwner := current
	otherOwner.OwnerID = 8
	if virtualPersistSnapshotMoved(base, otherOwner, refreshVirtualPersistArgsForRow(base, otherOwner)) {
		t.Fatal("a metadata-only write followed a row that changed owner")
	}

	unchanged := base
	unchanged.UpdatedAt = current.UpdatedAt
	if virtualPersistSnapshotMoved(unchanged, current, refreshed) {
		t.Fatal("an unchanged snapshot was treated as movable")
	}

	adopt := base
	adopt.RequireAdopt = true
	adopt.AdoptPath = "virtual://movie/tt-cas?profile=4K+Dolby+Vision"
	movedAdopt := current
	movedAdopt.FilePath = "virtual://movie/tt-cas?profile=4K+Dolby+Vision&result=previous"
	if !virtualPersistSnapshotMoved(adopt, movedAdopt, refreshVirtualPersistArgsForRow(adopt, movedAdopt)) {
		t.Fatal("a required adoption did not follow the provider's churned candidate token")
	}

	// A provider re-list can advance the row revision while a required
	// adoption is probing the same path. That is catalog churn, not a second
	// probe, so the same-path required write must also be re-based.
	samePathCurrent := current
	samePathCurrent.UpdatedAt = base.UpdatedAt.Add(time.Minute)
	if !virtualPersistSnapshotMoved(adopt, samePathCurrent, refreshVirtualPersistArgsForRow(adopt, samePathCurrent)) {
		t.Fatal("a required adoption was not re-based after a same-path catalog revision advance")
	}
}
