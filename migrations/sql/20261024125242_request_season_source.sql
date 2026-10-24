-- +goose Up
ALTER TABLE media_requests
    ADD COLUMN season_source TEXT NOT NULL DEFAULT '';

-- NOT VALID avoids scanning and locking every existing request while the
-- migration runs. New and updated rows still enforce the check; a later
-- maintenance migration can validate existing rows without blocking writes.
ALTER TABLE media_requests
    ADD CONSTRAINT media_requests_season_source_check
    CHECK (season_source IN ('', 'tvdb', 'tmdb')) NOT VALID;

-- +goose Down
ALTER TABLE media_requests
    DROP CONSTRAINT media_requests_season_source_check;

ALTER TABLE media_requests
    DROP COLUMN season_source;
