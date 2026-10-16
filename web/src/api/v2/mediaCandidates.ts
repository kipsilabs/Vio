import { fetchWithSession } from "@/api/client";

export async function discoverVirtualCandidates(
  mediaId: string,
  signal?: AbortSignal,
): Promise<void> {
  const { res } = await fetchWithSession(
    `/api/v2/media/${encodeURIComponent(mediaId)}/virtual-candidates:discover`,
    { method: "POST", headers: { Accept: "application/json" }, signal },
  );
  if (!res.ok) {
    throw new Error(`Discovering versions failed (${res.status}).`);
  }
}

/**
 * Asks the server to re-list a title's video candidates.
 *
 * The refresh is asynchronous: the endpoint answers `202 Accepted` with a
 * `Location` pointing at an admin job and a `Retry-After`, then re-lists the
 * provider's candidates in the background. The caller waits for the job to
 * reach a terminal state and only then re-reads the list. The client must not
 * use a failed or in-flight refresh to shrink the list it already has.
 *
 * The endpoint is not in the committed v2 OpenAPI document yet, so the request
 * is hand-rolled through the shared session client instead of `v2(...)`. The
 * paths live in these templates so they are easy to change when the backend
 * endpoints land.
 */
export const VIRTUAL_CANDIDATES_REFRESH_PATH =
  "/api/v2/media/{media_id}/virtual-candidates:refresh";

export function virtualCandidatesRefreshPath(mediaId: string): string {
  return VIRTUAL_CANDIDATES_REFRESH_PATH.replace("{media_id}", encodeURIComponent(mediaId));
}

/** Every accepted admin job lives under this prefix in the `Location` header. */
export const VIRTUAL_CANDIDATES_JOB_PREFIX = "/api/v2/admin/jobs/";

/**
 * Cancels the caller's in-flight refresh of a title. The server resolves the
 * active job by media id and authorizes it to the job owner, so no job id is
 * needed. Cancellation is non-destructive: candidates already persisted stay,
 * and the automatic re-listing intervals are untouched.
 */
export const VIRTUAL_CANDIDATES_CANCEL_PATH =
  "/api/v2/media/{media_id}/virtual-candidates:refresh/cancel";

export function virtualCandidatesCancelPath(mediaId: string): string {
  return VIRTUAL_CANDIDATES_CANCEL_PATH.replace("{media_id}", encodeURIComponent(mediaId));
}

/**
 * Stops the refresh the caller started for a title. The first press starts a
 * job and locks the control; a second press calls this to cancel that job and
 * release the lock.
 */
export async function cancelVirtualCandidatesRefresh(mediaId: string): Promise<void> {
  const { res } = await fetchWithSession(virtualCandidatesCancelPath(mediaId), {
    method: "POST",
    headers: { Accept: "application/json" },
  });
  if (!res.ok) {
    throw new Error(`Canceling the refresh failed (${res.status}).`);
  }
}

/** The documented `Retry-After` fallback when the server does not send one. */
export const DEFAULT_REFRESH_RETRY_AFTER_MS = 5_000;

export interface VirtualCandidatesRefreshAccepted {
  /** The admin job id parsed from the `Location` header. */
  jobId: string;
  /** The `Retry-After` delay in milliseconds, or the documented default. */
  retryAfterMs: number;
}

/**
 * Starts the asynchronous re-list and returns the job to wait on. Resolves once
 * the server has accepted the work; the caller owns waiting for the job and
 * re-reading the version list afterwards.
 */
export async function startVirtualCandidatesRefresh(
  mediaId: string,
): Promise<VirtualCandidatesRefreshAccepted> {
  const { res } = await fetchWithSession(virtualCandidatesRefreshPath(mediaId), {
    method: "POST",
    headers: { Accept: "application/json" },
  });
  if (!res.ok) {
    throw new Error(`Refreshing the version list failed (${res.status}).`);
  }
  const jobId = adminJobIdFromLocation(res.headers.get("Location"));
  if (!jobId) {
    throw new Error("The refresh job was not acknowledged by the server.");
  }
  const seconds = Number(res.headers.get("Retry-After"));
  return {
    jobId,
    retryAfterMs:
      Number.isFinite(seconds) && seconds > 0 ? seconds * 1000 : DEFAULT_REFRESH_RETRY_AFTER_MS,
  };
}

/**
 * Runs the asynchronous "Refresh List" flow to completion: start the job, wait
 * for it to reach a terminal state through the caller's `awaitAdminJob`, and
 * resolve. The caller applies the refreshed list afterwards; the control that
 * triggered this stays locked for the entire promise.
 */
export async function awaitVirtualCandidatesRefresh(
  mediaId: string,
  awaitAdminJob: (jobId: string) => Promise<unknown>,
): Promise<void> {
  const accepted = await startVirtualCandidatesRefresh(mediaId);
  await awaitAdminJob(accepted.jobId);
}

/** Pulls the job id out of a `Location: /api/v2/admin/jobs/{id}` header. */
export function adminJobIdFromLocation(location: string | null | undefined): string | null {
  if (!location) return null;
  let path = location.trim();
  if (!path) return null;
  if (/^https?:\/\//i.test(path)) {
    try {
      path = new URL(path).pathname;
    } catch {
      return null;
    }
  }
  const pathname = path.split(/[?#]/)[0] ?? "";
  if (!pathname.startsWith(VIRTUAL_CANDIDATES_JOB_PREFIX)) return null;
  const jobId = pathname.slice(VIRTUAL_CANDIDATES_JOB_PREFIX.length).replace(/\/+$/, "");
  return jobId ? decodeURIComponent(jobId) : null;
}

/**
 * Requests one indexer release on the provider. Idempotent: a second call for
 * the same release returns the same state, so a retry after a failed request
 * is safe.
 */
export const VIRTUAL_RELEASE_REQUEST_PATH =
  "/api/v2/media/{media_id}/virtual-releases/{release_id}:request";

export function virtualReleaseRequestPath(mediaId: string, releaseId: string): string {
  return VIRTUAL_RELEASE_REQUEST_PATH.replace("{media_id}", encodeURIComponent(mediaId)).replace(
    "{release_id}",
    encodeURIComponent(releaseId),
  );
}

export interface IndexerReleaseRequestResult {
  release_id: string;
  state: "queued" | "failed";
  message?: string;
}

export async function requestVirtualRelease(
  mediaId: string,
  releaseId: string,
): Promise<IndexerReleaseRequestResult> {
  const { res } = await fetchWithSession(virtualReleaseRequestPath(mediaId, releaseId), {
    method: "POST",
    headers: { Accept: "application/json" },
  });
  if (!res.ok) {
    throw new Error(`Requesting this release failed (${res.status}).`);
  }
  const body = (await res.json()) as Partial<IndexerReleaseRequestResult> | null;
  return {
    release_id: typeof body?.release_id === "string" ? body.release_id : releaseId,
    // An unexpected state is treated as a failure so the row stays retryable
    // rather than falsely reading as requested.
    state: body?.state === "queued" ? "queued" : "failed",
    message: typeof body?.message === "string" ? body.message : undefined,
  };
}
