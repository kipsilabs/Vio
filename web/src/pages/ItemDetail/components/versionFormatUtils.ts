import type { VersionAudioTrack, VersionSubtitleTrack, VersionVideoTrack } from "@/api/types";
import { normalizeSortCriteria } from "@/components/streaming/scoringPresets";
import { englishLanguageName, getLanguageName } from "@/lib/languageNames";
import type { ServerVersionRanking } from "@/lib/qualityRanking";
import { videoRangeLabel, type VideoRangeTrack } from "@/lib/videoRange";
import {
  formatBitrate,
  formatChannels,
  formatFileSize,
  formatSampleRate,
  mapAudioLabel,
  stripReleaseSizeToken,
} from "@/lib/mediaFormat";

/** Languages that carry no real identity and are skipped in summaries. */
const LANGUAGE_PLACEHOLDERS = new Set(["und", "unknown", "unk", ""]);

/**
 * Collects deduplicated, human-readable language labels from a version's
 * tracks. Release markers like MULTI/DUAL and ISO/BCP 47 codes both resolve
 * through formatLanguageName, so a virtual candidate advertising
 * ["MULTI", "FR", "eng"] summarizes to "Multi/French/English".
 */
export function collectLanguageLabels(
  languages: ReadonlyArray<string | undefined | null>,
): string[] {
  const seen = new Set<string>();
  const labels: string[] = [];
  for (const value of languages ?? []) {
    const language = value?.trim();
    if (!language || LANGUAGE_PLACEHOLDERS.has(language.toLowerCase())) continue;
    const label = formatLanguageName(language);
    // Deduplicate on the resolved display label: "eng", "en", and "English"
    // all render as "English", while distinct regions keep their qualifiers.
    const identity = label.toLowerCase();
    if (seen.has(identity)) continue;
    seen.add(identity);
    labels.push(label);
  }
  return labels;
}

/** The track fields both the catalog and the player version shapes expose. */
interface LanguageTrack {
  language?: string;
  languages?: string[];
}

/**
 * Audio language badges for a version row. A track advertising a `languages`
 * list (a MULTI track) contributes the whole list; otherwise its single
 * `language`. This is the one derivation both the item-page picker and the
 * in-player menu render, so the two lists agree badge for badge.
 */
export function audioLanguageLabels(tracks: ReadonlyArray<LanguageTrack> | undefined): string[] {
  return collectLanguageLabels(
    (tracks ?? []).flatMap((track) => {
      const languages = track.languages?.filter((language) => language?.trim());
      return languages && languages.length > 0 ? languages : [track.language?.trim()];
    }),
  );
}

/** Subtitle language badges for a version row, from the same helper as audio. */
export function subtitleLanguageLabels(
  tracks: ReadonlyArray<{ language?: string }> | undefined,
): string[] {
  return collectLanguageLabels((tracks ?? []).map((track) => track.language ?? ""));
}

/** The version fields a quality summary reads. Both version shapes satisfy it. */
export interface QualitySummarySource {
  file_path?: string;
  resolution?: string;
  codec_video?: string;
  codec_audio?: string;
  container?: string;
  file_name?: string;
  edition_raw?: string;
  hdr?: boolean;
  video_tracks?: VideoRangeTrack[];
}

/**
 * The one-line quality summary a version row leads with, shared by every
 * version list. It joins resolution, a source hint, the video codec, the
 * dynamic range, and the audio codec, falling back to the container for
 * ebook-style files, and labels the just-in-time "More results…" action.
 */
export function buildQualitySummary(version: QualitySummarySource): string {
  if (version.file_path) {
    try {
      const parsed = new URL(version.file_path, "http://silo.local");
      if (parsed.searchParams.get("results") === "all" && !parsed.searchParams.has("result")) {
        return "More results…";
      }
    } catch {
      // Fall through to the regular media quality summary for malformed paths.
    }
  }
  const parts: string[] = [];

  if (version.resolution) parts.push(version.resolution);

  const textToScan = [version.file_name, version.edition_raw].filter(Boolean).join(" ");
  const sourceHint = textToScan ? extractSourceHint(textToScan) : null;
  if (sourceHint) parts.push(sourceHint);

  if (version.codec_video) parts.push(version.codec_video.toUpperCase());
  const rangeLabel = videoRangeLabel(version);
  if (rangeLabel) parts.push(rangeLabel);
  if (version.codec_audio) parts.push(mapAudioLabel(version.codec_audio));
  // Audio languages are rendered as badges in the UI.
  // "virtual" is internal plumbing, not a user-facing codec; leaving it out
  // lets the row fall back to its release identity instead of a bare "VIRTUAL".
  if (parts.length === 0 && version.container && version.container.toLowerCase() !== "virtual") {
    parts.push(version.container.toUpperCase());
  }

  return parts.join(" · ");
}

/** The version fields a detail line reads. Both version shapes satisfy it. */
export interface VersionDetailSource {
  release_name?: string;
  edition_raw?: string;
  file_size?: number;
  file_name?: string;
}

/**
 * The release + structured size + source hint line a version row shows below
 * the summary. The release name leads; `edition_raw` is the fallback for rows
 * scanned before `release_name` existed. Shared so the in-player menu and the
 * item-page picker read identically, including their empty-detail fallback.
 */
export function buildVersionDetailLine(version: VersionDetailSource): string {
  return formatVersionDetail({
    label: sanitizeVersionLabel(version.release_name || version.edition_raw),
    fileSize: version.file_size,
    scanText: [version.file_name, version.edition_raw, version.release_name]
      .filter(Boolean)
      .join(" "),
  });
}

/** True when a version's custom-format score should render a badge. */
export function hasFormatScore(score?: number): score is number {
  return typeof score === "number" && score !== 0;
}

/** The ★ badge text for a custom-format score. */
export function formatScoreBadgeLabel(score: number): string {
  return `★ ${score}`;
}

/** The tooltip a format-score badge shows, naming the profile when ranked. */
export function formatScoreTitle(score: number, profileLabel?: string | null): string {
  return `Format score ${score}${profileLabel ? ` · ${profileLabel}` : ""}`;
}

/** Compact, deduplicated audio language list ("Multi/French") or null. */
export function audioLanguageSummary(tracks: VersionAudioTrack[] | undefined): string | null {
  const languages = tracks?.flatMap((track) =>
    track.languages?.length ? track.languages : [track.language],
  );
  const labels = collectLanguageLabels(languages ?? []);
  return labels.length > 0 ? labels.join("/") : null;
}

/** Compact, deduplicated subtitle language list ("English/French") or null. */
export function subtitleLanguageSummary(tracks: VersionSubtitleTrack[] | undefined): string | null {
  const labels = collectLanguageLabels(tracks?.map((track) => track.language) ?? []);
  return labels.length > 0 ? labels.join("/") : null;
}

/**
 * The quality-profile label a virtual candidate was ranked under, taken from
 * its `?profile=` selector. Null for local files and any URI without one (the
 * server then ranks under its default order).
 */
export function profileLabelFromFilePath(filePath?: string): string | null {
  if (!filePath) return null;
  try {
    const parsed = new URL(filePath, "http://silo.local");
    const label = parsed.searchParams.get("profile")?.trim();
    return label ? label : null;
  } catch {
    return null;
  }
}

interface VirtualRankingPayload {
  profile_label?: unknown;
  source?: unknown;
  criteria?: unknown;
}

/**
 * Resolves the ranking in effect for a version list. Prefers the server's
 * `virtual_ranking` payload (`{profile_label, source, criteria}`) when it is
 * present; until that field lands, falls back to the profile label carried by
 * the candidates' `?profile=` selector with the server's default order.
 */
export function serverRankingFromVersions(
  versions: readonly { file_path?: string; virtual_ranking?: unknown }[],
): ServerVersionRanking {
  for (const version of versions) {
    const payload = version.virtual_ranking as VirtualRankingPayload | undefined;
    if (!payload || typeof payload !== "object") continue;
    return {
      profileLabel:
        typeof payload.profile_label === "string" && payload.profile_label.trim()
          ? payload.profile_label.trim()
          : null,
      criteria: normalizeSortCriteria(payload.criteria),
      source: typeof payload.source === "string" ? payload.source : null,
      fromPayload: true,
    };
  }
  const profileLabel =
    versions
      .map((version) => profileLabelFromFilePath(version.file_path))
      .find((label): label is string => Boolean(label)) ?? null;
  return { profileLabel, criteria: [], source: null, fromPayload: false };
}

/** True for zero-storage catalog entries backed by a virtual:// provider URI. */
export function isVirtualFileVersion(version: { container?: string; file_path?: string }): boolean {
  return (
    version.container === "virtual" ||
    Boolean(version.file_path?.toLowerCase().startsWith("virtual://"))
  );
}

/** Turns a release-style name ("Movie.2023.2160p.Remux-GRP") into a readable
 *  line ("Movie 2023 2160p Remux GRP"). */
export function prettifyReleaseName(releaseName?: string): string {
  if (!releaseName) return "";
  return releaseName.replace(/[._]+/g, " ").replace(/\s+/g, " ").trim();
}

/**
 * Removes catalog/provider plumbing that must never be a user-visible label: a
 * `virtual://…` URI, a `?result=`/`?results=` provider picker token, and a bare
 * `tt######` external id. A version row's identity is its release name and
 * quality summary, not the internal handle the store keys it by.
 */
export function sanitizeVersionLabel(text?: string): string {
  if (!text) return "";
  return text
    .replace(/\bvirtual:\/\/\S+/gi, " ")
    .replace(/\bresults?=[^\s&·|]+/gi, " ")
    .replace(/\btt\d{5,}\b/gi, " ")
    .replace(/[?&]+/g, " ")
    .replace(/\s+/g, " ")
    .trim();
}

/**
 * The release/size line a version row shows, shared by the item-page picker and
 * the in-player version menu so both carry the same information.
 *
 * `label` is the row's chosen release/file label; its embedded size is dropped
 * when the structured `fileSize` is known (the canonical value), and kept when
 * it is the only size evidence. `scanText` is scanned for a source hint
 * (Remux/WEB-DL/...). Bitrate and other attributes are untouched.
 */
export function formatVersionDetail({
  label,
  fileSize,
  scanText,
}: {
  label?: string;
  fileSize?: number;
  scanText?: string;
}): string {
  const parts: string[] = [];
  const size = formatFileSize(fileSize);
  const releaseName = prettifyReleaseName(
    size ? stripReleaseSizeToken(label ?? "") : (label ?? ""),
  );
  if (releaseName) parts.push(releaseName);
  if (size) parts.push(size);
  const hint = scanText ? extractSourceHint(scanText) : null;
  if (hint) parts.push(hint);
  return parts.join(" · ");
}

// Four-digit years, excluding a "1920x1080"-style resolution token. Used only
// to detect a year that would otherwise repeat in the same row.
const YEAR_TOKEN = /\b((?:19|20)\d{2})\b(?!x)/g;

function releaseYears(text?: string): string[] {
  if (!text) return [];
  return [...new Set(text.match(YEAR_TOKEN) ?? [])];
}

function stripYears(text: string, years: readonly string[]): string {
  if (!text || years.length === 0) return text;
  let out = text;
  for (const year of years) {
    out = out.replace(new RegExp(`[._\\- ]*\\b${year}\\b[._\\- ]*`, "g"), " ");
  }
  return out.replace(/\s{2,}/g, " ").trim();
}

/**
 * The fallback title the Media Info dialog shows when a version has no quality
 * summary. It is the file name with the parts that already appear on the
 * detail line removed: the embedded size when the structured `fileSize` is
 * known, and a year that is also on the detail line. When the file name holds
 * the only size evidence, it is kept verbatim (and left unprettified so a
 * decimal size like "9.31GB" is not mangled by the dot-to-space prettifier).
 */
export function buildVersionFallbackTitle(
  fileName: string | undefined,
  { fileSize, detailLine }: { fileSize?: number; detailLine?: string },
): string {
  const sanitized = sanitizeVersionLabel(fileName);
  if (!sanitized) return "";
  const hasStructuredSize = formatFileSize(fileSize).length > 0;
  let label = hasStructuredSize ? stripReleaseSizeToken(sanitized) : sanitized;
  const duplicateYears = releaseYears(detailLine);
  if (duplicateYears.length > 0) {
    label = stripYears(label, duplicateYears);
  }
  const stillHasSize = /\b\d+(?:\.\d+)?\s*(?:TB|GB|MB)\b/i.test(label);
  return stillHasSize ? label.trim() : prettifyReleaseName(label);
}

export function formatPageCount(pages?: number): string {
  if (!pages || pages <= 0) return "";
  return `${pages.toLocaleString()} ${pages === 1 ? "page" : "pages"}`;
}

export function formatAddedAt(value?: string): string {
  if (!value) return "";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "";
  return new Intl.DateTimeFormat("en-US", {
    dateStyle: "medium",
    timeStyle: "short",
  }).format(date);
}

export function formatLanguageName(language?: string): string {
  if (!language) return "";

  const trimmed = language.trim();
  const normalized = trimmed.toLowerCase();
  if (!normalized) return "";

  const standardizedName = englishLanguageName(trimmed);
  if (standardizedName) return standardizedName;

  if (normalized.length > 3) {
    return trimmed
      .split(/[\s_-]+/)
      .filter(Boolean)
      .map((part) => part[0]?.toUpperCase() + part.slice(1).toLowerCase())
      .join(" ");
  }
  return getLanguageName(trimmed);
}

const SOURCE_HINT_PATTERNS: Array<{ pattern: RegExp; canonical: string }> = [
  { pattern: /\bremux\b/i, canonical: "Remux" },
  { pattern: /\bweb-dl\b/i, canonical: "WEB-DL" },
  { pattern: /\bwebrip\b/i, canonical: "WEBRip" },
  { pattern: /\bbluray\b/i, canonical: "BluRay" },
  { pattern: /\bbdrip\b/i, canonical: "BDRip" },
  { pattern: /\bhdtv\b/i, canonical: "HDTV" },
  { pattern: /\bdvdrip\b/i, canonical: "DVDRip" },
];

export function extractSourceHint(fileName: string): string | null {
  for (const { pattern, canonical } of SOURCE_HINT_PATTERNS) {
    if (pattern.test(fileName)) return canonical;
  }
  return null;
}

export function metadataLine(parts: Array<string | undefined | false>): string {
  return parts.filter(Boolean).join(" \u00B7 ");
}

export function videoTitle(track: VersionVideoTrack): string {
  return (
    track.title ||
    [track.width && track.height ? `${track.width}x${track.height}` : "", track.codec]
      .filter(Boolean)
      .join(" ") ||
    "Video"
  );
}

export function audioTitle(track: VersionAudioTrack): string {
  return (
    track.title ||
    track.embedded_title ||
    [track.language, track.codec, formatChannels(track.channels)].filter(Boolean).join(" ") ||
    "Audio"
  );
}

export function subtitleTitle(track: VersionSubtitleTrack): string {
  const language = formatLanguageName(track.language);
  const title = track.title || track.embedded_title || track.codec || "Subtitle";

  const languageLower = language.toLowerCase();
  const titleLower = title.toLowerCase();

  if (language && title && !titleLower.includes(languageLower)) {
    return `${language} - ${title}`;
  }

  return title || language;
}

export function compactVideoMeta(track: VersionVideoTrack): string {
  return metadataLine([
    track.profile,
    track.aspect_ratio,
    track.frame_rate ? `${track.frame_rate} fps` : "",
    formatBitrate(track.bitrate),
    track.bit_depth ? `${track.bit_depth}-bit` : "",
    track.video_range,
    track.dolby_vision,
    track.color_space,
    track.interlaced ? "Interlaced" : "",
  ]);
}

export function compactAudioMeta(track: VersionAudioTrack): string {
  return metadataLine([
    track.layout,
    formatBitrate(track.bitrate),
    formatSampleRate(track.sample_rate),
    track.bit_depth ? `${track.bit_depth}-bit` : "",
  ]);
}

export function compactSubtitleMeta(track: VersionSubtitleTrack): string {
  return metadataLine([
    track.external ? "External" : "Embedded",
    track.forced ? "Forced" : "",
    track.hearing_impaired ? "HI" : "",
    track.default ? "Default" : "",
    track.resolution,
  ]);
}
