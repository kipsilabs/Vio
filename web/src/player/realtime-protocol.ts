import type { AudioTrackV3, SubtitleInventoryItemV3 } from "./protocol-v3";
import type { PlayerMarkerSegment } from "./types";

export type PlaybackRealtimeMessageType = "command" | "event" | "hello" | "ack" | "result";

export type PlaybackCommandName =
  | "pause"
  | "unpause"
  | "play_pause"
  | "seek"
  | "set_volume"
  | "stop"
  | "terminate"
  | "display_message"
  | "server_restarting"
  | "server_shutting_down"
  | "play_media"
  | "set_audio_track"
  | "set_subtitle_track"
  | "plan_invalidated";

export type PlaybackRealtimeAckStatus = "accepted";
export type PlaybackRealtimeResultStatus = "completed" | "rejected";
export type PlaybackRealtimeEventName =
  | "chapter_thumbnail_ready"
  | "markers_updated"
  | "subtitle_ready"
  | "subtitle_timing_changed"
  | "subtitle_translation_started"
  | "subtitle_translation_cues"
  | "subtitle_translation_completed"
  | "subtitle_translation_failed"
  | "inventory_updated";

export interface PlaybackRealtimeCommandEnvelope {
  type: "command";
  command_id: string;
  session_id: string;
  name: PlaybackCommandName;
  reason?: string;
  issued_by?: {
    kind: string;
  };
  deadline_ms?: number;
  payload?: Record<string, unknown>;
}

/**
 * Payload of the `plan_invalidated` command: the server decided, after the plan
 * was already playing, that the route it names cannot serve this source.
 *
 * `plan_id` is the invalidated plan, not necessarily the one on screen — a
 * client that has already replanned past it has nothing left to do.
 */
export interface PlaybackPlanInvalidatedPayload {
  reason: string;
  plan_id: string;
}

export interface PlaybackRealtimeHelloEnvelope {
  type: "hello";
  session_id: string;
  client: {
    name: string;
    version: string;
  };
  capabilities: {
    commands: PlaybackCommandName[];
  };
}

export interface PlaybackChapterThumbnailReadyPayload {
  session_id: string;
  file_id: number;
  chapter_index: number;
  thumbnail_url: string;
  thumbnail_thumbhash?: string;
}

export interface PlaybackTimeRangePayload {
  start: number;
  end: number;
}

export interface PlaybackMarkersUpdatedPayload {
  session_id: string;
  file_id: number;
  intro?: PlaybackTimeRangePayload | null;
  credits?: PlaybackTimeRangePayload | null;
  recap?: PlaybackTimeRangePayload | null;
  preview?: PlaybackTimeRangePayload | null;
  marker_segments?: PlayerMarkerSegment[];
}

/**
 * Broadcast to every session watching a file when a newly generated subtitle
 * track (AI translation, later ASR) has been persisted, so players can refresh
 * their track list and pick it up without a manual reload.
 */
export interface PlaybackSubtitleReadyPayload {
  session_id: string;
  file_id: number;
  subtitle_id: number;
  language: string;
  label?: string;
  /**
   * The new track's server-assigned combined ordinal, identity and stream URL.
   * The client folds it in at that ordinal rather than deriving one. Absent
   * when the server could not resolve the file's inventory, in which case the
   * client refetches its plan instead.
   */
  track?: SubtitleInventoryItemV3;
}

/**
 * Sent to every session of a file after a stored subtitle is retimed (an
 * automatic sync was applied or its timing was reset). The track's stream URL
 * already serves the new timing, so a player showing it fetches the cues again.
 */
export interface PlaybackSubtitleTimingChangedPayload {
  session_id: string;
  file_id: number;
  subtitle_id: number;
  /** See {@link PlaybackSubtitleReadyPayload.track}. */
  track?: SubtitleInventoryItemV3;
}

/** One translated subtitle cue pushed during a live translation (media seconds). */
export interface PlaybackStreamCue {
  start: number;
  end: number;
  text: string;
}

export interface PlaybackSubtitleTranslationStartedPayload {
  session_id: string;
  file_id: number;
  job_id: number;
  track_key: string;
  language: string;
  label?: string;
  total_cues: number;
}

export interface PlaybackSubtitleTranslationCuesPayload {
  session_id: string;
  file_id: number;
  job_id: number;
  track_key: string;
  cues: PlaybackStreamCue[];
  done: number;
  total: number;
}

export interface PlaybackSubtitleTranslationCompletedPayload {
  session_id: string;
  file_id: number;
  job_id: number;
  track_key: string;
  subtitle_id: number;
  language: string;
  label?: string;
  /** See {@link PlaybackSubtitleReadyPayload.track}. */
  track?: SubtitleInventoryItemV3;
}

export interface PlaybackSubtitleTranslationFailedPayload {
  session_id: string;
  file_id: number;
  job_id: number;
  track_key: string;
  message?: string;
}

/**
 * The effective version a transport committed to (fresh start or rotation) and
 * its declared audio inventory, pushed before the background probe lands.
 *
 * `effective_virtual_uri` is the identity the version menu keys on, so a client
 * re-keys to the streamed release immediately rather than waiting for a replan.
 * `audio_tracks` is declared metadata; the inventory poll upgrades it.
 */
export interface PlaybackSourceCommittedPayload {
  session_id: string;
  effective_media_file_id?: number;
  effective_virtual_uri?: string;
  virtual_source_revision?: string;
  inventory_status?: string;
  audio_tracks?: AudioTrackV3[];
}

/**
 * A live inventory revision for the session's effective source: the probed (or
 * repaired) audio and subtitle lists, plus the identity they belong to.
 *
 * This mirrors the `PlaybackInventoryV3` body of
 * `GET /api/v2/playback/{session_id}/inventory`, pushed over the realtime
 * socket so a player replaces its declared menus without a poll or a replan.
 * `inventory_status` says whether the list is "declared" (provider metadata) or
 * "verified" (probed bytes); `inventory_revision` is the opaque revision the
 * client would echo as an ETag when reading the endpoint directly. Every field
 * beyond `session_id` is optional so an older or partial server still yields a
 * usable event.
 */
export interface PlaybackInventoryUpdatedPayload {
  session_id: string;
  inventory_revision?: string;
  inventory_status?: string;
  /** See {@link PlaybackSourceCommittedPayload.audio_tracks}. */
  audio_tracks?: AudioTrackV3[];
  /** The complete, gap-free combined-ordinal subtitle list for the source. */
  subtitle_inventory?: SubtitleInventoryItemV3[];
  /** The catalog row the transport is committed to. See {@link PlaybackSourceCommittedPayload}. */
  effective_media_file_id?: number;
  /** The provider-neutral candidate URI the session is bound to. */
  effective_virtual_uri?: string;
  virtual_source_revision?: string;
}

export interface PlaybackRealtimeEventEnvelopeBase {
  type: "event";
  session_id: string;
}

export type PlaybackRealtimeEventEnvelope =
  | (PlaybackRealtimeEventEnvelopeBase & {
      name: "chapter_thumbnail_ready";
      payload: PlaybackChapterThumbnailReadyPayload;
    })
  | (PlaybackRealtimeEventEnvelopeBase & {
      name: "markers_updated";
      payload: PlaybackMarkersUpdatedPayload;
    })
  | (PlaybackRealtimeEventEnvelopeBase & {
      name: "subtitle_ready";
      payload: PlaybackSubtitleReadyPayload;
    })
  | (PlaybackRealtimeEventEnvelopeBase & {
      name: "subtitle_timing_changed";
      payload: PlaybackSubtitleTimingChangedPayload;
    })
  | (PlaybackRealtimeEventEnvelopeBase & {
      name: "subtitle_translation_started";
      payload: PlaybackSubtitleTranslationStartedPayload;
    })
  | (PlaybackRealtimeEventEnvelopeBase & {
      name: "subtitle_translation_cues";
      payload: PlaybackSubtitleTranslationCuesPayload;
    })
  | (PlaybackRealtimeEventEnvelopeBase & {
      name: "subtitle_translation_completed";
      payload: PlaybackSubtitleTranslationCompletedPayload;
    })
  | (PlaybackRealtimeEventEnvelopeBase & {
      name: "subtitle_translation_failed";
      payload: PlaybackSubtitleTranslationFailedPayload;
    })
  | (PlaybackRealtimeEventEnvelopeBase & {
      name: "source_committed";
      payload: PlaybackSourceCommittedPayload;
    })
  | (PlaybackRealtimeEventEnvelopeBase & {
      name: "inventory_updated";
      payload: PlaybackInventoryUpdatedPayload;
    });

export interface PlaybackRealtimeAckEnvelope {
  type: "ack";
  command_id: string;
  session_id: string;
  status: PlaybackRealtimeAckStatus;
}

export interface PlaybackRealtimeResultEnvelope {
  type: "result";
  command_id: string;
  session_id: string;
  status: PlaybackRealtimeResultStatus;
  error?: string;
}

export const ALL_PLAYBACK_COMMANDS: PlaybackCommandName[] = [
  "pause",
  "unpause",
  "play_pause",
  "seek",
  "set_volume",
  "stop",
  "terminate",
  "display_message",
  "server_restarting",
  "server_shutting_down",
  "play_media",
  "set_audio_track",
  "set_subtitle_track",
  "plan_invalidated",
];

/** The commands every realtime surface in this app executes. */
export const SUPPORTED_PLAYBACK_COMMANDS: PlaybackCommandName[] = [
  "pause",
  "unpause",
  "play_pause",
  "seek",
  "set_volume",
  "stop",
  "terminate",
  "display_message",
  "server_restarting",
  "server_shutting_down",
];

/**
 * What the video player executes on top of the shared set. `plan_invalidated`
 * needs a replan the audiobook surface has no route ladder for, so the hello is
 * per-surface rather than one list both over-claim.
 */
export const VIDEO_PLAYBACK_COMMANDS: PlaybackCommandName[] = [
  ...SUPPORTED_PLAYBACK_COMMANDS,
  "plan_invalidated",
];

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

function isCommandName(value: unknown): value is PlaybackCommandName {
  return typeof value === "string" && ALL_PLAYBACK_COMMANDS.includes(value as PlaybackCommandName);
}

/**
 * Reads a `plan_invalidated` payload, or null when it is not well formed.
 *
 * Both fields are required: without `plan_id` the client cannot tell whether
 * the invalidated plan is still the one playing, and acting anyway would evict
 * a route the server never complained about.
 */
export function readPlanInvalidatedPayload(
  payload: Record<string, unknown> | undefined,
): PlaybackPlanInvalidatedPayload | null {
  if (!isRecord(payload)) return null;
  const { reason, plan_id: planId } = payload;
  if (typeof reason !== "string" || reason.trim() === "") return null;
  if (typeof planId !== "string" || planId.trim() === "") return null;
  return { reason, plan_id: planId };
}

function isChapterThumbnailReadyPayload(
  value: unknown,
): value is PlaybackChapterThumbnailReadyPayload {
  return (
    isRecord(value) &&
    typeof value.session_id === "string" &&
    typeof value.file_id === "number" &&
    typeof value.chapter_index === "number" &&
    typeof value.thumbnail_url === "string" &&
    (value.thumbnail_thumbhash === undefined || typeof value.thumbnail_thumbhash === "string")
  );
}

function isTimeRangePayload(value: unknown): value is PlaybackTimeRangePayload {
  return isRecord(value) && typeof value.start === "number" && typeof value.end === "number";
}

function isMarkerSegment(value: unknown): value is PlayerMarkerSegment {
  return (
    isRecord(value) &&
    typeof value.kind === "string" &&
    ["intro", "credits", "recap", "preview"].includes(value.kind) &&
    typeof value.start_seconds === "number" &&
    Number.isFinite(value.start_seconds) &&
    value.start_seconds >= 0 &&
    typeof value.end_seconds === "number" &&
    Number.isFinite(value.end_seconds) &&
    value.end_seconds > value.start_seconds
  );
}

function isMarkersUpdatedPayload(value: unknown): value is PlaybackMarkersUpdatedPayload {
  const isOptionalRange = (range: unknown) =>
    range === undefined || range === null || isTimeRangePayload(range);
  return (
    isRecord(value) &&
    typeof value.session_id === "string" &&
    typeof value.file_id === "number" &&
    isOptionalRange(value.intro) &&
    isOptionalRange(value.credits) &&
    isOptionalRange(value.recap) &&
    isOptionalRange(value.preview) &&
    (value.marker_segments === undefined ||
      (Array.isArray(value.marker_segments) && value.marker_segments.every(isMarkerSegment)))
  );
}

/** An optional string field is valid only when absent or actually a string. */
function isOptionalString(value: unknown): boolean {
  return value === undefined || typeof value === "string";
}

/**
 * Validates a pushed inventory entry.
 *
 * The combined ordinal is the whole point of the block — it is what lets the
 * client select the track without counting — so an entry missing it is worse
 * than no entry at all, and is rejected in favour of refetching the plan.
 */
function isSubtitleInventoryItem(value: unknown): value is SubtitleInventoryItemV3 {
  return (
    isRecord(value) &&
    typeof value.track_id === "string" &&
    typeof value.combined_index === "number" &&
    Number.isInteger(value.combined_index) &&
    value.combined_index >= 0 &&
    typeof value.source === "string" &&
    typeof value.forced === "boolean" &&
    typeof value.default === "boolean" &&
    typeof value.hearing_impaired === "boolean" &&
    (value.delivery === "sidecar" || value.delivery === "burn_in_only") &&
    isOptionalString(value.codec) &&
    isOptionalString(value.language) &&
    isOptionalString(value.label) &&
    isOptionalString(value.url) &&
    isOptionalString(value.font_bundle_url)
  );
}

function isOptionalSubtitleInventoryItem(value: unknown): boolean {
  return value === undefined || value === null || isSubtitleInventoryItem(value);
}

function isSubtitleReadyPayload(value: unknown): value is PlaybackSubtitleReadyPayload {
  return (
    isRecord(value) &&
    typeof value.session_id === "string" &&
    typeof value.file_id === "number" &&
    typeof value.subtitle_id === "number" &&
    typeof value.language === "string" &&
    isOptionalString(value.label) &&
    isOptionalSubtitleInventoryItem(value.track)
  );
}

function isSubtitleTimingChangedPayload(
  value: unknown,
): value is PlaybackSubtitleTimingChangedPayload {
  return (
    isRecord(value) &&
    typeof value.session_id === "string" &&
    typeof value.file_id === "number" &&
    typeof value.subtitle_id === "number" &&
    isOptionalSubtitleInventoryItem(value.track)
  );
}

function isStreamCue(value: unknown): value is PlaybackStreamCue {
  return (
    isRecord(value) &&
    typeof value.start === "number" &&
    typeof value.end === "number" &&
    typeof value.text === "string"
  );
}

function isTranslationStartedPayload(
  value: unknown,
): value is PlaybackSubtitleTranslationStartedPayload {
  return (
    isRecord(value) &&
    typeof value.session_id === "string" &&
    typeof value.file_id === "number" &&
    typeof value.job_id === "number" &&
    typeof value.track_key === "string" &&
    typeof value.language === "string" &&
    typeof value.total_cues === "number" &&
    isOptionalString(value.label)
  );
}

function isTranslationCuesPayload(value: unknown): value is PlaybackSubtitleTranslationCuesPayload {
  return (
    isRecord(value) &&
    typeof value.session_id === "string" &&
    typeof value.file_id === "number" &&
    typeof value.job_id === "number" &&
    typeof value.track_key === "string" &&
    Array.isArray(value.cues) &&
    value.cues.every(isStreamCue) &&
    typeof value.done === "number" &&
    typeof value.total === "number"
  );
}

function isTranslationCompletedPayload(
  value: unknown,
): value is PlaybackSubtitleTranslationCompletedPayload {
  return (
    isRecord(value) &&
    typeof value.session_id === "string" &&
    typeof value.file_id === "number" &&
    typeof value.job_id === "number" &&
    typeof value.track_key === "string" &&
    typeof value.subtitle_id === "number" &&
    typeof value.language === "string" &&
    isOptionalString(value.label) &&
    isOptionalSubtitleInventoryItem(value.track)
  );
}

function isTranslationFailedPayload(
  value: unknown,
): value is PlaybackSubtitleTranslationFailedPayload {
  return (
    isRecord(value) &&
    typeof value.session_id === "string" &&
    typeof value.file_id === "number" &&
    typeof value.job_id === "number" &&
    typeof value.track_key === "string" &&
    isOptionalString(value.message)
  );
}

function isOptionalNumber(value: unknown): value is number | undefined {
  return value === undefined || typeof value === "number";
}

function isSourceCommittedPayload(value: unknown): value is PlaybackSourceCommittedPayload {
  return (
    isRecord(value) &&
    typeof value.session_id === "string" &&
    isOptionalNumber(value.effective_media_file_id) &&
    isOptionalString(value.effective_virtual_uri) &&
    isOptionalString(value.virtual_source_revision) &&
    isOptionalString(value.inventory_status) &&
    (value.audio_tracks === undefined || Array.isArray(value.audio_tracks))
  );
}

/**
 * Validates a live inventory push.
 *
 * Only `session_id` is required: the inventory lists are validated when
 * present, and a malformed list is dropped at parse time so the client falls
 * back to the inventory poll rather than rendering a partial menu.
 */
function isInventoryUpdatedPayload(value: unknown): value is PlaybackInventoryUpdatedPayload {
  return (
    isRecord(value) &&
    typeof value.session_id === "string" &&
    isOptionalString(value.inventory_revision) &&
    isOptionalString(value.inventory_status) &&
    isOptionalString(value.effective_virtual_uri) &&
    isOptionalString(value.virtual_source_revision) &&
    isOptionalNumber(value.effective_media_file_id) &&
    (value.audio_tracks === undefined || Array.isArray(value.audio_tracks)) &&
    (value.subtitle_inventory === undefined ||
      (Array.isArray(value.subtitle_inventory) &&
        value.subtitle_inventory.every(isSubtitleInventoryItem)))
  );
}

export function parsePlaybackRealtimeMessage(
  data: string,
): PlaybackRealtimeCommandEnvelope | PlaybackRealtimeEventEnvelope | null {
  try {
    const value = JSON.parse(data) as unknown;
    if (!isRecord(value) || typeof value.type !== "string") {
      return null;
    }
    if (value.type === "command") {
      if (
        typeof value.command_id !== "string" ||
        typeof value.session_id !== "string" ||
        !isCommandName(value.name)
      ) {
        return null;
      }
      return {
        type: "command",
        command_id: value.command_id,
        session_id: value.session_id,
        name: value.name,
        reason: typeof value.reason === "string" ? value.reason : undefined,
        issued_by:
          isRecord(value.issued_by) && typeof value.issued_by.kind === "string"
            ? { kind: value.issued_by.kind }
            : undefined,
        deadline_ms: typeof value.deadline_ms === "number" ? value.deadline_ms : undefined,
        payload: isRecord(value.payload) ? value.payload : {},
      };
    }
    if (value.type === "event" && typeof value.session_id === "string") {
      if (
        value.name === "chapter_thumbnail_ready" &&
        isChapterThumbnailReadyPayload(value.payload)
      ) {
        return {
          type: "event",
          session_id: value.session_id,
          name: value.name,
          payload: value.payload,
        };
      }
      if (value.name === "subtitle_ready" && isSubtitleReadyPayload(value.payload)) {
        return {
          type: "event",
          session_id: value.session_id,
          name: value.name,
          payload: value.payload,
        };
      }
      if (
        value.name === "subtitle_timing_changed" &&
        isSubtitleTimingChangedPayload(value.payload)
      ) {
        return {
          type: "event",
          session_id: value.session_id,
          name: value.name,
          payload: value.payload,
        };
      }
      if (value.name === "markers_updated" && isMarkersUpdatedPayload(value.payload)) {
        return {
          type: "event",
          session_id: value.session_id,
          name: value.name,
          payload: value.payload,
        };
      }
      if (
        value.name === "subtitle_translation_started" &&
        isTranslationStartedPayload(value.payload)
      ) {
        return {
          type: "event",
          session_id: value.session_id,
          name: value.name,
          payload: value.payload,
        };
      }
      if (value.name === "subtitle_translation_cues" && isTranslationCuesPayload(value.payload)) {
        return {
          type: "event",
          session_id: value.session_id,
          name: value.name,
          payload: value.payload,
        };
      }
      if (
        value.name === "subtitle_translation_completed" &&
        isTranslationCompletedPayload(value.payload)
      ) {
        return {
          type: "event",
          session_id: value.session_id,
          name: value.name,
          payload: value.payload,
        };
      }
      if (
        value.name === "subtitle_translation_failed" &&
        isTranslationFailedPayload(value.payload)
      ) {
        return {
          type: "event",
          session_id: value.session_id,
          name: value.name,
          payload: value.payload,
        };
      }
      if (value.name === "source_committed" && isSourceCommittedPayload(value.payload)) {
        return {
          type: "event",
          session_id: value.session_id,
          name: value.name,
          payload: value.payload,
        };
      }
      if (value.name === "inventory_updated" && isInventoryUpdatedPayload(value.payload)) {
        return {
          type: "event",
          session_id: value.session_id,
          name: value.name,
          payload: value.payload,
        };
      }
    }
    return null;
  } catch {
    return null;
  }
}

export function parsePlaybackRealtimeCommand(data: string): PlaybackRealtimeCommandEnvelope | null {
  const message = parsePlaybackRealtimeMessage(data);
  return message?.type === "command" ? message : null;
}

export function buildPlaybackRealtimeHello(
  sessionId: string,
  commands: PlaybackCommandName[] = SUPPORTED_PLAYBACK_COMMANDS,
): PlaybackRealtimeHelloEnvelope {
  return {
    type: "hello",
    session_id: sessionId,
    client: {
      name: "silo-web",
      version: "1",
    },
    capabilities: {
      commands: [...commands],
    },
  };
}

export function buildPlaybackRealtimeAck(
  sessionId: string,
  commandId: string,
): PlaybackRealtimeAckEnvelope {
  return {
    type: "ack",
    command_id: commandId,
    session_id: sessionId,
    status: "accepted",
  };
}

export function buildPlaybackRealtimeResult(
  sessionId: string,
  commandId: string,
  status: PlaybackRealtimeResultStatus,
  error?: string,
): PlaybackRealtimeResultEnvelope {
  return {
    type: "result",
    command_id: commandId,
    session_id: sessionId,
    status,
    error,
  };
}
