// @vitest-environment node

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  HLS_STARTUP_TIMEOUT_MS,
  HlsStartupGuard,
  ManifestLoadCoalescer,
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
    expect(
      recoverFromStaleGeneration(host, 42.5, (url, applyResume) =>
        coalescer.request(url, applyResume),
      ),
    ).toBe(true);
    expect(requests).toEqual([host.url]);
    // The direct loadSource path is bypassed in favour of the coalescer.
    expect(host.loadSource).not.toHaveBeenCalled();
    // The resume position is not applied yet: mutating it now would shift the
    // manifest load already in flight.
    expect(host.config.startPosition).toBeUndefined();

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

  it("ignores an empty MANIFEST_LOADING URL instead of marking itself busy", () => {
    const requests: string[] = [];
    const coalescer = new ManifestLoadCoalescer((url) => requests.push(url));

    // hls.js can raise MANIFEST_LOADING before it exposes the source URL. An
    // empty URL names no load to settle, so it must not block a later request.
    coalescer.begin("");
    expect(coalescer.request("/master.m3u8")).toBe(true);
    expect(requests).toEqual(["/master.m3u8"]);
  });

  it("runs the queued load's beforeStart hook only when it replays", () => {
    const order: string[] = [];
    const coalescer = new ManifestLoadCoalescer((url) => order.push(`load:${url}`));

    coalescer.begin("/master.m3u8");
    coalescer.request("/refetch.m3u8", () => order.push("resume"));
    // The hook waits for the load: applying it now would shift the load already
    // in flight.
    expect(order).toEqual([]);

    coalescer.settle();
    expect(order).toEqual(["resume", "load:/refetch.m3u8"]);
  });

  it("releases a queued recovery when a fatal error abandons the load", () => {
    const requests: string[] = [];
    const coalescer = new ManifestLoadCoalescer((url) => requests.push(url));

    // A manifest load is held and a recovery queues behind it.
    coalescer.begin("/master.m3u8");
    expect(coalescer.request("/refetch.m3u8")).toBe(false);
    expect(requests).toEqual([]);

    // A fatal non-manifest error tears the load down without a manifest event,
    // so the transport abandons it. A later request must start, not queue
    // behind a load that will never settle.
    coalescer.abandon();
    expect(coalescer.request("/recovered.m3u8")).toBe(true);
    expect(requests).toEqual(["/recovered.m3u8"]);
  });

  it("drops a queued recovery when an out-of-band load supersedes it", () => {
    const requests: string[] = [];
    const coalescer = new ManifestLoadCoalescer((url) => requests.push(url));

    // A manifest load is held and a stale-generation refetch queues behind it.
    coalescer.begin("/initial.m3u8");
    expect(coalescer.request("/recovery.m3u8")).toBe(false);

    // The window-edge refetch issues its own loadSource; hls.js raises
    // MANIFEST_LOADING for that newer URL. It supersedes the queued recovery,
    // which must not replay after it.
    coalescer.begin("/edge.m3u8");
    coalescer.settle();
    expect(requests).toEqual([]);
  });

  it("replays a queued refetch exactly once across duplicate MANIFEST_LOADED events", () => {
    const requests: string[] = [];
    const coalescer = new ManifestLoadCoalescer((url) => requests.push(url));

    const token = coalescer.begin("/master.m3u8");
    coalescer.request("/refetch.m3u8");
    coalescer.settle(token);
    // A duplicate completion for the superseded load is dropped; it must not
    // replay the refetch a second time or clear the load it started.
    coalescer.settle(token);
    expect(requests).toEqual(["/refetch.m3u8"]);

    // A request raised after the duplicate still queues behind the replayed
    // load instead of starting a second concurrent one.
    expect(coalescer.request("/late.m3u8")).toBe(false);
    expect(requests).toEqual(["/refetch.m3u8"]);
  });

  it("cannot clear the active replacement when a superseded load settles again", () => {
    const requests: string[] = [];
    const coalescer = new ManifestLoadCoalescer((url) => requests.push(url));

    // A is in flight and B queues behind it.
    const tokenA = coalescer.begin("/master.m3u8");
    expect(coalescer.request("/refetch.m3u8")).toBe(false);
    expect(requests).toEqual([]);

    // A completes: B starts and holds the slot.
    coalescer.settle(tokenA);
    expect(requests).toEqual(["/refetch.m3u8"]);

    // A second completion for A carries a stale token, so it must not clear
    // B's slot and let a later request run concurrently with it.
    coalescer.settle(tokenA);
    expect(coalescer.request("/later.m3u8")).toBe(false);
    expect(requests).toEqual(["/refetch.m3u8"]);

    // B settling runs the queued request exactly once.
    coalescer.settle();
    expect(requests).toEqual(["/refetch.m3u8", "/later.m3u8"]);
  });

  it("does not replay a load once it has settled", () => {
    const requests: string[] = [];
    const coalescer = new ManifestLoadCoalescer((url) => requests.push(url));

    // A is in flight and B queues behind it.
    const tokenA = coalescer.begin("/a.m3u8");
    expect(coalescer.request("/b.m3u8")).toBe(false);
    expect(requests).toEqual([]);

    // A settles: B starts and takes the slot.
    coalescer.settle(tokenA);
    expect(requests).toEqual(["/b.m3u8"]);

    // B settles. Nothing was queued behind it, so it must not replay itself.
    coalescer.settle();
    expect(requests).toEqual(["/b.m3u8"]);

    // The coalescer is idle, so a later request starts a fresh load instead of
    // replaying B.
    expect(coalescer.request("/c.m3u8")).toBe(true);
    expect(requests).toEqual(["/b.m3u8", "/c.m3u8"]);
  });

  it("settles the load the completing URL names, not a superseded one", () => {
    const requests: string[] = [];
    const coalescer = new ManifestLoadCoalescer((url) => requests.push(url));

    // A is in flight; a newer load for a different URL supersedes it, and a
    // recovery queues behind the newer load.
    const tokenA = coalescer.begin("/master.m3u8");
    const tokenB = coalescer.begin("/edge.m3u8");
    expect(tokenB).not.toBe(tokenA);
    expect(coalescer.request("/queued.m3u8")).toBe(false);

    // A completion naming the superseded load must not clear the newer slot,
    // even though that load's token no longer matches the in-flight token.
    coalescer.settleFor("/master.m3u8");
    expect(requests).toEqual([]);

    // The newer load's own completion settles it and runs the queued request.
    coalescer.settleFor("/edge.m3u8");
    expect(requests).toEqual(["/queued.m3u8"]);
  });

  it("ignores an unattributable completion while a superseded load is tracked", () => {
    const requests: string[] = [];
    const coalescer = new ManifestLoadCoalescer((url) => requests.push(url));

    // Two loads are tracked, so the newer one is in flight. A completion that
    // names neither cannot be attributed to the in-flight load — it could be a
    // redirect of the superseded load — so it must not settle the newer slot.
    coalescer.begin("/master.m3u8");
    coalescer.begin("/edge.m3u8");
    expect(coalescer.request("/refetch.m3u8")).toBe(false);

    coalescer.settleFor("/redirected.m3u8");
    expect(requests).toEqual([]);

    // The in-flight load's own completion still settles it and runs the queued
    // request.
    coalescer.settleFor("/edge.m3u8");
    expect(requests).toEqual(["/refetch.m3u8"]);
  });

  it("settles a redirected manifest so the next request starts instead of queueing", () => {
    const requests: string[] = [];
    const coalescer = new ManifestLoadCoalescer((url) => requests.push(url));

    // The load begins under the source URL but hls.js follows an HTTP redirect
    // and reports the loader's response URL on completion.
    coalescer.begin("/source.m3u8");
    coalescer.settleFor("/redirected.m3u8");

    // The redirect completion settled the load it actually finished, so the
    // coalescer is idle: the next request starts instead of queueing forever
    // behind a load that already completed.
    expect(coalescer.request("/next.m3u8")).toBe(true);
    expect(requests).toEqual(["/next.m3u8"]);
  });

  it("replays a queued refetch when a redirect renames the completed manifest", () => {
    const requests: string[] = [];
    const coalescer = new ManifestLoadCoalescer((url) => requests.push(url));

    coalescer.begin("/source.m3u8");
    expect(coalescer.request("/refetch.m3u8")).toBe(false);
    expect(requests).toEqual([]);

    // The completion names the redirect target, not the source the load began
    // under; it still settles that load and runs the queued refetch once.
    coalescer.settleFor("/redirected.m3u8");
    expect(requests).toEqual(["/refetch.m3u8"]);
  });

  it("charges a queued refetch when it replays, so queue/settle cycles count each start", () => {
    const requests: string[] = [];
    let recoveries = 0;
    const coalescer = new ManifestLoadCoalescer((url) => requests.push(url));
    const refetch = () =>
      coalescer.request("/master.m3u8", () => {
        recoveries++;
      });

    // Each cycle queues a refetch behind an in-flight load and settles it: the
    // charge hook runs when the replay actually starts, not when it queues.
    for (let cycle = 0; cycle < 3; cycle++) {
      coalescer.begin("/master.m3u8");
      expect(refetch()).toBe(false);
      expect(recoveries).toBe(cycle);
      coalescer.settle();
    }

    expect(recoveries).toBe(3);
    expect(requests).toEqual(["/master.m3u8", "/master.m3u8", "/master.m3u8"]);
  });
});
