import { describe, expect, it } from "vitest";

import {
  buildPrePlaySubtitleCandidates,
  formatAudioTrackSummary,
  formatSubtitlePillSummary,
  inferSubtitleFlagsFromTitle,
} from "./prePlaySelection";
import type { DownloadedSubtitle, VersionSubtitleTrack } from "@/api/types";

describe("formatAudioTrackSummary", () => {
  it("uses language and compact format metadata instead of a container title", () => {
    expect(
      formatAudioTrackSummary({
        language: "en",
        codec: "eac3",
        channels: 6,
        title: "ATSC A/52B (AC-3, E-AC-3)",
      }),
    ).toBe("English · EAC3 · 5.1");
  });
});

describe("inferSubtitleFlagsFromTitle", () => {
  it("recognizes flag-only embedded titles", () => {
    expect(inferSubtitleFlagsFromTitle("Forced")).toEqual({
      forced: true,
      hearingImpaired: false,
      flagOnly: true,
    });
    expect(inferSubtitleFlagsFromTitle("SDH")).toEqual({
      forced: false,
      hearingImpaired: true,
      flagOnly: true,
    });
  });

  it("preserves meaningful regional titles as detail text", () => {
    expect(inferSubtitleFlagsFromTitle("Latin American").flagOnly).toBe(false);
  });
});

describe("formatSubtitlePillSummary", () => {
  it("uses language and a friendly format when the track title repeats the codec", () => {
    expect(
      formatSubtitlePillSummary({
        label: "SUBRIP",
        languageLabel: "English",
        codec: "subrip",
      }),
    ).toBe("English · SRT");
  });

  it("keeps accessibility markers in the compact summary", () => {
    expect(
      formatSubtitlePillSummary({
        label: "SDH",
        languageLabel: "English",
        codec: "subrip",
        hearingImpaired: true,
      }),
    ).toBe("English (SDH) · SRT");
  });

  it("keeps forced markers in the compact summary", () => {
    expect(
      formatSubtitlePillSummary({
        label: "Forced",
        languageLabel: "English",
        codec: "subrip",
        forced: true,
      }),
    ).toBe("English (Forced) · SRT");
  });
});

/**
 * Regression guard for the track-index domain: `selection.track_index` must be
 * the dense combined ordinal in the server's inventory order
 * (external → embedded → downloaded), NEVER the container ffprobe stream
 * index. Container indexes are non-dense (2,3,4 after video 0 + audio 1); if
 * they were ever sent as subtitle_track_index, persisted preferences would
 * resolve to the wrong track.
 */
describe("buildPrePlaySubtitleCandidates track_index", () => {
  // Container stream indexes: video 0, audio 1, embedded subs 2/3, external 4.
  // Deliberately non-dense so any container-index leak fails the assertions.
  const tracks: VersionSubtitleTrack[] = [
    { index: 2, external: false, language: "ja", codec: "subrip" },
    { index: 3, external: false, language: "en", codec: "subrip", forced: true },
    { index: 4, external: true, language: "de", codec: "srt", title: "German.srt" },
  ];
  const downloaded: DownloadedSubtitle[] = [
    {
      id: 77,
      media_file_id: 42,
      language: "fr",
      format: "srt",
      provider: "open",
      release_name: "Movie.French",
      score: 9,
      hearing_impaired: false,
      created_at: "2026-01-01T00:00:00Z",
    },
  ];

  it("assigns dense combined ordinals in the server's external→embedded→downloaded order", () => {
    const { all } = buildPrePlaySubtitleCandidates(tracks, downloaded);
    const byLanguage = new Map(all.map((c) => [c.language, c.selection.track_index]));
    // Server order: externals first (German.srt → 0), then embedded in
    // container order (index 2 "ja" → 1, index 3 "en" → 2), then downloaded (77 → 3).
    expect(byLanguage.get("de")).toBe(0);
    expect(byLanguage.get("ja")).toBe(1);
    expect(byLanguage.get("en")).toBe(2);
    expect(byLanguage.get("fr")).toBe(3);
  });

  it("never sends the container stream index as subtitle_track_index", () => {
    const { all } = buildPrePlaySubtitleCandidates(tracks, downloaded);
    const byLanguage = new Map(all.map((c) => [c.language, c]));
    // Each candidate carries both the container stream index (row.index) and
    // the dense ordinal (selection.track_index). Assert they differ where the
    // domains diverge — this fails if track_index ever reverts to row.index.
    expect(byLanguage.get("de")?.index).toBe(4);
    expect(byLanguage.get("de")?.selection.track_index).toBe(0);
    expect(byLanguage.get("ja")?.index).toBe(2);
    expect(byLanguage.get("ja")?.selection.track_index).toBe(1);
    expect(byLanguage.get("en")?.index).toBe(3);
    expect(byLanguage.get("en")?.selection.track_index).toBe(2);
  });

  it("numbers a downloaded-only item starting at zero", () => {
    const { all } = buildPrePlaySubtitleCandidates(undefined, downloaded);
    expect(all).toHaveLength(1);
    expect(all[0]?.selection.track_index).toBe(0);
  });

  it("falls back to the catalog enumeration index when tracks carry no explicit index", () => {
    const noIndexTracks: VersionSubtitleTrack[] = [
      { external: true, language: "de", codec: "srt" },
      { external: false, language: "en", codec: "subrip" },
    ];
    const { all } = buildPrePlaySubtitleCandidates(noIndexTracks, undefined);
    const byLanguage = new Map(all.map((c) => [c.language, c.selection.track_index]));
    expect(byLanguage.get("de")).toBe(0);
    expect(byLanguage.get("en")).toBe(1);
  });
});
