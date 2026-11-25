import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

import type { RequestMediaDetail, RequestMediaSeason } from "@/api/types";
import { requestInputFromMediaResult } from "@/lib/mediaRequests";
import { RequestSeasonsDialog } from "./RequestSeasonsDialog";

/** What the dialog reads and posts, without a server. */
const requests = vi.hoisted(() => ({
  fetchedSources: [] as ("tvdb" | "tmdb")[],
  mutate: vi.fn(),
}));

vi.mock("@/hooks/queries/useRequests", () => {
  const season = (name: string): RequestMediaSeason => ({
    season_number: 1,
    name,
    air_date: "2020-01-01",
    episode_count: 8,
    availability: "missing",
    requested: false,
  });
  const detail = (source: "tvdb" | "tmdb"): RequestMediaDetail => ({
    media_type: "series",
    tmdb_id: 1234,
    title: "The Show",
    year: 2020,
    seasons: [season(source === "tmdb" ? "TMDB season" : "TVDB season")],
    availability: "missing",
    request: { requestable: true },
  });
  return {
    useRequestMediaDetail: (
      _mediaType: string,
      _tmdbID: number,
      options: { enabled?: boolean; seasonSource?: "tvdb" | "tmdb" } = {},
    ) => {
      const source = options.seasonSource ?? "tvdb";
      requests.fetchedSources.push(source);
      return { data: detail(source), isFetching: false, isError: false };
    },
    useCreateMediaRequest: () => ({ mutate: requests.mutate, isPending: false }),
  };
});

beforeEach(() => {
  requests.fetchedSources.length = 0;
  requests.mutate.mockReset();
});

function show() {
  render(<RequestSeasonsDialog open onOpenChange={vi.fn()} tmdbID={1234} title="The Show" />);
}

async function submit() {
  await userEvent.click(screen.getByRole("button", { name: /^Request/ }));
  await waitFor(() => expect(requests.mutate).toHaveBeenCalled());
  return requests.mutate.mock.calls[0]?.[0] as Record<string, unknown>;
}

describe("Request seasons dialog season numbering source", () => {
  it("starts on TVDB and posts season_source tvdb by default", async () => {
    show();

    expect(await screen.findByText(/TVDB season/)).toBeInTheDocument();
    expect(requests.fetchedSources).toEqual(["tvdb"]);

    expect(await submit()).toEqual(
      expect.objectContaining({
        media_type: "series",
        tmdb_id: 1234,
        season_source: "tvdb",
      }),
    );
  });

  it("switches to TMDB, refetches the seasons and posts season_source tmdb", async () => {
    show();
    expect(await screen.findByText(/TVDB season/)).toBeInTheDocument();

    const tmdb = screen.getByRole("button", { name: "TMDB" });
    expect(tmdb).toHaveAttribute("aria-pressed", "false");
    await userEvent.click(tmdb);

    expect(await screen.findByText(/TMDB season/)).toBeInTheDocument();
    expect(screen.queryByText(/TVDB season/)).not.toBeInTheDocument();
    expect(requests.fetchedSources).toContain("tmdb");
    expect(tmdb).toHaveAttribute("aria-pressed", "true");

    expect(await submit()).toEqual(expect.objectContaining({ season_source: "tmdb" }));
  });
});

describe("requestInputFromMediaResult", () => {
  it.each(["tvdb", "tmdb"] as const)("carries a detail's season_source %s", (source) => {
    expect(
      requestInputFromMediaResult({
        media_type: "series",
        tmdb_id: 1,
        title: "The Show",
        availability: "missing",
        request: { requestable: true },
        season_source: source,
      }),
    ).toMatchObject({ season_source: source });
  });
});
