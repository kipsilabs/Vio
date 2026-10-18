import { beforeEach, describe, expect, it, vi } from "vitest";

const mocks = vi.hoisted(() => ({
  fetchWithSession: vi.fn(),
}));

vi.mock("@/api/client", () => ({
  fetchWithSession: mocks.fetchWithSession,
}));

import { fetchVirtualLibraryCapability, VIRTUAL_LIBRARY_CAPABILITY_PATH } from "./virtualLibrary";

function sessionResult(res: Response) {
  return { res, requestProfileId: null, requestProfileToken: null };
}

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

beforeEach(() => {
  mocks.fetchWithSession.mockReset();
});

describe("fetchVirtualLibraryCapability", () => {
  it("GETs the capability and maps the published flags", async () => {
    mocks.fetchWithSession.mockResolvedValue(
      sessionResult(
        jsonResponse({
          state: "available",
          indexer_search: true,
          indexer_request: true,
          discover_before_playback: true,
          prepare_before_playback: true,
          revision: "rev-7",
        }),
      ),
    );

    const capability = await fetchVirtualLibraryCapability();

    const [url, init] = mocks.fetchWithSession.mock.calls[0] as [string, RequestInit];
    expect(url).toBe(VIRTUAL_LIBRARY_CAPABILITY_PATH);
    expect(url).toBe("/api/v2/capabilities/virtual-library");
    expect(init.method).toBe("GET");
    expect(capability).toEqual({
      state: "available",
      indexer_search: true,
      indexer_request: true,
      discover_before_playback: true,
      prepare_before_playback: true,
      revision: "rev-7",
    });
  });

  it("treats absent indexer flags as unavailable so the UI hides", async () => {
    mocks.fetchWithSession.mockResolvedValue(sessionResult(jsonResponse({ state: "available" })));

    const capability = await fetchVirtualLibraryCapability();

    expect(capability.indexer_search).toBe(false);
    expect(capability.indexer_request).toBe(false);
    expect(capability.discover_before_playback).toBe(false);
    expect(capability.prepare_before_playback).toBe(false);
    expect(capability.revision).toBeUndefined();
  });

  it("treats a false indexer_request as unavailable", async () => {
    mocks.fetchWithSession.mockResolvedValue(
      sessionResult(
        jsonResponse({ state: "available", indexer_search: true, indexer_request: false }),
      ),
    );

    const capability = await fetchVirtualLibraryCapability();

    expect(capability.indexer_request).toBe(false);
  });

  it("falls back to an unavailable state when the body omits it", async () => {
    mocks.fetchWithSession.mockResolvedValue(
      sessionResult(jsonResponse({ indexer_request: true })),
    );

    const capability = await fetchVirtualLibraryCapability();

    expect(capability.state).toBe("unavailable");
    expect(capability.indexer_request).toBe(true);
  });

  it("rejects on a non-2xx answer", async () => {
    mocks.fetchWithSession.mockResolvedValue(sessionResult(new Response("", { status: 503 })));

    await expect(fetchVirtualLibraryCapability()).rejects.toThrow("503");
  });

  it("propagates a transport rejection", async () => {
    mocks.fetchWithSession.mockRejectedValue(new Error("network"));

    await expect(fetchVirtualLibraryCapability()).rejects.toThrow("network");
  });
});
