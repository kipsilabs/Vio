import { act, renderHook, waitFor } from "@testing-library/react";
import { createElement, type ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { PlayerConfigProvider, type PlayerConfig } from "../context/PlayerConfigContext";
import {
  fixtureClientCapabilitiesV3,
  fixtureClientPlaybackContextV3,
  fixturePlanV3,
} from "../protocol-v3.fixtures";
import {
  buildReplanRequestV3,
  buildStartRequestV3,
  routeEventPlanIdentityV3,
  VIDEO_CLIENT_FEATURES_V3,
} from "../playback-session-wire-v3";
import { markPlaybackIntent } from "../first-frame";
import type { PlayerAudioTrack } from "../types";
import { usePlaybackSession } from "./usePlaybackSession";
import { resetCodecDetectionForTests } from "./useCodecDetection";
import { resetSessionMutations } from "../session-mutations";

// These hook tests exercise plan adoption and replacement against a transport
// boundary. The v2 start/replan helpers are covered by their own tests; here
// they forward to the same fetch stubs under the v2 base.
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

function wrapper({ children }: { children: ReactNode }) {
  return createElement(PlayerConfigProvider, { config: playerConfig, children });
}

function jsonResponse(body: unknown, init: ResponseInit = {}) {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { "Content-Type": "application/json" },
    ...init,
  });
}

afterEach(() => {
  resetCodecDetectionForTests();
  resetSessionMutations();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

const startBase = {
  fileId: 42,
  profileId: "profile-1",
  playbackAttemptId: "attempt-0123456789",
  qualityPreference: "auto",
  position: 0,
  forceStartPosition: false,
  metered: false,
  clientCapabilities: fixtureClientCapabilitiesV3(),
  clientPlaybackContext: fixtureClientPlaybackContextV3(),
};

const replanBase = {
  plan: fixturePlanV3(),
  playbackAttemptId: "attempt-0123456789",
  replanRequestId: "replan-0123456789",
  planAttemptId: "plan-attempt-0123456789",
  qualityPreference: "auto",
  positionSeconds: 120,
  attemptedPlanKeys: [],
  attemptCount: 1,
  metered: false,
  clientCapabilities: fixtureClientCapabilitiesV3(),
  clientPlaybackContext: fixtureClientPlaybackContextV3(),
};

describe("buildStartRequestV3", () => {
  it("pins the room source without changing the requested streaming quality", () => {
    expect(
      buildStartRequestV3({
        ...startBase,
        allowAlternateVersions: false,
        qualityPreference: "720p",
      }),
    ).toMatchObject({ allow_alternate_versions: false, quality_preference: "720p" });
    expect(buildStartRequestV3(startBase)).not.toHaveProperty("allow_alternate_versions");
  });

  // Feature tokens are promises the server enforces, so a surface advertises
  // only what it implements: the base set alone unless the caller names more.
  it("advertises only the surface's own features", () => {
    expect(
      buildStartRequestV3({ ...startBase, extraClientFeatures: VIDEO_CLIENT_FEATURES_V3 })
        .client_features,
    ).toEqual([
      "playback_plan_v3",
      "plan_invalidated_v1",
      "source_committed_event_v1",
      "inventory_updated_event_v1",
    ]);
    expect(buildStartRequestV3(startBase).client_features).toEqual(["playback_plan_v3"]);
  });

  it("declares the protocol version and the plan feature", () => {
    expect(buildStartRequestV3(startBase)).toMatchObject({
      protocol_version: 3,
      client_features: ["playback_plan_v3"],
      file_id: 42,
      profile_id: "profile-1",
      playback_attempt_id: "attempt-0123456789",
      subtitle_fidelity_preference: "preserve",
    });
  });

  it("includes an explicit zero start position when forced", () => {
    expect(
      buildStartRequestV3({ ...startBase, position: 0, forceStartPosition: true }),
    ).toMatchObject({ start_position: 0 });
  });

  it("declares client-owned progress with an explicit zero anchor", () => {
    expect(
      buildStartRequestV3({
        ...startBase,
        position: 0,
        forceStartPosition: true,
        progressPersistence: "client",
      }),
    ).toMatchObject({ start_position: 0, progress_persistence: "client" });
  });

  it("omits the start position when playback should resume normally", () => {
    expect(buildStartRequestV3(startBase)).not.toHaveProperty("start_position");
  });

  it("clamps an absurd start position to the contract bound", () => {
    expect(buildStartRequestV3({ ...startBase, position: 1e12 })).toMatchObject({
      start_position: 31_536_000,
    });
  });

  it("includes an explicit audio track override when present", () => {
    expect(buildStartRequestV3({ ...startBase, explicitAudioTrackIndex: 2 })).toMatchObject({
      audio_track_index: 2,
    });
  });

  it("maps a carried audio track id onto carried_audio_track_id", () => {
    expect(
      buildStartRequestV3({ ...startBase, carriedAudioTrackID: "file:7:audio:0" }),
    ).toMatchObject({ carried_audio_track_id: "file:7:audio:0" });
  });

  it("omits carried_audio_track_id when no audio track is carried", () => {
    expect(buildStartRequestV3(startBase)).not.toHaveProperty("carried_audio_track_id");
  });

  it("emits file_selection when the file was explicitly chosen", () => {
    expect(buildStartRequestV3({ ...startBase, fileSelection: "explicit" })).toMatchObject({
      file_selection: "explicit",
    });
  });

  it("omits file_selection when the file choice is server-owned", () => {
    expect(buildStartRequestV3(startBase)).not.toHaveProperty("file_selection");
  });

  it("includes the resolved subtitle track in the initial request", () => {
    expect(buildStartRequestV3({ ...startBase, subtitleTrackIndex: 0 })).toMatchObject({
      subtitle_track_index: 0,
    });
  });

  it("omits the bandwidth estimate when the browser reports none", () => {
    expect(buildStartRequestV3({ ...startBase, bandwidthEstimateKbps: null })).not.toHaveProperty(
      "bandwidth_estimate_kbps",
    );
  });

  it("sends the user bandwidth ceiling separately from the network estimate", () => {
    expect(
      buildStartRequestV3({
        ...startBase,
        bandwidthEstimateKbps: 25_000,
        bandwidthCapKbps: 6_000,
      }),
    ).toMatchObject({ bandwidth_estimate_kbps: 25_000, bandwidth_cap_kbps: 6_000 });
  });

  it("sends the quality preference verbatim for the server to normalize", () => {
    expect(buildStartRequestV3({ ...startBase, qualityPreference: "original" })).toMatchObject({
      quality_preference: "original",
    });
  });

  it("emits force_relink when forceRelink is true", () => {
    expect(buildStartRequestV3({ ...startBase, forceRelink: true })).toMatchObject({
      force_relink: true,
    });
  });

  it("omits force_relink when forceRelink is absent", () => {
    expect(buildStartRequestV3(startBase)).not.toHaveProperty("force_relink");
  });
});

describe("buildReplanRequestV3", () => {
  it("echoes the plan's identity so the server can detect a stale plan", () => {
    expect(buildReplanRequestV3({ ...replanBase, operation: "track_change" })).toMatchObject({
      protocol_version: 3,
      operation: "track_change",
      failed_plan_id: "plan:0123456789abcdef",
      plan_attempt_key: "v3:0123456789abcdef",
      position_seconds: 120,
    });
  });

  // A replan that sends `client_features` replaces the negotiated list, so a
  // replan which advertised less than the start did would silently withdraw the
  // promise the server gates the invalidation command on.
  it("re-advertises the same features a start negotiated", () => {
    expect(
      buildReplanRequestV3({
        ...replanBase,
        operation: "failure_recovery",
        extraClientFeatures: VIDEO_CLIENT_FEATURES_V3,
      }).client_features,
    ).toEqual([
      "playback_plan_v3",
      "plan_invalidated_v1",
      "source_committed_event_v1",
      "inventory_updated_event_v1",
    ]);
  });

  it("re-arms the server's auto fallback when the viewer selects Auto", () => {
    expect(
      buildReplanRequestV3({ ...replanBase, operation: "failure_recovery", autoFallback: true }),
    ).toMatchObject({ auto_fallback: true });
  });

  it("omits auto_fallback when the viewer's intent is unknown", () => {
    expect(
      buildReplanRequestV3({ ...replanBase, operation: "failure_recovery" }),
    ).not.toHaveProperty("auto_fallback");
  });

  it("names a new audio track by index alone", () => {
    // An empty id makes the server resolve the ordinal against the *effective*
    // file, which the client cannot name: it changes on a version fallback.
    const body = buildReplanRequestV3({
      ...replanBase,
      operation: "track_change",
      audio: { id: "", index: 3 },
    });

    expect(body.selected_tracks.audio).toEqual({ id: "", index: 3 });
  });

  it("resends the untouched subtitle track on an audio-only change", () => {
    const plan = fixturePlanV3({
      selected_tracks: {
        audio: { id: "file:7:audio:0", index: 0 },
        subtitle: { id: "file:7:subtitle:2", index: 2 },
      },
    });

    // Omitting the subtitle would read as "subtitles off", not "unchanged".
    const body = buildReplanRequestV3({
      ...replanBase,
      plan,
      operation: "track_change",
      audio: { id: "", index: 1 },
    });

    expect(body.selected_tracks.subtitle).toEqual({ id: "file:7:subtitle:2", index: 2 });
  });

  it("clears the subtitle selection when the subtitle override is null", () => {
    const plan = fixturePlanV3({
      selected_tracks: {
        audio: { id: "file:7:audio:0", index: 0 },
        subtitle: { id: "file:7:subtitle:2", index: 2 },
      },
    });

    const body = buildReplanRequestV3({
      ...replanBase,
      plan,
      operation: "track_change",
      subtitle: null,
    });

    expect(body.selected_tracks).not.toHaveProperty("subtitle");
    expect(body.selected_tracks.audio).toEqual({ id: "file:7:audio:0", index: 0 });
  });

  it("echoes the plan's tracks byte-for-byte on a seek reanchor", () => {
    const plan = fixturePlanV3({
      selected_tracks: {
        audio: { id: "file:7:audio:1", index: 1 },
        subtitle: { id: "file:7:subtitle:0", index: 0 },
      },
    });

    // Seek recovery is validated against the current plan's tracks exactly, so
    // the shorthand identity used for a track change would be rejected here.
    const body = buildReplanRequestV3({
      ...replanBase,
      plan,
      operation: "seek_reanchor",
      positionSeconds: 900,
    });

    expect(body.selected_tracks).toEqual(plan.selected_tracks);
    expect(body).not.toHaveProperty("failure");
  });

  it("carries the loop guard and the failure classification on a recovery", () => {
    const body = buildReplanRequestV3({
      ...replanBase,
      operation: "failure_recovery",
      attemptedPlanKeys: ["v3:aaaaaaaaaaaaaaaa"],
      attemptCount: 2,
      failure: { classification: "decoder_error", message: "no decoder" },
    });

    expect(body).toMatchObject({
      operation: "failure_recovery",
      attempted_plan_keys: ["v3:aaaaaaaaaaaaaaaa"],
      attempt_count: 2,
      failure: { classification: "decoder_error", message: "no decoder" },
    });
  });

  it("omits failure when nothing failed", () => {
    const body = buildReplanRequestV3({ ...replanBase, operation: "quality_change" });

    expect(body).not.toHaveProperty("failure");
    expect(body.attempted_plan_keys).toEqual([]);
    expect(body.attempt_count).toBe(1);
  });

  it("sends the quality preference on a track change so it is not reset", () => {
    // On a track change an absent preference *keeps* the current quality, but
    // sending the current value is behaviourally identical and unambiguous.
    const body = buildReplanRequestV3({
      ...replanBase,
      operation: "track_change",
      qualityPreference: "original",
    });

    expect(body.quality_preference).toBe("original");
  });

  it("preserves the user bandwidth ceiling across replans", () => {
    const body = buildReplanRequestV3({
      ...replanBase,
      operation: "failure_recovery",
      bandwidthCapKbps: 4_000,
      failure: { classification: "playback_error" },
    });

    expect(body.bandwidth_cap_kbps).toBe(4_000);
  });
});

describe("routeEventPlanIdentityV3", () => {
  it("omits every plan-scoped field for a terminal start", () => {
    expect(routeEventPlanIdentityV3(null, null, "plan-attempt-client-only")).toEqual({});
  });

  it("includes the complete identity after a plan is adopted", () => {
    const plan = fixturePlanV3();
    expect(routeEventPlanIdentityV3(plan, "session-1", "plan-attempt-1")).toEqual({
      sessionId: "session-1",
      planId: plan.plan_id,
      planAttemptId: "plan-attempt-1",
      planAttemptKey: plan.plan_attempt_key,
    });
  });
});

describe("usePlaybackSession quality changes", () => {
  it("rolls back a rejected quality and keeps it out of later replans", async () => {
    const plan = fixturePlanV3();
    let replanCount = 0;
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: plan,
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/session-1/replan")) {
        replanCount += 1;
        if (replanCount === 1) {
          return jsonResponse({ message: "temporary failure" }, { status: 500 });
        }
        return jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-1",
          playback_plan: fixturePlanV3({
            plan_id: "plan:fedcba9876543210",
            plan_attempt_key: "v3:fedcba9876543210",
          }),
        });
      }
      if (url.endsWith("/playback/route-events")) {
        return new Response(null, { status: 202 });
      }
      if (init?.method === "DELETE") {
        return jsonResponse({ outcome: "stopped" });
      }
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "original"),
      { wrapper },
    );

    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() => result.current.changeQuality("720p", 120));
    await waitFor(() => {
      expect(result.current.replanning).toBe(false);
      expect(result.current.qualityPreference).toBe("original");
      expect(result.current.error).toBeTruthy();
    });

    act(() => {
      // Deliberately not awaited: these tests drive the replan manually so the
      // promise must not block `act` on a queued/in-flight replan.
      void result.current.refreshSubtitles(120);
    });
    await waitFor(() => expect(replanCount).toBe(2));

    const replanBodies = fetchMock.mock.calls
      .filter(([url]) => String(url).endsWith("/playback/session-1/replan"))
      .map(([, init]) => JSON.parse(String(init?.body)) as { quality_preference: string });
    expect(replanBodies.map((body) => body.quality_preference)).toEqual(["720p", "original"]);

    unmount();
  });

  it("sets replanningQuality only for quality/output replans and resets it on adoption", async () => {
    const plan = fixturePlanV3();
    let replanCount = 0;
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: plan,
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/session-1/replan")) {
        replanCount += 1;
        return jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-1",
          playback_plan: fixturePlanV3({
            plan_id: `plan:replan-${replanCount}`,
            plan_attempt_key: `v3:replan-${replanCount}`,
          }),
        });
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());
    expect(result.current.replanningQuality).toBe(false);

    // A quality change lights the quality flag while in flight.
    act(() => result.current.changeQuality("720p", 120));
    expect(result.current.replanning).toBe(true);
    expect(result.current.replanningQuality).toBe(true);
    await waitFor(() => expect(result.current.replanningQuality).toBe(false));

    // A track change does not.
    act(() => result.current.switchAudioTrack(1, 120));
    expect(result.current.replanning).toBe(true);
    expect(result.current.replanningQuality).toBe(false);
    await waitFor(() => expect(result.current.replanning).toBe(false));

    unmount();
  });
});

describe("usePlaybackSession initial bitmap subtitles", () => {
  it("adopts a successful bitmap subtitle start without replanning", async () => {
    const subtitlePlan = fixturePlanV3({
      session_id: "session-1",
      plan_id: "plan:bitmap-subtitle",
      plan_attempt_key: "v3:bitmap-subtitle",
      selected_tracks: {
        audio: { id: "file:7:audio:0", index: 0 },
        subtitle: { id: "file:7:subtitle:0", index: 0 },
      },
    });
    const startBodies: Array<Record<string, unknown>> = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        startBodies.push(JSON.parse(String(init?.body)) as Record<string, unknown>);
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: subtitlePlan,
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return jsonResponse({ outcome: "stopped" });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () =>
        usePlaybackSession(
          "request-1",
          [],
          [],
          7,
          0,
          false,
          "auto",
          null,
          undefined,
          null,
          { 7: 0 },
          { 7: 0 },
        ),
      { wrapper },
    );

    await waitFor(() => expect(result.current.plan?.plan_id).toBe("plan:bitmap-subtitle"));
    expect(startBodies).toHaveLength(1);
    expect(startBodies[0]).toMatchObject({ subtitle_track_index: 0 });
    expect(fetchMock.mock.calls.some(([url]) => String(url).includes("/replan"))).toBe(false);
    expect(result.current.planRevision).toBe(1);
    expect(result.current.initialSubtitleError).toBeNull();
    unmount();
  });

  it("retries a refused bitmap subtitle start without subtitles", async () => {
    const fallbackPlan = fixturePlanV3({ session_id: "session-2" });
    const startBodies: Array<Record<string, unknown>> = [];
    let replanCount = 0;
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        startBodies.push(JSON.parse(String(init?.body)) as Record<string, unknown>);
        if (startBodies.length === 1) {
          return jsonResponse({
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "terminal",
            session_id: "session-1",
            terminal: {
              reason: "hdr_transcode_unsupported",
              message: "Enable HDR transcoding to use this subtitle.",
            },
          });
        }
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-2",
            playback_plan: fallbackPlan,
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/session-2/replan")) {
        replanCount += 1;
        return jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-2",
          playback_plan: fixturePlanV3({
            session_id: "session-2",
            plan_id: `plan:replan-${replanCount}`,
          }),
        });
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return jsonResponse({ outcome: "stopped" });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () =>
        usePlaybackSession(
          "request-1",
          [],
          [],
          7,
          0,
          false,
          "auto",
          null,
          undefined,
          null,
          { 7: 0 },
          { 7: 0 },
        ),
      { wrapper },
    );

    await waitFor(() => expect(result.current.plan?.plan_id).toBe(fallbackPlan.plan_id));
    expect(startBodies).toHaveLength(2);
    expect(startBodies[0]).toMatchObject({ subtitle_track_index: 0 });
    expect(startBodies[1]).not.toHaveProperty("subtitle_track_index");
    expect(startBodies[0]?.playback_attempt_id).not.toBe(startBodies[1]?.playback_attempt_id);
    expect(result.current.planRevision).toBe(1);
    expect(result.current.error).toBeNull();
    expect(result.current.initialSubtitleErrorTitle).toBe("This HDR format can't be converted");
    expect(result.current.initialSubtitleError).toContain("dynamic range can't be converted");

    act(() => result.current.changeQuality("1080p", 30));
    await waitFor(() => expect(result.current.plan?.plan_id).toBe("plan:replan-1"));
    expect(result.current.initialSubtitleError).toContain("dynamic range can't be converted");

    act(() => result.current.changeSubtitleTrack(0, 45));
    await waitFor(() => expect(result.current.plan?.plan_id).toBe("plan:replan-2"));
    expect(result.current.initialSubtitleErrorTitle).toBeNull();
    expect(result.current.initialSubtitleError).toBeNull();
    unmount();
  });
});

describe("usePlaybackSession output capability changes", () => {
  function outputProbe(initialHDR: boolean) {
    let hdr = initialHDR;
    const listeners = new Set<() => void>();
    const query = {
      get matches() {
        return hdr;
      },
      addEventListener: (_: string, listener: () => void) => listeners.add(listener),
      removeEventListener: (_: string, listener: () => void) => listeners.delete(listener),
    };
    vi.stubGlobal("matchMedia", () => query);
    // Decode answers are probed independently of the media query, so move the
    // simulated decoder along with the output this fixture switches between.
    vi.spyOn(HTMLMediaElement.prototype, "canPlayType").mockImplementation((mime) =>
      hdr && mime === 'video/mp4; codecs="dvh1.08.06"' ? "probably" : "",
    );
    return (nextHDR: boolean) => {
      hdr = nextHDR;
      for (const listener of listeners) listener();
    };
  }

  it("waits for the initial HDR10 probe before starting playback", async () => {
    outputProbe(false);
    let resolveProbe!: (value: MediaCapabilitiesDecodingInfo) => void;
    const probeResult = new Promise<MediaCapabilitiesDecodingInfo>((resolve) => {
      resolveProbe = resolve;
    });
    vi.stubGlobal("navigator", {
      userAgent: "test-browser",
      mediaCapabilities: { decodingInfo: vi.fn(() => probeResult) },
    });

    const startBodies: Array<{
      client_playback_context: { output: { hdr_details: { hdr10: boolean } } };
    }> = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        startBodies.push(JSON.parse(String(init?.body)) as (typeof startBodies)[number]);
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3", "output_change_v1"],
            outcome: "playable",
            session_id: "session-hdr",
            playback_plan: fixturePlanV3({ session_id: "session-hdr" }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return jsonResponse({ outcome: "stopped" });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "1080p"),
      { wrapper },
    );
    await act(async () => Promise.resolve());
    expect(startBodies).toHaveLength(0);

    await act(async () => {
      resolveProbe({
        supported: true,
        smooth: true,
        powerEfficient: true,
        keySystemAccess: null,
      });
      await probeResult;
    });
    await waitFor(() => expect(result.current.sessionId).toBe("session-hdr"));
    expect(startBodies).toHaveLength(1);
    expect(startBodies[0]?.client_playback_context.output.hdr_details.hdr10).toBe(true);
    unmount();
  });

  it("retries a terminal start when the window moves onto an HDR output", async () => {
    const setHDR = outputProbe(false);
    const startBodies: Array<{
      start_position?: number;
      client_capabilities: { hdr_details?: { dolby_vision_profiles: number[] } };
    }> = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        const body = JSON.parse(String(init?.body)) as (typeof startBodies)[number];
        startBodies.push(body);
        if (startBodies.length === 1) {
          return jsonResponse({
            protocol_version: 3,
            server_features: ["playback_plan_v3", "output_change_v1"],
            outcome: "terminal",
            terminal: { reason: "hdr_transcode_unsupported", message: "HDR unsupported" },
          });
        }
        return jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3", "output_change_v1"],
          outcome: "playable",
          session_id: "session-hdr",
          playback_plan: fixturePlanV3({ session_id: "session-hdr" }),
        });
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return jsonResponse({ outcome: "stopped" });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.error).not.toBeNull());

    act(() => setHDR(true));

    await waitFor(() => expect(result.current.sessionId).toBe("session-hdr"));
    expect(startBodies).toHaveLength(2);
    expect(startBodies[0]).not.toHaveProperty("start_position");
    expect(startBodies[1]).not.toHaveProperty("start_position");
    expect(startBodies[0]?.client_capabilities.hdr_details?.dolby_vision_profiles).toEqual([]);
    expect(startBodies[1]?.client_capabilities.hdr_details?.dolby_vision_profiles).toEqual([8]);
    unmount();
  });

  it("keeps the Play tap's clock when an output change retries a start that never played", async () => {
    const setHDR = outputProbe(false);
    let starts = 0;
    const routeEvents: Array<{ event: string; diagnostics: Record<string, string> }> = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        starts += 1;
        if (starts === 1) {
          return jsonResponse({
            protocol_version: 3,
            server_features: ["playback_plan_v3", "output_change_v1"],
            outcome: "terminal",
            terminal: { reason: "hdr_transcode_unsupported", message: "HDR unsupported" },
          });
        }
        return jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3", "output_change_v1"],
          outcome: "playable",
          session_id: "session-hdr",
          playback_plan: fixturePlanV3({ session_id: "session-hdr" }),
        });
      }
      if (url.endsWith("/playback/route-events")) {
        routeEvents.push(JSON.parse(String(init?.body)) as (typeof routeEvents)[number]);
        return new Response(null, { status: 202 });
      }
      if (init?.method === "DELETE") return jsonResponse({ outcome: "stopped" });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const tap = performance.now();
    markPlaybackIntent("request-1", tap);
    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.error).not.toBeNull());

    act(() => setHDR(true));
    await waitFor(() => expect(result.current.sessionId).toBe("session-hdr"));
    const clock = vi.spyOn(performance, "now").mockReturnValue(tap + 3_000);
    try {
      act(() => result.current.reportFirstFrame());
    } finally {
      clock.mockRestore();
    }

    await waitFor(() =>
      expect(routeEvents.filter((event) => event.event === "first_frame")).toHaveLength(1),
    );
    expect(routeEvents.find((event) => event.event === "first_frame")?.diagnostics).toEqual({
      first_frame_ms: "3000",
    });
    unmount();
  });

  it("replans an active HDR route with its tracks and paused state after moving to SDR", async () => {
    const setHDR = outputProbe(true);
    const audio = { id: "file:7:audio:1", index: 1 };
    const subtitle = { id: "file:7:subtitle:2", index: 2 };
    const initialPlan = fixturePlanV3({
      session_id: "session-hdr",
      selected_tracks: { audio, subtitle },
    });
    const replanBodies: Array<{
      operation: string;
      position_seconds: number;
      selected_tracks: { audio?: typeof audio; subtitle?: typeof subtitle };
      client_capabilities: { hdr: boolean };
    }> = [];
    let startCount = 0;
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        startCount += 1;
        return jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3", "output_change_v1"],
          outcome: "playable",
          session_id: "session-hdr",
          playback_plan: initialPlan,
        });
      }
      if (url.endsWith("/playback/session-hdr/replan")) {
        replanBodies.push(JSON.parse(String(init?.body)) as (typeof replanBodies)[number]);
        return jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3", "output_change_v1"],
          outcome: "playable",
          session_id: "session-hdr",
          playback_plan: fixturePlanV3({
            session_id: "session-hdr",
            plan_id: "plan:outputchanged001",
            plan_attempt_key: "v3:outputchanged001",
            selected_tracks: { audio, subtitle },
          }),
        });
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return jsonResponse({ outcome: "stopped" });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.sessionId).toBe("session-hdr"));
    act(() => {
      result.current.updatePlaybackState(300, true);
      result.current.updatePlaybackState(321, false);
    });

    act(() => setHDR(false));

    await waitFor(() => expect(result.current.plan?.plan_id).toBe("plan:outputchanged001"));
    expect(startCount).toBe(1);
    expect(replanBodies).toHaveLength(1);
    expect(replanBodies[0]).toMatchObject({
      operation: "output_change",
      position_seconds: 321,
      selected_tracks: { audio, subtitle },
      client_capabilities: { hdr: false },
    });
    expect(result.current.sessionId).toBe("session-hdr");
    expect(result.current.shouldAutoPlay).toBe(false);
    unmount();
  });

  it("seeds an early output replan from the server-resolved source position", async () => {
    const setHDR = outputProbe(true);
    const resumedPlan = fixturePlanV3({ session_id: "session-hdr" });
    resumedPlan.timeline = {
      ...resumedPlan.timeline,
      source_start_seconds: 275,
      player_start_seconds: 5,
      timeline_offset_seconds: 270,
    };
    const replanBodies: Array<{ position_seconds: number }> = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3", "output_change_v1"],
          outcome: "playable",
          session_id: "session-hdr",
          playback_plan: resumedPlan,
        });
      }
      if (url.endsWith("/playback/session-hdr/replan")) {
        replanBodies.push(JSON.parse(String(init?.body)) as { position_seconds: number });
        return jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3", "output_change_v1"],
          outcome: "playable",
          session_id: "session-hdr",
          playback_plan: fixturePlanV3({
            session_id: "session-hdr",
            plan_id: "plan:resumedoutput001",
            plan_attempt_key: "v3:resumedoutput001",
          }),
        });
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return jsonResponse({ outcome: "stopped" });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.sessionId).toBe("session-hdr"));

    act(() => result.current.updatePlaybackState(0, false));
    act(() => setHDR(false));

    await waitFor(() => expect(replanBodies).toHaveLength(1));
    expect(replanBodies[0]?.position_seconds).toBe(275);
    unmount();
  });

  it("retires an active plan when the refreshed output has no playable route", async () => {
    const setHDR = outputProbe(true);
    const stoppedSessions: string[] = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3", "output_change_v1"],
          outcome: "playable",
          session_id: "session-hdr",
          playback_plan: fixturePlanV3({ session_id: "session-hdr" }),
        });
      }
      if (url.endsWith("/playback/session-hdr/replan")) {
        return jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3", "output_change_v1"],
          outcome: "terminal",
          terminal: { reason: "hdr_transcode_unsupported", message: "HDR unsupported" },
        });
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") {
        stoppedSessions.push(url);
        return jsonResponse({ outcome: "stopped" });
      }
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.sessionId).toBe("session-hdr"));

    act(() => setHDR(false));

    await waitFor(() => expect(result.current.plan).toBeNull());
    expect(result.current.sessionId).toBeNull();
    expect(result.current.error).not.toBeNull();
    await waitFor(() => expect(stoppedSessions).toContain("/api/v2/playback/session-hdr"));
    unmount();
  });

  it("uses the latest capabilities when an output change queues behind another replan", async () => {
    const setHDR = outputProbe(true);
    let resolveFirstReplan: ((response: Response) => void) | undefined;
    const firstReplanResponse = new Promise<Response>((resolve) => {
      resolveFirstReplan = resolve;
    });
    const replanBodies: Array<{
      operation: string;
      position_seconds: number;
      client_capabilities: { hdr: boolean };
    }> = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3", "output_change_v1"],
          outcome: "playable",
          session_id: "session-hdr",
          playback_plan: fixturePlanV3({ session_id: "session-hdr" }),
        });
      }
      if (url.endsWith("/playback/session-hdr/replan")) {
        replanBodies.push(JSON.parse(String(init?.body)) as (typeof replanBodies)[number]);
        if (replanBodies.length === 1) return firstReplanResponse;
        return jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3", "output_change_v1"],
          outcome: "playable",
          session_id: "session-hdr",
          playback_plan: fixturePlanV3({
            session_id: "session-hdr",
            plan_id: "plan:queuedoutput0001",
            plan_attempt_key: "v3:queuedoutput0001",
          }),
        });
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return jsonResponse({ outcome: "stopped" });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.sessionId).toBe("session-hdr"));

    act(() => {
      // Deliberately not awaited: these tests drive the replan manually so the
      // promise must not block `act` on a queued/in-flight replan.
      void result.current.refreshSubtitles(120);
    });
    await waitFor(() => expect(replanBodies).toHaveLength(1));
    act(() => setHDR(false));
    act(() => {
      void result.current.reanchorSeek(555);
    });
    expect(replanBodies).toHaveLength(1);

    await act(async () => {
      resolveFirstReplan?.(
        jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3", "output_change_v1"],
          outcome: "playable",
          session_id: "session-hdr",
          playback_plan: fixturePlanV3({
            session_id: "session-hdr",
            plan_id: "plan:trackrefresh0001",
            plan_attempt_key: "v3:trackrefresh0001",
          }),
        }),
      );
      await firstReplanResponse;
    });

    await waitFor(() => expect(replanBodies).toHaveLength(2));
    expect(replanBodies[1]).toMatchObject({
      operation: "output_change",
      position_seconds: 555,
      client_capabilities: { hdr: false },
    });
    unmount();
  });

  it("runs the latest output refresh before retiring a terminal replan", async () => {
    const setHDR = outputProbe(true);
    let resolveFirstReplan: ((response: Response) => void) | undefined;
    const firstReplanResponse = new Promise<Response>((resolve) => {
      resolveFirstReplan = resolve;
    });
    const replanBodies: Array<{ client_capabilities: { hdr: boolean } }> = [];
    const stoppedSessions: string[] = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3", "output_change_v1"],
          outcome: "playable",
          session_id: "session-hdr",
          playback_plan: fixturePlanV3({ session_id: "session-hdr" }),
        });
      }
      if (url.endsWith("/playback/session-hdr/replan")) {
        replanBodies.push(
          JSON.parse(String(init?.body)) as { client_capabilities: { hdr: boolean } },
        );
        if (replanBodies.length === 1) return firstReplanResponse;
        return jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3", "output_change_v1"],
          outcome: "playable",
          session_id: "session-hdr",
          playback_plan: fixturePlanV3({
            session_id: "session-hdr",
            plan_id: "plan:latestoutput0001",
            plan_attempt_key: "v3:latestoutput0001",
          }),
        });
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") {
        stoppedSessions.push(url);
        return jsonResponse({ outcome: "stopped" });
      }
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.sessionId).toBe("session-hdr"));

    act(() => setHDR(false));
    await waitFor(() => expect(replanBodies).toHaveLength(1));
    act(() => setHDR(true));

    await act(async () => {
      resolveFirstReplan?.(
        jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3", "output_change_v1"],
          outcome: "terminal",
          terminal: { reason: "hdr_transcode_unsupported", message: "HDR unsupported" },
        }),
      );
      await firstReplanResponse;
    });

    await waitFor(() => expect(result.current.plan?.plan_id).toBe("plan:latestoutput0001"));
    expect(replanBodies).toHaveLength(2);
    expect(replanBodies[1]).toMatchObject({ client_capabilities: { hdr: true } });
    expect(stoppedSessions).toEqual([]);
    unmount();
  });

  it("retires after a queued user intent also refuses refreshed output", async () => {
    const setHDR = outputProbe(true);
    let resolveOutputReplan: ((response: Response) => void) | undefined;
    const outputReplanResponse = new Promise<Response>((resolve) => {
      resolveOutputReplan = resolve;
    });
    const replanOperations: string[] = [];
    const stoppedSessions: string[] = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3", "output_change_v1"],
          outcome: "playable",
          session_id: "session-hdr",
          playback_plan: fixturePlanV3({ session_id: "session-hdr" }),
        });
      }
      if (url.endsWith("/playback/session-hdr/replan")) {
        replanOperations.push((JSON.parse(String(init?.body)) as { operation: string }).operation);
        if (replanOperations.length === 1) return outputReplanResponse;
        return jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3", "output_change_v1"],
          outcome: "terminal",
          terminal: { reason: "hdr_transcode_unsupported", message: "HDR unsupported" },
        });
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") {
        stoppedSessions.push(url);
        return jsonResponse({ outcome: "stopped" });
      }
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.sessionId).toBe("session-hdr"));

    act(() => setHDR(false));
    await waitFor(() => expect(replanOperations).toEqual(["output_change"]));
    act(() => result.current.changeQuality("1080p", 91));
    expect(replanOperations).toEqual(["output_change"]);

    await act(async () => {
      resolveOutputReplan?.(
        jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3", "output_change_v1"],
          outcome: "terminal",
          terminal: { reason: "hdr_transcode_unsupported", message: "HDR unsupported" },
        }),
      );
      await outputReplanResponse;
    });

    await waitFor(() => expect(replanOperations).toEqual(["output_change", "quality_change"]));
    await waitFor(() => expect(result.current.plan).toBeNull());
    expect(stoppedSessions).toContain("/api/v2/playback/session-hdr");
    unmount();
  });

  it("keeps the active plan when an output refresh request fails", async () => {
    const setHDR = outputProbe(true);
    const stoppedSessions: string[] = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3", "output_change_v1"],
          outcome: "playable",
          session_id: "session-hdr",
          playback_plan: fixturePlanV3({ session_id: "session-hdr" }),
        });
      }
      if (url.endsWith("/playback/session-hdr/replan")) {
        return jsonResponse({ error: "temporary failure" }, { status: 503 });
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") {
        stoppedSessions.push(url);
        return jsonResponse({ outcome: "stopped" });
      }
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.sessionId).toBe("session-hdr"));
    const activePlan = result.current.plan;

    act(() => setHDR(false));

    await waitFor(() => expect(result.current.replanning).toBe(false));
    expect(result.current.plan).toBe(activePlan);
    expect(result.current.sessionId).toBe("session-hdr");
    expect(result.current.error).toBeNull();
    expect(stoppedSessions).toEqual([]);
    unmount();
  });

  it("queues the latest user intent behind an output refresh", async () => {
    const setHDR = outputProbe(true);
    let resolveOutputReplan: ((response: Response) => void) | undefined;
    const outputReplanResponse = new Promise<Response>((resolve) => {
      resolveOutputReplan = resolve;
    });
    const replanBodies: Array<{
      operation: string;
      quality_preference: string;
      selected_tracks: { audio?: { index?: number } };
    }> = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3", "output_change_v1"],
          outcome: "playable",
          session_id: "session-hdr",
          playback_plan: fixturePlanV3({ session_id: "session-hdr" }),
        });
      }
      if (url.endsWith("/playback/session-hdr/replan")) {
        replanBodies.push(JSON.parse(String(init?.body)) as (typeof replanBodies)[number]);
        if (replanBodies.length === 1) return outputReplanResponse;
        return jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3", "output_change_v1"],
          outcome: "playable",
          session_id: "session-hdr",
          playback_plan: fixturePlanV3({
            session_id: "session-hdr",
            plan_id: "plan:userintent00001",
            plan_attempt_key: "v3:userintent00001",
          }),
        });
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return jsonResponse({ outcome: "stopped" });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.sessionId).toBe("session-hdr"));

    act(() => setHDR(false));
    await waitFor(() => expect(replanBodies).toHaveLength(1));
    act(() => result.current.switchAudioTrack(2, 90));
    act(() => result.current.changeQuality("1080p", 91));
    expect(replanBodies).toHaveLength(1);

    await act(async () => {
      resolveOutputReplan?.(
        jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3", "output_change_v1"],
          outcome: "playable",
          session_id: "session-hdr",
          playback_plan: fixturePlanV3({ session_id: "session-hdr" }),
        }),
      );
      await outputReplanResponse;
    });

    await waitFor(() => expect(replanBodies).toHaveLength(2));
    expect(replanBodies[1]).toMatchObject({
      operation: "quality_change",
      quality_preference: "1080p",
    });
    expect(result.current.qualityPreference).toBe("1080p");
    unmount();
  });

  it("drops a predecessor failure after an output refresh adopts a new plan", async () => {
    const setHDR = outputProbe(true);
    let resolveOutputReplan: ((response: Response) => void) | undefined;
    const outputReplanResponse = new Promise<Response>((resolve) => {
      resolveOutputReplan = resolve;
    });
    const replanOperations: string[] = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3", "output_change_v1"],
          outcome: "playable",
          session_id: "session-hdr",
          playback_plan: fixturePlanV3({ session_id: "session-hdr" }),
        });
      }
      if (url.endsWith("/playback/session-hdr/replan")) {
        replanOperations.push((JSON.parse(String(init?.body)) as { operation: string }).operation);
        return outputReplanResponse;
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return jsonResponse({ outcome: "stopped" });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.sessionId).toBe("session-hdr"));

    act(() => setHDR(false));
    await waitFor(() => expect(replanOperations).toEqual(["output_change"]));
    act(() => result.current.recoverFromFailure({ classification: "decoder_failure" }, 120));

    await act(async () => {
      resolveOutputReplan?.(
        jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3", "output_change_v1"],
          outcome: "playable",
          session_id: "session-hdr",
          playback_plan: fixturePlanV3({
            session_id: "session-hdr",
            plan_id: "plan:replacement0001",
            plan_attempt_key: "v3:replacement0001",
          }),
        }),
      );
      await outputReplanResponse;
    });

    await waitFor(() => expect(result.current.plan?.plan_id).toBe("plan:replacement0001"));
    expect(replanOperations).toEqual(["output_change"]);
    unmount();
  });

  it("keeps playback unchanged when the server lacks output-change support", async () => {
    const setHDR = outputProbe(true);
    const replanOperations: string[] = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-hdr",
          playback_plan: fixturePlanV3({ session_id: "session-hdr" }),
        });
      }
      if (url.endsWith("/playback/session-hdr/replan")) {
        replanOperations.push((JSON.parse(String(init?.body)) as { operation: string }).operation);
        return jsonResponse({ error: "unsupported operation" }, { status: 400 });
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return jsonResponse({ outcome: "stopped" });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.sessionId).toBe("session-hdr"));

    await act(async () => {
      setHDR(false);
      await Promise.resolve();
    });

    expect(replanOperations).toEqual([]);
    expect(result.current.sessionId).toBe("session-hdr");
    expect(result.current.plan).not.toBeNull();
    unmount();
  });
});

describe("usePlaybackSession version switches", () => {
  it("clears and stops the previous session when a new request ends terminally", async () => {
    let startCount = 0;
    const stoppedSessions: string[] = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        startCount += 1;
        if (startCount === 2) {
          return jsonResponse({
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "terminal",
            terminal: {
              reason: "no_playable_route",
              message: "The next item has no playable route.",
              retryable: false,
            },
          });
        }
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({ session_id: "session-1" }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) {
        return new Response(null, { status: 202 });
      }
      if (init?.method === "DELETE") {
        stoppedSessions.push(url);
        return jsonResponse({ outcome: "stopped" });
      }
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, rerender, unmount } = renderHook(
      ({ requestKey, fileId }: { requestKey: string; fileId: number }) =>
        usePlaybackSession(requestKey, [], [], fileId, 0, false, "auto"),
      { wrapper, initialProps: { requestKey: "episode-1", fileId: 7 } },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    rerender({ requestKey: "episode-2", fileId: 8 });

    await waitFor(() => {
      expect(result.current.plan).toBeNull();
      expect(result.current.error).toBe("The next item has no playable route.");
      expect(result.current.errorReason).toBe("no_playable_route");
    });
    expect(result.current.streamUrl).toBeNull();
    expect(result.current.sessionId).toBeNull();
    expect(stoppedSessions).toEqual(["/api/v2/playback/session-1"]);

    unmount();
  });

  it("clears and stops the previous session when a replacement start request fails", async () => {
    const startBodies: Array<{ playback_attempt_id: string; start_position?: number }> = [];
    const stoppedSessions: string[] = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        const body = JSON.parse(String(init?.body)) as {
          playback_attempt_id: string;
          start_position?: number;
        };
        startBodies.push(body);
        if (startBodies.length === 2) {
          return jsonResponse(
            { error: "internal_error", message: "replacement failed" },
            { status: 500 },
          );
        }
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3(),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") {
        stoppedSessions.push(url);
        return jsonResponse({ outcome: "stopped" });
      }
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() => result.current.switchVersion(99, 0));
    await waitFor(() => expect(startBodies).toHaveLength(2));
    await waitFor(() => expect(result.current.sessionId).toBeNull());
    const originalStart = startBodies[0];
    const replacementStart = startBodies[1];
    if (!originalStart || !replacementStart) throw new Error("expected two start requests");
    expect(replacementStart.start_position).toBe(0);
    expect(replacementStart.playback_attempt_id).not.toBe(originalStart.playback_attempt_id);
    expect(result.current.plan).toBeNull();
    expect(result.current.streamUrl).toBeNull();
    expect(result.current.sessionId).toBeNull();
    expect(result.current.error).toContain("could not start playback");
    expect(stoppedSessions).toEqual(["/api/v2/playback/session-1"]);

    unmount();
  });

  it("sets pendingSwitchFileId optimistically and clears it when the new plan lands", async () => {
    const startBodies: Array<{ file_id: number }> = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        const body = JSON.parse(String(init?.body)) as { file_id: number };
        startBodies.push(body);
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: `session-${startBodies.length}`,
            playback_plan: fixturePlanV3({
              session_id: `session-${startBodies.length}`,
              plan_id: `plan:switch-${startBodies.length}`,
              requested_media_file_id: body.file_id,
              effective_media_file_id: body.file_id,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());
    expect(result.current.pendingSwitchFileId).toBeNull();

    act(() => result.current.switchVersion(99, 0));
    // Set synchronously, before the replacement plan lands.
    expect(result.current.pendingSwitchFileId).toBe(99);
    expect(result.current.replacing).toBe(true);

    await waitFor(() => expect(result.current.mediaFileId).toBe(99));
    expect(result.current.pendingSwitchFileId).toBeNull();
    expect(result.current.replacing).toBe(false);
    expect(startBodies.map((body) => body.file_id)).toEqual([7, 99]);

    unmount();
  });

  it("carries the current audio track identity into a version-switch start", async () => {
    const startBodies: Array<Record<string, unknown>> = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        const body = JSON.parse(String(init?.body)) as Record<string, unknown>;
        startBodies.push(body);
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: `session-${startBodies.length}`,
            playback_plan: fixturePlanV3({
              session_id: `session-${startBodies.length}`,
              plan_id: `plan:switch-${startBodies.length}`,
              requested_media_file_id: (body.file_id as number) ?? 7,
              effective_media_file_id: (body.file_id as number) ?? 7,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto", null, undefined, 2),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());
    // The initial start sends the mount-time audio ordinal.
    expect(startBodies[0]).toMatchObject({ file_id: 7, audio_track_index: 2 });

    act(() => result.current.switchVersion(99, 120));
    await waitFor(() => expect(startBodies).toHaveLength(2));
    const replacement = startBodies[1]!;
    expect(replacement.file_id).toBe(99);
    // The replacement start carries the current plan's file-bound audio identity
    // (remapped by family server-side) and drops the mount-time ordinal, which
    // is meaningless for the new file, in favor of the carried identity.
    expect(replacement.carried_audio_track_id).toBe("file:7:audio:0");
    expect(replacement).not.toHaveProperty("audio_track_index");

    unmount();
  });

  it("marks a version-switch start as an explicit file selection", async () => {
    const startBodies: Array<Record<string, unknown>> = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        const body = JSON.parse(String(init?.body)) as Record<string, unknown>;
        startBodies.push(body);
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: `session-${startBodies.length}`,
            playback_plan: fixturePlanV3({
              session_id: `session-${startBodies.length}`,
              plan_id: `plan:switch-${startBodies.length}`,
              requested_media_file_id: (body.file_id as number) ?? 7,
              effective_media_file_id: (body.file_id as number) ?? 7,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());
    // The auto-selected initial start declares the server-owned choice.
    expect(startBodies[0]).toMatchObject({ file_id: 7, file_selection: "auto" });

    act(() => result.current.switchVersion(99, 0));
    await waitFor(() => expect(startBodies).toHaveLength(2));
    // A version switch is always an explicit user action.
    expect(startBodies[1]).toMatchObject({ file_id: 99, file_selection: "explicit" });

    unmount();
  });

  it("bumps transportRevision when a version switch collapses to the playing effective file", async () => {
    // A collapsed virtual row: the server resolves the newly requested catalog
    // row (99) to the same concrete candidate (7) the outgoing plan already
    // plays, so effective_media_file_id and stream.url are unchanged. The
    // requested identity still changed, which means the mounted element holds
    // the old session's stream; the transport revision must bump so the player
    // tears it down and re-attaches.
    const startBodies: Array<Record<string, unknown>> = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        const body = JSON.parse(String(init?.body)) as Record<string, unknown>;
        startBodies.push(body);
        const requested = (body.file_id as number) ?? 7;
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: `session-${startBodies.length}`,
            playback_plan: fixturePlanV3({
              session_id: `session-${startBodies.length}`,
              plan_id: `plan:switch-${startBodies.length}`,
              requested_media_file_id: requested,
              // The collapsed row resolves to the already-playing candidate.
              effective_media_file_id: 7,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());
    expect(result.current.transportRevision).toBe(1);

    act(() => result.current.switchVersion(99, 0));
    await waitFor(() => expect(result.current.plan?.requested_media_file_id).toBe(99));

    expect(result.current.planRevision).toBe(2);
    expect(result.current.mediaFileId).toBe(7);
    expect(result.current.transportRevision).toBe(2);

    unmount();
  });

  it("coalesces a rapid second version click to the latest target", async () => {
    const startBodies: Array<{ file_id: number; start_position?: number }> = [];
    let releaseFirstSwitch: ((response: Response) => void) | undefined;
    const firstSwitchResponse = new Promise<Response>((resolve) => {
      releaseFirstSwitch = resolve;
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        const body = JSON.parse(String(init?.body)) as { file_id: number; start_position?: number };
        startBodies.push(body);
        if (startBodies.length === 2) {
          // Hold the first switch open so the second click lands while it is
          // still in flight.
          return firstSwitchResponse;
        }
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: `session-${startBodies.length}`,
            playback_plan: fixturePlanV3({
              session_id: `session-${startBodies.length}`,
              plan_id: `plan:switch-${startBodies.length}`,
              requested_media_file_id: body.file_id,
              effective_media_file_id: body.file_id,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() => result.current.switchVersion(99, 15));
    await waitFor(() => expect(startBodies).toHaveLength(2));
    // The second click lands while the first switch is still in flight.
    act(() => result.current.switchVersion(123, 347));
    expect(startBodies).toHaveLength(2);
    expect(result.current.pendingSwitchFileId).toBe(123);

    await act(async () => {
      releaseFirstSwitch?.(
        jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-2",
          playback_plan: fixturePlanV3({
            session_id: "session-2",
            plan_id: "plan:switch-2",
            requested_media_file_id: 99,
            effective_media_file_id: 99,
          }),
        }),
      );
      await firstSwitchResponse;
    });

    // The coalesced switch to the latest target runs immediately after.
    await waitFor(() => expect(startBodies).toHaveLength(3));
    expect(startBodies.map((body) => body.file_id)).toEqual([7, 99, 123]);
    // The first switch seeks to where its click was; the chained one must use
    // the live position at second-click time, not the first click's closure
    // position (15).
    expect(startBodies[1]?.start_position).toBe(15);
    expect(startBodies[2]?.start_position).toBe(347);
    await waitFor(() => expect(result.current.mediaFileId).toBe(123));
    expect(result.current.pendingSwitchFileId).toBeNull();

    unmount();
  });

  it("refuses replans issued while a version switch is in flight", async () => {
    const startBodies: Array<{ file_id: number }> = [];
    const replanBodies: Array<{ operation: string }> = [];
    let releaseSwitch: ((response: Response) => void) | undefined;
    const switchResponse = new Promise<Response>((resolve) => {
      releaseSwitch = resolve;
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        const body = JSON.parse(String(init?.body)) as { file_id: number };
        startBodies.push(body);
        if (startBodies.length === 2) {
          // Hold the switch open so replans land while it is rebuilding the
          // session.
          return switchResponse;
        }
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3(),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/replan")) {
        replanBodies.push(JSON.parse(String(init?.body)) as { operation: string });
        return jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-2",
          playback_plan: fixturePlanV3({
            session_id: "session-2",
            plan_id: "plan:switch-2",
            plan_attempt_key: "v3:switch-2",
            requested_media_file_id: 99,
            effective_media_file_id: 99,
          }),
        });
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() => result.current.switchVersion(99, 0));
    await waitFor(() => expect(startBodies).toHaveLength(2));
    expect(result.current.replacing).toBe(true);

    // A stale render, the realtime channel and the output-capability effect
    // can all try to replan while the switch is rebuilding the session. Each
    // would be built from the outgoing plan but stamped with the sequence the
    // switch just took, so its response could overwrite the replacement plan.
    // All must be refused.
    act(() => {
      result.current.switchAudioTrack(2, 120);
      result.current.changeQuality("1080p", 130);
      void result.current.reanchorSeek(140);
    });
    expect(replanBodies).toEqual([]);

    await act(async () => {
      releaseSwitch?.(
        jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-2",
          playback_plan: fixturePlanV3({
            session_id: "session-2",
            plan_id: "plan:switch-2",
            plan_attempt_key: "v3:switch-2",
            requested_media_file_id: 99,
            effective_media_file_id: 99,
            stream: { ...fixturePlanV3().stream, url: "/stream/session-2/master.m3u8" },
            source: { ...fixturePlanV3().source, media_file_id: 99 },
          }),
        }),
      );
      await switchResponse;
    });

    await waitFor(() => expect(result.current.mediaFileId).toBe(99));
    expect(result.current.plan?.plan_id).toBe("plan:switch-2");
    expect(replanBodies).toEqual([]);

    // Once the switch settles the fence lifts and replans flow again.
    act(() => {
      void result.current.reanchorSeek(150);
    });
    await waitFor(() => expect(replanBodies).toHaveLength(1));

    unmount();
  });

  it("ignores a committed source rotation while a version switch is replacing", async () => {
    const planAudioTracks = [
      { codec: "eac3", channels: 6, layout: "5.1", language: "eng", default: true },
      { codec: "ac3", channels: 6, layout: "5.1", language: "spa", default: false },
    ];
    const startBodies: Array<{ file_id: number }> = [];
    let releaseSwitch: ((response: Response) => void) | undefined;
    const switchResponse = new Promise<Response>((resolve) => {
      releaseSwitch = resolve;
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        const body = JSON.parse(String(init?.body)) as { file_id: number };
        startBodies.push(body);
        if (startBodies.length === 2) {
          // Hold the switch open so the rotation lands while it rebuilds.
          return switchResponse;
        }
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              effective_media_file_id: 7,
              effective_virtual_uri: "virtual://movie/x?result=A",
              audio_tracks: planAudioTracks,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() => result.current.switchVersion(99, 0));
    await waitFor(() => expect(startBodies).toHaveLength(2));
    expect(result.current.replacing).toBe(true);

    // The outgoing transport rotates to another release while the switch is
    // still in flight. Its identity and inventory must not be written under the
    // pending switch.
    act(() =>
      result.current.applyCommittedSource(
        {
          effectiveMediaFileId: 8,
          effectiveVirtualUri: "virtual://movie/x?result=B",
          inventoryStatus: "declared",
        },
        [{ codec: "eac3", channels: 6, layout: "5.1", language: "deu", default: true }],
      ),
    );

    expect(result.current.mediaFileId).toBe(7);
    expect(result.current.effectiveVirtualUri).toBe("virtual://movie/x?result=A");
    expect(result.current.planAudioTracks).toEqual(planAudioTracks);

    await act(async () => {
      releaseSwitch?.(
        jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-2",
          playback_plan: fixturePlanV3({
            session_id: "session-2",
            plan_id: "plan:switch-2",
            plan_attempt_key: "v3:switch-2",
            requested_media_file_id: 99,
            effective_media_file_id: 99,
            audio_tracks: [],
          }),
        }),
      );
      await switchResponse;
    });

    // The switch's replacement plan is the authority once it lands.
    await waitFor(() => expect(result.current.mediaFileId).toBe(99));
    expect(result.current.effectiveVirtualUri).toBeNull();
    expect(result.current.planAudioTracks).toEqual([]);

    unmount();
  });

  it("seeks a chained switch from the live playhead after a rotation", async () => {
    const startBodies: Array<{ file_id: number; start_position?: number }> = [];
    let releaseFirstSwitch: ((response: Response) => void) | undefined;
    const firstSwitchResponse = new Promise<Response>((resolve) => {
      releaseFirstSwitch = resolve;
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        const body = JSON.parse(String(init?.body)) as { file_id: number; start_position?: number };
        startBodies.push(body);
        if (startBodies.length === 2) return firstSwitchResponse;
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: `session-${startBodies.length}`,
            playback_plan: fixturePlanV3({
              session_id: `session-${startBodies.length}`,
              plan_id: `plan:switch-${startBodies.length}`,
              requested_media_file_id: body.file_id,
              effective_media_file_id: body.file_id,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() => result.current.switchVersion(99, 15));
    await waitFor(() => expect(startBodies).toHaveLength(2));
    // A second click queues a chained switch with the live position at click
    // time.
    act(() => result.current.switchVersion(123, 347));
    expect(startBodies).toHaveLength(2);

    // A rotation arrives while the first switch is in flight: the queued
    // position was captured against the outgoing timeline and must be dropped.
    act(() =>
      result.current.applyCommittedSource(
        { effectiveMediaFileId: 8, effectiveVirtualUri: null, inventoryStatus: "declared" },
        [],
      ),
    );

    await act(async () => {
      releaseFirstSwitch?.(
        jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-2",
          playback_plan: fixturePlanV3({
            session_id: "session-2",
            plan_id: "plan:switch-2",
            requested_media_file_id: 99,
            effective_media_file_id: 99,
            // The server starts the replacement stream at the live playhead.
            timeline: { ...fixturePlanV3().timeline, source_start_seconds: 500 },
          }),
        }),
      );
      await firstSwitchResponse;
    });

    await waitFor(() => expect(startBodies).toHaveLength(3));
    expect(startBodies.map((body) => body.file_id)).toEqual([7, 99, 123]);
    // The stale captured position (347) was discarded; the chained switch uses
    // the live playhead seeded by the replacement plan.
    expect(startBodies[2]?.start_position).toBe(500);

    unmount();
  });

  it("carries a mid-session Auto re-arm onto the next failure recovery", async () => {
    const replanBodies: Array<Record<string, unknown>> = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({ session_id: "session-1" }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/session-1/replan")) {
        replanBodies.push(JSON.parse(String(init?.body)) as Record<string, unknown>);
        return jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-1",
          playback_plan: fixturePlanV3({
            session_id: "session-1",
            plan_id: "plan:auto-replan-1",
            plan_attempt_key: "v3:autoreplan0001",
          }),
        });
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    // Started on an explicit pick, so the session begins with Auto disarmed.
    const { result, unmount } = renderHook(
      () =>
        usePlaybackSession(
          "request-1",
          [],
          [],
          7,
          0,
          false,
          "auto",
          null,
          undefined,
          null,
          undefined,
          undefined,
          true,
        ),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());
    expect(result.current.autoFallback).toBe(false);

    // Re-arming Auto while a plan is live has no start request to carry the
    // intent, so the next replan must state it or a dead-source recovery stays
    // pinned to the explicit pick while the menu shows Auto.
    act(() => result.current.selectAutoVersion());
    expect(result.current.autoFallback).toBe(true);

    act(() => result.current.recoverFromFailure({ classification: "decoder_failure" }, 120));
    await waitFor(() => expect(replanBodies).toHaveLength(1));
    expect(replanBodies[0]).toMatchObject({
      operation: "failure_recovery",
      auto_fallback: true,
    });

    unmount();
  });
});

describe("usePlaybackSession replans", () => {
  it("drops a queued predecessor failure after the in-flight replan adopts a new plan", async () => {
    const initialPlan = fixturePlanV3();
    let resolveFirstReplan: ((response: Response) => void) | undefined;
    const firstReplanResponse = new Promise<Response>((resolve) => {
      resolveFirstReplan = resolve;
    });
    const replanBodies: Array<{ operation: string; position_seconds: number }> = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: initialPlan,
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/session-1/replan")) {
        replanBodies.push(
          JSON.parse(String(init?.body)) as { operation: string; position_seconds: number },
        );
        return firstReplanResponse;
      }
      if (url.endsWith("/playback/route-events")) {
        return new Response(null, { status: 202 });
      }
      if (init?.method === "DELETE") {
        return jsonResponse({ outcome: "stopped" });
      }
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() => {
      // Deliberately not awaited: these tests drive the replan manually so the
      // promise must not block `act` on a queued/in-flight replan.
      void result.current.refreshSubtitles(120);
    });
    await waitFor(() => expect(replanBodies).toHaveLength(1));

    act(() => {
      void result.current.reanchorSeek(300);
      result.current.recoverFromFailure({ classification: "decoder_error" }, 450);
      void result.current.reanchorSeek(600);
    });
    expect(replanBodies).toHaveLength(1);

    await act(async () => {
      resolveFirstReplan?.(
        jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-1",
          playback_plan: fixturePlanV3({
            plan_id: "plan:1111111111111111",
            plan_attempt_key: "v3:1111111111111111",
          }),
        }),
      );
      await firstReplanResponse;
    });

    await waitFor(() => expect(result.current.plan?.plan_id).toBe("plan:1111111111111111"));
    expect(
      replanBodies.map(({ operation, position_seconds }) => ({ operation, position_seconds })),
    ).toEqual([{ operation: "track_change", position_seconds: 120 }]);

    unmount();
  });

  it("coalesces reanchor seeks behind an in-flight replan and keeps the latest position", async () => {
    const initialPlan = fixturePlanV3();
    const firstReplannedPlan = fixturePlanV3({
      plan_id: "plan:1111111111111111",
      plan_attempt_key: "v3:1111111111111111",
    });
    let resolveFirstReplan: ((response: Response) => void) | undefined;
    const firstReplanResponse = new Promise<Response>((resolve) => {
      resolveFirstReplan = resolve;
    });
    const replanBodies: Array<{ operation: string; position_seconds: number }> = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: initialPlan,
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/session-1/replan")) {
        replanBodies.push(
          JSON.parse(String(init?.body)) as { operation: string; position_seconds: number },
        );
        if (replanBodies.length === 1) return firstReplanResponse;
        return jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-1",
          playback_plan: fixturePlanV3({
            plan_id: "plan:2222222222222222",
            plan_attempt_key: "v3:2222222222222222",
          }),
        });
      }
      if (url.endsWith("/playback/route-events")) {
        return new Response(null, { status: 202 });
      }
      if (init?.method === "DELETE") {
        return jsonResponse({ outcome: "stopped" });
      }
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() => {
      // Deliberately not awaited: these tests drive the replan manually so the
      // promise must not block `act` on a queued/in-flight replan.
      void result.current.refreshSubtitles(120);
    });
    await waitFor(() => expect(replanBodies).toHaveLength(1));

    act(() => {
      void result.current.reanchorSeek(300);
      void result.current.reanchorSeek(450);
    });
    expect(replanBodies).toHaveLength(1);

    await act(async () => {
      resolveFirstReplan?.(
        jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-1",
          playback_plan: firstReplannedPlan,
        }),
      );
      await firstReplanResponse;
    });

    await waitFor(() => expect(replanBodies).toHaveLength(2));
    expect(
      replanBodies.map(({ operation, position_seconds }) => ({ operation, position_seconds })),
    ).toEqual([
      { operation: "track_change", position_seconds: 120 },
      { operation: "seek_reanchor", position_seconds: 450 },
    ]);

    unmount();
  });

  it("drops a queued track change whose plan the in-flight replan replaced", async () => {
    const initialPlan = fixturePlanV3();
    const replannedPlan = fixturePlanV3({
      plan_id: "plan:2222222222222222",
      plan_attempt_key: "v3:2222222222222222",
    });
    let resolveFirstReplan: ((response: Response) => void) | undefined;
    const firstReplanResponse = new Promise<Response>((resolve) => {
      resolveFirstReplan = resolve;
    });
    const replanBodies: Array<Record<string, unknown>> = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: initialPlan,
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/session-1/replan")) {
        replanBodies.push(JSON.parse(String(init?.body)) as Record<string, unknown>);
        return firstReplanResponse;
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() => {
      // In flight, and it will replace the plan the queued audio change is
      // built from.
      void result.current.refreshSubtitles(120);
    });
    await waitFor(() => expect(replanBodies).toHaveLength(1));

    // The audio ordinal was resolved off the outgoing plan's menu. Once the
    // in-flight refresh adopts a different plan identity that ordinal no longer
    // names the same track, so the queued change must be dropped rather than
    // replayed against the replacement plan.
    act(() => result.current.switchAudioTrack(2, 130));
    expect(replanBodies).toHaveLength(1);

    await act(async () => {
      resolveFirstReplan?.(
        jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-1",
          playback_plan: replannedPlan,
        }),
      );
      await firstReplanResponse;
    });

    await waitFor(() => expect(result.current.plan?.plan_id).toBe("plan:2222222222222222"));
    expect(replanBodies).toHaveLength(1);

    unmount();
  });

  it("drops a queued plan-bound replan when a rotation moved the effective file", async () => {
    const initialPlan = fixturePlanV3({
      audio_tracks: [
        { codec: "eac3", channels: 6, layout: "5.1", language: "eng", default: true },
        { codec: "ac3", channels: 6, layout: "5.1", language: "spa", default: false },
      ],
    });
    let releaseReplan: ((response: Response) => void) | undefined;
    const heldReplan = new Promise<Response>((resolve) => {
      releaseReplan = resolve;
    });
    const replanBodies: Array<Record<string, unknown>> = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: initialPlan,
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/session-1/replan")) {
        replanBodies.push(JSON.parse(String(init?.body)) as Record<string, unknown>);
        return heldReplan;
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() => {
      void result.current.refreshSubtitles(120);
    });
    await waitFor(() => expect(replanBodies).toHaveLength(1));

    // Queue a plan-bound audio change (its ordinal was resolved off the plan's
    // menu), then let a serve-layer rotation move the effective file while the
    // replan is still in flight. The rotation observes the same adoption
    // barrier as an inventory push, so it is held rather than folded under the
    // pending replan.
    act(() => result.current.switchAudioTrack(1, 130));
    expect(replanBodies).toHaveLength(1);
    act(() =>
      result.current.applyCommittedSource(
        { effectiveMediaFileId: 8, effectiveVirtualUri: null, inventoryStatus: "declared" },
        [],
      ),
    );
    expect(result.current.mediaFileId).toBe(7);

    // The in-flight replan is refused without replacing the plan, so on settle
    // the held rotation folds (the plan was not replaced) while the queued
    // ordinal, built against the outgoing file, is dropped rather than
    // replayed against the rotated source.
    await act(async () => {
      releaseReplan?.(
        jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "adaptation_unavailable",
          terminal: {
            reason: "video_conversion_unsupported",
            message: "No executor can transcode this source.",
            retryable: false,
          },
        }),
      );
      await heldReplan;
    });

    await waitFor(() => expect(result.current.mediaFileId).toBe(8));
    expect(replanBodies).toHaveLength(1);
    expect(result.current.plan?.plan_id).toBe("plan:0123456789abcdef");

    unmount();
  });

  it("surfaces an error after the failure-recovery cap is exhausted", async () => {
    let replanCount = 0;
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3(),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/session-1/replan")) {
        replanCount += 1;
        return jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-1",
          playback_plan: fixturePlanV3(),
        });
      }
      if (url.endsWith("/playback/route-events")) {
        return new Response(null, { status: 202 });
      }
      if (init?.method === "DELETE") {
        return jsonResponse({ outcome: "stopped" });
      }
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.planRevision).toBe(1));

    for (let attempt = 0; attempt < 8; attempt += 1) {
      act(() => {
        result.current.recoverFromFailure({ classification: "decoder_error" }, 120 + attempt);
      });
      await waitFor(() => expect(result.current.planRevision).toBe(attempt + 2));
      // Every recovery prepares a fresh server generation even when the plan is
      // transport-identical (the A/V-byte heuristic cannot see regeneration),
      // so the transport revision must bump alongside the plan revision.
      expect(result.current.transportRevision).toBe(attempt + 2);
    }

    act(() => {
      result.current.recoverFromFailure({ classification: "decoder_error" }, 200);
    });
    await waitFor(() => {
      expect(result.current.errorTitle).toBe("Playback failed");
      expect(result.current.error).toBe("Playback failed after repeated recovery attempts.");
    });
    expect(replanCount).toBe(8);

    unmount();
  });

  it("subtitle track_change with a transport-identical plan does not bump transportRevision", async () => {
    const initialPlan = fixturePlanV3();
    const subtitlePlan = fixturePlanV3({
      plan_id: "plan:2222222222222222",
      plan_attempt_key: "v3:2222222222222222",
      subtitle: {
        mode: "render",
        track_id: "file:7:subtitle:0",
        inventory: [],
      },
    });
    const replanBodies: Array<Record<string, unknown>> = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: initialPlan,
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/session-1/replan")) {
        replanBodies.push(JSON.parse(String(init?.body)) as Record<string, unknown>);
        return jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-1",
          playback_plan: subtitlePlan,
        });
      }
      if (url.endsWith("/playback/route-events")) {
        return new Response(null, { status: 202 });
      }
      if (init?.method === "DELETE") {
        return new Response(null, { status: 204 });
      }
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());
    expect(result.current.planRevision).toBe(1);
    expect(result.current.transportRevision).toBe(1);

    act(() => result.current.changeSubtitleTrack(0, 45));
    await waitFor(() => expect(result.current.plan?.plan_id).toBe("plan:2222222222222222"));
    expect(replanBodies[0]?.operation).toBe("track_change");
    expect(result.current.planRevision).toBe(2);
    // The replan only changed the subtitle artifact; the A/V transport bytes
    // are identical, so the player must keep the mounted element.
    expect(result.current.transportRevision).toBe(1);

    unmount();
  });
});

describe("usePlaybackSession dead-session recovery", () => {
  // A plan the viewer has selections on, so the rebuild can be checked to carry
  // them rather than falling back to server defaults.
  const selectedPlan = (sessionId: string, mediaFileId = 7) =>
    fixturePlanV3({
      session_id: sessionId,
      requested_media_file_id: mediaFileId,
      effective_media_file_id: mediaFileId,
      selected_tracks: {
        audio: { id: "file:7:audio:1", index: 1 },
        subtitle: { id: "file:7:subtitle:2", index: 2 },
      },
    });

  it("rebuilds a reaped session once at the live position and selections", async () => {
    const startBodies: Array<Record<string, unknown>> = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        startBodies.push(JSON.parse(String(init?.body)) as Record<string, unknown>);
        const sessionId = startBodies.length === 1 ? "session-1" : "session-2";
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: sessionId,
            playback_plan: selectedPlan(sessionId),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/session-1/replan")) {
        return jsonResponse(
          { error: "playback_session_not_found", message: "Playback session not found" },
          { status: 404 },
        );
      }
      if (url.endsWith("/playback/route-events")) {
        return new Response(null, { status: 202 });
      }
      if (init?.method === "DELETE") {
        return jsonResponse({ outcome: "stopped" });
      }
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    // The viewer pauses at 321s, then the server reaps the session under them.
    act(() => {
      result.current.updatePlaybackState(300, true);
      result.current.updatePlaybackState(321, false);
    });

    await act(async () => {
      await result.current.reanchorSeek(321);
    });

    await waitFor(() => expect(result.current.plan?.session_id).toBe("session-2"));
    expect(startBodies).toHaveLength(2);
    const rebuild = startBodies[1]!;
    expect(rebuild.file_id).toBe(7);
    expect(rebuild.start_position).toBe(321);
    expect(rebuild.carried_audio_track_id).toBe("file:7:audio:1");
    expect(rebuild.subtitle_track_index).toBe(2);
    // The dead plan no longer owns the player, and no error dead-ends it.
    expect(result.current.error).toBeNull();
    // Play/pause state survives the rebuild.
    expect(result.current.shouldAutoPlay).toBe(false);

    // One reaped session gets exactly one rebuild: flushing further turns must
    // not produce a third start.
    await act(async () => {
      await Promise.resolve();
    });
    expect(startBodies).toHaveLength(2);

    unmount();
  });

  it("rebuilds a reaped version-switch session with its explicit selection", async () => {
    const startBodies: Array<Record<string, unknown>> = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        const body = JSON.parse(String(init?.body)) as Record<string, unknown>;
        startBodies.push(body);
        const sessionId = `session-${startBodies.length}`;
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: sessionId,
            playback_plan: selectedPlan(sessionId, (body.file_id as number) ?? 7),
          },
          { status: 201 },
        );
      }
      if (url.includes("/replan")) {
        return jsonResponse(
          { error: "playback_session_not_found", message: "Playback session not found" },
          { status: 404 },
        );
      }
      if (url.endsWith("/playback/route-events")) {
        return new Response(null, { status: 202 });
      }
      if (init?.method === "DELETE") {
        return jsonResponse({ outcome: "stopped" });
      }
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());
    // The auto-selected initial start declares the server-owned choice.
    expect(startBodies[0]).toMatchObject({ file_id: 7, file_selection: "auto" });

    // A version switch starts explicit; the viewer picked version B.
    act(() => result.current.switchVersion(99, 120));
    await waitFor(() => expect(result.current.plan?.requested_media_file_id).toBe(99));
    expect(startBodies[1]).toMatchObject({ file_id: 99, file_selection: "explicit" });

    // B is reaped under the viewer. The rebuild must carry the switch's
    // explicit selection, not revert to the mount-time auto choice; otherwise
    // the server may silently substitute another edition for B.
    await act(async () => {
      await result.current.reanchorSeek(150);
    });
    await waitFor(() => expect(startBodies).toHaveLength(3));

    const rebuild = startBodies[2]!;
    expect(rebuild.file_id).toBe(99);
    expect(rebuild.file_selection).toBe("explicit");
    expect(result.current.error).toBeNull();

    unmount();
  });

  it("does not rebuild the same dead session twice", async () => {
    const startBodies: Array<Record<string, unknown>> = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        startBodies.push(JSON.parse(String(init?.body)) as Record<string, unknown>);
        // The replacement start re-mints the same session id, so the second
        // refusal names the session already rebuilt.
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({ session_id: "session-1" }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/session-1/replan")) {
        return jsonResponse(
          { error: "playback_session_not_found", message: "Playback session not found" },
          { status: 404 },
        );
      }
      if (url.endsWith("/playback/route-events")) {
        return new Response(null, { status: 202 });
      }
      if (init?.method === "DELETE") {
        return jsonResponse({ outcome: "stopped" });
      }
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    await act(async () => {
      await result.current.reanchorSeek(300);
    });
    await waitFor(() => expect(startBodies).toHaveLength(2));

    // A second refusal for the session already rebuilt surfaces its copy
    // instead of starting a third time.
    await act(async () => {
      await result.current.reanchorSeek(600);
    });
    await waitFor(() =>
      expect(result.current.error).toBe(
        "This playback session is no longer active. Start it again to keep watching.",
      ),
    );
    expect(startBodies).toHaveLength(2);

    unmount();
  });
});

describe("usePlaybackSession server-invalidated plans", () => {
  function invalidationFetchMock(replanBodies: Array<Record<string, unknown>>, replan: unknown) {
    return vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3(),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/session-1/replan")) {
        replanBodies.push(JSON.parse(String(init?.body)) as Record<string, unknown>);
        return jsonResponse(replan);
      }
      if (url.endsWith("/playback/route-events")) {
        return new Response(null, { status: 202 });
      }
      if (init?.method === "DELETE") {
        return jsonResponse({ outcome: "stopped" });
      }
      throw new Error(`Unexpected request: ${url}`);
    });
  }

  it("recovers off the invalidated plan and excludes its attempt key", async () => {
    const replanBodies: Array<Record<string, unknown>> = [];
    vi.stubGlobal(
      "fetch",
      invalidationFetchMock(replanBodies, {
        protocol_version: 3,
        server_features: ["playback_plan_v3"],
        outcome: "playable",
        session_id: "session-1",
        playback_plan: fixturePlanV3({
          plan_id: "plan:2222222222222222",
          plan_attempt_key: "v3:2222222222222222",
          delivery: "server_transcode_hls",
        }),
      }),
    );

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan?.plan_id).toBe("plan:0123456789abcdef"));

    let outcome: boolean | undefined;
    await act(async () => {
      outcome = await result.current.invalidatePlan(
        "plan:0123456789abcdef",
        "video_copy_unsafe",
        450,
      );
    });

    // The server only pushes the command to a session that promised to handle
    // it, so the promise has to be on the wire for any of this to be reachable.
    const startCall = vi
      .mocked(fetch)
      .mock.calls.find(([url]) => String(url).endsWith("/playback/start"));
    const startBody = JSON.parse(String(startCall?.[1]?.body)) as { client_features: string[] };
    expect(startBody.client_features).toContain("plan_invalidated_v1");

    expect(outcome).toBe(true);
    expect(replanBodies).toHaveLength(1);
    expect(replanBodies[0]).toMatchObject({
      operation: "failure_recovery",
      failed_plan_id: "plan:0123456789abcdef",
      position_seconds: 450,
      // The invalidated route is excluded by key, so the replacement plan
      // cannot be the same copy route the server just disqualified.
      attempted_plan_keys: ["v3:0123456789abcdef"],
      failure: { classification: "video_copy_unsafe" },
    });
    await waitFor(() => expect(result.current.plan?.plan_id).toBe("plan:2222222222222222"));

    unmount();
  });

  it("does nothing for a plan the session already moved past", async () => {
    const replanBodies: Array<Record<string, unknown>> = [];
    vi.stubGlobal("fetch", invalidationFetchMock(replanBodies, {}));

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan?.plan_id).toBe("plan:0123456789abcdef"));

    let outcome: boolean | undefined;
    await act(async () => {
      outcome = await result.current.invalidatePlan("plan:superseded", "video_copy_unsafe", 12);
    });

    // Reported as handled: the invalidated route is already gone, and replanning
    // would evict a plan the server never complained about.
    expect(outcome).toBe(true);
    expect(replanBodies).toHaveLength(0);

    unmount();
  });

  function deferred<T>() {
    let resolve!: (value: T) => void;
    const promise = new Promise<T>((settle) => {
      resolve = settle;
    });
    return { promise, resolve };
  }

  /**
   * A start and a replan whose response is held open, so a test can act while
   * the client has a decision in flight — the state the server is always in
   * when it pushes an invalidation: it commits the replacement plan and starts
   * the copy-safety scan behind it before the response is on the wire.
   */
  function gatedReplanFetchMock(
    replanBodies: Array<Record<string, unknown>>,
    replans: unknown[],
    gate: Promise<void>,
  ) {
    return vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3(),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/session-1/replan")) {
        replanBodies.push(JSON.parse(String(init?.body)) as Record<string, unknown>);
        if (replanBodies.length === 1) await gate;
        return jsonResponse(replans.shift());
      }
      if (url.endsWith("/playback/route-events")) {
        return new Response(null, { status: 202 });
      }
      if (init?.method === "DELETE") {
        return jsonResponse({ outcome: "stopped" });
      }
      throw new Error(`Unexpected request: ${url}`);
    });
  }

  function playableDecision(plan: ReturnType<typeof fixturePlanV3>) {
    return {
      protocol_version: 3,
      server_features: ["playback_plan_v3"],
      outcome: "playable",
      session_id: "session-1",
      playback_plan: plan,
    };
  }

  // The server commits the replacement plan and starts the scan behind it
  // before the client can read the response, so the invalidation can name a
  // plan this client has not adopted yet. Deciding against the plan on screen
  // would complete the command as a no-op and then let the pending response
  // install the very route the server withdrew.
  it("waits out an in-flight replan and recovers off the plan it adopts", async () => {
    const replanBodies: Array<Record<string, unknown>> = [];
    const gate = deferred<void>();
    vi.stubGlobal(
      "fetch",
      gatedReplanFetchMock(
        replanBodies,
        [
          playableDecision(
            fixturePlanV3({
              plan_id: "plan:2222222222222222",
              plan_attempt_key: "v3:2222222222222222",
            }),
          ),
          playableDecision(
            fixturePlanV3({
              plan_id: "plan:3333333333333333",
              plan_attempt_key: "v3:3333333333333333",
              delivery: "server_transcode_hls",
            }),
          ),
        ],
        gate.promise,
      ),
    );

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan?.plan_id).toBe("plan:0123456789abcdef"));

    act(() => {
      result.current.changeQuality("720p", 100);
    });
    await waitFor(() => expect(replanBodies).toHaveLength(1));

    let invalidation: Promise<boolean> | undefined;
    act(() => {
      invalidation = result.current.invalidatePlan(
        "plan:2222222222222222",
        "video_copy_unsafe",
        100,
      );
    });
    // Nothing may be decided yet: the plan the command names is still in the
    // response the client has not read.
    expect(replanBodies).toHaveLength(1);

    let outcome: boolean | undefined;
    await act(async () => {
      gate.resolve();
      outcome = await invalidation;
    });

    expect(outcome).toBe(true);
    expect(replanBodies).toHaveLength(2);
    expect(replanBodies[1]).toMatchObject({
      operation: "failure_recovery",
      failed_plan_id: "plan:2222222222222222",
      // The plan that was invalidated mid-adoption is the one excluded, not the
      // one that was on screen when the command arrived.
      attempted_plan_keys: ["v3:2222222222222222"],
      failure: { classification: "video_copy_unsafe" },
    });
    await waitFor(() => expect(result.current.plan?.plan_id).toBe("plan:3333333333333333"));

    unmount();
  });

  // The mirror image: the client really did move past the invalidated plan
  // while the command was in flight. Waiting must not turn that into a replan —
  // it would evict a route the server never complained about.
  it("stays a no-op for a plan the in-flight replan replaced", async () => {
    const replanBodies: Array<Record<string, unknown>> = [];
    const gate = deferred<void>();
    vi.stubGlobal(
      "fetch",
      gatedReplanFetchMock(
        replanBodies,
        [
          playableDecision(
            fixturePlanV3({
              plan_id: "plan:2222222222222222",
              plan_attempt_key: "v3:2222222222222222",
            }),
          ),
        ],
        gate.promise,
      ),
    );

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan?.plan_id).toBe("plan:0123456789abcdef"));

    act(() => {
      result.current.changeQuality("720p", 100);
    });
    await waitFor(() => expect(replanBodies).toHaveLength(1));

    let invalidation: Promise<boolean> | undefined;
    act(() => {
      invalidation = result.current.invalidatePlan(
        "plan:0123456789abcdef",
        "video_copy_unsafe",
        100,
      );
    });

    let outcome: boolean | undefined;
    await act(async () => {
      gate.resolve();
      outcome = await invalidation;
    });

    // Reported as handled, with no second replan: the invalidated route is gone.
    expect(outcome).toBe(true);
    expect(replanBodies).toHaveLength(1);
    expect(result.current.plan?.plan_id).toBe("plan:2222222222222222");

    unmount();
  });

  it("reports failure when the replan produces no replacement plan", async () => {
    const replanBodies: Array<Record<string, unknown>> = [];
    vi.stubGlobal(
      "fetch",
      invalidationFetchMock(replanBodies, {
        protocol_version: 3,
        server_features: ["playback_plan_v3"],
        outcome: "adaptation_unavailable",
        terminal: {
          reason: "video_conversion_unsupported",
          message: "No executor can transcode this source.",
          retryable: false,
        },
      }),
    );

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan?.plan_id).toBe("plan:0123456789abcdef"));

    let outcome: boolean | undefined;
    await act(async () => {
      outcome = await result.current.invalidatePlan(
        "plan:0123456789abcdef",
        "video_copy_unsafe",
        30,
      );
    });

    // The caller rejects the realtime command on false, which is what makes the
    // server stop the session instead of leaving the copy route playing.
    expect(outcome).toBe(false);
    expect(replanBodies).toHaveLength(1);

    unmount();
  });
});

describe("usePlaybackSession server-invalidated plans", () => {
  // An invalidation waits out whatever is still being adopted, so it decides
  // against the plan that actually won rather than the one on screen. The wait
  // has to be scoped to the request that can still own the session: a start
  // abandoned by a version switch cannot install anything any more, and a hung
  // one would otherwise hold the invalidation past the server's 8s deadline —
  // which stops the very session that is playing fine.
  it("does not wait on a superseded start that never settles", async () => {
    let startCount = 0;
    const replanBodies: Array<{ operation: string }> = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        startCount += 1;
        if (startCount === 1) {
          // The abandoned request: it never settles, and nothing will ever
          // count it out.
          return new Promise<Response>(() => {});
        }
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-2",
            playback_plan: fixturePlanV3({ session_id: "session-2" }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/session-2/replan")) {
        replanBodies.push(JSON.parse(String(init?.body)) as { operation: string });
        return jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-2",
          playback_plan: fixturePlanV3({
            session_id: "session-2",
            plan_id: "plan:2222222222222222",
            plan_attempt_key: "v3:2222222222222222",
          }),
        });
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return jsonResponse({ outcome: "stopped" });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, rerender, unmount } = renderHook(
      ({ requestKey, fileId }: { requestKey: string; fileId: number }) =>
        usePlaybackSession(requestKey, [], [], fileId, 0, false, "auto"),
      { wrapper, initialProps: { requestKey: "episode-1", fileId: 7 } },
    );
    await waitFor(() => expect(startCount).toBe(1));

    rerender({ requestKey: "episode-2", fileId: 8 });
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    const planId = result.current.plan?.plan_id;
    if (!planId) throw new Error("expected an adopted plan");

    const outcome = await act(async () =>
      Promise.race([
        result.current.invalidatePlan(planId, "video_copy_unsafe", 120),
        new Promise<"blocked">((resolve) => {
          setTimeout(() => resolve("blocked"), 500);
        }),
      ]),
    );

    expect(outcome).toBe(true);
    expect(replanBodies.map(({ operation }) => operation)).toEqual(["failure_recovery"]);
    await waitFor(() => expect(result.current.plan?.plan_id).toBe("plan:2222222222222222"));

    unmount();
  });

  // The scoping must not weaken the guarantee it was built for: an invalidation
  // that arrives while the *current* start is still in flight still waits, so
  // it decides against the plan that response installs rather than no-opping
  // against the one already on screen.
  it("still waits for the start that currently owns the session", async () => {
    let releaseStart: ((response: Response) => void) | undefined;
    const pendingStart = new Promise<Response>((resolve) => {
      releaseStart = resolve;
    });
    let startCount = 0;
    const replanBodies: Array<{ operation: string }> = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        startCount += 1;
        return pendingStart;
      }
      if (url.endsWith("/playback/session-1/replan")) {
        replanBodies.push(JSON.parse(String(init?.body)) as { operation: string });
        return jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-1",
          playback_plan: fixturePlanV3({
            session_id: "session-1",
            plan_id: "plan:3333333333333333",
            plan_attempt_key: "v3:3333333333333333",
          }),
        });
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return jsonResponse({ outcome: "stopped" });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(startCount).toBe(1));

    // The verdict names a plan this client has not read the response for yet.
    let settled = false;
    const invalidation = result.current
      .invalidatePlan("plan:0123456789abcdef", "video_copy_unsafe", 120)
      .then((adopted) => {
        settled = true;
        return adopted;
      });
    await act(async () => {
      await Promise.resolve();
    });
    expect(settled).toBe(false);
    expect(replanBodies).toHaveLength(0);

    await act(async () => {
      releaseStart?.(
        jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({ session_id: "session-1" }),
          },
          { status: 201 },
        ),
      );
      await expect(invalidation).resolves.toBe(true);
    });
    expect(replanBodies.map(({ operation }) => operation)).toEqual(["failure_recovery"]);

    unmount();
  });
});

describe("usePlaybackSession plan audio inventory", () => {
  it("exposes the plan's audio_tracks as planAudioTracks on adoption", async () => {
    const planAudioTracks = [
      { index: 0, codec: "aac", channels: 2, layout: "stereo", language: "eng", default: true },
      { index: 1, codec: "ac3", channels: 6, layout: "5.1", language: "spa", default: false },
    ];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({ audio_tracks: planAudioTracks }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());
    expect(result.current.planAudioTracks).toEqual(planAudioTracks);
    unmount();
  });

  it("defaults planAudioTracks to an empty list when the plan publishes none", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3(),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());
    expect(result.current.planAudioTracks).toEqual([]);
    unmount();
  });

  it("fills in a richer probed inventory without bumping the plan or transport revision", async () => {
    const planAudioTracks = [
      { codec: "eac3", channels: 6, layout: "5.1", language: "eng", default: true },
    ];
    const richer = [
      { codec: "eac3", channels: 6, layout: "5.1", language: "eng", default: true },
      { codec: "ac3", channels: 6, layout: "5.1", language: "spa", index: 9, default: false },
    ];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              audio_tracks: planAudioTracks,
              subtitle: { mode: "off", inventory: [] },
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    const planRevision = result.current.planRevision;
    const transportRevision = result.current.transportRevision;

    act(() => result.current.applyAudioInventory(richer));

    expect(result.current.planAudioTracks).toEqual(richer);
    // A menu-data fill-in must not reload the stream.
    expect(result.current.planRevision).toBe(planRevision);
    expect(result.current.transportRevision).toBe(transportRevision);
    // The plan's unversioned delivery path is projected into the v2 namespace
    // (see buildPlayerStreamUrl); a menu-data fill-in must not reload it.
    expect(result.current.streamUrl).toBe("/api/v2/stream/session-1/master.m3u8?token=token");
    expect(
      fetchMock.mock.calls.filter(([url]) => String(url).endsWith("/playback/start")),
    ).toHaveLength(1);
    unmount();
  });

  it("ignores a refreshed inventory no richer than the plan's", async () => {
    const planAudioTracks = [
      { codec: "eac3", channels: 6, layout: "5.1", language: "eng", default: true },
      { codec: "ac3", channels: 6, layout: "5.1", language: "spa", default: false },
    ];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({ audio_tracks: planAudioTracks }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() =>
      result.current.applyAudioInventory([
        { codec: "eac3", channels: 6, layout: "5.1", language: "eng" },
      ]),
    );

    expect(result.current.planAudioTracks).toEqual(planAudioTracks);
    unmount();
  });

  it("replaces the inventory when the poll resolved a different file", async () => {
    const planAudioTracks = [
      { codec: "eac3", channels: 6, layout: "5.1", language: "eng", default: true },
      { codec: "ac3", channels: 6, layout: "5.1", language: "spa", default: false },
    ];
    // The plan names the collapsed row (7); the poll resolved the effective
    // candidate (8), whose probed inventory is poorer but authoritative for the
    // file actually playing.
    const poorerCandidateInventory = [
      { codec: "eac3", channels: 6, layout: "5.1", language: "eng", default: true },
    ];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              effective_media_file_id: 7,
              audio_tracks: planAudioTracks,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() => result.current.applyAudioInventory(poorerCandidateInventory, 8));

    expect(result.current.planAudioTracks).toEqual(poorerCandidateInventory);
    unmount();
  });

  it("re-keys an unchanged-length candidate when only its URI moved under the same file", async () => {
    const planAudioTracks = [
      { codec: "eac3", channels: 6, layout: "5.1", language: "eng", default: true },
    ];
    // The rotation landed on another candidate of the same resolved row: the
    // file id is unchanged and the catalogue list is the same length, so only
    // the URI distinguishes the new source.
    const sameLength = [
      { codec: "ac3", channels: 2, layout: "stereo", language: "spa", default: true },
    ];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              effective_media_file_id: 8,
              effective_virtual_uri: "virtual://movie/x?result=A",
              audio_tracks: planAudioTracks,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 8, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    const planRevision = result.current.planRevision;
    const transportRevision = result.current.transportRevision;

    act(() => result.current.applyAudioInventory(sameLength, 8, "virtual://movie/x?result=B"));

    expect(result.current.mediaFileId).toBe(8);
    expect(result.current.effectiveVirtualUri).toBe("virtual://movie/x?result=B");
    expect(result.current.planAudioTracks).toEqual(sameLength);
    expect(result.current.audioInventoryProvisional).toBe(false);
    // Menu data only: the transport must not reload.
    expect(result.current.planRevision).toBe(planRevision);
    expect(result.current.transportRevision).toBe(transportRevision);
    unmount();
  });

  it("re-keys a moved candidate with an empty list without claiming a verified inventory", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              effective_media_file_id: 8,
              effective_virtual_uri: "virtual://movie/x?result=A",
              inventory_status: "declared",
              audio_tracks: [
                { codec: "eac3", channels: 6, layout: "5.1", language: "eng", default: true },
              ],
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 8, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());
    expect(result.current.audioInventoryProvisional).toBe(true);

    act(() => result.current.applyAudioInventory([], 8, "virtual://movie/x?result=B"));

    // The identity moved and the previous release's tracks are gone, but an
    // empty list is not probe evidence: the menu stays marked.
    expect(result.current.effectiveVirtualUri).toBe("virtual://movie/x?result=B");
    expect(result.current.planAudioTracks).toEqual([]);
    expect(result.current.audioInventoryProvisional).toBe(true);
    unmount();
  });

  it("keeps the superset guard when the poll names the plan's own file", async () => {
    const planAudioTracks = [
      { codec: "eac3", channels: 6, layout: "5.1", language: "eng", default: true },
      { codec: "ac3", channels: 6, layout: "5.1", language: "spa", default: false },
    ];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              effective_media_file_id: 7,
              audio_tracks: planAudioTracks,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() =>
      result.current.applyAudioInventory(
        [{ codec: "eac3", channels: 6, layout: "5.1", language: "eng" }],
        7,
      ),
    );

    expect(result.current.planAudioTracks).toEqual(planAudioTracks);
    unmount();
  });

  it("adopts a committed source's identity and declared inventory without reloading", async () => {
    const planAudioTracks = [
      { codec: "eac3", channels: 6, layout: "5.1", language: "eng", default: true },
      { codec: "ac3", channels: 6, layout: "5.1", language: "spa", default: false },
    ];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              effective_media_file_id: 7,
              effective_virtual_uri: "virtual://movie/x?result=A",
              audio_tracks: planAudioTracks,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    const planRevision = result.current.planRevision;
    const transportRevision = result.current.transportRevision;

    act(() =>
      result.current.applyCommittedSource(
        {
          effectiveMediaFileId: 8,
          effectiveVirtualUri: "virtual://movie/x?result=B",
          inventoryStatus: "declared",
        },
        [{ codec: "eac3", channels: 6, layout: "5.1", language: "deu", default: true }],
      ),
    );

    expect(result.current.mediaFileId).toBe(8);
    expect(result.current.effectiveVirtualUri).toBe("virtual://movie/x?result=B");
    expect(result.current.planAudioTracks).toEqual([
      { codec: "eac3", channels: 6, layout: "5.1", language: "deu", default: true },
    ]);
    // A committed-source push is menu data: the stream must not reload.
    expect(result.current.planRevision).toBe(planRevision);
    expect(result.current.transportRevision).toBe(transportRevision);
    expect(
      fetchMock.mock.calls.filter(([url]) => String(url).endsWith("/playback/start")),
    ).toHaveLength(1);
    unmount();
  });

  it("clears the previous release's inventory when a committed source declares none", async () => {
    const planAudioTracks = [
      { codec: "eac3", channels: 6, layout: "5.1", language: "eng", default: true },
      { codec: "ac3", channels: 6, layout: "5.1", language: "spa", default: false },
    ];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              effective_media_file_id: 7,
              effective_virtual_uri: "virtual://movie/x?result=A",
              audio_tracks: planAudioTracks,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() =>
      result.current.applyCommittedSource(
        {
          effectiveMediaFileId: 8,
          effectiveVirtualUri: "virtual://movie/x?result=B",
          inventoryStatus: "declared",
        },
        [],
      ),
    );

    // The previous release's tracks must not survive under the new source.
    expect(result.current.planAudioTracks).toEqual([]);
    unmount();
  });

  it("carries the plan's audio identity for a pick from a repaired inventory", async () => {
    const planAudioTracks = [
      {
        codec: "eac3",
        channels: 6,
        layout: "5.1",
        language: "eng",
        default: true,
        track_id: "file:7:audio:0",
        selection_index: 0,
      },
      {
        codec: "ac3",
        channels: 2,
        layout: "stereo",
        language: "spa",
        default: false,
        track_id: "file:7:audio:1",
        selection_index: 1,
      },
    ];
    // A probe repair reveals a third track and reorders the existing two.
    const repaired: PlayerAudioTrack[] = [
      { codec: "ac3", channels: 2, layout: "stereo", language: "spa" },
      { codec: "eac3", channels: 6, layout: "5.1", language: "eng" },
      { codec: "aac", channels: 2, layout: "stereo", language: "fra" },
    ];
    const replanBodies: Array<Record<string, unknown>> = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              audio_tracks: planAudioTracks,
              selected_tracks: { audio: { id: "file:7:audio:1", index: 1 } },
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/session-1/replan")) {
        replanBodies.push(JSON.parse(String(init?.body)) as Record<string, unknown>);
        return jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-1",
          playback_plan: fixturePlanV3({
            plan_id: "plan:repaired000000001",
            plan_attempt_key: "v3:repaired000000001",
            audio_tracks: planAudioTracks,
          }),
        });
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() => result.current.applyAudioInventory(repaired, 7));
    expect(result.current.planAudioTracks).toEqual(repaired);

    // The menu's English row is at displayed index 1, but the plan holds it at
    // ordinal 0. The identity names the picked language; the client ordinal
    // rides along as the fallback.
    act(() => result.current.switchAudioTrack(1, 130));
    await waitFor(() => expect(replanBodies).toHaveLength(1));
    expect(replanBodies[0]).toMatchObject({
      operation: "track_change",
      selected_tracks: { audio: { id: "file:7:audio:0", index: 1 } },
    });

    // Picking the already-playing track resolves to the same identity and is a
    // no-op, even though its displayed index (1) no longer matches the plan's
    // selected ordinal (0).
    act(() => result.current.applyAudioInventory(repaired, 7));
    act(() => result.current.switchAudioTrack(1, 131));
    expect(replanBodies).toHaveLength(1);

    unmount();
  });

  it("exposes a declared inventory as provisional on both menus", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              inventory_status: "declared",
              inventory_provenance: "declared",
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());
    expect(result.current.audioInventoryProvisional).toBe(true);
    expect(result.current.subtitleInventoryProvisional).toBe(true);
    unmount();
  });

  it("keeps a verified inventory confident", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              inventory_status: "verified",
              inventory_provenance: "verified",
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());
    expect(result.current.audioInventoryProvisional).toBe(false);
    expect(result.current.subtitleInventoryProvisional).toBe(false);
    unmount();
  });

  it("clears only the audio flag when the poll folds in probed tracks", async () => {
    const planAudioTracks = [
      { codec: "eac3", channels: 6, layout: "5.1", language: "eng", default: true },
    ];
    const richer = [
      { codec: "eac3", channels: 6, layout: "5.1", language: "eng", default: true },
      { codec: "ac3", channels: 6, layout: "5.1", language: "spa", index: 9, default: false },
    ];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              inventory_status: "declared",
              audio_tracks: planAudioTracks,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());
    expect(result.current.audioInventoryProvisional).toBe(true);

    act(() => result.current.applyAudioInventory(richer));

    expect(result.current.planAudioTracks).toEqual(richer);
    expect(result.current.audioInventoryProvisional).toBe(false);
    // The subtitle list resolves through its own replan, not the audio poll.
    expect(result.current.subtitleInventoryProvisional).toBe(true);
    unmount();
  });

  it("folds a verified live inventory update into both menus without a replan", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              inventory_status: "declared",
              audio_tracks: [
                { codec: "eac3", channels: 6, layout: "5.1", language: "eng", default: true },
              ],
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());
    expect(result.current.audioInventoryProvisional).toBe(true);
    expect(result.current.subtitleInventoryProvisional).toBe(true);

    const verifiedAudio = [
      { codec: "eac3", channels: 6, layout: "5.1", language: "eng", default: true },
      { codec: "ac3", channels: 2, layout: "stereo", language: "spa", default: false },
    ];

    act(() =>
      result.current.applyInventoryUpdate({
        session_id: "session-1",
        inventory_revision: "inv:1",
        inventory_status: "verified",
        effective_media_file_id: 7,
        audio_tracks: verifiedAudio,
        subtitle_inventory: [
          {
            track_id: "file:7:subtitle:0",
            combined_index: 0,
            source: "embedded",
            codec: "subrip",
            language: "eng",
            forced: false,
            default: false,
            hearing_impaired: false,
            delivery: "sidecar",
            url: "/stream/session-1/subtitles/0.vtt?file_id=7",
          },
        ],
      }),
    );

    expect(result.current.planAudioTracks).toEqual(verifiedAudio);
    expect(result.current.audioInventoryProvisional).toBe(false);
    expect(result.current.subtitleInventoryProvisional).toBe(false);
    expect(result.current.subtitleUrls).toHaveLength(1);
    expect(result.current.subtitleUrls[0]?.language).toBe("eng");

    // A later verified revision that drops every subtitle clears the menu
    // instead of leaving the lighter declared entry behind.
    act(() =>
      result.current.applyInventoryUpdate({
        session_id: "session-1",
        inventory_revision: "inv:2",
        inventory_status: "verified",
        effective_media_file_id: 7,
        audio_tracks: verifiedAudio,
        subtitle_inventory: [],
      }),
    );
    expect(result.current.subtitleUrls).toHaveLength(0);
    expect(result.current.subtitleInventoryProvisional).toBe(false);

    // Stale A/B/A replay: delivering previously-seen inv:1 again is rejected.
    act(() =>
      result.current.applyInventoryUpdate({
        session_id: "session-1",
        inventory_revision: "inv:1",
        inventory_status: "verified",
        effective_media_file_id: 7,
        audio_tracks: verifiedAudio,
        subtitle_inventory: [
          {
            track_id: "file:7:subtitle:0",
            combined_index: 0,
            source: "embedded",
            codec: "subrip",
            language: "eng",
            forced: false,
            default: false,
            hearing_impaired: false,
            delivery: "sidecar",
            url: "/stream/session-1/subtitles/0.vtt?file_id=7",
          },
        ],
      }),
    );
    expect(result.current.subtitleUrls).toHaveLength(0);

    // Stale push naming another source identity (file 99) is rejected.
    act(() =>
      result.current.applyInventoryUpdate({
        session_id: "session-1",
        inventory_revision: "inv:3",
        inventory_status: "verified",
        effective_media_file_id: 99,
        audio_tracks: verifiedAudio,
        subtitle_inventory: [
          {
            track_id: "file:99:subtitle:0",
            combined_index: 0,
            source: "embedded",
            codec: "subrip",
            language: "spa",
            forced: false,
            default: false,
            hearing_impaired: false,
            delivery: "sidecar",
            url: "/stream/session-1/subtitles/0.vtt?file_id=99",
          },
        ],
      }),
    );
    expect(result.current.subtitleUrls).toHaveLength(0);

    // Menu data only: the fold never hits the transport boundary for a replan.
    expect(
      fetchMock.mock.calls.filter(([input]) => String(input).includes("/replan")),
    ).toHaveLength(0);
    unmount();
  });

  it("upgrades a declared inventory with a shorter verified list", async () => {
    const declared = [
      { codec: "eac3", channels: 6, layout: "5.1", language: "eng", default: true },
      { codec: "ac3", channels: 6, layout: "5.1", language: "spa", default: false },
    ];
    // The probe found only one real track; the declared second was synthesized.
    const verified = [
      { codec: "eac3", channels: 6, layout: "5.1", language: "eng", default: true },
    ];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              inventory_status: "declared",
              audio_tracks: declared,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());
    expect(result.current.audioInventoryProvisional).toBe(true);

    act(() => result.current.applyAudioInventory(verified));

    expect(result.current.planAudioTracks).toEqual(verified);
    expect(result.current.audioInventoryProvisional).toBe(false);
    unmount();
  });

  it("marks a committed declared source provisional after a verified plan", async () => {
    const verifiedTracks = [
      { codec: "eac3", channels: 6, layout: "5.1", language: "eng", default: true },
    ];
    const declaredTracks = [
      { codec: "eac3", channels: 6, layout: "5.1", language: "eng", default: true },
      { codec: "ac3", channels: 6, layout: "5.1", language: "spa", default: false },
    ];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              inventory_status: "verified",
              inventory_provenance: "verified",
              audio_tracks: verifiedTracks,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());
    expect(result.current.audioInventoryProvisional).toBe(false);

    // A same-source committed push carries declared metadata; it must re-mark
    // the menu provisional rather than rendering the list as final.
    act(() =>
      result.current.applyCommittedSource(
        { effectiveMediaFileId: 7, effectiveVirtualUri: null, inventoryStatus: "declared" },
        declaredTracks,
      ),
    );

    expect(result.current.planAudioTracks).toEqual(declaredTracks);
    expect(result.current.audioInventoryProvisional).toBe(true);
    unmount();
  });

  it("adopts a shorter verified committed source over a longer declared inventory", async () => {
    const declared = [
      { codec: "eac3", channels: 6, layout: "5.1", language: "eng", default: true },
      { codec: "ac3", channels: 6, layout: "5.1", language: "spa", default: false },
    ];
    const verified = [
      { codec: "eac3", channels: 6, layout: "5.1", language: "eng", default: true },
    ];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              inventory_status: "declared",
              audio_tracks: declared,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());
    expect(result.current.audioInventoryProvisional).toBe(true);

    act(() =>
      result.current.applyCommittedSource(
        { effectiveMediaFileId: 7, effectiveVirtualUri: null, inventoryStatus: "verified" },
        verified,
      ),
    );

    expect(result.current.planAudioTracks).toEqual(verified);
    expect(result.current.audioInventoryProvisional).toBe(false);
    unmount();
  });
});

describe("usePlaybackSession effective virtual source", () => {
  it("exposes the plan's effective_virtual_uri on adoption", async () => {
    const effectiveVirtualUri = "/media/Movies/Example (2024)/Example.1080p.mkv";
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              effective_media_file_id: 100,
              effective_virtual_uri: effectiveVirtualUri,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());
    // The collapsed id still names the requested frame; the path names the
    // concrete candidate the menus should adopt. It is menu data only.
    expect(result.current.mediaFileId).toBe(100);
    expect(result.current.effectiveVirtualUri).toBe(effectiveVirtualUri);
    unmount();
  });

  it("defaults effectiveVirtualUri to null when the plan omits it", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3(),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());
    expect(result.current.effectiveVirtualUri).toBeNull();
    unmount();
  });
});

describe("usePlaybackSession retryable terminals", () => {
  it("retries the same file with force_relink and coalesces a double-tap", async () => {
    const startBodies: Array<Record<string, unknown>> = [];
    let startCount = 0;
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        startCount += 1;
        startBodies.push(JSON.parse(String(init?.body)) as Record<string, unknown>);
        if (startCount === 1) {
          return jsonResponse(
            {
              protocol_version: 3,
              server_features: ["playback_plan_v3"],
              outcome: "terminal",
              terminal: {
                reason: "virtual_source_unavailable",
                message: "The virtual source could not be resolved for playback.",
                retryable: true,
              },
            },
            { status: 201 },
          );
        }
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({ session_id: "session-1" }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );

    await waitFor(() => expect(result.current.error).toBeTruthy());
    expect(result.current.errorReason).toBe("virtual_source_unavailable");
    expect(result.current.errorRetryable).toBe(true);
    expect(result.current.retrying).toBe(false);

    // A second tap while the retry is in flight must not issue a second start.
    act(() => {
      result.current.retryStart();
      result.current.retryStart();
    });

    await waitFor(() => expect(result.current.plan).not.toBeNull());
    expect(startBodies).toHaveLength(2);
    expect(startBodies[0]?.file_id).toBe(7);
    expect(startBodies[1]).toMatchObject({ file_id: 7, force_relink: true });
    expect(startBodies[1]?.playback_attempt_id).not.toBe(startBodies[0]?.playback_attempt_id);
    expect(result.current.retrying).toBe(false);
    expect(result.current.error).toBeNull();
    unmount();
  });

  it("keeps a retryable terminal off the screen and retryable after a failed retry", async () => {
    let startCount = 0;
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        startCount += 1;
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "terminal",
            terminal: {
              reason: "virtual_source_unavailable",
              message:
                startCount === 1
                  ? "The virtual source could not be resolved for playback."
                  : "The virtual source could not be refreshed for playback.",
              retryable: true,
            },
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );

    await waitFor(() => expect(result.current.error).toBeTruthy());

    act(() => result.current.retryStart());

    await waitFor(() =>
      expect(result.current.error).toBe("The virtual source could not be refreshed for playback."),
    );
    expect(result.current.errorReason).toBe("virtual_source_unavailable");
    expect(result.current.errorRetryable).toBe(true);
    expect(result.current.plan).toBeNull();
    expect(result.current.retrying).toBe(false);
    expect(startCount).toBe(2);
    unmount();
  });
});

describe("usePlaybackSession deferred identity pushes", () => {
  it("re-keys the live identity when a poll resolves another effective source", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              effective_media_file_id: 7,
              audio_tracks: [
                { codec: "eac3", channels: 6, layout: "5.1", language: "eng", default: true },
              ],
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());
    const transportRevision = result.current.transportRevision;

    // A poll resolved the effective virtual candidate while the plan still
    // names the collapsed row. The inventory and the version identity must
    // move together.
    act(() =>
      result.current.applyAudioInventory(
        [{ codec: "ac3", channels: 2, layout: "stereo", language: "spa", default: true }],
        8,
        "virtual://movie/x?result=B",
      ),
    );

    expect(result.current.mediaFileId).toBe(8);
    expect(result.current.effectiveVirtualUri).toBe("virtual://movie/x?result=B");
    expect(result.current.planAudioTracks).toEqual([
      { codec: "ac3", channels: 2, layout: "stereo", language: "spa", default: true },
    ]);
    expect(result.current.audioInventoryProvisional).toBe(false);
    // Menu data only: the transport must not reload.
    expect(result.current.transportRevision).toBe(transportRevision);
    expect(
      fetchMock.mock.calls.filter(([url]) => String(url).endsWith("/playback/start")),
    ).toHaveLength(1);
    unmount();
  });

  it("preserves the live virtual URI when an ordinary version poll moves by id alone", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              effective_media_file_id: 7,
              effective_virtual_uri: "virtual://movie/x?result=A",
              audio_tracks: [
                { codec: "eac3", channels: 6, layout: "5.1", language: "eng", default: true },
              ],
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() =>
      result.current.applyAudioInventory(
        [{ codec: "ac3", channels: 6, layout: "5.1", language: "fra", default: true }],
        8,
      ),
    );

    // The id moved but no candidate was named, so the rotation's effective URI
    // must not be cleared under the new file.
    expect(result.current.mediaFileId).toBe(8);
    expect(result.current.effectiveVirtualUri).toBe("virtual://movie/x?result=A");
    unmount();
  });

  it("applies a source commit deferred by a version switch once it settles", async () => {
    const planAudioTracks = [
      { codec: "eac3", channels: 6, layout: "5.1", language: "eng", default: true },
    ];
    const rotatedTracks = [
      { codec: "ac3", channels: 2, layout: "stereo", language: "deu", default: true },
    ];
    const startBodies: Array<{ file_id: number }> = [];
    let releaseSwitch: ((response: Response) => void) | undefined;
    const switchResponse = new Promise<Response>((resolve) => {
      releaseSwitch = resolve;
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        const body = JSON.parse(String(init?.body)) as { file_id: number };
        startBodies.push(body);
        if (startBodies.length === 2) {
          // Hold the switch open so the push lands while it rebuilds.
          return switchResponse;
        }
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              effective_media_file_id: 7,
              effective_virtual_uri: "virtual://movie/x?result=A",
              audio_tracks: planAudioTracks,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    // Switch to the very file the rotation will name. The v2 wire omits the
    // candidate URI, so only the deferred push can re-key the version menu.
    act(() => result.current.switchVersion(8, 0));
    await waitFor(() => expect(startBodies).toHaveLength(2));
    expect(result.current.replacing).toBe(true);

    act(() =>
      result.current.applyCommittedSource(
        {
          effectiveMediaFileId: 8,
          effectiveVirtualUri: "virtual://movie/x?result=B",
          inventoryStatus: "verified",
        },
        rotatedTracks,
      ),
    );
    // Under the pending switch the menus stay on the outgoing identity.
    expect(result.current.mediaFileId).toBe(7);
    expect(result.current.effectiveVirtualUri).toBe("virtual://movie/x?result=A");

    await act(async () => {
      releaseSwitch?.(
        jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-2",
          playback_plan: fixturePlanV3({
            session_id: "session-2",
            plan_id: "plan:switch-2",
            plan_attempt_key: "v3:switch-2",
            requested_media_file_id: 8,
            effective_media_file_id: 8,
            audio_tracks: [],
          }),
        }),
      );
      await switchResponse;
    });

    // The replacement plan names file 8, so the deferred commit is the
    // carrier that re-keys the candidate URI and its declared inventory.
    await waitFor(() => expect(result.current.mediaFileId).toBe(8));
    expect(result.current.effectiveVirtualUri).toBe("virtual://movie/x?result=B");
    expect(result.current.planAudioTracks).toEqual(rotatedTracks);
    unmount();
  });

  it("drops a source commit deferred by a switch when it names another file", async () => {
    const startBodies: Array<{ file_id: number }> = [];
    let releaseSwitch: ((response: Response) => void) | undefined;
    const switchResponse = new Promise<Response>((resolve) => {
      releaseSwitch = resolve;
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        const body = JSON.parse(String(init?.body)) as { file_id: number };
        startBodies.push(body);
        if (startBodies.length === 2) return switchResponse;
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              effective_media_file_id: 7,
              effective_virtual_uri: "virtual://movie/x?result=A",
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() => result.current.switchVersion(8, 0));
    await waitFor(() => expect(startBodies).toHaveLength(2));
    expect(result.current.replacing).toBe(true);

    // A stale rotation names file 9, which the switch never lands on. It must
    // not be replayed against the replacement plan.
    act(() =>
      result.current.applyCommittedSource(
        {
          effectiveMediaFileId: 9,
          effectiveVirtualUri: "virtual://movie/x?result=C",
          inventoryStatus: "verified",
        },
        [{ codec: "ac3", channels: 2, layout: "stereo", language: "ita", default: true }],
      ),
    );

    await act(async () => {
      releaseSwitch?.(
        jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-2",
          playback_plan: fixturePlanV3({
            session_id: "session-2",
            plan_id: "plan:switch-2",
            plan_attempt_key: "v3:switch-2",
            requested_media_file_id: 8,
            effective_media_file_id: 8,
            audio_tracks: [],
          }),
        }),
      );
      await switchResponse;
    });

    await waitFor(() => expect(result.current.mediaFileId).toBe(8));
    expect(result.current.effectiveVirtualUri).toBeNull();
    expect(result.current.planAudioTracks).toEqual([]);
    unmount();
  });
});

describe("usePlaybackSession deferred push authority", () => {
  const outgoingAudio = [
    { codec: "eac3", channels: 6, layout: "5.1", language: "eng", default: true },
  ];
  const rotatedAudio = [
    { codec: "ac3", channels: 2, layout: "stereo", language: "deu", default: true },
  ];
  const collisionAudio = [
    { codec: "ac3", channels: 2, layout: "stereo", language: "fra", default: true },
  ];
  // Two entries so a fold that lands is observable even when the incumbent menu
  // is non-empty and no identity move is involved.
  const richerCollisionAudio = [
    { codec: "ac3", channels: 2, layout: "stereo", language: "fra", default: true },
    { codec: "ac3", channels: 2, layout: "stereo", language: "ita", default: false },
  ];

  it("rejects an outgoing source commit even when it matches the outgoing rendered file", async () => {
    const startBodies: Array<{ file_id: number }> = [];
    let releaseSwitch: ((response: Response) => void) | undefined;
    const switchResponse = new Promise<Response>((resolve) => {
      releaseSwitch = resolve;
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        const body = JSON.parse(String(init?.body)) as { file_id: number };
        startBodies.push(body);
        if (startBodies.length === 2) return switchResponse;
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              effective_media_file_id: 7,
              effective_virtual_uri: "virtual://movie/x?result=A",
              audio_tracks: outgoingAudio,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() => result.current.switchVersion(8, 0));
    await waitFor(() => expect(startBodies).toHaveLength(2));

    // The push names the OUTGOING source (file 7, candidate A). The rendered
    // state still says file 7 at a settle path, so matching it there would let
    // this stale carrier survive; it must be rejected against the settled plan.
    act(() =>
      result.current.applyCommittedSource(
        {
          effectiveMediaFileId: 7,
          effectiveVirtualUri: "virtual://movie/x?result=A",
          inventoryStatus: "verified",
        },
        rotatedAudio,
      ),
    );

    await act(async () => {
      releaseSwitch?.(
        jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-2",
          playback_plan: fixturePlanV3({
            session_id: "session-2",
            plan_id: "plan:switch-2",
            plan_attempt_key: "v3:switch-2",
            requested_media_file_id: 8,
            effective_media_file_id: 8,
            effective_virtual_uri: "virtual://movie/x?result=B",
            audio_tracks: [],
          }),
        }),
      );
      await switchResponse;
    });

    await waitFor(() => expect(result.current.mediaFileId).toBe(8));
    expect(result.current.effectiveVirtualUri).toBe("virtual://movie/x?result=B");
    // The outgoing release's tracks must not be folded under the settled plan.
    expect(result.current.planAudioTracks).toEqual([]);
    unmount();
  });

  it("rejects a same-file candidate collision the settled plan cannot vouch for", async () => {
    let releaseReplan: ((response: Response) => void) | undefined;
    const heldReplan = new Promise<Response>((resolve) => {
      releaseReplan = resolve;
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              effective_media_file_id: 8,
              effective_virtual_uri: "virtual://movie/x?result=A",
              audio_tracks: outgoingAudio,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/session-1/replan")) return heldReplan;
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 8, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() => {
      void result.current.refreshSubtitles(120);
    });
    await waitFor(() => expect(result.current.replanning).toBe(true));

    // Candidate C rides the same collapsed file id (8) as the settled plan's B.
    // File equality alone would accept it; the plan names B, so C is stale. It
    // arrives as an inventory revision, the path that defers under a replan.
    act(() =>
      result.current.applyInventoryUpdate({
        session_id: "session-1",
        inventory_revision: "inv:collision",
        inventory_status: "verified",
        effective_media_file_id: 8,
        effective_virtual_uri: "virtual://movie/x?result=C",
        audio_tracks: collisionAudio,
      }),
    );

    await act(async () => {
      releaseReplan?.(
        jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-1",
          playback_plan: fixturePlanV3({
            plan_id: "plan:collision0001",
            plan_attempt_key: "v3:collision0001",
            effective_media_file_id: 8,
            effective_virtual_uri: "virtual://movie/x?result=B",
            audio_tracks: [],
          }),
        }),
      );
      await heldReplan;
    });

    await waitFor(() =>
      expect(result.current.plan?.effective_virtual_uri).toBe("virtual://movie/x?result=B"),
    );
    expect(result.current.effectiveVirtualUri).toBe("virtual://movie/x?result=B");
    expect(result.current.planAudioTracks).toEqual([]);
    unmount();
  });

  it("replays an older inventory before a newer source commit in arrival order", async () => {
    const startBodies: Array<{ file_id: number }> = [];
    let releaseSwitch: ((response: Response) => void) | undefined;
    const switchResponse = new Promise<Response>((resolve) => {
      releaseSwitch = resolve;
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        const body = JSON.parse(String(init?.body)) as { file_id: number };
        startBodies.push(body);
        if (startBodies.length === 2) return switchResponse;
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              effective_media_file_id: 7,
              effective_virtual_uri: "virtual://movie/x?result=A",
              audio_tracks: outgoingAudio,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() => result.current.switchVersion(8, 0));
    await waitFor(() => expect(startBodies).toHaveLength(2));

    // Older inventory revision for candidate C arrives first, then the newer
    // source commit for candidate B. The settled plan names file 8 with no
    // candidate URI, so both pass the identity gate; only arrival order decides
    // which one wins, and the newer source must.
    act(() =>
      result.current.applyInventoryUpdate({
        session_id: "session-1",
        inventory_revision: "inv:older",
        inventory_status: "verified",
        effective_media_file_id: 8,
        effective_virtual_uri: "virtual://movie/x?result=C",
        audio_tracks: collisionAudio,
      }),
    );
    act(() =>
      result.current.applyCommittedSource(
        {
          effectiveMediaFileId: 8,
          effectiveVirtualUri: "virtual://movie/x?result=B",
          inventoryStatus: "verified",
        },
        rotatedAudio,
      ),
    );

    await act(async () => {
      releaseSwitch?.(
        jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-2",
          playback_plan: fixturePlanV3({
            session_id: "session-2",
            plan_id: "plan:switch-order",
            plan_attempt_key: "v3:switch-order",
            requested_media_file_id: 8,
            effective_media_file_id: 8,
            audio_tracks: [],
          }),
        }),
      );
      await switchResponse;
    });

    await waitFor(() => expect(result.current.mediaFileId).toBe(8));
    // The newer source commit wins; the older inventory must not overwrite it.
    expect(result.current.effectiveVirtualUri).toBe("virtual://movie/x?result=B");
    expect(result.current.planAudioTracks).toEqual(rotatedAudio);
    unmount();
  });

  it("folds an inventory revision deferred by a replan once the replacement plan adopts it", async () => {
    let releaseReplan: ((response: Response) => void) | undefined;
    const heldReplan = new Promise<Response>((resolve) => {
      releaseReplan = resolve;
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              effective_media_file_id: 7,
              effective_virtual_uri: "virtual://movie/x?result=A",
              audio_tracks: outgoingAudio,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/session-1/replan")) return heldReplan;
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() => {
      void result.current.refreshSubtitles(120);
    });
    await waitFor(() => expect(result.current.replanning).toBe(true));

    // The push names the replacement plan's incoming identity, which is only
    // knowable once the plan settles; it must be held, not dropped.
    act(() =>
      result.current.applyInventoryUpdate({
        session_id: "session-1",
        inventory_revision: "inv:deferred",
        inventory_status: "verified",
        effective_media_file_id: 8,
        effective_virtual_uri: "virtual://movie/x?result=B",
        audio_tracks: rotatedAudio,
      }),
    );

    await act(async () => {
      releaseReplan?.(
        jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-1",
          playback_plan: fixturePlanV3({
            plan_id: "plan:deferred00001",
            plan_attempt_key: "v3:deferred00001",
            effective_media_file_id: 8,
            effective_virtual_uri: "virtual://movie/x?result=B",
            audio_tracks: [],
          }),
        }),
      );
      await heldReplan;
    });

    await waitFor(() => expect(result.current.mediaFileId).toBe(8));
    expect(result.current.effectiveVirtualUri).toBe("virtual://movie/x?result=B");
    expect(result.current.planAudioTracks).toEqual(rotatedAudio);
    unmount();
  });

  it("clears a deferred push when a new request retires the session", async () => {
    const startBodies: Array<{ file_id: number }> = [];
    const switchResponse = new Promise<Response>(() => {
      // Hold the pending switch open forever; the new request retires it before
      // its replacement ever lands.
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        const body = JSON.parse(String(init?.body)) as { file_id: number };
        startBodies.push(body);
        if (startBodies.length === 2) return switchResponse;
        if (startBodies.length === 3) {
          return jsonResponse(
            {
              protocol_version: 3,
              server_features: ["playback_plan_v3"],
              outcome: "playable",
              session_id: "session-3",
              playback_plan: fixturePlanV3({
                session_id: "session-3",
                plan_id: "plan:episode-two",
                plan_attempt_key: "v3:episode-two",
                effective_media_file_id: 9,
                effective_virtual_uri: "virtual://movie/x?result=D",
                audio_tracks: [],
              }),
            },
            { status: 201 },
          );
        }
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              effective_media_file_id: 7,
              effective_virtual_uri: "virtual://movie/x?result=A",
              audio_tracks: outgoingAudio,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, rerender, unmount } = renderHook(
      ({ requestKey, fileId }: { requestKey: string; fileId: number }) =>
        usePlaybackSession(requestKey, [], [], fileId, 0, false, "auto"),
      { wrapper, initialProps: { requestKey: "episode-1", fileId: 7 } },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() => result.current.switchVersion(8, 0));
    await waitFor(() => expect(startBodies).toHaveLength(2));

    act(() =>
      result.current.applyCommittedSource(
        {
          effectiveMediaFileId: 8,
          effectiveVirtualUri: "virtual://movie/x?result=B",
          inventoryStatus: "verified",
        },
        rotatedAudio,
      ),
    );

    // A new request retires the pending switch; its deferred push must not leak
    // into the replacement session.
    rerender({ requestKey: "episode-2", fileId: 9 });

    await waitFor(() => expect(result.current.mediaFileId).toBe(9));
    expect(result.current.effectiveVirtualUri).toBe("virtual://movie/x?result=D");
    expect(result.current.planAudioTracks).toEqual([]);
    unmount();
  });

  it("drops a same-file sibling inventory after the live source already rotated", async () => {
    // The plan owns file 8 without naming a candidate (v2 wire). A poll moves
    // the live source to candidate A. During a replacement that also settles on
    // file 8 without a candidate, a C inventory is queued. C is a same-file
    // sibling of the live outgoing A: the URI-less plan cannot vouch for it and
    // the concrete live URI contradicts it, so it is a genuine collision and is
    // dropped. (A same-file push that agrees with the live source is admitted;
    // see the URI-less acceptance regression below.)
    const startBodies: Array<{ file_id: number }> = [];
    let releaseSwitch: ((response: Response) => void) | undefined;
    const switchResponse = new Promise<Response>((resolve) => {
      releaseSwitch = resolve;
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        const body = JSON.parse(String(init?.body)) as { file_id: number };
        startBodies.push(body);
        if (startBodies.length === 2) return switchResponse;
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              effective_media_file_id: 8,
              audio_tracks: outgoingAudio,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 8, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    // A poll resolves the live source to candidate A under the same file.
    act(() => result.current.applyAudioInventory(collisionAudio, 8, "virtual://movie/x?result=A"));
    expect(result.current.effectiveVirtualUri).toBe("virtual://movie/x?result=A");

    act(() => result.current.switchVersion(9, 0));
    await waitFor(() => expect(startBodies).toHaveLength(2));

    // Candidate C on the same file is deferred behind the pending replacement.
    // It contradicts the live outgoing A, so the URI-less winner cannot vouch
    // for it and it must not be folded.
    act(() =>
      result.current.applyInventoryUpdate({
        session_id: "session-1",
        inventory_revision: "inv:sibling-c",
        inventory_status: "verified",
        effective_media_file_id: 8,
        effective_virtual_uri: "virtual://movie/x?result=C",
        audio_tracks: outgoingAudio,
      }),
    );
    expect(result.current.effectiveVirtualUri).toBe("virtual://movie/x?result=A");

    await act(async () => {
      releaseSwitch?.(
        jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-2",
          playback_plan: fixturePlanV3({
            session_id: "session-2",
            plan_id: "plan:live-a-2",
            plan_attempt_key: "v3:live-a-2",
            requested_media_file_id: 9,
            // The replacement falls back onto the same file 8, URI-less.
            effective_media_file_id: 8,
            audio_tracks: [],
          }),
        }),
      );
      await switchResponse;
    });

    await waitFor(() => expect(result.current.sessionId).toBe("session-2"));
    // Candidate C is a same-file sibling of the live outgoing A, so the
    // URI-less winner cannot vouch for it and it is dropped; the plan's own
    // (empty, candidate-less) identity is what stands.
    expect(result.current.effectiveVirtualUri).toBeNull();
    expect(result.current.planAudioTracks).toEqual([]);
    unmount();
  });

  it("admits a same-file inventory push against a URI-less settled plan", async () => {
    // The v2 start wire omits the candidate URI, so the settled replacement plan
    // names only file 8. The verified inventory push names that same file with
    // the candidate the plan leaves out; it is the only carrier of the live
    // identity and must be folded rather than refused for the plan's silence.
    const startBodies: Array<{ file_id: number }> = [];
    let releaseSwitch: ((response: Response) => void) | undefined;
    const switchResponse = new Promise<Response>((resolve) => {
      releaseSwitch = resolve;
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        const body = JSON.parse(String(init?.body)) as { file_id: number };
        startBodies.push(body);
        if (startBodies.length === 2) return switchResponse;
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              effective_media_file_id: 7,
              effective_virtual_uri: "virtual://movie/x?result=A",
              audio_tracks: outgoingAudio,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() => result.current.switchVersion(8, 0));
    await waitFor(() => expect(startBodies).toHaveLength(2));

    // A verified inventory for candidate B on the replacement's file 8 is
    // deferred behind the pending switch.
    act(() =>
      result.current.applyInventoryUpdate({
        session_id: "session-1",
        inventory_revision: "inv:uri-less",
        inventory_status: "verified",
        effective_media_file_id: 8,
        effective_virtual_uri: "virtual://movie/x?result=B",
        audio_tracks: rotatedAudio,
      }),
    );

    await act(async () => {
      releaseSwitch?.(
        jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-2",
          playback_plan: fixturePlanV3({
            session_id: "session-2",
            plan_id: "plan:uri-less-2",
            plan_attempt_key: "v3:uri-less-2",
            requested_media_file_id: 8,
            effective_media_file_id: 8,
            audio_tracks: [],
          }),
        }),
      );
      await switchResponse;
    });

    await waitFor(() => expect(result.current.mediaFileId).toBe(8));
    // The push names the plan's own file, so it is folded; the plan itself
    // carries no candidate URI.
    expect(result.current.plan?.effective_virtual_uri).toBeUndefined();
    expect(result.current.effectiveVirtualUri).toBe("virtual://movie/x?result=B");
    expect(result.current.planAudioTracks).toEqual(rotatedAudio);
    unmount();
  });

  it("holds an older inventory behind a newer source commit during a replan", async () => {
    // A replanning session defers an inventory for candidate C, then a newer
    // source commit for candidate B arrives while the same replan is still in
    // flight. Both must observe the adoption barrier and flush in arrival
    // order against the URI-less winning plan, so B wins.
    let releaseReplan: ((response: Response) => void) | undefined;
    const heldReplan = new Promise<Response>((resolve) => {
      releaseReplan = resolve;
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              effective_media_file_id: 7,
              effective_virtual_uri: "virtual://movie/x?result=A",
              audio_tracks: outgoingAudio,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/session-1/replan")) return heldReplan;
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() => {
      void result.current.refreshSubtitles(120);
    });
    await waitFor(() => expect(result.current.replanning).toBe(true));

    // Older inventory for C, then the newer source commit for B. The replan
    // response settles on file 8 with no candidate URI.
    act(() =>
      result.current.applyInventoryUpdate({
        session_id: "session-1",
        inventory_revision: "inv:older",
        inventory_status: "verified",
        effective_media_file_id: 8,
        effective_virtual_uri: "virtual://movie/x?result=C",
        audio_tracks: collisionAudio,
      }),
    );
    act(() =>
      result.current.applyCommittedSource(
        {
          effectiveMediaFileId: 8,
          effectiveVirtualUri: "virtual://movie/x?result=B",
          inventoryStatus: "verified",
        },
        rotatedAudio,
      ),
    );
    // Both are held while the replan owns the session.
    expect(result.current.mediaFileId).toBe(7);

    await act(async () => {
      releaseReplan?.(
        jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-1",
          playback_plan: fixturePlanV3({
            plan_id: "plan:order000000001",
            plan_attempt_key: "v3:order000000001",
            effective_media_file_id: 8,
            audio_tracks: [],
          }),
        }),
      );
      await heldReplan;
    });

    await waitFor(() => expect(result.current.mediaFileId).toBe(8));
    // The URI-less plan cannot distinguish B from C, so only the newest
    // rotation (B) may be the carrier; the older inventory (C) must not win.
    expect(result.current.effectiveVirtualUri).toBe("virtual://movie/x?result=B");
    expect(result.current.planAudioTracks).toEqual(rotatedAudio);
    unmount();
  });

  it("folds a source commit deferred by a replan when the plan keeps the file", async () => {
    // Exercises the source-commit path (not the inventory path): a rotation to
    // a new candidate on the same file lands inside a replan, and the settled
    // URI-less plan still names that file, so the commit is admitted.
    let releaseReplan: ((response: Response) => void) | undefined;
    const heldReplan = new Promise<Response>((resolve) => {
      releaseReplan = resolve;
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              effective_media_file_id: 7,
              effective_virtual_uri: "virtual://movie/x?result=A",
              audio_tracks: outgoingAudio,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/session-1/replan")) return heldReplan;
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() => {
      void result.current.refreshSubtitles(120);
    });
    await waitFor(() => expect(result.current.replanning).toBe(true));

    // A rotation moves the transport to candidate B on file 8 while the replan
    // is in flight; the response settles on the same file with no candidate.
    act(() =>
      result.current.applyCommittedSource(
        {
          effectiveMediaFileId: 8,
          effectiveVirtualUri: "virtual://movie/x?result=B",
          inventoryStatus: "verified",
        },
        rotatedAudio,
      ),
    );
    expect(result.current.mediaFileId).toBe(7);

    await act(async () => {
      releaseReplan?.(
        jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-1",
          playback_plan: fixturePlanV3({
            plan_id: "plan:commit-replan01",
            plan_attempt_key: "v3:commit-replan01",
            effective_media_file_id: 8,
            audio_tracks: [],
          }),
        }),
      );
      await heldReplan;
    });

    // The winning plan names file 8; the deferred commit is the carrier that
    // re-keys the candidate to B and its inventory.
    await waitFor(() => expect(result.current.mediaFileId).toBe(8));
    expect(result.current.effectiveVirtualUri).toBe("virtual://movie/x?result=B");
    expect(result.current.planAudioTracks).toEqual(rotatedAudio);
    unmount();
  });

  it("keeps a deferred push whose identity the winning plan retains", async () => {
    // The replacement retains the effective candidate the session was already
    // on. The winner explicitly names it, so the deferred probe update for that
    // candidate is kept rather than rejected as an outgoing carrier.
    const startBodies: Array<{ file_id: number }> = [];
    let releaseSwitch: ((response: Response) => void) | undefined;
    const switchResponse = new Promise<Response>((resolve) => {
      releaseSwitch = resolve;
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        const body = JSON.parse(String(init?.body)) as { file_id: number };
        startBodies.push(body);
        if (startBodies.length === 2) return switchResponse;
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              effective_media_file_id: 7,
              effective_virtual_uri: "virtual://movie/x?result=A",
              audio_tracks: outgoingAudio,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() => result.current.switchVersion(8, 0));
    await waitFor(() => expect(startBodies).toHaveLength(2));

    // A probe update names the very candidate the replacement will retain.
    act(() =>
      result.current.applyInventoryUpdate({
        session_id: "session-1",
        inventory_revision: "inv:retained",
        inventory_status: "verified",
        effective_media_file_id: 7,
        effective_virtual_uri: "virtual://movie/x?result=A",
        audio_tracks: rotatedAudio,
      }),
    );

    await act(async () => {
      releaseSwitch?.(
        jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-2",
          playback_plan: fixturePlanV3({
            session_id: "session-2",
            plan_id: "plan:retained-2",
            plan_attempt_key: "v3:retained-2",
            requested_media_file_id: 8,
            // The replacement falls back onto the same candidate A, file 7.
            effective_media_file_id: 7,
            effective_virtual_uri: "virtual://movie/x?result=A",
            audio_tracks: [],
          }),
        }),
      );
      await switchResponse;
    });

    await waitFor(() => expect(result.current.mediaFileId).toBe(7));
    // The winner names candidate A, so the corroborating verified update is
    // applied rather than discarded as the outgoing source.
    expect(result.current.effectiveVirtualUri).toBe("virtual://movie/x?result=A");
    expect(result.current.planAudioTracks).toEqual(rotatedAudio);
    unmount();
  });

  it("preserves a legitimate rotation when a URI-ful original plan is refused", async () => {
    let releaseReplan: ((response: Response) => void) | undefined;
    const heldReplan = new Promise<Response>((resolve) => {
      releaseReplan = resolve;
    });
    const replanBodies: Array<Record<string, unknown>> = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              effective_media_file_id: 7,
              effective_virtual_uri: "virtual://movie/x?result=A",
              audio_tracks: outgoingAudio,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/session-1/replan")) {
        replanBodies.push(JSON.parse(String(init?.body)) as Record<string, unknown>);
        return heldReplan;
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() => {
      void result.current.refreshSubtitles(120);
    });
    await waitFor(() => expect(result.current.replanning).toBe(true));

    // The transport rotates to file 8 candidate B while the replan is in
    // flight. The plan being replanned names file 7 candidate A, so when the
    // replan is refused the original URI-ful plan is still the live plan and
    // must not veto the rotation the transport already committed to.
    act(() =>
      result.current.applyCommittedSource(
        {
          effectiveMediaFileId: 8,
          effectiveVirtualUri: "virtual://movie/x?result=B",
          inventoryStatus: "verified",
        },
        rotatedAudio,
      ),
    );
    expect(result.current.mediaFileId).toBe(7);

    await act(async () => {
      releaseReplan?.(
        jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "adaptation_unavailable",
          terminal: {
            reason: "video_conversion_unsupported",
            message: "No executor can transcode this source.",
            retryable: false,
          },
        }),
      );
      await heldReplan;
    });

    // The plan was refused, not replaced, so it still names file 7 candidate A.
    // The rotation to B must survive: the refusal is not evidence that the
    // transport stayed on A.
    await waitFor(() => expect(result.current.mediaFileId).toBe(8));
    expect(result.current.effectiveVirtualUri).toBe("virtual://movie/x?result=B");
    expect(result.current.planAudioTracks).toEqual(rotatedAudio);
    expect(result.current.plan?.plan_id).toBe("plan:0123456789abcdef");
    expect(replanBodies).toHaveLength(1);
    unmount();
  });

  it("uses the live rotation as the outgoing baseline before the next render", async () => {
    let releaseReplan: ((response: Response) => void) | undefined;
    const heldReplan = new Promise<Response>((resolve) => {
      releaseReplan = resolve;
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              effective_media_file_id: 7,
              effective_virtual_uri: "virtual://movie/x?result=A",
              audio_tracks: outgoingAudio,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/session-1/replan")) return heldReplan;
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() => {
      void result.current.refreshSubtitles(120);
    });
    await waitFor(() => expect(result.current.replanning).toBe(true));

    // No render between the poll rotation and the deferred push: the menus move
    // to file 8 candidate B, then a source commit for the same file's other
    // candidate C is held behind the replan. The next transition must read the
    // rotation, not the pre-rotation rendered state; otherwise the URI-less
    // winner would misread C as a cross-file rotation off file 7 and admit it.
    act(() => {
      result.current.applyAudioInventory(rotatedAudio, 8, "virtual://movie/x?result=B");
      result.current.applyCommittedSource(
        {
          effectiveMediaFileId: 8,
          effectiveVirtualUri: "virtual://movie/x?result=C",
          inventoryStatus: "verified",
        },
        collisionAudio,
      );
    });

    await act(async () => {
      releaseReplan?.(
        jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-1",
          playback_plan: fixturePlanV3({
            plan_id: "plan:no-render001",
            plan_attempt_key: "v3:no-render001",
            effective_media_file_id: 8,
            audio_tracks: [],
          }),
        }),
      );
      await heldReplan;
    });

    await waitFor(() => expect(result.current.mediaFileId).toBe(8));
    // The winner is URI-less on file 8 and the live rotation is already B, so C
    // shares the live file and cannot be proven. It must not be folded.
    expect(result.current.effectiveVirtualUri).toBeNull();
    expect(result.current.planAudioTracks).toEqual([]);
    unmount();
  });

  it("refolds a refused inventory revision once a later rotation matches it", async () => {
    // A revision refused by a flush is not the same statement as a revision that
    // was applied: the refusal is about the settled plan at that moment, and the
    // live source can still move onto the candidate the revision names later in
    // the same generation. Recording the refusal would swallow that redelivery
    // and leave the track menus stale for the rest of the session.
    let releaseReplan: ((response: Response) => void) | undefined;
    const heldReplan = new Promise<Response>((resolve) => {
      releaseReplan = resolve;
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              effective_media_file_id: 8,
              effective_virtual_uri: "virtual://movie/x?result=B",
              audio_tracks: outgoingAudio,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/session-1/replan")) return heldReplan;
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 8, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() => {
      void result.current.refreshSubtitles(120);
    });
    await waitFor(() => expect(result.current.replanning).toBe(true));

    // A revision for candidate C is held behind the replan. The settled
    // replacement names candidate B, so C is a same-file collision and is
    // refused rather than folded.
    act(() =>
      result.current.applyInventoryUpdate({
        session_id: "session-1",
        inventory_revision: "inv:redelivered",
        inventory_status: "verified",
        effective_media_file_id: 8,
        effective_virtual_uri: "virtual://movie/x?result=C",
        audio_tracks: collisionAudio,
      }),
    );

    await act(async () => {
      releaseReplan?.(
        jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-1",
          playback_plan: fixturePlanV3({
            plan_id: "plan:redeliver0001",
            plan_attempt_key: "v3:redeliver0001",
            effective_media_file_id: 8,
            effective_virtual_uri: "virtual://movie/x?result=B",
            audio_tracks: [],
          }),
        }),
      );
      await heldReplan;
    });

    await waitFor(() => expect(result.current.plan?.plan_id).toBe("plan:redeliver0001"));
    expect(result.current.effectiveVirtualUri).toBe("virtual://movie/x?result=B");
    expect(result.current.planAudioTracks).toEqual([]);

    // A rotation moves the live source onto C with an empty inventory. The
    // revision the flush refused is still unrecorded, so the redelivery now
    // matches the live identity and must fold.
    act(() =>
      result.current.applyCommittedSource(
        {
          effectiveMediaFileId: 8,
          effectiveVirtualUri: "virtual://movie/x?result=C",
          inventoryStatus: "verified",
        },
        [],
      ),
    );
    expect(result.current.effectiveVirtualUri).toBe("virtual://movie/x?result=C");
    expect(result.current.planAudioTracks).toEqual([]);

    act(() =>
      result.current.applyInventoryUpdate({
        session_id: "session-1",
        inventory_revision: "inv:redelivered",
        inventory_status: "verified",
        effective_media_file_id: 8,
        effective_virtual_uri: "virtual://movie/x?result=C",
        audio_tracks: collisionAudio,
      }),
    );
    expect(result.current.planAudioTracks).toEqual(collisionAudio);

    // Now that the revision has genuinely been applied, its duplicate is
    // suppressed: a redelivery with a different inventory must not overwrite.
    act(() =>
      result.current.applyInventoryUpdate({
        session_id: "session-1",
        inventory_revision: "inv:redelivered",
        inventory_status: "verified",
        effective_media_file_id: 8,
        effective_virtual_uri: "virtual://movie/x?result=C",
        audio_tracks: rotatedAudio,
      }),
    );
    expect(result.current.planAudioTracks).toEqual(collisionAudio);
    unmount();
  });

  it("admits a deferred URI-only inventory push against a URI-less settled plan", async () => {
    // A partial push may name only the candidate URI and no file id. It is
    // deferred behind a replan whose replacement plan is URI-less (v2 wire) and
    // the live source is URI-less too, so the URI is the only key the push
    // offers. It must be folded rather than dropped for the missing file.
    let releaseReplan: ((response: Response) => void) | undefined;
    const heldReplan = new Promise<Response>((resolve) => {
      releaseReplan = resolve;
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              effective_media_file_id: 8,
              audio_tracks: outgoingAudio,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/session-1/replan")) return heldReplan;
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 8, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() => {
      void result.current.refreshSubtitles(120);
    });
    await waitFor(() => expect(result.current.replanning).toBe(true));

    act(() =>
      result.current.applyInventoryUpdate({
        session_id: "session-1",
        inventory_revision: "inv:uri-only",
        inventory_status: "verified",
        effective_virtual_uri: "virtual://movie/x?result=B",
        audio_tracks: rotatedAudio,
      }),
    );

    await act(async () => {
      releaseReplan?.(
        jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-1",
          playback_plan: fixturePlanV3({
            plan_id: "plan:uri-only0001",
            plan_attempt_key: "v3:uri-only0001",
            effective_media_file_id: 8,
            audio_tracks: [],
          }),
        }),
      );
      await heldReplan;
    });

    await waitFor(() => expect(result.current.plan?.plan_id).toBe("plan:uri-only0001"));
    expect(result.current.effectiveVirtualUri).toBe("virtual://movie/x?result=B");
    expect(result.current.planAudioTracks).toEqual(rotatedAudio);
    unmount();
  });

  it("drops a file-only inventory under a concrete live candidate on the same file", async () => {
    // The plan is URI-less on file 8 and a poll has resolved the live source to
    // candidate A. A later file-only inventory revision names file 8 but no
    // candidate, so it cannot be tied to A; folding it would overwrite the
    // poll's proven inventory with an unproven one.
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              effective_media_file_id: 8,
              audio_tracks: outgoingAudio,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 8, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() => result.current.applyAudioInventory(rotatedAudio, 8, "virtual://movie/x?result=A"));
    expect(result.current.effectiveVirtualUri).toBe("virtual://movie/x?result=A");
    expect(result.current.planAudioTracks).toEqual(rotatedAudio);

    act(() =>
      result.current.applyInventoryUpdate({
        session_id: "session-1",
        inventory_revision: "inv:file-only",
        inventory_status: "verified",
        effective_media_file_id: 8,
        audio_tracks: richerCollisionAudio,
      }),
    );

    // The concrete live candidate A stands; the file-only revision is refused
    // even though its richer list would otherwise replace the menu.
    expect(result.current.effectiveVirtualUri).toBe("virtual://movie/x?result=A");
    expect(result.current.planAudioTracks).toEqual(rotatedAudio);
    unmount();
  });

  it("drops a file-only inventory against a URI-ful settled plan via the direct path", async () => {
    // The settled plan names candidate B by URI. A file-only inventory revision
    // (the shape a refused-then-redelivered revision takes) names the plan's file
    // but no candidate, so the direct path must not fold it over B's inventory.
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              effective_media_file_id: 8,
              effective_virtual_uri: "virtual://movie/x?result=B",
              audio_tracks: outgoingAudio,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 8, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() =>
      result.current.applyInventoryUpdate({
        session_id: "session-1",
        inventory_revision: "inv:file-only-uri-ful",
        inventory_status: "verified",
        effective_media_file_id: 8,
        audio_tracks: richerCollisionAudio,
      }),
    );

    expect(result.current.effectiveVirtualUri).toBe("virtual://movie/x?result=B");
    expect(result.current.planAudioTracks).toEqual(outgoingAudio);
    unmount();
  });

  it("refuses a file-only inventory deferred before a poll folded a same-file candidate", async () => {
    // A FILE-ONLY inventory revision is queued behind a replan. Its captured
    // outgoing identity is URI-less, so the arrival-time baseline alone would
    // admit it. While it waits, a poll (which bypasses the adoption barrier)
    // folds candidate B on the same file. The flush must judge the queued push
    // against the candidate the menus now carry, not only the arrival capture:
    // otherwise the unproven file-only revision overwrites B's proven inventory.
    let releaseReplan: ((response: Response) => void) | undefined;
    const heldReplan = new Promise<Response>((resolve) => {
      releaseReplan = resolve;
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              effective_media_file_id: 8,
              audio_tracks: outgoingAudio,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/session-1/replan")) return heldReplan;
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 8, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() => {
      void result.current.refreshSubtitles(120);
    });
    await waitFor(() => expect(result.current.replanning).toBe(true));

    // The file-only inventory is held; its captured outgoing identity is
    // URI-less, and the richer list would replace a one-entry menu.
    act(() =>
      result.current.applyInventoryUpdate({
        session_id: "session-1",
        inventory_revision: "inv:file-only-deferred",
        inventory_status: "verified",
        effective_media_file_id: 8,
        audio_tracks: richerCollisionAudio,
      }),
    );
    // The poll folds candidate B under the same file while the push is queued.
    act(() => result.current.applyAudioInventory(rotatedAudio, 8, "virtual://movie/x?result=B"));
    expect(result.current.effectiveVirtualUri).toBe("virtual://movie/x?result=B");

    await act(async () => {
      releaseReplan?.(
        jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "adaptation_unavailable",
          terminal: {
            reason: "video_conversion_unsupported",
            message: "No executor can transcode this source.",
            retryable: false,
          },
        }),
      );
      await heldReplan;
    });

    // The plan was refused, so it still stands URI-less on file 8; the live source
    // is the folded B. The file-only revision must not overwrite B's inventory.
    expect(result.current.effectiveVirtualUri).toBe("virtual://movie/x?result=B");
    expect(result.current.planAudioTracks).toEqual(rotatedAudio);
    unmount();
  });

  it("refuses a file-only inventory deferred while a concrete candidate is live", async () => {
    // A poll moves the live source to candidate B on file 8, then a switch starts
    // and a file-only inventory revision for file 8 is deferred. The replacement
    // settles URI-less on file 8, so file equality alone would admit it; the
    // concrete arrival-time live candidate B must refuse it instead.
    const startBodies: Array<{ file_id: number }> = [];
    let releaseSwitch: ((response: Response) => void) | undefined;
    const switchResponse = new Promise<Response>((resolve) => {
      releaseSwitch = resolve;
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        const body = JSON.parse(String(init?.body)) as { file_id: number };
        startBodies.push(body);
        if (startBodies.length === 2) return switchResponse;
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              effective_media_file_id: 7,
              effective_virtual_uri: "virtual://movie/x?result=A",
              audio_tracks: outgoingAudio,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    // The poll folds candidate B on file 8 before the switch barrier opens.
    act(() => result.current.applyAudioInventory(rotatedAudio, 8, "virtual://movie/x?result=B"));
    act(() => result.current.switchVersion(9, 0));
    await waitFor(() => expect(startBodies).toHaveLength(2));

    act(() =>
      result.current.applyInventoryUpdate({
        session_id: "session-1",
        inventory_revision: "inv:file-only-live",
        inventory_status: "verified",
        effective_media_file_id: 8,
        audio_tracks: richerCollisionAudio,
      }),
    );

    await act(async () => {
      releaseSwitch?.(
        jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-2",
          playback_plan: fixturePlanV3({
            session_id: "session-2",
            plan_id: "plan:file-only-live2",
            plan_attempt_key: "v3:file-only-live2",
            requested_media_file_id: 9,
            // The replacement falls back onto file 8, URI-less.
            effective_media_file_id: 8,
            audio_tracks: [],
          }),
        }),
      );
      await switchResponse;
    });

    await waitFor(() => expect(result.current.sessionId).toBe("session-2"));
    // The replacement plan is URI-less on file 8, and the file-only revision is
    // refused against the concrete candidate that was live when it arrived, so
    // its richer track list is not folded and the plan's own (empty) identity
    // and inventory stand.
    expect(result.current.effectiveVirtualUri).toBeNull();
    expect(result.current.planAudioTracks).toEqual([]);
    unmount();
  });

  it("keeps the newer of two same-file inventories replayed in one flush", async () => {
    // Two concrete inventory revisions for the same file are queued behind a
    // replan: C first, then B. The settled plan is URI-less on file 8, so both
    // pass the identity gate and arrival order must decide — B, sent later, wins.
    // If the flush judged each entry against the menu identity an earlier entry
    // had already moved, C would veto B and invert arrival order.
    let releaseReplan: ((response: Response) => void) | undefined;
    const heldReplan = new Promise<Response>((resolve) => {
      releaseReplan = resolve;
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              effective_media_file_id: 7,
              effective_virtual_uri: "virtual://movie/x?result=A",
              audio_tracks: outgoingAudio,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/session-1/replan")) return heldReplan;
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() => {
      void result.current.refreshSubtitles(120);
    });
    await waitFor(() => expect(result.current.replanning).toBe(true));

    act(() =>
      result.current.applyInventoryUpdate({
        session_id: "session-1",
        inventory_revision: "inv:order-c",
        inventory_status: "verified",
        effective_media_file_id: 8,
        effective_virtual_uri: "virtual://movie/x?result=C",
        audio_tracks: collisionAudio,
      }),
    );
    act(() =>
      result.current.applyInventoryUpdate({
        session_id: "session-1",
        inventory_revision: "inv:order-b",
        inventory_status: "verified",
        effective_media_file_id: 8,
        effective_virtual_uri: "virtual://movie/x?result=B",
        audio_tracks: rotatedAudio,
      }),
    );

    await act(async () => {
      releaseReplan?.(
        jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-1",
          playback_plan: fixturePlanV3({
            plan_id: "plan:order-b0000001",
            plan_attempt_key: "v3:order-b0000001",
            effective_media_file_id: 8,
            audio_tracks: [],
          }),
        }),
      );
      await heldReplan;
    });

    await waitFor(() => expect(result.current.mediaFileId).toBe(8));
    // The later inventory (B) wins; the earlier one (C) must not veto it.
    expect(result.current.effectiveVirtualUri).toBe("virtual://movie/x?result=B");
    expect(result.current.planAudioTracks).toEqual(rotatedAudio);
    unmount();
  });

  it("admits a queued push naming the replaced plan's explicitly winning candidate", async () => {
    // The session was on (file 7, candidate A) and the viewer explicitly picked
    // a row the server resolved to the same file's candidate B. A probe push for
    // B is queued behind the switch. The outgoing candidate A must not veto the
    // plan the server just selected: a replaced URI-ful winner is decided by
    // exact identity before any ambiguity guard.
    const startBodies: Array<{ file_id: number }> = [];
    let releaseSwitch: ((response: Response) => void) | undefined;
    const switchResponse = new Promise<Response>((resolve) => {
      releaseSwitch = resolve;
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        const body = JSON.parse(String(init?.body)) as { file_id: number };
        startBodies.push(body);
        if (startBodies.length === 2) return switchResponse;
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              effective_media_file_id: 7,
              effective_virtual_uri: "virtual://movie/x?result=A",
              audio_tracks: outgoingAudio,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() => result.current.switchVersion(99, 0));
    await waitFor(() => expect(startBodies).toHaveLength(2));

    act(() =>
      result.current.applyCommittedSource(
        {
          effectiveMediaFileId: 7,
          effectiveVirtualUri: "virtual://movie/x?result=B",
          inventoryStatus: "verified",
        },
        rotatedAudio,
      ),
    );
    expect(result.current.effectiveVirtualUri).toBe("virtual://movie/x?result=A");

    await act(async () => {
      releaseSwitch?.(
        jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-2",
          playback_plan: fixturePlanV3({
            session_id: "session-2",
            plan_id: "plan:retain-b000001",
            plan_attempt_key: "v3:retain-b000001",
            requested_media_file_id: 99,
            effective_media_file_id: 7,
            effective_virtual_uri: "virtual://movie/x?result=B",
            audio_tracks: [],
          }),
        }),
      );
      await switchResponse;
    });

    await waitFor(() =>
      expect(result.current.effectiveVirtualUri).toBe("virtual://movie/x?result=B"),
    );
    // The winner names candidate B outright, so its corroborating inventory is
    // folded rather than vetoed by the outgoing candidate A.
    expect(result.current.planAudioTracks).toEqual(rotatedAudio);
    unmount();
  });

  it("keeps the newer same-file source commit over an earlier deferred source commit", async () => {
    // The transport commits to candidate C, then to candidate B, on the same
    // file while a replan is in flight. Both are source commits. The settled
    // winner is URI-less, so arrival order decides: the queue's own earlier
    // commit is not an external authority the newer one must answer to.
    let releaseReplan: ((response: Response) => void) | undefined;
    const heldReplan = new Promise<Response>((resolve) => {
      releaseReplan = resolve;
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              effective_media_file_id: 7,
              effective_virtual_uri: "virtual://movie/x?result=A",
              audio_tracks: outgoingAudio,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/session-1/replan")) return heldReplan;
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() => {
      void result.current.refreshSubtitles(120);
    });
    await waitFor(() => expect(result.current.replanning).toBe(true));

    act(() =>
      result.current.applyCommittedSource(
        {
          effectiveMediaFileId: 8,
          effectiveVirtualUri: "virtual://movie/x?result=C",
          inventoryStatus: "verified",
        },
        collisionAudio,
      ),
    );
    act(() =>
      result.current.applyCommittedSource(
        {
          effectiveMediaFileId: 8,
          effectiveVirtualUri: "virtual://movie/x?result=B",
          inventoryStatus: "verified",
        },
        rotatedAudio,
      ),
    );

    await act(async () => {
      releaseReplan?.(
        jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-1",
          playback_plan: fixturePlanV3({
            plan_id: "plan:source-order01",
            plan_attempt_key: "v3:source-order01",
            effective_media_file_id: 8,
            audio_tracks: [],
          }),
        }),
      );
      await heldReplan;
    });

    await waitFor(() => expect(result.current.mediaFileId).toBe(8));
    // The newer commit (B) must remain the final identity and inventory; the
    // earlier same-file commit (C) is not an authority that can veto it.
    expect(result.current.effectiveVirtualUri).toBe("virtual://movie/x?result=B");
    expect(result.current.planAudioTracks).toEqual(rotatedAudio);
    unmount();
  });

  it("does not fold a URI-only inventory for the outgoing file onto a replaced file", async () => {
    // The plan owns file 7 with no candidate URI. A URI-only inventory push for
    // its candidate is queued during a switch, and the replacement settles on a
    // different file 8, also URI-less. The push proves no relation to file 8, so
    // it must not inherit the replacement's file id.
    const startBodies: Array<{ file_id: number }> = [];
    let releaseSwitch: ((response: Response) => void) | undefined;
    const switchResponse = new Promise<Response>((resolve) => {
      releaseSwitch = resolve;
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/playback/start")) {
        const body = JSON.parse(String(init?.body)) as { file_id: number };
        startBodies.push(body);
        if (startBodies.length === 2) return switchResponse;
        return jsonResponse(
          {
            protocol_version: 3,
            server_features: ["playback_plan_v3"],
            outcome: "playable",
            session_id: "session-1",
            playback_plan: fixturePlanV3({
              effective_media_file_id: 7,
              audio_tracks: outgoingAudio,
            }),
          },
          { status: 201 },
        );
      }
      if (url.endsWith("/playback/route-events")) return new Response(null, { status: 202 });
      if (init?.method === "DELETE") return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result, unmount } = renderHook(
      () => usePlaybackSession("request-1", [], [], 7, 0, false, "auto"),
      { wrapper },
    );
    await waitFor(() => expect(result.current.plan).not.toBeNull());

    act(() => result.current.switchVersion(8, 0));
    await waitFor(() => expect(startBodies).toHaveLength(2));

    act(() =>
      result.current.applyInventoryUpdate({
        session_id: "session-1",
        inventory_revision: "inv:uri-only-outgoing",
        inventory_status: "verified",
        effective_virtual_uri: "virtual://movie/x?result=C",
        audio_tracks: rotatedAudio,
      }),
    );

    await act(async () => {
      releaseSwitch?.(
        jsonResponse({
          protocol_version: 3,
          server_features: ["playback_plan_v3"],
          outcome: "playable",
          session_id: "session-2",
          playback_plan: fixturePlanV3({
            session_id: "session-2",
            plan_id: "plan:cross-file-01",
            plan_attempt_key: "v3:cross-file-01",
            requested_media_file_id: 8,
            effective_media_file_id: 8,
            audio_tracks: [],
          }),
        }),
      );
      await switchResponse;
    });

    await waitFor(() => expect(result.current.sessionId).toBe("session-2"));
    // No proven relation to the settled file, so the push is refused and a
    // current-source refresh must re-establish the candidate instead.
    expect(result.current.effectiveVirtualUri).toBeNull();
    expect(result.current.planAudioTracks).toEqual([]);
    unmount();
  });
});
