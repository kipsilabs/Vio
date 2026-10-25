import { describe, expect, it } from "vitest";

import { isNeutralVirtualVersion } from "./neutralVirtualVersion";

function neutral(overrides: Partial<Parameters<typeof isNeutralVirtualVersion>[0]> = {}) {
  return {
    container: "virtual",
    file_path: "virtual://series/tt0434665/17/1",
    resolution: "",
    codec_video: "",
    codec_audio: "",
    video_tracks: [],
    audio_tracks: [],
    subtitle_tracks: [],
    ...overrides,
  };
}

describe("isNeutralVirtualVersion", () => {
  it("matches a collapsed series episode row with no probe evidence", () => {
    expect(isNeutralVirtualVersion(neutral())).toBe(true);
  });

  it("does not match a concrete provider result", () => {
    expect(
      isNeutralVirtualVersion(
        neutral({
          file_path: "virtual://series/tt0434665/17/1?result=23519",
        }),
      ),
    ).toBe(false);
    expect(
      isNeutralVirtualVersion(
        neutral({
          file_path: "virtual://series/tt0434665/17/1?profile=1080p&result=23519",
        }),
      ),
    ).toBe(false);
  });

  it("does not treat the plural results selector as a provider result", () => {
    expect(
      isNeutralVirtualVersion(
        neutral({ file_path: "virtual://series/tt0434665/17/1?results=all" }),
      ),
    ).toBe(true);
  });

  it("does not match a local file", () => {
    expect(
      isNeutralVirtualVersion(
        neutral({ container: "mkv", file_path: "/media/tv/Show/S01E01.mkv" }),
      ),
    ).toBe(false);
  });

  it("does not match a row with a published resolution or codec", () => {
    expect(isNeutralVirtualVersion(neutral({ resolution: "1080p" }))).toBe(false);
    expect(isNeutralVirtualVersion(neutral({ codec_video: "h264" }))).toBe(false);
    expect(isNeutralVirtualVersion(neutral({ codec_audio: "aac" }))).toBe(false);
  });

  it("does not match a row carrying audio or subtitle inventory", () => {
    expect(isNeutralVirtualVersion(neutral({ audio_tracks: [{ language: "eng" }] }))).toBe(false);
    expect(isNeutralVirtualVersion(neutral({ subtitle_tracks: [{ language: "eng" }] }))).toBe(
      false,
    );
    expect(isNeutralVirtualVersion(neutral({ video_tracks: [{ codec: "h264" }] }))).toBe(false);
  });

  it("does not match a virtual row outside the series namespace", () => {
    expect(
      isNeutralVirtualVersion(neutral({ file_path: "virtual://movie/tt1", resolution: "" })),
    ).toBe(false);
  });
});
