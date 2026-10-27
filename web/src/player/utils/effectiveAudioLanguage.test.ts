import { describe, expect, it } from "vitest";

import { resolveVersionAudioLanguage } from "./effectiveAudioLanguage";

describe("resolveVersionAudioLanguage", () => {
  it("reads the selected track's own language", () => {
    const version = {
      audio_tracks: [{ language: "eng" }, { language: "fra" }],
      effective_audio_track_index: 0,
      effective_audio_language: "eng",
    };

    expect(resolveVersionAudioLanguage(version, 1)).toBe("fra");
  });

  it("falls back to the version's effective language for the effective track", () => {
    const version = {
      audio_tracks: [{ language: "" }],
      effective_audio_track_index: 0,
      effective_audio_language: "eng",
    };

    expect(resolveVersionAudioLanguage(version, 0)).toBe("eng");
  });

  it("treats a negative marker as no track instead of the version's effective track", () => {
    // `resolveAppliedAudioIndex` returns -1 when the committed selection has no
    // row in the applied inventory. The marker names no track, so it must not
    // inherit the version's effective language.
    const version = {
      audio_tracks: [{ language: "eng" }],
      effective_audio_track_index: -1,
      effective_audio_language: "eng",
    };

    expect(resolveVersionAudioLanguage(version, -1)).toBeNull();
    expect(resolveVersionAudioLanguage(version, null)).toBeNull();
  });
});
