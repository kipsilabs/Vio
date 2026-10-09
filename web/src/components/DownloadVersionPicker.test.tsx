import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { FileVersion } from "@/api/types";

import DownloadVersionPicker from "./DownloadVersionPicker";

vi.mock("@/hooks/queries/downloads", () => ({
  buildDirectDownloadUrl: (fileId: number) => `/api/downloads/files/${fileId}`,
  useDownloadCapability: () => ({
    data: { enabled: true, allowed: true, download_allowed: true },
    isLoading: false,
    isError: false,
    refetch: () => {},
  }),
}));

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
  };
}

describe("DownloadVersionPicker", () => {
  it("uses file-size copy instead of quality copy for multi-file downloads", () => {
    render(
      <DownloadVersionPicker
        open
        onOpenChange={() => undefined}
        title="A Psalm for the Wild-Built"
        versions={[
          version({ file_id: 1, container: "epub", file_size: 512 * 1024 }),
          version({ file_id: 2, container: "pdf", file_size: 2 * 1024 * 1024 }),
        ]}
      />,
    );

    expect(screen.getByText("Larger files require more storage space.")).toBeTruthy();
    expect(screen.queryByText("Higher quality files require more storage space.")).toBeNull();
  });

  it("describes download choices as files instead of versions", () => {
    render(
      <DownloadVersionPicker
        open
        onOpenChange={() => undefined}
        title="A Psalm for the Wild-Built"
        versions={[version({ file_id: 1, container: "epub", file_size: 512 * 1024 })]}
      />,
    );

    expect(
      screen.getByText("Choose a file to download. Make sure you have enough disk space."),
    ).toBeTruthy();
    expect(
      screen.queryByText("Choose a version to download. Make sure you have enough disk space."),
    ).toBeNull();
  });

  it("allows ebook download rows to show page counts", () => {
    render(
      <DownloadVersionPicker
        open
        onOpenChange={() => undefined}
        title="A Psalm for the Wild-Built"
        versions={[
          version({
            file_id: 1,
            container: "cbz",
            file_size: 25 * 1024 ** 2,
            duration: 48,
          }),
        ]}
        summaryBuilder={(file) => `${file.container.toUpperCase()} · 48 pages`}
      />,
    );

    expect(screen.getByText("CBZ · 48 pages")).toBeTruthy();
  });

  it("labels each row with its release name so same-quality rows stay distinct", () => {
    render(
      <DownloadVersionPicker
        open
        onOpenChange={() => undefined}
        versions={[
          version({ file_id: 1, resolution: "1080p", release_name: "Alpha Release" }),
          version({ file_id: 2, resolution: "1080p", release_name: "Beta Release" }),
        ]}
      />,
    );

    expect(screen.getByText("Alpha Release")).toBeTruthy();
    expect(screen.getByText("Beta Release")).toBeTruthy();
  });

  it("highlights the caller's selected version with a visible marker", () => {
    render(
      <DownloadVersionPicker
        open
        onOpenChange={() => undefined}
        selectedFileId={2}
        versions={[
          version({ file_id: 1, resolution: "1080p", release_name: "Alpha Release" }),
          version({ file_id: 2, resolution: "2160p", release_name: "Beta Release" }),
        ]}
      />,
    );

    const selected = screen.getByRole("button", { name: /2160p/ });
    expect(selected.getAttribute("aria-current")).toBe("true");
    expect(screen.getByText("Playing")).toBeTruthy();
    expect(screen.getByRole("button", { name: /1080p/ }).getAttribute("aria-current")).toBeNull();
  });
});
