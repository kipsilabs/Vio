package scanner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fakeVirtualPinDB scripts the database operations the pin writes use. The
// collision paths deliberately have no live Postgres: the pre-CAS revision read
// is scripted, the CAS returns 23505, and the fake supplies the owner the
// fallback must resolve.
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
	id        int
	path      string
	updatedAt time.Time
	cleared   int
	err       error
}

func (r fakePinRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	ints := 0
	for _, d := range dest {
		switch out := d.(type) {
		case *int:
			if ints == 0 {
				*out = r.id
			} else {
				*out = r.cleared
			}
			ints++
		case *string:
			*out = r.path
		case *time.Time:
			*out = r.updatedAt
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

func collisionRevision() time.Time {
	return time.Date(2026, 10, 19, 12, 0, 0, 123456000, time.UTC)
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
	observedAt := collisionRevision()

	db := &fakeVirtualPinDB{}
	db.execFunc = func(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
		if len(db.execs) != 1 {
			t.Fatalf("unexpected extra exec: %s", sql)
		}
		if !strings.Contains(sql, "SET file_path = $1") {
			t.Fatalf("first write is not the path CAS: %s", sql)
		}
		return pgconn.CommandTag{}, uniquePathViolation()
	}
	db.queryRowFunc = func(_ context.Context, sql string, args ...any) pgx.Row {
		switch len(db.queries) {
		case 1:
			if !strings.Contains(sql, "SELECT file_path, updated_at FROM media_files WHERE id = $1") {
				t.Fatalf("first query is not the revision read: %s", sql)
			}
			if len(args) != 1 || args[0] != fileID {
				t.Fatalf("revision read args = %v, want [%d]", args, fileID)
			}
			return fakePinRow{path: deadPath, updatedAt: observedAt}
		case 2:
			if strings.Contains(sql, "SET file_path") {
				t.Fatalf("collision fallback rewrote file_path: %s", sql)
			}
			if !strings.Contains(sql, "SET probe_updated_at = NULL") {
				t.Fatalf("collision fallback did not clear candidate evidence: %s", sql)
			}
			if !strings.Contains(sql, "file_path = $3") || !strings.Contains(sql, "updated_at = $4") {
				t.Fatalf("evidence clear is not revision-fenced: %s", sql)
			}
			if !strings.Contains(sql, "EXISTS (SELECT 1 FROM owner)") {
				t.Fatalf("evidence clear does not fence on the owner: %s", sql)
			}
			want := []any{fileID, livePath, deadPath, observedAt}
			if len(args) != len(want) {
				t.Fatalf("evidence clear args = %v, want %v", args, want)
			}
			for i := range want {
				if args[i] != want[i] {
					t.Fatalf("evidence clear args[%d] = %v, want %v", i, args[i], want[i])
				}
			}
			return fakePinRow{id: ownerID, path: livePath, cleared: 1}
		default:
			t.Fatalf("unexpected extra query: %s", sql)
			return fakePinRow{err: pgx.ErrNoRows}
		}
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
	if len(db.execs) != 1 {
		t.Fatalf("exec calls = %d, want the CAS only", len(db.execs))
	}
	if len(db.queries) != 2 {
		t.Fatalf("query calls = %d, want the revision read plus the fenced clear", len(db.queries))
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
		queryRowFunc: func(context.Context, string, ...any) pgx.Row {
			return fakePinRow{path: "virtual://movie/tt7?result=dead", updatedAt: collisionRevision()}
		},
	}

	_, err := replaceVirtualResultPinWithDB(context.Background(), db, 7, "virtual://movie/tt7?result=dead", "virtual://movie/tt7?result=live")
	if err == nil {
		t.Fatal("genuine write failure was swallowed")
	}
	if !errors.Is(err, writeErr) {
		t.Fatalf("error = %v, want the underlying write failure", err)
	}
	if len(db.queries) != 1 {
		t.Fatalf("a non-23505 failure performed %d extra queries", len(db.queries)-1)
	}
}

// TestReplaceVirtualResultPin_CollisionWithoutOwnerIsAnError proves a 23505
// that cannot be explained by a live owner row is not silently converted to
// success: the pin would otherwise be left stale with no row representing it.
func TestReplaceVirtualResultPin_CollisionWithoutOwnerIsAnError(t *testing.T) {
	db := &fakeVirtualPinDB{}
	db.execFunc = func(context.Context, string, ...any) (pgconn.CommandTag, error) {
		return pgconn.CommandTag{}, uniquePathViolation()
	}
	db.queryRowFunc = func(_ context.Context, sql string, _ ...any) pgx.Row {
		if strings.Contains(sql, "WITH owner AS") {
			return fakePinRow{err: pgx.ErrNoRows}
		}
		return fakePinRow{path: "virtual://movie/tt7?result=dead", updatedAt: collisionRevision()}
	}

	_, err := replaceVirtualResultPinWithDB(context.Background(), db, 7, "virtual://movie/tt7?result=dead", "virtual://movie/tt7?result=live")
	if err == nil {
		t.Fatal("unresolved path owner was reported as success")
	}
	if !strings.Contains(err.Error(), "no live owner row") {
		t.Fatalf("error = %v, want the unresolved-owner cause", err)
	}
}

// TestReplaceVirtualResultPin_CollisionAfterConcurrentRepairSurvives is the
// review regression. The CAS collides, then another session commits a repair
// that rotates this row onto a healthy candidate and stores fresh probe and URL
// evidence before the fallback's clear runs. The clear is fenced on the path and
// updated_at observed before the CAS, so it misses: the newer path and evidence
// must survive and the operation must report the concurrent modification
// instead of a clean success.
func TestReplaceVirtualResultPin_CollisionAfterConcurrentRepairSurvives(t *testing.T) {
	const fileID = 6506162
	deadPath := "virtual://movie/tt1200?result=dead"
	livePath := "virtual://movie/tt1200?result=live"
	repairedPath := "virtual://movie/tt1200?result=repaired"

	observedAt := collisionRevision()
	repairedAt := observedAt.Add(time.Second)
	repairedURL := "https://provider.invalid/repaired"

	// state is the losing row as another session leaves it after the collision.
	state := struct {
		path      string
		updatedAt time.Time
		url       *string
		probeAt   *time.Time
	}{
		path:      deadPath,
		updatedAt: observedAt,
	}

	db := &fakeVirtualPinDB{}
	db.execFunc = func(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
		if !strings.Contains(sql, "SET file_path = $1") {
			t.Fatalf("first write is not the path CAS: %s", sql)
		}
		// The CAS fails against the sibling owner, then the competing repair
		// commits before the fallback resumes.
		state.path = repairedPath
		state.updatedAt = repairedAt
		state.url = &repairedURL
		state.probeAt = &repairedAt
		return pgconn.CommandTag{}, uniquePathViolation()
	}
	db.queryRowFunc = func(_ context.Context, sql string, args ...any) pgx.Row {
		if !strings.Contains(sql, "WITH owner AS") {
			if !strings.Contains(sql, "SELECT file_path, updated_at FROM media_files WHERE id = $1") {
				t.Fatalf("unexpected query: %s", sql)
			}
			return fakePinRow{path: state.path, updatedAt: state.updatedAt}
		}
		// Model the revision fence: the clear only applies to the state the
		// caller observed before its CAS.
		if len(args) != 4 || args[2] != state.path || !args[3].(time.Time).Equal(state.updatedAt) {
			return fakePinRow{id: 6507000, path: livePath, cleared: 0}
		}
		state.url = nil
		state.probeAt = nil
		return fakePinRow{id: 6507000, path: livePath, cleared: 1}
	}

	update, err := replaceVirtualResultPinWithDB(context.Background(), db, fileID, deadPath, livePath)
	if err == nil {
		t.Fatalf("stale collision cleanup reported success with owner %+v; the newer repair must not be erased", update.Owner)
	}
	if update.Replaced {
		t.Fatal("stale collision cleanup reported the pin as replaced")
	}
	if !errors.Is(err, errVirtualPinRevisionSuperseded) {
		t.Fatalf("error = %v, want the revision-superseded cause", err)
	}
	if state.path != repairedPath {
		t.Fatalf("newer path was overwritten: got %q, want %q", state.path, repairedPath)
	}
	if state.url == nil || *state.url != repairedURL {
		t.Fatalf("newer resolved URL was erased: got %v, want %q", state.url, repairedURL)
	}
	if state.probeAt == nil || !state.probeAt.Equal(repairedAt) {
		t.Fatalf("newer probe evidence was erased: got %v, want %v", state.probeAt, repairedAt)
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
	observedAt := collisionRevision()

	db := &fakeVirtualPinDB{}
	db.queryRowFunc = func(_ context.Context, sql string, args ...any) pgx.Row {
		switch len(db.queries) {
		case 1:
			if !strings.Contains(sql, "SELECT file_path, updated_at FROM media_files WHERE id = $1") {
				t.Fatalf("first query is not the revision read: %s", sql)
			}
			return fakePinRow{path: deadPath, updatedAt: observedAt}
		case 2:
			if !strings.Contains(sql, "WITH owner AS") {
				t.Fatalf("owner lookup does not resolve the neutral path: %s", sql)
			}
			if !strings.Contains(sql, "updated_at = $4") || !strings.Contains(sql, "EXISTS (SELECT 1 FROM owner)") {
				t.Fatalf("evidence clear is not atomically fenced: %s", sql)
			}
			want := []any{fileID, neutralPath, deadPath, observedAt}
			if len(args) != len(want) {
				t.Fatalf("evidence clear args = %v, want %v", args, want)
			}
			for i := range want {
				if args[i] != want[i] {
					t.Fatalf("evidence clear args[%d] = %v, want %v", i, args[i], want[i])
				}
			}
			return fakePinRow{id: ownerID, path: neutralPath, cleared: 1}
		default:
			t.Fatalf("unexpected extra query: %s", sql)
			return fakePinRow{err: pgx.ErrNoRows}
		}
	}
	db.execFunc = func(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
		if len(db.execs) != 1 {
			t.Fatalf("unexpected extra exec: %s", sql)
		}
		if !strings.Contains(sql, "SET file_path = $1") {
			t.Fatalf("first write is not the unpin CAS: %s", sql)
		}
		if len(args) != 3 || args[0] != neutralPath || args[1] != fileID || args[2] != deadPath {
			t.Fatalf("unpin args = %v, want neutral=%q id=%d expected=%q", args, neutralPath, fileID, deadPath)
		}
		return pgconn.CommandTag{}, uniquePathViolation()
	}

	if err := unpinVirtualResultWithDB(context.Background(), db, fileID, deadPath); err != nil {
		t.Fatalf("neutral-path collision reported a failure: %v", err)
	}
	if len(db.execs) != 1 {
		t.Fatalf("exec calls = %d, want the unpin CAS only", len(db.execs))
	}
	if len(db.queries) != 2 {
		t.Fatalf("query calls = %d, want the revision read plus the fenced clear", len(db.queries))
	}
}

// TestUnpinVirtualResult_GenuineUpdateFailureSurfaces keeps ordinary unpin
// errors visible instead of treating every failure as a collision.
func TestUnpinVirtualResult_GenuineUpdateFailureSurfaces(t *testing.T) {
	writeErr := errors.New("update path failed")
	db := &fakeVirtualPinDB{
		queryRowFunc: func(context.Context, string, ...any) pgx.Row {
			return fakePinRow{path: "virtual://movie/tt7?result=dead", updatedAt: collisionRevision()}
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
		t.Fatalf("a non-23505 failure performed %d extra queries", len(db.queries)-1)
	}
}

// racingVirtualPinDB wraps a real pool and runs afterCAS once, immediately
// after the first path CAS Exec returns (success or 23505) and before the
// caller's collision fallback resumes. That deterministic interleaving is what
// lets the test commit a competing repair between the collision and the clear.
type racingVirtualPinDB struct {
	pool     *pgxpool.Pool
	afterCAS func()
	raced    bool
}

func (db *racingVirtualPinDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	tag, err := db.pool.Exec(ctx, sql, args...)
	if !db.raced && strings.Contains(sql, "SET file_path = $1") {
		db.raced = true
		db.afterCAS()
	}
	return tag, err
}

func (db *racingVirtualPinDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return db.pool.QueryRow(ctx, sql, args...)
}

// TestReplaceVirtualResultPin_ConcurrentRepairAfterCollisionKeepsNewEvidence
// runs the real repository against Postgres and deterministically interleaves
// another session's repair between the failed CAS and the collision fallback.
// The repair rotates the losing row onto a healthy candidate and writes fresh
// probe and URL evidence. The stale fallback must not erase that evidence and
// must not report a clean success.
func TestReplaceVirtualResultPin_ConcurrentRepairAfterCollisionKeepsNewEvidence(t *testing.T) {
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)

	suffix := time.Now().UnixNano()
	contentID := fmt.Sprintf("virtual-pin-collision-race-%d", suffix)
	deadPath := fmt.Sprintf("virtual://movie/tt%d?result=dead-candidate", suffix)
	livePath := fmt.Sprintf("virtual://movie/tt%d?result=live-winner", suffix)
	repairedPath := fmt.Sprintf("virtual://movie/tt%d?result=repaired-candidate", suffix)

	var folderID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Pin Collision Race %d", suffix)).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_files WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_item_libraries WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, folderID)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id,type,title,status,genres)
		VALUES($1,'movie','Pin Collision Race Item','matched','{}'::text[])`, contentID); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_item_libraries(content_id,media_folder_id)
		VALUES($1,$2)`, contentID, folderID); err != nil {
		t.Fatalf("seed item library: %v", err)
	}

	// Row A: the losing row, with dead-candidate evidence the stale fallback
	// would clear if it were not revision-fenced.
	var deadFileID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id,
			probe_updated_at,probe_source,resolved_url,resolved_url_expires_at)
		VALUES($1,$2,$3,0,'mkv',5,NOW(),'dead-probe','https://provider.invalid/dead',NOW() + interval '1 hour')
		RETURNING id`, contentID, folderID, deadPath).Scan(&deadFileID); err != nil {
		t.Fatalf("seed dead-pinned file: %v", err)
	}
	// Row B: the sibling live row that already owns the winning tuple.
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id)
		VALUES($1,$2,$3,0,'mkv',5)`, contentID, folderID, livePath); err != nil {
		t.Fatalf("seed sibling live file: %v", err)
	}

	db := &racingVirtualPinDB{pool: pool}
	db.afterCAS = func() {
		// Another session repairs the losing row after the collision commits,
		// before this session's fallback gets to clear evidence.
		tag, err := pool.Exec(ctx, `
			UPDATE media_files
			SET file_path = $1,
				probe_updated_at = NOW(),
				probe_source = 'repair-probe',
				resolved_url = 'https://provider.invalid/healthy',
				resolved_url_expires_at = NOW() + interval '1 hour',
				updated_at = GREATEST(clock_timestamp(), updated_at + interval '1 microsecond')
			WHERE id = $2 AND file_path = $3`, repairedPath, deadFileID, deadPath)
		if err != nil {
			t.Fatalf("commit competing repair: %v", err)
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("competing repair affected %d rows, want 1", tag.RowsAffected())
		}
	}

	replaced, err := replaceVirtualResultPinWithDB(ctx, db, deadFileID, deadPath, livePath)
	if err == nil {
		t.Fatal("stale collision fallback reported success; it must not erase the committed repair")
	}
	if replaced.Replaced {
		t.Fatal("stale collision fallback reported the pin as replaced")
	}
	if !errors.Is(err, errVirtualPinRevisionSuperseded) {
		t.Fatalf("error = %v, want the revision-superseded cause", err)
	}

	var gotPath, gotProbeSource string
	var gotURL string
	var gotProbeUpdatedAt time.Time
	if err := pool.QueryRow(ctx, `
		SELECT file_path, probe_source, resolved_url, probe_updated_at
		FROM media_files WHERE id=$1`, deadFileID).
		Scan(&gotPath, &gotProbeSource, &gotURL, &gotProbeUpdatedAt); err != nil {
		t.Fatalf("fetch repaired row: %v", err)
	}
	if gotPath != repairedPath {
		t.Fatalf("repair path was overwritten: got %q, want %q", gotPath, repairedPath)
	}
	if gotProbeSource != "repair-probe" || gotURL != "https://provider.invalid/healthy" {
		t.Fatalf("repair evidence was erased: probe_source=%q resolved_url=%q", gotProbeSource, gotURL)
	}
	if gotProbeUpdatedAt.IsZero() {
		t.Fatal("repair probe evidence was erased")
	}
}
