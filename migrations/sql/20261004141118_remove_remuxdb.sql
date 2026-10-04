-- +goose Up
DROP TABLE IF EXISTS public.remuxdb_match_evidence;
DELETE FROM server_settings
WHERE key IN ('remuxdb.enabled', 'remuxdb.base_url', 'remuxdb.token', 'remuxdb.submit_enabled');

-- +goose Down
SELECT 1;
