/** Feedback shown under the Prowlarr URL field. */
export interface ProwlarrURLFeedback {
  tone: "ok" | "warn";
  message: string;
}

const PROWLARR_BASE_URL_ONLY = "Enter the base URL only — no indexer path or query string";
const PROWLARR_FULL_BASE_URL = "Enter the full base URL, including http:// or https://";

/**
 * Client-side mirror of the server's validateProwlarrBaseURL. The setting takes
 * Prowlarr's server base URL; an indexer-scoped path or a query string makes the
 * client append "/api/v1/search" onto a newznab route, which Prowlarr answers
 * with 400. Returns null for an empty value so the feedback row is hidden until
 * something is typed.
 */
export function prowlarrURLFeedback(raw: string): ProwlarrURLFeedback | null {
  const trimmed = raw.trim();
  if (trimmed === "") {
    return null;
  }
  let parsed: URL;
  try {
    parsed = new URL(trimmed);
  } catch {
    return { tone: "warn", message: PROWLARR_FULL_BASE_URL };
  }
  if ((parsed.protocol !== "http:" && parsed.protocol !== "https:") || parsed.host === "") {
    return { tone: "warn", message: PROWLARR_FULL_BASE_URL };
  }
  if (parsed.search !== "" || parsed.hash !== "" || hasProwlarrIndexerPath(parsed.pathname)) {
    return { tone: "warn", message: PROWLARR_BASE_URL_ONLY };
  }
  return { tone: "ok", message: "Base URL looks good" };
}

/**
 * Mirrors the server's hasIndexerPath: a trailing numeric segment is Prowlarr's
 * indexer id, and "api", "newznab" and "torznab" segments are the search proxy
 * routes the client would otherwise duplicate. A reverse-proxy subpath such as
 * /prowlarr is a legitimate base and is not flagged.
 */
function hasProwlarrIndexerPath(pathname: string): boolean {
  const segments = pathname.replace(/^\/+|\/+$/g, "").split("/");
  for (let i = 0; i < segments.length; i += 1) {
    const segment = (segments[i] ?? "").toLowerCase();
    if (segment === "") continue;
    if (segment === "api" || segment === "newznab" || segment === "torznab") {
      return true;
    }
    if (i === segments.length - 1 && /^\d+$/.test(segment)) {
      return true;
    }
  }
  return false;
}
