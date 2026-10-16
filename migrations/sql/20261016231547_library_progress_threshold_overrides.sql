-- +goose Up
-- +goose StatementBegin
-- Per-library percent overrides of the server watched / min-resume progress
-- thresholds (playback.watched_threshold, default 90; and
-- playback.min_resume_threshold, default 5). NULL inherits the server
-- setting. Ranges match the settings: 1-100 watched, 1-99 min-resume; the
-- resolver keeps the effective pair consistent so min-resume stays below
-- watched even when only one side is overridden.
ALTER TABLE media_folders
    ADD COLUMN IF NOT EXISTS watched_threshold SMALLINT
        CHECK (watched_threshold BETWEEN 1 AND 100),
    ADD COLUMN IF NOT EXISTS min_resume_threshold SMALLINT
        CHECK (min_resume_threshold BETWEEN 1 AND 99);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE media_folders
    DROP COLUMN IF EXISTS watched_threshold,
    DROP COLUMN IF EXISTS min_resume_threshold;
-- +goose StatementEnd
