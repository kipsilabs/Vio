import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ComponentProps } from "react";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { Plus } from "lucide-react";
import type { FileVersion } from "@/api/types";
import ActionBar from "./ActionBar";

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

function renderActionBar(overrides: Partial<ActionBarProps> = {}) {
  return render(
    <QueryClientProvider client={new QueryClient()}>
      <MemoryRouter>
        <ActionBar playHref="/watch/movie-1" {...overrides} />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("ActionBar", () => {
  afterEach(() => {
    startPlayback.mockClear();
  });

  it.each([
    [false, "Mark Watched"],
    [true, "Mark Unwatched"],
  ])("labels the compact watched action by its effect (watched %s)", (isWatched, shortLabel) => {
    renderActionBar({
      compactMobile: true,
      isWatched,
      watchedLabel: isWatched ? "Mark Series Unwatched" : "Mark Series Watched",
      onToggleWatched: vi.fn(),
    });
    const button = screen.getByRole("button", {
      name: new RegExp(isWatched ? "Unwatched" : "Series Watched"),
    });
    expect(button).not.toHaveAttribute("aria-pressed");
    expect(button.querySelector(".detail-short-label")).toHaveTextContent(shortLabel);
  });

  it("does not expose an enabled pointer affordance while the watched action is pending", () => {
    renderActionBar({
      watchedLabel: "Mark Watched",
      onToggleWatched: () => {},
      isUpdatingWatched: true,
    });

    const watchedAction = screen.getByRole("button", { name: "Mark Watched" });
    expect(watchedAction).toBeDisabled();
    expect(watchedAction).toHaveClass("enabled:cursor-pointer");
    expect(watchedAction).not.toHaveClass("cursor-pointer");
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

  it("shows a status as a disabled primary action without the hover affordance", () => {
    renderWithPrimary({ primaryAction: { label: "Requested", disabled: true } });

    const status = screen.getByRole("button", { name: "Requested" });
    expect(status).toBeDisabled();
    expect(status).not.toHaveClass("cursor-pointer");
    expect(status).not.toHaveClass("motion-safe:hover:scale-[1.02]");
  });

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

  it("renders secondary glass actions and external links after the primary action", () => {
    const onFollow = vi.fn();
    renderWithPrimary({
      primaryAction: { label: "Approved", disabled: true },
      secondaryActions: [
        { id: "follow", label: "Stop notifying me", onClick: onFollow, pressed: true },
      ],
      links: [{ label: "TMDB", href: "https://www.themoviedb.org/movie/603" }],
    });

    const follow = screen.getByRole("button", { name: "Stop notifying me" });
    expect(follow).toHaveAttribute("aria-pressed", "true");
    expect(follow).toHaveClass("glass-hover", "rounded-full", "h-11");
    fireEvent.click(follow);
    expect(onFollow).toHaveBeenCalledOnce();

    const link = screen.getByRole("link", { name: "TMDB" });
    expect(link).toHaveAttribute("href", "https://www.themoviedb.org/movie/603");
    expect(link).toHaveAttribute("target", "_blank");
    expect(link).toHaveAttribute("rel", "noreferrer");
    // Without library props there are no library actions, so no overflow menu.
    expect(screen.queryByRole("button", { name: "More actions" })).not.toBeInTheDocument();
  });
});
