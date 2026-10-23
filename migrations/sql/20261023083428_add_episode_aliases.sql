-- +goose Up
-- Canonical episode identity plus provider/season-layout aliases. The alias
-- row names the episode the catalog should display and, when the alias already
-- exists as an episode row, the row it replaces for layout purposes.
CREATE TABLE episode_aliases (
    id                        BIGSERIAL PRIMARY KEY,
    series_id                 TEXT NOT NULL REFERENCES media_items (content_id) ON DELETE CASCADE,
    canonical_episode_id      TEXT NOT NULL REFERENCES episodes (content_id) ON DELETE CASCADE,
    alias_episode_id          TEXT REFERENCES episodes (content_id) ON DELETE CASCADE,
    provider_installation_id  BIGINT NOT NULL DEFAULT 0,
    provider_series_id        TEXT NOT NULL DEFAULT '',
    provider_season_number    INTEGER NOT NULL DEFAULT 0,
    provider_episode_number   INTEGER NOT NULL DEFAULT 0,
    source                    TEXT NOT NULL,
    confidence                SMALLINT NOT NULL DEFAULT 100 CHECK (confidence BETWEEN 0 AND 100),
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (alias_episode_id IS NULL OR alias_episode_id <> canonical_episode_id)
);

-- One mapping per canonical episode and provider coordinate. The local-layout
-- alias uses installation/series zero and the layout's season/episode values.
CREATE UNIQUE INDEX episode_aliases_canonical_provider_key
    ON episode_aliases (
        canonical_episode_id,
        provider_installation_id,
        provider_series_id,
        provider_season_number,
        provider_episode_number
    );

-- A concrete episode row can be an alias of only one canonical episode.
CREATE UNIQUE INDEX episode_aliases_alias_episode_key
    ON episode_aliases (alias_episode_id)
    WHERE alias_episode_id IS NOT NULL;

CREATE INDEX episode_aliases_series_idx ON episode_aliases (series_id);

-- +goose Down
DROP TABLE episode_aliases;
