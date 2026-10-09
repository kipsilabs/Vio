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

/** Whether the browser still holds the user activation a programmatic click needs. */
function userActivationAvailable(): boolean {
  if (typeof navigator === "undefined") return true;
  const activation = (navigator as Navigator & { userActivation?: { isActive?: boolean } })
    .userActivation;
  return !activation || activation.isActive !== false;
}

// Browser navigation cannot set headers. Retain the existing account-token
// authority; do not imply that the selected profile/PIN travels in this URL.
export async function launchDirectDownload(
  fileId: number,
  isCurrent: () => boolean,
): Promise<void> {
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
  // One request only, using the same verb and URL the browser will navigate to.
  // A HEAD 200 could pass while the navigation GET was refused, which stranded
  // the user on a silent no-op; the GET probe authorizes exactly what follows.
  // A one-byte range keeps the probe from streaming the file, and the captured
  // token is reused for the navigation so a rotation cannot split the two.
  const controller = new AbortController();
  let res: Response;
  try {
    res = await fetch(url, {
      method: "GET",
      headers: { Range: "bytes=0-0" },
      cache: "no-store",
      signal: controller.signal,
    });
  } catch (error) {
    // A rejected probe must not report into a replacement authority either.
    requireCurrent();
    throw error;
  } finally {
    // Only the status was needed; stop any body the probe started.
    controller.abort();
  }
  requireCurrent();
  if (res.status !== 200 && res.status !== 206) {
    throw new DirectDownloadError(res.status, directDownloadErrorMessage(res.status));
  }
  if (!userActivationAvailable()) {
    throw new DirectDownloadError(
      0,
      "Your browser blocked the download. Allow downloads for this site and try again.",
    );
  }
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = "";
  anchor.referrerPolicy = "no-referrer";
  document.body.appendChild(anchor);
  try {
    anchor.click();
  } catch {
    throw new DirectDownloadError(
      0,
      "Your browser blocked the download. Allow downloads for this site and try again.",
    );
  } finally {
    anchor.remove();
  }
}
