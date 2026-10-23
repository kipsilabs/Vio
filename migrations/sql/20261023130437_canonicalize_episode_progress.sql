-- +goose Up
-- Canonical progress identity migration.
--
-- Playback now resolves an alias-owned media file to its canonical episode
-- before writing progress, history, resume hints, or scrobbles. Rows written
-- before that fix (and rows written by old clients through progress sync) can
-- still be keyed by episode_aliases.alias_episode_id. Move those rows onto the
-- canonical episode so season/series completion, next-up, and Continue
-- Watching see one identity.
--
-- This is a one-time data repair; the alias map is additive and the original
-- alias rows are preserved.

-- Keep one progress row per (user, profile, canonical episode). When a
-- canonical already has progress, merge the newest alias row into it.
WITH moved AS (
    SELECT DISTINCT ON (p.user_id, p.profile_id, ea.canonical_episode_id)
        p.user_id,
        p.profile_id,
        ea.canonical_episode_id AS media_item_id,
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
        p.synced_seq
    FROM user_watch_progress p
    JOIN episode_aliases ea ON ea.alias_episode_id = p.media_item_id
    WHERE ea.alias_episode_id IS NOT NULL
    ORDER BY p.user_id, p.profile_id, ea.canonical_episode_id, p.updated_at DESC
)
INSERT INTO user_watch_progress (
    user_id, profile_id, media_item_id, position_seconds, duration_seconds,
    completed, updated_at, last_file_id, last_resolution, last_hdr,
    last_codec_video, last_edition_key, hide_from_continue, event_at, synced_seq
)
SELECT
    user_id, profile_id, media_item_id, position_seconds, duration_seconds,
    completed, updated_at, last_file_id, last_resolution, last_hdr,
    last_codec_video, last_edition_key, hide_from_continue, event_at, synced_seq
FROM moved
ON CONFLICT (user_id, profile_id, media_item_id) DO UPDATE
SET position_seconds = CASE
        WHEN EXCLUDED.updated_at >= user_watch_progress.updated_at
        THEN EXCLUDED.position_seconds
        ELSE user_watch_progress.position_seconds
    END,
    duration_seconds = GREATEST(
        user_watch_progress.duration_seconds,
        EXCLUDED.duration_seconds
    ),
    completed = user_watch_progress.completed OR EXCLUDED.completed,
    updated_at = GREATEST(user_watch_progress.updated_at, EXCLUDED.updated_at),
    last_file_id = CASE
        WHEN EXCLUDED.updated_at >= user_watch_progress.updated_at
        THEN EXCLUDED.last_file_id ELSE user_watch_progress.last_file_id
    END,
    last_resolution = CASE
        WHEN EXCLUDED.updated_at >= user_watch_progress.updated_at
        THEN EXCLUDED.last_resolution ELSE user_watch_progress.last_resolution
    END,
    last_hdr = CASE
        WHEN EXCLUDED.updated_at >= user_watch_progress.updated_at
        THEN EXCLUDED.last_hdr ELSE user_watch_progress.last_hdr
    END,
    last_codec_video = CASE
        WHEN EXCLUDED.updated_at >= user_watch_progress.updated_at
        THEN EXCLUDED.last_codec_video ELSE user_watch_progress.last_codec_video
    END,
    last_edition_key = CASE
        WHEN EXCLUDED.updated_at >= user_watch_progress.updated_at
        THEN EXCLUDED.last_edition_key ELSE user_watch_progress.last_edition_key
    END,
    hide_from_continue = user_watch_progress.hide_from_continue OR EXCLUDED.hide_from_continue,
    event_at = GREATEST(user_watch_progress.event_at, EXCLUDED.event_at),
    synced_seq = GREATEST(user_watch_progress.synced_seq, EXCLUDED.synced_seq);

DELETE FROM user_watch_progress p
USING episode_aliases ea
WHERE p.media_item_id = ea.alias_episode_id
  AND ea.alias_episode_id IS NOT NULL;

-- History rows have their own immutable row id, so retarget them in place.
UPDATE user_watch_history h
SET media_item_id = ea.canonical_episode_id
FROM episode_aliases ea
WHERE h.media_item_id = ea.alias_episode_id
  AND ea.alias_episode_id IS NOT NULL;

-- +goose Down
-- The repair is intentionally irreversible: without the alias map there is no
-- safe way to know which canonical row originally came from an alias, and
-- re-splitting merged progress would lose user state.
SELECT 1;
