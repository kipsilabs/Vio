import { act, fireEvent, render, screen } from "@testing-library/react";
import { beforeEach, expect, it, vi } from "vitest";
import type { FileVersion } from "@/api/types";
import { SaveCancelledError } from "@/api/v2/downloadPreparation";
import DownloadVersionPicker from "./DownloadVersionPicker";

const mocks = vi.hoisted(() => ({
  create: vi.fn(),
  poll: vi.fn(),
  save: vi.fn(),
  remove: vi.fn(),
  toastError: vi.fn(),
  refetch: vi.fn(),
}));

vi.mock("@/api/v2/downloadPreparation", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/v2/downloadPreparation")>();
  return {
    ...actual,
    createPreparedDownload: mocks.create,
    getPreparedDownload: mocks.poll,
    savePreparedDownload: mocks.save,
  };
});
vi.mock("@/api/v2/downloadRegistry", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/v2/downloadRegistry")>();
  return { ...actual, deleteDownloadEntry: mocks.remove };
});
vi.mock("@/hooks/queries/downloads", () => ({
  useDownloadCapability: () => ({
    data: { enabled: true, allowed: true, download_allowed: true },
    isLoading: false,
    isError: false,
    refetch: mocks.refetch,
  }),
}));
vi.mock("sonner", () => ({ toast: { error: mocks.toastError } }));

const virtualMovie = {
  file_id: 42,
  resolution: "1080p",
  file_size: 0,
  container: "virtual",
  file_path: "virtual://movie/tt1?result=a",
  file_name: "Movie.mp4",
} as FileVersion;

const createTarget = { contentId: "mv_1" };

beforeEach(() => {
  vi.clearAllMocks();
});

async function openVirtualPanel() {
  render(
    <DownloadVersionPicker
      open
      onOpenChange={() => undefined}
      versions={[virtualMovie]}
      createTarget={createTarget}
    />,
  );
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /1080p/ }));
  });
  expect(screen.getByRole("button", { name: /Prepare download/i })).toBeVisible();
}

it("creates a virtual download at original quality and prepares it", async () => {
  mocks.create.mockResolvedValue({ id: "dl_1", status: "preparing" });
  mocks.poll.mockResolvedValue({ id: "dl_1", status: "ready" });
  await openVirtualPanel();

  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /Prepare download/i }));
  });

  expect(mocks.create).toHaveBeenCalledTimes(1);
  const [input, signal] = mocks.create.mock.calls[0] as [
    { contentId: string; episodeId?: string; mediaFileId: number; quality: string },
    AbortSignal,
  ];
  expect(input).toMatchObject({
    contentId: "mv_1",
    mediaFileId: 42,
    quality: "original",
  });
  expect(signal).toBeInstanceOf(AbortSignal);

  await vi.waitFor(() =>
    expect(screen.getByRole("button", { name: /Save file/i })).toBeInTheDocument(),
  );
  expect(mocks.poll).toHaveBeenCalledWith("dl_1", expect.any(AbortSignal));
});

it("saves a ready virtual download with the server's authority", async () => {
  mocks.create.mockResolvedValue({ id: "dl_2", status: "ready" });
  await openVirtualPanel();

  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /Prepare download/i }));
  });

  const save = screen.getByRole("button", { name: /Save file/i });
  await act(async () => {
    fireEvent.click(save);
  });
  expect(mocks.save).toHaveBeenCalledWith("dl_2", "Movie.mp4");
});

it("stops polling and refuses when the per-entry read reports the row gone", async () => {
  mocks.create.mockResolvedValue({ id: "dl_gone", status: "preparing" });
  mocks.poll.mockResolvedValue(null);
  await openVirtualPanel();

  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /Prepare download/i }));
  });

  await vi.waitFor(() =>
    expect(mocks.toastError).toHaveBeenCalledWith(
      "This download is no longer available. Prepare it again.",
    ),
  );
  expect(screen.queryByText(/Preparing your download/i)).toBeNull();
});

it("stays silent and keeps the file ready when the save picker is cancelled", async () => {
  mocks.create.mockResolvedValue({ id: "dl_cancel", status: "ready" });
  mocks.save.mockRejectedValue(new SaveCancelledError());
  await openVirtualPanel();

  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /Prepare download/i }));
  });
  const save = await screen.findByRole("button", { name: /Save file/i });
  await act(async () => {
    fireEvent.click(save);
  });

  expect(mocks.toastError).not.toHaveBeenCalled();
  expect(screen.getByRole("button", { name: /Save file/i })).toBeInTheDocument();
});

it("cancels a preparing virtual download through delete", async () => {
  mocks.create.mockResolvedValue({ id: "dl_3", status: "preparing" });
  mocks.poll.mockResolvedValue({ id: "dl_3", status: "preparing" });
  await openVirtualPanel();

  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /Prepare download/i }));
  });
  await vi.waitFor(() => expect(mocks.poll).toHaveBeenCalled());

  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /^Cancel$/i }));
  });
  expect(mocks.remove).toHaveBeenCalledWith("dl_3");
  expect(screen.queryByText(/Preparing your download/i)).toBeNull();
});

it("toasts a concrete reason when preparation is refused", async () => {
  mocks.create.mockRejectedValue(new Error("provider exploded"));
  await openVirtualPanel();

  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /Prepare download/i }));
  });
  expect(mocks.toastError).toHaveBeenCalledWith(
    "Download could not be prepared. Check your connection and try again.",
  );
});

it("offers an episode virtual download with the episode identity", async () => {
  mocks.create.mockResolvedValue({ id: "dl_4", status: "ready" });
  render(
    <DownloadVersionPicker
      open
      onOpenChange={() => undefined}
      versions={[
        {
          file_id: 43,
          resolution: "1080p",
          file_size: 0,
          container: "virtual",
          file_path: "virtual://series/tt9/1/1?result=a",
          file_name: "Episode.mkv",
        } as FileVersion,
      ]}
      createTarget={{ contentId: "sr_9", episodeId: "ep_1" }}
    />,
  );
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /1080p/ }));
  });
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /Prepare download/i }));
  });
  expect(mocks.create).toHaveBeenCalledTimes(1);
  const [episodeInput] = mocks.create.mock.calls[0] as [
    { contentId: string; episodeId?: string; mediaFileId: number },
  ];
  expect(episodeInput).toMatchObject({
    contentId: "sr_9",
    episodeId: "ep_1",
    mediaFileId: 43,
  });
});

it("keeps an unavailable version out of the offer list", async () => {
  render(
    <DownloadVersionPicker
      open
      onOpenChange={() => undefined}
      versions={[{ ...virtualMovie, available: false } as FileVersion]}
      createTarget={createTarget}
    />,
  );
  expect(screen.queryByRole("button", { name: /1080p/ })).toBeNull();
  expect(screen.getByText("1 version is no longer available.")).toBeVisible();
});

it("repopulates the list when the versions refresh", async () => {
  const { rerender } = render(
    <DownloadVersionPicker
      open
      onOpenChange={() => undefined}
      versions={[virtualMovie]}
      createTarget={createTarget}
    />,
  );
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /1080p/ }));
  });
  expect(screen.getByRole("button", { name: /Prepare download/i })).toBeVisible();

  const refreshed = {
    ...virtualMovie,
    file_id: 77,
    file_path: "virtual://movie/tt1?result=b",
  } as FileVersion;
  await act(async () => {
    rerender(
      <DownloadVersionPicker
        open
        onOpenChange={() => undefined}
        versions={[refreshed]}
        createTarget={createTarget}
      />,
    );
  });
  expect(screen.queryByRole("button", { name: /Prepare download/i })).toBeNull();
  expect(screen.getByRole("button", { name: /1080p/ })).toBeEnabled();
});
