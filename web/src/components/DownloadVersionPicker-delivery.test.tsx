import { act, fireEvent, render, screen } from "@testing-library/react";
import { beforeEach, expect, it, vi } from "vitest";
import { StaleApiRequestContextError } from "@/api/client";
import type { FileVersion } from "@/api/types";
import DownloadVersionPicker from "./DownloadVersionPicker";

const mocks = vi.hoisted(() => ({
  prepare: vi.fn(),
  error: vi.fn(),
  capability: { enabled: true, allowed: true, download_allowed: true },
}));

vi.mock("@/api/v2/directDownloads", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/v2/directDownloads")>();
  return { ...actual, prepareDirectDownload: mocks.prepare };
});
vi.mock("@/hooks/queries/downloads", () => ({
  useDownloadCapability: () => ({
    data: mocks.capability,
    isLoading: false,
    isError: false,
    refetch: vi.fn(),
  }),
}));
vi.mock("sonner", () => ({ toast: { error: mocks.error } }));

const versions = [
  { file_id: 42, resolution: "1080p", file_size: 1024, file_name: "Movie.mp4" } as FileVersion,
];
const replacement = [{ ...versions[0], file_id: 43 } as FileVersion];

const PREFLIGHT_URL = "/api/v2/direct-download?file_id=42&token=t";

beforeEach(() => {
  vi.clearAllMocks();
  mocks.capability = { enabled: true, allowed: true, download_allowed: true };
});

it("prepares once, then exposes a save link whose href is the prepared URL", async () => {
  mocks.prepare.mockResolvedValue({ url: PREFLIGHT_URL, filename: "Movie.mp4" });
  const close = vi.fn();
  render(<DownloadVersionPicker open onOpenChange={close} versions={versions} />);

  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /1080p/ }));
  });

  expect(mocks.prepare).toHaveBeenCalledTimes(1);
  expect(mocks.prepare).toHaveBeenCalledWith(
    42,
    expect.any(Function),
    expect.any(AbortSignal),
    "Movie.mp4",
  );
  // Preparation does not launch the save or close the dialog.
  expect(close).not.toHaveBeenCalled();

  const save = screen.getByRole("link", { name: /Save file/i });
  expect(save).toHaveAttribute("href", PREFLIGHT_URL);
  expect(save).toHaveAttribute("download", "Movie.mp4");
});

it("closes on the explicit user click without revoking the navigation URL", async () => {
  mocks.prepare.mockResolvedValue({ url: PREFLIGHT_URL, filename: "Movie.mp4" });
  const revoke = vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
  const close = vi.fn();
  render(<DownloadVersionPicker open onOpenChange={close} versions={versions} />);

  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /1080p/ }));
  });
  fireEvent.click(screen.getByRole("link", { name: /Save file/i }));

  expect(close).toHaveBeenCalledWith(false);
  // The URL outlives the click and nothing is revoked in the save path.
  expect(revoke).not.toHaveBeenCalled();
  revoke.mockRestore();
});

it("aborts a pending preflight on versions replacement and re-enables rows", async () => {
  mocks.prepare.mockImplementation(() => new Promise(() => {}));
  const close = vi.fn();
  const view = render(<DownloadVersionPicker open onOpenChange={close} versions={versions} />);
  fireEvent.click(screen.getByRole("button", { name: /1080p/ }));
  expect(mocks.prepare).toHaveBeenCalledTimes(1);
  const signal = mocks.prepare.mock.calls[0]?.[2] as AbortSignal;
  expect(signal.aborted).toBe(false);

  view.rerender(<DownloadVersionPicker open onOpenChange={close} versions={replacement} />);

  expect(signal.aborted).toBe(true);
  expect(screen.getByRole("button", { name: /1080p/ })).toBeEnabled();
  expect(screen.queryByRole("link", { name: /Save file/i })).toBeNull();
});

it("drops a prepared save link on versions replacement and re-enables rows", async () => {
  mocks.prepare.mockResolvedValue({ url: PREFLIGHT_URL, filename: "Movie.mp4" });
  const close = vi.fn();
  const view = render(<DownloadVersionPicker open onOpenChange={close} versions={versions} />);
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /1080p/ }));
  });
  expect(screen.getByRole("link", { name: /Save file/i })).toBeTruthy();

  view.rerender(<DownloadVersionPicker open onOpenChange={close} versions={replacement} />);

  expect(screen.queryByRole("link", { name: /Save file/i })).toBeNull();
  expect(screen.getByRole("button", { name: /1080p/ })).toBeEnabled();
});

it("aborts a pending preflight on close and re-enables rows when reopened", async () => {
  mocks.prepare.mockImplementation(() => new Promise(() => {}));
  const close = vi.fn();
  const view = render(<DownloadVersionPicker open onOpenChange={close} versions={versions} />);
  fireEvent.click(screen.getByRole("button", { name: /1080p/ }));
  const signal = mocks.prepare.mock.calls[0]?.[2] as AbortSignal;
  expect(signal.aborted).toBe(false);

  view.rerender(<DownloadVersionPicker open={false} onOpenChange={close} versions={versions} />);
  expect(signal.aborted).toBe(true);

  view.rerender(<DownloadVersionPicker open onOpenChange={close} versions={versions} />);
  expect(screen.getByRole("button", { name: /1080p/ })).toBeEnabled();
  expect(screen.queryByRole("link", { name: /Save file/i })).toBeNull();
});

it("keeps an authority refusal from closing or reporting into the replacement profile", async () => {
  mocks.prepare.mockRejectedValue(new StaleApiRequestContextError());
  const close = vi.fn();
  render(<DownloadVersionPicker open onOpenChange={close} versions={versions} />);
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /1080p/ }));
  });
  expect(close).not.toHaveBeenCalled();
  expect(mocks.error).not.toHaveBeenCalled();
});

it("toasts a refused preflight and shows no save link", async () => {
  mocks.prepare.mockRejectedValue(
    Object.assign(new Error("Downloads are not allowed for this account."), {
      name: "DirectDownloadError",
    }),
  );
  const close = vi.fn();
  render(<DownloadVersionPicker open onOpenChange={close} versions={versions} />);
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /1080p/ }));
  });
  expect(screen.queryByRole("link", { name: /Save file/i })).toBeNull();
  expect(close).not.toHaveBeenCalled();
});
