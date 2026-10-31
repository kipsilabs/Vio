import { act, fireEvent, render, renderHook, screen, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { describe, expect, it, vi } from "vitest";

import type { FileVersion } from "@/api/types";
import VersionDropdown from "./VersionDropdown";
import VersionFlyoutItems from "./VersionFlyout";
import { useVersionVisibility } from "./versionAvailability";

// The picker reads the watch detail lazily when it opens (score/ranking); these
// tests mount it bare, so the query is stubbed.
vi.mock("@/hooks/queries/items", () => ({
  useWatchDetail: () => ({ data: undefined, isLoading: false }),
}));

vi.mock("@/components/ui/dropdown-menu", () => ({
  DropdownMenuLabel: ({ children }: { children: ReactNode }) => <div>{children}</div>,
  DropdownMenuSeparator: () => <div />,
  DropdownMenuItem: ({
    children,
    onSelect,
    className,
  }: {
    children: ReactNode;
    onSelect?: () => void;
    className?: string;
  }) => (
    <div role="menuitem" className={className} onClick={onSelect}>
      {children}
    </div>
  ),
}));

function makeVersion(overrides: Partial<FileVersion> = {}): FileVersion {
  return {
    file_id: overrides.file_id ?? 1,
    resolution: overrides.resolution ?? "1080p",
    codec_video: overrides.codec_video ?? "h264",
    codec_audio: overrides.codec_audio ?? "aac",
    hdr: overrides.hdr ?? false,
    container: overrides.container ?? "mkv",
    file_size: overrides.file_size ?? 0,
    duration: overrides.duration ?? 0,
    bitrate: overrides.bitrate ?? 0,
    available: overrides.available,
    failed: overrides.failed,
    source_mismatch: overrides.source_mismatch,
  };
}

function openVersionDropdown() {
  fireEvent.click(screen.getByRole("button", { name: /Version/ }));
  return within(screen.getByRole("dialog"));
}

describe("VersionDropdown unavailable versions", () => {
  it("hides unavailable versions by default and shows them via the toggle", () => {
    const versions = [
      makeVersion({ file_id: 1, resolution: "2160p" }),
      makeVersion({ file_id: 2, resolution: "1080p", available: false }),
    ];
    const onSelectVersion = vi.fn();
    render(
      <VersionDropdown
        versions={versions}
        selectedVersion={versions[0]!}
        onSelectVersion={onSelectVersion}
      />,
    );

    const dialog = openVersionDropdown();

    // The unavailable version is hidden; the toggle reveals it.
    expect(dialog.queryByText("1080p")).not.toBeInTheDocument();
    fireEvent.click(dialog.getByRole("button", { name: /Show 1 unavailable version/ }));

    const unavailableRow = dialog.getByRole("button", { name: /1080p/ });
    expect(unavailableRow).toBeInTheDocument();
    expect(dialog.getByText("Will retry on play")).toBeInTheDocument();
    expect(unavailableRow).toHaveClass("opacity-80");
  });

  it("keeps the active selection visible even when it becomes unavailable", () => {
    const versions = [
      makeVersion({ file_id: 1, resolution: "2160p" }),
      makeVersion({ file_id: 2, resolution: "1080p", available: false }),
    ];
    render(
      <VersionDropdown
        versions={versions}
        selectedVersion={versions[1]!}
        onSelectVersion={vi.fn()}
      />,
    );

    const dialog = openVersionDropdown();

    // The selected (unavailable) version stays in the list, and the toggle is
    // not offered because nothing else is hidden.
    expect(dialog.getByRole("button", { name: /1080p/ })).toBeInTheDocument();
    expect(dialog.getByText("Will retry on play")).toBeInTheDocument();
    expect(dialog.queryByRole("button", { name: /Show .* unavailable/ })).not.toBeInTheDocument();
  });

  it("shows a recovered version without an unavailable toggle after a successful check", () => {
    // The item metadata marked the version unavailable, but the liveness check
    // reported it available again; the merged version (available: true) must
    // render normally with no "Show N unavailable" toggle.
    const versions = [
      makeVersion({ file_id: 1, resolution: "2160p" }),
      makeVersion({ file_id: 2, resolution: "1080p", available: true }),
    ];
    render(
      <VersionDropdown
        versions={versions}
        selectedVersion={versions[0]!}
        onSelectVersion={vi.fn()}
      />,
    );

    const dialog = openVersionDropdown();

    expect(dialog.getByRole("button", { name: /1080p/ })).toBeInTheDocument();
    expect(dialog.queryByText("Unavailable")).not.toBeInTheDocument();
    expect(dialog.queryByRole("button", { name: /Show .* unavailable/ })).not.toBeInTheDocument();
  });

  it("does not filter when every version is unavailable", () => {
    const versions = [
      makeVersion({ file_id: 1, resolution: "2160p", available: false }),
      makeVersion({ file_id: 2, resolution: "1080p", available: false }),
    ];
    render(
      <VersionDropdown
        versions={versions}
        selectedVersion={versions[0]!}
        onSelectVersion={vi.fn()}
      />,
    );

    const dialog = openVersionDropdown();

    expect(dialog.getByRole("button", { name: /2160p/ })).toBeInTheDocument();
    expect(dialog.getByRole("button", { name: /1080p/ })).toBeInTheDocument();
    expect(dialog.queryByRole("button", { name: /Show .* unavailable/ })).not.toBeInTheDocument();
  });
});

describe("VersionDropdown source mismatch", () => {
  it("renders the served-vs-declared state instead of the retry badge", () => {
    const versions = [
      makeVersion({
        file_id: 1,
        resolution: "1080p",
        available: true,
        source_mismatch: { served_bytes: 7_427_636_945, declared_bytes: 8_449_999_999 },
      }),
      makeVersion({ file_id: 2, resolution: "2160p" }),
    ];
    render(
      <VersionDropdown
        versions={versions}
        selectedVersion={versions[0]!}
        onSelectVersion={vi.fn()}
      />,
    );

    const dialog = openVersionDropdown();
    expect(
      dialog.getByText("Source mismatch — served 6.9 GB of declared 7.9 GB"),
    ).toBeInTheDocument();
    expect(dialog.queryByText("Will retry on play")).not.toBeInTheDocument();
  });

  it("keeps the generic presentation for a genuinely unprobed row", () => {
    const versions = [
      makeVersion({ file_id: 1, resolution: "1080p", available: true }),
      makeVersion({ file_id: 2, resolution: "2160p" }),
    ];
    render(
      <VersionDropdown
        versions={versions}
        selectedVersion={versions[0]!}
        onSelectVersion={vi.fn()}
      />,
    );

    const dialog = openVersionDropdown();
    expect(dialog.queryByText(/Source mismatch/)).not.toBeInTheDocument();
    expect(dialog.queryByText("Will retry on play")).not.toBeInTheDocument();
    expect(dialog.getByRole("button", { name: /1080p/ })).toBeInTheDocument();
  });

  it("never renders the mismatch as a failed row", () => {
    const versions = [
      makeVersion({
        file_id: 1,
        resolution: "1080p",
        available: true,
        source_mismatch: { served_bytes: 7_000, declared_bytes: 8_000 },
      }),
      makeVersion({ file_id: 2, resolution: "2160p" }),
    ];
    render(
      <VersionDropdown
        versions={versions}
        selectedVersion={versions[0]!}
        onSelectVersion={vi.fn()}
      />,
    );

    const dialog = openVersionDropdown();
    expect(dialog.queryByText("Failed")).not.toBeInTheDocument();
    expect(dialog.getByRole("button", { name: /1080p/ })).toBeInTheDocument();
  });
});

describe("VersionFlyoutItems unavailable versions", () => {
  it("hides unavailable versions by default and shows them via the toggle", () => {
    const versions = [
      makeVersion({ file_id: 1, resolution: "2160p" }),
      makeVersion({ file_id: 2, resolution: "1080p", available: false }),
    ];
    render(<VersionFlyoutItems versions={versions} onPlayVersion={vi.fn()} />);

    expect(screen.queryByText("1080p")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("menuitem", { name: /Show 1 unavailable version/ }));

    const unavailableRow = screen.getByRole("menuitem", { name: /1080p/ });
    expect(unavailableRow).toBeInTheDocument();
    expect(screen.getByText("Will retry on play")).toBeInTheDocument();
    expect(unavailableRow).toHaveClass("opacity-80");
  });

  it("shows a source mismatch on a row that is still available", () => {
    const versions = [
      makeVersion({
        file_id: 1,
        resolution: "1080p",
        available: true,
        source_mismatch: { served_bytes: 7_427_636_945, declared_bytes: 8_449_999_999 },
      }),
    ];
    render(<VersionFlyoutItems versions={versions} onPlayVersion={vi.fn()} />);

    expect(
      screen.getByText("Source mismatch — served 6.9 GB of declared 7.9 GB"),
    ).toBeInTheDocument();
    expect(screen.queryByText("Will retry on play")).not.toBeInTheDocument();
    expect(screen.queryByText("Failed")).not.toBeInTheDocument();
  });
});

describe("useVersionVisibility toggle stability", () => {
  function renderVisibility(versions: FileVersion[]) {
    return renderHook(({ list }: { list: FileVersion[] }) => useVersionVisibility(list, null), {
      initialProps: { list: versions },
    });
  }

  it("keeps the Show unavailable toggle when candidate ids rotate under the same count", () => {
    const { result, rerender } = renderVisibility([
      makeVersion({ file_id: 1 }),
      makeVersion({ file_id: 2, available: false }),
    ]);

    act(() => result.current.setShowUnavailable(true));
    expect(result.current.showUnavailable).toBe(true);

    // A liveness poll resolves the virtual candidate to a different id while
    // the count is unchanged; the viewer's toggle must not snap back.
    rerender({
      list: [makeVersion({ file_id: 11 }), makeVersion({ file_id: 12, available: false })],
    });

    expect(result.current.showUnavailable).toBe(true);
  });

  it("resets the Show unavailable toggle when the version count changes", () => {
    const { result, rerender } = renderVisibility([
      makeVersion({ file_id: 1 }),
      makeVersion({ file_id: 2, available: false }),
    ]);

    act(() => result.current.setShowUnavailable(true));
    expect(result.current.showUnavailable).toBe(true);

    rerender({
      list: [
        makeVersion({ file_id: 3 }),
        makeVersion({ file_id: 4 }),
        makeVersion({ file_id: 5, available: false }),
      ],
    });

    expect(result.current.showUnavailable).toBe(false);
  });
});

describe("useVersionVisibility failed rows", () => {
  function visibleIds(versions: FileVersion[], activeFileId?: number | null) {
    const { result } = renderHook(() => useVersionVisibility(versions, activeFileId));
    return result.current.visibleVersions.map((v) => v.file_id);
  }

  function hiddenCount(versions: FileVersion[], activeFileId?: number | null) {
    const { result } = renderHook(() => useVersionVisibility(versions, activeFileId));
    return result.current.hiddenUnavailableCount;
  }

  const live = (id: number): FileVersion => makeVersion({ file_id: id });
  const dead = (id: number): FileVersion =>
    makeVersion({ file_id: id, available: false, failed: true });
  const stale = (id: number): FileVersion => makeVersion({ file_id: id, available: false });

  it("removes failed rows instead of hiding them behind the toggle", () => {
    expect(visibleIds([live(1), dead(2), live(3)])).toEqual([1, 3]);
  });

  it("does not count removed rows as hidden", () => {
    expect(hiddenCount([live(1), dead(2), stale(3)])).toBe(1);
  });

  it("keeps the active selection visible even when it failed", () => {
    expect(visibleIds([live(1), dead(2)], 2)).toEqual([1, 2]);
  });

  it("renders the full list instead of an empty menu when everything failed", () => {
    expect(visibleIds([dead(1), dead(2)])).toEqual([1, 2]);
  });

  it("keeps transiently unavailable rows behind the toggle", () => {
    expect(visibleIds([live(1), stale(2)])).toEqual([1]);
    expect(hiddenCount([live(1), stale(2)])).toBe(1);
  });
});
