import { createElement, type ReactNode } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderHook, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import type { FileVersion } from "@/api/types";

const mocks = vi.hoisted(() => ({
  api: vi.fn(),
}));

vi.mock("@/api/client", () => ({
  api: mocks.api,
}));

import {
  VERSION_LIVENESS_BATCH_SIZE,
  applyVersionAvailability,
  chunkVirtualFileIds,
  fetchVersionLiveness,
  mergeVersionLiveness,
  useVersionLiveness,
} from "./versionLiveness";

function makeVersion(overrides: Partial<FileVersion> = {}): FileVersion {
  return {
    file_id: overrides.file_id ?? 1,
    resolution: overrides.resolution ?? "1080p",
    codec_video: overrides.codec_video ?? "h264",
    codec_audio: overrides.codec_audio ?? "aac",
    hdr: overrides.hdr ?? false,
    container: overrides.container ?? "mkv",
    file_size: overrides.file_size ?? 0,
    duration: overrides.duration ?? 0,
    bitrate: overrides.bitrate ?? 0,
    file_path: overrides.file_path,
    available: overrides.available,
  };
}

function createQueryClient() {
  return new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
}

function createWrapper(queryClient: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return createElement(QueryClientProvider, { client: queryClient }, children);
  };
}

describe("fetchVersionLiveness", () => {
  beforeEach(() => {
    mocks.api.mockReset();
  });

  it("POSTs the file_ids to the batched check endpoint", async () => {
    mocks.api.mockResolvedValue({ results: [] });
    await fetchVersionLiveness([1, 2, 3]);

    expect(mocks.api).toHaveBeenCalledWith("/catalog/versions/check", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ file_ids: [1, 2, 3] }),
    });
  });
});

describe("chunkVirtualFileIds", () => {
  it("only includes virtual versions and sorts them ascending", () => {
    const versions = [
      makeVersion({ file_id: 3, container: "virtual" }),
      makeVersion({ file_id: 1, file_path: "virtual://movie/tt1?result=abc" }),
      makeVersion({ file_id: 2, container: "mkv" }),
    ];
    expect(chunkVirtualFileIds(versions)).toEqual([[1, 3]]);
  });

  it("chunks batches larger than the cap", () => {
    const versions = Array.from({ length: VERSION_LIVENESS_BATCH_SIZE + 5 }, (_, i) =>
      makeVersion({ file_id: i + 1, container: "virtual" }),
    );
    const chunks = chunkVirtualFileIds(versions);
    expect(chunks).toHaveLength(2);
    expect(chunks[0]).toHaveLength(VERSION_LIVENESS_BATCH_SIZE);
    expect(chunks[1]).toEqual([
      VERSION_LIVENESS_BATCH_SIZE + 1,
      VERSION_LIVENESS_BATCH_SIZE + 2,
      VERSION_LIVENESS_BATCH_SIZE + 3,
      VERSION_LIVENESS_BATCH_SIZE + 4,
      VERSION_LIVENESS_BATCH_SIZE + 5,
    ]);
  });

  it("returns no chunks when there are no virtual versions", () => {
    expect(chunkVirtualFileIds([makeVersion({ file_id: 1 })])).toEqual([]);
  });
});

describe("mergeVersionLiveness", () => {
  it("marks a version unavailable from item metadata", () => {
    const versions = [makeVersion({ file_id: 1, available: false })];
    expect(mergeVersionLiveness(versions, undefined).get(1)).toBe(false);
  });

  it("marks a version unavailable from a check result", () => {
    const versions = [makeVersion({ file_id: 1 })];
    const merged = mergeVersionLiveness(versions, {
      results: [{ file_id: 1, available: false }],
    });
    expect(merged.get(1)).toBe(false);
  });

  it("keeps a version available when the check reports it so", () => {
    const versions = [makeVersion({ file_id: 1 })];
    const merged = mergeVersionLiveness(versions, {
      results: [{ file_id: 1, available: true }],
    });
    expect(merged.get(1)).toBe(true);
  });

  it("recovers a version the metadata marked unavailable when the check reports it available", () => {
    const versions = [makeVersion({ file_id: 1, available: false })];
    const merged = mergeVersionLiveness(versions, {
      results: [{ file_id: 1, available: true }],
    });
    expect(merged.get(1)).toBe(true);
  });

  it("keeps a version unavailable when the check confirms the metadata", () => {
    const versions = [makeVersion({ file_id: 1, available: false })];
    const merged = mergeVersionLiveness(versions, {
      results: [{ file_id: 1, available: false }],
    });
    expect(merged.get(1)).toBe(false);
  });

  it("marks a version unavailable when the check reports it so despite metadata saying available", () => {
    const versions = [makeVersion({ file_id: 1, available: true })];
    const merged = mergeVersionLiveness(versions, {
      results: [{ file_id: 1, available: false }],
    });
    expect(merged.get(1)).toBe(false);
  });

  it("leaves virtual versions without a result unknown (absent from the map)", () => {
    const versions = [makeVersion({ file_id: 1, container: "virtual" })];
    expect(mergeVersionLiveness(versions, undefined).has(1)).toBe(false);
  });

  it("does not mark non-virtual versions unavailable", () => {
    const versions = [makeVersion({ file_id: 1 })];
    expect(mergeVersionLiveness(versions, undefined).has(1)).toBe(false);
  });
});

describe("useVersionLiveness", () => {
  beforeEach(() => {
    mocks.api.mockReset();
  });

  it("does not fire while disabled", () => {
    const versions = [makeVersion({ file_id: 1, container: "virtual" })];
    renderHook(() => useVersionLiveness(versions, false), {
      wrapper: createWrapper(createQueryClient()),
    });

    expect(mocks.api).not.toHaveBeenCalled();
  });

  it("fires one batched request per chunk with the sorted virtual file_ids", async () => {
    const versions = [
      makeVersion({ file_id: 2, container: "virtual" }),
      makeVersion({ file_id: 1, container: "virtual" }),
      makeVersion({ file_id: 3, container: "mkv" }),
    ];
    mocks.api.mockResolvedValue({
      results: [
        { file_id: 1, available: true },
        { file_id: 2, available: false },
      ],
    });

    const { result } = renderHook(() => useVersionLiveness(versions, true), {
      wrapper: createWrapper(createQueryClient()),
    });

    await waitFor(() => expect(result.current.get(2)).toBe(false));

    expect(mocks.api).toHaveBeenCalledTimes(1);
    expect(mocks.api).toHaveBeenCalledWith("/catalog/versions/check", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ file_ids: [1, 2] }),
    });
    expect(result.current.get(1)).toBe(true);
    expect(result.current.has(3)).toBe(false);
  });

  it("merges item metadata with check results (check result wins as fresher)", async () => {
    const versions = [
      makeVersion({ file_id: 1, container: "virtual", available: false }),
      makeVersion({ file_id: 2, container: "virtual" }),
    ];
    mocks.api.mockResolvedValue({
      results: [
        { file_id: 1, available: true },
        { file_id: 2, available: false },
      ],
    });

    const { result } = renderHook(() => useVersionLiveness(versions, true), {
      wrapper: createWrapper(createQueryClient()),
    });

    await waitFor(() => expect(result.current.get(2)).toBe(false));

    // The check ran after the item snapshot, so its result wins: the version
    // the metadata marked unavailable is recovered.
    expect(result.current.get(1)).toBe(true);
  });

  it("caches results across re-enables (no second request while fresh)", async () => {
    const versions = [makeVersion({ file_id: 1, container: "virtual" })];
    mocks.api.mockResolvedValue({ results: [{ file_id: 1, available: false }] });

    const queryClient = createQueryClient();
    const { result, rerender } = renderHook(
      ({ enabled }: { enabled: boolean }) => useVersionLiveness(versions, enabled),
      {
        wrapper: createWrapper(queryClient),
        initialProps: { enabled: true },
      },
    );

    await waitFor(() => expect(result.current.get(1)).toBe(false));
    expect(mocks.api).toHaveBeenCalledTimes(1);

    rerender({ enabled: false });
    rerender({ enabled: true });

    await waitFor(() => expect(result.current.get(1)).toBe(false));
    expect(mocks.api).toHaveBeenCalledTimes(1);
  });
});

describe("applyVersionAvailability", () => {
  it("stamps a reported verdict and clears a stale one", () => {
    const versions = [makeVersion({ file_id: 1 }), makeVersion({ file_id: 2, available: false })];

    const next = applyVersionAvailability(
      versions,
      new Map([
        [1, false],
        [2, true],
      ]),
    );

    expect(next[0]?.available).toBe(false);
    expect(next[1]?.available).toBe(true);
  });

  it("leaves rows the check did not report, and their identity, untouched", () => {
    const versions = [makeVersion({ file_id: 1 })];

    const next = applyVersionAvailability(versions, new Map());

    expect(next).toBe(versions);
    expect(next[0]?.available).toBeUndefined();
  });

  it("keeps the list identity when nothing changed", () => {
    const versions = [makeVersion({ file_id: 1, available: true })];

    expect(applyVersionAvailability(versions, new Map([[1, true]]))).toBe(versions);
  });
});
