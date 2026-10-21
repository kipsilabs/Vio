-- +goose Up
-- +goose StatementBegin
-- The fork channel moved from drondeseries/silo-virtual-library to
-- drondeseries/vio-virtual-library. Reconciliation keys on url, so the legacy
-- row moves in place: id, managed_key, source_kind and installation bindings
-- survive. Fresh installs are a no-op; reconcile recreates the corrected row.
UPDATE public.plugin_repositories
SET url = 'https://raw.githubusercontent.com/drondeseries/vio-virtual-library/main/catalog.json',
    display_name = 'Vio Virtual Library',
    updated_at = NOW()
WHERE url = 'https://raw.githubusercontent.com/drondeseries/silo-virtual-library/main/catalog.json'
  AND NOT EXISTS (
      SELECT 1 FROM public.plugin_repositories
      WHERE url = 'https://raw.githubusercontent.com/drondeseries/vio-virtual-library/main/catalog.json'
  );
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Restores database routing only, not availability of the old endpoint.
-- Display name intentionally stays "Vio Virtual Library"; the rebrand is not rolled back.
UPDATE public.plugin_repositories
SET url = 'https://raw.githubusercontent.com/drondeseries/silo-virtual-library/main/catalog.json',
    updated_at = NOW()
WHERE url = 'https://raw.githubusercontent.com/drondeseries/vio-virtual-library/main/catalog.json'
  AND NOT EXISTS (
      SELECT 1 FROM public.plugin_repositories
      WHERE url = 'https://raw.githubusercontent.com/drondeseries/silo-virtual-library/main/catalog.json'
  );
-- +goose StatementEnd
