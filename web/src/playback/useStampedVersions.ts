import { useMemo } from "react";

import {
  applyVersionAvailability,
  type VersionLivenessCandidate,
} from "@/hooks/queries/versionLiveness";

const EMPTY_VERSION_ROWS: never[] = [];

/**
 * The version rows with the server's liveness verdicts stamped on, kept
 * referentially stable while neither input changes.
 *
 * `applyVersionAvailability` builds a new array whenever a verdict differs
 * from its row — rows without a probed `available` flag always differ — so
 * calling it inline hands downstream consumers a new `versions` identity on
 * every render. The player's reconnect reconcile lists that prop as a
 * dependency and refetches the whole watch payload (`staleTime: 0`) on every
 * change: roughly one 176 kB request per second for the entire session.
 * Memoized, the prop — and the reconcile — only move when the rows or the
 * verdicts actually change.
 */
export function useStampedVersions<T extends VersionLivenessCandidate>(
  versions: readonly T[] | undefined,
  liveness: Map<number, boolean>,
): T[] {
  // `applyVersionAvailability` only reads (it maps to a new array), so the
  // readonly input is safe to pass through.
  const rows = (versions ?? EMPTY_VERSION_ROWS) as T[];
  return useMemo(() => applyVersionAvailability(rows, liveness), [rows, liveness]);
}
