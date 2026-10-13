-- +goose Up
-- Durable, cluster-wide consecutive-refusal state for evidence-adoption writes.
--
-- A virtual row pinned to a provider ?result= id can keep losing a required
-- identity adoption (the provider renumbers result ids, so the pin is stale).
-- The refusal is safe on its own but must be bounded so the writer eventually
-- rotates onto the release that actually owns the bytes or reports a terminal
-- instead of looping forever. The trigger that reaches the bound must survive a
-- process restart and agree across replicas, so the consecutive count lives
-- here rather than in per-process memory. One row per (media file, pinned
-- path); an entry older than its expiry is treated as absent, so a provider
-- that recovers is not permanently treated as stale.
CREATE TABLE virtual_adoption_refusals (
    file_id       INTEGER  NOT NULL REFERENCES media_files (id) ON DELETE CASCADE,
    expected_path TEXT     NOT NULL,
    refusal_count INTEGER  NOT NULL DEFAULT 0,
    expires_at    TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (file_id, expected_path)
);

-- Bounds the opportunistic expired-row sweep so a long-lived deployment does
-- not retain refusal state past its window.
CREATE INDEX virtual_adoption_refusals_expires_at_idx
    ON virtual_adoption_refusals (expires_at);

-- +goose Down
DROP TABLE virtual_adoption_refusals;
