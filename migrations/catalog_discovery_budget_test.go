package migrations

import "testing"

// The catalog detail discovery budget moves from the old ceiling to the new
// default. Any value the old 10 s ceiling could express was either the seed or
// a cutoff that starved provider listings, so it is raised; a value above the
// ceiling is an administrator's choice and stays, and a missing row already
// falls back to the code default.
func TestCatalogDiscoveryBudgetMigrationPostgres(t *testing.T) {
	const (
		migration = "20261108201341_raise_stuck_catalog_discovery_budget"
		key       = "virtual_library.catalog_discovery_budget_ms"
	)
	for _, tc := range []struct {
		name   string
		stored string // "" leaves the row out
		want   string
	}{
		{"old ceiling", "10000", "20000"},
		{"old default", "3000", "20000"},
		{"old minimum", "500", "20000"},
		{"padded old ceiling", " 10000 ", "20000"},
		{"admin value at the new maximum", "20000", "20000"},
		{"admin value above the old ceiling", "15000", "15000"},
		{"unparseable value is left to the reader", "soon", "soon"},
		{"no row", "", "no row"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, schema := adminMigrationFixture(t)
			migrationExec(t, tx, `CREATE TABLE server_settings (key text PRIMARY KEY, value text NOT NULL);`)
			if tc.stored != "" {
				if _, err := tx.Exec(t.Context(), `INSERT INTO server_settings (key, value) VALUES ($1, $2)`, key, tc.stored); err != nil {
					t.Fatal(err)
				}
			}
			migrationExec(t, tx, adminMigrationSQL(t, migration, schema, false))
			var got string
			if err := tx.QueryRow(t.Context(), `SELECT
    COALESCE((SELECT value FROM server_settings WHERE key = $1), 'no row')`, key).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("%s = %q, want %q", key, got, tc.want)
			}
			// Down keeps the raised value: it cannot be told apart from a
			// chosen 20 s.
			migrationExec(t, tx, adminMigrationSQL(t, migration, schema, true))
			if err := tx.QueryRow(t.Context(), `SELECT
    COALESCE((SELECT value FROM server_settings WHERE key = $1), 'no row')`, key).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("after down, %s = %q, want %q", key, got, tc.want)
			}
		})
	}
}
