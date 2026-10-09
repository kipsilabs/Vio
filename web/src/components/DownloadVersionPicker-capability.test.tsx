import { act, fireEvent, render, screen } from "@testing-library/react";
import { beforeEach, expect, it, vi } from "vitest";
import { DirectDownloadError } from "@/api/v2/directDownloads";
import type { FileVersion } from "@/api/types";
import DownloadVersionPicker from "./DownloadVersionPicker";

const mocks = vi.hoisted(() => ({
  launch: vi.fn(),
  error: vi.fn(),
  capability: { enabled: true, allowed: true, download_allowed: true },
}));

vi.mock("@/hooks/queries/downloads", () => ({
  useDownloadCapability: () => ({ data: mocks.capability }),
}));
vi.mock("@/api/v2/directDownloads", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/v2/directDownloads")>();
  return { ...actual, launchDirectDownload: mocks.launch };
});
vi.mock("sonner", () => ({ toast: { error: mocks.error } }));

const versions = [{ file_id: 42, resolution: "1080p", file_size: 1024 } as FileVersion];

beforeEach(() => {
  vi.clearAllMocks();
  mocks.capability = { enabled: true, allowed: true, download_allowed: true };
});

it("disables the picker and explains when the server turns downloads off", () => {
  vi.useFakeTimers();
  mocks.capability = { enabled: false, allowed: true, download_allowed: true };
  render(<DownloadVersionPicker open onOpenChange={() => {}} versions={versions} />);
  const row = screen.getByRole("button", { name: /1080p/ });
  expect(row).toBeDisabled();
  fireEvent.pointerMove(row, { pointerType: "mouse" });
  act(() => vi.advanceTimersByTime(0));
  expect(screen.getByRole("tooltip")).toHaveTextContent("Downloads are turned off on this server.");
  vi.useRealTimers();
});

it("disables the picker and explains when the account may not download", () => {
  vi.useFakeTimers();
  mocks.capability = { enabled: true, allowed: false, download_allowed: true };
  render(<DownloadVersionPicker open onOpenChange={() => {}} versions={versions} />);
  const row = screen.getByRole("button", { name: /1080p/ });
  expect(row).toBeDisabled();
  fireEvent.pointerMove(row, { pointerType: "mouse" });
  act(() => vi.advanceTimersByTime(0));
  expect(screen.getByRole("tooltip")).toHaveTextContent(
    "Downloads are not allowed for this account.",
  );
  vi.useRealTimers();
});

it("toasts the concrete reason when the server refuses with 403", async () => {
  mocks.launch.mockRejectedValue(
    new DirectDownloadError(403, "Downloads are not allowed for this account."),
  );
  render(<DownloadVersionPicker open onOpenChange={() => {}} versions={versions} />);
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /1080p/ }));
  });
  expect(mocks.error).toHaveBeenCalledWith("Downloads are not allowed for this account.");
});

it("toasts a concrete reason for a generic transport failure", async () => {
  mocks.launch.mockRejectedValue(new TypeError("network"));
  render(<DownloadVersionPicker open onOpenChange={() => {}} versions={versions} />);
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /1080p/ }));
  });
  expect(mocks.error).toHaveBeenCalledWith(
    "Download could not be started. Check your connection and try again.",
  );
});
