import { act, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { setAccessToken, setProfileId, setProfileToken } from "@/api/client";
import type { FileVersion } from "@/api/types";
import DownloadVersionPicker from "./DownloadVersionPicker";

const mocks = vi.hoisted(() => ({ error: vi.fn() }));
vi.mock("sonner", () => ({ toast: { error: mocks.error } }));
vi.mock("@/hooks/queries/downloads", () => ({
  useDownloadCapability: () => ({
    data: { enabled: true, allowed: true, download_allowed: true },
    isLoading: false,
    isError: false,
    refetch: vi.fn(),
  }),
}));
// Use the real directDownloads helper and real client authority state.
const versions = [{ file_id: 42, resolution: "1080p", file_size: 1024 } as FileVersion];
beforeEach(() => {
  mocks.error.mockClear();
  setAccessToken("original-account");
  setProfileId("original-profile");
  setProfileToken("original-pin");
  vi.spyOn(URL, "createObjectURL").mockReturnValue("blob:download");
  vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
});
afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  setAccessToken(null);
  setProfileId(null);
  setProfileToken(null);
});

it.each(["account", "profile", "pin", "unchanged"])(
  "fences a rejected actual probe for %s authority without replacing the picker",
  async (authority) => {
    let reject!: (error: Error) => void;
    const fetch = vi.fn(
      () =>
        new Promise<Response>((_, fail) => {
          reject = fail;
        }),
    );
    vi.stubGlobal("fetch", fetch);
    const close = vi.fn();
    render(<DownloadVersionPicker open onOpenChange={close} versions={versions} />);
    fireEvent.click(screen.getByRole("button", { name: /1080p/ }));
    expect(fetch).toHaveBeenCalledTimes(1);
    expect(fetch).toHaveBeenCalledWith(
      "/api/v2/direct-download?file_id=42&token=original-account",
      expect.objectContaining({ method: "GET", cache: "no-store" }),
    );
    // No close/rerender/versions replacement: only the client authority changes.
    if (authority === "account") setAccessToken("replacement-account");
    if (authority === "profile") setProfileId("replacement-profile");
    if (authority === "pin") setProfileToken("replacement-pin");
    await act(async () => {
      reject(new TypeError("connection lost"));
    });
    expect(fetch).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("link", { name: /Save file/i })).toBeNull();
    expect(close).not.toHaveBeenCalled();
    expect(mocks.error).toHaveBeenCalledTimes(authority === "unchanged" ? 1 : 0);
  },
);

it("fences a late second request after an authority change with no save link", async () => {
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
  const close = vi.fn();
  render(<DownloadVersionPicker open onOpenChange={close} versions={versions} />);
  fireEvent.click(screen.getByRole("button", { name: /1080p/ }));
  await vi.waitFor(() => expect(resolveTransfer).toBeDefined());
  // The probe authorized the transfer; the profile changes before it lands.
  setProfileId("replacement-profile");
  await act(async () => {
    resolveTransfer(new Response("bytes", { status: 200 }));
  });
  expect(screen.queryByRole("link", { name: /Save file/i })).toBeNull();
  expect(close).not.toHaveBeenCalled();
  expect(mocks.error).not.toHaveBeenCalled();
});
