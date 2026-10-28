// @vitest-environment jsdom

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { FileVersion } from "@/api/types";
import { PROVISIONAL_INVENTORY_TIMEOUT_MS } from "@/player/utils/inventoryProvenance";
import ActionBar from "./ActionBar";

// The media page reads the selected file's probed tracks from the watch detail.
// These tests drive that read directly so they can hold it at "no rows yet"
// (the catalog fallback the page used to offer) and then publish rows.
const watchDetail = vi.hoisted(() => ({
  data: undefined as { versions?: FileVersion[] } | undefined,
  refetch: vi.fn(),
}));

const playback = vi.hoisted(() => ({ startPlayback: vi.fn() }));

vi.mock("@/hooks/queries/items", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/hooks/queries/items")>();
  return {
    ...actual,
    useWatchDetail: () => watchDetail,
    useDeleteMediaItem: () => ({ isPending: false, mutate: vi.fn() }),
  };
});

vi.mock("@/hooks/queries/subtitles", () => ({
  useDownloadedSubtitles: () => ({ data: [], isLoading: false }),
}));

vi.mock("@/playback/watchPlaybackContext", () => ({
  useWatchPlaybackController: () => ({ startPlayback: playback.startPlayback }),
}));

vi.mock("@/components/AddToCollectionDialog", () => ({
  default: () => null,
}));

const selectedVersion: FileVersion = {
  file_id: 7,
  file_path: "/media/heat.mkv",
  resolution: "1080p",
  codec_video: "h264",
  codec_audio: "eac3",
  hdr: false,
  container: "mkv",
  file_size: 0,
  duration: 7_200,
  bitrate: 0,
  // Catalog fallback rows. They are not probe evidence for this file until the
  // watch detail publishes its own inventory.
  audio_tracks: [{ language: "eng", codec: "eac3", channels: 6 }],
  subtitle_tracks: [{ index: 0, language: "en", codec: "subrip", title: "English" }],
};

const probedVersion: FileVersion = {
  ...selectedVersion,
  audio_tracks: [{ language: "eng", codec: "eac3", channels: 6, default: true }],
  subtitle_tracks: [
    { index: 0, language: "en", codec: "subrip", title: "English" },
    { index: 1, language: "fr", codec: "subrip", title: "French" },
  ],
};

function actionBarUi(
  queryClient: QueryClient,
  overrides: Partial<Parameters<typeof ActionBar>[0]> = {},
) {
  return (
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <ActionBar
          contentId="movie-1"
          versions={[selectedVersion]}
          selectedVersion={selectedVersion}
          playHref="/watch/movie-1"
          onSelectSubtitle={vi.fn()}
          onSelectSubtitleOff={vi.fn()}
          onSelectAudioTrack={vi.fn()}
          {...overrides}
        />
      </MemoryRouter>
    </QueryClientProvider>
  );
}

function renderActionBar(overrides: Partial<Parameters<typeof ActionBar>[0]> = {}) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  const view = render(actionBarUi(queryClient, overrides));
  return {
    ...view,
    rerender: (next: Partial<Parameters<typeof ActionBar>[0]> = {}) =>
      view.rerender(actionBarUi(queryClient, { ...overrides, ...next })),
  };
}

function openPopover(name: RegExp) {
  fireEvent.click(screen.getByRole("button", { name }));
}

describe("ActionBar pre-play track inventory", () => {
  afterEach(() => {
    vi.useRealTimers();
    watchDetail.data = undefined;
    watchDetail.refetch.mockReset();
    playback.startPlayback.mockReset();
  });

  it("withholds catalog subtitle rows until the selected file publishes its own inventory", () => {
    watchDetail.data = { versions: [{ ...selectedVersion, subtitle_tracks: [] }] };
    renderActionBar();

    expect(screen.getByRole("button", { name: /Subs/ })).toHaveTextContent("Discovering…");

    openPopover(/Subs/);
    expect(screen.getByText("Discovering subtitles…")).toBeTruthy();
    // The fallback row is not evidence the file carries it, so offering it is
    // how a stale pick reached playback.
    expect(screen.queryByText("English")).toBeNull();
  });

  it("withholds catalog audio rows until the selected file publishes its own inventory", () => {
    watchDetail.data = { versions: [{ ...selectedVersion, audio_tracks: [] }] };
    renderActionBar();

    expect(screen.getByRole("button", { name: /Audio/ })).toHaveTextContent("Discovering…");

    openPopover(/Audio/);
    expect(screen.getByText("Discovering audio tracks…")).toBeTruthy();
    expect(screen.queryByText("English")).toBeNull();
  });

  it("shows the live rows once the watch detail publishes the probed inventory", () => {
    watchDetail.data = { versions: [{ ...selectedVersion, subtitle_tracks: [] }] };
    const view = renderActionBar();
    openPopover(/Subs/);
    expect(screen.getByText("Discovering subtitles…")).toBeTruthy();

    watchDetail.data = { versions: [probedVersion] };
    view.rerender();

    expect(screen.queryByText("Discovering subtitles…")).toBeNull();
    expect(screen.getAllByText("English").length).toBeGreaterThan(0);
    expect(screen.getAllByText("French").length).toBeGreaterThan(0);
  });

  it("keeps Play available while the inventory probe is still running", () => {
    watchDetail.data = { versions: [{ ...selectedVersion, subtitle_tracks: [] }] };
    renderActionBar();

    const play = screen.getByRole("button", { name: /Play/ });
    expect(play).toBeEnabled();
    expect(screen.getByText("Discovering…")).toBeTruthy();

    // The probe gates the menu rows only; starting playback never waits for it.
    fireEvent.click(play);
    expect(playback.startPlayback).toHaveBeenCalledOnce();
  });

  it("replaces the stalled subtitle spinner with a retry that re-reads the watch detail", () => {
    vi.useFakeTimers();
    watchDetail.data = { versions: [{ ...selectedVersion, subtitle_tracks: [] }] };
    renderActionBar();
    openPopover(/Subs/);

    act(() => {
      vi.advanceTimersByTime(PROVISIONAL_INVENTORY_TIMEOUT_MS);
    });

    expect(screen.queryByText("Discovering subtitles…")).toBeNull();
    expect(screen.getByText("Subtitles unavailable.")).toBeTruthy();
    expect(screen.queryByText("English")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Retry" }));

    expect(watchDetail.refetch).toHaveBeenCalledOnce();
    expect(screen.getByText("Discovering subtitles…")).toBeTruthy();
    expect(screen.getByRole("button", { name: /Subs/ })).toHaveTextContent("Discovering…");
  });

  it("replaces the stalled audio spinner with a retry that re-reads the watch detail", () => {
    vi.useFakeTimers();
    watchDetail.data = { versions: [{ ...selectedVersion, audio_tracks: [] }] };
    renderActionBar();
    openPopover(/Audio/);

    act(() => {
      vi.advanceTimersByTime(PROVISIONAL_INVENTORY_TIMEOUT_MS);
    });

    expect(screen.queryByText("Discovering audio tracks…")).toBeNull();
    expect(screen.getByText("Audio tracks unavailable.")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Retry" }));

    expect(watchDetail.refetch).toHaveBeenCalledOnce();
    expect(screen.getByText("Discovering audio tracks…")).toBeTruthy();
  });

  it("keeps the empty subtitle state for a version with no catalog rows at all", () => {
    watchDetail.data = { versions: [{ ...selectedVersion, subtitle_tracks: [] }] };
    const emptyVersion: FileVersion = { ...selectedVersion, subtitle_tracks: [] };
    renderActionBar({
      versions: [emptyVersion],
      selectedVersion: emptyVersion,
    });

    expect(screen.getByRole("button", { name: /Subs/ })).not.toHaveTextContent("Discovering…");
    openPopover(/Subs/);
    expect(screen.getByText("No subtitles available.")).toBeTruthy();
    expect(screen.queryByText("Discovering subtitles…")).toBeNull();
  });
});
