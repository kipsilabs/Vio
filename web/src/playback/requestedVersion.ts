import { storage } from "@/utils/storage";

/**
 * The video version the viewer asked to play, kept apart from the row that
 * actually served the bytes.
 *
 * Progress and history persistence record the effective file as
 * `last_file_id` (the server prefers the bound virtual candidate), so a
 * neutral or explicitly requested row that resolved to another candidate comes
 * back as the substitute on the next load. This store keeps the requested row
 * on the client instead, keyed by profile and item, and the media page and
 * resume hints read it before the server's version-history fields. Nothing on
 * the wire changes, so native and Jellyfin clients keep their existing meaning
 * for `last_file_id`.
 *
 * A stored row is a hint, not a source of truth: when the catalog no longer
 * carries it the page stays in its pick-again state instead of selecting
 * something else.
 */
const STORAGE_KEY = "silo-requested-version";
/** Bounded so a long-lived browser cannot grow the map without limit. */
const MAX_ENTRIES = 200;

interface RequestedVersionEntry {
  fileId: number;
  updatedAt: number;
}

type RequestedVersionMap = Record<string, RequestedVersionEntry>;

function entryKey(contentId: string): string {
  // Profiles share a browser, so one profile's pick must not become another's.
  return `${storage.get(storage.KEYS.PROFILE_ID) ?? "device"}:${contentId}`;
}

function readMap(): RequestedVersionMap {
  let parsed: unknown;
  try {
    parsed = JSON.parse(localStorage.getItem(STORAGE_KEY) ?? "{}");
  } catch {
    return {};
  }
  if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
    return {};
  }

  const map: RequestedVersionMap = {};
  for (const [key, value] of Object.entries(parsed as Record<string, unknown>)) {
    if (value === null || typeof value !== "object") continue;
    const { fileId, updatedAt } = value as { fileId?: unknown; updatedAt?: unknown };
    if (typeof fileId !== "number" || !Number.isFinite(fileId) || fileId <= 0) continue;
    map[key] = {
      fileId,
      updatedAt: typeof updatedAt === "number" && Number.isFinite(updatedAt) ? updatedAt : 0,
    };
  }
  return map;
}

/**
 * The row the viewer last requested for `contentId`, or null when they never
 * picked one. The caller falls back to the server's `last_file_id`.
 */
export function readRequestedFileId(contentId: string): number | null {
  if (!contentId) return null;
  return readMap()[entryKey(contentId)]?.fileId ?? null;
}

/**
 * Records an explicit version pick for `contentId`. The caller must only pass
 * a row the viewer actually requested; the effective playback row belongs to
 * the server's progress/history fields, not here.
 */
export function rememberRequestedFileId(
  contentId: string,
  fileId: number | null | undefined,
): void {
  if (!contentId || fileId == null || !Number.isFinite(fileId) || fileId <= 0) return;

  const map = readMap();
  const key = entryKey(contentId);
  // Re-insert so the map keeps most-recently-used order before trimming.
  delete map[key];
  map[key] = { fileId, updatedAt: Date.now() };

  const entries = Object.entries(map);
  if (entries.length > MAX_ENTRIES) {
    entries.sort(([, a], [, b]) => a.updatedAt - b.updatedAt);
    for (const [staleKey] of entries.slice(0, entries.length - MAX_ENTRIES)) {
      delete map[staleKey];
    }
  }

  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(map));
  } catch {
    // Storage full or unavailable: the page falls back to the server's row.
  }
}
