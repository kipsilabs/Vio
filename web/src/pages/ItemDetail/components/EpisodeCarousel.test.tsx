import { renderToStaticMarkup } from "react-dom/server";
import { act, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { beforeEach, describe, expect, it, vi } from "vitest";
import EpisodeCarousel from "./EpisodeCarousel";

const capturedMenuProps: Record<string, unknown>[] = [];
const prefetchEpisodeDetail = vi.hoisted(() => vi.fn());

vi.mock("@/hooks/queries/catalogRead", () => ({
  usePrefetchCatalogItemDetail: () => prefetchEpisodeDetail,
}));

vi.mock("@/hooks/useOverlayPrefs", () => ({
  useOverlayPrefs: () => ({ quickActionMode: "watched" }),
}));

vi.mock("@/components/MediaItemMenu", () => ({
  default: (props: Record<string, unknown>) => {
    capturedMenuProps.push(props);
    return <div />;
  },
}));

vi.mock("@/hooks/useCarouselEmbla", () => ({
  useCarouselEmbla: () => ({
    emblaApi: null,
    emblaRef: { current: null },
    canScrollPrev: false,
    canScrollNext: false,
    scrollPrev: () => {},
    scrollNext: () => {},
  }),
}));

// Capability gating: the carousel stands in for the resolved answer
// (available) so the badge path is exercised without a QueryClient.
vi.mock("@/hooks/queries/episodeRelease", async (importOriginal) => {
  const original = (await importOriginal()) as typeof import("@/hooks/queries/episodeRelease");
  return {
    ...original,
    useEpisodeReleaseCapability: () => ({ data: { available: true } }),
  };
});

describe("EpisodeCarousel", () => {
  beforeEach(() => {
    capturedMenuProps.length = 0;
    prefetchEpisodeDetail.mockClear();
  });

  it("prefetches only after a card shows sustained navigation intent", () => {
    // A pointer sweeping the rail crosses every card; prefetching each one it
    // passes would start cache work for the whole season. Intent is a dwell.
    vi.useFakeTimers();
    try {
      render(
        <MemoryRouter>
          <EpisodeCarousel
            currentEpisodeNumber={2}
            episodes={[
              {
                content_id: "ep-1",
                season_number: 1,
                episode_number: 1,
                title: "Pilot",
                overview: "",
                air_date: null,
                runtime: 42,
                still_url: "",
                still_thumbhash: "",
                files: [],
              },
            ]}
          />
        </MemoryRouter>,
      );

      const card = screen.getAllByRole("link", { name: /Pilot/ })[0]!.closest("div.group\\/card")!;

      fireEvent.mouseEnter(card);
      act(() => vi.advanceTimersByTime(100));
      fireEvent.mouseLeave(card);
      act(() => vi.advanceTimersByTime(200));
      expect(prefetchEpisodeDetail).not.toHaveBeenCalled();

      fireEvent.mouseEnter(card);
      act(() => vi.advanceTimersByTime(140));
      expect(prefetchEpisodeDetail).toHaveBeenCalledWith("ep-1");
    } finally {
      vi.useRealTimers();
    }
  });

  it("enables unwatched shortcuts without losing partial-progress restart eligibility", () => {
    renderToStaticMarkup(
      <MemoryRouter>
        <EpisodeCarousel
          currentEpisodeNumber={2}
          episodes={[
            {
              content_id: "ep-1",
              season_number: 1,
              episode_number: 1,
              title: "Pilot",
              overview: "",
              air_date: null,
              runtime: 42,
              still_url: "",
              still_thumbhash: "",
              files: [],
            },
            {
              content_id: "ep-2",
              season_number: 1,
              episode_number: 2,
              title: "Next",
              overview: "",
              air_date: null,
              runtime: 43,
              still_url: "",
              still_thumbhash: "",
              files: [],
              user_data: {
                played: false,
                position_seconds: 120,
                duration_seconds: 1800,
              },
            },
          ]}
        />
      </MemoryRouter>,
    );

    expect(capturedMenuProps[0]).toMatchObject({
      contentId: "ep-1",
      mediaType: "episode",
      userState: {
        played: false,
        is_favorite: false,
        in_watchlist: false,
      },
      showCollectionActions: false,
      showWatchedShortcut: true,
      hasPartialProgress: false,
      quickActionMode: "watched",
    });
    expect(capturedMenuProps[1]).toMatchObject({
      contentId: "ep-2",
      mediaType: "episode",
      userState: {
        played: false,
        is_favorite: false,
        in_watchlist: false,
      },
      showWatchedShortcut: true,
      hasPartialProgress: true,
      quickActionMode: "watched",
    });
  });

  it("badges server-classified upcoming episodes and keeps navigation", () => {
    const markup = renderToStaticMarkup(
      <MemoryRouter>
        <EpisodeCarousel
          currentEpisodeNumber={1}
          episodes={[
            {
              content_id: "ep-aired",
              season_number: 2,
              episode_number: 1,
              title: "Remembrance Day",
              overview: "",
              air_date: "2022-02-18",
              release_state: "released",
              runtime: 60,
              still_url: "https://stills.example/e1.jpg",
              still_thumbhash: "",
              files: [],
            },
            {
              content_id: "ep-future",
              season_number: 2,
              episode_number: 3,
              title: "Rabbits Don't Swim",
              overview: "",
              air_date: "2099-10-03",
              release_state: "upcoming",
              runtime: 60,
              still_url: "https://stills.example/e3.jpg",
              still_thumbhash: "",
              files: [],
            },
          ]}
        />
      </MemoryRouter>,
    );

    expect(markup).toContain("Upcoming");
    expect(markup).toContain("opacity-45");
    expect(markup).toContain("/item/ep-future");
  });
});
