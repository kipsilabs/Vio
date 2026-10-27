import { canonicalLanguageTag } from "@/lib/languageTags";
import type { TrackIdentityV3 } from "../protocol-v3";

/**
 * The audio fields shared by the plan's inventory (`AudioTrackV3`) and the
 * menu's track shape (`PlayerAudioTrack`). Both describe the same probed
 * streams; the plan's list is the server's authoritative inventory, and the
 * catalog row the poll resolved is what a probe repair can rewrite.
 */
export interface AudioInventoryTrack {
  language?: string;
  languages?: string[];
  title?: string;
  embedded_title?: string;
  codec?: string;
  layout?: string;
  channels?: number;
  /** Canonical per-track identity (`file:<id>:audio:<selection_index>`). */
  track_id?: string;
  /** The plan ordinal this track is selected by (`selected_tracks.audio.index`). */
  selection_index?: number;
}

function normalize(value: string | undefined | null): string {
  return (value ?? "").trim().toLowerCase();
}

/**
 * The concrete languages a track names, canonicalized and sorted so two
 * spellings of the same language set compare equal. Undetermined/multiple
 * placeholders carry no language intent and are dropped.
 */
function languageSet(track: AudioInventoryTrack): string[] {
  const raw = track.languages?.length ? track.languages : track.language ? [track.language] : [];
  const set = new Set<string>();
  for (const code of raw) {
    const canonical = canonicalLanguageTag(code);
    if (!canonical || canonical === "und" || canonical === "mul") continue;
    set.add(canonical);
  }
  return [...set].sort();
}

function sameLanguages(a: AudioInventoryTrack, b: AudioInventoryTrack): boolean {
  const left = languageSet(a);
  const right = languageSet(b);
  if (left.length === 0 || left.length !== right.length) return false;
  return left.every((code, index) => code === right[index]);
}

function sameSignature(a: AudioInventoryTrack, b: AudioInventoryTrack): boolean {
  return (
    normalize(a.codec) === normalize(b.codec) &&
    normalize(a.layout) === normalize(b.layout) &&
    (a.channels ?? 0) === (b.channels ?? 0) &&
    normalize(a.title) === normalize(b.title) &&
    normalize(a.embedded_title) === normalize(b.embedded_title) &&
    sameLanguages(a, b)
  );
}

/** Raw language tags, order-insensitive, keeping und/mul that `languageSet` drops. */
function sameRawLanguages(a: AudioInventoryTrack, b: AudioInventoryTrack): boolean {
  const raw = (track: AudioInventoryTrack) =>
    (track.languages?.length ? track.languages : track.language ? [track.language] : [])
      .map(normalize)
      .filter(Boolean)
      .sort();
  const left = raw(a);
  const right = raw(b);
  return left.length === right.length && left.every((code, index) => code === right[index]);
}

/**
 * Compares two rows that already share an id. Unlike `sameSignature`, this
 * keeps undetermined/multiple language tags: those carry no matching intent for
 * a pick, but they are still identity evidence for a known id pair.
 */
function sameTrackDescriptor(a: AudioInventoryTrack, b: AudioInventoryTrack): boolean {
  return (
    normalize(a.codec) === normalize(b.codec) &&
    normalize(a.layout) === normalize(b.layout) &&
    (a.channels ?? 0) === (b.channels ?? 0) &&
    normalize(a.title) === normalize(b.title) &&
    normalize(a.embedded_title) === normalize(b.embedded_title) &&
    sameRawLanguages(a, b)
  );
}

/** The sole index matching `predicate`, or null when none or several do. */
function uniqueMatch<T>(items: readonly T[], predicate: (item: T) => boolean): number | null {
  let found: number | null = null;
  for (let index = 0; index < items.length; index += 1) {
    if (!predicate(items[index]!)) continue;
    if (found !== null) return null;
    found = index;
  }
  return found;
}

function matchPlanTrack(
  planTracks: readonly AudioInventoryTrack[],
  picked: AudioInventoryTrack | undefined,
): AudioInventoryTrack | undefined {
  if (!picked) return undefined;
  const bySignature = uniqueMatch(planTracks, (track) => sameSignature(track, picked));
  if (bySignature !== null) return planTracks[bySignature];
  const byLanguage = uniqueMatch(planTracks, (track) => sameLanguages(track, picked));
  if (byLanguage !== null) return planTracks[byLanguage];
  return undefined;
}

/**
 * The identity a plan track must be selected by: its stable `track_id` and the
 * ordinal the server publishes beside it. The ordinal is the plan's own
 * `selection_index`, never the menu position the viewer clicked.
 */
function planTrackIdentity(
  planTracks: readonly AudioInventoryTrack[],
  planTrack: AudioInventoryTrack,
): TrackIdentityV3 {
  const index = planTracks.indexOf(planTrack);
  return {
    id: planTrack.track_id?.trim() ?? "",
    index: planTrack.selection_index ?? (index >= 0 ? index : 0),
  };
}

/**
 * Resolves the identity to send for an explicit audio pick.
 *
 * The menu renders `displayed`, which `applyAudioInventory` may have replaced
 * with the probed catalog list. A probe repair can reorder the effective
 * file's tracks, so a bare position no longer names the picked language. Send
 * the plan inventory's canonical `track_id` and the plan ordinal that id
 * encodes — matched by stable signature, then by language family when the menu
 * no longer shows the plan's own list. Keeping the client's menu ordinal would
 * make the pair internally inconsistent, and the server rejects a `track_id`
 * whose encoded ordinal differs from the accompanying index.
 */
export function resolvePlanAudioIdentity(
  planTracks: readonly AudioInventoryTrack[],
  displayed: readonly AudioInventoryTrack[],
  clientIndex: number,
): TrackIdentityV3 {
  const planTrack =
    displayed === planTracks
      ? planTracks[clientIndex]
      : matchPlanTrack(planTracks, displayed[clientIndex]);
  if (!planTrack) return { id: "", index: clientIndex };
  const identity = planTrackIdentity(planTracks, planTrack);
  return identity.id === "" ? { id: "", index: clientIndex } : identity;
}

/**
 * Resolves a menu row captured at pick time against the plan that is current
 * when the pick is finally dispatched. Queued picks outlive the plan they were
 * clicked against (a seek reanchor or a version switch adopts a new plan), so
 * the menu ordinal cannot be trusted; the row's stable descriptor is matched
 * against the new plan's inventory instead.
 */
export function resolveAudioPickIdentity(
  planTracks: readonly AudioInventoryTrack[],
  picked: AudioInventoryTrack | undefined,
  fallbackIndex: number,
): TrackIdentityV3 | null {
  if (planTracks.length === 0) {
    return Number.isInteger(fallbackIndex) && fallbackIndex >= 0
      ? { id: "", index: fallbackIndex }
      : null;
  }
  const planTrack = matchPlanTrack(planTracks, picked);
  if (planTrack) {
    const identity = planTrackIdentity(planTracks, planTrack);
    if (identity.id !== "") return identity;
  }
  const hasStableIdentities = planTracks.some((track) => (track.track_id ?? "").trim() !== "");
  if (hasStableIdentities) return null;
  return fallbackIndex >= 0 && fallbackIndex < planTracks.length
    ? { id: "", index: fallbackIndex }
    : null;
}

/**
 * Whether a direct `track_id` lookup can be trusted across the two lists.
 *
 * The server derives an audio id from the file id and the track's published
 * ordinal, so a replacement inventory can reuse the same ids for different
 * tracks. A lookup is only an identity match when every non-empty id in the
 * plan appears in the displayed list and names the track with the same
 * descriptor. An ID-less list shares no identity mapping.
 */
function sharesAudioIdMapping(
  planTracks: readonly AudioInventoryTrack[],
  displayed: readonly AudioInventoryTrack[],
): boolean {
  if (displayed === planTracks) return true;

  const planById = new Map<string, AudioInventoryTrack>();
  for (const track of planTracks) {
    const id = (track.track_id ?? "").trim();
    if (id) planById.set(id, track);
  }
  if (planById.size === 0) return false;

  let seen = 0;
  for (const track of displayed) {
    const id = (track.track_id ?? "").trim();
    if (!id) return false;
    const planTrack = planById.get(id);
    if (!planTrack || !sameTrackDescriptor(planTrack, track)) return false;
    seen += 1;
  }
  return seen === planById.size;
}

/**
 * Maps the committed plan selection onto the inventory the menu currently
 * renders. `applyAudioInventory` can replace that list with probed catalog rows
 * that carry no stable id, so the marker follows the matched row (signature
 * first, then language) rather than the plan ordinal. A direct id lookup is
 * only used when both lists agree on what each id names; otherwise a re-minted
 * replacement id could mark an unrelated track. `-1` means no row in the
 * applied list is the committed track: lighting an unrelated row would be
 * worse than showing no marker.
 */
export function resolveAppliedAudioIndex(
  planTracks: readonly AudioInventoryTrack[],
  displayed: readonly AudioInventoryTrack[],
  selected: { id?: string; index?: number } | null | undefined,
): number {
  if (!selected) return displayed.length > 0 ? 0 : -1;
  if (displayed === planTracks && selected.index != null) return selected.index;

  const selectedId = (selected.id ?? "").trim();
  const planTrack =
    (selectedId
      ? planTracks.find((track) => (track.track_id ?? "").trim() === selectedId)
      : null) ?? (selected.index != null ? planTracks[selected.index] : undefined);
  if (!planTrack) {
    return selected.index != null && selected.index < displayed.length ? selected.index : -1;
  }

  if (selectedId !== "" && sharesAudioIdMapping(planTracks, displayed)) {
    const directId = displayed.findIndex((track) => (track.track_id ?? "").trim() === selectedId);
    if (directId >= 0) return directId;
  }

  const matched = matchPlanTrack(displayed, planTrack);
  if (matched) return displayed.indexOf(matched);
  return -1;
}
