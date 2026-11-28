package handlers

import (
	"os"
	"testing"

	"github.com/Silo-Server/silo-server/internal/testdb"
)

// TestMain provisions a disposable migrated database for this package so
// destructive fixtures cannot affect other suites sharing SILO_TEST_DATABASE_URL.
func TestMain(m *testing.M) {
	os.Exit(testdb.SetupPackage(m, "handlers"))
}
