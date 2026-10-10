import {
  captureProfileRequestContext,
  fetchWithSession,
  isProfileRequestContextCurrent,
  StaleApiRequestContextError,
  type ProfileRequestContextSnapshot,
} from "@/api/client";
import { V2ProblemError, v2, type V2Result } from "./request";

/** A download row created by `POST /api/v2/downloads`, as the create response returns it. */
export type PreparedDownloadEntry = V2Result<"POST /api/v2/downloads">["items"][number];

/** The public quality ladder a virtual download may request. */
export type PreparedDownloadQuality =
  | "original"
  | "20mbps"
  | "10mbps"
  | "5mbps"
  | "2mbps"
  | "1mbps";

const FAILED_STATUSES = new Set(["failed", "cancelled", "revoked"]);

/** Whether a status still needs polling before the prepared file can be fetched. */
export function isDownloadPreparing(status: string): boolean {
  return status === "preparing" || status === "queued";
}

/** Whether a status is a terminal refusal rather than a pending prepare. */
export function isDownloadRefused(status: string): boolean {
  return FAILED_STATUSES.has(status);
}

/** A concrete, user-actionable reason for a refused or failed preparation. */
export function downloadPreparationErrorMessage(error: unknown): string {
  if (error instanceof DownloadPreparationError) return error.message;
  if (error instanceof V2ProblemError) {
    if (error.problemType === "capability_unsupported") {
      return "This version can't be prepared for download at the selected quality.";
    }
    if (error.problemType === "dependency_unavailable") {
      return "The provider is temporarily unavailable. Try again in a moment.";
    }
    if (error.problemType === "rate_limited") {
      return error.retryAfterSeconds
        ? `Too many downloads. Try again in ${error.retryAfterSeconds}s.`
        : "Too many downloads. Try again shortly.";
    }
    return error.problem.detail || error.problem.title;
  }
  return "Download could not be prepared. Check your connection and try again.";
}

/** A preparation the picker must report, with a concrete reason. */
export class DownloadPreparationError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "DownloadPreparationError";
  }
}

/**
 * Creates a virtual download, preparing a compatible artifact server-side. The
 * returned row is `preparing` until the artifact exists, then `ready`. The
 * server picks the version from `mediaFileId` and applies the requested quality
 * through the same policy and capability caps playback uses.
 */
export async function createPreparedDownload(
  input: {
    contentId: string;
    episodeId?: string;
    mediaFileId: number;
    quality: PreparedDownloadQuality;
  },
  signal?: AbortSignal,
): Promise<PreparedDownloadEntry> {
  if (!Number.isSafeInteger(input.mediaFileId) || input.mediaFileId <= 0) {
    throw new DownloadPreparationError("Invalid file ID.");
  }
  const snapshot = requireSnapshot();
  const result = await v2("POST /api/v2/downloads", {
    body: {
      content_id: input.contentId,
      episode_id: input.episodeId || undefined,
      media_file_id: String(input.mediaFileId),
      quality: input.quality,
    },
    profileContext: snapshot,
    signal,
    retryAuthentication: false,
  });
  requireCurrent(snapshot);
  const entry = result.items?.[0];
  if (!entry) throw new DownloadPreparationError("The server did not create a download.");
  return entry;
}

/**
 * Reads the current state of one download by paging the account's ephemeral
 * registry. The picker filters to the row it created, so a rotation on another
 * item never confuses the pending preparation.
 */
export async function getPreparedDownload(
  downloadId: string,
  signal?: AbortSignal,
): Promise<PreparedDownloadEntry | null> {
  const snapshot = requireSnapshot();
  const page = await v2("GET /api/v2/downloads", {
    query: { limit: 100 },
    profileContext: snapshot,
    signal,
    retryAuthentication: false,
  });
  requireCurrent(snapshot);
  return page.items?.find((item) => String(item.id) === downloadId) ?? null;
}

/**
 * Fetches the prepared file with the session's authority and hands it to the
 * browser as a file save. A plain link cannot carry the bearer token, so the
 * bytes are fetched as a blob and released through a temporary object URL.
 */
export async function savePreparedDownload(
  downloadId: string,
  filenameHint: string,
  signal?: AbortSignal,
): Promise<void> {
  const snapshot = requireSnapshot();
  const headers: Record<string, string> = {
    Authorization: `Bearer ${snapshot.accessToken}`,
    "X-Profile-Id": snapshot.profileId,
  };
  if (snapshot.profileToken) headers["X-Profile-Token"] = snapshot.profileToken;

  const { res } = await fetchWithSession(
    `/api/v2/downloads/${encodeURIComponent(downloadId)}/file`,
    { method: "GET", headers, signal, cache: "no-store" },
    snapshot,
    false,
  );
  requireCurrent(snapshot);
  if (!res.ok) {
    throw new DownloadPreparationError(
      res.status === 409
        ? "This download is no longer available."
        : "The prepared file could not be downloaded. Try preparing it again.",
    );
  }
  const blob = await res.blob();
  const filename =
    filenameFromDisposition(res.headers.get("Content-Disposition")) || filenameHint || "download";
  triggerBlobSave(blob, filename);
}

function requireSnapshot(): ProfileRequestContextSnapshot {
  const snapshot = captureProfileRequestContext();
  if (!snapshot) throw new StaleApiRequestContextError();
  return snapshot;
}

function requireCurrent(snapshot: ProfileRequestContextSnapshot): void {
  if (!isProfileRequestContextCurrent(snapshot)) throw new StaleApiRequestContextError();
}

function filenameFromDisposition(header: string | null): string {
  const match = header?.match(/filename\*?=(?:UTF-8''|")?([^";]+)/i);
  if (!match) return "";
  let value = match[1]?.trim() ?? "";
  try {
    value = decodeURIComponent(value);
  } catch {
    // keep the raw value
  }
  return value.replace(/[\p{Cc}\\/]+/gu, "_").trim();
}

function triggerBlobSave(blob: Blob, filename: string): void {
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = filename;
  document.body.appendChild(anchor);
  anchor.click();
  anchor.remove();
  URL.revokeObjectURL(url);
}
