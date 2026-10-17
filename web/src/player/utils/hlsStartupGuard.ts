import type { DecodeErrorCarrier } from "../decode-failure";

export const HLS_STARTUP_TIMEOUT_MS = 60_000;

const MAX_FATAL_NETWORK_RECOVERIES = 1;

type StartupState = "starting" | "playable" | "failed" | "disposed";

export class HlsStartupGuard {
  private state: StartupState = "starting";
  private fatalNetworkRecoveries = 0;
  private timeoutId: ReturnType<typeof setTimeout> | null;

  constructor(private readonly onFailure: () => void) {
    this.timeoutId = setTimeout(() => this.fail(), HLS_STARTUP_TIMEOUT_MS);
  }

  handleFatalNetworkError(): boolean {
    if (this.state === "playable") return true;
    if (this.state !== "starting") return false;

    if (this.fatalNetworkRecoveries < MAX_FATAL_NETWORK_RECOVERIES) {
      this.fatalNetworkRecoveries++;
      return true;
    }

    this.fail();
    return false;
  }

  markPlaybackStarted() {
    if (this.state !== "starting") return;

    this.state = "playable";
    this.clearTimeout();
  }

  hasFailed() {
    return this.state === "failed";
  }

  /** True only while initial startup is still in flight (not yet playable). */
  isStarting() {
    return this.state === "starting";
  }

  dispose() {
    this.state = "disposed";
    this.clearTimeout();
  }

  private fail() {
    if (this.state !== "starting") return;

    this.state = "failed";
    this.clearTimeout();
    this.onFailure();
  }

  private clearTimeout() {
    if (this.timeoutId === null) return;

    clearTimeout(this.timeoutId);
    this.timeoutId = null;
  }
}

/**
 * HTTP status the server returns for a segment URL whose opaque `sgen`
 * generation token names a replaced FFmpeg incarnation (the playback segment
 * handler's `stale_generation`). Retrying that exact URL can never succeed: the
 * bytes are fenced to the generation that produced the playlist, and the
 * session has moved on.
 */
export const STALE_GENERATION_STATUS = 412;

/**
 * Cap on consecutive playlist refetches a single transport may perform for the
 * stale-generation fence. Each refetch rebuilds the segment URLs with the
 * current `sgen`; if the server keeps answering 412 the session is genuinely
 * moving faster than the client can adopt it, so the player stops refetching
 * and lets the ordinary failure path replan instead of spinning.
 */
export const MAX_STALE_GENERATION_RECOVERIES = 2;

/**
 * Reports whether an hls.js error is the server's stale-generation fence. The
 * HTTP status is the authority: the segment route uses 412 for this verdict
 * only, and the v2 delivery adapter rewrites the v1 `stale_generation` body
 * code into the generic `precondition_failed` problem type, so the body cannot
 * be relied on.
 */
export function isStaleGenerationError(carrier: DecodeErrorCarrier | null | undefined): boolean {
  if (!carrier) return false;
  const fromResponse = carrier.response?.code;
  if (typeof fromResponse === "number") {
    return fromResponse === STALE_GENERATION_STATUS;
  }
  const details = carrier.networkDetails as { status?: unknown } | null | undefined;
  return details?.status === STALE_GENERATION_STATUS;
}

/**
 * The hls.js surface the stale-generation recovery drives. It is structural so
 * the recovery can be unit-tested without loading hls.js.
 */
export interface StaleGenerationRecoveryHost {
  /** The playlist URL the instance last loaded, or null before any load. */
  readonly url: string | null;
  /** hls.js's merged config; `startPosition` is read when the refetch autostarts. */
  config: { startPosition?: number };
  loadSource(url: string): void;
}

/**
 * Recovers hls.js from the server's stale-generation fence on a segment URL.
 *
 * The failing segment URI carried an `sgen` token minted against an FFmpeg
 * incarnation the server has since replaced. hls.js keeps no record of the
 * playlist's own segment URLs — it stores parsed fragments, not their source
 * text — so the only correct recovery is to invalidate the cached playlist and
 * fetch it again: the fresh manifest rebuilds every segment URL with the
 * current `sgen`. Retrying the dead URL would loop on a URL the server will
 * keep refusing. The refetch resumes at the current element position, and the
 * caller is expected to have bounded the number of refetches so a pathological
 * session still reaches the ordinary failure path.
 *
 * `requestLoad`, when supplied, is how the caller serializes the refetch
 * against any manifest load already in flight (see `ManifestLoadCoalescer`);
 * the refetch then lands after the in-flight load instead of racing it. Without
 * it the refetch calls `host.loadSource` directly, which is what the recovery's
 * unit tests exercise.
 *
 * The resume position is set through the `beforeStart` hook the recovery hands
 * to `requestLoad`, so it lands when the refetch actually starts — immediately,
 * or when a queued refetch replays. Setting it up front would mutate a load
 * that is already in flight, whose autostart position has already been read.
 *
 * Returns true when a recovery was accepted (started now or queued behind an
 * in-flight load), and false when there is no playlist URL to refetch, so the
 * caller can fall through to the ordinary failure recovery instead of stalling.
 */
export function recoverFromStaleGeneration(
  host: StaleGenerationRecoveryHost,
  resumePositionSeconds: number,
  requestLoad?: (url: string, beforeStart: () => void) => void,
): boolean {
  const playlistUrl = host.url;
  if (!playlistUrl) return false;
  const applyResume = () => {
    if (Number.isFinite(resumePositionSeconds) && resumePositionSeconds > 0) {
      host.config.startPosition = resumePositionSeconds;
    }
  };
  if (requestLoad) {
    requestLoad(playlistUrl, applyResume);
  } else {
    applyResume();
    host.loadSource(playlistUrl);
  }
  return true;
}

/**
 * Normalizes a manifest URL to the absolute form hls.js addresses, so the URL
 * on `MANIFEST_LOADING` (the source passed to `loadSource`, possibly relative)
 * and on `MANIFEST_LOADED` (the loader's absolute response URL) compare equal.
 */
function normalizeManifestUrl(url: string): string {
  if (url === "") return url;
  const base = typeof location === "undefined" ? undefined : location.href;
  try {
    return new URL(url, base).href;
  } catch {
    return url;
  }
}

/**
 * Serializes the manifest (re)loads one hls.js transport issues.
 *
 * The player raises a manifest load from three places that overlap: the
 * transport init, the not-ready pollback, and the stale-generation recovery.
 * The coalescer keys off hls.js's own manifest lifecycle: the player calls
 * `begin` on `MANIFEST_LOADING` and `settleFor` on `MANIFEST_LOADED`, and routes
 * every manifest load through `request`. A request raised while one is in
 * flight is remembered and replayed once, after the in-flight load settles, so
 * overlapping requests land in sequence instead of both opening a fetch.
 *
 * This governs the manifest requests the player itself issues; it does not
 * claim to fix level-load concurrency. hls.js's own `loadSource()` calls
 * `stopLoad()` first and its playlist loader ignores a matching ongoing
 * request, so a repeated `loadSource()` for the same URL does not by itself
 * leave a second fetch open. The paired manifest load seen in the playback HAR
 * — one copy showing a long `receive` the server never produced — was
 * attributed to an external interceptor on the session in the PR discussion;
 * that attribution is not established here.
 *
 * A fatal error is different: hls.js's error controller calls `stopLoad()` for
 * every fatal error, destroying the playlist loaders — including any manifest
 * request still open — without emitting `MANIFEST_LOADED` or a manifest-scoped
 * error. A transport that takes over recovery from a fatal error must
 * therefore `abandon` the load hls.js tore down; otherwise the slot stays set
 * with no load open and every later `request` queues behind a load that will
 * never settle.
 */
export class ManifestLoadCoalescer {
  private inFlightUrl: string | null = null;
  private inFlightToken = 0;
  private nextToken = 0;
  private pending: (() => void) | null = null;
  /**
   * Tokens for the loads `begin` adopted, keyed by their normalized URL, so a
   * completion can be matched to the load it finished rather than to whichever
   * load happens to be in flight when the completion arrives.
   *
   * A superseded load's entry is deliberately retained after a newer load takes
   * the slot: if that older load still reports a completion, its URL must stay
   * attributable so the completion is dropped as stale (see `settle`) instead
   * of falling through the redirect settlement in `settleFor` and clearing the
   * newer load. Entries are removed when each URL's completion arrives, when
   * the URL is the in-flight one that settles, or on `abandon`; a load hls.js
   * aborted emits no completion, so its entry can outlive it. That leaves one
   * small entry per distinct manifest URL begun since the last `abandon`, which
   * is bounded in practice — small, and tiny next to the media buffers — so the
   * retention is intentional rather than capped.
   */
  private loadsByUrl = new Map<string, number>();

  constructor(private readonly load: (url: string) => void) {}

  /**
   * Marks the start of an hls.js manifest load (`MANIFEST_LOADING`) and returns
   * a token identifying it, so its completion can be matched to it. An empty
   * URL names no load, so it is ignored (no token) rather than marking the
   * coalescer busy with a load that has no URL to settle against.
   *
   * A `begin` for a URL other than the one the coalescer has in flight is a
   * load the coalescer did not issue (the window-edge refetch). That newer load
   * supersedes any queued recovery, which would otherwise replay after it; drop
   * the queued one and adopt the newer load as in flight.
   */
  begin(url: string): number | undefined {
    if (url === "") return undefined;
    if (this.pending !== null && url !== this.inFlightUrl) {
      this.pending = null;
    }
    this.inFlightUrl = url;
    this.inFlightToken = ++this.nextToken;
    this.loadsByUrl.set(normalizeManifestUrl(url), this.inFlightToken);
    return this.inFlightToken;
  }

  /**
   * Marks the end of an hls.js manifest load (`MANIFEST_LOADED`). Pass the
   * token `begin` returned for that load so a late or duplicate completion for
   * a superseded load cannot clear the slot a newer load now holds; omit it
   * only to settle whatever is currently in flight.
   */
  settle(token?: number): void {
    if (token !== undefined && token !== this.inFlightToken) {
      // A stale completion for a superseded load. Drop its token so it cannot
      // settle again, but leave the newer load's slot alone.
      for (const [url, tracked] of this.loadsByUrl) {
        if (tracked === token) {
          this.loadsByUrl.delete(url);
          break;
        }
      }
      return;
    }

    const pending = this.pending;
    if (this.inFlightUrl === null && pending === null) return;
    if (this.inFlightUrl !== null) {
      this.loadsByUrl.delete(normalizeManifestUrl(this.inFlightUrl));
    }
    this.inFlightUrl = null;
    this.inFlightToken = 0;
    // Clear the queued load before replaying it, so its own completion cannot
    // find it still queued and run it a second time.
    this.pending = null;
    pending?.();
  }

  /**
   * Settles the manifest load whose completion names `url`. The
   * `MANIFEST_LOADED` event carries no per-load id — only the finished
   * manifest's URL — so the URL is the correlation hls.js offers: a completion
   * for a load a newer one superseded names that older load and is dropped,
   * instead of being mistaken for the load now in flight. hls.js reports the
   * loader's absolute response URL, which normalization maps to the same key as
   * the source URL it began under.
   *
   * A completion can also name no recorded load. That is what an HTTP redirect
   * produces: `MANIFEST_LOADING` carries the source URL the load began under,
   * while `MANIFEST_LOADED` carries the loader's response URL, which a redirect
   * replaces. Such a completion is still that load's own and must settle it,
   * or the coalescer stays busy and queues every later request behind a load
   * that already finished; `settleRedirected` decides when it is safe to treat
   * it that way.
   */
  settleFor(url: string): void {
    const token = this.loadsByUrl.get(normalizeManifestUrl(url));
    if (token === undefined) {
      this.settleRedirected();
      return;
    }
    this.settle(token);
  }

  /**
   * Settles the in-flight load when a completion names no tracked URL yet can
   * only describe that load.
   *
   * The guard is what keeps a stale completion from being mistaken for a
   * redirected one. It settles only when `loadsByUrl` holds exactly one entry
   * and that entry is the in-flight load's own begin, so the completion is the
   * next manifest event after that begin. A superseded load keeps its own
   * entry, so a completion that could name it (a redirect of an aborted load)
   * is not treated as this load's; a newer `begin` adds an entry, so a
   * completion that could belong to it is not either. Tokens are unique and
   * increasing, so matching the single entry's token to `inFlightToken` proves
   * the entry came from the begin that adopted the in-flight load. Without the
   * guard, any unattributable completion would settle whichever load is in
   * flight, reopening the cross-URL stale-completion hole `settleFor` closes.
   */
  private settleRedirected(): void {
    if (this.inFlightUrl === null || this.loadsByUrl.size !== 1) return;
    for (const tracked of this.loadsByUrl.values()) {
      if (tracked !== this.inFlightToken) return;
    }
    this.settle(this.inFlightToken);
  }

  /**
   * Releases the in-flight and queued loads without replaying either. A
   * torn-down transport calls this so a destroyed instance cannot hold a queued
   * recovery behind a load that will never settle; the fatal-error handler also
   * calls it because hls.js tears the manifest loader down for every fatal
   * error without a manifest event.
   */
  abandon(): void {
    this.inFlightUrl = null;
    this.inFlightToken = 0;
    this.pending = null;
    this.loadsByUrl.clear();
  }

  /**
   * Issues a manifest load, or queues it behind the one already in flight.
   * `beforeStart`, when supplied, runs when the load actually starts — now, or
   * when a queued load replays — so a recovery's resume position cannot shift a
   * load that is already in flight, and a budget charged there counts every
   * start, including a replayed one. Returns true when the request started
   * immediately, false when it was coalesced into the in-flight load.
   */
  request(url: string, beforeStart?: () => void): boolean {
    const run = () => {
      beforeStart?.();
      this.start(url);
    };
    if (this.inFlightUrl !== null) {
      this.pending = run;
      return false;
    }
    run();
    return true;
  }

  private start(url: string): void {
    this.inFlightUrl = url;
    this.inFlightToken = ++this.nextToken;
    this.load(url);
  }
}
