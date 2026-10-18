package handlers

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/jackc/pgx/v5"
)

// adoptionCASStateRow scans the current CAS state the rebase retry reads after
// a required adoption matched no row. It fills the destinations positionally,
// matching virtualRowCASStateSQL's column order.
type adoptionCASStateRow struct{ state virtualRowCASState }

func (r adoptionCASStateRow) Scan(dest ...any) error {
	if len(dest) != 10 {
		return fmt.Errorf("adoptionCASStateRow scan: got %d destinations, want 10", len(dest))
	}
	*dest[0].(*string) = r.state.FilePath
	*dest[1].(*time.Time) = r.state.UpdatedAt
	*dest[2].(**time.Time) = r.state.ProbeUpdatedAt
	*dest[3].(*int) = r.state.OwnerID
	*dest[4].(*int) = r.state.LibraryID
	*dest[5].(*string) = r.state.ProbeSource
	*dest[6].(*string) = r.state.VideoHash
	*dest[7].(*string) = r.state.GUID
	*dest[8].(*string) = r.state.ReleaseName
	*dest[9].(*int64) = r.state.ReleaseSize
	return nil
}

// adoptionSnapshotDB is a VirtualFileMetadataDB (no transaction support) that
// serves the CAS-state read and a scripted sequence of update results.
type adoptionSnapshotDB struct {
	state      adoptionCASStateRow
	updates    []pgx.Row
	stateCalls int
	updateCall int
}

func (d *adoptionSnapshotDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	if sql == virtualRowCASStateSQL {
		d.stateCalls++
		return d.state
	}
	d.updateCall++
	if d.updateCall <= len(d.updates) {
		return d.updates[d.updateCall-1]
	}
	return scriptedRow{str: ""}
}

// staleChurnedAdoptionArgs builds a required-adoption write whose caller
// snapshot says the row still carries the pinned result token, while the row has
// since been re-listed onto a churned token (same item/profile).
func staleChurnedAdoptionArgs(row *models.MediaFile, adopt string) (models.VirtualFilePersistArgs, virtualRowCASState) {
	args := txAdoptionArgs(row.ID, row.FilePath, adopt)
	args.OwnerID = row.VirtualOwnerInstallationID
	args.LibraryID = row.MediaFolderID
	args.ExpectedProviderVideoHash = row.ProviderVideoHash
	args.ExpectedProviderReleaseName = row.ProviderReleaseName
	args.ExpectedProviderReleaseSize = row.ProviderReleaseSize
	state := virtualRowCASState{
		FilePath:    "virtual://series/tt39087958/1/1?profile=4K+Dolby+Vision&result=bbbbbbbbbbbbbbbbbbbbbbbb",
		UpdatedAt:   args.UpdatedAt.Add(2 * time.Minute),
		OwnerID:     row.VirtualOwnerInstallationID,
		LibraryID:   row.MediaFolderID,
		VideoHash:   row.ProviderVideoHash,
		ReleaseName: row.ProviderReleaseName,
		ReleaseSize: row.ProviderReleaseSize,
	}
	return args, state
}

// TestVirtualAdoptionTxRebasesStaleSnapshotOnChurnedPath proves a provider
// re-list that churns the ?result= token between the caller's snapshot and the
// write does not cost the session its evidence: the transactional saver re-reads
// the locked row and retries the same adoption against the current snapshot
// instead of recording a refusal.
func TestVirtualAdoptionTxRebasesStaleSnapshotOnChurnedPath(t *testing.T) {
	ctx := context.Background()
	row := adoptionBoundTestRow(5040201, "tx-hash")
	adopt := "virtual://series/tt39087958/1/1?result=tx-rebased"
	args, state := staleChurnedAdoptionArgs(row, adopt)

	updateCalls := 0
	tx := &fakeAdoptionTx{rowFor: func(sql string, _ ...any) pgx.Row {
		switch sql {
		case lockVirtualFileRowSQL:
			return scriptedRow{id: row.ID}
		case virtualRowCASStateSQL:
			return adoptionCASStateRow{state: state}
		default:
			updateCalls++
			if updateCalls == 1 {
				return scriptedRow{str: ""} // the caller's snapshot is stale
			}
			return scriptedRow{str: adopt}
		}
	}}
	db := &fakeTxBeginner{tx: tx}
	store := newFakeAdoptionTxStore()

	result, err := ExecVirtualFileMetadataUpdateResultWithRefusals(ctx, db, args, store)
	if err != nil {
		t.Fatalf("re-based adoption failed: %v", err)
	}
	if !result.MetadataUpdated || !result.IdentityAdopted {
		t.Fatalf("result = %+v, want a metadata update with adoption", result)
	}
	if updateCalls != 2 {
		t.Fatalf("update statements = %d, want the stale write retried once", updateCalls)
	}
	if store.records != 0 {
		t.Fatalf("refusals recorded = %d, want none for a stale snapshot", store.records)
	}
	if !tx.committed || tx.rolledBack {
		t.Fatalf("committed=%v rolledBack=%v, want committed and not rolled back", tx.committed, tx.rolledBack)
	}
}

// TestVirtualAdoptionUntxRebasesStaleSnapshotOnChurnedPath is the same
// regression for the best-effort saver used without transaction support.
func TestVirtualAdoptionUntxRebasesStaleSnapshotOnChurnedPath(t *testing.T) {
	ctx := context.Background()
	row := adoptionBoundTestRow(5040202, "tx-hash")
	adopt := "virtual://series/tt39087958/1/1?result=tx-rebased-untx"
	args, state := staleChurnedAdoptionArgs(row, adopt)

	db := &adoptionSnapshotDB{
		state:   adoptionCASStateRow{state: state},
		updates: []pgx.Row{scriptedRow{str: ""}, scriptedRow{str: adopt}},
	}
	store := newFakeAdoptionRefusalStore()

	result, err := ExecVirtualFileMetadataUpdateResultWithRefusals(ctx, db, args, store)
	if err != nil {
		t.Fatalf("re-based adoption failed: %v", err)
	}
	if !result.MetadataUpdated || !result.IdentityAdopted {
		t.Fatalf("result = %+v, want a metadata update with adoption", result)
	}
	if db.stateCalls != 1 || db.updateCall != 2 {
		t.Fatalf("state reads = %d, update statements = %d; want one re-read and one retry", db.stateCalls, db.updateCall)
	}
	if store.records != 0 {
		t.Fatalf("refusals recorded = %d, want none for a stale snapshot", store.records)
	}
}

// TestVirtualAdoptionStaleSnapshotOutsideNeutralKeyStillRefuses pins the guard:
// a row that moved to a different item/profile is never re-based, so the
// required adoption still refuses and counts toward the bound.
func TestVirtualAdoptionStaleSnapshotOutsideNeutralKeyStillRefuses(t *testing.T) {
	ctx := context.Background()
	row := adoptionBoundTestRow(5040203, "tx-hash")
	adopt := "virtual://series/tt39087958/1/1?result=tx-foreign-row"
	args, state := staleChurnedAdoptionArgs(row, adopt)
	state.FilePath = "virtual://movie/tt9999999?profile=4K+Dolby+Vision&result=bbbbbbbbbbbbbbbbbbbbbbbb"

	db := &adoptionSnapshotDB{
		state:   adoptionCASStateRow{state: state},
		updates: []pgx.Row{scriptedRow{str: ""}, scriptedRow{str: adopt}},
	}
	store := newFakeAdoptionRefusalStore()

	if _, err := ExecVirtualFileMetadataUpdateResultWithRefusals(ctx, db, args, store); err == nil {
		t.Fatal("adoption onto a different item/profile reported success")
	}
	if db.updateCall != 1 {
		t.Fatalf("update statements = %d, want no retry outside the neutral key", db.updateCall)
	}
	if store.counts[fakeAdoptionRefusalKey(args.FileID, args.ExpectedFilePath)] != 1 {
		t.Fatalf("refusals = %v, want the stale foreign row counted once", store.counts)
	}
}
