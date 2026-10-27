import type { PlayerSubtitleInfo } from "../types";

function hasPlayableUrl(track: PlayerSubtitleInfo): boolean {
  return track.url.trim().length > 0;
}

/**
 * A track from the plan's inventory is selectable when the client can fetch it
 * *or* when the server publishes it as `burn_in_only` — the latter has no URL
 * on purpose, because selecting it asks the server to composite it into the
 * video instead of handing over a sidecar.
 */
function isSelectableSessionTrack(track: PlayerSubtitleInfo): boolean {
  return hasPlayableUrl(track) || track.burn_in_only === true;
}

/**
 * Whether the plan's own inventory offers anything the menu can render. A
 * published-but-unselectable entry (no URL and not `burn_in_only`) does not
 * count, so callers that gate on "the menu is still empty" keep looking for
 * the probe's real inventory instead of treating the placeholder as complete.
 */
export function hasSelectableSessionSubtitles(sessionTracks: PlayerSubtitleInfo[]): boolean {
  return sessionTracks.some(isSelectableSessionTrack);
}

export function resolvePlayableSubtitles(
  sessionTracks: PlayerSubtitleInfo[],
  fallbackTracks: PlayerSubtitleInfo[],
): PlayerSubtitleInfo[] {
  const selectableSessionTracks = sessionTracks.filter(isSelectableSessionTrack);
  if (selectableSessionTracks.length > 0) {
    return selectableSessionTracks;
  }
  if (sessionTracks.length === 0) {
    // The plan published no inventory at all (deferred probe, or an effective
    // candidate the plan has not learned yet). The watch detail is the only
    // session evidence available; list it so the overlay menu is useful, and
    // let the bounded catalog poll repair the plan with real URLs. A track
    // without a URL is still selectable here: the server mints the sidecar when
    // the track change replans.
    return fallbackTracks;
  }
  // A non-empty plan list that is merely unselectable is a declared
  // placeholder, not evidence the file has tracks. The watch-detail fallback
  // carries no delivery information, so only rows with a URL are playable.
  return fallbackTracks.filter(hasPlayableUrl);
}

/**
 * The stable identity fields of one subtitle track, independent of the dense
 * combined ordinal the server assigns it. A replan that re-mints the inventory
 * (or the server resolving the selection to a different ordinal for the same
 * asset) changes `index` but leaves these fields alone.
 *
 * When `track_id` is absent the descriptor tuple alone can name several real,
 * distinct tracks (a file with two embedded `eng|srt|forced` streams, say).
 * `source` plus the sidecar artifact identity (`streamIndex` from the embedded
 * container index, or `artifactId` from the external path key / downloaded row
 * id) keep those tracks apart instead of conflating them.
 */
export interface SubtitleTrackIdentity {
  /** Combined ordinal the client echoes back on a track change. */
  index: number | null;
  /** Server-assigned asset identity (`track_id`). */
  trackId?: string | null;
  language?: string | null;
  codec?: string | null;
  forced?: boolean;
  hearingImpaired?: boolean;
  /** True when the server can only deliver this track by burning it in. */
  burnInOnly?: boolean;
  /** Delivery source (`embedded` | `external` | `downloaded`). */
  source?: string | null;
  /** Embedded container stream index, when the sidecar URL publishes one. */
  streamIndex?: number | null;
  /** Sidecar path/artifact identity (external key or downloaded row id). */
  artifactId?: string | null;
}

function normalizeIdentityValue(value: string | null | undefined): string {
  return (value ?? "").trim().toLowerCase();
}

function subtitleDescriptorKey(identity: SubtitleTrackIdentity): string {
  const parts = [
    normalizeIdentityValue(identity.language),
    normalizeIdentityValue(identity.codec),
    identity.forced ? "forced" : "",
    identity.hearingImpaired ? "hearing_impaired" : "",
  ];
  // Append the artifact discriminators only when any is present, so an identity
  // that predates them (a synthesized or older-plan track) keeps the historical
  // descriptor-only key and still dedupes across a re-mint.
  const source = normalizeIdentityValue(identity.source);
  const streamIndex = identity.streamIndex != null ? String(identity.streamIndex) : "";
  const artifactId = normalizeIdentityValue(identity.artifactId);
  if (source || streamIndex || artifactId) {
    parts.push(source, streamIndex, artifactId);
  }
  return parts.join("|");
}

const EMBEDDED_SUBTITLE_STREAM_INDEX_PARAM = "embedded_stream_index";
const EXTERNAL_SUBTITLE_KEY_PARAM = "external_subtitle_key";
const DOWNLOADED_SUBTITLE_ID_PARAM = "downloaded_subtitle_id";

/**
 * Extracts the stable sidecar artifact discriminator from a track URL. The
 * server binds external sidecars to a hashed path key, embedded tracks to the
 * container stream index, and downloaded rows to their row id; these survive an
 * inventory re-mint that changes the combined ordinal. Session ids, combined
 * ordinals and access tokens in the URL are deliberately ignored.
 */
export function subtitleArtifactIdentity(url: string | null | undefined): {
  streamIndex: number | null;
  artifactId: string | null;
} {
  if (!url) return { streamIndex: null, artifactId: null };
  let parsed: URL;
  try {
    parsed = new URL(url, "http://silo.local");
  } catch {
    return { streamIndex: null, artifactId: null };
  }
  const rawStreamIndex = parsed.searchParams.get(EMBEDDED_SUBTITLE_STREAM_INDEX_PARAM);
  const streamIndex =
    rawStreamIndex !== null && /^\d+$/.test(rawStreamIndex) ? Number(rawStreamIndex) : null;
  const externalKey = parsed.searchParams.get(EXTERNAL_SUBTITLE_KEY_PARAM);
  const downloadedId = parsed.searchParams.get(DOWNLOADED_SUBTITLE_ID_PARAM);
  const artifactId = externalKey ?? (downloadedId ? `downloaded:${downloadedId}` : null);
  return { streamIndex, artifactId };
}

/**
 * Builds the stable identity for a selectable track from its player shape,
 * populating the discriminator fields so `track_id`-less inventories cannot
 * conflate distinct assets.
 */
export function subtitleTrackIdentityFromInfo(
  track: Pick<
    PlayerSubtitleInfo,
    | "index"
    | "track_id"
    | "language"
    | "codec"
    | "forced"
    | "hearing_impaired"
    | "burn_in_only"
    | "source"
    | "url"
    | "font_bundle_url"
  >,
): SubtitleTrackIdentity {
  const { streamIndex, artifactId } = subtitleArtifactIdentity(
    track.url || track.font_bundle_url || null,
  );
  return {
    index: track.index,
    trackId: track.track_id ?? null,
    language: track.language ?? null,
    codec: track.codec ?? null,
    forced: track.forced,
    hearingImpaired: track.hearing_impaired,
    burnInOnly: track.burn_in_only === true,
    source: track.source ?? null,
    streamIndex,
    artifactId,
  };
}

/**
 * Stable key for a logical subtitle track, for deduping a pending selection
 * across plans. The server's `track_id` is the authoritative asset identity
 * when present; the descriptor tuple is the fallback for rows that do not
 * carry one.
 */
export function subtitleTrackIdentityKey(identity: SubtitleTrackIdentity): string {
  const trackId = normalizeIdentityValue(identity.trackId);
  if (trackId) return `id:${trackId}`;
  return `desc:${subtitleDescriptorKey(identity)}`;
}

/**
 * True when two descriptors name the same logical subtitle asset. Comparing
 * `track_id` first keeps a selection settled across a re-minted or reordered
 * inventory where the combined ordinal changed but the asset did not.
 */
export function isSameSubtitleTrack(a: SubtitleTrackIdentity, b: SubtitleTrackIdentity): boolean {
  const aTrackId = normalizeIdentityValue(a.trackId);
  const bTrackId = normalizeIdentityValue(b.trackId);
  if (aTrackId && bTrackId) return aTrackId === bTrackId;
  return subtitleDescriptorKey(a) === subtitleDescriptorKey(b);
}

/**
 * Whether the identity carries any descriptor evidence at all. An identity
 * built only from a committed ordinal and a re-mintable id cannot name a row
 * against a different inventory, so it must not fall through to a match on
 * empty descriptor fields.
 */
function hasSubtitleDescriptor(identity: SubtitleTrackIdentity): boolean {
  return (
    normalizeIdentityValue(identity.language) !== "" ||
    normalizeIdentityValue(identity.codec) !== "" ||
    identity.forced === true ||
    identity.hearingImpaired === true ||
    normalizeIdentityValue(identity.source) !== "" ||
    identity.streamIndex != null ||
    normalizeIdentityValue(identity.artifactId) !== ""
  );
}

/**
 * Maps the committed subtitle selection onto the inventory the menu currently
 * renders.
 *
 * A committed ordinal names a row in the plan's own inventory. A replacement
 * plan can re-mint the ordinals, and adoption can preserve the verified rows
 * while the incoming plan publishes a different order, so stamping the plan
 * ordinal onto the displayed rows can light (and request) another language.
 * The asset identity is the authority: a direct `track_id` lookup is only
 * accepted when the descriptor agrees, and a descriptor is only accepted when
 * it names exactly one displayed row. `null` means no unambiguous row matches,
 * which must hold outbound selection rather than guess.
 */
export function resolveAppliedSubtitleIndex(
  displayed: readonly SubtitleTrackIdentity[],
  selected: SubtitleTrackIdentity | null | undefined,
): number | null {
  if (!selected || !hasSubtitleDescriptor(selected)) return null;
  const selectedId = normalizeIdentityValue(selected.trackId);
  let idMatch: number | null = null;
  let idMatches = 0;
  let descriptorMatch: number | null = null;
  let descriptorMatches = 0;
  for (let index = 0; index < displayed.length; index += 1) {
    const candidate = displayed[index]!;
    const sameDescriptor = subtitleDescriptorKey(candidate) === subtitleDescriptorKey(selected);
    if (!sameDescriptor) continue;
    descriptorMatches += 1;
    descriptorMatch = index;
    if (selectedId && normalizeIdentityValue(candidate.trackId) === selectedId) {
      idMatches += 1;
      idMatch = index;
    }
  }
  if (idMatches === 1) return displayed[idMatch!]!.index;
  if (descriptorMatches === 1) return displayed[descriptorMatch!]!.index;
  return null;
}

/**
 * Returns the selection that must be sent to settle the plan's authoritative
 * selected_tracks state, or undefined when the current plan already matches
 * the UI. Sidecar selections are included: the server owns durable track
 * intent even when the browser renders the selected artifact itself.
 *
 * The comparison is by stable track identity, not by ordinal: a replan that
 * re-mints the inventory (or resolves the same asset to a different combined
 * index) must not restart the request. A plan that resolved the selection to
 * `off`/absent is treated as settled by the caller's per-identity request
 * guard, so one user action produces at most one request until the plan
 * acknowledges it.
 */
export function pendingServerSubtitleSelection(
  planSelected: SubtitleTrackIdentity | null,
  active: SubtitleTrackIdentity | null,
): number | null | undefined {
  // Nothing selected in the UI. Asking the server to clear a selection it does
  // not have would replan on every plan; only an outstanding server selection
  // needs an explicit `null`.
  if (active === null) {
    if (planSelected === null || planSelected.index === null) return undefined;
    return null;
  }

  // The plan already carries the requested logical track. Its ordinal may
  // differ from the UI index after a re-mint or reorder; that is not a reason
  // to replan again.
  if (planSelected !== null && isSameSubtitleTrack(planSelected, active)) {
    return undefined;
  }

  // The plan does not carry this asset yet: request it at its current ordinal.
  // For a bitmap `burn_in_only` track this is the only way to ask the server
  // to composite it; for a text sidecar the caller's identity guard keeps this
  // to a single request until the plan acknowledges the selection.
  return active.index;
}
