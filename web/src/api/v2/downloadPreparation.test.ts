import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setAccessToken, setProfileId, setProfileToken } from "@/api/client";
import {
  getPreparedDownload,
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
});
