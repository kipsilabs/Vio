import { useMemo, useState } from "react";
import type { FileVersion, VersionPlayability } from "@/api/types";

/** A version is unavailable when the item metadata or the liveness check
 *  reported it so. Absent `available` means available/unknown. */
export function isVersionUnavailable(version: FileVersion): boolean {
  return version.available === false;
}

/** A version the server confirmed dead (durable failed verdict). Unlike a
 *  merely unavailable row, which may recover and stays behind the "show
 *  unavailable" toggle, a failed row is removed from the list entirely:
 *  offering it again would only replay a known-dead release. */
export function isVersionFailed(version: FileVersion): boolean {
  return version.failed === true;
}

/**
 * The one field a playability verdict needs from a row. Both the item page's
 * `FileVersion` and the player menu's row satisfy it, so every version list
 * shares one definition of "unplayable" instead of each mapping its own.
 */
export interface VersionPlayabilitySource {
  playability?: VersionPlayability;
}

/**
 * A version the server's planner refused terminally for this client: the start
 * would repeat the same refusal. The row stays visible (so the list does not
 * silently change shape) but the picker disables it and names the reason. An
 * absent verdict is unknown, not unplayable.
 */
export function isVersionUnplayable(version: VersionPlayabilitySource): boolean {
  return version.playability?.playable === false;
}

/** The badge a picker shows on an unplayable row. The title carries the start
 *  path's own message so the reason is the planner's, not a client guess. */
export function versionPlayabilityBadge(version: VersionPlayabilitySource): {
  label: string;
  title: string;
} | null {
  const playability = version.playability;
  if (!playability || playability.playable) {
    return null;
  }
  const title =
    playability.message?.trim() ||
    "This version cannot play on this device. Choose another version.";
  return { label: "Can't play on this device", title };
}

export interface VersionVisibility {
  /** Versions to render in the picker. */
  visibleVersions: FileVersion[];
  /** Count of unavailable versions hidden behind the toggle (0 when none). */
  hiddenUnavailableCount: number;
  showUnavailable: boolean;
  setShowUnavailable: (show: boolean) => void;
}

/**
 * Defaults to hiding unavailable versions, with three exceptions:
 * - no unavailable versions → nothing to hide;
 * - every version is unavailable → show everything rather than an empty list;
 * - the active selection is unavailable → it stays visible.
 * Rows the server confirmed dead (durable failed verdict) are removed
 * outright instead of hidden: no toggle brings them back. The active
 * selection is still kept so a mid-playback verdict cannot yank the
 * playing row, and an all-failed list still renders rather than an
 * empty menu.
 * Rows this client cannot play are never hidden: the picker shows them
 * disabled with the planner's reason, so an unsupported release is visible
 * instead of silently disappearing. They are also excluded from the
 * unavailable count, which keeps that toggle about retryable health only.
 * When some versions are hidden, the caller renders a "Show N unavailable
 * versions" toggle that flips `showUnavailable`.
 */
export function useVersionVisibility(
  versions: FileVersion[],
  activeFileId: number | null | undefined,
): VersionVisibility {
  const [showUnavailable, setShowUnavailable] = useState(false);

  // Reset the toggle when the version set changes (e.g. navigating to a
  // different item or a different edition count) so a stale "show all" never
  // leaks into a fresh list. Same render-time adjustment pattern as
  // MovieContent. The signature is the version count, not the file_ids: a
  // virtual row's resolved candidate can rotate its id between liveness polls
  // while the viewer is looking, and keying on ids reset the toggle under
  // them. A genuinely different list with the same count keeps the toggle too;
  // that churn is frequent and availability state, not row identity, drives
  // the rows.
  const signature = versions.length;
  const [prevSignature, setPrevSignature] = useState(signature);
  if (prevSignature !== signature) {
    setPrevSignature(signature);
    setShowUnavailable(false);
  }

  return useMemo(() => {
    const alive = versions.filter(
      (version) => !isVersionFailed(version) || version.file_id === activeFileId,
    );
    const list = alive.length > 0 ? alive : versions;
    const unavailable = list.filter(isVersionUnavailable);
    const allUnavailable = unavailable.length > 0 && unavailable.length === list.length;
    // Unplayable rows are not hidden behind the availability toggle: the
    // picker shows them disabled with the planner's reason, so the list keeps
    // its shape and the viewer can see what an unsupported row would have been.
    const hideableUnavailable = unavailable.filter((version) => !isVersionUnplayable(version));

    if (hideableUnavailable.length === 0 || allUnavailable || showUnavailable) {
      return {
        visibleVersions: list,
        hiddenUnavailableCount: 0,
        showUnavailable,
        setShowUnavailable,
      };
    }

    const visibleVersions = list.filter(
      (version) =>
        !isVersionUnavailable(version) ||
        isVersionUnplayable(version) ||
        version.file_id === activeFileId,
    );
    const hiddenUnavailableCount = hideableUnavailable.filter(
      (version) => version.file_id !== activeFileId,
    ).length;

    return { visibleVersions, hiddenUnavailableCount, showUnavailable, setShowUnavailable };
  }, [activeFileId, showUnavailable, versions]);
}
