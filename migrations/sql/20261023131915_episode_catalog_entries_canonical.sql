-- +goose Up
-- Make episode_catalog_entries canonical by construction. The entry is keyed
-- by the canonical episode and aggregates membership and file statistics from
-- that episode's direct aliases, so search, home sections, facets and counts
-- keep reading ece.episode_id without a read-time mapping.
--
-- Membership can live on an alias row while the canonical episode has no
-- episode_libraries row and no file. The refresh function below resolves the
-- canonical id first, then builds the member set (canonical plus direct
-- aliases; chains are forbidden by the alias invariants) and refreshes one
-- entry under the canonical id.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.refresh_episode_catalog_entry(p_episode_id text, p_media_folder_id integer)
RETURNS void
LANGUAGE plpgsql
AS $$
DECLARE
    src record;
    v_canonical_id text;
    v_member_ids text[];
BEGIN
    IF p_episode_id IS NULL OR p_media_folder_id IS NULL THEN
        RETURN;
    END IF;

    -- An alias row names the episode the catalog displays. Any other episode
    -- (including a canonical id) is its own canonical id.
    SELECT canonical_episode_id
    INTO v_canonical_id
    FROM public.episode_aliases
    WHERE alias_episode_id = p_episode_id;

    v_canonical_id := COALESCE(v_canonical_id, p_episode_id);

    -- Direct aliases only. The invariants migration forbids alias chains and
    -- cycles, so one level is the complete member set.
    v_member_ids := ARRAY(
        SELECT member_id
        FROM (
            SELECT v_canonical_id AS member_id
            UNION
            SELECT alias_episode_id
            FROM public.episode_aliases
            WHERE canonical_episode_id = v_canonical_id
              AND alias_episode_id IS NOT NULL
        ) members
    );

    -- Defensive: an entry keyed by an alias member is stale under the
    -- canonical-by-construction rule. Only the canonical id may own a row.
    DELETE FROM public.episode_catalog_entries
    WHERE media_folder_id = p_media_folder_id
      AND episode_id = ANY(v_member_ids)
      AND episode_id <> v_canonical_id;

    -- No membership for any member in this folder: the canonical entry must
    -- not exist, even if a previous refresh created it from a since-removed
    -- alias membership.
    IF NOT EXISTS (
        SELECT 1
        FROM public.episode_libraries
        WHERE media_folder_id = p_media_folder_id
          AND episode_id = ANY(v_member_ids)
    ) THEN
        DELETE FROM public.episode_catalog_entries
        WHERE media_folder_id = p_media_folder_id
          AND episode_id = v_canonical_id;
        RETURN;
    END IF;

    SELECT
        e.series_id,
        LOWER(COALESCE(NULLIF(BTRIM(e.title), ''), 'Episode ' || e.episode_number::text)) AS sort_key,
        COALESCE(NULLIF(BTRIM(e.title), ''), 'Episode ' || e.episode_number::text) AS title,
        membership.added_at,
        e.air_date AS episode_air_date,
        COALESCE(si.year, EXTRACT(YEAR FROM e.air_date)::integer, 0) AS year,
        COALESCE(si.genres, '{}'::text[]) AS genres,
        COALESCE(si.studios, '{}'::text[]) AS studios,
        COALESCE(si.networks, '{}'::text[]) AS networks,
        COALESCE(si.countries, '{}'::text[]) AS countries,
        COALESCE(si.original_language, '') AS original_language,
        COALESCE(si.content_rating, '') AS content_rating,
        LOWER(COALESCE(NULLIF(BTRIM(si.content_rating), ''), '~~~~')) AS content_rating_label,
        si.content_rating_age,
        si.advisory_age,
        COALESCE(NULLIF(BTRIM(si.status), ''), 'matched') AS status,
        COALESCE(NULLIF(e.runtime, 0), COALESCE(si.runtime, 0)) AS runtime,
        e.rating_imdb,
        e.rating_tmdb,
        stats.max_resolution_rank,
        COALESCE(stats.resolution_codes, '{}'::text[]) AS resolution_codes,
        stats.max_bitrate,
        stats.min_bitrate,
        COALESCE(stats.has_hdr, false) AS has_hdr,
        COALESCE(stats.has_non_hdr, false) AS has_non_hdr,
        COALESCE(stats.has_dolby_vision, false) AS has_dolby_vision,
        COALESCE(stats.has_non_dolby_vision, false) AS has_non_dolby_vision,
        COALESCE(stats.audio_language_codes, '{}'::text[]) AS audio_language_codes,
        COALESCE(stats.subtitle_language_codes, '{}'::text[]) AS subtitle_language_codes,
        e.created_at AS episode_created_at
    INTO src
    FROM public.episodes e
    JOIN public.media_items si ON si.content_id = e.series_id
    CROSS JOIN LATERAL (
        SELECT MIN(el.first_seen_at) AS added_at
        FROM public.episode_libraries el
        WHERE el.episode_id = ANY(v_member_ids)
          AND el.media_folder_id = p_media_folder_id
    ) membership
    LEFT JOIN LATERAL (
        SELECT
            MAX(public.episode_catalog_resolution_rank(mf.resolution)) AS max_resolution_rank,
            ARRAY(
                SELECT DISTINCT code
                FROM public.media_files mf_res
                CROSS JOIN LATERAL (
                    SELECT public.episode_catalog_normalized_resolution(mf_res.resolution) AS code
                ) normalized
                WHERE mf_res.episode_id = ANY(v_member_ids)
                  AND mf_res.media_folder_id = p_media_folder_id
                  AND mf_res.missing_since IS NULL
                  AND normalized.code IS NOT NULL
                ORDER BY code
            ) AS resolution_codes,
            MAX(mf.bitrate) FILTER (WHERE mf.bitrate IS NOT NULL AND mf.bitrate > 0) AS max_bitrate,
            MIN(mf.bitrate) FILTER (WHERE mf.bitrate IS NOT NULL AND mf.bitrate > 0) AS min_bitrate,
            BOOL_OR(mf.hdr IS TRUE) AS has_hdr,
            BOOL_OR(mf.hdr IS FALSE) AS has_non_hdr,
            BOOL_OR(EXISTS (
                SELECT 1
                FROM jsonb_array_elements(COALESCE(mf.video_tracks, '[]'::jsonb)) AS vt
                WHERE NULLIF(BTRIM(vt->>'dolby_vision'), '') IS NOT NULL
            )) AS has_dolby_vision,
            BOOL_OR(NOT EXISTS (
                SELECT 1
                FROM jsonb_array_elements(COALESCE(mf.video_tracks, '[]'::jsonb)) AS vt
                WHERE NULLIF(BTRIM(vt->>'dolby_vision'), '') IS NOT NULL
            )) AS has_non_dolby_vision,
            ARRAY(
                SELECT DISTINCT LOWER(NULLIF(BTRIM(lang), ''))
                FROM public.media_files mf_audio
                CROSS JOIN LATERAL UNNEST(COALESCE(mf_audio.audio_language_codes, '{}'::text[])) AS lang
                WHERE mf_audio.episode_id = ANY(v_member_ids)
                  AND mf_audio.media_folder_id = p_media_folder_id
                  AND mf_audio.missing_since IS NULL
                  AND NULLIF(BTRIM(lang), '') IS NOT NULL
                ORDER BY LOWER(NULLIF(BTRIM(lang), ''))
            ) AS audio_language_codes,
            ARRAY(
                SELECT DISTINCT lang_code
                FROM (
                    SELECT LOWER(NULLIF(BTRIM(lang), '')) AS lang_code
                    FROM public.media_files mf_sub
                    CROSS JOIN LATERAL UNNEST(COALESCE(mf_sub.subtitle_language_codes, '{}'::text[])) AS lang
                    WHERE mf_sub.episode_id = ANY(v_member_ids)
                      AND mf_sub.media_folder_id = p_media_folder_id
                      AND mf_sub.missing_since IS NULL
                    UNION
                    SELECT LOWER(NULLIF(BTRIM(track->>'language'), '')) AS lang_code
                    FROM public.media_files mf_ext
                    CROSS JOIN LATERAL jsonb_array_elements(COALESCE(mf_ext.external_subtitles, '[]'::jsonb)) AS track
                    WHERE mf_ext.episode_id = ANY(v_member_ids)
                      AND mf_ext.media_folder_id = p_media_folder_id
                      AND mf_ext.missing_since IS NULL
                ) subtitle_codes
                WHERE lang_code IS NOT NULL
                ORDER BY lang_code
            ) AS subtitle_language_codes
        FROM public.media_files mf
        WHERE mf.episode_id = ANY(v_member_ids)
          AND mf.media_folder_id = p_media_folder_id
          AND mf.missing_since IS NULL
    ) stats ON TRUE
    WHERE e.content_id = v_canonical_id;

    IF NOT FOUND THEN
        DELETE FROM public.episode_catalog_entries
        WHERE media_folder_id = p_media_folder_id
          AND episode_id = v_canonical_id;
        RETURN;
    END IF;

    -- Update an existing entry in place, and only when a stored value changed:
    -- rewriting an unchanged entry still writes a new row version and an entry
    -- in each of its indexes. Missing search documents still get rebuilt by the
    -- BEFORE UPDATE trigger. An upsert fires the BEFORE INSERT trigger, which
    -- rebuilds both search documents before ON CONFLICT discards them. Retry
    -- when another session inserts or deletes this entry between statements.
    -- The two ROW comparisons must list every column the UPDATE assigns.
    LOOP
        UPDATE public.episode_catalog_entries SET
            series_id = src.series_id,
            sort_key = src.sort_key,
            title = src.title,
            added_at = src.added_at,
            episode_air_date = src.episode_air_date,
            year = src.year,
            genres = src.genres,
            studios = src.studios,
            networks = src.networks,
            countries = src.countries,
            original_language = src.original_language,
            content_rating = src.content_rating,
            content_rating_label = src.content_rating_label,
            content_rating_age = src.content_rating_age,
            advisory_age = src.advisory_age,
            status = src.status,
            runtime = src.runtime,
            rating_imdb = src.rating_imdb,
            rating_tmdb = src.rating_tmdb,
            max_resolution_rank = src.max_resolution_rank,
            resolution_codes = src.resolution_codes,
            max_bitrate = src.max_bitrate,
            min_bitrate = src.min_bitrate,
            has_hdr = src.has_hdr,
            has_non_hdr = src.has_non_hdr,
            has_dolby_vision = src.has_dolby_vision,
            has_non_dolby_vision = src.has_non_dolby_vision,
            audio_language_codes = src.audio_language_codes,
            subtitle_language_codes = src.subtitle_language_codes,
            episode_created_at = src.episode_created_at,
            updated_at = NOW()
        WHERE media_folder_id = p_media_folder_id
          AND episode_id = v_canonical_id
          AND (ROW(series_id, sort_key, title, added_at, episode_air_date, year, genres,
                  studios, networks, countries, original_language, content_rating,
                  content_rating_label, content_rating_age, advisory_age, status,
                  runtime, rating_imdb, rating_tmdb, max_resolution_rank,
                  resolution_codes, max_bitrate, min_bitrate, has_hdr, has_non_hdr,
                  has_dolby_vision, has_non_dolby_vision, audio_language_codes,
                  subtitle_language_codes, episode_created_at)
              IS DISTINCT FROM ROW(src.series_id, src.sort_key, src.title, src.added_at,
                  src.episode_air_date, src.year, src.genres, src.studios, src.networks,
                  src.countries, src.original_language, src.content_rating,
                  src.content_rating_label, src.content_rating_age, src.advisory_age,
                  src.status, src.runtime, src.rating_imdb, src.rating_tmdb,
                  src.max_resolution_rank, src.resolution_codes, src.max_bitrate,
                  src.min_bitrate, src.has_hdr, src.has_non_hdr, src.has_dolby_vision,
                  src.has_non_dolby_vision, src.audio_language_codes,
                  src.subtitle_language_codes, src.episode_created_at)
            OR search_title_normalized IS NULL
            OR search_title_vector IS NULL
            OR search_overview_vector IS NULL);
        EXIT WHEN FOUND;

        -- The entry already holds these values.
        PERFORM 1
        FROM public.episode_catalog_entries
        WHERE media_folder_id = p_media_folder_id
          AND episode_id = v_canonical_id
          AND ROW(series_id, sort_key, title, added_at, episode_air_date, year, genres,
                  studios, networks, countries, original_language, content_rating,
                  content_rating_label, content_rating_age, advisory_age, status,
                  runtime, rating_imdb, rating_tmdb, max_resolution_rank,
                  resolution_codes, max_bitrate, min_bitrate, has_hdr, has_non_hdr,
                  has_dolby_vision, has_non_dolby_vision, audio_language_codes,
                  subtitle_language_codes, episode_created_at)
              IS NOT DISTINCT FROM ROW(src.series_id, src.sort_key, src.title, src.added_at,
                  src.episode_air_date, src.year, src.genres, src.studios, src.networks,
                  src.countries, src.original_language, src.content_rating,
                  src.content_rating_label, src.content_rating_age, src.advisory_age,
                  src.status, src.runtime, src.rating_imdb, src.rating_tmdb,
                  src.max_resolution_rank, src.resolution_codes, src.max_bitrate,
                  src.min_bitrate, src.has_hdr, src.has_non_hdr, src.has_dolby_vision,
                  src.has_non_dolby_vision, src.audio_language_codes,
                  src.subtitle_language_codes, src.episode_created_at);
        EXIT WHEN FOUND;

        INSERT INTO public.episode_catalog_entries (
            media_folder_id,
            episode_id,
            series_id,
            sort_key,
            title,
            added_at,
            episode_air_date,
            year,
            genres,
            studios,
            networks,
            countries,
            original_language,
            content_rating,
            content_rating_label,
            content_rating_age,
            advisory_age,
            status,
            runtime,
            rating_imdb,
            rating_tmdb,
            max_resolution_rank,
            resolution_codes,
            max_bitrate,
            min_bitrate,
            has_hdr,
            has_non_hdr,
            has_dolby_vision,
            has_non_dolby_vision,
            audio_language_codes,
            subtitle_language_codes,
            episode_created_at,
            updated_at
        ) VALUES (
            p_media_folder_id,
            v_canonical_id,
            src.series_id,
            src.sort_key,
            src.title,
            src.added_at,
            src.episode_air_date,
            src.year,
            src.genres,
            src.studios,
            src.networks,
            src.countries,
            src.original_language,
            src.content_rating,
            src.content_rating_label,
            src.content_rating_age,
            src.advisory_age,
            src.status,
            src.runtime,
            src.rating_imdb,
            src.rating_tmdb,
            src.max_resolution_rank,
            src.resolution_codes,
            src.max_bitrate,
            src.min_bitrate,
            src.has_hdr,
            src.has_non_hdr,
            src.has_dolby_vision,
            src.has_non_dolby_vision,
            src.audio_language_codes,
            src.subtitle_language_codes,
            src.episode_created_at,
            NOW()
        )
        ON CONFLICT (media_folder_id, episode_id) DO NOTHING;
        EXIT WHEN FOUND;
    END LOOP;
END;
$$;

-- Refresh every folder where the requested episode or one of its direct
-- aliases has membership. The refresh itself resolves the canonical row, so a
-- canonical episode with no membership still gets refreshed from its aliases.
CREATE OR REPLACE FUNCTION public.refresh_episode_catalog_entries_for_episode(p_episode_id text)
RETURNS void
LANGUAGE sql
AS $$
    WITH canonical AS (
        SELECT COALESCE(
            (SELECT canonical_episode_id
             FROM public.episode_aliases
             WHERE alias_episode_id = p_episode_id),
            p_episode_id
        ) AS episode_id
    ),
    members AS (
        SELECT episode_id FROM canonical
        UNION
        SELECT alias_episode_id
        FROM public.episode_aliases
        JOIN canonical ON canonical.episode_id = episode_aliases.canonical_episode_id
        WHERE alias_episode_id IS NOT NULL
    )
    SELECT public.refresh_episode_catalog_entry(p_episode_id, folders.media_folder_id)
    FROM (
        SELECT DISTINCT el.media_folder_id
        FROM public.episode_libraries el
        WHERE el.episode_id IN (SELECT episode_id FROM members)
    ) folders;
$$;

CREATE OR REPLACE FUNCTION public.episode_catalog_entries_episode_libraries_trigger()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        -- Refresh after the row is gone: the deleted membership may have been
        -- the only one backing a canonical entry through its alias.
        PERFORM public.refresh_episode_catalog_entry(OLD.episode_id, OLD.media_folder_id);
        RETURN OLD;
    END IF;

    PERFORM public.refresh_episode_catalog_entry(NEW.episode_id, NEW.media_folder_id);
    IF TG_OP = 'UPDATE'
       AND (OLD.episode_id IS DISTINCT FROM NEW.episode_id OR OLD.media_folder_id IS DISTINCT FROM NEW.media_folder_id) THEN
        PERFORM public.refresh_episode_catalog_entry(OLD.episode_id, OLD.media_folder_id);
    END IF;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION public.episode_catalog_entries_media_files_trigger()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        PERFORM public.refresh_episode_catalog_entry(OLD.episode_id, OLD.media_folder_id);
        RETURN OLD;
    END IF;

    -- Refresh the old canonical id when the file moves episode or folder; the
    -- alias-aware refresh covers the canonical row on either side. For a plain
    -- attribute edit one refresh of the new row is enough.
    IF TG_OP = 'UPDATE' AND
       (OLD.episode_id IS DISTINCT FROM NEW.episode_id OR OLD.media_folder_id IS DISTINCT FROM NEW.media_folder_id) THEN
        PERFORM public.refresh_episode_catalog_entry(OLD.episode_id, OLD.media_folder_id);
    END IF;
    PERFORM public.refresh_episode_catalog_entry(NEW.episode_id, NEW.media_folder_id);
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION public.episode_catalog_entries_episodes_trigger()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE catalog_changed boolean; overview_changed boolean;
BEGIN
    IF TG_OP = 'DELETE' THEN
        -- A deleted alias episode does not delete the canonical entry: the
        -- canonical row is keyed by the canonical id and is reconciled by the
        -- membership, file and alias triggers. Drop only the entry keyed by
        -- this episode (its FK cascade normally does this already).
        PERFORM public.refresh_episode_catalog_entries_for_episode(OLD.content_id);
        DELETE FROM public.episode_catalog_entries WHERE episode_id = OLD.content_id;
        RETURN OLD;
    END IF;
    IF TG_OP = 'UPDATE' THEN
        catalog_changed := ROW(NEW.content_id, NEW.series_id, NEW.title,
            NEW.episode_number, NEW.air_date, NEW.runtime, NEW.rating_imdb,
            NEW.rating_tmdb, NEW.created_at)
            IS DISTINCT FROM ROW(OLD.content_id, OLD.series_id, OLD.title,
            OLD.episode_number, OLD.air_date, OLD.runtime, OLD.rating_imdb,
            OLD.rating_tmdb, OLD.created_at);
        overview_changed := NEW.overview IS DISTINCT FROM OLD.overview;
        IF NOT catalog_changed THEN
            IF overview_changed THEN
                UPDATE public.episode_catalog_entries
                SET search_overview_vector = to_tsvector('english', COALESCE(NEW.overview, '')),
                    updated_at = NOW()
                WHERE episode_id = NEW.content_id;
            END IF;
            RETURN NEW;
        END IF;
    END IF;
    PERFORM public.refresh_episode_catalog_entries_for_episode(NEW.content_id);
    IF TG_OP = 'UPDATE' AND OLD.content_id IS DISTINCT FROM NEW.content_id THEN
        DELETE FROM public.episode_catalog_entries WHERE episode_id = OLD.content_id;
    END IF;
    IF TG_OP = 'UPDATE' AND overview_changed THEN
        UPDATE public.episode_catalog_entries
        SET search_overview_vector = to_tsvector('english', COALESCE(NEW.overview, '')),
                    updated_at = NOW()
        WHERE episode_id = NEW.content_id;
    END IF;
    RETURN NEW;
END;
$$;

-- Alias reconciliation inserts/deletes mappings after episodes, memberships
-- and files already exist. Refresh every affected id (old and new sides) for
-- each folder where either side has membership. The refresh resolves aliases,
-- so both the canonical and alias calls converge on the same canonical row.
CREATE OR REPLACE FUNCTION public.episode_catalog_entries_episode_aliases_trigger()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    affected_ids text[];
    folder_id integer;
    affected_id text;
BEGIN
    IF TG_OP = 'INSERT' THEN
        affected_ids := ARRAY[NEW.canonical_episode_id, NEW.alias_episode_id];
    ELSIF TG_OP = 'DELETE' THEN
        affected_ids := ARRAY[OLD.canonical_episode_id, OLD.alias_episode_id];
    ELSE
        affected_ids := ARRAY[
            OLD.canonical_episode_id,
            OLD.alias_episode_id,
            NEW.canonical_episode_id,
            NEW.alias_episode_id
        ];
    END IF;

    FOR folder_id IN
        SELECT DISTINCT el.media_folder_id
        FROM public.episode_libraries el
        WHERE el.episode_id = ANY(affected_ids)
    LOOP
        FOR affected_id IN
            SELECT DISTINCT ids.id
            FROM UNNEST(affected_ids) AS ids(id)
            WHERE ids.id IS NOT NULL
        LOOP
            PERFORM public.refresh_episode_catalog_entry(affected_id, folder_id);
        END LOOP;
    END LOOP;

    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_episode_catalog_entries_episode_aliases ON public.episode_aliases;
CREATE TRIGGER trg_episode_catalog_entries_episode_aliases
AFTER INSERT OR UPDATE OR DELETE ON public.episode_aliases
FOR EACH ROW
EXECUTE FUNCTION public.episode_catalog_entries_episode_aliases_trigger();
-- +goose StatementEnd

-- Rebuild the read model from the canonical side. Alias-keyed entries are
-- stale by definition; refreshing every membership creates (or updates) the
-- canonical entry and aggregates the alias files. Re-running converges: the
-- delete removes any alias row a partial run left behind and the refresh is
-- skip-unchanged.
DELETE FROM public.episode_catalog_entries ece
USING public.episode_aliases ea
WHERE ece.episode_id = ea.alias_episode_id;

SELECT public.refresh_episode_catalog_entry(members.episode_id, members.media_folder_id)
FROM (
    SELECT DISTINCT episode_id, media_folder_id
    FROM public.episode_libraries
) members;

-- +goose Down
-- Restore the pre-alias read model: entries are keyed by the episode that
-- owns membership, with no canonical resolution.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.refresh_episode_catalog_entry(p_episode_id text, p_media_folder_id integer)
RETURNS void
LANGUAGE plpgsql
AS $$
DECLARE
    src record;
BEGIN
    IF p_episode_id IS NULL OR p_media_folder_id IS NULL THEN
        RETURN;
    END IF;

    SELECT
        e.series_id,
        LOWER(COALESCE(NULLIF(BTRIM(e.title), ''), 'Episode ' || e.episode_number::text)) AS sort_key,
        COALESCE(NULLIF(BTRIM(e.title), ''), 'Episode ' || e.episode_number::text) AS title,
        el.first_seen_at AS added_at,
        e.air_date AS episode_air_date,
        COALESCE(si.year, EXTRACT(YEAR FROM e.air_date)::integer, 0) AS year,
        COALESCE(si.genres, '{}'::text[]) AS genres,
        COALESCE(si.studios, '{}'::text[]) AS studios,
        COALESCE(si.networks, '{}'::text[]) AS networks,
        COALESCE(si.countries, '{}'::text[]) AS countries,
        COALESCE(si.original_language, '') AS original_language,
        COALESCE(si.content_rating, '') AS content_rating,
        LOWER(COALESCE(NULLIF(BTRIM(si.content_rating), ''), '~~~~')) AS content_rating_label,
        si.content_rating_age,
        si.advisory_age,
        COALESCE(NULLIF(BTRIM(si.status), ''), 'matched') AS status,
        COALESCE(NULLIF(e.runtime, 0), COALESCE(si.runtime, 0)) AS runtime,
        e.rating_imdb,
        e.rating_tmdb,
        stats.max_resolution_rank,
        COALESCE(stats.resolution_codes, '{}'::text[]) AS resolution_codes,
        stats.max_bitrate,
        stats.min_bitrate,
        COALESCE(stats.has_hdr, false) AS has_hdr,
        COALESCE(stats.has_non_hdr, false) AS has_non_hdr,
        COALESCE(stats.has_dolby_vision, false) AS has_dolby_vision,
        COALESCE(stats.has_non_dolby_vision, false) AS has_non_dolby_vision,
        COALESCE(stats.audio_language_codes, '{}'::text[]) AS audio_language_codes,
        COALESCE(stats.subtitle_language_codes, '{}'::text[]) AS subtitle_language_codes,
        e.created_at AS episode_created_at
    INTO src
    FROM public.episode_libraries el
    JOIN public.episodes e ON e.content_id = el.episode_id
    JOIN public.media_items si ON si.content_id = e.series_id
    LEFT JOIN LATERAL (
        SELECT
            MAX(public.episode_catalog_resolution_rank(mf.resolution)) AS max_resolution_rank,
            ARRAY(
                SELECT DISTINCT code
                FROM public.media_files mf_res
                CROSS JOIN LATERAL (
                    SELECT public.episode_catalog_normalized_resolution(mf_res.resolution) AS code
                ) normalized
                WHERE mf_res.episode_id = e.content_id
                  AND mf_res.media_folder_id = el.media_folder_id
                  AND mf_res.missing_since IS NULL
                  AND normalized.code IS NOT NULL
                ORDER BY code
            ) AS resolution_codes,
            MAX(mf.bitrate) FILTER (WHERE mf.bitrate IS NOT NULL AND mf.bitrate > 0) AS max_bitrate,
            MIN(mf.bitrate) FILTER (WHERE mf.bitrate IS NOT NULL AND mf.bitrate > 0) AS min_bitrate,
            BOOL_OR(mf.hdr IS TRUE) AS has_hdr,
            BOOL_OR(mf.hdr IS FALSE) AS has_non_hdr,
            BOOL_OR(EXISTS (
                SELECT 1
                FROM jsonb_array_elements(COALESCE(mf.video_tracks, '[]'::jsonb)) AS vt
                WHERE NULLIF(BTRIM(vt->>'dolby_vision'), '') IS NOT NULL
            )) AS has_dolby_vision,
            BOOL_OR(NOT EXISTS (
                SELECT 1
                FROM jsonb_array_elements(COALESCE(mf.video_tracks, '[]'::jsonb)) AS vt
                WHERE NULLIF(BTRIM(vt->>'dolby_vision'), '') IS NOT NULL
            )) AS has_non_dolby_vision,
            ARRAY(
                SELECT DISTINCT LOWER(NULLIF(BTRIM(lang), ''))
                FROM public.media_files mf_audio
                CROSS JOIN LATERAL UNNEST(COALESCE(mf_audio.audio_language_codes, '{}'::text[])) AS lang
                WHERE mf_audio.episode_id = e.content_id
                  AND mf_audio.media_folder_id = el.media_folder_id
                  AND mf_audio.missing_since IS NULL
                  AND NULLIF(BTRIM(lang), '') IS NOT NULL
                ORDER BY LOWER(NULLIF(BTRIM(lang), ''))
            ) AS audio_language_codes,
            ARRAY(
                SELECT DISTINCT lang_code
                FROM (
                    SELECT LOWER(NULLIF(BTRIM(lang), '')) AS lang_code
                    FROM public.media_files mf_sub
                    CROSS JOIN LATERAL UNNEST(COALESCE(mf_sub.subtitle_language_codes, '{}'::text[])) AS lang
                    WHERE mf_sub.episode_id = e.content_id
                      AND mf_sub.media_folder_id = el.media_folder_id
                      AND mf_sub.missing_since IS NULL
                    UNION
                    SELECT LOWER(NULLIF(BTRIM(track->>'language'), '')) AS lang_code
                    FROM public.media_files mf_ext
                    CROSS JOIN LATERAL jsonb_array_elements(COALESCE(mf_ext.external_subtitles, '[]'::jsonb)) AS track
                    WHERE mf_ext.episode_id = e.content_id
                      AND mf_ext.media_folder_id = el.media_folder_id
                      AND mf_ext.missing_since IS NULL
                ) subtitle_codes
                WHERE lang_code IS NOT NULL
                ORDER BY lang_code
            ) AS subtitle_language_codes
        FROM public.media_files mf
        WHERE mf.episode_id = e.content_id
          AND mf.media_folder_id = el.media_folder_id
          AND mf.missing_since IS NULL
    ) stats ON TRUE
    WHERE el.episode_id = p_episode_id
      AND el.media_folder_id = p_media_folder_id;

    IF NOT FOUND THEN
        DELETE FROM public.episode_catalog_entries
        WHERE episode_id = p_episode_id
          AND media_folder_id = p_media_folder_id;
        RETURN;
    END IF;

    -- Update an existing entry in place, and only when a stored value changed:
    -- rewriting an unchanged entry still writes a new row version and an entry
    -- in each of its indexes. Missing search documents still get rebuilt by the
    -- BEFORE UPDATE trigger. An upsert fires the BEFORE INSERT trigger, which
    -- rebuilds both search documents before ON CONFLICT discards them. Retry
    -- when another session inserts or deletes this entry between statements.
    -- The two ROW comparisons must list every column the UPDATE assigns.
    LOOP
        UPDATE public.episode_catalog_entries SET
            series_id = src.series_id,
            sort_key = src.sort_key,
            title = src.title,
            added_at = src.added_at,
            episode_air_date = src.episode_air_date,
            year = src.year,
            genres = src.genres,
            studios = src.studios,
            networks = src.networks,
            countries = src.countries,
            original_language = src.original_language,
            content_rating = src.content_rating,
            content_rating_label = src.content_rating_label,
            content_rating_age = src.content_rating_age,
            advisory_age = src.advisory_age,
            status = src.status,
            runtime = src.runtime,
            rating_imdb = src.rating_imdb,
            rating_tmdb = src.rating_tmdb,
            max_resolution_rank = src.max_resolution_rank,
            resolution_codes = src.resolution_codes,
            max_bitrate = src.max_bitrate,
            min_bitrate = src.min_bitrate,
            has_hdr = src.has_hdr,
            has_non_hdr = src.has_non_hdr,
            has_dolby_vision = src.has_dolby_vision,
            has_non_dolby_vision = src.has_non_dolby_vision,
            audio_language_codes = src.audio_language_codes,
            subtitle_language_codes = src.subtitle_language_codes,
            episode_created_at = src.episode_created_at,
            updated_at = NOW()
        WHERE media_folder_id = p_media_folder_id
          AND episode_id = p_episode_id
          AND (ROW(series_id, sort_key, title, added_at, episode_air_date, year, genres,
                  studios, networks, countries, original_language, content_rating,
                  content_rating_label, content_rating_age, advisory_age, status,
                  runtime, rating_imdb, rating_tmdb, max_resolution_rank,
                  resolution_codes, max_bitrate, min_bitrate, has_hdr, has_non_hdr,
                  has_dolby_vision, has_non_dolby_vision, audio_language_codes,
                  subtitle_language_codes, episode_created_at)
              IS DISTINCT FROM ROW(src.series_id, src.sort_key, src.title, src.added_at,
                  src.episode_air_date, src.year, src.genres, src.studios, src.networks,
                  src.countries, src.original_language, src.content_rating,
                  src.content_rating_label, src.content_rating_age, src.advisory_age,
                  src.status, src.runtime, src.rating_imdb, src.rating_tmdb,
                  src.max_resolution_rank, src.resolution_codes, src.max_bitrate,
                  src.min_bitrate, src.has_hdr, src.has_non_hdr, src.has_dolby_vision,
                  src.has_non_dolby_vision, src.audio_language_codes,
                  src.subtitle_language_codes, src.episode_created_at)
            OR search_title_normalized IS NULL
            OR search_title_vector IS NULL
            OR search_overview_vector IS NULL);
        EXIT WHEN FOUND;

        -- The entry already holds these values.
        PERFORM 1
        FROM public.episode_catalog_entries
        WHERE media_folder_id = p_media_folder_id
          AND episode_id = p_episode_id
          AND ROW(series_id, sort_key, title, added_at, episode_air_date, year, genres,
                  studios, networks, countries, original_language, content_rating,
                  content_rating_label, content_rating_age, advisory_age, status,
                  runtime, rating_imdb, rating_tmdb, max_resolution_rank,
                  resolution_codes, max_bitrate, min_bitrate, has_hdr, has_non_hdr,
                  has_dolby_vision, has_non_dolby_vision, audio_language_codes,
                  subtitle_language_codes, episode_created_at)
              IS NOT DISTINCT FROM ROW(src.series_id, src.sort_key, src.title, src.added_at,
                  src.episode_air_date, src.year, src.genres, src.studios, src.networks,
                  src.countries, src.original_language, src.content_rating,
                  src.content_rating_label, src.content_rating_age, src.advisory_age,
                  src.status, src.runtime, src.rating_imdb, src.rating_tmdb,
                  src.max_resolution_rank, src.resolution_codes, src.max_bitrate,
                  src.min_bitrate, src.has_hdr, src.has_non_hdr, src.has_dolby_vision,
                  src.has_non_dolby_vision, src.audio_language_codes,
                  src.subtitle_language_codes, src.episode_created_at);
        EXIT WHEN FOUND;

        INSERT INTO public.episode_catalog_entries (
            media_folder_id,
            episode_id,
            series_id,
            sort_key,
            title,
            added_at,
            episode_air_date,
            year,
            genres,
            studios,
            networks,
            countries,
            original_language,
            content_rating,
            content_rating_label,
            content_rating_age,
            advisory_age,
            status,
            runtime,
            rating_imdb,
            rating_tmdb,
            max_resolution_rank,
            resolution_codes,
            max_bitrate,
            min_bitrate,
            has_hdr,
            has_non_hdr,
            has_dolby_vision,
            has_non_dolby_vision,
            audio_language_codes,
            subtitle_language_codes,
            episode_created_at,
            updated_at
        ) VALUES (
            p_media_folder_id,
            p_episode_id,
            src.series_id,
            src.sort_key,
            src.title,
            src.added_at,
            src.episode_air_date,
            src.year,
            src.genres,
            src.studios,
            src.networks,
            src.countries,
            src.original_language,
            src.content_rating,
            src.content_rating_label,
            src.content_rating_age,
            src.advisory_age,
            src.status,
            src.runtime,
            src.rating_imdb,
            src.rating_tmdb,
            src.max_resolution_rank,
            src.resolution_codes,
            src.max_bitrate,
            src.min_bitrate,
            src.has_hdr,
            src.has_non_hdr,
            src.has_dolby_vision,
            src.has_non_dolby_vision,
            src.audio_language_codes,
            src.subtitle_language_codes,
            src.episode_created_at,
            NOW()
        )
        ON CONFLICT (media_folder_id, episode_id) DO NOTHING;
        EXIT WHEN FOUND;
    END LOOP;
END;
$$;

CREATE OR REPLACE FUNCTION public.refresh_episode_catalog_entries_for_episode(p_episode_id text)
RETURNS void
LANGUAGE sql
AS $$
    SELECT public.refresh_episode_catalog_entry(el.episode_id, el.media_folder_id)
    FROM public.episode_libraries el
    WHERE el.episode_id = p_episode_id;
$$;

CREATE OR REPLACE FUNCTION public.episode_catalog_entries_episode_libraries_trigger()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        DELETE FROM public.episode_catalog_entries
        WHERE episode_id = OLD.episode_id
          AND media_folder_id = OLD.media_folder_id;
        RETURN OLD;
    END IF;

    PERFORM public.refresh_episode_catalog_entry(NEW.episode_id, NEW.media_folder_id);
    IF TG_OP = 'UPDATE'
       AND (OLD.episode_id IS DISTINCT FROM NEW.episode_id OR OLD.media_folder_id IS DISTINCT FROM NEW.media_folder_id) THEN
        PERFORM public.refresh_episode_catalog_entry(OLD.episode_id, OLD.media_folder_id);
    END IF;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION public.episode_catalog_entries_media_files_trigger()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        PERFORM public.refresh_episode_catalog_entry(OLD.episode_id, OLD.media_folder_id);
        RETURN OLD;
    END IF;
    IF TG_OP = 'UPDATE' AND
       (OLD.episode_id IS DISTINCT FROM NEW.episode_id OR OLD.media_folder_id IS DISTINCT FROM NEW.media_folder_id) THEN
        PERFORM public.refresh_episode_catalog_entry(OLD.episode_id, OLD.media_folder_id);
    END IF;
    PERFORM public.refresh_episode_catalog_entry(NEW.episode_id, NEW.media_folder_id);
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION public.episode_catalog_entries_episodes_trigger()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE catalog_changed boolean; overview_changed boolean;
BEGIN
    IF TG_OP = 'DELETE' THEN
        DELETE FROM public.episode_catalog_entries WHERE episode_id = OLD.content_id;
        RETURN OLD;
    END IF;
    IF TG_OP = 'UPDATE' THEN
        catalog_changed := ROW(NEW.content_id, NEW.series_id, NEW.title,
            NEW.episode_number, NEW.air_date, NEW.runtime, NEW.rating_imdb,
            NEW.rating_tmdb, NEW.created_at)
            IS DISTINCT FROM ROW(OLD.content_id, OLD.series_id, OLD.title,
            OLD.episode_number, OLD.air_date, OLD.runtime, OLD.rating_imdb,
            OLD.rating_tmdb, OLD.created_at);
        overview_changed := NEW.overview IS DISTINCT FROM OLD.overview;
        IF NOT catalog_changed THEN
            IF overview_changed THEN
                UPDATE public.episode_catalog_entries
                SET search_overview_vector = to_tsvector('english', COALESCE(NEW.overview, '')),
                    updated_at = NOW()
                WHERE episode_id = NEW.content_id;
            END IF;
            RETURN NEW;
        END IF;
    END IF;
    PERFORM public.refresh_episode_catalog_entries_for_episode(NEW.content_id);
    IF TG_OP = 'UPDATE' AND OLD.content_id IS DISTINCT FROM NEW.content_id THEN
        DELETE FROM public.episode_catalog_entries WHERE episode_id = OLD.content_id;
    END IF;
    IF TG_OP = 'UPDATE' AND overview_changed THEN
        UPDATE public.episode_catalog_entries
        SET search_overview_vector = to_tsvector('english', COALESCE(NEW.overview, '')),
                    updated_at = NOW()
        WHERE episode_id = NEW.content_id;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_episode_catalog_entries_episode_aliases ON public.episode_aliases;
DROP FUNCTION IF EXISTS public.episode_catalog_entries_episode_aliases_trigger();
-- +goose StatementEnd

-- Rebuild alias-keyed entries so the previous trigger functions see the
-- same read model they wrote before this migration.
DELETE FROM public.episode_catalog_entries;

SELECT public.refresh_episode_catalog_entry(members.episode_id, members.media_folder_id)
FROM (
    SELECT DISTINCT episode_id, media_folder_id
    FROM public.episode_libraries
) members;
