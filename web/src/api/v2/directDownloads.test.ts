import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setAccessToken, setProfileId, setProfileToken } from "@/api/client";
import { launchDirectDownload } from "./directDownloads";
import { pendingResponse } from "./pendingFetch.test-support";

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
  // The link check runs after the mint and the probe check after the HEAD
  // probe; each stage refuses every kind of authority change.
  it.each(
    (["link", "probe"] as const).flatMap((stage) =>
      (["account", "profile", "pin", "closed"] as const).map((kind) => [stage, kind] as const),
    ),
  )("refuses a late %s after %s authority changes", async (stage, kind) => {
    const late = pendingResponse();
    const fetch = vi.fn<typeof globalThis.fetch>();
    if (stage === "link") fetch.mockReturnValue(late.promise);
    else fetch.mockResolvedValueOnce(linkResponse()).mockReturnValueOnce(late.promise);
    vi.stubGlobal("fetch", fetch);
    let current = true;
    const pending = launchDirectDownload(42, () => current);
    await vi.waitFor(() => expect(fetch).toHaveBeenCalledTimes(stage === "link" ? 1 : 2));
    if (kind === "account") setAccessToken("replacement");
    if (kind === "profile") setProfileId("profile-two");
    if (kind === "pin") setProfileToken("pin-two");
    if (kind === "closed") current = false;
    late.finish(stage === "link" ? linkResponse() : new Response(null, { status: 200 }));
    await expect(pending).rejects.toThrow();
    if (stage === "link") expect(fetch).toHaveBeenCalledTimes(1);
    expect(click).not.toHaveBeenCalled();
  });

  it("carries the problem code and a refresh prompt for a stale file_id", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(
          JSON.stringify({
            type: "https://siloserver.org/docs/api/v2/problems/file_unavailable",
            title: "File unavailable",
            status: 404,
            detail: "This file is no longer available",
          }),
          { status: 404, headers: { "Content-Type": "application/problem+json" } },
        ),
      ),
    );
    const pending = prepareDirectDownload(42, () => true, new AbortController().signal);
    await expect(pending).rejects.toMatchObject({
      status: 404,
      code: "file_unavailable",
      message: "This version is no longer available. Refresh the list to see current versions.",
    });
  });

  it("keeps the denial text for a file-level access refusal", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(
          JSON.stringify({
            type: "https://siloserver.org/docs/api/v2/problems/file_access_denied",
            title: "File access denied",
            status: 403,
          }),
          { status: 403, headers: { "Content-Type": "application/problem+json" } },
        ),
      ),
    );
    await expect(
      prepareDirectDownload(42, () => true, new AbortController().signal),
    ).rejects.toMatchObject({
      status: 403,
      code: "file_access_denied",
      message: "You do not have access to this file.",
    });
  });

  it("classifies a stale refusal as needing a version refresh and a denial as not", () => {
    expect(isStaleFileError(new DirectDownloadError(404, "x", "file_unavailable"))).toBe(true);
    // A bare 404 on the direct URL has the same corrective action.
    expect(isStaleFileError(new DirectDownloadError(404, "x"))).toBe(true);
    expect(isStaleFileError(new DirectDownloadError(403, "x", "file_access_denied"))).toBe(false);
    expect(isStaleFileError(new Error("network"))).toBe(false);
  });
});
