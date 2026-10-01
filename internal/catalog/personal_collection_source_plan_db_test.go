package catalog

import (
	"context"
	"strings"
	"testing"
)

func TestPersonalCollectionSourcePlanDB(t *testing.T) {
	f := newSeasonCollectionFixture(t)
	executor := f.resolver.queryExecutorForScope("", nil)
	usePersonalCollectionSource(executor, f.userID, f.collectionID)
	sql, args, err := executor.buildPreviewPageSQL(QueryDefinition{}, f.access(), 50, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := f.pool.Query(context.Background(), "EXPLAIN "+sql, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(line, "Seq Scan on seasons") {
			t.Fatalf("season branch scans all seasons:\n%s", line)
		}
		t.Log(line)
	}
}
