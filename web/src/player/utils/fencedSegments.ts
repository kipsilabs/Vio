import type {
  FragmentLoaderConstructor,
  FragmentLoaderContext,
  HlsConfig,
  Loader,
  LoaderCallbacks,
  LoaderConfiguration,
  LoaderContext,
  LoaderResponse,
  LoaderStats,
} from "hls.js";

import { generationTokenFromSegmentUrl } from "./fencedPlaylist";
import { STALE_GENERATION_STATUS } from "./startupGuard";

/**
 * The exact segment URLs the server has already refused with a 412, and the
 * fence answers the player has already spent.
 *
 * A fence is final for a URL, not for the transport that happened to request
 * it. hls.js retries a failed fragment by issuing the same URL again, and a
 * reanchor that reuses the generation can reload a stopped instance whose
 * playlist still names the dead URL. Both layers would ask the server the same
 * question and get the same 412. Keeping the answer here, above the fragment
 * loader, makes the second request impossible instead of merely ignored.
 *
 * The ledger belongs to the media being played, not to one playback session.
 * A session replacement for the same title and file inherits it, so a URL the
 * old session answered is not requested again by the replacement's transport,
 * and one fence cannot buy a second reanchor. A genuinely different playback
 * (another title, another version, or a new player mount) starts a fresh
 * ledger; see `VideoPlayer`'s media identity.
 */
export interface SegmentFenceLedger {
  /** The server refused this exact URL; it is dead for this playback. */
  refuse(url: string | null | undefined): void;
  /** Whether this exact URL has already been answered with a 412. */
  isFinal(url: string | null | undefined): boolean;
  /**
   * Record that this exact URL's fence has been answered (a playlist refetch,
   * a bounded rebuild, or a reanchor). Returns whether this is the first
   * answer. A queued retry, a stopped instance's reload and the replacement
   * transport of the same media all get false, so one fence cannot spend two
   * rebuilds across a session boundary.
   */
  claimFence(url: string | null | undefined): boolean;
  /**
   * Note the generation an accepted playlist addresses. A change aborts
   * in-flight fragment loads that still name an earlier generation.
   */
  observeGeneration(token: string | null | undefined): void;
  /**
   * Register an in-flight load so a generation change can cancel it. The
   * returned function releases it again.
   */
  track(load: ActiveFragmentLoad): () => void;
  /** The generation named by the most recent accepted playlist, if any. */
  readonly generation: string | null;
  /** Forget every fence for a genuinely different playback. */
  reset(): void;
}

interface ActiveFragmentLoad {
  readonly url: string;
  abort(): void;
}

export function createSegmentFenceLedger(): SegmentFenceLedger {
  const finalUrls = new Set<string>();
  const answeredFences = new Set<string>();
  const active = new Set<ActiveFragmentLoad>();
  let generation: string | null = null;

  return {
    refuse(url) {
      if (!url) return;
      finalUrls.add(url);
    },
    isFinal(url) {
      return url !== null && url !== undefined && finalUrls.has(url);
    },
    claimFence(url) {
      if (!url) return true;
      if (answeredFences.has(url)) return false;
      answeredFences.add(url);
      return true;
    },
    observeGeneration(token) {
      if (!token || token === generation) return;
      generation = token;
      // A request for the new generation is the proof the previous one is
      // gone. Cancel anything still out under an older token so a slow response
      // cannot later be appended as if it belonged to the live window.
      for (const load of [...active]) {
        const loadToken = generationTokenFromSegmentUrl(load.url);
        if (loadToken === null || loadToken === token) continue;
        active.delete(load);
        load.abort();
      }
    },
    track(load) {
      active.add(load);
      return () => {
        active.delete(load);
      };
    },
    get generation() {
      return generation;
    },
    reset() {
      finalUrls.clear();
      answeredFences.clear();
      active.clear();
      generation = null;
    },
  };
}

/** A loader error names the fence either directly or on its network details. */
function isRefusedFragmentResponse(
  error: { code?: number } | null | undefined,
  networkDetails: unknown,
): boolean {
  if (error?.code === STALE_GENERATION_STATUS) return true;
  const status = (networkDetails as { status?: unknown } | null | undefined)?.status;
  return status === STALE_GENERATION_STATUS;
}

/**
 * Builds the fragment loader class for one transport.
 *
 * The wrapper delegates authentication, ranges, redirects and abort semantics
 * to hls.js's own loader, and adds the fence ledger above it:
 *
 * - a URL already answered with 412 is never requested again; the caller gets
 *   the same fence answer locally, so hls.js settles instead of hanging;
 * - the exact URL is recorded as final when the base loader reports 412;
 * - a request naming a newer generation aborts in-flight loads that still name
 *   an older one, shared across the transports of one player.
 *
 * hls.js creates a fragment loader per attempt, so the ledger is what spans
 * attempts and transports. The constructor mirrors hls.js's own fragment
 * loader signature.
 */
export function createFencedFragmentLoader(
  BaseLoader: new (config: HlsConfig) => Loader<LoaderContext>,
  ledger: SegmentFenceLedger,
): FragmentLoaderConstructor {
  return class FencedFragmentLoader implements Loader<FragmentLoaderContext> {
    private readonly inner: Loader<LoaderContext>;
    private currentContext: FragmentLoaderContext | null = null;
    private active: ActiveFragmentLoad | null = null;
    private untrack: (() => void) | null = null;

    constructor(config: HlsConfig) {
      this.inner = new BaseLoader(config);
    }

    get context(): FragmentLoaderContext | null {
      return this.currentContext;
    }

    get stats(): LoaderStats {
      return this.inner.stats;
    }

    getCacheAge = () => this.inner.getCacheAge?.() ?? null;
    getResponseHeader = (name: string) => this.inner.getResponseHeader?.(name) ?? null;

    destroy() {
      this.cancelActive();
      this.inner.destroy();
    }

    abort() {
      this.cancelActive();
      this.inner.abort();
    }

    load(
      context: FragmentLoaderContext,
      config: LoaderConfiguration,
      callbacks: LoaderCallbacks<FragmentLoaderContext>,
    ) {
      this.cancelActive();
      this.currentContext = context;
      const url = context.url;

      if (ledger.isFinal(url)) {
        // The server has already answered this exact URL. Do not ask again;
        // answer the fence locally so hls.js can run its bounded recovery.
        this.currentContext = null;
        const stats = this.inner.stats;
        queueMicrotask(() => {
          callbacks.onError(
            { code: STALE_GENERATION_STATUS, text: "segment generation is stale" },
            context,
            null,
            stats,
          );
        });
        return;
      }

      const active: ActiveFragmentLoad = {
        url,
        abort: () => {
          this.cancelActive();
          this.inner.abort();
        },
      };
      this.active = active;
      this.untrack = ledger.track(active);

      this.inner.load(context, config, {
        onSuccess: (
          response: LoaderResponse,
          stats: LoaderStats,
          loadedContext: LoaderContext,
          networkDetails: unknown,
        ) => {
          this.release(active);
          this.currentContext = null;
          callbacks.onSuccess(
            response,
            stats,
            loadedContext as FragmentLoaderContext,
            networkDetails,
          );
        },
        onError: (
          error: { code: number; text: string },
          errorContext: LoaderContext,
          networkDetails: unknown,
          stats: LoaderStats,
        ) => {
          if (isRefusedFragmentResponse(error, networkDetails)) ledger.refuse(url);
          this.release(active);
          this.currentContext = null;
          callbacks.onError(error, errorContext as FragmentLoaderContext, networkDetails, stats);
        },
        onTimeout: (stats: LoaderStats, timeoutContext: LoaderContext, networkDetails: unknown) => {
          this.release(active);
          this.currentContext = null;
          callbacks.onTimeout(stats, timeoutContext as FragmentLoaderContext, networkDetails);
        },
        ...(callbacks.onAbort
          ? {
              onAbort: (
                stats: LoaderStats,
                abortContext: LoaderContext,
                networkDetails: unknown,
              ) => {
                this.release(active);
                this.currentContext = null;
                callbacks.onAbort?.(stats, abortContext as FragmentLoaderContext, networkDetails);
              },
            }
          : {}),
        ...(callbacks.onProgress
          ? {
              onProgress: (
                stats: LoaderStats,
                progressContext: LoaderContext,
                data: string | ArrayBuffer,
                networkDetails: unknown,
              ) => {
                callbacks.onProgress?.(
                  stats,
                  progressContext as FragmentLoaderContext,
                  data,
                  networkDetails,
                );
              },
            }
          : {}),
      });
    }

    private release(load: ActiveFragmentLoad) {
      if (this.active !== load) return;
      this.untrack?.();
      this.untrack = null;
      this.active = null;
    }

    private cancelActive() {
      const active = this.active;
      if (active === null) return;
      this.untrack?.();
      this.untrack = null;
      this.active = null;
    }
  };
}
