import { act, fireEvent, render, screen } from "@testing-library/react";
import { beforeEach, expect, it, vi } from "vitest";
import type { FileVersion } from "@/api/types";
import {
  SaveCancelledError,
  BUFFERED_SAVE_LIMIT_BYTES,
  DownloadPreparationError,
} from "@/api/v2/downloadPreparation";
import DownloadVersionPicker from "./DownloadVersionPicker";

const mocks = vi.hoisted(() => ({
  create: vi.fn(),
  poll: vi.fn(),
  file: vi.fn(),
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
    getPreparedDownloadFile: mocks.file,
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
  // The ready state's filename and size come from the authoritative HEAD, not
  // from the create/poll row. Every test that reaches "ready" needs this mock
  // so no real session/network code runs in jsdom.
  mocks.file.mockReset();
  mocks.file.mockImplementation((id: string) =>
    Promise.resolve({ id, filename: "Movie.mp4", fileSize: 1024 }),
  );
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
  expect(mocks.file).toHaveBeenCalledWith("dl_1", "Movie.mp4", expect.any(AbortSignal));
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

it("communicates an over-budget file the browser cannot stream", async () => {
  mocks.create.mockResolvedValue({ id: "dl_big", status: "ready" });
  mocks.file.mockResolvedValue({
    id: "dl_big",
    filename: "Movie.mp4",
    fileSize: BUFFERED_SAVE_LIMIT_BYTES + 1,
  });
  await openVirtualPanel();

  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /Prepare download/i }));
  });

  expect(screen.getByText(/too large to save in this browser/i)).toBeVisible();
  expect(screen.getByRole("button", { name: /Save file/i })).toBeDisabled();
});

it("stops polling when a ready file is permanently refused", async () => {
  mocks.create.mockResolvedValue({ id: "dl_gone2", status: "preparing" });
  mocks.poll.mockResolvedValue({ id: "dl_gone2", status: "ready" });
  mocks.file.mockRejectedValue(
    new DownloadPreparationError(
      "This download is expired or no longer available. Prepare it again.",
      { status: 404, retryable: false },
    ),
  );
  await openVirtualPanel();

  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /Prepare download/i }));
  });

  await vi.waitFor(() =>
    expect(mocks.toastError).toHaveBeenCalledWith(
      "This download is expired or no longer available. Prepare it again.",
    ),
  );
  expect(screen.queryByRole("button", { name: /Save file/i })).toBeNull();
  expect(screen.queryByText(/Preparing your download/i)).toBeNull();
  const calls = mocks.file.mock.calls.length;
  await new Promise((resolve) => window.setTimeout(resolve, 50));
  expect(mocks.file).toHaveBeenCalledTimes(calls);
});

it("keeps a successful create and retries a transient HEAD failure", async () => {
  mocks.create.mockResolvedValue({ id: "dl_retry", status: "ready" });
  mocks.poll.mockResolvedValue({ id: "dl_retry", status: "ready" });
  mocks.file
    .mockRejectedValueOnce(new DownloadPreparationError("busy", { status: 503, retryable: true }))
    .mockResolvedValue({ id: "dl_retry", filename: "Movie.mp4", fileSize: 1024 });
  await openVirtualPanel();

  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /Prepare download/i }));
  });

  expect(await screen.findByRole("button", { name: /Save file/i })).toBeInTheDocument();
  expect(mocks.file).toHaveBeenCalledTimes(2);
  expect(mocks.toastError).not.toHaveBeenCalled();
});

it("stops after the transient HEAD retry budget is exhausted", async () => {
  vi.useFakeTimers();
  try {
    mocks.create.mockResolvedValue({ id: "dl_tired", status: "preparing" });
    mocks.poll.mockResolvedValue({ id: "dl_tired", status: "ready" });
    mocks.file.mockRejectedValue(
      new DownloadPreparationError("busy", { status: 503, retryable: true }),
    );
    await openVirtualPanel();

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /Prepare download/i }));
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2000 * 30);
    });

    expect(mocks.toastError).toHaveBeenCalledWith(
      "The file is ready, but this browser could not verify it. Prepare it again.",
    );
    expect(screen.queryByText(/Preparing your download/i)).toBeNull();
  } finally {
    vi.useRealTimers();
  }
});

it("drops a HEAD result that arrives after the user cancels", async () => {
  let resolveFile!: (value: { id: string; filename: string; fileSize: number }) => void;
  mocks.create.mockResolvedValue({ id: "dl_stale", status: "preparing" });
  mocks.poll.mockResolvedValue({ id: "dl_stale", status: "ready" });
  mocks.file.mockReturnValue(
    new Promise((resolve) => {
      resolveFile = resolve;
    }),
  );
  await openVirtualPanel();

  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /Prepare download/i }));
  });
  await vi.waitFor(() => expect(mocks.file).toHaveBeenCalled());
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /^Cancel$/i }));
  });
  await act(async () => {
    resolveFile({ id: "dl_stale", filename: "Movie.mp4", fileSize: 1024 });
  });

  expect(screen.queryByRole("button", { name: /Save file/i })).toBeNull();
  expect(mocks.toastError).not.toHaveBeenCalled();
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

it("shows the reconciliation reason when a managed conflict cannot be resolved", async () => {
  mocks.create.mockRejectedValue(
    new DownloadPreparationError(
      "This download changed on the server. Reopen the download picker and try again.",
    ),
  );
  await openVirtualPanel();

  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /Prepare download/i }));
  });
  expect(mocks.toastError).toHaveBeenCalledWith(
    "This download changed on the server. Reopen the download picker and try again.",
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

it("ignores a poll response that lands after the preparation is cancelled", async () => {
  mocks.create.mockResolvedValue({ id: "dl_late_cancel", status: "preparing" });
  let resolvePoll: (entry: unknown) => void = () => undefined;
  mocks.poll.mockImplementation(
    () =>
      new Promise((resolve) => {
        resolvePoll = resolve;
      }),
  );
  await openVirtualPanel();

  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /Prepare download/i }));
  });
  await vi.waitFor(() => expect(mocks.poll).toHaveBeenCalled());

  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /^Cancel$/i }));
  });
  expect(mocks.remove).toHaveBeenCalledWith("dl_late_cancel");
  expect(screen.queryByText(/Preparing your download/i)).toBeNull();

  // The read was still in flight when the user cancelled; its late reply must
  // not restore readiness over the cancel.
  await act(async () => {
    resolvePoll({ id: "dl_late_cancel", status: "ready" });
  });

  expect(screen.queryByRole("button", { name: /Save file/i })).toBeNull();
  expect(screen.queryByText(/Preparing your download/i)).toBeNull();
  expect(mocks.toastError).not.toHaveBeenCalled();
});

it("ignores a poll response that lands after the versions refresh", async () => {
  mocks.create.mockResolvedValue({ id: "dl_late_refresh", status: "preparing" });
  let resolvePoll: (entry: unknown) => void = () => undefined;
  mocks.poll.mockImplementation(
    () =>
      new Promise((resolve) => {
        resolvePoll = resolve;
      }),
  );
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
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /Prepare download/i }));
  });
  await vi.waitFor(() => expect(mocks.poll).toHaveBeenCalled());

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
  expect(screen.queryByText(/Preparing your download/i)).toBeNull();

  // The refresh reset the panel; the superseded read's reply must not reopen
  // readiness against the new file list.
  await act(async () => {
    resolvePoll({ id: "dl_late_refresh", status: "ready" });
  });

  expect(screen.queryByRole("button", { name: /Save file/i })).toBeNull();
  expect(screen.getByRole("button", { name: /1080p/ })).toBeEnabled();
  expect(mocks.toastError).not.toHaveBeenCalled();
});

it("serializes poll reads so a slow response is not overtaken", async () => {
  vi.useFakeTimers();
  try {
    mocks.create.mockResolvedValue({ id: "dl_serial", status: "preparing" });
    const resolvers: Array<(entry: unknown) => void> = [];
    mocks.poll.mockImplementation(
      () =>
        new Promise((resolve) => {
          resolvers.push(resolve);
        }),
    );
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
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /Prepare download/i }));
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(mocks.poll).toHaveBeenCalledTimes(1);

    // Time passes while the first read is still open: no second read stacks up.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(6000);
    });
    expect(mocks.poll).toHaveBeenCalledTimes(1);

    // Settling the first read schedules exactly one follow-up.
    await act(async () => {
      resolvers[0]?.({ id: "dl_serial", status: "preparing" });
      await vi.advanceTimersByTimeAsync(0);
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2000);
    });
    expect(mocks.poll).toHaveBeenCalledTimes(2);
  } finally {
    vi.useRealTimers();
  }
});
