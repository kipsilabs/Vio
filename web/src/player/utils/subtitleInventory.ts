import type { PlayerSubtitleInfo } from "../types";
import { canonicalLanguageTag } from "./languageNames";

/**
 * One subtitle track as a file version publishes it on the wire
 * (`VersionSubtitleTrack` / `PlayerVersionSubtitleTrack`). Structural, so both
 * shapes satisfy it without the player module importing app API types.
 */
export interface SubtitleInventoryTrackInput {
  index?: number;
  language?: string;
  codec?: string;
  title?: string;
  embedded_title?: string;
  file_name?: string;
  /** Opaque stable hash of an external sidecar's full path (newer servers). */
  path_key?: string;
  forced?: boolean;
  hearing_impaired?: boolean;
  external?: boolean;
  /** The file's default subtitle stream, per the probed inventory. */
  default?: boolean;
}

/**
 * A downloaded/AI subtitle as the downloaded-subtitles endpoint publishes it.
 * The stable database row id is the per-track identity the server keys its
 * de-duplication on.
 */
export interface DownloadedSubtitleInput {
  id: number;
  language?: string;
  format?: string;
  forced?: boolean;
  hearing_impaired?: boolean;
}

/**
 * A published subtitle track at its dense combined ordinal, in the player's
 * render shape. `index` is the server's published combined ordinal that a
 * track change echoes back — never a container stream index or an array
 * position.
 */
export type PublishedSubtitleTrack = PlayerSubtitleInfo & {
  source: "external" | "embedded" | "downloaded";
  /**
   * The source row this published track came from (container stream index for
   * an embedded track, sidecar position for an external one). Exposed so
   * callers can map a catalog row to the ordinal this single derivation
   * assigned it without re-implementing the rules.
   */
  containerIndex?: number;
  /** Downloaded subtitle row id, when the source is a downloaded/generated row. */
  downloadedSubtitleId?: number;
};

// ---------------------------------------------------------------------------
// Language base — ported from internal/virtuallibrary/stream/stream.go
// (canonicalAudioLanguage + CanonicalLanguageBase), the exact key playback's
// subtitleInventoryDuplicateV3 folds language aliases with.
// ---------------------------------------------------------------------------

/**
 * The server's base-language table. It maps the server's intermediate
 * canonical token (e.g. "ENG", "EN-US") to a base. Only tokens the server
 * recognizes reach it; an unknown bare code yields "" and never alias-collapses.
 * Mirrored verbatim so a client-computed ordinal equals the published one.
 */
const CANONICAL_LANGUAGE_BASES: Record<string, string> = {
  ENG: "en",
  FRE: "fr",
  DEU: "de",
  ITA: "it",
  SPA: "es",
  JPN: "ja",
  KOR: "ko",
  RUS: "ru",
  ZHO: "zh",
  POR: "pt",
  ARA: "ar",
  HIN: "hi",
  NLD: "nl",
  POL: "pl",
  SWE: "sv",
  NOR: "no",
  DAN: "da",
  FIN: "fi",
  TUR: "tr",
  UKR: "uk",
};

/**
 * The server's ISO-alias/display-name switch (stream.go canonicalAudioLanguage).
 * Keys are lowercased, values are the server's intermediate canonical token.
 */
const AUDIO_LANGUAGE_ALIASES: Record<string, string> = {
  eng: "ENG",
  en: "ENG",
  english: "ENG",
  fre: "FRE",
  fra: "FRE",
  fr: "FRE",
  french: "FRE",
  ger: "DEU",
  deu: "DEU",
  de: "DEU",
  german: "DEU",
  deutsch: "DEU",
  ita: "ITA",
  it: "ITA",
  italian: "ITA",
  spa: "SPA",
  es: "SPA",
  spanish: "SPA",
  jpn: "JPN",
  ja: "JPN",
  japanese: "JPN",
  kor: "KOR",
  ko: "KOR",
  korean: "KOR",
  rus: "RUS",
  ru: "RUS",
  russian: "RUS",
  zho: "ZHO",
  chi: "ZHO",
  zh: "ZHO",
  chinese: "ZHO",
  por: "POR",
  pt: "POR",
  portuguese: "POR",
  "pt-br": "PT-BR",
  "brazilian portuguese": "PT-BR",
  "pt-pt": "PT-PT",
  "european portuguese": "PT-PT",
  "es-419": "ES-419",
  "es-es": "ES-ES",
  castilian: "ES-ES",
  "zh-hans": "ZH-HANS",
  "simplified chinese": "ZH-HANS",
  "zh-hant": "ZH-HANT",
  "zh-tw": "ZH-HANT",
  "traditional chinese": "ZH-HANT",
  "en-us": "EN-US",
  "en-gb": "EN-GB",
  "fr-ca": "FR-CA",
  ara: "ARA",
  ar: "ARA",
  arabic: "ARA",
  hin: "HIN",
  hi: "HIN",
  hindi: "HIN",
  nld: "NLD",
  dut: "NLD",
  nl: "NLD",
  dutch: "NLD",
  pol: "POL",
  pl: "POL",
  polish: "POL",
  swe: "SWE",
  sv: "SWE",
  swedish: "SWE",
  nor: "NOR",
  no: "NOR",
  norwegian: "NOR",
  dan: "DAN",
  da: "DAN",
  danish: "DAN",
  fin: "FIN",
  fi: "FIN",
  finnish: "FIN",
  tur: "TUR",
  tr: "TUR",
  turkish: "TUR",
  ukr: "UKR",
  uk: "UKR",
  ukrainian: "UKR",
};

/** Mirrors lang.PrimaryLanguage: canonicalize, drop everything after the base, reject undefined. */
function primaryLanguage(value: string): string {
  const canonical = canonicalLanguageTag(value) ?? value.trim().toLowerCase();
  const base = canonical.split("-")[0] ?? "";
  if (base === "und" || base === "x") return "";
  return base;
}

/**
 * Mirrors stream.CanonicalLanguageBase: fold ISO 639-1/2 tags, their
 * bibliographic variants, English display names and regional/script forms onto
 * one base-language key — "EN-US", "en-GB", "ENG", "en" and "English" all yield
 * "en"; "FR-CA", "FRE" and "fr" yield "fr". Unidentifiable input yields "".
 *
 * This is the de-duplication key shared with the server's subtitle inventory;
 * it must stay in lockstep with the Go implementation or a client-computed
 * ordinal diverges from the published one. Notably the server only recognizes
 * a fixed set of bare codes (English, French, German, …): other bare tags such
 * as "sr" or "nb" yield "" and are never alias-collapsed.
 */
export function subtitleLanguageBase(value: string | undefined | null): string {
  const lower = (value ?? "").trim().toLowerCase();
  if (lower === "") return "";
  let code = AUDIO_LANGUAGE_ALIASES[lower];
  if (code === undefined) {
    // canonicalAudioLanguage: any 3-letter token except und/mul is accepted
    // uppercase; a bare 2-letter code outside the switch yields "".
    if (lower.length === 3 && lower !== "und" && lower !== "mul") {
      code = lower.toUpperCase();
    } else {
      return "";
    }
  }
  const mapped = CANONICAL_LANGUAGE_BASES[code];
  if (mapped !== undefined) return mapped;
  const dash = code.indexOf("-");
  if (dash > 0) {
    const base = code.slice(0, dash).toLowerCase();
    if (base !== "" && base !== "und" && base !== "mul") return base;
  }
  const primary = primaryLanguage(code);
  if (primary !== "" && primary !== "und" && primary !== "mul") return primary;
  const lowerCode = code.toLowerCase();
  const fallbackDash = lowerCode.indexOf("-");
  return fallbackDash > 0 ? lowerCode.slice(0, fallbackDash) : lowerCode;
}

// ---------------------------------------------------------------------------
// Inventory de-duplication and dense ordinal assignment — ported from
// internal/playback/subtitle_inventory_v3.go (BuildSubtitleInventoryV3 and
// subtitleInventoryDuplicateV3).
// ---------------------------------------------------------------------------

/** Mirrors the subtitle-relevant subset of playback.normalizeCodecV3. */
function normalizeDedupCodec(codec: string | undefined): string {
  const value = (codec ?? "").trim().toLowerCase();
  switch (value) {
    case "pgssub":
      return "hdmv_pgs_subtitle";
    case "dvdsub":
    case "vobsub":
      return "dvd_subtitle";
    case "dvbsub":
      return "dvb_subtitle";
    default:
      return value;
  }
}

/**
 * Mirrors playback.subtitleInventoryDuplicateV3. A bare alias (no per-track
 * discriminator) collapses only against an earlier bare alias with the same
 * source, codec, base language and flags — the EN-US-beside-ENG pair for one
 * track. An entry with a discriminator collapses only against the exact same
 * discriminator (the same track described twice). An entry with neither a
 * language base nor a discriminator is never suppressed. The seen-set spans all
 * three ranges, so a duplicate later in the combined list de-duplicates against
 * an earlier one.
 */
function isInventoryDuplicate(
  seen: Set<string>,
  source: string,
  identity: string,
  codec: string | undefined,
  language: string | undefined,
  forced: boolean | undefined,
  hearingImpaired: boolean | undefined,
): boolean {
  const base = subtitleLanguageBase(language);
  if (base === "" && identity === "") return false;
  const aliasKey = [
    "alias",
    source,
    normalizeDedupCodec(codec),
    base,
    String(Boolean(forced)),
    String(Boolean(hearingImpaired)),
  ].join("\u0000");
  if (identity === "") {
    if (seen.has(aliasKey)) return true;
    seen.add(aliasKey);
    return false;
  }
  const trackKey = ["track", identity, aliasKey].join("\u0000");
  if (seen.has(trackKey)) return true;
  seen.add(trackKey);
  return false;
}

/**
 * Mirrors playback.externalSubtitleIdentityV3. The server keys a sidecar on its
 * full path; the wire carries an opaque `path_key` hash of that path when the
 * server computes one, which keeps two same-basename sidecars in different
 * directories distinct. Older servers publish only the basename
 * (`file_name`), used as the closest faithful fallback; two same-basename
 * sidecars then collapse, a gap only a newer server can close.
 */
function externalIdentity(track: SubtitleInventoryTrackInput): string {
  const key = (track.path_key ?? "").trim();
  if (key) return `path:${key}`;
  const name = (track.file_name ?? "").trim();
  return name ? `path:${name}` : "";
}

/**
 * Mirrors playback.embeddedSubtitleIdentityV3. The container stream index plus
 * the authored title (raw embedded title, else stored title) is the positive
 * discriminator: two streams at different indexes are different physical tracks
 * even when they share a title, codec, language and flags, while the same
 * stream described twice collapses.
 */
function embeddedIdentity(track: SubtitleInventoryTrackInput): string {
  const index = track.index ?? 0;
  const authored = track.embedded_title?.trim() ? track.embedded_title : track.title;
  const title = authored?.trim() ?? "";
  return title ? `stream:${index}\u0000title:${title}` : `stream:${index}`;
}

function normalizedLanguage(language: string | undefined): string {
  return language?.trim() || "unknown";
}

function normalizedLabel(track: SubtitleInventoryTrackInput, ordinal: number): string {
  return (
    track.title?.trim() ||
    track.embedded_title?.trim() ||
    track.file_name?.trim() ||
    track.language?.trim() ||
    `Subtitle ${ordinal + 1}`
  );
}

/**
 * The single client-side implementation of the server's combined-ordinal rule.
 *
 * Ordinals are assigned across three consecutive ranges in the server's order —
 * external sidecars, embedded container tracks, downloaded/generated rows — and
 * are always dense and gap-free: each surviving track takes its position in the
 * published list, and a duplicate the de-duplication suppresses consumes no
 * ordinal, shifting every later survivor down.
 *
 * The returned track's `index` is therefore the exact ordinal the server
 * publishes for the same inventory, so an index-derived request
 * (`subtitle_track_index`) resolves to the track the viewer picked instead of
 * silently degrading to subtitles-off. Mirrors
 * playback.BuildSubtitleInventoryV3 (internal/playback/subtitle_inventory_v3.go),
 * whose rules the previous client reconstructions each approximated with a
 * position-in-array index.
 */
export function buildPublishedSubtitleTracks(
  tracks: SubtitleInventoryTrackInput[] | undefined,
  downloaded: DownloadedSubtitleInput[] = [],
): PublishedSubtitleTrack[] {
  const list = tracks ?? [];
  const seen = new Set<string>();
  const published: PublishedSubtitleTrack[] = [];

  // The wire's `subtitle_tracks` is embedded-first (the catalog appends external
  // sidecars after embedded ones); the server's ordinal space is external-first.
  const externalTracks = list.filter((track) => track.external);
  const embeddedTracks = list.filter((track) => !track.external);

  for (const track of externalTracks) {
    if (
      isInventoryDuplicate(
        seen,
        "external",
        externalIdentity(track),
        track.codec,
        track.language,
        track.forced,
        track.hearing_impaired,
      )
    ) {
      continue;
    }
    published.push({
      index: published.length,
      language: normalizedLanguage(track.language),
      codec: track.codec,
      label: normalizedLabel(track, published.length),
      source: "external",
      forced: track.forced,
      hearing_impaired: track.hearing_impaired,
      default: track.default,
      url: "",
      containerIndex: track.index ?? list.indexOf(track),
    });
  }

  for (const track of embeddedTracks) {
    if (
      isInventoryDuplicate(
        seen,
        "embedded",
        embeddedIdentity(track),
        track.codec,
        track.language,
        track.forced,
        track.hearing_impaired,
      )
    ) {
      continue;
    }
    published.push({
      index: published.length,
      language: normalizedLanguage(track.language),
      codec: track.codec,
      label: normalizedLabel(track, published.length),
      source: "embedded",
      forced: track.forced,
      hearing_impaired: track.hearing_impaired,
      default: track.default,
      url: "",
      containerIndex: track.index ?? list.indexOf(track),
    });
  }

  for (const subtitle of downloaded) {
    // A downloaded row keys on its stable row id, so two distinct downloads in
    // one language are both kept; the source range differing from the file's
    // own tracks also keeps them from collapsing against an embedded track.
    const identity = subtitle.id > 0 ? String(subtitle.id) : "";
    if (
      isInventoryDuplicate(
        seen,
        "downloaded",
        identity,
        subtitle.format,
        subtitle.language,
        subtitle.forced,
        subtitle.hearing_impaired,
      )
    ) {
      continue;
    }
    published.push({
      index: published.length,
      language: normalizedLanguage(subtitle.language),
      codec: subtitle.format,
      label: subtitle.language?.trim() || "Downloaded",
      source: "downloaded",
      forced: subtitle.forced,
      hearing_impaired: subtitle.hearing_impaired,
      url: "",
      downloadedSubtitleId: subtitle.id > 0 ? subtitle.id : undefined,
    });
  }

  return published;
}
