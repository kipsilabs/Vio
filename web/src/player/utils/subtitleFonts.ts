import notoSansArabicUrl from "@fontsource/noto-sans-arabic/files/noto-sans-arabic-arabic-400-normal.woff?url";
import notoSansArabicLatinExtUrl from "@fontsource/noto-sans-arabic/files/noto-sans-arabic-latin-ext-400-normal.woff?url";
import notoSansArabicLatinUrl from "@fontsource/noto-sans-arabic/files/noto-sans-arabic-latin-400-normal.woff?url";
import notoSansThaiLatinExtUrl from "@fontsource/noto-sans-thai/files/noto-sans-thai-latin-ext-400-normal.woff?url";
import notoSansThaiLatinUrl from "@fontsource/noto-sans-thai/files/noto-sans-thai-latin-400-normal.woff?url";
import notoSansThaiUrl from "@fontsource/noto-sans-thai/files/noto-sans-thai-thai-400-normal.woff?url";

/**
 * A fallback font for a writing system that JASSUB's built-in default
 * (Liberation Sans, Latin-only) cannot render.
 *
 * JASSUB/libass renders missing glyphs with its `defaultFont` — the font used
 * whenever a subtitle style's named font is absent OR lacks glyphs for the text
 * being drawn. This libass build does NOT search other loaded fonts for glyph
 * coverage, so merely adding a font (via `fonts`/`availableFonts`) does nothing
 * unless a style names it; the font has to be the `defaultFont`. A single font
 * can't cover every script, so we pick the default per track/script.
 */
export interface SubtitleFallbackFont {
  /** Font family key, lower-cased to match JASSUB's case-insensitive lookup. */
  family: string;
  /** Font file URLs preloaded into JASSUB for this family. */
  urls: string[];
}

const NOTO_SANS_ARABIC: SubtitleFallbackFont = {
  family: "noto sans arabic",
  urls: [notoSansArabicUrl, notoSansArabicLatinUrl, notoSansArabicLatinExtUrl],
};

const NOTO_SANS_THAI: SubtitleFallbackFont = {
  family: "noto sans thai",
  urls: [notoSansThaiUrl, notoSansThaiLatinUrl, notoSansThaiLatinExtUrl],
};

// Normalized ISO-639 language code (2- and 3-letter forms) -> fallback font
// whose script the Latin default cannot render. Prefer per-language gating like
// this over a single global default, especially for large CJK fonts.
const FALLBACK_BY_LANGUAGE: Record<string, SubtitleFallbackFont> = {
  ar: NOTO_SANS_ARABIC,
  ara: NOTO_SANS_ARABIC,
  th: NOTO_SANS_THAI,
  tha: NOTO_SANS_THAI,
};

const FALLBACK_BY_SCRIPT: Array<{ pattern: RegExp; font: SubtitleFallbackFont }> = [
  // Arabic, Arabic Supplement, Arabic Extended-A, Arabic Presentation Forms.
  {
    pattern: /[\u0600-\u06ff\u0750-\u077f\u08a0-\u08ff\ufb50-\ufdff\ufe70-\ufeff]/,
    font: NOTO_SANS_ARABIC,
  },
  { pattern: /[\u0e00-\u0e7f]/, font: NOTO_SANS_THAI },
];

/**
 * Returns the font to use as JASSUB's `defaultFont` for a subtitle track in the
 * given language, or null to keep JASSUB's built-in Latin default.
 */
export function fallbackFontForLanguage(language: string | undefined): SubtitleFallbackFont | null {
  if (!language) return null;
  return FALLBACK_BY_LANGUAGE[language.toLowerCase()] ?? null;
}

/**
 * Returns a fallback font by explicit language first, then by scanning subtitle
 * text for scripts that need a non-Latin default.
 */
export function fallbackFontForSubtitle(
  language: string | undefined,
  subtitleContent: string,
): SubtitleFallbackFont | null {
  const languageFont = fallbackFontForLanguage(language);
  if (languageFont) return languageFont;

  return FALLBACK_BY_SCRIPT.find(({ pattern }) => pattern.test(subtitleContent))?.font ?? null;
}

const fontDataCache = new Map<string, Promise<Uint8Array[]>>();
const MAX_FONT_BUNDLE_CACHE_ENTRIES = 4;
const MAX_FONT_BUNDLE_CACHE_BYTES = 64 * 1024 * 1024;

/**
 * The outcome of one font-bundle fetch. `pending` is true when the server
 * answered with an empty bundle while its extraction was still in flight (the
 * response carries the cache-busting pending marker); a definitive font-less
 * file answers empty without it. A pending result must never be retained as a
 * final "this track has no fonts" state — the client re-fetches it.
 */
export interface SubtitleFontBundleResult {
  fonts: Uint8Array[];
  pending: boolean;
}

interface FontBundleCacheEntry {
  promise: Promise<SubtitleFontBundleResult>;
  bytes: number;
}

const fontBundleCache = new Map<string, FontBundleCacheEntry>();

interface SubtitleFontBundleItem {
  name: string;
  data: string;
}

/**
 * Decodes a font-bundle payload. The v2 fonts endpoint answers with the shared
 * collection envelope (`{"items": [...]}`), while the bridge API's font route
 * serves the historical bare array; both shapes carry the same items.
 */
function decodeFontBundleItems(payload: unknown): SubtitleFontBundleItem[] {
  if (Array.isArray(payload)) return payload as SubtitleFontBundleItem[];
  if (
    typeof payload === "object" &&
    payload !== null &&
    Array.isArray((payload as { items?: unknown }).items)
  ) {
    return (payload as { items: SubtitleFontBundleItem[] }).items;
  }
  throw new TypeError("font bundle payload is not a recognized collection shape");
}

/**
 * The response header the server sets on an in-flight font bundle. A definitive
 * font-less file stays cacheable and does not carry it. Pending bundles are also
 * served with `Cache-Control: no-store`; either signal marks the response.
 */
export const FONT_BUNDLE_PENDING_HEADER = "X-Vio-Font-Bundle-Pending";

function isPendingFontBundleResponse(response: Response): boolean {
  const marker = response.headers?.get?.(FONT_BUNDLE_PENDING_HEADER);
  if (marker === "true" || marker === "1") return true;
  const cacheControl = response.headers?.get?.("Cache-Control") ?? "";
  return cacheControl.toLowerCase().includes("no-store");
}

/**
 * Normalizes a font bundle URL for cache keying. The N embedded ASS tracks of
 * one file share one font payload, but the server URLs differ only by
 * `embedded_stream_index`; without stripping that parameter each track would
 * get its own cache entry for identical bytes (and the prefetch of one track
 * would not warm the selection of another). The fetch URL itself is unchanged.
 */
export function fontBundleCacheKey(url: string): string {
  // Parse with a base so relative URLs (the common case for API calls)
  // work; fall back to the raw URL for truly unparseable inputs.
  let parsed: URL;
  try {
    parsed = new URL(url, "http://silo.local");
  } catch {
    return url;
  }
  parsed.searchParams.delete("embedded_stream_index");
  const qs = parsed.searchParams.toString();
  return qs ? `${parsed.pathname}?${qs}` : parsed.pathname;
}

export function loadSubtitleFallbackFontData(font: SubtitleFallbackFont): Promise<Uint8Array[]> {
  const cached = fontDataCache.get(font.family);
  if (cached) return cached;

  const promise = Promise.all(
    font.urls.map(async (url) => {
      const response = await fetch(url);
      if (!response.ok) {
        throw new Error(`HTTP ${response.status}`);
      }
      return new Uint8Array(await response.arrayBuffer());
    }),
  ).catch((err) => {
    fontDataCache.delete(font.family);
    throw err;
  });
  fontDataCache.set(font.family, promise);
  return promise;
}

/**
 * Fetches a font bundle and reports whether the server was still producing it.
 * A pending (in-flight) bundle is never cached: the next window, the prefetch,
 * or the bounded font refresh must be able to fetch the completed bytes. A
 * definitive bundle (fonts, or a cacheable empty one for a genuinely font-less
 * file) is cached as before.
 */
export function loadSubtitleFontBundleResult(
  url: string,
  signal?: AbortSignal,
  onSourceChanged?: () => void,
): Promise<SubtitleFontBundleResult> {
  const cacheKey = fontBundleCacheKey(url);
  const cached = fontBundleCache.get(cacheKey);
  if (cached) {
    fontBundleCache.delete(cacheKey);
    fontBundleCache.set(cacheKey, cached);
    return cached.promise;
  }

  const entry: FontBundleCacheEntry = {
    promise: Promise.resolve({ fonts: [], pending: false }),
    bytes: 0,
  };
  const promise = fetch(url, { signal })
    .then(async (response): Promise<SubtitleFontBundleResult> => {
      if (!response.ok) {
        if (response.status === 409) {
          // Virtual release rotation made the font URL stale; signal the
          // player to refresh the subtitle inventory. The text fetcher
          // usually fires first, but the font prefetch at plan adoption
          // can hit this before any text fetch. Mark it pending so the empty
          // result is never cached.
          onSourceChanged?.();
          return { fonts: [], pending: true };
        }
        // Any other non-OK status is definitive for this URL: re-requesting
        // the same bytes cannot succeed on its own, so a 5xx is the server's
        // answer, not a budget miss. Marking it pending drove a bounded but
        // repeated refresh per window on top of the prefetch, which flooded
        // the fonts endpoint with identical failing requests. Return a
        // cacheable empty bundle instead; a new plan or session mints new
        // URLs (the session id is in the path) and gets a fresh attempt.
        return { fonts: [], pending: false };
      }
      const pending = isPendingFontBundleResponse(response);
      const items = decodeFontBundleItems(await response.json());
      return { fonts: items.map((item) => base64ToBytes(item.data)), pending };
    })
    .then((result) => {
      if (result.pending) {
        fontBundleCache.delete(cacheKey);
      } else {
        entry.bytes = totalByteLength(result.fonts);
        if (entry.bytes > MAX_FONT_BUNDLE_CACHE_BYTES) {
          fontBundleCache.delete(cacheKey);
        } else {
          evictFontBundleCache();
        }
      }
      return result;
    });

  // Do not poison the cache with transient network errors or aborted requests.
  const cachedPromise = promise.catch((err) => {
    fontBundleCache.delete(cacheKey);
    throw err;
  });
  entry.promise = cachedPromise;
  fontBundleCache.set(cacheKey, entry);
  evictFontBundleCache();
  return cachedPromise;
}

export function loadSubtitleFontBundle(
  url: string,
  signal?: AbortSignal,
  onSourceChanged?: () => void,
): Promise<Uint8Array[]> {
  return loadSubtitleFontBundleResult(url, signal, onSourceChanged).then((result) => result.fonts);
}

function totalByteLength(chunks: Uint8Array[]): number {
  return chunks.reduce((total, chunk) => total + chunk.byteLength, 0);
}

function evictFontBundleCache(): void {
  while (fontBundleCache.size > MAX_FONT_BUNDLE_CACHE_ENTRIES) {
    const oldest = fontBundleCache.keys().next().value;
    if (!oldest) return;
    fontBundleCache.delete(oldest);
  }

  let total = 0;
  for (const entry of fontBundleCache.values()) {
    total += entry.bytes;
  }

  while (total > MAX_FONT_BUNDLE_CACHE_BYTES) {
    const oldest = fontBundleCache.entries().next().value;
    if (!oldest) return;
    const [url, entry] = oldest;
    fontBundleCache.delete(url);
    total -= entry.bytes;
  }
}

function base64ToBytes(value: string): Uint8Array {
  const binary = atob(value);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i += 1) {
    bytes[i] = binary.charCodeAt(i);
  }
  return bytes;
}

/**
 * Forces ASS style font names and inline \fn overrides to the selected fallback
 * family. Without this, libass repeatedly asks for unavailable style fonts such
 * as Trebuchet MS before falling back, which both logs noisily and can leave
 * non-Latin glyphs boxed if fallback fonts are not loaded yet.
 */
export function forceASSFontFamily(content: string, family: string): string {
  const normalized = content.replace(/\r\n/g, "\n").replace(/\r/g, "\n");
  let inStyleSection = false;
  let fontNameIndex = -1;

  return normalized
    .split("\n")
    .map((line) => {
      const section = line.match(/^\s*\[([^\]]+)]\s*$/);
      if (section) {
        const sectionName = section[1]!.trim().toLowerCase();
        inStyleSection = sectionName === "v4+ styles" || sectionName === "v4 styles";
        fontNameIndex = -1;
        return line;
      }

      if (inStyleSection) {
        const format = line.match(/^(\s*Format\s*:\s*)(.*)$/i);
        if (format) {
          const fields = format[2]!.split(",").map((field) => field.trim().toLowerCase());
          fontNameIndex = fields.indexOf("fontname");
          return line;
        }

        const style = line.match(/^(\s*Style\s*:\s*)(.*)$/i);
        if (style && fontNameIndex >= 0) {
          const fields = style[2]!.split(",");
          if (fontNameIndex < fields.length) {
            fields[fontNameIndex] = family;
          }
          return `${style[1]}${fields.join(",")}`;
        }
      }

      return line.replace(/\\fn[^\\}]*/g, `\\fn${family}`);
    })
    .join("\n");
}
