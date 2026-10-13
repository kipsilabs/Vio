import {
  captureProfileRequestContext,
  isCapturedProfileAuthorityActive,
  StaleApiRequestContextError,
} from "@/api/client";
import { v2, type V2Result } from "./request";

/** One download row as the registry list returns it. */
export type DownloadRegistryEntry = V2Result<"GET /api/v2/downloads">["items"][number];

export async function fetchDownloadCapability() {
  const profileContext = captureProfileRequestContext();
  if (!profileContext) throw new StaleApiRequestContextError();
  const result = await v2("GET /api/v2/capabilities/downloads", { profileContext });
  if (!isCapturedProfileAuthorityActive(profileContext)) throw new StaleApiRequestContextError();
  return result;
}

/**
 * Reads every managed entry the calling profile and this browser device own,
 * following the registry's cursor pages so a reconcile sees the whole list
 * rather than only the first fifty entries. A per-entry read cannot be used
 * here: the caller is looking for an entry whose id it does not yet know.
 */
export async function listDownloads(signal?: AbortSignal): Promise<DownloadRegistryEntry[]> {
  const profileContext = captureProfileRequestContext();
  if (!profileContext) throw new StaleApiRequestContextError();
  const entries: DownloadRegistryEntry[] = [];
  let cursor: string | undefined;
  for (;;) {
    const result = await v2("GET /api/v2/downloads", {
      query: cursor ? { cursor } : undefined,
      profileContext,
      signal,
    });
    if (!isCapturedProfileAuthorityActive(profileContext)) throw new StaleApiRequestContextError();
    entries.push(...result.items);
    const next = result.page?.next_cursor;
    // A page with no cursor, or one that repeats the cursor just read, is the
    // end of the list; stop instead of paging forever on a malformed reply.
    if (!result.page?.has_more || !next || next === cursor) break;
    cursor = next;
  }
  return entries;
}

/**
 * The managed entry a create request for this content and episode would target:
 * the server keys a managed single download on `(content, episode)`, not the
 * selected file, so re-downloading any version of the title resolves to the
 * same row.
 */
export function findManagedEntry(
  entries: readonly DownloadRegistryEntry[],
  target: { contentId: string; episodeId?: string },
): DownloadRegistryEntry | undefined {
  return entries.find(
    (entry) =>
      entry.content_id === target.contentId &&
      (target.episodeId ? entry.episode_id === target.episodeId : !entry.episode_id),
  );
}
export async function deleteDownloadEntry(id: string) {
  const profileContext = captureProfileRequestContext();
  if (!profileContext) throw new StaleApiRequestContextError();
  await v2("DELETE /api/v2/downloads/{id}", {
    path: { id },
    profileContext,
    retryAuthentication: false,
  });
  if (!isCapturedProfileAuthorityActive(profileContext)) throw new StaleApiRequestContextError();
}
