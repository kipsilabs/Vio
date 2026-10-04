import type { DecodeErrorCarrier } from "./decode-failure";

/**
 * Retryable transcode-manifest code the server sets when the encoder is alive
 * but slow (upstream stall, slow probe). Unlike a dead encoder (`unavailable`),
 * polling the manifest again later can succeed, so the player reloads the
 * source outside the fatal network budget instead of replanning.
 */
export const MANIFEST_NOT_READY_CODE = "not_ready_retry";

/** Default poll delay when the server sends no Retry-After. */
export const MANIFEST_NOT_READY_DEFAULT_DELAY_MS = 2000;

/** Upper bound for one poll-back delay; the encoder-side budget stays 30s. */
export const MANIFEST_NOT_READY_MAX_DELAY_MS = 5000;

function retryAfterMs(carrier: DecodeErrorCarrier): number | undefined {
  const details = carrier.networkDetails as
    | { headers?: { get?: (name: string) => string | null } }
    | null
    | undefined;
  const get = details?.headers?.get;
  if (typeof get !== "function") return undefined;
  const raw = get.call(details?.headers, "retry-after");
  if (raw == null) return undefined;
  const seconds = Number(raw);
  if (!Number.isFinite(seconds) || seconds < 0) return undefined;
  return Math.min(seconds * 1000, MANIFEST_NOT_READY_MAX_DELAY_MS);
}

/**
 * Reports whether a fatal hls.js network error is the server's retryable
 * not-ready manifest rather than a transport failure. The 503 alone is not
 * sufficient: only the `not_ready_retry` code authorizes poll-back.
 */
export function isServerManifestNotReady(carrier: DecodeErrorCarrier | null | undefined): boolean {
  if (!carrier) return false;
  const status =
    carrier.response?.code ??
    (carrier.networkDetails as { status?: unknown } | null | undefined)?.status;
  if (status !== 503) return false;
  const code = (() => {
    let data = carrier.response?.data;
    if (typeof data === "string") {
      if (data.trim() === "") return undefined;
      try {
        data = JSON.parse(data);
      } catch {
        return undefined;
      }
    }
    if (!data || typeof data !== "object") return undefined;
    const envelope = data as { error?: unknown; code?: unknown };
    if (typeof envelope.error === "string") return envelope.error;
    if (typeof envelope.code === "string") return envelope.code;
    return undefined;
  })();
  return code === MANIFEST_NOT_READY_CODE;
}

/** Poll delay for a not-ready manifest: Retry-After when sane, else default. */
export function manifestRetryAfterMs(carrier: DecodeErrorCarrier | null | undefined): number {
  if (!carrier) return MANIFEST_NOT_READY_DEFAULT_DELAY_MS;
  return retryAfterMs(carrier) ?? MANIFEST_NOT_READY_DEFAULT_DELAY_MS;
}
