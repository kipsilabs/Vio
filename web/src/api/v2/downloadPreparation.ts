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
 * Reads the current state of exactly one download. A per-entry read cannot be
 * rotated out from under the picker the way a page of the registry can, so a
 * slow preparation is never mistaken for a vanished one. Returns the entry,
 * or null when the server reports it gone (a 404 means the row was deleted,
 * which the caller treats as a terminal refusal).
 */
export async function getPreparedDownload(
  downloadId: string,
  signal?: AbortSignal,
): Promise<PreparedDownloadEntry | null> {
  const snapshot = requireSnapshot();
  try {
    const entry = await v2("GET /api/v2/downloads/{id}", {
      path: { id: downloadId },
      profileContext: snapshot,
      signal,
      retryAuthentication: false,
    });
    requireCurrent(snapshot);
    return entry;
  } catch (error) {
    // A 404 is the server saying the row is gone (deleted or out of scope);
    // the caller stops polling and reports a terminal refusal. Any other
    // failure is transient and the caller retries on the next tick.
    if (error instanceof V2ProblemError && error.status === 404) return null;
    throw error;
  }
}

/** Reports how the prepared bytes reached the user's disk, for a caller's status text. */
export type PreparedDownloadSave = "streamed" | "buffered";

/**
 * Fetches the prepared file with the session's authority and saves it without
 * ever holding the whole file in memory when the browser can stream to disk.
 *
 * The bearer token cannot ride a plain navigation, so the bytes are fetched
 * under the existing auth model and then either streamed through the browser's
 * file-system picker (Chromium: memory stays bounded regardless of title size)
 * or, where that API is absent, buffered as a blob for a browser-managed object
 * URL save. A user who cancels the picker throws SaveCancelledError so the
 * caller can stay silent.
 */
export async function savePreparedDownload(
  downloadId: string,
  filenameHint: string,
  signal?: AbortSignal,
): Promise<PreparedDownloadSave> {
  const snapshot = requireSnapshot();
  const headers: Record<string, string> = {
    Authorization: `Bearer ${snapshot.accessToken}`,
    "X-Profile-Id": snapshot.profileId,
  };
  if (snapshot.profileToken) headers["X-Profile-Token"] = snapshot.profileToken;
  const fileUrl = `/api/v2/downloads/${encodeURIComponent(downloadId)}/file`;

  // The picker needs transient user activation, which any await would spend
  // before it runs, so it opens before the first network round-trip. The
  // server's filename is not known yet; the caller's hint is what the user
  // selected, which is the right suggestion.
  if (supportsStreamingSave()) {
    const writable = await openSaveWritable(filenameHint || "download");
    try {
      const { res } = await fetchWithSession(
        fileUrl,
        { method: "GET", headers, signal, cache: "no-store" },
        snapshot,
        false,
      );
      requireCurrent(snapshot);
      if (!res.ok) {
        await writable.abort().catch(() => undefined);
        throw new DownloadPreparationError(preparedFileRefusalMessage(res.status));
      }
      await streamBodyToWritable(res, writable, signal);
      await writable.close();
      requireCurrent(snapshot);
      return "streamed";
    } catch (error) {
      await writable.abort().catch(() => undefined);
      if (error instanceof DOMException && error.name === "AbortError")
        throw new SaveCancelledError();
      throw error;
    }
  }

  const { res } = await fetchWithSession(
    fileUrl,
    { method: "GET", headers, signal, cache: "no-store" },
    snapshot,
    false,
  );
  requireCurrent(snapshot);
  if (!res.ok) {
    throw new DownloadPreparationError(preparedFileRefusalMessage(res.status));
  }
  const blob = await res.blob();
  requireCurrent(snapshot);
  const filename =
    filenameFromDisposition(res.headers.get("Content-Disposition")) || filenameHint || "download";
  triggerBlobSave(blob, filename);
  return "buffered";
}

/** The user cancelled the browser's save picker; this is not an error to report. */
export class SaveCancelledError extends Error {
  constructor() {
    super("save cancelled");
    this.name = "SaveCancelledError";
  }
}

function preparedFileRefusalMessage(status: number): string {
  switch (status) {
    case 404:
    case 409:
      return "This download is expired or no longer available. Prepare it again.";
    case 410:
      return "This download was removed.";
    case 416:
      return "The server could not read the prepared file.";
    case 503:
      return "Downloads are temporarily unavailable. Try again shortly.";
    default:
      return "The prepared file could not be downloaded. Try preparing it again.";
  }
}

interface SaveFilePickerWindow {
  showSaveFilePicker?: (options: { suggestedName?: string }) => Promise<FileSystemWritableHandle>;
}
interface FileSystemWritableHandle {
  createWritable: () => Promise<FileSystemWritableStream>;
}
interface FileSystemWritableStream {
  write: (data: Uint8Array) => Promise<void>;
  close: () => Promise<void>;
  abort: () => Promise<void>;
}

/** Whether the browser can stream a response straight to a user-chosen file. */
function supportsStreamingSave(): boolean {
  return (
    typeof window !== "undefined" &&
    typeof (window as SaveFilePickerWindow).showSaveFilePicker === "function"
  );
}

/**
 * Opens the browser's save picker and returns its writable stream. Called
 * before any network await so transient user activation is intact. A user who
 * cancels the picker is reported as SaveCancelledError, not a failure.
 */
async function openSaveWritable(filename: string): Promise<FileSystemWritableStream> {
  const picker = (window as SaveFilePickerWindow).showSaveFilePicker;
  if (!picker) throw new DownloadPreparationError("Streaming saves are unavailable.");
  let handle: FileSystemWritableHandle;
  try {
    handle = await picker({ suggestedName: filename });
  } catch (error) {
    if (error instanceof DOMException && error.name === "AbortError")
      throw new SaveCancelledError();
    throw error;
  }
  return handle.createWritable();
}

/**
 * Streams an authorized response body into an already-open file, one chunk at
 * a time. Memory stays bounded by the chunk size, so a multi-gigabyte title does
 * not exhaust the tab. The caller aborts the writable on failure, removing the
 * partial file rather than leaving a truncated one behind.
 */
async function streamBodyToWritable(
  res: Response,
  writable: FileSystemWritableStream,
  signal?: AbortSignal,
): Promise<void> {
  const reader = res.body?.getReader();
  if (!reader) {
    // A browser that exposes the picker but not a streaming body: fall back to
    // a single buffered write so the save still succeeds.
    await writable.write(new Uint8Array(await res.arrayBuffer()));
    return;
  }
  for (;;) {
    if (signal?.aborted) throw new DOMException("save aborted", "AbortError");
    const { done, value } = await reader.read();
    if (done) break;
    if (value) await writable.write(value);
  }
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
