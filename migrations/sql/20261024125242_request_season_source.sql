-- +goose Up
ALTER TABLE media_requests
    ADD COLUMN season_source TEXT NOT NULL DEFAULT '';

ALTER TABLE media_requests
    ADD CONSTRAINT media_requests_season_source_check
    CHECK (season_source IN ('', 'tvdb', 'tmdb'));

-- +goose Down
ALTER TABLE media_requests
    DROP CONSTRAINT media_requests_season_source_check;

ALTER TABLE media_requests
    DROP COLUMN season_source;
