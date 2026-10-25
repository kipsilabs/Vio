-- +goose Up
ALTER TABLE playback_v3_attempts
    ADD COLUMN stream_reissues INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN rendered_plan_keys TEXT[] NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE playback_v3_attempts
    DROP COLUMN stream_reissues,
    DROP COLUMN rendered_plan_keys;
