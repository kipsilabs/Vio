import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setAccessToken, setProfileId } from "@/api/client";
import { installPolicyStorageMocks } from "@/pages/admin-policy/policyTestUtils";
import { deleteDownloadEntry, fetchDownloadCapability, listDownloads } from "./downloadRegistry";

/** A registry row with the fields the list reconciliation reads. */
const registryRow = (id: string) => ({
  id,
  content_id: "mv_1",
  media_file_id: "42",
  file_size: 1,
  bytes_sent: 0,
  kind: "queued",
  status: "ready",
  quality: "original",
  effective_quality: "original",
  delivery_format: "original",
  target_bitrate_kbps: 0,
  revision: 1,
  created_at: "2026-01-01T00:00:00Z",
});

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    headers: { "Content-Type": "application/json" },
  });
}

describe("download registry browser boundary", () => {
  beforeEach(() => {
    installPolicyStorageMocks();
    setAccessToken("synthetic-download-token");
    setProfileId("reader-one");
  });
  afterEach(() => vi.unstubAllGlobals());
  it("uses the v2 capability and retains proxy delivery", async () => {
    const fetchMock = vi
      .fn<typeof fetch>()
      .mockResolvedValue(
        new Response(
          JSON.stringify({ enabled: true, proxy_delivery: true, ordered_status: true }),
          { headers: { "Content-Type": "application/json" } },
        ),
      );
    vi.stubGlobal("fetch", fetchMock);
    expect(await fetchDownloadCapability()).toMatchObject({
      proxy_delivery: true,
      ordered_status: true,
    });
    expect(String(fetchMock.mock.calls[0]?.[0])).toContain("/api/v2/capabilities/downloads");
  });
  it("pages the registry so a reconcile sees every managed entry", async () => {
    const fetchMock = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(
        jsonResponse({ items: [registryRow("dl-1")], page: { has_more: true, next_cursor: "c1" } }),
      )
      .mockResolvedValueOnce(
        jsonResponse({ items: [registryRow("dl-2")], page: { has_more: false } }),
      );
    vi.stubGlobal("fetch", fetchMock);

    const entries = await listDownloads();

    expect(entries.map((row) => row.id)).toEqual(["dl-1", "dl-2"]);
    expect(String(fetchMock.mock.calls[0]?.[0])).toContain("/api/v2/downloads");
    expect(String(fetchMock.mock.calls[1]?.[0])).toContain("cursor=c1");
  });
  it("retains the browser device and captured profile authority on deletion", async () => {
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);
    await deleteDownloadEntry("entry");
    expect(String(fetchMock.mock.calls[0]?.[0])).toContain("/api/v2/downloads/entry");
    const headers = new Headers(fetchMock.mock.calls[0]?.[1]?.headers);
    expect(headers.get("X-Profile-Id")).toBe("reader-one");
    expect(headers.get("X-Silo-Device-Id")).toBeTruthy();
  });
  it("does not publish a delayed capability after profile switch", async () => {
    let complete: ((response: Response) => void) | undefined;
    vi.stubGlobal(
      "fetch",
      vi.fn<typeof fetch>().mockReturnValue(
        new Promise((resolve) => {
          complete = resolve;
        }),
      ),
    );
    const pending = fetchDownloadCapability();
    await vi.waitFor(() => expect(complete).toBeDefined());
    setProfileId("reader-two");
    complete!(
      new Response(JSON.stringify({ enabled: true }), {
        headers: { "Content-Type": "application/json" },
      }),
    );
    await expect(pending).rejects.toMatchObject({ name: "StaleApiRequestContextError" });
  });
});
