package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
	"github.com/jackc/pgx/v5"
)

// sqlPlaceholderRe matches a positional parameter, $1 through $N.
var sqlPlaceholderRe = regexp.MustCompile(`\$(\d+)`)

// sqlShapeDB captures the statement and argument slice the production saver
// passes to QueryRow, so a shape test can compare the two without a database.
type sqlShapeDB struct {
	sql  string
	args []any
}

func (d *sqlShapeDB) QueryRow(_ context.Context, sql string, arguments ...any) pgx.Row {
	d.sql = sql
	d.args = arguments
	return sqlShapeRow{}
}

// sqlShapeRow satisfies pgx.Row with a successful one-string scan so
// ExecVirtualFileMetadataUpdateResult runs a single QueryRow and returns the
// captured persisted path.
type sqlShapeRow struct{}

func (sqlShapeRow) Scan(dest ...any) error {
	if len(dest) == 1 {
		if s, ok := dest[0].(*string); ok {
			*s = "virtual://sql-shape"
		}
	}
	return nil
}

// stripSQLStringLiterals removes single-quoted string literals (honoring the
// doubled single-quote escape) so structural punctuation inside them is not
// counted. It reports whether every opened quote was closed.
func stripSQLStringLiterals(sql string) (string, bool) {
	var b strings.Builder
	inString := false
	for i := 0; i < len(sql); i++ {
		ch := sql[i]
		if !inString {
			if ch == '\'' {
				inString = true
				continue
			}
			b.WriteByte(ch)
			continue
		}
		if ch == '\'' {
			if i+1 < len(sql) && sql[i+1] == '\'' {
				i++ // escaped quote stays inside the literal
				continue
			}
			inString = false
		}
	}
	return b.String(), !inString
}

// TestVirtualFileMetadataUpdateSQLShape validates the production statement
// without Postgres: parentheses and quotes balance, and the $N placeholders
// agree exactly with the argument slice the production saver passes. The
// statement is captured from ExecVirtualFileMetadataUpdateResult, so the test
// fails if the SQL and the args drift apart, not just if the literal is
// malformed. This is the class of defect that a DB-gated test would catch only
// when SILO_TEST_DATABASE_URL is set; the fencing and adoption semantics are
// still proven by the DB-gated tests in
// playback_virtual_metadata_update_db_test.go and
// playback_virtual_verdict_fence_test.go.
//
// It proves: token-level balance and placeholder/arg agreement. It does not
// prove that the SQL parses server-side, that a placeholder sits in the
// semantically correct clause, or that column/table names exist.
func TestVirtualFileMetadataUpdateSQLShape(t *testing.T) {
	db := &sqlShapeDB{}
	args := models.VirtualFilePersistArgs{
		FileID:           1,
		ExpectedFilePath: "virtual://movie/tt-sql-shape",
		UpdatedAt:        time.Now(),
		StampProbe:       true,
		OwnerID:          5,
		LibraryID:        9,
	}
	if _, err := ExecVirtualFileMetadataUpdateResult(context.Background(), db, args); err != nil {
		t.Fatalf("ExecVirtualFileMetadataUpdateResult: %v", err)
	}
	if db.sql != VirtualFileMetadataUpdateSQL {
		t.Fatalf("captured statement does not match VirtualFileMetadataUpdateSQL:\n%s", db.sql)
	}

	stripped, balancedQuotes := stripSQLStringLiterals(db.sql)
	if !balancedQuotes {
		t.Fatal("VirtualFileMetadataUpdateSQL has an unterminated string literal")
	}
	if got := strings.Count(stripped, "(") - strings.Count(stripped, ")"); got != 0 {
		t.Fatalf("parenthesis balance = %d, want 0", got)
	}

	occurrences := map[int]int{}
	for _, match := range sqlPlaceholderRe.FindAllStringSubmatch(db.sql, -1) {
		n, err := strconv.Atoi(match[1])
		if err != nil {
			t.Fatalf("placeholder %q is not numeric: %v", match[0], err)
		}
		if n <= 0 {
			t.Fatalf("placeholder $%d is not 1-based", n)
		}
		occurrences[n]++
	}

	// The highest placeholder must equal the number of arguments, every index
	// below it must be used, and no index may exceed the arg count. A $22 with
	// 21 args, or an arg added without its placeholder, fails here.
	wantMax := len(db.args)
	for n := 1; n <= wantMax; n++ {
		if occurrences[n] == 0 {
			t.Fatalf("placeholder $%d is missing; only %d args were passed", n, wantMax)
		}
	}
	for n := range occurrences {
		if n > wantMax {
			t.Fatalf("placeholder $%d has no argument; only %d args were passed", n, wantMax)
		}
	}

	// Pin the expected multiplicity so a placeholder that is dropped or
	// duplicated without an accompanying arg change is caught. Reuse is
	// intentional (e.g. $16/$17 fence the sibling, verdict and CAS clauses).
	wantCounts := map[int]int{
		1: 1, 2: 3, 3: 1, 4: 1, 5: 1, 6: 1, 7: 1, 8: 1, 9: 1, 10: 2,
		11: 1, 12: 2, 13: 2, 14: 1, 15: 1, 16: 8, 17: 8, 18: 13, 19: 6,
		20: 2, 21: 2, 22: 1, 23: 4, 24: 1, 25: 2, 26: 2, 27: 2, 28: 2, 29: 2,
		30: 8, 31: 2, 32: 2,
	}
	for n, want := range wantCounts {
		if got := occurrences[n]; got != want {
			t.Fatalf("placeholder $%d used %d times, want %d", n, got, want)
		}
	}
	if len(occurrences) != len(wantCounts) {
		t.Fatalf("statement uses %d distinct placeholders, want %d", len(occurrences), len(wantCounts))
	}
}

// TestWritePlaybackSegmentErrorMapping pins the stopped-session outcome: a
// transcode that exited before the segment materialized (the error shape
// WaitForOpenSegment returns after a user stop kills ffmpeg) is a 404
// not-found, not a 500. A genuinely transient ErrManifestNotReady stays a
// retryable 503, and an unexpected error remains 500.
func TestWritePlaybackSegmentErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "missing segment",
			err:        playback.ErrSegmentNotFound,
			wantStatus: http.StatusNotFound,
			wantCode:   "not_found",
		},
		{
			name:       "transcode exited before segment",
			err:        fmt.Errorf("%w: %w", playback.ErrTranscodeFailed, errors.New("signal: killed")),
			wantStatus: http.StatusNotFound,
			wantCode:   "not_found",
		},
		{
			name:       "manifest not ready",
			err:        fmt.Errorf("restart: %w", playback.ErrManifestNotReady),
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "unavailable",
		},
		{
			name:       "session-bound candidate absent during restart",
			err:        fmt.Errorf("refresh transcode input: %w", fmt.Errorf("resolve virtual input: %w", virtuallibrary.ErrSessionBoundCandidateAbsent)),
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "virtual_resolve_failed",
		},
		{
			name:       "persisted candidate trusted during restart",
			err:        fmt.Errorf("refresh transcode input: %w", fmt.Errorf("resolve virtual input: %w", virtuallibrary.ErrPersistedCandidateTrusted)),
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "virtual_resolve_failed",
		},
		{
			name:       "unexpected",
			err:        errors.New("disk exploded"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   "internal_error",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writePlaybackSegmentError(rec, tc.err)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantCode) {
				t.Fatalf("body %q does not carry error code %q", rec.Body.String(), tc.wantCode)
			}
		})
	}
}
