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
 * Reports whether an hls.js error ended the manifest load rather than a
 * fragment or media load. hls.js names manifest-scoped failures with a
 * `manifest` prefix (`manifestLoadError`, `manifestLoadTimeOut`,
 * `manifestParsingError`, …) and settles the manifest request either way. A
 * fragment error carries its own `fragLoadError` detail and does not mean a
 * concurrent manifest load finished, so it must not release the coalescer.
 */
export function isManifestLoadError(carrier: { details?: unknown } | null | undefined): boolean {
  return typeof carrier?.details === "string" && carrier.details.startsWith("manifest");
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
 * Serializes the manifest (re)loads one hls.js transport issues.
 *
 * `hls.js`'s `loadSource()` restarts the manifest fetch, and the player can
 * raise it from three places that overlap: the transport init, the not-ready
 * pollback, and the stale-generation recovery. When a request lands while an
 * earlier manifest load is still in flight — a stale-generation fence on a
 * segment is independent of the manifest request hls.js already has open —
 * issuing `loadSource()` again leaves two concurrent requests for the same
 * playlist: the fresh one supersedes the old, and the abandoned request is held
 * to completion. That is the paired manifest load in the playback HAR, where
 * one copy shows a long `receive` the server never produced.
 *
 * The coalescer keys off hls.js's own manifest lifecycle: the player calls
 * `begin` on `MANIFEST_LOADING` and `settle` on `MANIFEST_LOADED` or a
 * manifest-scoped fatal error, and routes every manifest load through
 * `request`. A request raised while one is in flight is remembered and replayed
 * once, after the in-flight load settles, so the recovery still happens without
 * a second concurrent request.
 *
 * An in-flight load is only ever released by hls.js's own manifest events, so a
 * transport that hls.js abandons without those events (a decode replan or an
 * exhausted recovery that destroys it) must call `abandon`; otherwise the slot
 * stays set with no load open and every later `request` queues behind a load
 * that will never settle.
 */
export class ManifestLoadCoalescer {
  private inFlightUrl: string | null = null;
  private pending: (() => void) | null = null;

  constructor(private readonly load: (url: string) => void) {}

  /**
   * Marks the start of an hls.js manifest load (`MANIFEST_LOADING`). An empty
   * URL names no load, so it is ignored rather than marking the coalescer busy
   * with a load that has no URL to settle against.
   *
   * A `begin` for a URL other than the one the coalescer has in flight is a
   * load the coalescer did not issue (the window-edge refetch). That newer load
   * supersedes any queued recovery, which would otherwise replay after it; drop
   * the queued one and adopt the newer load as in flight.
   */
  begin(url: string): void {
    if (url === "") return;
    if (this.pending !== null && url !== this.inFlightUrl) {
      this.pending = null;
    }
    this.inFlightUrl = url;
  }

  /** Marks the end of an hls.js manifest load (`MANIFEST_LOADED` or a fatal manifest error). */
  settle(): void {
    const pending = this.pending;
    if (this.inFlightUrl === null && pending === null) return;
    this.inFlightUrl = null;
    this.pending = null;
    pending?.();
  }

  /**
   * Releases the in-flight and queued loads without replaying either. A
   * torn-down transport calls this so a destroyed instance cannot hold a queued
   * recovery behind a load that will never settle.
   */
  abandon(): void {
    this.inFlightUrl = null;
    this.pending = null;
  }

  /**
   * Issues a manifest load, or queues it behind the one already in flight.
   * `beforeStart`, when supplied, runs when the load actually starts — now, or
   * when a queued load replays — so a recovery's resume position cannot shift a
   * load that is already in flight. Returns true when the request started
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
    this.load(url);
  }
}
