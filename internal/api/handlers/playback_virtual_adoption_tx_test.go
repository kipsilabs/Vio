package handlers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The durable saver runs transactionally only when a real database and the
// durable refusal store are wired. These fakes prove the orchestration without a
// database: the evidence write and the exact-pin clear are invoked on the same
// transaction, the count is re-read in-transaction before the terminal decision,
// and a clear or record failure rolls the whole thing back. The real-Postgres
// counterparts live in playback_virtual_adoption_bound_tx_db_test.go.

// fakeAdoptionTx embeds a nil pgx.Tx so only the methods the saver and store
// actually use need an override; any other call panics loudly.
type fakeAdoptionTx struct {
	pgx.Tx
	rowFor     func(sql string, args ...any) pgx.Row
	execSQL    []string
	committed  bool
	rolledBack bool
}

func (t *fakeAdoptionTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	return t.rowFor(sql, args...)
}

func (t *fakeAdoptionTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	t.execSQL = append(t.execSQL, sql)
	return pgconn.CommandTag{}, nil
}

func (t *fakeAdoptionTx) Commit(context.Context) error {
	t.committed = true
	return nil
}

func (t *fakeAdoptionTx) Rollback(context.Context) error {
	t.rolledBack = true
	return nil
}

// fakeTxBeginner is a VirtualFileMetadataDB that also begins transactions. Its
// QueryRow reaches the transaction unless a test wires poolRow for the
// non-transactional fallback.
type fakeTxBeginner struct {
	tx      *fakeAdoptionTx
	poolRow func(sql string, args ...any) pgx.Row
	began   int
}

func (d *fakeTxBeginner) Begin(context.Context) (pgx.Tx, error) {
	d.began++
	return d.tx, nil
}

func (d *fakeTxBeginner) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if d.poolRow == nil {
		panic("the transactional path must operate on the transaction, not the database")
	}
	return d.poolRow(sql, args...)
}

// scriptedRow fills whichever destination the caller scans: a string (the
// persisted file_path) or an int (the locked row id).
type scriptedRow struct {
	str string
	id  int
	err error
}

func (r scriptedRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for _, d := range dest {
		switch out := d.(type) {
		case *string:
			*out = r.str
		case *int:
			*out = r.id
		}
	}
	return nil
}

// fakeAdoptionTxStore records, clears and terminally stamps refusal state on the
// transaction it is handed and proves the transaction was still open when the
// clear ran. It embeds the base fake so it also satisfies
// VirtualAdoptionRefusalStore for the fallback path.
type fakeAdoptionTxStore struct {
	*fakeAdoptionRefusalStore
	clearErr        error
	clearCalls      int
	clearWhileOpen  bool
	terminalCalls   int
	terminalStamped bool
	terminalErr     error
	swept           bool
}

func (s *fakeAdoptionTxStore) key(fileID int, expectedPath string) string {
	return fakeAdoptionRefusalKey(fileID, expectedPath)
}

func (s *fakeAdoptionTxStore) RecordVirtualAdoptionRefusalTx(_ context.Context, _ pgx.Tx, fileID int, expectedPath string, _ time.Duration) (int, error) {
	if s.recordErr != nil {
		return 0, s.recordErr
	}
	key := s.key(fileID, expectedPath)
	s.counts[key]++
	return s.counts[key], nil
}

func (s *fakeAdoptionTxStore) ClearVirtualAdoptionRefusalTx(_ context.Context, tx pgx.Tx, fileID int, expectedPath string) error {
	if s.clearErr != nil {
		return s.clearErr
	}
	s.clearCalls++
	if ft, ok := tx.(*fakeAdoptionTx); ok && !ft.committed {
		s.clearWhileOpen = true
	}
	delete(s.counts, s.key(fileID, expectedPath))
	return nil
}

func (s *fakeAdoptionTxStore) MarkVirtualAdoptionTerminalRefusalTx(_ context.Context, _ pgx.Tx, _ int, _ string, _ *time.Time, _ time.Time, _ *time.Time) (bool, error) {
	s.terminalCalls++
	if s.terminalErr != nil {
		return false, s.terminalErr
	}
	return s.terminalStamped, nil
}

func (s *fakeAdoptionTxStore) SweepVirtualAdoptionRefusals(context.Context) error {
	s.swept = true
	return nil
}

func newFakeAdoptionTxStore() *fakeAdoptionTxStore {
	return &fakeAdoptionTxStore{fakeAdoptionRefusalStore: newFakeAdoptionRefusalStore()}
}

// txAdoptionArgs builds a required-adoption write for the fakes.
func txAdoptionArgs(fileID int, expectedPath, adoptPath string) models.VirtualFilePersistArgs {
	return models.VirtualFilePersistArgs{
		FileID:           fileID,
		ExpectedFilePath: expectedPath,
		AdoptPath:        adoptPath,
		RequireAdopt:     true,
		UpdatedAt:        time.Unix(1_700_000_000, 0),
	}
}

// TestVirtualAdoptionTxClearsRefusalWithEvidence proves the core fix: the clear
// runs on the same transaction the evidence write used and before that
// transaction commits, so a cancel between the two is impossible.
func TestVirtualAdoptionTxClearsRefusalWithEvidence(t *testing.T) {
	ctx := context.Background()
	row := adoptionBoundTestRow(5040001, "tx-hash")
	adopt := "virtual://series/tt39087958/1/1?result=tx-success"
	args := txAdoptionArgs(row.ID, row.FilePath, adopt)

	tx := &fakeAdoptionTx{rowFor: func(sql string, _ ...any) pgx.Row {
		if sql == lockVirtualFileRowSQL {
			return scriptedRow{id: row.ID}
		}
		return scriptedRow{str: adopt}
	}}
	db := &fakeTxBeginner{tx: tx}
	store := newFakeAdoptionTxStore()
	store.counts[store.key(row.ID, row.FilePath)] = 2

	result, err := ExecVirtualFileMetadataUpdateResultWithRefusals(ctx, db, args, store)
	if err != nil {
		t.Fatalf("transactional adopting write failed: %v", err)
	}
	if !result.MetadataUpdated || !result.IdentityAdopted {
		t.Fatalf("result = %+v, want a metadata update with adoption", result)
	}
	if db.began != 1 {
		t.Fatalf("Begin calls = %d, want 1", db.began)
	}
	if !tx.committed || tx.rolledBack {
		t.Fatalf("committed=%v rolledBack=%v, want committed and not rolled back", tx.committed, tx.rolledBack)
	}
	if store.clearCalls != 1 || !store.clearWhileOpen {
		t.Fatalf("clear calls = %d while-open=%v, want the clear inside the open transaction", store.clearCalls, store.clearWhileOpen)
	}
	if got := store.counts[store.key(row.ID, row.FilePath)]; got != 0 {
		t.Fatalf("refusal count after the write = %d, want 0", got)
	}
	if !store.swept {
		t.Fatal("the post-commit refusal sweep was not run")
	}
}

// TestVirtualAdoptionTxClearFailureRollsBackEvidence proves the atomicity is
// real: when the exact-pin clear fails the evidence write does not commit, so a
// committed row can never sit behind a still-live threshold.
func TestVirtualAdoptionTxClearFailureRollsBackEvidence(t *testing.T) {
	ctx := context.Background()
	row := adoptionBoundTestRow(5040002, "tx-hash")
	adopt := "virtual://series/tt39087958/1/1?result=tx-clear-fail"
	args := txAdoptionArgs(row.ID, row.FilePath, adopt)

	tx := &fakeAdoptionTx{rowFor: func(sql string, _ ...any) pgx.Row {
		if sql == lockVirtualFileRowSQL {
			return scriptedRow{id: row.ID}
		}
		return scriptedRow{str: adopt}
	}}
	db := &fakeTxBeginner{tx: tx}
	store := newFakeAdoptionTxStore()
	store.clearErr = errors.New("clear unavailable")

	result, err := ExecVirtualFileMetadataUpdateResultWithRefusals(ctx, db, args, store)
	if err == nil {
		t.Fatalf("clear failure reported success: %+v", result)
	}
	if tx.committed || !tx.rolledBack {
		t.Fatalf("committed=%v rolledBack=%v, want rolled back and not committed", tx.committed, tx.rolledBack)
	}
}

// TestVirtualAdoptionTxRefusalRecheckStampsTerminal proves the refusal path
// re-reads the count in-transaction and installs the terminal verdict from that
// count, not from the caller's earlier read.
func TestVirtualAdoptionTxRefusalRecheckStampsTerminal(t *testing.T) {
	ctx := context.Background()
	row := adoptionBoundTestRow(5040003, "tx-hash")
	adopt := "virtual://series/tt39087958/1/1?result=tx-refused"
	args := txAdoptionArgs(row.ID, row.FilePath, adopt)
	args.RefusalReason = virtualProbeRefusalAdoptionBoundExhausted

	tx := &fakeAdoptionTx{rowFor: func(sql string, _ ...any) pgx.Row {
		if sql == lockVirtualFileRowSQL {
			return scriptedRow{id: row.ID}
		}
		return scriptedRow{err: pgx.ErrNoRows}
	}}
	db := &fakeTxBeginner{tx: tx}
	store := newFakeAdoptionTxStore()
	// One short of the bound, so this refusal is the one that reaches it.
	store.counts[store.key(row.ID, row.FilePath)] = virtualAdoptionRefusalBound - 1
	store.terminalStamped = true

	if _, err := ExecVirtualFileMetadataUpdateResultWithRefusals(ctx, db, args, store); err == nil {
		t.Fatal("refused adoption reported success")
	}
	if !tx.committed || tx.rolledBack {
		t.Fatalf("committed=%v rolledBack=%v, want committed and not rolled back", tx.committed, tx.rolledBack)
	}
	if store.terminalCalls != 1 {
		t.Fatalf("terminal stamp calls = %d, want the in-transaction count to reach the bound", store.terminalCalls)
	}
	if got := store.counts[store.key(row.ID, row.FilePath)]; got != virtualAdoptionRefusalBound {
		t.Fatalf("refusal count = %d, want the bound %d", got, virtualAdoptionRefusalBound)
	}
}

// TestVirtualAdoptionTxRefusalBelowBoundDoesNotStamp proves the terminal is only
// decided once the in-transaction count has actually reached the bound.
func TestVirtualAdoptionTxRefusalBelowBoundDoesNotStamp(t *testing.T) {
	ctx := context.Background()
	row := adoptionBoundTestRow(5040004, "tx-hash")
	adopt := "virtual://series/tt39087958/1/1?result=tx-below-bound"
	args := txAdoptionArgs(row.ID, row.FilePath, adopt)

	tx := &fakeAdoptionTx{rowFor: func(sql string, _ ...any) pgx.Row {
		if sql == lockVirtualFileRowSQL {
			return scriptedRow{id: row.ID}
		}
		return scriptedRow{err: pgx.ErrNoRows}
	}}
	db := &fakeTxBeginner{tx: tx}
	store := newFakeAdoptionTxStore()
	store.terminalStamped = true

	if _, err := ExecVirtualFileMetadataUpdateResultWithRefusals(ctx, db, args, store); err == nil {
		t.Fatal("refused adoption reported success")
	}
	if store.terminalCalls != 0 {
		t.Fatalf("terminal stamp calls = %d, want 0 below the bound", store.terminalCalls)
	}
	if !tx.committed {
		t.Fatal("the refusal increment was not committed below the bound")
	}
}

// TestVirtualAdoptionTxRecordFailureRollsBack proves a refusal-store outage
// neither advances the count nor installs a terminal: the increment rolls back
// and the write is reported refused, never as a reached bound.
func TestVirtualAdoptionTxRecordFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	row := adoptionBoundTestRow(5040005, "tx-hash")
	adopt := "virtual://series/tt39087958/1/1?result=tx-record-fail"
	args := txAdoptionArgs(row.ID, row.FilePath, adopt)

	tx := &fakeAdoptionTx{rowFor: func(sql string, _ ...any) pgx.Row {
		if sql == lockVirtualFileRowSQL {
			return scriptedRow{id: row.ID}
		}
		return scriptedRow{err: pgx.ErrNoRows}
	}}
	db := &fakeTxBeginner{tx: tx}
	store := newFakeAdoptionTxStore()
	store.recordErr = errors.New("database unavailable")

	if _, err := ExecVirtualFileMetadataUpdateResultWithRefusals(ctx, db, args, store); err == nil {
		t.Fatal("refused adoption reported success")
	}
	if tx.committed || !tx.rolledBack {
		t.Fatalf("committed=%v rolledBack=%v, want rolled back and not committed", tx.committed, tx.rolledBack)
	}
	if store.terminalCalls != 0 {
		t.Fatalf("an unreadable counter installed %d terminals, want 0", store.terminalCalls)
	}
}

// TestVirtualAdoptionTxCollisionRetryClearsOnSameTransaction proves the
// collision-retry path stays inside the transaction: the unique violation
// rewinds to a savepoint, the metadata-only retry runs, and the exact-pin clear
// commits with it.
func TestVirtualAdoptionTxCollisionRetryClearsOnSameTransaction(t *testing.T) {
	ctx := context.Background()
	row := adoptionBoundTestRow(5040006, "tx-hash")
	adopt := "virtual://series/tt39087958/1/1?result=tx-collide"
	args := txAdoptionArgs(row.ID, row.FilePath, adopt)
	// The collision retry only exists for a best-effort (non-required) adoption.
	args.RequireAdopt = false

	calls := 0
	tx := &fakeAdoptionTx{rowFor: func(sql string, _ ...any) pgx.Row {
		if sql == lockVirtualFileRowSQL {
			return scriptedRow{id: row.ID}
		}
		calls++
		if calls == 1 {
			return scriptedRow{err: &pgconn.PgError{Code: "23505"}}
		}
		return scriptedRow{str: row.FilePath}
	}}
	db := &fakeTxBeginner{tx: tx}
	store := newFakeAdoptionTxStore()
	store.counts[store.key(row.ID, row.FilePath)] = virtualAdoptionRefusalBound

	result, err := ExecVirtualFileMetadataUpdateResultWithRefusals(ctx, db, args, store)
	if err != nil {
		t.Fatalf("metadata-only retry failed: %v", err)
	}
	if !result.MetadataUpdated || result.IdentityAdopted {
		t.Fatalf("result = %+v, want a metadata update without adoption", result)
	}
	if !tx.committed || tx.rolledBack {
		t.Fatalf("committed=%v rolledBack=%v, want committed and not rolled back", tx.committed, tx.rolledBack)
	}
	if !containsSQL(tx.execSQL, rollbackVirtualAdoptSQL) {
		t.Fatalf("the collision was not rewound to its savepoint: %v", tx.execSQL)
	}
	if store.clearCalls != 1 || !store.clearWhileOpen {
		t.Fatalf("clear calls = %d while-open=%v, want the clear inside the transaction", store.clearCalls, store.clearWhileOpen)
	}
	if got := store.counts[store.key(row.ID, row.FilePath)]; got != 0 {
		t.Fatalf("refusal count after the retry = %d, want 0", got)
	}
}

// TestVirtualAdoptionTxMissingRowIsNoOp proves a vanished row is a plain no-op:
// the lock finds nothing, so no refusal is recorded and no transaction is
// committed over a missing foreign key.
func TestVirtualAdoptionTxMissingRowIsNoOp(t *testing.T) {
	ctx := context.Background()
	row := adoptionBoundTestRow(5040007, "tx-hash")
	adopt := "virtual://series/tt39087958/1/1?result=tx-gone"
	args := txAdoptionArgs(row.ID, row.FilePath, adopt)

	tx := &fakeAdoptionTx{rowFor: func(string, ...any) pgx.Row {
		return scriptedRow{err: pgx.ErrNoRows}
	}}
	db := &fakeTxBeginner{tx: tx}
	store := newFakeAdoptionTxStore()

	result, err := ExecVirtualFileMetadataUpdateResultWithRefusals(ctx, db, args, store)
	if err != nil {
		t.Fatalf("missing row reported an error: %v", err)
	}
	if result.MetadataUpdated || result.RowsAffected != 0 {
		t.Fatalf("result = %+v, want a no-op", result)
	}
	if store.recordErr != nil || store.clearCalls != 0 || store.terminalCalls != 0 {
		t.Fatalf("a missing row touched refusal state: %+v", store)
	}
}

// TestVirtualAdoptionSaverWithoutTxStoreFallsBack proves graceful degradation:
// when the durable tx store is not wired the saver keeps the previous
// best-effort path even though the database can begin a transaction.
func TestVirtualAdoptionSaverWithoutTxStoreFallsBack(t *testing.T) {
	ctx := context.Background()
	row := adoptionBoundTestRow(5040008, "tx-hash")
	adopt := "virtual://series/tt39087958/1/1?result=tx-fallback"
	args := txAdoptionArgs(row.ID, row.FilePath, adopt)

	tx := &fakeAdoptionTx{rowFor: func(sql string, _ ...any) pgx.Row {
		if sql == lockVirtualFileRowSQL {
			t.Errorf("the fallback path must not lock a transaction row")
		}
		return scriptedRow{str: adopt}
	}}
	db := &fakeTxBeginner{tx: tx, poolRow: func(string, ...any) pgx.Row {
		return scriptedRow{str: adopt}
	}}
	// A store that does not implement VirtualAdoptionRefusalTxStore.
	store := newFakeAdoptionRefusalStore()
	store.counts[fakeAdoptionRefusalKey(row.ID, row.FilePath)] = 1

	if _, err := ExecVirtualFileMetadataUpdateResultWithRefusals(ctx, db, args, store); err != nil {
		t.Fatalf("fallback write failed: %v", err)
	}
	if db.began != 0 {
		t.Fatalf("Begin calls = %d, want the fallback path to skip the transaction", db.began)
	}
	if got := store.counts[fakeAdoptionRefusalKey(row.ID, row.FilePath)]; got != 0 {
		t.Fatalf("fallback clear left count = %d, want 0", got)
	}
}

func containsSQL(execs []string, want string) bool {
	for _, sql := range execs {
		if sql == want {
			return true
		}
	}
	return false
}

// TestVirtualAdoptionTxNonRequiredZeroRowIsNotARefusal is the blocker-1
// regression: only a required adoption that matched no row is a refusal. An
// ordinary metadata CAS miss (no RequireAdopt, or an empty adopt path) must not
// increment the exact-pin counter or install a terminal, or a clear followed by
// an ordinary miss replenishes the count and a neutral-owner non-required write
// can terminal against a current revision.
func TestVirtualAdoptionTxNonRequiredZeroRowIsNotARefusal(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		fileID int
		adopt  string
	}{
		{"empty-adopt-path", 5040101, ""},
		{"best-effort-adopt-path", 5040102, "virtual://series/tt1/1/1?result=best-effort"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := txAdoptionArgs(tc.fileID, "virtual://series/tt1/1/1?result=pinned", tc.adopt)
			args.RequireAdopt = false
			tx := &fakeAdoptionTx{rowFor: func(sql string, _ ...any) pgx.Row {
				if sql == lockVirtualFileRowSQL {
					return scriptedRow{id: tc.fileID}
				}
				return scriptedRow{err: pgx.ErrNoRows}
			}}
			db := &fakeTxBeginner{tx: tx}
			store := newFakeAdoptionTxStore()
			key := store.key(tc.fileID, args.ExpectedFilePath)
			store.counts[key] = virtualAdoptionRefusalBound
			store.terminalStamped = true

			result, err := ExecVirtualFileMetadataUpdateResultWithRefusals(ctx, db, args, store)
			if err != nil {
				t.Fatalf("non-required zero-row miss returned an error: %v", err)
			}
			if result.MetadataUpdated || result.RowsAffected != 0 || result.IdentityAdopted {
				t.Fatalf("result = %+v, want a plain no-op", result)
			}
			if store.terminalCalls != 0 {
				t.Fatalf("an ordinary zero-row miss installed %d terminals, want 0", store.terminalCalls)
			}
			if got := store.counts[key]; got != virtualAdoptionRefusalBound {
				t.Fatalf("refusal count = %d, want the pre-existing %d (an ordinary miss must not increment)", got, virtualAdoptionRefusalBound)
			}
			if tx.committed {
				t.Fatal("an ordinary zero-row miss committed a refusal transaction")
			}
		})
	}
}
