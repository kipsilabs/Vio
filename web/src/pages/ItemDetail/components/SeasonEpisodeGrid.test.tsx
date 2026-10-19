import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { beforeEach, describe, expect, it, vi } from "vitest";
import SeasonEpisodeGrid from "./SeasonEpisodeGrid";

const capturedMenuProps: Record<string, unknown>[] = [];
const spoilerPrefs = { hideImages: false, hideOverviews: false };

vi.mock("@/components/MediaItemMenu", () => ({
  default: (props: Record<string, unknown>) => {
    capturedMenuProps.push(props);
    return null;
  },
}));

vi.mock("@/hooks/useOverlayPrefs", () => ({
  useOverlayPrefs: () => ({ prefs: null, quickActionMode: "watched" }),
}));

vi.mock("@/hooks/useEpisodeSpoilerPrefs", () => ({
  useEpisodeSpoilerPrefs: () => spoilerPrefs,
}));

vi.mock("@/hooks/queries/catalogRead", () => ({
  usePrefetchCatalogItemDetail: () => vi.fn(),
}));

// Capability gating: the grid stands in for the resolved answer (available)
// so the badge path is exercised without a QueryClient.
vi.mock("@/hooks/queries/episodeRelease", async (importOriginal) => {
  const original = (await importOriginal()) as typeof import("@/hooks/queries/episodeRelease");
  return {
    ...original,
    useEpisodeReleaseCapability: () => ({ data: { available: true } }),
  };
});

describe("SeasonEpisodeGrid", () => {
  beforeEach(() => {
    capturedMenuProps.length = 0;
    spoilerPrefs.hideImages = false;
    spoilerPrefs.hideOverviews = false;
  });

  it("hides the still and overview only for episodes the profile has not started", () => {
    spoilerPrefs.hideImages = true;
    spoilerPrefs.hideOverviews = true;
    const episode = (n: number, userData?: Record<string, unknown>) => ({
      content_id: `ep-${n}`,
      season_number: 1,
      episode_number: n,
      title: `Episode title ${n}`,
      overview: `Overview ${n}`,
      air_date: null,
      runtime: 42,
      still_url: `https://img.test/still-${n}.jpg`,
      still_thumbhash: "",
      files: [],
      user_data: userData as never,
    });
    render(
      <MemoryRouter>
        <SeasonEpisodeGrid
          isLoading={false}
          episodes={[
            episode(1, { played: true }),
            episode(2, { played: false, is_in_progress: true, position_seconds: 300 }),
            episode(3, { played: false }),
            episode(4),
          ]}
        />
      </MemoryRouter>,
    );

    for (const n of [1, 2]) {
      expect(screen.getByAltText(`Episode title ${n}`)).not.toHaveClass("blur-xl");
      expect(screen.getByText(`Overview ${n}`)).toBeInTheDocument();
    }
    for (const n of [3, 4]) {
      expect(screen.getByAltText(`Episode title ${n}`)).toHaveClass("blur-xl");
      expect(screen.queryByText(`Overview ${n}`)).not.toBeInTheDocument();
    }
  });

  it("marks an episode only when none of its files can be read", () => {
    const file = {
      resolution: "",
      codec_video: "",
      hdr: false,
      audio_channels: 0,
      container: "",
      file_size: 0,
    };
    const episode = (id: string, number: number, files: object[]) => ({
      content_id: id,
      season_number: 1,
      episode_number: number,
      title: `Episode title ${number}`,
      overview: "",
      air_date: null,
      runtime: 0,
      still_url: "",
      still_thumbhash: "",
      files: files as never,
    });
    render(
      <MemoryRouter>
        <SeasonEpisodeGrid
          isLoading={false}
          episodes={[
            episode("ep-3", 3, [{ ...file, file_id: 3, unreadable: true }]),
            episode("ep-4", 4, [
              { ...file, file_id: 4, unreadable: true },
              { ...file, file_id: 5, resolution: "1080p" },
            ]),
            episode("ep-5", 5, [{ ...file, file_id: 6, resolution: "1080p" }]),
          ]}
        />
      </MemoryRouter>,
    );

    expect(capturedMenuProps[0]).toMatchObject({
      contentId: "ep-3",
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

    // Only episode 3 has no readable version; episode 4 still plays its
    // second file.
    expect(screen.getAllByText("Damaged file")).toHaveLength(1);
    const card = screen.getByText("Episode title 3").closest(".media-card");
    expect(card).not.toBeNull();
    expect(card).toHaveTextContent("Damaged file");
  });

  it("places the watched circle-check beside the episode label instead of over the artwork", () => {
    render(
      <MemoryRouter>
        <SeasonEpisodeGrid
          isLoading={false}
          episodes={[
            {
              content_id: "ep-1",
              season_number: 1,
              episode_number: 1,
              title: "Pilot",
              overview: "A beginning.",
              air_date: null,
              runtime: 42,
              still_url: "",
              still_thumbhash: "",
              files: [],
              user_data: {
                played: true,
                position_seconds: 1800,
                duration_seconds: 1800,
              },
            },
            {
              content_id: "ep-2",
              season_number: 1,
              episode_number: 2,
              title: "Next",
              overview: "Another episode.",
              air_date: null,
              runtime: 43,
              still_url: "",
              still_thumbhash: "",
              files: [],
              user_data: {
                played: false,
                position_seconds: 0,
                duration_seconds: 1800,
              },
            },
          ]}
        />
      </MemoryRouter>,
    );

    const episodeLabel = screen.getByText("Episode 1");
    const watchedIndicator = screen.getByLabelText("Watched");

    expect(episodeLabel.parentElement).toContainElement(watchedIndicator);
    expect(watchedIndicator).toHaveAttribute("data-watched-indicator", "icon-only");
    expect(watchedIndicator.querySelector(".lucide-circle-check")).toBeTruthy();
    expect(watchedIndicator.closest(".media-card-image")).toBeNull();
    expect(screen.getByText("Episode 2").parentElement).not.toContainElement(watchedIndicator);
    expect(screen.getAllByLabelText("Watched")).toHaveLength(1);
  });
  it("badges server-classified upcoming episodes and leaves released ones alone", () => {
    render(
      <MemoryRouter>
        <SeasonEpisodeGrid
          isLoading={false}
          episodes={[
            {
              content_id: "ep-future",
              season_number: 2,
              episode_number: 3,
              title: "Rabbits Don't Swim",
              overview: "Not yet aired.",
              air_date: "2099-10-03",
              release_state: "upcoming",
              runtime: 60,
              still_url: "https://stills.example/e3.jpg",
              still_thumbhash: "",
              files: [],
            },
            {
              content_id: "ep-aired",
              season_number: 2,
              episode_number: 1,
              title: "Remembrance Day",
              overview: "Aired.",
              air_date: "2022-02-18",
              release_state: "released",
              runtime: 60,
              still_url: "https://stills.example/e1.jpg",
              still_thumbhash: "",
              files: [],
            },
          ]}
        />
      </MemoryRouter>,
    );

    const upcomingBadges = screen.getAllByText("Upcoming", { exact: false });
    // Artwork overlay badge plus the meta-line label ("Upcoming · Oct 3, 2099").
    expect(upcomingBadges.length).toBeGreaterThanOrEqual(2);
    expect(screen.getByText("Remembrance Day")).toBeTruthy();
  });
  it("caps the grid at four rows and scrolls the rest", () => {
    render(
      <MemoryRouter>
        <SeasonEpisodeGrid
          isLoading={false}
          episodes={[
            {
              content_id: "ep-1",
              season_number: 1,
              episode_number: 1,
              title: "Pilot",
              overview: "An episode.",
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
              title: "Second",
              overview: "An episode.",
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

    const grid = screen.getByText("Pilot").closest(".grid");
    expect(grid).not.toBeNull();
    // Two columns is the narrowest layout. A single column made the capped
    // section over two viewports tall on a phone, so it scrolled inside a
    // region the reader could not see the extent of.
    expect(grid).toHaveClass("grid-cols-2", "sm:grid-cols-3", "lg:grid-cols-5");
    expect(grid).toHaveClass("overflow-y-auto");
    // Two episodes is under the cap, so nothing is clipped and the section
    // keeps its natural height rather than showing an inert scrollport.
    expect((grid as HTMLElement).style.maxHeight).toBe("");
  });

  it("marks episodes whose overview was machine-translated", () => {
    const episode = (id: string, number: number, fields?: string[]) => ({
      content_id: id,
      season_number: 1,
      episode_number: number,
      title: `Episode title ${number}`,
      overview: `Overview ${number}`,
      air_date: null,
      runtime: 0,
      still_url: "",
      still_thumbhash: "",
      files: [],
      machine_translated_fields: fields,
    });
    render(
      <MemoryRouter>
        <SeasonEpisodeGrid
          isLoading={false}
          episodes={[episode("ep-1", 1, ["overview"]), episode("ep-2", 2)]}
        />
      </MemoryRouter>,
    );
    expect(screen.getAllByText("Translated by AI")).toHaveLength(1);
  });

  it("drops the translation label with a spoiler-hidden overview", () => {
    spoilerPrefs.hideOverviews = true;
    const episode = (id: string, number: number, userData?: Record<string, unknown>) => ({
      content_id: id,
      season_number: 1,
      episode_number: number,
      title: `Episode title ${number}`,
      overview: `Overview ${number}`,
      air_date: null,
      runtime: 0,
      still_url: "",
      still_thumbhash: "",
      files: [],
      machine_translated_fields: ["overview"],
      user_data: userData as never,
    });
    render(
      <MemoryRouter>
        <SeasonEpisodeGrid
          isLoading={false}
          episodes={[episode("ep-1", 1, { played: true }), episode("ep-2", 2)]}
        />
      </MemoryRouter>,
    );
    // Only the watched episode shows its overview, so only it carries the label.
    expect(screen.getAllByText("Translated by AI")).toHaveLength(1);
    const hiddenCard = screen.getByText("Episode title 2").closest("a");
    expect(hiddenCard).not.toHaveTextContent("Translated by AI");
  });
});
