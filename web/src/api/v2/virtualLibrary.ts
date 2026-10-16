import { fetchWithSession } from "@/api/client";

/**
 * The virtual-library capability, as the feature-detection surface publishes
 * it. `indexer_request` (or `indexer_search`) gates the indexer UI: a server
 * that cannot request releases must not offer the action.
 *
 * The endpoint is not in the committed v2 OpenAPI document yet, so it is
 * hand-rolled through the shared session client. An absent or unrecognized
 * answer leaves both flags false, which hides the indexer UI.
 */
export const VIRTUAL_LIBRARY_CAPABILITY_PATH = "/api/v2/capabilities/virtual-library";

export interface VirtualLibraryCapability {
  state: string;
  indexer_search: boolean;
  indexer_request: boolean;
  discover_before_playback?: boolean;
  revision?: string;
}

export async function fetchVirtualLibraryCapability(): Promise<VirtualLibraryCapability> {
  const { res } = await fetchWithSession(VIRTUAL_LIBRARY_CAPABILITY_PATH, {
    method: "GET",
    headers: { Accept: "application/json" },
  });
  if (!res.ok) {
    throw new Error(`Reading the virtual library capability failed (${res.status}).`);
  }
  const body = (await res.json()) as Record<string, unknown> | null;
  return {
    state: typeof body?.state === "string" ? body.state : "unavailable",
    indexer_search: body?.indexer_search === true,
    // Absent or a server that predates the flag fails closed.
    indexer_request: body?.indexer_request === true,
    discover_before_playback: body?.discover_before_playback === true,
    revision: typeof body?.revision === "string" ? body.revision : undefined,
  };
}
