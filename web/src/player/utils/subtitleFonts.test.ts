import { describe, expect, it, vi } from "vitest";
import {
  fontBundleCacheKey,
  loadSubtitleFontBundle,
  loadSubtitleFontBundleResult,
} from "./subtitleFonts";

describe("fontBundleCacheKey", () => {
  it("strips only the embedded_stream_index query param", () => {
    const url = "/api/v1/stream/x/subtitles/8/fonts?embedded_stream_index=2&file_id=7&token=abc";
    const twin = "/api/v1/stream/x/subtitles/8/fonts?embedded_stream_index=3&file_id=7&token=abc";
    expect(fontBundleCacheKey(url)).toBe(fontBundleCacheKey(twin));
    expect(fontBundleCacheKey(url)).toBe("/api/v1/stream/x/subtitles/8/fonts?file_id=7&token=abc");
  });

  it("keeps a URL without the param unchanged", () => {
    const url = "/api/v1/stream/x/subtitles/8/fonts?file_id=7&token=abc";
    expect(fontBundleCacheKey(url)).toBe(url);
  });

  it("keeps a parameterless URL unchanged", () => {
    expect(fontBundleCacheKey("/api/v1/stream/x/subtitles/8/fonts")).toBe(
      "/api/v1/stream/x/subtitles/8/fonts",
    );
  });
});

describe("loadSubtitleFontBundle cache sharing", () => {
  it("shares one cache entry across URLs differing only in embedded_stream_index", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue({
      ok: true,
      status: 200,
      json: vi.fn().mockResolvedValue([{ name: "F.ttf", data: btoa("font-bytes") }]),
    } as unknown as Response);
    try {
      const urlA = "/fonts?embedded_stream_index=2&file_id=7&token=t";
      const urlB = "/fonts?embedded_stream_index=3&file_id=7&token=t";
      const [fontsA, fontsB] = await Promise.all([
        loadSubtitleFontBundle(urlA),
        loadSubtitleFontBundle(urlB),
      ]);
      expect(fontsA).toEqual(fontsB);
      expect(fontsA).toEqual([expect.any(Uint8Array)]);
      // The second URL hit the shared (normalized-key) cache entry: exactly one
      // network fetch happened.
      expect(fetchMock).toHaveBeenCalledTimes(1);
    } finally {
      fetchMock.mockRestore();
    }
  });

  it("treats a non-409 HTTP error as definitive and does not refetch it", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue({
      ok: false,
      status: 500,
      headers: { get: () => null },
    } as unknown as Response);
    try {
      const result = await loadSubtitleFontBundleResult("/fonts/definitive-500");
      // A 5xx names this URL as broken, not a budget miss: it must not be
      // reported pending, or the bounded refresh re-requests the same failure.
      expect(result).toEqual({ fonts: [], pending: false });

      const again = await loadSubtitleFontBundleResult("/fonts/definitive-500");
      expect(again).toEqual({ fonts: [], pending: false });
      // The definitive empty result is cached, so a second read is not a
      // second request against a URL the server just failed.
      expect(fetchMock).toHaveBeenCalledTimes(1);
    } finally {
      fetchMock.mockRestore();
    }
  });
});
