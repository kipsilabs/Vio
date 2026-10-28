import { useMemo, useState } from "react";
import type { FileVersion } from "@/api/types";

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
    const alive = versions.filter((version) => !isVersionFailed(version) || version.file_id === activeFileId);
    const list = alive.length > 0 ? alive : versions;
    const unavailable = list.filter(isVersionUnavailable);
    const allUnavailable = unavailable.length > 0 && unavailable.length === list.length;

    if (unavailable.length === 0 || allUnavailable || showUnavailable) {
      return {
        visibleVersions: list,
        hiddenUnavailableCount: 0,
        showUnavailable,
        setShowUnavailable,
      };
    }

    const visibleVersions = list.filter(
      (version) => !isVersionUnavailable(version) || version.file_id === activeFileId,
    );
    const hiddenUnavailableCount = unavailable.filter(
      (version) => version.file_id !== activeFileId,
    ).length;

    return { visibleVersions, hiddenUnavailableCount, showUnavailable, setShowUnavailable };
  }, [activeFileId, showUnavailable, versions]);
}
