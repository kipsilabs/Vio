-- +goose Up
-- Provider listings that back a virtual library regularly take 17–25 s. The
-- old default (3 s) and ceiling (10 s) for the catalog detail discovery budget
-- cut every one of those listings off, so a detail read never learned the
-- item's versions. Saving the Streaming settings form persisted the value the
-- form showed, and a deployment that saved it under the old ceiling reads its
-- stored 10 s back instead of the new 20 s default: stored values win. Raise
-- the band the old ceiling could express to the new 20 s maximum. A value in
-- that band cannot be told apart from the seed the form wrote, and the budget
-- only bounds detached discovery (it never delays the read that starts it), so
-- moving the whole band costs no response latency. A value above the old
-- ceiling (10000) is an explicit choice and stays, and a missing row already
-- falls back to the 20 s default.
UPDATE server_settings
SET value = '20000'
WHERE key = 'virtual_library.catalog_discovery_budget_ms'
  AND btrim(value) ~ '^[0-9]+$'
  AND btrim(value)::numeric <= 10000;

-- +goose Down
-- Intentionally a no-op: after the update there is no reliable way to
-- distinguish a raised old value from an operator-chosen 20 s.
SELECT 1;
