/**
 * True when a subtitle fetch response is the server's `subtitle_source_changed`
 * 409 (a virtual release rotated under this plan). The client must refresh the
 * plan's subtitle inventory; retrying the same URL can never succeed.
 */
export async function isSubtitleSourceChanged(resp: Response): Promise<boolean> {
  if (resp.status !== 409) return false;
  try {
    const body = (await resp.json()) as {
      error?: string;
      type?: string;
      title?: string;
      status?: number;
    };
    if (body?.error === "subtitle_source_changed") return true;
    // v2 serves the bridge through RFC problem documents, which do not carry
    // the legacy `error` field. A conflict from a subtitle fetch is the same
    // source-rotation signal; the subtitle route has no other 409 outcome.
    return body?.status === 409 && body?.type?.endsWith("/problems/conflict") === true;
  } catch {
    return false;
  }
}
