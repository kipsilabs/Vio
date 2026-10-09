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
// The filter panel stays real in this file; only its data hooks are stubbed.
vi.mock("@/hooks/queries/catalog", () => ({
  useCatalogWindow: (...args: unknown[]) => mocks.useCatalogWindow(...args),
  useCatalogFilters: () => ({ data: { genres: [] }, isLoading: false }),
  useCatalogMetadataFilters: () => ({ data: undefined, isLoading: false }),
}));
vi.mock("@/hooks/queries/ratingsCapability", () => ({
  useShownRatingSources: () => new Set(["imdb"]),
}));
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

// Two OR-connected groups plus an unrelated media scope, library, and sort.
const catalogFilterParams = new URLSearchParams({
  source: "query",
  library_id: "3",
  type: "movie",
  match: "any",
  sort: "year",
  order: "asc",
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

function showSeededEditor() {
  const seedHref = buildSaveAsSmartCollectionHref(parseCatalogSearchParams(catalogFilterParams));
  return render(
    <QueryClientProvider client={new QueryClient()}>
      <MemoryRouter initialEntries={[seedHref]}>
        <Routes>
          <Route path="/collections/new" element={<CollectionEditor />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

function lastCreateBody() {
  const createCall = mocks.v2.mock.calls.find(([operation]) =>
    String(operation).startsWith("POST /api/v2/collections"),
  );
  return (createCall?.[1] as { body: Record<string, unknown> }).body;
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
      return Promise.resolve({ id: "nc1", name: "Kept OR filters", collection_type: "smart" });
    }
    return Promise.reject(new Error(`unexpected operation: ${operation}`));
  });
});

describe("seeded OR filters through the real filter panel", () => {
  it("keeps both OR groups after an unrelated edit made in the seeded wizard", async () => {
    showSeededEditor();

    // The real panel interprets the seed: both groups surface as active
    // filter badges rather than a mocked-out panel.
    expect(await screen.findByText("Genre: Action")).toBeInTheDocument();
    expect(screen.getByText("Year: >= 2000")).toBeInTheDocument();
    expect(mocks.useCatalogWindow).toHaveBeenCalledWith(
      expect.objectContaining({
        query_definition: expect.objectContaining({ match: "any", groups: seededGroups }),
      }),
      expect.anything(),
    );

    // Unrelated edit: change the result cap, not the filter groups.
    fireEvent.change(screen.getByLabelText("Max items"), { target: { value: "50" } });
    fireEvent.blur(screen.getByLabelText("Max items"));

    fireEvent.click(screen.getByRole("button", { name: "Next: Details" }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Kept OR filters" } });
    fireEvent.click(screen.getByRole("button", { name: "Create Collection" }));

    await waitFor(() =>
      expect(mocks.v2).toHaveBeenCalledWith("POST /api/v2/collections", expect.anything()),
    );
    expect(lastCreateBody()).toMatchObject({
      name: "Kept OR filters",
      collection_type: "smart",
      query_definition: expect.objectContaining({
        match: "any",
        groups: seededGroups,
        limit: 50,
      }),
    });
  });
});
