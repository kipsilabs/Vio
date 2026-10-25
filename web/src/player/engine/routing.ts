import type { FailureV3, PlanV3 } from "../protocol-v3";
import type { PlayerAudioTrack } from "../types";
import type { EngineCapabilities } from "./capabilities";
import type { EngineSourceSpec, EngineTransfer } from "./source";

/** What the browser's own media element accepts for an untouched original. */
export interface NativeOriginalSupport {
  containers: string[];
  videoCodecs: string[];
  audioCodecs: string[];
}

export type OriginalPlaybackRoute =
  | { kind: "native" }
  | { kind: "engine"; source: EngineSourceSpec }
  | { kind: "unsupported"; failure: FailureV3 };

/** Typed failure the server's replan ladder records against this plan. */
export const ENGINE_UNSUPPORTED_SOURCE = "browser_engine_unsupported_source";

function lower(value: string | undefined | null): string {
  return (value ?? "").trim().toLowerCase();
}

/** The audio ordinal a media element plays when it is not told otherwise. */
export function containerDefaultAudioOrdinal(audioTracks: readonly PlayerAudioTrack[]): number {
  const index = audioTracks.findIndex((track) => track.default === true);
  return index >= 0 ? index : 0;
}

/**
 * The Dolby Vision base-layer compatibility ids the engine presents.
 * Profile 8.1 and Profile 7 carry HDR10 (ids 1 and 6), 8.4 carries HLG (4) and
 * 8.2 carries SDR (2). Profile 5 has no compatible base layer. Capability
 * claims derive from this map, so the list the server sees cannot drift from
 * the ids routing accepts.
 */
const DV_BASE_LAYER_TRANSFERS: ReadonlyMap<number, EngineTransfer> = new Map([
  [1, "pq"],
  [6, "pq"],
  [4, "hlg"],
  [2, "sdr"],
]);

/** The ids in {@link DV_BASE_LAYER_TRANSFERS}; safe to publish as claims. */
export const ENGINE_DV_BL_COMPAT_IDS: readonly number[] = Array.from(
  DV_BASE_LAYER_TRANSFERS.keys(),
);

/**
 * Maps a plan's dynamic range onto the transfer the engine presents, or null
 * when no route can present it. Dolby Vision is played through its
 * standards-compatible base layer: Profile 8.1 and Profile 7 carry HDR10, 8.4
 * carries HLG and 8.2 carries SDR. Profile 5 has no compatible base layer.
 */
export function engineTransferForPlan(plan: PlanV3): EngineTransfer | null {
  const range = lower(plan.effective_recipe.dynamic_range || plan.source.dynamic_range);
  switch (range) {
    case "":
    case "sdr":
      return "sdr";
    case "hdr10":
    case "hdr10_plus":
    case "hdr_unknown":
      return "pq";
    case "hlg":
      return "hlg";
    case "dolby_vision":
      return DV_BASE_LAYER_TRANSFERS.get(plan.source.dv_bl_compat_id ?? -1) ?? null;
    default:
      return null;
  }
}

function unsupported(message: string): OriginalPlaybackRoute {
  return { kind: "unsupported", failure: { classification: ENGINE_UNSUPPORTED_SOURCE, message } };
}

/**
 * Chooses who plays an `original_http` plan: the media element when it can
 * take the file as it is, otherwise the decode engine, otherwise nobody — in
 * which case the typed failure lets the server replan onto a packaged route.
 *
 * The media element wins whenever it can, because it decodes in hardware with
 * the browser's own A/V sync. It cannot pick an audio track, present an HDR
 * source on a page it does not control, or decode the formats the engine
 * exists for.
 */
export function resolveOriginalPlaybackRoute(input: {
  plan: PlanV3;
  streamUrl: string;
  native: NativeOriginalSupport;
  engine: EngineCapabilities | null;
  audioTracks: readonly PlayerAudioTrack[];
}): OriginalPlaybackRoute {
  const { plan, streamUrl, native, engine, audioTracks } = input;
  const container = lower(plan.source.container || plan.stream.container);
  const videoCodec = lower(plan.effective_recipe.video_codec || plan.source.video_codec);
  const audioCodec = lower(plan.effective_recipe.audio_codec || plan.source.audio_codec);
  const defaultOrdinal = containerDefaultAudioOrdinal(audioTracks);
  const audioOrdinal = plan.selected_tracks.audio?.index ?? defaultOrdinal;
  const transfer = engineTransferForPlan(plan);

  // An HDR original the plan validated for this display plays natively, in
  // HDR; the engine would tone-map it down to SDR.
  const claims = plan.claims.video;
  const nativeRange =
    transfer === "sdr" ||
    (transfer === "hlg" && claims.hlg) ||
    (transfer === "pq" && (claims.hdr10 || claims.hdr10_plus || claims.dolby_vision));
  const nativePlays =
    native.containers.includes(container) &&
    native.videoCodecs.includes(videoCodec) &&
    nativeRange &&
    (audioCodec === "" || native.audioCodecs.includes(audioCodec)) &&
    audioOrdinal === defaultOrdinal;
  if (nativePlays) return { kind: "native" };

  if (!engine) return unsupported("The browser cannot play this file as it is.");
  if (!engine.containers.includes(container)) {
    return unsupported(`The browser engine cannot read ${container || "this"} containers.`);
  }
  if (!engine.videoCodecs.includes(videoCodec)) {
    return unsupported(`This browser cannot decode ${videoCodec || "this"} video.`);
  }
  if (audioCodec !== "" && !engine.audioCodecs.includes(audioCodec)) {
    return unsupported(`The browser engine cannot decode ${audioCodec} audio.`);
  }
  if (transfer === null) {
    return unsupported("The browser engine cannot present this dynamic range.");
  }
  if (transfer !== "sdr" && !engine.hdr) {
    return unsupported("HDR needs a working WebGPU presenter, which this browser does not have.");
  }

  return {
    kind: "engine",
    source: {
      url: streamUrl,
      headers: plan.stream.headers ?? {},
      container,
      durationSeconds: plan.source.duration_seconds ?? null,
      startSeconds: Math.max(0, plan.timeline.player_start_seconds),
      video: {
        codec: videoCodec,
        width: plan.source.width ?? 0,
        height: plan.source.height ?? 0,
        transfer,
        stripDolbyVision: lower(plan.source.dynamic_range) === "dolby_vision",
      },
      audio:
        audioCodec === ""
          ? null
          : {
              ordinal: audioOrdinal,
              codec: audioCodec,
              channels: plan.effective_recipe.audio_channels ?? plan.source.audio_channels ?? null,
            },
    },
  };
}
