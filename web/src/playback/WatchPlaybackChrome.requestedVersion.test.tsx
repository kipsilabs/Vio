// @vitest-environment jsdom

import { useEffect, type ComponentProps } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { createWatchRouteRequest, type WatchPlaybackStartInput } from "@/pages/watchRouteHelpers";
import type { WatchPage } from "@/player/components/WatchPage";
import { WatchPlaybackHost, WatchPlaybackProvider } from "./WatchPlaybackChrome";
import { readRequestedFileId } from "./requestedVersion";
import {
  useWatchPlaybackController,
  type WatchPlaybackControllerValue,
} from "./watchPlaybackContext";

type WatchPageProps = ComponentProps<typeof WatchPage>;

const mocks = vi.hoisted(() => ({
  navigate: vi.fn(),
  player: { props: null as WatchPageProps | null },
  // One object for every render, as the query cache would return.
  detail: {
    data: {
      content_id: "movie-1",
      title: "The Movie",
      year: 2024,
      versions: [],
    },
    error: null,
  },
}));

vi.mock("@/hooks/useViewTransition", () => ({
  useViewTransitionNavigate: () => mocks.navigate,
}));
vi.mock("@/hooks/useAuth", () => ({
  useAuth: () => ({ user: { id: 1 } }),
}));
vi.mock("@/hooks/useCurrentProfile", () => ({
  useCurrentProfile: () => ({ profile: { id: "profile-1" } }),
}));
vi.mock("@/hooks/queries/seekPreferences", () => ({
  useSeekPreferences: () => ({ skipBack: 10, skipForward: 10 }),
}));
vi.mock("@/hooks/queries/settingValues", () => ({
  settingsCapabilitiesSupportKey: () => false,
  useEffectiveSettings: () => ({ data: undefined }),
  useSettingsCapabilities: () => ({ data: undefined, isSuccess: false }),
}));
vi.mock("@/hooks/queries/progress", () => ({
  useContinueWatching: () => ({ items: [] }),
}));
vi.mock("@/hooks/queries/items", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/hooks/queries/items")>()),
  useWatchDetail: () => mocks.detail,
}));
// The item's own props do not matter here, only the host's callbacks.
vi.mock("@/pages/watchRouteHelpers", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/pages/watchRouteHelpers")>()),
  buildWatchPageProps: () => ({}),
}));
vi.mock("@/player/hooks/useSeriesEpisodes", () => ({
  useSeriesEpisodes: () => ({ episodes: [] }),
}));
// Stands in for the player: the host hands it the callbacks under test.
vi.mock("@/player/components/WatchPage", () => ({
  WatchPage: (props: WatchPageProps) => {
    mocks.player.props = props;
    return <div data-testid="player" />;
  },
}));

async function renderMovie(input: WatchPlaybackStartInput) {
  const controllerRef: { current: WatchPlaybackControllerValue | null } = { current: null };
  function ControllerProbe() {
    const controller = useWatchPlaybackController();
    useEffect(() => {
      controllerRef.current = controller;
    });
    return null;
  }

  render(
    <QueryClientProvider client={new QueryClient()}>
      <MemoryRouter initialEntries={["/watch/movie-1"]}>
        <WatchPlaybackProvider>
          <ControllerProbe />
          <WatchPlaybackHost />
        </WatchPlaybackProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  );
  await waitFor(() => expect(controllerRef.current).not.toBeNull());
  act(() => controllerRef.current!.syncRouteRequest(createWatchRouteRequest(input)));
  await screen.findByTestId("player");

  return { player: () => mocks.player.props! };
}

describe("WatchPlaybackHost requested version", () => {
  beforeEach(() => {
    localStorage.clear();
    mocks.navigate.mockReset();
    mocks.player.props = null;
  });
  afterEach(cleanup);

  it("keeps the requested row after exiting a substituted playback", async () => {
    const { player } = await renderMovie({ contentId: "movie-1", libraryId: 1 });
    // The viewer asked for row 2 and the server served row 1 instead; progress
    // and history will record the effective row, so only the requested one can
    // keep the picker on the viewer's choice after a reload.
    expect(readRequestedFileId("movie-1", "profile-1")).toBeNull();

    act(() =>
      player().onExit?.({
        positionSeconds: 600,
        durationSeconds: 3600,
        selectedFileId: 2,
        lastFileId: 1,
      }),
    );

    expect(readRequestedFileId("movie-1", "profile-1")).toBe(2);
  });
});
