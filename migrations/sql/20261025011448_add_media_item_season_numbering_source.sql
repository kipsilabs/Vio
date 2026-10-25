-- +goose Up
-- The provider whose season and episode numbering a series follows. A virtual
-- import records the source it actually used; empty means unknown. Request
-- season joins require a known source because TVDB and TMDB split seasons
-- differently, so an unknown row must not be joined by season number.
ALTER TABLE media_items
    ADD COLUMN season_numbering_source TEXT NOT NULL DEFAULT '';

-- NOT VALID avoids scanning every existing item while the migration runs. New
-- and updated rows still enforce the check; existing rows default to ''.
ALTER TABLE media_items
    ADD CONSTRAINT media_items_season_numbering_source_check
    CHECK (season_numbering_source IN ('', 'tvdb', 'tmdb')) NOT VALID;

-- +goose Down
ALTER TABLE media_items
    DROP CONSTRAINT media_items_season_numbering_source_check;

ALTER TABLE media_items
    DROP COLUMN season_numbering_source;
