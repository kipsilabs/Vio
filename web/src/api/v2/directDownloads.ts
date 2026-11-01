import {
  captureProfileRequestContext,
  captureSessionIdentity,
  getAccessToken,
  isCapturedProfileAuthorityActive,
  isSessionIdentityCurrent,
  StaleApiRequestContextError,
} from "@/api/client";
import { problemId } from "./problemId";

/** A direct-download attempt the user must be told about, with a concrete reason. */
export class DirectDownloadError extends Error {
  /** The HTTP status that refused the attempt, or 0 for a non-HTTP failure. */
  readonly status: number;
  /**
   * The v2 problem identifier (`file_unavailable`, `file_access_denied`, …)
   * when the server answered with a Problem Details body; otherwise "". The
   * identifier distinguishes a stale file_id, which needs a version refresh,
   * from an access refusal, which needs the denial text.
   */
  readonly code: string;

  constructor(status: number, message: string, code = "") {
    super(message);
    this.name = "DirectDownloadError";
    this.status = status;
    this.code = code;
  }
}

/** Plain-language reason for a refused direct-download response. */
export function directDownloadErrorMessage(status: number, code = ""): string {
  switch (code) {
    case "file_unavailable":
      return "This version is no longer available. Refresh the list to see current versions.";
    case "file_access_denied":
      return "You do not have access to this file.";
    case "format_unavailable":
      return "This version can't be downloaded as a file. Play it instead.";
  }
  switch (status) {
    case 401:
      return "Your session ended. Sign in again to download.";
    case 403:
      return "Downloads are not allowed for this account.";
    case 404:
      return "This file is no longer available to download.";
    case 416:
      return "The server could not read this file to download.";
    case 503:
      return "Downloads are temporarily unavailable. Try again.";
    default:
      return `Download could not be started (HTTP ${status}).`;
  }
}

/**
 * Whether a refusal means the selected file_id no longer resolves, so the
 * caller should re-fetch the item's versions instead of only toasting. A bare
 * 404 on the direct-download URL has the same corrective action as the
 * server's `file_unavailable` code.
 */
export function isStaleFileError(error: unknown): boolean {
  return (
    error instanceof DirectDownloadError &&
    (error.code === "file_unavailable" || error.status === 404)
  );
}

/** Reads the problem identifier out of a refusal body, or "" when it is absent. */
async function probeProblemCode(res: Response): Promise<string> {
  try {
    const parsed = JSON.parse(await res.text()) as { type?: unknown };
    if (typeof parsed?.type === "string") return problemId({ type: parsed.type });
  } catch {
    // A non-JSON body (a proxy error page, an empty body) carries no code.
  }
  return "";
}

/**
 * A preflighted direct-download link. The URL is the authenticated
 * direct-download endpoint itself; the browser fetches it when the user clicks
 * the save link, so there is no object URL to own or revoke.
 */
export interface PreparedDirectDownload {
  /** The authenticated direct-download URL the explicit save link points at. */
  readonly url: string;
  /** Filename suggested by the server, or a safe fallback. */
  readonly filename: string;
}

function decodeFilenamePart(value: string): string {
  try {
    return decodeURIComponent(value);
  } catch {
    return value;
  }
}

/** A filesystem-safe filename from Content-Disposition, falling back to the caller's hint. */
function filenameFromDisposition(header: string | null, hint?: string): string {
  const match = header?.match(/filename\*?=(?:UTF-8''|")?([^";]+)/i);
  const candidate = (match ? decodeFilenamePart(match[1]?.trim() ?? "") : (hint ?? ""))
    .replace(/[\p{Cc}\\/]+/gu, "_")
    .trim();
  return candidate.length > 0 ? candidate : "download";
}

/** Whether a probe status authorizes the file (200 full body or 206 partial). */
function probeAccepted(status: number): boolean {
  return status === 200 || status === 206;
}

// Browser navigation cannot set headers. Retain the existing account-token
// authority; do not imply that the selected profile/PIN travels in this URL.
//
// The ranged GET is a preflight, not delivery proof: it establishes that the
// captured token is still authorized for this exact verb and URL so a refusal
// surfaces as text instead of a silent navigation. The bytes are never fetched
// here — the explicit save link's own navigation performs the transfer, with
// real user activation behind it. Abort is threaded through the preflight and
// its body consumption via `signal`; the transfer itself is browser-owned
// navigation, so there is nothing for JS to abort or revoke on the save path.
export async function prepareDirectDownload(
  fileId: number,
  isCurrent: () => boolean,
  signal: AbortSignal,
  filenameHint?: string,
): Promise<PreparedDirectDownload> {
  if (!Number.isSafeInteger(fileId) || fileId <= 0)
    throw new DirectDownloadError(0, "Invalid file ID.");
  const token = getAccessToken();
  const identity = captureSessionIdentity();
  const profile = captureProfileRequestContext();
  const requireCurrent = () => {
    if (
      signal.aborted ||
      !token ||
      !isCurrent() ||
      !isSessionIdentityCurrent(identity) ||
      (profile && !isCapturedProfileAuthorityActive(profile))
    )
      throw new StaleApiRequestContextError();
  };
  requireCurrent();
  const url = `/api/v2/direct-download?${new URLSearchParams({ file_id: String(fileId), token: token! })}`;

  let probeRes: Response;
  try {
    probeRes = await fetch(url, {
      method: "GET",
      headers: { Range: "bytes=0-0" },
      cache: "no-store",
      signal,
    });
  } catch (error) {
    // A rejected or aborted probe must not report into a replacement authority.
    requireCurrent();
    throw error;
  }
  requireCurrent();
  if (!probeAccepted(probeRes.status)) {
    const code = await probeProblemCode(probeRes);
    requireCurrent();
    throw new DirectDownloadError(
      probeRes.status,
      directDownloadErrorMessage(probeRes.status, code),
      code,
    );
  }
  // Only the status and headers are needed. Cancel the one-byte probe body so
  // the connection is released instead of left half-read; aborting `signal`
  // while the probe was in flight cancels the transfer the same way.
  void probeRes.body?.cancel().catch(() => undefined);
  return {
    url,
    filename: filenameFromDisposition(probeRes.headers.get("Content-Disposition"), filenameHint),
  };
}
