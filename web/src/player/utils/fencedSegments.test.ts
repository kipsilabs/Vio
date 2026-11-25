import { describe, expect, it, vi } from "vitest";

import {
  createFencedFragmentLoader,
  createSegmentFenceLedger,
  type SegmentFenceLedger,
} from "./fencedSegments";

const stats = () => ({
  aborted: false,
  loaded: 0,
  retry: 0,
  total: 0,
  chunkCount: 0,
  bwEstimate: 0,
  loading: { start: 0, first: 0, end: 0 },
  parsing: { start: 0, end: 0 },
  buffering: { start: 0, first: 0, end: 0 },
});

type Callbacks = {
  onSuccess: (response: unknown, stats: unknown, context: unknown, networkDetails: unknown) => void;
  onError: (
    error: { code: number; text: string },
    context: unknown,
    networkDetails: unknown,
    stats: unknown,
  ) => void;
  onTimeout: (stats: unknown, context: unknown, networkDetails: unknown) => void;
  onAbort?: (stats: unknown, context: unknown, networkDetails: unknown) => void;
};

const segmentUrl = (name: string, generation: string) =>
  `/playback/transcode/session-1/${name}?sgen=${encodeURIComponent(generation)}`;

const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

function harness(
  respond: (callbacks: Callbacks, context: { url: string }) => void = (callbacks, context) =>
    queueMicrotask(() =>
      callbacks.onError(
        { code: 412, text: "Precondition Failed" },
        context,
        { status: 412 },
        stats(),
      ),
    ),
) {
  const requests: string[] = [];
  const aborted: string[] = [];

  class BaseLoader {
    stats = stats();
    context = null;
    private current: { callbacks: Callbacks; context: { url: string } } | null = null;
    load(context: { url: string }, _config: unknown, callbacks: Callbacks) {
      requests.push(context.url);
      this.current = { callbacks, context };
      respond(callbacks, context);
    }
    abort() {
      const current = this.current;
      if (!current) return;
      this.current = null;
      aborted.push(current.context.url);
      current.callbacks.onAbort?.(stats(), current.context, null);
    }
    destroy() {}
    getCacheAge() {
      return null;
    }
    getResponseHeader() {
      return null;
    }
  }

  const ledger: SegmentFenceLedger = createSegmentFenceLedger();
  const Loader = createFencedFragmentLoader(BaseLoader as never, ledger);
  const loader = () => new Loader({} as never);
  return { ledger, loader, requests, aborted };
}

function callbacks() {
  return {
    onSuccess: vi.fn(),
    onError: vi.fn(),
    onTimeout: vi.fn(),
    onAbort: vi.fn(),
  };
}

describe("createSegmentFenceLedger", () => {
  it("forgets every fence when the playback changes", () => {
    const ledger = createSegmentFenceLedger();
    const url = segmentUrl("seg_00584.ts", "session-1:inc:1");
    ledger.refuse(url);
    expect(ledger.claimFence(url)).toBe(true);
    expect(ledger.isFinal(url)).toBe(true);

    ledger.reset();
    expect(ledger.isFinal(url)).toBe(false);
    expect(ledger.claimFence(url)).toBe(true);
  });

  it("answers one fence once, even when a replacement transport asks again", () => {
    const ledger = createSegmentFenceLedger();
    const refused = segmentUrl("seg_00584.ts", "session-1:inc:1");
    const other = segmentUrl("seg_00585.ts", "session-1:inc:1");

    expect(ledger.claimFence(refused)).toBe(true);
    // hls.js's queued retry, or the session replacement's transport, names the
    // same URL. It already spent its answer before the session changed.
    expect(ledger.claimFence(refused)).toBe(false);
    // A different segment is a different fence, even under the same sgen.
    expect(ledger.claimFence(other)).toBe(true);
  });
});

describe("createFencedFragmentLoader", () => {
  it("never issues a segment URL the server already refused", async () => {
    const run = harness();
    const url = segmentUrl("seg_00584.ts", "session-1:inc:1");

    const first = run.loader();
    const firstCallbacks = callbacks();
    first.load({ url, responseType: "arraybuffer" } as never, {} as never, firstCallbacks);
    await flush();

    expect(run.requests).toEqual([url]);
    expect(firstCallbacks.onError).toHaveBeenCalledOnce();
    expect(run.ledger.isFinal(url)).toBe(true);

    // hls.js's queued retry asks through a new loader instance, exactly as the
    // replacement session's transport does after a session swap. The ledger is
    // keyed by the media, so the new transport inherits it; the exact URL is
    // dead and no second request crosses the boundary.
    const retry = run.loader();
    const retryCallbacks = callbacks();
    retry.load({ url, responseType: "arraybuffer" } as never, {} as never, retryCallbacks);
    await flush();

    expect(run.requests).toEqual([url]);
    expect(retryCallbacks.onError).toHaveBeenCalledOnce();
    expect(retryCallbacks.onError.mock.calls[0]?.[0]).toMatchObject({ code: 412 });
  });

  it("still requests a different segment under the same generation", async () => {
    const run = harness();
    const refused = segmentUrl("seg_00584.ts", "session-1:inc:1");
    const other = segmentUrl("seg_00585.ts", "session-1:inc:1");

    const first = run.loader();
    first.load({ url: refused, responseType: "arraybuffer" } as never, {} as never, callbacks());
    await flush();

    const second = run.loader();
    second.load({ url: other, responseType: "arraybuffer" } as never, {} as never, callbacks());
    await flush();

    expect(run.requests).toEqual([refused, other]);
  });

  it("cancels an in-flight request when the generation changes", async () => {
    const run = harness(() => {
      // Leave the first request pending: it is the one a newer generation must
      // cancel before its response can be appended to the live window.
    });
    const oldUrl = segmentUrl("seg_00584.ts", "session-1:inc:1");
    const newUrl = segmentUrl("seg_00584.ts", "session-1:inc:2");

    const old = run.loader();
    const oldCallbacks = callbacks();
    old.load({ url: oldUrl, responseType: "arraybuffer" } as never, {} as never, oldCallbacks);
    expect(run.requests).toEqual([oldUrl]);

    // The gate accepted a playlist naming the rebuilt generation; hls.js has
    // not asked for its first fragment yet. The old load is already queued.
    run.ledger.observeGeneration("session-1:inc:2");
    const next = run.loader();
    const nextCallbacks = callbacks();
    next.load({ url: newUrl, responseType: "arraybuffer" } as never, {} as never, nextCallbacks);

    expect(run.requests).toEqual([oldUrl, newUrl]);
    expect(run.aborted).toEqual([oldUrl]);
    expect(oldCallbacks.onAbort).toHaveBeenCalledOnce();
    expect(nextCallbacks.onAbort).not.toHaveBeenCalled();
  });

  it("does not cancel requests that name the same generation", () => {
    const run = harness(() => {
      // Both loads stay pending.
    });
    const firstUrl = segmentUrl("seg_00584.ts", "session-1:inc:2");
    const secondUrl = segmentUrl("seg_00585.ts", "session-1:inc:2");
    run.ledger.observeGeneration("session-1:inc:2");

    const first = run.loader();
    const firstCallbacks = callbacks();
    first.load(
      { url: firstUrl, responseType: "arraybuffer" } as never,
      {} as never,
      firstCallbacks,
    );

    const second = run.loader();
    second.load({ url: secondUrl, responseType: "arraybuffer" } as never, {} as never, callbacks());

    expect(run.aborted).toEqual([]);
    expect(firstCallbacks.onAbort).not.toHaveBeenCalled();
  });
});
