import { formatFileSize } from "@/lib/mediaFormat";

/**
 * Health badges for a version row, derived only from server-published state.
 *
 * The server is the sole authority. A row is "Source mismatch" only while the
 * server holds a recent probe verdict whose served bytes clearly differ from
 * the bytes the row declares: the release itself is alive, so the badge never
 * means failed. A row is "Will retry on play" only when the catalog's liveness
 * check reports `available === false` (the provider listed and the pinned
 * release was gone, or a durable failure stamp is set), and "Failed" only when
 * the server's candidate record carries `failed`. An absent flag means unknown
 * and renders no badge, so a menu never infers a row's health from the client's
 * own transport errors or a failed play attempt.
 */
export type VersionHealthTone = "danger" | "warn";

export interface VersionHealthBadge {
  /** Stable key for React and for the render's tone branch. */
  key: "failed" | "unavailable" | "source_mismatch";
  label: string;
  /** Longer explanation for the badge's title/tooltip. */
  title: string;
  tone: VersionHealthTone;
}

/** The server fields a version row is allowed to derive its health from. */
export interface ServerVersionHealth {
  /**
   * Catalog liveness. Absent means unknown; `false` means the server's last
   * check could not resolve the row (a confirmed dead pin or a durable failed
   * stamp). A provider outage is reported as ambiguous, not `false`.
   */
  available?: boolean;
  /** The server's candidate record marked this version failed. */
  failed?: boolean;
  /**
   * A recent probe read bytes that disagree with the declared release. The
   * server reports this only while its short wrong-source verdict lives; it
   * is never a failure verdict.
   */
  source_mismatch?: { served_bytes: number; declared_bytes: number };
}

export function deriveVersionHealth(version: ServerVersionHealth): VersionHealthBadge | null {
  if (version.failed === true) {
    return {
      key: "failed",
      label: "Failed",
      title: "The server marked this version as failed.",
      tone: "danger",
    };
  }
  if (version.source_mismatch) {
    const served = formatFileSize(version.source_mismatch.served_bytes);
    const declared = formatFileSize(version.source_mismatch.declared_bytes);
    // Humanized sizes are part of the visible state, not just the tooltip:
    // "the provider served the wrong file" needs to read differently from
    // "this row has not been probed yet".
    const sizes = served && declared ? ` — served ${served} of declared ${declared}` : "";
    return {
      key: "source_mismatch",
      label: `Source mismatch${sizes}`,
      title: `Source mismatch${sizes}. The provider is serving different bytes for this release; playing it retries the link.`,
      tone: "warn",
    };
  }
  if (version.available === false) {
    return {
      key: "unavailable",
      label: "Will retry on play",
      title: "The provider is not serving this version right now; playing it retries the link.",
      tone: "warn",
    };
  }
  return null;
}
