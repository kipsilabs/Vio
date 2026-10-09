import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setAccessToken, setProfileId, setProfileToken } from "@/api/client";
import { DirectDownloadError, launchDirectDownload } from "./directDownloads";

let click: ReturnType<typeof vi.spyOn>;
beforeEach(() => {
  setAccessToken("original-account-token");
  setProfileId("profile-one");
  setProfileToken("pin-one");
  click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
});
afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  setAccessToken(null);
  setProfileId(null);
  setProfileToken(null);
});

describe("direct download navigation", () => {
  it("probes with one ranged GET, then navigates to the same captured account URL", async () => {
    const fetch = vi.fn().mockResolvedValue(new Response(null, { status: 206 }));
    vi.stubGlobal("fetch", fetch);
    await launchDirectDownload(42, () => true);
    // A single request: no HEAD probe followed by a separately reauthorized GET.
    expect(fetch).toHaveBeenCalledTimes(1);
    const call = fetch.mock.calls[0];
    if (!call) throw new Error("Expected one GET call");
    const url = "/api/v2/direct-download?file_id=42&token=original-account-token";
    expect(call[0]).toBe(url);
    expect(call[1]).toMatchObject({
      method: "GET",
      cache: "no-store",
      headers: { Range: "bytes=0-0" },
    });
    expect(click).toHaveBeenCalledTimes(1);
    expect((click.mock.instances[0] as HTMLAnchorElement).getAttribute("href")).toBe(url);
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
      const pending = launchDirectDownload(42, () => current);
      if (kind === "account") setAccessToken("replacement");
      if (kind === "profile") setProfileId("profile-two");
      if (kind === "pin") setProfileToken("pin-two");
      if (kind === "closed") current = false;
      resolve(new Response(null, { status: 200 }));
      await expect(pending).rejects.toThrow();
      expect(click).not.toHaveBeenCalled();
    },
  );

  it("refuses refused statuses with a concrete, status-specific reason", async () => {
    const fetch = vi.fn().mockResolvedValue(new Response(null, { status: 403 }));
    vi.stubGlobal("fetch", fetch);
    const pending = launchDirectDownload(42, () => true);
    await expect(pending).rejects.toBeInstanceOf(DirectDownloadError);
    await expect(pending).rejects.toMatchObject({
      status: 403,
      message: "Downloads are not allowed for this account.",
    });
    expect(fetch).toHaveBeenCalledTimes(1);
    expect(click).not.toHaveBeenCalled();
  });

  it.each([401, 500, 503])("reports status %s instead of a silent no-op", async (status) => {
    const fetch = vi.fn().mockResolvedValue(new Response(null, { status }));
    vi.stubGlobal("fetch", fetch);
    await expect(launchDirectDownload(42, () => true)).rejects.toBeInstanceOf(DirectDownloadError);
    expect(click).not.toHaveBeenCalled();
  });

  it("reports a blocked programmatic click instead of doing nothing", async () => {
    const fetch = vi.fn().mockResolvedValue(new Response(null, { status: 200 }));
    vi.stubGlobal("fetch", fetch);
    click.mockImplementation(() => {
      throw new Error("blocked");
    });
    await expect(launchDirectDownload(42, () => true)).rejects.toMatchObject({
      name: "DirectDownloadError",
      message: "Your browser blocked the download. Allow downloads for this site and try again.",
    });
  });

  it("does not turn a failed probe into a transfer", async () => {
    const fetch = vi.fn().mockRejectedValue(new TypeError("network"));
    vi.stubGlobal("fetch", fetch);
    await expect(launchDirectDownload(42, () => true)).rejects.toThrow();
    expect(fetch).toHaveBeenCalledTimes(1);
    expect(click).not.toHaveBeenCalled();
  });
});
