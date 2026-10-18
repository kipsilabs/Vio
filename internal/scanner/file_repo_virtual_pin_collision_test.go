package scanner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeVirtualPinDB scripts the two database operations the pin writes use. The
// collision paths deliberately have no live Postgres: the first write returns
// 23505, then the fake supplies the owner row the fix must resolve.
type fakeVirtualPinDB struct {
	execFunc     func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	queryRowFunc func(ctx context.Context, sql string, args ...any) pgx.Row

	execs   []fakePinCall
	queries []fakePinCall
}

type fakePinCall struct {
	sql  string
	args []any
}

func (db *fakeVirtualPinDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	db.execs = append(db.execs, fakePinCall{sql: sql, args: args})
	if db.execFunc == nil {
		return pgconn.NewCommandTag("UPDATE 1"), nil
	}
	return db.execFunc(ctx, sql, args...)
}

func (db *fakeVirtualPinDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	db.queries = append(db.queries, fakePinCall{sql: sql, args: args})
	if db.queryRowFunc == nil {
		return fakePinRow{err: pgx.ErrNoRows}
	}
	return db.queryRowFunc(ctx, sql, args...)
}

type fakePinRow struct {
	id   int
	path string
	err  error
}

func (r fakePinRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for _, d := range dest {
		switch out := d.(type) {
		case *int:
			*out = r.id
		case *string:
			*out = r.path
		default:
			return fmt.Errorf("unexpected scan destination %T", d)
		}
	}
	return nil
}

func uniquePathViolation() error {
	return &pgconn.PgError{
		Code:           "23505",
		ConstraintName: "media_files_virtual_file_owner_key",
	}
}

// TestReplaceVirtualResultPin_PathOwnerCollisionAdoptsOwner is the #337
// regression: the winning path is already owned by a sibling row, so the CAS
// write returns 23505. The update must resolve that owner, clear this row's
// stale candidate evidence by id without rewriting file_path, and report
// success instead of leaving the dead pin's URL cached.
func TestReplaceVirtualResultPin_PathOwnerCollisionAdoptsOwner(t *testing.T) {
	const (
		fileID  = 6506162
		ownerID = 6507000
	)
	deadPath := "virtual://movie/tt1200?result=dead"
	livePath := "virtual://movie/tt1200?result=live"

	db := &fakeVirtualPinDB{}
	db.execFunc = func(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
		switch len(db.execs) {
		case 1:
			if !strings.Contains(sql, "SET file_path = $1") {
				t.Fatalf("first write is not the path CAS: %s", sql)
			}
			return pgconn.CommandTag{}, uniquePathViolation()
		case 2:
			if strings.Contains(sql, "SET file_path") {
				t.Fatalf("collision fallback rewrote file_path: %s", sql)
			}
			if !strings.Contains(sql, "SET probe_updated_at = NULL") {
				t.Fatalf("collision fallback did not clear candidate evidence: %s", sql)
			}
			if len(args) != 1 || args[0] != fileID {
				t.Fatalf("evidence clear args = %v, want the losing row id %d", args, fileID)
			}
			return pgconn.NewCommandTag("UPDATE 1"), nil
		default:
			t.Fatalf("unexpected extra exec: %s", sql)
			return pgconn.CommandTag{}, nil
		}
	}
	db.queryRowFunc = func(_ context.Context, sql string, args ...any) pgx.Row {
		if !strings.Contains(sql, "owner.file_path = $2") {
			t.Fatalf("owner lookup does not resolve the target path: %s", sql)
		}
		if len(args) != 2 || args[0] != fileID || args[1] != livePath {
			t.Fatalf("owner lookup args = %v, want [%d %q]", args, fileID, livePath)
		}
		return fakePinRow{id: ownerID, path: livePath}
	}

	update, err := replaceVirtualResultPinWithDB(context.Background(), db, fileID, deadPath, livePath)
	if err != nil {
		t.Fatalf("collision reported a failure: %v", err)
	}
	if !update.Replaced {
		t.Fatal("collision reported the pin as not replaced; the owner already holds it")
	}
	if update.Owner == nil || update.Owner.ID != ownerID || update.Owner.FilePath != livePath {
		t.Fatalf("pin landed on %+v, want owner id %d at %q", update.Owner, ownerID, livePath)
	}
	if len(db.execs) != 2 {
		t.Fatalf("exec calls = %d, want the CAS plus the evidence clear", len(db.execs))
	}
}

// TestReplaceVirtualResultPin_GenuineUpdateFailureSurfaces keeps the collision
// tolerance narrow: an ordinary write error must reach the caller, not be read
// as a path-owner collision.
func TestReplaceVirtualResultPin_GenuineUpdateFailureSurfaces(t *testing.T) {
	writeErr := errors.New("database write failed")
	db := &fakeVirtualPinDB{
		execFunc: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, writeErr
		},
	}

	_, err := replaceVirtualResultPinWithDB(context.Background(), db, 7, "virtual://movie/tt7?result=dead", "virtual://movie/tt7?result=live")
	if err == nil {
		t.Fatal("genuine write failure was swallowed")
	}
	if !errors.Is(err, writeErr) {
		t.Fatalf("error = %v, want the underlying write failure", err)
	}
	if len(db.queries) != 0 {
		t.Fatalf("a non-23505 failure performed %d owner lookups", len(db.queries))
	}
}

// TestReplaceVirtualResultPin_CollisionWithoutOwnerIsAnError proves a 23505
// that cannot be explained by a live owner row is not silently converted to
// success: the pin would otherwise be left stale with no row representing it.
func TestReplaceVirtualResultPin_CollisionWithoutOwnerIsAnError(t *testing.T) {
	db := &fakeVirtualPinDB{
		execFunc: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, uniquePathViolation()
		},
		queryRowFunc: func(context.Context, string, ...any) pgx.Row {
			return fakePinRow{err: pgx.ErrNoRows}
		},
	}

	_, err := replaceVirtualResultPinWithDB(context.Background(), db, 7, "virtual://movie/tt7?result=dead", "virtual://movie/tt7?result=live")
	if err == nil {
		t.Fatal("unresolved path owner was reported as success")
	}
	if !strings.Contains(err.Error(), "no live owner row") {
		t.Fatalf("error = %v, want the unresolved-owner cause", err)
	}
}

// TestUnpinVirtualResult_NeutralPathOwnerCollisionAdoptsOwner reproduces the
// production error: the replacement collision falls back to stripping the dead
// ?result= pin, but the neutral path is also owned. The neutral state already
// exists on that owner, so the unpin reports success and clears the losing
// row's evidence without touching file_path.
func TestUnpinVirtualResult_NeutralPathOwnerCollisionAdoptsOwner(t *testing.T) {
	const (
		fileID  = 6506162
		ownerID = 6507000
	)
	deadPath := "virtual://movie/tt1200?result=dead"
	neutralPath := "virtual://movie/tt1200"

	db := &fakeVirtualPinDB{}
	db.queryRowFunc = func(_ context.Context, sql string, args ...any) pgx.Row {
		switch len(db.queries) {
		case 1:
			if !strings.Contains(sql, "SELECT file_path FROM media_files WHERE id = $1") {
				t.Fatalf("first query is not the row read: %s", sql)
			}
			return fakePinRow{path: deadPath}
		case 2:
			if !strings.Contains(sql, "owner.file_path = $2") {
				t.Fatalf("owner lookup does not resolve the neutral path: %s", sql)
			}
			if len(args) != 2 || args[0] != fileID || args[1] != neutralPath {
				t.Fatalf("owner lookup args = %v, want [%d %q]", args, fileID, neutralPath)
			}
			return fakePinRow{id: ownerID, path: neutralPath}
		default:
			t.Fatalf("unexpected extra query: %s", sql)
			return fakePinRow{err: pgx.ErrNoRows}
		}
	}
	db.execFunc = func(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
		switch len(db.execs) {
		case 1:
			if !strings.Contains(sql, "SET file_path = $1") {
				t.Fatalf("first write is not the unpin CAS: %s", sql)
			}
			if len(args) != 3 || args[0] != neutralPath || args[1] != fileID || args[2] != deadPath {
				t.Fatalf("unpin args = %v, want neutral=%q id=%d expected=%q", args, neutralPath, fileID, deadPath)
			}
			return pgconn.CommandTag{}, uniquePathViolation()
		case 2:
			if strings.Contains(sql, "SET file_path") {
				t.Fatalf("collision fallback rewrote file_path: %s", sql)
			}
			if !strings.Contains(sql, "SET probe_updated_at = NULL") {
				t.Fatalf("collision fallback did not clear candidate evidence: %s", sql)
			}
			if len(args) != 1 || args[0] != fileID {
				t.Fatalf("evidence clear args = %v, want the losing row id %d", args, fileID)
			}
			return pgconn.NewCommandTag("UPDATE 1"), nil
		default:
			t.Fatalf("unexpected extra exec: %s", sql)
			return pgconn.CommandTag{}, nil
		}
	}

	if err := unpinVirtualResultWithDB(context.Background(), db, fileID, deadPath); err != nil {
		t.Fatalf("neutral-path collision reported a failure: %v", err)
	}
	if len(db.execs) != 2 {
		t.Fatalf("exec calls = %d, want the unpin plus the evidence clear", len(db.execs))
	}
	if len(db.queries) != 2 {
		t.Fatalf("query calls = %d, want the row read plus the owner lookup", len(db.queries))
	}
}

// TestUnpinVirtualResult_GenuineUpdateFailureSurfaces keeps ordinary unpin
// errors visible instead of treating every failure as a collision.
func TestUnpinVirtualResult_GenuineUpdateFailureSurfaces(t *testing.T) {
	writeErr := errors.New("update path failed")
	db := &fakeVirtualPinDB{
		queryRowFunc: func(context.Context, string, ...any) pgx.Row {
			return fakePinRow{path: "virtual://movie/tt7?result=dead"}
		},
		execFunc: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, writeErr
		},
	}

	err := unpinVirtualResultWithDB(context.Background(), db, 7, "virtual://movie/tt7?result=dead")
	if err == nil {
		t.Fatal("genuine unpin failure was swallowed")
	}
	if !errors.Is(err, writeErr) {
		t.Fatalf("error = %v, want the underlying write failure", err)
	}
	if len(db.queries) != 1 {
		t.Fatalf("a non-23505 failure performed %d owner lookups", len(db.queries)-1)
	}
}
