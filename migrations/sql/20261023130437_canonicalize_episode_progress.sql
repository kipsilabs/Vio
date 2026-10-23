-- +goose Up
-- Canonical progress identity migration.
--
-- Playback resolves an alias-owned media file to its canonical episode before
-- writing progress, history, resume hints, or scrobbles, and every watch-state
-- writer resolves through public.canonical_episode_id(). Rows written before
-- that fix (and rows written by old clients through progress sync or a direct
-- store call) can still be keyed by episode_aliases.alias_episode_id. Move those
-- rows onto the canonical episode so season/series completion, next-up, and
-- Continue Watching see one identity.
--
-- This is a one-time data repair. The same repair function is called by
-- ReconcileSeries after it inserts mappings, because the documented activation
-- sequence applies migrations with an empty alias table and populates it later.

-- One identity lookup for every watch-state write. The partial unique index on
-- episode_aliases.alias_episode_id makes this one indexed probe. An id with no
-- alias mapping is already canonical and is returned unchanged.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.canonical_episode_id(episode_row_id text)
RETURNS text
LANGUAGE sql
STABLE
AS $$
    SELECT COALESCE(
        (
            SELECT ea.canonical_episode_id
            FROM public.episode_aliases ea
            WHERE ea.alias_episode_id = episode_row_id
        ),
        episode_row_id
    )
$$;
-- +goose StatementEnd

-- Aggregate every progress row that names an alias of one canonical episode
-- and merge it with the canonical row that already exists, if any:
--
--   * completed        = OR across every alias and the canonical row;
--   * position/hints   = the row with the greatest updated_at, highest
--                        position_seconds on a tie, then the canonical row
--                        before an alias, then the lexicographically smallest
--                        source id. This is deterministic and keeps the
--                        newest resume point;
--   * duration         = greatest duration across every row (zero only when
--                        every row has zero);
--   * hide_from_continue = OR, so a hide intent on either identity survives;
--   * event_at         = greatest event time;
--   * updated_at       = greatest write time.
--
-- History rows keep their own ids and are retargeted in place. A history-hide
-- watermark moves to the canonical row and keeps the greatest watermark, so an
-- episode removed under either identity stays removed.
--
-- p_series_id limits the repair to one series' aliases; NULL repairs every
-- mapping. The function is idempotent: after it deletes the alias rows, a
-- second call for the same series finds nothing to move.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.canonicalize_episode_watch_state(p_series_id text DEFAULT NULL)
RETURNS bigint
LANGUAGE plpgsql
AS $$
DECLARE
    merged_rows bigint;
BEGIN
    -- The merge chooses event_at explicitly and advances updated_at to the
    -- greatest write time. Keep that chosen event_at instead of letting the
    -- stamp trigger replace it with the newer updated_at.
    PERFORM set_config('silo.explicit_progress_event_time', 'on', true);

    WITH alias_progress AS (
        SELECT
            p.user_id,
            p.profile_id,
            ea.canonical_episode_id AS media_item_id,
            p.media_item_id AS source_media_item_id,
            p.position_seconds,
            p.duration_seconds,
            p.completed,
            p.updated_at,
            p.last_file_id,
            p.last_resolution,
            p.last_hdr,
            p.last_codec_video,
            p.last_edition_key,
            p.hide_from_continue,
            p.event_at,
            TRUE AS is_alias
        FROM public.user_watch_progress p
        JOIN public.episode_aliases ea
          ON ea.alias_episode_id = p.media_item_id
        WHERE ea.alias_episode_id IS NOT NULL
          AND (p_series_id IS NULL OR ea.series_id = p_series_id)
    ),
    canonical_targets AS (
        SELECT DISTINCT user_id, profile_id, media_item_id
        FROM alias_progress
    ),
    candidates AS (
        SELECT * FROM alias_progress
        UNION ALL
        SELECT
            p.user_id,
            p.profile_id,
            p.media_item_id,
            p.media_item_id,
            p.position_seconds,
            p.duration_seconds,
            p.completed,
            p.updated_at,
            p.last_file_id,
            p.last_resolution,
            p.last_hdr,
            p.last_codec_video,
            p.last_edition_key,
            p.hide_from_continue,
            p.event_at,
            FALSE AS is_alias
        FROM public.user_watch_progress p
        JOIN canonical_targets t
          ON t.user_id = p.user_id
         AND t.profile_id = p.profile_id
         AND t.media_item_id = p.media_item_id
    ),
    ranked AS (
        SELECT
            c.*,
            ROW_NUMBER() OVER (
                PARTITION BY c.user_id, c.profile_id, c.media_item_id
                ORDER BY c.updated_at DESC,
                         c.position_seconds DESC,
                         c.is_alias ASC,
                         c.source_media_item_id ASC
            ) AS row_rank,
            BOOL_OR(c.completed) OVER (
                PARTITION BY c.user_id, c.profile_id, c.media_item_id
            ) AS any_completed,
            MAX(c.duration_seconds) OVER (
                PARTITION BY c.user_id, c.profile_id, c.media_item_id
            ) AS max_duration,
            BOOL_OR(c.hide_from_continue) OVER (
                PARTITION BY c.user_id, c.profile_id, c.media_item_id
            ) AS any_hidden,
            MAX(c.event_at) OVER (
                PARTITION BY c.user_id, c.profile_id, c.media_item_id
            ) AS max_event_at,
            MAX(c.updated_at) OVER (
                PARTITION BY c.user_id, c.profile_id, c.media_item_id
            ) AS max_updated_at
        FROM candidates c
    ),
    merged AS (
        SELECT
            user_id,
            profile_id,
            media_item_id,
            position_seconds,
            max_duration AS duration_seconds,
            any_completed AS completed,
            max_updated_at AS updated_at,
            last_file_id,
            last_resolution,
            last_hdr,
            last_codec_video,
            last_edition_key,
            any_hidden AS hide_from_continue,
            max_event_at AS event_at
        FROM ranked
        WHERE row_rank = 1
    )
    INSERT INTO public.user_watch_progress (
        user_id, profile_id, media_item_id, position_seconds, duration_seconds,
        completed, updated_at, last_file_id, last_resolution, last_hdr,
        last_codec_video, last_edition_key, hide_from_continue, event_at
    )
    SELECT
        user_id, profile_id, media_item_id, position_seconds, duration_seconds,
        completed, updated_at, last_file_id, last_resolution, last_hdr,
        last_codec_video, last_edition_key, hide_from_continue, event_at
    FROM merged
    ON CONFLICT (user_id, profile_id, media_item_id) DO UPDATE SET
        position_seconds = EXCLUDED.position_seconds,
        duration_seconds = EXCLUDED.duration_seconds,
        completed = EXCLUDED.completed,
        updated_at = EXCLUDED.updated_at,
        last_file_id = EXCLUDED.last_file_id,
        last_resolution = EXCLUDED.last_resolution,
        last_hdr = EXCLUDED.last_hdr,
        last_codec_video = EXCLUDED.last_codec_video,
        last_edition_key = EXCLUDED.last_edition_key,
        hide_from_continue = EXCLUDED.hide_from_continue,
        event_at = EXCLUDED.event_at;

    GET DIAGNOSTICS merged_rows = ROW_COUNT;

    -- Retarget history-hide watermarks before deleting the alias rows. An alias
    -- row and a canonical row can both carry a watermark for the same episode;
    -- keep the later one so removing either identity stays effective.
    WITH moved_hidden AS (
        SELECT
            hhi.user_id,
            hhi.profile_id,
            ea.canonical_episode_id AS media_item_id,
            MAX(hhi.hidden_before) AS hidden_before,
            MAX(hhi.updated_at) AS updated_at
        FROM public.user_history_hidden_items hhi
        JOIN public.episode_aliases ea
          ON ea.alias_episode_id = hhi.media_item_id
        WHERE ea.alias_episode_id IS NOT NULL
          AND (p_series_id IS NULL OR ea.series_id = p_series_id)
        GROUP BY hhi.user_id, hhi.profile_id, ea.canonical_episode_id
    )
    INSERT INTO public.user_history_hidden_items (
        user_id, profile_id, media_item_id, hidden_before, updated_at
    )
    SELECT user_id, profile_id, media_item_id, hidden_before, updated_at
    FROM moved_hidden
    ON CONFLICT (user_id, profile_id, media_item_id) DO UPDATE SET
        hidden_before = GREATEST(
            public.user_history_hidden_items.hidden_before,
            EXCLUDED.hidden_before
        ),
        updated_at = GREATEST(
            public.user_history_hidden_items.updated_at,
            EXCLUDED.updated_at
        );

    DELETE FROM public.user_history_hidden_items hhi
    USING public.episode_aliases ea
    WHERE hhi.media_item_id = ea.alias_episode_id
      AND ea.alias_episode_id IS NOT NULL
      AND (p_series_id IS NULL OR ea.series_id = p_series_id);

    -- History rows have their own immutable row id, so retarget them in place.
    UPDATE public.user_watch_history h
    SET media_item_id = ea.canonical_episode_id
    FROM public.episode_aliases ea
    WHERE h.media_item_id = ea.alias_episode_id
      AND ea.alias_episode_id IS NOT NULL
      AND (p_series_id IS NULL OR ea.series_id = p_series_id);

    DELETE FROM public.user_watch_progress p
    USING public.episode_aliases ea
    WHERE p.media_item_id = ea.alias_episode_id
      AND ea.alias_episode_id IS NOT NULL
      AND (p_series_id IS NULL OR ea.series_id = p_series_id);

    RETURN merged_rows;
END;
$$;
-- +goose StatementEnd

SELECT public.canonicalize_episode_watch_state(NULL);

-- +goose Down
-- The repair is intentionally irreversible: without the alias map there is no
-- safe way to know which canonical row originally came from an alias, and
-- re-splitting merged progress would lose user state. The functions are part of
-- this migration's write and repair contract, so they go with it.
DROP FUNCTION IF EXISTS public.canonicalize_episode_watch_state(text);
DROP FUNCTION IF EXISTS public.canonical_episode_id(text);
