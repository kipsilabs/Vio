import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setAccessToken, setProfileId, setProfileToken } from "@/api/client";
import { DirectDownloadError, prepareDirectDownload } from "./directDownloads";

const URL_PATH = "/api/v2/direct-download?file_id=42&token=original-account-token";

function transferResponse(body = "bytes", headers: Record<string, string> = {}): Response {
  return new Response(body, { status: 200, headers });
}

let createObjectURL: ReturnType<typeof vi.spyOn>;
let revokeObjectURL: ReturnType<typeof vi.spyOn>;

beforeEach(() => {
  setAccessToken("original-account-token");
  setProfileId("profile-one");
  setProfileToken("pin-one");
  // jsdom does not implement object URLs; observe the lifecycle instead.
  createObjectURL = vi
    .spyOn(URL, "createObjectURL")
    .mockImplementation(() => "blob:download-one") as ReturnType<typeof vi.spyOn>;
  revokeObjectURL = vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {}) as ReturnType<
    typeof vi.spyOn
  >;
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  setAccessToken(null);
  setProfileId(null);
  setProfileToken(null);
});

describe("direct download preparation", () => {
  it("probes then transfers the same captured URL and returns a save handle", async () => {
    const fetch = vi
      .fn()
      .mockResolvedValueOnce(new Response(null, { status: 206 }))
      .mockResolvedValueOnce(
        transferResponse("file-bytes", {
          "Content-Disposition": `attachment; filename="Movie.mp4"`,
        }),
      );
    vi.stubGlobal("fetch", fetch);

    const download = await prepareDirectDownload(42, () => true);

    // Two awaited requests: a ranged preflight, then the observable transfer.
    expect(fetch).toHaveBeenCalledTimes(2);
    expect(fetch.mock.calls[0]?.[0]).toBe(URL_PATH);
    expect(fetch.mock.calls[0]?.[1]).toMatchObject({
      method: "GET",
      cache: "no-store",
      headers: { Range: "bytes=0-0" },
    });
    expect(fetch.mock.calls[1]).toEqual([URL_PATH, { method: "GET", cache: "no-store" }]);
    expect(download.url).toBe("blob:download-one");
    expect(download.filename).toBe("Movie.mp4");
    expect(createObjectURL).toHaveBeenCalledTimes(1);

    download.dispose();
    expect(revokeObjectURL).toHaveBeenCalledWith("blob:download-one");
  });

  it("falls back to the caller filename when the server sends no disposition", async () => {
    const fetch = vi
      .fn()
      .mockResolvedValueOnce(new Response(null, { status: 200 }))
      .mockResolvedValueOnce(transferResponse());
    vi.stubGlobal("fetch", fetch);

    const download = await prepareDirectDownload(42, () => true, "Episode.mkv");
    expect(download.filename).toBe("Episode.mkv");
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
      const pending = prepareDirectDownload(42, () => current);
      if (kind === "account") setAccessToken("replacement");
      if (kind === "profile") setProfileId("profile-two");
      if (kind === "pin") setProfileToken("pin-two");
      if (kind === "closed") current = false;
      resolve(new Response(null, { status: 200 }));
      await expect(pending).rejects.toThrow();
      expect(createObjectURL).not.toHaveBeenCalled();
    },
  );

  it.each(["account", "profile", "pin", "closed"])(
    "refuses a late transfer after %s authority changes",
    async (kind) => {
      let resolveTransfer!: (r: Response) => void;
      vi.stubGlobal(
        "fetch",
        vi
          .fn()
          .mockResolvedValueOnce(new Response(null, { status: 206 }))
          .mockImplementationOnce(
            () =>
              new Promise<Response>((r) => {
                resolveTransfer = r;
              }),
          ),
      );
      let current = true;
      const pending = prepareDirectDownload(42, () => current);
      await vi.waitFor(() => expect(resolveTransfer).toBeDefined());
      if (kind === "account") setAccessToken("replacement");
      if (kind === "profile") setProfileId("profile-two");
      if (kind === "pin") setProfileToken("pin-two");
      if (kind === "closed") current = false;
      resolveTransfer(transferResponse());
      await expect(pending).rejects.toThrow();
      expect(createObjectURL).not.toHaveBeenCalled();
    },
  );

  it("reports a refused probe with a concrete, status-specific reason", async () => {
    const fetch = vi.fn().mockResolvedValue(new Response(null, { status: 403 }));
    vi.stubGlobal("fetch", fetch);
    const pending = prepareDirectDownload(42, () => true);
    await expect(pending).rejects.toBeInstanceOf(DirectDownloadError);
    await expect(pending).rejects.toMatchObject({
      status: 403,
      message: "Downloads are not allowed for this account.",
    });
    expect(fetch).toHaveBeenCalledTimes(1);
    expect(createObjectURL).not.toHaveBeenCalled();
  });

  it("reports a refused transfer even though the probe passed", async () => {
    const fetch = vi
      .fn()
      .mockResolvedValueOnce(new Response(null, { status: 206 }))
      .mockResolvedValueOnce(new Response(null, { status: 403 }));
    vi.stubGlobal("fetch", fetch);
    const pending = prepareDirectDownload(42, () => true);
    await expect(pending).rejects.toBeInstanceOf(DirectDownloadError);
    await expect(pending).rejects.toMatchObject({ status: 403 });
    expect(fetch).toHaveBeenCalledTimes(2);
    expect(createObjectURL).not.toHaveBeenCalled();
  });

  it("reports a dropped transfer body instead of leaving a partial file", async () => {
    const fetch = vi
      .fn()
      .mockResolvedValueOnce(new Response(null, { status: 206 }))
      .mockResolvedValueOnce({
        ok: true,
        status: 200,
        blob: () => Promise.reject(new Error("aborted")),
      });
    vi.stubGlobal("fetch", fetch);
    await expect(prepareDirectDownload(42, () => true)).rejects.toMatchObject({
      name: "DirectDownloadError",
      message: "The download stopped before it finished. Try again.",
    });
    expect(createObjectURL).not.toHaveBeenCalled();
  });

  it("does not turn a failed probe into a transfer", async () => {
    const fetch = vi.fn().mockRejectedValue(new TypeError("network"));
    vi.stubGlobal("fetch", fetch);
    await expect(prepareDirectDownload(42, () => true)).rejects.toThrow();
    expect(fetch).toHaveBeenCalledTimes(1);
    expect(createObjectURL).not.toHaveBeenCalled();
  });
});
