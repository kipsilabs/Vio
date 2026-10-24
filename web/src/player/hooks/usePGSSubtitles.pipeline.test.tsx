// @vitest-environment jsdom

import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { act, renderHook, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { PlayerSubtitleInfo } from "../types";
import { usePGSSubtitles } from "./usePGSSubtitles";

const fixture = new Uint8Array(
  readFileSync(resolve(process.cwd(), "src/player/testdata/pgs/dam-cav.sup")),
);

class FakeImageData {
  data: Uint8ClampedArray;
  width: number;
  height: number;

  constructor(data: Uint8ClampedArray, width: number, height: number) {
    this.data = data;
    this.width = width;
    this.height = height;
  }
}

class FakeContext {
  drawImage = vi.fn();
  putImageData = vi.fn();
  clearRect = vi.fn();
  setTransform = vi.fn();
  imageSmoothingEnabled = false;
  imageSmoothingQuality = "high";
}

let contexts = new WeakMap<HTMLCanvasElement, FakeContext>();
let rafCallbacks: FrameRequestCallback[] = [];

function contextFor(canvas: HTMLCanvasElement): FakeContext {
  const existing = contexts.get(canvas);
  if (existing) return existing;
  const created = new FakeContext();
  contexts.set(canvas, created);
  return created;
}

function flushFrame() {
  const callback = rafCallbacks.shift();
  callback?.(0);
}

function makeVideo() {
  const video = document.createElement("video");
  Object.defineProperty(video, "readyState", { value: 1, configurable: true });
  Object.defineProperty(video, "currentTime", {
    value: 0,
    writable: true,
    configurable: true,
  });
  Object.defineProperty(video, "videoWidth", { value: 1280, configurable: true });
  Object.defineProperty(video, "videoHeight", { value: 720, configurable: true });
  return video;
}

function makeCanvas() {
  const canvas = document.createElement("canvas");
  Object.defineProperty(canvas, "clientWidth", { value: 1280, configurable: true });
  Object.defineProperty(canvas, "clientHeight", { value: 720, configurable: true });
  return canvas;
}

const pgsTrack: PlayerSubtitleInfo = {
  index: 1,
  language: "eng",
  codec: "pgs",
  label: "English PGS",
  source: "embedded",
  url: "/api/v2/stream/session/subtitles/1.sup?file_id=7",
};

type Props = {
  refreshRevision: number;
};

function renderPGS(fetchMock: ReturnType<typeof vi.fn>, onSourceChanged = vi.fn()) {
  const video = makeVideo();
  const canvas = makeCanvas();
  const onLoadState = vi.fn();
  const base = {
    videoRef: { current: video },
    canvasRef: { current: canvas },
    subtitleUrls: [pgsTrack],
    activeSubtitleIndex: 1,
    enabled: true,
    streamOriginSeconds: 0,
    subtitleDelayMs: 0,
    durationRef: { current: 60 },
    fetchAnchorRef: { current: 0 },
    videoFit: "contain" as const,
    bottomInsetPx: 0,
    onLoadState,
    onSourceChanged,
  };

  const hook = renderHook(
    ({ refreshRevision }: Props) =>
      usePGSSubtitles({
        ...base,
        refreshRevision,
      }),
    { initialProps: { refreshRevision: 0 } },
  );

  return { ...hook, video, canvas, onLoadState, onSourceChanged, fetchMock };
}

beforeEach(() => {
  contexts = new WeakMap();
  rafCallbacks = [];
  vi.stubGlobal("ImageData", FakeImageData);
  vi.stubGlobal("requestAnimationFrame", (callback: FrameRequestCallback) => {
    rafCallbacks.push(callback);
    return rafCallbacks.length;
  });
  vi.stubGlobal("cancelAnimationFrame", vi.fn());
  vi.spyOn(HTMLCanvasElement.prototype, "getContext").mockImplementation(function (
    this: HTMLCanvasElement,
  ) {
    return contextFor(this) as unknown as CanvasRenderingContext2D;
  } as unknown as typeof HTMLCanvasElement.prototype.getContext);
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("PGS pipeline", () => {
  it("fetches a real .sup in a 30-second first window and paints it", async () => {
    const fetchMock = vi.fn(
      async (_url: string, _init?: RequestInit) => new Response(fixture, { status: 200 }),
    );
    vi.stubGlobal("fetch", fetchMock);
    const hook = renderPGS(fetchMock);

    await waitFor(() => expect(hook.onLoadState).toHaveBeenCalledWith("ready"));
    const firstUrl = String(fetchMock.mock.calls[0]![0]);
    expect(firstUrl).toContain("windowed=1");
    expect(firstUrl).toContain("position=0");
    expect(firstUrl).toContain("duration=30");

    act(() => flushFrame());
    expect(contexts.get(hook.canvas)?.drawImage).toHaveBeenCalled();

    hook.unmount();
  });

  it("signals a v2 source-change 409 and refetches after a refresh revision", async () => {
    const fetchMock = vi
      .fn(async (_url: string, _init?: RequestInit) => new Response(fixture, { status: 200 }))
      .mockResolvedValueOnce(new Response(fixture, { status: 200 }))
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            type: "https://siloserver.org/docs/api/v2/problems/conflict",
            title: "Conflict",
            status: 409,
          }),
          { status: 409, headers: { "Content-Type": "application/problem+json" } },
        ),
      )
      .mockResolvedValue(new Response(fixture, { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    const hook = renderPGS(fetchMock);

    await waitFor(() => expect(hook.onLoadState).toHaveBeenCalledWith("ready"));

    hook.video.currentTime = 100;
    act(() => {
      hook.video.dispatchEvent(new Event("seeking"));
    });

    await waitFor(() => expect(hook.onSourceChanged).toHaveBeenCalledTimes(1));
    expect(hook.onLoadState).toHaveBeenCalledWith("error");
    expect(fetchMock).toHaveBeenCalledTimes(2);

    // A replan can re-mint the same URL. The revision must rebuild the
    // fetcher and retry once instead of leaving the overlay on "loading".
    hook.rerender({ refreshRevision: 1 });
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3));
    const retryUrl = String(fetchMock.mock.calls[2]![0]);
    expect(retryUrl).toContain("windowed=1");
    await waitFor(() => expect(hook.onLoadState).toHaveBeenCalledWith("ready"));

    hook.unmount();
  });
});
