import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router";
import { beforeEach, describe, expect, it, vi } from "vitest";

import CollectionEditor from "./CollectionEditor";
import { buildSaveAsSmartCollectionHref, parseCatalogSearchParams } from "./catalogSearchParams";

const mocks = vi.hoisted(() => ({
  v2: vi.fn(),
  useCatalogWindow: vi.fn(),
}));

vi.mock("@/api/v2/request", async () => {
  const actual = await vi.importActual<typeof import("@/api/v2/request")>("@/api/v2/request");
  return {
    ...actual,
    v2: (operation: string, options?: unknown) => mocks.v2(operation, options),
  };
});
vi.mock("@/hooks/queries/catalog", () => ({
  useCatalogWindow: (...args: unknown[]) => mocks.useCatalogWindow(...args),
}));
vi.mock("@/components/catalog/CatalogFiltersPanel", () => ({ default: () => null }));
vi.mock("@/components/ItemGrid", () => ({ default: () => null }));
vi.mock("@/hooks/queries/profiles", () => ({ useProfiles: () => ({ data: [] }) }));
vi.mock("@/hooks/queries/libraries", () => ({ useUserLibraries: () => ({ data: [] }) }));
vi.mock("@/hooks/useCurrentProfile", () => ({
  useCurrentProfile: () => ({ profile: { id: "p" } }),
}));
vi.mock("sonner", () => ({
  toast: { success: vi.fn(), error: vi.fn() },
}));

class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}

const catalogFilterParams = new URLSearchParams({
  source: "query",
  q: "cruise", // text search — excluded from the structured-filter seed
  library_id: "3",
  type: "movie", // media scope
  match: "any", // top-level OR across groups
  sort: "year",
  order: "asc",
  query_limit: "25",
  request_page: "4", // pagination — excluded from the seed
  "groups[0][match]": "all",
  "groups[0][rules][0][field]": "genre",
  "groups[0][rules][0][op]": "contains",
  "groups[0][rules][0][value]": "Action",
  "groups[1][match]": "all",
  "groups[1][rules][0][field]": "year",
  "groups[1][rules][0][op]": "gte",
  "groups[1][rules][0][value]": "2000",
});

const seededGroups = [
  { match: "all", rules: [{ field: "genre", op: "contains", value: "Action" }] },
  { match: "all", rules: [{ field: "year", op: "gte", value: 2000 }] },
];

function seededHref() {
  return buildSaveAsSmartCollectionHref(parseCatalogSearchParams(catalogFilterParams));
}

function showSeededEditor() {
  render(
    <QueryClientProvider client={new QueryClient()}>
      <MemoryRouter initialEntries={[seededHref()]}>
        <Routes>
          <Route path="/collections/new" element={<CollectionEditor />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  vi.stubGlobal("ResizeObserver", ResizeObserverStub);
  mocks.useCatalogWindow.mockReturnValue({
    data: { totalItems: 3, pages: new Map() },
    isLoading: false,
  });
  mocks.v2.mockImplementation((operation: string) => {
    if (operation === "GET /api/v2/collections") {
      return Promise.resolve({ items: [], groups: [] });
    }
    if (operation === "GET /api/v2/collections/capabilities") {
      return Promise.resolve({ artwork: false, display_filter_presets: [] });
    }
    if (operation === "POST /api/v2/collections") {
      return Promise.resolve({ id: "nc1", name: "Action shelf", collection_type: "smart" });
    }
    return Promise.reject(new Error(`unexpected operation: ${operation}`));
  });
});

describe("save catalog filters as a smart collection", () => {
  it("round-trips the catalog filter state through the seed href", () => {
    const href = seededHref();
    const seedParams = new URLSearchParams(href.split("?")[1]);
    const parsed = parseCatalogSearchParams(seedParams);

    expect(parsed.source).toBe("query");
    // The structured filters survive whole: top-level match, groups,
    // sort/order, limit, library, and media scope.
    expect(parsed.query_definition.match).toBe("any");
    expect(parsed.query_definition.groups).toEqual(seededGroups);
    expect(parsed.query_definition.sort).toEqual({ field: "year", order: "asc" });
    expect(parsed.query_definition.limit).toBe(25);
    expect(parsed.query_definition.library_ids).toEqual([3]);
    expect(parsed.query_definition.media_scope).toBe("movie");
    // Text search and pagination are view state, not collection filters.
    expect(seedParams.has("q")).toBe(false);
    expect(seedParams.has("request_page")).toBe(false);
  });

  it("seeds the smart wizard with the current filters and saves via POST /api/v2/collections", async () => {
    showSeededEditor();

    // Step 1 previews the seeded query definition.
    expect(await screen.findByRole("heading", { name: "New Collection" })).toBeInTheDocument();
    expect(mocks.useCatalogWindow).toHaveBeenCalledWith(
      expect.objectContaining({
        source: "query",
        query_definition: expect.objectContaining({
          match: "any",
          library_ids: [3],
          groups: seededGroups,
        }),
      }),
      expect.anything(),
    );

    fireEvent.click(screen.getByRole("button", { name: "Next: Details" }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Action shelf" } });
    fireEvent.click(screen.getByRole("button", { name: "Create Collection" }));

    await waitFor(() =>
      expect(mocks.v2).toHaveBeenCalledWith("POST /api/v2/collections", expect.anything()),
    );
    const createCall = mocks.v2.mock.calls.find(([operation]) =>
      String(operation).startsWith("POST /api/v2/collections"),
    );
    expect(createCall?.[1]).toMatchObject({
      body: {
        name: "Action shelf",
        collection_type: "smart",
        query_definition: expect.objectContaining({
          match: "any",
          library_ids: [3],
          media_scope: "movie",
          limit: 25,
          groups: seededGroups,
        }),
      },
    });
  });
});
