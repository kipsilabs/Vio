-- +goose Up
ALTER TABLE playback_v3_attempts
    ADD COLUMN audio_selection JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN audio_reconcile_ledger JSONB NOT NULL DEFAULT '{}'::jsonb;

-- +goose Down
ALTER TABLE playback_v3_attempts
    DROP COLUMN audio_reconcile_ledger,
    DROP COLUMN audio_selection;
