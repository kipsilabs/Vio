-- +goose Up
-- Enforce the alias map's identity invariants at the database boundary. The
-- original table migration only checked FK existence and self-aliasing; these
-- constraints make a malformed manual or reconciliation write fail closed.

-- Composite identity for same-series ownership checks. content_id is already
-- unique, so this index only supplies the referenced key.
CREATE UNIQUE INDEX episodes_series_content_key
    ON episodes (series_id, content_id);

ALTER TABLE episode_aliases
    ADD CONSTRAINT episode_aliases_canonical_series_fkey
        FOREIGN KEY (series_id, canonical_episode_id)
        REFERENCES episodes (series_id, content_id)
        ON DELETE CASCADE,
    ADD CONSTRAINT episode_aliases_alias_series_fkey
        FOREIGN KEY (series_id, alias_episode_id)
        REFERENCES episodes (series_id, content_id)
        ON DELETE CASCADE;

-- A provider coordinate identifies exactly one canonical episode in a series.
-- The old key included canonical_episode_id, so two canonical rows could claim
-- the same coordinate and ResolveCanonicalEpisodeID picked one arbitrarily.
CREATE UNIQUE INDEX episode_aliases_series_provider_key
    ON episode_aliases (
        series_id,
        provider_installation_id,
        provider_series_id,
        provider_season_number,
        provider_episode_number
    );

DROP INDEX episode_aliases_canonical_provider_key;

-- A canonical episode cannot itself be an alias, and an alias row cannot
-- become a canonical row. This blocks C->A plus A->B chains and cycles such as
-- C->A plus A->C without materializing or deleting episode rows.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION episode_aliases_guard_identity()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM episode_aliases
        WHERE id <> NEW.id
          AND alias_episode_id = NEW.canonical_episode_id
    ) THEN
        RAISE EXCEPTION 'episode % is already an alias row', NEW.canonical_episode_id
            USING ERRCODE = '23514';
    END IF;

    IF NEW.alias_episode_id IS NOT NULL AND EXISTS (
        SELECT 1
        FROM episode_aliases
        WHERE id <> NEW.id
          AND canonical_episode_id = NEW.alias_episode_id
    ) THEN
        RAISE EXCEPTION 'episode % is already a canonical episode', NEW.alias_episode_id
            USING ERRCODE = '23514';
    END IF;

    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER episode_aliases_guard_identity_trg
    BEFORE INSERT OR UPDATE ON episode_aliases
    FOR EACH ROW
    EXECUTE FUNCTION episode_aliases_guard_identity();

-- +goose Down
DROP TRIGGER IF EXISTS episode_aliases_guard_identity_trg ON episode_aliases;
DROP FUNCTION IF EXISTS episode_aliases_guard_identity();

DROP INDEX IF EXISTS episode_aliases_series_provider_key;
CREATE UNIQUE INDEX episode_aliases_canonical_provider_key
    ON episode_aliases (
        canonical_episode_id,
        provider_installation_id,
        provider_series_id,
        provider_season_number,
        provider_episode_number
    );

ALTER TABLE episode_aliases
    DROP CONSTRAINT IF EXISTS episode_aliases_alias_series_fkey,
    DROP CONSTRAINT IF EXISTS episode_aliases_canonical_series_fkey;

DROP INDEX IF EXISTS episodes_series_content_key;
