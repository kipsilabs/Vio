import { act, renderHook } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { useSubtitleFontPrefetch } from "./useSubtitleFontPrefetch";
import type { PlayerSubtitleInfo } from "../types";

const loadSubtitleFontBundle = vi.hoisted(() =>
  vi.fn((_url: string, _signal?: AbortSignal) => Promise.resolve([])),
);

vi.mock("../utils/subtitleFonts", () => ({
  fontBundleCacheKey: (url: string) => url,
  loadSubtitleFontBundle: (url: string) => loadSubtitleFontBundle(url),
}));

function track(overrides: Partial<PlayerSubtitleInfo> = {}): PlayerSubtitleInfo {
  return {
    index: 0,
    language: "eng",
    label: "English",
    codec: "ass",
    url: "/stream/x/subtitles/0.ass",
    ...overrides,
  };
}

afterEach(() => {
  loadSubtitleFontBundle.mockClear();
});

describe("useSubtitleFontPrefetch", () => {
  it("prefetches one font bundle per unique font_bundle_url on mount", () => {
    const urls = [
      track({ index: 1, font_bundle_url: "/fonts/a" }),
      track({ index: 2, font_bundle_url: "/fonts/a" }),
      track({ index: 3, font_bundle_url: "/fonts/b" }),
      track({ index: 4 }),
    ];
    renderHook(() => useSubtitleFontPrefetch(urls));
    expect(loadSubtitleFontBundle).toHaveBeenCalledTimes(2);
    expect(loadSubtitleFontBundle).toHaveBeenCalledWith("/fonts/a");
    expect(loadSubtitleFontBundle).toHaveBeenCalledWith("/fonts/b");
  });

  it("does not fetch when no track advertises a font bundle", () => {
    renderHook(() => useSubtitleFontPrefetch([track({ index: 1 }), track({ index: 2 })]));
    expect(loadSubtitleFontBundle).not.toHaveBeenCalled();
  });

  it("does not refire on unrelated re-renders with the same subtitleUrls", () => {
    const urls = [track({ font_bundle_url: "/fonts/a" })];
    const { rerender } = renderHook(
      ({ subtitleUrls }: { subtitleUrls: PlayerSubtitleInfo[] }) =>
        useSubtitleFontPrefetch(subtitleUrls),
      { initialProps: { subtitleUrls: urls } },
    );
    expect(loadSubtitleFontBundle).toHaveBeenCalledTimes(1);
    rerender({ subtitleUrls: urls });
    expect(loadSubtitleFontBundle).toHaveBeenCalledTimes(1);
  });

  it("prefetches only the effective release's tracks when the list spans releases", () => {
    const urls = [
      track({ index: 1, media_file_id: 10, font_bundle_url: "/fonts/effective" }),
      track({ index: 2, media_file_id: 99, font_bundle_url: "/fonts/other-release" }),
      // A track with no file identity belongs to the effective release.
      track({ index: 3, font_bundle_url: "/fonts/unattributed" }),
    ];
    renderHook(() => useSubtitleFontPrefetch(urls, 10));
    expect(loadSubtitleFontBundle).toHaveBeenCalledTimes(2);
    expect(loadSubtitleFontBundle).toHaveBeenCalledWith("/fonts/effective");
    expect(loadSubtitleFontBundle).toHaveBeenCalledWith("/fonts/unattributed");
    expect(loadSubtitleFontBundle).not.toHaveBeenCalledWith("/fonts/other-release");
  });

  it("still prefetches every track when no effective file id is known", () => {
    const urls = [
      track({ index: 1, media_file_id: 10, font_bundle_url: "/fonts/a" }),
      track({ index: 2, media_file_id: 99, font_bundle_url: "/fonts/b" }),
    ];
    renderHook(() => useSubtitleFontPrefetch(urls, null));
    expect(loadSubtitleFontBundle).toHaveBeenCalledTimes(2);
  });

  it("never prefetches a font bundle on a non-ASS codec", () => {
    const urls = [
      track({ index: 1, codec: "ass", font_bundle_url: "/fonts/ass" }),
      track({ index: 2, codec: "srt", font_bundle_url: "/fonts/srt" }),
      track({ index: 3, codec: "pgs", font_bundle_url: "/fonts/pgs" }),
      track({ index: 4, codec: "subrip", font_bundle_url: "/fonts/subrip" }),
    ];
    renderHook(() => useSubtitleFontPrefetch(urls, undefined));
    expect(loadSubtitleFontBundle).toHaveBeenCalledTimes(1);
    expect(loadSubtitleFontBundle).toHaveBeenCalledWith("/fonts/ass");
  });

  it("swallows a rejected font bundle fetch", async () => {
    const unhandled: unknown[] = [];
    const onUnhandled = (reason: unknown) => {
      unhandled.push(reason);
    };
    process.on("unhandledRejection", onUnhandled);
    try {
      loadSubtitleFontBundle.mockRejectedValueOnce(new Error("font extraction failed"));
      renderHook(() => useSubtitleFontPrefetch([track({ font_bundle_url: "/fonts/a" })]));
      await act(async () => {
        await Promise.resolve();
      });
      expect(loadSubtitleFontBundle).toHaveBeenCalledTimes(1);
      expect(unhandled).toHaveLength(0);
    } finally {
      process.removeListener("unhandledRejection", onUnhandled);
    }
  });
});
