import { describe, expect, it } from "vitest";
import {
  resolveAppliedAudioIndex,
  resolveAudioPickIdentity,
  resolvePlanAudioIdentity,
  type AudioInventoryTrack,
} from "./audioTrackMatch";

const plan: AudioInventoryTrack[] = [
  { codec: "eac3", channels: 6, layout: "5.1", language: "eng", track_id: "file:7:audio:0" },
  { codec: "ac3", channels: 2, layout: "stereo", language: "spa", track_id: "file:7:audio:1" },
];

describe("resolvePlanAudioIdentity", () => {
  it("returns the plan identity when the menu shows the plan's own list", () => {
    expect(resolvePlanAudioIdentity(plan, plan, 1)).toEqual({
      id: "file:7:audio:1",
      index: 1,
    });
  });

  it("matches a reordered probed pick to the plan identity by language", () => {
    // The probe repair reordered the same languages and added a third track.
    const repaired: AudioInventoryTrack[] = [
      { codec: "ac3", channels: 2, layout: "stereo", language: "spa" },
      { codec: "eac3", channels: 6, layout: "5.1", language: "eng" },
      { codec: "aac", channels: 2, layout: "stereo", language: "fra" },
    ];
    // Picking spa (displayed index 0) names the plan's spa identity; the index
    // must be the plan ordinal the identity encodes, not the menu position.
    expect(resolvePlanAudioIdentity(plan, repaired, 0)).toEqual({
      id: "file:7:audio:1",
      index: 1,
    });
    expect(resolvePlanAudioIdentity(plan, repaired, 1)).toEqual({
      id: "file:7:audio:0",
      index: 0,
    });
    // The repaired-only track has no plan equivalent.
    expect(resolvePlanAudioIdentity(plan, repaired, 2)).toEqual({ id: "", index: 2 });
  });

  it("uses the plan's published selection_index for the id/index pair", () => {
    const withOrdinals: AudioInventoryTrack[] = [
      {
        codec: "eac3",
        channels: 6,
        layout: "5.1",
        language: "eng",
        track_id: "file:7:audio:4",
        selection_index: 4,
      },
      {
        codec: "ac3",
        channels: 2,
        layout: "stereo",
        language: "spa",
        track_id: "file:7:audio:5",
        selection_index: 5,
      },
    ];
    const displayed: AudioInventoryTrack[] = [
      { codec: "ac3", channels: 2, layout: "stereo", language: "spa" },
      { codec: "eac3", channels: 6, layout: "5.1", language: "eng" },
    ];
    expect(resolvePlanAudioIdentity(withOrdinals, displayed, 0)).toEqual({
      id: "file:7:audio:5",
      index: 5,
    });
    expect(resolvePlanAudioIdentity(withOrdinals, displayed, 1)).toEqual({
      id: "file:7:audio:4",
      index: 4,
    });
  });

  it("matches a MULTi language list regardless of its order", () => {
    const multiPlan: AudioInventoryTrack[] = [
      {
        codec: "eac3",
        channels: 6,
        language: "und",
        languages: ["en", "fr", "de"],
        track_id: "file:7:audio:0",
      },
      { codec: "ac3", channels: 2, language: "spa", track_id: "file:7:audio:1" },
    ];
    const displayed: AudioInventoryTrack[] = [
      { codec: "ac3", channels: 2, language: "spa" },
      { codec: "eac3", channels: 6, language: "und", languages: ["de", "fr", "en"] },
    ];
    expect(resolvePlanAudioIdentity(multiPlan, displayed, 1)).toEqual({
      id: "file:7:audio:0",
      index: 0,
    });
  });

  it("prefers an exact signature over a same-language sibling", () => {
    const commentaryPlan: AudioInventoryTrack[] = [
      {
        codec: "ac3",
        channels: 2,
        language: "eng",
        title: "Commentary",
        track_id: "file:7:audio:0",
      },
      { codec: "eac3", channels: 6, language: "eng", track_id: "file:7:audio:1" },
    ];
    const displayed: AudioInventoryTrack[] = [
      { codec: "eac3", channels: 6, language: "eng" },
      { codec: "ac3", channels: 2, language: "eng", title: "Commentary" },
    ];
    expect(resolvePlanAudioIdentity(commentaryPlan, displayed, 0).id).toBe("file:7:audio:1");
    expect(resolvePlanAudioIdentity(commentaryPlan, displayed, 1).id).toBe("file:7:audio:0");
  });

  it("falls back to the client ordinal when the plan names no identity", () => {
    const synthesized: AudioInventoryTrack[] = [{ codec: "eac3", channels: 6, language: "eng" }];
    const displayed: AudioInventoryTrack[] = [
      { codec: "eac3", channels: 6, language: "eng" },
      { codec: "ac3", channels: 2, language: "spa" },
    ];
    expect(resolvePlanAudioIdentity(synthesized, displayed, 1)).toEqual({ id: "", index: 1 });
  });

  it("falls back when a language is ambiguous in the plan", () => {
    const duplicatePlan: AudioInventoryTrack[] = [
      { codec: "eac3", channels: 6, language: "eng", track_id: "file:7:audio:0" },
      { codec: "ac3", channels: 2, language: "eng", track_id: "file:7:audio:1" },
    ];
    const displayed: AudioInventoryTrack[] = [{ codec: "aac", channels: 2, language: "eng" }];
    expect(resolvePlanAudioIdentity(duplicatePlan, displayed, 0)).toEqual({ id: "", index: 0 });
  });

  it("falls back when the pick is out of range", () => {
    expect(resolvePlanAudioIdentity(plan, [], 4)).toEqual({ id: "", index: 4 });
  });
});

describe("resolveAudioPickIdentity", () => {
  it("re-resolves a captured menu row against the replacement plan", () => {
    const picked: AudioInventoryTrack = {
      codec: "ac3",
      channels: 2,
      layout: "stereo",
      language: "spa",
    };
    expect(resolveAudioPickIdentity(plan, picked, 0)).toEqual({
      id: "file:7:audio:1",
      index: 1,
    });
  });

  it("returns null when a stable-identity plan has no counterpart", () => {
    const picked: AudioInventoryTrack = {
      codec: "aac",
      channels: 2,
      layout: "stereo",
      language: "fra",
    };
    expect(resolveAudioPickIdentity(plan, picked, 0)).toBeNull();
  });
});

describe("resolveAppliedAudioIndex", () => {
  it("follows the applied rows when a probe repair reorders them", () => {
    const displayed: AudioInventoryTrack[] = [
      { codec: "ac3", channels: 2, layout: "stereo", language: "spa" },
      { codec: "eac3", channels: 6, layout: "5.1", language: "eng" },
    ];
    expect(resolveAppliedAudioIndex(plan, displayed, { id: "file:7:audio:0", index: 0 })).toBe(1);
    expect(resolveAppliedAudioIndex(plan, displayed, { id: "file:7:audio:1", index: 1 })).toBe(0);
  });

  it("reports no marker when the committed track is gone from the applied list", () => {
    const displayed: AudioInventoryTrack[] = [
      { codec: "ac3", channels: 2, layout: "stereo", language: "spa" },
    ];
    expect(resolveAppliedAudioIndex(plan, displayed, { id: "file:7:audio:0", index: 0 })).toBe(-1);
  });
});
