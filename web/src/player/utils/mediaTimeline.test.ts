import { describe, expect, it } from "vitest";

import {
  mediaDurationSeconds,
  subtitleChangePositionSeconds,
  subtitleStartPositionSeconds,
  toMediaTime,
  toPlayerTime,
} from "./mediaTimeline";

describe("toMediaTime / toPlayerTime", () => {
  it("round-trips a position through a stream origin", () => {
    expect(toMediaTime(60, 3000)).toBe(3060);
    expect(toPlayerTime(3060, 3000)).toBe(60);
  });

  it("preserves an out-of-window target before the stream origin", () => {
    expect(toMediaTime(-10, 0)).toBe(0);
    expect(toPlayerTime(10, 3000)).toBe(-2990);
  });
});

describe("mediaDurationSeconds", () => {
  // The regression this function exists for: a copy remux resumed at 50
  // minutes reports a player-local duration covering only the produced
  // window. Pairing that with a media-time position of ~3060 would read as
  // "finished", latching the item watched and clearing its resume point.
  it("does not let a produced-window duration stand in for the runtime", () => {
    const positionSeconds = toMediaTime(60, 3000);
    const duration = mediaDurationSeconds(5400, 120);

    expect(duration).toBe(5400);
    expect(positionSeconds >= (duration ?? 0)).toBe(false);
  });

  it("falls back to the element duration only when the server has none", () => {
    expect(mediaDurationSeconds(0, 120)).toBe(120);
    expect(mediaDurationSeconds(null, 120)).toBe(120);
    expect(mediaDurationSeconds(undefined, 120)).toBe(120);
  });

  it("returns undefined when neither runtime is known, so callers omit it", () => {
    expect(mediaDurationSeconds(0, 0)).toBeUndefined();
    expect(mediaDurationSeconds(null, undefined)).toBeUndefined();
    expect(mediaDurationSeconds(undefined, NaN)).toBeUndefined();
  });
});

describe("subtitleStartPositionSeconds", () => {
  it("uses the resume anchor while media metadata is not ready", () => {
    const resumePosition = toMediaTime(60, 3000);

    expect(subtitleStartPositionSeconds(0, 0, resumePosition)).toBe(3060);
  });
});


describe("subtitleChangePositionSeconds", () => {
  it("prefers the element's live position over the plan's start anchor", () => {
    expect(
      subtitleChangePositionSeconds({
        liveElementSeconds: 187.5,
        pendingSeekSeconds: null,
        lastPlayedSeconds: 180,
        fetchAnchorSeconds: 0,
      }),
    ).toBe(187.5);
  });

  it("uses an in-flight seek target when the element has no media yet", () => {
    expect(
      subtitleChangePositionSeconds({
        liveElementSeconds: null,
        pendingSeekSeconds: 900,
        lastPlayedSeconds: 180,
        fetchAnchorSeconds: 0,
      }),
    ).toBe(900);
  });

  it("uses the last reported position after a reload before the anchor", () => {
    expect(
      subtitleChangePositionSeconds({
        liveElementSeconds: null,
        pendingSeekSeconds: null,
        lastPlayedSeconds: 180,
        fetchAnchorSeconds: 0,
      }),
    ).toBe(180);
  });

  it("falls back to the fetch anchor before the transport has played", () => {
    expect(
      subtitleChangePositionSeconds({
        liveElementSeconds: null,
        pendingSeekSeconds: null,
        lastPlayedSeconds: null,
        fetchAnchorSeconds: 45,
      }),
    ).toBe(45);
    expect(
      subtitleChangePositionSeconds({
        liveElementSeconds: Number.NaN,
        pendingSeekSeconds: -1,
        lastPlayedSeconds: null,
        fetchAnchorSeconds: null,
      }),
    ).toBe(0);
  });
});
