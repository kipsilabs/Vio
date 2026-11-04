package handlers

import (
	"context"
	"os"
	"testing"

	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/jackc/pgx/v5/pgxpool"
)

func catalogTransferPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func attemptEndedByAdmin(record *playback.AttemptRecordV3) bool {
	return record != nil && record.StoppedAt != nil
}
