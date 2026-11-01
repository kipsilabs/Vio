import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ComponentProps } from "react";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { Plus } from "lucide-react";
import type { FileVersion } from "@/api/types";
import ActionBar from "./ActionBar";

const discoveryMocks = vi.hoisted(() => ({
  capability: vi.fn(),
  discover: vi.fn(),
}));
vi.mock("@/api/v2/virtualLibrary", () => ({
  fetchVirtualLibraryCapability: discoveryMocks.capability,
}));
vi.mock("@/api/v2/mediaCandidates", () => ({
  discoverVirtualCandidates: discoveryMocks.discover,
  awaitVirtualCandidatesRefresh: vi.fn(),
  cancelVirtualCandidatesRefresh: vi.fn(),
}));

const startPlayback = vi.fn();
vi.mock("@/playback/watchPlaybackContext", () => ({
  useWatchPlaybackController: () => ({ startPlayback }),
}));

vi.mock("./SubtitlesPopover", () => ({
  default: () => null,
}));

vi.mock("@/components/AddToCollectionDialog", () => ({
  default: () => null,
}));

type ActionBarProps = ComponentProps<typeof ActionBar>;

const selectedVersion: FileVersion = {
  file_id: 1,
  resolution: "1080p",
  codec_video: "h264",
  codec_audio: "aac",
  hdr: false,
  container: "mkv",
  file_size: 0,
  duration: 7_200,
  bitrate: 0,
};

function actionBarUi(overrides: Partial<ActionBarProps> = {}) {
  return (
    <QueryClientProvider client={new QueryClient()}>
      <MemoryRouter>
        <ActionBar playHref="/watch/movie-1" {...overrides} />
      </MemoryRouter>
    </QueryClientProvider>
  );
}

function renderActionBar(overrides: Partial<ActionBarProps> = {}) {
  return render(actionBarUi(overrides));
}

describe("ActionBar", () => {
  it("does not expose an enabled pointer affordance while the watched action is pending", () => {
    renderActionBar({
      watchedLabel: "Mark Watched",
      onToggleWatched: () => {},
      isUpdatingWatched: true,
    });

    const watchedAction = screen.getByRole("button", { name: "Mark Watched" });
    expect(watchedAction).toBeDisabled();
  });

  it("sends forceRelink when the selected version is unavailable", () => {
    const unavailableVersion: FileVersion = { ...selectedVersion, available: false };
    renderActionBar({
      selectedVersion: unavailableVersion,
      versions: [unavailableVersion],
      contentId: "movie-1",
    });

    screen.getByRole("button", { name: "Play" }).click();
    expect(startPlayback).toHaveBeenCalledOnce();
    const arg = startPlayback.mock.calls[0]?.[0] as { forceRelink?: boolean } | undefined;
    expect(arg?.forceRelink).toBe(true);
  });

  it("does not send forceRelink when the selected version is available", () => {
    renderActionBar({
      selectedVersion,
      versions: [selectedVersion],
      contentId: "movie-1",
    });

    screen.getByRole("button", { name: "Play" }).click();
    expect(startPlayback).toHaveBeenCalledOnce();
    const arg = startPlayback.mock.calls[0]?.[0] as { forceRelink?: boolean } | undefined;
    expect(arg?.forceRelink).toBeUndefined();
  });

  it("sends forceRelink from the overflow restart path too", () => {
    const unavailableVersion: FileVersion = { ...selectedVersion, available: false };
    const secondVersion: FileVersion = { ...selectedVersion, file_id: 2, resolution: "2160p" };
    renderActionBar({
      selectedVersion: unavailableVersion,
      versions: [unavailableVersion, secondVersion],
      contentId: "movie-1",
      playHref: "/watch/movie-1",
      playLabel: "Resume",
      restartHref: "/watch/movie-1?restart=1",
    });

    fireEvent.click(screen.getByRole("button", { name: "More actions" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Play from Beginning" }));

    expect(startPlayback).toHaveBeenCalledOnce();
    const arg = startPlayback.mock.calls[0]?.[0] as
      | { fileId?: number; forceRelink?: boolean }
      | undefined;

    expect(arg?.fileId).toBe(unavailableVersion.file_id);
    expect(arg?.forceRelink).toBe(true);
  });

  it("offers a replacement row when the chosen version vanished", () => {
    const replacement = { ...selectedVersion, file_id: 2, resolution: "2160p" };
    const onSelectVersion = vi.fn();
    const view = renderActionBar({
      playHref: "/watch/movie-1",
      selectedVersion: null,
      selectedVersionMissing: true,
      versions: [replacement],
      onSelectVersion,
    });

    expect(screen.getByRole("button", { name: "Version unavailable" })).toBeDisabled();
    expect(
      screen.getByText("The selected version is no longer available. Pick a version to play."),
    ).toBeInTheDocument();
    expect(startPlayback).not.toHaveBeenCalled();

    // The picker is reachable even though only one replacement remains, and it
    // does not pretend that row is already selected.
    fireEvent.click(screen.getByRole("button", { name: "Version" }));
    fireEvent.click(screen.getByRole("button", { name: /2160p/ }));
    expect(onSelectVersion).toHaveBeenCalledWith(replacement);
    expect(startPlayback).not.toHaveBeenCalled();

    // The page applies the pick: Play becomes available and starts that row.
    view.rerender(
      actionBarUi({
        contentId: "movie-1",
        playHref: "/watch/movie-1",
        selectedVersion: replacement,
        selectedVersionMissing: false,
        versions: [replacement],
        onSelectVersion,
      }),
    );
    const play = screen.getByRole("button", { name: "Play" });
    expect(play).toBeEnabled();
    fireEvent.click(play);
    expect(startPlayback).toHaveBeenCalledOnce();
  });
});

it("keeps compact secondary actions available in the overflow menu", () => {
  const onToggleFavorite = vi.fn();
  const onRatingChange = vi.fn();
  renderActionBar({ compactMobile: true, onToggleFavorite, onRatingChange });
  fireEvent.click(screen.getByRole("button", { name: "More actions" }));
  const menu = screen.getByRole("menu");
  fireEvent.keyDown(within(menu).getByRole("radio", { name: "1 star" }), { key: "ArrowRight" });
  expect(onRatingChange).toHaveBeenCalledWith(1);
  fireEvent.click(within(menu).getByRole("menuitem", { name: "Add to favorites" }));
  expect(onToggleFavorite).toHaveBeenCalledOnce();
  expect(screen.queryByRole("menu")).not.toBeInTheDocument();
});

it("skips desktop-hidden actions when focusing and navigating the menu", () => {
  const rects = vi.spyOn(HTMLElement.prototype, "getClientRects").mockImplementation(function (
    this: HTMLElement,
  ) {
    return (this.closest(".detail-mobile-menu-actions")
      ? []
      : [new DOMRect(0, 0, 100, 30)]) as unknown as DOMRectList;
  });
  try {
    renderActionBar({
      compactMobile: true,
      onToggleFavorite: vi.fn(),
      onToggleWatchlist: vi.fn(),
      canCurateMetadata: true,
      onEditMetadata: vi.fn(),
    });
    fireEvent.click(screen.getByRole("button", { name: "More actions" }));
    const first = screen.getByRole("menuitem", { name: "Add to Watchlist" });
    const last = screen.getByRole("menuitem", { name: "Edit Metadata" });
    expect(first).toHaveFocus();
    fireEvent.keyDown(first, { key: "ArrowUp" });
    expect(last).toHaveFocus();
    fireEvent.keyDown(last, { key: "ArrowDown" });
    expect(first).toHaveFocus();
    fireEvent.keyDown(first, { key: "End" });
    expect(last).toHaveFocus();
    fireEvent.keyDown(last, { key: "Home" });
    expect(first).toHaveFocus();
    fireEvent.keyDown(first, { key: "e" });
    expect(last).toHaveFocus();
  } finally {
    rects.mockRestore();
  }
});

it.each([null, 3])("focuses the active star in a rating-only menu (rating %s)", (rating) => {
  const rects = vi
    .spyOn(HTMLElement.prototype, "getClientRects")
    .mockReturnValue([new DOMRect(0, 0, 100, 30)] as unknown as DOMRectList);
  try {
    const onRatingChange = vi.fn();
    renderActionBar({ compactMobile: true, rating, onRatingChange });
    const trigger = screen.getByRole("button", { name: "More actions" });
    fireEvent.click(trigger);
    const menu = screen.getByRole("menu");
    const star = within(menu).getByRole("radio", { name: rating === 3 ? "3 stars" : "1 star" });
    expect(star).toHaveFocus();
    for (const key of ["ArrowDown", "ArrowUp", "Home", "End"]) {
      trigger.focus();
      fireEvent.keyDown(menu, { key });
      expect(star).toHaveFocus();
    }
    fireEvent.keyDown(star, { key: "ArrowRight" });
    expect(onRatingChange).toHaveBeenCalledWith((rating ?? 0) + 1);
    fireEvent.keyDown(star, { key: "Escape" });
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
  } finally {
    rects.mockRestore();
  }
});

it("moves through the menu with ArrowUp/ArrowDown instead of changing the rating", () => {
  const rects = vi
    .spyOn(HTMLElement.prototype, "getClientRects")
    .mockReturnValue([new DOMRect(0, 0, 100, 30)] as unknown as DOMRectList);
  try {
    const onRatingChange = vi.fn();
    renderActionBar({
      compactMobile: true,
      rating: null,
      onRatingChange,
      onToggleFavorite: vi.fn(),
      onToggleWatchlist: vi.fn(),
    });
    fireEvent.click(screen.getByRole("button", { name: "More actions" }));
    const menu = screen.getByRole("menu");
    const star = within(menu).getByRole("radio", { name: "1 star" });
    for (const key of ["ArrowDown", "ArrowUp"]) {
      star.focus();
      fireEvent.keyDown(star, { key });
      expect(document.activeElement).toHaveAttribute("role", "menuitem");
    }
    expect(onRatingChange).not.toHaveBeenCalled();
  } finally {
    rects.mockRestore();
  }
});

describe("ActionBar primary action", () => {
  function renderWithPrimary(overrides: Partial<ActionBarProps>) {
    return render(
      <QueryClientProvider client={new QueryClient()}>
        <MemoryRouter>
          <ActionBar {...overrides} />
        </MemoryRouter>
      </QueryClientProvider>,
    );
  }

  it("blocks the action and marks it busy while it is pending", () => {
    const onClick = vi.fn();
    renderWithPrimary({
      primaryAction: { label: "Request movie", icon: Plus, onClick, pending: true },
    });

    const request = screen.getByRole("button", { name: "Request movie" });
    expect(request).toBeDisabled();
    expect(request).toHaveAttribute("aria-busy", "true");
    expect(request.querySelector("svg")).toHaveClass("animate-spin");
  });
});
