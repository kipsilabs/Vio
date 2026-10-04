import { useEffect, useMemo } from "react";
import type { PlayerSubtitleInfo } from "../types";
import { fontBundleCacheKey, loadSubtitleFontBundle } from "../utils/subtitleFonts";
import { isASSCodec } from "../utils/subtitleCodecs";

/**
 * Prefetches ASS font bundles when the plan is adopted or refreshed. Selection
 * then hits the in-memory font cache instead of waiting on a cold server
 * extraction. Purely a warm-up: errors are swallowed and never affect playback.
 *
 * Only ASS/SSA sidecar tracks are warmed: JASSUB is the sole consumer of a font
 * bundle, and it renders only ASS/SSA. A font URL on another codec is a server
 * contract violation, and requesting it is what flooded the fonts route with
 * 400s/500s for combos the endpoint can never satisfy.
 *
 * Scoped to the effective release. A track names the media file whose inventory
 * assigned it (`media_file_id`); prefetching a track from another release spends
 * an extraction on a bundle that release's JASSUB instance can never render —
 * `useASSSubtitles` only ever fetches the active track's bundle from the
 * effective release's list. When the caller passes a list that spans releases,
 * only the effective file's tracks (and tracks with no file identity, which are
 * the effective release by construction) are warmed. Tracks are deduped on the
 * normalized font-bundle cache key so two URL spellings of one payload fetch
 * once.
 */
export function useSubtitleFontPrefetch(
  subtitleUrls: PlayerSubtitleInfo[],
  effectiveFileId?: number | null,
) {
  const fontUrls = useMemo(() => {
    // JASSUB renders ASS/SSA only; a font bundle on any other codec is
    // unreachable by the renderer.
    const candidates = subtitleUrls.filter(
      (track) => track.font_bundle_url && isASSCodec(track.codec),
    );
    // Scope to the effective release when the list spans releases. A track
    // carries the media file whose inventory assigned it; tracks with no file
    // identity belong to the effective release by construction. When the
    // caller's list already names the effective file, only its tracks (and the
    // unattributed ones) are warmed. When the effective file is unknown, or no
    // track's file id matches it — a virtual session collapses the requested
    // row's id, and older plans omit the file id entirely — the list is treated
    // as already scoped rather than dropping every track. Tracks are deduped on
    // the normalized font-bundle cache key so two URL spellings of one payload
    // fetch once.
    const identified = candidates.filter((track) => track.media_file_id != null);
    const effectiveKnown =
      effectiveFileId != null &&
      identified.length > 0 &&
      identified.some((track) => track.media_file_id === effectiveFileId);
    const byCacheKey = new Map<string, string>();
    for (const track of candidates) {
      const url = track.font_bundle_url!;
      if (
        effectiveKnown &&
        track.media_file_id != null &&
        track.media_file_id !== effectiveFileId
      ) {
        continue;
      }
      const cacheKey = fontBundleCacheKey(url);
      if (!byCacheKey.has(cacheKey)) byCacheKey.set(cacheKey, url);
    }
    return [...byCacheKey.values()];
  }, [subtitleUrls, effectiveFileId]);
  const key = fontUrls.join("|");
  useEffect(() => {
    if (!key) return;
    for (const url of fontUrls) {
      void loadSubtitleFontBundle(url).catch(() => {});
    }
  }, [key, fontUrls]);
}
