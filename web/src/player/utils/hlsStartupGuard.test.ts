// @vitest-environment node

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  HLS_STARTUP_TIMEOUT_MS,
  HlsStartupGuard,
  ManifestLoadCoalescer,
  isManifestLoadError,
  isStaleGenerationError,
  recoverFromStaleGeneration,
} from "./hlsStartupGuard";

describe("HlsStartupGuard", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("fails startup when no playable frame arrives before the deadline", () => {
    const onFailure = vi.fn();
    const guard = new HlsStartupGuard(onFailure);

    vi.advanceTimersByTime(HLS_STARTUP_TIMEOUT_MS - 1);
    expect(onFailure).not.toHaveBeenCalled();
    expect(guard.hasFailed()).toBe(false);

    vi.advanceTimersByTime(1);
    expect(onFailure).toHaveBeenCalledOnce();
    expect(guard.hasFailed()).toBe(true);
    guard.dispose();
  });

  it("disarms startup limits after playable media arrives", () => {
    const onFailure = vi.fn();
    const guard = new HlsStartupGuard(onFailure);

    guard.markPlaybackStarted();
    vi.advanceTimersByTime(HLS_STARTUP_TIMEOUT_MS);

    expect(onFailure).not.toHaveBeenCalled();
    expect(guard.hasFailed()).toBe(false);
    expect(guard.handleFatalNetworkError()).toBe(true);
    expect(guard.handleFatalNetworkError()).toBe(true);
  });

  it("does not fail after disposal", () => {
    const onFailure = vi.fn();
    const guard = new HlsStartupGuard(onFailure);

    guard.dispose();
    vi.advanceTimersByTime(HLS_STARTUP_TIMEOUT_MS);

    expect(onFailure).not.toHaveBeenCalled();
  });
});

describe("isStaleGenerationError", () => {
  it("detects the server's 412 stale-generation fence on a fetch response", () => {
    expect(isStaleGenerationError({ response: { code: 412 } })).toBe(true);
  });

  it("detects the fence from the xhr network detail when the loader response omits it", () => {
    expect(isStaleGenerationError({ networkDetails: { status: 412 } })).toBe(true);
  });

  it("does not treat another status as the fence", () => {
    for (const code of [200, 404, 422, 500, 503]) {
      expect(isStaleGenerationError({ response: { code } })).toBe(false);
    }
    expect(isStaleGenerationError({ networkDetails: { status: 404 } })).toBe(false);
  });

  it("ignores an unreadable or absent carrier", () => {
    expect(isStaleGenerationError(null)).toBe(false);
    expect(isStaleGenerationError(undefined)).toBe(false);
    expect(isStaleGenerationError({})).toBe(false);
  });
});

describe("recoverFromStaleGeneration", () => {
  it("refetches the cached playlist instead of retrying the dead segment URL", () => {
    const host = {
      url: "/api/v2/playback/transcode/session-1/master.m3u8?st=jwt",
      config: {} as { startPosition?: number },
      loadSource: vi.fn(),
    };

    expect(recoverFromStaleGeneration(host, 42.5)).toBe(true);
    expect(host.loadSource).toHaveBeenCalledOnce();
    // The same playlist URL is reloaded, so the server re-mints every segment
    // URI with the current sgen; the dead URL is never re-requested directly.
    expect(host.loadSource).toHaveBeenCalledWith(host.url);
    // The refetch resumes where the viewer is, rather than jumping to the
    // generation origin.
    expect(host.config.startPosition).toBe(42.5);
  });

  it("omits the resume position when the playhead is not usable", () => {
    const host = {
      url: "/master.m3u8",
      config: {} as { startPosition?: number },
      loadSource: vi.fn(),
    };

    expect(recoverFromStaleGeneration(host, 0)).toBe(true);
    expect(host.config.startPosition).toBeUndefined();
    expect(host.loadSource).toHaveBeenCalledWith("/master.m3u8");
  });

  it("reports false when no playlist URL was loaded", () => {
    const host = {
      url: null,
      config: {} as { startPosition?: number },
      loadSource: vi.fn(),
    };

    expect(recoverFromStaleGeneration(host, 12)).toBe(false);
    expect(host.loadSource).not.toHaveBeenCalled();
  });
});

describe("isManifestLoadError", () => {
  it("recognizes hls.js's manifest-scoped failures", () => {
    for (const details of ["manifestLoadError", "manifestLoadTimeOut", "manifestParsingError"]) {
      expect(isManifestLoadError({ details })).toBe(true);
    }
  });

  it("leaves a fragment error holding the in-flight manifest load", () => {
    expect(isManifestLoadError({ details: "fragLoadError" })).toBe(false);
    expect(isManifestLoadError({ details: "bufferAppendError" })).toBe(false);
    expect(isManifestLoadError(null)).toBe(false);
    expect(isManifestLoadError({})).toBe(false);
  });
});

describe("ManifestLoadCoalescer", () => {
  it("does not open a second concurrent manifest load while one is in flight", () => {
    const requests: string[] = [];
    const coalescer = new ManifestLoadCoalescer((url) => requests.push(url));

    // hls.js marks the transport's own manifest load in flight through its
    // MANIFEST_LOADING event; the player's request during it is queued.
    coalescer.begin("/master.m3u8");
    expect(coalescer.request("/master.m3u8")).toBe(false);
    expect(requests).toEqual([]);

    // The in-flight load settles, and the queued refetch runs exactly once.
    coalescer.settle();
    expect(requests).toEqual(["/master.m3u8"]);
  });

  it("coalesces a stale-generation refetch behind the manifest load already open", () => {
    const requests: string[] = [];
    const coalescer = new ManifestLoadCoalescer((url) => requests.push(url));
    const host = {
      url: "/api/v2/playback/transcode/session-1/master.m3u8?st=jwt",
      config: {} as { startPosition?: number },
      loadSource: vi.fn(),
    };

    // The transport started its manifest load and hls.js is still fetching it.
    coalescer.request(host.url);
    expect(requests).toEqual([host.url]);

    // A stale-generation fence arrives while it is in flight. The recovery
    // records the refetch instead of issuing a second concurrent request.
    expect(recoverFromStaleGeneration(host, 42.5, (url) => coalescer.request(url))).toBe(true);
    expect(requests).toEqual([host.url]);
    // The direct loadSource path is bypassed in favour of the coalescer.
    expect(host.loadSource).not.toHaveBeenCalled();

    // Releasing the in-flight load replays the refetch once.
    coalescer.settle();
    expect(requests).toEqual([host.url, host.url]);
    // It still resumes where the viewer is.
    expect(host.config.startPosition).toBe(42.5);
  });

  it("runs one queued refetch after a burst of stale fences", () => {
    const requests: string[] = [];
    const coalescer = new ManifestLoadCoalescer((url) => requests.push(url));

    coalescer.begin("/master.m3u8");
    coalescer.request("/master.m3u8");
    coalescer.request("/master.m3u8");
    coalescer.request("/master.m3u8");
    expect(requests).toEqual([]);

    coalescer.settle();
    // The in-flight load is the freshest request; only one refetch follows it.
    expect(requests).toEqual(["/master.m3u8"]);
  });
});
