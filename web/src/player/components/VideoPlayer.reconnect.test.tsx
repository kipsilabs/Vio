import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { createElement, useEffect, type ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { PlayerConfigProvider, type PlayerConfig } from "../context/PlayerConfigContext";
import { fixturePlanV3, fixtureSubtitleInventoryItemV3 } from "../protocol-v3.fixtures";
import type { PlanV3 } from "../protocol-v3";
import type { PlayerFileVersion, SubtitleMode } from "../types";
import { resetCodecDetectionForTests } from "../hooks/useCodecDetection";
import { usePlaybackSession, type UsePlaybackSessionResult } from "../hooks/usePlaybackSession";
import { resetSessionMutations } from "../session-mutations";
import { VideoPlayer } from "./VideoPlayer";

// The player and the session hook together, wired the way WatchPage wires
// them, so the sequences reviewers reported run through both: transport
// events in the player, requests and recovery in the hook.

const controls = vi.hoisted(() => ({
  current: null as null | {
    activeSubtitleIndex: number | null;
    playing?: boolean;
    currentTime?: number;
    onSeek?: (seconds: number) => void;
  },
}));
const hlsJS = vi.hoisted(() => ({
  latest: null as null | { emit: (event: string, data: unknown) => void },
  loadSource: vi.fn(),
  startLoad: vi.fn(),
}));
// Captures the progress sampler's gone-session callback so a test can drive a
// mid-stream eviction the way the real sampler reports one.
const progressOptions = vi.hoisted(() => ({
  current: null as null | { onSessionLost?: () => void },
}));

vi.mock("sonner", () => ({
  toast: { error: vi.fn(), info: vi.fn(), success: vi.fn(), message: vi.fn() },
}));
vi.mock("../hooks/usePlaybackRealtime", () => ({
  usePlaybackRealtime: () => ({ connectionState: "connected" }),
}));
vi.mock("../hooks/useWatchProgress", () => ({
  useWatchProgress: (...args: unknown[]) => {
    progressOptions.current = { onSessionLost: args[3] as (() => void) | undefined };
    return vi.fn().mockResolvedValue(undefined);
  },
}));
vi.mock("../hooks/useKeyboardShortcuts", () => ({ useKeyboardShortcuts: vi.fn() }));
vi.mock("../hooks/useSubtitleTracks", () => ({ useSubtitleTracks: () => [] }));
vi.mock("../hooks/useASSSubtitles", () => ({ useASSSubtitles: () => ({ isActive: false }) }));
// The delay is the profile's saved offset; plain state stands in for the query.
vi.mock("../hooks/useSubtitleDelay", async () => {
  const { useState } = await import("react");
  return { useSubtitleDelay: () => useState(0) };
});
vi.mock("../hooks/useSubtitleAppearance", () => ({
  useSubtitleAppearance: () => ({
    settings: { position: "bottom", fontSize: "large" },
    containerStyle: {},
    cueStyle: {},
  }),
}));
vi.mock("../hooks/useSubtitleLayout", () => ({
  useSubtitleLayout: () => ({ positionStyle: {}, fontScale: 1 }),
}));
vi.mock("./PlayerControls", () => ({
  SKIP_BACK_SECONDS: 10,
  SKIP_FORWARD_SECONDS: 30,
  PlayerControls: (props: { activeSubtitleIndex: number | null; playing?: boolean }) => {
    controls.current = props;
    return null;
  },
}));
vi.mock("hls.js", () => ({
  default: class MockHls {
    static Events = {
      ERROR: "error",
      MANIFEST_PARSED: "manifestParsed",
      BUFFER_APPENDED: "bufferAppended",
    };
    static ErrorTypes = { NETWORK_ERROR: "networkError", MEDIA_ERROR: "mediaError" };
    static ErrorDetails = {
      MANIFEST_LOAD_ERROR: "manifestLoadError",
      MANIFEST_LOAD_TIMEOUT: "manifestLoadTimeOut",
      MANIFEST_PARSING_ERROR: "manifestParsingError",
      FRAG_LOAD_ERROR: "fragLoadError",
    };
    static isSupported = () => true;

    handlers = new Map<string, (event: string, data: unknown) => void>();

    constructor() {
      hlsJS.latest = this;
    }

    on(event: string, handler: (event: string, data: unknown) => void) {
      this.handlers.set(event, handler);
    }
    emit(event: string, data: unknown) {
      this.handlers.get(event)?.(event, data);
    }
    startLoad() {
      hlsJS.startLoad();
    }
    stopLoad() {}
    loadSource() {
      hlsJS.loadSource();
    }
    attachMedia() {}
    destroy() {}
  },
}));
// Start and replan go to the fetch stub below, as in the session hook tests.
vi.mock("../start-v2", () => ({
  startPlaybackV2: async (config: PlayerConfig, body: unknown) => {
    const { playerFetch } = await import("../player-fetch");
    const { registerSessionMutations } = await import("../session-mutations");
    const decision = await playerFetch<import("../protocol-v3").DecisionResponseV3>(
      { ...config, apiBaseUrl: "/api/v2" },
      "/playback/start",
      { method: "POST", body: JSON.stringify(body) },
    );
    const sessionId = decision.playback_plan?.session_id ?? decision.session_id;
    if (decision.playback_plan && sessionId) registerSessionMutations(sessionId, "installation");
    return decision;
  },
}));
vi.mock("../lifecycle-v2", () => ({
  replanV2: async (config: PlayerConfig, sessionId: string, body: unknown) => {
    const { playerFetch } = await import("../player-fetch");
    return playerFetch({ ...config, apiBaseUrl: "/api/v2" }, `/playback/${sessionId}/replan`, {
      method: "POST",
      body: JSON.stringify(body),
    });
  },
}));

const playerConfig: PlayerConfig = {
  apiBaseUrl: "/api/v1",
  getAccessToken: () => "token",
  getProfileId: () => "profile-1",
  getDeviceId: () => "test-device",
  getProfileToken: () => null,
};

type Body = Record<string, unknown>;

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function playable(sessionId: string, plan: PlanV3) {
  return {
    protocol_version: 3,
    server_features: ["playback_plan_v3"],
    outcome: "playable",
    session_id: sessionId,
    playback_plan: plan,
  };
}

function directPlan(overrides: Partial<PlanV3> = {}): PlanV3 {
  const sessionId = overrides.session_id ?? "session-1";
  return fixturePlanV3({
    delivery: "original_http",
    stream: {
      url: `/stream/${sessionId}`,
      protocol: "http_progressive",
      headers: {},
      header_refresh: "none",
    },
    ...overrides,
  });
}

function remuxPlan(overrides: Partial<PlanV3> = {}): PlanV3 {
  return fixturePlanV3({
    delivery: "server_remux_progressive",
    stream: {
      url: "/stream/session-1",
      protocol: "http_progressive",
      mime_type: "video/mp4",
      headers: {},
      header_refresh: "none",
    },
    timeline: {
      source_start_seconds: 10,
      stream_origin_seconds: 8,
      player_start_seconds: 2,
      timeline_offset_seconds: 8,
      can_seek_anywhere: false,
      seek_restoration: "source_position",
    },
    selected_tracks: {
      audio: { id: "file:7:audio:0", index: 0 },
      subtitle: { id: "file:7:subtitle:0", index: 0 },
    },
    audio_tracks: [
      {
        index: 0,
        language: "en",
        codec: "aac",
        default: true,
        track_id: "file:7:audio:0",
        selection_index: 0,
      },
      {
        index: 1,
        language: "de",
        codec: "aac",
        default: false,
        track_id: "file:7:audio:1",
        selection_index: 1,
      },
    ],
    ...overrides,
  });
}

interface Requests {
  starts: Body[];
  replans: Array<{ url: string; body: Body }>;
}

/**
 * Routes start and replan requests to the test. A handler that throws stands
 * in for an unreachable server.
 */
function stubServer(handlers: {
  start: (body: Body, count: number) => Response | Promise<Response>;
  replan: (url: string, body: Body, count: number) => Response | Promise<Response>;
}): Requests {
  const requests: Requests = { starts: [], replans: [] };
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        const body = JSON.parse(String(init?.body)) as Body;
        requests.starts.push(body);
        return handlers.start(body, requests.starts.length);
      }
      if (url.endsWith("/replan")) {
        const body = JSON.parse(String(init?.body)) as Body;
        requests.replans.push({ url, body });
        return handlers.replan(url, body, requests.replans.length);
      }
      if (init?.method === "DELETE") return jsonResponse({ outcome: "stopped" });
      if (url.endsWith("/route-events")) return new Response(null, { status: 202 });
      return new Response(null, { status: 404 });
    }),
  );
  return requests;
}

const sessionRef: { current: UsePlaybackSessionResult | null } = { current: null };

const NO_VERSIONS: PlayerFileVersion[] = [];

function Harness({
  subtitleMode = "auto",
  preferredSubtitleLanguage = null,
  versions = NO_VERSIONS,
  initialSubtitleTrackIndexByFileId,
}: {
  subtitleMode?: SubtitleMode;
  preferredSubtitleLanguage?: string | null;
  versions?: PlayerFileVersion[];
  initialSubtitleTrackIndexByFileId?: Record<number, number>;
}) {
  const playback = usePlaybackSession(
    "request-1",
    versions,
    [],
    7,
    0,
    false,
    "auto",
    null,
    undefined,
    null,
    initialSubtitleTrackIndexByFileId,
  );
  // Published after commit, so the test reads the session the player rendered.
  useEffect(() => {
    sessionRef.current = playback;
  });
  if (!playback.plan || !playback.streamUrl || !playback.sessionId) return null;
  return createElement(VideoPlayer, {
    title: "Test episode",
    streamUrl: playback.streamUrl,
    plan: playback.plan,
    planRevision: playback.planRevision,
    transportRevision: playback.transportRevision,
    replayRevision: playback.replayRevision,
    shouldAutoPlay: playback.shouldAutoPlay,
    replanning: playback.replanning,
    replanError: playback.error,
    replanErrorTitle: playback.errorTitle,
    replanErrorRetryable: playback.errorRetryable,
    replanErrorReason: playback.errorReason,
    sessionId: playback.sessionId,
    activeFileId: playback.mediaFileId,
    subtitleUrls: playback.subtitleUrls,
    initialPosition: playback.initialPosition,
    qualityPreference: playback.qualityPreference,
    onQualitySelect: playback.changeQuality,
    onSubtitleTrackChange: playback.changeSubtitleTrack,
    onPlanFailure: playback.recoverFromFailure,
    onConnectionLost: playback.recoverConnection,
    onRetryConnection: playback.retryConnection,
    connectionStatus: playback.connectionStatus,
    connectionErrorTitle: playback.connectionErrorTitle,
    connectionError: playback.connectionError,
    onReanchorSeek: playback.reanchorSeek,
    onRestartPlayback: playback.restartPlayback,
    onPlaybackStateChange: (state) =>
      playback.updatePlaybackState(state.currentTime, state.playing),
    onFirstFrame: playback.reportFirstFrame,
    // As WatchPage: a subtitle the start had to drop turns subtitles off.
    subtitleMode: playback.initialSubtitleError ? "off" : subtitleMode,
    preferredSubtitleLanguage,
    intro: null,
    credits: null,
    onExit: vi.fn(),
    seekIntervals: { back: 10, forward: 30 },
  });
}

function renderHarness(props: Parameters<typeof Harness>[0] = {}) {
  return render(createElement(Harness, props), {
    wrapper: ({ children }: { children: ReactNode }) =>
      createElement(PlayerConfigProvider, { config: playerConfig, children }),
  });
}

/** Fires the `timeupdate` of a source that has data for its position. */
function showFrame(video: HTMLVideoElement) {
  Object.defineProperty(video, "readyState", { configurable: true, value: 2 });
  fireEvent.timeUpdate(video);
  Reflect.deleteProperty(video, "readyState");
}

/** Makes the element report a seekable window, as a produced playlist does. */
function seekableWindow(video: HTMLVideoElement, end: number) {
  Object.defineProperty(video, "seekable", {
    configurable: true,
    value: { length: 1, start: () => 0, end: () => end },
  });
}

/** The viewer is watching: the element plays and shows a frame. */
async function startWatching(): Promise<HTMLVideoElement> {
  let video: HTMLVideoElement | null = null;
  await waitFor(() => {
    video = document.querySelector("video");
    expect(video?.getAttribute("src")).toBeTruthy();
  });
  const element = video as unknown as HTMLVideoElement;
  Object.defineProperty(element, "paused", { configurable: true, value: false });
  fireEvent.play(element);
  showFrame(element);
  await waitFor(() => expect(sessionRef.current?.connectionStatus).toBe("connected"));
  return element;
}

beforeEach(() => {
  sessionRef.current = null;
  controls.current = null;
  hlsJS.latest = null;
  hlsJS.loadSource.mockClear();
  hlsJS.startLoad.mockClear();
  progressOptions.current = null;
  vi.spyOn(HTMLMediaElement.prototype, "play").mockResolvedValue(undefined);
  vi.spyOn(HTMLMediaElement.prototype, "pause").mockImplementation(() => {});
  vi.spyOn(HTMLMediaElement.prototype, "load").mockImplementation(() => {});
});

afterEach(() => {
  cleanup();
  resetCodecDetectionForTests();
  resetSessionMutations();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe("VideoPlayer with the playback session: reconnect", () => {
  it("leaves a session an administrator ended when a resumed remux reopens", async () => {
    const remuxPlan = directPlan({
      delivery: "server_remux_progressive",
      timeline: { ...directPlan().timeline, can_seek_anywhere: false },
    });
    const requests = stubServer({
      start: () => jsonResponse(playable("session-1", remuxPlan), 201),
      replan: () =>
        jsonResponse(
          {
            type: "https://siloserver.org/docs/api/v2/problems/playback_session_ended",
            title: "Playback session ended",
            status: 410,
            detail: "An administrator ended this playback session",
          },
          410,
        ),
    });
    renderHarness();
    const video = await startWatching();
    const now = Date.now();
    fireEvent.pause(video);

    // The viewer comes back after ten minutes, so the player reopens the stream.
    vi.spyOn(Date, "now").mockReturnValue(now + 10 * 60_000);
    fireEvent.play(video);

    await waitFor(() => expect(sessionRef.current?.connectionStatus).toBe("ended"));
    expect(requests.replans.map((replan) => replan.body.operation)).toEqual(["seek_reanchor"]);
    expect(requests.starts).toHaveLength(1);
  });

  it("keeps playing after a reconnect that follows a failed HLS startup", async () => {
    // HLS goes through hls.js: the browser reports no native support.
    vi.spyOn(HTMLMediaElement.prototype, "canPlayType").mockReturnValue("");
    const hlsPlan = fixturePlanV3({
      plan_id: "plan:hls-quality",
      plan_attempt_key: "v3:hls-quality",
      stream: {
        url: "/playback/transcode/session-1/master.m3u8",
        protocol: "hls",
        headers: {},
        header_refresh: "none",
      },
    });
    let rejectRecovery: ((error: Error) => void) | undefined;
    const requests = stubServer({
      start: () => jsonResponse(playable("session-1", directPlan()), 201),
      replan: (_url, body) => {
        if (body.operation === "quality_change") {
          return jsonResponse(playable("session-1", hlsPlan));
        }
        if (body.operation === "failure_recovery") {
          // The server goes away while the recovery is in flight.
          return new Promise<Response>((_resolve, reject) => {
            rejectRecovery = reject;
          });
        }
        // The reconnect finds it back.
        return jsonResponse(playable("session-1", directPlan({ plan_id: "plan:reconnected" })));
      },
    });

    renderHarness();
    const video = await startWatching();

    // A playing direct stream switches to an HLS rung.
    act(() => sessionRef.current?.changeQuality("720p", 100));
    await waitFor(() => expect(hlsJS.latest).not.toBeNull());

    // The new transport never starts: hls.js gives up twice on the network,
    // and the startup guard reports the route as failed. The recovery is
    // delayed with backoff, so the second failure is the reload's.
    const fatal = { fatal: true, type: "networkError", details: "manifestLoadError" };
    const now = vi.spyOn(Date, "now").mockReturnValue(10_000);
    act(() => hlsJS.latest?.emit("error", fatal));
    await waitFor(() => expect(hlsJS.loadSource).toHaveBeenCalledTimes(2), { timeout: 3_000 });
    now.mockReturnValue(20_000);
    act(() => hlsJS.latest?.emit("error", fatal));
    now.mockRestore();
    await waitFor(() => expect(rejectRecovery).toBeDefined());

    // Tearing the failed transport down pauses the element. That pause is
    // reported, but it is not the viewer's.
    Object.defineProperty(video, "paused", { configurable: true, value: true });
    fireEvent.pause(video);
    await act(async () => Promise.resolve());

    await act(async () => {
      rejectRecovery?.(new TypeError("Failed to fetch"));
      await Promise.resolve();
    });
    await waitFor(() => expect(sessionRef.current?.connectionStatus).toBe("reconnecting"));

    await waitFor(() => expect(sessionRef.current?.plan?.plan_id).toBe("plan:reconnected"), {
      timeout: 4_000,
    });
    expect(sessionRef.current?.connectionStatus).toBe("connected");
    expect(sessionRef.current?.shouldAutoPlay).toBe(true);
    expect(requests.replans.map(({ body }) => body.operation)).toEqual([
      "quality_change",
      "failure_recovery",
      "track_change",
    ]);
  });

  it("does not re-request a burned-in subtitle the restarted session was refused", async () => {
    const burnIn = fixtureSubtitleInventoryItemV3({
      track_id: "file:7:subtitle:3",
      combined_index: 3,
      codec: "hdmv_pgs_subtitle",
      delivery: "burn_in_only",
      url: undefined,
    });
    const requests = stubServer({
      start: (body, count) => {
        if (count === 1) {
          return jsonResponse(
            playable(
              "session-1",
              directPlan({
                selected_tracks: {
                  audio: { id: "file:7:audio:0", index: 0 },
                  subtitle: { id: "file:7:subtitle:3", index: 3 },
                },
                subtitle: { mode: "burn_in", inventory: [burnIn] },
              }),
            ),
            201,
          );
        }
        if (body.subtitle_track_index === 3) {
          return jsonResponse(
            {
              protocol_version: 3,
              server_features: ["playback_plan_v3"],
              outcome: "terminal",
              terminal: {
                reason: "subtitle_burn_in_source_unsupported",
                message: "The selected subtitle can't be burned into this source.",
                retryable: false,
              },
            },
            201,
          );
        }
        return jsonResponse(
          playable(
            "session-2",
            directPlan({
              session_id: "session-2",
              plan_id: "plan:session-2",
              subtitle: { mode: "off", inventory: [burnIn] },
            }),
          ),
          201,
        );
      },
      replan: (url) =>
        // After the restart the burned-in track is refused on session-2 too;
        // before it, the server has lost session-1.
        url.includes("/session-2/")
          ? jsonResponse({
              protocol_version: 3,
              server_features: ["playback_plan_v3"],
              outcome: "terminal",
              terminal: {
                reason: "subtitle_burn_in_source_unsupported",
                message: "The selected subtitle can't be burned into this source.",
                retryable: false,
              },
            })
          : jsonResponse({ error: "playback_session_not_found" }, 404),
    });

    renderHarness({ subtitleMode: "always", preferredSubtitleLanguage: "eng" });
    const video = await startWatching();
    await waitFor(() => expect(controls.current?.activeSubtitleIndex).toBe(3));
    expect(requests.replans).toHaveLength(0);

    // The server restarts mid-stream.
    Object.defineProperty(video, "error", {
      configurable: true,
      value: { code: 2, message: "PIPELINE_ERROR_NETWORK" },
    });
    fireEvent.error(video);
    await waitFor(() => expect(sessionRef.current?.sessionId).toBe("session-2"), {
      timeout: 4_000,
    });
    // Let every effect of the handover settle.
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 50));
    });

    expect(requests.starts.map((body) => body.subtitle_track_index)).toEqual([
      undefined,
      3,
      undefined,
    ]);
    expect(requests.replans.filter(({ url }) => url.includes("/session-2/"))).toEqual([]);
    expect(controls.current?.activeSubtitleIndex).toBeNull();
    expect(sessionRef.current?.connectionStatus).toBe("connected");
    expect(sessionRef.current?.error).toBeNull();
    expect(sessionRef.current?.initialSubtitleErrorTitle).toBe("That subtitle track can't be used");
  });

  it("restarts a mid-stream eviction at the current position", async () => {
    const requests = stubServer({
      start: (_body, count) =>
        count === 1
          ? jsonResponse(playable("session-1", directPlan()), 201)
          : jsonResponse(
              playable(
                "session-2",
                directPlan({ session_id: "session-2", plan_id: "plan:session-2" }),
              ),
              201,
            ),
      // The old session is gone: the reconnect's replan cannot succeed, so it
      // falls through to a fresh start.
      replan: () => jsonResponse({ error: "playback_session_not_found" }, 404),
    });

    renderHarness();
    const video = await startWatching();

    // The server evicts the session while the viewer is at 321.5s.
    video.currentTime = 321.5;
    act(() => progressOptions.current?.onSessionLost?.());

    await waitFor(() => expect(sessionRef.current?.sessionId).toBe("session-2"), {
      timeout: 4_000,
    });
    // The restart preserves the viewer's place, and playback resumes.
    expect(requests.starts[1]?.start_position).toBe(321.5);
    expect(sessionRef.current?.connectionStatus).toBe("connected");
    expect(sessionRef.current?.shouldAutoPlay).toBe(true);
  });
});

describe("VideoPlayer with the playback session: audio switch", () => {
  it("ends an audio switch on the remux route playing", async () => {
    const first = remuxPlan();
    const switched = remuxPlan({
      plan_id: "plan:switched",
      plan_attempt_key: "v3:switched",
      selected_tracks: {
        audio: { id: "file:7:audio:1", index: 1 },
        subtitle: { id: "file:7:subtitle:0", index: 0 },
      },
      timeline: {
        ...first.timeline,
        source_start_seconds: 12,
        stream_origin_seconds: 10,
        player_start_seconds: 2,
        timeline_offset_seconds: 10,
      },
    });
    const requests = stubServer({
      start: () => jsonResponse(playable("session-1", first), 201),
      replan: () => jsonResponse(playable("session-1", switched)),
    });
    renderHarness();
    const video = await startWatching();
    expect(controls.current?.playing).toBe(true);
    vi.mocked(video.load).mockClear();
    vi.mocked(HTMLMediaElement.prototype.play).mockClear();

    act(() => sessionRef.current?.switchAudioTrack(1, 12));
    await waitFor(() => expect(requests.replans[0]?.body.operation).toBe("track_change"));
    await waitFor(() => expect(video.load).toHaveBeenCalled());
    expect(sessionRef.current?.plan?.selected_tracks.audio?.index).toBe(1);

    // The switch tore the old transport down; the load algorithm pauses the
    // element, and the replacement starts from the viewer's live intent.
    Object.defineProperty(video, "paused", { configurable: true, value: true });
    fireEvent.emptied(video);
    Object.defineProperty(video, "readyState", {
      configurable: true,
      value: HTMLMediaElement.HAVE_FUTURE_DATA,
    });
    fireEvent.loadedData(video);
    fireEvent.canPlay(video);
    await waitFor(() => expect(HTMLMediaElement.prototype.play).toHaveBeenCalled());

    Object.defineProperty(video, "paused", { configurable: true, value: false });
    fireEvent.play(video);
    expect(video.paused).toBe(false);
    expect(controls.current?.playing).toBe(true);
  });
});

describe("VideoPlayer with the playback session: a replacement's start", () => {
  /**
   * Records every write to the element's clock. jsdom has no media pipeline, so
   * a seek never fires `seeking`; the write is what the player's own start
   * leaves behind, and counting it is how the tests see a single seek.
   */
  function countClockWrites(video: HTMLVideoElement): number[] {
    const writes: number[] = [];
    let current = video.currentTime;
    Object.defineProperty(video, "currentTime", {
      configurable: true,
      get: () => current,
      set: (next: number) => {
        current = next;
        writes.push(next);
      },
    });
    return writes;
  }

  function seekableRange(end: number): TimeRanges {
    return { length: 1, start: () => 0, end: () => end } as unknown as TimeRanges;
  }

  /**
   * Starts a remux session and switches audio through a replan the test
   * releases by hand, so the outgoing transport keeps playing for as long as a
   * slow anchor probe would take. Returns the element and the writes made to
   * its clock once the replacement's stream has been given to it.
   */
  async function switchAudioAfterASlowReplan() {
    const first = remuxPlan();
    // The new plan starts where the viewer was when the switch was requested
    // (source 12, anchored at keyframe 10): two seconds of copied pre-roll.
    const switched = remuxPlan({
      plan_id: "plan:switched",
      plan_attempt_key: "v3:switched",
      selected_tracks: {
        audio: { id: "file:7:audio:1", index: 1 },
        subtitle: { id: "file:7:subtitle:0", index: 0 },
      },
      timeline: {
        ...first.timeline,
        source_start_seconds: 12,
        stream_origin_seconds: 10,
        player_start_seconds: 2,
        timeline_offset_seconds: 10,
      },
    });
    let releaseReplan: (() => void) | null = null;
    const replanGate = new Promise<void>((resolve) => {
      releaseReplan = resolve;
    });
    const requests = stubServer({
      start: () => jsonResponse(playable("session-1", first), 201),
      replan: async () => {
        await replanGate;
        return jsonResponse(playable("session-1", switched));
      },
    });
    renderHarness();
    const video = await startWatching();
    vi.mocked(video.load).mockClear();

    act(() => sessionRef.current?.switchAudioTrack(1, 12));
    await waitFor(() => expect(requests.replans[0]?.body.operation).toBe("track_change"));
    // The probe is still running and the outgoing transport plays on, so the
    // element's live clock is well past the plan start.
    video.currentTime = 12.9;
    act(() => releaseReplan?.());

    await waitFor(() => expect(video.load).toHaveBeenCalled());
    expect(sessionRef.current?.plan?.selected_tracks.audio?.index).toBe(1);
    return video;
  }

  /**
   * The element's load algorithm ran: emptied paused it, and the clock is back
   * at the stream's own first frame. A browser discards the pre-metadata start
   * position here, which is why the plan's start has to be applied after
   * metadata.
   */
  function loadFirstFrame(video: HTMLVideoElement, seekableEnd: number | null) {
    Object.defineProperty(video, "paused", { configurable: true, value: true });
    fireEvent.emptied(video);
    Object.defineProperty(video, "currentTime", {
      configurable: true,
      writable: true,
      value: 0,
    });
    if (seekableEnd !== null) {
      Object.defineProperty(video, "seekable", {
        configurable: true,
        value: seekableRange(seekableEnd),
      });
    }
    Object.defineProperty(video, "readyState", {
      configurable: true,
      value: HTMLMediaElement.HAVE_METADATA,
    });
  }

  it("lands a replacement on the plan start with one seek when the element can serve it", async () => {
    const video = await switchAudioAfterASlowReplan();
    // This replacement's element can seek its stream (a byte-addressable
    // route, or a browser that reports the buffered range as seekable).
    loadFirstFrame(video, 60);
    const writes = countClockWrites(video);
    fireEvent.loadedMetadata(video);

    expect(video.currentTime).toBe(2);
    expect(writes).toEqual([2]);

    Object.defineProperty(video, "readyState", {
      configurable: true,
      value: HTMLMediaElement.HAVE_FUTURE_DATA,
    });
    fireEvent.loadedData(video);
    fireEvent.canPlay(video);
    await waitFor(() => expect(HTMLMediaElement.prototype.play).toHaveBeenCalled());
    Object.defineProperty(video, "paused", { configurable: true, value: false });
    fireEvent.play(video);

    // The probe's delay did not move the start: the plan's own start did.
    expect(video.currentTime).toBe(2);
    expect(writes).toEqual([2]);
    expect(controls.current?.playing).toBe(true);
  });

  it("clamps a plan start behind the stream's first frame instead of seeking back", async () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    const video = await switchAudioAfterASlowReplan();
    loadFirstFrame(video, 60);
    // This response begins after the plan's start: its origin is already past
    // the position the plan names, so the only frame it can show is the origin.
    video.currentTime = 5;
    const writes = countClockWrites(video);
    fireEvent.loadedMetadata(video);

    expect(writes).toEqual([]);
    expect(video.currentTime).toBe(5);
    expect(warn.mock.calls.some((call) => String(call[0]).includes("begins after"))).toBe(true);
    warn.mockRestore();
  });

  it("keeps the stream's first frame and issues no seek on a forward-only replacement", async () => {
    const info = vi.spyOn(console, "info").mockImplementation(() => {});
    const video = await switchAudioAfterASlowReplan();
    // A progressive remux reports nothing seekable: its one response has no
    // byte range that maps back to a media position, so a seek would replay
    // it from the origin instead of landing on the plan start.
    loadFirstFrame(video, null);
    const writes = countClockWrites(video);
    fireEvent.loadedMetadata(video);
    Object.defineProperty(video, "readyState", {
      configurable: true,
      value: HTMLMediaElement.HAVE_FUTURE_DATA,
    });
    fireEvent.loadedData(video);
    fireEvent.canPlay(video);
    await waitFor(() => expect(HTMLMediaElement.prototype.play).toHaveBeenCalled());
    Object.defineProperty(video, "paused", { configurable: true, value: false });
    fireEvent.play(video);

    expect(writes).toEqual([]);
    expect(video.currentTime).toBe(0);
    expect(controls.current?.playing).toBe(true);
    expect(info.mock.calls.some((call) => String(call[0]).includes("not seekable"))).toBe(true);
    info.mockRestore();
  });
});

describe("VideoPlayer with the playback session: version projection", () => {
  it("does not reload the media element when a version check restamps the list", async () => {
    const requests = stubServer({
      start: () => jsonResponse(playable("session-1", directPlan()), 201),
      replan: () => jsonResponse(playable("session-1", directPlan()), 200),
    });
    const versions: PlayerFileVersion[] = [
      {
        file_id: 7,
        file_path: "/release-a.mkv",
        resolution: "1080p",
        codec_video: "h264",
        codec_audio: "aac",
        hdr: false,
        container: "mkv",
        file_size: 1,
        duration: 3600,
        bitrate: 1,
      },
    ];
    const view = renderHarness({ versions });
    const video = await startWatching();
    const src = video.getAttribute("src");
    const loadCalls = vi.mocked(HTMLMediaElement.prototype.load).mock.calls.length;

    // The catalog's version-liveness check answered: the same rows, now
    // carrying the verdict it stamped on them. That is a new list identity
    // arriving while the transport plays.
    view.rerender(
      createElement(Harness, {
        versions: [{ ...versions[0]!, available: true }],
      }),
    );
    await act(async () => {});

    expect(video.getAttribute("src")).toBe(src);
    expect(document.querySelector("video")).toBe(video);
    expect(vi.mocked(HTMLMediaElement.prototype.load).mock.calls.length).toBe(loadCalls);
    expect(requests.starts).toHaveLength(1);
    expect(requests.replans).toHaveLength(0);
    expect(sessionRef.current?.planRevision).toBe(1);
  });
});

describe("VideoPlayer with the playback session: transport identity", () => {
  it("initialises one transport per plan when the host re-renders", async () => {
    const requests = stubServer({
      start: () => jsonResponse(playable("session-1", directPlan()), 201),
      replan: () => jsonResponse(playable("session-1", directPlan()), 200),
    });
    const view = renderHarness();
    const video = await startWatching();
    const src = video.getAttribute("src");
    const loadCalls = vi.mocked(HTMLMediaElement.prototype.load).mock.calls.length;

    // WatchPlaybackChrome rebuilds the pre-play subtitle index maps on every
    // host render. As `loadSession` dependencies they re-keyed `onPlanFailure`,
    // which the transport effect lists, so each render tore the media element
    // down and reloaded the stream from the plan's origin.
    view.rerender(createElement(Harness, { initialSubtitleTrackIndexByFileId: { 7: 0 } }));
    await act(async () => {});
    view.rerender(createElement(Harness, { initialSubtitleTrackIndexByFileId: { 7: 1 } }));
    await act(async () => {});

    expect(video.getAttribute("src")).toBe(src);
    expect(document.querySelector("video")).toBe(video);
    expect(vi.mocked(HTMLMediaElement.prototype.load).mock.calls.length).toBe(loadCalls);
    expect(sessionRef.current?.planRevision).toBe(1);
    expect(requests.starts).toHaveLength(1);
    expect(requests.replans).toHaveLength(0);
  });
});

describe("VideoPlayer with the playback session: transport identity", () => {
  it("keeps the HLS transport for a track_change the server answers on the same stream", async () => {
    vi.spyOn(HTMLMediaElement.prototype, "canPlayType").mockReturnValue("");
    const hlsPlan = fixturePlanV3({
      plan_id: "plan:hls",
      plan_attempt_key: "v3:hls",
      stream: {
        url: "/playback/transcode/session-1/master.m3u8",
        protocol: "hls",
        mime_type: "application/vnd.apple.mpegurl",
        headers: {},
        header_refresh: "none",
      },
    });
    // The server kept the running generation: same stream, same route, same
    // audio selection, only the plan's own start follows the live playhead.
    const sameStreamPlan = fixturePlanV3({
      plan_id: "plan:hls-track",
      plan_attempt_key: "v3:hls-track",
      stream: hlsPlan.stream,
      timeline: { ...hlsPlan.timeline, player_start_seconds: 150 },
    });
    const requests = stubServer({
      start: () => jsonResponse(playable("session-1", directPlan()), 201),
      replan: (_url, body) =>
        jsonResponse(
          playable("session-1", body.operation === "quality_change" ? hlsPlan : sameStreamPlan),
        ),
    });

    renderHarness();
    const video = await startWatching();
    act(() => sessionRef.current?.changeQuality("720p", 100));
    await waitFor(() => expect(hlsJS.latest).not.toBeNull());
    expect(hlsJS.loadSource).toHaveBeenCalledTimes(1);
    vi.mocked(HTMLMediaElement.prototype.load).mockClear();
    const src = video.getAttribute("src");

    // Subtitles off on a plan that shows none is a track change that changes
    // nothing: the A/V transport is reused, so the element must not reload and
    // the manifest must not be fetched again.
    act(() => sessionRef.current?.changeSubtitleTrack(null, 150));
    await waitFor(() =>
      expect(requests.replans.map(({ body }) => body.operation)).toEqual([
        "quality_change",
        "track_change",
      ]),
    );
    await waitFor(() => expect(sessionRef.current?.planRevision).toBe(3));
    await act(async () => Promise.resolve());

    expect(sessionRef.current?.transportRevision).toBe(2);
    expect(hlsJS.loadSource).toHaveBeenCalledTimes(1);
    expect(HTMLMediaElement.prototype.load).not.toHaveBeenCalled();
    expect(video.getAttribute("src")).toBe(src);
  });
});

describe("VideoPlayer with the playback session: a refused seek retry", () => {
  const PRE_SEEK_POSITION = 32.172995;
  const SEEK_TARGET = 918.161458;

  function seekRefusal(sessionId: string) {
    return {
      protocol_version: 3,
      server_features: ["playback_plan_v3"],
      outcome: "adaptation_unavailable",
      session_id: sessionId,
      terminal: {
        reason: "seek_reanchor_evidence_unavailable",
        message: "The frozen playback source could not be reopened; start a new playback attempt.",
        retryable: true,
      },
    };
  }

  /** Answers the player's automatic non-seek replans without touching the plan. */
  function seekReplan(url: string, body: Body) {
    if (body.operation === "seek_reanchor") {
      return jsonResponse(seekRefusal(/playback\/([^/]+)\/replan/.exec(url)?.[1] ?? "session-1"));
    }
    return jsonResponse(playable("session-1", retryablePlan()));
  }

  /** A remux plan whose window makes the far seek a server reanchor. */
  function retryablePlan(sessionId = "session-1", overrides: Partial<PlanV3> = {}): PlanV3 {
    return remuxPlan({
      effective_virtual_uri: "virtual://movie/x?result=1",
      session_id: sessionId,
      selected_tracks: { audio: { id: "file:7:audio:0", index: 0 } },
      ...overrides,
      stream: {
        ...remuxPlan().stream,
        url: `/stream/${sessionId}`,
      },
    });
  }

  async function showSeekCard(requests: Requests, video: HTMLVideoElement) {
    // The viewer was 32 s in when they asked for 918 s. The remux window only
    // reaches 300 s, so the player asks the server for a new window.
    Object.defineProperty(video, "currentTime", {
      configurable: true,
      writable: true,
      value: PRE_SEEK_POSITION - remuxPlan().timeline.timeline_offset_seconds,
    });
    showFrame(video);
    seekableWindow(video, 300);

    await act(async () => {
      controls.current?.onSeek?.(SEEK_TARGET);
      // Let the replan settle inside act, so the card's state is the one the
      // next render reads rather than a pending update from outside it.
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
    await waitFor(() =>
      expect(requests.replans.some(({ body }) => body.operation === "seek_reanchor")).toBe(true),
    );
    const seekRequest = requests.replans.find(({ body }) => body.operation === "seek_reanchor");
    expect(seekRequest?.body).toMatchObject({
      operation: "seek_reanchor",
      position_seconds: SEEK_TARGET,
    });
    await waitFor(() => expect(screen.getByText("Try again")).toBeInTheDocument());
  }

  it("restores the pre-seek position on the card and retries at the seek target", async () => {
    const requests = stubServer({
      start: () => jsonResponse(playable("session-1", retryablePlan()), 201),
      replan: seekReplan,
    });

    renderHarness();
    const video = await startWatching();
    await showSeekCard(requests, video);

    // The card shows where the viewer was, not the target that never played.
    expect(controls.current?.currentTime).toBeCloseTo(PRE_SEEK_POSITION, 3);

    fireEvent.click(screen.getByText("Try again"));
    await waitFor(() => expect(requests.starts).toHaveLength(2));
    expect(requests.starts[1]?.start_position).toBeCloseTo(SEEK_TARGET, 3);
    expect(requests.starts[1]?.file_id).toBe(7);
  });

  it("plays a successful retry at the target and advances from there", async () => {
    const requests = stubServer({
      start: (body, count) => {
        if (count === 1) return jsonResponse(playable("session-1", retryablePlan()), 201);
        const target = typeof body.start_position === "number" ? body.start_position : 0;
        return jsonResponse(
          playable(
            "session-2",
            retryablePlan("session-2", {
              plan_id: "plan:seek-retry",
              plan_attempt_key: "v3:seek-retry",
              timeline: {
                ...remuxPlan().timeline,
                source_start_seconds: target,
                stream_origin_seconds: target,
                player_start_seconds: target,
                timeline_offset_seconds: 0,
              },
            }),
          ),
          201,
        );
      },
      replan: seekReplan,
    });

    renderHarness();
    const video = await startWatching();
    await showSeekCard(requests, video);

    fireEvent.click(screen.getByText("Try again"));
    await waitFor(() => expect(sessionRef.current?.plan?.session_id).toBe("session-2"));
    expect(requests.starts[1]?.start_position).toBeCloseTo(SEEK_TARGET, 3);
    expect(sessionRef.current?.plan?.timeline.player_start_seconds).toBeCloseTo(SEEK_TARGET, 3);
    expect(sessionRef.current?.shouldAutoPlay).toBe(true);

    const retried = await waitFor(() => {
      const element = document.querySelector("video");
      expect(element?.getAttribute("src")).toContain("session-2");
      return element as HTMLVideoElement;
    });
    Object.defineProperty(retried, "currentTime", {
      configurable: true,
      writable: true,
      value: SEEK_TARGET + 1.5,
    });
    showFrame(retried);
    expect(controls.current?.currentTime).toBeGreaterThan(SEEK_TARGET);
  });

  it("says the target cannot be honoured when the source moved with the card up", async () => {
    const requests = stubServer({
      start: () => jsonResponse(playable("session-1", retryablePlan()), 201),
      replan: seekReplan,
    });

    renderHarness();
    const video = await startWatching();
    await showSeekCard(requests, video);

    // A serve-layer rotation moves the live candidate without adopting a new
    // plan. The target belongs to the outgoing source, so the retry must not
    // send it to this one.
    act(() =>
      sessionRef.current?.applyCommittedSource(
        {
          effectiveMediaFileId: 7,
          effectiveVirtualUri: "virtual://movie/x?result=2",
        },
        [],
      ),
    );
    fireEvent.click(screen.getByText("Try again"));

    await waitFor(() =>
      expect(sessionRef.current?.errorReason).toBe("seek_retry_target_unavailable"),
    );
    expect(sessionRef.current?.errorRetryable).toBe(false);
    expect(requests.starts).toHaveLength(1);
    await waitFor(() => expect(screen.getByText(/source changed/i)).toBeInTheDocument());
  });
});
