import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setAccessToken, setProfileId, setProfileToken } from "@/api/client";
import { DirectDownloadError, prepareDirectDownload } from "./directDownloads";

const URL_PATH = "/api/v2/direct-download?file_id=42&token=original-account-token";

let createObjectURL: ReturnType<typeof vi.spyOn>;

beforeEach(() => {
  setAccessToken("original-account-token");
  setProfileId("profile-one");
  setProfileToken("pin-one");
  // The transfer is the browser's own navigation; preparation must never mint
  // an object URL because there is nothing here to revoke.
  createObjectURL = vi
    .spyOn(URL, "createObjectURL")
    .mockImplementation(() => "blob:never") as ReturnType<typeof vi.spyOn>;
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  setAccessToken(null);
  setProfileId(null);
  setProfileToken(null);
});

describe("direct download preflight", () => {
  it("ranged-probes the captured URL and returns the authenticated save link", async () => {
    const fetch = vi.fn().mockResolvedValue(
      new Response(null, {
        status: 206,
        headers: { "Content-Disposition": `attachment; filename="Movie.mp4"` },
      }),
    );
    vi.stubGlobal("fetch", fetch);

    const download = await prepareDirectDownload(42, () => true, new AbortController().signal);

    // One request only: a ranged preflight. The transfer is the link's own
    // navigation, not an awaited fetch.
    expect(fetch).toHaveBeenCalledTimes(1);
    expect(fetch.mock.calls[0]?.[0]).toBe(URL_PATH);
    expect(fetch.mock.calls[0]?.[1]).toMatchObject({
      method: "GET",
      cache: "no-store",
      headers: { Range: "bytes=0-0" },
    });
    expect(download.url).toBe(URL_PATH);
    expect(download.filename).toBe("Movie.mp4");
    expect(createObjectURL).not.toHaveBeenCalled();
  });

  it("falls back to the caller filename when the server sends no disposition", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(null, { status: 200 })));

    const download = await prepareDirectDownload(
      42,
      () => true,
      new AbortController().signal,
      "Episode.mkv",
    );
    expect(download.filename).toBe("Episode.mkv");
    expect(createObjectURL).not.toHaveBeenCalled();
  });

  it.each(["account", "profile", "pin", "closed"])(
    "refuses a late probe after %s authority changes",
    async (kind) => {
      let resolve!: (r: Response) => void;
      vi.stubGlobal(
        "fetch",
        vi.fn(
          () =>
            new Promise<Response>((r) => {
              resolve = r;
            }),
        ),
      );
      let current = true;
      const pending = prepareDirectDownload(42, () => current, new AbortController().signal);
      if (kind === "account") setAccessToken("replacement");
      if (kind === "profile") setProfileId("profile-two");
      if (kind === "pin") setProfileToken("pin-two");
      if (kind === "closed") current = false;
      resolve(new Response(null, { status: 200 }));
      await expect(pending).rejects.toThrow();
      expect(createObjectURL).not.toHaveBeenCalled();
    },
  );

  it("aborts an in-flight probe when its controller fires", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(
        (_url: string, init?: RequestInit) =>
          new Promise<Response>((_resolve, reject) => {
            init?.signal?.addEventListener("abort", () =>
              reject(new DOMException("aborted", "AbortError")),
            );
          }),
      ),
    );
    const controller = new AbortController();
    const pending = prepareDirectDownload(42, () => true, controller.signal);
    controller.abort();
    await expect(pending).rejects.toThrow();
    expect(createObjectURL).not.toHaveBeenCalled();
  });

  it("never fetches when the signal is already aborted", async () => {
    const fetch = vi.fn();
    vi.stubGlobal("fetch", fetch);
    const controller = new AbortController();
    controller.abort();
    await expect(prepareDirectDownload(42, () => true, controller.signal)).rejects.toThrow();
    expect(fetch).not.toHaveBeenCalled();
    expect(createObjectURL).not.toHaveBeenCalled();
  });

  it("reports a refused probe with a concrete, status-specific reason", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(null, { status: 403 })));
    const pending = prepareDirectDownload(42, () => true, new AbortController().signal);
    await expect(pending).rejects.toBeInstanceOf(DirectDownloadError);
    await expect(pending).rejects.toMatchObject({
      status: 403,
      message: "Downloads are not allowed for this account.",
    });
    expect(createObjectURL).not.toHaveBeenCalled();
  });

  it("does not attempt a transfer after a failed probe", async () => {
    const fetch = vi.fn().mockRejectedValue(new TypeError("network"));
    vi.stubGlobal("fetch", fetch);
    await expect(
      prepareDirectDownload(42, () => true, new AbortController().signal),
    ).rejects.toThrow();
    expect(fetch).toHaveBeenCalledTimes(1);
    expect(createObjectURL).not.toHaveBeenCalled();
  });
});
