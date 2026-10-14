import { describe, expect, it } from "vitest";

import {
  WINDOWED_MANIFEST_QUERY_PARAM,
  WINDOWED_MANIFEST_RELOAD_AHEAD_SECONDS,
  hlsTargetWithinServedWindow,
  servedHlsWindow,
  shouldAdvanceHlsWindow,
  withWindowedHlsManifest,
} from "./hlsWindowedManifest";

describe("withWindowedHlsManifest", () => {
  it("adds the opt-in to a path without a query", () => {
    expect(withWindowedHlsManifest("/api/v2/playback/transcode/abc/master.m3u8", true)).toBe(
      `/api/v2/playback/transcode/abc/master.m3u8?${WINDOWED_MANIFEST_QUERY_PARAM}=1`,
    );
  });

  it("preserves an existing query", () => {
    expect(withWindowedHlsManifest("/x/master.m3u8?st=token", true)).toBe(
      `/x/master.m3u8?st=token&${WINDOWED_MANIFEST_QUERY_PARAM}=1`,
    );
  });

  it("is idempotent and leaves the URL alone when disabled", () => {
    const once = withWindowedHlsManifest("/x/master.m3u8?st=token", true);
    expect(withWindowedHlsManifest(once, true)).toBe(once);
    expect(withWindowedHlsManifest("/x/master.m3u8?st=token", false)).toBe(
      "/x/master.m3u8?st=token",
    );
    expect(withWindowedHlsManifest("", true)).toBe("");
  });
});

describe("servedHlsWindow", () => {
  it("skips the gap prefix and reports absolute media times", () => {
    const window = servedHlsWindow({
      live: false,
      edge: 470,
      fragments: [
        { gap: true, start: 0 },
        { gap: true, start: 200 },
        { start: 400 },
        { start: 402 },
      ],
    });
    expect(window).toEqual({ start: 400, end: 470, live: false });
  });

  it("reports a full manifest that starts at zero", () => {
    const window = servedHlsWindow({
      live: false,
      edge: 3600,
      fragments: [{ start: 0 }, { start: 2 }],
    });
    expect(window).toEqual({ start: 0, end: 3600, live: false });
  });

  it("returns null when the details are not readable", () => {
    expect(servedHlsWindow(null)).toBeNull();
    expect(servedHlsWindow({ live: false, edge: 10, fragments: [] })).toBeNull();
    expect(
      servedHlsWindow({ live: false, edge: Number.NaN, fragments: [{ start: 0 }] }),
    ).toBeNull();
  });
});

describe("shouldAdvanceHlsWindow", () => {
  const window = { start: 400, end: 470, live: false };

  it("refetches as the playhead nears the window edge of a shorter playlist", () => {
    expect(shouldAdvanceHlsWindow(window, 3600, 451)).toBe(true);
    expect(shouldAdvanceHlsWindow(window, 3600, 449)).toBe(false);
  });

  it("never refetches a live playlist, which hls.js reloads itself", () => {
    expect(shouldAdvanceHlsWindow({ ...window, live: true }, 3600, 469)).toBe(false);
  });

  it("does not refetch when the window reaches the media end", () => {
    expect(shouldAdvanceHlsWindow({ start: 0, end: 3600, live: false }, 3600, 3590)).toBe(false);
  });

  it("keeps the default ahead margin", () => {
    expect(WINDOWED_MANIFEST_RELOAD_AHEAD_SECONDS).toBeGreaterThan(0);
    expect(shouldAdvanceHlsWindow(window, 3600, 470 - WINDOWED_MANIFEST_RELOAD_AHEAD_SECONDS)).toBe(
      true,
    );
  });
});

describe("hlsTargetWithinServedWindow", () => {
  it("allows a target inside the served window", () => {
    const window = { start: 400, end: 470, live: false };
    expect(hlsTargetWithinServedWindow(window, 400)).toBe(true);
    expect(hlsTargetWithinServedWindow(window, 450)).toBe(true);
    expect(hlsTargetWithinServedWindow(window, 470)).toBe(true);
  });

  it("refuses a target inside the omitted gap prefix", () => {
    const window = { start: 400, end: 470, live: false };
    expect(hlsTargetWithinServedWindow(window, 399)).toBe(false);
  });

  it("refuses a forward target past the listed window", () => {
    const window = { start: 400, end: 470, live: false };
    expect(hlsTargetWithinServedWindow(window, 600)).toBe(false);
  });

  it("allows any target when no window is known", () => {
    expect(hlsTargetWithinServedWindow(null, 0)).toBe(true);
    expect(hlsTargetWithinServedWindow(null, 10_000)).toBe(true);
  });
});
