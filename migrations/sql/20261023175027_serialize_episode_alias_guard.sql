-- +goose Up
-- The identity guard's EXISTS checks are only correct when concurrent writers
-- to the same series are serialized. The repository takes this advisory lock
-- itself, but direct SQL and other writers do not, so take it in the trigger
-- at the database boundary. The key matches
-- lockEpisodeAliasSeries in internal/catalog/episode_alias_repo.go:

-- +goose StatementBegin
DO $$
DECLARE
    offending_episode_id TEXT;
BEGIN
    SELECT aliased.alias_episode_id
      INTO offending_episode_id
      FROM episode_aliases aliased
      JOIN episode_aliases canonical
        ON canonical.canonical_episode_id = aliased.alias_episode_id
       AND canonical.id <> aliased.id
     ORDER BY aliased.alias_episode_id
     LIMIT 1;

    IF offending_episode_id IS NOT NULL THEN
        RAISE EXCEPTION 'episode_aliases is not installable: episode % is both a canonical episode and an alias episode, so the table already contains a chain or cycle', offending_episode_id
            USING ERRCODE = '23514';
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION episode_aliases_guard_identity()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    -- Serialize every identity validation for this series, including writers
    -- that bypass the repository. The lock is transaction-scoped and
    -- reentrant, so the repository's earlier acquisition is not a conflict.
    PERFORM pg_advisory_xact_lock(hashtextextended(NEW.series_id, 0));

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

-- +goose Down
-- Restore the unsynchronized guard from 20261023125706.
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
