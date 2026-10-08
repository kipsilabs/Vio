import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { V2ProblemError } from "@/api/v2/request";
import { useUpdateSection, useRestoreDefaultSections, useDeleteSection } from "./sections";
const request = vi.hoisted(() => vi.fn());
vi.mock("@/api/v2/request", async () => ({
  ...(await vi.importActual<typeof import("@/api/v2/request")>("@/api/v2/request")),
  v2: request,
}));
afterEach(() => {
  request.mockReset();
});
function setup() {
  const client = new QueryClient({
    defaultOptions: { mutations: { retry: 3, retryDelay: 0 }, queries: { retry: false } },
  });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  const hook = renderHook(
    () => ({
      update: useUpdateSection(),
      restore: useRestoreDefaultSections(),
      remove: useDeleteSection(),
    }),
    { wrapper },
  );
  return { ...hook, client };
}
describe("administrator section mutation hooks", () => {
  it("never replays a captured edit even when the query client defaults to retries", async () => {
    const stale = new V2ProblemError(
      "updateAdminSection",
      {
        type: "stale_version",
        instance: "/api/v2/admin/sections/s1",
        title: "Changed",
        detail: "Changed",
        status: 412,
      },
      null,
      '"current"',
    );
    request.mockRejectedValue(stale);
    const { result, client } = setup();
    try {
      await act(async () => {
        await expect(
          result.current.update.mutateAsync({ id: "s1", etag: '"draft"', title: "Draft" }),
        ).rejects.toBe(stale);
      });
      expect(request).toHaveBeenCalledTimes(1);
      expect(request.mock.calls[0]![1].headers).toEqual({ "If-Match": '"draft"' });
    } finally {
      client.clear();
    }
  });
  it("does not replay restore or delete after a network failure", async () => {
    request.mockRejectedValue(new Error("Network interrupted"));
    const { result, client } = setup();
    try {
      await act(async () => {
        await expect(
          result.current.restore.mutateAsync({
            scope: "home",
            reset_profiles: true,
            etag: '"dialog"',
          }),
        ).rejects.toThrow("Network");
        await expect(
          result.current.remove.mutateAsync({ id: "s1", etag: '"delete"' }),
        ).rejects.toThrow("Network");
      });
      expect(request).toHaveBeenCalledTimes(2);
      expect(request.mock.calls.map(([, options]) => options.headers["If-Match"])).toEqual([
        '"dialog"',
        '"delete"',
      ]);
    } finally {
      client.clear();
    }
  });
});
