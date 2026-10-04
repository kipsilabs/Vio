import { useEffect, useRef } from "react";
import type JASSUB from "jassub";
import type { PlayerSubtitleInfo, VideoFitMode } from "../types";
import { isASSCodec } from "../utils/subtitleCodecs";
import { isSubtitleSourceChanged } from "../utils/subtitleSourceChanged";
import {
  applyASSMarginInset,
  coverZoom,
  NO_ASS_MARGIN_INSET,
  NO_COVER_CROP,
  resolveASSMarginInset,
  sameASSMarginInset,
  type ASSMarginInset,
  type CoverCrop,
} from "../utils/assFillMargins";
import {
  fallbackFontForSubtitle,
  forceASSFontFamily,
  loadSubtitleFontBundleResult,
  loadSubtitleFallbackFontData,
  type SubtitleFontBundleResult,
} from "../utils/subtitleFonts";
// Liberation Sans (SIL OFL 1.1; license colocated as liberation-sans.LICENSE),
// the font JASSUB uses as its built-in Latin default, taken verbatim from
// jassub@2.4.2's dist/default.woff2. Vendored because jassub >= 2.5.4 still
// references that file but no longer ships it in the npm package, which would
// leave libass with no usable default font (queryFonts is disabled) and
// silently render nothing.
import liberationSansUrl from "../assets/liberation-sans.woff2?url";

// A font bundle extraction (up to 32MiB, server-side, over network-backed or
// virtual storage) is allowed this much time before JASSUB is constructed
// without it. Subtitles render with the fallback/default fonts meanwhile; the
// losing fetch keeps running in the background and lands in the shared
// fontBundleCache, so a later re-select or the prefetch path picks up the real
// fonts. The subtitle TEXT is never gated by this budget.
const FONT_BUNDLE_BUDGET_MS = 3000;

// Explicitly bound each ASS fetch to this many source-time seconds, matching
// the VTT window (`useSubtitleTracks`). The server extracts the whole track
// otherwise — ~100s for an embedded ASS track over a relay — long enough to
// trip the stall watchdog and loop. Windowed extraction is seconds.
const ASS_WINDOW_DURATION_SECONDS = 600;
// Start a window this many seconds before the playhead so the cue currently
// on screen is inside the requested interval rather than cut off at its start.
const ASS_WINDOW_LEAD_SECONDS = 20;
// Fetch the next window once playback comes this close to the current one's
// end. The replacement window restarts `ASS_WINDOW_LEAD_SECONDS` behind the
// playhead, so it overlaps the outgoing window and the swap drops no cues.
const ASS_WINDOW_PREFETCH_LEAD_SECONDS = 60;
// A windowed extraction is seconds of work, not a whole-track 100s: allow this
// long without a chunk before abandoning the attempt. Covers a cold ffmpeg
// start plus a slow-but-progressing extraction.
const ASS_STALL_TIMEOUT_MS = 60_000;
// Consecutive windowed failures tolerated before falling back to the
// param-less whole-track URL once.
const ASS_WINDOW_MAX_ATTEMPTS = 3;
// Retry backoff after a failed windowed attempt.
const ASS_RETRY_BACKOFF_MS = 5_000;
// Hard ceiling on consecutive subtitle TEXT fetch failures for one mount,
// counting the windowed attempts and the whole-track fallback together. The
// per-position windowed budget resets on a seek so a new position gets a fair
// try, which lets a release that errors every request be re-fetched forever as
// playback re-anchors; this counter does not reset on a seek. Once it is spent
// the pipeline latches terminal.
const ASS_TEXT_MAX_FAILURES = 4;
// Generous ceiling for explicitly-retryable TEXT failures (502/503/504/429/
// 408) on the same backoff loop. A transient upstream flap must not spend the
// 4-deep terminal budget above, but a source that never recovers still
// terminates instead of polling forever.
const ASS_RETRYABLE_TEXT_MAX_FAILURES = 12;

/** True for statuses the server marks retry-after-backoff rather than failed. */
function isRetryableSubtitleStatus(status: number): boolean {
  return status === 408 || status === 429 || status === 502 || status === 503 || status === 504;
}

/** A non-OK subtitle response, carrying the status so the retry policy can
 * distinguish a definitive 4xx from a possibly-transient failure. */
class SubtitleFetchError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
    this.name = "SubtitleFetchError";
  }
}

/** True when a status cannot be satisfied by retrying the same URL. */
function isDefinitiveSubtitleStatus(status: number): boolean {
  // 409 is resolved by a replan, not a retry, and is handled before this.
  return status >= 400 && status < 500 && status !== 409;
}
// Bounded background refresh of a font bundle the server reported as pending
// (empty bundle, extraction still running). It stops as soon as fonts arrive
// or a definitive font-less bundle is reported, so it can never poll forever.
const ASS_FONT_REFRESH_MAX_ATTEMPTS = 3;
// Backoff between pending-font refresh attempts.
const ASS_FONT_REFRESH_BACKOFF_MS = 5_000;

/** `url` with the bounded window query the VTT path also uses. */
function appendSubtitleWindow(url: string, position: number): string {
  const sep = url.includes("?") ? "&" : "?";
  return `${url}${sep}position=${Math.max(0, Math.floor(position))}&duration=${ASS_WINDOW_DURATION_SECONDS}`;
}

/**
 * Fetches an ASS font bundle but abandons it at a time budget. On a budget miss
 * the in-flight fetch continues in the background (warming the shared cache)
 * and this resolves with no attached fonts so subtitle appearance is never
 * delayed. Errors degrade to [] exactly like the old parallel Promise.all.
 */
async function loadSubtitleFontBundleWithinBudget(
  url: string,
  signal: AbortSignal,
  onSourceChanged?: () => void,
): Promise<SubtitleFontBundleResult> {
  const fontPromise = loadSubtitleFontBundleResult(url, signal, onSourceChanged);
  let budgetTimer: ReturnType<typeof setTimeout> | null = null;
  const budgetMiss = new Promise<SubtitleFontBundleResult>((resolve) => {
    // A budget miss means the bytes have not arrived yet, not that the track
    // has none: mark it pending so a bounded background refresh adopts them
    // into the live renderer when the extraction completes.
    budgetTimer = setTimeout(() => resolve({ fonts: [], pending: true }), FONT_BUNDLE_BUDGET_MS);
  });
  try {
    const result = await Promise.race([fontPromise, budgetMiss]);
    // The race is settled on the font result; never leave the budget timer
    // dangling to fire into a settled pipeline.
    if (budgetTimer !== null) clearTimeout(budgetTimer);
    return result;
  } catch (err) {
    if (budgetTimer !== null) clearTimeout(budgetTimer);
    if ((err as Error).name === "AbortError") {
      return { fonts: [], pending: false };
    }
    console.error(`[useASSSubtitles] Failed to load subtitle font bundle ${url}:`, err);
    // A transient failure is retryable, independently of whether the server
    // marked the bundle pending.
    return { fonts: [], pending: true };
  }
}
// Window drags and fullscreen transitions resize the player many times in a
// row; reload the track once the crop settles rather than on every frame.
const FILL_MARGIN_DEBOUNCE_MS = 150;

interface ASSFillState {
  instance: JASSUB;
  /** Track content before any Fill margin inset. */
  baseContent: string;
  /** Inset currently loaded into the renderer. */
  inset: ASSMarginInset;
  /** JASSUB's own render-resolution settings, restored in Fit. */
  prescaleFactor: number;
  prescaleHeightLimit: number;
}

function fillInset(content: string, videoFit: VideoFitMode, coverCrop: CoverCrop) {
  return videoFit === "cover" ? resolveASSMarginInset(content, coverCrop) : NO_ASS_MARGIN_INSET;
}

/**
 * Puts the canvas on the video's Fit/Fill crop, reloads the track with margins
 * that keep regular events inside the visible area, and repaints. JASSUB sizes
 * its canvas as if the video always used object-fit: contain, so Fill relies on
 * the `player-ass-fill` override to crop the canvas exactly like the video and
 * raises the render resolution by the zoom so the enlarged bitmap stays sharp.
 */
async function syncASSFill(
  fill: ASSFillState,
  videoFit: VideoFitMode,
  coverCrop: CoverCrop,
  isCurrent: () => boolean,
): Promise<void> {
  const { instance } = fill;
  instance._canvas.classList.toggle("player-ass-fill", videoFit === "cover");
  if (videoFit === "cover") {
    instance.prescaleFactor = coverZoom(coverCrop);
    instance.prescaleHeightLimit = Number.POSITIVE_INFINITY;
  } else {
    instance.prescaleFactor = fill.prescaleFactor;
    instance.prescaleHeightLimit = fill.prescaleHeightLimit;
  }
  const inset = fillInset(fill.baseContent, videoFit, coverCrop);
  if (!sameASSMarginInset(inset, fill.inset)) {
    fill.inset = inset;
    await instance.renderer.setTrack(applyASSMarginInset(fill.baseContent, inset));
  }
  if (isCurrent()) await instance.resize(true);
}

/**
 * Manages client-side ASS/SSA subtitle rendering via JASSUB (libass WASM).
 *
 * When an ASS-codec subtitle track is active, this hook lazy-loads JASSUB,
 * creates an instance attached to the video element, and renders styled
 * subtitles onto a canvas overlay. When a non-ASS track is selected (or
 * subtitles are turned off), the JASSUB instance is destroyed.
 *
 * The existing VTT subtitle pipeline (useSubtitleTracks) handles SRT/VTT;
 * this hook handles ASS/SSA. The two are coordinated by the `isActive`
 * return value — when true, the VTT overlay should be suppressed.
 */
export function useASSSubtitles(
  videoRef: React.RefObject<HTMLVideoElement | null>,
  subtitleUrls: PlayerSubtitleInfo[],
  activeSubtitleIndex: number | null,
  isDetached: boolean,
  streamOriginSeconds: number,
  subtitleDelayMs: number,
  onLoadState?: (state: "idle" | "loading" | "ready" | "error") => void,
  // Fired when the server answers the subtitle fetch with
  // `subtitle_source_changed` (409): a virtual release rotated under this plan
  // and every URL for the active track is stale. The caller must refresh the
  // plan's subtitle inventory; retrying the same URL can never succeed.
  onSourceChanged?: () => void,
  // The effective subtitle source generation. It survives plan swaps and only
  // changes when the underlying source identity changes, so a subtitle-only
  // replan does not tear down JASSUB and immediately refetch a possibly-still-
  // stale URL. A genuine source change still rebuilds.
  sourceGeneration = 0,
  videoFit: VideoFitMode = "contain",
  coverCrop: CoverCrop = NO_COVER_CROP,
  // Bumped when the server retimed the active track behind an unchanged URL
  // (subtitle sync or a timing reset); changing it reloads the track.
  cueRevision = 0,
): { isActive: boolean } {
  const onLoadStateRef = useRef(onLoadState);
  onLoadStateRef.current = onLoadState;
  const onSourceChangedRef = useRef(onSourceChanged);
  onSourceChangedRef.current = onSourceChanged;
  const videoFitRef = useRef(videoFit);
  videoFitRef.current = videoFit;
  const coverCropRef = useRef(coverCrop);
  coverCropRef.current = coverCrop;

  const jassubRef = useRef<JASSUB | null>(null);
  const fillRef = useRef<ASSFillState | null>(null);
  const syncedFitRef = useRef(videoFit);
  const jassubImportRef = useRef<Promise<typeof JASSUB> | null>(null);
  // Effective JASSUB time offset. JASSUB renders the ASS event matching
  // `video.currentTime + timeOffset`, so an event at source time S appears
  // at video time S - timeOffset. `streamOriginSeconds` accounts for HLS
  // PTS rebasing; the user-facing delay (ms → s) must be SUBTRACTED so that
  // positive delay = subtitles shown later, matching the VTT path's
  // `start - origin + delay` cue shift.
  const effectiveOffset = streamOriginSeconds - subtitleDelayMs / 1000;
  const streamOriginRef = useRef(effectiveOffset);
  streamOriginRef.current = effectiveOffset;
  // Raw stream origin (without the user sync delay). The player clock plus
  // this is the source time a window request must be anchored to; the delay
  // only shifts rendering and must not shift the requested source interval.
  const sourceOriginRef = useRef(streamOriginSeconds);
  sourceOriginRef.current = streamOriginSeconds;

  // Resolve the active subtitle track.
  const activeSub =
    activeSubtitleIndex !== null
      ? (subtitleUrls.find((s) => s.index === activeSubtitleIndex) ?? null)
      : null;

  const isASS = activeSub !== null && isASSCodec(activeSub.codec);
  const activeUrl = isASS ? activeSub.url : null;
  const activeLanguage = isASS ? activeSub.language : "";
  const activeFontBundleUrl = isASS ? activeSub.font_bundle_url : undefined;

  // Main effect: create/destroy JASSUB based on active track.
  useEffect(() => {
    const video = videoRef.current;
    onLoadStateRef.current?.("idle");

    // Destroy JASSUB if the active track is not ASS, or player is detached,
    // or no video element is available.
    if (!activeUrl || !video || isDetached) {
      if (jassubRef.current) {
        jassubRef.current.destroy();
        jassubRef.current = null;
      }
      return;
    }

    let cancelled = false;
    // Exactly one attempt (initial load or boundary/seek refresh) runs at a
    // time; its AbortController is the cancel handle for seek supersession and
    // teardown.
    let activeController: AbortController | null = null;
    let retryTimer: ReturnType<typeof setTimeout> | null = null;
    // Set when the subtitle fetch answers 409 subtitle_source_changed: the
    // source rotated under this plan, so the retry must not re-run the whole
    // pipeline against the same stale URL.
    let sourceChangedSignaled = false;
    // Set when a seek cancels the in-flight attempt: the aborted request must
    // not count against the attempt budget, and the replacement restarts it.
    let supersededBySeek = false;

    // Sliding-window state. The first window is anchored just behind the
    // playhead so the cue already on screen is included. `windowEnd` is
    // Infinity once the whole-track fallback is in use, so boundary refresh
    // never schedules.
    let usingWholeTrack = false;
    let windowStart = Math.max(
      0,
      (video.readyState > 0 ? video.currentTime : 0) +
        sourceOriginRef.current -
        ASS_WINDOW_LEAD_SECONDS,
    );
    let windowEnd = windowStart + ASS_WINDOW_DURATION_SECONDS;
    // Consecutive windowed failures since the last successful window, shared by
    // the initial load and boundary refreshes so the 3→1→terminal budget is one
    // finite policy. `terminal` latches after the whole-track fallback fails:
    // no further attempts, ever.
    let windowFailures = 0;
    let terminal = false;
    // Consecutive TEXT fetch failures for this mount, reset only on a
    // successful load — not on a seek. Unlike `windowFailures` (which a seek
    // resets so a new position gets a fair try), this is the backstop that
    // stops a release erroring every request from being re-fetched forever.
    let textFailures = 0;
    // Consecutive explicitly-retryable TEXT failures (502/503/504/429/408)
    // since the last successful load. Same backoff loop on a separate generous
    // budget so a transient upstream flap does not spend the terminal budget
    // and wedge the mount on "Retrying…".
    let retryableTextFailures = 0;
    let busy = false;
    // Newest seek target queued while an attempt is in flight.
    let pendingStart: number | null = null;

    // Fonts belong to the track, not the window: resolve them once (after the
    // first window's text lands) and reuse them for every later window. A
    // pending bundle leaves `attached` empty but is NOT final: a bounded
    // background refresh adopts the completed bytes into the live instance.
    let fontState: {
      attached: Uint8Array[];
      fallbackFont: ReturnType<typeof fallbackFontForSubtitle>;
      fallbackFontData: Uint8Array[] | null;
    } | null = null;
    let fontBundlePending = false;
    let fontRefreshAttempts = 0;
    let fontRefreshTimer: ReturnType<typeof setTimeout> | null = null;
    let fontRefreshController: AbortController | null = null;

    /** Source-time position of the playhead (player clock + stream origin). */
    function sourcePosition(): number {
      return (video!.readyState > 0 ? video!.currentTime : 0) + sourceOriginRef.current;
    }

    /**
     * Attaches a completed font bundle to the live renderer without reloading
     * the subtitle script or the track: libass picks the newly registered fonts
     * up on the next repaint.
     */
    function adoptFontsIntoLiveInstance(fonts: Uint8Array[]) {
      const instance = jassubRef.current;
      if (!instance || fonts.length === 0) return;
      const renderer = (
        instance as unknown as {
          renderer?: { addFonts?: (values: Uint8Array[]) => Promise<unknown> };
        }
      ).renderer;
      if (!renderer?.addFonts) return;
      void Promise.resolve(renderer.addFonts(fonts))
        .then(() => instance.ready)
        .then(() => {
          if (jassubRef.current === instance) return instance.resize(true);
        })
        .catch((err) => {
          console.error("[useASSSubtitles] Failed to attach refreshed fonts:", err);
        });
    }

    /**
     * Re-fetches a font bundle the server reported as pending and hot-swaps it
     * into the live renderer when it completes. Finite: it stops on fonts, on a
     * definitive font-less bundle, or after ASS_FONT_REFRESH_MAX_ATTEMPTS.
     */
    async function refreshPendingFonts() {
      fontRefreshTimer = null;
      if (cancelled || !fontBundlePending || !activeFontBundleUrl) return;
      if (fontRefreshAttempts >= ASS_FONT_REFRESH_MAX_ATTEMPTS) return;
      fontRefreshAttempts += 1;
      const controller = new AbortController();
      fontRefreshController = controller;
      try {
        const result = await loadSubtitleFontBundleResult(
          activeFontBundleUrl,
          controller.signal,
          onSourceChangedRef.current ?? undefined,
        );
        if (cancelled) return;
        if (!result.pending) {
          // Definitive answer (fonts or a genuinely font-less file): stop.
          fontBundlePending = false;
          if (result.fonts.length > 0) {
            if (fontState) fontState.attached = result.fonts;
            adoptFontsIntoLiveInstance(result.fonts);
          }
          return;
        }
      } catch (err) {
        if ((err as Error).name !== "AbortError") {
          console.error(
            `[useASSSubtitles] Failed to refresh subtitle font bundle ${activeFontBundleUrl}:`,
            err,
          );
        }
      } finally {
        if (fontRefreshController === controller) fontRefreshController = null;
      }
      if (fontBundlePending && fontRefreshAttempts < ASS_FONT_REFRESH_MAX_ATTEMPTS) {
        fontRefreshTimer = setTimeout(
          () => void refreshPendingFonts(),
          ASS_FONT_REFRESH_BACKOFF_MS,
        );
      }
    }

    function scheduleFontRefresh() {
      if (cancelled || !fontBundlePending || !activeFontBundleUrl) return;
      if (fontRefreshTimer !== null) return;
      if (fontRefreshAttempts >= ASS_FONT_REFRESH_MAX_ATTEMPTS) return;
      fontRefreshTimer = setTimeout(() => void refreshPendingFonts(), ASS_FONT_REFRESH_BACKOFF_MS);
    }

    /** Resolve the per-track font state once, reusing it across windows. */
    async function resolveFontState(signal: AbortSignal, subContent: string) {
      if (fontState) return fontState;
      let attachedFontData: Uint8Array[] = [];
      // The subtitle TEXT drives the pipeline (and the stall watchdog); the
      // font bundle is raced against a budget so a slow extraction never
      // delays subtitle appearance. JASSUB renders with fallback fonts
      // meanwhile, and the budget-losing fetch continues in the background.
      if (activeFontBundleUrl) {
        const result = await loadSubtitleFontBundleWithinBudget(
          activeFontBundleUrl,
          signal,
          onSourceChangedRef.current ?? undefined,
        );
        attachedFontData = result.fonts;
        fontBundlePending = result.pending;
      }
      // libass renders missing glyphs with its *default* font — it does not
      // search other loaded fonts for coverage. JASSUB's built-in default
      // (Liberation Sans) lacks many non-Latin glyphs, so for those scripts we
      // point `defaultFont` at a font that covers them, chosen by track metadata
      // first and subtitle text as a fallback.
      const fallbackFont = fallbackFontForSubtitle(activeLanguage, subContent);
      let fallbackFontData: Uint8Array[] | null = null;
      if (fallbackFont) {
        try {
          fallbackFontData = await loadSubtitleFallbackFontData(fallbackFont);
        } catch (err) {
          if (!cancelled) {
            console.error(
              `[useASSSubtitles] Failed to load fallback font ${fallbackFont.family}:`,
              err,
            );
          }
        }
      }
      fontState = { attached: attachedFontData, fallbackFont, fallbackFontData };
      // A pending bundle must not persist as "this track has no fonts": fetch
      // the completed bytes in the background and hot-swap them in.
      if (fontBundlePending) scheduleFontRefresh();
      return fontState;
    }

    async function initJASSUB(
      signal: AbortSignal,
      progress: () => void,
      start: number,
      wholeTrack: boolean,
    ) {
      if (!video || cancelled) return;
      onLoadStateRef.current?.("loading");

      // Lazy-load JASSUB module (only once).
      if (!jassubImportRef.current) {
        jassubImportRef.current = import("jassub")
          .then((m) => m.default)
          .catch((err) => {
            jassubImportRef.current = null;
            throw err;
          });
      }

      const classPromise = jassubImportRef.current;
      void classPromise.catch(() => {});

      const url = wholeTrack ? activeUrl! : appendSubtitleWindow(activeUrl!, start);

      let subContent: string;
      let fonts: Uint8Array[];
      let renderedSubContent: string;
      try {
        subContent = await fetch(url, { signal }).then(async (response) => {
          if (!response.ok) {
            if (await isSubtitleSourceChanged(response)) {
              sourceChangedSignaled = true;
              onSourceChangedRef.current?.();
              throw new DOMException("Subtitle source changed", "AbortError"); // bypass the retry
            }
            throw new SubtitleFetchError(response.status, `HTTP ${response.status}`);
          }
          progress();
          if (!response.body) return response.text();
          const reader = response.body.getReader();
          const decoder = new TextDecoder();
          let text = "";
          while (!signal.aborted && !cancelled) {
            const { value, done } = await reader.read();
            if (done) return text + decoder.decode();
            progress();
            text += decoder.decode(value, { stream: true });
          }
          throw new DOMException("Subtitle loading cancelled", "AbortError");
        });

        const resolvedFonts = await resolveFontState(signal, subContent);
        renderedSubContent =
          resolvedFonts.fallbackFont && resolvedFonts.fallbackFontData
            ? forceASSFontFamily(subContent, resolvedFonts.fallbackFont.family)
            : subContent;
        fonts = [...resolvedFonts.attached, ...(resolvedFonts.fallbackFontData ?? [])];
      } catch (err) {
        if (!cancelled && (err as Error).name !== "AbortError") {
          console.error(`[useASSSubtitles] Failed to fetch ${url}:`, err);
        }
        throw err;
      }

      if (cancelled || signal.aborted) return;

      const JASSUBClass = await classPromise;
      if (cancelled || signal.aborted) return;
      const initialInset = fillInset(renderedSubContent, videoFitRef.current, coverCropRef.current);
      const instance = new JASSUBClass({
        video,
        subContent: applyASSMarginInset(renderedSubContent, initialInset),
        timeOffset: streamOriginRef.current,
        // The browser Local Font Access API is inconsistent and permissioned.
        // Letting JASSUB probe it produces noisy console warnings for common ASS
        // style fonts without making playback reliable across clients.
        queryFonts: false,
        availableFonts: { "liberation sans": liberationSansUrl },
        ...(fonts.length > 0
          ? {
              fonts,
              ...(fontState?.fallbackFont && { defaultFont: fontState.fallbackFont.family }),
            }
          : {}),
      });

      // Guard against the effect being cleaned up while the constructor ran.
      if (cancelled || signal.aborted) {
        instance.destroy();
        return;
      }

      const previous = jassubRef.current;
      jassubRef.current = instance;
      const fill: ASSFillState = {
        instance,
        baseContent: renderedSubContent,
        inset: initialInset,
        prescaleFactor: instance.prescaleFactor,
        prescaleHeightLimit: instance.prescaleHeightLimit,
      };
      fillRef.current = fill;
      instance._canvas.classList.toggle("player-ass-fill", videoFitRef.current === "cover");
      await instance.ready;
      if (cancelled || signal.aborted || jassubRef.current !== instance) return;
      if (previous && previous !== instance) previous.destroy();

      // Fit and the player size can change while the subtitle source, fonts,
      // or renderer are still loading. Re-read both after readiness so the
      // first rendered frame cannot inherit what this effect started with.
      await syncASSFill(
        fill,
        videoFitRef.current,
        coverCropRef.current,
        () => jassubRef.current === instance,
      );
      if (!cancelled && !signal.aborted && jassubRef.current === instance) {
        // Advance to the window this instance renders so the next
        // timeupdate check measures against the loaded track, then clear
        // the failure budgets so a later unrelated error starts fresh.
        usingWholeTrack = wholeTrack;
        windowStart = start;
        windowEnd = wholeTrack ? Infinity : start + ASS_WINDOW_DURATION_SECONDS;
        windowFailures = 0;
        textFailures = 0;
        retryableTextFailures = 0;
        onLoadStateRef.current?.("ready");
      }
    }

    /**
     * One bounded attempt at `start`. Arming the watchdog before any network
     * work means a pre-header hang times out and counts as a failed attempt
     * instead of blocking the pipeline forever. Success resets the shared
     * budget inside `initJASSUB`; failure applies the one finite policy:
     * 3 windowed attempts, then 1 whole-track fallback, then terminal.
     *
     * Two things end the loop early rather than retrying: a definitive 4xx
     * (the URL can never satisfy this request, so the whole-track fallback is
     * pointless too), and the shared ASS_TEXT_MAX_FAILURES ceiling, which stops
     * a release that errors every request from being re-fetched forever as the
     * window re-anchors on seeks.
     */
    async function attempt(start: number, wholeTrack: boolean) {
      if (cancelled || terminal || busy) return;
      busy = true;
      supersededBySeek = false;
      const attemptController = new AbortController();
      activeController = attemptController;
      let timeout: ReturnType<typeof setTimeout> | null = null;
      const progress = () => {
        if (cancelled || attemptController.signal.aborted) return;
        if (timeout !== null) clearTimeout(timeout);
        timeout = setTimeout(() => attemptController.abort(), ASS_STALL_TIMEOUT_MS);
      };
      // Arm the watchdog before the fetch, not on first progress.
      progress();
      try {
        await initJASSUB(attemptController.signal, progress, start, wholeTrack);
      } catch (err) {
        if (cancelled) return;
        // A signaled source change must not re-run against the same stale URL;
        // the rebuilt JASSUB comes from the main effect re-running when the
        // refresh adopts a new plan and activeUrl changes.
        if (sourceChangedSignaled) return;
        if (supersededBySeek) {
          // A seek cancelled this request to replace it: it is not a failure.
          return;
        }
        console.error("[useASSSubtitles] Unable to load subtitles:", err);
        onLoadStateRef.current?.("error");
        // Count every genuine failure (a 4xx, 5xx, stall, or network error)
        // against the shared ceiling before deciding the next step — except
        // explicitly-retryable statuses, which ride the same backoff loop on
        // a separate generous budget so a transient upstream flap does not
        // spend the terminal budget and wedge the mount on "Retrying…".
        const retryableFailure =
          err instanceof SubtitleFetchError && isRetryableSubtitleStatus(err.status);
        if (retryableFailure) {
          retryableTextFailures += 1;
          if (retryableTextFailures >= ASS_RETRYABLE_TEXT_MAX_FAILURES) {
            terminal = true;
            return;
          }
        } else {
          retryableTextFailures = 0;
          textFailures += 1;
        }
        if (wholeTrack && !retryableFailure) {
          // The whole-track fallback failed too: terminal, no retry loop.
          // A retryable whole-track failure falls through to the backoff
          // retry below so a flap recovers instead of wedging the mount.
          terminal = true;
          return;
        }
        // A definitive 4xx cannot be fixed by the whole-track fallback or
        // another windowed attempt: stop now instead of burning the budget on
        // requests the server has already rejected.
        if (err instanceof SubtitleFetchError && isDefinitiveSubtitleStatus(err.status)) {
          terminal = true;
          return;
        }
        windowFailures += 1;
        if (windowFailures >= ASS_WINDOW_MAX_ATTEMPTS) {
          // Bounded windowed retries exhausted: request the param-less
          // whole-track URL once instead of hammering the failing window.
          usingWholeTrack = true;
        }
        if (textFailures >= ASS_TEXT_MAX_FAILURES) {
          // The shared failure ceiling is spent: no further attempt, even the
          // whole-track fallback.
          terminal = true;
          return;
        }
        const retryStart = usingWholeTrack
          ? windowStart
          : Math.max(0, sourcePosition() - ASS_WINDOW_LEAD_SECONDS);
        retryTimer = setTimeout(() => {
          retryTimer = null;
          void attempt(retryStart, usingWholeTrack);
        }, ASS_RETRY_BACKOFF_MS);
      } finally {
        if (timeout !== null) clearTimeout(timeout);
        if (activeController === attemptController) activeController = null;
        busy = false;
        const next = pendingStart;
        pendingStart = null;
        if (next !== null && !cancelled && !terminal) {
          void attempt(next, false);
        }
      }
    }

    function maybeRefreshWindow() {
      if (cancelled || terminal || usingWholeTrack || !jassubRef.current) return;
      const source = sourcePosition();
      const outside = source < windowStart - 1 || source > windowEnd + 1;
      const nearEnd = !outside && source > windowEnd - ASS_WINDOW_PREFETCH_LEAD_SECONDS;
      if (!outside && !nearEnd) return;
      // Restart a full lead behind the playhead so the replacement window
      // overlaps the cue currently on screen regardless of why it fired.
      const target = Math.max(0, source - ASS_WINDOW_LEAD_SECONDS);
      if (busy) {
        // A seek while an attempt is in flight must win: cancel it and restart
        // the budget at the new position once it settles.
        if (outside) {
          supersededBySeek = true;
          windowFailures = 0;
          usingWholeTrack = false;
          if (retryTimer !== null) {
            clearTimeout(retryTimer);
            retryTimer = null;
          }
          activeController?.abort();
          pendingStart = target;
        }
        return;
      }
      // A scheduled retry owns the next attempt; event-driven refreshes wait.
      if (retryTimer !== null) return;
      void attempt(target, false);
    }

    void attempt(windowStart, false);
    video.addEventListener("timeupdate", maybeRefreshWindow);
    video.addEventListener("seeking", maybeRefreshWindow);
    video.addEventListener("seeked", maybeRefreshWindow);

    return () => {
      cancelled = true;
      if (retryTimer !== null) clearTimeout(retryTimer);
      if (fontRefreshTimer !== null) clearTimeout(fontRefreshTimer);
      fontRefreshController?.abort();
      activeController?.abort();
      video.removeEventListener("timeupdate", maybeRefreshWindow);
      video.removeEventListener("seeking", maybeRefreshWindow);
      video.removeEventListener("seeked", maybeRefreshWindow);
      // Destroy the current instance if the effect is being torn down
      // (e.g. track switch or unmount). This covers the common case where
      // initJASSUB has already completed and stored the instance.
      if (jassubRef.current) {
        jassubRef.current.destroy();
        jassubRef.current = null;
      }
    };
    // videoRef is a stable ref object. streamOriginSeconds is read from
    // sourceOriginRef inside the async function to always get the latest value.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [activeUrl, activeLanguage, activeFontBundleUrl, isDetached, sourceGeneration, cueRevision]);

  // Update JASSUB's time offset when either the media timeline remaps or
  // the user nudges subtitle sync. Avoids destroying and recreating the
  // instance for offset-only changes.
  useEffect(() => {
    const instance = jassubRef.current;
    if (!instance || !activeUrl) return;

    instance.timeOffset = effectiveOffset;
    void instance.ready
      .then(() => {
        if (jassubRef.current === instance) return instance.resize(true);
      })
      .catch((err) => {
        if (jassubRef.current === instance) {
          console.error("[useASSSubtitles] Unable to repaint subtitles:", err);
        }
      });
  }, [effectiveOffset, activeUrl]);

  // Keep the canvas crop and the Fill margins in step with the video. A fit
  // toggle applies immediately; crop changes from resizing are debounced.
  const cropX = coverCrop.x;
  const cropY = coverCrop.y;
  useEffect(() => {
    const fitChanged = syncedFitRef.current !== videoFit;
    syncedFitRef.current = videoFit;
    const fill = fillRef.current;
    const instance = jassubRef.current;
    if (!fill || !instance || fill.instance !== instance || !activeUrl) return;

    instance._canvas.classList.toggle("player-ass-fill", videoFit === "cover");
    const isCurrent = () => jassubRef.current === instance;
    const timer = setTimeout(
      () => {
        void instance.ready
          .then(() => {
            if (isCurrent()) {
              return syncASSFill(fill, videoFit, { x: cropX, y: cropY }, isCurrent);
            }
          })
          .catch((err) => {
            if (isCurrent()) {
              console.error("[useASSSubtitles] Unable to apply video fit to subtitles:", err);
            }
          });
      },
      fitChanged ? 0 : FILL_MARGIN_DEBOUNCE_MS,
    );
    return () => clearTimeout(timer);
  }, [activeUrl, videoFit, cropX, cropY]);

  // Cleanup on unmount.
  useEffect(() => {
    return () => {
      if (jassubRef.current) {
        jassubRef.current.destroy();
        jassubRef.current = null;
      }
    };
  }, []);

  return { isActive: isASS && !isDetached };
}
