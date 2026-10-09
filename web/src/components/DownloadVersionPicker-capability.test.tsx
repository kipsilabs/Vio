import { act, fireEvent, render, screen } from "@testing-library/react";
import { beforeEach, expect, it, vi } from "vitest";
import { DirectDownloadError } from "@/api/v2/directDownloads";
import type { FileVersion } from "@/api/types";
import DownloadVersionPicker from "./DownloadVersionPicker";

const mocks = vi.hoisted(() => ({
  prepare: vi.fn(),
  error: vi.fn(),
  refetch: vi.fn(),
  capability: { enabled: true, allowed: true, download_allowed: true } as
    | { enabled: boolean; allowed: boolean; download_allowed: boolean }
    | undefined,
  loading: false,
  errored: false,
}));

vi.mock("@/api/v2/directDownloads", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/v2/directDownloads")>();
  return { ...actual, prepareDirectDownload: mocks.prepare };
});
vi.mock("@/hooks/queries/downloads", () => ({
  useDownloadCapability: () => ({
    data: mocks.capability,
    isLoading: mocks.loading,
    isError: mocks.errored,
    refetch: mocks.refetch,
  }),
}));
vi.mock("sonner", () => ({ toast: { error: mocks.error } }));

const versions = [{ file_id: 42, resolution: "1080p", file_size: 1024 } as FileVersion];

beforeEach(() => {
  vi.clearAllMocks();
  mocks.capability = { enabled: true, allowed: true, download_allowed: true };
  mocks.loading = false;
  mocks.errored = false;
});

it("disables rows until the capability has loaded", () => {
  mocks.loading = true;
  mocks.capability = undefined;
  render(<DownloadVersionPicker open onOpenChange={() => {}} versions={versions} />);
  expect(screen.getByRole("button", { name: /1080p/ })).toBeDisabled();
  expect(screen.getByRole("status")).toHaveTextContent("Checking download availability…");
});

it("disables rows and offers a retry when the capability read fails", () => {
  mocks.errored = true;
  mocks.capability = undefined;
  render(<DownloadVersionPicker open onOpenChange={() => {}} versions={versions} />);
  expect(screen.getByRole("button", { name: /1080p/ })).toBeDisabled();
  expect(screen.getByRole("alert")).toHaveTextContent("Couldn't check download availability.");
  fireEvent.click(screen.getByRole("button", { name: /Retry/i }));
  expect(mocks.refetch).toHaveBeenCalledTimes(1);
});

it("shows the reason as visible dialog text when the server turns downloads off", () => {
  mocks.capability = { enabled: false, allowed: true, download_allowed: true };
  render(<DownloadVersionPicker open onOpenChange={() => {}} versions={versions} />);
  expect(screen.getByRole("button", { name: /1080p/ })).toBeDisabled();
  // Visible, focusable-independent copy rather than a hover-only tooltip.
  expect(screen.getByText("Downloads are turned off on this server.")).toBeVisible();
});

it("shows the reason as visible dialog text when the account may not download", () => {
  mocks.capability = { enabled: true, allowed: false, download_allowed: true };
  render(<DownloadVersionPicker open onOpenChange={() => {}} versions={versions} />);
  expect(screen.getByRole("button", { name: /1080p/ })).toBeDisabled();
  expect(screen.getByText("Downloads are not allowed for this account.")).toBeVisible();
});

it("toasts the concrete reason when the server refuses with 403", async () => {
  mocks.prepare.mockRejectedValue(
    new DirectDownloadError(403, "Downloads are not allowed for this account."),
  );
  render(<DownloadVersionPicker open onOpenChange={() => {}} versions={versions} />);
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /1080p/ }));
  });
  expect(mocks.error).toHaveBeenCalledWith("Downloads are not allowed for this account.");
});

it("toasts a concrete reason for a generic transport failure", async () => {
  mocks.prepare.mockRejectedValue(new TypeError("network"));
  render(<DownloadVersionPicker open onOpenChange={() => {}} versions={versions} />);
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /1080p/ }));
  });
  expect(mocks.error).toHaveBeenCalledWith(
    "Download could not be started. Check your connection and try again.",
  );
});
