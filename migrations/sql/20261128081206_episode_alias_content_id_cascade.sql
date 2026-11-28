-- +goose Up
-- episode_aliases joined the content_id family after the online re-id
-- (silo_rename_content_id) was defined. Renaming a series or episode updates
-- media_items/episodes in place, so every foreign key that names one must
-- cascade the UPDATE or the rename fails with a foreign key violation (#933).
-- The two composite same-series keys from 20261023125706 guard ownership as
-- well as identity, so they cascade both of their referenced columns.

ALTER TABLE episode_aliases
    DROP CONSTRAINT episode_aliases_series_id_fkey,
    DROP CONSTRAINT episode_aliases_canonical_episode_id_fkey,
    DROP CONSTRAINT episode_aliases_alias_episode_id_fkey,
    DROP CONSTRAINT episode_aliases_canonical_series_fkey,
    DROP CONSTRAINT episode_aliases_alias_series_fkey;

ALTER TABLE episode_aliases
    ADD CONSTRAINT episode_aliases_series_id_fkey
        FOREIGN KEY (series_id)
        REFERENCES media_items (content_id)
        ON DELETE CASCADE ON UPDATE CASCADE,
    ADD CONSTRAINT episode_aliases_canonical_episode_id_fkey
        FOREIGN KEY (canonical_episode_id)
        REFERENCES episodes (content_id)
        ON DELETE CASCADE ON UPDATE CASCADE,
    ADD CONSTRAINT episode_aliases_alias_episode_id_fkey
        FOREIGN KEY (alias_episode_id)
        REFERENCES episodes (content_id)
        ON DELETE CASCADE ON UPDATE CASCADE,
    ADD CONSTRAINT episode_aliases_canonical_series_fkey
        FOREIGN KEY (series_id, canonical_episode_id)
        REFERENCES episodes (series_id, content_id)
        ON DELETE CASCADE ON UPDATE CASCADE,
    ADD CONSTRAINT episode_aliases_alias_series_fkey
        FOREIGN KEY (series_id, alias_episode_id)
        REFERENCES episodes (series_id, content_id)
        ON DELETE CASCADE ON UPDATE CASCADE;

-- +goose Down
ALTER TABLE episode_aliases
    DROP CONSTRAINT episode_aliases_series_id_fkey,
    DROP CONSTRAINT episode_aliases_canonical_episode_id_fkey,
    DROP CONSTRAINT episode_aliases_alias_episode_id_fkey,
    DROP CONSTRAINT episode_aliases_canonical_series_fkey,
    DROP CONSTRAINT episode_aliases_alias_series_fkey;

ALTER TABLE episode_aliases
    ADD CONSTRAINT episode_aliases_series_id_fkey
        FOREIGN KEY (series_id)
        REFERENCES media_items (content_id)
        ON DELETE CASCADE,
    ADD CONSTRAINT episode_aliases_canonical_episode_id_fkey
        FOREIGN KEY (canonical_episode_id)
        REFERENCES episodes (content_id)
        ON DELETE CASCADE,
    ADD CONSTRAINT episode_aliases_alias_episode_id_fkey
        FOREIGN KEY (alias_episode_id)
        REFERENCES episodes (content_id)
        ON DELETE CASCADE,
    ADD CONSTRAINT episode_aliases_canonical_series_fkey
        FOREIGN KEY (series_id, canonical_episode_id)
        REFERENCES episodes (series_id, content_id)
        ON DELETE CASCADE,
    ADD CONSTRAINT episode_aliases_alias_series_fkey
        FOREIGN KEY (series_id, alias_episode_id)
        REFERENCES episodes (series_id, content_id)
        ON DELETE CASCADE;
