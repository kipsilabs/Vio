import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setAccessToken, setProfileId, setProfileToken } from "@/api/client";
import {
  BUFFERED_SAVE_LIMIT_BYTES,
  canSavePreparedDownload,
  createPreparedDownload,
  DownloadPreparationError,
  getPreparedDownload,
  getPreparedDownloadFile,
  readBoundedSaveBlob,
  savePreparedDownload,
  SaveCancelledError,
  type PreparedDownloadEntry,
} from "./downloadPreparation";

const entry: PreparedDownloadEntry = {
  id: "dl-1",
  content_id: "mv_1",
  media_file_id: "42",
  file_size: 1024,
  bytes_sent: 0,
  kind: "queued",
  status: "preparing",
  quality: "original",
  effective_quality: "original",
  delivery_format: "original",
  target_bitrate_kbps: 0,
  revision: 1,
  created_at: "2026-01-01T00:00:00Z",
};

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function problemResponse(status: number): Response {
  return new Response(JSON.stringify({ type: "about:blank", title: "gone", status }), {
    status,
    headers: { "Content-Type": "application/problem+json" },
  });
}

beforeEach(() => {
  setAccessToken("account-token");
  setProfileId("profile-one");
  setProfileToken("pin-one");
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  setAccessToken(null);
  setProfileId(null);
  setProfileToken(null);
});

describe("createPreparedDownload", () => {
  const created = (items: PreparedDownloadEntry[]) =>
    jsonResponse({ items, skipped: [], page: { has_more: false } }, 202);
  const registry = (items: PreparedDownloadEntry[]) =>
    jsonResponse({ items, page: { has_more: false } });

  it("sends expected_revision zero for a fresh managed single download", async () => {
    const fetch = vi.fn().mockResolvedValue(created([entry]));
    vi.stubGlobal("fetch", fetch);

    const got = await createPreparedDownload({
      contentId: "mv_1",
      mediaFileId: 42,
      quality: "original",
    });

    expect(got).toMatchObject({ id: "dl-1" });
    expect(fetch).toHaveBeenCalledTimes(1);
    const [url, init] = fetch.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v2/downloads");
    expect(init.method).toBe("POST");
    const body = JSON.parse(String(init.body)) as Record<string, unknown>;
    expect(body).toMatchObject({ content_id: "mv_1", media_file_id: "42", expected_revision: 0 });
    // Revision zero asserts absence, so it must not name an entry.
    expect(body.expected_download_id).toBeUndefined();
  });

  it("reconciles a conflict and retries with the existing entry's id and revision", async () => {
    const existing = { ...entry, id: "dl-9", revision: 3 };
    const fetch = vi
      .fn()
      .mockResolvedValueOnce(problemResponse(409))
      .mockResolvedValueOnce(registry([existing]))
      .mockResolvedValueOnce(created([{ ...entry, id: "dl-9", revision: 4 }]));
    vi.stubGlobal("fetch", fetch);

    const got = await createPreparedDownload({
      contentId: "mv_1",
      mediaFileId: 42,
      quality: "original",
    });

    expect(got.id).toBe("dl-9");
    expect(fetch).toHaveBeenCalledTimes(3);
    const [listUrl] = fetch.mock.calls[1] as [string];
    expect(listUrl).toBe("/api/v2/downloads");
    const retry = fetch.mock.calls[2] as [string, RequestInit];
    const body = JSON.parse(String(retry[1].body)) as Record<string, unknown>;
    expect(body).toMatchObject({ expected_revision: 3, expected_download_id: "dl-9" });
  });

  it("reports an actionable refusal when the entry changes before the retry lands", async () => {
    const existing = { ...entry, id: "dl-9", revision: 3 };
    const fetch = vi
      .fn()
      .mockResolvedValueOnce(problemResponse(409))
      .mockResolvedValueOnce(registry([existing]))
      .mockResolvedValueOnce(problemResponse(409));
    vi.stubGlobal("fetch", fetch);

    const attempt = createPreparedDownload({
      contentId: "mv_1",
      mediaFileId: 42,
      quality: "original",
    });
    await expect(attempt).rejects.toBeInstanceOf(DownloadPreparationError);
    await expect(attempt).rejects.toThrow(/changed on the server/i);
  });

  it("matches a conflict to an episode entry by series and episode identity", async () => {
    const existing = {
      ...entry,
      id: "dl-ep",
      content_id: "sr_9",
      episode_id: "ep_1",
      revision: 2,
    };
    const fetch = vi
      .fn()
      .mockResolvedValueOnce(problemResponse(409))
      .mockResolvedValueOnce(registry([existing]))
      .mockResolvedValueOnce(created([{ ...existing, revision: 3 }]));
    vi.stubGlobal("fetch", fetch);

    await createPreparedDownload({
      contentId: "sr_9",
      episodeId: "ep_1",
      mediaFileId: 43,
      quality: "original",
    });

    const retry = fetch.mock.calls[2] as [string, RequestInit];
    const body = JSON.parse(String(retry[1].body)) as Record<string, unknown>;
    expect(body).toMatchObject({
      content_id: "sr_9",
      episode_id: "ep_1",
      media_file_id: "43",
      expected_revision: 2,
      expected_download_id: "dl-ep",
    });
  });

  it("refuses with a clear reason when the conflict has no matching entry", async () => {
    const fetch = vi
      .fn()
      .mockResolvedValueOnce(problemResponse(409))
      .mockResolvedValueOnce(registry([{ ...entry, id: "dl-other", content_id: "mv_other" }]));
    vi.stubGlobal("fetch", fetch);

    await expect(
      createPreparedDownload({ contentId: "mv_1", mediaFileId: 42, quality: "original" }),
    ).rejects.toThrow(/already exists/i);
  });
});

describe("getPreparedDownload", () => {
  it("reads the single entry by id instead of paging the registry", async () => {
    const fetch = vi.fn().mockResolvedValue(jsonResponse(entry));
    vi.stubGlobal("fetch", fetch);

    const got = await getPreparedDownload("dl-1");

    expect(got).toMatchObject({ id: "dl-1", status: "preparing" });
    expect(fetch).toHaveBeenCalledTimes(1);
    const [url] = fetch.mock.calls[0] as [string];
    expect(url).toBe("/api/v2/downloads/dl-1");
    // A registry page would have carried a limit query; a per-entry read does not.
    expect(url).not.toContain("limit=");
  });

  it("reports a 404 as a gone entry so the caller stops polling", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(problemResponse(404)));
    await expect(getPreparedDownload("dl-1")).resolves.toBeNull();
  });

  it("rethrows a transient failure so the caller retries", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(problemResponse(503)));
    await expect(getPreparedDownload("dl-1")).rejects.toBeTruthy();
  });
});

describe("savePreparedDownload", () => {
  it("streams the prepared bytes to a picked file without buffering the whole title", async () => {
    const payload = new Uint8Array([1, 2, 3, 4, 5, 6]);
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(payload, {
          status: 200,
          headers: { "Content-Disposition": `attachment; filename="Movie.mp4"` },
        }),
      ),
    );

    const written: Uint8Array[] = [];
    const writable = {
      write: vi.fn(async (chunk: Uint8Array) => {
        written.push(chunk);
      }),
      close: vi.fn(async () => undefined),
      abort: vi.fn(async () => undefined),
    };
    const picker = vi.fn(async () => ({ createWritable: async () => writable }));
    vi.stubGlobal("showSaveFilePicker", picker);

    const outcome = await savePreparedDownload("dl-1", "fallback.mp4");

    expect(outcome).toBe("streamed");
    // The picker opens before the fetch (to keep user activation), so it
    // suggests the caller's filename, not the server disposition.
    expect(picker).toHaveBeenCalledWith({ suggestedName: "fallback.mp4" });
    const joined = new Uint8Array(written.reduce((n, c) => n + c.length, 0));
    let offset = 0;
    for (const chunk of written) {
      joined.set(chunk, offset);
      offset += chunk.length;
    }
    expect(Array.from(joined)).toEqual(Array.from(payload));
    expect(writable.close).toHaveBeenCalledTimes(1);
    expect(writable.abort).not.toHaveBeenCalled();
  });

  it("opens the save picker before fetching so user activation is intact", async () => {
    const order: string[] = [];
    const writable = {
      write: vi.fn(async () => undefined),
      close: vi.fn(async () => undefined),
      abort: vi.fn(async () => undefined),
    };
    vi.stubGlobal(
      "showSaveFilePicker",
      vi.fn(async () => {
        order.push("picker");
        return { createWritable: async () => writable };
      }),
    );
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        order.push("fetch");
        return new Response(new Uint8Array([1]), { status: 200 });
      }),
    );

    await savePreparedDownload("dl-1", "Movie.mp4");

    expect(order).toEqual(["picker", "fetch"]);
  });

  it("treats a cancelled save picker as a silent, non-error outcome", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(new Response(new Uint8Array([1]), { status: 200 })),
    );
    vi.stubGlobal(
      "showSaveFilePicker",
      vi.fn().mockRejectedValue(new DOMException("user cancelled", "AbortError")),
    );

    await expect(savePreparedDownload("dl-1", "Movie.mp4")).rejects.toBeInstanceOf(
      SaveCancelledError,
    );
  });

  it("buffers a browser-managed blob save when streaming is unavailable", async () => {
    expect((window as { showSaveFilePicker?: unknown }).showSaveFilePicker).toBeUndefined();
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(new Response(new Uint8Array([9, 9]), { status: 200 })),
    );
    const createObjectURL = vi.spyOn(URL, "createObjectURL").mockImplementation(() => "blob:saved");

    const outcome = await savePreparedDownload("dl-1", "Movie.mp4");

    expect(outcome).toBe("buffered");
    expect(createObjectURL).toHaveBeenCalledTimes(1);
  });

  it("maps an expired prepared file to an actionable reason", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(problemResponse(409)));
    await expect(savePreparedDownload("dl-1", "Movie.mp4")).rejects.toThrow(
      /expired or no longer available/i,
    );
  });

  it("refuses to buffer a file larger than the browser budget", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(new Uint8Array([1, 2]), {
          status: 200,
          headers: { "Content-Length": String(BUFFERED_SAVE_LIMIT_BYTES + 1) },
        }),
      ),
    );
    const createObjectURL = vi.spyOn(URL, "createObjectURL").mockImplementation(() => "blob:saved");

    await expect(savePreparedDownload("dl-1", "Movie.mp4")).rejects.toThrow(/too large/i);
    expect(createObjectURL).not.toHaveBeenCalled();
  });
});

describe("readBoundedSaveBlob", () => {
  it("reads a body within the budget into a blob", async () => {
    const blob = await readBoundedSaveBlob(
      new Response(new Uint8Array([1, 2, 3, 4])),
      undefined,
      8,
    );
    expect(blob.size).toBe(4);
  });

  it("refuses an oversized advertised body before reading it", async () => {
    const res = new Response("small fixture", {
      headers: { "Content-Length": "9" },
    });
    await expect(readBoundedSaveBlob(res, undefined, 8)).rejects.toThrow(/too large/i);
  });

  it("enforces the budget on the body when the advertised length is absent or understated", async () => {
    await expect(
      readBoundedSaveBlob(new Response(new Uint8Array([1, 2])), undefined, 1),
    ).rejects.toThrow(/too large/i);
  });
});

describe("canSavePreparedDownload", () => {
  it("caps an over-budget file only when the browser cannot stream", () => {
    expect((window as { showSaveFilePicker?: unknown }).showSaveFilePicker).toBeUndefined();
    expect(canSavePreparedDownload(BUFFERED_SAVE_LIMIT_BYTES + 1)).toBe(false);
    expect(canSavePreparedDownload(BUFFERED_SAVE_LIMIT_BYTES)).toBe(true);
    expect(canSavePreparedDownload(undefined)).toBe(true);

    vi.stubGlobal("showSaveFilePicker", vi.fn());
    expect(canSavePreparedDownload(BUFFERED_SAVE_LIMIT_BYTES + 1)).toBe(true);
  });
});

describe("getPreparedDownloadFile", () => {
  const headResponse = (status: number, headers: Record<string, string> = {}) =>
    new Response(null, { status, headers });

  it("probes the file with the session's authority", async () => {
    const fetch = vi.fn().mockResolvedValue(
      headResponse(200, {
        "Content-Length": "1024",
        "Content-Disposition": 'attachment; filename="Server.mkv"',
      }),
    );
    vi.stubGlobal("fetch", fetch);

    const got = await getPreparedDownloadFile("dl-1", "Hint.mp4");

    expect(got).toEqual({ id: "dl-1", filename: "Server.mkv", fileSize: 1024 });
    const [url, init] = fetch.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v2/downloads/dl-1/file");
    expect(init.method).toBe("HEAD");
    expect(init.cache).toBe("no-store");
    const headers = new Headers(init.headers);
    expect(headers.get("Authorization")).toBe("Bearer account-token");
    expect(headers.get("X-Profile-Id")).toBe("profile-one");
    expect(headers.get("X-Profile-Token")).toBe("pin-one");
  });

  it("parses an RFC 5987 disposition before falling back to the hint", async () => {
    const encoded = vi.fn().mockResolvedValue(
      headResponse(200, {
        "Content-Disposition": "attachment; filename*=UTF-8''Na%C3%AFve%20Movie.mp4",
      }),
    );
    vi.stubGlobal("fetch", encoded);
    await expect(getPreparedDownloadFile("dl-1", "Hint.mp4")).resolves.toMatchObject({
      filename: "Naïve Movie.mp4",
      fileSize: undefined,
    });

    const bare = vi.fn().mockResolvedValue(headResponse(200, { "Content-Length": "0" }));
    vi.stubGlobal("fetch", bare);
    await expect(getPreparedDownloadFile("dl-1", "Hint.mp4")).resolves.toEqual({
      id: "dl-1",
      filename: "Hint.mp4",
      fileSize: undefined,
    });

    await expect(getPreparedDownloadFile("dl-1", "")).resolves.toMatchObject({
      filename: "download.bin",
    });
  });

  it("classifies a permanent refusal as non-retryable", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(problemResponse(404)));

    const attempt = getPreparedDownloadFile("dl-1", "Movie.mp4");
    await expect(attempt).rejects.toBeInstanceOf(DownloadPreparationError);
    await expect(attempt).rejects.toMatchObject({
      status: 404,
      retryable: false,
      message: "This download is expired or no longer available. Prepare it again.",
    });
  });

  it("classifies a server failure and a transport failure as retryable", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(problemResponse(503)));
    await expect(getPreparedDownloadFile("dl-1", "Movie.mp4")).rejects.toMatchObject({
      status: 503,
      retryable: true,
    });

    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new TypeError("fetch failed")));
    await expect(getPreparedDownloadFile("dl-1", "Movie.mp4")).rejects.toMatchObject({
      retryable: true,
    });
  });

  it("propagates an abort instead of reporting a refusal", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new DOMException("aborted", "AbortError")));

    await expect(getPreparedDownloadFile("dl-1", "Movie.mp4")).rejects.toMatchObject({
      name: "AbortError",
    });
  });
});
