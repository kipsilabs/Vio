import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { createEmptyQueryDefinition, type QueryDefinition } from "@/api/types";
import type { CatalogSearchState } from "@/pages/catalogSearchParams";

import CatalogFiltersPanel from "./CatalogFiltersPanel";

vi.mock("@/hooks/queries/catalog", () => ({
  useCatalogFilters: () => ({ data: { genres: ["Fantasy"] }, isLoading: false }),
  useCatalogMetadataFilters: () => ({ data: { genres: ["Fantasy"] }, isLoading: false }),
}));

vi.mock("@/hooks/queries/ratingsCapability", () => ({
  useShownRatingSources: () => new Set(["imdb", "tmdb"]),
}));

vi.mock("@/hooks/queries/personSearch", () => ({
  usePersonSearch: () => ({ data: [], isLoading: false }),
}));

// Rules the Guided view can't show: a negated genre, two actors, and an OR group.
const ADVANCED_RULES: QueryDefinition = {
  library_ids: [7],
  match: "all",
  groups: [
    {
      match: "all",
      rules: [
        { field: "genre", op: "is_not", value: "Horror" },
        { field: "actor", op: "is", value: "Actor A" },
        { field: "actor", op: "is", value: "Actor B" },
      ],
    },
    {
      match: "any",
      rules: [
        { field: "genre", op: "is", value: "Comedy" },
        { field: "genre", op: "is", value: "Drama" },
      ],
    },
  ],
  sort: { field: "added_at", order: "desc" },
  limit: 250,
};

// What a genre link on a book page puts in the catalog URL.
const GENRE_LINK: QueryDefinition = {
  ...createEmptyQueryDefinition(),
  groups: [{ match: "all", rules: [{ field: "genre", op: "contains", value: "Fantasy" }] }],
};

beforeEach(() => {
  HTMLElement.prototype.scrollIntoView = vi.fn();
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function renderPanel(state: CatalogSearchState) {
  const onStateChange = vi.fn<(next: CatalogSearchState) => void>();
  render(<CatalogFiltersPanel state={state} onStateChange={onStateChange} />);
  return onStateChange;
}

function choose(currentLabel: string, option: string) {
  const select = screen
    .getAllByRole("combobox")
    .find((element) => element.textContent?.trim() === currentLabel);
  if (!select) throw new Error(`No select shows ${currentLabel}`);
  fireEvent.click(select);
  fireEvent.click(screen.getByRole("option", { name: option }));
}

function lastQuery(onStateChange: ReturnType<typeof renderPanel>) {
  return onStateChange.mock.lastCall?.[0].query_definition;
}

describe("CatalogFiltersPanel toolbar", () => {
  it("changes only the sort and keeps rules the Guided view can't show", () => {
    const onStateChange = renderPanel({ source: "query", query_definition: ADVANCED_RULES });

    choose("Date Added", "Title");

    expect(lastQuery(onStateChange)).toEqual({
      ...ADVANCED_RULES,
      sort: { field: "title", order: "asc" },
    });
  });

  it("changes only the media type and keeps rules the Guided view can't show", () => {
    const onStateChange = renderPanel({ source: "query", query_definition: ADVANCED_RULES });

    choose("All Media", "Movies");

    expect(lastQuery(onStateChange)).toEqual({ ...ADVANCED_RULES, media_scope: "movie" });
  });

  it("keeps the saved sort when switching to collection order", () => {
    const onStateChange = renderPanel({
      source: "user_collection",
      collection_id: "col-1",
      query_definition: ADVANCED_RULES,
    });

    choose("Date Added", "Collection Order");

    expect(onStateChange.mock.lastCall?.[0].uses_source_order).toBe(true);
    expect(lastQuery(onStateChange)).toEqual(ADVANCED_RULES);
  });

  it("leaves collection order when a sort is picked", () => {
    const onStateChange = renderPanel({
      source: "user_collection",
      collection_id: "col-1",
      uses_source_order: true,
      query_definition: ADVANCED_RULES,
    });

    choose("Collection Order", "Title");

    expect(onStateChange.mock.lastCall?.[0].uses_source_order).toBe(false);
    expect(lastQuery(onStateChange)).toEqual({
      ...ADVANCED_RULES,
      sort: { field: "title", order: "asc" },
    });
  });
});

describe("CatalogFiltersPanel rules the Guided view can't show", () => {
  it("counts every rule on the Filters button and hides partial badges", () => {
    renderPanel({ source: "query", query_definition: ADVANCED_RULES });

    expect(screen.getByRole("button", { name: /Filters/ })).toHaveTextContent("5");
    expect(screen.queryByText(/^Actor:/)).toBeNull();
  });

  it("opens the Filters sheet in Advanced with Guided switched off", () => {
    renderPanel({ source: "query", query_definition: ADVANCED_RULES });

    fireEvent.click(screen.getByRole("button", { name: /Filters/ }));
    const sheet = screen.getByRole("dialog");

    expect(within(sheet).getByRole("button", { name: "Guided" })).toBeDisabled();
    expect(
      within(sheet).getByText("These rules use options the Guided view can't show."),
    ).toBeVisible();
    expect(within(sheet).getByText("Rule Groups")).toBeVisible();
  });
});

describe("CatalogFiltersPanel catalog filters the Guided view can show", () => {
  it("shows a badge per filter and opens the Guided editor", () => {
    renderPanel({ source: "query", query_definition: GENRE_LINK });

    expect(screen.getByText("Genre: Fantasy")).toBeVisible();
    expect(screen.getByRole("button", { name: /Filters/ })).toHaveTextContent("1");

    fireEvent.click(screen.getByRole("button", { name: /Filters/ }));
    const sheet = screen.getByRole("dialog");

    expect(within(sheet).getByRole("button", { name: "Guided" })).toBeEnabled();
    expect(within(sheet).getByText("Genres")).toBeVisible();
    expect(within(sheet).queryByText(/Guided view can't show/)).toBeNull();
  });

  it("clears a filter from its badge", () => {
    const onStateChange = renderPanel({ source: "query", query_definition: GENRE_LINK });

    fireEvent.click(screen.getByRole("button", { name: "Remove Genre: Fantasy" }));

    expect(lastQuery(onStateChange)?.groups).toEqual([]);
  });
});
