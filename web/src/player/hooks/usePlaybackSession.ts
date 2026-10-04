import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { usePlayerConfig } from "../context/PlayerConfigContext";
import type { PlayerConfig } from "../context/PlayerConfigContext";
import { startPlaybackV2 } from "../start-v2";
import { hasSequencedProgress, stopSequencedSession } from "../session-mutations";
import {
  describePlanTerminal,
  describePlaybackTransportError,
  isDeadPlaybackSessionError,
  type PlaybackPolicyErrorDescription,
} from "../playback-errors";
import { useCodecDetection } from "./useCodecDetection";
import {
  buildClientCapabilitiesV3,
  buildClientPlaybackContextV3,
  detectBandwidthEstimateKbpsV3,
  detectMeteredV3,
} from "../client-context-v3";
import { buildRouteEventV3 } from "../route-events-v3";
import { reportSessionRouteEventV2 } from "../route-events-v2";
import { replanV2 } from "../lifecycle-v2";
import { takePlaybackIntent } from "../first-frame";
import { buildPlayerStreamUrl } from "../stream-url";
import { randomUUID } from "@/lib/uuid";
import { matchSubtitleTrackAcrossVersions } from "../utils/subtitleSort";
import { resolvePlanAudioIdentity } from "../utils/audioTrackMatch";
import { buildPublishedSubtitleTracks } from "../utils/subtitleInventory";
import { isBitmapCodec } from "../utils/subtitleCodecs";
import { isInventoryProvisional } from "../utils/inventoryProvenance";
import {
  FEATURE_OUTPUT_CHANGE_V3,
  MAX_ATTEMPT_COUNT_V3,
  MAX_ATTEMPTED_PLAN_KEYS_V3,
  QUALITY_ORIGINAL_V3,
  type DecisionResponseV3,
  type FailureV3,
  type PlanV3,
  type RouteEventNameV3,
  type StartRequestV3,
  type SubtitleInventoryItemV3,
} from "../protocol-v3";
import {
  buildReplanRequestV3,
  buildStartRequestV3,
  routeEventPlanIdentityV3,
  VIDEO_CLIENT_FEATURES_V3,
  type ReplanOptions,
} from "../playback-session-wire-v3";
import type { PlaybackInventoryUpdatedPayload } from "../realtime-protocol";
import type {
  PlayerAudioTrack,
  PlayerFileVersion,
  PlayerPlaybackVariant,
  PlayerSubtitleInfo,
  ResumeHints,
} from "../types";

interface PlaybackSessionState {
  /**
   * The server's plan, verbatim. It is the single source of truth for the
   * stream URL, the timeline, the selected tracks, the subtitle inventory and
   * the quality menu — the client derives none of those itself any more.
   */
  plan: PlanV3 | null;
  /**
   * Bumped every time a new plan is adopted. Consumers key stream-reload
   * effects on it rather than on object identity.
   */
  planRevision: number;
  transportRevision: number;
  streamUrl: string | null;
  sessionId: string | null;
  playbackAttemptId: string | null;
  mediaFileId: number | null;
  /**
   * The file path of the concrete candidate the server resolved a neutral
   * `virtual://…` requested row to, when the effective source is virtual. Null
   * for an ordinary file or an older plan that does not publish it. This is
   * menu data only: adopting it must never reload the stream, so it does not
   * touch `planRevision`/`transportRevision`.
   */
  effectiveVirtualUri: string | null;
  initialPosition: number;
  audioTrackIndex: number;
  durationSeconds: number | null;
  subtitleUrls: PlayerSubtitleInfo[];
  /**
   * The plan's authoritative audio inventory for the effective source, when
   * the server publishes one. Consumers render the audio menu from this in
   * preference to item metadata, which can be stale after a version fallback.
   */
  planAudioTracks: PlayerAudioTrack[];
  /**
   * True while the plan's audio inventory is still declared metadata (or a
   * deferred/failed probe) rather than probe evidence. The audio menu reads it
   * to say the list may not match the bytes yet. Cleared when the inventory
   * poll folds in probed catalog tracks or a verified replan lands.
   */
  audioInventoryProvisional: boolean;
  /**
   * True while the plan's subtitle inventory is still declared metadata rather
   * than probe evidence. Cleared when a verified replan publishes the real
   * inventory; the poll's subtitle fill is a replan, so it resolves here too.
   */
  subtitleInventoryProvisional: boolean;
  qualityPreference: string;
  shouldAutoPlay: boolean;
  loading: boolean;
  replacing: boolean;
  replanning: boolean;
  /**
   * True only while the in-flight replan is a quality/output change. The
   * quality menu's "…" label keys on this rather than on `replanning`, which
   * is also set for track changes, seek reanchors and failure recovery.
   */
  replanningQuality: boolean;
  /**
   * The file the viewer most recently asked to switch to, while the switch is
   * still in flight. Lets the version menu light the clicked entry as
   * "Requested" optimistically, before the replacement plan lands.
   */
  pendingSwitchFileId: number | null;
  /**
   * Whether automatic version fallback is armed for this session. On by
   * default for an auto start; the viewer can turn it off by picking an
   * explicit version and back on through the version menu's Auto entry. It
   * only authorizes a dead/unplayable source to move to another version, never
   * a healthy mid-play switch.
   */
  autoFallback: boolean;
  errorTitle: string | null;
  error: string | null;
  /**
   * The v3 terminal reason behind `errorTitle`/`error`, when the error came
   * from a server decision rather than a transport failure.
   */
  errorReason: string | null;
  /** The server's retry verdict for the current error, false when unnamed. */
  errorRetryable: boolean;
  /** True while a viewer-initiated retry of a refused start is in flight. */
  retrying: boolean;
  initialSubtitleErrorTitle: string | null;
  initialSubtitleError: string | null;
}

interface PlaybackSessionErrorState {
  title: string;
  message: string;
  reason?: string;
  retryable?: boolean;
}

/** The live effective identity of a source: its file and, when virtual, its candidate. */
interface SourceIdentity {
  fileId: number | null;
  uri: string | null;
}

/**
 * A realtime push held back while a start, switch, or replan owns the session.
 *
 * Arrival order is preserved so a flush never replays a newer source commit
 * before an older inventory revision. Each entry carries the adoption it
 * arrived under — the plan, the live outgoing identity, the generation, and the
 * session — so the flush decides against the adoption that actually won rather
 * than the rendered state, which still describes the outgoing source at a
 * settle path.
 */
interface DeferredPushBase {
  /** Local, monotonically increasing arrival order. */
  seq: number;
  /**
   * The source-identity transition clock at arrival. A later external transition
   * (a poll fold, or an adopted plan) raises the clock above this; a value still
   * at or below it is the state the entry already saw at capture and is already
   * represented by its `outgoing` baseline.
   */
  arrivalTransitionClock: number;
  /**
   * Whether the wire shape named any source at all. An identity-less push
   * cannot be tied to an adoption, so it may not cross a session change the
   * way an identity-carrying one is allowed to.
   */
  hasIdentity: boolean;
  identity: SourceIdentity;
  /** The plan in force at arrival, or null before any plan. */
  plan: PlanV3 | null;
  /** The live source identity in force at arrival (rotations included). */
  outgoing: SourceIdentity;
  /**
   * True when `outgoing` was itself produced by an earlier deferred source
   * commit rather than by the settled plan or an external poll. Such a baseline
   * is just the transport's previous move captured from the same queue, so it is
   * not authority a newer source commit must answer to.
   */
  outgoingFromDeferredSource: boolean;
  /** The adoption generation (`loadSequence`) that owned the session. */
  generation: number;
  /** The session id at arrival. */
  sessionId: string | null;
}

type DeferredPush =
  | (DeferredPushBase & {
      kind: "source";
      source: {
        effectiveMediaFileId?: number | null;
        effectiveVirtualUri?: string | null;
        inventoryStatus?: string | null;
      };
      audioTracks: PlayerAudioTrack[];
    })
  | (DeferredPushBase & { kind: "inventory"; payload: PlaybackInventoryUpdatedPayload });

type DeferredPushInput =
  | {
      kind: "source";
      identity: SourceIdentity;
      source: {
        effectiveMediaFileId?: number | null;
        effectiveVirtualUri?: string | null;
        inventoryStatus?: string | null;
      };
      audioTracks: PlayerAudioTrack[];
    }
  | {
      kind: "inventory";
      identity: SourceIdentity;
      payload: PlaybackInventoryUpdatedPayload;
    };

/** Whether an identity names a concrete source at all. */
function identityNamesSource(identity: SourceIdentity): boolean {
  return identity.uri != null || identity.fileId != null;
}

/**
 * Whether the settled plan explicitly names the push's exact identity.
 *
 * This is ownership evidence: when it holds, the winning plan itself vouches
 * for the source, so the push is retained even if it also names the candidate
 * the session was leaving — a replacement can retain the same effective
 * candidate, and the winner's own identity takes precedence.
 */
function identityMatchesPlan(identity: SourceIdentity, plan: PlanV3): boolean {
  if (identity.uri != null) {
    if (plan.effective_virtual_uri == null) return false;
    if (identity.uri !== plan.effective_virtual_uri) return false;
    if (identity.fileId != null && identity.fileId !== plan.effective_media_file_id) return false;
    return true;
  }
  // A file-only push cannot be tied to a candidate when the plan names one.
  return (
    plan.effective_virtual_uri == null &&
    identity.fileId != null &&
    identity.fileId === plan.effective_media_file_id
  );
}

/**
 * Whether the settled plan is a different adoption than the one the push
 * arrived under.
 *
 * A plan that was replaced is the winning adoption and its file identity is
 * authoritative; a plan that is byte-for-byte the arrival plan is the same
 * (possibly refused) adoption, so it knows no more about the live source than
 * the arrival plan already did.
 */
function planWasReplaced(arrivalPlan: PlanV3 | null, plan: PlanV3): boolean {
  if (arrivalPlan == null) return true;
  return (
    arrivalPlan.plan_id !== plan.plan_id || arrivalPlan.plan_attempt_key !== plan.plan_attempt_key
  );
}

/**
 * Whether an identity names the live outgoing source.
 *
 * A URI is the discriminator whenever both sides carry one: a different URI is
 * a different candidate even on the same file. When either side is URI-less,
 * the file is the only key, and a shared file is treated as the outgoing source
 * because two candidates can share a file and the plan cannot tell them apart.
 */
function identityNamesSameSource(a: SourceIdentity, b: SourceIdentity): boolean {
  if (a.uri != null || b.uri != null) return a.uri === b.uri;
  return a.fileId != null && a.fileId === b.fileId;
}

/**
 * Whether a held-back push may be folded under the settled plan.
 *
 * The plan is the adoption that won; the rendered state is not consulted.
 * Whether it was replaced at all is decided first, because a refused plan is
 * still the pre-rotation plan and its URI would otherwise veto a legitimate
 * rotation. Against a replaced plan that names a candidate URI, that URI is the
 * only candidate it vouches for, and an exact match is retained even when the
 * live outgoing source names a different candidate (a replacement can retain
 * the effective candidate) — so a replaced URI-ful winner is decided by exact
 * identity *before* any ambiguity guard, or an outgoing candidate the session
 * was leaving would veto the plan the server just selected.
 *
 * When the replaced plan is URI-less (the v2 wire omits the candidate) file
 * equality is still the only key the wire offers, so a candidate-bearing push
 * naming the plan's own file is admitted — the payload routinely carries the
 * candidate the plan omits, and refusing it would leave the track menus stale
 * for the whole session. A URI-only push (no file id) cannot be tied to the
 * settled file by itself; it is admitted only when the arrival-time live source
 * was already on the winner's file, which corroborates that the candidate
 * belongs to the adoption the flush kept. Otherwise the push may be inventory
 * for the file the session was leaving, and folding it would inherit an
 * unrelated replacement file.
 *
 * A same-file source the plan cannot resolve stays refused whenever a concrete
 * candidate is already current on that file: a file-only push under any
 * concrete candidate, and a concrete push whose URI disagrees with one. That is
 * checked against two baselines — `outgoing`, the live source captured when the
 * push arrived, and `applied`, the identity the menus carry when the flush runs
 * — because a poll bypasses the adoption barrier and can fold a concrete
 * candidate after the push was queued, including when the plan is later refused
 * rather than replaced.
 *
 * A URI-bearing source commit is the transport's own move, so arrival order
 * decides among the queue's own commits: when such a commit supersedes a
 * previous deferred source commit, `outgoingSuperseded` drops that queue-produced
 * `outgoing` baseline. It does not license the commit to overwrite external
 * state: a candidate a poll applied after the entry was queued is newer than the
 * commit and stays authoritative. `appliedPollNewer` carries that candidate — the
 * menus may have moved on since — and is checked as an extra baseline. A
 * candidate the menus carried before the entry was queued is either the arrival
 * state already captured by `outgoing`, or a sibling the arrival collision
 * already rejects, so it is not re-checked.
 *
 * When the plan was refused rather than replaced, the live source is the
 * authority and `allowRotation` decides whether a cross-file source commit may
 * move it: a push that still names the live source is kept — the source never
 * moved — while a source commit naming another file is a rotation the transport
 * already made and is kept too.
 */
function deferredIdentityIsAdmissible(
  identity: SourceIdentity,
  plan: PlanV3,
  arrivalPlan: PlanV3 | null,
  outgoing: SourceIdentity,
  applied: SourceIdentity,
  allowRotation: boolean,
  /**
   * The concrete candidate a poll applied *after* this entry was queued, or null
   * when none did. A URI-bearing source commit is judged by arrival order against
   * its own queue, but this state is newer than the commit and still vetoes it.
   */
  appliedPollNewer: SourceIdentity | null,
  outgoingSuperseded = false,
): boolean {
  if (!identityNamesSource(identity)) return false;

  // Whether the settled plan is a different adoption or the byte-for-byte
  // arrival plan. A refused plan still names the pre-rotation source, so its
  // URI must not veto a rotation the transport already committed to.
  const replaced = planWasReplaced(arrivalPlan, plan);

  // The winner was replaced and names a concrete candidate: its URI is the only
  // candidate it vouches for, and an exact match wins outright. This must be
  // decided before the collision guards below, which would otherwise let the
  // outgoing candidate the session was leaving veto the explicitly winning plan.
  if (replaced && plan.effective_virtual_uri != null) {
    return identityMatchesPlan(identity, plan);
  }

  // A concrete candidate already current on the push's file is authority the
  // push cannot outweigh: a file-only push carries no candidate, and a push
  // naming a different candidate is a sibling collision. A cross-file baseline
  // is a different source and does not apply.
  //
  // A URI-bearing source commit is the transport's own move, so arrival order
  // decides among the queue's own commits: `outgoingSuperseded` drops the
  // queue-produced `outgoing` baseline of an earlier deferred source commit. The
  // live `outgoing` baseline is still consulted — it is the source the transport
  // was on at arrival, and a differing concrete candidate on the same file is a
  // real collision. What arrival order does *not* license is overwriting a
  // candidate a poll folded *after* the entry was queued: that `appliedPollNewer`
  // state is newer than the commit, so it is added as a veto even for a commit.
  // An older applied candidate predates the entry and is either the arrival state
  // already captured by `outgoing` or a sibling the arrival collision already
  // rejects, so it is not re-checked.
  const judgedByArrival = allowRotation && identity.uri != null;
  const baselines: SourceIdentity[] = [outgoing];
  if (!judgedByArrival) baselines.push(applied);
  else if (appliedPollNewer != null) baselines.push(appliedPollNewer);
  for (const baseline of baselines) {
    if (outgoingSuperseded && baseline === outgoing) continue;
    if (baseline.uri == null) continue;
    // An unknown file on either side is treated as the same file: neither can
    // prove a different source, so a differing concrete URI is a collision.
    const sameFile =
      baseline.fileId == null || identity.fileId == null || baseline.fileId === identity.fileId;
    if (!sameFile) continue;
    if (identity.uri == null || identity.uri !== baseline.uri) return false;
  }

  // The plan was refused, not replaced. `allowRotation` applies to this branch
  // only; the replaced-plan branches decide from the winner.
  if (!replaced) {
    // The live source is authoritative: a push that names it exactly is a
    // same-source update, and a source commit may also prove the transport
    // rotated to another file. The refused plan's own URI describes the source
    // before that rotation, so it is not applied as authority here.
    if (identityNamesSameSource(identity, outgoing)) return true;
    return (
      allowRotation &&
      identity.fileId != null &&
      (outgoing.fileId == null || identity.fileId !== outgoing.fileId)
    );
  }

  // The winner is URI-less. The baseline loop above already refused a same-file
  // collision, so a push naming the plan's own file is admitted; a push naming
  // another file is a rotation the URI-less plan cannot vouch for.
  if (identity.fileId != null) {
    return identity.fileId === plan.effective_media_file_id;
  }
  // A URI-only push offers no file key. Admit it only when the arrival-time live
  // source was on the winner's file, which ties the candidate to the settled
  // adoption; otherwise it may belong to the file being replaced and must wait
  // for a current-source refresh instead of inheriting the replacement's id.
  return outgoing.fileId != null && outgoing.fileId === plan.effective_media_file_id;
}

/** A start body plus the optional force-relink flag the retry path adds. */
type StartRequestWithForceRelinkV3 = StartRequestV3 & { force_relink?: boolean };

export interface UsePlaybackSessionResult extends PlaybackSessionState {
  /** Starts a fresh session against another file (edition/version switch). */
  switchVersion: (fileId: number, currentPosition: number) => void;
  /**
   * Arms automatic version fallback (the version menu's Auto entry). When the
   * current source is already terminal, recovers immediately with an auto
   * re-resolve; otherwise it only arms Auto for a later dead source. A healthy
   * mid-play source is never switched.
   */
  selectAutoVersion: () => void;
  /**
   * Re-issues the start request for the file whose start was refused with a
   * retryable terminal, forcing a fresh release relist. No-op while a retry is
   * already in flight or once a plan is playing.
   */
  retryStart: () => void;
  /** `track_change` replan selecting another audio track by combined index. */
  switchAudioTrack: (index: number, currentPosition: number) => void;
  /**
   * `track_change` replan selecting (or clearing) the server-side subtitle
   * track. Only needed for tracks the server has to render — burn-in and
   * conversion. Sidecar tracks are fetched from the plan's inventory and drawn
   * by the client without involving the server.
   */
  changeSubtitleTrack: (combinedIndex: number | null, currentPosition: number) => void;
  /** `quality_change` replan for a label taken from `plan.available_qualities`. */
  changeQuality: (label: string, currentPosition: number) => void;
  /** `failure_recovery` replan after the client could not play the plan. */
  recoverFromFailure: (failure: FailureV3, currentPosition: number) => void;
  /**
   * `failure_recovery` replan for a plan the *server* invalidated over the
   * realtime `plan_invalidated` command. Resolves to whether a replacement plan
   * is now playing; the caller reports that back as the command's result.
   */
  invalidatePlan: (planId: string, reason: string, currentPosition: number) => Promise<boolean>;
  /**
   * `seek_reanchor` replan when the target lies outside the seekable window.
   * Resolves with whether a plan at the new position was adopted.
   */
  reanchorSeek: (positionSeconds: number) => Promise<boolean>;
  /**
   * Re-reads the subtitle inventory by replanning with the selection unchanged.
   * Resolves to whether a fresh plan carrying the inventory was adopted, so a
   * caller polling for late subtitles can tell a real fill-in from a transient
   * replan failure.
   */
  refreshSubtitles: (currentPosition: number) => Promise<boolean>;
  /** Folds a realtime-delivered inventory entry in without a server round trip. */
  applySubtitleTrack: (track: SubtitleInventoryItemV3) => void;
  /**
   * Folds a richer probed audio inventory read from the live catalog into the
   * menu without touching the plan or the transport.
   *
   * A session that started before probe repair carries the synthesized
   * inventory the server had then. Polling the catalog later can reveal the
   * file's real tracks; this adopts them for rendering only. It never bumps
   * `planRevision`/`transportRevision`, so the stream is not reloaded, and the
   * plan stays the source of truth: for the file the plan already names, a
   * request no richer than the current inventory is ignored. When `fileId`
   * names a different file — a poll that resolved the effective virtual
   * candidate while the plan still names the collapsed row — the inventory is
   * for another target and replaces the menu wholesale, and `effectiveVirtualUri`
   * re-keys the live identity so the version menu follows that candidate too.
   */
  applyAudioInventory: (
    tracks: PlayerAudioTrack[],
    fileId?: number | null,
    effectiveVirtualUri?: string | null,
  ) => void;
  /**
   * Adopts the effective version a transport just committed to, and that
   * release's declared audio inventory, from the realtime `source_committed`
   * event.
   *
   * A serve-layer rotation moves the live source without a plan rebuild, so
   * nothing else updates `mediaFileId`/`effectiveVirtualUri`; this is what lets
   * the version and audio menus follow the streamed release. A changed identity
   * replaces the audio inventory wholesale — including with an empty list — so
   * the previous release's tracks are never shown under the new one. The plan
   * and its revisions are untouched, so the stream does not reload; the
   * existing inventory poll upgrades the declared list to probe evidence.
   */
  applyCommittedSource: (
    source: {
      effectiveMediaFileId?: number | null;
      effectiveVirtualUri?: string | null;
      inventoryStatus?: string | null;
    },
    audioTracks: PlayerAudioTrack[],
  ) => void;
  /**
   * Adopts a live inventory revision from the realtime `inventory_updated`
   * event: the committed identity, its audio list, and the complete subtitle
   * inventory in one fold.
   *
   * It obeys the same replacement-start guard as `applyCommittedSource` (a
   * pending switch owns the menus), is status-aware so only verified data
   * clears the provisional markers, and never bumps the plan or transport
   * revisions, so the stream keeps playing.
   */
  applyInventoryUpdate: (payload: PlaybackInventoryUpdatedPayload) => void;
  /** Keeps transport state current for output-capability replans. */
  updatePlaybackState: (positionSeconds: number, playing: boolean) => void;
  /**
   * Called when a transport shows its first frame. Reports `first_frame` once
   * per playback attempt, with `first_frame_ms` measured from the viewer's
   * request when one timed it; later transports of the attempt are ignored.
   */
  reportFirstFrame: () => void;
  /** Reports a playback route event as a diagnostic. Never affects playback. */
  reportEvent: (
    event: RouteEventNameV3,
    extra?: {
      failureClassification?: string;
      fallbackReason?: string;
      diagnostics?: Record<string, string | number | boolean | undefined | null>;
    },
  ) => void;
}

function subtitleSourceOf(source: string): PlayerSubtitleInfo["source"] {
  switch (source) {
    case "external":
    case "embedded":
    case "downloaded":
      return source;
    default:
      return undefined;
  }
}

/**
 * Maps the plan's subtitle inventory onto the player's track shape.
 *
 * `index` is the server's combined ordinal, copied verbatim: it is the identity
 * the client echoes back on a track change, and the key every subtitle consumer
 * in the player looks tracks up by. Entries the server publishes as
 * `burn_in_only` have no URL and are kept anyway — they are selectable, and
 * selecting one is what asks the server to burn them in.
 */
function mapSubtitleInventory(
  inventory: SubtitleInventoryItemV3[],
  mediaFileId: number,
  config: PlayerConfig,
): PlayerSubtitleInfo[] {
  const token = config.getAccessToken();
  return inventory.map((item) => ({
    index: item.combined_index,
    media_file_id: mediaFileId,
    track_id: item.track_id,
    burn_in_only: item.delivery === "burn_in_only",
    language: item.language ?? "",
    codec: item.codec,
    label: item.label ?? item.language ?? `Track ${item.combined_index + 1}`,
    source: subtitleSourceOf(item.source),
    forced: item.forced,
    hearing_impaired: item.hearing_impaired,
    url: item.url ? buildPlayerStreamUrl(config.apiBaseUrl, item.url, token) : "",
    font_bundle_url: item.font_bundle_url
      ? buildPlayerStreamUrl(config.apiBaseUrl, item.font_bundle_url, token)
      : undefined,
  }));
}

/**
 * Projects a plan onto the session state consumers render from.
 *
 * `durationSeconds` comes from `plan.source.duration_seconds` and nowhere else:
 * the spec forbids substituting the playback engine's reported duration, which
 * on an HLS copy remux is only the length produced so far.
 */
function resolveCarriedSubtitleIndexAcrossVersions(
  outgoingPlan: PlanV3 | null,
  targetFileId: number,
  versions: PlayerFileVersion[],
): number | null {
  if (!outgoingPlan) {
    return null;
  }
  const outgoingIndex = outgoingPlan.selected_tracks.subtitle?.index;
  if (outgoingIndex == null || outgoingIndex < 0) {
    return null;
  }
  const outgoingTrack = outgoingPlan.subtitle.inventory.find(
    (item) => item.combined_index === outgoingIndex,
  );
  if (!outgoingTrack) {
    return null;
  }
  const targetVersion = versions.find((v) => v.file_id === targetFileId);
  if (!targetVersion?.subtitle_tracks || targetVersion.subtitle_tracks.length === 0) {
    return null;
  }
  // Shared ordinal derivation so the carried index names the same track the
  // server publishes in the target version's inventory; see subtitleInventory.ts.
  const candidates = buildPublishedSubtitleTracks(targetVersion.subtitle_tracks);
  const match = matchSubtitleTrackAcrossVersions(candidates, outgoingTrack);
  return match?.index ?? null;
}

// Temporary client-side heuristic for A/V transport equivalence.
// TODO: replace with a server-authoritative transport_generation on PlanV3
// that bumps only when the A/V bytes change, so sidecar-only replans never
// guess. Until then, compare the fields that determine the produced bytes.
function isSameAVTransport(prev: PlanV3 | null, next: PlanV3): boolean {
  if (!prev) return false;
  if (prev.delivery !== next.delivery) return false;
  if (prev.stream.url !== next.stream.url) return false;
  if (prev.stream.protocol !== next.stream.protocol) return false;
  if (prev.stream.container !== next.stream.container) return false;
  if (prev.stream.mime_type !== next.stream.mime_type) return false;
  if (prev.effective_media_file_id !== next.effective_media_file_id) return false;
  // A start naming a different requested file is a version switch even when the
  // server resolves both rows to the same effective file and reuses the same
  // transport URL: collapsed `virtual://…` rows share one concrete candidate, so
  // `effective_media_file_id` and `stream.url` can be identical across the two
  // plans. Reusing the mounted element there leaves the old session's picture on
  // screen, so the requested identity is part of the transport identity. Replans
  // carry no file id and keep the durable requested row, so this never fires on
  // an ordinary track/quality replan.
  if (prev.requested_media_file_id !== next.requested_media_file_id) return false;
  if (prev.selected_tracks.audio?.index !== next.selected_tracks.audio?.index) return false;
  if (prev.timeline.stream_origin_seconds !== next.timeline.stream_origin_seconds) return false;
  if (prev.timeline.can_seek_anywhere !== next.timeline.can_seek_anywhere) return false;
  // A text-sidecar selection (or a `track_change` that re-mints the same
  // sidecar URLs) does not change the produced A/V bytes, so it must not bump
  // the transport revision. Only a burn-in route changes the picture, and only
  // it makes the selected subtitle track part of the transport identity. The
  // server reuses the active transport for a sidecar-only replan, so the URL
  // compared above stays identical; if it did not, that URL change is a real
  // transport change and must reload the element.
  if (prev.subtitle.mode === "burn_in" || next.subtitle.mode === "burn_in") {
    if (prev.subtitle.mode !== next.subtitle.mode) return false;
    if (prev.subtitle.track_id !== next.subtitle.track_id) return false;
  }
  return true;
}

function planToSessionState(
  plan: PlanV3,
  sessionId: string | null,
  playbackAttemptId: string,
  planRevision: number,
  transportRevision: number,
  qualityPreference: string,
  shouldAutoPlay: boolean,
  autoFallback: boolean,
  config: PlayerConfig,
): PlaybackSessionState {
  return {
    plan,
    planRevision,
    transportRevision,
    streamUrl: buildPlayerStreamUrl(config.apiBaseUrl, plan.stream.url, config.getAccessToken()),
    sessionId,
    playbackAttemptId,
    mediaFileId: plan.effective_media_file_id,
    effectiveVirtualUri: plan.effective_virtual_uri ?? null,
    initialPosition: plan.timeline.player_start_seconds,
    audioTrackIndex: plan.selected_tracks.audio?.index ?? 0,
    durationSeconds: plan.source.duration_seconds ?? null,
    planAudioTracks: plan.audio_tracks ?? [],
    audioInventoryProvisional: isInventoryProvisional(
      plan.inventory_status,
      plan.inventory_provenance,
    ),
    subtitleInventoryProvisional: isInventoryProvisional(
      plan.inventory_status,
      plan.inventory_provenance,
    ),
    subtitleUrls: mapSubtitleInventory(
      plan.subtitle.inventory,
      plan.effective_media_file_id,
      config,
    ),
    qualityPreference,
    shouldAutoPlay,
    loading: false,
    replacing: false,
    replanning: false,
    autoFallback,
    replanningQuality: false,
    pendingSwitchFileId: null,
    errorTitle: null,
    error: null,
    errorReason: null,
    errorRetryable: false,
    retrying: false,
    initialSubtitleErrorTitle: null,
    initialSubtitleError: null,
  };
}

/**
 * Turns a v3 decision into an error state.
 *
 * A conforming decision has exactly two shapes: a plan or a terminal. The
 * fallback below is defensive handling for a malformed or future response; it
 * is not a third protocol outcome.
 */
function describeDecisionWithoutPlan(decision: DecisionResponseV3): PlaybackSessionErrorState {
  if (decision.terminal) {
    return describePlanTerminal(decision.terminal);
  }
  return {
    title: "Playback unavailable",
    message: "This server is not accepting playback requests right now.",
  };
}

function describePlaybackSessionError(
  error: unknown,
  fallbackMessage: string,
): PlaybackSessionErrorState {
  const transportError = describePlaybackTransportError(error);
  if (transportError) {
    return transportError;
  }

  if (error instanceof Error && error.message.trim().length > 0) {
    return {
      title: "Playback unavailable",
      message: error.message,
    };
  }

  return {
    title: "Playback unavailable",
    message: fallbackMessage,
  };
}

/**
 * Owns the v3 playback session: the start decision, the plan it produced, and
 * every replan that supersedes it.
 *
 * 1. On mount: probe the browser → POST `/playback/start` with a v3 request →
 *    adopt the returned plan.
 * 2. Quality, track and recovery changes go to `/playback/{id}/replan`; each
 *    answers with a whole new plan, never a patch.
 * 3. On unmount: DELETE `/playback/{session_id}` with a keepalive fallback.
 */
export function usePlaybackSession(
  requestKey: string,
  versions: PlayerFileVersion[],
  playbackVariants: PlayerPlaybackVariant[] = [],
  fileId?: number,
  initialPosition = 0,
  forceInitialPosition = false,
  qualityPreference?: string | null,
  maxBitrateKbps?: number | null,
  resumeHints?: ResumeHints,
  explicitAudioTrackIndex?: number | null,
  initialSubtitleTrackIndexByFileId?: Record<number, number>,
  initialBitmapSubtitleTrackIndexByFileId?: Record<number, number>,
  /** True when the initial `fileId` was explicitly chosen by the viewer (not
   * auto-selected); the server must not silently substitute another version. */
  explicitFileSelection = false,
  /** When true, the server should force a re-link/re-query of the virtual file
   *  on this start attempt. Only set when the viewer explicitly picks an
   *  unavailable version. */
  forceRelink = false,
  allowAlternateVersions = true,
): UsePlaybackSessionResult {
  const config = usePlayerConfig();
  const probe = useCodecDetection();
  const capabilitiesSettled = probe.settled;
  const clientCapabilities = useMemo(() => buildClientCapabilitiesV3(probe), [probe]);
  const clientPlaybackContext = useMemo(() => buildClientPlaybackContextV3(probe), [probe]);
  const capabilityRequestKey = useMemo(
    () => JSON.stringify([clientCapabilities, clientPlaybackContext]),
    [clientCapabilities, clientPlaybackContext],
  );

  const [state, setState] = useState<PlaybackSessionState>({
    plan: null,
    planRevision: 0,
    transportRevision: 0,
    streamUrl: null,
    sessionId: null,
    playbackAttemptId: null,
    mediaFileId: null,
    effectiveVirtualUri: null,
    initialPosition: 0,
    audioTrackIndex: 0,
    durationSeconds: null,
    subtitleUrls: [],
    planAudioTracks: [],
    audioInventoryProvisional: false,
    subtitleInventoryProvisional: false,
    qualityPreference: qualityPreference?.trim() || "auto",
    shouldAutoPlay: true,
    loading: true,
    replacing: false,
    replanning: false,
    autoFallback: allowAlternateVersions && !explicitFileSelection,
    replanningQuality: false,
    pendingSwitchFileId: null,
    errorTitle: null,
    error: null,
    errorReason: null,
    errorRetryable: false,
    retrying: false,
    initialSubtitleErrorTitle: null,
    initialSubtitleError: null,
  });

  const sessionIdRef = useRef<string | null>(null);
  const planRef = useRef<PlanV3 | null>(null);
  const planRevisionRef = useRef(0);
  const transportRevisionRef = useRef(0);
  const serverFeaturesRef = useRef<string[]>([]);
  const stateRef = useRef(state);
  // The live effective source identity, folded through the same commits and
  // rotations as `state` but readable synchronously inside callbacks. Deferring
  // a push still moves where the stream actually is; later decisions (a queued
  // replan's staleness check, the next rotation's outgoing baseline) must see
  // that move, which the rendered state at a settle path does not.
  const liveSourceIdentityRef = useRef<SourceIdentity>({
    fileId: state.mediaFileId,
    uri: state.effectiveVirtualUri,
  });
  // The identity the most recent deferred source commit moved the live mirror
  // to, when nothing external has transitioned it since. A later deferred source
  // commit must not treat its own predecessor from the same queue as an
  // authority to answer to, while a poll fold is external and stays
  // authoritative; `transitionSourceIdentity` clears this on every such fold.
  const lastDeferredSourceIdentityRef = useRef<SourceIdentity | null>(null);
  // A monotonic clock raised on every *external* source-identity transition (a
  // poll fold or an adopted plan), never by the deferred queue's own replay. A
  // deferred push records the clock at arrival; a later poll fold raises it, so
  // the flush can tell a candidate folded after the entry was queued from one
  // the menus already carried at capture. See `captureDeferredPush`.
  const identityTransitionClockRef = useRef(0);
  // The live candidate a poll folded on the last external transition, with the
  // clock reading of that transition. Between an entry's arrival and its flush a
  // poll can move the menus to a candidate, and the winning plan's own adoption
  // can then overwrite the menu mirror before the flush runs. Remembering the
  // poll's candidate lets the flush veto a stale commit that would otherwise
  // overwrite it; the clock says whether the fold was newer than the entry.
  const lastPollAppliedRef = useRef<{ clock: number; identity: SourceIdentity } | null>(null);
  // The identity the menus render, mirrored outside the state updater. A poll
  // or rotation can fold before React has processed an earlier push in the same
  // tick, so the next transition is computed against this synchronous mirror;
  // the rendered state would still name the previous source.
  const menuIdentityRef = useRef<SourceIdentity>({
    fileId: state.mediaFileId,
    uri: state.effectiveVirtualUri,
  });
  const activeRequestKeyRef = useRef<string | null>(null);
  const activeCapabilityRequestKeyRef = useRef<string | null>(null);
  const playbackPositionRef = useRef(initialPosition);
  const startIntentRef = useRef({
    position: initialPosition,
    forceStartPosition: forceInitialPosition,
    // When the viewer asked for this request, for its first_frame_ms.
    intentAt: null as number | null,
  });
  const hasAdoptedPlanRef = useRef(false);
  const awaitingInitialPlayerPositionRef = useRef(false);
  const playbackPlayingRef = useRef(true);
  const playbackStartedRef = useRef(false);
  const switchingRef = useRef(false);
  // Mirrors state.autoFallback so callbacks can read the armed state without
  // depending on the rendered value.
  const autoFallbackRef = useRef(allowAlternateVersions && !explicitFileSelection);
  // The last live inventory revision folded into the menus. The server stamps
  // every `inventory_updated` push with its inventory_revision and documents a
  // duplicate or stale revision as a no-op, so this gates repeat deliveries
  // without comparing plan identity (which a rotation can move).
  const inventoryRevisionRef = useRef<string | null>(null);
  // Identity-carrying pushes dropped while a start/switch/replan owns the
  // session. Applying them immediately would mutate the menus under the pending
  // replacement, but discarding them loses the only carrier of the incoming
  // source's virtual identity: the v2 wire omits `effective_virtual_uri`, so the
  // replacement plan may not name the candidate the push does. They are held in
  // one arrival-ordered queue and replayed once that operation settles and the
  // settled plan can vouch for the source. The order matters: a source commit
  // and an inventory revision for the same source must fold in the order the
  // server sent them, so a newer source is never overwritten by an older
  // inventory and vice versa.
  const deferredPushesRef = useRef<DeferredPush[]>([]);
  const deferredPushSeqRef = useRef(0);
  const deferredFlushTimerRef = useRef<number | null>(null);
  // Late-bound sink: the flusher is defined later in the render, after the
  // fold callbacks it closes over, so the interval reads it through this ref.
  const flushDeferredPushesRef = useRef<() => void>(() => {});
  // A bounded retry cadence so a push dropped while a start/switch/replan owns
  // the session is still folded in when that operation settles without a signal
  // this hook observes. The settle paths call the flusher directly; this is the
  // backstop for the fast pending/declared start.
  useEffect(() => {
    deferredFlushTimerRef.current = window.setInterval(() => {
      if (deferredPushesRef.current.length > 0) {
        flushDeferredPushesRef.current();
      }
    }, 2_000);
    return () => {
      if (deferredFlushTimerRef.current !== null) {
        window.clearInterval(deferredFlushTimerRef.current);
        deferredFlushTimerRef.current = null;
      }
    };
  }, []);
  // Latest-wins coalescing for version switches: while a switch is in flight,
  // a second click records the newest target here instead of being dropped, and
  // the completion handler starts the switch to it immediately.
  const pendingSwitchFileIdRef = useRef<number | null>(null);
  // The live position the viewer was at when they made that second click. The
  // chained switch starts only after the first one settles, so reusing the
  // first click's closure position would seek the replacement back to where the
  // viewer was dozens of seconds earlier.
  const pendingSwitchPositionRef = useRef<number | null>(null);
  const loadSequenceRef = useRef(0);
  // The start that produced the current terminal, so a retry can re-issue it
  // against the same file with the viewer's original position and selection.
  const retryTargetRef = useRef<{
    fileId: number;
    position: number;
    forceStartPosition: boolean;
    fileSelection: "auto" | "explicit";
    carriedAudioTrackId: string | null;
  } | null>(null);
  const retryingRef = useRef(false);
  // The dead session whose refused replan has already triggered one fresh
  // start. A reaped session gets exactly one rebuild; the replacement start is
  // a `start`, not a replan, so it cannot recursively trigger another. The key
  // is the session id: recovering the replacement is a different id and is
  // allowed, while a replan still refused for the session already rebuilt
  // surfaces the error instead of starting again.
  const recoveredDeadSessionRef = useRef<string | null>(null);

  // v3 identity. `playback_attempt_id` spans one whole attempt chain (a start
  // and every replan that follows it); `plan_attempt_id` identifies the single
  // plan currently on screen. Both are minted by the client — the server mints
  // `plan_id` and `plan_attempt_key`, which the client only ever echoes.
  const playbackAttemptIdRef = useRef<string | null>(null);
  const planAttemptIdRef = useRef<string>(randomUUID());
  const attemptedPlanKeysRef = useRef<string[]>([]);
  const attemptCountRef = useRef(1);
  const replanInFlightRef = useRef(false);
  // The attempt whose first frame is still to be reported. `intentAt` is the
  // `performance.now()` of the viewer's request that started it, or null when
  // nothing timed it; the event is still sent, just without a duration.
  const firstFrameRef = useRef<{
    attemptId: string;
    intentAt: number | null;
    reported: boolean;
  } | null>(null);
  // Adoptions in flight, counted per load sequence: a start or a replan whose
  // decision has not been applied yet. The server commits a replacement plan —
  // and starts the copy-safety scan behind it — before the client can read the
  // response, so a `plan_invalidated` command can name a plan this client is
  // still adopting. Waiters registered here are woken once their own sequence
  // has nothing in flight, which lets an invalidation decide against the plan
  // that actually won.
  //
  // The key is what makes the wait bounded. A superseded request — a version
  // switch abandoned mid-flight, a start whose `fetch` never settles — is not a
  // candidate to own the session any more, so waiting for it decides nothing
  // and is worse than not waiting: the server's invalidation deadline is 8s,
  // and a session that misses it is stopped outright. Only the sequence that
  // currently owns the session can still change what the invalidation should
  // decide against, so only it is waited on.
  const adoptionsInFlightRef = useRef(new Map<number, number>());
  const adoptionWaitersRef = useRef<Array<{ loadSequence: number; resolve: () => void }>>([]);
  const pendingReplanRef = useRef<{
    options: ReplanOptions;
    loadSequence: number;
    retireSessionOnRefusal: boolean;
    resolve: (adopted: boolean) => void;
    planId: string;
    // The live effective file the op was built against. A serve-layer rotation
    // moves it without changing `plan_id`, so the plan-id check alone would
    // replay a plan-bound op against another release's track ordinals.
    mediaFileId: number | null;
  } | null>(null);
  const issueReplanRef = useRef<
    (options: ReplanOptions, retireSessionOnRefusal?: boolean) => Promise<boolean>
  >(async () => false);
  const qualityRef = useRef(qualityPreference?.trim() || "auto");

  useEffect(() => {
    stateRef.current = state;
  }, [state]);

  /**
   * The single transition point for the authoritative source identity.
   *
   * The rendered state is not a usable baseline for the next fold: React may
   * batch two pushes into one render, so a rotation followed by a stale push in
   * the same tick would compare against the pre-rotation identity. Every fold
   * moves `menuIdentityRef` and the live mirror here, synchronously and outside
   * any state updater, and the updater only mirrors the value it wrote.
   */
  const transitionSourceIdentity = useCallback(
    (fileId: number | null, uri: string | null): boolean => {
      const previous = menuIdentityRef.current;
      const changed = previous.fileId !== fileId || previous.uri !== uri;
      menuIdentityRef.current = { fileId, uri };
      // A fold of the menu identity is also a commit of the live source, so the
      // outgoing baseline for the next rotation follows it. A deferred push
      // moves the live mirror separately in `captureDeferredPush`. Any external
      // transition supersedes the last deferred source commit, so it is no
      // longer a baseline a later queued commit may ignore.
      liveSourceIdentityRef.current = { fileId, uri };
      lastDeferredSourceIdentityRef.current = null;
      identityTransitionClockRef.current += 1;
      return changed;
    },
    [],
  );

  /**
   * Records a candidate a catalog poll folded, with the transition clock of the
   * fold. `deferredIdentityIsAdmissible` consults it so a source commit queued
   * before the poll cannot overwrite the poll's newer candidate, even after the
   * winning plan's own adoption has since overwritten the menu mirror.
   */
  const recordPollApplied = useCallback((identity: SourceIdentity) => {
    lastPollAppliedRef.current = { clock: identityTransitionClockRef.current, identity };
  }, []);

  /**
   * The revision bookkeeping scope for the current session generation.
   *
   * A revision is the server's opaque digest of the whole inventory *and* the
   * effective source it describes, so within one session redelivering a revision
   * that was already folded is the same payload and folding it twice is wrong.
   * Only genuinely applied revisions are recorded; a revision the flush refused
   * is deliberately left unrecorded, so a redelivery that the plan (or a later
   * rotation) vouches for still folds. The scope is keyed by the load generation
   * and resets only when that moves; an identity rotation must not clear it, or a
   * revision folded before the rotation would fold again once the source moved.
   */
  const revisionScopeRef = useRef<{ generation: number; revisions: Set<string> }>({
    generation: -1,
    revisions: new Set(),
  });
  const syncRevisionScope = useCallback(() => {
    const scope = revisionScopeRef.current;
    if (scope.generation !== loadSequenceRef.current) {
      scope.generation = loadSequenceRef.current;
      scope.revisions.clear();
    }
    return scope;
  }, []);

  const beginAdoption = useCallback((loadSequence: number) => {
    const inFlight = adoptionsInFlightRef.current;
    inFlight.set(loadSequence, (inFlight.get(loadSequence) ?? 0) + 1);
  }, []);

  /**
   * Counts one in-flight adoption out of its load sequence.
   *
   * A sequence's waiters are woken only when nothing is left in flight for it:
   * a queued replan is dispatched from its predecessor's `finally` before the
   * predecessor is counted out, so the count tracks the whole chain rather than
   * one request.
   */
  const endAdoption = useCallback((loadSequence: number) => {
    const inFlight = adoptionsInFlightRef.current;
    const remaining = (inFlight.get(loadSequence) ?? 0) - 1;
    if (remaining > 0) {
      inFlight.set(loadSequence, remaining);
      return;
    }
    inFlight.delete(loadSequence);
    const waiters = adoptionWaitersRef.current;
    if (waiters.length === 0) return;
    const settled = waiters.filter((waiter) => !inFlight.has(waiter.loadSequence));
    if (settled.length === 0) return;
    adoptionWaitersRef.current = waiters.filter((waiter) => inFlight.has(waiter.loadSequence));
    for (const waiter of settled) waiter.resolve();
  }, []);

  /**
   * Resolves once the sequence that currently owns the session has no start or
   * replan in flight, or null when it has none — callers act synchronously in
   * the common case rather than deferring a turn.
   *
   * A request from a superseded sequence is deliberately not waited for. It can
   * no longer install a plan (every path re-checks the sequence before adopting
   * one), so it has nothing left to say about what an invalidation should
   * decide against, and a hung one would otherwise hold the wait open past the
   * server's deadline and cost the live session its stream.
   */
  const awaitAdoptionSettled = useCallback((): Promise<void> | null => {
    const loadSequence = loadSequenceRef.current;
    if (!adoptionsInFlightRef.current.has(loadSequence)) return null;
    return new Promise<void>((resolve) => {
      adoptionWaitersRef.current.push({ loadSequence, resolve });
    });
  }, []);

  const reportEvent = useCallback(
    (
      event: RouteEventNameV3,
      extra?: {
        failureClassification?: string;
        fallbackReason?: string;
        diagnostics?: Record<string, string | number | boolean | undefined | null>;
      },
    ) => {
      const attemptId = playbackAttemptIdRef.current;
      if (!attemptId) return;
      const plan = planRef.current;
      const input = {
        event,
        playbackAttemptId: attemptId,
        ...routeEventPlanIdentityV3(plan, sessionIdRef.current, planAttemptIdRef.current),
        ...extra,
      };
      const sessionId = sessionIdRef.current;
      if (sessionId && hasSequencedProgress(sessionId)) {
        void reportSessionRouteEventV2(config, sessionId, buildRouteEventV3(input));
      }
      // A terminal start never produced a session; there is nothing to report it against.
    },
    [config],
  );

  /**
   * Adopts a decision as the live session, or surfaces its terminal.
   * Returns whether a plan was adopted.
   */
  const adoptDecision = useCallback(
    (
      decision: DecisionResponseV3,
      initialSubtitleFailure?: PlaybackSessionErrorState | null,
      forceTransportBump?: boolean,
    ): boolean => {
      serverFeaturesRef.current = decision.server_features;
      const plan = decision.playback_plan;
      if (!plan) {
        const failure = describeDecisionWithoutPlan(decision);
        if (decision.terminal) {
          reportEvent("terminal", { failureClassification: decision.terminal.reason });
        }
        // The plan already on screen is left alone. A refused replan surfaces
        // its reason without taking away the stream that is still playing; a
        // refused start had no stream to take away in the first place.
        setState((current) => ({
          ...current,
          loading: false,
          replacing: false,
          replanning: false,
          replanningQuality: false,
          pendingSwitchFileId: null,
          errorTitle: failure.title,
          error: failure.message,
          errorReason: failure.reason ?? null,
          errorRetryable: failure.retryable ?? false,
        }));
        return false;
      }

      const prevPlan = planRef.current;
      const sessionId = plan.session_id ?? decision.session_id ?? sessionIdRef.current;
      planAttemptIdRef.current = randomUUID();
      planRef.current = plan;
      sessionIdRef.current = sessionId ?? null;
      // The adopted plan is the new live source; a rotation deferred before it
      // landed is superseded by what the plan itself names.
      transitionSourceIdentity(plan.effective_media_file_id, plan.effective_virtual_uri ?? null);
      // A live plan means the dead session that forced a rebuild is behind us;
      // a replacement that landed on a new id clears the one-recovery guard so
      // that session can recover once in its turn.
      if (
        recoveredDeadSessionRef.current !== null &&
        recoveredDeadSessionRef.current !== sessionId
      ) {
        recoveredDeadSessionRef.current = null;
      }
      planRevisionRef.current += 1;
      // A failure recovery always prepares a fresh server generation, even when
      // the replacement plan reuses the same session-scoped URL and transport
      // shape; the A/V-byte heuristic cannot see that regeneration. Force a
      // transport revision bump so the player reloads and re-fetches the stream.
      if (forceTransportBump || !isSameAVTransport(prevPlan, plan)) {
        transportRevisionRef.current += 1;
      }
      hasAdoptedPlanRef.current = true;
      if (
        Number.isFinite(plan.timeline.source_start_seconds) &&
        plan.timeline.source_start_seconds >= 0
      ) {
        // Replan positions use the source timeline. Seed it immediately so an
        // output refresh before the first timeupdate preserves server-resolved
        // resume state instead of falling back to the caller's default zero.
        playbackPositionRef.current = plan.timeline.source_start_seconds;
        awaitingInitialPlayerPositionRef.current = plan.timeline.source_start_seconds > 0;
      }

      setState((current) => ({
        ...planToSessionState(
          plan,
          sessionId ?? null,
          playbackAttemptIdRef.current ?? "",
          planRevisionRef.current,
          transportRevisionRef.current,
          qualityRef.current,
          playbackPlayingRef.current,
          current.autoFallback,
          config,
        ),
        initialSubtitleErrorTitle:
          initialSubtitleFailure === undefined
            ? current.initialSubtitleErrorTitle
            : (initialSubtitleFailure?.title ?? null),
        initialSubtitleError:
          initialSubtitleFailure === undefined
            ? current.initialSubtitleError
            : (initialSubtitleFailure?.message ?? null),
      }));
      reportEvent("plan_selected");
      return true;
    },
    [config, reportEvent, transitionSourceIdentity],
  );

  const requestStart = useCallback(
    async (
      targetFileId: number,
      position: number,
      forceStartPosition: boolean,
      playbackAttemptId: string,
      subtitleTrackIndex: number | undefined,
      carriedAudioTrackID: string | null,
      fileSelection: "auto" | "explicit",
      forceRelink?: boolean,
    ): Promise<DecisionResponseV3> => {
      const body: StartRequestWithForceRelinkV3 = buildStartRequestV3({
        extraClientFeatures: VIDEO_CLIENT_FEATURES_V3,
        fileId: targetFileId,
        profileId: config.getProfileId() ?? "",
        playbackAttemptId,
        qualityPreference: qualityRef.current,
        allowAlternateVersions,
        position,
        forceStartPosition,
        // A carried audio track (version switch) names the file-bound identity
        // explicitly; the mount-time ordinal is meaningless for the new file
        // and must not be sent alongside it.
        explicitAudioTrackIndex: carriedAudioTrackID ? null : explicitAudioTrackIndex,
        carriedAudioTrackID,
        fileSelection,
        forceRelink,
        subtitleTrackIndex,
        metered: detectMeteredV3(),
        bandwidthEstimateKbps: detectBandwidthEstimateKbpsV3(),
        bandwidthCapKbps: maxBitrateKbps,
        clientCapabilities,
        clientPlaybackContext,
      });
      // force_relink asks the server to re-query the provider and drop a stale
      // virtual-source pin; without it a retry replays the cached candidate.
      if (forceRelink) {
        body.force_relink = true;
      }

      return await startPlaybackV2(config, body);
      // carriedAudioTrackID is a per-call argument, not render-scope state, so
      // it is intentionally absent from the deps array.
    },
    [
      allowAlternateVersions,
      clientCapabilities,
      clientPlaybackContext,
      config,
      explicitAudioTrackIndex,
      maxBitrateKbps,
    ],
  );

  const stopSession = useCallback(
    async (sessionId: string) => {
      await stopSequencedSession(config, sessionId);
    },
    [config],
  );

  const retireActiveSession = useCallback(
    (expectedSessionId: string) => {
      if (sessionIdRef.current !== expectedSessionId) return;
      loadSequenceRef.current += 1;
      void stopSession(expectedSessionId).catch(() => {
        // Best effort — stale session will time out server-side.
      });
      planRef.current = null;
      sessionIdRef.current = null;
      planAttemptIdRef.current = randomUUID();
      deferredPushesRef.current = [];
      transitionSourceIdentity(null, null);
      setState((current) => {
        if (current.sessionId !== expectedSessionId) return current;
        return {
          ...current,
          plan: null,
          streamUrl: null,
          sessionId: null,
          mediaFileId: null,
          effectiveVirtualUri: null,
          initialPosition: 0,
          audioTrackIndex: 0,
          durationSeconds: null,
          subtitleUrls: [],
          planAudioTracks: [],
          audioInventoryProvisional: false,
          subtitleInventoryProvisional: false,
          loading: false,
          replacing: false,
          replanning: false,
          replanningQuality: false,
          pendingSwitchFileId: null,
        };
      });
    },
    [stopSession, transitionSourceIdentity],
  );

  /**
   * Picks which file to *request*. The server owns adaptation — this only
   * expresses which edition the viewer is resuming, which is knowledge the
   * server does not have.
   */
  const selectFileId = useCallback(
    (preferredFileId?: number) => {
      if (preferredFileId) return preferredFileId;
      if (resumeHints?.lastFileId) {
        const exact = versions.find((v) => v.file_id === resumeHints.lastFileId);
        if (exact) return exact.file_id;
      }
      const variantFileId = selectDefaultVariantFile(playbackVariants, versions, resumeHints);
      if (variantFileId) return variantFileId;
      return versions[0]?.file_id ?? null;
    },
    [playbackVariants, resumeHints, versions],
  );

  const loadSession = useCallback(
    async ({
      preferredFileId,
      position,
      forceStartPosition,
      allowPreserveExistingSessionOnError,
      replacementErrorMessage,
      initialErrorMessage,
      carriedAudioTrackId,
      carriedSubtitleTrackIndex,
      fileSelection,
      forceRelink,
      intentAt,
    }: {
      preferredFileId?: number;
      position: number;
      forceStartPosition: boolean;
      allowPreserveExistingSessionOnError: boolean;
      replacementErrorMessage: string;
      initialErrorMessage: string;
      // Audio track identity carried from the current plan into a replacement
      // start (a version switch). The server remaps it by family to the new
      // file instead of dropping the viewer's selection.
      carriedAudioTrackId?: string | null;
      // Subtitle track selection carried across a version switch. Null means
      // subtitles were explicitly off on the outgoing plan and must stay off;
      // undefined leaves the start to initialSubtitleTrackIndexByFileId.
      carriedSubtitleTrackIndex?: number | null;
      /** How the requested file was chosen; `explicit` forbids silent
       * server-side version substitution. */
      fileSelection?: "auto" | "explicit";
      /** When true, the server should force a re-link/re-query of the virtual
       * file on this start attempt. Set when the viewer explicitly picks an
       * unavailable version, or by the retry path after a no-streams terminal. */
      forceRelink?: boolean;
      /** When the viewer asked for this start, for its first_frame_ms. */
      intentAt: number | null;
    }) => {
      const previousState = stateRef.current;
      const previousSessionId = sessionIdRef.current;
      // The session id comes from the ref, not the (possibly stale) state
      // snapshot: a recovery that retires a dead session before starting its
      // replacement nulls the ref in the same tick, and the replacement must be
      // treated as a fresh start rather than a replacement of the dead plan.
      const hasExistingSession = !!previousSessionId && !!previousState.streamUrl;
      const loadSequence = ++loadSequenceRef.current;
      const previousAttempt = {
        playbackAttemptId: playbackAttemptIdRef.current,
        planAttemptId: planAttemptIdRef.current,
        attemptedPlanKeys: [...attemptedPlanKeysRef.current],
        attemptCount: attemptCountRef.current,
      };
      const restorePreviousAttempt = () => {
        playbackAttemptIdRef.current = previousAttempt.playbackAttemptId;
        planAttemptIdRef.current = previousAttempt.planAttemptId;
        attemptedPlanKeysRef.current = previousAttempt.attemptedPlanKeys;
        attemptCountRef.current = previousAttempt.attemptCount;
      };

      setState((current) => ({
        ...current,
        loading: !hasExistingSession,
        replacing: hasExistingSession,
        replanning: false,
        replanningQuality: false,
        errorTitle: hasExistingSession ? current.errorTitle : null,
        error: hasExistingSession ? current.error : null,
        errorReason: hasExistingSession ? current.errorReason : null,
        errorRetryable: hasExistingSession ? current.errorRetryable : false,
        initialSubtitleErrorTitle: hasExistingSession ? current.initialSubtitleErrorTitle : null,
        initialSubtitleError: hasExistingSession ? current.initialSubtitleError : null,
      }));

      // A start begins a new attempt chain: fresh attempt id, empty loop guard.
      let playbackAttemptId = randomUUID();
      playbackAttemptIdRef.current = playbackAttemptId;
      attemptedPlanKeysRef.current = [];
      attemptCountRef.current = 1;

      const retirePreviousSession = (nextError?: PlaybackPolicyErrorDescription) => {
        if (previousSessionId) {
          void stopSession(previousSessionId).catch(() => {
            // Best effort — stale session will time out server-side.
          });
        }
        planRef.current = null;
        sessionIdRef.current = null;
        planAttemptIdRef.current = randomUUID();
        deferredPushesRef.current = [];
        transitionSourceIdentity(null, null);
        setState((current) => ({
          ...current,
          plan: null,
          streamUrl: null,
          sessionId: null,
          playbackAttemptId,
          mediaFileId: null,
          effectiveVirtualUri: null,
          initialPosition: 0,
          audioTrackIndex: 0,
          durationSeconds: null,
          subtitleUrls: [],
          planAudioTracks: [],
          audioInventoryProvisional: false,
          subtitleInventoryProvisional: false,
          loading: false,
          replacing: false,
          replanning: false,
          replanningQuality: false,
          pendingSwitchFileId: null,
          errorTitle: nextError?.title ?? current.errorTitle,
          error: nextError?.message ?? current.error,
          errorReason: nextError ? (nextError.reason ?? null) : current.errorReason,
          errorRetryable: nextError?.retryable ?? current.errorRetryable,
        }));
      };

      beginAdoption(loadSequence);
      try {
        const selectedFileId = selectFileId(preferredFileId);
        if (!selectedFileId) {
          throw new Error("No playable version found");
        }
        // Remember what to re-issue if this start ends in a retryable terminal.
        retryTargetRef.current = {
          fileId: selectedFileId,
          position,
          forceStartPosition,
          fileSelection: fileSelection ?? "auto",
          carriedAudioTrackId: carriedAudioTrackId ?? null,
        };

        const targetSubtitleIndex =
          carriedSubtitleTrackIndex !== undefined
            ? (carriedSubtitleTrackIndex ?? undefined)
            : initialSubtitleTrackIndexByFileId?.[selectedFileId];

        const decision = await requestStart(
          selectedFileId,
          position,
          forceStartPosition,
          playbackAttemptId,
          targetSubtitleIndex,
          carriedAudioTrackId ?? null,
          fileSelection ?? "auto",
          forceRelink,
        );

        if (loadSequence !== loadSequenceRef.current) {
          const staleSessionId = decision.playback_plan?.session_id ?? decision.session_id;
          if (staleSessionId) {
            await stopSession(staleSessionId).catch(() => {
              // Best effort cleanup for stale session starts.
            });
          }
          return;
        }

        let decisionToAdopt = decision;
        let initialSubtitleFailure: PlaybackSessionErrorState | null = null;
        const targetVersion = versions.find((v) => v.file_id === selectedFileId);
        // Shared ordinal derivation; `targetSubtitleIndex` is the server's
        // published index, so look it up on the same mapping. See
        // subtitleInventory.ts.
        const targetTracks = buildPublishedSubtitleTracks(targetVersion?.subtitle_tracks);
        const requestedTrack =
          targetSubtitleIndex !== undefined
            ? targetTracks.find((track) => track.index === targetSubtitleIndex)
            : undefined;
        const requestedIsBitmap = requestedTrack?.codec
          ? isBitmapCodec(requestedTrack.codec)
          : false;
        const hasBitmapSubtitle =
          requestedIsBitmap ||
          (carriedSubtitleTrackIndex === undefined &&
            initialBitmapSubtitleTrackIndexByFileId?.[selectedFileId] !== undefined);
        if (!decision.playback_plan && hasBitmapSubtitle) {
          initialSubtitleFailure = describeDecisionWithoutPlan(decision);
          if (decision.session_id) {
            void stopSession(decision.session_id).catch(() => {
              // Best effort cleanup for the refused subtitle-bearing start.
            });
          }

          // A fresh attempt id avoids colliding with start idempotency: the
          // retry intentionally changes the request by dropping the bitmap
          // subtitle that made the first plan impossible.
          const fallbackPlaybackAttemptId = randomUUID();
          playbackAttemptId = fallbackPlaybackAttemptId;
          playbackAttemptIdRef.current = fallbackPlaybackAttemptId;
          decisionToAdopt = await requestStart(
            selectedFileId,
            position,
            forceStartPosition,
            fallbackPlaybackAttemptId,
            undefined,
            carriedAudioTrackId ?? null,
            fileSelection ?? "auto",
            forceRelink,
          );
          if (!decisionToAdopt.playback_plan) {
            initialSubtitleFailure = null;
          }
        }

        if (loadSequence !== loadSequenceRef.current) {
          const staleSessionId =
            decisionToAdopt.playback_plan?.session_id ??
            decisionToAdopt.session_id ??
            decision.playback_plan?.session_id ??
            decision.session_id;
          if (staleSessionId) {
            await stopSession(staleSessionId).catch(() => {
              // Best effort cleanup for a superseded start/replan chain.
            });
          }
          return;
        }

        const adopted = adoptDecision(decisionToAdopt, initialSubtitleFailure);
        if (adopted) {
          // A start opens a new attempt. Its first frame is still to come:
          // whatever the player shows until the new transport loads belongs to
          // the attempt it replaced.
          firstFrameRef.current = { attemptId: playbackAttemptId, intentAt, reported: false };
        }
        if (!adopted && hasExistingSession && allowPreserveExistingSessionOnError) {
          restorePreviousAttempt();
          setState((current) => ({
            ...current,
            loading: false,
            replacing: false,
            replanning: false,
            replanningQuality: false,
            pendingSwitchFileId: null,
            errorTitle: previousState.errorTitle,
            error: previousState.error,
            errorReason: previousState.errorReason,
            errorRetryable: previousState.errorRetryable,
          }));
          return;
        }
        if (!adopted) {
          retirePreviousSession();
          return;
        }
        if (adopted && previousSessionId && previousSessionId !== sessionIdRef.current) {
          void stopSession(previousSessionId).catch(() => {
            // Best effort — stale session will time out server-side.
          });
        }
      } catch (err) {
        if (loadSequence !== loadSequenceRef.current) {
          return;
        }

        if (hasExistingSession && allowPreserveExistingSessionOnError) {
          console.error(replacementErrorMessage, err);
          restorePreviousAttempt();
          setState((current) => ({
            ...current,
            loading: false,
            replacing: false,
            replanning: false,
            replanningQuality: false,
            pendingSwitchFileId: null,
          }));
          return;
        }

        const nextError = describePlaybackSessionError(err, initialErrorMessage);
        retirePreviousSession(nextError);
      } finally {
        endAdoption(loadSequence);
        // A push deferred while this start/replan owned the session has waited
        // long enough: fold it in now that the sequence has no adoption left in
        // flight, so the incoming identity follows the plan that won.
        flushDeferredPushesRef.current();
      }
    },
    [
      adoptDecision,
      beginAdoption,
      endAdoption,
      initialBitmapSubtitleTrackIndexByFileId,
      initialSubtitleTrackIndexByFileId,
      requestStart,
      selectFileId,
      stopSession,
      transitionSourceIdentity,
    ],
  );

  useEffect(() => {
    if (!capabilitiesSettled) return;
    if (activeRequestKeyRef.current === requestKey) {
      return;
    }
    activeRequestKeyRef.current = requestKey;
    activeCapabilityRequestKeyRef.current = capabilityRequestKey;
    qualityRef.current = qualityPreference?.trim() || "auto";
    playbackPositionRef.current = initialPosition;
    const intentAt = takePlaybackIntent(requestKey);
    startIntentRef.current = {
      position: initialPosition,
      forceStartPosition: forceInitialPosition,
      intentAt,
    };
    hasAdoptedPlanRef.current = false;
    awaitingInitialPlayerPositionRef.current = false;
    playbackPlayingRef.current = true;
    playbackStartedRef.current = false;
    inventoryRevisionRef.current = null;
    revisionScopeRef.current.revisions.clear();
    deferredPushesRef.current = [];
    transitionSourceIdentity(null, null);
    // A new request re-derives Auto intent from its own props: without this,
    // the previous request's armed/disarmed state leaks into the new session.
    // Inline (not setAutoFallback: that callback is declared below this
    // effect and block-scoped use would trip TS2448).
    autoFallbackRef.current = allowAlternateVersions && !explicitFileSelection;
    setState((current) =>
      current.autoFallback === autoFallbackRef.current
        ? current
        : { ...current, autoFallback: autoFallbackRef.current },
    );

    void loadSession({
      preferredFileId: fileId,
      position: initialPosition,
      forceStartPosition: forceInitialPosition,
      allowPreserveExistingSessionOnError: false,
      replacementErrorMessage: "Failed to replace playback request",
      initialErrorMessage: "Failed to start playback",
      fileSelection: explicitFileSelection ? "explicit" : "auto",
      forceRelink,
      intentAt,
    });
  }, [
    allowAlternateVersions,
    capabilityRequestKey,
    capabilitiesSettled,
    explicitFileSelection,
    fileId,
    forceInitialPosition,
    forceRelink,
    initialPosition,
    loadSession,
    qualityPreference,
    requestKey,
    transitionSourceIdentity,
  ]);

  // Clean up session on unmount.
  useEffect(() => {
    return () => {
      // A start can finish after unmount, before it has published a session ID.
      // Let that reply take the stale-start path and stop its own session
      // instead of adopting it into an abandoned player.
      loadSequenceRef.current += 1;
      const sid = sessionIdRef.current;
      if (!sid) return;
      // sendBeacon doesn't support DELETE, so the stop uses fetch keepalive.
      void stopSequencedSession(config, sid, true).catch(() => {
        // Best effort: the session expires server-side.
      });
    };
  }, [config]);

  /**
   * Issues one replan and adopts whatever plan comes back.
   *
   * Replans are serialized server-side behind a lease, so a second concurrent
   * request would earn a `409 replan_in_progress`; the in-flight guard here
   * means the client never asks for one.
   */
  const replan = useCallback(
    async function issueReplan(
      options: ReplanOptions,
      retireSessionOnRefusal = false,
    ): Promise<boolean> {
      const plan = planRef.current;
      // Stamp the load sequence with the plan, at the same read. A version
      // switch bumps the sequence before it adopts its replacement plan, so a
      // replan that read the sequence later would pair the outgoing plan with
      // the incoming generation and its stale discard could never fire.
      const loadSequence = loadSequenceRef.current;
      const sessionId = sessionIdRef.current;
      const playbackAttemptId = playbackAttemptIdRef.current;
      if (!plan || !sessionId || !playbackAttemptId) return false;
      // A switch is replacing the whole session and about to retire the plan
      // below. A replan built from that outgoing plan would be stamped with the
      // switch's sequence once the switch lands, so it could clobber the
      // replacement instead of being discarded. Refuse until the switch
      // settles; the switch completion path owns what happens next.
      if (switchingRef.current) return false;
      if (replanInFlightRef.current) {
        const isPendingFailureRecovery =
          options.operation === "failure_recovery" || options.operation === "seek_failure_recovery";
        const isPendingOutputChange = options.operation === "output_change";
        const queuedOperation = pendingReplanRef.current?.options.operation;
        const hasQueuedFailureRecovery =
          queuedOperation === "failure_recovery" || queuedOperation === "seek_failure_recovery";
        const hasQueuedOutputChange = queuedOperation === "output_change";
        if (
          options.operation === "seek_reanchor" &&
          hasQueuedOutputChange &&
          pendingReplanRef.current
        ) {
          // The output refresh will open a fresh route at its position anyway;
          // fold a later seek into that queued intent rather than silently
          // dropping the viewer's newest target.
          pendingReplanRef.current = {
            ...pendingReplanRef.current,
            options: {
              ...pendingReplanRef.current.options,
              positionSeconds: options.positionSeconds,
            },
          };
          return false;
        }
        const isPendingUserIntent =
          options.operation === "track_change" || options.operation === "quality_change";
        const hasQueuedUserIntent =
          queuedOperation === "track_change" || queuedOperation === "quality_change";
        const shouldQueue =
          isPendingFailureRecovery ||
          (isPendingUserIntent && !hasQueuedFailureRecovery) ||
          (isPendingOutputChange && !hasQueuedFailureRecovery && !hasQueuedUserIntent) ||
          (options.operation === "seek_reanchor" &&
            !hasQueuedFailureRecovery &&
            !hasQueuedOutputChange &&
            !hasQueuedUserIntent);
        if (shouldQueue) {
          pendingReplanRef.current?.resolve(false);
          return new Promise<boolean>((resolve) => {
            pendingReplanRef.current = {
              options,
              loadSequence,
              retireSessionOnRefusal,
              resolve,
              planId: plan.plan_id,
              mediaFileId: liveSourceIdentityRef.current.fileId,
            };
          });
        }
        return false;
      }

      const isFailureRecovery =
        options.operation === "failure_recovery" || options.operation === "seek_failure_recovery";
      if (isFailureRecovery && attemptCountRef.current > MAX_ATTEMPT_COUNT_V3) {
        setState((current) => ({
          ...current,
          replanning: false,
          replanningQuality: false,
          errorTitle: "Playback failed",
          error: "Playback failed after repeated recovery attempts.",
          errorReason: null,
          errorRetryable: false,
        }));
        return false;
      }

      // On an intent change nothing failed, so the previous route stays
      // eligible: the loop guard and the recovery counter reset. Only a
      // recovery accumulates them.
      const attemptedPlanKeys = isFailureRecovery
        ? [...attemptedPlanKeysRef.current, plan.plan_attempt_key].slice(
            -MAX_ATTEMPTED_PLAN_KEYS_V3,
          )
        : [];
      const attemptCount = isFailureRecovery ? attemptCountRef.current : 1;

      const body = buildReplanRequestV3({
        ...options,
        extraClientFeatures: VIDEO_CLIENT_FEATURES_V3,
        plan,
        playbackAttemptId,
        replanRequestId: randomUUID(),
        planAttemptId: planAttemptIdRef.current,
        qualityPreference: qualityRef.current,
        attemptedPlanKeys,
        attemptCount,
        metered: detectMeteredV3(),
        // State the viewer's current Auto intent on every replan so the
        // server's session flag cannot drift from the menu: a viewer who
        // started on an explicit pick and later re-armed Auto must still
        // rotate on a dead source.
        autoFallback: autoFallbackRef.current,
        bandwidthEstimateKbps: detectBandwidthEstimateKbpsV3(),
        bandwidthCapKbps: maxBitrateKbps,
        clientCapabilities,
        clientPlaybackContext,
      });

      replanInFlightRef.current = true;
      beginAdoption(loadSequence);
      const isQualityReplan =
        options.operation === "quality_change" || options.operation === "output_change";
      setState((current) => ({
        ...current,
        replanning: true,
        replanningQuality: isQualityReplan,
        errorTitle: null,
        error: null,
        errorReason: null,
        errorRetryable: false,
      }));

      try {
        const decision = await replanV2(config, sessionId, body);

        // A version switch or a fresh start that landed while this was in
        // flight owns the session now; this plan is already superseded.
        if (loadSequence !== loadSequenceRef.current) return false;

        if (isFailureRecovery) {
          attemptedPlanKeysRef.current = attemptedPlanKeys;
          attemptCountRef.current = Math.min(attemptCount + 1, MAX_ATTEMPT_COUNT_V3 + 1);
        } else {
          attemptedPlanKeysRef.current = [];
          attemptCountRef.current = 1;
        }

        // Keep a refused-start subtitle pinned off across unrelated replans.
        // A successful explicit subtitle choice is the one action that clears
        // the marker and lets the new server-selected track take ownership.
        const adopted = adoptDecision(
          decision,
          options.operation === "track_change" && options.subtitle !== undefined ? null : undefined,
          isFailureRecovery,
        );
        if (!adopted && retireSessionOnRefusal) {
          const pending = pendingReplanRef.current;
          const pendingCanValidateOutput =
            pending?.loadSequence === loadSequence &&
            pending.options.operation !== "seek_reanchor" &&
            pending.options.operation !== "seek_failure_recovery";
          if (pending && pendingCanValidateOutput) {
            // The output refusal proves the predecessor route is incompatible.
            // Let a queued planner-backed intent try the latest output evidence,
            // but require it to retire the session too if it is refused. Frozen
            // seek replans cannot validate new output evidence, so they must not
            // postpone retirement.
            pendingReplanRef.current = {
              ...pending,
              retireSessionOnRefusal: true,
            };
          } else {
            retireActiveSession(sessionId);
          }
        }
        return adopted;
      } catch (err) {
        if (loadSequence !== loadSequenceRef.current) return false;
        // A reaped or ended session cannot be replanned, and every retry against
        // it answers the same way. Retire it and rebuild once from the position
        // and selections the dead plan held, so the viewer keeps watching
        // instead of being pinned to a session that no longer exists.
        const deadSession = isDeadPlaybackSessionError(err);
        if (deadSession && recoveredDeadSessionRef.current !== sessionId) {
          recoveredDeadSessionRef.current = sessionId;
          const restartPosition = playbackPositionRef.current;
          // Clear the dead plan/session first: the replacement start must not
          // race a replan built against the session that just died, and the
          // player must stop driving its stream URL.
          retireActiveSession(sessionId);
          void loadSession({
            preferredFileId: plan.requested_media_file_id,
            position: restartPosition,
            // The viewer was mid-playback; resume where the dead session left
            // off rather than at the server's stored resume point.
            forceStartPosition: true,
            allowPreserveExistingSessionOnError: false,
            replacementErrorMessage: "Failed to restart playback",
            initialErrorMessage: "Failed to restart playback",
            carriedAudioTrackId: plan.selected_tracks.audio?.id ?? null,
            carriedSubtitleTrackIndex: plan.selected_tracks.subtitle?.index ?? null,
            // Preserve the viewer's version choice; the server must not
            // silently substitute another edition behind a rebuild. Use the
            // live Auto intent first (a mid-session re-arm never re-records
            // the retry target), then the most recent start's selection.
            fileSelection: autoFallbackRef.current
              ? "auto"
              : (retryTargetRef.current?.fileSelection ??
                (explicitFileSelection ? "explicit" : "auto")),
            intentAt: null,
          });
          return false;
        }
        if (deadSession) {
          // Already rebuilt once for this session. Retire the plan it left
          // behind and fall through to surface the copy rather than starting
          // again in a loop.
          retireActiveSession(sessionId);
        }
        if (retireSessionOnRefusal && !deadSession) {
          console.error("Failed to refresh playback output", err);
          return false;
        }
        const nextError = describePlaybackSessionError(err, "Failed to update playback");
        setState((current) => ({
          ...current,
          replanning: false,
          replanningQuality: false,
          errorTitle: nextError.title,
          error: nextError.message,
          errorReason: nextError.reason ?? null,
          errorRetryable: nextError.retryable ?? false,
        }));
        return false;
      } finally {
        replanInFlightRef.current = false;
        setState((current) =>
          current.replanning || current.replanningQuality
            ? { ...current, replanning: false, replanningQuality: false }
            : current,
        );

        const pendingReplan = pendingReplanRef.current;
        pendingReplanRef.current = null;
        if (pendingReplan?.loadSequence === loadSequenceRef.current) {
          // The queued op was built against the plan its `planId` names and the
          // live source its `mediaFileId` names. When the in-flight replan has
          // replaced that plan, an op may only be replayed if it carries no
          // plan-derived state: a seek target, a quality label and an output
          // refresh are resolved against the live plan, either by this
          // re-dispatch or by the server. A track selection bakes in a
          // plan-derived ordinal, and a failure recovery bakes in the plan to
          // exclude, so both are dropped once the plan identity they name is
          // gone. A rotation moves the effective source without changing the
          // plan id, so the live file id is checked too: the plan-bound ordinal
          // no longer names the same bytes once the transport rebinds.
          const planStillCurrent =
            pendingReplan.planId === planRef.current?.plan_id &&
            pendingReplan.mediaFileId === liveSourceIdentityRef.current.fileId;
          const pendingIsPlanBound =
            pendingReplan.options.operation === "failure_recovery" ||
            pendingReplan.options.operation === "seek_failure_recovery" ||
            pendingReplan.options.audio !== undefined ||
            pendingReplan.options.subtitle !== undefined;
          if (planStillCurrent || !pendingIsPlanBound) {
            // A capability change can queue behind a replan created by an older
            // render. Dispatch through the latest callback so its request carries
            // the current output evidence rather than the closed-over snapshot.
            void issueReplanRef
              .current(pendingReplan.options, pendingReplan.retireSessionOnRefusal)
              .then(pendingReplan.resolve);
          } else {
            pendingReplan.resolve(false);
          }
        } else {
          pendingReplan?.resolve(false);
        }
        // Last: a queued replan dispatched just above has already counted
        // itself in, so waiters are not woken between the two links of a chain.
        endAdoption(loadSequence);
        // Fold any push deferred while this replan owned the session. When a
        // queued replan was just dispatched this is a no-op (the guard sees it
        // in flight) and that link's own settle path flushes instead.
        flushDeferredPushesRef.current();
      }
    },
    [
      adoptDecision,
      beginAdoption,
      clientCapabilities,
      clientPlaybackContext,
      config,
      endAdoption,
      explicitFileSelection,
      loadSession,
      maxBitrateKbps,
      retireActiveSession,
    ],
  );
  issueReplanRef.current = replan;

  useEffect(() => {
    if (!capabilitiesSettled) return;
    if (
      activeRequestKeyRef.current !== requestKey ||
      activeCapabilityRequestKeyRef.current === capabilityRequestKey
    ) {
      return;
    }
    activeCapabilityRequestKeyRef.current = capabilityRequestKey;

    const plan = planRef.current;
    const sessionId = sessionIdRef.current;
    if (!plan || !sessionId) {
      const startIntent = startIntentRef.current;
      const resumeFromAdoptedPlan = hasAdoptedPlanRef.current;
      void loadSession({
        preferredFileId: fileId,
        position: resumeFromAdoptedPlan ? playbackPositionRef.current : startIntent.position,
        forceStartPosition: resumeFromAdoptedPlan ? true : startIntent.forceStartPosition,
        allowPreserveExistingSessionOnError: false,
        replacementErrorMessage: "Failed to refresh playback output",
        initialErrorMessage: "Failed to refresh playback output",
        // Before any plan was adopted, the viewer is still waiting on the
        // Play they pressed, so the replacement start keeps its clock. After
        // one, video has been shown and nobody pressed Play for this start.
        intentAt: resumeFromAdoptedPlan ? null : startIntent.intentAt,
      });
      return;
    }

    if (!serverFeaturesRef.current.includes(FEATURE_OUTPUT_CHANGE_V3)) {
      return;
    }

    void replan(
      {
        operation: "output_change",
        positionSeconds: playbackPositionRef.current,
      },
      true,
    );
  }, [
    capabilityRequestKey,
    capabilitiesSettled,
    fileId,
    loadSession,
    replan,
    requestKey,
    retireActiveSession,
  ]);

  const switchAudioTrack = useCallback(
    (index: number, currentPosition: number) => {
      const plan = planRef.current;
      if (!plan) return;
      // The menu renders the probed inventory `applyAudioInventory` may have
      // replaced, whose ordering a probe repair can change. Carry the plan's
      // canonical identity for the picked track — matched by signature, then
      // by language family when the menu no longer shows the plan's own list —
      // so a raw position cannot name a different language. The client ordinal
      // stays as the request's index fallback when the identity cannot be
      // named.
      const audio = resolvePlanAudioIdentity(
        plan.audio_tracks ?? [],
        stateRef.current.planAudioTracks,
        index,
      );
      const current = plan.selected_tracks.audio;
      // Compare the resolved identity, not the menu position: after a probe
      // repair the already-playing track can sit at a different displayed
      // index, and comparing ordinals would drop the viewer's real pick (or
      // replan a track they already have).
      const isCurrent = audio.id ? current?.id === audio.id : current?.index === audio.index;
      if (isCurrent) return;
      void replan({
        operation: "track_change",
        positionSeconds: currentPosition,
        audio,
      });
    },
    [replan],
  );

  const changeSubtitleTrack = useCallback(
    (combinedIndex: number | null, currentPosition: number) => {
      const plan = planRef.current;
      if (!plan) return;
      void replan({
        operation: "track_change",
        positionSeconds: currentPosition,
        subtitle: combinedIndex == null ? null : { id: "", index: combinedIndex },
      });
    },
    [replan],
  );

  const changeQuality = useCallback(
    (label: string, currentPosition: number) => {
      const normalized = label.trim() || QUALITY_ORIGINAL_V3;
      const previousPreference = qualityRef.current;
      qualityRef.current = normalized;
      setState((current) => ({ ...current, qualityPreference: normalized }));
      void replan({ operation: "quality_change", positionSeconds: currentPosition }).then(
        (adopted) => {
          if (adopted || qualityRef.current !== normalized) return;
          qualityRef.current = previousPreference;
          setState((current) =>
            current.qualityPreference === normalized
              ? { ...current, qualityPreference: previousPreference }
              : current,
          );
        },
      );
    },
    [replan],
  );

  const recoverFromFailure = useCallback(
    (failure: FailureV3, currentPosition: number) => {
      reportEvent("plan_failed", {
        failureClassification: failure.classification,
        ...(failure.message ? { diagnostics: { message: failure.message } } : {}),
      });
      void replan({ operation: "failure_recovery", positionSeconds: currentPosition, failure });
    },
    [replan, reportEvent],
  );

  /**
   * Replans off a plan the server invalidated mid-playback.
   *
   * This is an ordinary `failure_recovery`, deliberately: that operation is
   * what folds the current plan's attempt key into `attempted_plan_keys`, so
   * the route the server just disqualified is excluded from the replacement
   * plan without the client reasoning about deliveries at all. The plan
   * revision the adopted plan bumps rebuilds the transport and restores the
   * position, exactly as it does after a client-detected failure.
   *
   * A start or replan already in flight is waited out first. The server commits
   * a replacement plan and starts the copy-safety scan behind it *before* the
   * response reaches the client, so an invalidation can name a plan this client
   * has not adopted yet. Deciding against the plan currently on screen would
   * complete the command as a no-op and then let the pending response install
   * the very route the server just withdrew.
   */
  const invalidatePlan = useCallback(
    async (planId: string, reason: string, currentPosition: number): Promise<boolean> => {
      const settling = awaitAdoptionSettled();
      if (settling) await settling;
      const plan = planRef.current;
      if (!plan) return false;
      // The command names the plan the server invalidated. Once the client has
      // moved past it there is nothing to recover from, and replanning anyway
      // would evict a route the server never complained about. That stays true
      // for a plan id this client has never seen: it is a verdict for a route
      // that has already been replaced, not a reason to tear the session down.
      if (plan.plan_id !== planId) return true;
      const classification = reason.trim().slice(0, 64) || "plan_invalidated";
      reportEvent("plan_invalidated", { fallbackReason: classification });
      return replan({
        operation: "failure_recovery",
        positionSeconds: currentPosition,
        failure: { classification, message: "The server invalidated this plan." },
      });
    },
    [awaitAdoptionSettled, replan, reportEvent],
  );

  const reanchorSeek = useCallback(
    (positionSeconds: number): Promise<boolean> => {
      playbackPositionRef.current = positionSeconds;
      awaitingInitialPlayerPositionRef.current = false;
      reportEvent("seek_reanchor_requested");
      return replan({ operation: "seek_reanchor", positionSeconds });
    },
    [replan, reportEvent],
  );

  /**
   * Re-reads the subtitle inventory.
   *
   * There is no "refresh" operation in v3, and there does not need to be: a
   * `track_change` that changes nothing returns a fresh plan — inventory
   * included — without excluding the route already playing.
   */
  const refreshSubtitles = useCallback(
    async (currentPosition: number): Promise<boolean> => {
      if (!planRef.current) return false;
      return replan({ operation: "track_change", positionSeconds: currentPosition });
    },
    [replan],
  );

  /**
   * Folds a track the server pushed over the realtime socket into the plan's
   * inventory at the ordinal the server assigned it. The ordinal is never
   * derived client-side, which is why the payload carries the whole entry.
   */
  const applySubtitleTrack = useCallback(
    (track: SubtitleInventoryItemV3) => {
      const plan = planRef.current;
      if (!plan) return;
      const inventory = [
        ...plan.subtitle.inventory.filter((item) => item.combined_index !== track.combined_index),
        track,
      ].sort((a, b) => a.combined_index - b.combined_index);
      const nextPlan: PlanV3 = { ...plan, subtitle: { ...plan.subtitle, inventory } };
      planRef.current = nextPlan;
      setState((current) => ({
        ...current,
        plan: nextPlan,
        subtitleUrls: mapSubtitleInventory(inventory, nextPlan.effective_media_file_id, config),
      }));
    },
    [config],
  );

  /**
   * Fills in a richer probed audio inventory discovered after the plan landed.
   *
   * A catalog list is probe evidence, so it replaces a declared (provisional)
   * inventory even when it is shorter: the declared list may be synthesized or
   * stale, and only positive probe evidence resolves the provisional marker.
   * For a same-file refresh against an already-verified inventory, only a strict
   * superset is accepted — once the plan carries a full inventory it stays
   * authoritative, so a poorer catalog row never overwrites it. When `fileId`
   * names another file (a poll that resolved the effective virtual candidate
   * while the plan names the collapsed row) the inventory belongs to a different
   * target, so it replaces the menu even when it is not larger, and the live
   * identity is re-keyed to that file and resolved virtual URI so the version
   * menu follows the same source the inventory does. The plan object and its
   * revisions are untouched, so menus re-render while the transport keeps
   * playing.
   */
  const applyAudioInventory = useCallback(
    (tracks: PlayerAudioTrack[], fileId?: number | null, effectiveVirtualUri?: string | null) => {
      // Resolve the transition against the synchronous menu mirror, not the
      // rendered state: a poll that lands in the same tick as another push must
      // see the identity that push already committed, or it would misclassify a
      // same-file source as a cross-file rotation.
      const identity = menuIdentityRef.current;
      const sameFile = fileId == null || fileId === identity.fileId;
      const nextFileId = sameFile ? identity.fileId : (fileId ?? identity.fileId);
      // Keep the current URI when the caller names no candidate: an ordinary
      // version switch moves by id alone and must not clear a rotation's
      // effective candidate under it. A virtual poll always names the resolved
      // path, so a moved candidate re-keys here.
      const nextUri =
        effectiveVirtualUri === undefined ? identity.uri : (effectiveVirtualUri ?? null);
      const identityChanged = nextFileId !== identity.fileId || nextUri !== identity.uri;
      // A candidate URI that moved under the same file id is an authoritative
      // identity update even when the catalog list is the same length or empty:
      // the live source changed, so the menu must follow it. Commit the
      // transition synchronously and outside the updater; the revision scope is
      // generation-keyed and is deliberately not cleared here.
      if (identityChanged) {
        transitionSourceIdentity(nextFileId, nextUri);
        // A catalog poll is external state. Record the candidate it moved to,
        // with the transition clock, so a deferred source commit queued before
        // this fold cannot overwrite it even if a later plan adoption replaces
        // the menu mirror before the flush runs.
        recordPollApplied({ fileId: nextFileId, uri: nextUri });
      }
      setState((current) => {
        if (identityChanged) {
          return {
            ...current,
            mediaFileId: nextFileId,
            effectiveVirtualUri: nextUri,
            planAudioTracks: tracks.map((track) => ({ ...track })),
            // An empty list on a moved identity carries no probe evidence, so
            // the menu stays marked rather than claiming a verified empty
            // inventory.
            audioInventoryProvisional: tracks.length === 0,
          };
        }
        // Same identity: only a strictly richer list (or the first probe
        // evidence) is allowed to replace the current menu, computed against
        // the queued state so batched folds compose.
        if (tracks.length === 0) return current;
        if (!current.audioInventoryProvisional && tracks.length <= current.planAudioTracks.length) {
          return current;
        }
        return {
          ...current,
          planAudioTracks: tracks.map((track) => ({ ...track })),
          // Catalog tracks are probe-persisted, so a non-empty fold is the
          // probed evidence the provisional marker was waiting for.
          audioInventoryProvisional: false,
        };
      });
    },
    [recordPollApplied, transitionSourceIdentity],
  );

  const foldCommittedSource = useCallback(
    (
      source: {
        effectiveMediaFileId?: number | null;
        effectiveVirtualUri?: string | null;
        inventoryStatus?: string | null;
      },
      audioTracks: PlayerAudioTrack[],
    ) => {
      const identity = menuIdentityRef.current;
      // Resolve the next identity against the synchronous mirror; a deferred
      // source that folded earlier in the same tick has already moved it.
      const nextFileId = source.effectiveMediaFileId ?? identity.fileId;
      const nextUri = source.effectiveVirtualUri ?? identity.uri;
      const identityChanged = nextFileId !== identity.fileId || nextUri !== identity.uri;
      if (identityChanged) transitionSourceIdentity(nextFileId, nextUri);
      setState((current) => {
        // Only positive probe evidence clears the marker. A declared push after
        // a verified one marks the menu provisional again rather than rendering
        // the (possibly empty) declared list as final.
        const verified = source.inventoryStatus === "verified";
        const provisional =
          source.inventoryStatus == null ? current.audioInventoryProvisional : !verified;
        const richer = audioTracks.length > current.planAudioTracks.length;
        // A verified push is the probe landing: it replaces a declared list even
        // when shorter. A same-source push that is no richer and adds no
        // evidence leaves the menu alone.
        const replaceInventory =
          identityChanged || richer || (verified && current.audioInventoryProvisional);
        if (!identityChanged && !replaceInventory) return current;
        return {
          ...current,
          mediaFileId: nextFileId,
          effectiveVirtualUri: nextUri,
          // A moved effective source replaces the menu wholesale, including
          // with an empty list, so the previous release's tracks are never
          // shown under the new one.
          planAudioTracks: replaceInventory
            ? audioTracks.map((track) => ({ ...track }))
            : current.planAudioTracks,
          audioInventoryProvisional: provisional,
        };
      });
    },
    [transitionSourceIdentity],
  );

  /**
   * Captures a push that arrived while a start/switch/replan owned the session.
   *
   * The entry records the adoption it belongs to — generation, session, the
   * arrival plan, and the *live* outgoing identity — so the flush can tell
   * which adoption it belongs to without reading the outgoing rendered state.
   * The live identity is captured separately because a rotation or poll can
   * move the source after the arrival plan landed, and the plan alone would
   * then name the wrong outgoing candidate.
   */
  const captureDeferredPush = useCallback((input: DeferredPushInput) => {
    const live = liveSourceIdentityRef.current;
    const lastDeferredSource = lastDeferredSourceIdentityRef.current;
    const base: DeferredPushBase = {
      seq: deferredPushSeqRef.current++,
      arrivalTransitionClock: identityTransitionClockRef.current,
      hasIdentity: identityNamesSource(input.identity),
      identity: input.identity,
      plan: planRef.current,
      outgoing: { fileId: live.fileId, uri: live.uri },
      outgoingFromDeferredSource:
        lastDeferredSource != null &&
        lastDeferredSource.fileId === live.fileId &&
        lastDeferredSource.uri === live.uri,
      generation: loadSequenceRef.current,
      sessionId: sessionIdRef.current,
    };
    deferredPushesRef.current.push(
      input.kind === "source"
        ? {
            ...base,
            kind: "source",
            source: input.source,
            audioTracks: input.audioTracks,
          }
        : { ...base, kind: "inventory", payload: input.payload },
    );
    // A deferred source commit still moves where the stream actually is. Record
    // it so a queued plan-bound replan built against the outgoing source sees
    // the rotation and is dropped, even though the rendered state has not moved.
    if (input.kind === "source" && identityNamesSource(input.identity)) {
      liveSourceIdentityRef.current = {
        fileId: input.identity.fileId ?? live.fileId,
        uri: input.identity.uri ?? live.uri,
      };
      lastDeferredSourceIdentityRef.current = liveSourceIdentityRef.current;
    }
  }, []);

  const applyCommittedSource = useCallback(
    (
      source: {
        effectiveMediaFileId?: number | null;
        effectiveVirtualUri?: string | null;
        inventoryStatus?: string | null;
      },
      audioTracks: PlayerAudioTrack[],
      identity?: SourceIdentity,
    ) => {
      // A start/replan/switch is rebuilding the session and its plan is the
      // authority for identity and inventory. A rotation on the outgoing
      // transport must not mutate the menus under the pending adoption, nor
      // move the live identity the chained-switch completion compares against.
      // Defer the push instead of discarding it: for a fast pending/declared
      // start the replacement plan may not name the candidate this push does
      // (the v2 wire omits `effective_virtual_uri`), so this is the only
      // carrier of the incoming source identity. When it actually moved the
      // source, the queued chained-switch position was captured against the
      // outgoing timeline, so discard that position and let the chained switch
      // seek from the live playhead.
      const currentState = stateRef.current;
      const liveIdentity = menuIdentityRef.current;
      if (switchingRef.current || replanInFlightRef.current || currentState.replacing) {
        const movedSource =
          (source.effectiveMediaFileId != null &&
            source.effectiveMediaFileId !== liveIdentity.fileId) ||
          (source.effectiveVirtualUri != null && source.effectiveVirtualUri !== liveIdentity.uri);
        if (movedSource) pendingSwitchPositionRef.current = null;
        captureDeferredPush({
          kind: "source",
          source,
          audioTracks,
          identity: identity ?? {
            fileId: source.effectiveMediaFileId ?? null,
            uri: source.effectiveVirtualUri ?? null,
          },
        });
        return;
      }
      foldCommittedSource(source, audioTracks);
    },
    [captureDeferredPush, foldCommittedSource],
  );

  /**
   * Replaces the plan's subtitle inventory with a server-pushed revision.
   *
   * Unlike {@link applySubtitleTrack}, which folds one ordinal into whatever
   * the plan already carried, this adopts the whole authoritative list the
   * live-inventory push carries, so a revision that drops, reorders, or empties
   * the tracks renders exactly what the server published. Only positive probe
   * evidence (`inventory_status: "verified"`) clears the provisional marker; a
   * declared push after a verified one marks the menu provisional again rather
   * than rendering the (possibly shorter) declared list as final. The plan
   * object and its revisions are untouched, so the transport keeps playing.
   */
  const applySubtitleInventory = useCallback(
    (
      inventory: SubtitleInventoryItemV3[],
      inventoryStatus?: string | null,
      fileId?: number | null,
    ) => {
      const plan = planRef.current;
      if (!plan) return;
      const effectiveFileId = fileId ?? plan.effective_media_file_id;
      const effectiveVirtualUri =
        stateRef.current.effectiveVirtualUri ?? plan.effective_virtual_uri;
      const verified = inventoryStatus === "verified";
      const sameFile = effectiveFileId === plan.effective_media_file_id;
      const provisional =
        inventoryStatus == null ? stateRef.current.subtitleInventoryProvisional : !verified;
      // A verified push is the probe landing: it replaces a declared list even
      // when shorter. A same-source declared push only replaces when it is
      // richer or the menu is still declared.
      const replace =
        !sameFile ||
        verified ||
        inventory.length > plan.subtitle.inventory.length ||
        stateRef.current.subtitleInventoryProvisional;
      const nextPlan: PlanV3 = {
        ...plan,
        effective_media_file_id: effectiveFileId,
        effective_virtual_uri: effectiveVirtualUri,
        subtitle: { ...plan.subtitle, inventory },
      };
      if (replace) planRef.current = nextPlan;
      setState((current) => {
        if (!replace) {
          return provisional === current.subtitleInventoryProvisional
            ? current
            : { ...current, subtitleInventoryProvisional: provisional };
        }
        return {
          ...current,
          plan: nextPlan,
          subtitleUrls: mapSubtitleInventory(inventory, effectiveFileId, config),
          subtitleInventoryProvisional: provisional,
        };
      });
    },
    [config],
  );

  /**
   * The shared inventory fold used by both a direct push and the deferred
   * flush.
   *
   * Callers have already cleared the switch/replan/source guards, so this only
   * performs the revision gate — a duplicate or out-of-order revision names one
   * already folded in and is a documented no-op — then folds the audio list
   * (with the identity it carries) and the complete subtitle inventory without
   * bumping the plan or transport revisions.
   */
  const foldInventoryUpdate = useCallback(
    (payload: PlaybackInventoryUpdatedPayload) => {
      const scope = syncRevisionScope();
      if (payload.inventory_revision != null && scope.revisions.has(payload.inventory_revision)) {
        return;
      }
      const hasIdentity =
        payload.effective_media_file_id != null || payload.effective_virtual_uri != null;
      if (payload.audio_tracks !== undefined || hasIdentity) {
        foldCommittedSource(
          {
            effectiveMediaFileId: payload.effective_media_file_id ?? null,
            effectiveVirtualUri: payload.effective_virtual_uri ?? null,
            inventoryStatus: payload.inventory_status ?? null,
          },
          payload.audio_tracks ?? [],
        );
      }
      if (payload.subtitle_inventory !== undefined) {
        applySubtitleInventory(
          payload.subtitle_inventory,
          payload.inventory_status ?? null,
          payload.effective_media_file_id ?? null,
        );
      }
      if (payload.inventory_revision != null) {
        inventoryRevisionRef.current = payload.inventory_revision;
        scope.revisions.add(payload.inventory_revision);
      }
    },
    [applySubtitleInventory, foldCommittedSource, syncRevisionScope],
  );

  /**
   * Folds a live inventory revision pushed over the realtime socket into the
   * session's menus.
   *
   * A `source_committed` push re-keys the version and audio menus; this is the
   * same fold with probe evidence attached: identity and audio, then the
   * complete subtitle inventory. It shares the replacement-start guard with
   * {@link applyCommittedSource} — a pending switch owns the menus — and neither
   * bumps the plan or transport revision.
   */
  const applyInventoryUpdate = useCallback(
    (payload: PlaybackInventoryUpdatedPayload) => {
      const identity: SourceIdentity = {
        fileId: payload.effective_media_file_id ?? null,
        uri: payload.effective_virtual_uri ?? null,
      };
      const current = stateRef.current;
      const liveIdentity = menuIdentityRef.current;
      const movedSource =
        (payload.effective_media_file_id != null &&
          payload.effective_media_file_id !== liveIdentity.fileId) ||
        (payload.effective_virtual_uri != null &&
          payload.effective_virtual_uri !== liveIdentity.uri);
      // The same adoption barrier as applyCommittedSource: a switch, an
      // in-flight replan, or a replacement start owns the session, so hold the
      // push and replay it against the plan that wins. Both event kinds must
      // observe one barrier, otherwise an inventory deferred by a replan can
      // flush before a newer source commit that folded immediately.
      if (switchingRef.current || replanInFlightRef.current || current.replacing) {
        if (movedSource) pendingSwitchPositionRef.current = null;
        captureDeferredPush({ kind: "inventory", payload, identity });
        return;
      }
      // No adoption in flight: a push that names another source than the player
      // is on is a stale delivery and is dropped. The synchronous mirror, not
      // the rendered state, is the live identity a rotation already committed.
      if (
        (payload.effective_media_file_id != null &&
          liveIdentity.fileId != null &&
          payload.effective_media_file_id !== liveIdentity.fileId) ||
        (payload.effective_virtual_uri != null &&
          liveIdentity.uri != null &&
          payload.effective_virtual_uri !== liveIdentity.uri)
      ) {
        return;
      }
      // A file-only push cannot be tied to a candidate. When the live source
      // already names a concrete candidate on that same file, the file alone is
      // not proof of ownership and must not re-key the menus under that
      // candidate. This mirrors `deferredIdentityIsAdmissible` so a refused
      // file-only revision cannot slip back in through the direct path.
      if (
        payload.effective_media_file_id != null &&
        payload.effective_virtual_uri == null &&
        liveIdentity.uri != null &&
        payload.effective_media_file_id === liveIdentity.fileId
      ) {
        return;
      }
      foldInventoryUpdate(payload);
    },
    [captureDeferredPush, foldInventoryUpdate],
  );

  /**
   * Applies identity-carrying pushes that were deferred while a
   * start/switch/replan owned the session.
   *
   * Called from each operation's settle path and from an interval fallback, so
   * a push dropped during a fast pending/declared start is folded in the moment
   * the replacement plan lands rather than being lost. The queue is replayed in
   * arrival order. The settled plan is the adoption that won: a push captured
   * under another adoption generation or session is dropped, and a push whose
   * identity the plan cannot vouch for is dropped. A refused inventory revision
   * is left unrecorded, so a redelivery that the plan (or a later rotation) does
   * vouch for still folds; a genuinely applied revision is recorded by the fold
   * itself, so duplicate deliveries remain no-ops.
   */
  const flushDeferredPushes = useCallback(() => {
    // A start/replan owns the session while its adoption is in flight, and a
    // version switch sets `switchingRef` before it retires the outgoing plan.
    // The rendered `replacing` flag is stale at a settle path (it is set through
    // state), so the in-flight adoption count is the reliable signal here.
    const adoptionInFlight = adoptionsInFlightRef.current.has(loadSequenceRef.current);
    if (switchingRef.current || replanInFlightRef.current || adoptionInFlight) {
      return;
    }
    const plan = planRef.current;
    if (!plan) {
      deferredPushesRef.current = [];
      return;
    }
    const queue = deferredPushesRef.current.slice().sort((a, b) => a.seq - b.seq);
    if (queue.length === 0) return;
    deferredPushesRef.current = [];
    // The identity the menus carry when the flush starts. It captures movements
    // an outside push made while these entries waited (a poll folds without the
    // adoption barrier); it is snapshotted once so the entries replayed below do
    // not become a veto baseline for each other, which would invert arrival
    // order when two concrete candidates are queued.
    const appliedIdentity = menuIdentityRef.current;
    const handleRefusedPush = (entry: DeferredPush) => {
      // A deferred source commit had moved the live baseline when it was
      // captured. If nothing later moved it on, put it back on the winning plan
      // so a source the flush just refused is not left recorded as live.
      //
      // A refused inventory revision is deliberately *not* recorded as folded:
      // the refusal is a statement about this flush's settled plan, not about
      // the revision, and the identity can move later in the same generation
      // (a rotation onto a candidate the revision names). Recording it here
      // would swallow the redelivery that finally matches, leaving the menus
      // stale for the rest of the session. Only a genuinely applied revision is
      // recorded, by `foldInventoryUpdate`, so duplicates of applied revisions
      // stay suppressed while a refused one can still apply once it matches.
      if (
        entry.kind === "source" &&
        identityNamesSameSource(liveSourceIdentityRef.current, entry.identity)
      ) {
        liveSourceIdentityRef.current = {
          fileId: plan.effective_media_file_id,
          uri: plan.effective_virtual_uri ?? null,
        };
        lastDeferredSourceIdentityRef.current = null;
      }
    };
    for (const entry of queue) {
      // Only the adoption that won may be mutated. The rendered state still
      // describes the outgoing source here, so it is deliberately not consulted.
      if (entry.generation !== loadSequenceRef.current) continue;
      // A session change is normally foreign, but the replacement this entry
      // was queued behind adopts a new session id; an identity-carrying entry
      // captured across exactly that adoption is allowed through to the
      // identity checks below. An identity-less entry cannot be tied to any
      // adoption and is dropped with the outgoing session.
      if (
        entry.sessionId != null &&
        entry.sessionId !== sessionIdRef.current &&
        !(entry.plan != null && entry.hasIdentity)
      ) {
        continue;
      }
      // The settled plan is the primary authority. When it names a candidate
      // URI, that URI wins over the outgoing one (a replacement may retain the
      // effective candidate) and an exact match is kept. When it is URI-less,
      // the push's own file is the key, except for a same-file source the plan
      // cannot resolve. `appliedIdentity` is the menu identity at flush start, so
      // a push deferred before a poll folded a concrete candidate cannot
      // overwrite it. A URI-bearing source commit is judged by arrival order, but
      // only against state older than it: a poll that folded a candidate after
      // this entry was queued is external and newer and still vetoes the commit.
      // See deferredIdentityIsAdmissible.
      const pollApplied = lastPollAppliedRef.current;
      const appliedPollNewer =
        pollApplied != null && pollApplied.clock > entry.arrivalTransitionClock
          ? pollApplied.identity
          : null;
      if (
        !deferredIdentityIsAdmissible(
          entry.identity,
          plan,
          entry.plan,
          entry.outgoing,
          appliedIdentity,
          entry.kind === "source",
          appliedPollNewer,
          entry.kind === "source" && entry.outgoingFromDeferredSource,
        )
      ) {
        handleRefusedPush(entry);
        continue;
      }
      if (entry.kind === "source") {
        foldCommittedSource(entry.source, entry.audioTracks);
      } else {
        foldInventoryUpdate(entry.payload);
      }
    }
  }, [foldCommittedSource, foldInventoryUpdate]);
  flushDeferredPushesRef.current = flushDeferredPushes;

  const updatePlaybackState = useCallback((positionSeconds: number, playing: boolean) => {
    if (Number.isFinite(positionSeconds) && positionSeconds >= 0) {
      const isUninitializedPlayerZero =
        awaitingInitialPlayerPositionRef.current &&
        !playing &&
        positionSeconds === 0 &&
        playbackPositionRef.current > 0;
      if (!isUninitializedPlayerZero) {
        playbackPositionRef.current = positionSeconds;
        awaitingInitialPlayerPositionRef.current = false;
      }
    }
    if (playing) {
      playbackStartedRef.current = true;
      playbackPlayingRef.current = true;
    } else if (playbackStartedRef.current) {
      playbackPlayingRef.current = false;
    }
  }, []);

  const reportFirstFrame = useCallback(() => {
    const attempt = firstFrameRef.current;
    // Until a start's plan is adopted, the attempt id has already moved on and
    // the frame on screen belongs to the attempt being replaced.
    if (!attempt || attempt.reported || attempt.attemptId !== playbackAttemptIdRef.current) return;
    attempt.reported = true;
    reportEvent(
      "first_frame",
      attempt.intentAt === null
        ? undefined
        : { diagnostics: { first_frame_ms: Math.round(performance.now() - attempt.intentAt) } },
    );
  }, [reportEvent]);

  const setAutoFallback = useCallback((enabled: boolean) => {
    autoFallbackRef.current = enabled;
    setState((current) =>
      current.autoFallback === enabled ? current : { ...current, autoFallback: enabled },
    );
  }, []);

  const switchVersion = useCallback(
    (newFileId: number, currentPosition: number) => {
      if (!allowAlternateVersions) return;
      if (newFileId === stateRef.current.mediaFileId) return;
      // The viewer picked a concrete version, so Auto is no longer armed: the
      // server must not silently move off the chosen release.
      setAutoFallback(false);
      if (switchingRef.current) {
        // A switch is already in flight. Remember the newest target and the
        // live position it was clicked at; the completion handler starts the
        // switch to it once the current one settles (latest-wins, mirroring the
        // replan queue pattern).
        pendingSwitchFileIdRef.current = newFileId;
        pendingSwitchPositionRef.current = currentPosition;
        setState((current) => ({ ...current, pendingSwitchFileId: newFileId }));
        return;
      }
      switchingRef.current = true;
      setState((current) => ({ ...current, pendingSwitchFileId: newFileId }));
      // The viewer picked the version in the player: the new attempt's first
      // frame is timed from here.
      const intentAt = performance.now();

      (async () => {
        try {
          await loadSession({
            preferredFileId: newFileId,
            position: currentPosition,
            forceStartPosition: true,
            // The server retires the old session before it attempts a
            // replacement start. A failed response therefore cannot leave the
            // old plan active on the client.
            allowPreserveExistingSessionOnError: false,
            replacementErrorMessage: "Failed to switch playback version",
            initialErrorMessage: "Failed to switch version",
            // Carry the current audio selection across the version switch: the
            // server remaps the file-bound identity by track family onto the
            // new file instead of dropping it and auto-picking.
            carriedAudioTrackId: planRef.current?.selected_tracks.audio?.id ?? null,
            carriedSubtitleTrackIndex: resolveCarriedSubtitleIndexAcrossVersions(
              planRef.current,
              newFileId,
              versions,
            ),
            // A version switch is always an explicit user action: the server
            // must not silently substitute yet another version.
            fileSelection: "explicit",
            intentAt,
          });
        } finally {
          switchingRef.current = false;
          // A source push that landed while this switch owned the session was
          // held back; now that the replacement plan has settled, fold it in if
          // it still names the file that won.
          flushDeferredPushesRef.current();
          const latest = pendingSwitchFileIdRef.current;
          const latestPosition = pendingSwitchPositionRef.current;
          pendingSwitchFileIdRef.current = null;
          pendingSwitchPositionRef.current = null;
          if (latest !== null && latest !== stateRef.current.mediaFileId) {
            // A rotation during the switch clears the queued position (it was
            // captured against the outgoing timeline), so fall back to the live
            // playhead rather than the first click's stale closure position.
            switchVersion(latest, latestPosition ?? playbackPositionRef.current);
          }
        }
      })();
    },
    [allowAlternateVersions, loadSession, setAutoFallback],
  );

  /**
   * Re-issues the start that was refused with a retryable terminal.
   *
   * The retry differs from the original in one way: `force_relink` asks the
   * server to re-query the provider for the virtual source instead of replaying
   * the candidate it already had. A fresh attempt id is minted by `loadSession`
   * so the server cannot answer the retry from start idempotency. Double-taps
   * are ignored while a retry is in flight; a retry also refuses to disturb a
   * plan that is already playing.
   */
  const retryStart = useCallback(() => {
    if (retryingRef.current) return;
    if (planRef.current) return;
    const target = retryTargetRef.current;
    if (!target) return;

    retryingRef.current = true;
    setState((current) => ({
      ...current,
      retrying: true,
      errorTitle: null,
      error: null,
      errorReason: null,
      errorRetryable: false,
    }));

    void loadSession({
      preferredFileId: target.fileId,
      position: target.position,
      forceStartPosition: target.forceStartPosition,
      allowPreserveExistingSessionOnError: false,
      replacementErrorMessage: "Failed to retry playback",
      initialErrorMessage: "Failed to retry playback",
      carriedAudioTrackId: target.carriedAudioTrackId,
      // When Auto is armed the retry re-selects automatically, so the server
      // may walk alternate versions instead of replaying the dead pin.
      fileSelection: autoFallbackRef.current ? "auto" : target.fileSelection,
      forceRelink: true,
      intentAt: performance.now(),
    }).finally(() => {
      retryingRef.current = false;
      setState((current) => (current.retrying ? { ...current, retrying: false } : current));
    });
  }, [loadSession]);

  /**
   * The version menu's Auto entry. Arms automatic fallback and, when the
   * current source is already terminal (no active plan), recovers immediately
   * with an auto re-resolve. A healthy plan is left playing: Auto only ever
   * fires on a dead/unplayable source.
   */
  const selectAutoVersion = useCallback(() => {
    if (!allowAlternateVersions) return;
    setAutoFallback(true);
    if (!planRef.current) {
      retryStart();
    }
  }, [allowAlternateVersions, retryStart, setAutoFallback]);

  return {
    ...state,
    switchVersion,
    selectAutoVersion,
    retryStart,
    switchAudioTrack,
    changeSubtitleTrack,
    changeQuality,
    recoverFromFailure,
    invalidatePlan,
    reanchorSeek,
    refreshSubtitles,
    applySubtitleTrack,
    applyAudioInventory,
    applyCommittedSource,
    applyInventoryUpdate,
    updatePlaybackState,
    reportFirstFrame,
    reportEvent,
  };
}

/**
 * Resolves the default file for the variant the viewer is resuming.
 *
 * This is edition/part selection, not adaptation: it answers "which cut of this
 * item" from client-held resume state. Which encode of that cut plays is the
 * server's decision.
 */
function selectDefaultVariantFile(
  playbackVariants: PlayerPlaybackVariant[],
  versions: PlayerFileVersion[],
  resumeHints?: ResumeHints,
): number | null {
  if (playbackVariants.length === 0) {
    return null;
  }

  let candidateVariants = playbackVariants;
  if (
    resumeHints?.lastEditionKey &&
    playbackVariants.some((variant) => variant.edition_key === resumeHints.lastEditionKey)
  ) {
    candidateVariants = playbackVariants.filter(
      (variant) => variant.edition_key === resumeHints.lastEditionKey,
    );
  } else if (playbackVariants.some((variant) => !variant.edition_key)) {
    candidateVariants = playbackVariants.filter((variant) => !variant.edition_key);
  }

  for (const variant of candidateVariants) {
    const firstPart = [...(variant.parts ?? [])].sort((a, b) => a.part_index - b.part_index)[0];
    if (!firstPart) {
      continue;
    }

    if (firstPart.default_file_id != null) {
      const known = versions.find((version) => version.file_id === firstPart.default_file_id);
      if (known) return known.file_id;
    }

    const firstVersion = (firstPart.versions ?? [])[0];
    if (firstVersion) return firstVersion.file_id;
  }

  return null;
}
