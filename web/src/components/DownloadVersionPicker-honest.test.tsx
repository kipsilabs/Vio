import { act, fireEvent, render, screen } from "@testing-library/react";
import { beforeEach, expect, it, vi } from "vitest";
import type { FileVersion } from "@/api/types";
import DownloadVersionPicker from "./DownloadVersionPicker";

const mocks = vi.hoisted(() => ({
  prepare: vi.fn(),
  error: vi.fn(),
}));

vi.mock("@/api/v2/directDownloads", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/v2/directDownloads")>();
  return { ...actual, prepareDirectDownload: mocks.prepare };
});
vi.mock("@/hooks/queries/downloads", () => ({
  useDownloadCapability: () => ({
    data: { enabled: true, allowed: true, download_allowed: true },
    isLoading: false,
    isError: false,
    refetch: vi.fn(),
  }),
}));
vi.mock("sonner", () => ({ toast: { error: mocks.error } }));

function version(overrides: Partial<FileVersion>): FileVersion {
  return {
    file_id: overrides.file_id ?? 1,
    file_name: overrides.file_name,
    file_path: overrides.file_path,
    resolution: overrides.resolution ?? "",
    codec_video: overrides.codec_video ?? "",
    codec_audio: overrides.codec_audio ?? "",
    hdr: overrides.hdr ?? false,
    container: overrides.container ?? "",
    file_size: overrides.file_size ?? 0,
    duration: overrides.duration ?? 0,
    bitrate: overrides.bitrate ?? 0,
    release_name: overrides.release_name,
    available: overrides.available,
  };
}

const local = version({
  file_id: 1,
  resolution: "1080p",
  container: "mkv",
  file_size: 2 * 1024 ** 3,
  file_name: "Movie.mkv",
});
const virtualByContainer = version({
  file_id: 2,
  resolution: "2160p",
  container: "virtual",
  file_size: 20 * 1024 ** 3,
  release_name: "Provider 4K",
});
const virtualByPath = version({
  file_id: 3,
  resolution: "1080p",
  container: "mkv",
  file_size: 8 * 1024 ** 3,
  release_name: "Provider 1080p",
  file_path: "virtual://movie/tt123?profile=HD&result=abc",
});

beforeEach(() => {
  vi.clearAllMocks();
});

it("keeps virtual versions out of the list and states why", () => {
  render(
    <DownloadVersionPicker
      open
      onOpenChange={() => undefined}
      title="A Movie"
      versions={[local, virtualByContainer, virtualByPath]}
    />,
  );

  // The directly downloadable row stays; both virtual rows are gone.
  expect(screen.getByRole("button", { name: /1080p/ })).toBeTruthy();
  expect(screen.queryByText("Provider 4K")).toBeNull();
  expect(screen.queryByText("Provider 1080p")).toBeNull();
  // Counted in plain language with the reason.
  expect(
    screen.getByText("2 virtual versions stream from a provider and aren't listed."),
  ).toBeTruthy();
});

it("says to play the item when every version streams from a provider", () => {
  render(
    <DownloadVersionPicker
      open
      onOpenChange={() => undefined}
      title="A Movie"
      versions={[virtualByContainer, virtualByPath]}
    />,
  );

  expect(
    screen.getByText(
      "This item's versions stream from a provider and can't be downloaded directly. Play it instead.",
    ),
  ).toBeTruthy();
  expect(screen.queryByRole("button", { name: /2160p|1080p/ })).toBeNull();
});

it("drops label-less placeholder rows", () => {
  const placeholder = version({ file_id: 9, file_size: 0 });
  render(
    <DownloadVersionPicker
      open
      onOpenChange={() => undefined}
      title="A Movie"
      versions={[placeholder]}
    />,
  );

  expect(screen.getByText("No downloadable files are available for this item.")).toBeTruthy();
  // The dropped placeholder leaves no row button; only the dialog close remains.
  expect(screen.getAllByRole("button")).toHaveLength(1);
});

it("keeps directly downloadable rows selectable and preflights only those", async () => {
  mocks.prepare.mockResolvedValue({
    url: "/api/v2/direct-download?file_id=1&token=t",
    filename: "Movie.mkv",
  });
  render(
    <DownloadVersionPicker
      open
      onOpenChange={() => undefined}
      title="A Movie"
      versions={[virtualByContainer, local]}
    />,
  );

  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /1080p/ }));
  });

  expect(mocks.prepare).toHaveBeenCalledTimes(1);
  expect(mocks.prepare).toHaveBeenCalledWith(
    1,
    expect.any(Function),
    expect.any(AbortSignal),
    "Movie.mkv",
  );
});

it("does not offer a version the catalog already reports unavailable", () => {
  const gone = version({
    file_id: 4,
    resolution: "1080p",
    container: "mkv",
    file_size: 1024,
    release_name: "Gone Release",
    available: false,
  });
  render(
    <DownloadVersionPicker
      open
      onOpenChange={() => undefined}
      title="A Movie"
      versions={[gone, local]}
    />,
  );

  expect(screen.getByText("1 version is no longer available.")).toBeTruthy();
  expect(screen.queryByText("Gone Release")).toBeNull();
  // The healthy row is still offered.
  expect(screen.getByRole("button", { name: /1080p/ })).toBeTruthy();
});
