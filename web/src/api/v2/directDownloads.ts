import {
  captureProfileRequestContext,
  captureSessionIdentity,
  getAccessToken,
  isCapturedProfileAuthorityActive,
  isSessionIdentityCurrent,
  StaleApiRequestContextError,
} from "@/api/client";

/** A direct-download attempt the user must be told about, with a concrete reason. */
export class DirectDownloadError extends Error {
  /** The HTTP status that refused the attempt, or 0 for a non-HTTP failure. */
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = "DirectDownloadError";
    this.status = status;
  }
}

/** Plain-language reason for a refused direct-download response. */
export function directDownloadErrorMessage(status: number): string {
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

/** A fetched, save-ready download. The caller owns the object URL and disposes it. */
export interface PreparedDirectDownload {
  /** Same-origin blob URL holding the fetched file. */
  readonly url: string;
  /** Filename suggested by the server, or a safe fallback. */
  readonly filename: string;
  /** Release the object URL once the save link is gone. */
  dispose(): void;
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
// Two requests, both awaited: a ranged GET is a preflight that fails fast when
// the captured token is no longer authorized, and a full GET is the transfer
// itself. Because the transfer is an awaited fetch, a refusal, network failure
// or mid-stream drop is observable instead of vanishing behind a navigation.
// The bytes come back as a blob object URL for an explicit user-clicked save,
// so no inferred activation check can turn a slow probe into a false refusal.
export async function prepareDirectDownload(
  fileId: number,
  isCurrent: () => boolean,
  filenameHint?: string,
): Promise<PreparedDirectDownload> {
  if (!Number.isSafeInteger(fileId) || fileId <= 0)
    throw new DirectDownloadError(0, "Invalid file ID.");
  const token = getAccessToken();
  const identity = captureSessionIdentity();
  const profile = captureProfileRequestContext();
  const requireCurrent = () => {
    if (
      !token ||
      !isCurrent() ||
      !isSessionIdentityCurrent(identity) ||
      (profile && !isCapturedProfileAuthorityActive(profile))
    )
      throw new StaleApiRequestContextError();
  };
  requireCurrent();
  const url = `/api/v2/direct-download?${new URLSearchParams({ file_id: String(fileId), token: token! })}`;

  const probe = new AbortController();
  let probeRes: Response;
  try {
    probeRes = await fetch(url, {
      method: "GET",
      headers: { Range: "bytes=0-0" },
      cache: "no-store",
      signal: probe.signal,
    });
  } catch (error) {
    // A rejected probe must not report into a replacement authority either.
    requireCurrent();
    throw error;
  } finally {
    // Only the status was needed; stop any body the probe started.
    probe.abort();
  }
  requireCurrent();
  if (!probeAccepted(probeRes.status)) {
    throw new DirectDownloadError(probeRes.status, directDownloadErrorMessage(probeRes.status));
  }

  let transferRes: Response;
  try {
    transferRes = await fetch(url, { method: "GET", cache: "no-store" });
  } catch (error) {
    requireCurrent();
    throw error;
  }
  requireCurrent();
  if (!transferRes.ok) {
    throw new DirectDownloadError(
      transferRes.status,
      directDownloadErrorMessage(transferRes.status),
    );
  }
  let blob: Blob;
  try {
    blob = await transferRes.blob();
  } catch {
    requireCurrent();
    throw new DirectDownloadError(0, "The download stopped before it finished. Try again.");
  }
  requireCurrent();
  const objectUrl = URL.createObjectURL(blob);
  return {
    url: objectUrl,
    filename: filenameFromDisposition(transferRes.headers.get("Content-Disposition"), filenameHint),
    dispose: () => URL.revokeObjectURL(objectUrl),
  };
}
