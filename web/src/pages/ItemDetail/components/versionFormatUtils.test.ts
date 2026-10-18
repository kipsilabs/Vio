// @vitest-environment node

import { describe, expect, it } from "vitest";
import { buildReleasePresentation, type ReleasePresentationSource } from "./versionFormatUtils";

function makeSource(overrides: ReleasePresentationSource = {}): ReleasePresentationSource {
  return { ...overrides };
}

describe("buildReleasePresentation", () => {
  it("returns empty for an empty version", () => {
    expect(buildReleasePresentation(makeSource())).toBe("");
  });

  it("falls back for unscored and local rows even when quality pieces exist", () => {
    expect(
      buildReleasePresentation(
        makeSource({
          resolution: "1080p",
          codec_video: "h264",
          codec_audio: "aac",
          release_group: "GRP",
        }),
      ),
    ).toBe("");
  });

  it("elides empty pieces so no dangling brackets remain", () => {
    expect(buildReleasePresentation(makeSource({ matched_formats: ["", "  "] }))).toBe("");
    expect(
      buildReleasePresentation(
        makeSource({ matched_formats: ["English", " ", ""], resolution: "1080p" }),
      ),
    ).toBe("[English][1080p]");
  });

  it("renders matched formats alone", () => {
    expect(buildReleasePresentation(makeSource({ matched_formats: ["English"] }))).toBe(
      "[English]",
    );
  });

  it("renders the full template with every piece", () => {
    expect(
      buildReleasePresentation(
        makeSource({
          matched_formats: ["English", "Remux Tier 1"],
          resolution: "2160p",
          file_name: "Movie.2023.2160p.REMUX.mkv",
          hdr: true,
          codec_audio: "dts-hd ma",
          audio_channels: 6,
          codec_video: "hevc",
          release_group: "SPARKS",
        }),
      ),
    ).toBe("[English, Remux Tier 1][2160p Remux][HDR][DTS-HD 5.1][HEVC]-SPARKS");
  });

  it("omits the trailing hyphen when the release group is missing", () => {
    expect(
      buildReleasePresentation(
        makeSource({
          matched_formats: ["WEB Tier"],
          resolution: "1080p",
          codec_video: "h264",
        }),
      ),
    ).toBe("[WEB Tier][1080p][H264]");
  });

  it("keeps the label casing of the audio and video codec labels", () => {
    expect(
      buildReleasePresentation(
        makeSource({
          matched_formats: ["Atmos"],
          codec_audio: "truehd atmos",
          codec_video: "x265",
          release_group: "GRP",
        }),
      ),
    ).toBe("[Atmos][Atmos][X265]-GRP");
  });

  it("derives the video range from probed tracks", () => {
    expect(
      buildReleasePresentation(
        makeSource({
          matched_formats: ["HDR"],
          video_tracks: [{ video_range_type: "HDR10Plus" }],
        }),
      ),
    ).toBe("[HDR][HDR10+]");
  });

  it("prefers the structured channel count, then the effective audio track", () => {
    expect(
      buildReleasePresentation(
        makeSource({
          matched_formats: ["AAC"],
          codec_audio: "aac",
          audio_channels: 2,
          audio_tracks: [{ channels: 6 }],
        }),
      ),
    ).toBe("[AAC][AAC stereo]");
    expect(
      buildReleasePresentation(
        makeSource({
          matched_formats: ["AAC"],
          codec_audio: "aac",
          effective_audio_track_index: 1,
          audio_tracks: [{ channels: 2 }, { channels: 8 }],
        }),
      ),
    ).toBe("[AAC][AAC 7.1]");
    expect(
      buildReleasePresentation(
        makeSource({
          matched_formats: ["AAC"],
          codec_audio: "aac",
          effective_audio_track_index: 0,
          audio_tracks: [{ channels: 6 }, { channels: 2 }],
        }),
      ),
    ).toBe("[AAC][AAC 5.1]");
  });

  it("preserves odd and unicode names verbatim", () => {
    expect(
      buildReleasePresentation(
        makeSource({
          matched_formats: ["Résumé", "日本語"],
          resolution: "1080p",
          release_group: "WØLF-Québec",
        }),
      ),
    ).toBe("[Résumé, 日本語][1080p]-WØLF-Québec");
  });
});
