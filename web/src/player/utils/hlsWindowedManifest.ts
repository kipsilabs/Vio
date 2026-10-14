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

/** Fetch the next window once the playhead is this close to the window edge. */
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
 */
export function withWindowedHlsManifest(url: string, enabled: boolean): string {
  if (!enabled || url === "") return url;
  if (url.includes(`${WINDOWED_MANIFEST_QUERY_PARAM}=1`)) return url;
  return `${url}${url.includes("?") ? "&" : "?"}${WINDOWED_MANIFEST_QUERY_PARAM}=1`;
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
 */
export function hlsTargetWithinServedWindow(
  window: ServedHlsWindow | null,
  targetSeconds: number,
): boolean {
  if (!window) return true;
  return (
    targetSeconds >= window.start - SERVED_WINDOW_BOUNDS_SLACK_SECONDS &&
    targetSeconds <= window.end + SERVED_WINDOW_BOUNDS_SLACK_SECONDS
  );
}
