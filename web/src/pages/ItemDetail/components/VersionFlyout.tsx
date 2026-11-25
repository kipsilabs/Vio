import type { FileVersion } from "@/api/types";
import { buildQualitySummary, buildVersionDetailLine } from "./versionFormatUtils";
import { audioScore, resolutionScore } from "./versionRankingUtils";

export { buildQualitySummary };

/**
 * Shared version-display helpers for the item page's catalog rows: the version
 * picker, the media-info dialog, and the download picker. The rows themselves
 * live in those components; this module only derives text, so every list reads
 * the same for one version.
 */

// The item-page picker's detail line, sharing one derivation with the player
// menu so empty details read identically across both lists.
export function buildDetailLine(version: FileVersion): string {
  return buildVersionDetailLine(version);
}

export function sortByResolution(versions: FileVersion[]): FileVersion[] {
  return [...versions].sort(
    (a, b) =>
      resolutionScore(b.resolution) - resolutionScore(a.resolution) ||
      Number(b.hdr) - Number(a.hdr) ||
      audioScore(b.codec_audio) - audioScore(a.codec_audio) ||
      (b.file_size ?? 0) - (a.file_size ?? 0),
  );
}
