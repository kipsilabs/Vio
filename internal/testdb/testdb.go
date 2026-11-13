// Package testdb provisions a disposable PostgreSQL database per test-binary
// run so destructive suites cannot truncate each other's fixtures.
//
// Usage from a package's TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(testdb.SetupPackage(m, "catalog")) }
//
// When SILO_TEST_DATABASE_URL is unset the suite runs unchanged (DB tests
// skip). Otherwise a fresh database is created, migrated with the real goose
// provider, pointed at via SILO_TEST_DATABASE_URL, and dropped afterwards.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/database"
	"github.com/Silo-Server/silo-server/migrations"
)

// SetupPackage creates a migrated disposable database, rewrites
// SILO_TEST_DATABASE_URL for the duration of m.Run, then drops it.
// It returns the exit code for os.Exit.
func SetupPackage(m *testing.M, tag string) (code int) {
	baseDSN := os.Getenv("SILO_TEST_DATABASE_URL")
	if baseDSN == "" {
		return m.Run()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	baseCfg, err := pgxpool.ParseConfig(baseDSN)
	if err != nil {
		log.Print("testdb: invalid SILO_TEST_DATABASE_URL; refusing to run")
		return 1
	}
	if !isDisposableBaseName(baseCfg.ConnConfig.Database) {
		log.Print("testdb: refusing to clone non-test database")
		return 1
	}
	name, err := disposableName(baseCfg.ConnConfig.Database, tag)
	if err != nil {
		log.Print("testdb: generate disposable name failed")
		return 1
	}
	dsn, err := rewriteDSN(baseDSN, name)
	if err != nil {
		log.Print("testdb: rewrite disposable DSN failed")
		return 1
	}
	dsnCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		log.Print("testdb: parse disposable config failed")
		return 1
	}
	adminCfg := baseCfg.Copy()
	adminCfg.ConnConfig.Database = "postgres"
	admin, err := pgxpool.NewWithConfig(ctx, adminCfg)
	if err != nil {
		log.Print("testdb: connect maintenance database failed")
		return 1
	}
	defer admin.Close()

	if _, err := admin.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{name}.Sanitize()); err != nil {
		log.Print("testdb: create disposable database failed")
		return 1
	}
	defer func() {
		adminCtx, adminCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer adminCancel()
		if _, err := admin.Exec(adminCtx, `DROP DATABASE IF EXISTS `+pgx.Identifier{name}.Sanitize()+` WITH (FORCE)`); err != nil {
			log.Print("testdb: drop disposable database failed")
			code = 1
		}
	}()

	pool, err := pgxpool.NewWithConfig(ctx, dsnCfg)
	if err != nil {
		log.Print("testdb: connect disposable database failed")
		return 1
	}
	defer pool.Close()
	if err := database.RunMigrations(ctx, pool, migrations.FS, "sql"); err != nil {
		log.Print("testdb: migrate disposable database failed")
		return 1
	}
	pool.Close()
	if err := os.Setenv("SILO_TEST_DATABASE_URL", dsn); err != nil {
		log.Print("testdb: set disposable DSN failed")
		return 1
	}
	defer func() {
		if err := os.Setenv("SILO_TEST_DATABASE_URL", baseDSN); err != nil {
			log.Print("testdb: restore test DSN failed")
			code = 1
		}
	}()
	return m.Run()
}

func isDisposableBaseName(name string) bool {
	lower := strings.ToLower(name)
	// scripts/ci/dbpins names every copy it makes with this prefix, so a
	// clone a test binary is handed is disposable by construction even when
	// the template's name carries no marker.
	return strings.HasPrefix(lower, "dbpins_") ||
		strings.Contains(lower, "test") || strings.Contains(lower, "purge")
}

func disposableName(base, tag string) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", errors.New("generate disposable name failed")
	}
	var b strings.Builder
	for _, r := range strings.ToLower(base + "_scope_" + tag) {
		if b.Len() == 30 {
			break
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String() + "_" + hex.EncodeToString(random[:]), nil
}

func rewriteDSN(dsn, name string) (string, error) {
	if name == "" || len(name) > 63 || strings.ContainsRune(name, 0) {
		return "", errors.New("invalid disposable database name")
	}
	if _, err := pgxpool.ParseConfig(dsn); err != nil {
		return "", errors.New("invalid base DSN")
	}
	var rewritten string
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", errors.New("invalid database URL")
		}
		q, err := url.ParseQuery(u.RawQuery)
		if err != nil {
			return "", errors.New("invalid database URL query")
		}
		q.Del("dbname")
		u.RawQuery = q.Encode()
		u.Path = "/" + name
		u.RawPath = ""
		rewritten = u.String()
	} else {
		escaped := strings.ReplaceAll(strings.ReplaceAll(name, `\`, `\\`), "'", `\'`)
		rewritten = dsn + " dbname='" + escaped + "'"
	}
	cfg, err := pgxpool.ParseConfig(rewritten)
	if err != nil || cfg.ConnConfig.Database != name {
		return "", errors.New("disposable DSN target verification failed")
	}
	return rewritten, nil
}
