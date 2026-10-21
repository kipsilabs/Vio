package plugins

import "testing"

func TestForkVirtualRepositoryChannel(t *testing.T) {
	if ForkVirtualRepositoryURL != "https://raw.githubusercontent.com/drondeseries/vio-virtual-library/main/catalog.json" {
		t.Fatalf("unexpected fork URL: %s", ForkVirtualRepositoryURL)
	}
	for _, definition := range managedRepositoryDefinitions {
		if definition.Key == ForkVirtualRepositoryManagedKey {
			if definition.URL != ForkVirtualRepositoryURL || definition.DisplayName != "Vio Virtual Library" || definition.SourceKind != RepositorySourceSilo {
				t.Fatalf("unexpected fork definition: %+v", definition)
			}
			return
		}
	}
	t.Fatal("fork channel is not managed")
}

func TestForkVirtualRepositoryReconcilePreservesBindings(t *testing.T) {
	s := catalogSettingsTestStore(t)
	ctx := t.Context()
	var id int
	if err := s.pool.QueryRow(ctx, `INSERT INTO plugin_repositories (url, display_name, enabled, managed_key, source_kind) VALUES ($1, 'old name', false, $2, $3) RETURNING id`, ForkVirtualRepositoryURL, ForkVirtualRepositoryManagedKey, RepositorySourceSilo).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO plugin_installations (repository_id) VALUES ($1)`, id); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := s.ReconcileManaged(ctx); err != nil {
			t.Fatal(err)
		}
		var gotID int
		var name, key, source string
		var enabled bool
		if err := s.pool.QueryRow(ctx, `SELECT r.id, r.display_name, r.managed_key, r.source_kind, r.enabled FROM plugin_repositories r JOIN plugin_installations i ON i.repository_id = r.id WHERE r.url = $1`, ForkVirtualRepositoryURL).Scan(&gotID, &name, &key, &source, &enabled); err != nil {
			t.Fatal(err)
		}
		if gotID != id || name != ForkVirtualRepositoryName || key != ForkVirtualRepositoryManagedKey || source != RepositorySourceSilo || !enabled {
			t.Fatalf("reconciliation changed identity or failed to restore channel: %d %s %s %s %t", gotID, name, key, source, enabled)
		}
	}
}
