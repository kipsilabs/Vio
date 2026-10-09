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

beforeEach(() => {
  vi.clearAllMocks();
  mocks.capability = { enabled: true, allowed: true, download_allowed: true };
});

it("prepares once, then exposes an explicit save link without closing the dialog", async () => {
  const dispose = vi.fn();
  mocks.prepare.mockResolvedValue({ url: "blob:movie", filename: "Movie.mp4", dispose });
  const close = vi.fn();
  render(<DownloadVersionPicker open onOpenChange={close} versions={versions} />);

  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /1080p/ }));
  });

  expect(mocks.prepare).toHaveBeenCalledTimes(1);
  expect(mocks.prepare).toHaveBeenCalledWith(42, expect.any(Function), "Movie.mp4");
  // Preparation does not launch the save or close the dialog.
  expect(close).not.toHaveBeenCalled();

  const save = screen.getByRole("link", { name: /Save file/i });
  expect(save).toHaveAttribute("href", "blob:movie");
  expect(save).toHaveAttribute("download", "Movie.mp4");
});

it("saves on the explicit user click and closes only then", async () => {
  const dispose = vi.fn();
  mocks.prepare.mockResolvedValue({ url: "blob:movie", filename: "Movie.mp4", dispose });
  const close = vi.fn();
  render(<DownloadVersionPicker open onOpenChange={close} versions={versions} />);

  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /1080p/ }));
  });
  fireEvent.click(screen.getByRole("link", { name: /Save file/i }));

  expect(dispose).toHaveBeenCalledTimes(1);
  expect(close).toHaveBeenCalledWith(false);
});

it("retires a pending selection on replacement and rejects duplicate clicks", async () => {
  let resolve!: (value: unknown) => void;
  mocks.prepare.mockImplementation(
    () =>
      new Promise((r) => {
        resolve = r;
      }),
  );
  const close = vi.fn();
  const view = render(<DownloadVersionPicker open onOpenChange={close} versions={versions} />);
  fireEvent.click(screen.getByRole("button", { name: /1080p/ }));
  fireEvent.click(screen.getByRole("button", { name: /1080p/ }));
  expect(mocks.prepare).toHaveBeenCalledTimes(1);
  const call = mocks.prepare.mock.calls[0];
  if (!call) throw new Error("Expected one prepare call");
  const current = call[1] as () => boolean;
  view.rerender(
    <DownloadVersionPicker
      open
      onOpenChange={close}
      versions={[{ ...versions[0], file_id: 43 } as FileVersion]}
    />,
  );
  expect(current()).toBe(false);
  await act(async () => resolve({ url: "blob:stale", filename: "stale", dispose: vi.fn() }));
  expect(screen.queryByRole("link", { name: /Save file/i })).toBeNull();
  expect(close).not.toHaveBeenCalled();
  expect(mocks.error).not.toHaveBeenCalled();
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

it("toasts a refused transfer and shows no save link", async () => {
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
