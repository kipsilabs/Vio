import { describe, expect, it } from "vitest";
import {
  buildPublishedSubtitleTracks,
  subtitleLanguageBase,
  type SubtitleInventoryTrackInput,
} from "./subtitleInventory";

/**
 * Round-trip guards for the shared subtitle-ordinal derivation.
 *
 * Every expected ordinal below was produced by the server's normative
 * implementation, `playback.BuildSubtitleInventoryV3`
 * (internal/playback/subtitle_inventory_v3.go), for the same inventory. The
 * client helper must reproduce the server's deduplicated, dense combined
 * ordinal space: external sidecars → embedded container tracks → downloaded
 * rows, with suppressed duplicates consuming no ordinal.
 */

describe("subtitleLanguageBase", () => {
  it("folds ISO aliases, display names and regional/script forms like the server", () => {
    for (const value of ["en", "eng", "ENG", "English", "EN-US", "en-GB"]) {
      expect(subtitleLanguageBase(value)).toBe("en");
    }
    for (const value of ["fr", "fre", "fra", "French", "FR-CA"]) {
      expect(subtitleLanguageBase(value)).toBe("fr");
    }
    expect(subtitleLanguageBase("zh-Hant")).toBe("zh");
    expect(subtitleLanguageBase("pt-BR")).toBe("pt");
    expect(subtitleLanguageBase("es-ES")).toBe("es");
  });

  it("does not alias-collapse bare codes the server's switch omits", () => {
    // CanonicalLanguageBase("sr") and ("nb") are "" on the server, so these
    // must not join an alias key.
    expect(subtitleLanguageBase("sr")).toBe("");
    expect(subtitleLanguageBase("nb")).toBe("");
    expect(subtitleLanguageBase("xx")).toBe("");
    expect(subtitleLanguageBase("")).toBe("");
    expect(subtitleLanguageBase(undefined)).toBe("");
  });

  it("accepts any three-letter code except und/mul, like the server", () => {
    expect(subtitleLanguageBase("heb")).toBe("he");
    expect(subtitleLanguageBase("fil")).toBe("fil");
    expect(subtitleLanguageBase("und")).toBe("");
    expect(subtitleLanguageBase("mul")).toBe("");
  });
});

describe("buildPublishedSubtitleTracks round-trip against the server", () => {
  it("deduplicates an EN-US beside ENG bare alias to a single dense ordinal", () => {
    // Server ground truth (external bare aliases, one track described twice):
    // combined=0 count=1.
    const tracks: SubtitleInventoryTrackInput[] = [
      { index: 2, language: "EN-US", codec: "subrip", title: "English" },
      { index: 2, language: "ENG", codec: "subrip", title: "English" },
    ];
    // The wire marks externals with `external: true`; keep them embedded-shaped
    // here to prove the same-index same-title collapse (H case).
    const published = buildPublishedSubtitleTracks(tracks);
    expect(published).toHaveLength(1);
    expect(published[0]?.index).toBe(0);
    expect(published[0]?.language).toBe("EN-US");
  });

  it("collapses a duplicated external path and keeps the distinct sidecar", () => {
    // Server ground truth (B case): external /m/a.en.srt described twice
    // collapses; a.fr.srt forced survives; embedded ENG/FRE follow.
    // combined=0 external ENG, 1 external FRE forced, 2 embedded ENG, 3 embedded FRE.
    const wireOrder: SubtitleInventoryTrackInput[] = [
      // Wire order is embedded-first; the server's ordinal space is external-first.
      { index: 2, language: "ENG", codec: "subrip", title: "English" },
      { index: 3, language: "FRE", codec: "ass", title: "French" },
      { index: 4, external: true, language: "ENG", codec: "srt", file_name: "a.en.srt" },
      { index: 5, external: true, language: "EN-US", codec: "srt", file_name: "a.en.srt" },
      {
        index: 6,
        external: true,
        language: "FRE",
        codec: "srt",
        file_name: "a.fr.srt",
        forced: true,
      },
    ];
    const published = buildPublishedSubtitleTracks(wireOrder);
    expect(published.map((track) => track.index)).toEqual([0, 1, 2, 3]);
    expect(published.map((track) => track.source)).toEqual([
      "external",
      "external",
      "embedded",
      "embedded",
    ]);
    expect(published.map((track) => track.language)).toEqual(["ENG", "FRE", "ENG", "FRE"]);
    expect(published[1]?.forced).toBe(true);
  });

  it("keeps distinct same-language streams apart by container index (forced/SDH variants)", () => {
    // Server ground truth (C case): three embedded ENG streams at indexes 2/3/4
    // with differing flags are all published, dense 0..2.
    const tracks: SubtitleInventoryTrackInput[] = [
      { index: 2, language: "ENG", codec: "srt", title: "English" },
      { index: 3, language: "ENG", codec: "srt", title: "English", forced: true },
      {
        index: 4,
        language: "ENG",
        codec: "srt",
        title: "English SDH",
        hearing_impaired: true,
      },
    ];
    const published = buildPublishedSubtitleTracks(tracks);
    expect(published.map((track) => track.index)).toEqual([0, 1, 2]);
    expect(published.map((track) => Boolean(track.forced))).toEqual([false, true, false]);
    expect(published.map((track) => Boolean(track.hearing_impaired))).toEqual([false, false, true]);
  });

  it("carries the container default flag onto the same-language streams", () => {
    // Two German SRT streams with identical codec and title are distinguishable
    // only by their ordinal and the default flag the menu badges.
    const published = buildPublishedSubtitleTracks([
      { index: 3, language: "de", codec: "srt", title: "German", default: true },
      { index: 4, language: "de", codec: "srt", title: "German" },
    ]);
    expect(published.map((track) => track.index)).toEqual([0, 1]);
    expect(published.map((track) => Boolean(track.default))).toEqual([true, false]);
  });

  it("keeps two same-basename sidecars distinct when path_key is present", () => {
    // The server keys a sidecar on its full path and now publishes an opaque
    // path_key hash of it. Two sidecars named a.en.srt in different directories
    // must not collapse: with path_key the client matches the server.
    const published = buildPublishedSubtitleTracks([
      {
        index: 4,
        external: true,
        language: "ENG",
        codec: "srt",
        file_name: "a.en.srt",
        path_key: "hash-dir-one",
      },
      {
        index: 5,
        external: true,
        language: "ENG",
        codec: "srt",
        file_name: "a.en.srt",
        path_key: "hash-dir-two",
      },
    ]);
    expect(published).toHaveLength(2);
    expect(published.map((track) => track.index)).toEqual([0, 1]);
  });

  it("collapses two same-basename sidecars when path_key is absent (older server)", () => {
    // Fallback contract: a server predating path_key publishes only the
    // basename, so the client cannot separate two same-basename sidecars and
    // must dedupe them rather than invent ordinals the server did not publish.
    const published = buildPublishedSubtitleTracks([
      { index: 4, external: true, language: "ENG", codec: "srt", file_name: "a.en.srt" },
      { index: 5, external: true, language: "ENG", codec: "srt", file_name: "a.en.srt" },
    ]);
    expect(published).toHaveLength(1);
  });

  it("treats forced as part of the identity so a forced and plain variant coexist", () => {
    // Same stream index and title, differing only in forced: the server's
    // alias key includes forced, so both survive.
    const published = buildPublishedSubtitleTracks([
      { index: 2, language: "ENG", codec: "srt", title: "English" },
      { index: 2, language: "ENG", codec: "srt", title: "English", forced: true },
    ]);
    expect(published).toHaveLength(2);
    expect(published.map((track) => Boolean(track.forced))).toEqual([false, true]);
  });

  it("includes downloaded rows after the file's own ranges and keys them by row id", () => {
    // Server ground truth (D/K case): one external (0), one embedded (1), two
    // distinct download rows in one language (2, 3) — both kept because the
    // stable row id discriminates and the source range differs.
    const published = buildPublishedSubtitleTracks(
      [
        { index: 2, language: "ENG", codec: "subrip", title: "English" },
        { index: 3, external: true, language: "ENG", codec: "srt", file_name: "a.eng.srt" },
      ],
      [
        { id: 11, language: "ENG", format: "srt" },
        { id: 12, language: "ENG", format: "srt" },
      ],
    );
    expect(published.map((track) => track.index)).toEqual([0, 1, 2, 3]);
    expect(published.map((track) => track.source)).toEqual([
      "external",
      "embedded",
      "downloaded",
      "downloaded",
    ]);
  });

  it("assigns dense ordinals with no gaps when an early entry is suppressed", () => {
    // Regression for the desync: the suppressed duplicate consumes no ordinal,
    // so the surviving track that follows is published at 2, not 3.
    const published = buildPublishedSubtitleTracks([
      { index: 2, language: "ENG", codec: "srt", title: "English" },
      { index: 2, language: "EN-US", codec: "srt", title: "English" }, // collapsed
      { index: 3, language: "FRE", codec: "srt", title: "French" },
    ]);
    expect(published.map((track) => track.index)).toEqual([0, 1]);
    expect(published.map((track) => track.language)).toEqual(["ENG", "FRE"]);
  });

  it("never suppresses an unknown-language track with no other identity", () => {
    const published = buildPublishedSubtitleTracks([
      { index: 2, language: "", codec: "subrip" },
      { index: 3, language: "unknown", codec: "subrip" },
    ]);
    expect(published).toHaveLength(2);
    expect(published.map((track) => track.index)).toEqual([0, 1]);
  });
});
