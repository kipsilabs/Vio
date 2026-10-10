import { act, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { setAccessToken, setProfileId, setProfileToken } from "@/api/client";
import type { FileVersion } from "@/api/types";
import DownloadVersionPicker from "./DownloadVersionPicker";
import { DirectDownloadError } from "@/api/v2/directDownloads";

const mocks = vi.hoisted(() => ({ prepare: vi.fn(), error: vi.fn() }));

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

const versions = [
  { file_id: 42, resolution: "1080p", file_size: 1024, file_name: "Movie.mp4" } as FileVersion,
];

beforeEach(() => {
  vi.clearAllMocks();
  setAccessToken("original-account");
  setProfileId("original-profile");
  setProfileToken("original-pin");
});
afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  setAccessToken(null);
  setProfileId(null);
  setProfileToken(null);
});

it("prompts a version refresh on a stale file_id and shows the reason", async () => {
  mocks.prepare.mockRejectedValue(
    new DirectDownloadError(
      404,
      "This version is no longer available. Refresh the list to see current versions.",
      "file_unavailable",
    ),
  );
  const onStaleVersion = vi.fn();
  render(
    <DownloadVersionPicker
      open
      onOpenChange={() => undefined}
      versions={versions}
      onStaleVersion={onStaleVersion}
    />,
  );

  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /1080p/ }));
  });

  expect(onStaleVersion).toHaveBeenCalledWith(42);
  expect(
    screen.getByText(
      "This version is no longer available. Refresh the list to see current versions.",
    ),
  ).toBeTruthy();
  expect(screen.queryByRole("link", { name: /Save file/i })).toBeNull();
});

it("shows a file-level access denial without prompting a refresh", async () => {
  mocks.prepare.mockRejectedValue(
    new DirectDownloadError(403, "You do not have access to this file.", "file_access_denied"),
  );
  const onStaleVersion = vi.fn();
  render(
    <DownloadVersionPicker
      open
      onOpenChange={() => undefined}
      versions={versions}
      onStaleVersion={onStaleVersion}
    />,
  );

  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /1080p/ }));
  });

  expect(onStaleVersion).not.toHaveBeenCalled();
  expect(screen.getByText("You do not have access to this file.")).toBeTruthy();
  expect(screen.queryByRole("link", { name: /Save file/i })).toBeNull();
});
