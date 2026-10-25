-- +goose Up
ALTER TABLE playback_v3_attempts
    ADD COLUMN stream_reissues INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN current_transport_rendered BOOLEAN NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE playback_v3_attempts
    DROP COLUMN stream_reissues,
    DROP COLUMN current_transport_rendered;
