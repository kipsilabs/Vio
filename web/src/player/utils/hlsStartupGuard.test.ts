// @vitest-environment node

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  HLS_STARTUP_TIMEOUT_MS,
  HlsStartupGuard,
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
