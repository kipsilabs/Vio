import { describe, expect, it } from "vitest";
import type { FileVersion } from "@/api/types";
import { buildQualitySummary, buildDetailLine, sortByResolution } from "./VersionFlyout";
import {
  audioLanguageLabels,
  buildQualitySummary as buildSharedQualitySummary,
  buildVersionDetailLine,
  formatScoreBadgeLabel,
  formatScoreTitle,
  hasFormatScore,
  subtitleLanguageLabels,
} from "./versionFormatUtils";

function makeVersion(overrides: Partial<FileVersion> = {}): FileVersion {
  return {
    file_id: overrides.file_id ?? 1,
    resolution: overrides.resolution ?? "1080p",
    codec_video: overrides.codec_video ?? "h264",
    codec_audio: overrides.codec_audio ?? "aac",
    hdr: overrides.hdr ?? false,
    container: overrides.container ?? "mkv",
    file_size: overrides.file_size ?? 0,
    duration: overrides.duration ?? 0,
    bitrate: overrides.bitrate ?? 0,
    file_name: overrides.file_name,
    file_path: overrides.file_path,
    edition_raw: overrides.edition_raw,
    release_name: overrides.release_name,
    release_group: overrides.release_group,
    audio_tracks: overrides.audio_tracks,
    video_tracks: overrides.video_tracks,
    subtitle_tracks: overrides.subtitle_tracks,
  };
}

describe("buildQualitySummary", () => {
  it("joins resolution, codec, HDR, and audio", () => {
    const version = makeVersion({
      resolution: "2160p",
      codec_video: "hevc",
      hdr: true,
      codec_audio: "truehd",
    });
    expect(buildQualitySummary(version)).toBe("2160p · HEVC · HDR · TrueHD");
  });

  it("labels the just-in-time virtual results action", () => {
    expect(buildQualitySummary(makeVersion({ file_path: "virtual://movie/tt1?results=all" }))).toBe(
      "More results…",
    );
  });

  it("does not label returned result files as the action", () => {
    expect(
      buildQualitySummary(
        makeVersion({ file_path: "virtual://movie/tt1?results=all&result=abc123" }),
      ),
    ).toBe("1080p · H264 · AAC");
  });

  it("omits HDR segment when hdr is false", () => {
    const version = makeVersion({
      resolution: "1080p",
      codec_video: "h264",
      hdr: false,
      codec_audio: "aac",
    });
    const result = buildQualitySummary(version);
    expect(result).not.toContain("HDR");
    expect(result).toBe("1080p · H264 · AAC");
  });

  it("omits empty segments", () => {
    const version = makeVersion({
      resolution: "1080p",
      codec_video: "",
      hdr: false,
      codec_audio: "",
    });
    expect(buildQualitySummary(version)).toBe("1080p");
  });

  it("includes resolution even when other fields are empty", () => {
    const version = makeVersion({
      resolution: "720p",
      codec_video: "",
      hdr: false,
      codec_audio: "",
    });
    expect(buildQualitySummary(version)).toBe("720p");
  });

  it("maps audio codec label using mapAudioLabel", () => {
    const version = makeVersion({
      resolution: "2160p",
      codec_video: "hevc",
      hdr: true,
      codec_audio: "TrueHD Atmos",
    });
    expect(buildQualitySummary(version)).toBe("2160p · HEVC · HDR · Atmos");
  });

  it("appends audio languages after the audio codec", () => {
    const version = makeVersion({
      resolution: "1080p",
      codec_video: "h264",
      codec_audio: "eac3",
      audio_tracks: [{ language: "MULTI" }, { language: "fra" }],
    });
    expect(buildQualitySummary(version)).toBe("1080p · H264 · EAC3");
  });

  it("shows audio languages even when the audio codec is missing", () => {
    const version = makeVersion({
      resolution: "1080p",
      codec_video: "h264",
      codec_audio: "",
      audio_tracks: [{ language: "eng" }],
    });
    expect(buildQualitySummary(version)).toBe("1080p · H264");
  });

  it("falls back to container for ebook-style files without video quality", () => {
    const version = makeVersion({
      resolution: "",
      codec_video: "",
      codec_audio: "",
      hdr: false,
      container: "epub",
      file_name: "A Psalm for the Wild-Built.epub",
    });

    expect(buildQualitySummary(version)).toBe("EPUB");
  });
});

describe("buildDetailLine", () => {
  it("shows file size and source hint when present", () => {
    const version = makeVersion({
      file_size: 45 * 1024 ** 3,
      file_name: "Movie.2160p.Remux.mkv",
    });
    expect(buildDetailLine(version)).toBe("45.0 GB · Remux");
  });

  it("shows only file size when no source hint matches", () => {
    const version = makeVersion({
      file_size: 10 * 1024 ** 3,
      file_name: "movie.mkv",
    });
    expect(buildDetailLine(version)).toBe("10.0 GB");
  });

  it("returns empty string when file_size is zero and no name", () => {
    const version = makeVersion({ file_size: 0, file_name: undefined });
    expect(buildDetailLine(version)).toBe("");
  });

  it("shows source hint only when file_size is zero but name matches", () => {
    const version = makeVersion({ file_size: 0, file_name: "Movie.WEB-DL.mkv" });
    expect(buildDetailLine(version)).toBe("WEB-DL");
  });

  it("shows subtitle languages in the detail line", () => {
    const version = makeVersion({
      file_size: 0,
      subtitle_tracks: [{ language: "eng" }, { language: "fra" }],
    });
    expect(buildDetailLine(version)).toBe("");
  });

  it("leads virtual versions with the provider release name", () => {
    const version = makeVersion({
      container: "virtual",
      file_path: "virtual://movie/tt1?result=abc123",
      edition_raw: "Disclosure Day 2160p DV HDR10 TrueHD MULTI",
      file_size: 0,
      subtitle_tracks: [{ language: "eng" }, { language: "fra" }],
    });
    expect(buildDetailLine(version)).toBe("Disclosure Day 2160p DV HDR10 TrueHD MULTI");
  });

  it("keeps size and source hint for virtual versions", () => {
    const version = makeVersion({
      container: "virtual",
      file_path: "virtual://movie/tt1?result=abc123",
      edition_raw: "Movie.2160p.Remux.mkv",
      file_size: 45 * 1024 ** 3,
    });
    expect(buildDetailLine(version)).toBe("Movie 2160p Remux mkv · 45.0 GB · Remux");
  });

  it("prettifies release-style separators in the release name", () => {
    const version = makeVersion({
      release_name: "Mission.Impossible.2023.2160p.Multi-AltMount",
      file_size: 0,
    });
    expect(buildDetailLine(version)).toBe("Mission Impossible 2023 2160p Multi-AltMount");
  });

  it("leads with release_name and prefers it over edition_raw", () => {
    const version = makeVersion({
      edition_raw: "Provider.Raw.Label",
      release_name: "Movie.2023.1080p.WEB-DL.x264-GRP",
      file_size: 10 * 1024 ** 3,
    });
    expect(buildDetailLine(version)).toBe("Movie 2023 1080p WEB-DL x264-GRP · 10.0 GB · WEB-DL");
  });

  it("falls back to edition_raw when release_name is absent", () => {
    const version = makeVersion({
      edition_raw: "Movie.2023.1080p.WEB-DL.x264-GRP",
      file_size: 0,
    });
    expect(buildDetailLine(version)).toBe("Movie 2023 1080p WEB-DL x264-GRP · WEB-DL");
  });

  it("shows the provider label's embedded size only once when file_size is known", () => {
    const version = makeVersion({
      container: "virtual",
      file_path: "virtual://movie/tt1?result=abc123",
      edition_raw: "Disclosure Day 2160p DV HDR10 TrueHD MULTI · 45.2 GB",
      file_size: 45 * 1024 ** 3,
    });
    // The structured file_size is kept; the label's parsed copy is dropped.
    expect(buildDetailLine(version)).toBe("Disclosure Day 2160p DV HDR10 TrueHD MULTI · 45.0 GB");
  });

  it("keeps the provider label's size when file_size is unknown", () => {
    const version = makeVersion({
      container: "virtual",
      file_path: "virtual://movie/tt1?result=abc123",
      edition_raw: "Disclosure Day 2160p · 45 GB",
      file_size: 0,
    });
    expect(buildDetailLine(version)).toBe("Disclosure Day 2160p · 45 GB");
  });
});

describe("sortByResolution", () => {
  it("sorts versions descending by resolution score", () => {
    const versions = [
      makeVersion({ file_id: 1, resolution: "720p" }),
      makeVersion({ file_id: 2, resolution: "2160p" }),
      makeVersion({ file_id: 3, resolution: "1080p" }),
    ];
    const sorted = sortByResolution(versions);
    expect(sorted.map((v) => v.resolution)).toEqual(["2160p", "1080p", "720p"]);
  });

  it("does not mutate the original array", () => {
    const versions = [
      makeVersion({ file_id: 1, resolution: "720p" }),
      makeVersion({ file_id: 2, resolution: "2160p" }),
    ];
    const original = [...versions];
    sortByResolution(versions);
    expect(versions[0]!.resolution).toBe(original[0]!.resolution);
    expect(versions[1]!.resolution).toBe(original[1]!.resolution);
  });

  it("keeps equal resolutions in original order (stable)", () => {
    const versions = [
      makeVersion({ file_id: 1, resolution: "1080p" }),
      makeVersion({ file_id: 2, resolution: "1080p" }),
    ];
    const sorted = sortByResolution(versions);
    expect(sorted.map((v) => v.file_id)).toEqual([1, 2]);
  });

  it("handles empty array", () => {
    expect(sortByResolution([])).toEqual([]);
  });

  it("handles single element", () => {
    const versions = [makeVersion({ file_id: 1, resolution: "1080p" })];
    expect(sortByResolution(versions)).toHaveLength(1);
  });

  it("places unknown resolutions at the end", () => {
    const versions = [
      makeVersion({ file_id: 1, resolution: "unknown" }),
      makeVersion({ file_id: 2, resolution: "1080p" }),
    ];
    const sorted = sortByResolution(versions);
    expect(sorted[0]!.resolution).toBe("1080p");
    expect(sorted[1]!.resolution).toBe("unknown");
  });
});

describe("shared version row builders", () => {
  // These are the exact call sites the in-player menu and the item-page picker
  // use, driven through the public exports, so a drift between the two lists
  // fails here.
  function playerSummary(version: FileVersion): string {
    return buildSharedQualitySummary(version);
  }
  function playerDetail(version: FileVersion): string {
    return buildVersionDetailLine(version);
  }

  it("produces the identical quality summary for both list builders", () => {
    const cases: FileVersion[] = [
      makeVersion({ resolution: "2160p", codec_video: "hevc", hdr: true, codec_audio: "truehd" }),
      makeVersion({
        resolution: "2160p",
        codec_video: "hevc",
        hdr: true,
        codec_audio: "TrueHD Atmos",
        file_name: "Movie.2160p.Remux.mkv",
      }),
      makeVersion({ resolution: "", codec_video: "", codec_audio: "", container: "epub" }),
      makeVersion({ file_path: "virtual://movie/tt1?results=all" }),
      makeVersion({ file_path: "virtual://movie/tt1?results=all&result=abc123" }),
      makeVersion({ resolution: "1080p", codec_video: "", codec_audio: "", hdr: false }),
    ];

    for (const version of cases) {
      expect(buildQualitySummary(version)).toBe(playerSummary(version));
    }

    // The "More results…" action must reach the player list too.
    expect(playerSummary(makeVersion({ file_path: "virtual://movie/tt1?results=all" }))).toBe(
      "More results…",
    );
  });

  it("produces the identical detail line for both list builders", () => {
    const cases: FileVersion[] = [
      makeVersion({ file_size: 45 * 1024 ** 3, file_name: "Movie.2160p.Remux.mkv" }),
      makeVersion({ release_name: "Movie.2023.1080p.WEB-DL.x264-GRP", file_size: 10 * 1024 ** 3 }),
      makeVersion({
        edition_raw: "Disclosure Day 2160p · 45 GB",
        container: "virtual",
        file_size: 0,
      }),
      makeVersion({ file_size: 0, file_name: undefined }),
    ];

    for (const version of cases) {
      expect(buildDetailLine(version)).toBe(playerDetail(version));
    }

    // Empty details read the same (empty) on both lists.
    expect(playerDetail(makeVersion({ file_size: 0 }))).toBe("");
  });

  it("derives audio and subtitle badges through the shared language helper", () => {
    const version = makeVersion({
      audio_tracks: [{ language: "eng" }, { languages: ["en", "fr", "es"] }],
      subtitle_tracks: [{ language: "deu" }, { language: "German" }],
    });
    expect(audioLanguageLabels(version.audio_tracks)).toEqual(["English", "French", "Spanish"]);
    expect(subtitleLanguageLabels(version.subtitle_tracks)).toEqual(["German"]);
  });

  it("derives the format-score badge identically for both lists", () => {
    expect(hasFormatScore(850)).toBe(true);
    expect(hasFormatScore(0)).toBe(false);
    expect(hasFormatScore(undefined)).toBe(false);
    expect(hasFormatScore(-200)).toBe(true);

    expect(formatScoreBadgeLabel(850)).toBe("★ 850");
    expect(formatScoreBadgeLabel(-200)).toBe("★ -200");
    expect(formatScoreTitle(850, "4K+HDR")).toBe("Format score 850 · 4K+HDR");
    expect(formatScoreTitle(850, null)).toBe("Format score 850");
  });
});
