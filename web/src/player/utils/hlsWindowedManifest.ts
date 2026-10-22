/**
 * Client half of the windowed synthetic transcode manifest.
 *
 * A long encoded transcode is served from a bounded VOD playlist that lists only
 * the segments around the playhead (the server opts in when the plan URL carries
 * {@link WINDOWED_MANIFEST_QUERY_PARAM}). The playlist keeps the absolute media
 * timeline by carrying the omitted prefix as `#EXT-X-GAP` entries, so
 * `video.currentTime` still reads true media seconds, but two things no longer
 * hold that the whole-title manifest guaranteed:
 *
 *  - The playlist ends (with `#EXT-X-ENDLIST`) well before the media does, so
 *    hls.js will stop when the playhead reaches the window edge. The player has
 *    to fetch the manifest again as the playhead approaches that edge; the
 *    server re-windows around where the client has buffered to.
 *  - A target in the omitted prefix is inside `video.seekable` (the gap entries
 *    contribute duration) but has no real segment to load: hls.js would silently
 *    skip forward to the first listed segment. Such a target must go through the
 *    seek-reanchor path instead of a local seek.
 */

/** Opt-in query parameter that asks the server for the bounded manifest. */
export const WINDOWED_MANIFEST_QUERY_PARAM = "hls_window";

/**
 * Fetch the next window once the playhead is this close to the window edge.
 *
 * The refetch is a full hls.js MediaSource reset, so this margin does not buy
 * re-download time: the buffer is dropped the moment the refetch fires and the
 * gap the viewer sees is the first refetched segment's fetch latency, not this
 * number. What the margin must guarantee is only that the reset starts before
 * the element reaches `#EXT-X-ENDLIST` and reports a (premature) end. Twenty
 * seconds is ten default segments — far more than the one segment the resume
 * needs even on a slow link — while staying small enough that the window is not
 * refetched more often than necessary, since every refetch resets the buffer.
 */
export const WINDOWED_MANIFEST_RELOAD_AHEAD_SECONDS = 20;

/** Minimum gap between window refetches, so a stalled refill cannot spin. */
export const WINDOWED_MANIFEST_RELOAD_MIN_INTERVAL_MS = 10_000;

/**
 * Slack, in seconds, on the served window's bounds when deciding whether a seek
 * target is locally serveable. It absorbs the float arithmetic in EXTINF sums
 * without admitting a genuinely out-of-window target.
 */
export const SERVED_WINDOW_BOUNDS_SLACK_SECONDS = 0.5;

/**
 * Adds the windowed-manifest opt-in to an HLS stream URL. The parameter is
 * additive and idempotent; an empty URL is returned untouched.
 *
 * Idempotence is decided on the parsed query, not on a substring of the whole
 * URL: a URL whose *value* merely contains the parameter's text (or whose
 * query uses the key with a blank value, which the server also reads as the
 * opt-in) must not be mistaken for one already carrying it.
 */
export function withWindowedHlsManifest(url: string, enabled: boolean): string {
  if (!enabled || url === "") return url;
  const queryStart = url.indexOf("?");
  if (queryStart >= 0) {
    for (const part of url.slice(queryStart + 1).split("&")) {
      const key = part.split("=", 1)[0];
      if (key === WINDOWED_MANIFEST_QUERY_PARAM) return url;
    }
  }
  return `${url}${queryStart >= 0 ? "&" : "?"}${WINDOWED_MANIFEST_QUERY_PARAM}=1`;
}

/** The slice of hls.js's level details this module reads. */
export interface HlsLevelDetailsView {
  live?: boolean;
  edge: number;
  fragments: ReadonlyArray<{ gap?: boolean; start: number }>;
}

/** The window of media the currently loaded playlist can actually serve. */
export interface ServedHlsWindow {
  /** Absolute media time of the first segment the playlist lists. */
  start: number;
  /** Absolute media time at the end of the listed window. */
  end: number;
  /** Whether hls.js still treats the playlist as live (it reloads itself). */
  live: boolean;
}

/**
 * Reads the served window from hls.js level details. The start is the first
 * non-gap fragment's time: the leading `#EXT-X-GAP` prefix contributes duration
 * (so later fragments keep absolute times) but is not fetchable. Returns null
 * when the details are not readable yet.
 */
export function servedHlsWindow(
  details: HlsLevelDetailsView | null | undefined,
): ServedHlsWindow | null {
  if (!details || !Array.isArray(details.fragments) || details.fragments.length === 0) {
    return null;
  }
  const firstReal = details.fragments.find((fragment) => !fragment.gap)?.start;
  if (typeof firstReal !== "number" || !Number.isFinite(firstReal)) return null;
  if (!Number.isFinite(details.edge)) return null;
  return { start: firstReal, end: details.edge, live: details.live === true };
}

/**
 * Whether the playlist has run out ahead of the playhead and needs to be
 * refetched. A live playlist is reloaded by hls.js itself, and a window that
 * reaches the media end is the normal end of playback.
 *
 * `mediaDurationSeconds` is the plan's `source.duration_seconds`, the same
 * `file.Duration` the server uses for the transcode session's total runtime. So
 * the final window's edge reaches it and the `+ 1` slack only absorbs the float
 * sums of the EXTINF entries; the two values are not independent estimates.
 */
export function shouldAdvanceHlsWindow(
  window: ServedHlsWindow | null,
  mediaDurationSeconds: number,
  currentTimeSeconds: number,
  reloadAheadSeconds: number = WINDOWED_MANIFEST_RELOAD_AHEAD_SECONDS,
): boolean {
  if (!window || window.live) return false;
  if (!(mediaDurationSeconds > window.end + 1)) return false;
  return currentTimeSeconds >= window.end - reloadAheadSeconds;
}

/**
 * Whether a seek target can be served locally. A target below the served
 * window's start is inside the playlist's seekable range only because of the
 * gap prefix and cannot be loaded; a target past the window's end is not listed
 * at all. Both must reanchor, even though the plan may report
 * `can_seek_anywhere` for its whole-title manifest.
 *
 * An unknown window (null, before the first `LEVEL_LOADED` publishes one) is
 * treated as *not* locally serveable: the player cannot yet tell whether the
 * target lies in the omitted gap prefix, and a native seek there would be
 * silently skipped forward by hls.js rather than reporting a miss. The
 * conservative result routes the seek through the reanchor path, which is
 * correct for a windowed manifest and merely costs a replan for the brief
 * startup interval on any other HLS route. A genuinely buffered target never
 * reaches this function — the caller checks the element's buffer first.
 */
export function hlsTargetWithinServedWindow(
  window: ServedHlsWindow | null,
  targetSeconds: number,
): boolean {
  if (!window) return false;
  return (
    targetSeconds >= window.start - SERVED_WINDOW_BOUNDS_SLACK_SECONDS &&
    targetSeconds <= window.end + SERVED_WINDOW_BOUNDS_SLACK_SECONDS
  );
}
